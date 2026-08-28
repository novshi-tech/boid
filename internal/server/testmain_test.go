package server

import (
	"os"
	"testing"

	"github.com/novshi-tech/boid/testutil/homeenv"
)

// TestMain isolates this package's whole test binary from the real developer
// machine's $HOME and XDG base directories.
//
// It is needed for the same reason internal/dispatcher's TestMain is, but the
// isolation there does NOT reach this package: a TestMain only governs the
// test binary of its own directory. internal/server's suite constructs bare
// `dispatcher.Runner{}` values — no RuntimesDir, no DataHomeDir — and drives
// real Dispatch calls through them (boid_executor_task_context_wiring_test.go,
// deliberately: its whole point is exercising the production Runner rather
// than a stub). Dispatch unconditionally calls resolveWorkspaceHome, and with
// both roots empty that falls back to $XDG_DATA_HOME/boid — so before this
// TestMain existed, `go test ./internal/server/` on an ordinary developer
// machine would:
//
//   - mkdir ~/.local/share/boid/homes/default and create the skill
//     discovery roots under the home, with a symlink per skill into the
//     runner image (internal/dispatcher's skillLinks),
//   - write ~/.local/share/boid/homes-meta/default.{init.json,lock}
//     (docs/plans/workspace-home-volume-persistence.md 論点b, PR2 — this
//     directory is what made the pre-existing homes/ pollution visible), and
//   - EXECUTE ~/.config/boid/workspaces/default/init.sh whenever the marker
//     did not already record that script's hash, since a marker miss is
//     indistinguishable from a first dispatch.
//
// That last one is the sharp edge: init.sh is arbitrary trusted host shell
// (on the machine this was found on, an 8KB toolchain installer), so a plain
// `go test ./...` could kick off a multi-gigabyte install against the real
// workspace home.
//
// Per-package rather than per-test on purpose. Setting RuntimesDir/DataHomeDir
// on the two Runners that exist today would fix today's tests and silently
// re-open the hole for the next one; a TestMain covers every test this package
// will ever grow, including ones that reach Dispatch indirectly.
func TestMain(m *testing.M) {
	os.Exit(homeenv.Run(m))
}

// TestServerSuite_IsIsolatedFromRealUserHome is the standing guard for the
// property TestMain below provides. See homeenv.AssertIsolated's own doc
// comment for why an assertion is needed on top of the TestMain: losing the
// isolation is otherwise completely silent — the suite stays green and simply
// starts operating on the developer's real boid installation.
func TestServerSuite_IsIsolatedFromRealUserHome(t *testing.T) {
	homeenv.AssertIsolated(t)
}
