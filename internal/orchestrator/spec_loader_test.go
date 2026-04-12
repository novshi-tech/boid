package orchestrator_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

func mustMergeKitMeta(t *testing.T, base *projectspec.ProjectMeta, kits []*projectspec.KitMeta, consumers []string) *projectspec.ProjectMeta {
	t.Helper()
	result, err := projectspec.MergeKitMeta(base, kits, consumers)
	if err != nil {
		t.Fatalf("MergeKitMeta: unexpected error: %v", err)
	}
	return result
}

func writeKitYAML(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "kit.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadProjectMeta_Valid(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	hooksDir := filepath.Join(boidDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
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
hooks:
  - id: run-agent
    on: executing
    requires_traits:
      - artifactompt
builtin_commands:
  - boid
host_commands:
  git:
    path: /usr/bin/git
env:
  KEY: val
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "run-agent.sh"), []byte("#!/bin/sh\necho hi"), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	meta, err := projectspec.ReadProjectMeta(dir)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}

	if meta.ID != "test-proj" || meta.Name != "Test Project" {
		t.Fatalf("unexpected meta: %+v", meta)
	}
	if len(meta.TaskBehaviors) != 1 || len(meta.Hooks) != 1 {
		t.Fatalf("unexpected counts: %+v", meta)
	}
	if meta.Hooks[0].ScriptPath != filepath.Join(hooksDir, "run-agent.sh") {
		t.Fatalf("expected script path %s, got %s", filepath.Join(hooksDir, "run-agent.sh"), meta.Hooks[0].ScriptPath)
	}
	if _, ok := meta.HostCommands["git"]; !ok {
		t.Fatal("expected host_commands to contain 'git'")
	}
	if len(meta.BuiltinCommands) != 1 || meta.BuiltinCommands[0] != "boid" {
		t.Fatalf("unexpected builtin commands: %+v", meta.BuiltinCommands)
	}
	if meta.Env["KEY"] != "val" {
		t.Fatalf("expected env KEY=val, got %s", meta.Env["KEY"])
	}
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

func TestReadProjectMeta_ScriptResolutionAndValidation(t *testing.T) {
	t.Run("python hook", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		hooksDir := filepath.Join(boidDir, "hooks")
		_ = os.MkdirAll(hooksDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test\nhooks:\n  - id: my-hook\n    on: executing\n"), 0o644)
		_ = os.WriteFile(filepath.Join(hooksDir, "my-hook.py"), []byte("print('hi')"), 0o755)

		meta, err := projectspec.ReadProjectMeta(dir)
		if err != nil {
			t.Fatalf("read meta: %v", err)
		}
		if meta.Hooks[0].ScriptPath != filepath.Join(hooksDir, "my-hook.py") {
			t.Fatalf("expected .py script path, got %s", meta.Hooks[0].ScriptPath)
		}
	})

	t.Run("missing hook script", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		hooksDir := filepath.Join(boidDir, "hooks")
		_ = os.MkdirAll(hooksDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test\nhooks:\n  - id: missing-hook\n    on: executing\n"), 0o644)

		_, err := projectspec.ReadProjectMeta(dir)
		if err == nil || !strings.Contains(err.Error(), "script not found") {
			t.Fatalf("expected script not found, got %v", err)
		}
	})

	t.Run("gates", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		gatesDir := filepath.Join(boidDir, "gates")
		_ = os.MkdirAll(gatesDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test\ngates:\n  - id: push-pr\n    on: executing\n    requires_traits:\n      - artifact\n"), 0o644)
		_ = os.WriteFile(filepath.Join(gatesDir, "push-pr.sh"), []byte("#!/bin/bash\n"), 0o755)

		meta, err := projectspec.ReadProjectMeta(dir)
		if err != nil {
			t.Fatalf("read meta: %v", err)
		}
		if len(meta.Gates) != 1 || meta.Gates[0].ScriptPath != filepath.Join(gatesDir, "push-pr.sh") {
			t.Fatalf("unexpected gates: %+v", meta.Gates)
		}
	})

	t.Run("missing gate script", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		gatesDir := filepath.Join(boidDir, "gates")
		_ = os.MkdirAll(gatesDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test\ngates:\n  - id: missing-gate\n    on: executing\n"), 0o644)

		_, err := projectspec.ReadProjectMeta(dir)
		if err == nil || !strings.Contains(err.Error(), "gate script not found") {
			t.Fatalf("expected gate script not found, got %v", err)
		}
	})

	t.Run("invalid gate on", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		gatesDir := filepath.Join(boidDir, "gates")
		_ = os.MkdirAll(gatesDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test\ngates:\n  - id: bad-gate\n    on: invalid_status\n"), 0o644)

		_, err := projectspec.ReadProjectMeta(dir)
		if err == nil || !strings.Contains(err.Error(), "invalid on value") {
			t.Fatalf("expected invalid on value, got %v", err)
		}
	})

	t.Run("invalid hook on", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		hooksDir := filepath.Join(boidDir, "hooks")
		_ = os.MkdirAll(hooksDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test\nhooks:\n  - id: bad-hook\n    on: invalid_status\n"), 0o644)

		_, err := projectspec.ReadProjectMeta(dir)
		if err == nil || !strings.Contains(err.Error(), "invalid on value") {
			t.Fatalf("expected invalid on value, got %v", err)
		}
	})
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
kits:
  add: [local/dev/repro-kit]
  remove: [github.com/acme/repo/default]
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
		if len(meta.Kits.Add) != 1 || meta.Kits.Add[0] != "local/dev/repro-kit" {
			t.Fatalf("unexpected kits.add: %+v", meta.Kits.Add)
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

	t.Run("builtin commands", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		if err := os.MkdirAll(boidDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(boidDir, "project.local.yaml"), []byte("builtin_commands:\n  - boid\n  - git\n"), 0o644); err != nil {
			t.Fatalf("write project.local.yaml: %v", err)
		}

		meta, err := projectspec.ReadProjectLocalMeta(dir)
		if err != nil {
			t.Fatalf("ReadProjectLocalMeta: %v", err)
		}
		if len(meta.BuiltinCommands) != 2 || meta.BuiltinCommands[0] != "boid" || meta.BuiltinCommands[1] != "git" {
			t.Fatalf("unexpected builtin_commands: %+v", meta.BuiltinCommands)
		}
	})

	t.Run("builtin commands conflict with host command", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		if err := os.MkdirAll(boidDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		content := "builtin_commands:\n  - boid\nhost_commands:\n  boid:\n    path: /usr/bin/boid\n"
		if err := os.WriteFile(filepath.Join(boidDir, "project.local.yaml"), []byte(content), 0o644); err != nil {
			t.Fatalf("write project.local.yaml: %v", err)
		}

		_, err := projectspec.ReadProjectLocalMeta(dir)
		if err == nil || !strings.Contains(err.Error(), "both builtin_commands and host_commands") {
			t.Fatalf("expected builtin/host conflict, got %v", err)
		}
	})
}

