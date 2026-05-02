package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	ErrCodeNotFound   = errors.New("pairing code not found")
	ErrCodeExpired    = errors.New("pairing code expired")
	ErrCodeConsumed   = errors.New("pairing code already consumed")
	ErrDeviceNotFound = errors.New("device not found")
)

type Device struct {
	ID         string
	Label      string
	CookieHash []byte
	CreatedAt  time.Time
	LastSeenAt time.Time
	RevokedAt  *time.Time
}

type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

func (s *Store) InsertPairingCode(ctx context.Context, codeHash []byte, label string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO web_pairing_codes (code_hash, label, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		codeHash, label, time.Now().UTC(), expiresAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("insert pairing code: %w", err)
	}
	return nil
}

func (s *Store) ConsumePairingCode(ctx context.Context, codeHash []byte) (label string, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var lbl sql.NullString
	var expiresAt time.Time
	var consumedAt sql.NullTime
	err = tx.QueryRowContext(ctx,
		`SELECT label, expires_at, consumed_at FROM web_pairing_codes WHERE code_hash = ?`,
		codeHash,
	).Scan(&lbl, &expiresAt, &consumedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrCodeNotFound
	}
	if err != nil {
		return "", fmt.Errorf("query pairing code: %w", err)
	}
	if consumedAt.Valid {
		return "", ErrCodeConsumed
	}
	if time.Now().After(expiresAt) {
		return "", ErrCodeExpired
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE web_pairing_codes SET consumed_at = ? WHERE code_hash = ?`,
		time.Now().UTC(), codeHash,
	); err != nil {
		return "", fmt.Errorf("consume pairing code: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return lbl.String, nil
}

func (s *Store) InsertDevice(ctx context.Context, id, label string, cookieHash []byte) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO web_devices (id, label, cookie_hash, created_at, last_seen_at) VALUES (?, ?, ?, ?, ?)`,
		id, label, cookieHash, now, now,
	)
	if err != nil {
		return fmt.Errorf("insert device: %w", err)
	}
	return nil
}

// GetDevice returns the device with the given id, or nil if not found or revoked.
func (s *Store) GetDevice(ctx context.Context, id string) (*Device, error) {
	var d Device
	var label sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, label, cookie_hash, created_at, last_seen_at FROM web_devices WHERE id = ? AND revoked_at IS NULL`,
		id,
	).Scan(&d.ID, &label, &d.CookieHash, &d.CreatedAt, &d.LastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get device: %w", err)
	}
	d.Label = label.String
	return &d, nil
}

func (s *Store) UpdateDeviceLastSeen(ctx context.Context, id string, t time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE web_devices SET last_seen_at = ? WHERE id = ?`,
		t.UTC(), id,
	)
	if err != nil {
		return fmt.Errorf("update last seen: %w", err)
	}
	return nil
}

func (s *Store) ListDevices(ctx context.Context) ([]*Device, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, label, cookie_hash, created_at, last_seen_at, revoked_at FROM web_devices ORDER BY created_at`,
	)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	defer rows.Close()

	var devices []*Device
	for rows.Next() {
		var d Device
		var label sql.NullString
		var revokedAt sql.NullTime
		if err := rows.Scan(&d.ID, &label, &d.CookieHash, &d.CreatedAt, &d.LastSeenAt, &revokedAt); err != nil {
			return nil, fmt.Errorf("scan device: %w", err)
		}
		d.Label = label.String
		if revokedAt.Valid {
			t := revokedAt.Time
			d.RevokedAt = &t
		}
		devices = append(devices, &d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate devices: %w", err)
	}
	return devices, nil
}

func (s *Store) RevokeDevice(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE web_devices SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		time.Now().UTC(), id,
	)
	if err != nil {
		return fmt.Errorf("revoke device: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoke device rows affected: %w", err)
	}
	if n == 0 {
		// Distinguish "device does not exist" from "device exists but already revoked"
		// so that callers (CLI) can surface the missing-id error instead of silently succeeding.
		var exists bool
		if err := s.db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM web_devices WHERE id = ?)`, id,
		).Scan(&exists); err != nil {
			return fmt.Errorf("revoke device exists check: %w", err)
		}
		if !exists {
			return ErrDeviceNotFound
		}
	}
	return nil
}

func (s *Store) RevokeAllDevices(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE web_devices SET revoked_at = ? WHERE revoked_at IS NULL`,
		time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("revoke all devices: %w", err)
	}
	return nil
}

func (s *Store) DeleteRevokedDevices(ctx context.Context, dryRun bool) (int64, error) {
	if dryRun {
		var n int64
		err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM web_devices WHERE revoked_at IS NOT NULL`).Scan(&n)
		if err != nil {
			return 0, fmt.Errorf("count revoked devices: %w", err)
		}
		return n, nil
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM web_devices WHERE revoked_at IS NOT NULL`)
	if err != nil {
		return 0, fmt.Errorf("delete revoked devices: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (s *Store) HasAnyDevice(ctx context.Context) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM web_devices WHERE revoked_at IS NULL`,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("has any device: %w", err)
	}
	return count > 0, nil
}
