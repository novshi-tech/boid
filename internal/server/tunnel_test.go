package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/apigateway"
	"github.com/novshi-tech/boid/internal/tunnel"
)

func TestTunnelSurfaceExposesOnlyJobReads(t *testing.T) {
	h := tunnelSurface(tunnelDeps{Jobs: &api.JobHandler{}}, "dev")
	for _, tc := range []struct{ method, path string }{
		{"POST", "/jobs/j1/done"},
		{"PATCH", "/jobs/j1"},
		{"POST", "/jobs/j1/agent-stop"},
		{"POST", "/jobs"},
		{"POST", "/jobs/j1/stop"},
		{"GET", "/tasks"},
		{"GET", "/secrets/x/value"},
		{"GET", "/api/secrets/x/value"},
		{"GET", "/workspaces"},
		{"GET", "/jobs/j1/log"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 404 or 405", tc.method, tc.path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "GET /jobs") {
		t.Fatalf("GET / = %d %q", rec.Code, rec.Body.String())
	}
}

func TestTunnelRelayUsesAPIGatewayJobToken(t *testing.T) {
	reg := apigateway.NewRegistry()
	allowedTok := reg.Register(nil, "dev", "task-1", false)
	otherTok := reg.Register(nil, "customer", "task-2", false)
	combined := &combinedGatewayHandler{tunnel: newTunnelRelay(&tunnel.Set{}, reg, []string{"dev"})}

	for _, tc := range []struct {
		path string
		want int
	}{
		{tunnel.RelayPathPrefix + allowedTok + "/freee/x", http.StatusServiceUnavailable},
		{tunnel.RelayPathPrefix + otherTok + "/freee/x", http.StatusForbidden},
		{tunnel.RelayPathPrefix + "unknown/freee/x", http.StatusUnauthorized},
	} {
		rec := httptest.NewRecorder()
		combined.ServeHTTP(rec, httptest.NewRequest("GET", tc.path, nil))
		if rec.Code != tc.want {
			t.Errorf("GET %s = %d (%s), want %d", tc.path, rec.Code, rec.Body.String(), tc.want)
		}
	}
}

type fakeGlobalJobs []api.JobWithContext

func (f fakeGlobalJobs) ListJobsWithContext(api.JobListFilter) ([]api.JobWithContext, error) {
	return append([]api.JobWithContext(nil), f...), nil
}

func TestTunnelJobListIsNewestFirstAndLimited(t *testing.T) {
	base := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	var jobs fakeGlobalJobs
	for i := 0; i < 60; i++ {
		ws := "dev"
		if i%2 == 1 {
			ws = "khi"
		}
		j := api.JobWithContext{}
		j.ID = fmt.Sprintf("j%02d", i)
		j.WorkspaceID = ws
		j.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		j.Output = strings.Repeat("x", 2000)
		jobs = append(jobs, j)
	}
	h := tunnelSurface(tunnelDeps{Jobs: &api.JobHandler{Global: jobs}}, "dev")

	get := func(path string) []api.JobWithContext {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body.String())
		}
		var out []api.JobWithContext
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	out := get("/jobs")
	if len(out) != tunnelJobsDefaultLimit || out[0].ID != "j58" || out[1].ID != "j56" {
		t.Fatalf("default list: %d jobs, first %q %q", len(out), out[0].ID, out[1].ID)
	}
	if n := len([]rune(out[0].Output)); n > tunnelJobOutputChars+1 {
		t.Fatalf("output not shortened: %d chars", n)
	}
	out = get("/jobs?workspace=khi&limit=3")
	if len(out) != 3 || out[0].ID != "j58" || out[2].ID != "j54" {
		t.Fatalf("the dev tunnel listed %+v", out)
	}
	for _, j := range get("/jobs?limit=1000") {
		if j.WorkspaceID != "dev" {
			t.Fatalf("the dev tunnel listed a %s job", j.WorkspaceID)
		}
	}
	if out := get("/jobs?limit=1000"); len(out) != 30 {
		t.Fatalf("limit is capped at %d, got %d", tunnelJobsMaxLimit, len(out))
	}
}

func TestTailJobLogKeepsTheEnd(t *testing.T) {
	full := strings.Repeat("a", 1000) + "THE END"
	log := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(full))
	}
	rec := httptest.NewRecorder()
	tailJobLog(rec, httptest.NewRequest("GET", "/jobs/j1/log?tail=10", nil), log)
	if rec.Code != http.StatusOK || rec.Body.String() != "aaaTHE END" || rec.Header().Get("X-Boid-Log-Truncated") != "997" {
		t.Fatalf("tail = %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
	rec = httptest.NewRecorder()
	tailJobLog(rec, httptest.NewRequest("GET", "/jobs/j1/log", nil), log)
	if rec.Body.String() != full || rec.Header().Get("X-Boid-Log-Truncated") != "" {
		t.Fatalf("short log was cut: %q", rec.Body.String())
	}
}
