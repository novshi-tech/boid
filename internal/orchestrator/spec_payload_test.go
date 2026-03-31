package orchestrator_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
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
      - prompt
      - artifact
hooks:
  - id: run-agent
    on: executing
    requires_traits:
      - prompt
host_commands:
  git:
    path: /usr/bin/git
  make:
    path: /usr/bin/make
env:
  FOO: bar
`
	var meta projectspec.ProjectMeta
	if err := yaml.Unmarshal([]byte(data), &meta); err != nil {
		t.Fatalf("unmarshal yaml: %v", err)
	}
	if len(meta.Hooks) != 1 || meta.Hooks[0].RequiresTraits[0] != projectspec.TraitPrompt {
		t.Fatalf("unexpected requires_traits: %v", meta.Hooks[0].RequiresTraits)
	}
}

func TestProjectMeta_YAMLWithGates(t *testing.T) {
	data := `
id: proj-1
name: My Project
gates:
  - id: push-pr
    on: executing
    requires_traits:
      - artifact
`
	var meta projectspec.ProjectMeta
	if err := yaml.Unmarshal([]byte(data), &meta); err != nil {
		t.Fatalf("unmarshal yaml: %v", err)
	}
	if len(meta.Gates) != 1 || meta.Gates[0].ID != "push-pr" {
		t.Fatalf("unexpected gates: %+v", meta.Gates)
	}
}

func TestProjectMeta_JSONRoundTrip(t *testing.T) {
	original := projectspec.ProjectMeta{
		ID:          "proj-1",
		WorkspaceID: "ws-1",
		Name:        "Test Project",
		TaskBehaviors: map[string]projectspec.TaskBehavior{
			"dev": {Name: "development", Transition: "one-shot", Traits: []string{"prompt"}},
		},
		Hooks:        []projectspec.Hook{{ID: "hook-1", On: "executing", RequiresTraits: []projectspec.TraitType{projectspec.TraitPrompt}}},
		HostCommands: map[string]projectspec.CommandDef{"git": {Path: "/usr/bin/git"}},
		Env:          map[string]string{"KEY": "val"},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded projectspec.ProjectMeta
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Hooks[0].ID != "hook-1" {
		t.Fatalf("unexpected decoded hook: %+v", decoded.Hooks[0])
	}
}

func TestGate_YAMLRoundTrip(t *testing.T) {
	data := `
id: push-pr
on: executing
requires_traits:
  - artifact
`
	var gate projectspec.Gate
	if err := yaml.Unmarshal([]byte(data), &gate); err != nil {
		t.Fatalf("unmarshal yaml: %v", err)
	}
	if len(gate.RequiresTraits) != 1 || gate.RequiresTraits[0] != projectspec.TraitArtifact {
		t.Fatalf("unexpected requires_traits: %v", gate.RequiresTraits)
	}
}

func TestResolveGateScript(t *testing.T) {
	dir := t.TempDir()
	gatesDir := filepath.Join(dir, "gates")
	if err := os.MkdirAll(gatesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	shPath := filepath.Join(gatesDir, "push-pr.sh")
	if err := os.WriteFile(shPath, []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatalf("write sh: %v", err)
	}
	got, err := projectspec.ResolveGateScript(gatesDir, "push-pr")
	if err != nil || got != shPath {
		t.Fatalf("ResolveGateScript() = %q, %v", got, err)
	}
}

func TestRoleConstants(t *testing.T) {
	if projectspec.RoleHook != "hook" || projectspec.RoleGate != "gate" {
		t.Fatalf("unexpected roles: %q %q", projectspec.RoleHook, projectspec.RoleGate)
	}
}

func TestActiveTraitTypes(t *testing.T) {
	raw := json.RawMessage(`{"prompt":"hello","artifact":"x"}`)
	traits, err := projectspec.ActiveTraitTypes(raw)
	if err != nil {
		t.Fatalf("ActiveTraitTypes: %v", err)
	}
	names := make([]string, len(traits))
	for i, trait := range traits {
		names[i] = string(trait)
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "artifact" || names[1] != "prompt" {
		t.Fatalf("unexpected traits: %v", names)
	}
}

func TestMergePayload(t *testing.T) {
	base := json.RawMessage(`{"a":"1","b":"2"}`)
	update := json.RawMessage(`{"b":"3","c":"4"}`)
	result, err := projectspec.MergePayload(base, update)
	if err != nil {
		t.Fatalf("MergePayload: %v", err)
	}
	var merged map[string]string
	if err := json.Unmarshal(result, &merged); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if merged["b"] != "3" || merged["c"] != "4" {
		t.Fatalf("unexpected merge result: %v", merged)
	}
}

func TestTraitMergeMode(t *testing.T) {
	if projectspec.TraitMergeMode(projectspec.TraitVerification) != projectspec.MergeModeShared {
		t.Fatal("verification should be shared")
	}
	if projectspec.TraitMergeMode(projectspec.TraitPrompt) != projectspec.MergeModeExclusive {
		t.Fatal("prompt should be exclusive")
	}
}

func TestValidatePayloadPatchAndMergePayloadPatch(t *testing.T) {
	patch := json.RawMessage(`{"prompt":"hello"}`)
	allowed := []projectspec.TraitType{projectspec.TraitPrompt}
	if err := projectspec.ValidatePayloadPatch(patch, allowed); err != nil {
		t.Fatalf("ValidatePayloadPatch: %v", err)
	}
	result, err := projectspec.MergePayloadPatch(json.RawMessage(`{}`), patch, "hook-1", allowed)
	if err != nil {
		t.Fatalf("MergePayloadPatch: %v", err)
	}
	if string(result) != `{"prompt":"hello"}` {
		t.Fatalf("unexpected merged payload: %s", result)
	}
}

func TestMergePayloadPatch_Shared(t *testing.T) {
	base := json.RawMessage(`{}`)
	allowed := []projectspec.TraitType{projectspec.TraitVerification}
	patch1 := json.RawMessage(`{"verification":{"findings":[{"message":"secure","status":"resolved"}]}}`)
	result, err := projectspec.MergePayloadPatch(base, patch1, "security-review", allowed)
	if err != nil {
		t.Fatalf("MergePayloadPatch 1: %v", err)
	}
	patch2 := json.RawMessage(`{"verification":{"findings":[{"message":"bug","status":"open"}]}}`)
	result, err = projectspec.MergePayloadPatch(result, patch2, "quality-review", allowed)
	if err != nil {
		t.Fatalf("MergePayloadPatch 2: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var verification map[string]json.RawMessage
	if err := json.Unmarshal(payload["verification"], &verification); err != nil {
		t.Fatalf("unmarshal verification: %v", err)
	}
	if verification["security-review"] == nil || verification["quality-review"] == nil {
		t.Fatalf("unexpected verification payload: %v", verification)
	}
}
