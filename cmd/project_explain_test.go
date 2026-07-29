package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
	"gopkg.in/yaml.v3"
)

func TestRenderProjectExplainSummary_ShowsIndicatorWhenApplied(t *testing.T) {
	e := &projectspec.ProjectExplain{AnyWorkspaceDefaultApplied: true}

	got := captureStdout(t, func() {
		renderProjectExplainSummary(e)
	})

	if !strings.Contains(got, "workspace default") {
		t.Errorf("output missing workspace-default indicator\n%s", got)
	}
}

func TestRenderProjectExplainSummary_SilentWhenNotApplied(t *testing.T) {
	e := &projectspec.ProjectExplain{AnyWorkspaceDefaultApplied: false}

	got := captureStdout(t, func() {
		renderProjectExplainSummary(e)
	})

	if got != "" {
		t.Errorf("output = %q, want empty when nothing is workspace-default-derived", got)
	}
}

func TestRenderProjectExplainSummary_NoYAMLProjectIndicator(t *testing.T) {
	e := &projectspec.ProjectExplain{IsNoYAMLProject: true, NameSource: "cached"}

	got := captureStdout(t, func() {
		renderProjectExplainSummary(e)
	})

	if !strings.Contains(got, "no project.yaml") {
		t.Errorf("output missing no-project.yaml indicator\n%s", got)
	}
	// Minor (Codex review round 1): the doc says `project show` — even
	// without --explain — should show name source for a no-YAML project,
	// not just the id-derived note.
	if !strings.Contains(got, "cached") {
		t.Errorf("output missing name source in the plain summary\n%s", got)
	}
}

// TestProjectShowOutput_JSONIncludesFullExplainOnlyWhenSet pins Codex review
// round 1 Major (structured output must carry explain data too) AND round 2
// Major (the fix must actually gate Explain on --explain, not populate it
// unconditionally — the round 1 fix set it regardless of the flag, so
// structured output did not differ with vs. without --explain). Here
// Explain is nil (mirroring `--explain` NOT passed) while
// WorkspaceDefaultApplied/NoYAMLProject are still set (the always-on tier).
func TestProjectShowOutput_JSONIncludesFullExplainOnlyWhenSet(t *testing.T) {
	p := &projectspec.Project{ID: "proj-abc", Meta: projectspec.ProjectMeta{Name: "My Project"}}
	out := &projectShowOutput{Project: p, WorkspaceDefaultApplied: true, NoYAMLProject: false}

	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	s := string(b)

	if !strings.Contains(s, `"id":"proj-abc"`) {
		t.Errorf("output missing promoted Project field\n%s", s)
	}
	if !strings.Contains(s, `"workspace_default_applied":true`) {
		t.Errorf("output missing always-on summary indicator\n%s", s)
	}
	if strings.Contains(s, `"explain"`) {
		t.Errorf("output should omit explain key when --explain was not passed\n%s", s)
	}

	// Now with --explain's full detail set: the key must appear and differ
	// from the no-flag case above.
	out.Explain = &projectspec.ProjectExplain{
		ProjectID:                  "proj-abc",
		AnyWorkspaceDefaultApplied: true,
		BaseBranch:                 projectspec.ProvenanceWorkspaceDefault,
	}
	b2, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	s2 := string(b2)
	if !strings.Contains(s2, `"explain"`) {
		t.Errorf("output missing explain field when Explain is set\n%s", s2)
	}
	if s == s2 {
		t.Error("structured output identical with and without --explain's Explain field — the flag has no effect")
	}
}

// TestProjectShowOutput_YAMLInlinesProjectFields pins Codex review round 2
// Major: gopkg.in/yaml.v3 does not inline an anonymous struct field unless
// tagged `yaml:",inline"` — without that tag, `project show -o yaml` would
// silently change from top-level project fields to a nested `project:` key,
// breaking existing YAML consumers.
func TestProjectShowOutput_YAMLInlinesProjectFields(t *testing.T) {
	p := &projectspec.Project{ID: "proj-abc"}
	out := &projectShowOutput{Project: p}

	b, err := yaml.Marshal(out)
	if err != nil {
		t.Fatalf("yaml.Marshal() error = %v", err)
	}
	s := string(b)

	if strings.Contains(s, "project:") {
		t.Errorf("Project field was nested under a \"project:\" key instead of inlined at the top level\n%s", s)
	}
	if !strings.Contains(s, "id: proj-abc") {
		t.Errorf("output missing top-level inlined id field\n%s", s)
	}
}

func TestRenderProjectExplainDetail_PerFieldProvenance(t *testing.T) {
	e := &projectspec.ProjectExplain{
		BaseBranch:             projectspec.ProvenanceProjectYAML,
		ForkPoint:              projectspec.ProvenanceWorkspaceDefault,
		DefaultTaskBehavior:    projectspec.ProvenanceUnset,
		BaseBranchSnapshotNote: "some snapshot note",
		TaskBehaviors: map[string]projectspec.FieldProvenance{
			"supervisor": projectspec.ProvenanceProjectYAML,
			"executor":   projectspec.ProvenanceWorkspaceDefault,
		},
	}

	got := captureStdout(t, func() {
		renderProjectExplainDetail(e)
	})

	checks := []string{
		"base_branch:", "project.yaml",
		"fork_point:", "workspace default",
		"default_task_behavior:", "unset",
		"supervisor", "executor",
		"some snapshot note",
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n%s", want, got)
		}
	}
}

// TestRenderProjectExplainSummary_WorkspaceUnavailableIsVisible pins Codex
// review round 2 Major: a real workspace-load failure must be surfaced even
// when nothing else in the summary would mention it (e.g. project.yaml
// supplies every scalar field itself, so AnyWorkspaceDefaultApplied is
// false).
func TestRenderProjectExplainSummary_WorkspaceUnavailableIsVisible(t *testing.T) {
	e := &projectspec.ProjectExplain{AnyWorkspaceDefaultApplied: false, WorkspaceUnavailable: true}

	got := captureStdout(t, func() {
		renderProjectExplainSummary(e)
	})

	if !strings.Contains(got, "could not be read") {
		t.Errorf("output missing workspace-unavailable note\n%s", got)
	}
}

func TestRenderProjectExplainDetail_WorkspaceUnavailableIsVisible(t *testing.T) {
	e := &projectspec.ProjectExplain{WorkspaceUnavailable: true, BaseBranchSnapshotNote: "note"}

	got := captureStdout(t, func() {
		renderProjectExplainDetail(e)
	})

	if !strings.Contains(got, "could not be read") {
		t.Errorf("output missing workspace-unavailable warning\n%s", got)
	}
}

func TestRenderProjectExplainDetail_NoYAMLProjectShowsIDAndNameSource(t *testing.T) {
	e := &projectspec.ProjectExplain{
		IsNoYAMLProject:        true,
		NameSource:             "cached",
		BaseBranchSnapshotNote: "note",
	}

	got := captureStdout(t, func() {
		renderProjectExplainDetail(e)
	})

	if !strings.Contains(got, "derived from git URL") {
		t.Errorf("output missing id-derived note\n%s", got)
	}
	if !strings.Contains(got, "cached") {
		t.Errorf("output missing name source\n%s", got)
	}
}
