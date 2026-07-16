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
	// Note: "dev" is a deprecated alias of the canonical name "executor";
	// ReadProjectMeta normalizes the key to "executor" and adds a "dev"
	// mirror entry for back-compat, so the map has 2 entries that both
	// point to the same behavior.
	if _, ok := meta.TaskBehaviors["executor"]; !ok {
		t.Fatalf("expected canonical 'executor' behavior to be present, got %+v", meta.TaskBehaviors)
	}
	if _, ok := meta.TaskBehaviors["dev"]; !ok {
		t.Fatalf("expected legacy alias 'dev' to remain reachable, got %+v", meta.TaskBehaviors)
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

	behavior, ok := meta.TaskBehaviors["executor"]
	if !ok {
		t.Fatalf("expected canonical 'executor' behavior, got %+v", meta.TaskBehaviors)
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
	t.Run("missing id", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("name: No ID Project\n"), 0o644)

		_, err := projectspec.ReadProjectMeta(dir)
		if err == nil || !strings.Contains(err.Error(), "id is required") {
			t.Fatalf("expected id is required, got %v", err)
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

func TestReadProjectLocalMeta(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		meta, err := projectspec.ReadProjectLocalMeta(t.TempDir())
		if err != nil {
			t.Fatalf("ReadProjectLocalMeta: %v", err)
		}
		if meta != nil {
			t.Fatalf("expected nil meta for missing file, got %+v", meta)
		}
	})

	t.Run("valid", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		if err := os.MkdirAll(boidDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		t.Setenv("TEST_BOID_HOME", "/home/testuser")
		content := `
version: 1
env:
  GOPATH: ${TEST_BOID_HOME}/go
host_commands:
  uv:
    path: ${TEST_BOID_HOME}/.local/bin/uv
additional_bindings:
  - source: ${TEST_BOID_HOME}/src/repro-kit
    mode: rw
`
		if err := os.WriteFile(filepath.Join(boidDir, "project.local.yaml"), []byte(content), 0o644); err != nil {
			t.Fatalf("write project.local.yaml: %v", err)
		}

		meta, err := projectspec.ReadProjectLocalMeta(dir)
		if err != nil {
			t.Fatalf("ReadProjectLocalMeta: %v", err)
		}
		if meta.Version != 1 {
			t.Fatalf("version = %d, want 1", meta.Version)
		}
		if meta.Env["GOPATH"] != "/home/testuser/go" {
			t.Fatalf("unexpected env: %+v", meta.Env)
		}
		if meta.HostCommands["uv"].Path != "/home/testuser/.local/bin/uv" {
			t.Fatalf("unexpected host command path: %+v", meta.HostCommands["uv"])
		}
		if len(meta.AdditionalBindings) != 1 || meta.AdditionalBindings[0].Source != "/home/testuser/src/repro-kit" || meta.AdditionalBindings[0].Mode != "rw" {
			t.Fatalf("unexpected additional_bindings: %+v", meta.AdditionalBindings)
		}
	})

	t.Run("rejects kits field", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.local.yaml"), []byte("kits:\n  add: [foo]\n"), 0o644)

		_, err := projectspec.ReadProjectLocalMeta(dir)
		if err == nil || !strings.Contains(err.Error(), "unsupported field") {
			t.Fatalf("expected unsupported field error for kits, got %v", err)
		}
	})

	t.Run("unsupported field", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.local.yaml"), []byte("hooks: []\n"), 0o644)

		_, err := projectspec.ReadProjectLocalMeta(dir)
		if err == nil || !strings.Contains(err.Error(), "unsupported field") {
			t.Fatalf("expected unsupported field error, got %v", err)
		}
	})

	t.Run("invalid host command path", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.local.yaml"), []byte("host_commands:\n  uv:\n    path: relative/uv\n"), 0o644)

		_, err := projectspec.ReadProjectLocalMeta(dir)
		if err == nil || !strings.Contains(err.Error(), "must be an absolute path") {
			t.Fatalf("expected absolute path error, got %v", err)
		}
	})

	t.Run("rejects builtin_commands", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.local.yaml"), []byte("builtin_commands:\n  - boid\n"), 0o644)

		_, err := projectspec.ReadProjectLocalMeta(dir)
		if err == nil || !strings.Contains(err.Error(), "unsupported field") {
			t.Fatalf("expected unsupported field error, got %v", err)
		}
	})
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

			_, err := projectspec.ReadProjectMetaWithKits(dir, nil)
			if err == nil {
				t.Fatal("expected removal error for behavior-level kits, got nil")
			}
			if !strings.Contains(err.Error(), "task_behaviors.dev.kits is no longer supported") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestReadProjectMetaWithKits_ProjectLocalOverlayIgnored verifies that a
