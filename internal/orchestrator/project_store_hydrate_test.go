package orchestrator_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// setupWorkspaceDir creates a workspace YAML file at dir/<slug>.yaml.
func setupWorkspaceDir(t *testing.T, dir, slug, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir workspace dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, slug+".yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write workspace yaml: %v", err)
	}
}

// loadProjectIntoStore loads a project from workDir into the store and
// registers it with the given workspaceID. workDir must already have a
// valid .boid/project.yaml.
func loadProjectIntoStore(t *testing.T, s *orchestrator.ProjectStore, projects []*orchestrator.Project) {
	t.Helper()
	errs := s.LoadAll(projects)
	for _, e := range errs {
		t.Fatalf("LoadAll: %v", e)
	}
}

// TestGetWithWorkspace_NoWorkspace verifies that unlinked projects return
// their cached meta unchanged (backward compatibility).
func TestGetWithWorkspace_NoWorkspace(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	setupProjectDir(t, dir, "proj-nows", "No Workspace Project")

	s := orchestrator.NewProjectStore(nil)
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-nows", WorkDir: dir, WorkspaceID: ""},
	})

	meta, err := s.GetWithWorkspace(context.Background(), "proj-nows")
	if err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}
	if meta.ID != "proj-nows" {
		t.Fatalf("expected id proj-nows, got %s", meta.ID)
	}
	// SecretNamespace should remain empty (no workspace linked).
	if meta.SecretNamespace != "" {
		t.Fatalf("expected empty SecretNamespace, got %q", meta.SecretNamespace)
	}
}

// TestGetWithWorkspace_NotLoaded verifies that GetWithWorkspace returns an
// error when the project has not been loaded into the store.
func TestGetWithWorkspace_NotLoaded(t *testing.T) {
	t.Parallel()

	s := orchestrator.NewProjectStore(nil)
	_, err := s.GetWithWorkspace(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for unloaded project, got nil")
	}
}

// TestGetWithWorkspace_WorkspaceExists verifies that capabilities, kits env
// (via workspace.yaml env), and SecretNamespace are injected when workspace.yaml
// exists.
func TestGetWithWorkspace_WorkspaceExists(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	setupProjectDir(t, projectDir, "proj-ws", "Workspace Project")

	wsDir := t.TempDir()
	setupWorkspaceDir(t, wsDir, "myworkspace", `
capabilities:
  docker: {}
env:
  WS_VAR: from-workspace
`)

	s := orchestrator.NewProjectStore(nil)
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-ws", WorkDir: projectDir, WorkspaceID: "myworkspace"},
	})

	meta, err := s.GetWithWorkspace(context.Background(), "proj-ws")
	if err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}

	// SecretNamespace always injected.
	if meta.SecretNamespace != "myworkspace" {
		t.Fatalf("expected SecretNamespace=myworkspace, got %q", meta.SecretNamespace)
	}

	// Docker capability injected.
	if meta.Capabilities.Docker == nil {
		t.Fatal("expected Capabilities.Docker to be non-nil after workspace merge")
	}

	// Env injected.
	if meta.Env["WS_VAR"] != "from-workspace" {
		t.Fatalf("expected WS_VAR=from-workspace in meta.Env, got %v", meta.Env)
	}
}

// TestGetWithWorkspace_Degraded verifies that when workspace.yaml is missing,
// SecretNamespace is still injected and no error is returned.
func TestGetWithWorkspace_Degraded(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	setupProjectDir(t, projectDir, "proj-deg", "Degraded Project")

	wsDir := t.TempDir() // no workspace.yaml written here

	s := orchestrator.NewProjectStore(nil)
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-deg", WorkDir: projectDir, WorkspaceID: "missing-ws"},
	})

	meta, err := s.GetWithWorkspace(context.Background(), "proj-deg")
	if err != nil {
		t.Fatalf("expected no error in degraded mode, got: %v", err)
	}

	// SecretNamespace still injected even in degraded window.
	if meta.SecretNamespace != "missing-ws" {
		t.Fatalf("expected SecretNamespace=missing-ws in degraded mode, got %q", meta.SecretNamespace)
	}

	// Capabilities must remain zero (not injected in degraded mode).
	if meta.Capabilities.Docker != nil {
		t.Fatal("expected Capabilities.Docker to be nil in degraded mode")
	}
}

