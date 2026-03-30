package kit_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/kit"
)

func writeKitYAML(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "kit.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadKit_Valid(t *testing.T) {
	dir := t.TempDir()
	hooksDir := filepath.Join(dir, "hooks")
	os.MkdirAll(hooksDir, 0o755)
	os.WriteFile(filepath.Join(hooksDir, "run-build.sh"), []byte("#!/bin/bash\necho ok"), 0o755)

	writeKitYAML(t, dir, `
hooks:
  - id: run-build
    on: executing
    requires_traits: [agent_prompt]
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
    traits: [agent_prompt]
`)

	m, err := kit.ReadKit(dir)
	if err != nil {
		t.Fatalf("ReadKit: %v", err)
	}

	if len(m.Hooks) != 1 || m.Hooks[0].ID != "run-build" {
		t.Errorf("hooks = %v, want 1 hook with id run-build", m.Hooks)
	}
	if m.Hooks[0].ScriptPath == "" {
		t.Error("hook ScriptPath not resolved")
	}
	if len(m.HostCommands) != 1 {
		t.Errorf("expected 1 host_command, got %d", len(m.HostCommands))
	}
	if _, ok := m.HostCommands["go"]; !ok {
		t.Error("expected host_commands to contain 'go'")
	}
	if m.Env["GOPATH"] != "/home/user/go" {
		t.Errorf("env GOPATH = %q", m.Env["GOPATH"])
	}
	if m.HooksDir != hooksDir {
		t.Errorf("HooksDir = %q, want %q", m.HooksDir, hooksDir)
	}
	if _, ok := m.TaskBehaviors["dev"]; !ok {
		t.Error("task_behaviors missing dev")
	}
}

func TestReadKit_EnvInterpolation(t *testing.T) {
	dir := t.TempDir()
	writeKitYAML(t, dir, `
additional_bindings:
  - source: ${TEST_BOID_HOME}/.local/share/go
env:
  GOPATH: ${TEST_BOID_HOME}/go
`)

	t.Setenv("TEST_BOID_HOME", "/home/testuser")

	m, err := kit.ReadKit(dir)
	if err != nil {
		t.Fatalf("ReadKit: %v", err)
	}

	if m.AdditionalBindings[0].Source != "/home/testuser/.local/share/go" {
		t.Errorf("binding = %q, want interpolated path", m.AdditionalBindings[0].Source)
	}
	if m.Env["GOPATH"] != "/home/testuser/go" {
		t.Errorf("env GOPATH = %q, want interpolated", m.Env["GOPATH"])
	}
}

func TestReadKit_MissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := kit.ReadKit(dir)
	if err == nil {
		t.Fatal("expected error for missing kit.yaml")
	}
}

func TestReadKit_InvalidHookOn(t *testing.T) {
	dir := t.TempDir()
	writeKitYAML(t, dir, `
hooks:
  - id: bad-hook
    on: invalid_status
`)
	_, err := kit.ReadKit(dir)
	if err == nil {
		t.Fatal("expected error for invalid hook on value")
	}
}

func TestReadKit_MissingHookScript(t *testing.T) {
	dir := t.TempDir()
	writeKitYAML(t, dir, `
hooks:
  - id: no-script
    on: executing
`)
	_, err := kit.ReadKit(dir)
	if err == nil {
		t.Fatal("expected error for missing hook script")
	}
}
