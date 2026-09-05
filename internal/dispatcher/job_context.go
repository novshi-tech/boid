package dispatcher

import (
	"encoding/json"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// JobContextSnapshot captures the per-job data the task-context RPCs
// (`boid task instructions` / `boid task env` / `boid task payload`) need
// but which has no standalone DB representation to re-derive live from.
// See wiring-seams.md #13 and #17 for the routing/gating invariants behind
// Instructions and PayloadPatchAllowedTraits.
//
// WorkspacePeerAdvertise carries a raw git gateway token scoped to this
// job's own permissions (embedded in CloneURL); a broker handler serving it
// MUST reject a request whose JobID doesn't match the calling token's own
// JobID, or a readonly job could read another job's writable-token clone URL.
//
// Runner.Dispatch populates one per job; UnregisterJob discards it,
// mirroring the broker token's own lifecycle so nothing outlives the job it
// describes.
type JobContextSnapshot struct {
	Instructions              []orchestrator.RoutedInstruction
	Env                       WorkspaceEnvView
	Payload                   json.RawMessage
	PayloadPatchAllowedTraits []orchestrator.TraitType
	WorkspacePeerAdvertise    map[string]PeerAdvertise
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

// routedInstructionSlice normalizes JobSpec.Instruction for
// JobContextSnapshot.Instructions: a nil JobSpec.Instruction (non-agent
// hook, or an agent-kind hook whose agent doesn't match the task's active
// instruction) produces no data — an empty, non-nil slice, matching the RPC
// convention of "no data" being an empty JSON array. A non-nil Instruction
// produces a single-element slice wrapping it.
func routedInstructionSlice(inst *orchestrator.RoutedInstruction) []orchestrator.RoutedInstruction {
	if inst == nil {
		return []orchestrator.RoutedInstruction{}
	}
	return []orchestrator.RoutedInstruction{*inst}
}
