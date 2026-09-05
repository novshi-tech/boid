// Package claude implements adapters.HarnessAdapter for Claude Code.
//
// Stopping convention: SIGUSR1 is delivered to the runtime process group by
// api.JobLifecycle.SignalJobRuntime. Run()'s signal.Notify(SIGUSR1) handler
// intercepts it and forwards SIGTERM to the claude child only, normalising
// the resulting exit status into Result.StoppedByDaemon=true. There is no
// separate "stop agent" entry on the adapter — the daemon owns the signal,
// the adapter owns the response.
package claude

import (
	"context"

	"github.com/novshi-tech/boid/internal/adapters"
)

// Adapter implements adapters.HarnessAdapter for Claude Code.
type Adapter struct{}

// New returns a new Adapter.
func New() *Adapter {
	return &Adapter{}
}

// Usage is not yet implemented; awaits the jobs table gaining usage columns
// and a jsonl read path.
func (a *Adapter) Usage(_ context.Context, _ string) (adapters.Usage, error) {
	return adapters.Usage{}, nil
}
