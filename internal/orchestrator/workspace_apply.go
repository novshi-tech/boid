package orchestrator

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
)

// WorkspaceApplyResult reports what ApplyWorkspaceEnvelope did (or, for
// dryRun=true, would do) for one workspace envelope document.
type WorkspaceApplyResult struct {
	Slug string `json:"slug"`
	// Created is true when slug had no workspaces row before this call.
	Created bool `json:"created"`
	// Previous is the workspace's meta before this apply (nil when
	// Created).
	Previous *WorkspaceMeta `json:"previous,omitempty"`
	// Meta is the merged/proposed meta this apply wrote (or, for dryRun,
	// would have written).
	Meta *WorkspaceMeta `json:"meta"`
	// Revision is the workspace's row revision after a committed apply.
	// Empty for a dry run (nothing was committed, so there is no new
	// revision to report).
	Revision string `json:"revision,omitempty"`

	// AttachedProjects/DetachedProjects/AlreadyAttachedProjects are project
	// IDs (not names — see ApplyWorkspaceEnvelope's projectIDsByName
	// parameter doc comment for why name resolution happens outside this
	// function). AttachedProjects were newly assigned to Slug by this call;
	// AlreadyAttachedProjects were already assigned to Slug and left alone;
	// DetachedProjects were previously assigned to Slug but absent from the
	// applied document's spec.projects[] list, so they were reassigned to
	// DefaultWorkspaceSlug: FieldsPresent["projects"] is honored as "replace
	// the whole assignment set", not just "attach anything additionally
	// listed".
	AttachedProjects        []string `json:"attached_projects,omitempty"`
	AlreadyAttachedProjects []string `json:"already_attached_projects,omitempty"`
	DetachedProjects        []string `json:"detached_projects,omitempty"`

	// MissingProjects is filled in by the caller (ProjectAppService.
	// ApplyWorkspace, internal/api/project_service.go) — a spec.projects[]
	// name that did not resolve to a registered project. Left empty by
	// ApplyWorkspaceEnvelope itself, which only ever sees already-resolved
	// IDs (projectIDsByName).
	MissingProjects []string `json:"missing_projects,omitempty"`

	// InitScriptAction reports what this apply did — or, for a dry run, would
	// do — to the workspace's init.sh: "written", "cleared", "unchanged", or
	// empty when the document carried no spec.init_script key at all.
	//
	// Also filled in by the caller, and for a stronger reason than
	// MissingProjects: init.sh is a FILE on the daemon, not a row this
	// function's transaction could touch (WorkspaceEnvelopeSpec.InitScript).
	// It is reported separately from Meta for the same reason — a reader of
	// this result must be able to see that the script and the metadata are
	// two commits, not one.
	InitScriptAction string `json:"init_script_action,omitempty"`

	// Warnings is filled in by the caller (WorkspaceHandler.Apply,
	// internal/api/workspace.go) — non-fatal notices about the applied
	// document that a direct HTTP caller (not going through `boid workspace
	// apply`, which already warns client-side) would otherwise never see,
	// e.g. a retired spec.additional_bindings key that was silently dropped.
	// Left empty by ApplyWorkspaceEnvelope itself.
	Warnings []string `json:"warnings,omitempty"`
}

