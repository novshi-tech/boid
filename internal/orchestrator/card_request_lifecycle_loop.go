package orchestrator

import (
	"context"
	"database/sql"
	"log/slog"
	"time"
)

// CardRequestLifecycleLoop periodically calls ReconcileCardRequestSlots to
// release attached card_requests rows whose continuation has terminated. It
// never releases on a timer — each tick just repeats the
// confirm-or-leave-alone check.
//
// RecoverLaunchingCardRequests is deliberately NOT part of this loop: it is
// the daemon-STARTUP recovery scan (a crash-window that only exists right
// after a restart), run once via RunStartupRecovery before this loop's first
// tick, not on every interval.
type CardRequestLifecycleLoop struct {
	DB           *sql.DB
	Interval     time.Duration
	InitialDelay time.Duration
}

// RunStartupRecovery runs RecoverLaunchingCardRequests once. Call it once at
// daemon startup, before Run's periodic reconcile loop begins.
func (l *CardRequestLifecycleLoop) RunStartupRecovery() {
	outcomes, err := RecoverLaunchingCardRequests(l.DB)
	if err != nil {
		slog.Warn("card request recovery scan failed", "error", err)
		return
	}
	if len(outcomes) > 0 {
		slog.Info("card request recovery scan completed", "rows", len(outcomes))
	}
}

// Run blocks until ctx is done, calling ReconcileCardRequestSlots every
// Interval (after an initial InitialDelay). Errors are logged as warnings;
// the loop always continues.
func (l *CardRequestLifecycleLoop) Run(ctx context.Context) {
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

func (l *CardRequestLifecycleLoop) runOnce() {
	outcomes, err := ReconcileCardRequestSlots(l.DB)
	if err != nil {
		slog.Warn("card request slot reconcile failed", "error", err)
	} else if len(outcomes) > 0 {
		slog.Info("card request slot reconcile completed", "released", len(outcomes))
	}

	// ReconcileLaunchingCardRequests is the OPERATIONAL self-heal for a
	// `run:` script that exits/hangs without ever calling `boid task create`
	// / `boid agent start`: without it, only a daemon restart's startup scan
	// (RecoverLaunchingCardRequests) could ever clear a stuck "launching"
	// row, leaving a card's slot occupied until an operator restarted the
	// daemon. Run every tick alongside the attached-row reconcile above —
	// see that function's own doc comment for why it is gated on the
	// launcher job's terminal status, never elapsed time.
	launchingOutcomes, lerr := ReconcileLaunchingCardRequests(l.DB)
	if lerr != nil {
		slog.Warn("card request launching self-heal failed", "error", lerr)
		return
	}
	if len(launchingOutcomes) > 0 {
		slog.Info("card request launching self-heal completed", "resolved", len(launchingOutcomes))
	}
}
