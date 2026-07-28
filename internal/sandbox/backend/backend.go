// Package backend defines the SandboxBackend/SandboxSession seam that
// separates "how a sandboxed agent process is launched and attached to"
// from the dispatcher's job orchestration.
//
// Phase 6 (docs/plans/phase6-container-backend.md) introduced it as an inert
// refactor: §PR1 extracted the then-only implementation, the userns backend
// (internal/dispatcher's usernsBackend/usernsSession), behind this interface so
// that a container backend could be added without touching the
// attach/resize/signal call sites.
//
// Both halves of that history are over. The container backend landed in §PR5,
// and PR-4 of docs/plans/volume-only-daemon.md (§論点e) then removed the userns
// backend outright, so internal/dispatcher's containerBackend is the ONLY
// implementation — the seam now earns its keep by keeping the ingress call
// sites (WS attach, the SSE follow endpoint, the resize route, `boid agent
// stop`) independent of docker rather than by hosting two backends.
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

// RuntimeExit reports how a sandboxed session's underlying process
// finished. Canonical definition — dispatcher.RuntimeExit is a type alias
// to this (see internal/dispatcher/runtime.go).
type RuntimeExit struct {
	ExitCode int
	// TranscriptPath is the file holding the child process's captured
	// stdout/stderr, when the backend supports it. Empty when unsupported.
	TranscriptPath string
	// EngineError carries the container ENGINE's own failure message when
	// the engine itself could not report an exit status for this session —
	// e.g. the ContainerWait API call failing outright, or its response
	// carrying an engine-side Error instead of a real StatusCode (see
	// waitResponseEngineError's doc comment in
	// internal/dispatcher/container_backend_workspace_init.go for why
	// StatusCode alone cannot be trusted in that case).
	//
	// This is NOT the exited process's own stderr or exit reason — ExitCode
	// above is a forced substitute value (containerSession.waitLoop sets it
	// to 1) precisely because the engine never filled one in. EngineError is
	// the only description of what actually went wrong, and the empty
	// string is the explicit "the engine reported a normal exit; ExitCode is
	// real" case — a caller (Runner.watchRuntime, in particular) must not
	// report a hook script as having failed on its own when this is set;
	// see watchRuntime's doc comment for how it renders the distinction.
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
	// unknown" — rather than the label being omitted entirely). Callers
	// should pass this explicitly rather than relying on a backend
	// inferring it from spec.Env: an env-var lookup is an implicit,
	// fragile routing path for something a label's reap/observability
	// consumers need to be able to trust is always present (PR5 review
	// Minor finding).
	Workspace string

	// WorkspaceSlug is the NORMALIZED workspace slug — what
	// dispatcher.resolveWorkspaceHome turned Workspace into, which for a
	// project with no explicit workspace assignment is the default slug rather
	// than the empty string Workspace carries.
	//
	// It is a second field rather than a normalization of the first because the
	// two answer different questions and PR6 of
	// docs/plans/workspace-home-volume-persistence.md (論点 D5) needed only one
	// of them changed. Workspace decides the boid.workspace label and whether
	// the job gets a per-workspace isolated network at all (empty means "no
	// workspace network"), so normalizing it would silently move every
	// unassigned project onto a `default` workspace network — a real behaviour
	// change to network isolation, unrelated to workspace homes.
	// WorkspaceSlug identifies the workspace whose persistent HOME volume this
	// job mounts, and that volume's NAME is already built from the normalized
	// slug; containerBackend stamps this value onto the volume's
	// boid.workspace_home label so name and label agree.
	//
	// Empty is tolerated the same way Workspace's empty is: the label is set to
	// it explicitly rather than omitted. Callers that resolve a workspace home
	// always have the value — it is resolveWorkspaceHome's second return.
	WorkspaceSlug string

	// WorkspaceHomeID is the identity dispatcher.resolveWorkspaceHome
	// OBSERVED on this workspace's HOME volume during this dispatch — the
	// value of its dockerres.LabelWorkspaceHomeID label, and the value the
	// completion marker that let init be skipped was compared against.
	//
	// It is threaded down here so the backend can check, at the moment it is
	// about to hand the volume to a container, that it is still the same
	// volume. Resolving and mounting are two engine calls apart, and in
	// between the volume can be removed — by an operator, by a reap misfire,
	// by a half-completed workspace remove — after which the mount silently
	// gets a BRAND NEW, empty, never-initialized home (the engine creates a
	// missing volume implicitly at container create, unlabelled). The
	// resolver's own check cannot see that far forward; only the caller of
	// VolumeCreate can, by comparing what it got back against this value.
	//
	// Empty means "no identity was resolved for this launch", which production
	// never produces (Runner.Dispatch resolves the home before it builds the
	// spec that mounts it) but DI/test wiring that calls Launch directly does.
	// The backend then falls back to its pre-PR6 behaviour of minting an
	// identity for a volume it has to create; see
	// containerBackend.ensureNamedVolumes for why that is not a silent
	// weakening of the check.
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
	// dispatcher.JobSpec's field of the same name (docs/plans/
	// phase6-container-backend.md §PR6, §決定5): true when the job declared
	// capabilities.docker. containerBackend uses this to decide whether to
	// issue a per-job dockerproxy client cert and deliver it via
	// DOCKER_HOST/DOCKER_CERT_PATH/DOCKER_TLS_VERIFY env (see
	// ContainerBackendOptions.DockerTLSCA's doc comment).
	//
	// Runner.launchSandbox fills it from the *orchestrator.JobSpec on every
	// dispatch. It did not always: through Phase 6 PR6 this field (and
	// Workspace) were left at their zero value on every real Dispatch, which
	// silently disabled per-job docker-capability delivery and workspace
	// network isolation, and nothing noticed because the userns backend of the
	// day read neither. PR9 closed that gap; it is named here because "the
	// struct has the field" and "the caller fills it in" are separate facts.
	DockerEnabled bool
}

