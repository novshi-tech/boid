package orchestrator

import "testing"

func TestComputeProjectExplain_AllUnset(t *testing.T) {
	raw := &ProjectMeta{ID: "p1"}
	got := ComputeProjectExplain("p1", "", raw, nil, false)

	if got.BaseBranch != ProvenanceUnset {
		t.Errorf("BaseBranch = %v, want unset", got.BaseBranch)
	}
	if got.ForkPoint != ProvenanceUnset {
		t.Errorf("ForkPoint = %v, want unset", got.ForkPoint)
	}
	if got.DefaultTaskBehavior != ProvenanceUnset {
		t.Errorf("DefaultTaskBehavior = %v, want unset", got.DefaultTaskBehavior)
	}
	if len(got.TaskBehaviors) != 0 {
		t.Errorf("TaskBehaviors = %v, want empty", got.TaskBehaviors)
	}
	if got.AnyWorkspaceDefaultApplied {
		t.Error("AnyWorkspaceDefaultApplied = true, want false")
	}
}

func TestComputeProjectExplain_ProjectYAMLWins(t *testing.T) {
	raw := &ProjectMeta{
		ID:                  "p1",
		BaseBranch:          "main",
		ForkPoint:           "origin/main",
		DefaultTaskBehavior: "supervisor",
		TaskBehaviors: map[string]TaskBehavior{
			"supervisor": {},
		},
	}
	ws := &WorkspaceMeta{
		BaseBranch:          "develop",
		ForkPoint:           "origin/develop",
		DefaultTaskBehavior: "executor",
		TaskBehaviors: map[string]TaskBehavior{
			"supervisor": {},
			"executor":   {},
		},
	}
	got := ComputeProjectExplain("p1", "ws1", raw, ws, false)

	if got.BaseBranch != ProvenanceProjectYAML {
		t.Errorf("BaseBranch = %v, want project.yaml", got.BaseBranch)
	}
	if got.ForkPoint != ProvenanceProjectYAML {
		t.Errorf("ForkPoint = %v, want project.yaml", got.ForkPoint)
	}
	if got.DefaultTaskBehavior != ProvenanceProjectYAML {
		t.Errorf("DefaultTaskBehavior = %v, want project.yaml", got.DefaultTaskBehavior)
	}
	if got.TaskBehaviors["supervisor"] != ProvenanceProjectYAML {
		t.Errorf("supervisor = %v, want project.yaml (same-name project.yaml wins outright, 決定4)", got.TaskBehaviors["supervisor"])
	}
	if got.TaskBehaviors["executor"] != ProvenanceWorkspaceDefault {
		t.Errorf("executor = %v, want workspace default", got.TaskBehaviors["executor"])
	}
	if !got.AnyWorkspaceDefaultApplied {
		t.Error("AnyWorkspaceDefaultApplied = false, want true (executor came from workspace default)")
	}
}

func TestComputeProjectExplain_EmptyMeansInheritFromWorkspace(t *testing.T) {
	raw := &ProjectMeta{ID: "p1"}
	ws := &WorkspaceMeta{
		BaseBranch:          "develop",
		ForkPoint:           "origin/develop",
		DefaultTaskBehavior: "executor",
		TaskBehaviors: map[string]TaskBehavior{
			"executor": {},
		},
	}
	got := ComputeProjectExplain("p1", "ws1", raw, ws, false)

	if got.BaseBranch != ProvenanceWorkspaceDefault {
		t.Errorf("BaseBranch = %v, want workspace default", got.BaseBranch)
	}
	if got.ForkPoint != ProvenanceWorkspaceDefault {
		t.Errorf("ForkPoint = %v, want workspace default", got.ForkPoint)
	}
	if got.DefaultTaskBehavior != ProvenanceWorkspaceDefault {
		t.Errorf("DefaultTaskBehavior = %v, want workspace default", got.DefaultTaskBehavior)
	}
	if got.TaskBehaviors["executor"] != ProvenanceWorkspaceDefault {
		t.Errorf("executor = %v, want workspace default", got.TaskBehaviors["executor"])
	}
	if !got.AnyWorkspaceDefaultApplied {
		t.Error("AnyWorkspaceDefaultApplied = false, want true")
	}
}

func TestComputeProjectExplain_NilWorkspaceMetaAllProjectYAMLOrUnset(t *testing.T) {
	raw := &ProjectMeta{
		ID:         "p1",
		BaseBranch: "main",
		TaskBehaviors: map[string]TaskBehavior{
			"supervisor": {},
		},
	}
	got := ComputeProjectExplain("p1", "", raw, nil, false)

	if got.BaseBranch != ProvenanceProjectYAML {
		t.Errorf("BaseBranch = %v, want project.yaml", got.BaseBranch)
	}
	if got.ForkPoint != ProvenanceUnset {
		t.Errorf("ForkPoint = %v, want unset", got.ForkPoint)
	}
	if got.TaskBehaviors["supervisor"] != ProvenanceProjectYAML {
		t.Errorf("supervisor = %v, want project.yaml", got.TaskBehaviors["supervisor"])
	}
	if got.AnyWorkspaceDefaultApplied {
		t.Error("AnyWorkspaceDefaultApplied = true, want false")
	}
}

