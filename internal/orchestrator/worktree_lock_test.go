package orchestrator_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestInMemoryWorktreeLockManager_BasicAcquireRelease(t *testing.T) {
	mgr := orchestrator.NewInMemoryWorktreeLockManager()

	release, err := mgr.Acquire(context.Background(), "proj-1")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Release should not panic
	release()
}

func TestInMemoryWorktreeLockManager_FIFO(t *testing.T) {
	mgr := orchestrator.NewInMemoryWorktreeLockManager()

	// Acquire the lock first
	release1, err := mgr.Acquire(context.Background(), "proj-1")
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}

	// Track acquisition order
	var mu sync.Mutex
	var order []int

	// Second and third goroutines should block and acquire in FIFO order
	var wg sync.WaitGroup
	ready2 := make(chan struct{})
	ready3 := make(chan struct{})

	wg.Add(2)
	go func() {
		defer wg.Done()
		close(ready2)
		release2, err := mgr.Acquire(context.Background(), "proj-1")
		if err != nil {
			t.Errorf("acquire2: %v", err)
			return
		}
		mu.Lock()
		order = append(order, 2)
		mu.Unlock()
		release2()
	}()

	// Ensure goroutine 2 is waiting before starting goroutine 3
	<-ready2
	time.Sleep(10 * time.Millisecond)

	go func() {
		defer wg.Done()
		close(ready3)
		release3, err := mgr.Acquire(context.Background(), "proj-1")
		if err != nil {
			t.Errorf("acquire3: %v", err)
			return
		}
		mu.Lock()
		order = append(order, 3)
		mu.Unlock()
		release3()
	}()

	<-ready3
	time.Sleep(10 * time.Millisecond)

	// Release the first lock, allowing waiters to proceed in order
	release1()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 {
		t.Fatalf("expected 2 acquisitions, got %d", len(order))
	}
	if order[0] != 2 || order[1] != 3 {
		t.Errorf("expected FIFO order [2, 3], got %v", order)
	}
}

func TestInMemoryWorktreeLockManager_ContextCancel(t *testing.T) {
	mgr := orchestrator.NewInMemoryWorktreeLockManager()

	// Hold the lock
	release1, err := mgr.Acquire(context.Background(), "proj-1")
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	defer release1()

	// Try to acquire with a cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err = mgr.Acquire(ctx, "proj-1")
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestInMemoryWorktreeLockManager_ContextTimeout(t *testing.T) {
	mgr := orchestrator.NewInMemoryWorktreeLockManager()

	// Hold the lock
	release1, err := mgr.Acquire(context.Background(), "proj-1")
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	defer release1()

	// Try to acquire with a short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err = mgr.Acquire(ctx, "proj-1")
	if err == nil {
		t.Fatal("expected error for timed out context")
	}
}

func TestInMemoryWorktreeLockManager_DifferentKeysIndependent(t *testing.T) {
	mgr := orchestrator.NewInMemoryWorktreeLockManager()

	release1, err := mgr.Acquire(context.Background(), "proj-1")
	if err != nil {
		t.Fatalf("acquire proj-1: %v", err)
	}

	// Different key should not block
	release2, err := mgr.Acquire(context.Background(), "proj-2")
	if err != nil {
		t.Fatalf("acquire proj-2: %v", err)
	}

	release1()
	release2()
}

func TestInMemoryWorktreeLockManager_ReleaseWakesNextWaiter(t *testing.T) {
	mgr := orchestrator.NewInMemoryWorktreeLockManager()

	release1, err := mgr.Acquire(context.Background(), "proj-1")
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		release2, err := mgr.Acquire(context.Background(), "proj-1")
		if err != nil {
			return
		}
		close(acquired)
		release2()
	}()

	// The second acquire should be blocking
	select {
	case <-acquired:
		t.Fatal("second acquire should be blocking")
	case <-time.After(20 * time.Millisecond):
		// expected
	}

	// Release should wake the waiter
	release1()

	select {
	case <-acquired:
		// expected
	case <-time.After(time.Second):
		t.Fatal("second acquire was not woken after release")
	}
}

func TestInMemoryWorktreeLockManager_CancelRemovesWaiter(t *testing.T) {
	mgr := orchestrator.NewInMemoryWorktreeLockManager()

	// Hold the lock
	release1, err := mgr.Acquire(context.Background(), "proj-1")
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}

	// Start waiter 2 with cancellable context
	ctx2, cancel2 := context.WithCancel(context.Background())
	waiter2Done := make(chan error, 1)
	go func() {
		_, err := mgr.Acquire(ctx2, "proj-1")
		waiter2Done <- err
	}()

	// Wait a bit then start waiter 3
	time.Sleep(10 * time.Millisecond)
	waiter3Acquired := make(chan struct{})
	go func() {
		release3, err := mgr.Acquire(context.Background(), "proj-1")
		if err != nil {
			return
		}
		close(waiter3Acquired)
		release3()
	}()

	time.Sleep(10 * time.Millisecond)

	// Cancel waiter 2 — it should be removed from queue
	cancel2()
	<-waiter2Done

	// Release lock — waiter 3 should get it (not blocked by removed waiter 2)
	release1()

	select {
	case <-waiter3Acquired:
		// expected
	case <-time.After(time.Second):
		t.Fatal("waiter 3 was not woken after release and waiter 2 cancel")
	}
}
