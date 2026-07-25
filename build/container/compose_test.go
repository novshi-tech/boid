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
		Ports       []string          `yaml:"ports"`
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

// TestComposeDaemonHasSingleStateVolume pins docs/plans/
// volume-only-daemon.md §論点 d / §目的's "named volume に 1 本化" invariant
// (fix for [Major 6, PR829 round 1 codex review]: the original PR-1a
// implementation split state across TWO volumes — boid_data and
// boid_config — which lets an operator following the plan doc's own
// backup contract (§論点 g: "boid workspace export --all が唯一の正式 backup
// 経路", read alongside the volume itself as a disposable cache) back up
// or restore one and silently miss the other). A single volume mounted at
// /home/boid (the image's own $HOME, build/container/Dockerfile's
// `useradd --home-dir /home/boid`) covers both
// XDG_DATA_HOME=/home/boid/.local/share and
// XDG_CONFIG_HOME=/home/boid/.config as ordinary subdirectories — no
// separate mount needed for each.
func TestComposeDaemonHasSingleStateVolume(t *testing.T) {
	doc := loadComposeDoc(t)

	daemon, ok := doc.Services["daemon"]
	if !ok {
		t.Fatal(`compose.yml has no "daemon" service`)
	}

	want := "boid_state:/home/boid"
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

	// Neither the retracted host-bind-mount env vars nor the retracted
	// two-volume split may still appear as a mount source — a regression
	// back to either shape would defeat the point of this fix.
	for _, v := range daemon.Volumes {
		if strings.Contains(v, "BOID_DATA_DIR") || strings.Contains(v, "BOID_CONFIG_DIR") {
			t.Errorf("daemon volumes = %v, contains a retracted BOID_DATA_DIR/BOID_CONFIG_DIR host-path mount", daemon.Volumes)
		}
		if strings.HasPrefix(v, "boid_data:") || strings.HasPrefix(v, "boid_config:") {
			t.Errorf("daemon volumes = %v, contains a retracted split boid_data/boid_config mount (want single boid_state)", daemon.Volumes)
		}
	}
}

// TestComposeDeclaresSingleStateVolume pins that boid_state is actually
// declared in the top-level `volumes:` block (a compose service
// referencing an undeclared named volume is a config-time error under
// real docker/podman compose, not just a dangling reference this
// package's own narrow YAML model would silently accept), and that the
// retracted boid_data/boid_config pair is gone from that block too.
func TestComposeDeclaresSingleStateVolume(t *testing.T) {
	doc := loadComposeDoc(t)

	if _, ok := doc.TopVolumes["boid_state"]; !ok {
		t.Errorf("top-level volumes: = %v, want %q declared", doc.TopVolumes, "boid_state")
	}
	for _, retracted := range []string{"boid_data", "boid_config"} {
		if _, ok := doc.TopVolumes[retracted]; ok {
			t.Errorf("top-level volumes: = %v, still declares retracted %q (want consolidated into boid_state)", doc.TopVolumes, retracted)
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

// TestComposeDaemonHasCLITokenEnv pins the PR-3 Option 4 host-mode
// redesign (docs/plans/volume-only-daemon.md §論点c, nose directive
// 2026-07-25): the daemon service must interpolate BOID_CLI_TOKEN from
// its own launching environment (deploy-container.sh, invoked by cmd/
// host.go with the value already exported) — without this, the compose
// container never sees the token cmd/host.go generated/read on the host,
// and internal/api/auth.NewCLITokenAuthMiddleware fails closed (no
// listener bound at all — see server.Config.CLIAddr's own doc comment),
// silently stranding every host-mode CLI dispatch.
func TestComposeDaemonHasCLITokenEnv(t *testing.T) {
	doc := loadComposeDoc(t)

	daemon, ok := doc.Services["daemon"]
	if !ok {
		t.Fatal(`compose.yml has no "daemon" service`)
	}
	got, ok := daemon.Environment["BOID_CLI_TOKEN"]
	if !ok {
		t.Fatalf("daemon environment = %v, want %q present", daemon.Environment, "BOID_CLI_TOKEN")
	}
	if got != "${BOID_CLI_TOKEN:-}" {
		t.Errorf(`daemon environment["BOID_CLI_TOKEN"] = %q, want "${BOID_CLI_TOKEN:-}" (pass-through, optional for a manual compose up outside host mode)`, got)
	}
}

// TestComposeDaemonPublishesCLIPortOnLoopbackOnly pins the CLI listener's
// host-side publish: 127.0.0.1:8442:8442, matching client.DefaultCLIAddr()
// — a bare "8442:8442" or "0.0.0.0:8442:8442" would expose the token-only
// (no TLS) listener to every other interface on the host, not just the
// same-host `boid` CLI process host mode is designed for.
func TestComposeDaemonPublishesCLIPortOnLoopbackOnly(t *testing.T) {
	doc := loadComposeDoc(t)

	daemon, ok := doc.Services["daemon"]
	if !ok {
		t.Fatal(`compose.yml has no "daemon" service`)
	}
	want := "127.0.0.1:8442:8442"
	for _, p := range daemon.Ports {
		if p == want {
			return
		}
	}
	t.Errorf("daemon service ports = %v, want %q present", daemon.Ports, want)
}

// TestComposeDaemonUsesBridgeNetworking pins the REVERT of PR-3 round-1's
// `network_mode: host` pivot (docs/plans/volume-only-daemon.md §論点c,
// nose directive 2026-07-25 Option 4 redesign): host networking broke
// daemon<->job docker network attachment (a host-networked container
// cannot join any other docker network at all — a hard engine
// constraint), so Option 4 goes back to ordinary bridge networking + an
// explicit loopback-only port publish for the one listener a HOST process
// needs (TestComposeDaemonPublishesCLIPortOnLoopbackOnly above) instead of
// exposing the daemon's entire host network namespace.
func TestComposeDaemonUsesBridgeNetworking(t *testing.T) {
	data, err := os.ReadFile("compose.yml")
	if err != nil {
		t.Fatalf("read compose.yml: %v", err)
	}
	if strings.Contains(string(data), "network_mode: host") {
		t.Error("compose.yml still contains \"network_mode: host\" — Option 4 redesign reverts this back to bridge networking")
	}

	doc := loadComposeDoc(t)
	daemon, ok := doc.Services["daemon"]
	if !ok {
		t.Fatal(`compose.yml has no "daemon" service`)
	}
	if _, ok := daemon.Networks["boid_internal"]; !ok {
		t.Error(`daemon service is not a member of the "boid_internal" network (bridge networking must be restored)`)
	}
}