func TestComputeProjectExplain_AliasNameCollapsedToCanonical(t *testing.T) {
	// project.yaml authored under the legacy alias "dev"; addAliasMirrors
	// would have added a "dev" -> canonical mirror pair by the time this is
	// cached. The explain must compare in canonical space (決定4, 論点j) so a
	// workspace default entry for the canonical name is correctly seen as
	// "already defined by project.yaml", not double-counted as a separate
	// workspace-default addition.
	canonical, isAlias := CanonicalBehaviorName("dev")
	if !isAlias {
		t.Skip("dev is not a known alias in this build; skipping alias-specific assertion")
	}
	raw := &ProjectMeta{
		ID: "p1",
		TaskBehaviors: map[string]TaskBehavior{
			canonical: {},
			"dev":     {}, // alias mirror, as addAliasMirrors would produce
		},
	}
	ws := &WorkspaceMeta{
		TaskBehaviors: map[string]TaskBehavior{
			canonical: {},
		},
	}
	got := ComputeProjectExplain("p1", "ws1", raw, ws, false)

	if got.TaskBehaviors[canonical] != ProvenanceProjectYAML {
		t.Errorf("%s = %v, want project.yaml", canonical, got.TaskBehaviors[canonical])
	}
	if _, ok := got.TaskBehaviors["dev"]; ok {
		t.Error(`"dev" alias mirror key should not appear as its own explain entry`)
	}
	if got.AnyWorkspaceDefaultApplied {
		t.Error("AnyWorkspaceDefaultApplied = true, want false (canonical name already defined by project.yaml)")
	}

	// Codex review round 1 Major: stripAliasMirrors mutates its argument by
	// deleting alias keys in place. ComputeProjectExplain must operate on a
	// copy — the caller's rawMeta.TaskBehaviors (which, via
	// ProjectStore.Explain, is the SHARED cached map every concurrent
	// dispatch/hydrate also reads) must still contain the alias mirror entry
	// after this call, and a second call must produce an identical result.
	if _, ok := raw.TaskBehaviors["dev"]; !ok {
		t.Error(`raw.TaskBehaviors["dev"] was deleted by ComputeProjectExplain — it must not mutate its input`)
	}
	got2 := ComputeProjectExplain("p1", "ws1", raw, ws, false)
	if got2.TaskBehaviors[canonical] != got.TaskBehaviors[canonical] {
		t.Errorf("second call = %v, first call = %v — result must be stable across repeated calls", got2.TaskBehaviors[canonical], got.TaskBehaviors[canonical])
	}
}

func TestComputeProjectExplain_WorkspaceUnavailableIsNotUnset(t *testing.T) {
	// Codex review round 1 Major: a real workspace load failure must not be
	// reported the same way as "nothing configured anywhere" (ProvenanceUnset)
	// — this function cannot back up that stronger claim when it couldn't
	// even read the workspace side.
	raw := &ProjectMeta{ID: "p1"}
	got := ComputeProjectExplain("p1", "ws1", raw, nil, true)

	if got.BaseBranch != ProvenanceUnavailable {
		t.Errorf("BaseBranch = %v, want unavailable", got.BaseBranch)
	}
	if got.ForkPoint != ProvenanceUnavailable {
		t.Errorf("ForkPoint = %v, want unavailable", got.ForkPoint)
	}
	if got.DefaultTaskBehavior != ProvenanceUnavailable {
		t.Errorf("DefaultTaskBehavior = %v, want unavailable", got.DefaultTaskBehavior)
	}
	if !got.WorkspaceUnavailable {
		t.Error("WorkspaceUnavailable = false, want true")
	}
	if got.AnyWorkspaceDefaultApplied {
		t.Error("AnyWorkspaceDefaultApplied = true, want false (we don't know that anything was actually applied)")
	}
}

func TestComputeProjectExplain_ProjectYAMLValueWinsEvenWhenWorkspaceUnavailable(t *testing.T) {
	raw := &ProjectMeta{ID: "p1", BaseBranch: "main"}
	got := ComputeProjectExplain("p1", "ws1", raw, nil, true)

	if got.BaseBranch != ProvenanceProjectYAML {
		t.Errorf("BaseBranch = %v, want project.yaml (project.yaml's own value must win regardless of workspace availability)", got.BaseBranch)
	}
}

func TestComputeProjectExplain_NoYAMLProjectIDAndNameSource(t *testing.T) {
	raw := &ProjectMeta{ID: "url-abc123", Name: "myrepo", NameSource: "url"}
	got := ComputeProjectExplain("url-abc123", "ws1", raw, nil, false)

	if !got.IsNoYAMLProject {
		t.Error("IsNoYAMLProject = false, want true for url- prefixed id")
	}
	if got.NameSource != "url" {
		t.Errorf("NameSource = %q, want %q", got.NameSource, "url")
	}
}

func TestComputeProjectExplain_ProjectYAMLProjectHasNoNameSource(t *testing.T) {
	raw := &ProjectMeta{ID: "khi", Name: "khi"}
	got := ComputeProjectExplain("khi", "", raw, nil, false)

	if got.IsNoYAMLProject {
		t.Error("IsNoYAMLProject = true, want false for a plain id")
	}
	if got.NameSource != "" {
		t.Errorf("NameSource = %q, want empty for a project.yaml-bearing project", got.NameSource)
	}
}
