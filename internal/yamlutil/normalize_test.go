package yamlutil_test

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/yamlutil"
	"gopkg.in/yaml.v3"
)

// Phase 5b PR7 codex review Major 2 (docs/plans/phase5-shim-and-task-context.md):
// yaml.v3 decodes a YAML mapping with a non-string key (bool/int/null) as
// map[interface{}]interface{} instead of map[string]interface{}, which
// json.Marshal cannot handle. NormalizeKeys recursively stringifies such
// keys so any yaml.Unmarshal(..., &any{}) result is always JSON-marshalable.
// This must be a single shared implementation: before this PR,
// orchestrator's file-based payload_patch.json ingestion
// (parseHandlerResult) had this fix but internal/sandbox's CLI
// (--payload-patch) did not, so the same YAML content behaved differently
// depending on which path carried it (see wiring-seams.md #17's Major 2
// finding — a real historical incident: "agent が `on: verifying` と書いた
// YAML が PyYAML の round-trip で `true: verifying` に化けた").

func TestNormalizeKeys_StringifiesNonStringKeys(t *testing.T) {
	// A YAML document whose inner mapping has a bool key ("true:"), the
	// exact PyYAML round-trip incident coordinator.go's own doc comment
	// references.
	src := "artifact:\n  report:\n    true: verifying\n    42: also\n"
	var v any
	if err := yaml.Unmarshal([]byte(src), &v); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}

	// Without normalization this must fail to marshal (pins the failure
	// mode NormalizeKeys exists to fix, so the test itself would catch a
	// no-op stub).
	if _, err := json.Marshal(v); err == nil {
		t.Fatalf("expected raw yaml.Unmarshal result to be unmarshalable by json.Marshal without normalization")
	}

	normalized := yamlutil.NormalizeKeys(v)
	out, err := json.Marshal(normalized)
	if err != nil {
		t.Fatalf("json.Marshal(NormalizeKeys(v)): %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	var artifact struct {
		Report map[string]string `json:"report"`
	}
	if err := json.Unmarshal(decoded["artifact"], &artifact); err != nil {
		t.Fatalf("decode artifact: %v", err)
	}
	if artifact.Report["true"] != "verifying" || artifact.Report["42"] != "also" {
		t.Fatalf("unexpected normalized shape: %+v", artifact.Report)
	}
}

func TestNormalizeKeys_PreservesStringKeyedMapsAndSlices(t *testing.T) {
	src := "a:\n  - b: 1\n    c: 2\n  - b: 3\n    c: 4\n"
	var v any
	if err := yaml.Unmarshal([]byte(src), &v); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	normalized := yamlutil.NormalizeKeys(v)
	out, err := json.Marshal(normalized)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	want := `{"a":[{"b":1,"c":2},{"b":3,"c":4}]}`
	if string(out) != want {
		t.Fatalf("got %s, want %s", out, want)
	}
}

func TestNormalizeKeys_ScalarPassthrough(t *testing.T) {
	for _, v := range []any{"s", 1, true, nil} {
		if got := yamlutil.NormalizeKeys(v); got != nil && v == nil {
			t.Fatalf("expected nil passthrough, got %v", got)
		}
	}
}
