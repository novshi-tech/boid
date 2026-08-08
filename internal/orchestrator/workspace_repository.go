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
	"gopkg.in/yaml.v3"
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
	return loadWorkspaceMeta(r.conn, slug)
}

// loadWorkspaceMeta is the db.DBTX-scoped counterpart of
// WorkspaceRepository.Load, reused by ApplyWorkspaceEnvelope
// (workspace_apply.go) so a single all-or-nothing apply transaction can
// read a workspace's current row through the exact same decode logic
// against a *sql.Tx instead of r.conn (see saveWorkspaceRow's doc comment
// for why this split exists for the write side too).
func loadWorkspaceMeta(dbtx db.DBTX, slug string) (*WorkspaceMeta, error) {
	if err := ValidWorkspaceSlug(slug); err != nil {
		return nil, err
	}
	row := dbtx.QueryRow(`
		SELECT slug, container_image, host_commands, env, allowed_domains,
		       extra_repos, services, capabilities, additional_bindings,
		       task_behaviors, base_branch, fork_point, default_task_behavior
		FROM workspaces WHERE slug = ?`, slug)

	var cols workspaceMetaColumns
	if err := row.Scan(
		&slug, &cols.ContainerImage, &cols.HostCommandsJSON, &cols.EnvJSON,
		&cols.AllowedDomainsJSON, &cols.ExtraReposJSON, &cols.ServicesJSON, &cols.CapabilitiesJSON, &cols.BindingsJSON,
		&cols.TaskBehaviorsYAML, &cols.BaseBranch, &cols.ForkPoint, &cols.DefaultTaskBehavior,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("workspace %q: %w", slug, os.ErrNotExist)
		}
		return nil, fmt.Errorf("workspace %q: query: %w", slug, err)
	}

	return decodeWorkspaceMetaColumns(slug, cols)
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
	return loadWorkspaceMetaWithRevision(r.conn, slug)
}