// ApplyWorkspaceEnvelope upserts apply's workspace metadata and (when
// apply.FieldsPresent["projects"] is true) reconciles its project
// attach/detach bookkeeping, all in a SINGLE database transaction: either
// every change this call makes commits, or none of them do. This replaces
// a previous client-side "GET current, merge, PUT metadata, then separately
// attach each project one HTTP call at a time" dance, which had no way to
// roll back the metadata write if a later project assignment failed.
//
// projectIDsByName maps each spec.projects[].name entry that DID resolve
// to a registered project (via ProjectAppService.ResolveProjectRef) to that
// project's ID. A name present in apply.Envelope.Spec.Projects but absent
// from projectIDsByName is simply not attached/reconciled here — the
// caller surfaces that as a non-fatal warning (WorkspaceApplyResult.
// MissingProjects), per the plan doc's cross-PR coordination note that a
// not-yet-registered project must never fail apply. Resolving names to IDs
// is deliberately done by the caller, OUTSIDE this transaction: a
// project's display name lives in the in-memory ProjectMeta cache
// (hydrated from project.yaml on disk), not a DB column this transaction
// could query — see ProjectAppService.resolveWorkspaceApplyProjectNames.
//
// dryRun runs every read and write statement below — including the
// workspace upsert and the project_workspaces mutations — against a real
// transaction, then rolls back instead of committing. This means a dry run
// exercises the exact same validation/constraint path a real apply would,
// rather than stopping short of touching the DB and reporting success on a
// document a real apply would go on to reject.
func ApplyWorkspaceEnvelope(conn *sql.DB, apply *WorkspaceEnvelopeApply, projectIDsByName map[string]string, dryRun bool) (*WorkspaceApplyResult, error) {
	slug := apply.Envelope.Metadata.Name
	if err := ValidWorkspaceSlug(slug); err != nil {
		return nil, err
	}

	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("apply workspace %q: begin tx: %w", slug, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	current, _, loadErr := loadWorkspaceMetaWithRevision(tx, slug)
	created := false
	if loadErr != nil {
		if !errors.Is(loadErr, os.ErrNotExist) {
			return nil, fmt.Errorf("apply workspace %q: load current: %w", slug, loadErr)
		}
		created = true
		current = nil
	}

	merged := apply.MergeInto(current)
	if err := saveWorkspaceRow(tx, slug, merged); err != nil {
		return nil, fmt.Errorf("apply workspace %q: save: %w", slug, err)
	}

	_, newRevision, err := loadWorkspaceMetaWithRevision(tx, slug)
	if err != nil {
		return nil, fmt.Errorf("apply workspace %q: reload after save: %w", slug, err)
	}

	result := &WorkspaceApplyResult{
		Slug:     slug,
		Created:  created,
		Previous: current,
		Meta:     merged,
	}

	if apply.FieldsPresent["projects"] {
		attached, already, detached, err := applyWorkspaceProjectAssignments(tx, slug, projectIDsByName)
		if err != nil {
			return nil, fmt.Errorf("apply workspace %q: projects: %w", slug, err)
		}
		result.AttachedProjects = attached
		result.AlreadyAttachedProjects = already
		result.DetachedProjects = detached
	}

	if dryRun {
		// newRevision above was read from a row this same transaction is
		// about to roll back — it was never actually committed, so
		// reporting it would hand the caller an ETag for a revision that
		// does not exist in the DB. Leave Revision at its zero value,
		// honoring the field's own "empty for dry run" doc comment.
		return result, nil
	}
	result.Revision = newRevision
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("apply workspace %q: commit: %w", slug, err)
	}
	committed = true
	return result, nil
}

// applyWorkspaceProjectAssignments reconciles project_workspaces membership
// for slug against targetIDsByName's values: every target ID is attached
// (or left alone if already attached), and every project currently
// attached to slug but absent from the target set is detached — reassigned
// to DefaultWorkspaceSlug, mirroring WorkspaceRepository.Remove's own
// reassign-to-default precedent.
func applyWorkspaceProjectAssignments(tx *sql.Tx, slug string, targetIDsByName map[string]string) (attached, already, detached []string, err error) {
	current, err := currentWorkspaceProjectIDs(tx, slug)
	if err != nil {
		return nil, nil, nil, err
	}
	currentSet := make(map[string]bool, len(current))
	for _, id := range current {
		currentSet[id] = true
	}

	targetSet := make(map[string]bool, len(targetIDsByName))
	for _, id := range targetIDsByName {
		if targetSet[id] {
			continue // the same project resolved from two spec.projects[] entries; nothing new to do.
		}
		targetSet[id] = true
		if currentSet[id] {
			already = append(already, id)
			continue
		}
		if err := SetProjectWorkspace(tx, id, slug); err != nil {
			return nil, nil, nil, fmt.Errorf("attach project %q: %w", id, err)
		}
		attached = append(attached, id)
	}

	for _, id := range current {
		if targetSet[id] {
			continue
		}
		if err := SetProjectWorkspace(tx, id, DefaultWorkspaceSlug); err != nil {
			return nil, nil, nil, fmt.Errorf("detach project %q: %w", id, err)
		}
		detached = append(detached, id)
	}

	return attached, already, detached, nil
}

// currentWorkspaceProjectIDs returns every project_id currently assigned to
// slug (project_workspaces.workspace_id = slug), read from tx so it
// reflects the same in-flight transaction ApplyWorkspaceEnvelope is about
// to mutate.
func currentWorkspaceProjectIDs(tx *sql.Tx, slug string) ([]string, error) {
	rows, err := tx.Query(`SELECT project_id FROM project_workspaces WHERE workspace_id = ?`, slug)
	if err != nil {
		return nil, fmt.Errorf("list current project assignments: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan project assignment: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
