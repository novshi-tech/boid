package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/novshi-tech/boid/internal/apigateway"
	"github.com/novshi-tech/boid/internal/gitgateway"
)

// TestCombinedGatewayHandler_RoutesByPathPrefix pins docs/plans/api-gateway.md
// 論点1 ("同居 — path prefix /j/ と /api/ で分岐"): a request under
// gitgateway.PathPrefix reaches the git gateway handler, one under
// apigateway.PathPrefix reaches the API gateway handler, and anything else
// 404s — without either underlying gateway needing to know the other exists.
func TestCombinedGatewayHandler_RoutesByPathPrefix(t *testing.T) {
	gitReg := gitgateway.NewRegistry()
	gitSrv := gitgateway.NewServer(gitReg, nil, nil)

	apiReg := apigateway.NewRegistry()
	apiSrv := apigateway.NewServer(apiReg, apigateway.NewCredentialProvider(nil, nil), nil, nil)

	h := &combinedGatewayHandler{git: gitSrv, api: apiSrv}

	cases := []struct {
		name       string
		path       string
		wantStatus int
	}{
		{
			name: "git gateway prefix reaches gitgateway.Server (unknown token -> 401)",
			path: gitgateway.PathPrefix + "bogus-token/github.com/owner/repo/info/refs?service=git-upload-pack",
			// gitgateway.Server.ServeHTTP: unknown token -> 401.
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "api gateway prefix reaches apigateway.Server (unknown token -> 401)",
			path: apigateway.PathPrefix + "bogus-token/myapp/v1/users",
			// apigateway.Server.ServeHTTP: unknown token -> 401.
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "unmatched path 404s without reaching either gateway",
			path:       "/",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "unmatched path (neither prefix) 404s",
			path:       "/healthz",
			wantStatus: http.StatusNotFound,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", c.path, nil)
			h.ServeHTTP(w, req)
			if w.Code != c.wantStatus {
				t.Errorf("path %q: status = %d, want %d (body %q)", c.path, w.Code, c.wantStatus, w.Body.String())
			}
		})
	}
}

// TestCombinedGatewayHandler_NilSubHandlersFailClosed pins the defensive nil
// guard: a combinedGatewayHandler with a nil git or api field (should never
// happen in production — wire.go always constructs both) 404s the matching
// prefix rather than panicking with a nil-pointer dereference.
func TestCombinedGatewayHandler_NilSubHandlersFailClosed(t *testing.T) {
	h := &combinedGatewayHandler{}

	for _, path := range []string{
		gitgateway.PathPrefix + "tok/github.com/owner/repo/info/refs?service=git-upload-pack",
		apigateway.PathPrefix + "tok/myapp/v1/users",
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("path %q with nil sub-handler: status = %d, want 404 (fail closed, not panic)", path, w.Code)
		}
	}
}
