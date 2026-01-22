package app

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schemaSQL = `
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;
PRAGMA busy_timeout=5000;

CREATE TABLE IF NOT EXISTS summaries (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  type TEXT NOT NULL,
  period_key TEXT NOT NULL,
  start_date TEXT NOT NULL,
  end_date TEXT NOT NULL,
  json TEXT NOT NULL,
  text TEXT NOT NULL,
  source_path TEXT,
  created_at TEXT NOT NULL,
  UNIQUE(type, period_key)
);

CREATE INDEX IF NOT EXISTS idx_summaries_type_period
  ON summaries(type, period_key);

CREATE TABLE IF NOT EXISTS embeddings (
  summary_id INTEGER PRIMARY KEY,
  dim INTEGER NOT NULL,
  vec BLOB NOT NULL,
  l2 REAL NOT NULL,
  created_at TEXT NOT NULL,
  FOREIGN KEY(summary_id)
    REFERENCES summaries(id)
    ON DELETE CASCADE
);

/*
================================================
Embedding history（用于漂移检测 embedding_guard.go）
================================================
*/
CREATE TABLE IF NOT EXISTS summary_embeddings_history (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  summary_id INTEGER NOT NULL,
  vec BLOB NOT NULL,
  created_at TEXT NOT NULL,
  FOREIGN KEY(summary_id)
    REFERENCES summaries(id)
    ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_seh_summary_id_created
  ON summary_embeddings_history(summary_id, created_at);

/*
================================================
显式长期事实（/remember）
================================================
*/
CREATE TABLE IF NOT EXISTS user_facts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  fact TEXT NOT NULL,
  fact_key TEXT NOT NULL,
  is_active INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(fact_key)
);

CREATE INDEX IF NOT EXISTS idx_user_facts_active
  ON user_facts(is_active, updated_at);

/*
================================================
事实候选池（pending_facts）
- 来源：daily summary 的 user_facts_explicit（高置信）
- UI 可一键晋升为 user_facts（/remember 的同源写入）
================================================
*/
CREATE TABLE IF NOT EXISTS pending_facts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  fact TEXT NOT NULL,
  fact_key TEXT NOT NULL,
  confidence REAL NOT NULL DEFAULT 0.0,
  source_type TEXT NOT NULL,
  source_key TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(fact_key, status, source_type, source_key)
);

CREATE INDEX IF NOT EXISTS idx_pending_facts_status_created
  ON pending_facts(status, created_at);

/*
================================================
pending_facts embeddings（用于相似合并/聚类）
================================================
*/
CREATE TABLE IF NOT EXISTS pending_fact_embeddings (
  pending_fact_id INTEGER PRIMARY KEY,
  dim INTEGER NOT NULL,
  vec BLOB NOT NULL,
  l2 REAL NOT NULL,
  created_at TEXT NOT NULL,
  FOREIGN KEY(pending_fact_id)
    REFERENCES pending_facts(id)
    ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_pfe_created
  ON pending_fact_embeddings(created_at);

/*
================================================
user_facts history（版本化）
================================================
*/
CREATE TABLE IF NOT EXISTS user_facts_history (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  fact_key TEXT NOT NULL,
  fact TEXT NOT NULL,
  status TEXT NOT NULL,         -- active | archived | forgotten | conflict | rejected
  version INTEGER NOT NULL,
  source_type TEXT NOT NULL,
  source_key TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_ufh_key_version
  ON user_facts_history(fact_key, version);

CREATE INDEX IF NOT EXISTS idx_ufh_status_created
  ON user_facts_history(status, created_at);

/*
================================================
user_facts conflicts（冲突池）
================================================
*/
CREATE TABLE IF NOT EXISTS user_fact_conflicts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  fact_key TEXT NOT NULL,
  existing_fact TEXT NOT NULL,
  proposed_fact TEXT NOT NULL,
  proposed_source_type TEXT NOT NULL,
  proposed_source_key TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'conflict',  -- conflict | resolved_keep | resolved_replace
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_ufc_status_created
  ON user_fact_conflicts(status, created_at);

`

