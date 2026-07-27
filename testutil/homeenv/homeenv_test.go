package homeenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The docker-engine half of this package's isolation (PR7 round-2 codex
// review, Blocker 2 of docs/plans/workspace-home-volume-persistence.md 論点
// a-2). Since PR7, a test daemon built by testutil.NewTestServer resolves its
// engine handle from DOCKER_HOST via client.New(client.FromEnv) and
// `DELETE /api/workspaces/{slug}` issues VolumeRemove(Force: true) against
// whatever that lands on — the developer's own docker/podman engine, with
// their real workspace HOME volumes on it. Measured on the machine this was
// found on: `go test ./cmd/` with a reachable engine destroyed a
// `boid-ws-home-noinst-team-c` volume outright.
//
// So DOCKER_HOST belongs on the same list as HOME and the XDG roots: it names
// a piece of the developer's persistent state that boid writes to. These tests
// pin that it is on the list and that Isolate points it somewhere nothing can
// answer.

// TestIsolate_PointsDockerHostAtADeadSocket pins the mechanism: after Isolate,
// DOCKER_HOST addresses a unix socket inside the throwaway directory that
// nothing ever creates, so no engine — not the default /var/run/docker.sock,
// not a rootless podman socket the developer had configured — is reachable.
func TestIsolate_PointsDockerHostAtADeadSocket(t *testing.T) {
	before := os.Getenv("DOCKER_HOST")
	t.Cleanup(func() { _ = os.Setenv("DOCKER_HOST", before) })

	cleanup, err := Isolate()
	if err != nil {
		t.Fatalf("Isolate: %v", err)
	}
	defer cleanup()

	got := os.Getenv("DOCKER_HOST")
	if got == before {
		t.Fatalf("DOCKER_HOST = %q, unchanged by Isolate — a test daemon would still talk to the developer's engine", got)
	}
	path, ok := strings.CutPrefix(got, "unix://")
	if !ok {
		t.Fatalf("DOCKER_HOST = %q, want a unix:// address", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("DOCKER_HOST socket %q exists (stat err=%v) — the whole point is that nothing answers there", path, err)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("DOCKER_HOST socket path %q is not absolute; client.New would reject it", path)
	}
}

// TestIsolate_ClearsTheDockerTransportVariables pins the rest of what
// client.FromEnv reads. DOCKER_HOST alone decides WHICH engine is addressed,
// so these are not a safety hole — but a developer with DOCKER_CERT_PATH /
// DOCKER_TLS_VERIFY set would otherwise have client.New(client.FromEnv) load
// (or fail to load) their real TLS material for the dead socket, which turns
// `daemon startup refused: connect to docker` into a machine-dependent test
// failure. Clearing them makes the isolated environment fully self-consistent.
func TestIsolate_ClearsTheDockerTransportVariables(t *testing.T) {
	for _, key := range clearedKeys {
		before := os.Getenv(key)
		t.Cleanup(func() { _ = os.Setenv(key, before) })
		if err := os.Setenv(key, "leftover-from-the-developer-machine"); err != nil {
			t.Fatalf("Setenv %s: %v", key, err)
		}
	}

	cleanup, err := Isolate()
	if err != nil {
		t.Fatalf("Isolate: %v", err)
	}
	defer cleanup()

	for _, key := range clearedKeys {
		if got := os.Getenv(key); got != "" {
			t.Errorf("%s = %q after Isolate, want it cleared", key, got)
		}
	}
}

// TestUnisolated_ReportsEveryKeyThatIsStillTheDevelopersOwn is the predicate
// behind both AssertIsolated and testutil.NewTestServer's startup guard. It is
// a function rather than an inlined loop precisely so the guard — which runs
// in packages that may have no assertion test of their own — and the assertion
// cannot drift apart.
func TestUnisolated_ReportsEveryKeyThatIsStillTheDevelopersOwn(t *testing.T) {
	// Nothing has been isolated in this test's own process yet beyond what
	// this package's tests do to themselves, so establish both directions
	// explicitly rather than depending on ambient state.
	for _, key := range isolatedKeys {
		before := os.Getenv(key)
		t.Cleanup(func() { _ = os.Setenv(key, before) })
		if err := os.Setenv(key, original[key]); err != nil {
			t.Fatalf("Setenv %s: %v", key, err)
		}
	}
	for _, key := range clearedKeys {
		before := os.Getenv(key)
		t.Cleanup(func() { _ = os.Setenv(key, before) })
		if err := os.Setenv(key, "still-set"); err != nil {
			t.Fatalf("Setenv %s: %v", key, err)
		}
	}

	got := Unisolated()
	for _, key := range append(append([]string{}, isolatedKeys...), clearedKeys...) {
		if !containsString(got, key) {
			t.Errorf("Unisolated() = %v, want it to report %q as un-isolated", got, key)
		}
	}

	cleanup, err := Isolate()
	if err != nil {
		t.Fatalf("Isolate: %v", err)
	}
	defer cleanup()

	if got := Unisolated(); len(got) != 0 {
		t.Errorf("Unisolated() = %v after Isolate, want empty", got)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
