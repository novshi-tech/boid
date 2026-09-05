// Package backend defines the SandboxBackend/SandboxSession seam that
// separates "how a sandboxed agent process is launched and attached to"
// from the dispatcher's job orchestration. internal/dispatcher's
// containerBackend is the only implementation; the seam keeps the ingress
// call sites (WS attach, the SSE follow endpoint, the resize route, `boid
// agent stop`) independent of docker.
package backend

import (
	"context"
	"syscall"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// TerminalSize is a PTY window size in rows/cols. This is the canonical
// definition — dispatcher.TerminalSize is a type alias to this (see
// internal/dispatcher/runtime.go) so every existing call site across the
// codebase (ws_attach.go, job_runtime_routes.go, JobRuntime.Resize, ...)
// keeps compiling and behaving unchanged, and container backend code will
// use the exact same type without a conversion.
type TerminalSize struct {
	Rows int
	Cols int
}

// RuntimeSnapshot is everything a session has emitted so far, handed to a
// caller attaching mid-session, together with what that caller needs in
// order to decide HOW to hand it on. TTY and Geometry let ingress resolve
// Raw to the screen it painted instead of replaying the full recording
// verbatim — see internal/vtsnapshot.Render, and WSAttachHandler for the
// one caller that does it.
//
// A backend that cannot report a geometry leaves Geometry zero, which
// vtsnapshot.Render reads as "use the default 80x24".
type RuntimeSnapshot struct {
	// Raw is the recorded byte stream, verbatim. It stays the canonical
	// content — the rendered form is derived from it, never stored — and
	// it is what the reconnect splice (?replay_offset) indexes into, so
	// its length is the session's transcript byte position.
	Raw []byte
	// TTY reports whether Raw came from a PTY, i.e. whether it is a
	// terminal screen recording (renderable) rather than a plain log
	// stream (which must be passed through verbatim — see
	// JobLogSSEHandler, the ingress for exactly those).
	TTY bool
	// Geometry is the session's current terminal size, the width Raw's
	// most recent frames were painted at. Meaningless when TTY is false.
	Geometry TerminalSize
}

// RuntimeExit reports how a sandboxed session's underlying process
// finished. Canonical definition — dispatcher.RuntimeExit is a type alias
// to this (see internal/dispatcher/runtime.go).
type RuntimeExit struct {
	ExitCode int
	// TranscriptPath is the file holding the child process's captured
	// stdout/stderr, when the backend supports it. Empty when unsupported.
	TranscriptPath string
	// EngineError carries the container ENGINE's own failure message when
	// the engine itself could not report an exit status for this session
	// (see waitResponseEngineError in
	// internal/dispatcher/container_backend_workspace_init.go). Not the
	// exited process's own stderr: ExitCode above is a forced substitute
	// (1) when the engine never filled one in. Empty means "no engine
	// fault was reported" — not "ExitCode is real": a RuntimeExit{} paired
	// with ErrRuntimeUnsupported also has a meaningless zero ExitCode. A
	// caller must not report a hook script as having failed on its own when
	// EngineError is set — see Runner.watchRuntime's doc comment.
	EngineError string
}

// LaunchOptions carries the per-job parameters a backend needs to start a
// sandbox session. It mirrors dispatcher.RuntimeStartSpec's fields minus
// Command: a backend decides its own entrypoint/command internally
// (containerBackend builds a docker create/start call whose image entrypoint is
// `boid runner-container`; the retired userns backend built `boid runner-outer
// --spec ... --state ...`).
type LaunchOptions struct {
	JobID     string
	TaskID    string
	ProjectID string
	HandlerID string
	Role      string

	// Workspace is the job's workspace slug, when known. containerBackend
	// stamps it onto every container as the boid.workspace label
	// unconditionally (empty is a valid, explicit value — "workspace
	// unknown" — rather than the label being omitted). Callers should pass
	// this explicitly rather than relying on a backend inferring it from
	// spec.Env.
	Workspace string

	// WorkspaceSlug is the NORMALIZED workspace slug — what
	// dispatcher.resolveWorkspaceHome turned Workspace into, which for a
	// project with no explicit workspace assignment is the default slug
	// rather than the empty string Workspace carries. Kept as a second
	// field rather than a normalization of Workspace because normalizing
	// Workspace itself would silently move every unassigned project onto a
	// `default` workspace network. WorkspaceSlug identifies the workspace
	// whose persistent HOME volume this job mounts, and that volume's name
	// is already built from the normalized slug.
	//
	// Empty is tolerated the same way Workspace's empty is. Callers that
	// resolve a workspace home always have the value.
	WorkspaceSlug string

	// WorkspaceHomeID is the identity dispatcher.resolveWorkspaceHome
	// OBSERVED on this workspace's HOME volume during this dispatch. It is
	// threaded down here so the backend can check, right before handing the
	// volume to a container, that it is still the same volume — resolving
	// and mounting are two engine calls apart, and the volume can be
	// removed in between, silently getting replaced by a brand new,
	// never-initialized one at container create.
	//
	// Empty means "no identity was resolved for this launch" (production
	// always resolves it first; only DI/test wiring calling Launch directly
	// leaves it empty). The backend then falls back to minting an identity
	// for a volume it has to create — see
	// containerBackend.ensureNamedVolumes.
	WorkspaceHomeID string

	// Interactive and TTY mirror dispatcher.RuntimeStartSpec's fields of
	// the same name — see its doc comments for the PTY-vs-pipe distinction
	// they drive.
	Interactive bool
	TTY         bool

	// DesiredID, when non-empty, asks the backend to use this ID as the
	// session/runtime identifier instead of generating a fresh one (see
	// dispatcher.RuntimeStartSpec.DesiredID's doc comment: a per-sandbox
	// docker proxy socket is pre-allocated under this ID before Launch
	// runs).
	DesiredID string

	// StdinForward requests a dedicated stdin pipe for a non-interactive
	// session (see dispatcher.RuntimeStartSpec.StdinForward's doc
	// comment).
	StdinForward bool

	// DockerEnabled mirrors orchestrator.Visibility.DockerEnabled /
	// dispatcher.JobSpec's field of the same name: true when the job
	// declared capabilities.docker. containerBackend uses this to decide
	// whether to issue a per-job dockerproxy client cert and deliver it via
	// DOCKER_HOST/DOCKER_CERT_PATH/DOCKER_TLS_VERIFY env (see
	// ContainerBackendOptions.DockerTLSCA's doc comment).
	//
	// Runner.launchSandbox fills it from the *orchestrator.JobSpec on every
	// dispatch — "the struct has the field" and "the caller fills it in"
	// are separate facts, worth naming since a backend must not assume it.
	DockerEnabled bool
}

// ReapReport is the per-task result of a ReapOrphans pass: which jobs were
// successfully reconciled and which failed, so the caller can decide
// task-by-task whether to auto-reopen. A single GlobalError can't express
// "skip reopen for just the jobs reap failed on".
type ReapReport struct {
	ReapedJobIDs []string
	FailedJobIDs []string
	GlobalError  error
}

// SandboxBackend launches and re-attaches to sandboxed agent sessions. The
// only implementation is internal/dispatcher's containerBackend, which
// creates one throwaway docker container per job.
type SandboxBackend interface {
	// Launch prepares and starts a new sandbox session for spec.
	Launch(ctx context.Context, spec sandbox.Spec, opts LaunchOptions) (SandboxSession, error)
	// Adopt reconstructs a SandboxSession handle for a runtimeID obtained
	// from a previous Launch (typically the value persisted as
	// Job.RuntimeID), for subsequent attach / resize / signal / stop calls
	// that don't have the original Launch-time state at hand. ok is false
	// when runtimeID cannot be adopted (e.g. empty, or the backend has no
	// notion of that session).
	Adopt(ctx context.Context, runtimeID string) (SandboxSession, bool)
	// ReapOrphans reconciles sandbox resources left behind by a daemon
	// restart: containerBackend's label-based implementation removes
	// orphaned containers, networks and job volumes (leaving persistent
	// workspace HOME volumes alone). Called on every daemon startup, between
	// MarkStaleJobsFailed/task-abort and the daemon_shutdown auto-reopen
	// sweep (internal/server/wire.go's reapOrphansBeforeReopen).
	ReapOrphans(ctx context.Context) (ReapReport, error)
}

// SandboxSession is a single launched (or adopted) sandbox's live handle.
// Every attach/resize/signal ingress in the daemon — WS attach, the Web UI
// SSE follow endpoint, the HTTP resize route, and `boid agent stop`'s
// signal delivery — routes through one of these methods rather than
// reaching into a backend-specific transport directly.
type SandboxSession interface {
	// ID returns the backend-assigned runtime identifier (what's
	// persisted as Job.RuntimeID).
	ID() string

	// Subscribe atomically returns the current output snapshot plus a
	// channel of subsequent chunks — "atomically" so no output is lost
	// between snapshot and the first live chunk. ok is false when the
	// session has no live stream to offer right now.
	//
	// finished is only meaningful when ok is false, and answers WHY there
	// is no stream: true means the underlying process/container has
	// actually exited (or the backend has no notion of this session) —
	// safe to treat as "the job is done". false means the process is still
	// genuinely running but this session currently has no live attach
	// stream to offer (e.g. a prior attach dropped mid-stream and hasn't
	// been re-attached yet — see containerSession.reattachIfLost). Callers
	// must not treat every ok=false as "job finished" — that reports a
	// still-running job as having exited successfully.
	Subscribe() (snapshot RuntimeSnapshot, ch <-chan []byte, cancel func(), ok bool, finished bool)
	// WriteInput forwards raw bytes to the session's input (PTY master or
	// stdin pipe, depending on session type).
	WriteInput(data []byte) error
	// CloseInput signals that no more input is coming — the counterpart to
	// a real stdin EOF for non-interactive sessions. Stdin half-close does
	// not close the output stream (current contract, preserved as-is). A
	// no-op for sessions with no notion of "closing" input (interactive
	// PTY sessions).
	CloseInput() error
	// Resize applies a new terminal size. The single collapse point for
	// both resize ingress routes (the WS "resize" frame and the HTTP
	// POST /api/jobs/{id}/resize route).
	Resize(size TerminalSize) error
	// Wait blocks until the session's process exits.
	Wait(ctx context.Context) (RuntimeExit, error)
	// Stop terminates the session.
	Stop(ctx context.Context) error
	// Signal delivers a single Unix signal to the session's process group,
	// without any SIGKILL follow-up — used for the SIGUSR1 "agent-stop"
	// request (see dispatcher.JobRuntime.Signal's doc comment for the full
	// chain: runner subcommands keep it SIG_IGN, the harness adapter is
	// the one that actually reacts to it).
	Signal(ctx context.Context, sig syscall.Signal) error
}
