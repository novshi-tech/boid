package orchestrator

import (
	"context"
	"log/slog"
	"time"
)

// GCStore is the interface required by GCLoop to run garbage collection.
type GCStore interface {
	GC(olderThan time.Duration, dryRun bool) (*GCResult, error)
}

// GCLoop periodically calls GC on a GCStore.
type GCLoop struct {
	Store        GCStore
	Interval     time.Duration
	OlderThan    time.Duration
	InitialDelay time.Duration
}

// Run blocks until ctx is done. It waits InitialDelay before the first GC run,
// then calls Store.GC every Interval. Errors are logged as warnings; the loop
// always continues.
func (l *GCLoop) Run(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(l.InitialDelay):
	}

	l.runOnce()

	ticker := time.NewTicker(l.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.runOnce()
		}
	}
}

func (l *GCLoop) runOnce() {
	result, err := l.Store.GC(l.OlderThan, false)
	if err != nil {
		slog.Warn("gc failed", "error", err)
		return
	}
	slog.Info("gc completed",
		"tasks", result.Tasks,
		"jobs", result.Jobs,
		"actions", result.Actions,
		"worktrees", result.Worktrees,
		"runtimes", result.Runtimes,
		"sandbox_tmp", result.SandboxTmp,
	)
}
