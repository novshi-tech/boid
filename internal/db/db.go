package db

import (
	"database/sql"
	"embed"
	"fmt"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaFS embed.FS

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
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	conn.Exec("PRAGMA journal_mode=WAL")
	conn.Exec("PRAGMA foreign_keys=ON")
	return &DB{Conn: conn}, nil
}

func (d *DB) Migrate() error {
	schema, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("read schema: %w", err)
	}
	_, err = d.Conn.Exec(string(schema))
	if err != nil {
		return fmt.Errorf("exec schema: %w", err)
	}
	return nil
}

func (d *DB) Close() error {
	return d.Conn.Close()
}

// Tx wraps sql.Tx and implements DBTX. Kept for backward compatibility
// during migration; prefer InTx with DBTX directly.
type Tx struct {
	tx *sql.Tx
}

func (t *Tx) Exec(query string, args ...any) (sql.Result, error) {
	return t.tx.Exec(query, args...)
}

func (t *Tx) Query(query string, args ...any) (*sql.Rows, error) {
	return t.tx.Query(query, args...)
}

func (t *Tx) QueryRow(query string, args ...any) *sql.Row {
	return t.tx.QueryRow(query, args...)
}

// InTxDB runs fn inside a transaction, passing a DBTX for use with
// slice-level store functions. Commits on success, rolls back on error.
func InTxDB(conn *sql.DB, fn func(DBTX) error) error {
	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// InTx is the legacy method-based transaction helper kept for backward
// compatibility. New code should use InTxDB.
func (d *DB) InTx(fn func(tx *Tx) error) error {
	tx, err := d.Conn.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if err := fn(&Tx{tx: tx}); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
