package dispatcher

import (
	"context"
	"errors"
	"io"
	"syscall"
)

var ErrRuntimeUnsupported = errors.New("job runtime operation is not supported")

type TerminalSize struct {
	Rows int
	Cols int
}

type RuntimeStartSpec struct {
	JobID       string
	TaskID      string
	ProjectID   string
	HandlerID   string
	Role        string
	Command     string
	Interactive bool
	TTY         bool
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

type RuntimeExit struct {
	ExitCode int
	// TranscriptPath は子プロセスの stdout/stderr 全量を保存しているファイルへの
	// パス。 silent な exit_code!=0 (transcript が 0 byte) ケースを diag log で
	// 一発判別できるようにするために提供する。 サポートしない runtime は空文字。
	TranscriptPath string
}

type JobRuntime interface {
	Start(ctx context.Context, spec RuntimeStartSpec) (*RuntimeHandle, error)
	Attach(ctx context.Context, runtimeID string, req RuntimeAttachRequest) error
	Resize(ctx context.Context, runtimeID string, size TerminalSize) error
	Wait(ctx context.Context, runtimeID string) (RuntimeExit, error)
	Stop(ctx context.Context, runtimeID string) error
	// Signal sends a single signal to the runtime's process group without
	// any follow-up SIGKILL. Used by NotifyTask to drive an "agent-stop"
	// SIGUSR1 to run-agent.py while leaving bash / EXIT trap intact (see
	// generateOuterScript / generateInnerScript for the matching `trap ''
	// USR1`). Implementations should be no-op when the runtime has already
	// exited.
	Signal(ctx context.Context, runtimeID string, sig syscall.Signal) error
}
