package orchestrator

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/novshi-tech/boid/internal/db"
)

type projectScanner interface {
	Scan(dest ...any) error
}

// CreateProject inserts a new project record.
func CreateProject(dbtx db.DBTX, project *Project) error {
	now := time.Now().UTC()
	project.CreatedAt = now
	project.UpdatedAt = now
	_, err := dbtx.Exec(
		`INSERT INTO projects (id, work_dir, upstream_url, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		project.ID, project.WorkDir, nullableString(project.UpstreamURL), project.CreatedAt, project.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert project: %w", err)
	}
	return nil
}

// SetProjectUpstreamURL updates a project's captured upstream_url. Used by
// `project add` / `project reload` capture and the daemon-startup backfill
// for projects registered before this column existed.
func SetProjectUpstreamURL(dbtx db.DBTX, id, upstreamURL string) error {
	now := time.Now().UTC()
	res, err := dbtx.Exec(
		`UPDATE projects SET upstream_url = ?, updated_at = ? WHERE id = ?`,
		nullableString(upstreamURL), now, id,
	)
	if err != nil {
		return fmt.Errorf("set project upstream_url: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set project upstream_url: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("set project upstream_url: project %q not found", id)
	}
	return nil
}

// nullableString maps an empty string to SQL NULL so upstream_url reads back
// as "" (via scanProject's sql.NullString handling) rather than storing an
// empty string literal that would be indistinguishable from NULL in intent.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// GetProject retrieves a project by ID.
func GetProject(dbtx db.DBTX, id string) (*Project, error) {
	row := dbtx.QueryRow(
		`SELECT p.id, p.work_dir, pw.workspace_id, p.upstream_url, p.created_at, p.updated_at
		 FROM projects p
		 LEFT JOIN project_workspaces pw ON pw.project_id = p.id
		 WHERE p.id = ?`, id,
	)
	return scanProject(row)
}

// ListProjects returns all projects ordered by creation time.
func ListProjects(dbtx db.DBTX) ([]*Project, error) {
	rows, err := dbtx.Query(
		`SELECT p.id, p.work_dir, pw.workspace_id, p.upstream_url, p.created_at, p.updated_at
		 FROM projects p
		 LEFT JOIN project_workspaces pw ON pw.project_id = p.id
		 ORDER BY p.created_at`,
	)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	return scanProjects(rows)
}

// SetProjectWorkspace updates a project's local workspace membership.
//
// Domain-layer validation per plan (3-layer defense, last line before DB
// INSERT). Empty workspaceID clears the membership and bypasses slug
// validation; any non-empty slug must satisfy ValidWorkspaceSlug so we never
// persist a malformed identifier even if an upstream layer forgets to check.
func SetProjectWorkspace(dbtx db.DBTX, projectID, workspaceID string) error {
	if workspaceID == "" {
		if _, err := dbtx.Exec(`DELETE FROM project_workspaces WHERE project_id = ?`, projectID); err != nil {
			return fmt.Errorf("clear project workspace: %w", err)
		}
		return nil
	}

	if err := ValidWorkspaceSlug(workspaceID); err != nil {
		return fmt.Errorf("set project workspace: %w", err)
	}

	_, err := dbtx.Exec(
		`INSERT INTO project_workspaces (project_id, workspace_id) VALUES (?, ?)
		 ON CONFLICT(project_id) DO UPDATE SET workspace_id = excluded.workspace_id`,
		projectID, workspaceID,
	)
	if err != nil {
		return fmt.Errorf("set project workspace: %w", err)
	}
	return nil
}

// WorkspaceExists reports whether slug has a corresponding row in the
// workspaces table — used to guard against assigning a project to a slug
// that has no backing workspace row, which would leave a dangling
// project_workspaces reference nothing later self-heals.
func WorkspaceExists(dbtx db.DBTX, slug string) (bool, error) {
	var exists int
	err := dbtx.QueryRow(`SELECT 1 FROM workspaces WHERE slug = ? LIMIT 1`, slug).Scan(&exists)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("check workspace exists: %w", err)
	}
	return true, nil
}

// AssignWorkspaceIfExists atomically checks that workspaceID has a
// corresponding workspaces row and, if so, assigns projectID to it — all in
// a single transaction, so a DELETE landing between a separate existence
// check and the assign cannot leave a dangling project_workspaces
// reference. conn must be a *sql.DB (a fresh transaction is opened
// internally) — passing an existing *sql.Tx would attempt a nested BEGIN,
// which SQLite does not support the way this function needs.
//
// workspaceID == "" clears the assignment (bypassing the existence check —
// there is no slug to check), and DefaultWorkspaceSlug is exempt from the
// check (WorkspaceRepository.EnsureDefault guarantees it always exists),
// mirroring SetProjectWorkspace's own exemptions.
func AssignWorkspaceIfExists(conn *sql.DB, projectID, workspaceID string) error {
	if workspaceID == "" || workspaceID == DefaultWorkspaceSlug {
		return SetProjectWorkspace(conn, projectID, workspaceID)
	}
	if err := ValidWorkspaceSlug(workspaceID); err != nil {
		return fmt.Errorf("assign workspace if exists: %w", err)
	}

	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("assign workspace if exists: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	exists, err := WorkspaceExists(tx, workspaceID)
	if err != nil {
		return fmt.Errorf("assign workspace if exists: %w", err)
	}
	if !exists {
		return fmt.Errorf("workspace %q: %w", workspaceID, os.ErrNotExist)
	}
	if err := SetProjectWorkspace(tx, projectID, workspaceID); err != nil {
		return fmt.Errorf("assign workspace if exists: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("assign workspace if exists: commit: %w", err)
	}
	return nil
}

// AssignDefaultWorkspaceToUnlinked inserts a project_workspaces row pointing
// at workspaceID for every project that does not yet have one. Used at daemon
// startup to migrate legacy unlinked projects to the default workspace. The
// INSERT ... SELECT pattern keeps the operation idempotent and atomic in a
// single statement.
//
// Returns (number of rows inserted, error). Pass the DefaultWorkspaceSlug
// to land projects in the implicit default workspace.
func AssignDefaultWorkspaceToUnlinked(dbtx db.DBTX, workspaceID string) (int, error) {
	if workspaceID == "" {
		return 0, fmt.Errorf("assign default workspace: workspaceID is empty")
	}
	if err := ValidWorkspaceSlug(workspaceID); err != nil {
		return 0, fmt.Errorf("assign default workspace: %w", err)
	}
	res, err := dbtx.Exec(
		`INSERT INTO project_workspaces (project_id, workspace_id)
		 SELECT p.id, ?
		 FROM projects p
		 LEFT JOIN project_workspaces pw ON pw.project_id = p.id
		 WHERE pw.project_id IS NULL`,
		workspaceID,
	)
	if err != nil {
		return 0, fmt.Errorf("assign default workspace: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ListProjectWorkspaceReferences returns the distinct workspace_id values
// referenced by any project_workspaces row, with the count of projects
// referencing each. Unlike ListWorkspaces (workspaces-table-based), this
// reflects project_workspaces membership directly and surfaces a reference
// to a slug that has no corresponding workspaces row at all — needed by
// workspace_migration.go's preflight, which runs before the workspaces
// table has anything written to it and must detect a project referencing a
// slug with no legacy yaml file backing it. No other caller should need
// this function (dispatch and the API both go through ListWorkspaces).
func ListProjectWorkspaceReferences(dbtx db.DBTX) ([]*WorkspaceSummary, error) {
	rows, err := dbtx.Query(
		`SELECT workspace_id, COUNT(*)
		 FROM project_workspaces
		 GROUP BY workspace_id
		 ORDER BY workspace_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("list project workspace references: %w", err)
	}
	defer rows.Close()

	var workspaces []*WorkspaceSummary
	for rows.Next() {
		var workspace WorkspaceSummary
		if err := rows.Scan(&workspace.ID, &workspace.ProjectCount); err != nil {
			return nil, fmt.Errorf("scan workspace reference: %w", err)
		}
		workspaces = append(workspaces, &workspace)
	}
	return workspaces, rows.Err()
}

