package orchestrator_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

// captureSlog redirects the default slog logger to an in-memory buffer for the
// duration of the test. Helper for verifying deprecation warnings.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestReadProjectMeta_Valid(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	yaml := `
id: test-proj
name: Test Project
task_behaviors:
  dev:
    name: development
    traits:
      - artifactompt
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	meta, err := projectspec.ReadProjectMeta(dir)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}

	if meta.ID != "test-proj" || meta.Name != "Test Project" {
		t.Fatalf("unexpected meta: %+v", meta)
	}
	// "dev" is an ordinary behavior name: the key survives verbatim and
	// nothing else is synthesized alongside it.
	if _, ok := meta.TaskBehaviors["dev"]; !ok {
		t.Fatalf("expected 'dev' behavior to be present, got %+v", meta.TaskBehaviors)
	}
	if len(meta.TaskBehaviors) != 1 {
		t.Fatalf("expected exactly 1 behavior, got %+v", meta.TaskBehaviors)
	}
}

// TestReadProjectMeta_SessionBehaviors verifies that session_behaviors — a
// free-naming dictionary distinct from task_behaviors (sessions do not
// resolve through ResolveBehavior, see PostStartShapingSession's doc
// comment in internal/api/web.go) — parses into ProjectMeta.SessionBehaviors.
func TestReadProjectMeta_SessionBehaviors(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	yaml := `
id: test-proj
name: Test Project
session_behaviors:
  shape:
    harness_type: codex
    model: o3-mini
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	meta, err := projectspec.ReadProjectMeta(dir)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}

	shape, ok := meta.SessionBehaviors["shape"]
	if !ok {
		t.Fatalf("expected 'shape' session behavior, got %+v", meta.SessionBehaviors)
	}
	if shape.HarnessType != "codex" || shape.Model != "o3-mini" {
		t.Fatalf("unexpected shape session behavior: %+v", shape)
	}
}

// TestReadProjectMeta_SessionBehaviors_Empty verifies that a project.yaml
// with no session_behaviors key leaves ProjectMeta.SessionBehaviors empty —
// a regression guard so the new field doesn't synthesize entries out of
// nothing for the vast majority of projects that don't set it.
func TestReadProjectMeta_SessionBehaviors_Empty(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	yaml := `
id: test-proj
name: Test Project
task_behaviors:
  dev:
    name: development
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	meta, err := projectspec.ReadProjectMeta(dir)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}

	if len(meta.SessionBehaviors) != 0 {
		t.Fatalf("expected no session behaviors, got %+v", meta.SessionBehaviors)
	}
}

// TestReadProjectMeta_HookCommandField verifies that ReadProjectMeta parses
// the hooks[].command inline field (docs/plans/script-hook-removal.md) from
// YAML into Hook.Command. No backing .boid/hooks/<id>.sh file is required —
// script-hook resolution was removed entirely in PR3. The dispatch-time
// exclusivity rules for Command / Agent / Kind live in
// DispatchPlanner.PlanHook (see TestPlanHook_* in planner_test.go).
func TestReadProjectMeta_HookCommandField(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	yaml := `
id: test-proj
name: Test Project
task_behaviors:
  dev:
    hooks:
      - id: assert-clone-cwd
        command: |
          set -eu
          echo assert-clone-cwd ok
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	meta, err := projectspec.ReadProjectMeta(dir)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}

	behavior, ok := meta.TaskBehaviors["dev"]
	if !ok {
		t.Fatalf("expected 'dev' behavior, got %+v", meta.TaskBehaviors)
	}
	if len(behavior.Hooks) != 1 {
		t.Fatalf("hooks = %+v, want 1 entry", behavior.Hooks)
	}
	got := behavior.Hooks[0]
	const wantCommand = "set -eu\necho assert-clone-cwd ok\n"
	if got.Command != wantCommand {
		t.Errorf("hook.Command = %q, want %q", got.Command, wantCommand)
	}
	if got.Kind != "" {
		t.Errorf("hook.Kind = %q, want empty (this hook is non-agent, Command-only)", got.Kind)
	}
}

// TestReadProjectMeta_RejectsAgentKindHookWithCommand verifies the load-time
// counterpart of DispatchPlanner.validateHookCommandFields rule #1: an
// agent-kind hook must not carry an inline `command:`, because agent hooks
// are dispatched to a HarnessAdapter that builds its own argv, leaving the
// inline command with nowhere to run. Load-time rejection catches YAML
// authoring mistakes long before dispatch; the runtime check in PlanHook
// remains as defense-in-depth against programmatic construction and
// kit-merge drift (see spec_loader.go:validateHookKind for the paired
// rationale).
func TestReadProjectMeta_RejectsAgentKindHookWithCommand(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	yaml := `
id: test-proj
name: Test Project
task_behaviors:
  dev:
    hooks:
      - id: agent-with-command
        kind: agent
        command: echo hi
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	_, err := projectspec.ReadProjectMeta(dir)
	if err == nil {
		t.Fatal("ReadProjectMeta accepted agent-kind hook with command; want error")
	}
	if !strings.Contains(err.Error(), "does not allow 'command:'") {
		t.Errorf("error = %v, want one mentioning that kind: agent does not allow command", err)
	}
}

func TestReadProjectMeta_RejectedKeys(t *testing.T) {
	// These keys have been removed from project.yaml in the new schema.
	// Each one should produce a guidance error.
	for _, key := range []string{"host_commands", "env", "additional_bindings", "kits", "secret_namespace", "capabilities"} {
		t.Run(key, func(t *testing.T) {
			dir := t.TempDir()
			boidDir := filepath.Join(dir, ".boid")
			_ = os.MkdirAll(boidDir, 0o755)
			// Use a minimal value that parses correctly for the type.
			val := key + ": {}\n"
			if key == "kits" || key == "additional_bindings" {
				val = key + ": []\n"
			}
			content := "id: test-proj\nname: Test\n" + val
			_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(content), 0o644)

			_, err := projectspec.ReadProjectMeta(dir)
			if err == nil {
				t.Fatalf("expected error for %q, got nil", key)
			}
			if !strings.Contains(err.Error(), "is no longer supported") {
				t.Fatalf("expected 'is no longer supported' in error for %q, got: %v", key, err)
			}
		})
	}
}

