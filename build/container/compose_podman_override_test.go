package container

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// compose_podman_override_test.go covers compose.podman.override.yml, the
// engine-specific layer scripts/deploy-container.sh stacks on top of
// compose.yml when (and only when) it detects a ROOTLESS PODMAN engine.
//
// Why it cannot live in compose.yml itself: `userns_mode: keep-id` is a
// podman-only value. docker compose rejects it outright ("Value 'keep-id'
// is not valid"), so putting it in the base file would break every docker
// deployment — including the e2e-container CI job — to fix podman.
//
// Why it is needed at all (2026-07-26 volume-only dogfood): rootless podman
// maps the container's uid 1000 to a host SUBUID rather than to the host
// user who owns everything the compose file bind-mounts in. The daemon
// container came up unable to read ANY of them, dying in sequence on
// /run/user/1000/runtimes (permission denied), the engine socket
// (permission denied connecting to the docker API), and finally its own
// broker socket bind — `listen unix /run/user/1000/boid-broker.sock: bind:
// permission denied`. This is the same class of failure the 2026-07-24
// Phase 6 dogfood hit (docs/plans/volume-only-daemon.md's incident writeup,
// "wall 2": podman rootless subuid mapping making 0600 host files
// unreadable); the volume-only redesign removed it for daemon STATE by
// moving that onto a named volume, but BOID_RUNTIME_DIR remains a host bind
// mount by design (see compose.yml's own "Persistence" header comment —
// internal/server/wire.go's hostVisibleRuntimesDirFor needs a path the HOST
// engine can resolve for DooD sibling mounts), so the mapping still has to
// line up.
//
// The job-container half of the same fix lives in
// internal/dispatcher/container_backend_userns.go, which detects the engine
// through the docker API rather than a shell check.

func loadPodmanOverrideDoc(t *testing.T) composeDoc {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("compose.podman.override.yml"))
	if err != nil {
		t.Fatalf("read compose.podman.override.yml: %v", err)
	}
	var doc composeDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse compose.podman.override.yml: %v", err)
	}
	return doc
}

// TestPodmanOverrideSetsKeepIDUserns is the whole point of the file: the
// daemon service must request keep-id, or a rootless-podman deploy cannot
// read its own bind mounts.
func TestPodmanOverrideSetsKeepIDUserns(t *testing.T) {
	doc := loadPodmanOverrideDoc(t)

	daemon, ok := doc.Services["daemon"]
	if !ok {
		t.Fatal("compose.podman.override.yml has no `daemon` service (it must override the base file's service by the same name)")
	}
	if daemon.UsernsMode != "keep-id" {
		t.Errorf("daemon.userns_mode = %q, want %q", daemon.UsernsMode, "keep-id")
	}
}

// TestPodmanOverrideOverridesNothingElse pins the override's minimality. An
// override file is merged key-by-key over the base, so anything else
// declared here silently becomes a SECOND source of truth for that setting
// — one that only takes effect on podman, and only for whoever remembers to
// keep both files in sync. Every setting that is not engine-specific
// belongs in compose.yml alone.
func TestPodmanOverrideOverridesNothingElse(t *testing.T) {
	doc := loadPodmanOverrideDoc(t)

	if doc.Name != "" {
		t.Errorf("override declares a project name (%q); it must inherit the base file's", doc.Name)
	}
	if len(doc.TopVolumes) != 0 {
		t.Errorf("override declares top-level volumes (%v); it must inherit the base file's", doc.TopVolumes)
	}
	if len(doc.Services) != 1 {
		t.Errorf("override declares %d services, want exactly 1 (daemon)", len(doc.Services))
	}

	daemon := doc.Services["daemon"]
	if len(daemon.Environment) != 0 {
		t.Errorf("override sets environment %v; only userns_mode is engine-specific", daemon.Environment)
	}
	if len(daemon.Volumes) != 0 {
		t.Errorf("override sets volumes %v; only userns_mode is engine-specific", daemon.Volumes)
	}
	if len(daemon.Ports) != 0 {
		t.Errorf("override sets ports %v; only userns_mode is engine-specific", daemon.Ports)
	}
	if len(daemon.Networks) != 0 {
		t.Errorf("override sets networks %v; only userns_mode is engine-specific", daemon.Networks)
	}
	if len(daemon.GroupAdd) != 0 {
		t.Errorf("override sets group_add %v; only userns_mode is engine-specific", daemon.GroupAdd)
	}
}

// TestBaseComposeHasNoUsernsMode is the docker-side guard: keep-id in the
// base file would fail every docker compose deployment at parse time.
func TestBaseComposeHasNoUsernsMode(t *testing.T) {
	doc := loadComposeDoc(t)

	if got := doc.Services["daemon"].UsernsMode; got != "" {
		t.Errorf("compose.yml daemon.userns_mode = %q, want empty — podman-only values belong in compose.podman.override.yml", got)
	}
}

// TestPodmanOverrideIsEmbedded pins the override into the same go:embed set
// as compose.yml and Dockerfile. cmd/host.go's deployFromEmbeddedAssets
// extracts these to run the deploy script without a repo checkout; an
// override missing from that set would make the no-checkout fallback fail
// on exactly the engine it was written for, and only there.
func TestPodmanOverrideIsEmbedded(t *testing.T) {
	data, err := Assets.ReadFile("compose.podman.override.yml")
	if err != nil {
		t.Fatalf("read embedded compose.podman.override.yml: %v", err)
	}
	onDisk, err := os.ReadFile("compose.podman.override.yml")
	if err != nil {
		t.Fatalf("read compose.podman.override.yml from disk: %v", err)
	}
	if string(data) != string(onDisk) {
		t.Error("embedded compose.podman.override.yml differs from the on-disk file")
	}
}