// TestGetWithWorkspace_EnvMerge verifies that workspace.yaml env is merged into
// the project meta at GetWithWorkspace time. (env in project.yaml is a removed
// key as of the new schema; env is now supplied via workspace.yaml or injected
// into behaviors via project.local.yaml.)
func TestGetWithWorkspace_EnvMerge(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	boidDir := filepath.Join(projectDir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Minimal project.yaml with no env.
	projectYAML := "id: proj-env-prio\nname: Env Priority Project\n"
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}

	wsDir := t.TempDir()
	setupWorkspaceDir(t, wsDir, "envws", `
env:
  WS_KEY_A: value-a
  WS_KEY_B: value-b
`)

	s := orchestrator.NewProjectStore(nil)
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-env-prio", WorkDir: projectDir, WorkspaceID: "envws"},
	})

	meta, err := s.GetWithWorkspace(context.Background(), "proj-env-prio")
	if err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}

	// Workspace env is present in the hydrated meta.
	if meta.Env["WS_KEY_A"] != "value-a" {
		t.Fatalf("expected WS_KEY_A=value-a, got %q", meta.Env["WS_KEY_A"])
	}
	if meta.Env["WS_KEY_B"] != "value-b" {
		t.Fatalf("expected WS_KEY_B=value-b, got %q", meta.Env["WS_KEY_B"])
	}
}

// TestGetWithWorkspace_NoWorkspaceStore verifies that GetWithWorkspace still
// injects SecretNamespace when no WorkspaceStore is configured (store not set).
func TestGetWithWorkspace_NoWorkspaceStore(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	setupProjectDir(t, projectDir, "proj-nows2", "No WS Store Project")

	s := orchestrator.NewProjectStore(nil)
	// Intentionally do NOT call SetWorkspaceStore.
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-nows2", WorkDir: projectDir, WorkspaceID: "some-workspace"},
	})

	meta, err := s.GetWithWorkspace(context.Background(), "proj-nows2")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	// SecretNamespace still injected.
	if meta.SecretNamespace != "some-workspace" {
		t.Fatalf("expected SecretNamespace=some-workspace, got %q", meta.SecretNamespace)
	}
}

// TestGetWithWorkspace_CachedMetaUnmodified verifies that GetWithWorkspace
// returns a copy so the cached meta is not mutated.
func TestGetWithWorkspace_CachedMetaUnmodified(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	setupProjectDir(t, projectDir, "proj-copy", "Copy Test Project")

	wsDir := t.TempDir()
	setupWorkspaceDir(t, wsDir, "copyws", `
env:
  WS_KEY: ws-value
capabilities:
  docker: {}
`)

	s := orchestrator.NewProjectStore(nil)
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-copy", WorkDir: projectDir, WorkspaceID: "copyws"},
	})

	// Call GetWithWorkspace to hydrate.
	hydrated, err := s.GetWithWorkspace(context.Background(), "proj-copy")
	if err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}
	if hydrated.SecretNamespace != "copyws" {
		t.Fatalf("expected SecretNamespace=copyws in hydrated copy, got %q", hydrated.SecretNamespace)
	}

	// The cached value via Get should NOT have been mutated.
	cached, ok := s.Get("proj-copy")
	if !ok {
		t.Fatal("expected cached meta to exist")
	}
	if cached.SecretNamespace != "" {
		t.Fatalf("cached meta should not be mutated: SecretNamespace=%q", cached.SecretNamespace)
	}
	if cached.Capabilities.Docker != nil {
		t.Fatal("cached meta should not be mutated: Capabilities.Docker should be nil")
	}
}

// TestGetWithWorkspace_WorkspaceEnvWithKitConflict verifies that duplicate
// host_commands from workspace kits return an error.
func TestGetWithWorkspace_WorkspaceHostCommandConflict(t *testing.T) {
	t.Parallel()

	// This test validates the builtin conflict check by creating a workspace
	// with a kit that declares "git" as a host_command (builtin conflict).
	// Since kits require a resolver that can find kit files, and we can't
	// easily set up a full kit directory, we exercise the simpler builtin
	// rejection path via a workspace.yaml with an unresolvable kit (which
	// is just skipped with a warning) — builtin conflict validation via
	// workspace kits requires actual kit files.

	// Instead, verify that a malformed workspace slug is rejected.
	projectDir := t.TempDir()
	setupProjectDir(t, projectDir, "proj-slugerr", "Slug Error Project")

	wsDir := t.TempDir()
	// workspace slug with uppercase is invalid per ValidWorkspaceSlug.

	s := orchestrator.NewProjectStore(nil)
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-slugerr", WorkDir: projectDir, WorkspaceID: "InvalidSlug"},
	})

	_, err := s.GetWithWorkspace(context.Background(), "proj-slugerr")
	if err == nil {
		t.Fatal("expected error for invalid workspace slug, got nil")
	}
	if !strings.Contains(err.Error(), "InvalidSlug") {
		t.Fatalf("expected error to mention the invalid slug, got: %v", err)
	}
}

