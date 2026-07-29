package orchestrator

import "sort"

// FieldProvenance classifies where a workspace-default-mergeable field's
// EFFECTIVE value came from (docs/plans/workspace-default-project.md 論点e).
type FieldProvenance string

const (
	// ProvenanceProjectYAML means project.yaml itself set a non-empty value
	// (or, for task_behaviors, defines this canonical behavior name).
	ProvenanceProjectYAML FieldProvenance = "project.yaml"
	// ProvenanceWorkspaceDefault means project.yaml left the field/key unset
	// and the workspace's default project definition supplied it (決定1/決定5).
	ProvenanceWorkspaceDefault FieldProvenance = "workspace default"
	// ProvenanceUnset means neither project.yaml nor the workspace default
	// supplied a value — the field is genuinely empty / the behavior key
	// does not exist anywhere.
	ProvenanceUnset FieldProvenance = "unset"
	// ProvenanceUnavailable means project.yaml itself did not supply a value
	// AND the linked workspace's default project definition could not be
	// read (a real error, not "no workspace linked" / "not configured" /
	// os.ErrNotExist) — so it is genuinely unknown whether a workspace
	// default would have supplied one. Codex review round 1 Major: without
	// this distinction, a workspace load failure was misreported as
	// ProvenanceUnset ("neither source supplied a value"), which is a
	// stronger claim than the code can actually back up — an ordinary
	// dispatch hitting the identical error would hydration-fail, not
	// silently treat the field as unset.
	ProvenanceUnavailable FieldProvenance = "workspace unavailable"
)

// BaseBranchSnapshotNote documents 決定2's tradeoff for --explain output:
// BaseBranch is resolved and snapshotted onto the task row at task-creation
// time (task_create.go), so changing the workspace default's base_branch (or
// project.yaml's) after a task already exists does NOT retroactively change
// that task's branch — only tasks created AFTER the change see the new
// value. task_behaviors / hooks are evaluated at dispatch time and are NOT
// snapshotted, so they DO pick up workspace default changes immediately for
// every existing task too.
const BaseBranchSnapshotNote = "base_branch is snapshotted onto each task at creation time; changing project.yaml or the workspace default afterward only affects NEW tasks, not tasks that already exist. task_behaviors are resolved at dispatch time and are not snapshotted."

// ProjectExplain is the field-provenance view of a project's effective meta
// (docs/plans/workspace-default-project.md 論点e, PR6). It answers "where
// did this value come from" for the 4 fields a workspace default definition
// can supply: task_behaviors (per canonical behavior name), base_branch,
// fork_point, default_task_behavior.
type ProjectExplain struct {
	ProjectID   string `json:"project_id"`
	WorkspaceID string `json:"workspace_id,omitempty"`

	// TaskBehaviors maps each canonical behavior name present in EITHER
	// project.yaml or the workspace default to its provenance. A name absent
	// from both never appears here (there is nothing to explain).
	TaskBehaviors map[string]FieldProvenance `json:"task_behaviors"`

	BaseBranch          FieldProvenance `json:"base_branch"`
	ForkPoint           FieldProvenance `json:"fork_point"`
	DefaultTaskBehavior FieldProvenance `json:"default_task_behavior"`

	// AnyWorkspaceDefaultApplied is true when at least one field/key above is
	// ProvenanceWorkspaceDefault. Surfaced even without --explain as the
	// minimum one-line indicator the doc requires.
	AnyWorkspaceDefaultApplied bool `json:"any_workspace_default_applied"`

	// BaseBranchSnapshotNote is always populated (constant text) so CLI
	// output can print it without a separate lookup.
	BaseBranchSnapshotNote string `json:"base_branch_snapshot_note"`

	// WorkspaceUnavailable is true when the project is linked to a workspace
	// whose default project definition could not be read (a real error, not
	// merely "unset" — see ProvenanceUnavailable). CLI output surfaces this
	// so the operator knows the explain result is incomplete, not a
	// confirmed "nothing is configured".
	WorkspaceUnavailable bool `json:"workspace_unavailable,omitempty"`

	// IsNoYAMLProject is true when ProjectID carries
	// URLDerivedProjectIDPrefix — the project.yaml-less registration path
	// (PR5). Its id was derived from the git URL, never read from a
	// project.yaml `id:` field.
	IsNoYAMLProject bool `json:"is_no_yaml_project"`

	// NameSource explains how a no-YAML project's Name was determined, for
	// carry-over visibility from the PR5 completion report's flagged
	// follow-up. Empty for a project.yaml-bearing project (Name always comes
	// from project.yaml there, nothing to explain). One of "explicit",
	// "cached", "basename", "url", or "" (name could not be recovered).
	NameSource string `json:"name_source,omitempty"`
}

