package api

import (
	"os"
	"testing"

	"github.com/novshi-tech/boid/testutil/homeenv"
)

// TestMain isolates this package's test binary from the real developer
// machine's persistent state, for the same reason cmd's, internal/server's and
// internal/dispatcher's do (see homeenv's package doc for the shared list).
//
// This package was the last testutil.NewTestServer consumer without one, and
// the gap was not theoretical: workspace_integration_test.go boots a full
// production-path daemon and issues `DELETE /api/workspaces/team-a` against
// it. Since PR7 of docs/plans/workspace-home-volume-persistence.md wired that
// endpoint onto the engine's volume API, that DELETE reaches
// VolumeRemove(Force: true) on the developer's own docker/podman engine —
// destroying a real workspace HOME volume, credentials and all, if one happens
// to be named for the slug the test picked. homeenv.Isolate's DOCKER_HOST
// override is what makes that unreachable; this TestMain is what installs it
// here.
func TestMain(m *testing.M) {
	os.Exit(homeenv.Run(m))
}

// TestAPISuite_IsIsolatedFromRealUserState is the standing guard for the
// property TestMain above provides; see homeenv.AssertIsolated's own doc
// comment for why the TestMain alone is not enough.
func TestAPISuite_IsIsolatedFromRealUserState(t *testing.T) {
	homeenv.AssertIsolated(t)
}
