package orchestrator

import (
	"database/sql"
	"errors"
	"fmt"
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

// SetProjectUpstreamURL updates a project's captured upstream_url (see
// docs/plans/git-gateway-cutover.md PR2: project → upstream URL mapping).
// Used by `project add` / `project reload` capture and the daemon-startup
// backfill for projects registered before this column existed.
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
// workspaces table (MAJOR 5, codex review: SetProjectWorkspace previously
// assigned any syntactically valid slug without checking it actually
// existed, leaving a dangling project_workspaces reference — dispatch then
// runs in a permanently degraded window, and since
// workspace_db_consolidation's state=committed makes MigrateWorkspaceYAMLToDB
// a permanent no-op, no later startup ever re-validates and self-heals it).
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

// ListWorkspaces returns all configured workspaces with project counts.
func ListWorkspaces(dbtx db.DBTX) ([]*WorkspaceSummary, error) {
	rows, err := dbtx.Query(
		`SELECT workspace_id, COUNT(*)
		 FROM project_workspaces
		 GROUP BY workspace_id
		 ORDER BY workspace_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	defer rows.Close()

	var workspaces []*WorkspaceSummary
	for rows.Next() {
		var workspace WorkspaceSummary
		if err := rows.Scan(&workspace.ID, &workspace.ProjectCount); err != nil {
			return nil, fmt.Errorf("scan workspace: %w", err)
		}
		workspaces = append(workspaces, &workspace)
	}
	return workspaces, rows.Err()
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
// captured. This is the "既存 project の...欠落 project は...dispatch 時エラー"
// building block described in docs/plans/git-gateway-cutover.md's
// "本計画で確定する設計 § 1" — it is intentionally NOT wired into any dispatch
// path yet. Wiring it in now would reject every current e2e project fixture
// (none has a real git remote until PR7a's fixture-upstream-server harness
// lands) ahead of the plan's own PR ordering (PR2 → ... → PR7a → PR6). It is
// exposed and tested here so PR6 (cutover, where dispatch starts needing
// upstream_url to build the gateway clone URL) has a ready-made, already
// covered building block to call.
func RequireUpstreamURL(project *Project) error {
	if project == nil {
		return fmt.Errorf("require upstream_url: project is nil")
	}
	if project.UpstreamURL == "" {
		return fmt.Errorf("project %q has no upstream_url configured; add a git remote (git remote add origin <url>) and run `boid project reload`", project.ID)
	}
	return nil
}
