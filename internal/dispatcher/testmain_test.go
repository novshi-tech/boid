package dispatcher

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain isolates every test in this package — and its dispatcher_test
// counterpart, since Go links both `package dispatcher` and `package
// dispatcher_test` _test.go files into one test binary per directory —
// from the real developer machine's $HOME.
//
// Runner.Dispatch now unconditionally calls resolveWorkspaceHome (Phase 4
// PR1, docs/plans/home-workspace-volume.md), which mkdir's
// $XDG_DATA_HOME/boid/homes/<slug> and reads
// $XDG_CONFIG_HOME/boid/workspaces/<slug>/init.sh for every dispatch. Without
// this override, every existing test that calls r.Dispatch(...) — most of
// which never set a WorkspaceID and so resolve to the "default" slug — would
// mkdir and read from the real ~/.local/share/boid and ~/.config/boid on
// whatever machine `go test` runs on. Forcing HOME/XDG_* to a throwaway temp
// dir for the whole test binary process keeps every test hermetic.
//
// Individual tests (e.g. workspace_home_test.go) that need their own
// per-case isolation still call t.Setenv with a fresh t.TempDir() — that
// overrides this process-wide default for the duration of the single test
// and is automatically restored after, so there is no conflict.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "boid-dispatcher-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", dir)
	os.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
