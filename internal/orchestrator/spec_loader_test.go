package orchestrator_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

// mergeKitsIntoBehavior is a test helper that builds a fresh TaskBehavior and
// merges the given kits into it via MergeKitMetaIntoBehavior.
func mergeKitsIntoBehavior(t *testing.T, base projectspec.TaskBehavior, kits []*projectspec.KitMeta, agents []string) projectspec.TaskBehavior {
	t.Helper()
	if err := projectspec.MergeKitMetaIntoBehavior(&base, kits, agents); err != nil {
		t.Fatalf("MergeKitMetaIntoBehavior: unexpected error: %v", err)
	}
	return base
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
host_commands:
  git:
    path: /usr/bin/git
env:
  KEY: val
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
	if len(meta.TaskBehaviors) != 1 {
		t.Fatalf("unexpected task_behaviors count: %+v", meta.TaskBehaviors)
	}
	if _, ok := meta.HostCommands["git"]; !ok {
		t.Fatal("expected host_commands to contain 'git'")
	}
	if meta.Env["KEY"] != "val" {
		t.Fatalf("expected env KEY=val, got %s", meta.Env["KEY"])
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

func TestReadProjectMeta_TopLevelKitsAccepted(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	_ = os.MkdirAll(boidDir, 0o755)
	content := "id: test-proj\nname: Test\nkits:\n  - local/my-kit\n"
	_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(content), 0o644)

	meta, err := projectspec.ReadProjectMeta(dir)
	if err != nil {
		t.Fatalf("expected top-level kits to be accepted, got error: %v", err)
	}
	if len(meta.Kits) != 1 || meta.Kits[0].Ref != "local/my-kit" {
		t.Fatalf("unexpected kits: %+v", meta.Kits)
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

func TestReadProjectMeta_EnvInterpolation(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	_ = os.MkdirAll(boidDir, 0o755)
	yaml := "id: test-proj\nname: Test Project\nhost_commands:\n  my-tool:\n    path: ${TEST_BOID_HOME}/bin/my-tool\nadditional_bindings:\n  - source: ${TEST_BOID_HOME}/.local/share/go\n    target: ${TEST_BOID_HOME}/.cache/go\nenv:\n  MY_VAR: ${TEST_BOID_HOME}/my-dir\n"
	_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644)
	t.Setenv("TEST_BOID_HOME", "/home/testuser")

	meta, err := projectspec.ReadProjectMeta(dir)
	if err != nil {
		t.Fatalf("ReadProjectMeta: %v", err)
	}
	if got := meta.HostCommands["my-tool"].Path; got != "/home/testuser/bin/my-tool" {
		t.Fatalf("host_commands.path not interpolated: %q", got)
	}
	if got := meta.AdditionalBindings[0].Source; got != "/home/testuser/.local/share/go" {
		t.Fatalf("additional_bindings.source not interpolated: %q", got)
	}
	if got := meta.AdditionalBindings[0].Target; got != "/home/testuser/.cache/go" {
		t.Fatalf("additional_bindings.target not interpolated: %q", got)
	}
	if got := meta.Env["MY_VAR"]; got != "/home/testuser/my-dir" {
		t.Fatalf("env value not interpolated: %q", got)
	}
}

// ${WORKTREE} と ${PROJECT_WORKDIR} は dispatch 時に per-job で展開されるため、
// meta load 時には literal で温存されることを保証する。
func TestReadProjectMeta_DeferredWorktreeTokens(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	_ = os.MkdirAll(boidDir, 0o755)
	yaml := "id: test-proj\nname: Test Project\nadditional_bindings:\n  - source: ${PROJECT_WORKDIR}/global.json\n    target: ${WORKTREE}/global.json\n    is_file: true\n"
	_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644)
	// daemon env に PROJECT_WORKDIR / WORKTREE が混入しても影響されないことを確認
	t.Setenv("PROJECT_WORKDIR", "/should-not-be-used")
	t.Setenv("WORKTREE", "/should-not-be-used")

	meta, err := projectspec.ReadProjectMeta(dir)
	if err != nil {
		t.Fatalf("ReadProjectMeta: %v", err)
	}
	if got := meta.AdditionalBindings[0].Source; got != "${PROJECT_WORKDIR}/global.json" {
		t.Fatalf("Source must remain literal, got %q", got)
	}
	if got := meta.AdditionalBindings[0].Target; got != "${WORKTREE}/global.json" {
		t.Fatalf("Target must remain literal, got %q", got)
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

func TestReadProjectMetaWithKits_LocalKits(t *testing.T) {
	t.Run("single local kit", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		kitsDir := filepath.Join(boidDir, "kits", "go-dev")
		_ = os.MkdirAll(kitsDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test Project\ntask_behaviors:\n  dev:\n    name: dev\n    kits:\n      - go-dev\n"), 0o644)
		_ = os.WriteFile(filepath.Join(kitsDir, "kit.yaml"), []byte("additional_bindings:\n  - source: /usr/local/go\nenv:\n  GOPATH: /home/user/go\n"), 0o644)

		meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
		if err != nil {
			t.Fatalf("ReadProjectMetaWithKits: %v", err)
		}
		b := meta.TaskBehaviors["dev"]
		if b.Env["GOPATH"] != "/home/user/go" || len(b.AdditionalBindings) == 0 || b.AdditionalBindings[0].Source != "/usr/local/go" {
			t.Fatalf("unexpected merged behavior: %+v", b)
		}
	})

	t.Run("local kit with hooks", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		kitDir := filepath.Join(boidDir, "kits", "build")
		kitHooksDir := filepath.Join(kitDir, "hooks")
		_ = os.MkdirAll(kitHooksDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test Project\ntask_behaviors:\n  dev:\n    name: dev\n    kits:\n      - build\n"), 0o644)
		_ = os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte("hooks:\n  - id: run-build\n    requires_traits:\n      - artifactompt\n"), 0o644)
		_ = os.WriteFile(filepath.Join(kitHooksDir, "run-build.sh"), []byte("#!/bin/bash\necho build"), 0o755)

		meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
		if err != nil {
			t.Fatalf("ReadProjectMetaWithKits: %v", err)
		}
		b := meta.TaskBehaviors["dev"]
		if len(b.Hooks) != 1 || b.Hooks[0].ID != "build/run-build" || len(b.KitRoots) != 1 {
			t.Fatalf("unexpected merged hooks: %+v", b)
		}
	})

	t.Run("env interpolation", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		kitsDir := filepath.Join(boidDir, "kits", "go-dev")
		_ = os.MkdirAll(kitsDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test Project\ntask_behaviors:\n  dev:\n    name: dev\n    kits:\n      - go-dev\n"), 0o644)
		_ = os.WriteFile(filepath.Join(kitsDir, "kit.yaml"), []byte("additional_bindings:\n  - source: ${TEST_BOID_HOME}/.local/share/go\n    target: ${TEST_BOID_HOME}/.claude/skills/go\nenv:\n  GOPATH: ${TEST_BOID_HOME}/go\n"), 0o644)
		t.Setenv("TEST_BOID_HOME", "/home/testuser")

		meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
		if err != nil {
			t.Fatalf("ReadProjectMetaWithKits: %v", err)
		}
		b := meta.TaskBehaviors["dev"]
		if b.Env["GOPATH"] != "/home/testuser/go" || b.AdditionalBindings[0].Source != "/home/testuser/.local/share/go" {
			t.Fatalf("unexpected interpolated behavior: %+v", b)
		}
		if b.AdditionalBindings[0].Target != "/home/testuser/.claude/skills/go" {
			t.Fatalf("Target not interpolated: %+v", b.AdditionalBindings[0])
		}
	})

	t.Run("missing local kit", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test Project\ntask_behaviors:\n  dev:\n    name: dev\n    kits:\n      - nonexistent-kit\n"), 0o644)

		_, err := projectspec.ReadProjectMetaWithKits(dir, nil)
		if err == nil || !strings.Contains(err.Error(), "kit.yaml not found") {
			t.Fatalf("expected kit.yaml not found, got %v", err)
		}
	})

	t.Run("multiple local kits", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		for _, name := range []string{"go-dev", "gh"} {
			_ = os.MkdirAll(filepath.Join(boidDir, "kits", name), 0o755)
		}
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test Project\ntask_behaviors:\n  dev:\n    name: dev\n    kits:\n      - go-dev\n      - gh\n"), 0o644)
		_ = os.WriteFile(filepath.Join(boidDir, "kits", "go-dev", "kit.yaml"), []byte("env:\n  GOPATH: /home/user/go\nadditional_bindings:\n  - source: /usr/local/go\n"), 0o644)
		_ = os.WriteFile(filepath.Join(boidDir, "kits", "gh", "kit.yaml"), []byte("host_commands:\n  gh:\n    path: /usr/bin/gh\n"), 0o644)

		meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
		if err != nil {
			t.Fatalf("ReadProjectMetaWithKits: %v", err)
		}
		b := meta.TaskBehaviors["dev"]
		if b.Env["GOPATH"] != "/home/user/go" || len(b.AdditionalBindings) == 0 {
			t.Fatalf("expected merged env and bindings, got %+v", b)
		}
		if _, ok := b.HostCommands["gh"]; !ok {
			t.Fatal("expected host_commands to contain 'gh' from gh kit")
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
task_behaviors:
  dev:
    name: dev
    kits:
      - local/dev/repro-kit
env:
  FROM_PROJECT: base
host_commands:
  gh:
    path: /usr/bin/gh
additional_bindings:
  - source: /opt/base
    mode: ro
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}

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
	b := meta.TaskBehaviors["dev"]
	// local kit wins over nothing for FROM_LOCAL_KIT, but project/local overlays
	// win over kit for FROM_PROJECT.
	if b.Env["FROM_LOCAL_KIT"] != "yes" || b.Env["FROM_PROJECT"] != "local" || b.Env["LOCAL_ONLY"] != "enabled" {
		t.Fatalf("unexpected env merge on behavior: %+v", b.Env)
	}
	if b.HostCommands["uv"].Path != "/custom/bin/uv" {
		t.Fatalf("unexpected host command override: %+v", b.HostCommands["uv"])
	}
	if b.HostCommands["gh"].Path != "/usr/bin/gh" {
		t.Fatalf("project host command should be preserved: %+v", b.HostCommands)
	}
	// /opt/local-kit from kit (ro) is promoted to rw by project.local overlay.
	// /opt/base from project-level overlay is present too.
	var foundLocalKit, foundBase bool
	for _, bind := range b.AdditionalBindings {
		if bind.Source == "/opt/local-kit" && bind.Mode == "rw" {
			foundLocalKit = true
		}
		if bind.Source == "/opt/base" {
			foundBase = true
		}
	}
	if !foundLocalKit || !foundBase {
		t.Fatalf("expected bindings for /opt/local-kit (rw) and /opt/base, got %+v", b.AdditionalBindings)
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
		writeKitYAML(t, dir, "additional_bindings:\n  - source: ${TEST_BOID_HOME}/.local/share/go\n    target: ${TEST_BOID_HOME}/.claude/skills/go\nenv:\n  GOPATH: ${TEST_BOID_HOME}/go\n")
		t.Setenv("TEST_BOID_HOME", "/home/testuser")

		meta, err := projectspec.ReadKitMeta(dir)
		if err != nil {
			t.Fatalf("ReadKitMeta: %v", err)
		}
		if meta.AdditionalBindings[0].Source != "/home/testuser/.local/share/go" || meta.Env["GOPATH"] != "/home/testuser/go" {
			t.Fatalf("unexpected interpolation: %+v", meta)
		}
		if meta.AdditionalBindings[0].Target != "/home/testuser/.claude/skills/go" {
			t.Fatalf("Target not interpolated: %+v", meta.AdditionalBindings[0])
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
		writeKitYAML(t, dir, "hooks:\n  - id: no-script\n")
		_, err := projectspec.ReadKitMeta(dir)
		if err == nil {
			t.Fatal("expected error for missing hook script")
		}
	})

	t.Run("invalid kind value is rejected", func(t *testing.T) {
		dir := t.TempDir()
		hooksDir := filepath.Join(dir, "hooks")
		_ = os.MkdirAll(hooksDir, 0o755)
		_ = os.WriteFile(filepath.Join(hooksDir, "bad.sh"), []byte("#!/bin/bash\n"), 0o755)
		writeKitYAML(t, dir, "hooks:\n  - id: bad\n    kind: runner\n")
		_, err := projectspec.ReadKitMeta(dir)
		if err == nil || !strings.Contains(err.Error(), "invalid kind") {
			t.Fatalf("expected invalid kind error, got %v", err)
		}
	})

	t.Run("agent field without kind: agent is rejected", func(t *testing.T) {
		dir := t.TempDir()
		hooksDir := filepath.Join(dir, "hooks")
		_ = os.MkdirAll(hooksDir, 0o755)
		_ = os.WriteFile(filepath.Join(hooksDir, "util.sh"), []byte("#!/bin/bash\n"), 0o755)
		writeKitYAML(t, dir, "hooks:\n  - id: util\n    agent: claude-code\n")
		_, err := projectspec.ReadKitMeta(dir)
		if err == nil || !strings.Contains(err.Error(), "agent") {
			t.Fatalf("expected agent-without-kind error, got %v", err)
		}
	})

	t.Run("gate with kind is rejected", func(t *testing.T) {
		dir := t.TempDir()
		gatesDir := filepath.Join(dir, "gates")
		_ = os.MkdirAll(gatesDir, 0o755)
		_ = os.WriteFile(filepath.Join(gatesDir, "check.sh"), []byte("#!/bin/bash\n"), 0o755)
		writeKitYAML(t, dir, "gates:\n  - id: check\n    kind: agent\n")
		_, err := projectspec.ReadKitMeta(dir)
		if err == nil || !strings.Contains(err.Error(), "kind") {
			t.Fatalf("expected gate-kind error, got %v", err)
		}
	})
}

func TestReadProjectMetaWithKits_RejectsBuiltinInHostCommands(t *testing.T) {
	for _, name := range []string{"git", "boid"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			boidDir := filepath.Join(dir, ".boid")
			_ = os.MkdirAll(boidDir, 0o755)
			projectYAML := "id: test-proj\nname: Test Project\nhost_commands:\n  " + name + ":\n    path: /usr/bin/" + name + "\ntask_behaviors:\n  dev:\n    name: dev\n"
			_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644)

			_, err := projectspec.ReadProjectMetaWithKits(dir, nil)
			if err == nil || !strings.Contains(err.Error(), "builtin command") {
				t.Fatalf("expected builtin/host conflict for %q, got %v", name, err)
			}
		})
	}
}

