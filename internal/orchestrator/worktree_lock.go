package orchestrator

import (
	"context"
	"sync"
)

// WorktreeLocker manages exclusive write access to a shared worktree.
type WorktreeLocker interface {
	Acquire(ctx context.Context, key string) (release func(), err error)
}

// InMemoryWorktreeLockManager implements WorktreeLocker with per-key FIFO queueing.
type InMemoryWorktreeLockManager struct {
	mu    sync.Mutex
	locks map[string]*lockEntry
}

type lockEntry struct {
	held    bool
	waiters []chan struct{}
}

// NewInMemoryWorktreeLockManager creates a new InMemoryWorktreeLockManager.
func NewInMemoryWorktreeLockManager() *InMemoryWorktreeLockManager {
	return &InMemoryWorktreeLockManager{
		locks: make(map[string]*lockEntry),
	}
}

func (m *InMemoryWorktreeLockManager) Acquire(ctx context.Context, key string) (func(), error) {
	m.mu.Lock()
	entry, ok := m.locks[key]
	if !ok {
		entry = &lockEntry{}
		m.locks[key] = entry
	}

	if !entry.held {
		entry.held = true
		m.mu.Unlock()
		return m.releaseFunc(key), nil
	}

	// Lock is held — enqueue and wait
	ch := make(chan struct{}, 1)
	entry.waiters = append(entry.waiters, ch)
	m.mu.Unlock()

	select {
	case <-ch:
		return m.releaseFunc(key), nil
	case <-ctx.Done():
		// Remove this waiter from the queue
		m.mu.Lock()
		e := m.locks[key]
		for i, w := range e.waiters {
			if w == ch {
				e.waiters = append(e.waiters[:i], e.waiters[i+1:]...)
				break
			}
		}
		m.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (m *InMemoryWorktreeLockManager) releaseFunc(key string) func() {
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()

		entry, ok := m.locks[key]
		if !ok {
			return
		}

		if len(entry.waiters) > 0 {
			// Wake next waiter (FIFO)
			next := entry.waiters[0]
			entry.waiters = entry.waiters[1:]
			next <- struct{}{}
		} else {
			// No waiters — clean up
			delete(m.locks, key)
		}
	}
}
