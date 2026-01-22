package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

/*
========================
Daily Summary (FINAL)
- ALWAYS full raw
- ALWAYS chunked (token-safe)
- FORCE only controls delete/recompute
========================
*/

func ensureDaily(cfg Config, db *sql.DB, date string, force bool) error {
	// ---------- FORCE MODE ----------
	if force {
		_, _ = db.Exec(`
			DELETE FROM embeddings
			WHERE summary_id IN (
				SELECT id FROM summaries
				WHERE type='daily' AND period_key=?
			)
		`, date)

		_, _ = db.Exec(`
			DELETE FROM summaries
			WHERE type='daily' AND period_key=?
		`, date)

		_ = os.Remove(filepath.Join(cfg.LogDir, date+".daily.json"))
	}

	// ---------- IDEMPOTENT CHECK ----------
	if !force {
		if ok, _ := summaryExists(db, "daily", date); ok {
			// 即使 daily 已存在，也要确保 pending_facts 能被持续补齐
			if b, err := os.ReadFile(filepath.Join(cfg.LogDir, date+".daily.json")); err == nil {
				if err := EnsurePendingFactsFromDailyJSON(cfg, db, date, string(b)); err != nil {
					fmt.Fprintf(os.Stderr, "[warn] pending facts ingest failed: %v\n", err)
				}
			}
			return nil
		}
	}

	logPath := filepath.Join(cfg.LogDir, date+".jsonl")
	info, err := os.Stat(logPath)
	if err != nil || info.Size() == 0 {
		return nil
	}

	// ---------- READ FULL RAW ----------
	rawAll, err := os.ReadFile(logPath)
	if err != nil {
		return err
	}

	// ---------- SPLIT INTO TOKEN-SAFE CHUNKS ----------
	chunks := splitJSONLIntoChunks(rawAll, cfg.MaxDailyJSONLBytes)

	var dailyJSON string

	if len(chunks) == 1 {
		prompt := mustReadPrompt(cfg, "daily.txt")
		prompt = strings.ReplaceAll(prompt, "{{DATE}}", date)
		prompt = strings.ReplaceAll(prompt, "{{TRANSCRIPT}}", string(chunks[0]))

		out, err := callLLMNonStream(cfg, prompt)
		if err != nil {
			return err
		}
		if !json.Valid([]byte(out)) {
			return fmt.Errorf("daily llm output is not valid JSON\nraw:\n%s", out)
		}
		dailyJSON = out
	} else {
		partials := make([]string, 0, len(chunks))

		for i, c := range chunks {
			prompt := mustReadPrompt(cfg, "daily.txt")
			prompt = strings.ReplaceAll(prompt, "{{DATE}}", date)

			transcript := fmt.Sprintf(
				"【PART %d/%d】\n%s",
				i+1, len(chunks), string(c),
			)
			prompt = strings.ReplaceAll(prompt, "{{TRANSCRIPT}}", transcript)

			out, err := callLLMNonStream(cfg, prompt)
			if err != nil {
				return err
			}
			if !json.Valid([]byte(out)) {
				return fmt.Errorf(
					"daily chunk %d output invalid JSON\nraw:\n%s",
					i+1, out,
				)
			}
			partials = append(partials, out)
		}

		mergePrompt := buildDailyMergePrompt(date, partials)
		merged, err := callLLMNonStream(cfg, mergePrompt)
		if err != nil {
			return err
		}
		if !json.Valid([]byte(merged)) {
			return fmt.Errorf(
				"daily merged output invalid JSON\nraw:\n%s",
				merged,
			)
		}
		dailyJSON = merged
	}

	// ---------- USER FACT EXTRACTION ----------
	rawLines, _ := loadRawLinesForDate(cfg, date)
	userFacts := ExtractUserFactsFromRaw(rawLines)

	out, err := buildDailyFinal(dailyJSON, userFacts)
	if err != nil {
		return err
	}

	// ---------- PENDING FACT INGESTION (user_facts_explicit → pending_facts) ----------
	if err := EnsurePendingFactsFromDailyJSON(cfg, db, date, out); err != nil {
		fmt.Fprintf(os.Stderr, "[warn] pending facts ingest failed: %v\n", err)
	}

	// ---------- SUMMARY GUARDS ----------
	warnings := RunSummaryGuards(db, "daily", out)
	for _, w := range warnings {
		// 这里只报警，不中断
		log.Printf("[SUMMARY %s] %s", w.Type, w.Message)
	}

	// ---------- WRITE DAILY FILE ----------
	outPath := filepath.Join(cfg.LogDir, date+".daily.json")
	if err := os.WriteFile(outPath, []byte(out), 0644); err != nil {
		return fmt.Errorf("write daily file failed: %w", err)
	}

	// ---------- INDEX + DB ----------
	indexText := extractIndexText(out)

	_, err = upsertSummary(
		db,
		cfg,
		"daily",
		date,
		date,
		date,
		out,
		indexText,
		logPath,
	)
	if err != nil {
		return err
	}

	// ---------- EMBEDDING ----------
	// Best effort (non-fatal) - retrieval still works in degraded mode without new vectors.
	if err := ensureEmbedding(db, cfg, indexText, "daily", date); err != nil {
		log.Printf("[warn] ensureEmbedding failed for daily %s: %v", date, err)
	}
	return nil
}