func TestReadProjectMeta_RejectsTopLevelHooksGates(t *testing.T) {
	for _, field := range []string{"hooks", "gates"} {
		t.Run(field, func(t *testing.T) {
			dir := t.TempDir()
			boidDir := filepath.Join(dir, ".boid")
			_ = os.MkdirAll(boidDir, 0o755)
			content := "id: test-proj\nname: Test\n" + field + ": []\n"
			_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(content), 0o644)

			_, err := projectspec.ReadProjectMeta(dir)
			if err == nil || !strings.Contains(err.Error(), "no longer supported") {
				t.Fatalf("expected rejection of top-level %q, got %v", field, err)
			}
		})
	}
}

func TestReadProjectMeta_TopLevelKitsRejected(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	_ = os.MkdirAll(boidDir, 0o755)
	content := "id: test-proj\nname: Test\nkits:\n  - local/my-kit\n"
	_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(content), 0o644)

	_, err := projectspec.ReadProjectMeta(dir)
	if err == nil {
		t.Fatal("expected error for top-level kits, got nil")
	}
	if !strings.Contains(err.Error(), `top-level "kits" is no longer supported`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestReadProjectMeta_TopLevelWorktreeBaseBranch verifies the current handling
// of the project-level "worktree" and "base_branch" fields.
//   - "base_branch" is accepted and exposed on ProjectMeta as before.
//   - "worktree" is silently ignored: branch-policy-simplification Phase 2
//     retired the field, but existing project.yaml files that still carry it
//     must not fail to load (silent-ignore contract, see
//     docs/plans/branch-policy-simplification.md Phase 2).
//   - behavior-level worktree (task_behaviors.<name>.worktree) remains
//     rejected with a descriptive error via removedBehaviorFieldGuidance.
func TestReadProjectMeta_TopLevelWorktreeBaseBranch(t *testing.T) {
	t.Run("accepts new top-level base_branch and silently ignores worktree", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		if err := os.MkdirAll(boidDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		yaml := `
id: test-proj
name: Test Project
worktree: true
base_branch: develop
task_behaviors:
  dev:
    name: dev
`
		if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatalf("write yaml: %v", err)
		}

		meta, err := projectspec.ReadProjectMeta(dir)
		if err != nil {
			t.Fatalf("read meta: %v", err)
		}
		if meta.BaseBranch != "develop" {
			t.Errorf("expected project-level BaseBranch=develop, got %q", meta.BaseBranch)
		}
	})

	t.Run("defaults to zero values when omitted", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		if err := os.MkdirAll(boidDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		yaml := `
id: test-proj
name: Test Project
task_behaviors:
  dev:
    name: dev
`
		if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatalf("write yaml: %v", err)
		}

		meta, err := projectspec.ReadProjectMeta(dir)
		if err != nil {
			t.Fatalf("read meta: %v", err)
		}
		if meta.BaseBranch != "" {
			t.Errorf("expected project-level BaseBranch default empty, got %q", meta.BaseBranch)
		}
	})

	// Phase 3-1: behavior-level readonly / worktree / branch_prefix /
	// base_branch / default_payload are no longer supported. Files that
	// still carry them must produce a descriptive load-time error.
	t.Run("legacy behavior-level worktree is rejected", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		if err := os.MkdirAll(boidDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		yaml := `
id: test-proj
name: Test Project
task_behaviors:
  dev:
    name: dev
    worktree: true
`
		if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatalf("write yaml: %v", err)
		}

		_, err := projectspec.ReadProjectMeta(dir)
		if err == nil {
			t.Fatal("expected error for legacy behavior-level worktree, got nil")
		}
		if !strings.Contains(err.Error(), "task_behaviors.dev.worktree") {
			t.Errorf("expected error to point at task_behaviors.dev.worktree, got: %v", err)
		}
	})
}

func TestReadProjectMeta_Errors(t *testing.T) {
	t.Run("missing id is optional (workspace-default-project.md 論点h 案1)", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("name: No ID Project\n"), 0o644)

		meta, err := projectspec.ReadProjectMeta(dir)
		if err != nil {
			t.Fatalf("expected id-less project.yaml to parse, got error: %v", err)
		}
		if meta.ID != "" {
			t.Fatalf("expected empty ID when project.yaml omits id, got %q", meta.ID)
		}
		if meta.Name != "No ID Project" {
			t.Fatalf("expected name to still be parsed, got %q", meta.Name)
		}
	})

	// Codex review round 2 Major (docs/plans/workspace-default-project.md
	// 論点e, PR6): every "is this project.yaml-less" check in the codebase
	// gates purely on orchestrator.URLDerivedProjectIDPrefix ("url-"), on the
	// assumption that no hand-authored project.yaml id would ever collide
	// with it. That assumption was never enforced at load time — reject it
	// here so the collision can't happen.
	t.Run("id with reserved url- prefix is rejected", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: url-deadbeef\nname: Colliding Project\n"), 0o644)

		_, err := projectspec.ReadProjectMeta(dir)
		if err == nil || !strings.Contains(err.Error(), "url-") {
			t.Fatalf("expected rejection of a url--prefixed id, got %v", err)
		}
	})

	t.Run("missing name", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\n"), 0o644)

		_, err := projectspec.ReadProjectMeta(dir)
		if err == nil || !strings.Contains(err.Error(), "name is required") {
			t.Fatalf("expected name is required, got %v", err)
		}
	})

	// PR-1d codex round-5 Minor: a whitespace-only name (e.g. "  ") must be
	// rejected the same as an empty one. Before this fix, project.yaml
	// validation only checked `meta.Name == ""`, so a whitespace-only name
	// slipped through registration while workspace export/apply's own
	// checks (internal/api/project_service.go) already used
	// strings.TrimSpace — a project registered this way could be exported
	// successfully but its own export could never be applied back (apply
	// would treat the whitespace name as absent and detach the project).
	t.Run("whitespace-only name", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: \"   \"\n"), 0o644)

		_, err := projectspec.ReadProjectMeta(dir)
		if err == nil || !strings.Contains(err.Error(), "name is required") {
			t.Fatalf("expected name is required for a whitespace-only name, got %v", err)
		}
	})

	t.Run("missing project yaml", func(t *testing.T) {
		_, err := projectspec.ReadProjectMeta(t.TempDir())
		if err == nil {
			t.Fatal("expected error for missing project.yaml")
		}
	})

	t.Run("deprecated workspace id", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test\nworkspace_id: ws-1\n"), 0o644)

		_, err := projectspec.ReadProjectMeta(dir)
		if err == nil || !strings.Contains(err.Error(), "workspace_id is no longer supported") {
			t.Fatalf("expected deprecated workspace_id error, got %v", err)
		}
	})
}

