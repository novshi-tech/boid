package claude

import (
	"testing"
)

// TestBindings_ReturnsEmpty pins the Phase 4 PR3 retirement (docs/plans/
// home-workspace-volume.md): claude.Adapter no longer declares any bind
// mounts. ~/.claude, ~/.claude.json, and the claude CLI binary itself all
// live directly in the sandbox's $HOME because Runner.Dispatch bind-mounts
// the workspace's persistent home directory there; embedded skills are
// copy-synced by skills.DeployAll instead of bind-mounted. A regression here
// (a binding creeping back in) would silently reintroduce the retired
// bind-per-workspace-home coupling this PR removed.
func TestBindings_ReturnsEmpty(t *testing.T) {
	mounts := New().Bindings("/home/test")
	if len(mounts) != 0 {
		t.Errorf("Bindings() = %+v, want empty (Phase 4 PR3 retired all adapter-declared binds)", mounts)
	}
}
