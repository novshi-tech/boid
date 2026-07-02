package db

import (
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// DBTX is satisfied by both *sql.DB and *sql.Tx, enabling the same
// query functions to be used in and out of transactions.
type DBTX interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

type DB struct {
	Conn *sql.DB
}

func Open(path string) (*DB, error) {
	dsn := path
	if strings.Contains(dsn, "?") {
		dsn += "&_time_format=sqlite"
	} else {
		dsn += "?_time_format=sqlite"
	}
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	// SQLite is the sole persistence layer here, so a single pooled connection
	// plus a busy timeout avoids transient SQLITE_BUSY during concurrent job completion.
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	conn.SetConnMaxLifetime(0)

	// A silently-failed PRAGMA would leave the DB in an unsafe mode (no WAL / FK
	// off), so surface it rather than swallow.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := conn.Exec(pragma); err != nil {
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	return &DB{Conn: conn}, nil
}

func (d *DB) Close() error {
	return d.Conn.Close()
}

// InTxDB runs fn inside a transaction, passing a DBTX for use with
// repository/store functions. Commits on success, rolls back on error.
func InTxDB(conn *sql.DB, fn func(DBTX) error) error {
	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback() // best-effort; fn's error is the one that matters
		return err
	}
	return tx.Commit()
}
