package app

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"strings"
	"time"
)

/*
================================================
User Fact Extraction (V2)
用于「隐式事实抽取」（非 /remember）
================================================
*/

type RawLine struct {
	Role    string
	Content string
}

/*
------------------------------------------------
隐式事实抽取（保持不动）
------------------------------------------------
*/

func isUserFactV2(user RawLine, assistant RawLine) bool {
	if user.Role != "user" || assistant.Role != "assistant" {
		return false
	}

	u := normalizeText(user.Content)
	a := normalizeText(assistant.Content)

	if !looksLikeSelfStatement(u) {
		return false
	}

	if !assistantAffirmsUser(u, a) {
		return false
	}

	return true
}

func looksLikeSelfStatement(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	if !strings.HasPrefix(text, "我") {
		return false
	}
	if strings.HasSuffix(text, "吗") ||
		strings.HasSuffix(text, "?") ||
		strings.HasSuffix(text, "？") {
		return false
	}
	if strings.Contains(text, "帮我") ||
		strings.Contains(text, "请你") {
		return false
	}
	return true
}

func assistantAffirmsUser(userText, assistantText string) bool {
	if !strings.Contains(assistantText, "你") {
		return false
	}
	core := extractUserCore(userText)
	if core == "" {
		return false
	}
	return strings.Contains(assistantText, core)
}

func extractUserCore(text string) string {
	text = strings.TrimSpace(strings.TrimPrefix(text, "我"))
	text = strings.Trim(text, "。！! ")
	r := []rune(text)
	if len(r) > 20 {
		text = string(r[:20])
	}
	return text
}

func normalizeText(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "，", ",")
	s = strings.ReplaceAll(s, "。", "")
	s = strings.ReplaceAll(s, "！", "")
	s = strings.ReplaceAll(s, "？", "")
	return s
}

func ExtractUserFactsFromRaw(lines []RawLine) []string {
	var facts []string
	for i := 0; i+1 < len(lines); i++ {
		if isUserFactV2(lines[i], lines[i+1]) {
			facts = append(facts, lines[i].Content)
		}
	}
	return facts
}

/*
================================================
显式事实（/remember /forget）
================================================
*/

func normalizeFactKey(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Join(strings.Fields(s), " ")
	s = strings.ToLower(s)
	return s
}

/*
-------------------------
Fact 主体抽取（关键）
-------------------------
*/

// 从自然语言中抽取“现实对象主体”
func extractFactSubject(fact string) string {
	fact = strings.TrimSpace(fact)

	if i := strings.Index(fact, "就是"); i > 0 {
		return strings.TrimSpace(fact[:i])
	}
	if i := strings.Index(fact, "是"); i > 0 {
		return strings.TrimSpace(fact[:i])
	}
	return ""
}

// ✅ 从“主体”派生稳定 fact_key（根解法）
func deriveFactKeyFromSubject(content string) string {
	subject := extractFactSubject(content)
	if subject == "" {
		// 没有明确主体的事实，退回全文（行为与现在一致）
		return normalizeFactKey(content)
	}
	return "subject:" + normalizeFactKey(subject)
}

/*
-------------------------
Fact 冲突检测（保留，但已不再是唯一保障）
-------------------------
*/