// ReapReport is the per-task result of a ReapOrphans pass: which jobs were
// successfully reconciled and which failed, so the caller can decide
// task-by-task whether to auto-reopen. A single GlobalError can't express
// "skip reopen for just the jobs reap failed on" — see
// docs/plans/phase6-container-backend.md §決定 6/8.
type ReapReport struct {
	ReapedJobIDs []string
	FailedJobIDs []string
	GlobalError  error
}

// SandboxBackend launches and re-attaches to sandboxed agent sessions. The
// only implementation is internal/dispatcher's containerBackend, which creates
// one throwaway docker container per job; the userns backend this interface was
// extracted from was removed in PR-4 of docs/plans/volume-only-daemon.md.
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
	// restart: containerBackend's label-based implementation removes orphaned
	// containers, networks and job volumes (leaving persistent workspace HOME
	// volumes alone — docs/plans/workspace-home-volume-persistence.md 論点 a).
	// It is called on every daemon startup, between MarkStaleJobsFailed /
	// task-abort and the daemon_shutdown auto-reopen sweep
	// (internal/server/wire.go's reapOrphansBeforeReopen via
	// dispatcher.Runner.ReapOrphans) — see
	// docs/plans/phase6-container-backend.md §決定 6.
	ReapOrphans(ctx context.Context) (ReapReport, error)
}

// SandboxSession is a single launched (or adopted) sandbox's live handle.
// Every attach/resize/signal ingress in the daemon — WS attach, the Web UI
// SSE follow endpoint, the HTTP resize route, and `boid agent stop`'s
// signal delivery — routes through one of these methods rather than
// reaching into a backend-specific transport directly. See
// docs/plans/phase6-container-backend.md §決定 1 for why each method has
// the exact shape it does (in particular why live attach is collapsed to
// Subscribe/WriteInput/CloseInput/Resize with no Attach(ctx, req) method:
// the pre-Phase-6 WS-attach unification already made JobRuntime.Attach
// have no external caller).
type SandboxSession interface {
	// ID returns the backend-assigned runtime identifier (what's
	// persisted as Job.RuntimeID).
	ID() string

	// Subscribe atomically returns the current output snapshot plus a
	// channel of subsequent chunks — "atomically" so no output is lost
	// between snapshot and the first live chunk. ok is false when the
	// session has no live stream to offer (already exited, or the backend
	// doesn't support streaming for this session).
	Subscribe() (snapshot []byte, ch <-chan []byte, cancel func(), ok bool)
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
