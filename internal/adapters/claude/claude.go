// Package claude implements adapters.HarnessAdapter for Claude Code.
//
// Package placement: internal/adapters/claude/ (internal/adapters/ hosts the
// shared interface; each harness sub-package hosts its implementation).
//
// Stopping convention (Phase 3-b): SIGUSR1 is delivered to the runtime
// process group by api.JobLifecycle.SignalJobRuntime. Run()'s
// signal.Notify(SIGUSR1) handler intercepts it and forwards SIGTERM to the
// claude child only, normalising the resulting exit status into
// Result.StoppedByDaemon=true. There is no separate "stop agent" entry on
// the adapter — the daemon owns the signal, the adapter owns the response.
package claude

import (
	"context"

	"github.com/novshi-tech/boid/internal/adapters"
)

// Adapter implements adapters.HarnessAdapter for Claude Code.
type Adapter struct {
	// abortCodeLookup resolves lifecycle.abort.code for a task id. Defaults
	// to invoking `boid task get`; tests override via WithAbortCodeLookup.
	abortCodeLookup func(ctx context.Context, taskID string) string
}

// New returns a new Adapter.
func New() *Adapter {
	return &Adapter{
		abortCodeLookup: defaultAbortCodeLookup,
	}
}

// WithAbortCodeLookup overrides the abort-code resolver. Intended for tests
// that want to exercise Run without spawning the `boid` CLI.
func (a *Adapter) WithAbortCodeLookup(f func(ctx context.Context, taskID string) string) *Adapter {
	a.abortCodeLookup = f
	return a
}

// Usage is not yet implemented. It will be wired in Phase 4 when the jobs
// table gains usage columns and the jsonl read path is finalised.
func (a *Adapter) Usage(_ context.Context, _ string) (adapters.Usage, error) {
	return adapters.Usage{}, nil
}
