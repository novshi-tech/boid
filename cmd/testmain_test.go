package cmd

import (
	"os"
	"testing"

	"github.com/novshi-tech/boid/testutil/homeenv"
)

// TestMain isolates this package's test binary from the real developer
// machine's $HOME and XDG base directories, for the same reason
// internal/dispatcher's and internal/server's do (see either one's own
// comment, and homeenv's package doc).
//
// The concrete leak here is narrower than those two but has the same shape:
// cmd's own defaultDBPath() does a best-effort `MkdirAll($XDG_DATA_HOME/boid)`
// as a side effect of computing the default DB path, and buildStartConfig
// calls it. Most cmd tests already point XDG_DATA_HOME at a t.TempDir(), but
// the ones that only care about an unrelated field
// (TestBuildStartConfig_CLIAddrOverride, TestBuildStartConfig_CLITokenFromEnv)
// reasonably did not bother — and so created ~/.local/share/boid on whatever
// machine ran `go test ./...`. Fixing those two individually would leave the
// next such test to reintroduce it; a TestMain closes the class.
func TestMain(m *testing.M) {
	os.Exit(homeenv.Run(m))
}

// TestCmdSuite_IsIsolatedFromRealUserHome is the standing guard for the
// property TestMain above provides; see homeenv.AssertIsolated's own doc
// comment for why the TestMain alone is not enough.
func TestCmdSuite_IsIsolatedFromRealUserHome(t *testing.T) {
	homeenv.AssertIsolated(t)
}
