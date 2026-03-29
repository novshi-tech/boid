package model_test

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/hostcmd"
	"github.com/novshi-tech/boid/internal/model"
	"gopkg.in/yaml.v3"
)

func TestProjectMeta_YAMLUnmarshal(t *testing.T) {
	data := `
id: proj-1
workspace_id: ws-1
name: My Project
task_behaviors:
  dev:
    name: development
    transition: one-shot
    traits:
      - agent_prompt
      - pr
hooks:
  - id: run-agent
    on: executing
    requires_traits:
      - agent_prompt
host_commands:
  git:
    path: /usr/bin/git
  make:
    path: /usr/bin/make
env:
  FOO: bar
allowed_domains:
  - example.com
`
	var meta model.ProjectMeta
	if err := yaml.Unmarshal([]byte(data), &meta); err != nil {
		t.Fatalf("unmarshal yaml: %v", err)
	}

	if meta.ID != "proj-1" {
		t.Fatalf("expected id proj-1, got %s", meta.ID)
	}
	if meta.WorkspaceID != "ws-1" {
		t.Fatalf("expected workspace_id ws-1, got %s", meta.WorkspaceID)
	}
	if meta.Name != "My Project" {
		t.Fatalf("expected name My Project, got %s", meta.Name)
	}

	b, ok := meta.TaskBehaviors["dev"]
	if !ok {
		t.Fatal("expected task_behavior 'dev'")
	}
	if b.Transition != "one-shot" {
		t.Fatalf("expected transition one-shot, got %s", b.Transition)
	}
	if len(b.Traits) != 2 {
		t.Fatalf("expected 2 traits, got %d", len(b.Traits))
	}

	if len(meta.Hooks) != 1 {
		t.Fatalf("expected 1 hook, got %d", len(meta.Hooks))
	}
	if meta.Hooks[0].ID != "run-agent" {
		t.Fatalf("expected hook id run-agent, got %s", meta.Hooks[0].ID)
	}
	if meta.Hooks[0].On != "executing" {
		t.Fatalf("expected hook on executing, got %s", meta.Hooks[0].On)
	}
	if len(meta.Hooks[0].RequiresTraits) != 1 || meta.Hooks[0].RequiresTraits[0] != model.TraitAgentPrompt {
		t.Fatalf("unexpected requires_traits: %v", meta.Hooks[0].RequiresTraits)
	}

	if len(meta.HostCommands) != 2 {
		t.Fatalf("expected 2 host_commands, got %d", len(meta.HostCommands))
	}
	if meta.Env["FOO"] != "bar" {
		t.Fatalf("expected env FOO=bar, got %s", meta.Env["FOO"])
	}
	if len(meta.AllowedDomains) != 1 || meta.AllowedDomains[0] != "example.com" {
		t.Fatalf("unexpected allowed_domains: %v", meta.AllowedDomains)
	}
}

func TestProjectMeta_JSONRoundTrip(t *testing.T) {
	original := model.ProjectMeta{
		ID:          "proj-1",
		WorkspaceID: "ws-1",
		Name:        "Test Project",
		TaskBehaviors: map[string]model.TaskBehavior{
			"dev": {
				Name:       "development",
				Transition: "one-shot",
				Traits:     []string{"agent_prompt"},
			},
		},
		Hooks: []model.Hook{
			{
				ID:             "hook-1",
				On:             "executing",
				RequiresTraits: []model.TraitType{model.TraitAgentPrompt},
			},
		},
		HostCommands: map[string]hostcmd.CommandDef{"git": {Path: "/usr/bin/git"}},
		Env:          map[string]string{"KEY": "val"},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded model.ProjectMeta
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.ID != original.ID {
		t.Fatalf("expected id %s, got %s", original.ID, decoded.ID)
	}
	if decoded.Name != original.Name {
		t.Fatalf("expected name %s, got %s", original.Name, decoded.Name)
	}
	if len(decoded.TaskBehaviors) != 1 {
		t.Fatalf("expected 1 task_behavior, got %d", len(decoded.TaskBehaviors))
	}
	if decoded.TaskBehaviors["dev"].Transition != "one-shot" {
		t.Fatalf("expected transition one-shot, got %s", decoded.TaskBehaviors["dev"].Transition)
	}
	if len(decoded.Hooks) != 1 {
		t.Fatalf("expected 1 hook, got %d", len(decoded.Hooks))
	}
	if decoded.Hooks[0].ID != "hook-1" {
		t.Fatalf("expected hook id hook-1, got %s", decoded.Hooks[0].ID)
	}
}
