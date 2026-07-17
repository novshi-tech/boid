package orchestrator

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/novshi-tech/boid/internal/db"
)

// WorkspaceRepository provides DB-backed CRUD for WorkspaceMeta against the
// `workspaces` table (internal/db/migrate/migrations/0030_add_workspaces_table.sql).
// It is the authority WorkspaceStore delegates to once
// MigrateWorkspaceYAMLToDB has cut over (docs/plans/workspace-db-consolidation.md
// PR3): the yaml files under DefaultWorkspaceDir() become a read-only shadow
// kept for rollback/export, and this repository becomes the read/write path.
type WorkspaceRepository struct {
	conn *sql.DB
}

// NewWorkspaceRepository returns a WorkspaceRepository backed by conn.
func NewWorkspaceRepository(conn *sql.DB) *WorkspaceRepository {
	return &WorkspaceRepository{conn: conn}
}

// Load reads and decodes the WorkspaceMeta for the given slug. Returns an
// error wrapping os.ErrNotExist when no row exists — matching
// WorkspaceStore.Load's contract so callers do not need to branch on which
// backing store is in use.
func (r *WorkspaceRepository) Load(slug string) (*WorkspaceMeta, error) {
	if err := ValidWorkspaceSlug(slug); err != nil {
		return nil, err
	}
	row := r.conn.QueryRow(`
		SELECT slug, container_image, host_commands, env, allowed_domains,
		       extra_repos, capabilities, additional_bindings
		FROM workspaces WHERE slug = ?`, slug)

	var (
		containerImage     sql.NullString
		hostCommandsJSON   string
		envJSON            string
		allowedDomainsJSON string
		extraReposJSON     string
		capabilitiesJSON   string
		bindingsJSON       string
	)
	if err := row.Scan(
		&slug, &containerImage, &hostCommandsJSON, &envJSON,
		&allowedDomainsJSON, &extraReposJSON, &capabilitiesJSON, &bindingsJSON,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("workspace %q: %w", slug, os.ErrNotExist)
		}
		return nil, fmt.Errorf("workspace %q: query: %w", slug, err)
	}

	return decodeWorkspaceMetaColumns(slug, containerImage, hostCommandsJSON, envJSON, allowedDomainsJSON, extraReposJSON, capabilitiesJSON, bindingsJSON)
}

// LoadWithRevision reads meta and its revision (updated_at, formatted the
// same way GetWorkspaceSummary/ListWorkspaces do) from a single row —
// docs/plans/workspace-db-consolidation.md MAJOR 1 (codex review): GET
// /api/workspaces/{slug} previously read meta (this method's SELECT) and
// revision (a separate GetWorkspaceSummary query) as two round trips, which
// could straddle a concurrent PUT and return a meta/revision pair that never
// coexisted in the DB. Returns an error wrapping os.ErrNotExist when no row
// exists for slug, matching Load's contract.
func (r *WorkspaceRepository) LoadWithRevision(slug string) (*WorkspaceMeta, string, error) {
	if err := ValidWorkspaceSlug(slug); err != nil {
		return nil, "", err
	}
	row := r.conn.QueryRow(`
		SELECT container_image, host_commands, env, allowed_domains,
		       extra_repos, capabilities, additional_bindings, updated_at
		FROM workspaces WHERE slug = ?`, slug)

	var (
		containerImage     sql.NullString
		hostCommandsJSON   string
		envJSON            string
		allowedDomainsJSON string
		extraReposJSON     string
		capabilitiesJSON   string
		bindingsJSON       string
		updatedAt          time.Time
	)
	if err := row.Scan(
		&containerImage, &hostCommandsJSON, &envJSON,
		&allowedDomainsJSON, &extraReposJSON, &capabilitiesJSON, &bindingsJSON,
		&updatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", fmt.Errorf("workspace %q: %w", slug, os.ErrNotExist)
		}
		return nil, "", fmt.Errorf("workspace %q: query: %w", slug, err)
	}

	meta, err := decodeWorkspaceMetaColumns(slug, containerImage, hostCommandsJSON, envJSON, allowedDomainsJSON, extraReposJSON, capabilitiesJSON, bindingsJSON)
	if err != nil {
		return nil, "", err
	}
	return meta, formatRevision(updatedAt), nil
}

