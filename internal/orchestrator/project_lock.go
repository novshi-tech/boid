package orchestrator

import (
	"context"
	"fmt"
	"sync"
)

// BranchLockManager keeps exclusive access to a branch (identified by the
// compound key "<projectID>:<headBranch>") for the entire executing lifetime
// of a single task.
//
// Unlike the underlying WorktreeLocker (which returns an opaque release
// closure per acquire), BranchLockManager keys the held lock by task id so
// that Acquire and Release can be called from different goroutines and at
// different points in the task lifecycle (Acquire when the task enters
// executing, Release when it leaves).
//
// Calls are idempotent for the same task id: re-Acquire on a task that
// already holds the lock for the same branch is a no-op, and Release on an
// unheld task is a no-op. This matches the realistic dispatch flow where a
// task may go through multiple dispatch cycles inside a single executing window.
//
// BranchLockManager is safe for concurrent use.
type BranchLockManager struct {
	underlying WorktreeLocker

	mu   sync.Mutex
	held map[string]branchLockHolder
}

type branchLockHolder struct {
	lockKey string // "<projectID>:<headBranch>"
	release func()
}

// NewBranchLockManager wraps the given WorktreeLocker so the lock can be
// pinned to a task id rather than a single Acquire call. The underlying
// locker is responsible for the actual mutual exclusion semantics
// (FIFO queueing, context cancellation, etc.).
func NewBranchLockManager(underlying WorktreeLocker) *BranchLockManager {
	if underlying == nil {
		panic("orchestrator: NewBranchLockManager requires a non-nil underlying locker")
	}
	return &BranchLockManager{
		underlying: underlying,
		held:       make(map[string]branchLockHolder),
	}
}

// AcquireForTask acquires the branch lock keyed by "<projectID>:<headBranch>"
// on behalf of the given task. If the task already holds the lock for the
// same key, this is a no-op (no double-acquire). Otherwise it blocks on the
// underlying locker until either the lock is acquired or ctx is cancelled.
func (m *BranchLockManager) AcquireForTask(ctx context.Context, projectID, headBranch, taskID string) error {
	if m == nil {
		return nil
	}
	lockKey := fmt.Sprintf("%s:%s", projectID, headBranch)
	m.mu.Lock()
	if h, ok := m.held[taskID]; ok && h.lockKey == lockKey {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	release, err := m.underlying.Acquire(ctx, lockKey)
	if err != nil {
		return err
	}

	m.mu.Lock()
	// Race-safety: another goroutine might have stashed an entry for this
	// task while we were blocked. If so, prefer the existing holder and
	// drop the new acquisition.
	if h, ok := m.held[taskID]; ok && h.lockKey == lockKey {
		m.mu.Unlock()
		release()
		return nil
	}
	m.held[taskID] = branchLockHolder{lockKey: lockKey, release: release}
	m.mu.Unlock()
	return nil
}

// ReleaseForTask releases the branch lock held on behalf of the task.
// No-op when the task does not currently hold a lock — the goal is to make
// "release on every executing-leaving path" cheap to wire up.
func (m *BranchLockManager) ReleaseForTask(taskID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	h, ok := m.held[taskID]
	if ok {
		delete(m.held, taskID)
	}
	m.mu.Unlock()
	if ok && h.release != nil {
		h.release()
	}
}

// IsHeldForTask reports whether the given task currently holds a branch lock.
// Intended for tests and diagnostics.
func (m *BranchLockManager) IsHeldForTask(taskID string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.held[taskID]
	return ok
}
