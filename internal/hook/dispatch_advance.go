package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/novshi-tech/boid/internal/model"
	"github.com/novshi-tech/boid/internal/reducer"
)

// AdvancedDispatcher orchestrates the hook → gate → advance flow.
type AdvancedDispatcher struct {
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
func (d *AdvancedDispatcher) DispatchAndAdvance(
	ctx context.Context,
	task *model.Task,
	meta *model.ProjectMeta,
	behavior *model.TaskBehavior,
	sm *reducer.StateMachine,
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
				merged, err := model.MergePayloadPatch(payload, hr.PayloadPatch, hr.ID, hr.allowedTraits(matchedHooks))
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
				merged, err := model.MergePayloadPatch(payload, gr.PayloadPatch, gr.ID, gr.allowedTraits(nil))
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
func (d *AdvancedDispatcher) dispatchHooks(
	ctx context.Context,
	task *model.Task,
	hooks []model.Hook,
	parallel bool,
) ([]HandlerResult, error) {
	if parallel {
		return d.dispatchParallel(ctx, task, hooks)
	}
	return d.dispatchSequential(ctx, task, hooks)
}

func (d *AdvancedDispatcher) dispatchSequential(
	ctx context.Context,
	task *model.Task,
	hooks []model.Hook,
) ([]HandlerResult, error) {
	var results []HandlerResult
	for _, h := range hooks {
		event := &model.HookFireEvent{
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

		results = append(results, parseHandlerResult(h.ID, model.RoleHook, completion))
	}
	return results, nil
}

func (d *AdvancedDispatcher) dispatchParallel(
	ctx context.Context,
	task *model.Task,
	hooks []model.Hook,
) ([]HandlerResult, error) {
	type jobInfo struct {
		hookID string
		jobID  string
	}

	// Launch all hooks
	var jobs []jobInfo
	for _, h := range hooks {
		event := &model.HookFireEvent{
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
			results[idx] = parseHandlerResult(ji.hookID, model.RoleHook, completion)
		}(i, j)
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	return results, nil
}

// dispatchGates executes gates in parallel (gates have no FS, always safe).
func (d *AdvancedDispatcher) dispatchGates(
	ctx context.Context,
	task *model.Task,
	gates []model.Gate,
) ([]HandlerResult, error) {
	type jobInfo struct {
		gateID string
		jobID  string
	}

	var jobs []jobInfo
	for _, g := range gates {
		event := &model.GateFireEvent{
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
			results[idx] = parseHandlerResult(ji.gateID, model.RoleGate, completion)
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
		if model.TraitMergeMode(model.TraitType(key)) == model.MergeModeExclusive {
			if prev, exists := exclusiveWriters[key]; exists {
				return fmt.Errorf("exclusive trait %q written by both %q and %q", key, prev, writerID)
			}
			exclusiveWriters[key] = writerID
		}
	}
	return nil
}

// parseHandlerResult extracts payload_patch from job output.
func parseHandlerResult(id string, role model.Role, c JobCompletion) HandlerResult {
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

// allowedTraits returns the requires_traits for this handler from the hook list.
// For gates, the gates list would be needed; for simplicity we accept nil
// and allow all traits (validation is deferred to MergePayloadPatch).
func (hr *HandlerResult) allowedTraits(hooks []model.Hook) []model.TraitType {
	for _, h := range hooks {
		if h.ID == hr.ID {
			return h.RequiresTraits
		}
	}
	// If not found in hooks (e.g. gate), return nil to skip validation
	// Gate trait validation should be handled separately
	return nil
}
