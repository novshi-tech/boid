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
	// ProjectIDs itself. Every ID in ProjectIDs is guaranteed to have an
	// entry here (project_workspaces.project_id REFERENCES projects(id),
	// read within one transaction), so a caller indexing this map by an id
	// drawn from ProjectIDs never needs an "ok" check for correctness —
	// display name/url resolution still happens outside this transaction,
	// from the in-memory ProjectMeta cache, which is not DB-backed at all.
	Projects map[string]*Project
}

// SnapshotWorkspacesForExport reads meta + project assignment for every
// slug in slugs (or every workspace when slugs is nil/empty) from a single
// read-only DB transaction, so a concurrent `workspace assign` moving a
// project between two workspaces mid-export cannot make the project appear
// in neither (or both) exported documents. project_workspaces.project_id is
// the table's PRIMARY KEY, so a project can never appear against two
// different workspace_id values in the same read.
//
// Every assigned project's full row (WorkspaceExportSnapshot.Projects) is
// also read from this same transaction. Only the project's display name
// (WorkspaceEnvelopeProject.Name) is deliberately not part of this
// snapshot: it comes from the in-memory ProjectMeta cache (project.yaml's
// name: field), not a DB column — see ProjectAppService.
// ExportWorkspaceEnvelopes, which resolves that name afterward; doing so
// outside this transaction cannot reintroduce cross-workspace
// inconsistency, since it only changes how an already-fixed assignment is
// displayed.
//
// allProjects is EVERY registered project row — not just ones assigned to
// a slug in slugs — read from the exact same transaction, keyed by ID.
// ExportWorkspaceEnvelopes needs the full, tx-consistent project set to
// detect a project.yaml name colliding with another project's name or ID
// before emitting a spec.projects[] entry a later apply could misresolve.
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

// workspaceProjectAssignmentsByWorkspace reads every projects row once, in
// tx, via ListProjects — a single query instead of one per workspace
// (avoiding N+1) — and indexes it both by workspace_id (for
// WorkspaceExportSnapshot.ProjectIDs) and by project id (for
// WorkspaceExportSnapshot.Projects), so the full *Project row for every
// assigned project comes from this same transaction rather than a second,
// separately-fallible, post-transaction lookup per project.
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
