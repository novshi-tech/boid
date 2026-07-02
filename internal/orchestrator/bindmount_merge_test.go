package orchestrator

import "testing"

// These unit tests pin the merge semantics of the two bind-mount combinators
// that back workspace-kit → project binding hydration. The 2026-06-29 binding
// regression (workspace kit additional_bindings silently dropped) motivated
// Tier 1 #1 of docs/plans/quality-gates.md: mergeBindMounts feeds
// GetWithWorkspace's top-level meta.AdditionalBindings merge (project wins),
// while unionBindMountSlices backs the per-behavior/kit merge (mode promotion).

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

func TestUnionBindMountSlices_PromotesModeToRW(t *testing.T) {
	t.Parallel()

	// A ro binding in base and the same source rw in extra must promote to rw.
	base := []BindMount{{Source: "/kit/a", Mode: "ro"}}
	extra := []BindMount{{Source: "/kit/a", Mode: "rw"}}

	got := unionBindMountSlices(base, extra)
	if len(got) != 1 {
		t.Fatalf("expected 1 unioned binding, got %d: %+v", len(got), got)
	}
	if got[0].Mode != "rw" {
		t.Fatalf("union must promote ro→rw on conflict: got %q", got[0].Mode)
	}
}
