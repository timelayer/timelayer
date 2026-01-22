package app

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ============================================================
// User fact versioning + conflicts
// - user_facts: current authoritative state (UNIQUE fact_key)
// - user_facts_history: append-only versions (active/archived/forgotten/conflict/rejected)
// - user_fact_conflicts: unresolved conflict pool (needs user resolution)
// ============================================================

type UserFactConflict struct {
	ID                 int64  `json:"id"`
	FactKey            string `json:"fact_key"`
	ExistingFact       string `json:"existing_fact"`
	ProposedFact       string `json:"proposed_fact"`
	ProposedSourceType string `json:"proposed_source_type"`
	ProposedSourceKey  string `json:"proposed_source_key"`
	Status             string `json:"status"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
}

type UserFactRow struct {
	FactKey   string `json:"fact_key"`
	Fact      string `json:"fact"`
	IsActive  bool   `json:"is_active"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type UserFactHistoryRow struct {
	ID         int64  `json:"id"`
	FactKey    string `json:"fact_key"`
	Fact       string `json:"fact"`
	Status     string `json:"status"`
	Version    int    `json:"version"`
	SourceType string `json:"source_type"`
	SourceKey  string `json:"source_key"`
	CreatedAt  string `json:"created_at"`
}

func getActiveUserFactByKey(db dbTX, factKey string) (fact string, ok bool) {
	if db == nil || factKey == "" {
		return "", false
	}
	row := db.QueryRow(`SELECT fact FROM user_facts WHERE fact_key=? AND is_active=1 LIMIT 1`, factKey)
	if err := row.Scan(&fact); err != nil {
		return "", false
	}
	return fact, true
}

// getActiveUserFactBySlotKey finds an active fact that occupies the same (subject, relation) slot.
// This enables conflict detection even when different fact_key values were derived.
//
// NOTE: slotKey is produced by FactTriple.SlotKey(). It is non-empty only for conservative,
// single-valued relations (e.g. name/email/phone/identity/location/job).
func getActiveUserFactBySlotKey(db dbTX, slotKey string) (factKey, fact string, ok bool) {
	if db == nil || slotKey == "" {
		return "", "", false
	}
	rows, err := db.Query(`SELECT fact_key, fact FROM user_facts WHERE is_active=1`)
	if err != nil {
		return "", "", false
	}
	defer rows.Close()
	for rows.Next() {
		var k, f string
		if err := rows.Scan(&k, &f); err != nil {
			continue
		}
		tr := ExtractFactTriple(f)
		if tr.SlotKey() == slotKey {
			return k, f, true
		}
	}
	return "", "", false
}

func nextUserFactVersion(db dbTX, factKey string) int {
	if db == nil || factKey == "" {
		return 1
	}
	row := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM user_facts_history WHERE fact_key=?`, factKey)
	var max int
	if err := row.Scan(&max); err != nil {
		return 1
	}
	return max + 1
}

func appendUserFactHistory(db dbTX, factKey, fact, status, sourceType, sourceKey string, when time.Time, version int) error {
	if db == nil || factKey == "" || fact == "" {
		return nil
	}
	if status == "" {
		status = "active"
	}
	if sourceType == "" {
		sourceType = "unknown"
	}
	if sourceKey == "" {
		sourceKey = "-"
	}
	if version <= 0 {
		version = nextUserFactVersion(db, factKey)
	}
	ts := when.Format(time.RFC3339)
	_, err := db.Exec(`
		INSERT INTO user_facts_history(
		  fact_key, fact, status, version,
		  source_type, source_key, created_at
		) VALUES(?,?,?,?,?,?,?)
	`, factKey, fact, status, version, sourceType, sourceKey, ts)
	return err
}

func createUserFactConflict(db dbTX, factKey, existingFact, proposedFact, sourceType, sourceKey string, when time.Time) (int64, error) {
	if db == nil || factKey == "" || existingFact == "" || proposedFact == "" {
		return 0, nil
	}

	// de-dup: same proposed fact already exists as unresolved conflict
	row := db.QueryRow(`
        SELECT id FROM user_fact_conflicts
        WHERE status='conflict' AND fact_key=? AND proposed_fact=?
        ORDER BY id DESC LIMIT 1
    `, factKey, proposedFact)
	var existingID int64
	if err := row.Scan(&existingID); err == nil && existingID > 0 {
		return existingID, nil
	}

	ts := when.Format(time.RFC3339)
	res, err := db.Exec(`
        INSERT INTO user_fact_conflicts(
          fact_key, existing_fact, proposed_fact,
          proposed_source_type, proposed_source_key,
          status, created_at, updated_at
        ) VALUES(?,?,?,?,?,'conflict',?,?)
    `, factKey, existingFact, proposedFact, sourceType, sourceKey, ts, ts)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func CountFactConflicts(db *sql.DB) int {
	if db == nil {
		return 0
	}
	row := db.QueryRow(`SELECT COUNT(1) FROM user_fact_conflicts WHERE status='conflict'`)
	var n int
	_ = row.Scan(&n)
	return n
}

func ListFactConflicts(db *sql.DB, limit int) ([]UserFactConflict, error) {
	if db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.Query(`
        SELECT id, fact_key, existing_fact, proposed_fact,
               proposed_source_type, proposed_source_key,
               status, created_at, updated_at
        FROM user_fact_conflicts
        WHERE status='conflict'
        ORDER BY created_at DESC
        LIMIT ?
    `, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserFactConflict
	for rows.Next() {
		var c UserFactConflict
		if err := rows.Scan(&c.ID, &c.FactKey, &c.ExistingFact, &c.ProposedFact, &c.ProposedSourceType, &c.ProposedSourceKey, &c.Status, &c.CreatedAt, &c.UpdatedAt); err != nil {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func ListActiveFacts(db *sql.DB, limit int) ([]UserFactRow, error) {
	if db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.Query(`SELECT fact_key, fact, is_active, created_at, updated_at
FROM user_facts
WHERE is_active = 1
ORDER BY updated_at DESC
LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserFactRow
	for rows.Next() {
		var r UserFactRow
		var active int
		if err := rows.Scan(&r.FactKey, &r.Fact, &active, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.IsActive = active != 0
		out = append(out, r)
	}
	return out, nil
}

func ListUserFactHistory(db *sql.DB, limit int) ([]UserFactHistoryRow, error) {
	if db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 200
	}
	// NOTE: older versions mistakenly wrote "pending" into user_facts_history.
	// We hide those legacy rows here; pending facts belong to pending_facts (FACTS → PENDING).
	rows, err := db.Query(`SELECT id, fact_key, fact, status, version, source_type, source_key, created_at
FROM user_facts_history
WHERE status != 'pending'
ORDER BY created_at DESC
LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserFactHistoryRow
	for rows.Next() {
		var r UserFactHistoryRow
		if err := rows.Scan(&r.ID, &r.FactKey, &r.Fact, &r.Status, &r.Version, &r.SourceType, &r.SourceKey, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func getFactConflictByID(db dbTX, id int64) (*UserFactConflict, error) {
	if db == nil || id <= 0 {
		return nil, nil
	}
	row := db.QueryRow(`
        SELECT id, fact_key, existing_fact, proposed_fact,
               proposed_source_type, proposed_source_key,
               status, created_at, updated_at
        FROM user_fact_conflicts
        WHERE id=? LIMIT 1
    `, id)
	var c UserFactConflict
	if err := row.Scan(&c.ID, &c.FactKey, &c.ExistingFact, &c.ProposedFact, &c.ProposedSourceType, &c.ProposedSourceKey, &c.Status, &c.CreatedAt, &c.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &c, nil
}

// ResolveFactConflictKeep keeps the existing fact; the proposed fact is recorded as rejected.
// This function is transactional.
func ResolveFactConflictKeep(db *sql.DB, id int64, now time.Time) error {
	if db == nil || id <= 0 {
		return errors.New("conflict not found")
	}

	return withDBRetry(3, 25*time.Millisecond, func() error {
		return withTx(db, func(tx *sql.Tx) error {
			c, err := getFactConflictByID(tx, id)
			if err != nil {
				return err
			}
			if c == nil || c.Status != "conflict" {
				return errors.New("conflict not found")
			}

			// history: proposed fact rejected
			if err := appendUserFactHistory(tx, c.FactKey, c.ProposedFact, "rejected", "conflict_keep", "conflict:"+itoa64(c.ID), now, 0); err != nil {
				return err
			}

			ts := now.Format(time.RFC3339)
			_, err = tx.Exec(`UPDATE user_fact_conflicts SET status='resolved_keep', updated_at=? WHERE id=?`, ts, id)
			return err
		})
	})
}

// ResolveFactConflictReplace archives existing and replaces with proposed.
// This function is transactional (the semantic-search sync runs post-commit, best-effort).
func ResolveFactConflictReplace(cfg Config, db *sql.DB, id int64, replacement string, now time.Time) error {
	if db == nil || id <= 0 {
		return errors.New("conflict not found")
	}

	var factKey string
	var replacementTrim string

	err := withDBRetry(3, 25*time.Millisecond, func() error {
		return withTx(db, func(tx *sql.Tx) error {
			c, err := getFactConflictByID(tx, id)
			if err != nil {
				return err
			}
			if c == nil || c.Status != "conflict" {
				return errors.New("conflict not found")
			}
			factKey = c.FactKey

			repl := replacement
			if strings.TrimSpace(repl) == "" {
				repl = c.ProposedFact
			}
			repl = strings.TrimSpace(repl)
			if repl == "" {
				return errors.New("replacement fact empty")
			}
			replacementTrim = repl

			// capture current active (if exists) for history
			current, _ := getActiveUserFactByKey(tx, c.FactKey)

			// write new as active
			if err := upsertUserFact(tx, repl, c.FactKey, true, now); err != nil {
				return err
			}

			// history
			if strings.TrimSpace(current) != "" {
				if err := appendUserFactHistory(tx, c.FactKey, current, "archived", "conflict_replace", "conflict:"+itoa64(c.ID), now, 0); err != nil {
					return err
				}
			}
			if err := appendUserFactHistory(tx, c.FactKey, repl, "active", "conflict_replace", "conflict:"+itoa64(c.ID), now, 0); err != nil {
				return err
			}

			ts := now.Format(time.RFC3339)
			_, err = tx.Exec(`UPDATE user_fact_conflicts SET status='resolved_replace', updated_at=? WHERE id=?`, ts, id)
			return err
		})
	})
	if err != nil {
		return err
	}

	// best-effort: keep summaries+embedding aligned with current fact
	if factKey != "" && replacementTrim != "" {
		_ = syncFactToSearch(cfg, db, factKey, replacementTrim, "conflict_replace")
	}
	return nil
}

// itoa64 small helper (avoid strconv import in hot path files)
func itoa64(v int64) string {
	// minimal, safe implementation
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [32]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + (v % 10))
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
