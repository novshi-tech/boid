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
    traits:
      - instructions
      - artifact
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
	if meta.ID != "proj-1" || meta.Env["FOO"] != "bar" {
		t.Fatalf("unexpected meta: %+v", meta)
	}
}

func TestProjectMeta_JSONRoundTrip(t *testing.T) {
	original := projectspec.ProjectMeta{
		ID:   "proj-1",
		Name: "Test Project",
		TaskBehaviors: map[string]projectspec.TaskBehavior{
			"dev": {Name: "development", Traits: []string{"artifact"}},
		},
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
	if decoded.TaskBehaviors["dev"].Name != "development" {
		t.Fatalf("unexpected decoded: %+v", decoded.TaskBehaviors["dev"])
	}
}

func TestGate_YAMLRoundTrip(t *testing.T) {
	data := `
id: push-pr
phase: exit
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

func TestGatePhase_YAMLUnmarshal(t *testing.T) {
	tests := []struct {
		name     string
		yaml     string
		expected projectspec.GatePhase
	}{
		{
			name:     "explicit entry",
			yaml:     "id: g1\nphase: entry\n",
			expected: projectspec.GatePhaseEntry,
		},
		{
			name:     "explicit exit",
			yaml:     "id: g1\nphase: exit\n",
			expected: projectspec.GatePhaseExit,
		},
		{
			name:     "omitted defaults to exit",
			yaml:     "id: g1\nphase: exit\n",
			expected: projectspec.GatePhaseExit,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gate projectspec.Gate
			if err := yaml.Unmarshal([]byte(tt.yaml), &gate); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if gate.Phase != tt.expected {
				t.Fatalf("phase: got %q, want %q", gate.Phase, tt.expected)
			}
		})
	}
}

func TestGatePhase_JSONRoundTrip(t *testing.T) {
	gate := projectspec.Gate{
		ID:    "g1",
		Phase: projectspec.GatePhaseEntry,
	}
	data, err := json.Marshal(gate)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded projectspec.Gate
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Phase != projectspec.GatePhaseEntry {
		t.Fatalf("phase: got %q, want %q", decoded.Phase, projectspec.GatePhaseEntry)
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
	if projectspec.TraitMergeMode(projectspec.TraitArtifact) != projectspec.MergeModeExclusive {
		t.Fatal("artifact should be exclusive")
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

// TestMergePayloadPatch_ExclusiveDeepMergesObjectSubkeys verifies that an
// exclusive trait whose value is an object (e.g. `artifact`) deep-merges
// sub-keys when both base and patch are objects, instead of shallowly
// overwriting the whole value. This protects cross-phase hand-offs:
// a hook's `artifact.claude_code.sessions` must survive an exit gate's
// `artifact.auto-merge.merged` write within the same dispatch cycle.
// Scalar exclusive values (or where one side is non-object) keep the
// existing overwrite semantics.
func TestMergePayloadPatch_ExclusiveDeepMergesObjectSubkeys(t *testing.T) {
	base := json.RawMessage(`{"artifact":{"claude_code":{"sessions":[{"id":"sess-1"}]}}}`)
	patch := json.RawMessage(`{"artifact":{"auto-merge":{"merged":true}}}`)
	allowed := []projectspec.TraitType{projectspec.TraitArtifact}
	result, err := projectspec.MergePayloadPatch(base, patch, "auto-merge", allowed)
	if err != nil {
		t.Fatalf("MergePayloadPatch: %v", err)
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(result, &merged); err != nil {
		t.Fatalf("unmarshal merged: %v", err)
	}
	var artifact map[string]json.RawMessage
	if err := json.Unmarshal(merged["artifact"], &artifact); err != nil {
		t.Fatalf("unmarshal artifact: %v", err)
	}
	if _, ok := artifact["claude_code"]; !ok {
		t.Errorf("base sub-key claude_code must be preserved; got %v", artifact)
	}
	if _, ok := artifact["auto-merge"]; !ok {
		t.Errorf("patch sub-key auto-merge must be merged in; got %v", artifact)
	}
}

// TestMergePayloadPatch_ExclusiveOverwritesWhenNotBothObjects keeps the
// historical "exclusive = overwrite" behavior whenever either side is not
// an object (scalar or array). The deep-merge path is opt-in based purely
// on shape — never inferred from trait identity.
func TestMergePayloadPatch_ExclusiveOverwritesWhenNotBothObjects(t *testing.T) {
	allowed := []projectspec.TraitType{projectspec.TraitArtifact}

	// scalar base, object patch -> overwrite
	r1, err := projectspec.MergePayloadPatch(
		json.RawMessage(`{"artifact":"old"}`),
		json.RawMessage(`{"artifact":{"k":"v"}}`),
		"writer", allowed)
	if err != nil {
		t.Fatalf("scalar->object: %v", err)
	}
	if string(r1) != `{"artifact":{"k":"v"}}` {
		t.Errorf("scalar base must be overwritten by object patch: got %s", r1)
	}

	// object base, scalar patch -> overwrite
	r2, err := projectspec.MergePayloadPatch(
		json.RawMessage(`{"artifact":{"k":"v"}}`),
		json.RawMessage(`{"artifact":"new"}`),
		"writer", allowed)
	if err != nil {
		t.Fatalf("object->scalar: %v", err)
	}
	if string(r2) != `{"artifact":"new"}` {
		t.Errorf("scalar patch must overwrite object base: got %s", r2)
	}
}

func TestMergePayloadPatch_ProducesOutsideAllowed(t *testing.T) {
	patch := json.RawMessage(`{"artifact":"http://example.com"}`)
	// artifact is not in allowed produces
	allowed := []projectspec.TraitType{projectspec.TraitVerification}
	result, err := projectspec.MergePayloadPatch(json.RawMessage(`{}`), patch, "hook-1", allowed)
	if err != nil {
		t.Fatalf("MergePayloadPatch: %v", err)
	}
	if string(result) != `{}` {
		t.Fatalf("disallowed trait should be dropped, got: %s", result)
	}
}

// TestMergePayloadPatch_DropsUnknownTraitsAndMergesAllowed reproduces the silent
// data-loss bug where a single unknown top-level key (e.g. "status": "done")
// caused the whole payload_patch to be rejected, discarding valid traits like
// "artifact" that the agent produced successfully.
func TestMergePayloadPatch_DropsUnknownTraitsAndMergesAllowed(t *testing.T) {
	patch := json.RawMessage(`{"status":"done","artifact":{"commit":"abc1234"}}`)
	allowed := []projectspec.TraitType{projectspec.TraitArtifact}
	result, err := projectspec.MergePayloadPatch(json.RawMessage(`{}`), patch, "hook-1", allowed)
	if err != nil {
		t.Fatalf("MergePayloadPatch: %v", err)
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(result, &merged); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := merged["status"]; ok {
		t.Error("unknown trait \"status\" should have been dropped")
	}
	if got, ok := merged["artifact"]; !ok {
		t.Error("allowed trait \"artifact\" should have been merged")
	} else if string(got) != `{"commit":"abc1234"}` {
		t.Errorf("artifact value mismatch: %s", got)
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

func TestFilterPayloadByTraits_OptionalTraitIncludedWhenPresent(t *testing.T) {
	payload := json.RawMessage(`{
		"artifact":{"summary":"impl"},
		"verification":{"pr":{"findings":[{"message":"fail","status":"open"}]}},
		"tasks":[{"id":"x"}]
	}`)
	result := projectspec.FilterPayloadByTraits(payload, []projectspec.TraitType{
		projectspec.TraitArtifact, "verification?",
	})
	var m map[string]json.RawMessage
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["artifact"]; !ok {
		t.Error("expected artifact")
	}
	if _, ok := m["verification"]; !ok {
		t.Error("expected verification (optional but present)")
	}
	if _, ok := m["tasks"]; ok {
		t.Error("tasks should be filtered out")
	}
}

func TestFilterPayloadByTraits_OptionalTraitOmittedWhenAbsent(t *testing.T) {
	payload := json.RawMessage(`{
		"artifact":{"summary":"impl"}
	}`)
	result := projectspec.FilterPayloadByTraits(payload, []projectspec.TraitType{
		projectspec.TraitArtifact, "verification?",
	})
	var m map[string]json.RawMessage
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["artifact"]; !ok {
		t.Error("expected artifact")
	}
	if _, ok := m["verification"]; ok {
		t.Error("verification should not appear when absent from payload")
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
