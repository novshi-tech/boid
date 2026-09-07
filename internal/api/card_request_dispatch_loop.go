package api

// CardRequestDispatchLoop periodically scans for cards holding a queued
// card_requests row and attempts to dispatch each — the recovery fallback
// for a missed immediate (commit-triggered) dispatch attempt. The primary
// dispatch path is tryDispatchQueuedCardRequest, called right after the
// transaction that queues a row commits or a card's execution slot frees;
// this loop only exists to catch what that missed (a notification-loss
// window, a daemon restart).

import (
	"context"
	"log/slog"
	"time"
)

// CardRequestDispatchStore is CardRequestDispatchLoop's dependency, narrowed
// from *TaskWorkflowService — same idiom as TriggerLoopStore.
type CardRequestDispatchStore interface {
	SweepQueuedCardRequests(ctx context.Context, now time.Time) ([]string, error)
}

// CardRequestDispatchLoop mirrors TriggerLoop's shape: InitialDelay then
// Interval, Run(ctx) blocks until ctx is done, a sweep error is logged and
// never stops the loop.
type CardRequestDispatchLoop struct {
	Store        CardRequestDispatchStore
	Interval     time.Duration
	InitialDelay time.Duration
}

// Run blocks until ctx is done. It waits InitialDelay before the first
// sweep, then calls Store.SweepQueuedCardRequests every Interval.
func (l *CardRequestDispatchLoop) Run(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(l.InitialDelay):
	}

	l.runOnce(ctx)

	ticker := time.NewTicker(l.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.runOnce(ctx)
		}
	}
}

func (l *CardRequestDispatchLoop) runOnce(ctx context.Context) {
	if l.Store == nil {
		return
	}
	dispatched, err := l.Store.SweepQueuedCardRequests(ctx, time.Now().UTC())
	if err != nil {
		slog.Warn("card request dispatch sweep failed", "error", err)
		return
	}
	if len(dispatched) > 0 {
		slog.Info("card request dispatch sweep dispatched queued requests", "cards", dispatched)
	}
}
