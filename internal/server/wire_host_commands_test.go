package server

import (
	"context"
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
	_, _, err := buildProjectStore(cfg, d.Conn, repo)
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
	_, hostCommands, err := buildProjectStore(cfg, d.Conn, repo)
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
	if _, _, err := buildProjectStore(cfg, d.Conn, repo); err != nil {
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

// TestBuildProjectStore_WiresExpandedHostCommandsForDispatch pins MAJOR 2
// (codex review): the raw aggregated host_commands map (returned to the
// caller here, and what Server.HostCommands() later snapshots) must keep
// its ${VAR} placeholders unexpanded — the PR2 contract — while the
// store's own dispatch-facing copy (wired via SetHostCommands, and
// resolved into a project's meta by GetWithWorkspace) must be expanded, so
// a `path: ${HOME}/bin/gh`-shaped host_command actually resolves to a real
// path by the time dispatch would try exec.LookPath on it.
func TestBuildProjectStore_WiresExpandedHostCommandsForDispatch(t *testing.T) {
	xdgConfigHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgConfigHome)
	t.Setenv("GH_HOME_FOR_TEST", "/opt/gh-test-home")

	kitsDir := t.TempDir()
	writeTestKitYAML(t, kitsDir, "gh-kit", "host_commands:\n  gh:\n    path: ${GH_HOME_FOR_TEST}/bin/gh\n")
	writeTestWorkspaceYAML(t, xdgConfigHome, "team-expand", "kits:\n  - gh-kit\n")

	projectDir := t.TempDir()
	setupProjectYAML(t, projectDir, "proj-expand", "build")

	d := openTestDB(t)
	repo := orchestrator.NewProjectRepository(d.Conn)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-expand", WorkDir: projectDir}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-expand", "team-expand"); err != nil {
		t.Fatalf("SetProjectWorkspace: %v", err)
	}

	cfg := Config{DBPath: ":memory:", KitsDir: kitsDir}
	store, rawHostCommands, err := buildProjectStore(cfg, d.Conn, repo)
	if err != nil {
		t.Fatalf("buildProjectStore: %v", err)
	}

	// Raw snapshot (Server.HostCommands()' contract) stays unexpanded.
	if rawHostCommands["gh"].Path != "${GH_HOME_FOR_TEST}/bin/gh" {
		t.Errorf("raw hostCommands Path = %q, want unexpanded ${GH_HOME_FOR_TEST}/bin/gh", rawHostCommands["gh"].Path)
	}

	// Dispatch-facing path (via GetWithWorkspace, fed by the expanded
	// store.hostCommands) is expanded.
	meta, err := store.GetWithWorkspace(context.Background(), "proj-expand")
	if err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}
	gh, ok := meta.HostCommands["gh"]
	if !ok {
		t.Fatalf("expected 'gh' host_command hydrated, got %+v", meta.HostCommands)
	}
	if gh.Path != "/opt/gh-test-home/bin/gh" {
		t.Errorf("dispatch-facing Path = %q, want expanded /opt/gh-test-home/bin/gh", gh.Path)
	}
}

// TestBuildProjectStore_PreservesExistingHostCommandsConfig pins MAJOR 3
// (codex review): once host_commands.yaml exists on disk (written by the
// first daemon startup's migration), a later daemon startup must not
// clobber a hand edit to it by re-aggregating from kitsDir and rewriting —
// the aggregated config is meant to be the authority once cutover has
// happened (docs/plans/workspace-db-consolidation.md), editable by hand.
func TestBuildProjectStore_PreservesExistingHostCommandsConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	kitsDir := t.TempDir()
	writeTestKitYAML(t, kitsDir, "gh-kit", "host_commands:\n  gh:\n    allow: [pr]\n")

	d := openTestDB(t)
	repo := orchestrator.NewProjectRepository(d.Conn)

	cfg := Config{DBPath: ":memory:", KitsDir: kitsDir}
	if _, _, err := buildProjectStore(cfg, d.Conn, repo); err != nil {
		t.Fatalf("first buildProjectStore: %v", err)
	}

	// Hand-edit the aggregated config after the first (committed) startup:
	// add a command that no installed kit declares.
	hostCommandsPath, err := orchestrator.DefaultHostCommandsPath()
	if err != nil {
		t.Fatalf("DefaultHostCommandsPath: %v", err)
	}
	if err := orchestrator.WriteHostCommandsConfig(hostCommandsPath, map[string]orchestrator.HostCommandSpec{
		"gh":              {Allow: []string{"pr"}},
		"hand-edited-cmd": {Path: "/usr/local/bin/custom"},
	}); err != nil {
		t.Fatalf("hand-edit WriteHostCommandsConfig: %v", err)
	}

	// Second startup (simulating a daemon restart against the same DB):
	// migration is already committed and is a no-op, so only the MAJOR 3
	// conditional stands between the hand edit and a clobber.
	_, hostCommands, err := buildProjectStore(cfg, d.Conn, repo)
	if err != nil {
		t.Fatalf("second buildProjectStore: %v", err)
	}
	if _, ok := hostCommands["hand-edited-cmd"]; !ok {
		t.Errorf("hand edit was clobbered by a fresh aggregation on restart, got %v", hostCommands)
	}
}

