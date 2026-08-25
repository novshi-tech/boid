package api

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/web/templates"
)

// The Web UI showed only a child's status badge and title, so "what actually
// happens when I press Go" was unanswerable from the card — nose hit this on
// 2026-08-21 with a card whose two specced children were (a) a Bitbucket
// reply task and (b) a leftover verification stub, indistinguishable in the
// UI. The 2026-08-14 iteration of this section answered "is there a spec at
// all"; these tests cover the next question: "what does that spec say".

func TestTriageChildrenForDisplay_ResolvesProjectIDToName(t *testing.T) {
	svc := &stubWebService{projects: []*orchestrator.Project{
		{ID: "url-abc", Meta: orchestrator.ProjectMeta{Name: "rook-server"}},
	}}
	triage := &stubTriageStore{rows: map[string]*orchestrator.CardAttrs{
		"t1": {TaskID: "t1", Detail: []byte(`{"children":[
			{"id":"ch_00","title":"調査","status":"specced",
			 "spec":{"project":"url-abc","behavior":"research","description":"d","instruction":"i"}}
		]}`)},
	}}
	h := &WebHandler{Service: svc, TaskTriage: triage}

	children := h.triageChildrenForDisplay("t1")

	if len(children) != 1 {
		t.Fatalf("children = %d, want 1", len(children))
	}
	if got := children[0].Spec.Project; got != "rook-server" {
		t.Errorf("Spec.Project = %q, want the resolved name %q", got, "rook-server")
	}
}

func TestTriageChildrenForDisplay_KeepsUnresolvableProjectAsIs(t *testing.T) {
	// A project that was removed (or an id typo) must still render something
	// the reader can act on — silently blanking it would hide where the child
	// would run.
	svc := &stubWebService{projects: []*orchestrator.Project{
		{ID: "url-abc", Meta: orchestrator.ProjectMeta{Name: "rook-server"}},
	}}
	triage := &stubTriageStore{rows: map[string]*orchestrator.CardAttrs{
		"t1": {TaskID: "t1", Detail: []byte(`{"children":[
			{"id":"ch_00","status":"specced","spec":{"project":"url-gone","behavior":"research"}}
		]}`)},
	}}
	h := &WebHandler{Service: svc, TaskTriage: triage}

	if got := h.triageChildrenForDisplay("t1")[0].Spec.Project; got != "url-gone" {
		t.Errorf("Spec.Project = %q, want the raw id kept", got)
	}
}

func TestTriageChildrenForDisplay_LeavesSpeclessChildrenAlone(t *testing.T) {
	svc := &stubWebService{}
	triage := &stubTriageStore{rows: map[string]*orchestrator.CardAttrs{
		"t1": {TaskID: "t1", Detail: []byte(`{"children":[{"id":"ch_00","status":"open"}]}`)},
	}}
	h := &WebHandler{Service: svc, TaskTriage: triage}

	children := h.triageChildrenForDisplay("t1")
	if len(children) != 1 || children[0].Spec != nil {
		t.Fatalf("open child should pass through untouched, got %+v", children)
	}
}

// TestTriageChildrenForDisplay_DoesNotMutateStoredSpec pins the reason this
// is a separate "ForDisplay" helper: the name substitution is a rendering
// concern. If it wrote through to the parsed spec, any future caller that
// reads children for a non-display purpose (dispatch decides where the child
// runs from Spec.Project) would get a project NAME where the storage
// contract requires an ID — orchestrator.TaskTriageChildSpec's doc comment
// is explicit that an unresolvable Project surfaces as "project not found".
func TestTriageChildrenForDisplay_DoesNotMutateStoredSpec(t *testing.T) {
	svc := &stubWebService{projects: []*orchestrator.Project{
		{ID: "url-abc", Meta: orchestrator.ProjectMeta{Name: "rook-server"}},
	}}
	triage := &stubTriageStore{rows: map[string]*orchestrator.CardAttrs{
		"t1": {TaskID: "t1", Detail: []byte(`{"children":[
			{"id":"ch_00","status":"specced","spec":{"project":"url-abc","behavior":"research"}}
		]}`)},
	}}
	h := &WebHandler{Service: svc, TaskTriage: triage}

	_ = h.triageChildrenForDisplay("t1")

	// Re-read through the plain (non-display) path: it must still be the id.
	if got := h.triageChildrenFor("t1")[0].Spec.Project; got != "url-abc" {
		t.Errorf("stored Spec.Project = %q, want the id %q untouched", got, "url-abc")
	}
}

// TestTaskDetailChildrenSection_RendersSpec is the wiring half: the helper
// above can resolve everything correctly and still leave the reader blind if
// the template never prints it.
func TestTaskDetailChildrenSection_RendersSpec(t *testing.T) {
	children := []orchestrator.TaskTriageChild{{
		ID: "ch_00", Title: "id 競合の扱いを調査する", Status: "specced",
		Spec: &orchestrator.TaskTriageChildSpec{
			Project:     "rook-server",
			Behavior:    "research",
			Description: "PR #1063 の実装を読んで競合の扱いを切り分ける",
			Instruction: "推測で結論を書かない",
		},
	}}

	var buf bytes.Buffer
	if err := templates.TaskDetailChildrenSection(children).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	for _, want := range []string{
		"research",                                 // どの behavior で走るか
		"rook-server",                              // どのプロジェクトで走るか
		"PR #1063 の実装を読んで競合の扱いを切り分ける", // 何をするのか
		"推測で結論を書かない",                       // どう進めるのか
	} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered children section is missing %q — a reader still cannot tell what Go would do", want)
		}
	}
}
