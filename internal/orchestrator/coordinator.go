package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/novshi-tech/boid/internal/project"
)

// Coordinator orchestrates the hook → gate → advance flow.
type Coordinator struct {
	Evaluator    *Evaluator
	HookExecutor HookExecutor
	GateExecutor GateExecutor
	Waiter       JobWaiter
	MaxDepth     int
}

// DispatchAndAdvance runs the full dispatch cycle:
// 1. Evaluate and execute hooks (parallel if readonly, sequential otherwise)
// 2. Merge hook payload patches
// 3. Evaluate and execute gates (always parallel)
// 4. Merge gate payload patches
// 5. Evaluate condition-based auto-advance via state machine
func (d *Coordinator) DispatchAndAdvance(
	ctx context.Context,
	task *Task,
	meta *project.ProjectMeta,
	behavior *project.TaskBehavior,
	sm *StateMachine,
) (*DispatchResult, error) {
	readonly := IsReadonly(behavior, task.Status)
	payload := task.Payload
	var allResults []HandlerResult
	exclusiveWriters := map[string]string{} // trait key → first writer ID

	// 1. Evaluate and dispatch hooks
	matchedHooks := d.Evaluator.Evaluate(task, meta.Hooks)
	if len(matchedHooks) > 0 {
		hookResults, err := d.dispatchHooks(ctx, task, matchedHooks, readonly)
		if err != nil {
			return nil, fmt.Errorf("hook dispatch: %w", err)
		}
		for _, hr := range hookResults {
			allResults = append(allResults, hr)
			if err := checkExclusiveCollision(hr.PayloadPatch, hr.ID, exclusiveWriters); err != nil {
				return nil, err
			}
			if len(hr.PayloadPatch) > 0 && string(hr.PayloadPatch) != "{}" {
				hr.PayloadPatch = injectSourceState(hr.PayloadPatch, string(task.Status))
				merged, err := project.MergePayloadPatch(payload, hr.PayloadPatch, hr.ID, hr.allowedTraits(matchedHooks))
				if err != nil {
					slog.Warn("payload merge failed", "hook_id", hr.ID, "error", err)
					continue
				}
				payload = merged
			}
		}
	}

	// 2. Evaluate and dispatch gates (always parallel)
	matchedGates := d.Evaluator.EvaluateGates(task, meta.Gates)
	if len(matchedGates) > 0 {
		gateResults, err := d.dispatchGates(ctx, task, matchedGates)
		if err != nil {
			return nil, fmt.Errorf("gate dispatch: %w", err)
		}
		for _, gr := range gateResults {
			allResults = append(allResults, gr)
			if err := checkExclusiveCollision(gr.PayloadPatch, gr.ID, exclusiveWriters); err != nil {
				return nil, err
			}
			if len(gr.PayloadPatch) > 0 && string(gr.PayloadPatch) != "{}" {
				gr.PayloadPatch = injectSourceState(gr.PayloadPatch, string(task.Status))
				merged, err := project.MergePayloadPatch(payload, gr.PayloadPatch, gr.ID, gr.allowedTraits(nil))
				if err != nil {
					slog.Warn("payload merge failed", "gate_id", gr.ID, "error", err)
					continue
				}
				payload = merged
			}
		}
	}

	// 3. Evaluate auto-advance
	result := &DispatchResult{
		Results:      allResults,
		FinalPayload: payload,
	}

	advanceTask := *task
	advanceTask.Payload = payload
	if newTask, ok := sm.Advance(&advanceTask); ok {
		result.NewStatus = newTask.Status
	}

	return result, nil
}

// dispatchHooks executes hooks, either in parallel (readonly) or sequentially.
func (d *Coordinator) dispatchHooks(
	ctx context.Context,
	task *Task,
	hooks []project.Hook,
	parallel bool,
) ([]HandlerResult, error) {
	if parallel {
		return d.dispatchParallel(ctx, task, hooks)
	}
	return d.dispatchSequential(ctx, task, hooks)
}

func (d *Coordinator) dispatchSequential(
	ctx context.Context,
	task *Task,
	hooks []project.Hook,
) ([]HandlerResult, error) {
	var results []HandlerResult
	for _, h := range hooks {
		event := &project.HookFireEvent{
			EventID:   fmt.Sprintf("evt-%s-%s", task.ID[:8], h.ID),
			TaskID:    task.ID,
			ProjectID: task.ProjectID,
			Hook:      h,
		}

		jobID, err := d.HookExecutor.ExecuteHook(ctx, event)
		if err != nil {
			return results, fmt.Errorf("execute hook %q: %w", h.ID, err)
		}

		completion, err := d.Waiter.WaitForJob(ctx, jobID)
		if err != nil {
			return results, fmt.Errorf("wait hook %q: %w", h.ID, err)
		}

		results = append(results, parseHandlerResult(h.ID, project.RoleHook, completion))
	}
	return results, nil
}

