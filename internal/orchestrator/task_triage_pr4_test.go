package orchestrator_test

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestFoldDetailAttrs_LastWriteWinsAndPreservesOtherKeys pins the "attrs_set"
// side-effect's fold semantics (docs/plans/cross-project-issue-triage.md
// PR-4 設計メモ 論点4/6): a pure last-write-wins merge into "attrs", leaving
// every other top-level key (children, summary) untouched.
func TestFoldDetailAttrs_LastWriteWinsAndPreservesOtherKeys(t *testing.T) {
	detail := json.RawMessage(`{"summary":"keep me","attrs":{"urgency":"today","note":"old"}}`)
	patch := map[string]json.RawMessage{
		"urgency": json.RawMessage(`"now"`),
		"new_key": json.RawMessage(`"v"`),
	}
	out, err := orchestrator.FoldDetailAttrs(detail, patch)
	if err != nil {
		t.Fatalf("FoldDetailAttrs: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if string(m["summary"]) != `"keep me"` {
		t.Fatalf("summary not preserved: %s", m["summary"])
	}
	var attrs map[string]string
	if err := json.Unmarshal(m["attrs"], &attrs); err != nil {
		t.Fatalf("unmarshal attrs: %v", err)
	}
	if attrs["urgency"] != "now" {
		t.Fatalf("urgency = %q, want last-write-wins %q", attrs["urgency"], "now")
	}
	if attrs["note"] != "old" {
		t.Fatalf("note (untouched key) = %q, want preserved %q", attrs["note"], "old")
	}
	if attrs["new_key"] != "v" {
		t.Fatalf("new_key = %q, want %q", attrs["new_key"], "v")
	}
}

func TestFoldDetailAttrs_EmptyDetail(t *testing.T) {
	out, err := orchestrator.FoldDetailAttrs(nil, map[string]json.RawMessage{"k": json.RawMessage(`"v"`)})
	if err != nil {
		t.Fatalf("FoldDetailAttrs: %v", err)
	}
	children, err := orchestrator.DetailChildren(out)
	if err != nil || len(children) != 0 {
		t.Fatalf("expected no children, got %+v err=%v", children, err)
	}
}

func TestAddDetailChild_AppendsOpenByDefault(t *testing.T) {
	out, err := orchestrator.AddDetailChild(nil, orchestrator.TaskTriageChild{ID: "c1", Title: "fix it"})
	if err != nil {
		t.Fatalf("AddDetailChild: %v", err)
	}
	children, err := orchestrator.DetailChildren(out)
	if err != nil {
		t.Fatalf("DetailChildren: %v", err)
	}
	if len(children) != 1 || children[0].ID != "c1" || children[0].Status != orchestrator.TaskTriageChildStatusOpen {
		t.Fatalf("children = %+v, want single open c1", children)
	}
}

// TestAddDetailChild_IdempotentOnResend pins the crash-recovery dedup: a
// resent child_added with the same id must not create a duplicate entry.
func TestAddDetailChild_IdempotentOnResend(t *testing.T) {
	first, err := orchestrator.AddDetailChild(nil, orchestrator.TaskTriageChild{ID: "c1", Title: "fix it"})
	if err != nil {
		t.Fatalf("AddDetailChild 1: %v", err)
	}
	second, err := orchestrator.AddDetailChild(first, orchestrator.TaskTriageChild{ID: "c1", Title: "fix it (resend)"})
	if err != nil {
		t.Fatalf("AddDetailChild 2: %v", err)
	}
	children, err := orchestrator.DetailChildren(second)
	if err != nil {
		t.Fatalf("DetailChildren: %v", err)
	}
	if len(children) != 1 {
		t.Fatalf("children = %+v, want exactly 1 (idempotent resend)", children)
	}
}

func TestSpecDetailChild_OpenToSpecced(t *testing.T) {
	detail, err := orchestrator.AddDetailChild(nil, orchestrator.TaskTriageChild{ID: "c1", Title: "fix it"})
	if err != nil {
		t.Fatalf("AddDetailChild: %v", err)
	}
	out, err := orchestrator.SpecDetailChild(detail, "c1", orchestrator.TaskTriageChildSpec{Project: "proj-1", Behavior: "executor"}, "")
	if err != nil {
		t.Fatalf("SpecDetailChild: %v", err)
	}
	children, err := orchestrator.DetailChildren(out)
	if err != nil {
		t.Fatalf("DetailChildren: %v", err)
	}
	if len(children) != 1 || children[0].Status != orchestrator.TaskTriageChildStatusSpecced {
		t.Fatalf("children = %+v, want specced", children)
	}
	if children[0].Spec == nil || children[0].Spec.Project != "proj-1" {
		t.Fatalf("spec not set: %+v", children[0].Spec)
	}
}

// TestSpecDetailChild_DoesNotRegressDispatchedOrClosed pins the codex review
// Major fix: a replayed child_specced (e.g. after khi lost the ack and
// resent) must not regress a child the daemon has already mechanically
// advanced past specced.
func TestSpecDetailChild_DoesNotRegressDispatchedOrClosed(t *testing.T) {
	for _, status := range []string{orchestrator.TaskTriageChildStatusDispatched, orchestrator.TaskTriageChildStatusClosed} {
		detail, err := orchestrator.AddDetailChild(nil, orchestrator.TaskTriageChild{ID: "c1", Status: status, TaskRef: "task-x"})
		if err != nil {
			t.Fatalf("AddDetailChild: %v", err)
		}
		out, err := orchestrator.SpecDetailChild(detail, "c1", orchestrator.TaskTriageChildSpec{Project: "proj-1"}, "")
		if err != nil {
			t.Fatalf("SpecDetailChild(%s): unexpected error: %v", status, err)
		}
		children, err := orchestrator.DetailChildren(out)
		if err != nil {
			t.Fatalf("DetailChildren: %v", err)
		}
		if len(children) != 1 || children[0].Status != status {
			t.Fatalf("SpecDetailChild(%s) regressed status to %+v, want unchanged %s", status, children, status)
		}
	}
}

func TestSpecDetailChild_UnknownIDErrors(t *testing.T) {
	_, err := orchestrator.SpecDetailChild(nil, "missing", orchestrator.TaskTriageChildSpec{Project: "p"}, "")
	if err == nil {
		t.Fatal("expected error for unknown child id")
	}
}

func TestMarkDetailChildClosed_MarksMatchingTaskRef(t *testing.T) {
	detail, err := orchestrator.AddDetailChild(nil, orchestrator.TaskTriageChild{ID: "c1", Status: orchestrator.TaskTriageChildStatusDispatched, TaskRef: "task-abc"})
	if err != nil {
		t.Fatalf("AddDetailChild: %v", err)
	}
	out, changed, err := orchestrator.MarkDetailChildClosed(detail, "task-abc")
	if err != nil {
		t.Fatalf("MarkDetailChildClosed: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}
	children, err := orchestrator.DetailChildren(out)
	if err != nil {
		t.Fatalf("DetailChildren: %v", err)
	}
	if len(children) != 1 || children[0].Status != orchestrator.TaskTriageChildStatusClosed {
		t.Fatalf("children = %+v, want closed", children)
	}

	// Idempotent: calling again on the already-closed detail must report
	// changed=false rather than erroring or duplicating a mutation.
	_, changedAgain, err := orchestrator.MarkDetailChildClosed(out, "task-abc")
	if err != nil {
		t.Fatalf("MarkDetailChildClosed (repeat): %v", err)
	}
	if changedAgain {
		t.Fatal("expected changed=false on repeat call (idempotent)")
	}
}

func TestMarkDetailChildClosed_NoMatchingChild(t *testing.T) {
	out, changed, err := orchestrator.MarkDetailChildClosed(nil, "task-abc")
	if err != nil {
		t.Fatalf("MarkDetailChildClosed: %v", err)
	}
	if changed {
		t.Fatal("expected changed=false when no child matches")
	}
	if len(out) != 0 && string(out) != "" {
		// detail is returned unchanged (nil in, nil-ish out) — just make
		// sure no panic/garbage happened.
		if _, err := orchestrator.DetailChildren(out); err != nil {
			t.Fatalf("DetailChildren on unchanged detail: %v", err)
		}
	}
}