// loadWorkspaceMetaWithRevision is the db.DBTX-scoped counterpart of
// WorkspaceRepository.LoadWithRevision — see loadWorkspaceMeta's doc
// comment for why this split exists (PR-1d codex round-1 Blocker 2:
// ApplyWorkspaceEnvelope needs to read a workspace's current meta+revision
// from inside its own apply transaction, not a fresh r.conn query that
// could straddle the transaction's own write).
func loadWorkspaceMetaWithRevision(dbtx db.DBTX, slug string) (*WorkspaceMeta, string, error) {
	if err := ValidWorkspaceSlug(slug); err != nil {
		return nil, "", err
	}
	row := dbtx.QueryRow(`
		SELECT container_image, host_commands, env, allowed_domains,
		       extra_repos, services, capabilities, additional_bindings,
		       task_behaviors, base_branch, fork_point, default_task_behavior,
		       updated_at
		FROM workspaces WHERE slug = ?`, slug)

	var (
		cols      workspaceMetaColumns
		updatedAt time.Time
	)
	if err := row.Scan(
		&cols.ContainerImage, &cols.HostCommandsJSON, &cols.EnvJSON,
		&cols.AllowedDomainsJSON, &cols.ExtraReposJSON, &cols.ServicesJSON, &cols.CapabilitiesJSON, &cols.BindingsJSON,
		&cols.TaskBehaviorsYAML, &cols.BaseBranch, &cols.ForkPoint, &cols.DefaultTaskBehavior,
		&updatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", fmt.Errorf("workspace %q: %w", slug, os.ErrNotExist)
		}
		return nil, "", fmt.Errorf("workspace %q: query: %w", slug, err)
	}

	meta, err := decodeWorkspaceMetaColumns(slug, cols)
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

	cols, err := marshalWorkspaceMetaColumns(slug, meta)
	if err != nil {
		return "", false, err
	}

	newUpdatedAt := nowForRevision()
	res, err := r.conn.Exec(`
		UPDATE workspaces SET
			container_image = ?, host_commands = ?, env = ?, allowed_domains = ?,
			extra_repos = ?, services = ?, capabilities = ?, additional_bindings = ?,
			task_behaviors = ?, base_branch = ?, fork_point = ?, default_task_behavior = ?,
			updated_at = ?
		WHERE slug = ? AND updated_at = ?
	`,
		cols.ContainerImage, cols.HostCommandsJSON, cols.EnvJSON, cols.AllowedDomainsJSON,
		cols.ExtraReposJSON, cols.ServicesJSON, cols.CapabilitiesJSON, cols.BindingsJSON,
		cols.TaskBehaviorsYAML, cols.BaseBranch, cols.ForkPoint, cols.DefaultTaskBehavior,
		newUpdatedAt,
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

// workspaceMetaColumns bundles the `workspaces` table's column values shared
// by decodeWorkspaceMetaColumns/marshalWorkspaceMetaColumns
// (docs/plans/refactoring-backlog.md N10A): the pre-struct signatures were
// 13 positional arguments (decode) / 13 positional return values (marshal),
// all-but-one an unnamed string, with the error-path `return "", "", ...,
// err` boilerplate repeated 8 times across marshal's early-return branches.
// A single named struct removes both the transposition risk and the
// boilerplate.
//
// ContainerImage is sql.NullString (not a plain string) on both the decode
// and marshal sides: sql.NullString implements driver.Valuer, so
// marshalWorkspaceMetaColumns's result can be passed directly as an Exec/
// QueryRow argument (NULL when meta.ContainerImage is empty, the string
// itself otherwise) without a separate `any` conversion step — see
// marshalWorkspaceMetaColumns's own doc comment for why this mirrors the
// original tuple-returning signature's containerImage any exactly.
type workspaceMetaColumns struct {
	ContainerImage      sql.NullString
	HostCommandsJSON    string
	EnvJSON             string
	AllowedDomainsJSON  string
	ExtraReposJSON      string
	ServicesJSON        string
	CapabilitiesJSON    string
	BindingsJSON        string
	TaskBehaviorsYAML   string
	BaseBranch          string
	ForkPoint           string
	DefaultTaskBehavior string
}

// decodeWorkspaceMetaColumns decodes the JSON column values shared by Load
// and LoadWithRevision into a *WorkspaceMeta, so the two share identical
// decode logic rather than letting it drift out of sync.
//
// cols.BindingsJSON (the `workspaces.additional_bindings` column) is decoded
// and discarded rather than mapped onto the result: WorkspaceMeta.AdditionalBindings
// was retired outright in docs/plans/home-workspace-volume.md Phase 4 PR4
// (see that struct's own doc comment). The column itself is not dropped by
// this PR's migration — a future major schema cleanup removes it — so a row
// written before this binary can still carry a non-empty JSON array here;
// decoding it (rather than ignoring the column) still validates it is
// well-formed JSON and lets this function warn when it is non-trivial, so an
// operator inspecting logs after an upgrade understands why a previously
// working workspace-scoped bind mount stopped applying.
func decodeWorkspaceMetaColumns(slug string, cols workspaceMetaColumns) (*WorkspaceMeta, error) {
	meta := &WorkspaceMeta{}
	if cols.ContainerImage.Valid {
		meta.ContainerImage = cols.ContainerImage.String
	}
	if err := json.Unmarshal([]byte(cols.HostCommandsJSON), &meta.HostCommands); err != nil {
		return nil, fmt.Errorf("workspace %q: decode host_commands: %w", slug, err)
	}
	if err := json.Unmarshal([]byte(cols.EnvJSON), &meta.Env); err != nil {
		return nil, fmt.Errorf("workspace %q: decode env: %w", slug, err)
	}
	if err := json.Unmarshal([]byte(cols.AllowedDomainsJSON), &meta.AllowedDomains); err != nil {
		return nil, fmt.Errorf("workspace %q: decode allowed_domains: %w", slug, err)
	}
	if err := json.Unmarshal([]byte(cols.ExtraReposJSON), &meta.ExtraRepos); err != nil {
		return nil, fmt.Errorf("workspace %q: decode extra_repos: %w", slug, err)
	}
	if err := json.Unmarshal([]byte(cols.ServicesJSON), &meta.Services); err != nil {
		return nil, fmt.Errorf("workspace %q: decode services: %w", slug, err)
	}
	if err := json.Unmarshal([]byte(cols.CapabilitiesJSON), &meta.Capabilities); err != nil {
		return nil, fmt.Errorf("workspace %q: decode capabilities: %w", slug, err)
	}
	var discardedBindings []BindMount
	if err := json.Unmarshal([]byte(cols.BindingsJSON), &discardedBindings); err != nil {
		return nil, fmt.Errorf("workspace %q: decode additional_bindings: %w", slug, err)
	}
	if len(discardedBindings) > 0 {
		slog.Warn("workspace: additional_bindings is no longer supported (retired in docs/plans/home-workspace-volume.md Phase 4 PR4); the stored value is ignored",
			"slug", slug, "count", len(discardedBindings))
	}
	// task_behaviors is stored as YAML, not JSON, unlike every column above
	// (docs/plans/workspace-default-project.md §PR分割案 PR3): TaskBehavior.
	// Hooks is `json:"-"` (spec_types.go — deliberately excluded from the
	// JSON API response shape), so encoding/json would silently drop hook
	// definitions on every round trip. yaml.v3 has a proper
	// `yaml:"hooks,omitempty"` tag on that same field.
	if strings.TrimSpace(cols.TaskBehaviorsYAML) != "" {
		var behaviors map[string]TaskBehavior
		if err := yaml.Unmarshal([]byte(cols.TaskBehaviorsYAML), &behaviors); err != nil {
			return nil, fmt.Errorf("workspace %q: decode task_behaviors: %w", slug, err)
		}
		meta.TaskBehaviors = behaviors
	}
	meta.BaseBranch = cols.BaseBranch
	meta.ForkPoint = cols.ForkPoint
	meta.DefaultTaskBehavior = cols.DefaultTaskBehavior
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

	cols, err := marshalWorkspaceMetaColumns(slug, meta)
	if err != nil {
		return err
	}

	if _, err := r.conn.Exec(`
		INSERT INTO workspaces (
			slug, container_image, host_commands, env, allowed_domains,
			extra_repos, services, capabilities, additional_bindings,
			task_behaviors, base_branch, fork_point, default_task_behavior,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		slug, cols.ContainerImage, cols.HostCommandsJSON, cols.EnvJSON, cols.AllowedDomainsJSON,
		cols.ExtraReposJSON, cols.ServicesJSON, cols.CapabilitiesJSON, cols.BindingsJSON,
		cols.TaskBehaviorsYAML, cols.BaseBranch, cols.ForkPoint, cols.DefaultTaskBehavior,
		nowForRevision(),
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

	cols, err := marshalWorkspaceMetaColumns(slug, meta)
	if err != nil {
		return err
	}

	updatedAt := nowForRevision()
	if _, err := dbtx.Exec(`
		INSERT INTO workspaces (
			slug, container_image, host_commands, env, allowed_domains,
			extra_repos, services, capabilities, additional_bindings,
			task_behaviors, base_branch, fork_point, default_task_behavior,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(slug) DO UPDATE SET
			container_image       = excluded.container_image,
			host_commands         = excluded.host_commands,
			env                   = excluded.env,
			allowed_domains       = excluded.allowed_domains,
			extra_repos           = excluded.extra_repos,
			services              = excluded.services,
			capabilities          = excluded.capabilities,
			additional_bindings   = excluded.additional_bindings,
			task_behaviors        = excluded.task_behaviors,
			base_branch           = excluded.base_branch,
			fork_point            = excluded.fork_point,
			default_task_behavior = excluded.default_task_behavior,
			updated_at            = excluded.updated_at
	`,
		slug, cols.ContainerImage, cols.HostCommandsJSON, cols.EnvJSON, cols.AllowedDomainsJSON,
		cols.ExtraReposJSON, cols.ServicesJSON, cols.CapabilitiesJSON, cols.BindingsJSON,
		cols.TaskBehaviorsYAML, cols.BaseBranch, cols.ForkPoint, cols.DefaultTaskBehavior,
		updatedAt,
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

// marshalWorkspaceMetaColumns encodes meta's fields into the column values
// shared by saveWorkspaceRow's upsert and WorkspaceRepository.Create's
// insert-only path, so the two statements can never drift out of sync on how
// a given field is serialized. The returned workspaceMetaColumns.ContainerImage
// is sql.NullString (a driver.Valuer) so it can be passed straight to Exec:
// NULL when meta.ContainerImage is empty, or the string itself otherwise —
// see workspaceMetaColumns's own doc comment.
//
// The returned BindingsJSON (the `workspaces.additional_bindings` column) is
// always the empty-array literal: WorkspaceMeta has no AdditionalBindings
// field any more (Phase 4 PR4, docs/plans/home-workspace-volume.md — see that
// struct's doc comment) to source a value from, so every Save/Create/Update
// from this binary zeroes out whatever a previous binary may have stored
// there. The column itself is kept for now (a future major schema cleanup
// removes it outright); see decodeWorkspaceMetaColumns for the read side.
//
// The returned TaskBehaviorsYAML runs meta.TaskBehaviors through
// validateWorkspaceDefaultTaskBehaviors before encoding (docs/plans/
// workspace-default-project.md 決定4, 論点j's "DB save" entry point — the
// other of the two mandated entry points, alongside envelope decode in
// decodeWorkspaceEnvelopeSpec) — this is what makes a WorkspaceMeta written
// by ANY path (not just the envelope apply path) end up with validated,
// canonical-only task_behaviors (alias names renamed to their canonical
// form, no back-compat mirror entries added — see
// validateWorkspaceDefaultTaskBehaviors's own doc comment for why a mirror
// must never reach persisted storage), since this function is the single
// choke point every write (Create/Save/UpdateIfRevisionMatches) funnels
// through. Stored as YAML, not JSON, unlike every other column here — see
// decodeWorkspaceMetaColumns's matching comment for why (TaskBehavior.Hooks
// is `json:"-"`).
func marshalWorkspaceMetaColumns(slug string, meta *WorkspaceMeta) (workspaceMetaColumns, error) {
	var cols workspaceMetaColumns
	var err error

	cols.HostCommandsJSON, err = marshalJSONOrDefault(meta.HostCommands, len(meta.HostCommands) == 0, "[]")
	if err != nil {
		return workspaceMetaColumns{}, fmt.Errorf("workspace %q: encode host_commands: %w", slug, err)
	}
	cols.EnvJSON, err = marshalJSONOrDefault(meta.Env, len(meta.Env) == 0, "{}")
	if err != nil {
		return workspaceMetaColumns{}, fmt.Errorf("workspace %q: encode env: %w", slug, err)
	}
	cols.AllowedDomainsJSON, err = marshalJSONOrDefault(meta.AllowedDomains, len(meta.AllowedDomains) == 0, "[]")
	if err != nil {
		return workspaceMetaColumns{}, fmt.Errorf("workspace %q: encode allowed_domains: %w", slug, err)
	}
	cols.ExtraReposJSON, err = marshalJSONOrDefault(meta.ExtraRepos, len(meta.ExtraRepos) == 0, "[]")
	if err != nil {
		return workspaceMetaColumns{}, fmt.Errorf("workspace %q: encode extra_repos: %w", slug, err)
	}
	cols.ServicesJSON, err = marshalJSONOrDefault(meta.Services, len(meta.Services) == 0, "[]")
	if err != nil {
		return workspaceMetaColumns{}, fmt.Errorf("workspace %q: encode services: %w", slug, err)
	}
	cols.BindingsJSON = "[]"
	capabilitiesBytes, err := json.Marshal(meta.Capabilities)
	if err != nil {
		return workspaceMetaColumns{}, fmt.Errorf("workspace %q: encode capabilities: %w", slug, err)
	}
	cols.CapabilitiesJSON = string(capabilitiesBytes)

	if meta.ContainerImage != "" {
		cols.ContainerImage = sql.NullString{String: meta.ContainerImage, Valid: true}
	}

	normalized, err := validateWorkspaceDefaultTaskBehaviors(fmt.Sprintf("workspace %q", slug), meta.TaskBehaviors)
	if err != nil {
		return workspaceMetaColumns{}, err
	}
	if len(normalized) > 0 {
		behaviorsBytes, err := yaml.Marshal(normalized)
		if err != nil {
			return workspaceMetaColumns{}, fmt.Errorf("workspace %q: encode task_behaviors: %w", slug, err)
		}
		cols.TaskBehaviorsYAML = string(behaviorsBytes)
	}
	cols.BaseBranch = meta.BaseBranch
	cols.ForkPoint = meta.ForkPoint
	cols.DefaultTaskBehavior = meta.DefaultTaskBehavior

	return cols, nil
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