func mustOpenDB(cfg Config) *sql.DB {
	_ = os.MkdirAll(filepath.Dir(cfg.DBPath), 0755)

	db, err := sql.Open("sqlite", cfg.DBPath)
	if err != nil {
		panic(err)
	}

	// SQLite connection settings (production defaults)
	maxConns := cfg.SQLiteMaxOpenConns
	if maxConns <= 0 {
		maxConns = 1
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	db.SetConnMaxLifetime(0)

	// Apply pragmas (per-connection). With maxConns=1 this is stable.
	// NOTE: schemaSQL also includes safe defaults; these override them if set.
	if cfg.SQLiteBusyTimeoutMS > 0 {
		_, _ = db.Exec(fmt.Sprintf("PRAGMA busy_timeout=%d;", cfg.SQLiteBusyTimeoutMS))
	}
	jm := strings.ToUpper(strings.TrimSpace(cfg.SQLiteJournalMode))
	if jm != "" {
		// journal_mode does not accept bound parameters in all drivers; inject a safe, normalized literal.
		_, _ = db.Exec("PRAGMA journal_mode=" + jm + ";")
	}
	sync := strings.ToUpper(strings.TrimSpace(cfg.SQLiteSynchronous))
	if sync != "" {
		_, _ = db.Exec("PRAGMA synchronous=" + sync + ";")
	}
	_, _ = db.Exec("PRAGMA foreign_keys=ON;")

	if _, err := db.Exec(schemaSQL); err != nil {
		panic(err)
	}

	// ✅ Backward-compatible migrations for older DBs.
	// (CREATE TABLE IF NOT EXISTS does not update existing tables.)
	_ = ensurePendingFactsSchema(db, cfg)

	return db
}

// ensurePendingFactsSchema performs small, safe migrations for older DBs.
// Older installs may have a pending_facts table without the newer columns
// (or without the UNIQUE constraint used by some earlier versions).
//
// This function is best-effort: it never panics and tries to keep existing data.
func ensurePendingFactsSchema(db *sql.DB, cfg Config) error {
	if db == nil {
		return nil
	}

	// If the table doesn't exist yet, schemaSQL already created it.
	rows, err := db.Query(`PRAGMA table_info(pending_facts);`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err == nil {
			cols[name] = true
		}
	}

	now := time.Now()
	if cfg.Location != nil {
		now = now.In(cfg.Location)
	}
	nowS := now.Format(time.RFC3339)

	add := func(col, ddl, backfill string, args ...any) {
		if cols[col] {
			return
		}
		_, _ = db.Exec(ddl)
		if backfill != "" {
			_, _ = db.Exec(backfill, args...)
		}
		cols[col] = true
	}

	// Add missing columns (legacy DB compatibility)
	add("source_type", "ALTER TABLE pending_facts ADD COLUMN source_type TEXT DEFAULT 'legacy'", "UPDATE pending_facts SET source_type='legacy' WHERE source_type IS NULL OR source_type=''")
	add("source_key", "ALTER TABLE pending_facts ADD COLUMN source_key TEXT DEFAULT 'legacy'", "UPDATE pending_facts SET source_key='legacy' WHERE source_key IS NULL OR source_key=''")
	add("status", "ALTER TABLE pending_facts ADD COLUMN status TEXT DEFAULT 'pending'", "UPDATE pending_facts SET status='pending' WHERE status IS NULL OR status=''")
	add("confidence", "ALTER TABLE pending_facts ADD COLUMN confidence REAL DEFAULT 0", "")
	add("created_at", "ALTER TABLE pending_facts ADD COLUMN created_at TEXT DEFAULT ''", "UPDATE pending_facts SET created_at=? WHERE created_at IS NULL OR created_at=''", nowS)
	add("updated_at", "ALTER TABLE pending_facts ADD COLUMN updated_at TEXT DEFAULT ''", "UPDATE pending_facts SET updated_at=? WHERE updated_at IS NULL OR updated_at=''", nowS)

	// Helpful indexes (best-effort)
	_, _ = db.Exec("CREATE INDEX IF NOT EXISTS idx_pending_facts_status ON pending_facts(status)")
	_, _ = db.Exec("CREATE INDEX IF NOT EXISTS idx_pending_facts_updated ON pending_facts(updated_at)")
	_, _ = db.Exec("CREATE INDEX IF NOT EXISTS idx_pending_facts_source ON pending_facts(source_type, source_key)")

	// Try to provide the uniqueness needed by some historical ON CONFLICT uses.
	// If legacy rows contain duplicates, this may fail; ignore in that case.
	_, _ = db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS ux_pending_facts_key ON pending_facts(fact_key, status, source_type, source_key)")

	return nil
}

// =========================
// summaries helpers
// =========================

func summaryExists(db *sql.DB, typ, key string) (bool, error) {
	row := db.QueryRow(
		`SELECT 1 FROM summaries WHERE type=? AND period_key=? LIMIT 1`,
		typ, key,
	)
	var one int
	err := row.Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func upsertSummary(
	db *sql.DB,
	cfg Config,
	typ, key, startDate, endDate, js, text, srcPath string,
) (int64, error) {

	now := time.Now().In(cfg.Location).Format(time.RFC3339)

	_, err := db.Exec(`
		INSERT INTO summaries(
		  type, period_key, start_date, end_date,
		  json, text, source_path, created_at
		)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(type, period_key) DO UPDATE SET
		  json=excluded.json,
		  text=excluded.text,
		  source_path=excluded.source_path
	`, typ, key, startDate, endDate, js, text, srcPath, now)
	if err != nil {
		return 0, err
	}

	row := db.QueryRow(
		`SELECT id FROM summaries WHERE type=? AND period_key=?`,
		typ, key,
	)

	var id int64
	if err := row.Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// =========================
// embeddings helpers
// =========================

// 是否已有 embedding（1:1）
func hasEmbedding(db *sql.DB, summaryID int64) bool {
	row := db.QueryRow(
		`SELECT 1 FROM embeddings WHERE summary_id=? LIMIT 1`,
		summaryID,
	)
	var one int
	return row.Scan(&one) == nil
}

// 删除 embedding（用于 summary 更新）
func deleteEmbedding(db *sql.DB, summaryID int64) error {
	_, err := db.Exec(
		`DELETE FROM embeddings WHERE summary_id=?`,
		summaryID,
	)
	return err
}

// =========================
// user_facts helpers
// =========================

// upsertUserFact 写入或更新一条显式事实（由上层保证 fact_key 已规范化）
func upsertUserFact(
	db dbTX,
	fact string,
	factKey string,
	active bool,
	now time.Time,
) error {

	if db == nil || factKey == "" {
		return nil
	}

	activeInt := 0
	if active {
		activeInt = 1
	}

	ts := now.Format(time.RFC3339)

	_, err := db.Exec(`
		INSERT INTO user_facts(
		  fact, fact_key, is_active, created_at, updated_at
		)
		VALUES(?,?,?,?,?)
		ON CONFLICT(fact_key) DO UPDATE SET
		  fact=excluded.fact,
		  is_active=excluded.is_active,
		  updated_at=excluded.updated_at
	`, fact, factKey, activeInt, ts, ts)

	return err
}

// loadActiveUserFacts 读取当前有效的显式事实（按最近更新时间排序）
func loadActiveUserFacts(db *sql.DB, limit int) ([]string, error) {
	if db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}

	rows, err := db.Query(`
		SELECT fact
		FROM user_facts
		WHERE is_active=1
		ORDER BY updated_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var fact string
		if err := rows.Scan(&fact); err != nil {
			continue
		}
		out = append(out, fact)
	}
	return out, nil
}
