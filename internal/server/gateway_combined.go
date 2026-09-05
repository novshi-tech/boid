package server

import (
	"net/http"
	"strings"

	"github.com/novshi-tech/boid/internal/apigateway"
	"github.com/novshi-tech/boid/internal/gitgateway"
)

// combinedGatewayHandler dispatches a single shared listener's requests to
// either the git gateway or the API gateway by path prefix. See docs/plans/
// api-gateway.md for why the two share a listener instead of separate ports.
type combinedGatewayHandler struct {
	git *gitgateway.Server
	api *apigateway.Server
}

// ServeHTTP implements http.Handler, routing by path prefix and 404ing
// anything that matches neither gateway.
func (h *combinedGatewayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, gitgateway.PathPrefix):
		if h.git == nil {
			http.NotFound(w, r)
			return
		}
		h.git.ServeHTTP(w, r)
	case strings.HasPrefix(r.URL.Path, apigateway.PathPrefix):
		if h.api == nil {
			http.NotFound(w, r)
			return
		}
		h.api.ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}
