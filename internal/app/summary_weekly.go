package app

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log" // ⭐ 新增：用于 guard 报警
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

/*
========================
Weekly Summary (FINAL)
- Daily JSON slimming
- Chunked weekly if needed
========================
periodKey = YYYY-Www
*/

func ensureWeekly(cfg Config, db *sql.DB, weekKey string, force bool) error {
	// ---------- FORCE MODE ----------
	if force {
		_, _ = db.Exec(`
			DELETE FROM embeddings
			WHERE summary_id IN (
				SELECT id FROM summaries
				WHERE type='weekly' AND period_key=?
			)
		`, weekKey)

		_, _ = db.Exec(`
			DELETE FROM summaries
			WHERE type='weekly' AND period_key=?
		`, weekKey)

		_ = os.Remove(filepath.Join(cfg.LogDir, weekKey+".weekly.json"))
	}

	// ---------- IDEMPOTENT CHECK ----------
	if !force {
		if ok, _ := summaryExists(db, "weekly", weekKey); ok {
			return nil
		}
	}

	// ---------- COLLECT DAILY ----------
	dailies := collectDailySummariesForWeek(cfg, weekKey)
	if len(dailies) == 0 {
		return nil
	}

	// ---------- WEEK RANGE ----------
	year, week := parseWeekKey(weekKey)

	ref := time.Date(year, 1, 4, 0, 0, 0, 0, cfg.Location)
	for {
		y, w := ref.ISOWeek()
		if y == year && w == week {
			break
		}
		ref = ref.AddDate(0, 0, 1)
	}
	startT, endT := weekRange(ref, cfg.Location)
	weekStart := startT.Format("2006-01-02")
	weekEnd := endT.Format("2006-01-02")

	// ---------- SLIM DAILY JSON ----------
	slimmed := make([]map[string]any, 0, len(dailies))
	for _, s := range dailies {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !json.Valid([]byte(s)) {
			return fmt.Errorf("weekly refused: daily summary invalid JSON")
		}

		var obj map[string]any
		if err := json.Unmarshal([]byte(s), &obj); err != nil {
			return fmt.Errorf("weekly refused: daily json unmarshal failed: %w", err)
		}

		slim := map[string]any{
			"date":           obj["date"],
			"topics":         obj["topics"],
			"patterns":       obj["patterns"],
			"open_questions": obj["open_questions"],
			"highlights":     obj["highlights"],
			"lowlights":      obj["lowlights"],
		}
		slimmed = append(slimmed, slim)
	}

	rawBytes, err := json.Marshal(slimmed)
	if err != nil {
		return fmt.Errorf("weekly marshal slimmed dailies failed: %w", err)
	}

	// ---------- CHUNK IF NEEDED ----------
	chunks := splitJSONBytes(rawBytes, cfg.MaxDailyJSONLBytes)

	var weeklyJSON string

	if len(chunks) == 1 {
		prompt := mustReadPrompt(cfg, "weekly.txt")
		prompt = strings.ReplaceAll(prompt, "{{WEEK_START}}", weekStart)
		prompt = strings.ReplaceAll(prompt, "{{WEEK_END}}", weekEnd)
		prompt = strings.ReplaceAll(prompt, "{{DAILY_JSON_ARRAY}}", string(chunks[0]))

		out, err := callLLMNonStream(cfg, prompt)
		if err != nil {
			return err
		}
		out = strings.TrimSpace(out)
		if out == "" {
			return fmt.Errorf("weekly llm output is empty")
		}
		if !json.Valid([]byte(out)) {
			return fmt.Errorf("weekly llm output is not valid JSON\nraw:\n%s", out)
		}
		weeklyJSON = out
	} else {
		partials := make([]string, 0, len(chunks))

		for i, c := range chunks {
			prompt := mustReadPrompt(cfg, "weekly.txt")
			prompt = strings.ReplaceAll(prompt, "{{WEEK_START}}", weekStart)
			prompt = strings.ReplaceAll(prompt, "{{WEEK_END}}", weekEnd)

			prompt = strings.ReplaceAll(
				prompt,
				"{{DAILY_JSON_ARRAY}}",
				fmt.Sprintf("/* PART %d/%d */\n%s", i+1, len(chunks), string(c)),
			)

			out, err := callLLMNonStream(cfg, prompt)
			if err != nil {
				return err
			}
			out = strings.TrimSpace(out)
			if out == "" {
				return fmt.Errorf("weekly chunk %d output is empty", i+1)
			}
			if !json.Valid([]byte(out)) {
				return fmt.Errorf("weekly chunk %d output invalid JSON\nraw:\n%s", i+1, out)
			}
			partials = append(partials, out)
		}

		mergePrompt := buildWeeklyMergePrompt(weekKey, weekStart, weekEnd, partials)
		merged, err := callLLMNonStream(cfg, mergePrompt)
		if err != nil {
			return err
		}
		merged = strings.TrimSpace(merged)
		if merged == "" {
			return fmt.Errorf("weekly merged output is empty")
		}
		if !json.Valid([]byte(merged)) {
			return fmt.Errorf("weekly merged output invalid JSON\nraw:\n%s", merged)
		}
		weeklyJSON = merged
	}

	// ---------- ⭐ SUMMARY GUARDS（新增） ----------
	warnings := RunSummaryGuards(db, "weekly", weeklyJSON)
	for _, w := range warnings {
		log.Printf("[SUMMARY %s] %s", w.Type, w.Message)
	}

	// ---------- WRITE FILE ----------
	outPath := filepath.Join(cfg.LogDir, weekKey+".weekly.json")
	if err := os.WriteFile(outPath, []byte(weeklyJSON), 0644); err != nil {
		return err
	}

	// ---------- INDEX + DB ----------
	indexText := extractIndexText(weeklyJSON)

	summaryID, err := upsertSummary(
		db,
		cfg,
		"weekly",
		weekKey,
		weekStart,
		weekEnd,
		weeklyJSON,
		indexText,
		outPath,
	)
	if err != nil {
		return err
	}

	// ---------- ⭐ EMBEDDING DRIFT GUARD ----------
	{
		// 1. 手动调用 embedding HTTP（复用现有逻辑）
		payload := map[string]any{
			"input": indexText,
		}
		b, _ := json.Marshal(payload)

		req, err := http.NewRequest("POST", cfg.EmbedURL, bytes.NewReader(b))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")

			resp, err := embedHTTPClient.Do(req)
			if err == nil && resp.StatusCode/100 == 2 {
				raw, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()

				vec, err := decodeEmbedding(raw)
				if err == nil {
					// 2. embedding drift 检测
					if warn := CheckEmbeddingDrift(db, summaryID, vec); warn != nil {
						log.Printf("[EMBEDDING %s] %s", warn.Level, warn.Message)
						if warn.Level == "BLOCK" {
							return nil // ⛔ 阻断 embedding 覆盖
						}
					}

					// 3. 保存历史
					saveEmbeddingHistory(db, summaryID, vec)
				}
			}
		}
	}

	// ---------- EMBEDDING ----------
	// Best effort (non-fatal) - retrieval still works in degraded mode without new vectors.
	if err := ensureEmbedding(db, cfg, indexText, "weekly", weekKey); err != nil {
		log.Printf("[warn] ensureEmbedding failed for weekly %s: %v", weekKey, err)
	}

	return nil
}

