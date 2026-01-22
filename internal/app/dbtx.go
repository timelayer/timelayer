package app

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// dbTX is the minimal interface shared by *sql.DB and *sql.Tx.
// It enables us to keep critical write paths transactional without a large refactor.
type dbTX interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// withTx executes fn in a DB transaction.
// It commits on nil error and rolls back otherwise.
func withTx(db *sql.DB, fn func(tx *sql.Tx) error) error {
	if db == nil {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// isSQLiteBusy is a small helper to detect transient SQLite lock/busy errors.
// We keep it string-based to avoid coupling to a specific driver error type.
func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database is busy") ||
		strings.Contains(msg, "busy")
}

// withDBRetry retries fn on transient SQLite lock/busy errors.
// This is only used for short critical writes.
func withDBRetry(attempts int, baseDelay time.Duration, fn func() error) error {
	if attempts <= 1 {
		return fn()
	}
	if baseDelay <= 0 {
		baseDelay = 25 * time.Millisecond
	}
	var last error
	for i := 0; i < attempts; i++ {
		err := fn()
		if err == nil {
			return nil
		}
		last = err
		if !isSQLiteBusy(err) {
			return err
		}
		time.Sleep(baseDelay * time.Duration(1+i))
	}
	return last
}

var errUnauthorized = errors.New("unauthorized")
