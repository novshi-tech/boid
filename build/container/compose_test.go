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
		ExtraHosts  []string          `yaml:"extra_hosts"`
	} `yaml:"services"`
	// TopVolumes is the top-level named-volume declaration block (docs/
	// plans/volume-only-daemon.md §論点 d) — distinct from each service's
	// own `Volumes []string` mount-list field above (same YAML key,
	// different nesting level; Go field names need not match).
	TopVolumes map[string]any `yaml:"volumes"`
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

// TestComposeDaemonHasGatewayBrokerEgressAliases pins [Blocker 2, PR7 codex
// review]: the daemon service's boid_internal network membership must also
// carry "boid-gateway", "boid-broker", and "boid-egress" aliases — the DNS
// names internal/server/server.go's gatewayURLFor/composeBrokerServiceName
// and dispatcher.composeEgressServiceName resolve a container-backend
// job's git gateway clone URL and HTTP(S)_PROXY host to. Unlike
// boid-dockerproxy (still a bare alias with nothing reachable behind it as
// of PR7 — see compose.yml's own "NOT yet true of this file" note),
// boid-gateway and boid-egress ARE backed by a real listener as of this
// fix: Server.Start binds the git gateway TLS listener and the
// ProxyManager's default listener on 0.0.0.0 whenever sandbox.backend:
// container is selected.
func TestComposeDaemonHasGatewayBrokerEgressAliases(t *testing.T) {
	doc := loadComposeDoc(t)

	daemon, ok := doc.Services["daemon"]
	if !ok {
		t.Fatal(`compose.yml has no "daemon" service`)
	}
	net, ok := daemon.Networks["boid_internal"]
	if !ok {
		t.Fatal(`daemon service is not a member of the "boid_internal" network`)
	}
	want := map[string]bool{"boid-gateway": false, "boid-broker": false, "boid-egress": false}
	for _, a := range net.Aliases {
		if _, ok := want[a]; ok {
			want[a] = true
		}
	}
	for alias, found := range want {
		if !found {
			t.Errorf("daemon service's boid_internal network aliases = %v, want %q present", net.Aliases, alias)
		}
	}
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

// TestComposeDaemonDataAndConfigAreNamedVolumes pins docs/plans/
// volume-only-daemon.md §論点 d (supersedes/retracts the old Major 10
// source==target host-bind-mount pin this test used to be): BOID_DATA_DIR
// and BOID_CONFIG_DIR are no longer host bind mounts at all — they are
// daemon-owned named volumes (`boid_data`/`boid_config`) mounted at the
// image's fixed, baked-in container paths.
func TestComposeDaemonDataAndConfigAreNamedVolumes(t *testing.T) {
	doc := loadComposeDoc(t)

	daemon, ok := doc.Services["daemon"]
	if !ok {
		t.Fatal(`compose.yml has no "daemon" service`)
	}

	for _, want := range []string{
		"boid_data:/home/boid/.local/share/boid",
		"boid_config:/home/boid/.config/boid",
	} {
		found := false
		for _, v := range daemon.Volumes {
			if v == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("daemon volumes = %v, want %q present", daemon.Volumes, want)
		}
	}

	// Neither retracted host-bind-mount env var may still appear as a
	// mount source — a regression back to Major 10's source==target host
	// path form would defeat the whole point of this PR.
	for _, v := range daemon.Volumes {
		if strings.Contains(v, "BOID_DATA_DIR") || strings.Contains(v, "BOID_CONFIG_DIR") {
			t.Errorf("daemon volumes = %v, contains a retracted BOID_DATA_DIR/BOID_CONFIG_DIR host-path mount", daemon.Volumes)
		}
	}
}

// TestComposeDeclaresNamedDataConfigVolumes pins that boid_data/boid_config
// are actually declared in the top-level `volumes:` block — a compose
// service referencing an undeclared named volume is a config-time error
// under real docker/podman compose, not just a dangling reference this
// package's own narrow YAML model would silently accept.
func TestComposeDeclaresNamedDataConfigVolumes(t *testing.T) {
	doc := loadComposeDoc(t)

	for _, name := range []string{"boid_data", "boid_config"} {
		if _, ok := doc.TopVolumes[name]; !ok {
			t.Errorf("top-level volumes: = %v, want %q declared", doc.TopVolumes, name)
		}
	}
}

// TestComposeDaemonHasXDGEnv pins the other half of §論点 d: cmd/
// start.go's default*Dir/*Path helpers and Go's os.UserConfigDir() must
// resolve to exactly where the boid_data/boid_config named volumes are
// mounted — fixed literals now (no longer host-path-derived, unlike the
// retracted Major 10 design).
func TestComposeDaemonHasXDGEnv(t *testing.T) {
	doc := loadComposeDoc(t)

	daemon, ok := doc.Services["daemon"]
	if !ok {
		t.Fatal(`compose.yml has no "daemon" service`)
	}
	want := map[string]string{
		"XDG_DATA_HOME":   "/home/boid/.local/share",
		"XDG_CONFIG_HOME": "/home/boid/.config",
	}
	for key, wantVal := range want {
		got, ok := daemon.Environment[key]
		if !ok {
			t.Errorf("daemon environment = %v, want %q present", daemon.Environment, key)
			continue
		}
		if got != wantVal {
			t.Errorf("daemon environment[%q] = %q, want %q", key, got, wantVal)
		}
	}
}

// TestComposeDaemonEngineSocketIsParameterized pins docs/plans/
// volume-only-daemon.md §論点 i (案 X): the DooD engine-socket bind's
// SOURCE must be overridable via BOID_DOCKER_SOCK_SRC (podman rootless's
// socket lives at a different path than docker's fixed
// /var/run/docker.sock), while the container-side TARGET stays the fixed
// /var/run/docker.sock every DOCKER_HOST-consuming Go code path already
// expects — no Go-side change needed for engine portability.
func TestComposeDaemonEngineSocketIsParameterized(t *testing.T) {
	doc := loadComposeDoc(t)

	daemon, ok := doc.Services["daemon"]
	if !ok {
		t.Fatal(`compose.yml has no "daemon" service`)
	}
	want := "${BOID_DOCKER_SOCK_SRC:-/var/run/docker.sock}:/var/run/docker.sock"
	for _, v := range daemon.Volumes {
		if v == want {
			return
		}
	}
	t.Errorf("daemon volumes = %v, want %q present", daemon.Volumes, want)
}

// TestComposeDaemonHasXDGRuntimeDirEnv pins the PR9 fix for a real gap the
// e2e-container job's first real-docker run surfaced: XDG_RUNTIME_DIR was
// entirely missing from the PR6 skeleton's environment: block, so the
// daemon's own internal/client.DefaultSocketPath() fallback (`cmd/
// start.go`'s default when no --socket-path flag is given — exactly what
// `command: ["start"]` uses) never resolved to the bind-mounted, host-
// visible BOID_RUNTIME_DIR this compose file otherwise carefully sets up —
// breaking both the "server socket の host 同一 path bind (相互排他)"
// contract (§決定4) BOID_RUNTIME_DIR's own header comment describes and
// every host-side CLI/E2E caller expecting to reach this daemon's socket.
func TestComposeDaemonHasXDGRuntimeDirEnv(t *testing.T) {
	doc := loadComposeDoc(t)

	daemon, ok := doc.Services["daemon"]
	if !ok {
		t.Fatal(`compose.yml has no "daemon" service`)
	}
	got, ok := daemon.Environment["XDG_RUNTIME_DIR"]
	if !ok {
		t.Fatalf("daemon environment = %v, want %q present", daemon.Environment, "XDG_RUNTIME_DIR")
	}
	if got != "${BOID_RUNTIME_DIR}" {
		t.Errorf(`daemon environment["XDG_RUNTIME_DIR"] = %q, want "${BOID_RUNTIME_DIR}" (must match the socket bind mount source)`, got)
	}
}

// TestComposeDaemonHasHostGatewayExtraHost pins the PR9 addition
// e2e/run-container.sh's fixture git upstream reachability depends on: the
// daemon service must resolve "host.docker.internal" to the docker
// bridge-gateway address (Docker's "host-gateway" extra_hosts special
// value), the other half of the host<->container reachability trick this
// e2e job's own /etc/hosts line completes — see compose.yml's own
// extra_hosts comment for the full rationale.
func TestComposeDaemonHasHostGatewayExtraHost(t *testing.T) {
	doc := loadComposeDoc(t)

	daemon, ok := doc.Services["daemon"]
	if !ok {
		t.Fatal(`compose.yml has no "daemon" service`)
	}
	want := "host.docker.internal:host-gateway"
	for _, h := range daemon.ExtraHosts {
		if h == want {
			return
		}
	}
	t.Errorf("daemon extra_hosts = %v, want %q present", daemon.ExtraHosts, want)
}

// TestComposeDaemonHasXDGStateHomeEnv pins the PR9 debugging fix: without
// XDG_STATE_HOME, daemon.LogFilePath() (internal/daemon/daemon.go) resolves
// boid.log into this container's own ephemeral writable layer, and since
// runDaemonChild redirects stdin/stdout/stderr to that file as literally
// its first action, `docker logs` can never show anything for this
// service — not even a startup crash. Pointing XDG_STATE_HOME at the
// already-bind-mounted BOID_RUNTIME_DIR makes boid.log land at a
// host-visible path instead, readable even after the container exits.
func TestComposeDaemonHasXDGStateHomeEnv(t *testing.T) {
	doc := loadComposeDoc(t)

	daemon, ok := doc.Services["daemon"]
	if !ok {
		t.Fatal(`compose.yml has no "daemon" service`)
	}
	got, ok := daemon.Environment["XDG_STATE_HOME"]
	if !ok {
		t.Fatalf("daemon environment = %v, want %q present", daemon.Environment, "XDG_STATE_HOME")
	}
	if got != "${BOID_RUNTIME_DIR}" {
		t.Errorf(`daemon environment["XDG_STATE_HOME"] = %q, want "${BOID_RUNTIME_DIR}" (must be a directory already bind-mounted, so no new volume entry is needed)`, got)
	}
}

// TestComposeDaemonHasLogStdoutEnv pins the PR9 fix for the actual
// container startup crash the e2e-container job's debugging trail found
// (docs/plans/phase6-cutover-followups.md): daemon.RedirectToLogRotating's
// self-pipe dup2 dance does not survive this container's PID1
// (docker-init/tini) setup — the daemon reproducibly died (SIGPIPE, exit
// 141) within ~150ms of starting. BOID_LOG_STDOUT (daemon.
// ShouldLogToStdout's own doc comment) skips that redirect entirely.
func TestComposeDaemonHasLogStdoutEnv(t *testing.T) {
	doc := loadComposeDoc(t)

	daemon, ok := doc.Services["daemon"]
	if !ok {
		t.Fatal(`compose.yml has no "daemon" service`)
	}
	got, ok := daemon.Environment["BOID_LOG_STDOUT"]
	if !ok {
		t.Fatalf("daemon environment = %v, want %q present", daemon.Environment, "BOID_LOG_STDOUT")
	}
	if got != "1" {
		t.Errorf(`daemon environment["BOID_LOG_STDOUT"] = %q, want "1"`, got)
	}
}
