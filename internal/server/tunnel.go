package server

import (
	"encoding/json"
	"fmt"
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

// tunnelDescription is the agent-facing description boid sends in hello;
// the %q is the workspace the tunnel reaches.
const tunnelDescription = `boid: the owner's personal sandbox runner on their own machine. This connection
reaches only the boid workspace %q: its projects and its jobs. All responses are JSON.

Start a job (an agent working in a project's repository, in its own sandbox) and read its result:
- GET  /projects                             projects a job can run in (id, name, repository)
- POST /jobs                                 {"project": "<id or name>", "instruction": "...",
                                              "harness": "claude|codex|opencode" (default claude),
                                              "readonly": false, "model": "", "name": ""}
                                             answers 202 {"id", "result_url"} at once; the job runs on
- GET  /jobs/<id>/result                     status (running|completed|failed) and, once done, the result
                                             {"summary", "questions", "artifacts"} the agent wrote
- POST /jobs/<id>/stop                       stop a running job

A job does not wait for answers: questions come back in the result, and a follow-up is a new job
with the answer in its instruction. Poll the result every minute or so; jobs take minutes.

Look around:
- GET /jobs?status=<running|completed|failed>&limit=<n>
                                             recent jobs, newest first (limit 20 by default, at most 100)
- GET /jobs/<id>                             one job (status, timestamps, output)
- GET /jobs/<id>/log?tail=<bytes>            the end of the job's terminal transcript (raw text with
                                             ANSI escapes; 256 KiB by default, at most 4 MiB)`

func tunnelDescriptionFor(workspace string) string {
	return fmt.Sprintf(tunnelDescription, workspace)
}

// newTunnelSet returns the daemon's per-workspace tunnels, or nil when there
// is no data home to keep the keys in.
func newTunnelSet(cfg Config) *tunnel.Set {
	home := dataHomeFor(cfg)
	if home == "" {
		return nil
	}
	return &tunnel.Set{
		Dir:         filepath.Join(home, "tunnels"),
		LegacyPath:  filepath.Join(home, "tunnel.json"),
		Description: tunnelDescriptionFor,
		Version:     "boid " + version.Version(),
	}
}

// newTunnelRelay lets jobs in callbackWorkspaces call their own workspace's
// gateway back with their API gateway job token.
func newTunnelRelay(s *tunnel.Set, registry *apigateway.Registry, callbackWorkspaces []string) *tunnel.Relay {
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
		Caller:         s.Caller,
	}
}

// tunnelDeps is what the tunnel surface reaches inside the daemon.
type tunnelDeps struct {
	Jobs     *api.JobHandler
	Projects tunnelProjects
	Sessions tunnelSessions
}

// tunnelSurface is everything the owner reaches through one workspace's
// tunnel: that workspace's jobs and projects. Other workspaces and the rest
// of the daemon API (secrets, config, tasks) are deliberately absent.
func tunnelSurface(d tunnelDeps, workspace string) http.Handler {
	jobs := d.Jobs
	r := chi.NewRouter()
	r.Get("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(tunnelDescriptionFor(workspace) + "\n"))
	})
	r.Get("/jobs", func(w http.ResponseWriter, req *http.Request) { listRecentJobs(w, req, jobs.Global, workspace) })
	r.Route("/jobs/{id}", func(r chi.Router) {
		r.Use(jobInWorkspace(jobs.Jobs, workspace))
		r.Get("/", jobs.Get)
		if jobs.LogReader != nil {
			r.Get("/log", func(w http.ResponseWriter, req *http.Request) { tailJobLog(w, req, jobs.Log) })
		}
		r.Get("/result", func(w http.ResponseWriter, req *http.Request) { tunnelJobResult(w, req, jobs.Jobs) })
		if jobs.Service != nil {
			r.Post("/stop", jobs.AgentStop)
		}
	})
	if d.Projects != nil {
		r.Get("/projects", func(w http.ResponseWriter, req *http.Request) { listTunnelProjects(w, req, d.Projects, workspace) })
		if d.Sessions != nil {
			r.Post("/jobs", func(w http.ResponseWriter, req *http.Request) {
				startTunnelJob(w, req, d.Projects, d.Sessions, workspace)
			})
		}
	}
	return r
}

// jobInWorkspace answers 404 for a job of another workspace, as if it did not exist.
func jobInWorkspace(store api.JobStore, workspace string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if store == nil {
				writeTunnelError(w, http.StatusNotFound, "job not found")
				return
			}
			j, err := store.GetJob(chi.URLParam(r, "id"))
			if err != nil || j.WorkspaceID != workspace {
				writeTunnelError(w, http.StatusNotFound, "job not found")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

const (
	tunnelJobsDefaultLimit = 20
	tunnelJobsMaxLimit     = 100
	tunnelJobOutputChars   = 500
)

// listRecentJobs returns the newest jobs, small enough for a tunnel frame.
func listRecentJobs(w http.ResponseWriter, r *http.Request, store api.GlobalJobStore, workspace string) {
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
	out := make([]api.JobWithContext, 0, limit)
	for _, j := range all {
		if j.WorkspaceID == workspace {
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