func TestMergeKitMetaIntoBehavior(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		b := projectspec.TaskBehavior{Name: "dev", Env: map[string]string{"KEY": "val"}}
		result := mergeKitsIntoBehavior(t, b, nil, nil)
		if result.Env["KEY"] != "val" {
			t.Errorf("env KEY = %q, want val", result.Env["KEY"])
		}
	})

	t.Run("single kit", func(t *testing.T) {
		base := projectspec.TaskBehavior{
			Name:         "dev",
			HostCommands: projectspec.HostCommands{"git": {Path: "/usr/bin/git"}},
			Hooks:        []projectspec.Hook{{ID: "proj-hook"}},
			Env:          map[string]string{"PROJECT_VAR": "pval"},
		}
		kit := &projectspec.KitMeta{
			HostCommands:       projectspec.HostCommands{"go": {Path: "/usr/bin/go"}, "git": {Path: "/usr/bin/git"}},
			AdditionalBindings: []projectspec.BindMount{{Source: "/usr/local/go"}},
			Hooks:              []projectspec.Hook{{ID: "kit-hook", ScriptPath: "/kit/hooks/kit-hook.sh"}},
			HooksDir:           "/kit/hooks",
			KitRoot:            "/kit",
			Env:                map[string]string{"GOPATH": "/home/go", "PROJECT_VAR": "kit-overridden"},
		}

		result := mergeKitsIntoBehavior(t, base, []*projectspec.KitMeta{kit}, []string{"mykit"})
		if len(result.HostCommands) != 2 || len(result.AdditionalBindings) != 1 || len(result.Hooks) != 2 {
			t.Fatalf("unexpected merge result: %+v", result)
		}
		if result.Hooks[0].ID != "proj-hook" || result.Hooks[1].ID != "mykit/kit-hook" {
			t.Fatalf("unexpected hook order: %+v", result.Hooks)
		}
		if result.Env["GOPATH"] != "/home/go" || result.Env["PROJECT_VAR"] != "pval" {
			t.Fatalf("unexpected env: %+v", result.Env)
		}
		if result.HostCommands["git"].Path != "/usr/bin/git" {
			t.Fatalf("base host command should win over kit: %+v", result.HostCommands["git"])
		}
		if len(result.KitRoots) != 1 || result.KitRoots[0] != "/kit" {
			t.Fatalf("unexpected KitRoots: %+v", result.KitRoots)
		}
	})

	t.Run("multiple kits", func(t *testing.T) {
		base := projectspec.TaskBehavior{Name: "dev", Env: map[string]string{"PROJ": "yes"}}
		m1 := &projectspec.KitMeta{Env: map[string]string{"A": "from-m1", "SHARED": "m1"}, HostCommands: projectspec.HostCommands{"go": {Path: "/usr/bin/go"}}}
		m2 := &projectspec.KitMeta{Env: map[string]string{"B": "from-m2", "SHARED": "m2"}, HostCommands: projectspec.HostCommands{"gh": {Path: "/usr/bin/gh"}}}

		result := mergeKitsIntoBehavior(t, base, []*projectspec.KitMeta{m1, m2}, []string{"kit-a", "kit-b"})
		if result.Env["A"] != "from-m1" || result.Env["B"] != "from-m2" || result.Env["SHARED"] != "m2" || result.Env["PROJ"] != "yes" || len(result.HostCommands) != 2 {
			t.Fatalf("unexpected merge result: %+v", result)
		}
	})

	t.Run("same raw hook id across kit and base both survive with qualified IDs", func(t *testing.T) {
		base := projectspec.TaskBehavior{Name: "dev", Hooks: []projectspec.Hook{{ID: "build", ScriptPath: "/proj/hooks/build.sh"}}}
		kit := &projectspec.KitMeta{Hooks: []projectspec.Hook{{ID: "build", ScriptPath: "/kit/hooks/build.sh"}}, HooksDir: "/kit/hooks"}

		result := mergeKitsIntoBehavior(t, base, []*projectspec.KitMeta{kit}, []string{"mykit"})
		if len(result.Hooks) != 2 {
			t.Fatalf("expected 2 hooks (base + kit), got %d: %+v", len(result.Hooks), result.Hooks)
		}
		if result.Hooks[0].ID != "build" {
			t.Errorf("hook[0].ID = %q, want %q", result.Hooks[0].ID, "build")
		}
		if result.Hooks[1].ID != "mykit/build" {
			t.Errorf("hook[1].ID = %q, want %q", result.Hooks[1].ID, "mykit/build")
		}
	})
}

