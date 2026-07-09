package cmd

import (
	"strings"
	"testing"
	"time"

	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

func TestRenderProjectDetail_BasicFields(t *testing.T) {
	p := &projectspec.Project{
		ID:          "proj-abc",
		WorkspaceID: "ws-1",
		WorkDir:     "/home/user/repo",
		CreatedAt:   time.Unix(0, 0).UTC(),
		UpdatedAt:   time.Unix(0, 0).UTC(),
		Meta: projectspec.ProjectMeta{
			Name: "My Project",
		},
	}

	got := captureStdout(t, func() {
		renderProjectDetail(p)
	})

	checks := []string{
		"ID:", "proj-abc",
		"Name:", "My Project",
		"WorkDir:", "/home/user/repo",
		"WorkspaceID:", "ws-1",
		"CreatedAt:",
		"UpdatedAt:",
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n%s", want, got)
		}
	}
}

func TestRenderProjectDetail_UpstreamURL_Set(t *testing.T) {
	p := &projectspec.Project{
		ID:          "proj-abc",
		WorkDir:     "/home/user/repo",
		UpstreamURL: "https://github.com/owner/repo.git",
		Meta:        projectspec.ProjectMeta{Name: "My Project"},
	}

	got := captureStdout(t, func() {
		renderProjectDetail(p)
	})

	if !strings.Contains(got, "UpstreamURL: https://github.com/owner/repo.git") {
		t.Errorf("output missing captured UpstreamURL\n%s", got)
	}
}

func TestRenderProjectDetail_UpstreamURL_Empty(t *testing.T) {
	p := &projectspec.Project{
		ID:      "proj-abc",
		WorkDir: "/home/user/repo",
		Meta:    projectspec.ProjectMeta{Name: "My Project"},
	}

	got := captureStdout(t, func() {
		renderProjectDetail(p)
	})

	if !strings.Contains(got, "UpstreamURL: (none") {
		t.Errorf("output missing empty-UpstreamURL guidance\n%s", got)
	}
}

func TestRenderProjectDetail_MetaSections(t *testing.T) {
	p := &projectspec.Project{
		ID: "proj-meta",
		Meta: projectspec.ProjectMeta{
			Name: "Meta Test",
			TaskBehaviors: map[string]projectspec.TaskBehavior{
				"dev": {
					Hooks: []projectspec.Hook{
						{ID: "on-start", Requires: []string{"gh"}},
					},
				},
			},
			HostCommands: projectspec.HostCommands{"gh": {}},
			AdditionalBindings: []projectspec.BindMount{
				{Source: "/data", Mode: "ro"},
			},
			Env: map[string]string{
				"GITHUB_TOKEN": "secret",
				"FOO":          "bar",
			},
			SecretNamespace: "myns",
		},
	}

	got := captureStdout(t, func() {
		renderProjectDetail(p)
	})

	checks := []string{
		"TaskBehaviors:",
		"dev",
		"hook: on-start",
		"HostCommands:",
		"gh",
		"AdditionalBindings:",
		"/data",
		"ro",
		"Env:",
		"FOO",
		"GITHUB_TOKEN",
		"SecretNamespace:",
		"myns",
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n%s", want, got)
		}
	}
}

func TestRenderProjectBehaviors_AlphaOrder(t *testing.T) {
	p := &projectspec.Project{
		ID: "proj-beh",
		Meta: projectspec.ProjectMeta{
			TaskBehaviors: map[string]projectspec.TaskBehavior{
				"zzz": {},
				"aaa": {},
				"mmm": {},
			},
		},
	}

	got := captureStdout(t, func() {
		renderProjectBehaviors(p)
	})

	// キーがアルファベット順で出ること
	idxA := strings.Index(got, "aaa")
	idxM := strings.Index(got, "mmm")
	idxZ := strings.Index(got, "zzz")
	if idxA < 0 || idxM < 0 || idxZ < 0 {
		t.Fatalf("missing keys in output:\n%s", got)
	}
	if !(idxA < idxM && idxM < idxZ) {
		t.Errorf("behaviors not in alphabetical order (a=%d m=%d z=%d):\n%s", idxA, idxM, idxZ, got)
	}
}

func TestRenderProjectBehaviors_Fields(t *testing.T) {
	p := &projectspec.Project{
		ID: "proj-beh2",
		Meta: projectspec.ProjectMeta{
			TaskBehaviors: map[string]projectspec.TaskBehavior{
				"dev": {
					Traits: []string{"artifact", "worktree"},
				},
			},
		},
	}

	got := captureStdout(t, func() {
		renderProjectBehaviors(p)
	})

	checks := []string{
		"dev",
		"artifact",
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n%s", want, got)
		}
	}
}

func TestRenderProjectBehaviors_Empty(t *testing.T) {
	p := &projectspec.Project{
		ID: "proj-empty",
		Meta: projectspec.ProjectMeta{
			TaskBehaviors: map[string]projectspec.TaskBehavior{},
		},
	}

	got := captureStdout(t, func() {
		renderProjectBehaviors(p)
	})

	if !strings.Contains(got, "no behaviors") {
		t.Errorf("expected 'no behaviors' message, got:\n%s", got)
	}
}

// TestProjectAddCmd_HasWorkspaceFlag verifies that the --workspace flag is
// registered on `boid project add`.
func TestProjectAddCmd_HasWorkspaceFlag(t *testing.T) {
	f := projectAddCmd.Flags().Lookup("workspace")
	if f == nil {
		t.Fatal("--workspace flag not registered on project add")
	}
	if f.DefValue != "" {
		t.Errorf("expected empty default for --workspace, got %q", f.DefValue)
	}
}

// TestProjectInitSubCmd_HasWorkspaceFlag verifies that --workspace is
// registered on `boid project init`.
func TestProjectInitSubCmd_HasWorkspaceFlag(t *testing.T) {
	f := projectInitSubCmd.Flags().Lookup("workspace")
	if f == nil {
		t.Fatal("--workspace flag not registered on project init")
	}
	if f.DefValue != "" {
		t.Errorf("expected empty default for --workspace, got %q", f.DefValue)
	}
}
