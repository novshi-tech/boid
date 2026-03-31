package orchestrator_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

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
workspace_id: ws-1
task_behaviors:
  dev:
    name: development
    transition: one-shot
    traits:
      - artifactompt
hooks:
  - id: run-agent
    on: executing
    requires_traits:
      - artifactompt
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

	if meta.ID != "test-proj" || meta.Name != "Test Project" || meta.WorkspaceID != "ws-1" {
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
		if len(meta.Hooks) != 1 || meta.Hooks[0].ID != "run-build" || len(meta.KitHooksDirs) != 1 {
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
    transition: one-shot
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

func TestMergeKitMeta(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		base := &projectspec.ProjectMeta{ID: "proj", Name: "Project", Env: map[string]string{"KEY": "val"}}
		result := projectspec.MergeKitMeta(base, nil)
		if result.Env["KEY"] != "val" {
			t.Errorf("env KEY = %q, want val", result.Env["KEY"])
		}
	})

	t.Run("single kit", func(t *testing.T) {
		base := &projectspec.ProjectMeta{
			ID:           "proj",
			Name:         "Project",
			HostCommands: map[string]projectspec.CommandDef{"git": {Path: "/usr/bin/git"}},
			Hooks:        []projectspec.Hook{{ID: "proj-hook", On: "executing"}},
			Env:          map[string]string{"PROJECT_VAR": "pval"},
		}
		meta := &projectspec.KitMeta{
			HostCommands:       map[string]projectspec.CommandDef{"go": {Path: "/usr/bin/go"}, "git": {Path: "/usr/bin/git"}},
			AdditionalBindings: []projectspec.BindMount{{Source: "/usr/local/go"}},
			Hooks:              []projectspec.Hook{{ID: "kit-hook", On: "verifying", ScriptPath: "/kit/hooks/kit-hook.sh"}},
			HooksDir:           "/kit/hooks",
			Env:                map[string]string{"GOPATH": "/home/go", "PROJECT_VAR": "kit-overridden"},
			TaskBehaviors:      map[string]projectspec.TaskBehavior{"dev": {Name: "dev", Transition: "one-shot"}},
		}

		result := projectspec.MergeKitMeta(base, []*projectspec.KitMeta{meta})
		if len(result.HostCommands) != 2 || len(result.AdditionalBindings) != 1 || len(result.Hooks) != 2 {
			t.Fatalf("unexpected merge result: %+v", result)
		}
		if result.Hooks[0].ID != "kit-hook" || result.Hooks[1].ID != "proj-hook" {
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
		m1 := &projectspec.KitMeta{Env: map[string]string{"A": "from-m1", "SHARED": "m1"}, HostCommands: map[string]projectspec.CommandDef{"go": {Path: "/usr/bin/go"}}}
		m2 := &projectspec.KitMeta{Env: map[string]string{"B": "from-m2", "SHARED": "m2"}, HostCommands: map[string]projectspec.CommandDef{"go": {Path: "/usr/bin/go"}, "gh": {Path: "/usr/bin/gh"}}}

		result := projectspec.MergeKitMeta(base, []*projectspec.KitMeta{m1, m2})
		if result.Env["A"] != "from-m1" || result.Env["B"] != "from-m2" || result.Env["SHARED"] != "m2" || result.Env["PROJ"] != "yes" || len(result.HostCommands) != 2 {
			t.Fatalf("unexpected merge result: %+v", result)
		}
	})

	t.Run("hook id collision", func(t *testing.T) {
		base := &projectspec.ProjectMeta{ID: "proj", Name: "Project", Hooks: []projectspec.Hook{{ID: "build", On: "executing", ScriptPath: "/proj/hooks/build.sh"}}}
		meta := &projectspec.KitMeta{Hooks: []projectspec.Hook{{ID: "build", On: "executing", ScriptPath: "/kit/hooks/build.sh"}}, HooksDir: "/kit/hooks"}

		result := projectspec.MergeKitMeta(base, []*projectspec.KitMeta{meta})
		if len(result.Hooks) != 1 || result.Hooks[0].ScriptPath != "/proj/hooks/build.sh" {
			t.Fatalf("expected project hook to win, got %+v", result.Hooks)
		}
	})
}
