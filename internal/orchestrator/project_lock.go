package orchestrator

import (
	"context"
	"sync"
)

// ProjectLockManager keeps exclusive write access to a project's working
// directory for the entire executing lifetime of a single task.
//
// Unlike the underlying WorktreeLocker (which returns an opaque release
// closure per acquire), ProjectLockManager keys the held lock by task id so
// that Acquire and Release can be called from different goroutines and at
// different points in the task lifecycle (Acquire when the task enters
// executing, Release when it leaves).
//
// Calls are idempotent for the same task id: re-Acquire on a task that
// already holds the lock is a no-op, and Release on an unheld task is a
// no-op. This matches the realistic dispatch flow where a task may go
// through multiple dispatch cycles inside a single executing window.
//
// ProjectLockManager is safe for concurrent use.
type ProjectLockManager struct {
	underlying WorktreeLocker

	mu   sync.Mutex
	held map[string]projectLockHolder
}

type projectLockHolder struct {
	projectID string
	release   func()
}

// NewProjectLockManager wraps the given WorktreeLocker so the lock can be
// pinned to a task id rather than a single Acquire call. The underlying
// locker is responsible for the actual mutual exclusion semantics
// (FIFO queueing, context cancellation, etc.).
func NewProjectLockManager(underlying WorktreeLocker) *ProjectLockManager {
	if underlying == nil {
		panic("orchestrator: NewProjectLockManager requires a non-nil underlying locker")
	}
	return &ProjectLockManager{
		underlying: underlying,
		held:       make(map[string]projectLockHolder),
	}
}

// AcquireForTask acquires the project lock on behalf of the given task.
// If the task already holds the lock for the same project, this is a no-op
// (no double-acquire). Otherwise it blocks on the underlying locker until
// either the lock is acquired or ctx is cancelled.
func (m *ProjectLockManager) AcquireForTask(ctx context.Context, projectID, taskID string) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if h, ok := m.held[taskID]; ok && h.projectID == projectID {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	release, err := m.underlying.Acquire(ctx, projectID)
	if err != nil {
		return err
	}

	m.mu.Lock()
	// Race-safety: another goroutine might have stashed an entry for this
	// task while we were blocked. If so, prefer the existing holder and
	// drop the new acquisition.
	if h, ok := m.held[taskID]; ok && h.projectID == projectID {
		m.mu.Unlock()
		release()
		return nil
	}
	m.held[taskID] = projectLockHolder{projectID: projectID, release: release}
	m.mu.Unlock()
	return nil
}

// ReleaseForTask releases the project lock held on behalf of the task.
// No-op when the task does not currently hold a lock — the goal is to make
// "release on every executing-leaving path" cheap to wire up.
func (m *ProjectLockManager) ReleaseForTask(taskID string) {
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

// IsHeldForTask reports whether the given task currently holds a project
// lock. Intended for tests and diagnostics.
func (m *ProjectLockManager) IsHeldForTask(taskID string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.held[taskID]
	return ok
}
