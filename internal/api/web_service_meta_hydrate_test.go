package api

import (
	"errors"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// This file pins WebAppService.ListProjects and GetProjectByID's switch from
// bare Meta.Get to Meta.GetWithWorkspace (docs/plans/workspace-default-project.md
// §PR分割案 PR2, §現状の実測4's "Web UI project 一覧" / "Web UI project 単体"
// rows — the last 2 of the 5 mandatory-switch call sites; fable review M1
// flagged these as the task-creation-form behavior dropdown's supply source,
// not merely display).
//
// Both sites' CURRENT (pre-PR2) behavior on a meta miss is "silently leave
// project.Meta unset, no error" — so unlike workflow_replay.go's two sites
// (which already 500'd on a miss) this pair needs the SAME fallback-to-bare
// idiom task_create.go's CreateTask uses (and ProjectAppService.
// hydrateProjectWithWorkspace already established): try GetWithWorkspace,
// fall back to bare Get on any hydration error, leave Meta unset only if
// BOTH fail — reproducing "meta not loaded → skip, no error" exactly while
// gracefully degrading (instead of blanking a project's meta) for the two
// new failure modes GetWithWorkspace can produce that bare Get never could.

func TestWebAppServiceListProjects_UsesWorkspaceHydratedMeta(t *testing.T) {
	projects := []*orchestrator.Project{{ID: "proj-1"}}
	bareMeta := &orchestrator.ProjectMeta{ID: "bare"}
	hydratedMeta := &orchestrator.ProjectMeta{ID: "hydrated"}
	svc := &WebAppService{
		Projects: &stubProjectRepository{projects: projects},
		Meta:     stubMetaStore{meta: bareMeta, hydrated: hydratedMeta},
	}

	got, err := svc.ListProjects()
	if err != nil {
		t.Fatalf("ListProjects() error = %v", err)
	}
	if len(got) != 1 || got[0].Meta.ID != "hydrated" {
		t.Fatalf("projects = %+v, want Meta.ID=hydrated (proves GetWithWorkspace, not bare Get, was used)", got)
	}
}

func TestWebAppServiceListProjects_HydrationErrorFallsBackToBareMeta(t *testing.T) {
	projects := []*orchestrator.Project{{ID: "proj-1"}}
	bareMeta := &orchestrator.ProjectMeta{ID: "bare"}
	svc := &WebAppService{
		Projects: &stubProjectRepository{projects: projects},
		Meta:     stubMetaStore{meta: bareMeta, hydrateErr: errors.New("workspace.yaml: corrupt")},
	}

	got, err := svc.ListProjects()
	if err != nil {
		t.Fatalf("ListProjects() error = %v", err)
	}
	if len(got) != 1 || got[0].Meta.ID != "bare" {
		t.Fatalf("projects = %+v, want Meta.ID=bare (fallback to bare Get on hydration error)", got)
	}
}

func TestWebAppServiceListProjects_NoMetaAnywhere_LeavesMetaZeroNoError(t *testing.T) {
	projects := []*orchestrator.Project{{ID: "proj-1"}}
	svc := &WebAppService{
		Projects: &stubProjectRepository{projects: projects},
		Meta:     stubMetaStore{meta: nil, hydrateErr: errors.New("workspace.yaml: corrupt")},
	}

	got, err := svc.ListProjects()
	if err != nil {
		t.Fatalf("ListProjects() error = %v, want nil (legacy: a meta miss is silently tolerated)", err)
	}
	if len(got) != 1 || got[0].Meta.ID != "" {
		t.Fatalf("projects = %+v, want a zero-value Meta (both lookups failed)", got)
	}
}

func TestWebAppServiceGetProjectByID_UsesWorkspaceHydratedMeta(t *testing.T) {
	projects := []*orchestrator.Project{{ID: "proj-1"}}
	bareMeta := &orchestrator.ProjectMeta{ID: "bare"}
	hydratedMeta := &orchestrator.ProjectMeta{ID: "hydrated"}
	svc := &WebAppService{
		Projects: &stubProjectRepository{projects: projects},
		Meta:     stubMetaStore{meta: bareMeta, hydrated: hydratedMeta},
	}

	got, err := svc.GetProjectByID("proj-1")
	if err != nil {
		t.Fatalf("GetProjectByID() error = %v", err)
	}
	if got.Meta.ID != "hydrated" {
		t.Fatalf("project.Meta.ID = %q, want hydrated (proves GetWithWorkspace, not bare Get, was used)", got.Meta.ID)
	}
}

func TestWebAppServiceGetProjectByID_HydrationErrorFallsBackToBareMeta(t *testing.T) {
	projects := []*orchestrator.Project{{ID: "proj-1"}}
	bareMeta := &orchestrator.ProjectMeta{ID: "bare"}
	svc := &WebAppService{
		Projects: &stubProjectRepository{projects: projects},
		Meta:     stubMetaStore{meta: bareMeta, hydrateErr: errors.New("host_commands: builtin conflict")},
	}

	got, err := svc.GetProjectByID("proj-1")
	if err != nil {
		t.Fatalf("GetProjectByID() error = %v", err)
	}
	if got.Meta.ID != "bare" {
		t.Fatalf("project.Meta.ID = %q, want bare (fallback to bare Get on hydration error)", got.Meta.ID)
	}
}

func TestWebAppServiceGetProjectByID_NoMetaAnywhere_LeavesMetaZeroNoError(t *testing.T) {
	projects := []*orchestrator.Project{{ID: "proj-1"}}
	svc := &WebAppService{
		Projects: &stubProjectRepository{projects: projects},
		Meta:     stubMetaStore{meta: nil, hydrateErr: errors.New("workspace.yaml: corrupt")},
	}

	got, err := svc.GetProjectByID("proj-1")
	if err != nil {
		t.Fatalf("GetProjectByID() error = %v, want nil (legacy: a meta miss is silently tolerated)", err)
	}
	if got.Meta.ID != "" {
		t.Fatalf("project.Meta.ID = %q, want zero-value (both lookups failed)", got.Meta.ID)
	}
}
