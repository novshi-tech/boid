package server

import (
	"net/http"
	"strings"

	"github.com/novshi-tech/boid/internal/apigateway"
	"github.com/novshi-tech/boid/internal/gitgateway"
)

// combinedGatewayHandler dispatches a single shared listener's requests to
// either the git gateway or the API gateway by path prefix (docs/plans/
// api-gateway.md 論点1: "listener を git gateway と同居させるか別ポートか —
// 確定: 同居 (path prefix `/j/` と `/api/` で分岐)。mTLS listener も流用").
//
// Neither gateway package imports the other, and neither knows this wrapper
// exists — internal/server, the daemon's composition root, is where the two
// subsystems' listener sharing is decided, the same way it already composes
// the chi router with transport-aware auth middleware for the plain HTTP
// API (mountRoutes' tcpHandler) rather than baking that concern into
// internal/api itself.
type combinedGatewayHandler struct {
	git *gitgateway.Server
	api *apigateway.Server
}

// ServeHTTP implements http.Handler. A path matching neither gateway's
// prefix (or a gateway that is nil — defensive; wire.go always constructs
// both) gets a plain 404, the same "unrecognized route" contract each
// gateway's own ServeHTTP already gives an unmatched path within its own
// prefix.
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