func TestReadProjectMetaWithKits_LocalKits(t *testing.T) {
	t.Run("single local kit", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		kitsDir := filepath.Join(boidDir, "kits", "go-dev")
		_ = os.MkdirAll(kitsDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test Project\nkits:\n  - go-dev\n"), 0o644)
		_ = os.WriteFile(filepath.Join(kitsDir, "kit.yaml"), []byte("additional_bindings:\n  - source: /usr/local/go\nenv:\n  GOPATH: /home/user/go\n"), 0o644)

		meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
		if err != nil {
			t.Fatalf("ReadProjectMetaWithKits: %v", err)
		}
		if meta.Env["GOPATH"] != "/home/user/go" || len(meta.AdditionalBindings) == 0 || meta.AdditionalBindings[0].Source != "/usr/local/go" {
			t.Fatalf("unexpected merged meta: %+v", meta)
		}
	})

	t.Run("local kit with hooks", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		kitDir := filepath.Join(boidDir, "kits", "build")
		kitHooksDir := filepath.Join(kitDir, "hooks")
		_ = os.MkdirAll(kitHooksDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test Project\nkits:\n  - build\n"), 0o644)
		_ = os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte("hooks:\n  - id: run-build\n    on: executing\n    requires_traits:\n      - artifactompt\n"), 0o644)
		_ = os.WriteFile(filepath.Join(kitHooksDir, "run-build.sh"), []byte("#!/bin/bash\necho build"), 0o755)

		meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
		if err != nil {
			t.Fatalf("ReadProjectMetaWithKits: %v", err)
		}
		if len(meta.Hooks) != 1 || meta.Hooks[0].ID != "build/run-build" || len(meta.KitHooksDirs) != 1 {
			t.Fatalf("unexpected merged hooks: %+v", meta)
		}
	})

	t.Run("env interpolation", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		kitsDir := filepath.Join(boidDir, "kits", "go-dev")
		_ = os.MkdirAll(kitsDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test Project\nkits:\n  - go-dev\n"), 0o644)
		_ = os.WriteFile(filepath.Join(kitsDir, "kit.yaml"), []byte("additional_bindings:\n  - source: ${TEST_BOID_HOME}/.local/share/go\nenv:\n  GOPATH: ${TEST_BOID_HOME}/go\n"), 0o644)
		t.Setenv("TEST_BOID_HOME", "/home/testuser")

		meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
		if err != nil {
			t.Fatalf("ReadProjectMetaWithKits: %v", err)
		}
		if meta.Env["GOPATH"] != "/home/testuser/go" || meta.AdditionalBindings[0].Source != "/home/testuser/.local/share/go" {
			t.Fatalf("unexpected interpolated meta: %+v", meta)
		}
	})

	t.Run("codex kit bindings", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		kitsDir := filepath.Join(boidDir, "kits", "codex")
		_ = os.MkdirAll(kitsDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test Project\nkits:\n  - codex\n"), 0o644)
		_ = os.WriteFile(filepath.Join(kitsDir, "kit.yaml"), []byte("additional_bindings:\n  - source: ${TEST_BOID_HOME}/.volta\n  - source: ${TEST_BOID_HOME}/.codex\n    mode: rw\n"), 0o644)
		t.Setenv("TEST_BOID_HOME", "/home/testuser")

		meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
		if err != nil {
			t.Fatalf("ReadProjectMetaWithKits: %v", err)
		}
		if len(meta.AdditionalBindings) != 2 || meta.AdditionalBindings[0].Source != "/home/testuser/.volta" || meta.AdditionalBindings[1].Source != "/home/testuser/.codex" || meta.AdditionalBindings[1].Mode != "rw" {
			t.Fatalf("unexpected bindings: %+v", meta.AdditionalBindings)
		}
	})

	t.Run("missing local kit", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test Project\nkits:\n  - nonexistent-kit\n"), 0o644)

		_, err := projectspec.ReadProjectMetaWithKits(dir, nil)
		if err == nil || !strings.Contains(err.Error(), "kit.yaml not found") {
			t.Fatalf("expected kit.yaml not found, got %v", err)
		}
	})

	t.Run("multiple local kits", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		for _, name := range []string{"go-dev", "git"} {
			_ = os.MkdirAll(filepath.Join(boidDir, "kits", name), 0o755)
		}
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test Project\nkits:\n  - go-dev\n  - git\n"), 0o644)
		_ = os.WriteFile(filepath.Join(boidDir, "kits", "go-dev", "kit.yaml"), []byte("env:\n  GOPATH: /home/user/go\nadditional_bindings:\n  - source: /usr/local/go\n"), 0o644)
		_ = os.WriteFile(filepath.Join(boidDir, "kits", "git", "kit.yaml"), []byte("host_commands:\n  git:\n    path: /usr/bin/git\n    extract_subcommand_fn: git\n    allowed_subcommands:\n      - status\n"), 0o644)

		meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
		if err != nil {
			t.Fatalf("ReadProjectMetaWithKits: %v", err)
		}
		if meta.Env["GOPATH"] != "/home/user/go" || len(meta.AdditionalBindings) == 0 {
			t.Fatalf("expected merged env and bindings, got %+v", meta)
		}
		if _, ok := meta.HostCommands["git"]; !ok {
			t.Fatal("expected host_commands to contain 'git' from git kit")
		}
	})
}

func TestReadProjectMetaWithKits_ProjectLocalOverlay(t *testing.T) {
	baseDir := t.TempDir()
	registryDir := t.TempDir()
	boidDir := filepath.Join(baseDir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir boid dir: %v", err)
	}

	projectYAML := `
id: test-proj
name: Test Project
kits:
  - github.com/acme/repo/default
env:
  FROM_PROJECT: base
host_commands:
  git:
    path: /usr/bin/git
additional_bindings:
  - source: /opt/base
    mode: ro
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}

	defaultKitDir := filepath.Join(registryDir, "github.com", "acme", "repo", "default")
	if err := os.MkdirAll(defaultKitDir, 0o755); err != nil {
		t.Fatalf("mkdir default kit dir: %v", err)
	}
	writeKitYAML(t, defaultKitDir, `
env:
  FROM_DEFAULT: kit
host_commands:
  git:
    path: /usr/bin/git
additional_bindings:
  - source: /opt/default
    mode: ro
`)

	localKitDir := filepath.Join(registryDir, "local", "dev", "repro-kit")
	if err := os.MkdirAll(localKitDir, 0o755); err != nil {
		t.Fatalf("mkdir local kit dir: %v", err)
	}
	writeKitYAML(t, localKitDir, `
env:
  FROM_LOCAL_KIT: yes
  FROM_PROJECT: local-kit
host_commands:
  uv:
    path: /usr/bin/uv
additional_bindings:
  - source: /opt/local-kit
    mode: ro
`)

	projectLocalYAML := `
kits:
  add:
    - local/dev/repro-kit
  remove:
    - github.com/acme/repo/default
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

	meta, err := projectspec.ReadProjectMetaWithKits(baseDir, projectspec.NewRegistry(registryDir))
	if err != nil {
		t.Fatalf("ReadProjectMetaWithKits: %v", err)
	}
	if len(meta.Kits) != 1 || meta.Kits[0].Ref != "local/dev/repro-kit" {
		t.Fatalf("unexpected effective kits: %+v", meta.Kits)
	}
	if _, ok := meta.Env["FROM_DEFAULT"]; ok {
		t.Fatalf("default kit should have been removed, env=%+v", meta.Env)
	}
	if meta.Env["FROM_LOCAL_KIT"] != "yes" || meta.Env["FROM_PROJECT"] != "local" || meta.Env["LOCAL_ONLY"] != "enabled" {
		t.Fatalf("unexpected env merge: %+v", meta.Env)
	}
	if meta.HostCommands["uv"].Path != "/custom/bin/uv" {
		t.Fatalf("unexpected host command override: %+v", meta.HostCommands["uv"])
	}
	if meta.HostCommands["git"].Path != "/usr/bin/git" {
		t.Fatalf("project host command should be preserved: %+v", meta.HostCommands)
	}
	if len(meta.AdditionalBindings) != 2 {
		t.Fatalf("unexpected bindings: %+v", meta.AdditionalBindings)
	}
	if meta.AdditionalBindings[0].Source != "/opt/local-kit" || meta.AdditionalBindings[0].Mode != "rw" {
		t.Fatalf("expected local binding override, got %+v", meta.AdditionalBindings)
	}
}