func TestReadProjectMeta_EnvInterpolation(t *testing.T) {
	// env/host_commands/additional_bindings are no longer accepted in project.yaml
	// (they are now workspace-level or project.local.yaml fields). This test
	// verifies that these keys produce the removal error.
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	_ = os.MkdirAll(boidDir, 0o755)
	yaml := "id: test-proj\nname: Test Project\nhost_commands:\n  my-tool:\n    path: ${TEST_BOID_HOME}/bin/my-tool\n"
	_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644)
	t.Setenv("TEST_BOID_HOME", "/home/testuser")

	_, err := projectspec.ReadProjectMeta(dir)
	if err == nil {
		t.Fatal("expected error for host_commands in project.yaml, got nil")
	}
	if !strings.Contains(err.Error(), `top-level "host_commands" is no longer supported`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ${WORKTREE} と ${PROJECT_WORKDIR} トークンが project.local.yaml では温存されることを検証する。
// Note: project.yaml では additional_bindings は rejected になったため、
// このテストは project.local.yaml 経由での確認に切り替える。
func TestReadProjectMeta_DeferredWorktreeTokens(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	_ = os.MkdirAll(boidDir, 0o755)
	// additional_bindings in project.yaml は now a removed key → error expected
	yaml := "id: test-proj\nname: Test Project\nadditional_bindings:\n  - source: ${PROJECT_WORKDIR}/global.json\n    target: ${WORKTREE}/global.json\n    is_file: true\n"
	_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644)
	t.Setenv("PROJECT_WORKDIR", "/should-not-be-used")
	t.Setenv("WORKTREE", "/should-not-be-used")

	_, err := projectspec.ReadProjectMeta(dir)
	if err == nil {
		t.Fatal("expected error for additional_bindings in project.yaml, got nil")
	}
	if !strings.Contains(err.Error(), `top-level "additional_bindings" is no longer supported`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestReadProjectMetaWithKits_SessionBehaviors_Preserved verifies that
// session_behaviors survives ReadProjectMetaWithKits' meta cloning
// (cloneProjectMeta) — a field easy to lose silently since it isn't part of
// the task_behaviors overlay-merge path cloneProjectMeta otherwise exists
// for. This only checks the value round-trips through the load; the deep
// -copy guarantee itself (mutating a caller's copy must not corrupt a
// cached one) is checked where it actually matters — ProjectStore's cache —
// by TestGetWithWorkspace_SessionBehaviorsNotMutated in
// project_store_hydrate_test.go (ReadProjectMetaWithKits re-reads from disk
// every call, so mutating its return value here can never leak into a
// second call regardless of whether the clone is deep or shallow).
func TestReadProjectMetaWithKits_SessionBehaviors_Preserved(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	_ = os.MkdirAll(boidDir, 0o755)
	_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(`id: test-proj
name: Test
session_behaviors:
  shape:
    harness_type: codex
    model: o3-mini
task_behaviors:
  dev:
    name: dev
`), 0o644)

	meta, err := projectspec.ReadProjectMetaWithKits(dir)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	shape, ok := meta.SessionBehaviors["shape"]
	if !ok || shape.HarnessType != "codex" || shape.Model != "o3-mini" {
		t.Fatalf("session_behaviors not preserved: %+v", meta.SessionBehaviors)
	}
}

// TestReadProjectMetaWithKits_LocalKits verifies that behavior-level kits in
// project.yaml now produce a removal error (kits moved to workspace.yaml).
func TestReadProjectMetaWithKits_LocalKits(t *testing.T) {
	for _, name := range []string{
		"single local kit",
		"local kit with hooks",
		"env interpolation",
		"missing local kit is warned, not fatal",
		"multiple local kits",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			boidDir := filepath.Join(dir, ".boid")
			_ = os.MkdirAll(boidDir, 0o755)
			_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test Project\ntask_behaviors:\n  dev:\n    kits:\n      - some-kit\n"), 0o644)

			_, err := projectspec.ReadProjectMetaWithKits(dir)
			if err == nil {
				t.Fatal("expected removal error for behavior-level kits, got nil")
			}
			if !strings.Contains(err.Error(), "task_behaviors.dev.kits is no longer supported") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestReadProjectMetaWithKits_ProjectLocalYAMLIgnored verifies that a stray
// .boid/project.local.yaml (the file format this package no longer reads —
// its loader/writer were removed as dead code, git gateway cutover having
// made it unreachable in production) does not leak env/host_commands/
// additional_bindings into ReadProjectMetaWithKits' output, at either the
// top level or behavior level. This doesn't call any of the removed
// project.local.yaml APIs directly — it exercises the same silent-ignore
// guarantee the deleted TestReadProjectMetaWithKits_ProjectLocalOverlayIgnored
// used to, but through the one entry point that could still plausibly regress
// (a future re-introduction of project.local.yaml reading inside
// ReadProjectMetaWithKits itself).
func TestReadProjectMetaWithKits_ProjectLocalYAMLIgnored(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	_ = os.MkdirAll(boidDir, 0o755)
	projectYAML := "id: test-proj\nname: Test Project\ntask_behaviors:\n  dev: {}\n"
	_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644)
	localYAML := "version: 1\nhost_commands: [gh, aws]\nenv:\n  LOCAL_ONLY: leaked\nadditional_bindings:\n  - source: /tmp/local-only\n    target: /tmp/local-only\n"
	_ = os.WriteFile(filepath.Join(boidDir, "project.local.yaml"), []byte(localYAML), 0o644)

	meta, err := projectspec.ReadProjectMetaWithKits(dir)
	if err != nil {
		t.Fatalf("ReadProjectMetaWithKits: %v", err)
	}
	if len(meta.HostCommands) != 0 {
		t.Fatalf("expected project.local.yaml host_commands to be ignored, got %+v", meta.HostCommands)
	}
	if _, ok := meta.Env["LOCAL_ONLY"]; ok {
		t.Fatalf("expected project.local.yaml env to be ignored, got %+v", meta.Env)
	}
	if len(meta.AdditionalBindings) != 0 {
		t.Fatalf("expected project.local.yaml additional_bindings to be ignored, got %+v", meta.AdditionalBindings)
	}
	dev := meta.TaskBehaviors["dev"]
	if len(dev.HostCommands) != 0 {
		t.Fatalf("expected project.local.yaml host_commands to be ignored at behavior level, got %+v", dev.HostCommands)
	}
}

func TestReadProjectMetaWithKits_BehaviorLevelKitsRejected(t *testing.T) {
	t.Run("behavior-level kits rejected with guidance", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(filepath.Join(boidDir, "kits", "go-dev"), 0o755)
		projectYAML := "id: test-proj\nname: Test Project\ntask_behaviors:\n  dev:\n    kits:\n      - go-dev\n"
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644)
		_ = os.WriteFile(filepath.Join(boidDir, "kits", "go-dev", "kit.yaml"), []byte("env:\n  A: a\n"), 0o644)

		_, err := projectspec.ReadProjectMetaWithKits(dir)
		if err == nil {
			t.Fatal("expected error for behavior-level kits, got nil")
		}
		if !strings.Contains(err.Error(), "task_behaviors.dev.kits is no longer supported") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestHostCommands_NewDSL(t *testing.T) {
	// host_commands in project.yaml is now a removed key; these DSL tests
	// verify the rejection behavior. project.local.yaml (which used to carry
	// the map-form/policy DSL coverage) was removed entirely as dead code
	// (git gateway cutover made it unreachable in production); its DSL
	// parsing coverage lives on via kit.yaml / workspace host_commands
	// (internal/dispatcher/host_commands_test.go).
	t.Run("list form", func(t *testing.T) {
		// project.yaml host_commands now rejected; verify error message.
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)
		yaml := "id: test-proj\nname: Test Project\nhost_commands: [gh, aws, az]\n"
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644)

		_, err := projectspec.ReadProjectMeta(dir)
		if err == nil {
			t.Fatal("expected error for host_commands in project.yaml, got nil")
		}
		if !strings.Contains(err.Error(), `top-level "host_commands" is no longer supported`) {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("zero-config map form", func(t *testing.T) {
		// project.yaml host_commands now rejected; verify error message.
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)
		yaml := "id: test-proj\nname: Test Project\nhost_commands:\n  gh:\n  aws:\n"
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644)

		_, err := projectspec.ReadProjectMeta(dir)
		if err == nil {
			t.Fatal("expected error for host_commands in project.yaml, got nil")
		}
		if !strings.Contains(err.Error(), `top-level "host_commands" is no longer supported`) {
			t.Fatalf("unexpected error: %v", err)
		}
	})

}

func TestReadProjectMeta_HostCommandRelativePath(t *testing.T) {
	// host_commands in project.yaml is a removed key; sub-tests verify
	// that the key is rejected.

	t.Run("relative path in project.yaml rejected", func(t *testing.T) {
		// project.yaml no longer accepts host_commands.
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)

		yaml := "id: test-proj\nname: Test Project\nhost_commands:\n  my-cmd:\n    path: scripts/run.sh\n    allow: [\"*\"]\n"
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644)

		_, err := projectspec.ReadProjectMeta(dir)
		if err == nil {
			t.Fatal("expected error for host_commands in project.yaml, got nil")
		}
		if !strings.Contains(err.Error(), `top-level "host_commands" is no longer supported`) {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("directory traversal in project.yaml rejected (removed key)", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)

		yaml := "id: test-proj\nname: Test Project\nhost_commands:\n  my-cmd:\n    path: ../../../etc/passwd\n"
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644)

		_, err := projectspec.ReadProjectMeta(dir)
		if err == nil {
			t.Fatal("expected error for host_commands in project.yaml")
		}
		if !strings.Contains(err.Error(), `top-level "host_commands" is no longer supported`) {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("symlink traversal in project.yaml rejected (removed key)", func(t *testing.T) {
		// host_commands is a removed key in project.yaml; the removal error fires first.
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)

		yaml := "id: test-proj\nname: Test Project\nhost_commands:\n  my-cmd:\n    path: scripts/escape/passwd\n"
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644)

		_, err := projectspec.ReadProjectMeta(dir)
		if err == nil {
			t.Fatal("expected error for host_commands in project.yaml")
		}
		if !strings.Contains(err.Error(), `top-level "host_commands" is no longer supported`) {
			t.Fatalf("unexpected error: %v", err)
		}
	})

}

// ---------------------------------------------------------------------------
// Top-level kits tests
// ---------------------------------------------------------------------------

func TestReadProjectMetaWithKits_TopLevelKits_MergesIntoAllBehaviors(t *testing.T) {
	// top-level kits in project.yaml is now a removed key; verify rejection.
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	_ = os.MkdirAll(boidDir, 0o755)
	_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test\nkits:\n  - go-dev\ntask_behaviors:\n  dev:\n    name: dev\n"), 0o644)

	_, err := projectspec.ReadProjectMetaWithKits(dir)
	if err == nil {
		t.Fatal("expected error for top-level kits in project.yaml, got nil")
	}
	if !strings.Contains(err.Error(), `top-level "kits" is no longer supported`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestReadProjectMetaWithKits_TopLevelKits_PropagatedToMeta verifies that
// project.local.yaml host_commands and env are propagated to meta-level fields
// (used by session dispatch which bypasses behavior lookup). This replaces the
// former top-level-kits test which is now invalid (kits removed from project.yaml).
func TestReadProjectMetaWithKits_TopLevelKits_PropagatedToMeta(t *testing.T) {
	// top-level kits, env, host_commands, additional_bindings in project.yaml
	// are all removed keys; verify all are rejected with a single error.
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	_ = os.MkdirAll(boidDir, 0o755)

	_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(`id: test-proj
name: Test
kits:
  - github-cli
host_commands:
  playwright-cli:
    allow: ['*']
additional_bindings:
  - source: /opt/google/chrome
env:
  PROJ_ENV: from-project
task_behaviors:
  dev:
    name: dev
`), 0o644)

	_, err := projectspec.ReadProjectMetaWithKits(dir)
	if err == nil {
		t.Fatal("expected error for removed keys in project.yaml, got nil")
	}
	for _, key := range []string{"kits", "env", "host_commands", "additional_bindings"} {
		if !strings.Contains(err.Error(), fmt.Sprintf(`top-level %q is no longer supported`, key)) {
			t.Errorf("error should mention %q, got: %v", key, err)
		}
	}
}

// TestReadProjectMetaWithKits_TopLevelKits_ProjectLocalWinsOnMeta verifies that
// project.local.yaml host_commands and env win over workspace entries when
// merged into behavior-level fields.
func TestReadProjectMetaWithKits_TopLevelKits_ProjectLocalWinsOnMeta(t *testing.T) {
	// project.yaml top-level kits, env, host_commands are removed keys.
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	_ = os.MkdirAll(boidDir, 0o755)

	_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(`id: test-proj
name: Test
kits:
  - kit-a
host_commands:
  gh:
    path: /usr/bin/gh
    allow:
      - pr
env:
  FOO: project
task_behaviors:
  dev:
    name: dev
`), 0o644)

	_, err := projectspec.ReadProjectMetaWithKits(dir)
	if err == nil {
		t.Fatal("expected error for removed keys in project.yaml, got nil")
	}
	for _, key := range []string{"kits", "env", "host_commands"} {
		if !strings.Contains(err.Error(), fmt.Sprintf(`top-level %q is no longer supported`, key)) {
			t.Errorf("error should mention %q, got: %v", key, err)
		}
	}
}

// TestReadProjectMetaWithKits_MissingTopLevelKit_WarnsAndSkips verifies that
// the removal error message is returned for top-level kits reference (since
// kits is a removed key, not a warn-and-skip scenario any more).
func TestReadProjectMetaWithKits_MissingTopLevelKit_WarnsAndSkips(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	_ = os.MkdirAll(boidDir, 0o755)

	_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(`id: test-proj
name: Test
kits:
  - github.com/novshi-tech/boid-kits/claude-code
host_commands:
  gh:
    path: /usr/bin/gh
    allow: ['*']
task_behaviors:
  dev:
    name: dev
`), 0o644)

	_, err := projectspec.ReadProjectMetaWithKits(dir)
	if err == nil {
		t.Fatal("expected error for removed keys in project.yaml, got nil")
	}
	// Both kits and host_commands are rejected.
	if !strings.Contains(err.Error(), `top-level "kits" is no longer supported`) {
		t.Fatalf("expected kits rejection, got: %v", err)
	}
	if !strings.Contains(err.Error(), `top-level "host_commands" is no longer supported`) {
		t.Fatalf("expected host_commands rejection, got: %v", err)
	}
}

