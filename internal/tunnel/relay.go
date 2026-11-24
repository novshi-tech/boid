package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// RelayPathPrefix is where sandboxes call the gateway back:
// /heddleway/<job-token>/<service>/<path...>.
const RelayPathPrefix = "/heddleway/"

// JobGrant is what a job token allows on the relay.
type JobGrant struct {
	WorkspaceID string
	JobRef      string
	ReadOnly    bool
}

// Caller is the part of Client the relay needs.
type Caller interface {
	Call(ctx context.Context, method, service, path string, headers [][2]string, body []byte) (*CallResult, error)
}

// Relay lets sandboxed jobs call the gateway as the owner without holding
// the tunnel key: the job token picks the workspace, and only workspaces in
// AllowWorkspace may call back.
type Relay struct {
	Lookup         func(token string) (JobGrant, bool)
	AllowWorkspace func(workspaceID string) bool
	Caller         func() Caller
}

var serviceName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

var droppedRequestHeaders = map[string]bool{
	"authorization": true, "cookie": true, "origin": true, "proxy-authorization": true,
	"host": true, "connection": true, "content-length": true, "transfer-encoding": true,
	"keep-alive": true, "upgrade": true, "te": true, "trailer": true,
}

func relayError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// ServeHTTP relays /heddleway/<token>/<service>/<path> to the gateway.
func (rl *Relay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != "" {
		relayError(w, http.StatusForbidden, "browser requests are not relayed")
		return
	}
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), RelayPathPrefix)
	if !ok {
		http.NotFound(w, r)
		return
	}
	token, rest, _ := strings.Cut(rest, "/")
	service, tail, _ := strings.Cut(rest, "/")
	token, err := url.PathUnescape(token)
	if err != nil || token == "" {
		http.NotFound(w, r)
		return
	}
	grant, ok := rl.Lookup(token)
	if !ok {
		relayError(w, http.StatusUnauthorized, "invalid or expired job token")
		return
	}
	if !serviceName.MatchString(service) {
		relayError(w, http.StatusNotFound, "call "+RelayPathPrefix+"<token>/<service>/...")
		return
	}
	if rl.AllowWorkspace == nil || !rl.AllowWorkspace(grant.WorkspaceID) {
		relayError(w, http.StatusForbidden, "this workspace may not call the gateway back (tunnel.callback_workspaces)")
		return
	}
	if grant.ReadOnly && r.Method != http.MethodGet && r.Method != http.MethodHead {
		relayError(w, http.StatusForbidden, "a read-only job may only use GET/HEAD")
		return
	}
	var caller Caller
	if rl.Caller != nil {
		caller = rl.Caller()
	}
	if caller == nil {
		relayError(w, http.StatusServiceUnavailable, "boid is not connected to the gateway (boid tunnel status)")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		relayError(w, http.StatusRequestEntityTooLarge, "the request body is larger than the tunnel allows")
		return
	}
	path := "/" + tail
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	headers := [][2]string{}
	for name, values := range r.Header {
		lower := strings.ToLower(name)
		if droppedRequestHeaders[lower] {
			continue
		}
		for _, v := range values {
			headers = append(headers, [2]string{lower, v})
		}
	}
	headers = append(headers, [2]string{"x-boid-job", grant.JobRef})

	res, err := caller.Call(r.Context(), r.Method, service, path, headers, body)
	switch {
	case errors.Is(err, ErrNotConnected):
		relayError(w, http.StatusServiceUnavailable, "boid is not connected to the gateway (boid tunnel status)")
		return
	case errors.Is(err, ErrTooManyCalls):
		relayError(w, http.StatusTooManyRequests, "too many calls to the gateway in flight")
		return
	case err != nil:
		relayError(w, http.StatusBadGateway, "the call to the gateway failed")
		return
	}
	for _, h := range res.Headers {
		if hopByHop[strings.ToLower(h[0])] || strings.EqualFold(h[0], "content-encoding") {
			continue
		}
		w.Header().Add(h[0], h[1])
	}
	w.WriteHeader(res.Status)
	_, _ = w.Write(res.Body)
}
