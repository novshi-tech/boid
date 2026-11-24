package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/apigateway"
	"github.com/novshi-tech/boid/internal/tunnel"
)

func TestTunnelSurfaceExposesOnlyJobReads(t *testing.T) {
	h := tunnelSurface(&api.JobHandler{})
	for _, tc := range []struct{ method, path string }{
		{"POST", "/jobs/j1/done"},
		{"PATCH", "/jobs/j1"},
		{"POST", "/jobs/j1/agent-stop"},
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
	m := &tunnel.Manager{}
	combined := &combinedGatewayHandler{tunnel: newTunnelRelay(m, reg, []string{"dev"})}

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