func findConflictingFacts(db *sql.DB, subject string) ([]string, error) {
	if subject == "" {
		return nil, nil
	}

	rows, err := db.Query(`
		SELECT fact
		FROM user_facts
		WHERE is_active=1
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var conflicts []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			continue
		}
		if extractFactSubject(f) == subject {
			conflicts = append(conflicts, f)
		}
	}
	return conflicts, nil
}

/*
================================================
Embedding 打通：把 /remember 的 fact 写入 embeddings
================================================
*/

func upsertEmbedding(db *sql.DB, summaryID int64, vec []float32, l2 float64, createdAt string) error {
	var buf bytes.Buffer
	for _, v := range vec {
		_ = binary.Write(&buf, binary.LittleEndian, v)
	}

	_, _ = db.Exec(`DELETE FROM embeddings WHERE summary_id=?`, summaryID)

	_, err := db.Exec(`
		INSERT INTO embeddings(summary_id, dim, vec, l2, created_at)
		VALUES(?,?,?,?,?)
	`,
		summaryID,
		len(vec),
		buf.Bytes(),
		l2,
		createdAt,
	)
	return err
}

func upsertEmbeddingFromText(cfg Config, db *sql.DB, summaryID int64, text string) error {
	vec, l2, err := embedQueryText(cfg, text)
	if err != nil {
		return err
	}
	if len(vec) == 0 || l2 == 0 {
		return nil
	}

	now := time.Now().In(cfg.Location).Format(time.RFC3339)
	return upsertEmbedding(db, summaryID, vec, l2, now)
}

func removeFactFromSearch(db *sql.DB, factKey string, reason string) {
	summaryKey := "fact:" + factKey

	row := db.QueryRow(`SELECT id FROM summaries WHERE type='fact' AND period_key=?`, summaryKey)
	var id int64
	if err := row.Scan(&id); err == nil && id > 0 {
		_ = deleteEmbedding(db, id)
		_, _ = db.Exec(`UPDATE summaries SET source_path=? WHERE id=?`, reason, id)
	}
}

/*
-------------------------
/remember
-------------------------
*/

// RememberFactWithOutcome is the shared implementation for /remember.
// It writes raw logs (so daily pipeline can see the confirmation) and returns the outcome.
func RememberFactWithOutcome(lw *LogWriter, cfg Config, db *sql.DB, content string) (*RememberOutcome, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return &RememberOutcome{Status: "noop"}, nil
	}

	now := time.Now().In(cfg.Location)
	today := now.Format("2006-01-02")

	// ✅ 冲突/版本化：不同事实但同主体 -> 进入冲突池，等待用户裁决
	out, err := ProposeRememberFact(cfg, db, content, "remember_cli", today, now)
	if err != nil {
		return nil, err
	}

	// 4️⃣ raw 日志
	if lw != nil {
		if out != nil && out.Status == "conflict" {
			_ = lw.WriteRecord(map[string]string{
				"role":    "user",
				"content": "我提出一个可能的事实：" + content,
			})
			_ = lw.WriteRecord(map[string]string{
				"role":    "assistant",
				"content": "我记录了一个事实确认冲突，需要你在 FACTS 面板里裁决后才会晋升为长期事实。",
			})
		} else {
			_ = lw.WriteRecord(map[string]string{
				"role":    "user",
				"content": "我确认一个事实：" + content,
			})
			_ = lw.WriteRecord(map[string]string{
				"role":    "assistant",
				"content": "我理解了，你提到" + content,
			})
		}
	}

	return out, nil
}

func RememberFact(lw *LogWriter, cfg Config, db *sql.DB, content string) error {
	_, err := RememberFactWithOutcome(lw, cfg, db, content)
	return err
}

/*
-------------------------
RememberFactSilent (UI / API)
- 与 /remember 同源写入逻辑
- 不写 raw 日志（避免污染对话记录）
-------------------------
*/

func RememberFactSilent(cfg Config, db *sql.DB, content string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}

	now := time.Now().In(cfg.Location)
	today := now.Format("2006-01-02")

	// UI 一键记住也走同一套冲突/版本化逻辑
	_, err := ProposeRememberFact(cfg, db, content, "remember_ui", today, now)
	return err
}

/*
-------------------------
/forget
-------------------------
*/

func ForgetFact(lw *LogWriter, cfg Config, db *sql.DB, content string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}

	now := time.Now().In(cfg.Location)

	if err := RetractFact(cfg, db, content, "forget_cli", now.Format("2006-01-02"), now); err != nil {
		return err
	}

	if lw != nil {
		_ = lw.WriteRecord(map[string]string{
			"role":    "user",
			"content": "我撤回之前的事实：" + content,
		})
		_ = lw.WriteRecord(map[string]string{
			"role":    "assistant",
			"content": "我理解了，你明确表示之前关于「" + content + "」的事实不再成立。",
		})
	}

	return nil
}