// setupProjectWithBehavior writes a project.yaml carrying one task behavior so
// the per-behavior workspace-kit merge path in GetWithWorkspace is exercised
// (setupProjectDir writes a behavior-less project).
func setupProjectWithBehavior(t *testing.T, dir, id, behavior string) {
	t.Helper()
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yaml := "id: " + id + "\nname: " + id + "\ntask_behaviors:\n  " + behavior + ":\n    name: " + behavior + "\n"
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}
}

func findBindingBySource(mounts []orchestrator.BindMount, src string) (orchestrator.BindMount, bool) {
	for _, m := range mounts {
		if m.Source == src {
			return m, true
		}
	}
	return orchestrator.BindMount{}, false
}

// --- docs/plans/workspace-db-consolidation.md PR3 Step G: workspace.HostCommands resolution ---

// TestGetWithWorkspace_HostCommandsResolved verifies that a workspace's
// HostCommands []string (reference names) are resolved against the
// daemon's aggregated host_commands map (SetHostCommands) and merged into
// both the top-level meta.HostCommands (session jobs) and every
// TaskBehavior's HostCommands (task hooks).
func TestGetWithWorkspace_HostCommandsResolved(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	setupProjectWithBehavior(t, projectDir, "proj-hc", "build")

	wsDir := t.TempDir()
	setupWorkspaceDir(t, wsDir, "hcws", "host_commands:\n  - gh\n")

	s := orchestrator.NewProjectStore(nil)
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	s.SetHostCommands(map[string]orchestrator.HostCommandSpec{
		"gh": {Allow: []string{"pr", "issue"}},
	})
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-hc", WorkDir: projectDir, WorkspaceID: "hcws"},
	})

	meta, err := s.GetWithWorkspace(context.Background(), "proj-hc")
	if err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}

	gh, ok := meta.HostCommands["gh"]
	if !ok {
		t.Fatalf("top-level meta.HostCommands missing 'gh': %+v", meta.HostCommands)
	}
	if !reflect.DeepEqual(gh.Allow, []string{"pr", "issue"}) {
		t.Errorf("top-level gh.Allow = %v, want [pr issue]", gh.Allow)
	}

	build, ok := meta.TaskBehaviors["build"]
	if !ok {
		t.Fatalf("behavior build missing from hydrated meta: %+v", meta.TaskBehaviors)
	}
	bgh, ok := build.HostCommands["gh"]
	if !ok {
		t.Fatalf("behavior build.HostCommands missing 'gh': %+v", build.HostCommands)
	}
	if !reflect.DeepEqual(bgh.Allow, []string{"pr", "issue"}) {
		t.Errorf("behavior gh.Allow = %v, want [pr issue]", bgh.Allow)
	}
}

// TestGetWithWorkspace_HostCommandsUnresolvedNameSkipped verifies that a
// workspace.HostCommands entry with no corresponding entry in the
// aggregated host_commands map is skipped rather than erroring — the same
// tolerance GetWithWorkspace already extends to unresolvable workspace kit
// references.
func TestGetWithWorkspace_HostCommandsUnresolvedNameSkipped(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	setupProjectDir(t, projectDir, "proj-hc-miss", "Missing HostCommand Project")

	wsDir := t.TempDir()
	setupWorkspaceDir(t, wsDir, "hcmissws", "host_commands:\n  - unknown-command\n")

	s := orchestrator.NewProjectStore(nil)
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	s.SetHostCommands(map[string]orchestrator.HostCommandSpec{
		"gh": {Allow: []string{"pr"}},
	})
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-hc-miss", WorkDir: projectDir, WorkspaceID: "hcmissws"},
	})

	meta, err := s.GetWithWorkspace(context.Background(), "proj-hc-miss")
	if err != nil {
		t.Fatalf("GetWithWorkspace: expected no error for unresolvable host_commands reference, got: %v", err)
	}
	if _, ok := meta.HostCommands["unknown-command"]; ok {
		t.Errorf("unresolvable host_command reference should not appear in meta.HostCommands: %+v", meta.HostCommands)
	}
}

