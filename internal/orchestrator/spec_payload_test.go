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
name: My Project
task_behaviors:
  dev:
    name: development
    transition: one-shot
    traits:
      - instructions
      - artifact
hooks:
  - id: run-agent
    on: executing
    traits:
      consumes:
        - instructions
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
	if len(meta.Hooks) != 1 || meta.Hooks[0].Traits.Consumes[0] != projectspec.TraitInstructions {
		t.Fatalf("unexpected traits.consumes: %v", meta.Hooks[0].Traits.Consumes)
	}
}

func TestProjectMeta_YAMLWithGates(t *testing.T) {
	data := `
id: proj-1
name: My Project
gates:
  - id: push-pr
    on: executing
    traits:
      consumes:
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
		ID:   "proj-1",
		Name: "Test Project",
		TaskBehaviors: map[string]projectspec.TaskBehavior{
			"dev": {Name: "development", Transition: "one-shot", Traits: []string{"instructions"}},
		},
		Hooks:        []projectspec.Hook{{ID: "hook-1", On: projectspec.OnValues{"executing"}, Traits: projectspec.HandlerTraits{Consumes: []projectspec.TraitType{projectspec.TraitInstructions}}}},
		HostCommands: projectspec.HostCommands{"git": {Path: "/usr/bin/git"}},
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
traits:
  consumes:
    - artifact
`
	var gate projectspec.Gate
	if err := yaml.Unmarshal([]byte(data), &gate); err != nil {
		t.Fatalf("unmarshal yaml: %v", err)
	}
	if len(gate.Traits.Consumes) != 1 || gate.Traits.Consumes[0] != projectspec.TraitArtifact {
		t.Fatalf("unexpected traits.consumes: %v", gate.Traits.Consumes)
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
	if projectspec.TraitMergeMode(projectspec.TraitInstructions) != projectspec.MergeModeExclusive {
		t.Fatal("instructions should be exclusive")
	}
}

func TestValidatePayloadPatchAndMergePayloadPatch(t *testing.T) {
	patch := json.RawMessage(`{"artifact":"http://example.com"}`)
	allowed := []projectspec.TraitType{projectspec.TraitArtifact}
	if err := projectspec.ValidatePayloadPatch(patch, allowed); err != nil {
		t.Fatalf("ValidatePayloadPatch: %v", err)
	}
	result, err := projectspec.MergePayloadPatch(json.RawMessage(`{}`), patch, "hook-1", allowed)
	if err != nil {
		t.Fatalf("MergePayloadPatch: %v", err)
	}
	if string(result) != `{"artifact":"http://example.com"}` {
		t.Fatalf("unexpected merged payload: %s", result)
	}
}

func TestValidatePayloadPatch_InstructionsRejected(t *testing.T) {
	patch := json.RawMessage(`{"instructions":"do something"}`)
	// Even if instructions is listed in allowed produces, handlers must not write it.
	allowed := []projectspec.TraitType{projectspec.TraitInstructions}
	err := projectspec.ValidatePayloadPatch(patch, allowed)
	if err == nil {
		t.Fatal("expected error when instructions trait is in patch")
	}
}

func TestMergePayloadPatch_ProducesOutsideAllowed(t *testing.T) {
	patch := json.RawMessage(`{"artifact":"http://example.com"}`)
	// artifact is not in allowed produces
	allowed := []projectspec.TraitType{projectspec.TraitVerification}
	_, err := projectspec.MergePayloadPatch(json.RawMessage(`{}`), patch, "hook-1", allowed)
	if err == nil {
		t.Fatal("expected error when patch trait is not in produces")
	}
}

func TestFilterPayloadByTraits(t *testing.T) {
	t.Run("empty consumes returns empty payload", func(t *testing.T) {
		payload := json.RawMessage(`{"artifact":"url","instructions":{"r":{"type":"execution","consumer":"cc","message":"m"}}}`)
		result := projectspec.FilterPayloadByTraits(payload, nil)
		if string(result) != `{}` {
			t.Fatalf("expected {}, got %s", result)
		}
	})
	t.Run("filters to requested traits only", func(t *testing.T) {
		payload := json.RawMessage(`{"artifact":"url","instructions":{"r":{"type":"execution","consumer":"cc","message":"m"}},"tasks":[]}`)
		result := projectspec.FilterPayloadByTraits(payload, []projectspec.TraitType{projectspec.TraitArtifact})
		var m map[string]json.RawMessage
		if err := json.Unmarshal(result, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, ok := m["artifact"]; !ok {
			t.Error("expected artifact key")
		}
		if _, ok := m["instructions"]; ok {
			t.Error("unexpected instructions key")
		}
		if _, ok := m["tasks"]; ok {
			t.Error("unexpected tasks key")
		}
	})
	t.Run("missing trait in payload is omitted silently", func(t *testing.T) {
		payload := json.RawMessage(`{"artifact":"url"}`)
		result := projectspec.FilterPayloadByTraits(payload, []projectspec.TraitType{projectspec.TraitArtifact, projectspec.TraitVerification})
		var m map[string]json.RawMessage
		if err := json.Unmarshal(result, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, ok := m["artifact"]; !ok {
			t.Error("expected artifact key")
		}
		if len(m) != 1 {
			t.Errorf("expected 1 key, got %d", len(m))
		}
	})
	t.Run("empty payload returns empty payload", func(t *testing.T) {
		result := projectspec.FilterPayloadByTraits(json.RawMessage("{}"), []projectspec.TraitType{projectspec.TraitArtifact})
		if string(result) != `{}` {
			t.Fatalf("expected {}, got %s", result)
		}
	})
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
