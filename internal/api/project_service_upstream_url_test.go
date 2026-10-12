package api

import (
	"fmt"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

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
