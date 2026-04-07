package orchestrator

import (
	"context"
	"sync"
)

// WorktreeLocker manages exclusive write access to a shared worktree directory.
type WorktreeLocker interface {
	Acquire(ctx context.Context, key string) (release func(), err error)
}

// InMemoryWorktreeLockManager is an in-memory implementation of WorktreeLocker
// with per-key FIFO queuing.
type InMemoryWorktreeLockManager struct {
	mu    sync.Mutex
	locks map[string]*lockEntry
}

type lockEntry struct {
	held    bool
	waiters []waiter
}

type waiter struct {
	ch chan struct{}
}

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

	// Enqueue as a waiter
	w := waiter{ch: make(chan struct{}, 1)}
	entry.waiters = append(entry.waiters, w)
	m.mu.Unlock()

	select {
	case <-w.ch:
		return m.releaseFunc(key), nil
	case <-ctx.Done():
		m.removeWaiter(key, w)
		return nil, ctx.Err()
	}
}

func (m *InMemoryWorktreeLockManager) releaseFunc(key string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()

			entry, ok := m.locks[key]
			if !ok {
				return
			}

			if len(entry.waiters) > 0 {
				next := entry.waiters[0]
				entry.waiters = entry.waiters[1:]
				next.ch <- struct{}{}
			} else {
				delete(m.locks, key)
			}
		})
	}
}

func (m *InMemoryWorktreeLockManager) removeWaiter(key string, w waiter) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.locks[key]
	if !ok {
		return
	}
	for i, existing := range entry.waiters {
		if existing.ch == w.ch {
			entry.waiters = append(entry.waiters[:i], entry.waiters[i+1:]...)
			break
		}
	}
	if !entry.held && len(entry.waiters) == 0 {
		delete(m.locks, key)
	}
}