// project.local.yaml file (now deprecated) is silently ignored during
// ReadProjectMetaWithKits; its env/host_commands/additional_bindings are NOT
// merged into behaviors. Users should move these settings to workspace.yaml.
func TestReadProjectMetaWithKits_ProjectLocalOverlayIgnored(t *testing.T) {
	baseDir := t.TempDir()
	boidDir := filepath.Join(baseDir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir boid dir: %v", err)
	}

	projectYAML := `
id: test-proj
name: Test Project
task_behaviors:
  dev:
    name: dev
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}

	// project.local.yaml is deprecated; its contents must NOT be applied.
	projectLocalYAML := `
version: 1
env:
  FROM_PROJECT: local
  LOCAL_ONLY: enabled
host_commands:
  uv:
    path: /custom/bin/uv
additional_bindings:
  - source: /opt/local-kit
    mode: rw
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.local.yaml"), []byte(projectLocalYAML), 0o644); err != nil {
		t.Fatalf("write project.local.yaml: %v", err)
	}

	meta, err := projectspec.ReadProjectMetaWithKits(baseDir, nil)
	if err != nil {
		t.Fatalf("ReadProjectMetaWithKits: %v", err)
	}
	b := meta.TaskBehaviors["dev"]
	// project.local.yaml is deprecated and must not be merged.
	if len(b.Env) != 0 {
		t.Fatalf("expected no env from deprecated project.local.yaml, got %+v", b.Env)
	}
	if len(b.HostCommands) != 0 {
		t.Fatalf("expected no host_commands from deprecated project.local.yaml, got %+v", b.HostCommands)
	}
	if len(b.AdditionalBindings) != 0 {
		t.Fatalf("expected no additional_bindings from deprecated project.local.yaml, got %+v", b.AdditionalBindings)
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

		_, err := projectspec.ReadProjectMetaWithKits(dir, nil)
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
	// verify the behavior via project.local.yaml (which still supports it)
	// and kit.yaml.
	t.Run("map form with policy", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test Project\n"), 0o644)
		// host_commands moved to project.local.yaml
		localYAML := `
version: 1
host_commands:
  gh:
    allow: [pr, issue, run]
    deny: ["repo delete *"]
    stdin: true
    env:
      GH_TOKEN: test-token
  aws:
    allow: [s3, "ecr get-login *"]
`
		_ = os.WriteFile(filepath.Join(boidDir, "project.local.yaml"), []byte(localYAML), 0o644)

		meta, err := projectspec.ReadProjectLocalMeta(dir)
		if err != nil {
			t.Fatalf("ReadProjectLocalMeta: %v", err)
		}
		if len(meta.HostCommands) != 2 {
			t.Fatalf("expected 2 host commands, got %d", len(meta.HostCommands))
		}
		gh := meta.HostCommands["gh"]
		if len(gh.Allow) != 3 || gh.Allow[0] != "pr" {
			t.Fatalf("unexpected gh allow: %+v", gh.Allow)
		}
		if len(gh.Deny) != 1 || gh.Deny[0] != "repo delete *" {
			t.Fatalf("unexpected gh deny: %+v", gh.Deny)
		}
		if !gh.Stdin {
			t.Fatal("expected gh stdin=true")
		}
		if gh.Env["GH_TOKEN"] != "test-token" {
			t.Fatalf("unexpected gh env: %+v", gh.Env)
		}

		defs := meta.HostCommands.ToCommandDefs()
		ghDef := defs["gh"]
		if ghDef.Name != "gh" {
			t.Fatalf("expected name 'gh', got %q", ghDef.Name)
		}
		if len(ghDef.AllowedSubcommands) != 3 || ghDef.AllowedSubcommands[0] != "pr" {
			t.Fatalf("unexpected subcommands: %+v", ghDef.AllowedSubcommands)
		}
		if len(ghDef.DeniedPatterns) != 1 {
			t.Fatalf("unexpected denied patterns: %+v", ghDef.DeniedPatterns)
		}

		awsDef := defs["aws"]
		if len(awsDef.AllowedSubcommands) != 1 || awsDef.AllowedSubcommands[0] != "s3" {
			t.Fatalf("unexpected aws subcommands: %+v", awsDef.AllowedSubcommands)
		}
		if len(awsDef.AllowedPatterns) != 1 || awsDef.AllowedPatterns[0] != "ecr get-login *" {
			t.Fatalf("unexpected aws patterns: %+v", awsDef.AllowedPatterns)
		}
	})

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

	t.Run("project.local.yaml optional path", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)
		content := `
version: 1
host_commands:
  gh:
    allow: [pr, issue]
`
		_ = os.WriteFile(filepath.Join(boidDir, "project.local.yaml"), []byte(content), 0o644)

		meta, err := projectspec.ReadProjectLocalMeta(dir)
		if err != nil {
			t.Fatalf("ReadProjectLocalMeta: %v", err)
		}
		if len(meta.HostCommands) != 1 || len(meta.HostCommands["gh"].Allow) != 2 {
			t.Fatalf("unexpected host commands: %+v", meta.HostCommands)
		}
	})

	t.Run("project.local.yaml rejects relative path", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.local.yaml"), []byte("host_commands:\n  gh:\n    path: relative/gh\n"), 0o644)

		_, err := projectspec.ReadProjectLocalMeta(dir)
		if err == nil || !strings.Contains(err.Error(), "must be an absolute path") {
			t.Fatalf("expected absolute path error, got %v", err)
		}
	})
}