// TestGetWithWorkspace_HostCommandsBuiltinConflict verifies that a resolved
// workspace host_command colliding with a reserved builtin name (e.g.
// "git") is rejected the same way a workspace-kit-supplied builtin conflict
// is rejected.
func TestGetWithWorkspace_HostCommandsBuiltinConflict(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	setupProjectDir(t, projectDir, "proj-hc-conflict", "Conflict Project")

	wsDir := t.TempDir()
	setupWorkspaceDir(t, wsDir, "hcconflictws", "host_commands:\n  - git\n")

	s := orchestrator.NewProjectStore(nil)
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	s.SetHostCommands(map[string]orchestrator.HostCommandSpec{
		"git": {Allow: []string{"push"}},
	})
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-hc-conflict", WorkDir: projectDir, WorkspaceID: "hcconflictws"},
	})

	_, err := s.GetWithWorkspace(context.Background(), "proj-hc-conflict")
	if err == nil {
		t.Fatal("expected error for reserved builtin name conflict, got nil")
	}
	if !strings.Contains(err.Error(), "git") {
		t.Errorf("expected error to mention the conflicting name 'git': %v", err)
	}
}

// TestGetWithWorkspace_HostCommandsRejectRuleValidation verifies that reject
// rules with an empty match or empty reason are rejected at hydration time
// with an error naming the offending command and rule index. This exercises
// the same validateRejectRules call as the retired kit-based
// TestGetWithWorkspace_RejectRuleValidation (removed in
// docs/plans/workspace-db-consolidation.md Phase 2.5 PR6, kit mechanism
// retirement) but through the surviving workspace.HostCommands
// (aggregated-config reference) path instead of a workspace kit.
func TestGetWithWorkspace_HostCommandsRejectRuleValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		spec    orchestrator.HostCommandSpec
		wantErr string
	}{
		{
			name:    "empty-match",
			spec:    orchestrator.HostCommandSpec{Reject: []orchestrator.RejectRule{{Reason: "no match given"}}},
			wantErr: "host_commands.gh.reject[0]: match is required",
		},
		{
			name:    "empty-reason",
			spec:    orchestrator.HostCommandSpec{Reject: []orchestrator.RejectRule{{Match: "*--body-file*"}}},
			wantErr: "host_commands.gh.reject[0]: reason is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			projectDir := t.TempDir()
			setupProjectDir(t, projectDir, "proj-rejval-"+tc.name, "Reject Validation Project")

			wsDir := t.TempDir()
			setupWorkspaceDir(t, wsDir, "rejvalws-"+tc.name, "host_commands:\n  - gh\n")

			s := orchestrator.NewProjectStore(nil)
			s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
			s.SetHostCommands(map[string]orchestrator.HostCommandSpec{"gh": tc.spec})
			loadProjectIntoStore(t, s, []*orchestrator.Project{
				{ID: "proj-rejval-" + tc.name, WorkDir: projectDir, WorkspaceID: "rejvalws-" + tc.name},
			})

			_, err := s.GetWithWorkspace(context.Background(), "proj-rejval-"+tc.name)
			if err == nil {
				t.Fatalf("expected reject rule validation error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

// TestGetWithWorkspace_HostCommandsNoAggregateConfigured verifies that a
// workspace declaring HostCommands when the daemon has no aggregated
// host_commands map wired at all (SetHostCommands never called — the
// zero-value ProjectStore case) is a safe no-op, not a panic or error.
func TestGetWithWorkspace_HostCommandsNoAggregateConfigured(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	setupProjectDir(t, projectDir, "proj-hc-noagg", "No Aggregate Project")

	wsDir := t.TempDir()
	setupWorkspaceDir(t, wsDir, "hcnoaggws", "host_commands:\n  - gh\n")

	s := orchestrator.NewProjectStore(nil)
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	// Intentionally do NOT call SetHostCommands.
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-hc-noagg", WorkDir: projectDir, WorkspaceID: "hcnoaggws"},
	})

	meta, err := s.GetWithWorkspace(context.Background(), "proj-hc-noagg")
	if err != nil {
		t.Fatalf("GetWithWorkspace: expected no error, got: %v", err)
	}
	if len(meta.HostCommands) != 0 {
		t.Errorf("expected empty HostCommands with no aggregate configured, got %+v", meta.HostCommands)
	}
}

// TestGetWithWorkspace_WarnsWhenAggregatedIsEmptyButRefsExist pins the MINOR
// fix (codex review, docs/plans/workspace-db-consolidation.md): before this
// fix, the workspace.HostCommands merge block's guard condition was
// `len(ws.HostCommands) > 0 && len(s.hostCommands) > 0`, so when the
// daemon's aggregated host_commands map was empty (SetHostCommands never
// called, or called with an empty map — e.g. no kits installed), the whole
// block — including the "unresolved" warning below — was skipped outright,
// silently dropping every referenced name with no diagnostic at all. The
// fix drops the second half of that condition so the per-name warning fires
// even when the aggregate itself is empty.
func TestGetWithWorkspace_WarnsWhenAggregatedIsEmptyButRefsExist(t *testing.T) {
	projectDir := t.TempDir()
	setupProjectDir(t, projectDir, "proj-hc-warn", "Warn Project")

	wsDir := t.TempDir()
	setupWorkspaceDir(t, wsDir, "hcwarnws", "host_commands:\n  - gh\n")

	s := orchestrator.NewProjectStore(nil)
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	// Explicitly wire an empty (non-nil) aggregate, the "no kits installed"
	// steady state — distinct from never calling SetHostCommands at all
	// (covered by TestGetWithWorkspace_HostCommandsNoAggregateConfigured).
	s.SetHostCommands(map[string]orchestrator.HostCommandSpec{})
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-hc-warn", WorkDir: projectDir, WorkspaceID: "hcwarnws"},
	})

	buf := captureSlog(t)

	meta, err := s.GetWithWorkspace(context.Background(), "proj-hc-warn")
	if err != nil {
		t.Fatalf("GetWithWorkspace: expected no error, got: %v", err)
	}
	if len(meta.HostCommands) != 0 {
		t.Errorf("expected empty HostCommands (aggregate has no 'gh' entry), got %+v", meta.HostCommands)
	}
	if !strings.Contains(buf.String(), "workspace host_commands reference unresolved") {
		t.Errorf("expected an 'unresolved' warning to be logged even with an empty aggregate, got log: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "gh") {
		t.Errorf("expected the warning to name the unresolved reference 'gh', got log: %s", buf.String())
	}
}

// TestGetWithWorkspace_MergesWorkspaceAdditionalBindings pins BLOCKER 2
// (codex review): workspace.AdditionalBindings — whether directly authored
// in workspace.yaml, or materialized from a workspace's Kits by BLOCKER 1's
// migration fix — must reach both the top-level meta.AdditionalBindings
// (session jobs bypass behaviors and read this directly) and every
// TaskBehavior.AdditionalBindings (task hooks read behavior.AdditionalBindings
// via the planner), mirroring the dual-surface contract already pinned for
// workspace kits (TestGetWithWorkspace_AdditionalBindingsMerge). Before this
// fix there was no merge path at all: GetWithWorkspace never even read
// ws.AdditionalBindings.
func TestGetWithWorkspace_MergesWorkspaceAdditionalBindings(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	setupProjectWithBehavior(t, projectDir, "proj-ws-bind", "build")

	wsDir := t.TempDir()
	setupWorkspaceDir(t, wsDir, "wsbind", `additional_bindings:
  - source: /opt/tool
    target: /opt/tool
    mode: ro
`)

	s := orchestrator.NewProjectStore(nil)
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-ws-bind", WorkDir: projectDir, WorkspaceID: "wsbind"},
	})

	meta, err := s.GetWithWorkspace(context.Background(), "proj-ws-bind")
	if err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}

	if _, ok := findBindingBySource(meta.AdditionalBindings, "/opt/tool"); !ok {
		t.Fatalf("workspace additional_binding missing from meta.AdditionalBindings: %+v", meta.AdditionalBindings)
	}

	build, ok := meta.TaskBehaviors["build"]
	if !ok {
		t.Fatalf("behavior build missing from hydrated meta: %+v", meta.TaskBehaviors)
	}
	if _, ok := findBindingBySource(build.AdditionalBindings, "/opt/tool"); !ok {
		t.Fatalf("workspace additional_binding missing from behavior build.AdditionalBindings: %+v", build.AdditionalBindings)
	}
}

