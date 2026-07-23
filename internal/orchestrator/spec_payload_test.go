package orchestrator_test

import (
	"encoding/json"
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
			"dev": {Traits: []string{"artifact"}},
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
	if len(decoded.TaskBehaviors["dev"].Traits) == 0 || decoded.TaskBehaviors["dev"].Traits[0] != "artifact" {
		t.Fatalf("unexpected decoded: %+v", decoded.TaskBehaviors["dev"])
	}
}

func TestRoleConstants(t *testing.T) {
	if projectspec.RoleHook != "hook" {
		t.Fatalf("unexpected role: %q", projectspec.RoleHook)
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

// TestMergePayloadPatch_ArtifactClaudeCodeSessions_ConcurrentHooksBothSurvive
// reproduces PR #821 codex review Blocker 1 end to end through the exact
// sequence api.TaskAppService.UpdateTaskPayloadPatch drives (GetTask ->
// MergePayloadPatch -> UpdateTask, serialized per task by
// payloadPatchLockFor): two claude hooks dispatched in parallel within the
// same readonly task round each read the (still-empty) prior sessions list
// before either one has applied its own patch, so hook B's patch body never
// mentions hook A's session id. Before the fix, applying hook B's patch
// second replaced `artifact.claude_code` wholesale (mergeObjectsShallow
// operates one level up, at the `artifact` sub-key), silently discarding
// hook A's session id even though hook A's write had already landed. After
// the fix, mergeArtifactPatch recurses into claude_code.sessions and unions
// by (type, name) instead, so both survive.
func TestMergePayloadPatch_ArtifactClaudeCodeSessions_ConcurrentHooksBothSurvive(t *testing.T) {
	allowed := []projectspec.TraitType{projectspec.TraitArtifact}

	// Hook A's RPC lands first: base is still `{}` at the time A read prior
	// sessions, so A's patch is a single-entry list containing only its own
	// session id.
	patchA := json.RawMessage(`{"artifact":{"claude_code":{"sessions":[{"type":"execution","name":"hook-a","id":"sess-a"}]}}}`)
	afterA, err := projectspec.MergePayloadPatch(json.RawMessage(`{}`), patchA, "hook-a", allowed)
	if err != nil {
		t.Fatalf("MergePayloadPatch (hook-a): %v", err)
	}

	// Hook B's RPC lands second, but B computed its own patch from a read
	// that happened BEFORE A's write landed — so B's patch also only
	// mentions its own session id, not A's. This is the stale-read shape the
	// real concurrent-hook race produces.
	patchB := json.RawMessage(`{"artifact":{"claude_code":{"sessions":[{"type":"execution","name":"hook-b","id":"sess-b"}]}}}`)
	afterB, err := projectspec.MergePayloadPatch(afterA, patchB, "hook-b", allowed)
	if err != nil {
		t.Fatalf("MergePayloadPatch (hook-b): %v", err)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(afterB, &payload); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var artifact struct {
		ClaudeCode struct {
			Sessions []struct {
				Type string `json:"type"`
				Name string `json:"name"`
				ID   string `json:"id"`
			} `json:"sessions"`
		} `json:"claude_code"`
	}
	if err := json.Unmarshal(payload["artifact"], &artifact); err != nil {
		t.Fatalf("unmarshal artifact: %v", err)
	}

	byName := map[string]string{}
	for _, s := range artifact.ClaudeCode.Sessions {
		byName[s.Name] = s.ID
	}
	if byName["hook-a"] != "sess-a" {
		t.Errorf("hook-a's session id was lost; got sessions=%+v", artifact.ClaudeCode.Sessions)
	}
	if byName["hook-b"] != "sess-b" {
		t.Errorf("hook-b's session id was lost; got sessions=%+v", artifact.ClaudeCode.Sessions)
	}
	if len(artifact.ClaudeCode.Sessions) != 2 {
		t.Errorf("expected exactly 2 session entries, got %d: %+v", len(artifact.ClaudeCode.Sessions), artifact.ClaudeCode.Sessions)
	}
}

// TestMergePayloadPatch_ArtifactClaudeCodeSessions_SameKeyLastWriteWins
// covers the genuine-collision case mergeClaudeSessions cannot union away:
// two writes to the SAME (type, name) slot (e.g. a hook re-running under the
// same InvokedName). Since there is no way to merge two different ids for
// one logical session slot, the later-applied patch deterministically wins
// — mirroring ordinary exclusive-trait overwrite semantics — rather than
// silently duplicating the entry or corrupting the array.
func TestMergePayloadPatch_ArtifactClaudeCodeSessions_SameKeyLastWriteWins(t *testing.T) {
	allowed := []projectspec.TraitType{projectspec.TraitArtifact}

	base := json.RawMessage(`{"artifact":{"claude_code":{"sessions":[{"type":"execution","name":"hook-a","id":"sess-old"}]}}}`)
	patch := json.RawMessage(`{"artifact":{"claude_code":{"sessions":[{"type":"execution","name":"hook-a","id":"sess-new"}]}}}`)
	result, err := projectspec.MergePayloadPatch(base, patch, "hook-a", allowed)
	if err != nil {
		t.Fatalf("MergePayloadPatch: %v", err)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var artifact struct {
		ClaudeCode struct {
			Sessions []struct {
				Type string `json:"type"`
				Name string `json:"name"`
				ID   string `json:"id"`
			} `json:"sessions"`
		} `json:"claude_code"`
	}
	if err := json.Unmarshal(payload["artifact"], &artifact); err != nil {
		t.Fatalf("unmarshal artifact: %v", err)
	}
	if len(artifact.ClaudeCode.Sessions) != 1 {
		t.Fatalf("same-key write must not duplicate the entry, got %+v", artifact.ClaudeCode.Sessions)
	}
	if artifact.ClaudeCode.Sessions[0].ID != "sess-new" {
		t.Errorf("later patch must win on a matching (type, name) key; got id=%q", artifact.ClaudeCode.Sessions[0].ID)
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
		payload := json.RawMessage(`{"artifact":"url","instructions":{"r":{"type":"execution","agent":"cc","message":"m"}}}`)
		result := projectspec.FilterPayloadByTraits(payload, nil)
		if string(result) != `{}` {
			t.Fatalf("expected {}, got %s", result)
		}
	})
	t.Run("filters to requested traits only", func(t *testing.T) {
		payload := json.RawMessage(`{"artifact":"url","instructions":{"r":{"type":"execution","agent":"cc","message":"m"}},"tasks":[]}`)
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

// TestMergePayload_DeepMergesObjectSubkeys は executor が書いた artifact.report と
// runner が書いた artifact.claude_code が MergePayload 後に共存できることを確認する。
// これが退行の直接テスト: 修正前は artifact 丸ごと上書きされ report が消える。
func TestMergePayload_DeepMergesObjectSubkeys(t *testing.T) {
	base := json.RawMessage(`{"artifact":{"report":{"summary":"impl done"}}}`)
	update := json.RawMessage(`{"artifact":{"claude_code":{"sessions":[{"id":"s1"}]}}}`)
	result, err := projectspec.MergePayload(base, update)
	if err != nil {
		t.Fatalf("MergePayload: %v", err)
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(result, &merged); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var artifact map[string]json.RawMessage
	if err := json.Unmarshal(merged["artifact"], &artifact); err != nil {
		t.Fatalf("unmarshal artifact: %v", err)
	}
	if _, ok := artifact["report"]; !ok {
		t.Errorf("base sub-key report must be preserved; got artifact=%v", artifact)
	}
	if _, ok := artifact["claude_code"]; !ok {
		t.Errorf("update sub-key claude_code must be merged in; got artifact=%v", artifact)
	}
}

// TestMergePayload_OverwritesWhenNotBothObjects は base/update の一方が object でない
// ときに従来通り whole-value 上書きになることを確認する。
func TestMergePayload_OverwritesWhenNotBothObjects(t *testing.T) {
	// scalar base → object update: 上書き
	r1, err := projectspec.MergePayload(
		json.RawMessage(`{"artifact":"old"}`),
		json.RawMessage(`{"artifact":{"k":"v"}}`),
	)
	if err != nil {
		t.Fatalf("scalar->object: %v", err)
	}
	var m1 map[string]json.RawMessage
	if err := json.Unmarshal(r1, &m1); err != nil {
		t.Fatalf("unmarshal r1: %v", err)
	}
	if string(m1["artifact"]) != `{"k":"v"}` {
		t.Errorf("scalar base must be overwritten by object update: got %s", m1["artifact"])
	}

	// object base → scalar update: 上書き
	r2, err := projectspec.MergePayload(
		json.RawMessage(`{"artifact":{"k":"v"}}`),
		json.RawMessage(`{"artifact":"new"}`),
	)
	if err != nil {
		t.Fatalf("object->scalar: %v", err)
	}
	var m2 map[string]json.RawMessage
	if err := json.Unmarshal(r2, &m2); err != nil {
		t.Fatalf("unmarshal r2: %v", err)
	}
	if string(m2["artifact"]) != `"new"` {
		t.Errorf("scalar update must overwrite object base: got %s", m2["artifact"])
	}
}

// TestMergePayload_DeleteWithNull は update の null 値に対して既存挙動 (base を変更しない)
// が維持されることを確認する。
func TestMergePayload_DeleteWithNull(t *testing.T) {
	base := json.RawMessage(`{"a":"keep","b":"also-keep"}`)
	update := json.RawMessage(`{"a":null,"c":"new"}`)
	result, err := projectspec.MergePayload(base, update)
	if err != nil {
		t.Fatalf("MergePayload: %v", err)
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(result, &merged); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(merged["a"]) != `"keep"` {
		t.Errorf("null update should leave base value unchanged; got a=%s", merged["a"])
	}
	if string(merged["b"]) != `"also-keep"` {
		t.Errorf("untouched key b should remain; got b=%s", merged["b"])
	}
	if string(merged["c"]) != `"new"` {
		t.Errorf("new key c should be set; got c=%s", merged["c"])
	}
}
