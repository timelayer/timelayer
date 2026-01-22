package app

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type ContextBlockView struct {
	Role     string `json:"role"`
	Source   string `json:"source"`
	Priority int    `json:"priority"`
	Len      int    `json:"len"`
	Preview  string `json:"preview"`
}

type ChatContextAudit struct {
	Date         string             `json:"date"`
	Question     string             `json:"question"`
	Policy       map[string]any     `json:"policy"`
	Steps        []string           `json:"steps"`
	Blocks       []PromptBlock      `json:"blocks"`
	BlocksView   []ContextBlockView `json:"blocks_view"`
	SearchHits   []SearchHit        `json:"search_hits"`
	RememberedN  int                `json:"remembered_n"`
	PendingN     int                `json:"pending_n"`
	ConflictsN   int                `json:"conflicts_n"`
	RecentRawN   int                `json:"recent_raw_n"`
	DailySummary bool               `json:"daily_summary"`
}

func BuildChatContextAudit(cfg Config, db *sql.DB, date string, userQuestion string) ChatContextAudit {
	userQuestion = strings.TrimSpace(userQuestion)
	maxLines := cfg.RecentMaxLines
	if maxLines <= 0 {
		maxLines = 20
	}
	a := ChatContextAudit{
		Date:     date,
		Question: userQuestion,
		Policy: map[string]any{
			"search_top_k":   cfg.SearchTopK,
			"max_recent_raw": maxLines,
			"force_role":     "assistant",
			// final injection order after resolvePromptBlocks
			"order": []string{"remembered_fact", "daily_summary", "search_hit", "recent_raw"},
		},
		PendingN:   CountPendingFacts(db),
		ConflictsN: CountFactConflicts(db),
	}

	// 1) daily summary presence (content itself is shown in Blocks)
	if daily := loadDailySummary(cfg, date); daily != "" {
		a.DailySummary = true
		a.Steps = append(a.Steps, fmt.Sprintf("daily_summary: added=1 note=loaded %d chars", len([]rune(daily))))
	} else {
		a.Steps = append(a.Steps, "daily_summary: added=0 note=not found")
	}

	// 2) remembered facts (active)
	facts, _ := loadActiveUserFacts(db, 200)
	a.RememberedN = len(facts)
	if len(facts) > 0 {
		a.Steps = append(a.Steps, fmt.Sprintf("remembered_fact: added=1 note=%d active", len(facts)))
	} else {
		a.Steps = append(a.Steps, "remembered_fact: added=0 note=none")
	}

	// 3) recent raw (count lines)
	recent := strings.TrimSpace(loadRecentRaw(cfg, date, maxLines))
	if recent != "" {
		a.RecentRawN = len(strings.Split(recent, "\n"))
		a.Steps = append(a.Steps, fmt.Sprintf("recent_raw: added=1 note=%d lines", a.RecentRawN))
	} else {
		a.Steps = append(a.Steps, "recent_raw: added=0 note=empty")
	}

	// 4) search hits
	var hits []SearchHit
	if cfg.SearchTopK > 0 && userQuestion != "" {
		sh, err := SearchWithScore(db, cfg, userQuestion)
		if err == nil {
			hits = sh
		}
	}
	if len(hits) > 0 {
		a.SearchHits = hits
		a.Steps = append(a.Steps, fmt.Sprintf("search_hits: added=1 note=%d hits", len(hits)))
	} else {
		a.Steps = append(a.Steps, "search_hits: added=0 note=none")
	}

	// final prompt blocks (source of truth)
	a.Blocks = BuildChatContext(cfg, db, date, userQuestion)
	a.BlocksView = make([]ContextBlockView, 0, len(a.Blocks))
	prioOf := func(src string) int {
		switch src {
		case "remembered_fact":
			return 1000
		case "daily_summary":
			return 600
		case "search_hit":
			return 400
		case "recent_raw":
			return 200
		default:
			return 0
		}
	}

	for _, b := range a.Blocks {
		prev := strings.ReplaceAll(b.Content, "\n", " ")
		prev = strings.TrimSpace(prev)
		if len([]rune(prev)) > 160 {
			prev = string([]rune(prev)[:160]) + "…"
		}
		a.BlocksView = append(a.BlocksView, ContextBlockView{
			Role:     b.Role,
			Source:   b.Source,
			Priority: prioOf(b.Source),
			Len:      len([]rune(b.Content)),
			Preview:  prev,
		})
	}

	// include a timestamp so frontend can detect staleness
	a.Policy["generated_at"] = time.Now().In(cfg.Location).Format(time.RFC3339)
	return a
}