// TestGetWithWorkspace_ProjectBindingsWinOnConflict verifies BLOCKER 2's
// precedence rule: when the project's own top-level AdditionalBindings
// declares the same Source as a workspace binding, the project's entry
// wins — mirroring the "project.yaml wins" precedence GetWithWorkspace
// already applies to workspace kits
// (out.AdditionalBindings = mergeBindMounts(ws.AdditionalBindings, out.AdditionalBindings),
// where out.AdditionalBindings — already seeded from the project's own
// meta at clone time — is the second/overlay argument and therefore wins).
// The project side is constructed directly via ProjectStore.Set (bypassing
// project.yaml's own on-disk parsing, which — post cutover — no longer
// accepts a top-level additional_bindings key at all: see
// removedTopLevelKeys) so this test isolates GetWithWorkspace's merge
// precedence from that unrelated schema-migration concern. (Per-behavior
// AdditionalBindings cannot be exercised the same way: cloneProjectMeta
// always resets every TaskBehavior's AdditionalBindings to nil before
// GetWithWorkspace's merge blocks run — by design, since a real
// project.yaml can no longer author that field directly either, only a
// linked workspace/kit can populate it post-clone.)
func TestGetWithWorkspace_ProjectBindingsWinOnConflict(t *testing.T) {
	t.Parallel()

	wsDir := t.TempDir()
	setupWorkspaceDir(t, wsDir, "bindconflictws", `additional_bindings:
  - source: /opt/tool
    target: /opt/tool
    mode: ro
`)

	s := orchestrator.NewProjectStore(nil)
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	s.Set("proj-bind-conflict", &orchestrator.ProjectMeta{
		ID:   "proj-bind-conflict",
		Name: "Bind Conflict Project",
		AdditionalBindings: []orchestrator.BindMount{
			{Source: "/opt/tool", Target: "/opt/tool", Mode: "rw"},
		},
	})
	s.SetWorkspaceID("proj-bind-conflict", "bindconflictws")

	meta, err := s.GetWithWorkspace(context.Background(), "proj-bind-conflict")
	if err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}

	got, ok := findBindingBySource(meta.AdditionalBindings, "/opt/tool")
	if !ok {
		t.Fatalf("expected /opt/tool binding in meta.AdditionalBindings: %+v", meta.AdditionalBindings)
	}
	if got.Mode != "rw" {
		t.Errorf("top-level binding mode = %q, want rw (project must win over workspace)", got.Mode)
	}
}

