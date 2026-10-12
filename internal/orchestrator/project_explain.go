package orchestrator

import "sort"

// FieldProvenance identifies the source of a runtime setting.
type FieldProvenance string

const (
	ProvenanceWorkspaceDefault FieldProvenance = "workspace default"
	ProvenanceUnset            FieldProvenance = "unset"
	ProvenanceUnavailable      FieldProvenance = "workspace unavailable"
)

const BaseBranchSnapshotNote = "base_branch is snapshotted onto each task at creation time; changing the workspace afterward only affects NEW tasks. An empty base_branch uses the origin default branch at checkout. task_behaviors are resolved at dispatch time."

// ProjectExplain describes a registered repository's workspace configuration.
type ProjectExplain struct {
	ProjectID                  string                     `json:"project_id"`
	WorkspaceID                string                     `json:"workspace_id,omitempty"`
	TaskBehaviors              map[string]FieldProvenance `json:"task_behaviors"`
	BaseBranch                 FieldProvenance            `json:"base_branch"`
	ForkPoint                  FieldProvenance            `json:"fork_point"`
	DefaultTaskBehavior        FieldProvenance            `json:"default_task_behavior"`
	AnyWorkspaceDefaultApplied bool                       `json:"any_workspace_default_applied"`
	BaseBranchSnapshotNote     string                     `json:"base_branch_snapshot_note"`
	WorkspaceUnavailable       bool                       `json:"workspace_unavailable,omitempty"`
	IDSource                   string                     `json:"id_source"`
	NameSource                 string                     `json:"name_source,omitempty"`
}

// TaskBehaviorNames returns sorted behavior names for display.
func (e *ProjectExplain) TaskBehaviorNames() []string {
	names := make([]string, 0, len(e.TaskBehaviors))
	for name := range e.TaskBehaviors {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ComputeProjectExplain classifies settings supplied by the workspace.
func ComputeProjectExplain(projectID, workspaceID string, identity *ProjectMeta, ws *WorkspaceMeta, unavailable bool) *ProjectExplain {
	out := &ProjectExplain{ProjectID: projectID, WorkspaceID: workspaceID, TaskBehaviors: map[string]FieldProvenance{}, BaseBranchSnapshotNote: BaseBranchSnapshotNote, WorkspaceUnavailable: unavailable, IDSource: "registry"}
	if IsURLDerivedProjectID(projectID) {
		out.IDSource = "derived from git URL"
	}
	if identity != nil {
		out.NameSource = identity.NameSource
	}
	classify := func(value string) FieldProvenance {
		if value != "" {
			out.AnyWorkspaceDefaultApplied = true
			return ProvenanceWorkspaceDefault
		}
		if unavailable {
			return ProvenanceUnavailable
		}
		return ProvenanceUnset
	}
	var branch, fork, behavior string
	if ws != nil {
		branch, fork, behavior = ws.BaseBranch, ws.ForkPoint, ws.DefaultTaskBehavior
		for name := range ws.TaskBehaviors {
			out.TaskBehaviors[name] = ProvenanceWorkspaceDefault
			out.AnyWorkspaceDefaultApplied = true
		}
	}
	out.BaseBranch, out.ForkPoint, out.DefaultTaskBehavior = classify(branch), classify(fork), classify(behavior)
	return out
}