/*
========================
Helpers (ALL IN THIS FILE)
========================
*/

func parseWeekKey(weekKey string) (year int, week int) {
	fmt.Sscanf(weekKey, "%d-W%d", &year, &week)
	return
}

func collectDailySummariesForWeek(cfg Config, weekKey string) []string {
	year, week := parseWeekKey(weekKey)

	ref := time.Date(year, 1, 4, 0, 0, 0, 0, cfg.Location)
	for {
		y, w := ref.ISOWeek()
		if y == year && w == week {
			break
		}
		ref = ref.AddDate(0, 0, 1)
	}

	start, end := weekRange(ref, cfg.Location)

	var out []string
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		dateKey := d.Format("2006-01-02")
		path := filepath.Join(cfg.LogDir, dateKey+".daily.json")
		if b, err := os.ReadFile(path); err == nil {
			out = append(out, strings.TrimSpace(string(b)))
		}
	}
	return out
}

/* 后续 splitJSONBytes / hardSplitBytes / buildWeeklyMergePrompt
   完整保持你原样 —— 未删任何一行 */

// splitJSONBytes：把一个大的 JSON 字节串切成多个 chunk，每个 <= maxBytes。
// 注意：为了稳定性，这里按“对象级”切分（外层必须是 JSON array）。
func splitJSONBytes(arrJSON []byte, maxBytes int64) [][]byte {
	if maxBytes <= 0 {
		return [][]byte{arrJSON}
	}
	if int64(len(arrJSON)) <= maxBytes {
		return [][]byte{arrJSON}
	}

	// 外层必须是 array
	var items []json.RawMessage
	if err := json.Unmarshal(arrJSON, &items); err != nil || len(items) == 0 {
		// 兜底：无法解析时，直接硬切（仍然保证不会 OOM，只是不保证语义）
		return hardSplitBytes(arrJSON, maxBytes)
	}

	var chunks [][]byte
	var cur []json.RawMessage
	var curSize int64 = 2 // for "[]"

	flush := func() {
		if len(cur) == 0 {
			return
		}
		b, _ := json.Marshal(cur)
		chunks = append(chunks, b)
		cur = nil
		curSize = 2
	}

	for _, it := range items {
		itSize := int64(len(it))
		// 逗号 + 空间
		add := itSize
		if len(cur) > 0 {
			add += 1
		}

		if len(cur) > 0 && curSize+add > maxBytes {
			flush()
		}

		cur = append(cur, it)
		curSize += add

		// 极端：单个 item 就超过 maxBytes
		if int64(len(it)) > maxBytes {
			flush()
			chunks = append(chunks, []byte("["+string(it)+"]"))
		}
	}

	flush()

	if len(chunks) == 0 {
		return [][]byte{arrJSON}
	}
	return chunks
}

