package db

import (
	"database/sql"
	"embed"
	"fmt"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaFS embed.FS

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

type Tx struct {
	tx *sql.Tx
}

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
