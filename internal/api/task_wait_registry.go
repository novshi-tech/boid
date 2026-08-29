package api

import "sync"

// TaskWaitRegistry records which task each in-flight `boid task wait` is parked
// on, keyed by the waiting job.
//
// It exists for one question the trigger sweep cannot otherwise answer: when a
// round overruns its trigger's `timeout`, WHAT should be ended? The job is only
// the launcher — killing it leaves the task it started running, releases
// single-flight, and lets the next tick begin a second concurrent round of the
// same work, which is worse than the overrun. The thing to end is the task, and
// the only place that link exists is the wait itself: the executor knows both
// ids at the moment it parks (TokenContext.JobID and the resolved task id).
//
// Purely in-memory, like BlockingAskRegistry and for the same reason: the
// sandbox process backing a parked wait dies with the daemon, so a restart has
// nothing to resume. A lost entry degrades to today's behavior (the job is
// stopped, the task is left alone), never to a wrong task being aborted.
type TaskWaitRegistry struct {
	mu    sync.Mutex
	byJob map[string]string // jobID -> the task id that job is waiting on
}

// NewTaskWaitRegistry returns an initialised registry.
func NewTaskWaitRegistry() *TaskWaitRegistry {
	return &TaskWaitRegistry{byJob: make(map[string]string)}
}

// Register records that jobID is waiting on taskID and returns the release to
// call when the wait ends. Both a nil registry and an empty jobID are tolerated
// and return a no-op release, so a caller never has to branch: a job id is
// absent for host-side and test callers, and an unregistered wait simply cannot
// be attributed to a trigger run — which is the same position the daemon was in
// before this existed.
//
// A second Register for the same job overwrites the first. That is the right
// resolution rather than an error: a job runs its commands sequentially, so the
// only way to see two is if a release was missed, and the newer wait is the one
// actually parked.
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
