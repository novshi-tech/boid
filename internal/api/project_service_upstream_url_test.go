package api

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// upstreamURLMetaStore is a minimal ProjectAppService.Meta stub with a
// configurable Load result/error and a record of Remove calls, used to test
// the upstream_url capture wiring in CreateProject without needing a real
// project.yaml on disk.
type upstreamURLMetaStore struct {
	meta    *orchestrator.ProjectMeta
	loadErr error
	removed []string
}

func (s *upstreamURLMetaStore) Load(_ string) (*orchestrator.ProjectMeta, error) {
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	return s.meta, nil
}
func (s *upstreamURLMetaStore) Get(_ string) (*orchestrator.ProjectMeta, bool) { return nil, false }
func (s *upstreamURLMetaStore) Remove(id string)                               { s.removed = append(s.removed, id) }
func (s *upstreamURLMetaStore) LoadAll(_ []*orchestrator.Project) []error      { return nil }
func (s *upstreamURLMetaStore) SetWorkspaceID(_, _ string)                     {}

// TestProjectAppService_CreateProject_CapturesUpstreamURL verifies that a
// successful capture is normalized onto the created project (PR2 of
// docs/plans/git-gateway-cutover.md).
func TestProjectAppService_CreateProject_CapturesUpstreamURL(t *testing.T) {
	svc := &ProjectAppService{
		Projects: &stubProjectRepository{},
		Meta:     &upstreamURLMetaStore{meta: &orchestrator.ProjectMeta{ID: "proj-1", Name: "Proj 1"}},
		CaptureUpstreamURL: func(workDir string) (string, error) {
			return "https://github.com/owner/repo.git", nil
		},
	}

	project, err := svc.CreateProject("/some/work/dir")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if want := "https://github.com/owner/repo.git"; project.UpstreamURL != want {
		t.Errorf("UpstreamURL = %q, want %q", project.UpstreamURL, want)
	}
}

// TestProjectAppService_CreateProject_RejectsMissingUpstreamURL verifies the
// "origin の無い project は登録拒否" semantics: a capture failure rejects
// registration with a 400 and rolls back the cached meta (so a retry after
// adding a remote does not see a stale meta.Get hit).
func TestProjectAppService_CreateProject_RejectsMissingUpstreamURL(t *testing.T) {
	meta := &upstreamURLMetaStore{meta: &orchestrator.ProjectMeta{ID: "proj-1", Name: "Proj 1"}}
	svc := &ProjectAppService{
		Projects: &stubProjectRepository{},
		Meta:     meta,
		CaptureUpstreamURL: func(workDir string) (string, error) {
			return "", errors.New("no origin configured")
		},
	}

	_, err := svc.CreateProject("/some/work/dir")
	if err == nil {
		t.Fatal("expected CreateProject to reject a project with no origin remote")
	}
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected *StatusError, got %T: %v", err, err)
	}
	if statusErr.Code != 400 {
		t.Errorf("status code = %d, want 400", statusErr.Code)
	}
	if len(meta.removed) != 1 || meta.removed[0] != "proj-1" {
		t.Errorf("expected meta.Remove(%q) to be called, got removed=%v", "proj-1", meta.removed)
	}
}

// TestProjectAppService_CreateProject_NoCaptureFuncSkipsUpstreamURL verifies
// the nil-CaptureUpstreamURL escape hatch used by tests/wiring that do not
// exercise this path: no panic, no rejection, upstream_url stays empty.
func TestProjectAppService_CreateProject_NoCaptureFuncSkipsUpstreamURL(t *testing.T) {
	svc := &ProjectAppService{
		Projects: &stubProjectRepository{},
		Meta:     &upstreamURLMetaStore{meta: &orchestrator.ProjectMeta{ID: "proj-1", Name: "Proj 1"}},
	}

	project, err := svc.CreateProject("/some/work/dir")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if project.UpstreamURL != "" {
		t.Errorf("UpstreamURL = %q, want empty when CaptureUpstreamURL is nil", project.UpstreamURL)
	}
}

