package orchestrator

import "testing"

// These unit tests pin the merge semantics of mergeBindMounts, which backs
// project.yaml's AdditionalBindings → per-behavior overlay (ReadProjectMetaWithKits).
// The 2026-06-29 binding regression (workspace kit additional_bindings
// silently dropped) motivated Tier 1 #1 of docs/plans/quality-gates.md.
// (unionBindMountSlices, the sibling combinator this file used to also pin,
// backed the workspace-level kit-materialized AdditionalBindings merge —
// retired outright in docs/plans/home-workspace-volume.md Phase 4 PR4 along
// with the WorkspaceMeta field it fed.)

func bindBySource(mounts []BindMount, src string) (BindMount, bool) {
	for _, m := range mounts {
		if m.Source == src {
			return m, true
		}
	}
	return BindMount{}, false
}

func TestMergeBindMounts_OverlayWinsOnConflict(t *testing.T) {
	t.Parallel()

	base := []BindMount{
		{Source: "/kit/tool", Target: "/kit/tool", Mode: "ro"},
	}
	overlay := []BindMount{
		{Source: "/kit/tool", Target: "/kit/tool", Mode: "rw"},
	}

	got := mergeBindMounts(base, overlay)
	if len(got) != 1 {
		t.Fatalf("expected 1 merged binding, got %d: %+v", len(got), got)
	}
	if got[0].Mode != "rw" {
		t.Fatalf("overlay must win on conflicting source: got mode %q, want rw", got[0].Mode)
	}
}

func TestMergeBindMounts_DisjointSourcesUnion(t *testing.T) {
	t.Parallel()

	base := []BindMount{{Source: "/kit/a", Mode: "ro"}}
	overlay := []BindMount{{Source: "/project/b", Mode: "rw"}}

	got := mergeBindMounts(base, overlay)
	if len(got) != 2 {
		t.Fatalf("expected both bindings, got %d: %+v", len(got), got)
	}
	if _, ok := bindBySource(got, "/kit/a"); !ok {
		t.Errorf("base binding /kit/a dropped: %+v", got)
	}
	if _, ok := bindBySource(got, "/project/b"); !ok {
		t.Errorf("overlay binding /project/b dropped: %+v", got)
	}
}

func TestMergeBindMounts_EmptyOverlayClonesBase(t *testing.T) {
	t.Parallel()

	base := []BindMount{{Source: "/kit/a", Mode: "ro"}}
	got := mergeBindMounts(base, nil)
	if len(got) != 1 || got[0].Source != "/kit/a" {
		t.Fatalf("empty overlay must clone base: got %+v", got)
	}
	// Mutating the result must not touch the original (clone, not alias).
	got[0].Mode = "rw"
	if base[0].Mode != "ro" {
		t.Fatalf("mergeBindMounts must clone base, but base was mutated: %+v", base)
	}
}