func TestResolveKitAgent(t *testing.T) {
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
			got := projectspec.ResolveKitAgent(tc.ref)
			if got != tc.want {
				t.Errorf("ResolveKitAgent(%+v) = %q, want %q", tc.ref, got, tc.want)
			}
		})
	}
}

func TestReadProjectMetaWithKits_DuplicateAgent(t *testing.T) {
	t.Run("duplicate basename rejected within a behavior", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(filepath.Join(boidDir, "kits", "go-dev"), 0o755)
		_ = os.MkdirAll(filepath.Join(boidDir, "kits", "other", "go-dev"), 0o755)
		projectYAML := "id: test-proj\nname: Test Project\ntask_behaviors:\n  dev:\n    name: dev\n    kits:\n      - go-dev\n      - other/go-dev\n"
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644)
		_ = os.WriteFile(filepath.Join(boidDir, "kits", "go-dev", "kit.yaml"), []byte("env:\n  A: a\n"), 0o644)
		_ = os.WriteFile(filepath.Join(boidDir, "kits", "other", "go-dev", "kit.yaml"), []byte("env:\n  B: b\n"), 0o644)

		_, err := projectspec.ReadProjectMetaWithKits(dir, nil)
		if err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("expected ambiguous agent error, got %v", err)
		}
	})

	t.Run("disambiguation with as alias", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(filepath.Join(boidDir, "kits", "go-dev"), 0o755)
		_ = os.MkdirAll(filepath.Join(boidDir, "kits", "other", "go-dev"), 0o755)
		projectYAML := "id: test-proj\nname: Test Project\ntask_behaviors:\n  dev:\n    name: dev\n    kits:\n      - go-dev\n      - ref: other/go-dev\n        as: other-go-dev\n"
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644)
		_ = os.WriteFile(filepath.Join(boidDir, "kits", "go-dev", "kit.yaml"), []byte("env:\n  A: a\n"), 0o644)
		_ = os.WriteFile(filepath.Join(boidDir, "kits", "other", "go-dev", "kit.yaml"), []byte("env:\n  B: b\n"), 0o644)

		meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
		if err != nil {
			t.Fatalf("ReadProjectMetaWithKits: %v", err)
		}
		b := meta.TaskBehaviors["dev"]
		if b.Env["A"] != "a" || b.Env["B"] != "b" {
			t.Fatalf("unexpected env: %+v", b.Env)
		}
	})
}

