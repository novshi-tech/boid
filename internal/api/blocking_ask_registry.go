package api

import (
	"context"
	"errors"
	"sync"
)

// ErrAskPending is returned by BlockingAskRegistry.Register when the task
// already has an outstanding blocking ask. It implements decision B1: a second
// concurrent `boid task ask` for the same task fails immediately rather than
// queueing behind the first.
var ErrAskPending = errors.New("task_ask: another question is pending")

// BlockingAskRegistry coordinates harness-independent blocking Q&A. When an
// agent calls `boid task ask`, the broker holds the RPC connection open and the
// server-side handler blocks in Wait until the user/supervisor answers (via
// TaskHandler.Answer → AnswerTask → Notify) or the context is cancelled
// (daemon shutdown / agent disconnect).
//
// The registry is purely in-memory: a daemon restart drops every pending ask.
// That is consistent — the sandbox process backing the blocked RPC dies with
// the daemon too, so there is nothing to resume.
type BlockingAskRegistry struct {
	mu        sync.Mutex
	channels  map[string]chan string // questionID -> answer channel (buffered, cap 1)
	qidByTask map[string]string      // taskID -> questionID (B1 guard)
}

// NewBlockingAskRegistry returns an initialised registry.
func NewBlockingAskRegistry() *BlockingAskRegistry {
	return &BlockingAskRegistry{
		channels:  make(map[string]chan string),
		qidByTask: make(map[string]string),
	}
}

func (r *BlockingAskRegistry) ensureInit() {
	if r.channels == nil {
		r.channels = make(map[string]chan string)
	}
	if r.qidByTask == nil {
		r.qidByTask = make(map[string]string)
	}
}

// Register reserves the answer channel for (taskID, qid). It MUST be called
// before the task transitions to awaiting so an answer that arrives immediately
// afterwards is never dropped. Returns ErrAskPending if the task already has a
// pending blocking ask (decision B1). The check-and-insert is atomic under the
// registry lock, so two concurrent asks for the same task can never both
// succeed.
func (r *BlockingAskRegistry) Register(taskID, qid string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureInit()
	if _, ok := r.qidByTask[taskID]; ok {
		return ErrAskPending
	}
	r.channels[qid] = make(chan string, 1)
	r.qidByTask[taskID] = qid
	return nil
}

// Wait blocks until an answer is delivered for qid via Notify or ctx is
// cancelled. qid must have been registered; an unknown qid is a programming
// error and returns immediately. The caller is responsible for Cancel(qid)
// cleanup (typically via defer) on every exit path.
func (r *BlockingAskRegistry) Wait(ctx context.Context, qid string) (string, error) {
	r.mu.Lock()
	ch, ok := r.channels[qid]
	r.mu.Unlock()
	if !ok {
		return "", errors.New("task_ask: no pending question for id " + qid)
	}
	select {
	case answer := <-ch:
		return answer, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Notify delivers answer to the waiter registered for qid. It returns true when
// a registration exists and the answer was accepted, false when no waiter is
// registered (e.g. the agent already disconnected and Cancel ran) or an answer
// was already delivered. The send is non-blocking thanks to the buffered
// channel, so Notify never blocks even if Wait has not yet been scheduled.
func (r *BlockingAskRegistry) Notify(qid, answer string) bool {
	r.mu.Lock()
	ch, ok := r.channels[qid]
	r.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- answer:
		return true
	default:
		return false
	}
}

// Cancel removes the registration for qid (and the task it belongs to). It does
// not unblock Wait by itself — a blocked Wait observes ctx cancellation — but it
// frees the B1 slot so the task can ask again. Idempotent and safe to defer.
func (r *BlockingAskRegistry) Cancel(qid string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.channels, qid)
	for taskID, q := range r.qidByTask {
		if q == qid {
			delete(r.qidByTask, taskID)
		}
	}
}

// Has reports whether qid currently has a registration. Intended for tests and
// diagnostics.
func (r *BlockingAskRegistry) Has(qid string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.channels[qid]
	return ok
}
