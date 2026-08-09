package apiwire

import (
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ReplayHookResult is the output of a hook replay.
type ReplayHookResult struct {
	Task        *orchestrator.Task        `json:"task"`
	FiredEvents []orchestrator.FiredEvent `json:"fired_events,omitempty"`
}

// WorkspaceDetail is the response shape for the workspace create/show/update
// endpoints: the parsed meta plus enough bookkeeping (revision, assigned
// project ids) for a caller to display or re-PUT it with the correct
// If-Match header. docs/plans/workspace-db-consolidation.md Step C/D/E.
type WorkspaceDetail struct {
	Slug     string                      `json:"slug"`
	Meta     *orchestrator.WorkspaceMeta `json:"meta"`
	Revision string                      `json:"revision,omitempty"`
	// ProjectCount mirrors len(AssignedProjects); kept as its own field so
	// callers that only need the count (e.g. a future list-style summary
	// view) don't need to len() the slice themselves.
	ProjectCount     int      `json:"project_count"`
	AssignedProjects []string `json:"assigned_projects"`
	// Home reports the workspace home VOLUME's size, read from the engine's
	// own disk-usage accounting (docs/plans/workspace-home-volume-persistence.md
	// 論点 a-2, PR7 — it was a directory-tree walk under Phase 4 PR5, before
	// the home became a docker named volume). Populated only by GET
	// /api/workspaces/{slug} (WorkspaceHandler.Show) — Create/Update/Import
	// leave it nil, since reading it costs an engine round trip and none of
	// those callers need it. nil (omitted from the JSON body) when the
	// handler has no home store, i.e. no engine handle to ask.
	Home *WorkspaceHomeSize `json:"home,omitempty"`
}

type ImportError struct {
	Line     int    `json:"line"`
	RemoteID string `json:"remote_id"`
	Error    string `json:"error"`
}

type ImportResult struct {
	Created int           `json:"created"`
	Skipped int           `json:"skipped"`
	Errors  []ImportError `json:"errors"`
}

// StartSessionRequest is the body of POST /api/sessions and
// POST /api/projects/{id}/sessions. Phase 3-d (PR1) introduced the session
// concept as a first-class JobKind so the WebUI [New Session] dialog and
// the `boid agent` CLI share one daemon entry point.
type StartSessionRequest struct {
	// ProjectID names the project whose traits the session inherits. For the
	// project-scoped route (`/api/projects/{id}/sessions`) it is taken from
	// the URL and the body field is ignored.
	ProjectID string `json:"project_id"`

	// HarnessType selects the agent adapter. Must be one of "claude",
	// "codex", or "opencode". The historical "shell" session variant was
	// retired after the git gateway cutover — `boid exec -p <project> -- bash`
	// runs the shell adapter through the same Runner.Dispatch() with an
	// interactive PTY, so there is no session use case left for it.
	// sessionDispatcherAdapter.StartSession rejects any other value at the
	// API boundary.
	HarnessType string `json:"harness_type"`

	// Instruction is the optional bootstrap prompt for the first turn. Empty
	// leaves the harness to pick its default (no positional for session mode
	// on claude, since /boid-task is meaningless without a task.yaml).
	Instruction string `json:"instruction,omitempty"`

	// Readonly, when true, mounts the project workspace read-only. Sessions
	// default to writable (developer ergonomics > fail-safety).
	Readonly bool `json:"readonly,omitempty"`

	// Model overrides the harness binary's default model selection.
	Model string `json:"model,omitempty"`

	// DisplayName is the human-readable session label persisted to
	// jobs.display_name. Empty falls back to "<harness> session".
	DisplayName string `json:"display_name,omitempty"`
}

// StartSessionResult is the response shape for POST /api/sessions and the
// project-scoped variant.
type StartSessionResult struct {
	JobID     string `json:"job_id"`
	AttachURL string `json:"attach_url"`
}

// StartExecRequest is the body of POST /api/projects/{id}/exec. `boid exec`
// used to be a client-side-only path (the CLI built its own SandboxRuntimeInfo
// and syscall.Exec'd straight into the sandbox launcher), which is exactly why
// it never picked up the git gateway cutover's Dispatch()-only wiring
// (registerGatewayToken / GatewayURL / GatewayCloneURL) — see
// docs/plans/git-gateway-cutover.md. This request type is the daemon-side
// entry point that routes exec through the same Runner.Dispatch() path as
// every session, so any future dispatch-time wiring lands on both by
// construction instead of needing a second, easy-to-forget call site.
//
// Unlike sessions (fixed harness_type, agent-driven argv), exec runs an
// arbitrary user-supplied argv with no HarnessAdapter agent — see
// dispatcher.BuildExecJobSpec, which forces HarnessType="shell" underneath.
type StartExecRequest struct {
	// ProjectID is taken from the URL for the project-scoped route; there is
	// no top-level /api/exec (every exec is inherently project-scoped —
	// `boid exec -p <ref> -- argv...`), so the handler always fills this in
	// from chi.URLParam before it reaches the dispatcher.
	ProjectID string `json:"project_id"`

	// Argv is the literal program + arguments to run inside the sandbox.
	// Required, non-empty.
	Argv []string `json:"argv"`

	// Readonly, when true, mounts the project workspace read-only. Exec
	// defaults to writable, matching the CLI's --readonly flag default.
	Readonly bool `json:"readonly,omitempty"`

	// Interactive requests a PTY-backed sandbox. The CLI computes this from
	// isatty(stdin) && isatty(stdout) (see cmd/exec.go) — both, not stdin
	// alone, because a PTY is only correct when the whole terminal round-trip
	// is real; `boid exec -- cmd | grep pattern` must NOT get a PTY even
	// though its own stdin is a real terminal, or the PTY's line-discipline
	// framing would corrupt the piped bytes grep receives. false selects the
	// plain-pipe transport (see runtime_local_linux.go's non-interactive
	// branch and its StdinForward stdin-piping addition).
	Interactive bool `json:"interactive,omitempty"`

	// DisplayName is the human-readable label persisted to jobs.display_name.
	// Empty falls back to argv[0] (dispatcher.BuildExecJobSpec).
	DisplayName string `json:"display_name,omitempty"`
}

// StartExecResult is the response shape for POST /api/projects/{id}/exec.
type StartExecResult struct {
	JobID     string `json:"job_id"`
	AttachURL string `json:"attach_url"`
}
