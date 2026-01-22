package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	pendingFactMinConfidence = 0.75
	pendingFactDefaultConf   = 0.85
)

// PendingFact is a candidate fact waiting for user confirmation.
type PendingFact struct {
	ID         int64   `json:"id"`
	Fact       string  `json:"fact"`
	FactKey    string  `json:"fact_key"`
	Confidence float64 `json:"confidence"`
	SourceType string  `json:"source_type"`
	SourceKey  string  `json:"source_key"`
	Status     string  `json:"status"`
	CreatedAt  string  `json:"created_at"`
	UpdatedAt  string  `json:"updated_at"`
}

type pendingFactCandidate struct {
	Fact       string
	Confidence float64
}

// addPendingFact inserts/updates a pending candidate fact with custom source.
// It won't add duplicates or override active facts.
func addPendingFact(cfg Config, db dbTX, fact string, confidence float64, sourceType, sourceKey string) error {
	if db == nil {
		return nil
	}
	fact = strings.TrimSpace(fact)
	// Normalize common wrappers that may appear in daily summaries, e.g. "记住：xxx".
	fact = normalizePendingFactText(fact)
	if fact == "" {
		return nil
	}
	if confidence <= 0 {
		confidence = pendingFactDefaultConf
	}
	if confidence < pendingFactMinConfidence {
		return nil
	}
	if sourceType == "" {
		sourceType = "manual"
	}

	factKey := deriveFactKeyFromSubject(fact)
	if factKey == "" {
		return nil
	}

	// Skip if already an active remembered fact
	if hasActiveUserFact(db, factKey) {
		return nil
	}

	loc := cfg.Location
	if loc == nil {
		loc = time.Local
	}
	now := time.Now().In(loc)
	nowStr := now.Format(time.RFC3339)
	if sourceKey == "" {
		sourceKey = now.Format("2006-01-02")
	}

	// IMPORTANT:
	// Older DBs may not have a UNIQUE constraint matching the ON CONFLICT clause.
	// To avoid breaking upgrades, we do a read-then-update/insert upsert here.
	// This keeps pending ingestion working even if the schema evolved.
	var existingID int64
	var existingConf float64
	err := db.QueryRow(`
		SELECT id, confidence
		FROM pending_facts
		WHERE fact_key=? AND status='pending' AND source_type=? AND source_key=?
		ORDER BY updated_at DESC
		LIMIT 1
	`, factKey, sourceType, sourceKey).Scan(&existingID, &existingConf)

	if err == nil && existingID > 0 {
		newConf := confidence
		if existingConf > newConf {
			newConf = existingConf
		}
		_, uerr := db.Exec(`
			UPDATE pending_facts
			SET fact=?, confidence=?, updated_at=?
			WHERE id=?
		`, fact, newConf, nowStr, existingID)
		return uerr
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	_, ierr := db.Exec(`
		INSERT INTO pending_facts(
		  fact, fact_key, confidence,
		  source_type, source_key,
		  status, created_at, updated_at
		)
		VALUES(?,?,?,?,?, 'pending', ?, ?)
	`, fact, factKey, confidence, sourceType, sourceKey, nowStr, nowStr)
	return ierr
}

// normalizePendingFactText removes common instruction wrappers and trailing punctuation
// to avoid polluting fact_key derivation (e.g. "记住：我最喜欢的颜色是黄色。" -> "我最喜欢的颜色是黄色").
func normalizePendingFactText(s string) string {
	s = strings.TrimSpace(s)
	for _, p := range []string{"记住：", "记住:", "请记住：", "请记住:", "帮我记住：", "帮我记住:"} {
		if strings.HasPrefix(s, p) {
			s = strings.TrimSpace(strings.TrimPrefix(s, p))
			break
		}
	}
	s = strings.TrimSpace(strings.TrimRight(s, "。.!！"))
	return s
}

// AddPendingFactManual inserts a pending candidate fact directly (useful for testing the UI
// or for future manual workflows). It won't add duplicates or override active facts.
func AddPendingFactManual(cfg Config, db *sql.DB, fact string, confidence float64) error {
	return addPendingFact(cfg, db, fact, confidence, "manual", "")
}

// EnsurePendingFactsFromDailyJSON ingests high-confidence facts from daily summary JSON.
// SourceType is fixed to "daily" and SourceKey is the date (YYYY-MM-DD).
func EnsurePendingFactsFromDailyJSON(cfg Config, db *sql.DB, date string, dailyJSON string) error {
	if db == nil {
		return nil
	}
	dailyJSON = strings.TrimSpace(dailyJSON)
	if dailyJSON == "" {
		return nil
	}

	var obj map[string]any
	if err := json.Unmarshal([]byte(dailyJSON), &obj); err != nil {
		return nil // do not fail daily pipeline due to pending ingestion
	}

	explicitRaw, _ := obj["user_facts_explicit"]
	implicitRaw, _ := obj["user_facts_implicit"]
	if explicitRaw == nil && implicitRaw == nil {
		return nil
	}

	explicit := parsePendingCandidates(explicitRaw)
	implicit := parsePendingCandidates(implicitRaw)
	if len(explicit) == 0 && len(implicit) == 0 {
		return nil
	}

	// Avoid duplicate facts: prefer explicit over implicit when they overlap.
	seen := make(map[string]struct{}, len(explicit))
	for _, c := range explicit {
		f := strings.TrimSpace(c.Fact)
		if f != "" {
			seen[f] = struct{}{}
		}
	}

	ingest := func(sourceType string, cands []pendingFactCandidate, defaultConf float64) error {
		for _, c := range cands {
			fact := strings.TrimSpace(c.Fact)
			if fact == "" {
				continue
			}
			// Skip implicit duplicates if explicit already contains the same text.
			if sourceType == "daily_implicit" {
				if _, ok := seen[fact]; ok {
					continue
				}
			}

			conf := c.Confidence
			if conf <= 0 {
				conf = defaultConf
			}
			if conf < pendingFactMinConfidence {
				continue
			}

			factKey := deriveFactKeyFromSubject(fact)
			if factKey == "" {
				continue
			}

			// Skip if already an active remembered fact
			if hasActiveUserFact(db, factKey) {
				continue
			}

			// Use a single helper to avoid silent SQL incompatibilities.
			if err := addPendingFact(cfg, db, fact, conf, sourceType, date); err != nil {
				return err
			}
		}
		return nil
	}

	// 1) explicit facts from the daily summary LLM
	if err := ingest("daily", explicit, pendingFactDefaultConf); err != nil {
		return err
	}
	// 2) implicit candidates extracted from raw dialog (fallback)
	if err := ingest("daily_implicit", implicit, 0.80); err != nil {
		return err
	}

	return nil
}

func parsePendingCandidates(raw any) []pendingFactCandidate {
	var out []pendingFactCandidate

	switch v := raw.(type) {
	case []any:
		for _, it := range v {
			switch x := it.(type) {
			case string:
				out = append(out, pendingFactCandidate{Fact: x, Confidence: pendingFactDefaultConf})
			case map[string]any:
				fact, _ := x["fact"].(string)
				if fact == "" {
					fact, _ = x["content"].(string)
				}
				conf := pendingFactDefaultConf
				if cc, ok := x["confidence"]; ok {
					switch n := cc.(type) {
					case float64:
						conf = n
					case int:
						conf = float64(n)
					case json.Number:
						if f, err := n.Float64(); err == nil {
							conf = f
						}
					}
				}
				if strings.TrimSpace(fact) != "" {
					out = append(out, pendingFactCandidate{Fact: fact, Confidence: conf})
				}
			}
		}
	case string:
		if strings.TrimSpace(v) != "" {
			out = append(out, pendingFactCandidate{Fact: v, Confidence: pendingFactDefaultConf})
		}
	}

	return out
}

func hasActiveUserFact(db dbTX, factKey string) bool {
	row := db.QueryRow(`SELECT 1 FROM user_facts WHERE is_active=1 AND fact_key=? LIMIT 1`, factKey)
	var one int
	return row.Scan(&one) == nil
}

func CountPendingFacts(db *sql.DB) int {
	if db == nil {
		return 0
	}
	row := db.QueryRow(`SELECT COUNT(1) FROM pending_facts WHERE status='pending'`)
	var n int
	_ = row.Scan(&n)
	return n
}

func ListPendingFacts(db *sql.DB, limit int) ([]PendingFact, error) {
	if db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}

	rows, err := db.Query(`
		SELECT id, fact, fact_key, confidence, source_type, source_key, status, created_at, updated_at
		FROM pending_facts
		WHERE status='pending'
		ORDER BY created_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PendingFact
	for rows.Next() {
		var pf PendingFact
		if err := rows.Scan(&pf.ID, &pf.Fact, &pf.FactKey, &pf.Confidence, &pf.SourceType, &pf.SourceKey, &pf.Status, &pf.CreatedAt, &pf.UpdatedAt); err != nil {
			continue
		}
		out = append(out, pf)
	}
	return out, nil
}

func getPendingFactByID(db dbTX, id int64) (*PendingFact, error) {
	row := db.QueryRow(`
		SELECT id, fact, fact_key, confidence, source_type, source_key, status, created_at, updated_at
		FROM pending_facts
		WHERE id=?
		LIMIT 1
	`, id)
	var pf PendingFact
	if err := row.Scan(&pf.ID, &pf.Fact, &pf.FactKey, &pf.Confidence, &pf.SourceType, &pf.SourceKey, &pf.Status, &pf.CreatedAt, &pf.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &pf, nil
}

func RememberPendingFact(cfg Config, db *sql.DB, id int64) (*RememberOutcome, error) {
	if db == nil {
		return nil, nil
	}
	loc := cfg.Location
	if loc == nil {
		loc = time.Local
	}
	nowTime := time.Now().In(loc)

	var out *RememberOutcome
	var acceptedContent string
	var acceptedSource string

	err := withDBRetry(3, 25*time.Millisecond, func() error {
		return withTx(db, func(tx *sql.Tx) error {
			pf, err := getPendingFactByID(tx, id)
			if err != nil {
				return err
			}
			if pf == nil || pf.Status != "pending" {
				return fmt.Errorf("pending fact not found")
			}

			acceptedContent = strings.TrimSpace(pf.Fact)
			acceptedSource = pf.SourceType

			o, err := proposeRememberFactWith(cfg, tx, pf.Fact, "pending", pf.SourceKey, nowTime)
			if err != nil {
				return err
			}
			out = o

			newStatus := "accepted"
			if o != nil && o.Status == "conflict" {
				newStatus = "conflict"
			}
			now := nowTime.Format(time.RFC3339)
			_, err = tx.Exec(`UPDATE pending_facts SET status=?, updated_at=? WHERE id=?`, newStatus, now, id)
			return err
		})
	})
	if err != nil {
		return nil, err
	}

	// best-effort: keep semantic search aligned (post-commit)
	if out != nil && out.Status == "remembered" {
		_ = syncFactToSearch(cfg, db, out.FactKey, acceptedContent, acceptedSource)
	}
	return out, nil
}

func RejectPendingFact(cfg Config, db *sql.DB, id int64) error {
	if db == nil {
		return nil
	}
	loc := cfg.Location
	if loc == nil {
		loc = time.Local
	}
	nowTime := time.Now().In(loc)

	return withDBRetry(3, 25*time.Millisecond, func() error {
		return withTx(db, func(tx *sql.Tx) error {
			pf, err := getPendingFactByID(tx, id)
			if err != nil {
				return err
			}
			if pf == nil || pf.Status != "pending" {
				return fmt.Errorf("pending fact not found")
			}
			now := nowTime.Format(time.RFC3339)
			if _, err := tx.Exec(`UPDATE pending_facts SET status='rejected', updated_at=? WHERE id=?`, now, id); err != nil {
				return err
			}

			// Best-effort audit trail
			factKey := deriveFactKeyFromSubject(pf.Fact)
			_ = appendUserFactHistory(tx, factKey, strings.TrimSpace(pf.Fact), "rejected", "pending_reject", fmt.Sprintf("pending:%d", pf.ID), nowTime, 0)
			return nil
		})
	})
}

// RememberPendingFactsBatch processes multiple pending ids.
// Returns outcomes keyed by id.
func RememberPendingFactsBatch(cfg Config, db *sql.DB, ids []int64) (map[int64]*RememberOutcome, error) {
	out := make(map[int64]*RememberOutcome)
	if db == nil || len(ids) == 0 {
		return out, nil
	}
	// best-effort: process in order
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		o, err := RememberPendingFact(cfg, db, id)
		if err != nil {
			// keep going, but record nil for this id
			out[id] = nil
			continue
		}
		out[id] = o
	}
	return out, nil
}

func RejectPendingFactsBatch(cfg Config, db *sql.DB, ids []int64) error {
	if db == nil || len(ids) == 0 {
		return nil
	}

	var failed int
	var firstErr error
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if err := RejectPendingFact(cfg, db, id); err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if firstErr != nil {
		return fmt.Errorf("reject batch: %d/%d failed (first: %w)", failed, len(ids), firstErr)
	}
	return nil
}
