package dispatcher

import (
	"os"
	"testing"

	"github.com/novshi-tech/boid/testutil/homeenv"
)

// TestMain isolates every test in this package — and its dispatcher_test
// counterpart, since Go links both `package dispatcher` and `package
// dispatcher_test` _test.go files into one test binary per directory —
// from the real developer machine's $HOME.
//
// Runner.Dispatch unconditionally calls resolveWorkspaceHome (Phase 4
// PR1, docs/plans/home-workspace-volume.md), which mkdir's
// $XDG_DATA_HOME/boid/homes/<slug>, writes
// $XDG_DATA_HOME/boid/homes-meta/<slug>.{init.json,lock} (PR2 of
// docs/plans/workspace-home-volume-persistence.md, 論点b) and reads — and on
// a marker miss, EXECUTES — $XDG_CONFIG_HOME/boid/workspaces/<slug>/init.sh
// for every dispatch. Without this override, every existing test that calls
// r.Dispatch(...) — most of which never set a WorkspaceID and so resolve to
// the "default" slug — would operate on the real ~/.local/share/boid and
// ~/.config/boid on whatever machine `go test` runs on. Forcing HOME/XDG_* to
// a throwaway temp dir for the whole test binary process keeps every test
// hermetic.
//
// Individual tests (e.g. workspace_home_test.go) that need their own
// per-case isolation still call t.Setenv with a fresh t.TempDir() — that
// overrides this process-wide default for the duration of the single test
// and is automatically restored after, so there is no conflict.
//
// The body lives in testutil/homeenv rather than here so that internal/server
// — which has exactly the same problem via its own bare `dispatcher.Runner{}`
// wiring, and whose test binary this TestMain does not reach — shares one
// definition of "which variables count as isolation" instead of a second copy
// that can drift.
func TestMain(m *testing.M) {
	os.Exit(homeenv.Run(m))
}

// TestDispatcherSuite_IsIsolatedFromRealUserHome is the standing guard for
// the property TestMain above provides; see homeenv.AssertIsolated's own doc
// comment for why the TestMain alone is not enough (losing it is silent).
func TestDispatcherSuite_IsIsolatedFromRealUserHome(t *testing.T) {
	homeenv.AssertIsolated(t)
}