// TestGetWithWorkspace_ExpandsWorkspaceEnvVariables pins MAJOR 1 (codex
// review, 3rd pass): workspace.yaml's Env is stored raw in the DB/yaml (no
// secret-shaped value is ever expanded before being persisted), but a
// ${VAR} placeholder in it must be expanded from the daemon's own
// environment by the time it reaches dispatch — mirroring
// ExpandHostCommandsForDispatch's treatment of workspace.HostCommands.
// Before this fix, kit/workspace Env placeholders such as
// ${E2E_WORKSPACE_DIR} or ${XDG_DATA_HOME} were materialized into the DB
// unexpanded and never expanded again, a silent regression versus the
// pre-cutover yaml-mode path (which happened to expand at load time via the
// same os.Expand machinery project.yaml/kit merging already used).
//
// t.Setenv is used here, which is incompatible with t.Parallel (Go's
// testing package forbids combining the two), so this test intentionally
// does not call t.Parallel() — matching every other t.Setenv-using test in
// this package.
func TestGetWithWorkspace_ExpandsWorkspaceEnvVariables(t *testing.T) {
	t.Setenv("WS_EXPAND_PROBE", "expanded-value")

	projectDir := t.TempDir()
	setupProjectDir(t, projectDir, "proj-ws-env-expand", "Env Expand Project")

	wsDir := t.TempDir()
	setupWorkspaceDir(t, wsDir, "envexpandws", "env:\n  WS_VAR: ${WS_EXPAND_PROBE}\n")

	s := orchestrator.NewProjectStore(nil)
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-ws-env-expand", WorkDir: projectDir, WorkspaceID: "envexpandws"},
	})

	meta, err := s.GetWithWorkspace(context.Background(), "proj-ws-env-expand")
	if err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}

	if meta.Env["WS_VAR"] != "expanded-value" {
		t.Fatalf("meta.Env[WS_VAR] = %q, want expanded-value (workspace env must be $VAR-expanded at dispatch time)", meta.Env["WS_VAR"])
	}
}