func TestMergeKitMetaIntoBehavior_KitAgentFields(t *testing.T) {
	t.Run("kit agent hook without explicit agent inherits kit agent name", func(t *testing.T) {
		base := projectspec.TaskBehavior{Name: "dev"}
		kit := &projectspec.KitMeta{
			Hooks: []projectspec.Hook{{ID: "kit-hook", Kind: projectspec.HandlerKindAgent, ScriptPath: "/kit/hooks/kit-hook.sh"}},
		}

		result := mergeKitsIntoBehavior(t, base, []*projectspec.KitMeta{kit}, []string{"claude-code"})
		if len(result.Hooks) != 1 {
			t.Fatalf("expected 1 hook, got %d", len(result.Hooks))
		}
		h := result.Hooks[0]
		if h.Kit != "claude-code" || h.Agent != "claude-code" {
			t.Errorf("unexpected kit/agent: %+v", h)
		}
	})

	t.Run("kit non-agent hook gets Kit provenance but no Agent", func(t *testing.T) {
		base := projectspec.TaskBehavior{Name: "dev"}
		kit := &projectspec.KitMeta{
			Hooks: []projectspec.Hook{{ID: "util-hook", ScriptPath: "/kit/hooks/util-hook.sh"}},
		}

		result := mergeKitsIntoBehavior(t, base, []*projectspec.KitMeta{kit}, []string{"claude-code"})
		h := result.Hooks[0]
		if h.Kit != "claude-code" {
			t.Errorf("expected Kit=claude-code, got %q", h.Kit)
		}
		if h.Agent != "" {
			t.Errorf("non-agent hook should not inherit Agent, got %q", h.Agent)
		}
	})

	t.Run("kit agent hook with explicit agent retains its agent", func(t *testing.T) {
		base := projectspec.TaskBehavior{Name: "dev"}
		kit := &projectspec.KitMeta{
			Hooks: []projectspec.Hook{{ID: "kit-hook", Kind: projectspec.HandlerKindAgent, ScriptPath: "/kit/hooks/kit-hook.sh", Agent: "explicit-agent"}},
		}

		result := mergeKitsIntoBehavior(t, base, []*projectspec.KitMeta{kit}, []string{"claude-code"})
		h := result.Hooks[0]
		if h.Kit != "claude-code" || h.Agent != "explicit-agent" {
			t.Errorf("unexpected kit/agent: %+v", h)
		}
	})

	t.Run("kit hook/gate IDs are qualified with agent prefix", func(t *testing.T) {
		base := projectspec.TaskBehavior{Name: "dev"}
		kit := &projectspec.KitMeta{
			Hooks: []projectspec.Hook{{ID: "run-agent", ScriptPath: "/kit/hooks/run-agent.sh"}},
			Gates: []projectspec.Gate{{ID: "check-quality", ScriptPath: "/kit/gates/check-quality.sh"}},
		}

		result := mergeKitsIntoBehavior(t, base, []*projectspec.KitMeta{kit}, []string{"my-kit"})
		if result.Hooks[0].ID != "my-kit/run-agent" {
			t.Errorf("hook ID = %q, want my-kit/run-agent", result.Hooks[0].ID)
		}
		if result.Gates[0].ID != "my-kit/check-quality" || result.Gates[0].Kit != "my-kit" {
			t.Errorf("unexpected gate: %+v", result.Gates[0])
		}
	})

	t.Run("different kits with same hook ID both survive", func(t *testing.T) {
		base := projectspec.TaskBehavior{Name: "dev"}
		kitA := &projectspec.KitMeta{
			Hooks: []projectspec.Hook{{ID: "run-agent", ScriptPath: "/a/hooks/run-agent.sh"}},
		}
		kitB := &projectspec.KitMeta{
			Hooks: []projectspec.Hook{{ID: "run-agent", ScriptPath: "/b/hooks/run-agent.sh"}},
		}

		result := mergeKitsIntoBehavior(t, base, []*projectspec.KitMeta{kitA, kitB}, []string{"claude-code", "codex"})
		if len(result.Hooks) != 2 {
			t.Fatalf("expected 2 hooks, got %d", len(result.Hooks))
		}
		if result.Hooks[0].ID != "claude-code/run-agent" || result.Hooks[1].ID != "codex/run-agent" {
			t.Errorf("unexpected IDs: %+v", result.Hooks)
		}
	})

	t.Run("base hooks are not prefixed", func(t *testing.T) {
		base := projectspec.TaskBehavior{
			Name:  "dev",
			Hooks: []projectspec.Hook{{ID: "my-hook", ScriptPath: "/proj/hooks/my-hook.sh"}},
		}
		result := mergeKitsIntoBehavior(t, base, nil, nil)
		if len(result.Hooks) != 1 || result.Hooks[0].ID != "my-hook" {
			t.Errorf("unexpected hooks: %+v", result.Hooks)
		}
	})
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
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: test-proj\nname: Test\ntask_behaviors:\n  dev:\n    name: dev\n    kits:\n      - cloud\n"), 0o644)
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
		b := meta.TaskBehaviors["dev"]
		if len(b.HostCommands) != 2 {
			t.Fatalf("expected 2 host commands on behavior, got %d", len(b.HostCommands))
		}
		if b.HostCommands["aws"].Env["AWS_PROFILE"] != "sandbox" {
			t.Fatalf("unexpected aws env: %+v", b.HostCommands["aws"])
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

func TestReadKitMeta_ScriptsSection_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	writeKitYAML(t, dir, `
scripts:
  - id: notify
    on: [task_done]
`)
	_, err := projectspec.ReadKitMeta(dir)
	if err == nil || !strings.Contains(err.Error(), "scripts:") {
		t.Fatalf("expected scripts section error, got %v", err)
	}
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
  script: scripts/detect.sh
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
		if meta.Detect.Script != "scripts/detect.sh" {
			t.Errorf("Detect.Script = %q, want %q", meta.Detect.Script, "scripts/detect.sh")
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

	t.Run("new fields excluded from MergeKitMetaIntoBehavior", func(t *testing.T) {
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

		base := projectspec.TaskBehavior{Name: "dev"}
		merged := mergeKitsIntoBehavior(t, base, []*projectspec.KitMeta{kitMeta}, []string{"test-kit"})
		_ = merged
	})
}

func TestMergeKitMetaIntoBehavior_HostCommandConflict(t *testing.T) {
	t.Run("same command in two kits returns error", func(t *testing.T) {
		kitA := &projectspec.KitMeta{HostCommands: projectspec.HostCommands{"gh": {Path: "/usr/bin/gh"}}}
		kitB := &projectspec.KitMeta{HostCommands: projectspec.HostCommands{"gh": {Path: "/usr/local/bin/gh"}}}

		b := projectspec.TaskBehavior{Name: "dev"}
		err := projectspec.MergeKitMetaIntoBehavior(&b, []*projectspec.KitMeta{kitA, kitB}, []string{"kit-a", "kit-b"})
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

		b := projectspec.TaskBehavior{Name: "dev"}
		if err := projectspec.MergeKitMetaIntoBehavior(&b, []*projectspec.KitMeta{kitA, kitB}, []string{"kit-a", "kit-b"}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(b.HostCommands) != 2 {
			t.Errorf("expected 2 host_commands, got %d", len(b.HostCommands))
		}
	})

	t.Run("behavior-level host command wins over kit", func(t *testing.T) {
		kit := &projectspec.KitMeta{HostCommands: projectspec.HostCommands{"gh": {Path: "/usr/bin/gh"}}}
		b := projectspec.TaskBehavior{Name: "dev", HostCommands: projectspec.HostCommands{"gh": {Path: "/custom/gh"}}}
		if err := projectspec.MergeKitMetaIntoBehavior(&b, []*projectspec.KitMeta{kit}, []string{"mykit"}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if b.HostCommands["gh"].Path != "/custom/gh" {
			t.Errorf("expected behavior override /custom/gh, got %q", b.HostCommands["gh"].Path)
		}
	})
}

func TestMergeKitRuntime(t *testing.T) {
	t.Run("empty kits returns zero value", func(t *testing.T) {
		rt, err := projectspec.MergeKitRuntime(nil, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rt.Env != nil || rt.HostCommands != nil || rt.AdditionalBindings != nil {
			t.Errorf("expected zero KitRuntime, got %+v", rt)
		}
	})

	t.Run("single kit fields populated", func(t *testing.T) {
		kit := &projectspec.KitMeta{
			Env:                map[string]string{"A": "1"},
			HostCommands:       projectspec.HostCommands{"go": {Path: "/usr/bin/go"}},
			AdditionalBindings: []projectspec.BindMount{{Source: "/usr/local/go", Mode: "ro"}},
		}
		rt, err := projectspec.MergeKitRuntime([]*projectspec.KitMeta{kit}, []string{"go-kit"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rt.Env["A"] != "1" {
			t.Errorf("Env[A] = %q, want 1", rt.Env["A"])
		}
		if rt.HostCommands["go"].Path != "/usr/bin/go" {
			t.Errorf("HostCommands[go] = %+v", rt.HostCommands["go"])
		}
		if len(rt.AdditionalBindings) != 1 || rt.AdditionalBindings[0].Source != "/usr/local/go" {
			t.Errorf("AdditionalBindings = %+v", rt.AdditionalBindings)
		}
	})

	t.Run("multiple kits env last-wins", func(t *testing.T) {
		kit1 := &projectspec.KitMeta{Env: map[string]string{"A": "from-kit1", "SHARED": "kit1"}}
		kit2 := &projectspec.KitMeta{Env: map[string]string{"B": "from-kit2", "SHARED": "kit2"}}
		rt, err := projectspec.MergeKitRuntime([]*projectspec.KitMeta{kit1, kit2}, []string{"k1", "k2"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rt.Env["A"] != "from-kit1" || rt.Env["B"] != "from-kit2" || rt.Env["SHARED"] != "kit2" {
			t.Errorf("unexpected env: %+v", rt.Env)
		}
	})

	t.Run("multiple kits host commands merged", func(t *testing.T) {
		kit1 := &projectspec.KitMeta{HostCommands: projectspec.HostCommands{"go": {Path: "/usr/bin/go"}}}
		kit2 := &projectspec.KitMeta{HostCommands: projectspec.HostCommands{"gh": {Path: "/usr/bin/gh"}}}
		rt, err := projectspec.MergeKitRuntime([]*projectspec.KitMeta{kit1, kit2}, []string{"k1", "k2"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(rt.HostCommands) != 2 {
			t.Errorf("expected 2 host commands, got %d: %+v", len(rt.HostCommands), rt.HostCommands)
		}
		if rt.HostCommands["go"].Path != "/usr/bin/go" || rt.HostCommands["gh"].Path != "/usr/bin/gh" {
			t.Errorf("unexpected host commands: %+v", rt.HostCommands)
		}
	})

	t.Run("duplicate host command across kits returns error", func(t *testing.T) {
		kit1 := &projectspec.KitMeta{HostCommands: projectspec.HostCommands{"gh": {Path: "/usr/bin/gh"}}}
		kit2 := &projectspec.KitMeta{HostCommands: projectspec.HostCommands{"gh": {Path: "/usr/local/bin/gh"}}}
		_, err := projectspec.MergeKitRuntime([]*projectspec.KitMeta{kit1, kit2}, []string{"kit-a", "kit-b"})
		if err == nil {
			t.Fatal("expected error for duplicate host command, got nil")
		}
		if !strings.Contains(err.Error(), "gh") || !strings.Contains(err.Error(), "kit-a") || !strings.Contains(err.Error(), "kit-b") {
			t.Errorf("error should mention command and both kit names: %v", err)
		}
	})

	t.Run("additional bindings mode promotion across kits", func(t *testing.T) {
		kit1 := &projectspec.KitMeta{AdditionalBindings: []projectspec.BindMount{{Source: "/data", Mode: "ro"}}}
		kit2 := &projectspec.KitMeta{AdditionalBindings: []projectspec.BindMount{{Source: "/data", Mode: "rw"}}}
		rt, err := projectspec.MergeKitRuntime([]*projectspec.KitMeta{kit1, kit2}, []string{"k1", "k2"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(rt.AdditionalBindings) != 1 || rt.AdditionalBindings[0].Mode != "rw" {
			t.Errorf("expected single rw binding, got %+v", rt.AdditionalBindings)
		}
	})

	t.Run("result matches MergeKitMetaIntoBehavior for kit-derived fields on empty behavior", func(t *testing.T) {
		kit1 := &projectspec.KitMeta{
			Env:                map[string]string{"A": "a"},
			HostCommands:       projectspec.HostCommands{"go": {Path: "/usr/bin/go"}},
			AdditionalBindings: []projectspec.BindMount{{Source: "/data1", Mode: "ro"}},
		}
		kit2 := &projectspec.KitMeta{
			Env:                map[string]string{"B": "b"},
			HostCommands:       projectspec.HostCommands{"gh": {Path: "/usr/bin/gh"}},
			AdditionalBindings: []projectspec.BindMount{{Source: "/data2", Mode: "rw"}},
		}
		agents := []string{"kit1", "kit2"}

		rt, err := projectspec.MergeKitRuntime([]*projectspec.KitMeta{kit1, kit2}, agents)
		if err != nil {
			t.Fatalf("MergeKitRuntime error: %v", err)
		}

		b := projectspec.TaskBehavior{Name: "dev"}
		if err := projectspec.MergeKitMetaIntoBehavior(&b, []*projectspec.KitMeta{kit1, kit2}, agents); err != nil {
			t.Fatalf("MergeKitMetaIntoBehavior error: %v", err)
		}

		if rt.Env["A"] != b.Env["A"] || rt.Env["B"] != b.Env["B"] {
			t.Errorf("env mismatch: MergeKitRuntime=%v, MergeKitMetaIntoBehavior=%v", rt.Env, b.Env)
		}
		if len(rt.HostCommands) != len(b.HostCommands) {
			t.Errorf("host_commands count mismatch: %d vs %d", len(rt.HostCommands), len(b.HostCommands))
		}
		if len(rt.AdditionalBindings) != len(b.AdditionalBindings) {
			t.Errorf("bindings count mismatch: %d vs %d", len(rt.AdditionalBindings), len(b.AdditionalBindings))
		}
	})
}

func TestUnionBindMounts_ModePromotion(t *testing.T) {
	t.Run("ro+rw promotes to rw", func(t *testing.T) {
		kitA := &projectspec.KitMeta{AdditionalBindings: []projectspec.BindMount{{Source: "/data", Mode: "ro"}}}
		kitB := &projectspec.KitMeta{AdditionalBindings: []projectspec.BindMount{{Source: "/data", Mode: "rw"}}}

		result := mergeKitsIntoBehavior(t, projectspec.TaskBehavior{Name: "dev"}, []*projectspec.KitMeta{kitA, kitB}, []string{"kit-a", "kit-b"})
		if len(result.AdditionalBindings) != 1 || result.AdditionalBindings[0].Mode != "rw" {
			t.Errorf("expected 1 binding in rw mode, got %+v", result.AdditionalBindings)
		}
	})

	t.Run("rw+ro keeps rw", func(t *testing.T) {
		kitA := &projectspec.KitMeta{AdditionalBindings: []projectspec.BindMount{{Source: "/data", Mode: "rw"}}}
		kitB := &projectspec.KitMeta{AdditionalBindings: []projectspec.BindMount{{Source: "/data", Mode: "ro"}}}

		result := mergeKitsIntoBehavior(t, projectspec.TaskBehavior{Name: "dev"}, []*projectspec.KitMeta{kitA, kitB}, []string{"kit-a", "kit-b"})
		if len(result.AdditionalBindings) != 1 || result.AdditionalBindings[0].Mode != "rw" {
			t.Errorf("expected 1 binding in rw mode, got %+v", result.AdditionalBindings)
		}
	})

	t.Run("ro+ro stays ro", func(t *testing.T) {
		kitA := &projectspec.KitMeta{AdditionalBindings: []projectspec.BindMount{{Source: "/data", Mode: "ro"}}}
		kitB := &projectspec.KitMeta{AdditionalBindings: []projectspec.BindMount{{Source: "/data", Mode: "ro"}}}

		result := mergeKitsIntoBehavior(t, projectspec.TaskBehavior{Name: "dev"}, []*projectspec.KitMeta{kitA, kitB}, []string{"kit-a", "kit-b"})
		if len(result.AdditionalBindings) != 1 || result.AdditionalBindings[0].Mode != "ro" {
			t.Errorf("expected 1 binding in ro mode, got %+v", result.AdditionalBindings)
		}
	})

	t.Run("rw+rw stays rw", func(t *testing.T) {
		kitA := &projectspec.KitMeta{AdditionalBindings: []projectspec.BindMount{{Source: "/data", Mode: "rw"}}}
		kitB := &projectspec.KitMeta{AdditionalBindings: []projectspec.BindMount{{Source: "/data", Mode: "rw"}}}

		result := mergeKitsIntoBehavior(t, projectspec.TaskBehavior{Name: "dev"}, []*projectspec.KitMeta{kitA, kitB}, []string{"kit-a", "kit-b"})
		if len(result.AdditionalBindings) != 1 || result.AdditionalBindings[0].Mode != "rw" {
			t.Errorf("expected 1 binding in rw mode, got %+v", result.AdditionalBindings)
		}
	})
}

func TestCommands_YAMLParse(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	_ = os.MkdirAll(boidDir, 0o755)
	_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(`
id: test-proj
name: Test Project
commands:
  claude:
    command:
      - claude
      - --dangerously-skip-permissions
  shell:
    command: [bash]
`), 0o644)

	meta, err := projectspec.ReadProjectMeta(dir)
	if err != nil {
		t.Fatalf("ReadProjectMeta: %v", err)
	}
	if len(meta.Commands) != 2 {
		t.Fatalf("expected 2 commands, got %d", len(meta.Commands))
	}
	claude, ok := meta.Commands["claude"]
	if !ok {
		t.Fatal("expected 'claude' command")
	}
	if len(claude.Command) != 2 || claude.Command[0] != "claude" || claude.Command[1] != "--dangerously-skip-permissions" {
		t.Fatalf("unexpected claude command: %+v", claude.Command)
	}
	shell, ok := meta.Commands["shell"]
	if !ok {
		t.Fatal("expected 'shell' command")
	}
	if len(shell.Command) != 1 || shell.Command[0] != "bash" {
		t.Fatalf("unexpected shell command: %+v", shell.Command)
	}
}

func TestCommands_EnvVarExpansion(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	_ = os.MkdirAll(boidDir, 0o755)
	_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(`
id: test-proj
name: Test Project
commands:
  run:
    command:
      - claude
      - --append-system-prompt
      - ${TEST_BOID_HOME}/skills/sandbox.md
`), 0o644)
	t.Setenv("TEST_BOID_HOME", "/home/testuser")

	meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
	if err != nil {
		t.Fatalf("ReadProjectMetaWithKits: %v", err)
	}
	cmd, ok := meta.Commands["run"]
	if !ok {
		t.Fatal("expected 'run' command")
	}
	want := "/home/testuser/skills/sandbox.md"
	if len(cmd.ResolvedCommand) != 3 || cmd.ResolvedCommand[2] != want {
		t.Fatalf("expected ResolvedCommand[2] = %q, got %+v", want, cmd.ResolvedCommand)
	}
	// Original Command slice must remain unexpanded.
	if cmd.Command[2] != "${TEST_BOID_HOME}/skills/sandbox.md" {
		t.Fatalf("expected Command[2] to remain unexpanded, got %q", cmd.Command[2])
	}
}

func TestCommands_KitAggregation(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	kitADir := filepath.Join(boidDir, "kits", "kit-a")
	kitBDir := filepath.Join(boidDir, "kits", "kit-b")
	_ = os.MkdirAll(kitADir, 0o755)
	_ = os.MkdirAll(kitBDir, 0o755)
	_ = os.WriteFile(filepath.Join(kitADir, "kit.yaml"), []byte(`
env:
  FOO: from-kit-a
additional_bindings:
  - source: /mnt/data
    mode: ro
`), 0o644)
	_ = os.WriteFile(filepath.Join(kitBDir, "kit.yaml"), []byte(`
host_commands:
  curl:
    path: /usr/bin/curl
additional_bindings:
  - source: /mnt/cache
    mode: rw
`), 0o644)
	// kit-a と kit-b は project top-level kits に置く。commands はそこから runtime を継承する。
	_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(`
id: test-proj
name: Test Project
kits:
  - kit-a
  - kit-b
commands:
  run:
    command: [bash]
`), 0o644)

	meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
	if err != nil {
		t.Fatalf("ReadProjectMetaWithKits: %v", err)
	}
	cmd := meta.Commands["run"]

	if cmd.Env["FOO"] != "from-kit-a" {
		t.Errorf("expected FOO=from-kit-a, got %q", cmd.Env["FOO"])
	}
	if _, ok := cmd.HostCommands["curl"]; !ok {
		t.Error("expected host_commands to contain 'curl' from kit-b")
	}
	if len(cmd.AdditionalBindings) != 2 {
		t.Errorf("expected 2 additional_bindings, got %d: %+v", len(cmd.AdditionalBindings), cmd.AdditionalBindings)
	}
}

func TestCommands_ProjectEnvOverridesKitEnv(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	kitDir := filepath.Join(boidDir, "kits", "base-kit")
	_ = os.MkdirAll(kitDir, 0o755)
	_ = os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte("env:\n  KEY: from-kit\n"), 0o644)
	// base-kit は project top-level kits に置く。project.yaml の env が上書きされることを確認。
	_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(`
id: test-proj
name: Test Project
kits:
  - base-kit
env:
  KEY: from-project
commands:
  run:
    command: [bash]
`), 0o644)

	meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
	if err != nil {
		t.Fatalf("ReadProjectMetaWithKits: %v", err)
	}
	if got := meta.Commands["run"].Env["KEY"]; got != "from-project" {
		t.Errorf("expected project env to override kit env: got %q, want %q", got, "from-project")
	}
}

// ---------------------------------------------------------------------------
// Top-level kits tests
// ---------------------------------------------------------------------------

func TestReadProjectMetaWithKits_TopLevelKits_MergesIntoAllBehaviors(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	kitDir := filepath.Join(boidDir, "kits", "go-dev")
	_ = os.MkdirAll(kitDir, 0o755)

	_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(`id: test-proj
name: Test
kits:
  - go-dev
task_behaviors:
  dev:
    name: dev
  ci:
    name: ci
`), 0o644)
	_ = os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(`env:
  GOPATH: /home/user/go
additional_bindings:
  - source: /usr/local/go
`), 0o644)

	meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
	if err != nil {
		t.Fatalf("ReadProjectMetaWithKits: %v", err)
	}
	for _, name := range []string{"dev", "ci"} {
		b := meta.TaskBehaviors[name]
		if b.Env["GOPATH"] != "/home/user/go" {
			t.Errorf("behavior %q: expected GOPATH from top-level kit, got %q", name, b.Env["GOPATH"])
		}
		if len(b.AdditionalBindings) == 0 || b.AdditionalBindings[0].Source != "/usr/local/go" {
			t.Errorf("behavior %q: expected additional_bindings from top-level kit, got %v", name, b.AdditionalBindings)
		}
	}
}

func TestReadProjectMetaWithKits_TopLevelKits_AgentOnlyHooksAllowed(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	kitDir := filepath.Join(boidDir, "kits", "agent-kit")
	kitHooksDir := filepath.Join(kitDir, "hooks")
	_ = os.MkdirAll(kitHooksDir, 0o755)

	_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(`id: test-proj
name: Test
kits:
  - agent-kit
task_behaviors:
  dev:
    name: dev
`), 0o644)
	_ = os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(`hooks:
  - id: run-agent
    kind: agent
    agent: my-agent
`), 0o644)
	_ = os.WriteFile(filepath.Join(kitHooksDir, "run-agent.sh"), []byte("#!/bin/sh\necho ok"), 0o755)

	meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
	if err != nil {
		t.Fatalf("expected agent-only hook kit to be accepted at project scope, got: %v", err)
	}
	b := meta.TaskBehaviors["dev"]
	if len(b.Hooks) != 1 || !strings.Contains(b.Hooks[0].ID, "run-agent") {
		t.Errorf("unexpected hooks: %v", b.Hooks)
	}
}

func TestReadProjectMetaWithKits_TopLevelKits_ScopeValidation_GatesRejected(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	kitDir := filepath.Join(boidDir, "kits", "gate-kit")
	kitGatesDir := filepath.Join(kitDir, "gates")
	_ = os.MkdirAll(kitGatesDir, 0o755)

	_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(`id: test-proj
name: Test
kits:
  - gate-kit
task_behaviors:
  dev:
    name: dev
`), 0o644)
	_ = os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(`gates:
  - id: my-gate
    phase: exit
`), 0o644)
	_ = os.WriteFile(filepath.Join(kitGatesDir, "my-gate.sh"), []byte("#!/bin/sh\necho ok"), 0o755)

	_, err := projectspec.ReadProjectMetaWithKits(dir, nil)
	if err == nil || !strings.Contains(err.Error(), "gates を持つ kit は top-level kits に指定できません") {
		t.Fatalf("expected gates rejection error, got: %v", err)
	}
}

func TestReadProjectMetaWithKits_TopLevelKits_ScopeValidation_NonAgentHookRejected(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	kitDir := filepath.Join(boidDir, "kits", "hook-kit")
	kitHooksDir := filepath.Join(kitDir, "hooks")
	_ = os.MkdirAll(kitHooksDir, 0o755)

	_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(`id: test-proj
name: Test
kits:
  - hook-kit
task_behaviors:
  dev:
    name: dev
`), 0o644)
	_ = os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(`hooks:
  - id: run-build
`), 0o644)
	_ = os.WriteFile(filepath.Join(kitHooksDir, "run-build.sh"), []byte("#!/bin/sh\necho build"), 0o755)

	_, err := projectspec.ReadProjectMetaWithKits(dir, nil)
	if err == nil || !strings.Contains(err.Error(), "hook run-build の kind が agent 以外のため top-level kits に指定できません") {
		t.Fatalf("expected non-agent hook rejection error, got: %v", err)
	}
}

