package claude

import (
	"testing"
)

// TestBindings_ReturnsEmpty pins the Phase 4 PR3 retirement (docs/plans/
// home-workspace-volume.md): claude.Adapter no longer declares any bind
// mounts. ~/.claude, ~/.claude.json, and the claude CLI binary itself all
// live directly in the sandbox's $HOME because Runner.Dispatch bind-mounts
// the workspace's persistent home directory there.
//
// Embedded skills ARE bind-mounted again as of PR3 of
// docs/plans/workspace-home-volume-persistence.md (論点 e-2) — one read-only
// bind per skill onto ~/.claude/skills/<name> — but those mounts are
// declared by the dispatcher (internal/dispatcher/skills_overlay.go +
// homeMounts), not by this method, so the assertion below is unchanged. A
// regression here (a binding creeping back in) would silently reintroduce
// the retired per-adapter host-path coupling Phase 4 PR3 removed.
func TestBindings_ReturnsEmpty(t *testing.T) {
	mounts := New().Bindings("/home/test")
	if len(mounts) != 0 {
		t.Errorf("Bindings() = %+v, want empty (Phase 4 PR3 retired all adapter-declared binds)", mounts)
	}
}