// TestReadProjectMetaWithKits_MissingBehaviorKit_WarnsAndSkips verifies that
// behavior-level kits in project.yaml is a removed key (not a warn-and-skip).
func TestReadProjectMetaWithKits_MissingBehaviorKit_WarnsAndSkips(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	_ = os.MkdirAll(boidDir, 0o755)

	_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(`id: test-proj
name: Test
task_behaviors:
  dev:
    name: dev
    kits:
      - github.com/novshi-tech/boid-kits/claude-code
`), 0o644)

	_, err := projectspec.ReadProjectMetaWithKits(dir)
	if err == nil {
		t.Fatal("expected error for behavior-level kits in project.yaml, got nil")
	}
	if !strings.Contains(err.Error(), `task_behaviors.dev.kits is no longer supported`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReadProjectMetaWithKits_TopLevelKits_AgentOnlyHooksAllowed(t *testing.T) {
	// top-level kits in project.yaml is now a removed key; verify rejection.
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	_ = os.MkdirAll(boidDir, 0o755)

	_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test\nkits:\n  - agent-kit\ntask_behaviors:\n  dev:\n    name: dev\n"), 0o644)

	_, err := projectspec.ReadProjectMetaWithKits(dir)
	if err == nil {
		t.Fatal("expected error for top-level kits in project.yaml, got nil")
	}
	if !strings.Contains(err.Error(), `top-level "kits" is no longer supported`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReadProjectMetaWithKits_TopLevelKits_ScopeValidation_NonAgentHookRejected(t *testing.T) {
	// top-level kits in project.yaml is now a removed key; verify rejection.
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	_ = os.MkdirAll(boidDir, 0o755)

	_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test\nkits:\n  - hook-kit\ntask_behaviors:\n  dev:\n    name: dev\n"), 0o644)

	_, err := projectspec.ReadProjectMetaWithKits(dir)
	if err == nil {
		t.Fatal("expected error for top-level kits in project.yaml, got nil")
	}
	if !strings.Contains(err.Error(), `top-level "kits" is no longer supported`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBindMount_Optional_PropagatedFromKitYAML(t *testing.T) {
	// behavior-level kits in project.yaml is now a removed key; verify rejection.
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	_ = os.MkdirAll(boidDir, 0o755)
	_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test\ntask_behaviors:\n  dev:\n    name: dev\n    kits:\n      - opt-kit\n"), 0o644)

	_, err := projectspec.ReadProjectMetaWithKits(dir)
	if err == nil {
		t.Fatal("expected error for behavior-level kits in project.yaml, got nil")
	}
	if !strings.Contains(err.Error(), `task_behaviors.dev.kits is no longer supported`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Phase 1-2: supervisor / executor canonical names + plan / dev aliases.
//
// These tests pin down the behavior-name alias contract: the YAML loader
// accepts both the legacy alias keys ("plan" / "dev") and the new canonical
// keys ("supervisor" / "executor"). When an alias is used, the map is
// normalized to the canonical key and a deprecation warning is logged. When
// both an alias and its canonical counterpart appear in the same file, the
// loader fails with a duplicate-definition error.
// ---------------------------------------------------------------------------

// TestReadProjectMeta_LegacyNames_KeptVerbatim verifies that "plan" and "dev"
// are loaded as ordinary behavior names: the key survives unchanged, no
// canonical counterpart is invented, and no deprecation warning fires. These
// were the two entries of the retired BehaviorAliases table.
func TestReadProjectMeta_LegacyNames_KeptVerbatim(t *testing.T) {
	cases := []struct {
		name     string
		key      string
		notMade  string
	}{
		{name: "plan is not rewritten to supervisor", key: "plan", notMade: "supervisor"},
		{name: "dev is not rewritten to executor", key: "dev", notMade: "executor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureSlog(t)
			dir := t.TempDir()
			boidDir := filepath.Join(dir, ".boid")
			if err := os.MkdirAll(boidDir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			yaml := `
id: test-proj
name: Test Project
task_behaviors:
  ` + tc.key + `:
    name: ` + tc.key + `
    traits:
      - artifact
`
			if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
				t.Fatalf("write yaml: %v", err)
			}

			meta, err := projectspec.ReadProjectMeta(dir)
			if err != nil {
				t.Fatalf("ReadProjectMeta: %v", err)
			}
			if _, ok := meta.TaskBehaviors[tc.key]; !ok {
				t.Fatalf("expected key %q to survive verbatim, got keys=%v", tc.key, behaviorKeys(meta))
			}
			if _, ok := meta.TaskBehaviors[tc.notMade]; ok {
				t.Errorf("key %q was invented from %q; the alias table is supposed to be gone (keys=%v)",
					tc.notMade, tc.key, behaviorKeys(meta))
			}
			if len(meta.TaskBehaviors[tc.key].Traits) != 1 || meta.TaskBehaviors[tc.key].Traits[0] != "artifact" {
				t.Errorf("Traits fell off during load: %v", meta.TaskBehaviors[tc.key].Traits)
			}
			if strings.Contains(buf.String(), "deprecated") {
				t.Errorf("no deprecation warning expected for a free name, got:\n%s", buf.String())
			}
		})
	}
}

// TestReadProjectMeta_BehaviorCanonicalName_NoWarning verifies that project
// authors who already use the canonical names see no deprecation noise.
func TestReadProjectMeta_BehaviorCanonicalName_NoWarning(t *testing.T) {
	buf := captureSlog(t)
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yaml := `
id: test-proj
name: Test Project
worktree: true
task_behaviors:
  supervisor:
    name: supervisor
  executor:
    name: executor
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	meta, err := projectspec.ReadProjectMeta(dir)
	if err != nil {
		t.Fatalf("ReadProjectMeta: %v", err)
	}
	if _, ok := meta.TaskBehaviors["supervisor"]; !ok {
		t.Errorf("supervisor missing: keys=%v", behaviorKeys(meta))
	}
	if _, ok := meta.TaskBehaviors["executor"]; !ok {
		t.Errorf("executor missing: keys=%v", behaviorKeys(meta))
	}
	if strings.Contains(buf.String(), "deprecated") {
		t.Errorf("did not expect deprecation log for canonical names, got:\n%s", buf.String())
	}
}

// TestReadProjectMeta_RemovedBehaviorFields_RejectsAtLoad verifies that every
// field removed in Phase 3-1 produces a descriptive load-time error pointing
// callers at the new resolution path. The error format is fixed by
// removedBehaviorFieldGuidance — the test pins the message so accidental
// rewording trips CI.
func TestReadProjectMeta_RemovedBehaviorFields_RejectsAtLoad(t *testing.T) {
	cases := []struct {
		field string
		body  string
	}{
		{"worktree", "    worktree: true\n"},
		{"base_branch", "    base_branch: main\n"},
		{"branch_prefix", "    branch_prefix: feature/\n"},
		{"default_payload", "    default_payload:\n      foo: bar\n"},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			dir := t.TempDir()
			boidDir := filepath.Join(dir, ".boid")
			if err := os.MkdirAll(boidDir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			yaml := `id: test-proj
name: Test Project
task_behaviors:
  dev:
    name: dev
` + tc.body
			if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
				t.Fatalf("write yaml: %v", err)
			}
			_, err := projectspec.ReadProjectMeta(dir)
			if err == nil {
				t.Fatalf("expected error for %q, got nil", tc.field)
			}
			needle := "task_behaviors.dev." + tc.field + " is no longer supported"
			if !strings.Contains(err.Error(), needle) {
				t.Errorf("expected error to contain %q, got: %v", needle, err)
			}
		})
	}
}

// TestReadProjectMeta_LegacyNameAlongsideFormerCanonical verifies the change
// this removal is really about: "plan" and "supervisor" (and "dev" /
// "executor") are now unrelated names that may coexist in one project.yaml.
// While the alias table existed this exact document was rejected as a
// duplicate definition, which made "plan" unusable as a work-content behavior
// name for any project that also had a "supervisor".
func TestReadProjectMeta_LegacyNameAlongsideFormerCanonical(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yaml := `
id: test-proj
name: Test Project
task_behaviors:
  plan:
    name: plan
    traits:
      - artifact
  supervisor:
    name: supervisor
  dev:
    name: dev
  executor:
    name: executor
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	meta, err := projectspec.ReadProjectMeta(dir)
	if err != nil {
		t.Fatalf("ReadProjectMeta: %v", err)
	}
	for _, want := range []string{"plan", "supervisor", "dev", "executor"} {
		if _, ok := meta.TaskBehaviors[want]; !ok {
			t.Errorf("behavior %q missing, got keys=%v", want, behaviorKeys(meta))
		}
	}
	if len(meta.TaskBehaviors) != 4 {
		t.Errorf("len(TaskBehaviors) = %d, want 4 (no mirrors, no merges); keys=%v",
			len(meta.TaskBehaviors), behaviorKeys(meta))
	}
	// The four must stay distinct values, not collapse onto each other.
	if len(meta.TaskBehaviors["plan"].Traits) != 1 {
		t.Errorf("plan traits = %v, want [artifact]", meta.TaskBehaviors["plan"].Traits)
	}
	if len(meta.TaskBehaviors["supervisor"].Traits) != 0 {
		t.Errorf("supervisor picked up plan's traits: %v", meta.TaskBehaviors["supervisor"].Traits)
	}
}

// TestReadProjectMetaWithKits_NoAliasMirrorsAdded verifies that the runtime
// boundary no longer injects back-compat mirror entries: what the file says is
// exactly what runtime code sees.
func TestReadProjectMetaWithKits_NoAliasMirrorsAdded(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yaml := `
id: test-proj
name: Test Project
task_behaviors:
  supervisor:
    name: supervisor
    traits:
      - artifact
  executor:
    name: executor
    traits:
      - verification
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	meta, err := projectspec.ReadProjectMetaWithKits(dir)
	if err != nil {
		t.Fatalf("ReadProjectMetaWithKits: %v", err)
	}
	if len(meta.TaskBehaviors) != 2 {
		t.Errorf("len(TaskBehaviors) = %d, want 2 (no alias mirrors); keys=%v",
			len(meta.TaskBehaviors), behaviorKeys(meta))
	}
	for _, unwanted := range []string{"plan", "dev"} {
		if _, ok := meta.TaskBehaviors[unwanted]; ok {
			t.Errorf("alias mirror %q was added, got keys=%v", unwanted, behaviorKeys(meta))
		}
	}
}



func behaviorKeys(meta *projectspec.ProjectMeta) []string {
	out := make([]string, 0, len(meta.TaskBehaviors))
	for k := range meta.TaskBehaviors {
		out = append(out, k)
	}
	return out
}



// repoRootFromTestFile returns the absolute path to the boid repo root by
// walking up from the location of this test file. The test file lives at
// internal/orchestrator/spec_loader_test.go, so the repo root is two
// directories above it. The helper centralizes the lookup so the Phase 4-2
// self-yaml verify test below remains stable if the file is ever moved.
func repoRootFromTestFile(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller(0) failed; cannot locate test source path")
	}
	// thisFile = .../internal/orchestrator/spec_loader_test.go
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
}

// TestReadProjectMeta_BoidSelfProjectYAML_LoadsInCanonicalForm is the Phase
// 4-2 verify test: the boid repo's own .boid/project.yaml has been migrated
// to the canonical schema (project-top worktree + canonical behavior names
// supervisor / executor, with no behavior-level readonly/worktree/etc.).
// Loading it must succeed (i.e. Phase 3-1's reject-removed-fields check must
// not fire) and the canonical behaviors must be present.
//
// This test guards against accidental regressions where someone edits
// .boid/project.yaml in a way that re-introduces the removed fields or
// reverts to the legacy "plan" / "dev" keys without updating the canonical
// pair. It mirrors the spirit of the e2e fixtures migration done in P3-2
// (PR #408), but for the boid repo's own self-configuration.
func TestReadProjectMeta_BoidSelfProjectYAML_LoadsInCanonicalForm(t *testing.T) {
	repoRoot := repoRootFromTestFile(t)
	yamlPath := filepath.Join(repoRoot, ".boid", "project.yaml")
	if _, err := os.Stat(yamlPath); err != nil {
		t.Skipf("self project.yaml not found at %s (this is expected only when running tests outside a checkout): %v", yamlPath, err)
	}

	// ReadProjectMeta runs the same rejectRemovedBehaviorFields guard as the
	// daemon, so this also asserts that the file is free of the Phase 3-1
	// removed fields.
	meta, err := projectspec.ReadProjectMeta(repoRoot)
	if err != nil {
		t.Fatalf("ReadProjectMeta on boid self project.yaml failed: %v\n"+
			"Hint: behavior-level worktree/base_branch/branch_prefix/default_payload "+
			"were removed in Phase 3-1; if you see one of those in the error, migrate the field "+
			"to the project-top equivalent or remove it. readonly is allowed again as of Track A2.", err)
	}

	// branch-policy-simplification Phase 2 retired the project-top
	// worktree field entirely; per-task branch creation lives in the
	// executor's default_instruction (`git checkout -b
	// boid/${BOID_TASK_ID:0:8}` before /dev-pr-flow). base_branch is
	// intentionally omitted so the daemon defaults to the current HEAD
	// branch.

	// Canonical behaviors must be present.
	for _, name := range []string{"supervisor", "executor"} {
		if _, ok := meta.TaskBehaviors[name]; !ok {
			t.Errorf("canonical behavior %q missing from self project.yaml; keys=%v", name, behaviorKeys(meta))
		}
	}

	// Each canonical behavior must carry a default_instruction (the daemon
	// dispatches against it when a task is created without an explicit
	// payload). The exact message contents are out of scope here — P4-1
	// will refresh those — but the field must be populated.
	for _, name := range []string{"supervisor", "executor"} {
		b, ok := meta.TaskBehaviors[name]
		if !ok {
			continue
		}
		if b.DefaultInstruction == nil {
			t.Errorf("behavior %q has no default_instruction; agents would receive an empty prompt", name)
			continue
		}
		if strings.TrimSpace(b.DefaultInstruction.Message) == "" {
			t.Errorf("behavior %q default_instruction.message is empty", name)
		}
	}
}



func writeProjectYAML(t *testing.T, dir, content string) {
	t.Helper()
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir boid: %v", err)
	}
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}
}

func TestReadProjectMeta_Capabilities_DockerPresent(t *testing.T) {
	// capabilities is a removed key in project.yaml; verify it is rejected.
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: proj-docker
name: Docker Project
task_behaviors:
  executor:
    name: executor
capabilities:
  docker: {}
`)
	_, err := projectspec.ReadProjectMeta(dir)
	if err == nil {
		t.Fatal("expected error for capabilities in project.yaml, got nil")
	}
	if !strings.Contains(err.Error(), `top-level "capabilities" is no longer supported`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReadProjectMeta_Capabilities_DockerAbsent(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: proj-no-docker
name: No Docker
task_behaviors:
  executor:
    name: executor
`)
	meta, err := projectspec.ReadProjectMeta(dir)
	if err != nil {
		t.Fatalf("ReadProjectMeta: %v", err)
	}
	if meta.Capabilities.Docker != nil {
		t.Error("Capabilities.Docker should be nil when capabilities section is absent")
	}
}

func TestReadProjectMeta_Capabilities_NoDockerKey(t *testing.T) {
	// capabilities is a removed key in project.yaml; verify it is rejected.
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: proj-caps-no-docker
name: Caps No Docker
task_behaviors:
  executor:
    name: executor
capabilities: {}
`)
	_, err := projectspec.ReadProjectMeta(dir)
	if err == nil {
		t.Fatal("expected error for capabilities in project.yaml, got nil")
	}
	if !strings.Contains(err.Error(), `top-level "capabilities" is no longer supported`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Track A2: free naming, default_task_behavior, readonly in behaviors, and
// canonical-name deprecation warnings.

// TestReadProjectMeta_DefaultTaskBehavior_Parsed verifies that the new
// default_task_behavior top-level key is parsed correctly.
func TestReadProjectMeta_DefaultTaskBehavior_Parsed(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: proj-default-behavior
name: DefaultTaskBehavior Test
default_task_behavior: dev-task
task_behaviors:
  dev-task:
    traits:
      - artifact
`)
	meta, err := projectspec.ReadProjectMeta(dir)
	if err != nil {
		t.Fatalf("ReadProjectMeta: %v", err)
	}
	if meta.DefaultTaskBehavior != "dev-task" {
		t.Errorf("DefaultTaskBehavior = %q, want %q", meta.DefaultTaskBehavior, "dev-task")
	}
}

// TestReadProjectMeta_TaskBehaviorReadonly_Parsed verifies that readonly:false
// in a behavior entry is parsed correctly into TaskBehavior.Readonly.
func TestReadProjectMeta_TaskBehaviorReadonly_Parsed(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: proj-behavior-readonly
name: Readonly Test
task_behaviors:
  dev-task:
    readonly: false
  research:
    readonly: true
`)
	meta, err := projectspec.ReadProjectMeta(dir)
	if err != nil {
		t.Fatalf("ReadProjectMeta: %v", err)
	}
	devTask := meta.TaskBehaviors["dev-task"]
	if devTask.Readonly == nil {
		t.Error("dev-task: Readonly is nil, want *false")
	} else if *devTask.Readonly {
		t.Errorf("dev-task: Readonly = true, want false")
	}
	research := meta.TaskBehaviors["research"]
	if research.Readonly == nil {
		t.Error("research: Readonly is nil, want *true")
	} else if !*research.Readonly {
		t.Errorf("research: Readonly = false, want true")
	}
}

// TestReadProjectMetaWithKits_CanonicalNameDeprecation_EmitsWarn verifies that
// ReadProjectMetaWithKits emits deprecation warnings when the project uses
// canonical behavior names "supervisor" or "executor".
func TestReadProjectMetaWithKits_CanonicalNameDeprecation_EmitsWarn(t *testing.T) {
	buf := captureSlog(t)
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: proj-canonical-warn-`+t.Name()+`
name: Canonical Warn Test
task_behaviors:
  supervisor:
    traits:
      - artifact
  executor:
    readonly: false
`)
	_, err := projectspec.ReadProjectMetaWithKits(dir)
	if err != nil {
		t.Fatalf("ReadProjectMetaWithKits: %v", err)
	}
	log := buf.String()
	if !strings.Contains(log, "deprecated") {
		t.Errorf("expected deprecation warning, got:\n%s", log)
	}
	if !strings.Contains(log, "supervisor") || !strings.Contains(log, "executor") {
		t.Errorf("expected deprecation for both supervisor and executor, got:\n%s", log)
	}
}

// TestReadProjectMetaWithKits_CanonicalNameDeprecation_OncePerProject verifies
// that the deprecation warning fires at most once per project directory per
// daemon run (second call emits nothing new).
func TestReadProjectMetaWithKits_CanonicalNameDeprecation_OncePerProject(t *testing.T) {
	buf := captureSlog(t)
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: proj-canonical-once-`+t.Name()+`
name: Once Per Project Test
task_behaviors:
  supervisor:
    traits:
      - artifact
`)
	_, err := projectspec.ReadProjectMetaWithKits(dir)
	if err != nil {
		t.Fatalf("ReadProjectMetaWithKits (first): %v", err)
	}
	countAfterFirst := strings.Count(buf.String(), "deprecated")
	if countAfterFirst == 0 {
		t.Error("expected deprecation warning after first load, got none")
	}

	// Second load of same directory: no new deprecation warnings.
	_, err = projectspec.ReadProjectMetaWithKits(dir)
	if err != nil {
		t.Fatalf("ReadProjectMetaWithKits (second): %v", err)
	}
	countAfterSecond := strings.Count(buf.String(), "deprecated")
	if countAfterSecond != countAfterFirst {
		t.Errorf("second load of same project emitted new warnings: count went from %d to %d", countAfterFirst, countAfterSecond)
	}
}

// TestReadProjectMetaWithKits_CanonicalNameDeprecation_SuppressedByEnvVar verifies
// that BOID_NO_DEPRECATION_WARN=1 suppresses the canonical name warning.
func TestReadProjectMetaWithKits_CanonicalNameDeprecation_SuppressedByEnvVar(t *testing.T) {
	t.Setenv("BOID_NO_DEPRECATION_WARN", "1")
	buf := captureSlog(t)
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: proj-canonical-suppressed-`+t.Name()+`
name: Suppressed Warning Test
task_behaviors:
  supervisor:
    traits:
      - artifact
`)
	_, err := projectspec.ReadProjectMetaWithKits(dir)
	if err != nil {
		t.Fatalf("ReadProjectMetaWithKits: %v", err)
	}
	if strings.Contains(buf.String(), "deprecated") {
		t.Errorf("expected no deprecation warning with BOID_NO_DEPRECATION_WARN=1, got:\n%s", buf.String())
	}
}

