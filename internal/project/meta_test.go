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
