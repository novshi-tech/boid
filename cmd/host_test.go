package cmd

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

func TestHostModeEnabled(t *testing.T) {
	cases := []struct {
		env  string
		want bool
	}{
		{"", false},
		{"container", true},
		{"unix-socket", false},
		{"CONTAINER", false}, // case-sensitive, deliberately not tolerant
	}
	for _, c := range cases {
		t.Setenv(boidModeEnv, c.env)
		if got := hostModeEnabled(); got != c.want {
			t.Errorf("BOID_MODE=%q: hostModeEnabled() = %v, want %v", c.env, got, c.want)
		}
	}
}

func TestIsRemoteScope(t *testing.T) {
	cases := []struct {
		scope string
		want  bool
	}{
		{scopeRemote, true},
		{scopeLocal, false},
		{scopeNeutral, false},
		{"", false},
	}
	for _, c := range cases {
		cmd := &cobra.Command{Annotations: map[string]string{scopeAnnotationKey: c.scope}}
		if got := isRemoteScope(cmd); got != c.want {
			t.Errorf("scope=%q: isRemoteScope() = %v, want %v", c.scope, got, c.want)
		}
	}
}

// TestLoadOrCreateCLIToken_GeneratesAndPersists pins the "generate once,
// persist" contract: a first call creates a non-empty token file; a
// second call against the same config dir returns the IDENTICAL value
// (atomicfile.PublishIfAbsent's own "first writer wins, read back" - not
// regenerated every call).
func TestLoadOrCreateCLIToken_GeneratesAndPersists(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	first, err := loadOrCreateCLIToken()
	if err != nil {
		t.Fatalf("loadOrCreateCLIToken (first): %v", err)
	}
	if first == "" {
		t.Fatal("first token is empty")
	}

	second, err := loadOrCreateCLIToken()
	if err != nil {
		t.Fatalf("loadOrCreateCLIToken (second): %v", err)
	}
	if second != first {
		t.Errorf("second call returned a different token: first=%q second=%q, want identical (persist, not regenerate)", first, second)
	}
}

// TestLoadOrCreateCLIToken_FilePermissions pins the 0600 mode requirement
// — a shared secret must not be world/group readable.
func TestLoadOrCreateCLIToken_FilePermissions(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	if _, err := loadOrCreateCLIToken(); err != nil {
		t.Fatalf("loadOrCreateCLIToken: %v", err)
	}

	path := filepath.Join(configHome, "boid", cliTokenFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("cli-token mode = %o, want 0600", perm)
	}
}

func TestFindComposeRoot_EnvOverride(t *testing.T) {
	t.Setenv("BOID_COMPOSE_ROOT", "/some/explicit/root")
	root, err := findComposeRoot()
	if err != nil {
		t.Fatalf("findComposeRoot: %v", err)
	}
	if root != "/some/explicit/root" {
		t.Errorf("root = %q, want %q", root, "/some/explicit/root")
	}
}

func TestFindComposeRoot_WalksUpFromCwd(t *testing.T) {
	t.Setenv("BOID_COMPOSE_ROOT", "")

	repoRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoRoot, "scripts"), 0o755); err != nil {
		t.Fatalf("mkdir scripts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "scripts", "deploy-container.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write deploy-container.sh: %v", err)
	}
	nested := filepath.Join(repoRoot, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })
	if err := os.Chdir(nested); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	root, err := findComposeRoot()
	if err != nil {
		t.Fatalf("findComposeRoot: %v", err)
	}
	// Resolve symlinks (e.g. macOS /tmp -> /private/tmp; harmless on
	// Linux too) before comparing, since os.Getwd can return either form.
	wantRoot, _ := filepath.EvalSymlinks(repoRoot)
	gotRoot, _ := filepath.EvalSymlinks(root)
	if gotRoot != wantRoot {
		t.Errorf("root = %q, want %q", gotRoot, wantRoot)
	}
}

func TestFindComposeRoot_NotFound(t *testing.T) {
	t.Setenv("BOID_COMPOSE_ROOT", "")

	dir := t.TempDir()
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	if _, err := findComposeRoot(); err == nil {
		t.Fatal("expected an error when no scripts/deploy-container.sh is found by walking up from cwd")
	}
}

func TestHostModeHealthy(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	addr := ts.Listener.Addr().String()
	if !hostModeHealthy(context.Background(), addr) {
		t.Error("expected hostModeHealthy() == true against a live /api/health handler")
	}
}

func TestHostModeHealthy_Unreachable(t *testing.T) {
	// Nothing listens on this port (0 lets the OS pick one, then we close
	// immediately, so the port is very likely free but definitely not
	// serving anything by the time we probe it).
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	if hostModeHealthy(context.Background(), addr) {
		t.Error("expected hostModeHealthy() == false against a closed port")
	}
}

func TestFilterEnv(t *testing.T) {
	env := []string{"PATH=/usr/bin", "BOID_CLI_TOKEN=stale-value", "HOME=/home/x"}
	got := filterEnv(env, "BOID_CLI_TOKEN")
	want := []string{"PATH=/usr/bin", "HOME=/home/x"}
	if len(got) != len(want) {
		t.Fatalf("filterEnv() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("filterEnv()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFilterEnv_NoMatch(t *testing.T) {
	env := []string{"PATH=/usr/bin", "HOME=/home/x"}
	got := filterEnv(env, "BOID_CLI_TOKEN")
	if len(got) != 2 {
		t.Errorf("filterEnv() with no match = %v, want unchanged", got)
	}
}