func TestReadKitMeta(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		dir := t.TempDir()
		hooksDir := filepath.Join(dir, "hooks")
		_ = os.MkdirAll(hooksDir, 0o755)
		_ = os.WriteFile(filepath.Join(hooksDir, "run-build.sh"), []byte("#!/bin/bash\necho ok"), 0o755)
		writeKitYAML(t, dir, `
hooks:
  - id: run-build
    on: executing
    requires_traits: [prompt]
host_commands:
  go:
    path: /usr/bin/go
additional_bindings:
  - source: /usr/local/go
env:
  GOPATH: /home/user/go
task_behaviors:
  dev:
    name: development
    traits: [prompt]
`)

		meta, err := projectspec.ReadKitMeta(dir)
		if err != nil {
			t.Fatalf("ReadKitMeta: %v", err)
		}
		if len(meta.Hooks) != 1 || meta.Hooks[0].ID != "run-build" || meta.Hooks[0].ScriptPath == "" {
			t.Fatalf("unexpected hooks: %+v", meta.Hooks)
		}
		if _, ok := meta.HostCommands["go"]; !ok || meta.Env["GOPATH"] != "/home/user/go" || meta.HooksDir != hooksDir {
			t.Fatalf("unexpected meta: %+v", meta)
		}
	})

	t.Run("env interpolation", func(t *testing.T) {
		dir := t.TempDir()
		writeKitYAML(t, dir, "additional_bindings:\n  - source: ${TEST_BOID_HOME}/.local/share/go\nenv:\n  GOPATH: ${TEST_BOID_HOME}/go\n")
		t.Setenv("TEST_BOID_HOME", "/home/testuser")

		meta, err := projectspec.ReadKitMeta(dir)
		if err != nil {
			t.Fatalf("ReadKitMeta: %v", err)
		}
		if meta.AdditionalBindings[0].Source != "/home/testuser/.local/share/go" || meta.Env["GOPATH"] != "/home/testuser/go" {
			t.Fatalf("unexpected interpolation: %+v", meta)
		}
	})

	t.Run("builtin command conflicts with host command", func(t *testing.T) {
		dir := t.TempDir()
		writeKitYAML(t, dir, "builtin_commands:\n  - git\nhost_commands:\n  git:\n    path: /usr/bin/git\n")

		_, err := projectspec.ReadKitMeta(dir)
		if err == nil || !strings.Contains(err.Error(), "both builtin_commands and host_commands") {
			t.Fatalf("expected builtin/host conflict, got %v", err)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := projectspec.ReadKitMeta(t.TempDir())
		if err == nil {
			t.Fatal("expected error for missing kit.yaml")
		}
	})

	t.Run("invalid hook on", func(t *testing.T) {
		dir := t.TempDir()
		writeKitYAML(t, dir, "hooks:\n  - id: bad-hook\n    on: invalid_status\n")
		_, err := projectspec.ReadKitMeta(dir)
		if err == nil {
			t.Fatal("expected error for invalid hook on value")
		}
	})

	t.Run("missing hook script", func(t *testing.T) {
		dir := t.TempDir()
		writeKitYAML(t, dir, "hooks:\n  - id: no-script\n    on: executing\n")
		_, err := projectspec.ReadKitMeta(dir)
		if err == nil {
			t.Fatal("expected error for missing hook script")
		}
	})
}

func TestReadProjectMetaWithKits_BuiltinCommands(t *testing.T) {
	t.Run("merges builtin commands from kits and local overlay", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		gitKitDir := filepath.Join(boidDir, "kits", "git")
		if err := os.MkdirAll(gitKitDir, 0o755); err != nil {
			t.Fatalf("mkdir git kit: %v", err)
		}
		if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test Project\nkits:\n  - git\n"), 0o644); err != nil {
			t.Fatalf("write project.yaml: %v", err)
		}
		writeKitYAML(t, gitKitDir, "builtin_commands:\n  - git\n")
		if err := os.WriteFile(filepath.Join(boidDir, "project.local.yaml"), []byte("builtin_commands:\n  - git\n"), 0o644); err != nil {
			t.Fatalf("write project.local.yaml: %v", err)
		}

		meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
		if err != nil {
			t.Fatalf("ReadProjectMetaWithKits: %v", err)
		}
		if len(meta.BuiltinCommands) != 1 || meta.BuiltinCommands[0] != "git" {
			t.Fatalf("unexpected builtin_commands: %+v", meta.BuiltinCommands)
		}
	})

	t.Run("rejects effective builtin and host command conflict", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		gitKitDir := filepath.Join(boidDir, "kits", "git")
		if err := os.MkdirAll(gitKitDir, 0o755); err != nil {
			t.Fatalf("mkdir git kit: %v", err)
		}
		projectYAML := "id: test-proj\nname: Test Project\nhost_commands:\n  git:\n    path: /usr/bin/git\nkits:\n  - git\n"
		if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
			t.Fatalf("write project.yaml: %v", err)
		}
		writeKitYAML(t, gitKitDir, "builtin_commands:\n  - git\n")

		_, err := projectspec.ReadProjectMetaWithKits(dir, nil)
		if err == nil || !strings.Contains(err.Error(), "both builtin_commands and host_commands") {
			t.Fatalf("expected builtin/host conflict, got %v", err)
		}
	})
}

func TestMergeKitMeta(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		base := &projectspec.ProjectMeta{ID: "proj", Name: "Project", Env: map[string]string{"KEY": "val"}}
		result := mustMergeKitMeta(t,base, nil, nil)
		if result.Env["KEY"] != "val" {
			t.Errorf("env KEY = %q, want val", result.Env["KEY"])
		}
	})

	t.Run("single kit", func(t *testing.T) {
		base := &projectspec.ProjectMeta{
			ID:           "proj",
			Name:         "Project",
			HostCommands: projectspec.HostCommands{"git": {Path: "/usr/bin/git"}},
			Hooks:        []projectspec.Hook{{ID: "proj-hook", On: projectspec.OnValues{"executing"}}},
			Env:          map[string]string{"PROJECT_VAR": "pval"},
		}
		meta := &projectspec.KitMeta{
			HostCommands:       projectspec.HostCommands{"go": {Path: "/usr/bin/go"}, "git": {Path: "/usr/bin/git"}},
			AdditionalBindings: []projectspec.BindMount{{Source: "/usr/local/go"}},
			Hooks:              []projectspec.Hook{{ID: "kit-hook", On: projectspec.OnValues{"verifying"}, ScriptPath: "/kit/hooks/kit-hook.sh"}},
			HooksDir:           "/kit/hooks",
			Env:                map[string]string{"GOPATH": "/home/go", "PROJECT_VAR": "kit-overridden"},
			TaskBehaviors:      map[string]projectspec.TaskBehavior{"dev": {Name: "dev"}},
		}

		result := mustMergeKitMeta(t,base, []*projectspec.KitMeta{meta}, []string{"mykit"})
		if len(result.HostCommands) != 2 || len(result.AdditionalBindings) != 1 || len(result.Hooks) != 2 {
			t.Fatalf("unexpected merge result: %+v", result)
		}
		if result.Hooks[0].ID != "mykit/kit-hook" || result.Hooks[1].ID != "proj-hook" {
			t.Fatalf("unexpected hook order: %+v", result.Hooks)
		}
		if result.Env["GOPATH"] != "/home/go" || result.Env["PROJECT_VAR"] != "pval" {
			t.Fatalf("unexpected env: %+v", result.Env)
		}
		if _, ok := result.TaskBehaviors["dev"]; !ok || len(result.KitHooksDirs) != 1 || result.KitHooksDirs[0].HooksDir != "/kit/hooks" {
			t.Fatalf("unexpected metadata: %+v", result)
		}
	})

	t.Run("multiple kits", func(t *testing.T) {
		base := &projectspec.ProjectMeta{ID: "proj", Name: "Project", Env: map[string]string{"PROJ": "yes"}}
		m1 := &projectspec.KitMeta{Env: map[string]string{"A": "from-m1", "SHARED": "m1"}, HostCommands: projectspec.HostCommands{"go": {Path: "/usr/bin/go"}}}
		m2 := &projectspec.KitMeta{Env: map[string]string{"B": "from-m2", "SHARED": "m2"}, HostCommands: projectspec.HostCommands{"gh": {Path: "/usr/bin/gh"}}}

		result := mustMergeKitMeta(t,base, []*projectspec.KitMeta{m1, m2}, []string{"kit-a", "kit-b"})
		if result.Env["A"] != "from-m1" || result.Env["B"] != "from-m2" || result.Env["SHARED"] != "m2" || result.Env["PROJ"] != "yes" || len(result.HostCommands) != 2 {
			t.Fatalf("unexpected merge result: %+v", result)
		}
	})

	t.Run("same raw hook id across kit and base both survive with qualified IDs", func(t *testing.T) {
		base := &projectspec.ProjectMeta{ID: "proj", Name: "Project", Hooks: []projectspec.Hook{{ID: "build", On: projectspec.OnValues{"executing"}, ScriptPath: "/proj/hooks/build.sh"}}}
		meta := &projectspec.KitMeta{Hooks: []projectspec.Hook{{ID: "build", On: projectspec.OnValues{"executing"}, ScriptPath: "/kit/hooks/build.sh"}}, HooksDir: "/kit/hooks"}

		result := mustMergeKitMeta(t,base, []*projectspec.KitMeta{meta}, []string{"mykit"})
		if len(result.Hooks) != 2 {
			t.Fatalf("expected 2 hooks (kit + base), got %d: %+v", len(result.Hooks), result.Hooks)
		}
		if result.Hooks[0].ID != "mykit/build" {
			t.Errorf("hook[0].ID = %q, want %q", result.Hooks[0].ID, "mykit/build")
		}
		if result.Hooks[1].ID != "build" {
			t.Errorf("hook[1].ID = %q, want %q", result.Hooks[1].ID, "build")
		}
	})
}

