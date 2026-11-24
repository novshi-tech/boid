package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/apigateway"
	"github.com/novshi-tech/boid/internal/tunnel"
	"github.com/novshi-tech/boid/internal/version"
)

// tunnelDescription is the agent-facing description boid sends in hello.
const tunnelDescription = `boid: the owner's personal sandbox runner on their own machine.
Read-only for now. All responses are JSON.

- GET /jobs?status=<running|completed|failed>&workspace=<id>&limit=<n>
                                             recent jobs, newest first (limit 20 by default, at most 100)
- GET /jobs/<id>                             one job (status, workspace, task, timestamps)
- GET /jobs/<id>/log?tail=<bytes>            the end of the job's terminal transcript (raw text with
                                             ANSI escapes; 256 KiB by default, at most 4 MiB)

Starting jobs over the tunnel is not available yet.`

// newTunnelManager returns the daemon's tunnel, or nil when there is no
// data home to keep the key in.
func newTunnelManager(cfg Config) *tunnel.Manager {
	home := dataHomeFor(cfg)
	if home == "" {
		return nil
	}
	return &tunnel.Manager{
		IdentityPath: filepath.Join(home, "tunnel.json"),
		Description:  tunnelDescription,
		Version:      "boid " + version.Version(),
	}
}

// newTunnelRelay lets jobs in callbackWorkspaces call the gateway back with
// their API gateway job token.
func newTunnelRelay(m *tunnel.Manager, registry *apigateway.Registry, callbackWorkspaces []string) *tunnel.Relay {
	allowed := make(map[string]bool, len(callbackWorkspaces))
	for _, ws := range callbackWorkspaces {
		allowed[ws] = true
	}
	return &tunnel.Relay{
		Lookup: func(token string) (tunnel.JobGrant, bool) {
			e, ok := registry.Lookup(token)
			if !ok {
				return tunnel.JobGrant{}, false
			}
			return tunnel.JobGrant{WorkspaceID: e.Namespace, JobRef: e.TaskID, ReadOnly: e.ReadOnly}, true
		},
		AllowWorkspace: func(ws string) bool { return ws != "" && allowed[ws] },
		Caller:         m.Caller,
	}
}

// tunnelSurface is everything the owner reaches through the tunnel: a
// read-only view of jobs. The rest of the daemon API (secrets, config,
// workspaces) is deliberately absent.
func tunnelSurface(jobs *api.JobHandler) http.Handler {
	r := chi.NewRouter()
	r.Get("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(tunnelDescription + "\n"))
	})
	r.Get("/jobs", func(w http.ResponseWriter, req *http.Request) { listRecentJobs(w, req, jobs.Global) })
	r.Get("/jobs/{id}", jobs.Get)
	if jobs.LogReader != nil {
		r.Get("/jobs/{id}/log", func(w http.ResponseWriter, req *http.Request) { tailJobLog(w, req, jobs.Log) })
	}
	return r
}

const (
	tunnelJobsDefaultLimit = 20
	tunnelJobsMaxLimit     = 100
	tunnelJobOutputChars   = 500
)

// listRecentJobs returns the newest jobs, small enough for a tunnel frame.
func listRecentJobs(w http.ResponseWriter, r *http.Request, store api.GlobalJobStore) {
	w.Header().Set("Content-Type", "application/json")
	if store == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"job listing is not available"}`))
		return
	}
	q := r.URL.Query()
	limit := tunnelJobsDefaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"limit must be a positive number"}`))
			return
		}
		limit = min(n, tunnelJobsMaxLimit)
	}
	all, err := store.ListJobsWithContext(api.JobListFilter{Status: q.Get("status")})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	workspace := q.Get("workspace")
	out := make([]api.JobWithContext, 0, limit)
	for _, j := range all {
		if workspace == "" || j.WorkspaceID == workspace {
			out = append(out, j)
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].CreatedAt.After(out[b].CreatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	for i := range out {
		if r := []rune(out[i].Output); len(r) > tunnelJobOutputChars {
			out[i].Output = string(r[:tunnelJobOutputChars]) + "…"
		}
	}
	_ = json.NewEncoder(w).Encode(out)
}

const (
	tunnelLogDefaultTail = 256 << 10
	tunnelLogMaxTail     = 4 << 20
)

// logCapture buffers a handler's response so the log can be cut to its tail.
type logCapture struct {
	header http.Header
	status int
	body   []byte
}

func (c *logCapture) Header() http.Header { return c.header }
func (c *logCapture) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}
func (c *logCapture) Write(p []byte) (int, error) {
	c.WriteHeader(http.StatusOK)
	c.body = append(c.body, p...)
	return len(p), nil
}

// tailJobLog serves only the last bytes of a transcript, so a long log fits a tunnel frame.
func tailJobLog(w http.ResponseWriter, r *http.Request, log http.HandlerFunc) {
	tail := tunnelLogDefaultTail
	if v := r.URL.Query().Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			http.Error(w, "tail must be a positive number of bytes", http.StatusBadRequest)
			return
		}
		tail = min(n, tunnelLogMaxTail)
	}
	c := &logCapture{header: http.Header{}}
	log(c, r)
	for k, v := range c.header {
		w.Header()[k] = v
	}
	body := c.body
	if c.status == http.StatusOK && len(body) > tail {
		w.Header().Set("X-Boid-Log-Truncated", strconv.Itoa(len(body)-tail))
		body = body[len(body)-tail:]
	}
	if c.status == 0 {
		c.status = http.StatusOK
	}
	w.WriteHeader(c.status)
	_, _ = w.Write(body)
}
