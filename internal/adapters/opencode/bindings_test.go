package opencode

import (
	"testing"
)

// TestBindings_ReturnsEmpty pins the Phase 4 PR3 retirement (docs/plans/
// home-workspace-volume.md): opencode.Adapter no longer declares any bind
// mounts — not even the former per-entry ro binds for non-embedded host
// skills (bitbucket, jira, google-* etc.), which were a tmpfs-era workaround.
// ~/.opencode, ~/.config/opencode, the opencode CLI binary, and the workspace
// skills tree all live directly in the sandbox's $HOME because
// Runner.Dispatch bind-mounts the workspace's persistent home directory
// there; embedded skills are copy-synced by skills.DeployAll. Exposing
// non-embedded host skills to opencode is now the workspace's init.sh's
// responsibility (see bindings.go's doc comment). A regression here (a
// binding creeping back in) would silently reintroduce the retired
// bind-per-workspace-home coupling this PR removed.
func TestBindings_ReturnsEmpty(t *testing.T) {
	mounts := New().Bindings("/home/test")
	if len(mounts) != 0 {
		t.Errorf("Bindings() = %+v, want empty (Phase 4 PR3 retired all adapter-declared binds)", mounts)
	}
}
