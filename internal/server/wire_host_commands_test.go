package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func writeTestKitYAML(t *testing.T, kitsDir, name, content string) {
	t.Helper()
	dir := filepath.Join(kitsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir kit dir %q: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kit.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write kit.yaml: %v", err)
	}
}

// TestBuildProjectStore_HostCommandsCollisionAbortsStartup pins PR2's
// preflight (docs/plans/workspace-db-consolidation.md decision 9): if two
// installed kits declare the same host_command name with different
// definitions, buildProjectStore (and therefore daemon startup) must abort
// rather than silently picking one.
func TestBuildProjectStore_HostCommandsCollisionAbortsStartup(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	kitsDir := t.TempDir()
	writeTestKitYAML(t, kitsDir, "kit-a", "host_commands:\n  gh:\n    allow: [pr]\n")
	writeTestKitYAML(t, kitsDir, "kit-b", "host_commands:\n  gh:\n    allow: [issue]\n")

	d := openTestDB(t)
	repo := orchestrator.NewProjectRepository(d.Conn)

	cfg := Config{DBPath: ":memory:", KitsDir: kitsDir}
	_, _, err := buildProjectStore(cfg, repo)
	if err == nil {
		t.Fatal("expected buildProjectStore to abort on conflicting host_commands definitions")
	}
	if !strings.Contains(err.Error(), "gh") {
		t.Errorf("error should mention the conflicting command name: %v", err)
	}
	if !strings.Contains(err.Error(), "kit-a") || !strings.Contains(err.Error(), "kit-b") {
		t.Errorf("error should mention both kit directories: %v", err)
	}
}

// TestBuildProjectStore_AggregatesHostCommandsAndWritesConfig pins the
// success path: buildProjectStore aggregates every installed kit's
// host_commands, writes the aggregate to DefaultHostCommandsPath(), and the
// written file round-trips back to the same map via LoadHostCommandsConfig.
func TestBuildProjectStore_AggregatesHostCommandsAndWritesConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	kitsDir := t.TempDir()
	writeTestKitYAML(t, kitsDir, "gh-kit", "host_commands:\n  gh:\n    allow: [pr, issue]\n")
	writeTestKitYAML(t, kitsDir, "aws-kit", "host_commands:\n  aws:\n    allow: [s3]\n")

	d := openTestDB(t)
	repo := orchestrator.NewProjectRepository(d.Conn)

	cfg := Config{DBPath: ":memory:", KitsDir: kitsDir}
	_, hostCommands, err := buildProjectStore(cfg, repo)
	if err != nil {
		t.Fatalf("buildProjectStore: %v", err)
	}

	// The aggregated map returned to the caller (and stashed on Server) must
	// itself contain both commands, not just the file on disk.
	if _, ok := hostCommands["gh"]; !ok {
		t.Errorf("expected 'gh' command in returned map, got %v", hostCommands)
	}
	if _, ok := hostCommands["aws"]; !ok {
		t.Errorf("expected 'aws' command in returned map, got %v", hostCommands)
	}

	path, err := orchestrator.DefaultHostCommandsPath()
	if err != nil {
		t.Fatalf("DefaultHostCommandsPath: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected host_commands.yaml to be written: %v", err)
	}

	got, err := orchestrator.LoadHostCommandsConfig(path)
	if err != nil {
		t.Fatalf("LoadHostCommandsConfig: %v", err)
	}
	if _, ok := got["gh"]; !ok {
		t.Errorf("expected 'gh' command in written config, got %v", got)
	}
	if _, ok := got["aws"]; !ok {
		t.Errorf("expected 'aws' command in written config, got %v", got)
	}
}

// TestBuildProjectStore_NoKitsDirWritesEmptyHostCommandsConfig pins the
// degraded-but-fine case (no KitsDir configured, e.g. many existing tests'
// Config{DBPath: ":memory:"}): the preflight must not error, and must still
// produce a (possibly empty) host_commands.yaml so the daemon's read path is
// exercised even with zero kits installed.
func TestBuildProjectStore_NoKitsDirWritesEmptyHostCommandsConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	d := openTestDB(t)
	repo := orchestrator.NewProjectRepository(d.Conn)

	cfg := Config{DBPath: ":memory:"}
	if _, _, err := buildProjectStore(cfg, repo); err != nil {
		t.Fatalf("buildProjectStore: %v", err)
	}

	path, err := orchestrator.DefaultHostCommandsPath()
	if err != nil {
		t.Fatalf("DefaultHostCommandsPath: %v", err)
	}
	got, err := orchestrator.LoadHostCommandsConfig(path)
	if err != nil {
		t.Fatalf("LoadHostCommandsConfig: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty host_commands config, got %v", got)
	}
}

// TestServer_HostCommandsReturnsSnapshot pins the MINOR fix: Server.HostCommands()
// must return an independent copy each call, not the daemon's live internal
// map, so a caller mutating the returned value (or a slice/map nested inside
// one of its HostCommandSpec entries) can never corrupt daemon state — and,
// looking ahead to PR4's reload API, can never race against a concurrent
// reload either.
func TestServer_HostCommandsReturnsSnapshot(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	kitsDir := t.TempDir()
	writeTestKitYAML(t, kitsDir, "gh-kit", "host_commands:\n  gh:\n    allow: [pr]\n")

	srv, err := New(Config{DBPath: ":memory:", SocketPath: filepath.Join(t.TempDir(), "boid.sock"), KitsDir: kitsDir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first := srv.HostCommands()
	gh, ok := first["gh"]
	if !ok {
		t.Fatalf("expected 'gh' command, got %v", first)
	}

	// Mutate the returned map and the nested slice inside one of its entries.
	first["aws"] = orchestrator.HostCommandSpec{Allow: []string{"s3"}}
	gh.Allow[0] = "mutated"
	first["gh"] = gh

	second := srv.HostCommands()
	if _, ok := second["aws"]; ok {
		t.Error("mutating the first snapshot's top-level map leaked into a later call")
	}
	if second["gh"].Allow[0] != "pr" {
		t.Errorf("mutating the first snapshot's nested slice leaked into a later call: %v", second["gh"].Allow)
	}
}
