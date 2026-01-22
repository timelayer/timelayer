package app

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

/*
================================================
Embedding HTTP client
================================================
*/

var embedHTTPClient = &http.Client{
	Timeout: 120 * time.Second,
}

/*
================================================
Unified llama embedding decoder
兼容所有已知 llama-server 返回格式：
1) { "embedding": [...] }
2) [ ... ]
3) [[ ... ]]
4) [{ "index": 0, "embedding": [[...]] }]
================================================
*/

func decodeEmbedding(raw []byte) ([]float32, error) {
	// case 1: { "embedding": [...] }
	var obj struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && len(obj.Embedding) > 0 {
		return obj.Embedding, nil
	}

	// case 2: [ ... ]
	var flat []float32
	if err := json.Unmarshal(raw, &flat); err == nil && len(flat) > 0 {
		return flat, nil
	}

	// case 3: [[ ... ]]
	var matrix [][]float32
	if err := json.Unmarshal(raw, &matrix); err == nil && len(matrix) > 0 && len(matrix[0]) > 0 {
		return matrix[0], nil
	}

	// case 4: [{ index, embedding: [[...]] }]
	var batch []struct {
		Index     int         `json:"index"`
		Embedding [][]float32 `json:"embedding"`
	}
	if err := json.Unmarshal(raw, &batch); err == nil &&
		len(batch) > 0 &&
		len(batch[0].Embedding) > 0 &&
		len(batch[0].Embedding[0]) > 0 {
		return batch[0].Embedding[0], nil
	}

	msg := strings.TrimSpace(string(raw))
	if len(msg) > 500 {
		msg = msg[:500] + "..."
	}
	return nil, fmt.Errorf("unknown embedding response format: %s", msg)
}

/*
================================================
Embedding writer (1:1 with summary)
================================================
*/

func ensureEmbedding(db *sql.DB, cfg Config, text, typ, key string) error {
	// find summary id
	row := db.QueryRow(
		`SELECT id FROM summaries WHERE type=? AND period_key=?`,
		typ, key,
	)
	var sid int64
	if err := row.Scan(&sid); err != nil {
		return err
	}

	// already embedded
	if hasEmbedding(db, sid) {
		return nil
	}

	payload := map[string]any{
		"input": text,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", cfg.EmbedURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := embedHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf(
			"embedding http error %d: %s",
			resp.StatusCode,
			strings.TrimSpace(string(body)),
		)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	embedding, err := decodeEmbedding(raw)
	if err != nil {
		return err
	}

	// serialize + L2
	buf := new(bytes.Buffer)
	var l2 float64
	for _, v := range embedding {
		_ = binary.Write(buf, binary.LittleEndian, v)
		l2 += float64(v * v)
	}
	l2 = math.Sqrt(l2)

	_, err = db.Exec(`
		INSERT INTO embeddings(summary_id, dim, vec, l2, created_at)
		VALUES(?,?,?,?,?)
	`,
		sid,
		len(embedding),
		buf.Bytes(),
		l2,
		time.Now().Format(time.RFC3339),
	)

	return err
}

/*
================================================
/help 文本（恢复完整版）
================================================
*/

const helpText = `
/help
    Show this help message.


/chat <message>
    Chat freely with the assistant.
    Uses recent conversation and long-term memory,
    but does not guarantee factual completeness.


/ask <question>
    Ask a question and get a direct answer
    based ONLY on your own historical records.
    The assistant will reason, summarize, and cite memory.
    If memory is insufficient, it will say so explicitly.


/search <query>
    Inspect what the system remembers.
    Performs semantic search over all stored memories
    (facts, daily / weekly / monthly summaries),
    and shows raw matching records without answering.


/daily
    Generate today's daily summary from raw conversation logs.

/daily --force
    Force regenerate today's daily summary.


/weekly
    Generate the current week's weekly summary
    based on existing daily summaries.

/weekly --force
    Force regenerate the current week's weekly summary.


/monthly
    Generate the current month's monthly summary
    based on existing weekly summaries.

/monthly --force
    Force regenerate the current month's monthly summary.


/reindex daily|weekly|monthly|all
    Rebuild embeddings for existing summaries.
    Does NOT regenerate summaries themselves.


/remember <fact>
    Explicitly teach the system a confirmed fact.
    Stored as authoritative long-term memory.


/forget <fact>
    Explicitly retract a previously remembered fact.
    The fact will no longer be treated as authoritative.


/paste
    Enter multi-line input.
    Submit with an empty line.


/debug <message>
    Print the full system prompt and evidence chain
    that would be sent to the model (no model call).

`

/*
================================================
Command normalization
解决：中文空格 / 多空格 / `/cmd参数`
================================================
*/

func normalizeCommand(input string) (cmd string, arg string) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", ""
	}

	// 全角空格 → 半角
	s = strings.ReplaceAll(s, "　", " ")
	s = strings.Join(strings.Fields(s), " ")

	if !strings.HasPrefix(s, "/") {
		return "", ""
	}

	if i := strings.IndexByte(s, ' '); i >= 0 {
		return s[:i], strings.TrimSpace(s[i+1:])
	}

	return s, ""
}

/*
================================================
CLI command router
================================================
*/