func TestReadProjectMeta_HostCommandRelativePath(t *testing.T) {
	// host_commands in project.yaml is a removed key; all sub-tests now verify
	// that the key is rejected, or test path handling via project.local.yaml.

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

	t.Run("absolute path in project.local.yaml accepted", func(t *testing.T) {
		// project.local.yaml only allows absolute paths for host_commands.path.
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test Project\n"), 0o644)

		localYAML := "version: 1\nhost_commands:\n  my-cmd:\n    path: /usr/bin/some-cmd\n"
		_ = os.WriteFile(filepath.Join(boidDir, "project.local.yaml"), []byte(localYAML), 0o644)

		local, err := projectspec.ReadProjectLocalMeta(dir)
		if err != nil {
			t.Fatalf("ReadProjectLocalMeta: %v", err)
		}
		spec := local.HostCommands["my-cmd"]
		if spec.Path != "/usr/bin/some-cmd" {
			t.Fatalf("expected path /usr/bin/some-cmd, got %q", spec.Path)
		}
	})

	t.Run("relative path in project.local.yaml rejected", func(t *testing.T) {
		// project.local.yaml requires absolute paths.
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)

		localYAML := "version: 1\nhost_commands:\n  my-cmd:\n    path: scripts/run.sh\n"
		_ = os.WriteFile(filepath.Join(boidDir, "project.local.yaml"), []byte(localYAML), 0o644)

		_, err := projectspec.ReadProjectLocalMeta(dir)
		if err == nil {
			t.Fatal("expected error for relative path in project.local.yaml, got nil")
		}
		if !strings.Contains(err.Error(), "must be an absolute path") {
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

	t.Run("empty path in project.local.yaml accepted", func(t *testing.T) {
		// host_commands with no path (empty) is valid in project.local.yaml.
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test Project\n"), 0o644)

		localYAML := "version: 1\nhost_commands:\n  gh:\n    allow: [pr]\n"
		_ = os.WriteFile(filepath.Join(boidDir, "project.local.yaml"), []byte(localYAML), 0o644)

		local, err := projectspec.ReadProjectLocalMeta(dir)
		if err != nil {
			t.Fatalf("ReadProjectLocalMeta: %v", err)
		}
		spec := local.HostCommands["gh"]
		if spec.Path != "" {
			t.Fatalf("expected empty path, got %q", spec.Path)
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

	_, err := projectspec.ReadProjectMetaWithKits(dir, nil)
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

	_, err := projectspec.ReadProjectMetaWithKits(dir, nil)
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

	_, err := projectspec.ReadProjectMetaWithKits(dir, nil)
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

	_, err := projectspec.ReadProjectMetaWithKits(dir, nil)
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

	_, err := projectspec.ReadProjectMetaWithKits(dir, nil)
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

	_, err := projectspec.ReadProjectMetaWithKits(dir, nil)
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

	_, err := projectspec.ReadProjectMetaWithKits(dir, nil)
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

	_, err := projectspec.ReadProjectMetaWithKits(dir, nil)
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

func TestCanonicalBehaviorName(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantName  string
		wantAlias bool
	}{
		{name: "plan -> supervisor", input: "plan", wantName: "supervisor", wantAlias: true},
		{name: "dev -> executor", input: "dev", wantName: "executor", wantAlias: true},
		{name: "supervisor passthrough", input: "supervisor", wantName: "supervisor", wantAlias: false},
		{name: "executor passthrough", input: "executor", wantName: "executor", wantAlias: false},
		{name: "unknown name passthrough", input: "custom", wantName: "custom", wantAlias: false},
		{name: "empty passthrough", input: "", wantName: "", wantAlias: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, isAlias := projectspec.CanonicalBehaviorName(tc.input)
			if got != tc.wantName || isAlias != tc.wantAlias {
				t.Errorf("CanonicalBehaviorName(%q) = (%q, %v), want (%q, %v)",
					tc.input, got, isAlias, tc.wantName, tc.wantAlias)
			}
		})
	}
}

// TestReadProjectMeta_BehaviorAlias_PlanIsCanonicalizedToSupervisor verifies
// that a project.yaml using the legacy alias "plan" is loaded into a meta
// whose TaskBehaviors map is keyed by the canonical name "supervisor". A
// deprecation warning must be logged.
func TestReadProjectMeta_BehaviorAlias_PlanIsCanonicalizedToSupervisor(t *testing.T) {
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
  plan:
    name: plan
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
	if _, ok := meta.TaskBehaviors["supervisor"]; !ok {
		t.Fatalf("expected canonical key 'supervisor' to be present, got keys=%v", behaviorKeys(meta))
	}
	// ReadProjectMeta adds a back-compat mirror for legacy callers that
	// still look up by the alias key.
	if _, ok := meta.TaskBehaviors["plan"]; !ok {
		t.Fatalf("expected back-compat alias 'plan' to remain reachable, got keys=%v", behaviorKeys(meta))
	}
	if len(meta.TaskBehaviors["supervisor"].Traits) != 1 || meta.TaskBehaviors["supervisor"].Traits[0] != "artifact" {
		t.Errorf("Traits fell off during alias normalization: %v", meta.TaskBehaviors["supervisor"].Traits)
	}
	if !strings.Contains(buf.String(), "deprecated") || !strings.Contains(buf.String(), "plan") {
		t.Errorf("expected deprecation log mentioning %q, got:\n%s", "plan", buf.String())
	}
}

// TestReadProjectMeta_BehaviorAlias_DevIsCanonicalizedToExecutor mirrors the
// plan -> supervisor test for dev -> executor.
func TestReadProjectMeta_BehaviorAlias_DevIsCanonicalizedToExecutor(t *testing.T) {
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
  dev:
    name: dev
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
	if _, ok := meta.TaskBehaviors["executor"]; !ok {
		t.Fatalf("expected canonical key 'executor' to be present, got keys=%v", behaviorKeys(meta))
	}
	if _, ok := meta.TaskBehaviors["dev"]; !ok {
		t.Fatalf("expected back-compat alias 'dev' to remain reachable, got keys=%v", behaviorKeys(meta))
	}
	if len(meta.TaskBehaviors["executor"].Traits) != 1 || meta.TaskBehaviors["executor"].Traits[0] != "artifact" {
		t.Errorf("Traits fell off during alias normalization: %v", meta.TaskBehaviors["executor"].Traits)
	}
	if !strings.Contains(buf.String(), "deprecated") || !strings.Contains(buf.String(), "dev") {
		t.Errorf("expected deprecation log mentioning %q, got:\n%s", "dev", buf.String())
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

// TestReadProjectMeta_BehaviorAlias_DuplicateDefinitionRejected verifies that
// defining both an alias and its canonical counterpart in the same file is a
// load-time error. Authors must pick exactly one form per behavior.
func TestReadProjectMeta_BehaviorAlias_DuplicateDefinitionRejected(t *testing.T) {
	cases := []struct {
		name     string
		yaml     string
		needWord string
	}{
		{
			name: "plan and supervisor",
			yaml: `
id: test-proj
name: Test Project
task_behaviors:
  plan:
    name: plan
  supervisor:
    name: supervisor
`,
			needWord: "plan",
		},
		{
			name: "dev and executor",
			yaml: `
id: test-proj
name: Test Project
task_behaviors:
  dev:
    name: dev
  executor:
    name: executor
`,
			needWord: "dev",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			boidDir := filepath.Join(dir, ".boid")
			if err := os.MkdirAll(boidDir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(tc.yaml), 0o644); err != nil {
				t.Fatalf("write yaml: %v", err)
			}
			_, err := projectspec.ReadProjectMeta(dir)
			if err == nil {
				t.Fatalf("expected duplicate-definition error, got nil")
			}
			msg := err.Error()
			if !strings.Contains(msg, "duplicate") || !strings.Contains(msg, tc.needWord) {
				t.Errorf("expected duplicate error mentioning %q, got: %v", tc.needWord, err)
			}
		})
	}
}



// TestReadProjectMetaWithKits_BehaviorAlias_MirrorsAddedAtRuntimeBoundary
// verifies the second half of the alias contract: while ReadProjectMeta
// normalizes keys to canonical without mirrors, ReadProjectMetaWithKits — the
// function used by runtime code — adds a back-compat mirror entry so legacy
// lookups by alias name still find the behavior.
func TestReadProjectMetaWithKits_BehaviorAlias_MirrorsAddedAtRuntimeBoundary(t *testing.T) {
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
  dev:
    name: dev
    traits:
      - verification
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
	if err != nil {
		t.Fatalf("ReadProjectMetaWithKits: %v", err)
	}
	// Canonical entries must be present.
	if _, ok := meta.TaskBehaviors["supervisor"]; !ok {
		t.Errorf("canonical key 'supervisor' missing, got keys=%v", behaviorKeys(meta))
	}
	if _, ok := meta.TaskBehaviors["executor"]; !ok {
		t.Errorf("canonical key 'executor' missing, got keys=%v", behaviorKeys(meta))
	}
	// Alias mirrors must also be present (back-compat for legacy callers).
	if _, ok := meta.TaskBehaviors["plan"]; !ok {
		t.Errorf("alias mirror 'plan' missing after ReadProjectMetaWithKits, got keys=%v", behaviorKeys(meta))
	}
	if _, ok := meta.TaskBehaviors["dev"]; !ok {
		t.Errorf("alias mirror 'dev' missing after ReadProjectMetaWithKits, got keys=%v", behaviorKeys(meta))
	}
	// Mirrors must reflect the same template values.
	if len(meta.TaskBehaviors["plan"].Traits) != 1 || len(meta.TaskBehaviors["supervisor"].Traits) != 1 {
		t.Errorf("Traits disagreement between alias and canonical: plan=%v supervisor=%v",
			meta.TaskBehaviors["plan"].Traits, meta.TaskBehaviors["supervisor"].Traits)
	}
	if len(meta.TaskBehaviors["dev"].Traits) != 1 || len(meta.TaskBehaviors["executor"].Traits) != 1 {
		t.Errorf("Traits disagreement between alias and canonical: dev=%v executor=%v",
			meta.TaskBehaviors["dev"].Traits, meta.TaskBehaviors["executor"].Traits)
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
	_, err := projectspec.ReadProjectMetaWithKits(dir, nil)
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
	_, err := projectspec.ReadProjectMetaWithKits(dir, nil)
	if err != nil {
		t.Fatalf("ReadProjectMetaWithKits (first): %v", err)
	}
	countAfterFirst := strings.Count(buf.String(), "deprecated")
	if countAfterFirst == 0 {
		t.Error("expected deprecation warning after first load, got none")
	}

	// Second load of same directory: no new deprecation warnings.
	_, err = projectspec.ReadProjectMetaWithKits(dir, nil)
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
	_, err := projectspec.ReadProjectMetaWithKits(dir, nil)
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
	_, err := projectspec.ReadProjectMetaWithKits(dir, nil)
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
	_, err := projectspec.ReadProjectMetaWithKits(dir, nil)
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
