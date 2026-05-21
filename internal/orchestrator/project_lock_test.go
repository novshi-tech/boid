package orchestrator_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestBranchLockManager_BasicAcquireRelease verifies that a branch lock can
// be acquired by a task and released by task id.
func TestBranchLockManager_BasicAcquireRelease(t *testing.T) {
	mgr := orchestrator.NewBranchLockManager(orchestrator.NewInMemoryWorktreeLockManager())

	if err := mgr.AcquireForTask(context.Background(), "proj-1", "main", "task-a"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !mgr.IsHeldForTask("task-a") {
		t.Fatal("expected lock held for task-a")
	}
	mgr.ReleaseForTask("task-a")
	if mgr.IsHeldForTask("task-a") {
		t.Fatal("expected lock released for task-a")
	}
}

// TestBranchLockManager_AcquireIsIdempotentForSameTask verifies that calling
// AcquireForTask repeatedly for the same task is a no-op.
func TestBranchLockManager_AcquireIsIdempotentForSameTask(t *testing.T) {
	mgr := orchestrator.NewBranchLockManager(orchestrator.NewInMemoryWorktreeLockManager())

	if err := mgr.AcquireForTask(context.Background(), "proj-1", "main", "task-a"); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := mgr.AcquireForTask(context.Background(), "proj-1", "main", "task-a"); err != nil {
			t.Errorf("second acquire: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("second AcquireForTask for same task blocked unexpectedly")
	}

	mgr.ReleaseForTask("task-a")
	if mgr.IsHeldForTask("task-a") {
		t.Fatal("expected lock released after single release call")
	}

	acquired := make(chan struct{})
	go func() {
		if err := mgr.AcquireForTask(context.Background(), "proj-1", "main", "task-b"); err != nil {
			t.Errorf("task-b acquire: %v", err)
			return
		}
		close(acquired)
	}()
	select {
	case <-acquired:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("task-b could not acquire after task-a released")
	}
}

// TestBranchLockManager_ReleaseIsIdempotent verifies that calling
// ReleaseForTask on an unheld lock is a safe no-op.
func TestBranchLockManager_ReleaseIsIdempotent(t *testing.T) {
	mgr := orchestrator.NewBranchLockManager(orchestrator.NewInMemoryWorktreeLockManager())
	mgr.ReleaseForTask("task-a")
	mgr.ReleaseForTask("task-a")

	if err := mgr.AcquireForTask(context.Background(), "proj-1", "main", "task-a"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	mgr.ReleaseForTask("task-a")
	mgr.ReleaseForTask("task-a")
}

// TestBranchLockManager_SameBranchSerialized verifies that two tasks on the
// same project and same branch serialize (second blocks until first releases).
func TestBranchLockManager_SameBranchSerialized(t *testing.T) {
	mgr := orchestrator.NewBranchLockManager(orchestrator.NewInMemoryWorktreeLockManager())

	if err := mgr.AcquireForTask(context.Background(), "proj-1", "main", "task-a"); err != nil {
		t.Fatalf("task-a acquire: %v", err)
	}

	acquiredB := make(chan struct{})
	go func() {
		if err := mgr.AcquireForTask(context.Background(), "proj-1", "main", "task-b"); err != nil {
			t.Errorf("task-b acquire: %v", err)
			return
		}
		close(acquiredB)
	}()

	select {
	case <-acquiredB:
		t.Fatal("task-b acquired while task-a holds the lock on the same branch")
	case <-time.After(50 * time.Millisecond):
		// expected — task-b should block
	}

	mgr.ReleaseForTask("task-a")

	select {
	case <-acquiredB:
		// expected
	case <-time.After(time.Second):
		t.Fatal("task-b did not acquire after task-a released")
	}
}

// TestBranchLockManager_DifferentBranchesParallel verifies that tasks on the
// same project but different branches run in parallel.
func TestBranchLockManager_DifferentBranchesParallel(t *testing.T) {
	mgr := orchestrator.NewBranchLockManager(orchestrator.NewInMemoryWorktreeLockManager())

	if err := mgr.AcquireForTask(context.Background(), "proj-1", "main", "task-a"); err != nil {
		t.Fatalf("task-a acquire: %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		if err := mgr.AcquireForTask(context.Background(), "proj-1", "feature", "task-b"); err != nil {
			t.Errorf("task-b acquire: %v", err)
			return
		}
		close(acquired)
	}()

	select {
	case <-acquired:
		// expected — different branch
	case <-time.After(500 * time.Millisecond):
		t.Fatal("task-b on different branch blocked unexpectedly")
	}
}

// TestBranchLockManager_DifferentProjectsIndependent verifies that two tasks
// on different projects can hold their respective locks concurrently.
func TestBranchLockManager_DifferentProjectsIndependent(t *testing.T) {
	mgr := orchestrator.NewBranchLockManager(orchestrator.NewInMemoryWorktreeLockManager())

	if err := mgr.AcquireForTask(context.Background(), "proj-1", "main", "task-a"); err != nil {
		t.Fatalf("task-a acquire: %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		if err := mgr.AcquireForTask(context.Background(), "proj-2", "main", "task-b"); err != nil {
			t.Errorf("task-b acquire: %v", err)
			return
		}
		close(acquired)
	}()

	select {
	case <-acquired:
		// expected — different project
	case <-time.After(500 * time.Millisecond):
		t.Fatal("task-b on different project blocked unexpectedly")
	}
}

// TestBranchLockManager_ChildTasksParallel verifies that two child tasks
// (boid/<idA> and boid/<idB>) on the same project run in parallel because
// they occupy distinct branches.
func TestBranchLockManager_ChildTasksParallel(t *testing.T) {
	mgr := orchestrator.NewBranchLockManager(orchestrator.NewInMemoryWorktreeLockManager())

	if err := mgr.AcquireForTask(context.Background(), "proj-1", "boid/aaaa1111", "child-a"); err != nil {
		t.Fatalf("child-a acquire: %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		if err := mgr.AcquireForTask(context.Background(), "proj-1", "boid/bbbb2222", "child-b"); err != nil {
			t.Errorf("child-b acquire: %v", err)
			return
		}
		close(acquired)
	}()

	select {
	case <-acquired:
		// expected — distinct branch keys
	case <-time.After(500 * time.Millisecond):
		t.Fatal("child-b blocked unexpectedly despite distinct branch key")
	}
}

// TestBranchLockManager_ContextCancellation verifies that AcquireForTask
// honors context cancellation.
func TestBranchLockManager_ContextCancellation(t *testing.T) {
	mgr := orchestrator.NewBranchLockManager(orchestrator.NewInMemoryWorktreeLockManager())
	if err := mgr.AcquireForTask(context.Background(), "proj-1", "main", "task-a"); err != nil {
		t.Fatalf("task-a acquire: %v", err)
	}
	defer mgr.ReleaseForTask("task-a")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := mgr.AcquireForTask(ctx, "proj-1", "main", "task-b"); err == nil {
		t.Fatal("expected error for context timeout")
	}
	if mgr.IsHeldForTask("task-b") {
		t.Fatal("task-b should not be held after timeout")
	}
}

// TestBranchLockManager_HighConcurrency verifies that many goroutines can
// acquire and release without races.
func TestBranchLockManager_HighConcurrency(t *testing.T) {
	mgr := orchestrator.NewBranchLockManager(orchestrator.NewInMemoryWorktreeLockManager())
	const n = 32
	var counter int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			taskID := "task-" + string(rune('a'+idx%26))
			if err := mgr.AcquireForTask(context.Background(), "proj-1", "main", taskID); err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			atomic.AddInt64(&counter, 1)
			mgr.ReleaseForTask(taskID)
		}(i)
	}
	wg.Wait()
	if got := atomic.LoadInt64(&counter); got != int64(n) {
		t.Fatalf("expected %d acquires, got %d", n, got)
	}
}
