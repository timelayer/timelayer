package app

import (
	"database/sql"
	"strings"
	"time"
)

type RememberOutcome struct {
	Status     string `json:"status"` // remembered | pending | conflict | noop
	FactKey    string `json:"fact_key"`
	ConflictID int64  `json:"conflict_id,omitempty"`
	Existing   string `json:"existing,omitempty"`
}

// ProposePendingRememberFact behaves like ProposeRememberFact, but instead of immediately writing
// the new fact into the active truth store, it inserts it into pending_facts for user confirmation.
// Conflicts are still detected and recorded.
func ProposePendingRememberFact(cfg Config, db *sql.DB, content, sourceType, sourceKey string, when time.Time) (*RememberOutcome, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return &RememberOutcome{Status: "noop"}, nil
	}

	var out *RememberOutcome
	err := withDBRetry(3, 25*time.Millisecond, func() error {
		return withTx(db, func(tx *sql.Tx) error {
			o, err := proposePendingRememberFactWith(cfg, tx, content, sourceType, sourceKey, when)
			out = o
			return err
		})
	})
	return out, err
}

func proposePendingRememberFactWith(cfg Config, db dbTX, content, sourceType, sourceKey string, when time.Time) (*RememberOutcome, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return &RememberOutcome{Status: "noop"}, nil
	}
	factKey := deriveFactKeyFromSubject(content)
	if factKey == "" {
		return &RememberOutcome{Status: "noop"}, nil
	}
	if sourceType == "" {
		sourceType = "remember_auto"
	}
	if sourceKey == "" {
		sourceKey = when.Format("2006-01-02")
	}

	// 1) exact key conflicts
	if existing, ok := getActiveUserFactByKey(db, factKey); ok {
		if strings.TrimSpace(existing) == strings.TrimSpace(content) {
			if err := upsertUserFact(db, existing, factKey, true, when); err != nil {
				return nil, err
			}
			return &RememberOutcome{Status: "noop", FactKey: factKey}, nil
		}
		cid, err := createUserFactConflict(db, factKey, existing, content, sourceType, sourceKey, when)
		if err != nil {
			return nil, err
		}
		if cid > 0 {
			if err := appendUserFactHistory(db, factKey, content, "conflict", sourceType, sourceKey, when, 0); err != nil {
				return nil, err
			}
		}
		return &RememberOutcome{Status: "conflict", FactKey: factKey, ConflictID: cid, Existing: existing}, nil
	}

	// 2) subject+predicate slot conflicts
	tr := ExtractFactTriple(content)
	slotKey := tr.SlotKey()
	if slotKey != "" {
		if existingKey, existingFact, ok := getActiveUserFactBySlotKey(db, slotKey); ok {
			if strings.TrimSpace(existingFact) == strings.TrimSpace(content) {
				if err := upsertUserFact(db, existingFact, existingKey, true, when); err != nil {
					return nil, err
				}
				return &RememberOutcome{Status: "noop", FactKey: existingKey}, nil
			}
			cid, err := createUserFactConflict(db, existingKey, existingFact, content, sourceType, sourceKey, when)
			if err != nil {
				return nil, err
			}
			if cid > 0 {
				if err := appendUserFactHistory(db, existingKey, content, "conflict", sourceType, sourceKey, when, 0); err != nil {
					return nil, err
				}
			}
			return &RememberOutcome{Status: "conflict", FactKey: existingKey, ConflictID: cid, Existing: existingFact}, nil
		}
	}

	// new candidate -> pending
	if err := addPendingFact(cfg, db, content, 0.95, sourceType, sourceKey); err != nil {
		return nil, err
	}
	return &RememberOutcome{Status: "pending", FactKey: factKey}, nil
}

// syncFactToSearch writes the current remembered fact into summaries + embeddings for semantic search.
// NOTE: only call this after the fact is accepted as the current truth.
func syncFactToSearch(cfg Config, db *sql.DB, factKey, content, source string) error {
	content = strings.TrimSpace(content)
	if db == nil || factKey == "" || content == "" {
		return nil
	}

	now := time.Now().In(cfg.Location)
	today := now.Format("2006-01-02")

	summaryKey := "fact:" + factKey
	id, _ := upsertSummary(
		db,
		cfg,
		"fact",
		summaryKey,
		today,
		today,
		"",
		content,
		source,
	)
	if id > 0 {
		_ = upsertEmbeddingFromText(cfg, db, id, content)
	}
	return nil
}

// ProposeRememberFact stores a fact if it's new, or creates a conflict if it disagrees with an existing active fact.
// It also appends version history. This function is transactional.
func ProposeRememberFact(cfg Config, db *sql.DB, content, sourceType, sourceKey string, when time.Time) (*RememberOutcome, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return &RememberOutcome{Status: "noop"}, nil
	}

	var out *RememberOutcome
	err := withDBRetry(3, 25*time.Millisecond, func() error {
		return withTx(db, func(tx *sql.Tx) error {
			o, err := proposeRememberFactWith(cfg, tx, content, sourceType, sourceKey, when)
			out = o
			return err
		})
	})
	if err != nil {
		return nil, err
	}

	// Keep semantic search aligned with truth (best-effort, post-commit)
	if out != nil && out.Status == "remembered" {
		_ = syncFactToSearch(cfg, db, out.FactKey, content, sourceType)
	}
	return out, nil
}

