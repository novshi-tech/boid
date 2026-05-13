package orchestrator_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestProjectLockManager_BasicAcquireRelease verifies that a project lock can
// be acquired by a task and released by task id.
func TestProjectLockManager_BasicAcquireRelease(t *testing.T) {
	mgr := orchestrator.NewProjectLockManager(orchestrator.NewInMemoryWorktreeLockManager())

	if err := mgr.AcquireForTask(context.Background(), "proj-1", "task-a"); err != nil {
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

// TestProjectLockManager_AcquireIsIdempotentForSameTask verifies that calling
// AcquireForTask repeatedly for the same task is a no-op.
func TestProjectLockManager_AcquireIsIdempotentForSameTask(t *testing.T) {
	mgr := orchestrator.NewProjectLockManager(orchestrator.NewInMemoryWorktreeLockManager())

	if err := mgr.AcquireForTask(context.Background(), "proj-1", "task-a"); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	// Second acquire by the same task must NOT block and must NOT acquire twice.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := mgr.AcquireForTask(context.Background(), "proj-1", "task-a"); err != nil {
			t.Errorf("second acquire: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("second AcquireForTask for same task blocked unexpectedly")
	}

	// A single release must fully release the lock.
	mgr.ReleaseForTask("task-a")
	if mgr.IsHeldForTask("task-a") {
		t.Fatal("expected lock released after single release call")
	}

	// Another task can now acquire.
	acquired := make(chan struct{})
	go func() {
		if err := mgr.AcquireForTask(context.Background(), "proj-1", "task-b"); err != nil {
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

// TestProjectLockManager_ReleaseIsIdempotent verifies that calling
// ReleaseForTask on an unheld lock is a safe no-op.
func TestProjectLockManager_ReleaseIsIdempotent(t *testing.T) {
	mgr := orchestrator.NewProjectLockManager(orchestrator.NewInMemoryWorktreeLockManager())
	// No panic for unheld release.
	mgr.ReleaseForTask("task-a")
	mgr.ReleaseForTask("task-a")

	// Acquire and double-release must also be safe.
	if err := mgr.AcquireForTask(context.Background(), "proj-1", "task-a"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	mgr.ReleaseForTask("task-a")
	mgr.ReleaseForTask("task-a")
}

// TestProjectLockManager_DifferentTasksSameProjectSerialized verifies that
// when two tasks on the same project try to acquire, the second one blocks
// until the first releases.
func TestProjectLockManager_DifferentTasksSameProjectSerialized(t *testing.T) {
	mgr := orchestrator.NewProjectLockManager(orchestrator.NewInMemoryWorktreeLockManager())

	if err := mgr.AcquireForTask(context.Background(), "proj-1", "task-a"); err != nil {
		t.Fatalf("task-a acquire: %v", err)
	}

	acquiredB := make(chan struct{})
	go func() {
		if err := mgr.AcquireForTask(context.Background(), "proj-1", "task-b"); err != nil {
			t.Errorf("task-b acquire: %v", err)
			return
		}
		close(acquiredB)
	}()

	select {
	case <-acquiredB:
		t.Fatal("task-b acquired while task-a holds the lock")
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

// TestProjectLockManager_DifferentProjectsIndependent verifies that two tasks
// on different projects can hold their respective locks concurrently.
func TestProjectLockManager_DifferentProjectsIndependent(t *testing.T) {
	mgr := orchestrator.NewProjectLockManager(orchestrator.NewInMemoryWorktreeLockManager())

	if err := mgr.AcquireForTask(context.Background(), "proj-1", "task-a"); err != nil {
		t.Fatalf("task-a acquire: %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		if err := mgr.AcquireForTask(context.Background(), "proj-2", "task-b"); err != nil {
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

// TestProjectLockManager_ContextCancellation verifies that AcquireForTask
// honors context cancellation.
func TestProjectLockManager_ContextCancellation(t *testing.T) {
	mgr := orchestrator.NewProjectLockManager(orchestrator.NewInMemoryWorktreeLockManager())
	if err := mgr.AcquireForTask(context.Background(), "proj-1", "task-a"); err != nil {
		t.Fatalf("task-a acquire: %v", err)
	}
	defer mgr.ReleaseForTask("task-a")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := mgr.AcquireForTask(ctx, "proj-1", "task-b"); err == nil {
		t.Fatal("expected error for context timeout")
	}
	if mgr.IsHeldForTask("task-b") {
		t.Fatal("task-b should not be held after timeout")
	}
}

// TestProjectLockManager_NilUnderlyingPanicsOrNoOp documents that a nil
// underlying locker is a programmer error — but the manager constructor enforces
// non-nil so this test simply verifies that constructing with a non-nil locker works.
func TestProjectLockManager_HighConcurrency(t *testing.T) {
	mgr := orchestrator.NewProjectLockManager(orchestrator.NewInMemoryWorktreeLockManager())
	const n = 32
	var counter int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			taskID := "task-" + string(rune('a'+idx%26))
			if err := mgr.AcquireForTask(context.Background(), "proj-1", taskID); err != nil {
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
