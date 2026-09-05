package api

import "sync"

// TaskWaitRegistry records which task each in-flight `boid task wait` is
// parked on, keyed by the waiting job — so a trigger-timeout sweep can abort
// the task itself rather than just killing the launching job. In-memory only
// (like BlockingAskRegistry): a lost entry just means the abort is skipped,
// never that the wrong task gets aborted.
type TaskWaitRegistry struct {
	mu    sync.Mutex
	byJob map[string]string // jobID -> the task id that job is waiting on
}

// NewTaskWaitRegistry returns an initialised registry.
func NewTaskWaitRegistry() *TaskWaitRegistry {
	return &TaskWaitRegistry{byJob: make(map[string]string)}
}

// Register records that jobID is waiting on taskID and returns the release to
// call when the wait ends. A nil registry or empty jobID is tolerated and
// returns a no-op release. A second Register for the same job overwrites the
// first — the newer wait is the one actually parked.
func (r *TaskWaitRegistry) Register(jobID, taskID string) (release func()) {
	if r == nil || jobID == "" || taskID == "" {
		return func() {}
	}
	r.mu.Lock()
	if r.byJob == nil {
		r.byJob = make(map[string]string)
	}
	r.byJob[jobID] = taskID
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			// Only drop the entry if it is still ours — a Register that
			// overwrote us owns the key now, and releasing it would strand the
			// newer wait as unattributable.
			if r.byJob[jobID] == taskID {
				delete(r.byJob, jobID)
			}
			r.mu.Unlock()
		})
	}
}

// TaskFor returns the task jobID is currently waiting on, if any.
func (r *TaskWaitRegistry) TaskFor(jobID string) (string, bool) {
	if r == nil || jobID == "" {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	taskID, ok := r.byJob[jobID]
	return taskID, ok
}