// ListWorkspaces returns every workspace known to the workspaces table, each
// annotated with its assigned project count and a Revision token. The query
// is workspaces-table-based with project_workspaces LEFT JOINed in, so a
// workspace with zero assigned projects still appears (ProjectCount=0).
func ListWorkspaces(dbtx db.DBTX) ([]*WorkspaceSummary, error) {
	rows, err := dbtx.Query(
		`SELECT w.slug, w.updated_at, COUNT(pw.project_id)
		 FROM workspaces w
		 LEFT JOIN project_workspaces pw ON pw.workspace_id = w.slug
		 GROUP BY w.slug
		 ORDER BY w.slug`,
	)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	defer rows.Close()

	var workspaces []*WorkspaceSummary
	for rows.Next() {
		var workspace WorkspaceSummary
		var updatedAt time.Time
		if err := rows.Scan(&workspace.ID, &updatedAt, &workspace.ProjectCount); err != nil {
			return nil, fmt.Errorf("scan workspace: %w", err)
		}
		workspace.Revision = updatedAt.UTC().Format(time.RFC3339Nano)
		workspaces = append(workspaces, &workspace)
	}
	return workspaces, rows.Err()
}

// GetWorkspaceSummary returns a single workspace's summary (project count +
// revision), or an error wrapping os.ErrNotExist when slug has no
// corresponding workspaces row. Used by the workspace API handlers to build
// the create/show/update response and to read the current revision for the
// PUT If-Match check.
func GetWorkspaceSummary(dbtx db.DBTX, slug string) (*WorkspaceSummary, error) {
	row := dbtx.QueryRow(
		`SELECT w.slug, w.updated_at, COUNT(pw.project_id)
		 FROM workspaces w
		 LEFT JOIN project_workspaces pw ON pw.workspace_id = w.slug
		 WHERE w.slug = ?
		 GROUP BY w.slug`,
		slug,
	)
	var summary WorkspaceSummary
	var updatedAt time.Time
	if err := row.Scan(&summary.ID, &updatedAt, &summary.ProjectCount); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("workspace %q: %w", slug, os.ErrNotExist)
		}
		return nil, fmt.Errorf("get workspace summary %q: %w", slug, err)
	}
	summary.Revision = updatedAt.UTC().Format(time.RFC3339Nano)
	return &summary, nil
}

