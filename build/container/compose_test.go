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
