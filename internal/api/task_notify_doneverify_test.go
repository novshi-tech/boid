package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

type doneVerifyProjects struct{ workdir string }

func (p doneVerifyProjects) GetProject(id string) (*orchestrator.Project, error) {
	return &orchestrator.Project{ID: id, WorkDir: p.workdir}, nil
}

func TestVerifyDoneClaimWithoutMirror(t *testing.T) {
	bin := t.TempDir()
	marker := filepath.Join(bin, "git-called")
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	svc := &TaskAppService{Projects: doneVerifyProjects{workdir: t.TempDir()}}
	task := &orchestrator.Task{Exec: &orchestrator.ExecAttrs{Payload: json.RawMessage(`{"artifact":{"report":{"release":{"commit":"deadbeefcafefeed1234","branch":"new","pushed":true}}}}`)}}
	if err := svc.verifyDoneClaim(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("daemon invoked git: %v", err)
	}
	task.OpenChildCount = 2
	if err := svc.verifyDoneClaim(context.Background(), task); err == nil {
		t.Fatal("completion with open children accepted")
	}
}

func TestVerifyDoneClaimChecksPushedReleaseThroughGateway(t *testing.T) {
	task := &orchestrator.Task{ProjectID: "project", Exec: &orchestrator.ExecAttrs{Payload: json.RawMessage(`{"artifact":{"report":{"release":{"commit":"abcdef1234","branch":"feature","pushed":true}}}}`)}}
	calls := 0
	svc := &TaskAppService{Projects: doneVerifyProjects{workdir: "/missing/mirror"}, VerifyRelease: func(ctx context.Context, project *orchestrator.Project, branch, commit string) (bool, error) {
		calls++
		if branch != "feature" || commit != "abcdef1234" {
			t.Fatal("release arguments changed")
		}
		return false, nil
	}}
	if err := svc.verifyDoneClaim(context.Background(), task); err == nil {
		t.Fatal("mismatched remote release accepted")
	}
	if calls != 1 {
		t.Fatalf("gateway calls=%d", calls)
	}
	svc.VerifyRelease = func(context.Context, *orchestrator.Project, string, string) (bool, error) { return true, nil }
	if err := svc.verifyDoneClaim(context.Background(), task); err != nil {
		t.Fatal(err)
	}
}
