package app

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// HandleCommandWeb：复用 CLI 的命令体系，返回 (handled, output, err)
func HandleCommandWeb(cfg Config, db *sql.DB, lw *LogWriter, input string) (bool, string, error) {
	cmd, arg := normalizeCommand(input)
	if cmd == "" {
		return false, "", nil
	}

	switch cmd {

	case "/help":
		// helpText 在 embedding_search_reflect.go 里
		return true, helpText, nil

	case "/debug":
		if arg == "" {
			return true, "usage: /debug <msg>", nil
		}
		return true, DebugChatText(cfg, db, arg), nil

	case "/search":
		if arg == "" {
			return true, "usage: /search <query>", nil
		}
		hits, err := SearchWithScore(db, cfg, arg)
		if err != nil {
			return true, "", err
		}
		if len(hits) == 0 {
			return true, "no related memory", nil
		}
		var b strings.Builder
		for _, h := range hits {
			if h.Type == "fact" {
				b.WriteString(fmt.Sprintf("[%.4f] fact\n", h.Score))
				b.WriteString(h.Text)
			} else {
				b.WriteString(fmt.Sprintf("[%.4f] %s %s\n", h.Score, h.Date, h.Type))
				b.WriteString(h.Text)
			}
			b.WriteString("\n----------------------\n")
		}

		return true, b.String(), nil

	case "/ask":
		if arg == "" {
			return true, "usage: /ask <question>", nil
		}
		ans, err := Ask(db, cfg, arg)
		if err != nil {
			return true, "", err
		}
		return true, ans, nil

	case "/remember":
		if arg == "" {
			return true, "usage: /remember <fact>", nil
		}
		out, err := RememberFactWithOutcome(lw, cfg, db, arg)
		if err != nil {
			return true, "", err
		}
		if out != nil {
			switch out.Status {
			case "conflict":
				return true, "[conflict] 已进入 FACTS -> CONFLICTS，处理后才会晋升为长期事实。", nil
			case "remembered":
				return true, "[ok] fact recorded", nil
			case "noop":
				return true, "[noop] nothing to remember", nil
			}
		}
		return true, "[ok] fact recorded", nil

	case "/forget":
		if arg == "" {
			return true, "usage: /forget <fact>", nil
		}
		if err := ForgetFact(lw, cfg, db, arg); err != nil {
			return true, "", err
		}
		return true, "[ok] fact retracted", nil

	case "/pending_add":
		if strings.TrimSpace(arg) == "" {
			return true, "usage: /pending_add <fact> [--conf 0.85]", nil
		}
		conf := pendingFactDefaultConf
		fields := strings.Fields(arg)
		var factParts []string
		for i := 0; i < len(fields); i++ {
			if fields[i] == "--conf" && i+1 < len(fields) {
				// best-effort parse
				if v, err := strconv.ParseFloat(fields[i+1], 64); err == nil {
					conf = v
				}
				i++
				continue
			}
			if strings.HasPrefix(fields[i], "--") {
				continue
			}
			factParts = append(factParts, fields[i])
		}
		fact := strings.TrimSpace(strings.Join(factParts, " "))
		if fact == "" {
			return true, "usage: /pending_add <fact> [--conf 0.85]", nil
		}
		if err := AddPendingFactManual(cfg, db, fact, conf); err != nil {
			return true, "", err
		}
		return true, "[ok] pending fact added. Open FACTS -> PENDING.", nil

	case "/daily":
		force := strings.Contains(arg, "--force")

		// default: today
		day := time.Now().In(cfg.Location).Format("2006-01-02")

		// allow a bare date arg anywhere: /daily 2026-01-08 (skip --xxx)
		fields := strings.Fields(arg)
		for _, f := range fields {
			if strings.HasPrefix(f, "--") {
				continue
			}
			if t, err := time.ParseInLocation("2006-01-02", f, cfg.Location); err == nil && t.Format("2006-01-02") == f {
				day = f
				break
			}
		}

		if err := ensureDaily(cfg, db, day, force); err != nil {
			return true, "", err
		}
		return true, "[ok] daily summary ensured: " + day, nil

	case "/weekly":
		force := strings.Contains(arg, "--force")
		y, w := time.Now().In(cfg.Location).ISOWeek()
		key := fmt.Sprintf("%04d-W%02d", y, w)
		if err := ensureWeekly(cfg, db, key, force); err != nil {
			return true, "", err
		}
		return true, "[ok] weekly summary ensured: " + key, nil

	case "/monthly":
		force := strings.Contains(arg, "--force")
		key := time.Now().In(cfg.Location).Format("2006-01")
		if err := ensureMonthly(cfg, db, key, force); err != nil {
			return true, "", err
		}
		return true, "[ok] monthly summary ensured: " + key, nil

	case "/reindex":
		target := strings.TrimSpace(arg)
		if target == "" {
			target = "daily"
		}
		if err := Reindex(db, cfg, target); err != nil {
			return true, "", err
		}
		return true, "[ok] reindex done: " + target, nil

	default:
		return true, fmt.Sprintf("unknown command: %s", cmd), nil
	}
}