// TestReadProjectMetaWithKits_ExecutorNoReadonly_ExtraWarn verifies that
// "executor" without explicit readonly emits an extra compat warning.
func TestReadProjectMetaWithKits_ExecutorNoReadonly_ExtraWarn(t *testing.T) {
	buf := captureSlog(t)
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: proj-executor-compat-`+t.Name()+`
name: Executor Compat Test
task_behaviors:
  executor:
`)
	_, err := projectspec.ReadProjectMetaWithKits(dir)
	if err != nil {
		t.Fatalf("ReadProjectMetaWithKits: %v", err)
	}
	log := buf.String()
	if !strings.Contains(log, "readonly") {
		t.Errorf("expected compat readonly warning for executor without explicit readonly, got:\n%s", log)
	}
}

// TestReadProjectMetaWithKits_ExecutorExplicitReadonly_NoCompatWarn verifies
// that executor with explicit readonly:false does NOT emit the extra compat warning.
func TestReadProjectMetaWithKits_ExecutorExplicitReadonly_NoCompatWarn(t *testing.T) {
	buf := captureSlog(t)
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: proj-executor-explicit-`+t.Name()+`
name: Executor Explicit Readonly Test
task_behaviors:
  executor:
    readonly: false
`)
	_, err := projectspec.ReadProjectMetaWithKits(dir)
	if err != nil {
		t.Fatalf("ReadProjectMetaWithKits: %v", err)
	}
	log := buf.String()
	// Should still warn about canonical name "executor" being deprecated,
	// but NOT the readonly compat warning.
	if strings.Contains(log, "backward compatibility") {
		t.Errorf("unexpected compat readonly warning when explicit readonly:false is set, got:\n%s", log)
	}
}