func proposeRememberFactWith(cfg Config, db dbTX, content, sourceType, sourceKey string, when time.Time) (*RememberOutcome, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return &RememberOutcome{Status: "noop"}, nil
	}
	factKey := deriveFactKeyFromSubject(content)
	if factKey == "" {
		return &RememberOutcome{Status: "noop"}, nil
	}
	if sourceType == "" {
		sourceType = "remember"
	}
	if sourceKey == "" {
		sourceKey = when.Format("2006-01-02")
	}

	// ---- 1) exact key: same slot (legacy behaviour) ----
	if existing, ok := getActiveUserFactByKey(db, factKey); ok {
		if strings.TrimSpace(existing) == strings.TrimSpace(content) {
			// touch updated_at to keep it fresh
			if err := upsertUserFact(db, existing, factKey, true, when); err != nil {
				return nil, err
			}
			return &RememberOutcome{Status: "noop", FactKey: factKey}, nil
		}

		// conflict: keep current as truth, record proposal
		cid, err := createUserFactConflict(db, factKey, existing, content, sourceType, sourceKey, when)
		if err != nil {
			return nil, err
		}
		if cid > 0 {
			if err := appendUserFactHistory(db, factKey, content, "conflict", sourceType, sourceKey, when, 0); err != nil {
				return nil, err
			}
		}
		return &RememberOutcome{Status: "conflict", FactKey: factKey, ConflictID: cid, Existing: existing}, nil
	}

	// ---- 2) subject+predicate slot conflict: same subject slot, different fact_key ----
	tr := ExtractFactTriple(content)
	slotKey := tr.SlotKey()
	if slotKey != "" {
		if existingKey, existingFact, ok := getActiveUserFactBySlotKey(db, slotKey); ok {
			if strings.TrimSpace(existingFact) == strings.TrimSpace(content) {
				if err := upsertUserFact(db, existingFact, existingKey, true, when); err != nil {
					return nil, err
				}
				return &RememberOutcome{Status: "noop", FactKey: existingKey}, nil
			}
			cid, err := createUserFactConflict(db, existingKey, existingFact, content, sourceType, sourceKey, when)
			if err != nil {
				return nil, err
			}
			if cid > 0 {
				if err := appendUserFactHistory(db, existingKey, content, "conflict", sourceType, sourceKey, when, 0); err != nil {
					return nil, err
				}
			}
			return &RememberOutcome{Status: "conflict", FactKey: existingKey, ConflictID: cid, Existing: existingFact}, nil
		}
	}

	// accept as new truth
	if err := upsertUserFact(db, content, factKey, true, when); err != nil {
		return nil, err
	}
	if err := appendUserFactHistory(db, factKey, content, "active", sourceType, sourceKey, when, 0); err != nil {
		return nil, err
	}

	return &RememberOutcome{Status: "remembered", FactKey: factKey}, nil
}

// RetractFact deactivates the current fact (if any) and removes it from semantic search.
// This function is transactional.
func RetractFact(cfg Config, db *sql.DB, content, sourceType, sourceKey string, when time.Time) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	if sourceType == "" {
		sourceType = "forget"
	}
	if sourceKey == "" {
		sourceKey = when.Format("2006-01-02")
	}

	var removeKey string
	err := withDBRetry(3, 25*time.Millisecond, func() error {
		return withTx(db, func(tx *sql.Tx) error {
			factKey := deriveFactKeyFromSubject(content)
			if factKey != "" {
				if existing, ok := getActiveUserFactByKey(tx, factKey); ok {
					if err := upsertUserFact(tx, existing, factKey, false, when); err != nil {
						return err
					}
					if err := appendUserFactHistory(tx, factKey, existing, "forgotten", sourceType, sourceKey, when, 0); err != nil {
						return err
					}
					removeKey = factKey
					return nil
				}
			}

			// fallback: retract by subject+predicate slot (handles cases where different fact_key was derived)
			tr := ExtractFactTriple(content)
			slotKey := tr.SlotKey()
			if slotKey != "" {
				if existingKey, existingFact, ok := getActiveUserFactBySlotKey(tx, slotKey); ok {
					if err := upsertUserFact(tx, existingFact, existingKey, false, when); err != nil {
						return err
					}
					if err := appendUserFactHistory(tx, existingKey, existingFact, "forgotten", sourceType, sourceKey, when, 0); err != nil {
						return err
					}
					removeKey = existingKey
				}
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	if removeKey != "" {
		removeFactFromSearch(db, removeKey, "forgotten")
	}
	return nil
}
