package app

import "database/sql"

// MustInit initializes directories/prompts and opens DB + log writer.
func MustInit(cfg Config) (*sql.DB, *LogWriter) {
	mustEnsureDirs(cfg)
	mustEnsurePromptFiles(cfg)

	db := mustOpenDB(cfg)
	lw := NewLogWriter(cfg, db)
	return db, lw
}