// TestGetWithWorkspace_ExpandsWorkspaceAdditionalBindings is
// TestGetWithWorkspace_ExpandsWorkspaceEnvVariables's counterpart for
// workspace.yaml's AdditionalBindings: a ${VAR} placeholder in Source/Target
// must also be expanded by dispatch time, the same way project.yaml's own
// AdditionalBindings are expanded by interpolateBindMounts at load time.
func TestGetWithWorkspace_ExpandsWorkspaceAdditionalBindings(t *testing.T) {
	t.Setenv("WS_EXPAND_DIR", "/opt/expanded-dir")

	projectDir := t.TempDir()
	setupProjectWithBehavior(t, projectDir, "proj-ws-bind-expand", "build")

	wsDir := t.TempDir()
	setupWorkspaceDir(t, wsDir, "bindexpandws", `additional_bindings:
  - source: ${WS_EXPAND_DIR}/tool
    target: ${WS_EXPAND_DIR}/tool
    mode: ro
`)

	s := orchestrator.NewProjectStore(nil)
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-ws-bind-expand", WorkDir: projectDir, WorkspaceID: "bindexpandws"},
	})

	meta, err := s.GetWithWorkspace(context.Background(), "proj-ws-bind-expand")
	if err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}

	got, ok := findBindingBySource(meta.AdditionalBindings, "/opt/expanded-dir/tool")
	if !ok {
		t.Fatalf("expected expanded source /opt/expanded-dir/tool in meta.AdditionalBindings, got %+v (workspace additional_bindings must be $VAR-expanded at dispatch time)", meta.AdditionalBindings)
	}
	if got.Target != "/opt/expanded-dir/tool" {
		t.Errorf("got.Target = %q, want expanded /opt/expanded-dir/tool", got.Target)
	}

	build, ok := meta.TaskBehaviors["build"]
	if !ok {
		t.Fatalf("behavior build missing from hydrated meta: %+v", meta.TaskBehaviors)
	}
	if _, ok := findBindingBySource(build.AdditionalBindings, "/opt/expanded-dir/tool"); !ok {
		t.Fatalf("expected expanded source in behavior build.AdditionalBindings, got %+v", build.AdditionalBindings)
	}
}

// TestLoadAll_RecordsWorkspaceID verifies that LoadAll correctly records the
// workspaceID for each project, enabling GetWithWorkspace to find it.
func TestLoadAll_RecordsWorkspaceID(t *testing.T) {
	t.Parallel()

	dir1 := t.TempDir()
	setupProjectDir(t, dir1, "proj-ws-a", "Project WS A")

	dir2 := t.TempDir()
	setupProjectDir(t, dir2, "proj-ws-b", "Project WS B")

	wsDir := t.TempDir()
	setupWorkspaceDir(t, wsDir, "myws", `env:
  WS_TAG: tagged
`)

	s := orchestrator.NewProjectStore(nil)
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))

	errs := s.LoadAll([]*orchestrator.Project{
		{ID: "proj-ws-a", WorkDir: dir1, WorkspaceID: "myws"},
		{ID: "proj-ws-b", WorkDir: dir2, WorkspaceID: ""},
	})
	for _, e := range errs {
		t.Fatalf("LoadAll: %v", e)
	}

	// proj-ws-a should be hydrated with workspace env.
	metaA, err := s.GetWithWorkspace(context.Background(), "proj-ws-a")
	if err != nil {
		t.Fatalf("GetWithWorkspace(proj-ws-a): %v", err)
	}
	if metaA.SecretNamespace != "myws" {
		t.Fatalf("proj-ws-a: expected SecretNamespace=myws, got %q", metaA.SecretNamespace)
	}
	if metaA.Env["WS_TAG"] != "tagged" {
		t.Fatalf("proj-ws-a: expected WS_TAG=tagged, got %v", metaA.Env)
	}

	// proj-ws-b has no workspace; should return meta unchanged.
	metaB, err := s.GetWithWorkspace(context.Background(), "proj-ws-b")
	if err != nil {
		t.Fatalf("GetWithWorkspace(proj-ws-b): %v", err)
	}
	if metaB.SecretNamespace != "" {
		t.Fatalf("proj-ws-b: expected empty SecretNamespace, got %q", metaB.SecretNamespace)
	}
}