func handleCommand(cfg Config, db *sql.DB, lw *LogWriter, reader *bufio.Reader, input string) {
	cmd, arg := normalizeCommand(input)

	switch cmd {

	case "/help":
		fmt.Println(helpText)

	case "/debug":
		if arg == "" {
			fmt.Println("usage: /debug <msg>")
			return
		}
		DebugChat(cfg, db, arg)

	case "/paste":
		fmt.Println("(paste mode, empty line to submit)")
		msg, err := readMultiline(reader)
		if err != nil {
			fmt.Println("input error:", err)
			return
		}
		msg = strings.TrimSpace(msg)
		if msg == "" {
			return
		}

		fmt.Println("\nAssistant>")
		if DefaultUseLongTermChat {
			_ = Chat(lw, cfg, db, msg)
		} else {
			answer := streamChat(cfg, msg)
			_ = lw.WriteRecord(map[string]string{"role": "user", "content": msg})
			_ = lw.WriteRecord(map[string]string{"role": "assistant", "content": answer})
		}

	case "/search":
		if arg == "" {
			fmt.Println("usage: /search <query>")
			return
		}
		hits, err := SearchWithScore(db, cfg, arg)
		if err != nil {
			fmt.Println("search error:", err)
			return
		}
		if len(hits) == 0 {
			fmt.Println("no related memory")
			return
		}

		for _, h := range hits {
			// ⭐ 展示层分流：fact / 非 fact
			if h.Type == "fact" {
				fmt.Printf("[%.4f] fact\n", h.Score)
			} else {
				fmt.Printf("[%.4f] %s %s\n", h.Score, h.Date, h.Type)
			}

			if strings.TrimSpace(h.Text) != "" {
				fmt.Println(h.Text)
			}
			fmt.Println("----------------------")
		}

	case "/ask":
		if arg == "" {
			fmt.Println("usage: /ask <question>")
			return
		}
		ans, err := Ask(db, cfg, arg)
		if err != nil {
			fmt.Println("ask error:", err)
			return
		}
		fmt.Println(ans)

	case "/chat":
		if arg == "" {
			fmt.Println("usage: /chat <msg>")
			return
		}
		fmt.Println("\nAssistant>")
		if err := Chat(lw, cfg, db, arg); err != nil {
			fmt.Println("[error]", err)
			return
		}

	case "/remember":
		if arg == "" {
			fmt.Println("usage: /remember <fact>")
			return
		}
		out, err := RememberFactWithOutcome(lw, cfg, db, arg)
		if err != nil {
			fmt.Println("[error]", err)
			return
		}
		if out != nil {
			switch out.Status {
			case "conflict":
				fmt.Println("[conflict] 已进入 FACTS -> CONFLICTS，处理后才会晋升为长期事实。")
				return
			case "noop":
				fmt.Println("[noop] nothing to remember")
				return
			}
		}
		fmt.Println("[ok] fact recorded")

	case "/forget":
		if arg == "" {
			fmt.Println("usage: /forget <fact>")
			return
		}
		if err := ForgetFact(lw, cfg, db, arg); err != nil {
			fmt.Println("[error]", err)
			return
		}
		fmt.Println("[ok] fact retracted")

	case "/pending_add":
		if strings.TrimSpace(arg) == "" {
			fmt.Println("usage: /pending_add <fact> [--conf 0.85]")
			return
		}
		conf := pendingFactDefaultConf
		fields := strings.Fields(arg)
		var parts []string
		for i := 0; i < len(fields); i++ {
			if fields[i] == "--conf" && i+1 < len(fields) {
				if v, err := strconv.ParseFloat(fields[i+1], 64); err == nil {
					conf = v
				}
				i++
				continue
			}
			if strings.HasPrefix(fields[i], "--") {
				continue
			}
			parts = append(parts, fields[i])
		}
		fact := strings.TrimSpace(strings.Join(parts, " "))
		if fact == "" {
			fmt.Println("usage: /pending_add <fact> [--conf 0.85]")
			return
		}
		if err := AddPendingFactManual(cfg, db, fact, conf); err != nil {
			fmt.Println("[error]", err)
			return
		}
		fmt.Println("[ok] pending fact added (open FACTS -> PENDING)")

	case "/daily":
		force := strings.Contains(arg, "--force")

		// 默认今天
		day := time.Now().In(cfg.Location).Format("2006-01-02")

		// 只支持裸日期参数：/daily 2026-01-08（位置不限，跳过 --xxx）
		fields := strings.Fields(arg)
		for _, f := range fields {
			if strings.HasPrefix(f, "--") {
				continue
			}
			// 严格校验 YYYY-MM-DD（非法日期不会生效）
			if t, err := time.ParseInLocation("2006-01-02", f, cfg.Location); err == nil && t.Format("2006-01-02") == f {
				day = f
				break
			}
		}

		if err := ensureDaily(cfg, db, day, force); err != nil {
			fmt.Println("[error] daily summary failed:", err)
			return
		}

		fmt.Println("[ok] daily summary ensured:", day)

	case "/weekly":
		force := strings.Contains(arg, "--force")
		y, w := time.Now().In(cfg.Location).ISOWeek()
		key := fmt.Sprintf("%04d-W%02d", y, w)
		if err := ensureWeekly(cfg, db, key, force); err != nil {
			fmt.Println("[error] weekly summary failed:", err)
			return
		}
		fmt.Println("[ok] weekly summary ensured:", key)

	case "/monthly":
		force := strings.Contains(arg, "--force")
		key := time.Now().In(cfg.Location).Format("2006-01")
		if err := ensureMonthly(cfg, db, key, force); err != nil {
			fmt.Println("[error] monthly summary failed:", err)
			return
		}
		fmt.Println("[ok] monthly summary ensured:", key)

	case "/reindex":
		target := arg
		if target == "" {
			target = "daily"
		}
		if err := Reindex(db, cfg, target); err != nil {
			fmt.Println("reindex error:", err)
		}

	default:
		fmt.Println("unknown command, try /help")
	}
}
