package dispatcher

import (
	"context"
	"errors"
	"io"
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
}

type JobRuntime interface {
	Start(ctx context.Context, spec RuntimeStartSpec) (*RuntimeHandle, error)
	Attach(ctx context.Context, runtimeID string, req RuntimeAttachRequest) error
	Resize(ctx context.Context, runtimeID string, size TerminalSize) error
	Wait(ctx context.Context, runtimeID string) (RuntimeExit, error)
	Stop(ctx context.Context, runtimeID string) error
}
