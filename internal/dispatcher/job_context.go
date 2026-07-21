package dispatcher

import (
	"encoding/json"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// JobContextSnapshot captures the per-job data the Phase 5b PR1 task-context
// RPCs (`boid task instructions` / `boid task env` / `boid task payload`,
// docs/plans/phase5-shim-and-task-context.md) need but which has no
// standalone DB representation to re-derive live from:
//
//   - Instructions: the job's own routed instruction (JobSpec.Instruction,
//     see orchestrator.DispatchPlanner.PlanHook's selectInstruction). This is
//     NOT the task's "active" instruction — orchestrator.Evaluator fires
//     every agent-kind hook whose agent appears anywhere in the instruction
//     history (extractInstructionAgents), not just the most recent entry, so
//     a claude-code hook and a codex hook can both be dispatched from the
//     same task even though only one of them matches the history's last
//     entry. selectInstruction/FilterInstructions only route the *last*
//     entry, so the other hook's job gets Instruction=nil — and
//     `boid task instructions` correspondingly returns an empty list for
//     that job (routedInstructionSlice below returns [] when inst==nil).
//     Deriving "current instructions" from the task row instead of the job's own
//     JobSpec.Instruction would hand a claude job the codex instruction (or
//     vice versa) — a real regression codex review caught before merge; see
//     wiring-seams.md #13. orchestrator.CurrentInstructions still exists for
//     task-row-level callers that are NOT this RPC — see its own doc
//     comment.
//   - Env: the reduced environment view (allowed_domains + resolved host
//     commands, both dispatch-time-only runtime facts).
//   - Payload: the trait-filtered payload (depends on the firing hook's
//     declared Traits.Consumes — plan-time-only data that JobSpec does not
//     carry forward).
//
// Runner.Dispatch populates one per job; UnregisterJob discards it,
// mirroring the broker token's own lifecycle so nothing outlives the job it
// describes.
type JobContextSnapshot struct {
	Instructions []orchestrator.RoutedInstruction
	Env          WorkspaceEnvView
	Payload      json.RawMessage
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
