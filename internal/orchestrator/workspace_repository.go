package orchestrator

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"

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
	if err := json.Unmarshal([]byte(bindingsJSON), &meta.AdditionalBindings); err != nil {
		return nil, fmt.Errorf("workspace %q: decode additional_bindings: %w", slug, err)
	}
	return meta, nil
}

// Save upserts meta at slug: INSERT if the slug has no row yet, or
// overwrite every column in place if it does. updated_at is always bumped
// to the current time.
func (r *WorkspaceRepository) Save(slug string, meta *WorkspaceMeta) error {
	return saveWorkspaceRow(r.conn, slug, meta)
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

	hostCommandsJSON, err := marshalJSONOrDefault(meta.HostCommands, len(meta.HostCommands) == 0, "[]")
	if err != nil {
		return fmt.Errorf("workspace %q: encode host_commands: %w", slug, err)
	}
	envJSON, err := marshalJSONOrDefault(meta.Env, len(meta.Env) == 0, "{}")
	if err != nil {
		return fmt.Errorf("workspace %q: encode env: %w", slug, err)
	}
	allowedDomainsJSON, err := marshalJSONOrDefault(meta.AllowedDomains, len(meta.AllowedDomains) == 0, "[]")
	if err != nil {
		return fmt.Errorf("workspace %q: encode allowed_domains: %w", slug, err)
	}
	extraReposJSON, err := marshalJSONOrDefault(meta.ExtraRepos, len(meta.ExtraRepos) == 0, "[]")
	if err != nil {
		return fmt.Errorf("workspace %q: encode extra_repos: %w", slug, err)
	}
	bindingsJSON, err := marshalJSONOrDefault(meta.AdditionalBindings, len(meta.AdditionalBindings) == 0, "[]")
	if err != nil {
		return fmt.Errorf("workspace %q: encode additional_bindings: %w", slug, err)
	}
	capabilitiesBytes, err := json.Marshal(meta.Capabilities)
	if err != nil {
		return fmt.Errorf("workspace %q: encode capabilities: %w", slug, err)
	}

	var containerImage any
	if meta.ContainerImage != "" {
		containerImage = meta.ContainerImage
	}

	if _, err := dbtx.Exec(`
		INSERT INTO workspaces (
			slug, container_image, host_commands, env, allowed_domains,
			extra_repos, capabilities, additional_bindings, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(slug) DO UPDATE SET
			container_image     = excluded.container_image,
			host_commands        = excluded.host_commands,
			env                  = excluded.env,
			allowed_domains      = excluded.allowed_domains,
			extra_repos          = excluded.extra_repos,
			capabilities         = excluded.capabilities,
			additional_bindings  = excluded.additional_bindings,
			updated_at           = datetime('now')
	`,
		slug, containerImage, hostCommandsJSON, envJSON, allowedDomainsJSON,
		extraReposJSON, string(capabilitiesBytes), bindingsJSON,
	); err != nil {
		return fmt.Errorf("workspace %q: save: %w", slug, err)
	}
	return nil
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
