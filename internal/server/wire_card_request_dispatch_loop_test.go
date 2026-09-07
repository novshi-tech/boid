package server

// Pins the two halves of CardRequestDispatchLoop's wiring into the daemon:
// wire.go's construction of the loop struct, and server.go Start()'s own
// goroutine launch of it.

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestNew_WiresCardRequestDispatchLoop pins wire.go's own assignment: New()
// must leave srv.cardRequestDispatchLoop non-nil with a Store attached.
func TestNew_WiresCardRequestDispatchLoop(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	srv, err := New(Config{
		DBPath:     ":memory:",
		SocketPath: filepath.Join(t.TempDir(), "boid.sock"),
		HTTPAddr:   "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	if srv.cardRequestDispatchLoop == nil {
		t.Fatal("cardRequestDispatchLoop is nil — the queued card_requests recovery sweep is permanently unwired")
	}
	if srv.cardRequestDispatchLoop.Store == nil {
		t.Error("cardRequestDispatchLoop.Store is nil")
	}
}

// fakeCardRequestDispatchLoopStore records every SweepQueuedCardRequests
// call — this test's spy, swapped onto the daemon-wired loop struct in
// place of the real runtime.workflow before Start() runs.
type fakeCardRequestDispatchLoopStore struct {
	callCount atomic.Int64
}

func (f *fakeCardRequestDispatchLoopStore) SweepQueuedCardRequests(_ context.Context, _ time.Time) ([]string, error) {
	f.callCount.Add(1)
	return nil, nil
}

// TestServer_Start_RunsCardRequestDispatchLoop pins server.go Start()'s own
// half of the wiring: the loop New() already wired must actually get run.
// Swaps in a spy Store and a tiny Interval/InitialDelay so the sweep is
// observable inside a test-sized wait — the real daemon wires 30s/40s.
func TestServer_Start_RunsCardRequestDispatchLoop(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	srv, err := New(Config{
		DBPath:     ":memory:",
		SocketPath: filepath.Join(t.TempDir(), "boid.sock"),
		HTTPAddr:   "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if srv.cardRequestDispatchLoop == nil {
		t.Fatal("cardRequestDispatchLoop is nil — see TestNew_WiresCardRequestDispatchLoop")
	}
	spy := &fakeCardRequestDispatchLoopStore{}
	srv.cardRequestDispatchLoop.Store = spy
	srv.cardRequestDispatchLoop.Interval = 10 * time.Millisecond
	srv.cardRequestDispatchLoop.InitialDelay = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	deadline := time.Now().Add(2 * time.Second)
	for spy.callCount.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := spy.callCount.Load(); got == 0 {
		t.Fatal("SweepQueuedCardRequests was never called — Start() no longer runs cardRequestDispatchLoop")
	}
}
