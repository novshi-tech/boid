package orchestrator

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// EgressPortStore is the DB-backed implementation of sandbox.PortStore
// against the `workspace_egress_port` table
// (internal/db/migrate/migrations/0039_add_workspace_egress_port.sql).
//
// It lives here rather than in internal/sandbox because that package must
// not import internal/db — the same reason dispatcher.ProxyAllocator is an
// interface satisfied from outside. See
// docs/plans/egress-proxy-stable-port.md.
//
// The key is a proxy key, NOT necessarily a workspace slug: the
// no-workspace listener uses the reserved key
// dispatcher.NoWorkspaceProxyKey ("__no_workspace__"), which
// ValidWorkspaceSlug would reject. Nothing here validates the key as a slug
// for that reason.
type EgressPortStore struct {
	conn *sql.DB
}

// NewEgressPortStore returns an EgressPortStore backed by conn.
func NewEgressPortStore(conn *sql.DB) *EgressPortStore {
	return &EgressPortStore{conn: conn}
}

// LoadPort returns the port recorded for key, if any.
func (s *EgressPortStore) LoadPort(key string) (int, bool, error) {
	if key == "" {
		return 0, false, errors.New("egress port store: key is required")
	}
	var port int
	err := s.conn.QueryRow(
		`SELECT port FROM workspace_egress_port WHERE proxy_key = ?`, key,
	).Scan(&port)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("egress port store: load %q: %w", key, err)
	}
	return port, true, nil
}

// ReservedPorts returns every recorded reservation as port -> proxy key.
//
// Used by the allocator to avoid handing one key a port another key is
// already reserving. That distinction matters because proxy listeners are
// created lazily at dispatch time: a workspace that has not been dispatched
// since the last daemon restart has a row here with no listener behind it,
// so a bind attempt on its port succeeds and would silently take it.
func (s *EgressPortStore) ReservedPorts() (map[int]string, error) {
	rows, err := s.conn.Query(`SELECT port, proxy_key FROM workspace_egress_port`)
	if err != nil {
		return nil, fmt.Errorf("egress port store: list reservations: %w", err)
	}
	defer rows.Close()

	out := make(map[int]string)
	for rows.Next() {
		var (
			port int
			key  string
		)
		if err := rows.Scan(&port, &key); err != nil {
			return nil, fmt.Errorf("egress port store: scan reservation: %w", err)
		}
		out[port] = key
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("egress port store: list reservations: %w", err)
	}
	return out, nil
}

// SavePort records port as key's allocation.
//
// Two collisions have to be handled, and both resolve in favour of the
// caller — by the time this is called the listener is already bound, so the
// DB's job is to record reality, not to arbitrate it:
//
//   - the same key already has a row (the reallocation path, when a
//     persisted port turned out to be taken): the row is updated in place.
//   - a DIFFERENT key already holds this port (`port` is UNIQUE): the stale
//     row is deleted first. The previous holder cannot still be listening on
//     it — this process just bound it — so its record is simply out of date.
//
// Both happen in one transaction so a failure cannot leave two keys claiming
// one port, or the port claimed by nobody.
func (s *EgressPortStore) SavePort(key string, port int) error {
	if key == "" {
		return errors.New("egress port store: key is required")
	}
	if port <= 0 {
		return fmt.Errorf("egress port store: port must be positive, got %d", port)
	}

	tx, err := s.conn.Begin()
	if err != nil {
		return fmt.Errorf("egress port store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`DELETE FROM workspace_egress_port WHERE port = ? AND proxy_key <> ?`, port, key,
	); err != nil {
		return fmt.Errorf("egress port store: clear stale holder of port %d: %w", port, err)
	}
	if _, err := tx.Exec(`
		INSERT INTO workspace_egress_port (proxy_key, port, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(proxy_key) DO UPDATE SET port = excluded.port, updated_at = excluded.updated_at`,
		key, port, time.Now().UTC(),
	); err != nil {
		return fmt.Errorf("egress port store: save %q: %w", key, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("egress port store: commit: %w", err)
	}
	return nil
}