func (d *Coordinator) dispatchParallel(
	ctx context.Context,
	task *Task,
	hooks []project.Hook,
) ([]HandlerResult, error) {
	type jobInfo struct {
		hookID string
		jobID  string
	}

	// Launch all hooks
	var jobs []jobInfo
	for _, h := range hooks {
		event := &project.HookFireEvent{
			EventID:   fmt.Sprintf("evt-%s-%s", task.ID[:8], h.ID),
			TaskID:    task.ID,
			ProjectID: task.ProjectID,
			Hook:      h,
		}

		jobID, err := d.HookExecutor.ExecuteHook(ctx, event)
		if err != nil {
			return nil, fmt.Errorf("execute hook %q: %w", h.ID, err)
		}
		jobs = append(jobs, jobInfo{hookID: h.ID, jobID: jobID})
	}

	// Wait for all
	results := make([]HandlerResult, len(jobs))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for i, j := range jobs {
		wg.Add(1)
		go func(idx int, ji jobInfo) {
			defer wg.Done()
			completion, err := d.Waiter.WaitForJob(ctx, ji.jobID)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("wait hook %q: %w", ji.hookID, err)
				}
				mu.Unlock()
				return
			}
			results[idx] = parseHandlerResult(ji.hookID, project.RoleHook, completion)
		}(i, j)
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	return results, nil
}

// dispatchGates executes gates in parallel (gates have no FS, always safe).
func (d *Coordinator) dispatchGates(
	ctx context.Context,
	task *Task,
	gates []project.Gate,
) ([]HandlerResult, error) {
	type jobInfo struct {
		gateID string
		jobID  string
	}

	var jobs []jobInfo
	for _, g := range gates {
		event := &project.GateFireEvent{
			EventID:   fmt.Sprintf("evt-%s-%s", task.ID[:8], g.ID),
			TaskID:    task.ID,
			ProjectID: task.ProjectID,
			Gate:      g,
		}

		jobID, err := d.GateExecutor.ExecuteGate(ctx, event)
		if err != nil {
			return nil, fmt.Errorf("execute gate %q: %w", g.ID, err)
		}
		jobs = append(jobs, jobInfo{gateID: g.ID, jobID: jobID})
	}

	results := make([]HandlerResult, len(jobs))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for i, j := range jobs {
		wg.Add(1)
		go func(idx int, ji jobInfo) {
			defer wg.Done()
			completion, err := d.Waiter.WaitForJob(ctx, ji.jobID)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("wait gate %q: %w", ji.gateID, err)
				}
				mu.Unlock()
				return
			}
			results[idx] = parseHandlerResult(ji.gateID, project.RoleGate, completion)
		}(i, j)
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	return results, nil
}

// checkExclusiveCollision detects if an exclusive trait is written by multiple handlers.
func checkExclusiveCollision(patch json.RawMessage, writerID string, exclusiveWriters map[string]string) error {
	if len(patch) == 0 || string(patch) == "{}" || string(patch) == "null" {
		return nil
	}

	var patchMap map[string]json.RawMessage
	if err := json.Unmarshal(patch, &patchMap); err != nil {
		return nil // invalid patch will be caught later
	}

	for key := range patchMap {
		if project.TraitMergeMode(project.TraitType(key)) == project.MergeModeExclusive {
			if prev, exists := exclusiveWriters[key]; exists {
				return fmt.Errorf("exclusive trait %q written by both %q and %q", key, prev, writerID)
			}
			exclusiveWriters[key] = writerID
		}
	}
	return nil
}

// parseHandlerResult extracts payload_patch from job output.
func parseHandlerResult(id string, role project.Role, c JobCompletion) HandlerResult {
	hr := HandlerResult{
		ID:       id,
		Role:     role,
		ExitCode: c.ExitCode,
	}

	if c.Output == "" {
		return hr
	}

	// Parse {"payload_patch": {...}} from output
	var output struct {
		PayloadPatch json.RawMessage `json:"payload_patch"`
	}
	if err := json.Unmarshal([]byte(c.Output), &output); err != nil {
		slog.Warn("failed to parse handler output", "id", id, "error", err)
		return hr
	}
	hr.PayloadPatch = output.PayloadPatch
	return hr
}

// injectSourceState adds source_state to the verification value in a payload_patch.
func injectSourceState(patch json.RawMessage, state string) json.RawMessage {
	if len(patch) == 0 || string(patch) == "{}" || string(patch) == "null" {
		return patch
	}

	var patchMap map[string]json.RawMessage
	if err := json.Unmarshal(patch, &patchMap); err != nil {
		return patch
	}

	vRaw, ok := patchMap["verification"]
	if !ok || string(vRaw) == "null" {
		return patch
	}

	var vMap map[string]json.RawMessage
	if err := json.Unmarshal(vRaw, &vMap); err != nil {
		return patch
	}

	stateJSON, _ := json.Marshal(state)
	vMap["source_state"] = stateJSON

	vBytes, err := json.Marshal(vMap)
	if err != nil {
		return patch
	}
	patchMap["verification"] = vBytes

	result, err := json.Marshal(patchMap)
	if err != nil {
		return patch
	}
	return result
}

// allowedTraits returns the requires_traits for this handler from the hook list.
func (hr *HandlerResult) allowedTraits(hooks []project.Hook) []project.TraitType {
	for _, h := range hooks {
		if h.ID == hr.ID {
			return h.RequiresTraits
		}
	}
	return nil
}