// DeleteProject removes a project by ID.
// All tasks (and their dependent records) belonging to the project are deleted first.
// Standalone jobs (task_id NULL session / hook) are also swept by project_id so
// the jobs.project_id FK constraint does not refuse the project delete.
func DeleteProject(dbtx db.DBTX, id string) error {
	tasks, err := ListTasks(dbtx, TaskFilter{ProjectID: id})
	if err != nil {
		return fmt.Errorf("list tasks for project: %w", err)
	}
	for _, t := range tasks {
		if err := DeleteTask(dbtx, t.ID); err != nil {
			return fmt.Errorf("delete task %s: %w", t.ID, err)
		}
	}
	// task に紐付かない jobs (task_id NULL の session / standalone hook) を
	// 削除しないと jobs.project_id の FK 制約で project 削除が失敗する。
	// task 紐付きは上の DeleteTask で既に消えているが、 念のため
	// project_id ベースで一括削除する (二重削除は冪等)。
	if _, err := dbtx.Exec(`DELETE FROM jobs WHERE project_id = ?`, id); err != nil {
		return fmt.Errorf("delete project jobs: %w", err)
	}
	res, err := dbtx.Exec(`DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("project %q not found", id)
	}
	return nil
}

func scanProject(scanner projectScanner) (*Project, error) {
	var project Project
	var workspaceID sql.NullString
	var upstreamURL sql.NullString
	if err := scanner.Scan(&project.ID, &project.WorkDir, &workspaceID, &upstreamURL, &project.CreatedAt, &project.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("project not found")
		}
		return nil, fmt.Errorf("scan project: %w", err)
	}
	if workspaceID.Valid {
		project.WorkspaceID = workspaceID.String
	}
	if upstreamURL.Valid {
		project.UpstreamURL = upstreamURL.String
	}
	return &project, nil
}

func scanProjects(rows *sql.Rows) ([]*Project, error) {
	var projects []*Project
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	return projects, rows.Err()
}

// RequireUpstreamURL returns an error when project has no upstream_url
// captured. Intentionally NOT wired into any dispatch path yet — exposed
// and tested here as a ready-made building block for the future cutover
// where dispatch starts needing upstream_url to build the gateway clone
// URL.
func RequireUpstreamURL(project *Project) error {
	if project == nil {
		return fmt.Errorf("require upstream_url: project is nil")
	}
	if project.UpstreamURL == "" {
		return fmt.Errorf("project %q has no upstream_url configured; add a git remote (git remote add origin <url>) and run `boid project reload`", project.ID)
	}
	return nil
}
