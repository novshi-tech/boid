package project_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/project"
)

func TestReadMeta_Valid(t *testing.T) {
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
      - agent_prompt
hooks:
  - id: run-agent
    on: executing
    requires_traits:
      - agent_prompt
host_commands:
  git:
    path: /usr/bin/git
env:
  KEY: val
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	// Create hook script
	if err := os.WriteFile(filepath.Join(hooksDir, "run-agent.sh"), []byte("#!/bin/sh\necho hi"), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	meta, err := project.ReadMeta(dir)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}

	if meta.ID != "test-proj" {
		t.Fatalf("expected id test-proj, got %s", meta.ID)
	}
	if meta.Name != "Test Project" {
		t.Fatalf("expected name Test Project, got %s", meta.Name)
	}
	if meta.WorkspaceID != "ws-1" {
		t.Fatalf("expected workspace_id ws-1, got %s", meta.WorkspaceID)
	}
	if len(meta.TaskBehaviors) != 1 {
		t.Fatalf("expected 1 task_behavior, got %d", len(meta.TaskBehaviors))
	}
	if len(meta.Hooks) != 1 {
		t.Fatalf("expected 1 hook, got %d", len(meta.Hooks))
	}
	if meta.Hooks[0].ScriptPath != filepath.Join(hooksDir, "run-agent.sh") {
		t.Fatalf("expected script path %s, got %s", filepath.Join(hooksDir, "run-agent.sh"), meta.Hooks[0].ScriptPath)
	}
	if len(meta.HostCommands) != 1 {
		t.Fatalf("expected 1 host_command, got %d", len(meta.HostCommands))
	}
	if _, ok := meta.HostCommands["git"]; !ok {
		t.Fatal("expected host_commands to contain 'git'")
	}
	if meta.Env["KEY"] != "val" {
		t.Fatalf("expected env KEY=val, got %s", meta.Env["KEY"])
	}
}

func TestReadMeta_MissingID(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	yaml := `
name: No ID Project
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	_, err := project.ReadMeta(dir)
	if err == nil {
		t.Fatal("expected error for missing id")
	}
	if !strings.Contains(err.Error(), "id is required") {
		t.Fatalf("expected 'id is required' error, got: %v", err)
	}
}

func TestReadMeta_MissingName(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	yaml := `
id: test-proj
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	_, err := project.ReadMeta(dir)
	if err == nil {
		t.Fatal("expected error for missing name")
	}
	if !strings.Contains(err.Error(), "name is required") {
		t.Fatalf("expected 'name is required' error, got: %v", err)
	}
}

func TestReadMeta_MissingProjectYaml(t *testing.T) {
	dir := t.TempDir()

	_, err := project.ReadMeta(dir)
	if err == nil {
		t.Fatal("expected error for missing project.yaml")
	}
}