func TestResolveKitConsumer(t *testing.T) {
	tests := []struct {
		name string
		ref  projectspec.KitRef
		want string
	}{
		{
			name: "simple name",
			ref:  projectspec.KitRef{Ref: "codex"},
			want: "codex",
		},
		{
			name: "local path",
			ref:  projectspec.KitRef{Ref: "local/go-dev"},
			want: "go-dev",
		},
		{
			name: "deep github path",
			ref:  projectspec.KitRef{Ref: "github.com/novshi-tech/boid-kits/claude-code"},
			want: "claude-code",
		},
		{
			name: "alias overrides basename",
			ref:  projectspec.KitRef{Ref: "github.com/novshi-tech/boid-kits/claude-code", Alias: "myalias"},
			want: "myalias",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := projectspec.ResolveKitConsumer(tc.ref)
			if got != tc.want {
				t.Errorf("ResolveKitConsumer(%+v) = %q, want %q", tc.ref, got, tc.want)
			}
		})
	}
}

func TestReadProjectMetaWithKits_DuplicateConsumer(t *testing.T) {
	t.Run("duplicate basename rejected", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(filepath.Join(boidDir, "kits", "go-dev"), 0o755)
		_ = os.MkdirAll(filepath.Join(boidDir, "kits", "other", "go-dev"), 0o755)
		projectYAML := "id: test-proj\nname: Test Project\nkits:\n  - go-dev\n  - other/go-dev\n"
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644)
		_ = os.WriteFile(filepath.Join(boidDir, "kits", "go-dev", "kit.yaml"), []byte("env:\n  A: a\n"), 0o644)
		_ = os.WriteFile(filepath.Join(boidDir, "kits", "other", "go-dev", "kit.yaml"), []byte("env:\n  B: b\n"), 0o644)

		_, err := projectspec.ReadProjectMetaWithKits(dir, nil)
		if err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("expected ambiguous consumer error, got %v", err)
		}
	})

	t.Run("disambiguation with as alias", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(filepath.Join(boidDir, "kits", "go-dev"), 0o755)
		_ = os.MkdirAll(filepath.Join(boidDir, "kits", "other", "go-dev"), 0o755)
		projectYAML := "id: test-proj\nname: Test Project\nkits:\n  - go-dev\n  - ref: other/go-dev\n    as: other-go-dev\n"
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644)
		_ = os.WriteFile(filepath.Join(boidDir, "kits", "go-dev", "kit.yaml"), []byte("env:\n  A: a\n"), 0o644)
		_ = os.WriteFile(filepath.Join(boidDir, "kits", "other", "go-dev", "kit.yaml"), []byte("env:\n  B: b\n"), 0o644)

		meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
		if err != nil {
			t.Fatalf("ReadProjectMetaWithKits: %v", err)
		}
		if meta.Env["A"] != "a" || meta.Env["B"] != "b" {
			t.Fatalf("unexpected env: %+v", meta.Env)
		}
	})
}

func TestMergeKitMeta_KitConsumerFields(t *testing.T) {
	t.Run("kit hook without explicit consumer inherits kit consumer name", func(t *testing.T) {
		base := &projectspec.ProjectMeta{ID: "proj", Name: "Project"}
		meta := &projectspec.KitMeta{
			Hooks: []projectspec.Hook{{ID: "kit-hook", On: projectspec.OnValues{"executing"}, ScriptPath: "/kit/hooks/kit-hook.sh"}},
		}

		result := mustMergeKitMeta(t,base, []*projectspec.KitMeta{meta}, []string{"claude-code"})
		if len(result.Hooks) != 1 {
			t.Fatalf("expected 1 hook, got %d", len(result.Hooks))
		}
		h := result.Hooks[0]
		if h.Kit != "claude-code" {
			t.Errorf("Kit = %q, want %q", h.Kit, "claude-code")
		}
		if h.Consumer != "claude-code" {
			t.Errorf("Consumer = %q, want %q", h.Consumer, "claude-code")
		}
	})

	t.Run("kit hook with explicit consumer retains its consumer", func(t *testing.T) {
		base := &projectspec.ProjectMeta{ID: "proj", Name: "Project"}
		meta := &projectspec.KitMeta{
			Hooks: []projectspec.Hook{{ID: "kit-hook", On: projectspec.OnValues{"executing"}, ScriptPath: "/kit/hooks/kit-hook.sh", Consumer: "explicit-consumer"}},
		}

		result := mustMergeKitMeta(t,base, []*projectspec.KitMeta{meta}, []string{"claude-code"})
		if len(result.Hooks) != 1 {
			t.Fatalf("expected 1 hook, got %d", len(result.Hooks))
		}
		h := result.Hooks[0]
		if h.Kit != "claude-code" {
			t.Errorf("Kit = %q, want %q", h.Kit, "claude-code")
		}
		if h.Consumer != "explicit-consumer" {
			t.Errorf("Consumer = %q, want %q", h.Consumer, "explicit-consumer")
		}
	})

	t.Run("kit gate Kit field is set to kit consumer name", func(t *testing.T) {
		base := &projectspec.ProjectMeta{ID: "proj", Name: "Project"}
		meta := &projectspec.KitMeta{
			Gates: []projectspec.Gate{{ID: "kit-gate", On: projectspec.OnValues{"executing"}, ScriptPath: "/kit/gates/kit-gate.sh"}},
		}

		result := mustMergeKitMeta(t,base, []*projectspec.KitMeta{meta}, []string{"claude-code"})
		if len(result.Gates) != 1 {
			t.Fatalf("expected 1 gate, got %d", len(result.Gates))
		}
		g := result.Gates[0]
		if g.Kit != "claude-code" {
			t.Errorf("Gate.Kit = %q, want %q", g.Kit, "claude-code")
		}
	})

	t.Run("kit hook ID is qualified with consumer prefix", func(t *testing.T) {
		base := &projectspec.ProjectMeta{ID: "proj", Name: "Project"}
		meta := &projectspec.KitMeta{
			Hooks: []projectspec.Hook{{ID: "run-agent", On: projectspec.OnValues{"executing"}, ScriptPath: "/kit/hooks/run-agent.sh"}},
		}

		result := mustMergeKitMeta(t,base, []*projectspec.KitMeta{meta}, []string{"claude-code"})
		if len(result.Hooks) != 1 {
			t.Fatalf("expected 1 hook, got %d", len(result.Hooks))
		}
		if result.Hooks[0].ID != "claude-code/run-agent" {
			t.Errorf("ID = %q, want %q", result.Hooks[0].ID, "claude-code/run-agent")
		}
	})

	t.Run("kit gate ID is qualified with consumer prefix", func(t *testing.T) {
		base := &projectspec.ProjectMeta{ID: "proj", Name: "Project"}
		meta := &projectspec.KitMeta{
			Gates: []projectspec.Gate{{ID: "check-quality", On: projectspec.OnValues{"verifying"}, ScriptPath: "/kit/gates/check-quality.sh"}},
		}

		result := mustMergeKitMeta(t,base, []*projectspec.KitMeta{meta}, []string{"my-kit"})
		if len(result.Gates) != 1 {
			t.Fatalf("expected 1 gate, got %d", len(result.Gates))
		}
		if result.Gates[0].ID != "my-kit/check-quality" {
			t.Errorf("ID = %q, want %q", result.Gates[0].ID, "my-kit/check-quality")
		}
	})

	t.Run("different kits with same hook ID both survive", func(t *testing.T) {
		base := &projectspec.ProjectMeta{ID: "proj", Name: "Project"}
		kitA := &projectspec.KitMeta{
			Hooks: []projectspec.Hook{{ID: "run-agent", On: projectspec.OnValues{"executing"}, ScriptPath: "/a/hooks/run-agent.sh"}},
		}
		kitB := &projectspec.KitMeta{
			Hooks: []projectspec.Hook{{ID: "run-agent", On: projectspec.OnValues{"executing"}, ScriptPath: "/b/hooks/run-agent.sh"}},
		}

		result := mustMergeKitMeta(t,base, []*projectspec.KitMeta{kitA, kitB}, []string{"claude-code", "codex"})
		if len(result.Hooks) != 2 {
			t.Fatalf("expected 2 hooks, got %d", len(result.Hooks))
		}
		if result.Hooks[0].ID != "claude-code/run-agent" {
			t.Errorf("hook[0].ID = %q, want %q", result.Hooks[0].ID, "claude-code/run-agent")
		}
		if result.Hooks[1].ID != "codex/run-agent" {
			t.Errorf("hook[1].ID = %q, want %q", result.Hooks[1].ID, "codex/run-agent")
		}
	})

	t.Run("base hooks are not prefixed", func(t *testing.T) {
		base := &projectspec.ProjectMeta{
			ID:   "proj",
			Name: "Project",
			Hooks: []projectspec.Hook{{ID: "my-hook", On: projectspec.OnValues{"executing"}, ScriptPath: "/proj/hooks/my-hook.sh"}},
		}
		result := mustMergeKitMeta(t,base, nil, nil)
		if len(result.Hooks) != 1 {
			t.Fatalf("expected 1 hook, got %d", len(result.Hooks))
		}
		if result.Hooks[0].ID != "my-hook" {
			t.Errorf("base hook ID = %q, want %q", result.Hooks[0].ID, "my-hook")
		}
	})
}

