package dispatcher

import (
	"errors"

	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

var ErrRuntimeUnsupported = errors.New("job runtime operation is not supported")

// TerminalSize aliases backend.TerminalSize so this package's callers share
// one type without an import cycle (backend does not depend on dispatcher).
type TerminalSize = backend.TerminalSize

// RuntimeSnapshot aliases backend.RuntimeSnapshot, also keeping internal/api
// (which consumes it via RuntimeSubscriber) free of a direct backend dependency.
type RuntimeSnapshot = backend.RuntimeSnapshot

// RuntimeExit aliases backend.RuntimeExit. TranscriptPath points to the
// child process's full stdout/stderr capture, so a silent nonzero exit can
// still be diagnosed; runtimes that don't support it return an empty string.
type RuntimeExit = backend.RuntimeExit
