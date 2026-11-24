package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type fakeTunnelProjects []*orchestrator.Project

func (f fakeTunnelProjects) ListProjects(ws string) ([]*orchestrator.Project, error) {
	var out []*orchestrator.Project
	for _, p := range f {
		if ws == "" || p.WorkspaceID == ws {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f fakeTunnelProjects) ResolveProjectRef(ref string) ([]*orchestrator.Project, error) {
	var out []*orchestrator.Project
	for _, p := range f {
		if p.ID == ref || strings.HasPrefix(p.Meta.Name, ref) {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, &api.StatusError{Code: http.StatusNotFound, Message: "no project " + ref}
	}
	return out, nil
}

type fakeTunnelSessions struct{ got *api.StartSessionRequest }

func (f *fakeTunnelSessions) StartSession(_ context.Context, req api.StartSessionRequest) (*api.StartSessionResult, error) {
	f.got = &req
	return &api.StartSessionResult{JobID: "job-9"}, nil
}

type fakeJobStore map[string]*api.Job

func (f fakeJobStore) GetJob(id string) (*api.Job, error) {
	if j, ok := f[id]; ok {
		return j, nil
	}
	return nil, errors.New("job not found")
}
func (f fakeJobStore) ListJobsByTask(string) ([]*api.Job, error) { return nil, nil }
func (f fakeJobStore) UpdateJob(*api.Job) error                  { return nil }

func tunnelProjectsFixture() fakeTunnelProjects {
	p := func(id, name, ws string) *orchestrator.Project {
		pr := &orchestrator.Project{ID: id, WorkspaceID: ws}
		pr.Meta.Name = name
		return pr
	}
	return fakeTunnelProjects{p("p1", "boid", "dev"), p("p2", "heddleway", "dev"), p("p3", "heddleway-calendar", "dev"), p("p4", "rook", "khi")}
}

func serve(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

func TestTunnelStartJob(t *testing.T) {
	sessions := &fakeTunnelSessions{}
	h := tunnelSurface(tunnelDeps{Jobs: &api.JobHandler{}, Projects: tunnelProjectsFixture(), Sessions: sessions})

	rec := serve(h, "POST", "/jobs", `{"project":"boid","instruction":"  PR #1 をレビューして  ","readonly":true}`)
	if rec.Code != http.StatusAccepted || rec.Header().Get("Location") != "/jobs/job-9" {
		t.Fatalf("POST /jobs = %d %s %v", rec.Code, rec.Body.String(), rec.Header())
	}
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["id"] != "job-9" || resp["result_url"] != "/jobs/job-9/result" {
		t.Fatalf("response = %v", resp)
	}
	got := sessions.got
	if got.ProjectID != "p1" || got.HarnessType != "claude" || !got.Readonly || got.DisplayName != "tunnel: claude" {
		t.Fatalf("session request = %+v", got)
	}
	if !strings.HasPrefix(got.Instruction, "PR #1 をレビューして\n") || !strings.Contains(got.Instruction, `boid job done "$BOID_JOB_ID" --output-file /tmp/boid-result.json`) {
		t.Fatalf("instruction = %q", got.Instruction)
	}

	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"project":"boid"}`, http.StatusBadRequest},
		{`{"instruction":"x"}`, http.StatusBadRequest},
		{`{"project":"boid","instruction":"x","harness":"shell"}`, http.StatusBadRequest},
		{`{"project":"heddleway","instruction":"x"}`, http.StatusConflict},
		{`{"project":"nope","instruction":"x"}`, http.StatusNotFound},
		{`not json`, http.StatusBadRequest},
	} {
		sessions.got = nil
		if rec := serve(h, "POST", "/jobs", tc.body); rec.Code != tc.want || sessions.got != nil {
			t.Errorf("POST /jobs %s = %d %s, want %d without starting", tc.body, rec.Code, rec.Body.String(), tc.want)
		}
	}
}

func TestTunnelJobResult(t *testing.T) {
	done := &api.Job{ID: "a", Status: api.JobStatusCompleted, Output: `{"summary":"ok","questions":[],"artifacts":["https://example/pr/1"]}`}
	text := &api.Job{ID: "b", Status: api.JobStatusFailed, ExitCode: 1, Output: "job runtime exited without boid job done"}
	running := &api.Job{ID: "c", Status: api.JobStatusRunning, Output: "partial"}
	h := tunnelSurface(tunnelDeps{Jobs: &api.JobHandler{Jobs: fakeJobStore{"a": done, "b": text, "c": running}}})

	var got map[string]any
	rec := serve(h, "GET", "/jobs/a/result", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 200 || got["status"] != "completed" || got["result"].(map[string]any)["summary"] != "ok" {
		t.Fatalf("result a = %d %s", rec.Code, rec.Body.String())
	}
	got = nil
	_ = json.Unmarshal(serve(h, "GET", "/jobs/b/result", "").Body.Bytes(), &got)
	if got["result_text"] != text.Output || got["exit_code"].(float64) != 1 {
		t.Fatalf("result b = %v", got)
	}
	got = nil
	_ = json.Unmarshal(serve(h, "GET", "/jobs/c/result", "").Body.Bytes(), &got)
	if got["status"] != "running" || got["result"] != nil || got["result_text"] != nil {
		t.Fatalf("result c = %v", got)
	}
	if rec := serve(h, "GET", "/jobs/zzz/result", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown job = %d", rec.Code)
	}
}

func TestTunnelListProjects(t *testing.T) {
	h := tunnelSurface(tunnelDeps{Jobs: &api.JobHandler{}, Projects: tunnelProjectsFixture()})
	var got []map[string]any
	rec := serve(h, "GET", "/projects?workspace=khi", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 200 || len(got) != 1 || got[0]["id"] != "p4" || got[0]["name"] != "rook" {
		t.Fatalf("projects = %d %s", rec.Code, rec.Body.String())
	}
	if _, leaked := got[0]["meta"]; leaked {
		t.Fatalf("project meta leaked: %v", got[0])
	}
}