// TestServer_ReloadHostCommands_PicksUpHandEdit pins Step G
// (docs/plans/workspace-db-consolidation.md PR4, `boid host-commands
// reload` / POST /api/host_commands/reload): re-reading
// ~/.config/boid/host_commands.yaml after a hand edit must be visible both
// through Server.HostCommands() (the raw snapshot) and through dispatch-time
// hydration (GetWithWorkspace, fed by the store's expanded copy) — without
// requiring a daemon restart.
func TestServer_ReloadHostCommands_PicksUpHandEdit(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	kitsDir := t.TempDir()
	writeTestKitYAML(t, kitsDir, "gh-kit", "host_commands:\n  gh:\n    allow: [pr]\n")

	srv, err := New(Config{DBPath: ":memory:", SocketPath: filepath.Join(t.TempDir(), "boid.sock"), KitsDir: kitsDir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	before := srv.HostCommands()
	if _, ok := before["aws"]; ok {
		t.Fatalf("expected no 'aws' command before reload, got %v", before)
	}

	hostCommandsPath, err := orchestrator.DefaultHostCommandsPath()
	if err != nil {
		t.Fatalf("DefaultHostCommandsPath: %v", err)
	}
	if err := orchestrator.WriteHostCommandsConfig(hostCommandsPath, map[string]orchestrator.HostCommandSpec{
		"gh":  {Allow: []string{"pr"}},
		"aws": {Allow: []string{"s3"}},
	}); err != nil {
		t.Fatalf("hand-edit WriteHostCommandsConfig: %v", err)
	}

	if err := srv.ReloadHostCommands(); err != nil {
		t.Fatalf("ReloadHostCommands: %v", err)
	}

	after := srv.HostCommands()
	if _, ok := after["aws"]; !ok {
		t.Errorf("expected 'aws' command after reload, got %v", after)
	}
}

// TestBuildProjectStore_RegeneratesHostCommandsConfigIfMissing pins the
// other half of MAJOR 3: if host_commands.yaml is genuinely missing (e.g.
// deleted by hand) on a restart where the migration is already committed
// (and therefore will not rewrite it), buildProjectStore's own fallback
// must still self-heal by regenerating it from the installed kits, rather
// than leaving the daemon with an empty/absent aggregate forever.
func TestBuildProjectStore_RegeneratesHostCommandsConfigIfMissing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	kitsDir := t.TempDir()
	writeTestKitYAML(t, kitsDir, "gh-kit", "host_commands:\n  gh:\n    allow: [pr]\n")

	d := openTestDB(t)
	repo := orchestrator.NewProjectRepository(d.Conn)

	cfg := Config{DBPath: ":memory:", KitsDir: kitsDir}
	if _, _, err := buildProjectStore(cfg, d.Conn, repo); err != nil {
		t.Fatalf("first buildProjectStore: %v", err)
	}

	hostCommandsPath, err := orchestrator.DefaultHostCommandsPath()
	if err != nil {
		t.Fatalf("DefaultHostCommandsPath: %v", err)
	}
	if err := os.Remove(hostCommandsPath); err != nil {
		t.Fatalf("remove host_commands.yaml: %v", err)
	}

	_, hostCommands, err := buildProjectStore(cfg, d.Conn, repo)
	if err != nil {
		t.Fatalf("second buildProjectStore (after deletion): %v", err)
	}
	if _, ok := hostCommands["gh"]; !ok {
		t.Errorf("expected 'gh' to be regenerated from kitsDir after deletion, got %v", hostCommands)
	}
	if _, err := os.Stat(hostCommandsPath); err != nil {
		t.Errorf("expected host_commands.yaml to be re-written to disk: %v", err)
	}
}