// TaskBehaviorNames returns the explain's behavior-name keys sorted for
// stable display.
func (e *ProjectExplain) TaskBehaviorNames() []string {
	names := make([]string, 0, len(e.TaskBehaviors))
	for k := range e.TaskBehaviors {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// ComputeProjectExplain classifies rawMeta's (the project.yaml-only —
// pre-hydration — cached meta) 4 workspace-default-mergeable fields against
// wsMeta (the workspace's default project definition; nil when the project
// has no linked workspace, the workspace store is not configured, or
// workspace.yaml/row failed to load — see workspaceUnavailable below for how
// the last case is distinguished from the first two).
//
// rawMeta must not be nil. wsMeta may be nil.
//
// workspaceUnavailable must be true when wsMeta is nil SPECIFICALLY because
// an attempt to load the linked workspace failed with a real error (not
// os.ErrNotExist, not "no workspace linked", not "workspace store
// unconfigured") — i.e. the same condition that makes a real dispatch's
// GetWithWorkspace call fail outright. When true, a field that would
// otherwise read as ProvenanceUnset instead reads as ProvenanceUnavailable:
// "unset" asserts nothing supplies a value, which is a claim this function
// cannot back up when the workspace side could not even be read (Codex
// review round 1 Major).
//
// The task_behaviors comparison is done in the canonical name space (決定4,
// 論点j): rawMeta.TaskBehaviors is copied and stripped of alias mirror
// entries before comparison (stripAliasMirrors mutates its argument in
// place by deleting alias keys — Codex review round 1 Major: the original
// implementation passed the shared cached map straight in, permanently
// deleting its alias mirror entries on every read-only /explain call and
// racing any concurrent hydrate/dispatch touching the same map). A
// legacy-alias-authored project.yaml entry is compared under its canonical
// name exactly like GetWithWorkspace's own merge does. wsMeta.TaskBehaviors
// is always canonical-only already (normalizeWorkspaceDefaultTaskBehaviors's
// invariant) and is only ever read, never mutated, here.
func ComputeProjectExplain(projectID, workspaceID string, rawMeta *ProjectMeta, wsMeta *WorkspaceMeta, workspaceUnavailable bool) *ProjectExplain {
	out := &ProjectExplain{
		ProjectID:              projectID,
		WorkspaceID:            workspaceID,
		TaskBehaviors:          map[string]FieldProvenance{},
		BaseBranchSnapshotNote: BaseBranchSnapshotNote,
		IsNoYAMLProject:        IsURLDerivedProjectID(projectID),
		WorkspaceUnavailable:   workspaceUnavailable,
	}
	if rawMeta != nil {
		out.NameSource = rawMeta.NameSource
	}

	var rawBehaviors map[string]TaskBehavior
	if rawMeta != nil && len(rawMeta.TaskBehaviors) > 0 {
		cloned := make(map[string]TaskBehavior, len(rawMeta.TaskBehaviors))
		for k, v := range rawMeta.TaskBehaviors {
			cloned[k] = v
		}
		rawBehaviors = stripAliasMirrors(cloned)
	}
	var wsBehaviors map[string]TaskBehavior
	if wsMeta != nil {
		wsBehaviors = wsMeta.TaskBehaviors
	}

	for name := range rawBehaviors {
		out.TaskBehaviors[name] = ProvenanceProjectYAML
	}
	for name := range wsBehaviors {
		if _, exists := out.TaskBehaviors[name]; !exists {
			out.TaskBehaviors[name] = ProvenanceWorkspaceDefault
			out.AnyWorkspaceDefaultApplied = true
		}
	}

	classify := func(rawVal, wsVal string) FieldProvenance {
		if rawVal != "" {
			return ProvenanceProjectYAML
		}
		if wsVal != "" {
			out.AnyWorkspaceDefaultApplied = true
			return ProvenanceWorkspaceDefault
		}
		if workspaceUnavailable {
			return ProvenanceUnavailable
		}
		return ProvenanceUnset
	}

	var rawBaseBranch, rawForkPoint, rawDefaultBehavior string
	if rawMeta != nil {
		rawBaseBranch = rawMeta.BaseBranch
		rawForkPoint = rawMeta.ForkPoint
		rawDefaultBehavior = rawMeta.DefaultTaskBehavior
	}
	var wsBaseBranch, wsForkPoint, wsDefaultBehavior string
	if wsMeta != nil {
		wsBaseBranch = wsMeta.BaseBranch
		wsForkPoint = wsMeta.ForkPoint
		wsDefaultBehavior = wsMeta.DefaultTaskBehavior
	}

	out.BaseBranch = classify(rawBaseBranch, wsBaseBranch)
	out.ForkPoint = classify(rawForkPoint, wsForkPoint)
	out.DefaultTaskBehavior = classify(rawDefaultBehavior, wsDefaultBehavior)

	return out
}
