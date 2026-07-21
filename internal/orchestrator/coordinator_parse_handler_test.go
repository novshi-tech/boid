package orchestrator

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseHandlerResult_StringKeys(t *testing.T) {
	out := `payload_patch:
  artifact:
    summary: ok
    files:
    - foo.go
    - bar.go
`
	hr := parseHandlerResult("h1", RoleHook, JobCompletion{Output: out, ExitCode: 0})
	if len(hr.PayloadPatch) == 0 {
		t.Fatal("PayloadPatch is empty")
	}
	var got map[string]interface{}
	if err := json.Unmarshal(hr.PayloadPatch, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	art, _ := got["artifact"].(map[string]interface{})
	if art["summary"] != "ok" {
		t.Errorf("artifact.summary = %v, want ok", art["summary"])
	}
}

func TestParseHandlerResult_BoolKeyCoerced(t *testing.T) {
	// agent が `on: verifying` を書いて round-trip で `true: verifying` に化けたケースの
	// 再現。yaml.v3 は非 string キーを含む内側 map を map[interface{}]interface{} で
	// decode し、そのままでは json.Marshal で死ぬ。normalize で吸収する。
	out := `payload_patch:
  artifact:
    gates:
    - id: mergeable-check
      true: verifying
      phase: exit
`
	hr := parseHandlerResult("h1", RoleHook, JobCompletion{Output: out, ExitCode: 0})
	if len(hr.PayloadPatch) == 0 {
		t.Fatal("PayloadPatch dropped (silent fail). Layer 2 normalize is missing.")
	}
	var got map[string]interface{}
	if err := json.Unmarshal(hr.PayloadPatch, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	art, _ := got["artifact"].(map[string]interface{})
	gates, _ := art["gates"].([]interface{})
	if len(gates) != 1 {
		t.Fatalf("gates len = %d, want 1", len(gates))
	}
	g0 := gates[0].(map[string]interface{})
	if g0["id"] != "mergeable-check" {
		t.Errorf("gates[0].id = %v, want mergeable-check", g0["id"])
	}
	// bool key true was coerced to string "true"
	if g0["true"] != "verifying" {
		t.Errorf("gates[0].true (coerced) = %v, want verifying", g0["true"])
	}
}

func TestParseHandlerResult_IntKeyCoerced(t *testing.T) {
	// kit が python dict で int キーを yaml.dump したケース
	out := `payload_patch:
  artifact:
    fixes_by_line:
      42: off-by-one
      108: null check
`
	hr := parseHandlerResult("h1", RoleHook, JobCompletion{Output: out, ExitCode: 0})
	if len(hr.PayloadPatch) == 0 {
		t.Fatal("PayloadPatch dropped for int keys")
	}
	var got map[string]interface{}
	if err := json.Unmarshal(hr.PayloadPatch, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	art, _ := got["artifact"].(map[string]interface{})
	fixes, _ := art["fixes_by_line"].(map[string]interface{})
	if fixes["42"] != "off-by-one" {
		t.Errorf("fixes_by_line.42 = %v, want off-by-one", fixes["42"])
	}
	if fixes["108"] != "null check" {
		t.Errorf("fixes_by_line.108 = %v, want 'null check'", fixes["108"])
	}
}

func TestParseHandlerResult_NullKeyCoerced(t *testing.T) {
	// `~: foo` (null key) も coerce 対象
	out := `payload_patch:
  artifact:
    weird:
      ~: foo
      bar: baz
`
	hr := parseHandlerResult("h1", RoleHook, JobCompletion{Output: out, ExitCode: 0})
	if len(hr.PayloadPatch) == 0 {
		t.Fatal("PayloadPatch dropped for null key")
	}
	var got map[string]interface{}
	if err := json.Unmarshal(hr.PayloadPatch, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	art, _ := got["artifact"].(map[string]interface{})
	weird, _ := art["weird"].(map[string]interface{})
	if weird["bar"] != "baz" {
		t.Errorf("weird.bar = %v, want baz", weird["bar"])
	}
	// null key is coerced to "<nil>" (fmt.Sprint(nil)) — content preserved, not dropped
	if _, ok := weird["<nil>"]; !ok {
		// 受け入れ可能な代替表現としては "" や "null" もある。少なくとも消えていないことを確認。
		hasAlt := weird["null"] != nil || weird[""] != nil
		if !hasAlt {
			t.Errorf("null key was dropped; weird = %v", weird)
		}
	}
}

func TestParseHandlerResult_AcceptsJSON(t *testing.T) {
	// 移行後の JSON 出力が壊れずに通ることを確認
	out := `{"payload_patch":{"artifact":{"summary":"ok","files":["a","b"]}}}`
	hr := parseHandlerResult("h1", RoleHook, JobCompletion{Output: out, ExitCode: 0})
	if len(hr.PayloadPatch) == 0 {
		t.Fatal("PayloadPatch is empty for JSON input")
	}
	if !strings.Contains(string(hr.PayloadPatch), `"summary":"ok"`) {
		t.Errorf("PayloadPatch = %s, want summary:ok", hr.PayloadPatch)
	}
}

func TestParseHandlerResult_NoPayloadPatchKey(t *testing.T) {
	// payload_patch キーが無い → 空 PayloadPatch (silent ok)
	out := `something_else:
  foo: bar
`
	hr := parseHandlerResult("h1", RoleHook, JobCompletion{Output: out, ExitCode: 0})
	if len(hr.PayloadPatch) != 0 {
		t.Errorf("PayloadPatch should be empty, got %s", hr.PayloadPatch)
	}
}

func TestParseHandlerResult_EmptyOutput(t *testing.T) {
	hr := parseHandlerResult("h1", RoleHook, JobCompletion{Output: "", ExitCode: 0})
	if len(hr.PayloadPatch) != 0 {
		t.Errorf("PayloadPatch should be empty for empty output")
	}
}

// The pure recursive-normalization logic itself (non-string key
// stringification, nested map/slice traversal) is now covered directly by
// internal/yamlutil's own tests (Phase 5b PR7 codex review Major 2 fix,
// wiring-seams.md #17: normalizeYAMLKeys moved to the shared
// internal/yamlutil.NormalizeKeys so internal/sandbox's `--payload-patch`
// CLI can apply the identical normalization without orchestrator becoming a
// dependency of sandbox). TestParseHandlerResult_BoolKeyCoerced /
// _IntKeyCoerced / _NullKeyCoerced above pin the integration behavior
// (parseHandlerResult actually applying it to a real payload_patch output).