func TestIsProjectScopable(t *testing.T) {
	t.Run("no gates no hooks", func(t *testing.T) {
		km := &projectspec.KitMeta{}
		if err := projectspec.IsProjectScopable(km); err != nil {
			t.Errorf("expected nil, got %v", err)
		}
	})

	t.Run("agent-only hooks", func(t *testing.T) {
		km := &projectspec.KitMeta{
			Hooks: []projectspec.Hook{
				{ID: "h1", Kind: projectspec.HandlerKindAgent},
			},
		}
		if err := projectspec.IsProjectScopable(km); err != nil {
			t.Errorf("expected nil for agent-only hooks, got %v", err)
		}
	})

	t.Run("has gates", func(t *testing.T) {
		km := &projectspec.KitMeta{
			Gates: []projectspec.Gate{{ID: "g1"}},
		}
		if err := projectspec.IsProjectScopable(km); err == nil || !strings.Contains(err.Error(), "gates を持つ kit") {
			t.Errorf("expected gates rejection, got %v", err)
		}
	})

	t.Run("non-agent hook", func(t *testing.T) {
		km := &projectspec.KitMeta{
			Hooks: []projectspec.Hook{{ID: "h1"}},
		}
		if err := projectspec.IsProjectScopable(km); err == nil || !strings.Contains(err.Error(), "hook h1 の kind が agent 以外") {
			t.Errorf("expected non-agent hook rejection, got %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// KitMeta.Commands テスト (タスク A/B)
// ---------------------------------------------------------------------------

func TestKitMeta_Commands_ParsedFromYAML(t *testing.T) {
	dir := t.TempDir()
	writeKitYAML(t, dir, `
commands:
  claude:
    command:
      - claude
      - --permission-mode
      - bypassPermissions
  shell:
    command: [bash]
`)
	meta, err := projectspec.ReadKitMeta(dir)
	if err != nil {
		t.Fatalf("ReadKitMeta: %v", err)
	}
	if len(meta.Commands) != 2 {
		t.Fatalf("expected 2 commands, got %d", len(meta.Commands))
	}
	claude, ok := meta.Commands["claude"]
	if !ok {
		t.Fatal("expected 'claude' command")
	}
	if len(claude.Command) != 3 || claude.Command[0] != "claude" {
		t.Fatalf("unexpected claude command: %+v", claude.Command)
	}
}

func TestReadProjectMetaWithKits_KitCommandsMerge(t *testing.T) {
	t.Run("kit commands merged into project commands", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		kitDir := filepath.Join(boidDir, "kits", "agent-kit")
		_ = os.MkdirAll(kitDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(`
id: test-proj
name: Test Project
kits:
  - agent-kit
`), 0o644)
		writeKitYAML(t, kitDir, `
commands:
  claude:
    command: [claude, --permission-mode, bypassPermissions]
`)
		meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
		if err != nil {
			t.Fatalf("ReadProjectMetaWithKits: %v", err)
		}
		cmd, ok := meta.Commands["claude"]
		if !ok {
			t.Fatal("expected 'claude' command from kit")
		}
		if len(cmd.ResolvedCommand) == 0 || cmd.ResolvedCommand[0] != "claude" {
			t.Errorf("unexpected ResolvedCommand: %+v", cmd.ResolvedCommand)
		}
	})

	t.Run("kit-kit conflict on same command name is an error", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		kitADir := filepath.Join(boidDir, "kits", "kit-a")
		kitBDir := filepath.Join(boidDir, "kits", "kit-b")
		_ = os.MkdirAll(kitADir, 0o755)
		_ = os.MkdirAll(kitBDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(`
id: test-proj
name: Test Project
kits:
  - kit-a
  - kit-b
`), 0o644)
		writeKitYAML(t, kitADir, "commands:\n  run:\n    command: [bash]\n")
		writeKitYAML(t, kitBDir, "commands:\n  run:\n    command: [sh]\n")

		_, err := projectspec.ReadProjectMetaWithKits(dir, nil)
		if err == nil {
			t.Fatal("expected conflict error, got nil")
		}
		if !strings.Contains(err.Error(), `"run"`) || !strings.Contains(err.Error(), "kit-a") || !strings.Contains(err.Error(), "kit-b") {
			t.Errorf("error should mention command and both kits: %v", err)
		}
	})

	t.Run("project.yaml command field overrides kit-provided command field at field level", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		kitDir := filepath.Join(boidDir, "kits", "agent-kit")
		_ = os.MkdirAll(kitDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(`
id: test-proj
name: Test Project
kits:
  - agent-kit
commands:
  claude:
    command: [claude, --custom-flag]
`), 0o644)
		writeKitYAML(t, kitDir, "commands:\n  claude:\n    command: [claude, --kit-flag]\n")

		meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
		if err != nil {
			t.Fatalf("ReadProjectMetaWithKits: %v", err)
		}
		cmd := meta.Commands["claude"]
		if len(cmd.ResolvedCommand) < 2 || cmd.ResolvedCommand[1] != "--custom-flag" {
			t.Errorf("expected project.yaml command field to win: %+v", cmd.ResolvedCommand)
		}
	})

	t.Run("project.yaml entry without command inherits from kit", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		kitDir := filepath.Join(boidDir, "kits", "agent-kit")
		_ = os.MkdirAll(kitDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(`
id: test-proj
name: Test Project
kits:
  - agent-kit
commands:
  claude:
`), 0o644)
		writeKitYAML(t, kitDir, "commands:\n  claude:\n    command: [claude, --kit-flag]\n")

		meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
		if err != nil {
			t.Fatalf("ReadProjectMetaWithKits: %v", err)
		}
		cmd := meta.Commands["claude"]
		if len(cmd.ResolvedCommand) < 2 || cmd.ResolvedCommand[0] != "claude" || cmd.ResolvedCommand[1] != "--kit-flag" {
			t.Errorf("expected kit command to be inherited: %+v", cmd.ResolvedCommand)
		}
	})

	t.Run("disjoint project and kit commands coexist", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		kitDir := filepath.Join(boidDir, "kits", "agent-kit")
		_ = os.MkdirAll(kitDir, 0o755)
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(`
id: test-proj
name: Test Project
kits:
  - agent-kit
commands:
  bash:
    command: [bash]
`), 0o644)
		writeKitYAML(t, kitDir, "commands:\n  claude:\n    command: [claude]\n")

		meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
		if err != nil {
			t.Fatalf("ReadProjectMetaWithKits: %v", err)
		}
		bash := meta.Commands["bash"]
		if len(bash.ResolvedCommand) == 0 || bash.ResolvedCommand[0] != "bash" {
			t.Errorf("expected bash command from project: %+v", bash.ResolvedCommand)
		}
		claude := meta.Commands["claude"]
		if len(claude.ResolvedCommand) == 0 || claude.ResolvedCommand[0] != "claude" {
			t.Errorf("expected claude command from kit: %+v", claude.ResolvedCommand)
		}
	})
}

