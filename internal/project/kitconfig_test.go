package project_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/project"
)

// writeKitYAML is a helper for kit tests.
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

	m, err := project.ReadKit(dir)
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

	m, err := project.ReadKit(dir)
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
	_, err := project.ReadKit(dir)
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
	_, err := project.ReadKit(dir)
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
	_, err := project.ReadKit(dir)
	if err == nil {
		t.Fatal("expected error for missing hook script")
	}
}

func TestMergeKits_Empty(t *testing.T) {
	base := &project.ProjectMeta{
		ID:   "proj",
		Name: "Project",
		Env:  map[string]string{"KEY": "val"},
	}
	result := project.MergeKits(base, nil)
	if result.Env["KEY"] != "val" {
		t.Errorf("env KEY = %q, want val", result.Env["KEY"])
	}
}

func TestMergeKits_SingleKit(t *testing.T) {
	base := &project.ProjectMeta{
		ID:           "proj",
		Name:         "Project",
		HostCommands: map[string]project.CommandDef{"git": {Path: "/usr/bin/git"}},
		Hooks: []project.Hook{
			{ID: "proj-hook", On: "executing"},
		},
		Env: map[string]string{"PROJECT_VAR": "pval"},
	}
	m := &project.KitMeta{
		HostCommands:       map[string]project.CommandDef{"go": {Path: "/usr/bin/go"}, "git": {Path: "/usr/bin/git"}},
		AdditionalBindings: []project.BindMount{{Source: "/usr/local/go"}},
		Hooks: []project.Hook{
			{ID: "kit-hook", On: "verifying", ScriptPath: "/kit/hooks/kit-hook.sh"},
		},
		HooksDir: "/kit/hooks",
		Env:      map[string]string{"GOPATH": "/home/go", "PROJECT_VAR": "kit-overridden"},
		TaskBehaviors: map[string]project.TaskBehavior{
			"dev": {Name: "dev", Transition: "one-shot"},
		},
	}

	result := project.MergeKits(base, []*project.KitMeta{m})

	if len(result.HostCommands) != 2 {
		t.Errorf("host_commands = %v, want [go git]", result.HostCommands)
	}
	if len(result.AdditionalBindings) != 1 || result.AdditionalBindings[0].Source != "/usr/local/go" {
		t.Errorf("additional_bindings = %v", result.AdditionalBindings)
	}
	if len(result.Hooks) != 2 {
		t.Fatalf("hooks count = %d, want 2", len(result.Hooks))
	}
	if result.Hooks[0].ID != "kit-hook" {
		t.Errorf("first hook = %q, want kit-hook", result.Hooks[0].ID)
	}
	if result.Hooks[1].ID != "proj-hook" {
		t.Errorf("second hook = %q, want proj-hook", result.Hooks[1].ID)
	}
	if result.Env["GOPATH"] != "/home/go" {
		t.Errorf("env GOPATH = %q, want /home/go", result.Env["GOPATH"])
	}
	if result.Env["PROJECT_VAR"] != "pval" {
		t.Errorf("env PROJECT_VAR = %q, want pval (project should win)", result.Env["PROJECT_VAR"])
	}
	if _, ok := result.TaskBehaviors["dev"]; !ok {
		t.Error("task_behaviors missing dev")
	}
	if len(result.KitHooksDirs) != 1 || result.KitHooksDirs[0].HooksDir != "/kit/hooks" {
		t.Errorf("KitHooksDirs = %v", result.KitHooksDirs)
	}
}

func TestMergeKits_MultipleKits(t *testing.T) {
	base := &project.ProjectMeta{
		ID:   "proj",
		Name: "Project",
		Env:  map[string]string{"PROJ": "yes"},
	}
	m1 := &project.KitMeta{
		Env:          map[string]string{"A": "from-m1", "SHARED": "m1"},
		HostCommands: map[string]project.CommandDef{"go": {Path: "/usr/bin/go"}},
	}
	m2 := &project.KitMeta{
		Env:          map[string]string{"B": "from-m2", "SHARED": "m2"},
		HostCommands: map[string]project.CommandDef{"go": {Path: "/usr/bin/go"}, "gh": {Path: "/usr/bin/gh"}},
	}

	result := project.MergeKits(base, []*project.KitMeta{m1, m2})

	if result.Env["A"] != "from-m1" {
		t.Errorf("env A = %q", result.Env["A"])
	}
	if result.Env["B"] != "from-m2" {
		t.Errorf("env B = %q", result.Env["B"])
	}
	if result.Env["SHARED"] != "m2" {
		t.Errorf("env SHARED = %q, want m2 (later kit wins)", result.Env["SHARED"])
	}
	if result.Env["PROJ"] != "yes" {
		t.Errorf("env PROJ = %q", result.Env["PROJ"])
	}
	if len(result.HostCommands) != 2 {
		t.Errorf("host_commands = %v, want [go gh]", result.HostCommands)
	}
}

func TestMergeKits_HookIDCollision(t *testing.T) {
	base := &project.ProjectMeta{
		ID:   "proj",
		Name: "Project",
		Hooks: []project.Hook{
			{ID: "build", On: "executing", ScriptPath: "/proj/hooks/build.sh"},
		},
	}
	m := &project.KitMeta{
		Hooks: []project.Hook{
			{ID: "build", On: "executing", ScriptPath: "/kit/hooks/build.sh"},
		},
		HooksDir: "/kit/hooks",
	}

	result := project.MergeKits(base, []*project.KitMeta{m})

	if len(result.Hooks) != 1 {
		t.Fatalf("hooks count = %d, want 1 (dedup by ID)", len(result.Hooks))
	}
	if result.Hooks[0].ScriptPath != "/proj/hooks/build.sh" {
		t.Errorf("hook ScriptPath = %q, want project version", result.Hooks[0].ScriptPath)
	}
}