func hardSplitBytes(b []byte, maxBytes int64) [][]byte {
	if maxBytes <= 0 {
		return [][]byte{b}
	}
	var out [][]byte
	for i := int64(0); i < int64(len(b)); i += maxBytes {
		end := i + maxBytes
		if end > int64(len(b)) {
			end = int64(len(b))
		}
		out = append(out, b[i:end])
	}
	return out
}

func buildWeeklyMergePrompt(weekKey, weekStart, weekEnd string, partials []string) string {
	var b strings.Builder

	b.WriteString("You are a strict weekly summary reducer.\n")
	b.WriteString("Merge multiple partial weekly summaries into ONE final weekly summary.\n\n")

	b.WriteString("CRITICAL RULES:\n")
	b.WriteString("- Output JSON only.\n")
	b.WriteString("- Do NOT add new facts.\n")
	b.WriteString("- Do NOT infer user identity.\n")
	b.WriteString("- Deduplicate and merge semantically.\n\n")

	b.WriteString("OUTPUT FORMAT (JSON only):\n")
	b.WriteString("{\n")
	b.WriteString(`  "type": "weekly",` + "\n")
	b.WriteString(fmt.Sprintf(`  "week_key": "%s",`+"\n", weekKey))
	b.WriteString(fmt.Sprintf(`  "week_start": "%s",`+"\n", weekStart))
	b.WriteString(fmt.Sprintf(`  "week_end": "%s",`+"\n", weekEnd))
	b.WriteString(`  "themes": [],` + "\n")
	b.WriteString(`  "progress": [],` + "\n")
	b.WriteString(`  "recurring_blockers": [],` + "\n")
	b.WriteString(`  "notable_decisions": [],` + "\n")
	b.WriteString(`  "next_week_focus": []` + "\n")
	b.WriteString("}\n\n")

	b.WriteString("PARTIAL WEEKLY SUMMARIES:\n")
	for i, p := range partials {
		b.WriteString(fmt.Sprintf("\n--- PART %d/%d ---\n", i+1, len(partials)))
		b.WriteString(strings.TrimSpace(p))
		b.WriteString("\n")
	}

	return b.String()
}