func TestReadMeta_HookScriptResolution_Python(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	hooksDir := filepath.Join(boidDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	yaml := `
id: test-proj
name: Test
hooks:
  - id: my-hook
    on: executing
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	// Create .py hook script
	if err := os.WriteFile(filepath.Join(hooksDir, "my-hook.py"), []byte("print('hi')"), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	meta, err := project.ReadMeta(dir)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}

	if meta.Hooks[0].ScriptPath != filepath.Join(hooksDir, "my-hook.py") {
		t.Fatalf("expected .py script path, got %s", meta.Hooks[0].ScriptPath)
	}
}

func TestReadMeta_HookScriptMissing(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	hooksDir := filepath.Join(boidDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	yaml := `
id: test-proj
name: Test
hooks:
  - id: missing-hook
    on: executing
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	_, err := project.ReadMeta(dir)
	if err == nil {
		t.Fatal("expected error for missing hook script")
	}
	if !strings.Contains(err.Error(), "script not found") {
		t.Fatalf("expected 'script not found' error, got: %v", err)
	}
}

func TestReadMeta_WithGates(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	gatesDir := filepath.Join(boidDir, "gates")
	if err := os.MkdirAll(gatesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	yaml := `
id: test-proj
name: Test
gates:
  - id: push-pr
    on: executing
    requires_traits:
      - pr
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gatesDir, "push-pr.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatalf("write gate: %v", err)
	}

	meta, err := project.ReadMeta(dir)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}

	if len(meta.Gates) != 1 {
		t.Fatalf("expected 1 gate, got %d", len(meta.Gates))
	}
	if meta.Gates[0].ID != "push-pr" {
		t.Fatalf("expected gate id push-pr, got %s", meta.Gates[0].ID)
	}
	if meta.Gates[0].ScriptPath != filepath.Join(gatesDir, "push-pr.sh") {
		t.Fatalf("expected gate script path, got %s", meta.Gates[0].ScriptPath)
	}
}

func TestReadMeta_GateScriptMissing(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	gatesDir := filepath.Join(boidDir, "gates")
	if err := os.MkdirAll(gatesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	yaml := `
id: test-proj
name: Test
gates:
  - id: missing-gate
    on: executing
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	_, err := project.ReadMeta(dir)
	if err == nil {
		t.Fatal("expected error for missing gate script")
	}
	if !strings.Contains(err.Error(), "gate script not found") {
		t.Fatalf("expected 'gate script not found' error, got: %v", err)
	}
}

func TestReadMeta_InvalidGateOnValue(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	gatesDir := filepath.Join(boidDir, "gates")
	if err := os.MkdirAll(gatesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	yaml := `
id: test-proj
name: Test
gates:
  - id: bad-gate
    on: invalid_status
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	_, err := project.ReadMeta(dir)
	if err == nil {
		t.Fatal("expected error for invalid gate on value")
	}
	if !strings.Contains(err.Error(), "invalid on value") {
		t.Fatalf("expected 'invalid on value' error, got: %v", err)
	}
}

func TestReadMeta_InvalidHookOnValue(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	hooksDir := filepath.Join(boidDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	yaml := `
id: test-proj
name: Test
hooks:
  - id: bad-hook
    on: invalid_status
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	_, err := project.ReadMeta(dir)
	if err == nil {
		t.Fatal("expected error for invalid hook on value")
	}
	if !strings.Contains(err.Error(), "invalid on value") {
		t.Fatalf("expected 'invalid on value' error, got: %v", err)
	}
}

// --- Append the following test functions to meta_test.go ---

func TestReadMetaWithKits_LocalKit(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	kitsDir := filepath.Join(boidDir, "kits", "go-dev")
	if err := os.MkdirAll(kitsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	projectYAML := `
id: test-proj
name: Test Project
kits:
  - go-dev
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}

	kitYAML := `
additional_bindings:
  - source: /usr/local/go
env:
  GOPATH: /home/user/go
`
	if err := os.WriteFile(filepath.Join(kitsDir, "kit.yaml"), []byte(kitYAML), 0o644); err != nil {
		t.Fatalf("write kit.yaml: %v", err)
	}

	meta, err := project.ReadMetaWithKits(dir, nil)
	if err != nil {
		t.Fatalf("ReadMetaWithKits: %v", err)
	}

	if meta.Env["GOPATH"] != "/home/user/go" {
		t.Errorf("expected GOPATH=/home/user/go, got %s", meta.Env["GOPATH"])
	}
	if len(meta.AdditionalBindings) == 0 || meta.AdditionalBindings[0].Source != "/usr/local/go" {
		t.Errorf("expected additional_bindings to contain /usr/local/go, got %v", meta.AdditionalBindings)
	}
}

func TestReadMetaWithKits_LocalKitWithHooks(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	kitDir := filepath.Join(boidDir, "kits", "build")
	kitHooksDir := filepath.Join(kitDir, "hooks")
	if err := os.MkdirAll(kitHooksDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	projectYAML := `
id: test-proj
name: Test Project
kits:
  - build
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}

	kitYAML := `
hooks:
  - id: run-build
    on: executing
    requires_traits:
      - agent_prompt
`
	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(kitYAML), 0o644); err != nil {
		t.Fatalf("write kit.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kitHooksDir, "run-build.sh"), []byte("#!/bin/bash\necho build"), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	meta, err := project.ReadMetaWithKits(dir, nil)
	if err != nil {
		t.Fatalf("ReadMetaWithKits: %v", err)
	}

	if len(meta.Hooks) != 1 || meta.Hooks[0].ID != "run-build" {
		t.Errorf("expected 1 hook with id run-build, got %v", meta.Hooks)
	}
	if len(meta.KitHooksDirs) != 1 {
		t.Errorf("expected 1 KitHooksDirs entry, got %d", len(meta.KitHooksDirs))
	}
}

func TestReadMetaWithKits_LocalKitEnvInterpolation(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	kitsDir := filepath.Join(boidDir, "kits", "go-dev")
	if err := os.MkdirAll(kitsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	projectYAML := `
id: test-proj
name: Test Project
kits:
  - go-dev
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}

	kitYAML := `
additional_bindings:
  - source: ${TEST_BOID_HOME}/.local/share/go
env:
  GOPATH: ${TEST_BOID_HOME}/go
`
	if err := os.WriteFile(filepath.Join(kitsDir, "kit.yaml"), []byte(kitYAML), 0o644); err != nil {
		t.Fatalf("write kit.yaml: %v", err)
	}

	t.Setenv("TEST_BOID_HOME", "/home/testuser")

	meta, err := project.ReadMetaWithKits(dir, nil)
	if err != nil {
		t.Fatalf("ReadMetaWithKits: %v", err)
	}

	if meta.Env["GOPATH"] != "/home/testuser/go" {
		t.Errorf("expected GOPATH=/home/testuser/go, got %s", meta.Env["GOPATH"])
	}
	if meta.AdditionalBindings[0].Source != "/home/testuser/.local/share/go" {
		t.Errorf("expected interpolated binding, got %s", meta.AdditionalBindings[0].Source)
	}
}

func TestReadMetaWithKits_LocalKitNotFound(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	projectYAML := `
id: test-proj
name: Test Project
kits:
  - nonexistent-kit
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}

	_, err := project.ReadMetaWithKits(dir, nil)
	if err == nil {
		t.Fatal("expected error for nonexistent local kit")
	}
	if !strings.Contains(err.Error(), "kit.yaml not found") {
		t.Fatalf("expected 'kit.yaml not found' error, got: %v", err)
	}
}

func TestReadMetaWithKits_MultipleLocalKits(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")

	// Create two local kits
	for _, name := range []string{"go-dev", "git"} {
		kitDir := filepath.Join(boidDir, "kits", name)
		if err := os.MkdirAll(kitDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	projectYAML := `
id: test-proj
name: Test Project
kits:
  - go-dev
  - git
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}

	goKitYAML := `
env:
  GOPATH: /home/user/go
additional_bindings:
  - source: /usr/local/go
`
	if err := os.WriteFile(filepath.Join(boidDir, "kits", "go-dev", "kit.yaml"), []byte(goKitYAML), 0o644); err != nil {
		t.Fatalf("write go-dev kit.yaml: %v", err)
	}

	gitKitYAML := `
host_commands:
  git:
    path: /usr/bin/git
    extract_subcommand_fn: git
    allowed_subcommands:
      - status
      - diff
      - log
      - add
      - commit
      - push
      - pull
      - fetch
      - checkout
      - branch
      - merge
      - rebase
      - stash
      - tag
      - remote
      - clone
      - init
`
	if err := os.WriteFile(filepath.Join(boidDir, "kits", "git", "kit.yaml"), []byte(gitKitYAML), 0o644); err != nil {
		t.Fatalf("write git kit.yaml: %v", err)
	}

	meta, err := project.ReadMetaWithKits(dir, nil)
	if err != nil {
		t.Fatalf("ReadMetaWithKits: %v", err)
	}

	if meta.Env["GOPATH"] != "/home/user/go" {
		t.Errorf("expected GOPATH from go-dev kit, got %s", meta.Env["GOPATH"])
	}
	if _, ok := meta.HostCommands["git"]; !ok {
		t.Error("expected host_commands to contain 'git' from git kit")
	}
	if len(meta.AdditionalBindings) == 0 {
		t.Error("expected additional_bindings from go-dev kit")
	}
}