func TestBindMount_Optional_PropagatedFromKitYAML(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	kitsDir := filepath.Join(boidDir, "kits", "opt-kit")
	_ = os.MkdirAll(kitsDir, 0o755)
	_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(`
id: test-proj
name: Test
task_behaviors:
  dev:
    name: dev
    kits:
      - opt-kit
`), 0o644)
	_ = os.WriteFile(filepath.Join(kitsDir, "kit.yaml"), []byte(`
additional_bindings:
  - source: /opt/maybe-missing
    optional: true
  - source: /opt/always-present
`), 0o644)

	meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
	if err != nil {
		t.Fatalf("ReadProjectMetaWithKits: %v", err)
	}
	b := meta.TaskBehaviors["dev"]
	if len(b.AdditionalBindings) != 2 {
		t.Fatalf("expected 2 bindings, got %d: %+v", len(b.AdditionalBindings), b.AdditionalBindings)
	}
	if !b.AdditionalBindings[0].Optional {
		t.Errorf("expected AdditionalBindings[0].Optional=true, got false")
	}
	if b.AdditionalBindings[1].Optional {
		t.Errorf("expected AdditionalBindings[1].Optional=false, got true")
	}
}

func TestReadProjectMetaWithKits_BehaviorCommands(t *testing.T) {
	t.Run("env interpolation in command args", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)
		projectYAML := "id: test-proj\nname: Test Project\ntask_behaviors:\n  dev:\n    name: dev\n    commands:\n      build:\n        command: [\"${TEST_CMD_PREFIX}/bin/build\", \"--out\", \"${TEST_CMD_PREFIX}/out\"]\n"
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644)
		t.Setenv("TEST_CMD_PREFIX", "/opt/tool")

		meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
		if err != nil {
			t.Fatalf("ReadProjectMetaWithKits: %v", err)
		}
		b := meta.TaskBehaviors["dev"]
		cmd, ok := b.Commands["build"]
		if !ok {
			t.Fatal("expected build command in behavior")
		}
		want := []string{"/opt/tool/bin/build", "--out", "/opt/tool/out"}
		if len(cmd.ResolvedCommand) != 3 || cmd.ResolvedCommand[0] != want[0] || cmd.ResolvedCommand[1] != want[1] || cmd.ResolvedCommand[2] != want[2] {
			t.Errorf("ResolvedCommand = %v, want %v", cmd.ResolvedCommand, want)
		}
	})

	t.Run("host_commands inherited from behavior", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		_ = os.MkdirAll(boidDir, 0o755)
		projectYAML := "id: test-proj\nname: Test Project\nhost_commands:\n  my-linter:\n    path: /usr/local/bin/my-linter\ntask_behaviors:\n  dev:\n    name: dev\n    commands:\n      lint:\n        command: [bash, -c, \"my-linter ./...\"]\n"
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644)

		meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
		if err != nil {
			t.Fatalf("ReadProjectMetaWithKits: %v", err)
		}
		b := meta.TaskBehaviors["dev"]
		cmd, ok := b.Commands["lint"]
		if !ok {
			t.Fatal("expected lint command in behavior")
		}
		if _, ok := cmd.HostCommands["my-linter"]; !ok {
			t.Error("expected my-linter in cmd.HostCommands (inherited from project host_commands via behavior)")
		}
		if len(cmd.ResolvedCommand) != 3 || cmd.ResolvedCommand[0] != "bash" {
			t.Errorf("ResolvedCommand = %v, want [bash -c 'my-linter ./...']", cmd.ResolvedCommand)
		}
	})

	t.Run("kit env inherited by behavior commands", func(t *testing.T) {
		dir := t.TempDir()
		boidDir := filepath.Join(dir, ".boid")
		kitDir := filepath.Join(boidDir, "kits", "go-kit")
		_ = os.MkdirAll(kitDir, 0o755)
		projectYAML := "id: test-proj\nname: Test Project\ntask_behaviors:\n  dev:\n    name: dev\n    kits:\n      - go-kit\n    commands:\n      test:\n        command: [go, test, ./...]\n"
		_ = os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644)
		_ = os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte("env:\n  GOPATH: /home/user/go\n"), 0o644)

		meta, err := projectspec.ReadProjectMetaWithKits(dir, nil)
		if err != nil {
			t.Fatalf("ReadProjectMetaWithKits: %v", err)
		}
		b := meta.TaskBehaviors["dev"]
		cmd, ok := b.Commands["test"]
		if !ok {
			t.Fatal("expected test command in behavior")
		}
		if cmd.Env["GOPATH"] != "/home/user/go" {
			t.Errorf("cmd.Env[GOPATH] = %q, want /home/user/go", cmd.Env["GOPATH"])
		}
	})
}
