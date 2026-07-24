package orchestrator

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
)

// WorkspaceExportSnapshot is one workspace's meta + assigned project IDs,
// read from a single atomic DB transaction alongside every other
// workspace's snapshot in the same call to SnapshotWorkspacesForExport.
type WorkspaceExportSnapshot struct {
	Slug     string
	Meta     *WorkspaceMeta
	Revision string
	// ProjectIDs is the list of project IDs assigned to this workspace as
	// of the snapshot transaction. Names/URLs are resolved by the caller
	// afterward — see SnapshotWorkspacesForExport's doc comment.
	ProjectIDs []string
}

// SnapshotWorkspacesForExport reads meta + project assignment for every
// slug in slugs (or every workspace when slugs is nil/empty) from a SINGLE
// read-only DB transaction (docs/plans/volume-only-daemon.md PR-1d codex
// round-1 Blocker 3): `boid workspace export`'s previous per-workspace
// "GET meta, then GET its projects" loop (cmd/workspace_export.go's old
// exportWorkspaceEnvelopeYAML, called once per workspace) could straddle a
// concurrent `workspace assign` moving a project between two workspaces
// mid-export — the project could then appear in neither exported document,
// or (less likely but still possible) in both. Reading every workspaces row
// and the entire project_workspaces table inside one transaction closes
// that window: every project_id this call reports is assigned to exactly
// one workspace_id, as of one consistent instant — project_workspaces.
// project_id is the table's PRIMARY KEY (0001_initial.sql), so a project
// can never appear against two different workspace_id values in the same
// read.
//
// Project display names/URLs are deliberately NOT part of this snapshot: a
// project's name (WorkspaceEnvelopeProject.Name) comes from the in-memory
// ProjectMeta cache (project.yaml's name: field), not a DB column this
// transaction could read — see ProjectAppService.ExportWorkspaceEnvelopes,
// which resolves id -> name/url afterward. Doing that resolution outside
// this transaction cannot reintroduce the cross-workspace inconsistency
// this function exists to close: it cannot change which workspace_id a
// project_id is assigned to, only how that already-fixed assignment is
// displayed.
func SnapshotWorkspacesForExport(conn *sql.DB, slugs []string) ([]*WorkspaceExportSnapshot, error) {
	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("snapshot workspaces for export: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // read-only; never committed

	targetSlugs := slugs
	if len(targetSlugs) == 0 {
		targetSlugs, err = listWorkspaceSlugs(tx)
		if err != nil {
			return nil, fmt.Errorf("snapshot workspaces for export: %w", err)
		}
	}

	byWorkspace, err := workspaceProjectAssignmentsByWorkspace(tx)
	if err != nil {
		return nil, fmt.Errorf("snapshot workspaces for export: %w", err)
	}

	out := make([]*WorkspaceExportSnapshot, 0, len(targetSlugs))
	for _, slug := range targetSlugs {
		meta, revision, err := loadWorkspaceMetaWithRevision(tx, slug)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("workspace %q: %w", slug, os.ErrNotExist)
			}
			return nil, fmt.Errorf("snapshot workspace %q: %w", slug, err)
		}
		out = append(out, &WorkspaceExportSnapshot{
			Slug:       slug,
			Meta:       meta,
			Revision:   revision,
			ProjectIDs: byWorkspace[slug],
		})
	}
	return out, nil
}

// listWorkspaceSlugs returns every workspaces.slug value, sorted, read from
// tx (mirrors WorkspaceRepository.List, but dbtx-scoped so it can run
// inside SnapshotWorkspacesForExport's own transaction).
func listWorkspaceSlugs(tx *sql.Tx) ([]string, error) {
	rows, err := tx.Query(`SELECT slug FROM workspaces ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("list workspace slugs: %w", err)
	}
	defer rows.Close()
	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, fmt.Errorf("scan workspace slug: %w", err)
		}
		slugs = append(slugs, slug)
	}
	return slugs, rows.Err()
}

// workspaceProjectAssignmentsByWorkspace reads the ENTIRE project_workspaces
// table once, in tx, and indexes it by workspace_id — a single query
// instead of one per workspace (avoiding N+1), and — more importantly —
// guaranteeing every workspace's project list in the same
// SnapshotWorkspacesForExport call is read from the identical DB snapshot.
func workspaceProjectAssignmentsByWorkspace(tx *sql.Tx) (map[string][]string, error) {
	rows, err := tx.Query(`SELECT workspace_id, project_id FROM project_workspaces ORDER BY project_id`)
	if err != nil {
		return nil, fmt.Errorf("list project assignments: %w", err)
	}
	defer rows.Close()
	byWorkspace := map[string][]string{}
	for rows.Next() {
		var workspaceID, projectID string
		if err := rows.Scan(&workspaceID, &projectID); err != nil {
			return nil, fmt.Errorf("scan project assignment: %w", err)
		}
		byWorkspace[workspaceID] = append(byWorkspace[workspaceID], projectID)
	}
	return byWorkspace, rows.Err()
}
