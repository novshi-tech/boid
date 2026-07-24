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
	// of the snapshot transaction.
	ProjectIDs []string
	// Projects maps each of ProjectIDs to its full *Project row (id,
	// work_dir, upstream_url, ...), read from the SAME transaction as
	// ProjectIDs itself (PR-1d codex round-2 Major: the caller previously
	// re-queried each project ID with a separate, post-transaction
	// GetProject call whose error was silently skipped — a concurrent
	// delete landing in that window could produce a "consistent snapshot"
	// export that was quietly missing an assignment). Every ID in
	// ProjectIDs is guaranteed to have an entry here (project_workspaces.
	// project_id REFERENCES projects(id), read within one transaction), so
	// a caller indexing this map by an id drawn from ProjectIDs never needs
	// an "ok" check for correctness — Display name/url resolution (project.
	// yaml's name: field) still happens outside this transaction, from the
	// in-memory ProjectMeta cache, which is not DB-backed at all — see
	// SnapshotWorkspacesForExport's doc comment.
	Projects map[string]*Project
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
// Every assigned project's full row (WorkspaceExportSnapshot.Projects) is
// ALSO read from this same transaction (PR-1d codex round-2 Blocker 3
// follow-up) — the id/work_dir/upstream_url/workspace_id columns all live
// in the same `projects` table this function already reads via
// ListProjects, so there is no reason to defer that read to a second,
// separately-fallible, post-transaction lookup per project the way the
// round-1 version did. Only the project's DISPLAY NAME
// (WorkspaceEnvelopeProject.Name) is deliberately NOT part of this
// snapshot: it comes from the in-memory ProjectMeta cache (project.yaml's
// name: field), not a DB column at all — see
// ProjectAppService.ExportWorkspaceEnvelopes, which resolves that name
// afterward. Doing that resolution outside this transaction cannot
// reintroduce the cross-workspace inconsistency this function exists to
// close: it cannot change which workspace_id a project_id is assigned to,
// only how that already-fixed assignment is displayed.
// allProjects (PR-1d codex round-3 regression fix) is EVERY registered
// project row -- not just ones assigned to a slug in slugs -- read from the
// exact same transaction as everything else this function returns, keyed by
// ID. ExportWorkspaceEnvelopes needs the full, tx-consistent project set (not
// just the exported workspaces' assignments) to detect a project.yaml name
// colliding with ANOTHER project's name or ID before emitting a
// spec.projects[] entry a later apply could misresolve. Before this fix,
// that collision check ran its own separate, post-snapshot ListProjects()
// call, which could describe a different instant than the export snapshot
// itself under concurrent project create/delete.
func SnapshotWorkspacesForExport(conn *sql.DB, slugs []string) (snapshots []*WorkspaceExportSnapshot, allProjects map[string]*Project, err error) {
	tx, err := conn.Begin()
	if err != nil {
		return nil, nil, fmt.Errorf("snapshot workspaces for export: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // read-only; never committed

	targetSlugs := slugs
	if len(targetSlugs) == 0 {
		targetSlugs, err = listWorkspaceSlugs(tx)
		if err != nil {
			return nil, nil, fmt.Errorf("snapshot workspaces for export: %w", err)
		}
	}

	byWorkspace, projectsByID, err := workspaceProjectAssignmentsByWorkspace(tx)
	if err != nil {
		return nil, nil, fmt.Errorf("snapshot workspaces for export: %w", err)
	}

	out := make([]*WorkspaceExportSnapshot, 0, len(targetSlugs))
	for _, slug := range targetSlugs {
		meta, revision, err := loadWorkspaceMetaWithRevision(tx, slug)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil, fmt.Errorf("workspace %q: %w", slug, os.ErrNotExist)
			}
			return nil, nil, fmt.Errorf("snapshot workspace %q: %w", slug, err)
		}
		ids := byWorkspace[slug]
		projects := make(map[string]*Project, len(ids))
		for _, id := range ids {
			projects[id] = projectsByID[id]
		}
		out = append(out, &WorkspaceExportSnapshot{
			Slug:       slug,
			Meta:       meta,
			Revision:   revision,
			ProjectIDs: ids,
			Projects:   projects,
		})
	}
	return out, projectsByID, nil
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

// workspaceProjectAssignmentsByWorkspace reads every projects row ONCE, in
// tx, via ListProjects — a single query instead of one per workspace
// (avoiding N+1) — and indexes it both by workspace_id (for
// WorkspaceExportSnapshot.ProjectIDs) and by project id (for
// WorkspaceExportSnapshot.Projects). Doing this via ListProjects rather than
// a bare `SELECT workspace_id, project_id FROM project_workspaces` (the
// pre-round-2 query) is what closes Blocker 3 (codex round-2): the full
// *Project row for every assigned project now comes from this SAME
// transaction, so ExportWorkspaceEnvelopes no longer needs a second,
// post-transaction GetProject lookup per project — the exact call whose
// silently-skipped errors could previously drop an assignment from an
// otherwise-"consistent" snapshot.
func workspaceProjectAssignmentsByWorkspace(tx *sql.Tx) (byWorkspace map[string][]string, projectsByID map[string]*Project, err error) {
	projects, err := ListProjects(tx)
	if err != nil {
		return nil, nil, fmt.Errorf("list projects: %w", err)
	}
	byWorkspace = map[string][]string{}
	projectsByID = make(map[string]*Project, len(projects))
	for _, p := range projects {
		projectsByID[p.ID] = p
		if p.WorkspaceID != "" {
			byWorkspace[p.WorkspaceID] = append(byWorkspace[p.WorkspaceID], p.ID)
		}
	}
	return byWorkspace, projectsByID, nil
}
