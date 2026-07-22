// Package backend defines the SandboxBackend/SandboxSession seam that
// separates "how a sandboxed agent process is launched and attached to"
// from the dispatcher's job orchestration.
//
// Phase 6 (docs/plans/phase6-container-backend.md) introduces this
// interface so a second backend (container/docker, PR5) can be added
// later without touching the attach/resize/signal call sites that already
// exist for the current userns backend. PR1 (§PR1 / §決定 1 of the plan
// doc) only extracts the existing userns implementation behind this
// interface — internal/dispatcher's usernsBackend/usernsSession — and
// rewires the attach/resize/signal seams to go through it. Behavior is
// unchanged; this is an inert refactor.
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
}

// LaunchOptions carries the per-job parameters a backend needs to start a
// sandbox session. It mirrors dispatcher.RuntimeStartSpec's fields minus
// Command: a backend decides its own entrypoint/command internally
// (usernsBackend builds `boid runner-outer --spec ... --state ...`; a
// future containerBackend will build a docker create/start call instead).
type LaunchOptions struct {
	JobID     string
	TaskID    string
	ProjectID string
	HandlerID string
	Role      string

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
// current (and, until Phase 6 PR5, only) implementation is the userns
// backend (internal/dispatcher's usernsBackend), which wraps
// SandboxPreparer + `boid runner-outer` + JobRuntime — see
// docs/plans/phase6-container-backend.md §PR1.
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
	// restart. PR1 stubs this (returns a zero ReapReport, nil error); the
	// real implementation lands in PR7, wired into the startup
	// MarkStale*/auto-reopen sequence.
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
