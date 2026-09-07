package apigateway

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

// discoveryResponse is what GET /api/<job-token> returns: the authorization
// carried by that one token.
//
// JSON tags are a wire contract sandboxed callers depend on — treat a field
// rename or removal as breaking.
type discoveryResponse struct {
	ReadOnly bool               `json:"readonly"`
	Services []discoveryService `json:"services"`
}

// discoveryService is one reachable service: the name to put in the request
// path, and whether that name must carry an "@account" qualifier.
type discoveryService struct {
	Name            string `json:"name"`
	RequiresAccount bool   `json:"requires_account"`
}

// parseTokenOnlyPath returns the job token when reqPath is exactly
// /api/<token> or /api/<token>/ — the discovery route. It is checked before
// parsePath, which rejects both shapes for having no service segment.
func parseTokenOnlyPath(reqPath string) (string, bool) {
	if !strings.HasPrefix(reqPath, PathPrefix) {
		return "", false
	}
	token := strings.TrimSuffix(reqPath[len(PathPrefix):], "/")
	if token == "" || strings.Contains(token, "/") {
		return "", false
	}
	return token, true
}

// serveDiscovery answers the discovery route for token, reading the same
// registry entry ServeHTTP authorizes against so the two cannot disagree.
//
// Not passed to s.recorder: it forwards nothing and resolves no credential,
// so recording it would put an entry in the timeline for no external call.
func (s *Server) serveDiscovery(w http.ResponseWriter, r *http.Request, token string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed: the discovery route is read-only", http.StatusMethodNotAllowed)
		return
	}
	entry, ok := s.registry.Lookup(token)
	if !ok {
		http.Error(w, "unauthorized: invalid or expired job token", http.StatusUnauthorized)
		return
	}

	names := make([]string, 0, len(entry.Services))
	for name, allowed := range entry.Services {
		if allowed {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	resp := discoveryResponse{ReadOnly: entry.ReadOnly, Services: make([]discoveryService, 0, len(names))}
	for _, name := range names {
		resp.Services = append(resp.Services, discoveryService{
			Name:            name,
			RequiresAccount: s.credentials.RequiresAccount(name),
		})
	}

	body, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, "internal error: encode discovery response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
