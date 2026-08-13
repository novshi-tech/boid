package dispatcher

import (
	"errors"

	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

var ErrRuntimeUnsupported = errors.New("job runtime operation is not supported")

// TerminalSize is an alias for backend.TerminalSize. The canonical
// definition lives in internal/sandbox/backend (docs/plans/
// phase6-container-backend.md §PR1) so SandboxSession.Resize and every
// caller in this package share one type without an import cycle — backend
// has no dependency on dispatcher.
type TerminalSize = backend.TerminalSize

// RuntimeSnapshot is an alias for backend.RuntimeSnapshot (same rationale as
// TerminalSize above), and additionally keeps internal/api — which consumes
// it through RuntimeSubscriber — free of a direct dependency on the sandbox
// backend package.
type RuntimeSnapshot = backend.RuntimeSnapshot

// RuntimeExit is an alias for backend.RuntimeExit (same rationale as
// TerminalSize above). ExitCode is the process exit code; TranscriptPath
// is the path to a file holding the child process's stdout/stderr full
// capture, so a silent exit_code!=0 (transcript が 0 byte) ケースを diag
// log で一発判別できる。サポートしない runtime は空文字。
type RuntimeExit = backend.RuntimeExit