// TestProjectAppService_ReloadProjects_RecapturesChangedUpstreamURL verifies
// reload persists a newly-captured upstream_url that differs from what is
// stored (e.g. startup backfill left it empty, or the origin changed).
func TestProjectAppService_ReloadProjects_RecapturesChangedUpstreamURL(t *testing.T) {
	repo := &stubProjectRepository{
		projects: []*orchestrator.Project{{ID: "proj-1", WorkDir: "/work/proj-1"}},
	}
	svc := &ProjectAppService{
		Projects: repo,
		Meta:     &stubProjectMetaStore{},
		CaptureUpstreamURL: func(workDir string) (string, error) {
			return "https://github.com/owner/repo.git", nil
		},
	}

	result, err := svc.ReloadProjects()
	if err != nil {
		t.Fatalf("ReloadProjects: %v", err)
	}
	if result.Status != "ok" {
		t.Errorf("Status = %q, want %q (errors=%v)", result.Status, "ok", result.Errors)
	}
	if repo.setUpstreamURLCalls != 1 {
		t.Errorf("SetProjectUpstreamURL calls = %d, want 1", repo.setUpstreamURLCalls)
	}
	if repo.projects[0].UpstreamURL != "https://github.com/owner/repo.git" {
		t.Errorf("project upstream_url = %q, want captured URL", repo.projects[0].UpstreamURL)
	}
}

// TestProjectAppService_ReloadProjects_SkipsWriteWhenUnchanged verifies the
// idempotent-backfill contract: recapturing the same URL already on file
// must not issue a redundant write.
func TestProjectAppService_ReloadProjects_SkipsWriteWhenUnchanged(t *testing.T) {
	repo := &stubProjectRepository{
		projects: []*orchestrator.Project{{ID: "proj-1", WorkDir: "/work/proj-1", UpstreamURL: "https://github.com/owner/repo.git"}},
	}
	svc := &ProjectAppService{
		Projects: repo,
		Meta:     &stubProjectMetaStore{},
		CaptureUpstreamURL: func(workDir string) (string, error) {
			return "https://github.com/owner/repo.git", nil
		},
	}

	if _, err := svc.ReloadProjects(); err != nil {
		t.Fatalf("ReloadProjects: %v", err)
	}
	if repo.setUpstreamURLCalls != 0 {
		t.Errorf("SetProjectUpstreamURL calls = %d, want 0 (unchanged URL should not be re-written)", repo.setUpstreamURLCalls)
	}
}

// TestProjectAppService_ReloadProjects_CaptureFailureIsWarningNotFatal
// verifies a project missing its origin remote surfaces as a partial-status
// warning (not a hard reload failure) — matching the "起動時 backfill: 失敗は
// 警告" tone applied consistently to reload.
func TestProjectAppService_ReloadProjects_CaptureFailureIsWarningNotFatal(t *testing.T) {
	repo := &stubProjectRepository{
		projects: []*orchestrator.Project{{ID: "proj-1", WorkDir: "/work/proj-1"}},
	}
	svc := &ProjectAppService{
		Projects: repo,
		Meta:     &stubProjectMetaStore{},
		CaptureUpstreamURL: func(workDir string) (string, error) {
			return "", fmt.Errorf("no origin configured")
		},
	}

	result, err := svc.ReloadProjects()
	if err != nil {
		t.Fatalf("ReloadProjects: %v", err)
	}
	if result.Status != "partial" {
		t.Errorf("Status = %q, want %q", result.Status, "partial")
	}
	found := false
	for _, e := range result.Errors {
		if strings.Contains(e, "proj-1") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an error message mentioning proj-1, got %v", result.Errors)
	}
	if repo.setUpstreamURLCalls != 0 {
		t.Errorf("SetProjectUpstreamURL calls = %d, want 0 on capture failure", repo.setUpstreamURLCalls)
	}
}