func TestEffectiveKitRefs(t *testing.T) {
	t.Run("base plus add minus remove", func(t *testing.T) {
		got, err := projectspec.EffectiveKitRefs(
			[]projectspec.KitRef{{Ref: "github.com/acme/repo/default"}, {Ref: "github.com/acme/repo/shared"}},
			projectspec.ProjectLocalKits{
				Add:    []string{"local/dev/repro-kit", "github.com/acme/repo/shared"},
				Remove: []string{"github.com/acme/repo/default"},
			},
		)
		if err != nil {
			t.Fatalf("EffectiveKitRefs: %v", err)
		}
		want := []string{"github.com/acme/repo/shared", "local/dev/repro-kit"}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i].Ref != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	})

	t.Run("reject overlap between add and remove", func(t *testing.T) {
		_, err := projectspec.EffectiveKitRefs(nil, projectspec.ProjectLocalKits{
			Add:    []string{"local/dev/repro-kit"},
			Remove: []string{"local/dev/repro-kit"},
		})
		if err == nil || !strings.Contains(err.Error(), "both kits.add and kits.remove") {
			t.Fatalf("expected add/remove overlap error, got %v", err)
		}
	})
}

func TestApplyProjectLocalMeta(t *testing.T) {
	base := &projectspec.ProjectMeta{
		ID:   "proj",
		Name: "Project",
		Env: map[string]string{
			"BASE":   "yes",
			"SHARED": "base",
		},
		HostCommands: projectspec.HostCommands{
			"git": {Path: "/usr/bin/git"},
			"uv":  {Path: "/usr/bin/uv"},
		},
		AdditionalBindings: []projectspec.BindMount{
			{Source: "/opt/base", Mode: "ro"},
			{Source: "/opt/shared", Mode: "ro"},
		},
	}
	local := &projectspec.ProjectLocalMeta{
		Env: map[string]string{
			"LOCAL":  "yes",
			"SHARED": "local",
		},
		HostCommands: projectspec.HostCommands{
			"uv": {Path: "/custom/bin/uv"},
		},
		AdditionalBindings: []projectspec.BindMount{
			{Source: "/opt/shared", Mode: "rw"},
			{Source: "/opt/local", Mode: "ro"},
		},
	}

	got := projectspec.ApplyProjectLocalMeta(base, local)
	if got.Env["BASE"] != "yes" || got.Env["LOCAL"] != "yes" || got.Env["SHARED"] != "local" {
		t.Fatalf("unexpected env merge: %+v", got.Env)
	}
	if got.HostCommands["git"].Path != "/usr/bin/git" || got.HostCommands["uv"].Path != "/custom/bin/uv" {
		t.Fatalf("unexpected host command merge: %+v", got.HostCommands)
	}
	if len(got.AdditionalBindings) != 3 {
		t.Fatalf("unexpected bindings: %+v", got.AdditionalBindings)
	}
	if got.AdditionalBindings[1].Source != "/opt/shared" || got.AdditionalBindings[1].Mode != "rw" {
		t.Fatalf("expected binding override in place, got %+v", got.AdditionalBindings)
	}
}

