package dispatcher

import (
	"context"
	"errors"
	"io"
	"syscall"

	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

var ErrRuntimeUnsupported = errors.New("job runtime operation is not supported")

// TerminalSize is an alias for backend.TerminalSize. The canonical
// definition lives in internal/sandbox/backend (docs/plans/
// phase6-container-backend.md §PR1) so SandboxSession.Resize and
// JobRuntime.Resize below share one type without an import cycle — backend
// has no dependency on dispatcher. Every existing call site keeps
// compiling and behaving unchanged.
type TerminalSize = backend.TerminalSize

type RuntimeStartSpec struct {
	JobID       string
	TaskID      string
	ProjectID   string
	HandlerID   string
	Role        string
	Command     string
	Interactive bool
	TTY         bool
	// StdinForward requests a dedicated stdin pipe for a non-interactive
	// (Interactive=false) session, so a later Attach's RuntimeAttachRequest.Input
	// can feed real bytes to the child process — `boid exec` piped from a
	// non-TTY stdin (e.g. `echo hi | boid exec cat`) needs this; a hook job
	// never does. False (the default, every hook job) keeps stdin on the null
	// device exactly as before: a hook script that probes stdin must keep
	// seeing an immediate EOF, not block forever waiting for a forwarder that
	// will never attach. Ignored when Interactive is true — PTY sessions
	// always support input via the PTY master, forwarding or not.
	StdinForward bool
	// DesiredID, when non-empty, asks the runtime to use this UUID as its
	// session identifier instead of generating a fresh one. The caller uses
	// this to pre-allocate a runtime directory (e.g. for a per-sandbox docker
	// proxy socket) before Start is called. The runtime honours the request
	// on a best-effort basis: if the directory already exists or the ID is
	// otherwise unusable, Start returns an error.
	DesiredID string
}

type RuntimeHandle struct {
	ID          string
	Interactive bool
	TTY         bool
}

type RuntimeAttachRequest struct {
	Input  io.Reader
	Output io.Writer
	Error  io.Writer
}

// RuntimeExit is an alias for backend.RuntimeExit (same rationale as
// TerminalSize above). ExitCode is the process exit code; TranscriptPath
// is the path to a file holding the child process's stdout/stderr full
// capture, so a silent exit_code!=0 (transcript が 0 byte) ケースを diag
// log で一発判別できる。サポートしない runtime は空文字。
type RuntimeExit = backend.RuntimeExit

// Deprecated: retiring in a follow-up PR after container-backend dogfood
// stability, alongside usernsBackend (docs/plans/phase6-cutover-followups.md
// §「userns backend 撤去」) — JobRuntime is usernsBackend's internal process-
// transport seam (its only production implementation is LocalRuntime), kept
// in production use unchanged as of Phase 6 PR9's documentation-only
// marker.
type JobRuntime interface {
	Start(ctx context.Context, spec RuntimeStartSpec) (*RuntimeHandle, error)
	Attach(ctx context.Context, runtimeID string, req RuntimeAttachRequest) error
	Resize(ctx context.Context, runtimeID string, size TerminalSize) error
	Wait(ctx context.Context, runtimeID string) (RuntimeExit, error)
	Stop(ctx context.Context, runtimeID string) error
	// Signal sends a single signal to the runtime's process group without
	// any follow-up SIGKILL. Used by NotifyTask to drive an "agent-stop"
	// SIGUSR1 to run-agent.py while leaving the runner chain intact: the
	// go-native runner subcommands set this signal to SIG_IGN (see
	// runner.ignoreStopSignal), which is inherited across execve so pasta and
	// the child runners survive while run-agent.py re-installs its own handler.
	// Implementations should be no-op when the runtime has already exited.
	Signal(ctx context.Context, runtimeID string, sig syscall.Signal) error
}