/*
========================
Helpers
========================
*/

// -------- raw lines (for user facts) --------

func loadRawLinesForDate(cfg Config, date string) ([]RawLine, error) {
	path := filepath.Join(cfg.LogDir, date+".jsonl")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var lines []RawLine
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var r RawLine
		if err := json.Unmarshal([]byte(line), &r); err == nil {
			lines = append(lines, r)
		}
	}

	return lines, nil
}

// -------- final JSON builder --------

func buildDailyFinal(llmJSON string, userFacts []string) (string, error) {
	llmJSON = strings.TrimSpace(llmJSON)
	if llmJSON == "" {
		return "", fmt.Errorf("daily llm output is empty")
	}

	if !json.Valid([]byte(llmJSON)) {
		return "", fmt.Errorf(
			"daily llm output is not valid JSON\nraw:\n%s",
			llmJSON,
		)
	}

	var obj map[string]any
	if err := json.Unmarshal([]byte(llmJSON), &obj); err != nil {
		return "", fmt.Errorf(
			"daily llm output json unmarshal failed: %w\nraw:\n%s",
			err,
			llmJSON,
		)
	}

	// 注意：user_facts_explicit 是 LLM 或人工确认的“显式事实”，不要被隐式抽取覆盖。
	// 这里把从 raw 抽取到的事实放在 user_facts_implicit 里，便于后续对比/审核。
	if len(userFacts) > 0 {
		existing := extractStringList(obj["user_facts_implicit"])
		merged := append(existing, userFacts...)
		obj["user_facts_implicit"] = dedupStrings(merged)
	}

	outBytes, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return "", fmt.Errorf("daily json marshal failed: %w", err)
	}

	return string(outBytes), nil
}

func extractStringList(v any) []string {
	if v == nil {
		return nil
	}
	switch x := v.(type) {
	case []any:
		var out []string
		for _, it := range x {
			if s, ok := it.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return x
	case string:
		if strings.TrimSpace(x) == "" {
			return nil
		}
		return []string{x}
	default:
		return nil
	}
}

func dedupStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// -------- chunking (token-safe) --------

// 按 JSONL 行切分，保证每块 <= maxBytes，不破坏行结构
func splitJSONLIntoChunks(raw []byte, maxBytes int64) [][]byte {
	lines := strings.Split(string(raw), "\n")

	var chunks [][]byte
	var b strings.Builder

	flush := func() {
		if b.Len() > 0 {
			chunks = append(chunks, []byte(b.String()))
			b.Reset()
		}
	}

	for _, line := range lines {
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" {
			continue
		}

		nextLen := int64(b.Len() + len(line) + 1)
		if b.Len() > 0 && nextLen > maxBytes {
			flush()
		}

		b.WriteString(line)
		b.WriteString("\n")

		// 极端情况：单行超过 maxBytes
		if int64(b.Len()) > maxBytes {
			flush()
		}
	}

	flush()

	if len(chunks) == 0 {
		return [][]byte{[]byte{}}
	}
	return chunks
}

// -------- merge prompt --------

func buildDailyMergePrompt(date string, partials []string) string {
	var b strings.Builder

	b.WriteString("You are a strict daily summary reducer.\n")
	b.WriteString("Merge multiple partial daily summaries into ONE final daily summary.\n\n")

	b.WriteString("CRITICAL RULES:\n")
	b.WriteString("- Output JSON only.\n")
	b.WriteString("- Do NOT add new facts.\n")
	b.WriteString("- Do NOT infer user identity.\n")
	b.WriteString("- Deduplicate and merge semantically.\n\n")

	b.WriteString("OUTPUT FORMAT (JSON only):\n")
	b.WriteString("{\n")
	b.WriteString(`  "type": "daily",` + "\n")
	b.WriteString(fmt.Sprintf(`  "date": "%s",`+"\n", date))
	b.WriteString(`  "topics": [],` + "\n")
	b.WriteString(`  "patterns": [],` + "\n")
	b.WriteString(`  "open_questions": [],` + "\n")
	b.WriteString(`  "highlights": [],` + "\n")
	b.WriteString(`  "lowlights": []` + "\n")
	b.WriteString("}\n\n")

	b.WriteString("PARTIAL DAILY SUMMARIES:\n")
	for i, p := range partials {
		b.WriteString(fmt.Sprintf("\n--- PART %d/%d ---\n", i+1, len(partials)))
		b.WriteString(strings.TrimSpace(p))
		b.WriteString("\n")
	}

	return b.String()
}
