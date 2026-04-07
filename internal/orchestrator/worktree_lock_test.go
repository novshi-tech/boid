package orchestrator

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorktreeLock_BasicAcquireRelease(t *testing.T) {
	lm := NewInMemoryWorktreeLockManager()

	release, err := lm.Acquire(context.Background(), "proj-1")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	release()
}

func TestWorktreeLock_FIFOOrder(t *testing.T) {
	lm := NewInMemoryWorktreeLockManager()

	// First goroutine holds the lock
	release1, err := lm.Acquire(context.Background(), "proj-1")
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}

	const n = 5
	order := make([]int, 0, n)
	var mu sync.Mutex
	var wg sync.WaitGroup

	// Launch n goroutines that each try to acquire the same key.
	// They should acquire in FIFO order.
	ready := make([]chan struct{}, n)
	for i := range n {
		ready[i] = make(chan struct{})
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			close(ready[idx]) // signal that this goroutine is about to acquire
			rel, err := lm.Acquire(context.Background(), "proj-1")
			if err != nil {
				t.Errorf("acquire %d: %v", idx, err)
				return
			}
			mu.Lock()
			order = append(order, idx)
			mu.Unlock()
			rel()
		}(i)
		<-ready[i]
		// Give it time to enqueue
		time.Sleep(5 * time.Millisecond)
	}

	// Release the first lock so waiters proceed in order
	release1()
	wg.Wait()

	if len(order) != n {
		t.Fatalf("expected %d acquisitions, got %d", n, len(order))
	}
	for i, v := range order {
		if v != i {
			t.Errorf("expected order[%d]=%d, got %d (order=%v)", i, i, v, order)
			break
		}
	}
}

func TestWorktreeLock_ContextCancel(t *testing.T) {
	lm := NewInMemoryWorktreeLockManager()

	// Hold the lock
	release, err := lm.Acquire(context.Background(), "proj-1")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := lm.Acquire(ctx, "proj-1")
		done <- err
	}()

	// Give goroutine time to enqueue
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error on cancelled context, got nil")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for cancelled acquire")
	}
}

func TestWorktreeLock_DifferentKeysIndependent(t *testing.T) {
	lm := NewInMemoryWorktreeLockManager()

	release1, err := lm.Acquire(context.Background(), "proj-1")
	if err != nil {
		t.Fatalf("acquire proj-1: %v", err)
	}

	// Should not block because different key
	release2, err := lm.Acquire(context.Background(), "proj-2")
	if err != nil {
		t.Fatalf("acquire proj-2: %v", err)
	}

	release1()
	release2()
}

func TestWorktreeLock_ReleaseWakesNextWaiter(t *testing.T) {
	lm := NewInMemoryWorktreeLockManager()

	release1, err := lm.Acquire(context.Background(), "proj-1")
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}

	var acquired atomic.Bool
	done := make(chan struct{})
	go func() {
		rel, err := lm.Acquire(context.Background(), "proj-1")
		if err != nil {
			t.Errorf("acquire 2: %v", err)
			close(done)
			return
		}
		acquired.Store(true)
		rel()
		close(done)
	}()

	// Wait for waiter to enqueue
	time.Sleep(10 * time.Millisecond)
	if acquired.Load() {
		t.Fatal("second goroutine should not have acquired yet")
	}

	release1()

	select {
	case <-done:
		if !acquired.Load() {
			t.Error("expected second goroutine to have acquired the lock")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for second goroutine")
	}
}

func TestWorktreeLock_ContextCancelRemovesWaiter(t *testing.T) {
	lm := NewInMemoryWorktreeLockManager()

	// Hold the lock
	release1, err := lm.Acquire(context.Background(), "proj-1")
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}

	// Second goroutine: will be cancelled
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() {
		lm.Acquire(ctx2, "proj-1")
		close(done2)
	}()
	time.Sleep(10 * time.Millisecond)
	cancel2()
	<-done2

	// Third goroutine: should acquire after release1
	done3 := make(chan struct{})
	var acquired3 atomic.Bool
	go func() {
		rel, err := lm.Acquire(context.Background(), "proj-1")
		if err != nil {
			t.Errorf("acquire 3: %v", err)
			close(done3)
			return
		}
		acquired3.Store(true)
		rel()
		close(done3)
	}()

	time.Sleep(10 * time.Millisecond)
	release1()

	select {
	case <-done3:
		if !acquired3.Load() {
			t.Error("expected third goroutine to acquire after cancelled waiter removed")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for third goroutine")
	}
}
