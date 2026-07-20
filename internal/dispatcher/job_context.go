package dispatcher

import "encoding/json"

// JobContextSnapshot captures the per-job data the Phase 5b PR1 task-context
// RPCs (`boid task env` / `boid task payload`,
// docs/plans/phase5-shim-and-task-context.md) need but which has no
// standalone DB representation to re-derive live from: the reduced
// environment view (allowed_domains + resolved host commands, both
// dispatch-time-only runtime facts) and the trait-filtered payload (which
// depends on the firing hook's declared Traits.Consumes — plan-time-only
// data that JobSpec does not carry forward). Runner.Dispatch populates one
// per job; UnregisterJob discards it, mirroring the broker token's own
// lifecycle so nothing outlives the job it describes.
type JobContextSnapshot struct {
	Env     WorkspaceEnvView
	Payload json.RawMessage
}

// trackJobContext records snap for jobID, overwriting any previous entry.
func (r *Runner) trackJobContext(jobID string, snap JobContextSnapshot) {
	r.jobContextMu.Lock()
	defer r.jobContextMu.Unlock()
	if r.jobContexts == nil {
		r.jobContexts = make(map[string]JobContextSnapshot)
	}
	r.jobContexts[jobID] = snap
}

// JobContext returns the tracked JobContextSnapshot for jobID, and whether
// one was found. false covers both "no such job" and "job existed but its
// context was already unregistered" (UnregisterJob clears the entry).
func (r *Runner) JobContext(jobID string) (JobContextSnapshot, bool) {
	r.jobContextMu.Lock()
	defer r.jobContextMu.Unlock()
	snap, ok := r.jobContexts[jobID]
	return snap, ok
}

func (r *Runner) untrackJobContext(jobID string) {
	r.jobContextMu.Lock()
	defer r.jobContextMu.Unlock()
	delete(r.jobContexts, jobID)
}
