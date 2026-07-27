package testutil

import (
	"os"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/testutil/homeenv"
)

// TestMain gives this package's own tests the same isolation NewTestServer
// demands of its callers — including, since PR7 of
// docs/plans/workspace-home-volume-persistence.md, the docker engine.
func TestMain(m *testing.M) {
	os.Exit(homeenv.Run(m))
}

// TestNewTestServer_TalksToAnUnreachableEngineNotTheDevelopersOwn is the
// standing end-to-end guard for PR7 round-2 codex review Blocker 2.
//
// The two halves it holds together are in different packages and neither one
// alone is the property that matters: homeenv's tests pin that Isolate
// redirects DOCKER_HOST, and NewTestServer's own guard pins that a caller
// without that isolation is refused — but only an actual daemon can show that
// the redirect reaches the place the damage would happen, which is
// buildRuntime's client.New(client.FromEnv) deep inside server.New, wired into
// WorkspaceHandler.Homes.
//
// Asserting the failure MESSAGE, rather than merely that a failure occurred,
// is the point: "the size lookup failed" is also true of a reachable engine
// having a bad day, and would keep passing if the redirect were lost. Naming
// the socket in the assertion means the test can only pass while the daemon is
// dialling the throwaway path homeenv installed.
func TestNewTestServer_TalksToAnUnreachableEngineNotTheDevelopersOwn(t *testing.T) {
	isolatedSocket, ok := strings.CutPrefix(os.Getenv("DOCKER_HOST"), "unix://")
	if !ok {
		t.Fatalf("DOCKER_HOST = %q, want the unix:// address homeenv.Isolate installs", os.Getenv("DOCKER_HOST"))
	}

	ts := NewTestServer(t)

	// Decoded loosely rather than into api.WorkspaceDetail: testutil imports
	// internal/server, which imports internal/api, so importing internal/api
	// here would put testutil inside a cycle for that package's in-package
	// test files.
	var detail struct {
		Home struct {
			Volume    string `json:"volume"`
			SizeError string `json:"size_error"`
		} `json:"home"`
	}
	if err := ts.Client.Do("GET", "/api/workspaces/default", nil, &detail); err != nil {
		t.Fatalf("GET /api/workspaces/default: %v", err)
	}

	if detail.Home.Volume == "" {
		t.Fatal("home.volume is empty: the daemon built no workspace-home store at all, so this test would pass " +
			"even against a reachable engine. NewTestServer must still exercise the production wiring.")
	}
	if !strings.Contains(detail.Home.SizeError, isolatedSocket) {
		t.Errorf("home.size_error = %q, want it to name the isolated socket %q — the test daemon is reaching some OTHER engine, "+
			"and `workspace remove` against it would DELETE real workspace HOME volumes", detail.Home.SizeError, isolatedSocket)
	}
}
