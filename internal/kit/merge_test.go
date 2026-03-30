package kit_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/hostcmd"
	"github.com/novshi-tech/boid/internal/kit"
	"github.com/novshi-tech/boid/internal/model"
)

func TestMergeKits_Empty(t *testing.T) {
	base := &model.ProjectMeta{
		ID:   "proj",
		Name: "Project",
		Env:  map[string]string{"KEY": "val"},
	}
	result := kit.MergeKits(base, nil)
	if result.Env["KEY"] != "val" {
		t.Errorf("env KEY = %q, want val", result.Env["KEY"])
	}
}

func TestMergeKits_SingleKit(t *testing.T) {
	base := &model.ProjectMeta{
		ID:           "proj",
		Name:         "Project",
		HostCommands: map[string]hostcmd.CommandDef{"git": {Path: "/usr/bin/git"}},
		Hooks: []model.Hook{
			{ID: "proj-hook", On: "executing"},
		},
		Env: map[string]string{"PROJECT_VAR": "pval"},
	}
	m := &kit.KitMeta{
		HostCommands:       map[string]hostcmd.CommandDef{"go": {Path: "/usr/bin/go"}, "git": {Path: "/usr/bin/git"}},
		AdditionalBindings: []string{"/usr/local/go"},
		Hooks: []model.Hook{
			{ID: "kit-hook", On: "verifying", ScriptPath: "/kit/hooks/kit-hook.sh"},
		},
		HooksDir: "/kit/hooks",
		Env:      map[string]string{"GOPATH": "/home/go", "PROJECT_VAR": "kit-overridden"},
		TaskBehaviors: map[string]model.TaskBehavior{
			"dev": {Name: "dev", Transition: "one-shot"},
		},
	}

	result := kit.MergeKits(base, []*kit.KitMeta{m})

	// HostCommands: union
	if len(result.HostCommands) != 2 {
		t.Errorf("host_commands = %v, want [go git]", result.HostCommands)
	}

	// AdditionalBindings: from kit
	if len(result.AdditionalBindings) != 1 || result.AdditionalBindings[0] != "/usr/local/go" {
		t.Errorf("additional_bindings = %v", result.AdditionalBindings)
	}

	// Hooks: kit first, then project
	if len(result.Hooks) != 2 {
		t.Fatalf("hooks count = %d, want 2", len(result.Hooks))
	}
	if result.Hooks[0].ID != "kit-hook" {
		t.Errorf("first hook = %q, want kit-hook", result.Hooks[0].ID)
	}
	if result.Hooks[1].ID != "proj-hook" {
		t.Errorf("second hook = %q, want proj-hook", result.Hooks[1].ID)
	}

	// Env: project overrides kit
	if result.Env["GOPATH"] != "/home/go" {
		t.Errorf("env GOPATH = %q, want /home/go", result.Env["GOPATH"])
	}
	if result.Env["PROJECT_VAR"] != "pval" {
		t.Errorf("env PROJECT_VAR = %q, want pval (project should win)", result.Env["PROJECT_VAR"])
	}

	// TaskBehaviors: from kit
	if _, ok := result.TaskBehaviors["dev"]; !ok {
		t.Error("task_behaviors missing dev")
	}

	// KitHooksDirs: collected
	if len(result.KitHooksDirs) != 1 || result.KitHooksDirs[0].HooksDir != "/kit/hooks" {
		t.Errorf("KitHooksDirs = %v", result.KitHooksDirs)
	}
}

func TestMergeKits_MultipleKits(t *testing.T) {
	base := &model.ProjectMeta{
		ID:   "proj",
		Name: "Project",
		Env:  map[string]string{"PROJ": "yes"},
	}
	m1 := &kit.KitMeta{
		Env:          map[string]string{"A": "from-m1", "SHARED": "m1"},
		HostCommands: map[string]hostcmd.CommandDef{"go": {Path: "/usr/bin/go"}},
	}
	m2 := &kit.KitMeta{
		Env:          map[string]string{"B": "from-m2", "SHARED": "m2"},
		HostCommands: map[string]hostcmd.CommandDef{"go": {Path: "/usr/bin/go"}, "gh": {Path: "/usr/bin/gh"}},
	}

	result := kit.MergeKits(base, []*kit.KitMeta{m1, m2})

	// Env: m1 first, m2 overrides SHARED, project overrides all
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

	// HostCommands: union [go gh]
	if len(result.HostCommands) != 2 {
		t.Errorf("host_commands = %v, want [go gh]", result.HostCommands)
	}

}

func TestMergeKits_HookIDCollision(t *testing.T) {
	base := &model.ProjectMeta{
		ID:   "proj",
		Name: "Project",
		Hooks: []model.Hook{
			{ID: "build", On: "executing", ScriptPath: "/proj/hooks/build.sh"},
		},
	}
	m := &kit.KitMeta{
		Hooks: []model.Hook{
			{ID: "build", On: "executing", ScriptPath: "/kit/hooks/build.sh"},
		},
		HooksDir: "/kit/hooks",
	}

	result := kit.MergeKits(base, []*kit.KitMeta{m})

	// Same hook ID: project wins (last wins)
	if len(result.Hooks) != 1 {
		t.Fatalf("hooks count = %d, want 1 (dedup by ID)", len(result.Hooks))
	}
	if result.Hooks[0].ScriptPath != "/proj/hooks/build.sh" {
		t.Errorf("hook ScriptPath = %q, want project version", result.Hooks[0].ScriptPath)
	}
}