// UpdateIfRevisionMatches performs a compare-and-swap update: meta at slug
// is written only if the row's current revision (updated_at) equals
// expectedRevision, atomically with the check, via a single UPDATE
// statement (docs/plans/workspace-db-consolidation.md MAJOR 1, codex
// review). This closes the PUT race the previous "read revision, then Save
// unconditionally" two-step had: two concurrent PUTs against the same
// starting ETag could otherwise both pass their (separate-query) If-Match
// check and both Save, silently losing one writer's update; likewise a
// DELETE landing between a GET and a subsequent PUT could no longer be
// resurrected by an upsert-based Save.
//
// matched=false covers three cases the caller cannot tell apart from this
// return value alone: slug has no row at all, slug exists but its current
// revision differs from expectedRevision, or expectedRevision is not even a
// well-formed revision string (e.g. a client-supplied If-Match that was
// never a real ETag this server issued) — the last case is deliberately
// folded into "no match" rather than a hard error, since a malformed value
// trivially can never equal the real (always well-formed) current
// revision; this keeps the HTTP mapping a plain 412, not a spurious 500, for
// a garbage If-Match header. ProjectAppService.UpdateWorkspace
// distinguishes "no row" from the other two with a follow-up existence
// check (404 vs 412) — see its doc comment.
//
// On success, returns the freshly bumped revision (formatted the same way
// as LoadWithRevision/GetWorkspaceSummary) so the caller can hand it back to
// the client as the new ETag without a second read.
func (r *WorkspaceRepository) UpdateIfRevisionMatches(slug string, expectedRevision string, meta *WorkspaceMeta) (newRevision string, matched bool, err error) {
	if err := ValidWorkspaceSlug(slug); err != nil {
		return "", false, err
	}
	if meta == nil {
		meta = &WorkspaceMeta{}
	}
	expected, err := time.Parse(time.RFC3339Nano, expectedRevision)
	if err != nil {
		return "", false, nil
	}

	hostCommandsJSON, envJSON, allowedDomainsJSON, extraReposJSON, capabilitiesJSON, bindingsJSON, containerImage, err := marshalWorkspaceMetaColumns(slug, meta)
	if err != nil {
		return "", false, err
	}

	newUpdatedAt := nowForRevision()
	res, err := r.conn.Exec(`
		UPDATE workspaces SET
			container_image = ?, host_commands = ?, env = ?, allowed_domains = ?,
			extra_repos = ?, capabilities = ?, additional_bindings = ?, updated_at = ?
		WHERE slug = ? AND updated_at = ?
	`,
		containerImage, hostCommandsJSON, envJSON, allowedDomainsJSON,
		extraReposJSON, capabilitiesJSON, bindingsJSON, newUpdatedAt,
		slug, expected,
	)
	if err != nil {
		return "", false, fmt.Errorf("workspace %q: update if revision matches: %w", slug, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", false, fmt.Errorf("workspace %q: rows affected: %w", slug, err)
	}
	if n == 0 {
		return "", false, nil
	}
	return formatRevision(newUpdatedAt), true, nil
}

// formatRevision renders t as the canonical revision/ETag string, matching
// GetWorkspaceSummary/ListWorkspaces's `updatedAt.UTC().Format(time.RFC3339Nano)`.
func formatRevision(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// decodeWorkspaceMetaColumns decodes the JSON column values shared by Load
// and LoadWithRevision into a *WorkspaceMeta, so the two share identical
// decode logic rather than letting it drift out of sync.
//
// bindingsJSON (the `workspaces.additional_bindings` column) is decoded and
// discarded rather than mapped onto the result: WorkspaceMeta.AdditionalBindings
// was retired outright in docs/plans/home-workspace-volume.md Phase 4 PR4
// (see that struct's own doc comment). The column itself is not dropped by
// this PR's migration — a future major schema cleanup removes it — so a row
// written before this binary can still carry a non-empty JSON array here;
// decoding it (rather than ignoring the column) still validates it is
// well-formed JSON and lets this function warn when it is non-trivial, so an
// operator inspecting logs after an upgrade understands why a previously
// working workspace-scoped bind mount stopped applying.
func decodeWorkspaceMetaColumns(slug string, containerImage sql.NullString, hostCommandsJSON, envJSON, allowedDomainsJSON, extraReposJSON, capabilitiesJSON, bindingsJSON string) (*WorkspaceMeta, error) {
	meta := &WorkspaceMeta{}
	if containerImage.Valid {
		meta.ContainerImage = containerImage.String
	}
	if err := json.Unmarshal([]byte(hostCommandsJSON), &meta.HostCommands); err != nil {
		return nil, fmt.Errorf("workspace %q: decode host_commands: %w", slug, err)
	}
	if err := json.Unmarshal([]byte(envJSON), &meta.Env); err != nil {
		return nil, fmt.Errorf("workspace %q: decode env: %w", slug, err)
	}
	if err := json.Unmarshal([]byte(allowedDomainsJSON), &meta.AllowedDomains); err != nil {
		return nil, fmt.Errorf("workspace %q: decode allowed_domains: %w", slug, err)
	}
	if err := json.Unmarshal([]byte(extraReposJSON), &meta.ExtraRepos); err != nil {
		return nil, fmt.Errorf("workspace %q: decode extra_repos: %w", slug, err)
	}
	if err := json.Unmarshal([]byte(capabilitiesJSON), &meta.Capabilities); err != nil {
		return nil, fmt.Errorf("workspace %q: decode capabilities: %w", slug, err)
	}
	var discardedBindings []BindMount
	if err := json.Unmarshal([]byte(bindingsJSON), &discardedBindings); err != nil {
		return nil, fmt.Errorf("workspace %q: decode additional_bindings: %w", slug, err)
	}
	if len(discardedBindings) > 0 {
		slog.Warn("workspace: additional_bindings is no longer supported (retired in docs/plans/home-workspace-volume.md Phase 4 PR4); the stored value is ignored",
			"slug", slug, "count", len(discardedBindings))
	}
	return meta, nil
}

// Save upserts meta at slug: INSERT if the slug has no row yet, or
// overwrite every column in place if it does. updated_at is always bumped
// to the current time.
func (r *WorkspaceRepository) Save(slug string, meta *WorkspaceMeta) error {
	return saveWorkspaceRow(r.conn, slug, meta)
}

// Create inserts a brand-new workspace row at slug. Unlike Save, this is
// insert-only: a slug that already has a row is rejected with an error
// wrapping os.ErrExist (docs/plans/workspace-db-consolidation.md Step A —
// the API layer maps this to HTTP 409 for POST /api/workspaces) rather than
// silently overwriting it. The plain INSERT (no ON CONFLICT clause) makes
// SQLite itself the source of truth for the conflict, so a concurrent
// creator racing this call is still caught by the UNIQUE constraint on
// workspaces.slug rather than a separate (racy) existence check.
func (r *WorkspaceRepository) Create(slug string, meta *WorkspaceMeta) error {
	if err := ValidWorkspaceSlug(slug); err != nil {
		return err
	}
	if meta == nil {
		meta = &WorkspaceMeta{}
	}

	hostCommandsJSON, envJSON, allowedDomainsJSON, extraReposJSON, capabilitiesJSON, bindingsJSON, containerImage, err := marshalWorkspaceMetaColumns(slug, meta)
	if err != nil {
		return err
	}

	if _, err := r.conn.Exec(`
		INSERT INTO workspaces (
			slug, container_image, host_commands, env, allowed_domains,
			extra_repos, capabilities, additional_bindings, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		slug, containerImage, hostCommandsJSON, envJSON, allowedDomainsJSON,
		extraReposJSON, capabilitiesJSON, bindingsJSON, nowForRevision(),
	); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return fmt.Errorf("workspace %q: %w", slug, os.ErrExist)
		}
		return fmt.Errorf("workspace %q: create: %w", slug, err)
	}
	return nil
}

// saveWorkspaceRow holds the upsert logic shared by WorkspaceRepository.Save
// (via r.conn, autocommit) and MigrateWorkspaceYAMLToDB's cutover
// transaction (via a *sql.Tx — both satisfy db.DBTX, so the same statement
// runs against either without duplicating the SQL/marshal logic in two
// places that could drift out of sync).
func saveWorkspaceRow(dbtx db.DBTX, slug string, meta *WorkspaceMeta) error {
	if err := ValidWorkspaceSlug(slug); err != nil {
		return err
	}
	if meta == nil {
		meta = &WorkspaceMeta{}
	}

	hostCommandsJSON, envJSON, allowedDomainsJSON, extraReposJSON, capabilitiesJSON, bindingsJSON, containerImage, err := marshalWorkspaceMetaColumns(slug, meta)
	if err != nil {
		return err
	}

	updatedAt := nowForRevision()
	if _, err := dbtx.Exec(`
		INSERT INTO workspaces (
			slug, container_image, host_commands, env, allowed_domains,
			extra_repos, capabilities, additional_bindings, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(slug) DO UPDATE SET
			container_image     = excluded.container_image,
			host_commands        = excluded.host_commands,
			env                  = excluded.env,
			allowed_domains      = excluded.allowed_domains,
			extra_repos          = excluded.extra_repos,
			capabilities         = excluded.capabilities,
			additional_bindings  = excluded.additional_bindings,
			updated_at           = excluded.updated_at
	`,
		slug, containerImage, hostCommandsJSON, envJSON, allowedDomainsJSON,
		extraReposJSON, capabilitiesJSON, bindingsJSON, updatedAt,
	); err != nil {
		return fmt.Errorf("workspace %q: save: %w", slug, err)
	}
	return nil
}

// nowForRevision returns the current time at nanosecond precision (UTC),
// for binding as the workspaces.updated_at column value on Create/Save.
// Explicitly computing this in Go (rather than relying on SQLite's
// datetime('now'), which only has whole-second resolution) matters because
// updated_at is the source of WorkspaceSummary.Revision (the PUT
// /api/workspaces/{slug} If-Match ETag, decision 17): two writes to the same
// workspace within the same wall-clock second would otherwise produce an
// identical revision string, letting a stale If-Match check pass when it
// should not (a lost-update window, not just a cosmetic "revision didn't
// visibly change" annoyance).
func nowForRevision() time.Time {
	return time.Now().UTC()
}

// marshalWorkspaceMetaColumns encodes meta's fields into the JSON column
// values shared by saveWorkspaceRow's upsert and WorkspaceRepository.Create's
// insert-only path, so the two statements can never drift out of sync on how
// a given field is serialized. containerImage is returned as `any` so it can
// be passed straight to Exec: nil (SQL NULL) when meta.ContainerImage is
// empty, or the string itself otherwise.
//
// bindingsJSON (the `workspaces.additional_bindings` column) is always
// written as the empty-array literal: WorkspaceMeta has no AdditionalBindings
// field any more (Phase 4 PR4, docs/plans/home-workspace-volume.md — see that
// struct's doc comment) to source a value from, so every Save/Create/Update
// from this binary zeroes out whatever a previous binary may have stored
// there. The column itself is kept for now (a future major schema cleanup
// removes it outright); see decodeWorkspaceMetaColumns for the read side.
func marshalWorkspaceMetaColumns(slug string, meta *WorkspaceMeta) (hostCommandsJSON, envJSON, allowedDomainsJSON, extraReposJSON, capabilitiesJSON, bindingsJSON string, containerImage any, err error) {
	hostCommandsJSON, err = marshalJSONOrDefault(meta.HostCommands, len(meta.HostCommands) == 0, "[]")
	if err != nil {
		return "", "", "", "", "", "", nil, fmt.Errorf("workspace %q: encode host_commands: %w", slug, err)
	}
	envJSON, err = marshalJSONOrDefault(meta.Env, len(meta.Env) == 0, "{}")
	if err != nil {
		return "", "", "", "", "", "", nil, fmt.Errorf("workspace %q: encode env: %w", slug, err)
	}
	allowedDomainsJSON, err = marshalJSONOrDefault(meta.AllowedDomains, len(meta.AllowedDomains) == 0, "[]")
	if err != nil {
		return "", "", "", "", "", "", nil, fmt.Errorf("workspace %q: encode allowed_domains: %w", slug, err)
	}
	extraReposJSON, err = marshalJSONOrDefault(meta.ExtraRepos, len(meta.ExtraRepos) == 0, "[]")
	if err != nil {
		return "", "", "", "", "", "", nil, fmt.Errorf("workspace %q: encode extra_repos: %w", slug, err)
	}
	bindingsJSON = "[]"
	capabilitiesBytes, err := json.Marshal(meta.Capabilities)
	if err != nil {
		return "", "", "", "", "", "", nil, fmt.Errorf("workspace %q: encode capabilities: %w", slug, err)
	}
	capabilitiesJSON = string(capabilitiesBytes)

	if meta.ContainerImage != "" {
		containerImage = meta.ContainerImage
	}
	return hostCommandsJSON, envJSON, allowedDomainsJSON, extraReposJSON, capabilitiesJSON, bindingsJSON, containerImage, nil
}

// Remove deletes the workspace row for slug. The reserved DefaultWorkspaceSlug
// cannot be removed (docs/plans/workspace-db-consolidation.md 「default
// workspace の実装詳細」). Any project currently assigned to slug is
// re-pointed at DefaultWorkspaceSlug in the same transaction as the delete,
// so a project never ends up referencing a workspace that no longer exists.
// Returns an error wrapping os.ErrNotExist when no row exists for slug.
func (r *WorkspaceRepository) Remove(slug string) error {
	if err := ValidWorkspaceSlug(slug); err != nil {
		return err
	}
	if slug == DefaultWorkspaceSlug {
		return fmt.Errorf("workspace %q is reserved and cannot be removed", slug)
	}

	tx, err := r.conn.Begin()
	if err != nil {
		return fmt.Errorf("workspace %q: begin remove tx: %w", slug, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	if _, err := tx.Exec(
		`UPDATE project_workspaces SET workspace_id = ? WHERE workspace_id = ?`,
		DefaultWorkspaceSlug, slug,
	); err != nil {
		return fmt.Errorf("workspace %q: reassign projects to default: %w", slug, err)
	}

	res, err := tx.Exec(`DELETE FROM workspaces WHERE slug = ?`, slug)
	if err != nil {
		return fmt.Errorf("workspace %q: delete: %w", slug, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("workspace %q: rows affected: %w", slug, err)
	}
	if n == 0 {
		return fmt.Errorf("workspace %q: %w", slug, os.ErrNotExist)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("workspace %q: commit remove: %w", slug, err)
	}
	return nil
}

// List returns every configured workspace slug, sorted alphabetically.
func (r *WorkspaceRepository) List() ([]string, error) {
	rows, err := r.conn.Query(`SELECT slug FROM workspaces ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	defer rows.Close()

	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, fmt.Errorf("list workspaces: scan: %w", err)
		}
		slugs = append(slugs, slug)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	return slugs, nil
}

// EnsureDefault inserts an empty row for DefaultWorkspaceSlug if none exists
// yet. It is safe to call repeatedly: an existing default workspace (with or
// without user edits) is left untouched.
func (r *WorkspaceRepository) EnsureDefault() error {
	return ensureDefaultWorkspaceRow(r.conn)
}

// ensureDefaultWorkspaceRow is the db.DBTX-scoped counterpart of
// WorkspaceRepository.EnsureDefault, reused by MigrateWorkspaceYAMLToDB
// inside its cutover transaction (see saveWorkspaceRow's doc comment for why
// this split exists).
func ensureDefaultWorkspaceRow(dbtx db.DBTX) error {
	if _, err := dbtx.Exec(
		`INSERT OR IGNORE INTO workspaces (slug) VALUES (?)`, DefaultWorkspaceSlug,
	); err != nil {
		return fmt.Errorf("ensure default workspace: %w", err)
	}
	return nil
}

// marshalJSONOrDefault marshals v to JSON, unless empty is true — in which
// case it returns def directly. This keeps zero-value slices/maps stored as
// the column's own canonical empty literal ("[]" / "{}") rather than the
// "null" that json.Marshal(nil) would otherwise produce, matching the
// workspaces table's NOT NULL DEFAULT columns (0030_add_workspaces_table.sql).
func marshalJSONOrDefault(v any, empty bool, def string) (string, error) {
	if empty {
		return def, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
