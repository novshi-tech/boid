// Package container holds structural sanity checks for
// build/container/compose.yml — go test coverage for a YAML skeleton that
// has no other Go source validating it
// (docs/plans/phase6-container-backend.md §PR6). These are NOT a
// substitute for `podman-compose config` / `docker compose config`
// (compose.yml's own header comment: "Validated with podman-compose
// config... CI is the source of truth going forward") — they exist so a
// structural regression (e.g. a dropped network alias) fails
// `go test ./...` immediately, without needing docker/podman installed.
package container

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// composeDoc is the minimal shape this file's tests need out of
// compose.yml — deliberately narrow (not a full compose-spec model) so it
// only breaks when something these tests actually assert about changes.
type composeDoc struct {
	Services map[string]struct {
		Networks map[string]struct {
			Aliases []string `yaml:"aliases"`
		} `yaml:"networks"`
		GroupAdd    []string          `yaml:"group_add"`
		Volumes     []string          `yaml:"volumes"`
		Environment map[string]string `yaml:"environment"`
	} `yaml:"services"`
}

func loadComposeDoc(t *testing.T) composeDoc {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("compose.yml"))
	if err != nil {
		t.Fatalf("read compose.yml: %v", err)
	}
	var doc composeDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse compose.yml: %v", err)
	}
	return doc
}

// TestComposeDaemonHasDockerProxyAlias pins Blocker 3 (PR6 codex review):
// the daemon service's boid_internal network membership must carry a
// "boid-dockerproxy" alias — the DNS name a container-backend job's
// DOCKER_HOST env (internal/dispatcher/container_backend.go's
// withDockerTLSEnv) is set to. Without this alias, that env var points at
// a name compose never declares, so a container-backend job would fail
// DNS resolution outright trying to reach it — see compose.yml's own "NOT
// yet true of this file" note for what this alias does, and does not (the
// listener itself is not yet reachable — that's docs/plans/
// phase6-container-backend.md §PR9's e2e-container job), fix.
func TestComposeDaemonHasDockerProxyAlias(t *testing.T) {
	doc := loadComposeDoc(t)

	daemon, ok := doc.Services["daemon"]
	if !ok {
		t.Fatal(`compose.yml has no "daemon" service`)
	}
	net, ok := daemon.Networks["boid_internal"]
	if !ok {
		t.Fatal(`daemon service is not a member of the "boid_internal" network`)
	}
	for _, a := range net.Aliases {
		if a == "boid-dockerproxy" {
			return
		}
	}
	t.Errorf(`daemon service's boid_internal network aliases = %v, want "boid-dockerproxy" present`, net.Aliases)
}

// TestComposeDaemonHasDockerGroupAdd pins Major 9 (PR6 codex review): the
// non-root daemon process (user: 1000:1000 by default) needs supplementary
// membership in the host's docker group to open /var/run/docker.sock
// (DooD) — without group_add, every docker API call from inside the
// container fails with a permission error.
func TestComposeDaemonHasDockerGroupAdd(t *testing.T) {
	doc := loadComposeDoc(t)

	daemon, ok := doc.Services["daemon"]
	if !ok {
		t.Fatal(`compose.yml has no "daemon" service`)
	}
	if len(daemon.GroupAdd) == 0 {
		t.Fatal(`daemon service has no group_add entries, want a DOCKER_GID entry so the non-root daemon can open /var/run/docker.sock`)
	}
	for _, g := range daemon.GroupAdd {
		if strings.Contains(g, "DOCKER_GID") {
			return
		}
	}
	t.Errorf(`daemon service group_add = %v, want an entry referencing ${DOCKER_GID:-...}`, daemon.GroupAdd)
}

// TestComposeDaemonDataAndConfigVolumesSourceEqualsTarget pins Major 10
// (PR6 codex review): BOID_DATA_DIR/BOID_CONFIG_DIR must bind mount
// source == target, not remap onto some container-internal path — see
// compose.yml's own "Persistence" header comment for why (a DooD sibling
// container's mount Source must be a path the HOST filesystem actually
// has; a daemon-internal remap silently breaks the moment this daemon
// constructs a mount from an absolute path it computed itself).
func TestComposeDaemonDataAndConfigVolumesSourceEqualsTarget(t *testing.T) {
	doc := loadComposeDoc(t)

	daemon, ok := doc.Services["daemon"]
	if !ok {
		t.Fatal(`compose.yml has no "daemon" service`)
	}

	for _, want := range []string{"${BOID_DATA_DIR}", "${BOID_CONFIG_DIR}"} {
		wantEntry := want + ":" + want
		found := false
		for _, v := range daemon.Volumes {
			if v == wantEntry {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("daemon volumes = %v, want %q present (source == target)", daemon.Volumes, wantEntry)
		}
	}
}

// TestComposeDaemonHasXDGEnv pins the other half of Major 10: cmd/
// start.go's default*Dir/*Path helpers and Go's os.UserConfigDir() must
// resolve to the exact bind-mounted BOID_DATA_DIR/BOID_CONFIG_DIR
// (source == target above) rather than the image's own baked-in $HOME —
// achieved by passing XDG_DATA_HOME/XDG_CONFIG_HOME into the container
// explicitly.
func TestComposeDaemonHasXDGEnv(t *testing.T) {
	doc := loadComposeDoc(t)

	daemon, ok := doc.Services["daemon"]
	if !ok {
		t.Fatal(`compose.yml has no "daemon" service`)
	}
	for _, key := range []string{"XDG_DATA_HOME", "XDG_CONFIG_HOME"} {
		if _, ok := daemon.Environment[key]; !ok {
			t.Errorf("daemon environment = %v, want %q present", daemon.Environment, key)
		}
	}
}
