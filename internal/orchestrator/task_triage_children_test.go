package orchestrator_test

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestDetailChildren_EmptyOrAbsent covers the "no children yet" cases PR-2's
// Dispatch relies on to treat a bare/empty detail blob as "0 specced
// children" rather than an error.
func TestDetailChildren_EmptyOrAbsent(t *testing.T) {
	for _, detail := range []json.RawMessage{nil, []byte(""), []byte("null"), []byte("{}"), []byte(`{"summary":"x"}`)} {
		got, err := orchestrator.DetailChildren(detail)
		if err != nil {
			t.Fatalf("DetailChildren(%q): unexpected error: %v", detail, err)
		}
		if len(got) != 0 {
			t.Fatalf("DetailChildren(%q) = %+v, want empty", detail, got)
		}
	}
}

func TestDetailChildren_ParsesChildren(t *testing.T) {
	detail := json.RawMessage(`{
		"summary": "keep me",
		"children": [
			{"id": "ch_00", "title": "do the thing", "status": "specced", "spec": {"project": "proj-1", "behavior": "impl", "instruction": "do it"}},
			{"id": "ch_01", "title": "vague idea", "status": "open"}
		]
	}`)
	got, err := orchestrator.DetailChildren(detail)
	if err != nil {
		t.Fatalf("DetailChildren: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(children) = %d, want 2", len(got))
	}
	if got[0].ID != "ch_00" || got[0].Status != orchestrator.TaskTriageChildStatusSpecced {
		t.Fatalf("children[0] = %+v", got[0])
	}
	if got[0].Spec == nil || got[0].Spec.Project != "proj-1" || got[0].Spec.Behavior != "impl" || got[0].Spec.Instruction != "do it" {
		t.Fatalf("children[0].Spec = %+v", got[0].Spec)
	}
	if got[1].ID != "ch_01" || got[1].Status != orchestrator.TaskTriageChildStatusOpen || got[1].Spec != nil {
		t.Fatalf("children[1] = %+v", got[1])
	}
}

// TestSetDetailChildren_PreservesOtherKeys is the regression test for the
// "detail is a schema-light JSON blob, not a fixed Go struct" design (実測c,
// 逆輸入1/3): replacing children must not silently drop summary/source/
// content_ref/suggestion/observed or any other key PR-2's code doesn't know
// about.
func TestSetDetailChildren_PreservesOtherKeys(t *testing.T) {
	detail := json.RawMessage(`{"summary":"keep me","source":{"type":"mail","ref":"msg-1"},"suggestion":{"verb":"go"}}`)
	children := []orchestrator.TaskTriageChild{
		{ID: "ch_00", Status: orchestrator.TaskTriageChildStatusDispatched, TaskRef: "task-99"},
	}
	out, err := orchestrator.SetDetailChildren(detail, children)
	if err != nil {
		t.Fatalf("SetDetailChildren: %v", err)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if string(m["summary"]) != `"keep me"` {
		t.Fatalf("summary dropped: %s", m["summary"])
	}
	if string(m["source"]) != `{"type":"mail","ref":"msg-1"}` {
		t.Fatalf("source dropped: %s", m["source"])
	}
	if string(m["suggestion"]) != `{"verb":"go"}` {
		t.Fatalf("suggestion dropped: %s", m["suggestion"])
	}

	roundTripped, err := orchestrator.DetailChildren(out)
	if err != nil {
		t.Fatalf("DetailChildren(round-trip): %v", err)
	}
	if len(roundTripped) != 1 || roundTripped[0].Status != orchestrator.TaskTriageChildStatusDispatched || roundTripped[0].TaskRef != "task-99" {
		t.Fatalf("round-tripped children = %+v", roundTripped)
	}
}

// TestSetDetailChildren_EmptyDetail covers the first-write case (no prior
// detail at all) — must not error and must produce a valid JSON object with
// only "children" set.
func TestSetDetailChildren_EmptyDetail(t *testing.T) {
	out, err := orchestrator.SetDetailChildren(nil, []orchestrator.TaskTriageChild{{ID: "ch_00", Status: orchestrator.TaskTriageChildStatusOpen}})
	if err != nil {
		t.Fatalf("SetDetailChildren: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := m["children"]; !ok {
		t.Fatalf("children key missing from %s", out)
	}
}