func TestHostCommands_NewDSL(t *testing.T) {
	t.Run("map form with policy", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)
		yaml := `
id: test-proj
name: Test Project
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
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644)

		meta, err := projectspec.ReadProjectMeta(dir)
		if err != nil {
			t.Fatalf("ReadProjectMeta: %v", err)
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
		if !ghDef.AllowStdin {
			t.Fatal("expected AllowStdin=true")
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
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)
		yaml := `
id: test-proj
name: Test Project
host_commands: [gh, aws, az]
`
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644)

		meta, err := projectspec.ReadProjectMeta(dir)
		if err != nil {
			t.Fatalf("ReadProjectMeta: %v", err)
		}
		if len(meta.HostCommands) != 3 {
			t.Fatalf("expected 3 host commands, got %d", len(meta.HostCommands))
		}
		for _, name := range []string{"gh", "aws", "az"} {
			if _, ok := meta.HostCommands[name]; !ok {
				t.Fatalf("expected host_commands to contain %q", name)
			}
		}

		defs := meta.HostCommands.ToCommandDefs()
		ghDef := defs["gh"]
		if len(ghDef.AllowedSubcommands) != 0 && len(ghDef.AllowedPatterns) != 0 {
			t.Fatalf("zero-config should have no restrictions: %+v", ghDef)
		}
	})

	t.Run("zero-config map form", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)
		yaml := `
id: test-proj
name: Test Project
host_commands:
  gh:
  aws:
`
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644)

		meta, err := projectspec.ReadProjectMeta(dir)
		if err != nil {
			t.Fatalf("ReadProjectMeta: %v", err)
		}
		if len(meta.HostCommands) != 2 {
			t.Fatalf("expected 2 host commands, got %d", len(meta.HostCommands))
		}
	})

	t.Run("kit with new DSL", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		kitDir := filepath.Join(boidDir, "kits", "cloud")
		_ = os.MkdirAll(kitDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test\nkits:\n  - cloud\n"), 0o644)
		writeKitYAML(t, kitDir, `
host_commands:
  aws:
    allow: [s3, ecr, sts]
    env:
      AWS_PROFILE: sandbox
  gh:
    allow: [pr, issue]
`)

		meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
		if err != nil {
			t.Fatalf("ReadProjectMetaWithKits: %v", err)
		}
		if len(meta.HostCommands) != 2 {
			t.Fatalf("expected 2 host commands, got %d", len(meta.HostCommands))
		}
		if meta.HostCommands["aws"].Env["AWS_PROFILE"] != "sandbox" {
			t.Fatalf("unexpected aws env: %+v", meta.HostCommands["aws"])
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

func TestReadKitMeta_Scripts(t *testing.T) {
	t.Run("parses and resolves script path", func(t *testing.T) {
		dir := t.TempDir()
		scriptsDir := filepath.Join(dir, "scripts")
		_ = os.MkdirAll(scriptsDir, 0o755)
		_ = os.WriteFile(filepath.Join(scriptsDir, "notify.sh"), []byte("#!/bin/sh\necho ok"), 0o755)
		writeKitYAML(t, dir, `
scripts:
  - id: notify
    description: Sends notification
    on: [task_done, task_aborted]
    filter:
      behavior: dev
`)

		meta, err := projectspec.ReadKitMeta(dir)
		if err != nil {
			t.Fatalf("ReadKitMeta: %v", err)
		}
		if len(meta.Scripts) != 1 {
			t.Fatalf("expected 1 script, got %d", len(meta.Scripts))
		}
		s := meta.Scripts[0]
		if s.ID != "notify" {
			t.Errorf("ID = %q, want %q", s.ID, "notify")
		}
		if s.Description != "Sends notification" {
			t.Errorf("Description = %q, want %q", s.Description, "Sends notification")
		}
		if len(s.On) != 2 || s.On[0] != projectspec.ScriptTriggerTaskDone || s.On[1] != projectspec.ScriptTriggerTaskAborted {
			t.Errorf("On = %v, want [task_done task_aborted]", s.On)
		}
		if s.Filter.Behavior != "dev" {
			t.Errorf("Filter.Behavior = %q, want %q", s.Filter.Behavior, "dev")
		}
		if s.ScriptPath != filepath.Join(scriptsDir, "notify.sh") {
			t.Errorf("ScriptPath = %q, want %q", s.ScriptPath, filepath.Join(scriptsDir, "notify.sh"))
		}
		if meta.ScriptsDir != scriptsDir {
			t.Errorf("ScriptsDir = %q, want %q", meta.ScriptsDir, scriptsDir)
		}
	})

	t.Run("resolves python script", func(t *testing.T) {
		dir := t.TempDir()
		scriptsDir := filepath.Join(dir, "scripts")
		_ = os.MkdirAll(scriptsDir, 0o755)
		_ = os.WriteFile(filepath.Join(scriptsDir, "post-done.py"), []byte("print('ok')"), 0o755)
		writeKitYAML(t, dir, "scripts:\n  - id: post-done\n    on: [task_done]\n")

		meta, err := projectspec.ReadKitMeta(dir)
		if err != nil {
			t.Fatalf("ReadKitMeta: %v", err)
		}
		if len(meta.Scripts) != 1 || meta.Scripts[0].ScriptPath != filepath.Join(scriptsDir, "post-done.py") {
			t.Fatalf("unexpected script: %+v", meta.Scripts)
		}
	})

	t.Run("missing script file returns error", func(t *testing.T) {
		dir := t.TempDir()
		scriptsDir := filepath.Join(dir, "scripts")
		_ = os.MkdirAll(scriptsDir, 0o755)
		writeKitYAML(t, dir, "scripts:\n  - id: missing\n    on: [task_done]\n")

		_, err := projectspec.ReadKitMeta(dir)
		if err == nil || !strings.Contains(err.Error(), "script not found") {
			t.Fatalf("expected script not found error, got %v", err)
		}
	})

	t.Run("invalid trigger value returns error", func(t *testing.T) {
		dir := t.TempDir()
		writeKitYAML(t, dir, "scripts:\n  - id: bad\n    on: [invalid_trigger]\n")

		_, err := projectspec.ReadKitMeta(dir)
		if err == nil || !strings.Contains(err.Error(), "invalid trigger") {
			t.Fatalf("expected invalid trigger error, got %v", err)
		}
	})
}

func TestMergeKitMeta_Scripts(t *testing.T) {
	t.Run("scripts from kit get Kit field set", func(t *testing.T) {
		base := &projectspec.ProjectMeta{ID: "proj", Name: "Project"}
		meta := &projectspec.KitMeta{
			Scripts: []projectspec.Script{
				{ID: "notify", On: []projectspec.ScriptTrigger{projectspec.ScriptTriggerTaskDone}, ScriptPath: "/kit/scripts/notify.sh"},
			},
		}

		result := mustMergeKitMeta(t,base, []*projectspec.KitMeta{meta}, []string{"mykit"})
		if len(result.Scripts) != 1 {
			t.Fatalf("expected 1 script, got %d", len(result.Scripts))
		}
		if result.Scripts[0].Kit != "mykit" {
			t.Errorf("Kit = %q, want %q", result.Scripts[0].Kit, "mykit")
		}
		if result.Scripts[0].ID != "notify" {
			t.Errorf("ID = %q, want %q", result.Scripts[0].ID, "notify")
		}
	})

	t.Run("scripts from multiple kits are merged", func(t *testing.T) {
		base := &projectspec.ProjectMeta{ID: "proj", Name: "Project"}
		kitA := &projectspec.KitMeta{
			Scripts: []projectspec.Script{{ID: "script-a", ScriptPath: "/a/scripts/script-a.sh"}},
		}
		kitB := &projectspec.KitMeta{
			Scripts: []projectspec.Script{{ID: "script-b", ScriptPath: "/b/scripts/script-b.sh"}},
		}

		result := mustMergeKitMeta(t,base, []*projectspec.KitMeta{kitA, kitB}, []string{"kit-a", "kit-b"})
		if len(result.Scripts) != 2 {
			t.Fatalf("expected 2 scripts, got %d: %+v", len(result.Scripts), result.Scripts)
		}
		if result.Scripts[0].Kit != "kit-a" || result.Scripts[1].Kit != "kit-b" {
			t.Errorf("unexpected Kit fields: %+v", result.Scripts)
		}
	})
}

func TestMergeKitMeta_ScriptGates(t *testing.T) {
	t.Run("gate generated from kit script", func(t *testing.T) {
		base := &projectspec.ProjectMeta{ID: "proj", Name: "Project"}
		meta := &projectspec.KitMeta{
			ScriptsDir: "/kit/scripts",
			Scripts: []projectspec.Script{
				{ID: "detect-conflicts", ScriptPath: "/kit/scripts/detect-conflicts.sh"},
			},
		}

		result := mustMergeKitMeta(t,base, []*projectspec.KitMeta{meta}, []string{"github-pr"})

		var scriptGate *projectspec.Gate
		for i := range result.Gates {
			if result.Gates[i].ID == "github-pr/detect-conflicts" {
				scriptGate = &result.Gates[i]
				break
			}
		}
		if scriptGate == nil {
			t.Fatalf("expected gate github-pr/detect-conflicts, got gates: %+v", result.Gates)
		}
		if len(scriptGate.Behavior) != 1 || scriptGate.Behavior[0] != "_script:github-pr/detect-conflicts" {
			t.Errorf("Behavior = %v, want [_script:github-pr/detect-conflicts]", scriptGate.Behavior)
		}
		if len(scriptGate.On) != 1 || scriptGate.On[0] != "executing" {
			t.Errorf("On = %v, want [executing]", scriptGate.On)
		}
		produces := scriptGate.Traits.Produces
		if len(produces) != 2 {
			t.Errorf("Produces = %v, want [artifact tasks]", produces)
		}
		if scriptGate.Kit != "github-pr" {
			t.Errorf("Kit = %q, want %q", scriptGate.Kit, "github-pr")
		}
		if scriptGate.ScriptPath != "/kit/scripts/detect-conflicts.sh" {
			t.Errorf("ScriptPath = %q, want %q", scriptGate.ScriptPath, "/kit/scripts/detect-conflicts.sh")
		}
	})

	t.Run("KitScriptsDirs populated from kit scripts", func(t *testing.T) {
		base := &projectspec.ProjectMeta{ID: "proj", Name: "Project"}
		meta := &projectspec.KitMeta{
			ScriptsDir: "/kit/scripts",
			Scripts: []projectspec.Script{
				{ID: "notify", ScriptPath: "/kit/scripts/notify.sh"},
			},
		}

		result := mustMergeKitMeta(t,base, []*projectspec.KitMeta{meta}, []string{"mykit"})

		if len(result.KitScriptsDirs) != 1 {
			t.Fatalf("expected 1 KitScriptsDirs entry, got %d", len(result.KitScriptsDirs))
		}
		if result.KitScriptsDirs[0].ScriptsDir != "/kit/scripts" {
			t.Errorf("ScriptsDir = %q, want %q", result.KitScriptsDirs[0].ScriptsDir, "/kit/scripts")
		}
		if len(result.KitScriptsDirs[0].ScriptIDs) != 1 || result.KitScriptsDirs[0].ScriptIDs[0] != "notify" {
			t.Errorf("ScriptIDs = %v, want [notify]", result.KitScriptsDirs[0].ScriptIDs)
		}
	})

	t.Run("no KitScriptsDirs when ScriptsDir is empty", func(t *testing.T) {
		base := &projectspec.ProjectMeta{ID: "proj", Name: "Project"}
		meta := &projectspec.KitMeta{
			Scripts: []projectspec.Script{
				{ID: "notify", ScriptPath: "/kit/scripts/notify.sh"},
			},
		}

		result := mustMergeKitMeta(t,base, []*projectspec.KitMeta{meta}, []string{"mykit"})
		if len(result.KitScriptsDirs) != 0 {
			t.Errorf("expected no KitScriptsDirs, got %+v", result.KitScriptsDirs)
		}
	})
}

func TestBuildScriptTask(t *testing.T) {
	t.Run("basic fields", func(t *testing.T) {
		script := projectspec.Script{
			ID:  "detect-conflicts",
			Kit: "github-pr",
		}
		payload := []byte(`{"branch":"main"}`)

		task := projectspec.BuildScriptTask(script, "proj-1", payload)

		if task.ProjectID != "proj-1" {
			t.Errorf("ProjectID = %q, want proj-1", task.ProjectID)
		}
		if task.Behavior != "_script:github-pr/detect-conflicts" {
			t.Errorf("Behavior = %q, want _script:github-pr/detect-conflicts", task.Behavior)
		}
		if task.Title != "script: github-pr/detect-conflicts" {
			t.Errorf("Title = %q, want 'script: github-pr/detect-conflicts'", task.Title)
		}
		if !task.Readonly || !task.Ephemeral || !task.AutoStart {
			t.Errorf("Readonly=%v Ephemeral=%v AutoStart=%v, all want true", task.Readonly, task.Ephemeral, task.AutoStart)
		}
		if string(task.Payload) != string(payload) {
			t.Errorf("Payload = %s, want %s", task.Payload, payload)
		}
	})
}

func TestReadProjectMeta_HostCommandRelativePath(t *testing.T) {
	t.Run("relative path resolved to project root", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)

		scriptDir := filepath.Join(dir, "scripts")
		_ = os.MkdirAll(scriptDir, 0o755)
		_ = os.WriteFile(filepath.Join(scriptDir, "run.sh"), []byte("#!/bin/sh\necho ok"), 0o755)

		yaml := `
id: test-proj
name: Test Project
host_commands:
  my-cmd:
    path: scripts/run.sh
    allow: ["*"]
`
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644)

		meta, err := projectspec.ReadProjectMeta(dir)
		if err != nil {
			t.Fatalf("ReadProjectMeta: %v", err)
		}

		spec := meta.HostCommands["my-cmd"]
		want := filepath.Join(dir, "scripts", "run.sh")
		if spec.Path != want {
			t.Fatalf("expected path %q, got %q", want, spec.Path)
		}
	})

	t.Run("absolute path unchanged", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)

		yaml := `
id: test-proj
name: Test Project
host_commands:
  my-cmd:
    path: /usr/bin/some-cmd
`
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644)

		meta, err := projectspec.ReadProjectMeta(dir)
		if err != nil {
			t.Fatalf("ReadProjectMeta: %v", err)
		}

		spec := meta.HostCommands["my-cmd"]
		if spec.Path != "/usr/bin/some-cmd" {
			t.Fatalf("expected path /usr/bin/some-cmd, got %q", spec.Path)
		}
	})

	t.Run("directory traversal rejected", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)

		yaml := `
id: test-proj
name: Test Project
host_commands:
  my-cmd:
    path: ../../../etc/passwd
`
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644)

		_, err := projectspec.ReadProjectMeta(dir)
		if err == nil {
			t.Fatal("expected error for directory traversal")
		}
		if !strings.Contains(err.Error(), "outside project directory") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("symlink traversal rejected", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		scriptsDir := filepath.Join(dir, "scripts")
		_ = os.MkdirAll(boidDir, 0o755)
		_ = os.MkdirAll(scriptsDir, 0o755)

		// Create a symlink that points outside the project
		_ = os.Symlink("/etc", filepath.Join(scriptsDir, "escape"))

		yaml := `
id: test-proj
name: Test Project
host_commands:
  my-cmd:
    path: scripts/escape/passwd
`
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644)

		_, err := projectspec.ReadProjectMeta(dir)
		if err == nil {
			t.Fatal("expected error for symlink traversal")
		}
		if !strings.Contains(err.Error(), "outside project directory") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("empty path unchanged", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)

		yaml := `
id: test-proj
name: Test Project
host_commands:
  gh:
    allow: [pr]
`
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644)

		meta, err := projectspec.ReadProjectMeta(dir)
		if err != nil {
			t.Fatalf("ReadProjectMeta: %v", err)
		}

		spec := meta.HostCommands["gh"]
		if spec.Path != "" {
			t.Fatalf("expected empty path, got %q", spec.Path)
		}
	})
}

func TestReadKitMeta_NewFields(t *testing.T) {
	t.Run("parses meta/detect/requires/scaffold", func(t *testing.T) {
		dir := t.TempDir()
		writeKitYAML(t, dir, `
meta:
  name: go-kit
  description: Go language kit
  category: language
detect:
  files:
    - go.mod
    - go.sum
requires:
  commands:
    - go
scaffold:
  task_behaviors:
    description: Go task behaviors scaffold
    template: scaffold/task_behaviors.yaml
`)
		meta, err := projectspec.ReadKitMeta(dir)
		if err != nil {
			t.Fatalf("ReadKitMeta: %v", err)
		}

		if meta.Meta == nil {
			t.Fatal("expected Meta to be set")
		}
		if meta.Meta.Name != "go-kit" {
			t.Errorf("Meta.Name = %q, want %q", meta.Meta.Name, "go-kit")
		}
		if meta.Meta.Category != "language" {
			t.Errorf("Meta.Category = %q, want %q", meta.Meta.Category, "language")
		}

		if meta.Detect == nil {
			t.Fatal("expected Detect to be set")
		}
		if len(meta.Detect.Files) != 2 || meta.Detect.Files[0] != "go.mod" {
			t.Errorf("Detect.Files = %v", meta.Detect.Files)
		}

		if meta.Requires == nil {
			t.Fatal("expected Requires to be set")
		}
		if len(meta.Requires.Commands) != 1 || meta.Requires.Commands[0] != "go" {
			t.Errorf("Requires.Commands = %v", meta.Requires.Commands)
		}

		if meta.Scaffold == nil || meta.Scaffold.TaskBehaviors == nil {
			t.Fatal("expected Scaffold.TaskBehaviors to be set")
		}
		if meta.Scaffold.TaskBehaviors.Template != "scaffold/task_behaviors.yaml" {
			t.Errorf("Scaffold.TaskBehaviors.Template = %q", meta.Scaffold.TaskBehaviors.Template)
		}
	})

	t.Run("backward compatible: no new fields", func(t *testing.T) {
		dir := t.TempDir()
		writeKitYAML(t, dir, `
task_behaviors:
  dev:
    name: development
    traits: []
`)
		meta, err := projectspec.ReadKitMeta(dir)
		if err != nil {
			t.Fatalf("ReadKitMeta: %v", err)
		}
		if meta.Meta != nil {
			t.Error("expected Meta to be nil")
		}
		if meta.Detect != nil {
			t.Error("expected Detect to be nil")
		}
		if meta.Requires != nil {
			t.Error("expected Requires to be nil")
		}
		if meta.Scaffold != nil {
			t.Error("expected Scaffold to be nil")
		}
	})

	t.Run("new fields excluded from MergeKitMeta", func(t *testing.T) {
		dir := t.TempDir()
		writeKitYAML(t, dir, `
meta:
  name: test-kit
detect:
  files: [go.mod]
requires:
  commands: [go]
scaffold:
  task_behaviors:
    description: desc
    template: tmpl.yaml
`)
		kitMeta, err := projectspec.ReadKitMeta(dir)
		if err != nil {
			t.Fatalf("ReadKitMeta: %v", err)
		}

		base := &projectspec.ProjectMeta{}
		merged := mustMergeKitMeta(t, base, []*projectspec.KitMeta{kitMeta}, []string{"test-kit"})

		// Scripts field (merged) should not be affected by init-time fields.
		// Mainly verify the merged result has no side-effect from the new fields.
		if len(merged.Scripts) != 0 {
			t.Errorf("unexpected scripts in merged: %v", merged.Scripts)
		}
		// The merged ProjectMeta has no Meta/Detect/Requires/Scaffold fields —
		// those live only on KitMeta (confirmed by having compiled without them).
	})
}

func TestMergeKitMeta_HostCommandConflict(t *testing.T) {
	base := &projectspec.ProjectMeta{ID: "proj", Name: "Project"}

	t.Run("same command in two kits returns error", func(t *testing.T) {
		kitA := &projectspec.KitMeta{HostCommands: projectspec.HostCommands{"gh": {Path: "/usr/bin/gh"}}}
		kitB := &projectspec.KitMeta{HostCommands: projectspec.HostCommands{"gh": {Path: "/usr/local/bin/gh"}}}

		_, err := projectspec.MergeKitMeta(base, []*projectspec.KitMeta{kitA, kitB}, []string{"kit-a", "kit-b"})
		if err == nil {
			t.Fatal("expected error for duplicate host_commands, got nil")
		}
		if !strings.Contains(err.Error(), `"gh"`) || !strings.Contains(err.Error(), "kit-a") || !strings.Contains(err.Error(), "kit-b") {
			t.Errorf("error message should mention command name and both kits: %v", err)
		}
	})

	t.Run("different commands across kits is fine", func(t *testing.T) {
		kitA := &projectspec.KitMeta{HostCommands: projectspec.HostCommands{"go": {Path: "/usr/bin/go"}}}
		kitB := &projectspec.KitMeta{HostCommands: projectspec.HostCommands{"gh": {Path: "/usr/bin/gh"}}}

		result, err := projectspec.MergeKitMeta(base, []*projectspec.KitMeta{kitA, kitB}, []string{"kit-a", "kit-b"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result.HostCommands) != 2 {
			t.Errorf("expected 2 host_commands, got %d", len(result.HostCommands))
		}
	})

	t.Run("base project overrides kit command without error", func(t *testing.T) {
		kit := &projectspec.KitMeta{HostCommands: projectspec.HostCommands{"gh": {Path: "/usr/bin/gh"}}}
		baseWithCmd := &projectspec.ProjectMeta{
			ID:           "proj",
			Name:         "Project",
			HostCommands: projectspec.HostCommands{"gh": {Path: "/custom/gh"}},
		}

		result, err := projectspec.MergeKitMeta(baseWithCmd, []*projectspec.KitMeta{kit}, []string{"mykit"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.HostCommands["gh"].Path != "/custom/gh" {
			t.Errorf("expected base override /custom/gh, got %q", result.HostCommands["gh"].Path)
		}
	})
}

func TestUnionBindMounts_ModePromotion(t *testing.T) {
	base := &projectspec.ProjectMeta{ID: "proj", Name: "Project"}

	t.Run("ro+rw promotes to rw", func(t *testing.T) {
		kitA := &projectspec.KitMeta{AdditionalBindings: []projectspec.BindMount{{Source: "/data", Mode: "ro"}}}
		kitB := &projectspec.KitMeta{AdditionalBindings: []projectspec.BindMount{{Source: "/data", Mode: "rw"}}}

		result := mustMergeKitMeta(t, base, []*projectspec.KitMeta{kitA, kitB}, []string{"kit-a", "kit-b"})
		if len(result.AdditionalBindings) != 1 {
			t.Fatalf("expected 1 binding, got %d", len(result.AdditionalBindings))
		}
		if result.AdditionalBindings[0].Mode != "rw" {
			t.Errorf("expected mode rw after ro+rw, got %q", result.AdditionalBindings[0].Mode)
		}
	})

	t.Run("rw+ro keeps rw", func(t *testing.T) {
		kitA := &projectspec.KitMeta{AdditionalBindings: []projectspec.BindMount{{Source: "/data", Mode: "rw"}}}
		kitB := &projectspec.KitMeta{AdditionalBindings: []projectspec.BindMount{{Source: "/data", Mode: "ro"}}}

		result := mustMergeKitMeta(t, base, []*projectspec.KitMeta{kitA, kitB}, []string{"kit-a", "kit-b"})
		if len(result.AdditionalBindings) != 1 {
			t.Fatalf("expected 1 binding, got %d", len(result.AdditionalBindings))
		}
		if result.AdditionalBindings[0].Mode != "rw" {
			t.Errorf("expected mode rw, got %q", result.AdditionalBindings[0].Mode)
		}
	})

	t.Run("ro+ro stays ro", func(t *testing.T) {
		kitA := &projectspec.KitMeta{AdditionalBindings: []projectspec.BindMount{{Source: "/data", Mode: "ro"}}}
		kitB := &projectspec.KitMeta{AdditionalBindings: []projectspec.BindMount{{Source: "/data", Mode: "ro"}}}

		result := mustMergeKitMeta(t, base, []*projectspec.KitMeta{kitA, kitB}, []string{"kit-a", "kit-b"})
		if len(result.AdditionalBindings) != 1 {
			t.Fatalf("expected 1 binding, got %d", len(result.AdditionalBindings))
		}
		if result.AdditionalBindings[0].Mode != "ro" {
			t.Errorf("expected mode ro, got %q", result.AdditionalBindings[0].Mode)
		}
	})

	t.Run("rw+rw stays rw", func(t *testing.T) {
		kitA := &projectspec.KitMeta{AdditionalBindings: []projectspec.BindMount{{Source: "/data", Mode: "rw"}}}
		kitB := &projectspec.KitMeta{AdditionalBindings: []projectspec.BindMount{{Source: "/data", Mode: "rw"}}}

		result := mustMergeKitMeta(t, base, []*projectspec.KitMeta{kitA, kitB}, []string{"kit-a", "kit-b"})
		if len(result.AdditionalBindings) != 1 {
			t.Fatalf("expected 1 binding, got %d", len(result.AdditionalBindings))
		}
		if result.AdditionalBindings[0].Mode != "rw" {
			t.Errorf("expected mode rw, got %q", result.AdditionalBindings[0].Mode)
		}
	})
}
