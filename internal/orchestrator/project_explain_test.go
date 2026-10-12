package orchestrator

import "testing"

func TestComputeProjectExplainWorkspaceOnly(t *testing.T) {
	stale := &ProjectMeta{BaseBranch: "stale", TaskBehaviors: map[string]TaskBehavior{"stale": {}}, NameSource: "basename"}
	ws := &WorkspaceMeta{BaseBranch: "main", DefaultTaskBehavior: "dev", TaskBehaviors: map[string]TaskBehavior{"dev": {}}}
	got := ComputeProjectExplain("p", "team", stale, ws, false)
	if got.BaseBranch != ProvenanceWorkspaceDefault || got.DefaultTaskBehavior != ProvenanceWorkspaceDefault || got.ForkPoint != ProvenanceUnset || len(got.TaskBehaviors) != 1 || got.TaskBehaviors["dev"] != ProvenanceWorkspaceDefault {
		t.Fatalf("explain: %+v", got)
	}
	if got.NameSource != "basename" || !got.AnyWorkspaceDefaultApplied {
		t.Fatalf("identity: %+v", got)
	}
	for _, unavailable := range []bool{false, true} {
		got = ComputeProjectExplain("p", "team", stale, nil, unavailable)
		want := ProvenanceUnset
		if unavailable {
			want = ProvenanceUnavailable
		}
		if got.BaseBranch != want || len(got.TaskBehaviors) != 0 {
			t.Fatalf("missing workspace: %+v", got)
		}
	}
}
