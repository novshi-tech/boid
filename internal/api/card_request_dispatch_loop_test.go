package api

// Pins CardRequestDispatchLoop: the periodic recovery fallback for queued
// card_requests rows.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCardRequestDispatchStore records every SweepQueuedCardRequests call
// and can be scripted to return an error on demand.
type fakeCardRequestDispatchStore struct {
	callCount atomic.Int64
	err       error
	result    []string
}

func (f *fakeCardRequestDispatchStore) SweepQueuedCardRequests(_ context.Context, _ time.Time) ([]string, error) {
	f.callCount.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func TestCardRequestDispatchLoop_RunOnce_NilStore_NoOp(t *testing.T) {
	loop := &CardRequestDispatchLoop{}
	// Must not panic with no Store wired.
	loop.runOnce(context.Background())
}

func TestCardRequestDispatchLoop_RunOnce_CallsSweep(t *testing.T) {
	store := &fakeCardRequestDispatchStore{result: []string{"card-1"}}
	loop := &CardRequestDispatchLoop{Store: store}

	loop.runOnce(context.Background())

	if got := store.callCount.Load(); got != 1 {
		t.Fatalf("SweepQueuedCardRequests calls = %d, want 1", got)
	}
}

// TestCardRequestDispatchLoop_RunOnce_SweepErrorDoesNotPanic pins that a
// sweep error is swallowed (logged) by runOnce, not propagated — Run's
// per-tick loop must keep going after one failed sweep.
func TestCardRequestDispatchLoop_RunOnce_SweepErrorDoesNotPanic(t *testing.T) {
	store := &fakeCardRequestDispatchStore{err: errors.New("db is on fire")}
	loop := &CardRequestDispatchLoop{Store: store}

	loop.runOnce(context.Background())

	if got := store.callCount.Load(); got != 1 {
		t.Fatalf("SweepQueuedCardRequests calls = %d, want 1", got)
	}
}

// TestCardRequestDispatchLoop_CallsSweepMultipleTimes pins InitialDelay then
// Interval ticking, same shape as TestTriggerLoop_CallsSweepMultipleTimes.
func TestCardRequestDispatchLoop_CallsSweepMultipleTimes(t *testing.T) {
	store := &fakeCardRequestDispatchStore{}
	loop := &CardRequestDispatchLoop{Store: store, Interval: 10 * time.Millisecond, InitialDelay: 1 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	time.Sleep(150 * time.Millisecond)
	cancel()

	if got := store.callCount.Load(); got < 3 {
		t.Fatalf("SweepQueuedCardRequests calls = %d, want at least 3", got)
	}
}

// TestCardRequestDispatchLoop_CallsSweepEvenAfterSweepErrors pins that a
// sweep error on one tick does not stop later ticks from firing — the
// periodic recovery fallback must keep retrying, not go silent after one
// bad tick.
func TestCardRequestDispatchLoop_CallsSweepEvenAfterSweepErrors(t *testing.T) {
	store := &fakeCardRequestDispatchStore{err: errors.New("db is on fire")}
	loop := &CardRequestDispatchLoop{Store: store, Interval: 10 * time.Millisecond, InitialDelay: 1 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	time.Sleep(150 * time.Millisecond)
	cancel()

	if got := store.callCount.Load(); got < 3 {
		t.Fatalf("SweepQueuedCardRequests calls = %d, want at least 3 (sweep errors must not stop the loop)", got)
	}
}

func TestCardRequestDispatchLoop_CtxCancelExits(t *testing.T) {
	store := &fakeCardRequestDispatchStore{}
	loop := &CardRequestDispatchLoop{Store: store, Interval: 10 * time.Millisecond, InitialDelay: 1 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		loop.Run(ctx)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("CardRequestDispatchLoop.Run did not exit after ctx cancel")
	}
}

// TestCardRequestDispatchLoop_Run_WaitsForInitialDelayBeforeFirstSweep pins
// that Run does not sweep immediately — it waits InitialDelay first (the
// primary dispatch path is the commit-triggered immediate attempt; this
// loop only exists to catch what that missed).
func TestCardRequestDispatchLoop_Run_WaitsForInitialDelayBeforeFirstSweep(t *testing.T) {
	store := &fakeCardRequestDispatchStore{}
	loop := &CardRequestDispatchLoop{Store: store, Interval: time.Hour, InitialDelay: 50 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	time.Sleep(10 * time.Millisecond)
	if got := store.callCount.Load(); got != 0 {
		t.Fatalf("SweepQueuedCardRequests calls = %d before InitialDelay elapsed, want 0", got)
	}

	time.Sleep(80 * time.Millisecond)
	if got := store.callCount.Load(); got != 1 {
		t.Fatalf("SweepQueuedCardRequests calls = %d after InitialDelay elapsed, want 1", got)
	}
}
