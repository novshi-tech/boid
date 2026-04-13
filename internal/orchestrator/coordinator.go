package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"gopkg.in/yaml.v3"
)

// Coordinator orchestrates the hook → gate → advance flow.
type Coordinator struct {
	Evaluator    *Evaluator
	HookExecutor HookExecutor
	GateExecutor GateExecutor
	Waiter       JobWaiter
	MaxDepth     int
	Locker       WorktreeLocker // optional; nil skips locking
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
	meta *ProjectMeta,
	sm *StateMachine,
) (*DispatchResult, error) {
	readonly := IsReadonly(task)
	payload := task.Payload
	var allResults []HandlerResult
	var firedEvents []FiredEvent
	exclusiveWriters := map[string]string{} // trait key → first writer ID

	// 1. Evaluate and dispatch hooks
	matchedHooks := d.Evaluator.Evaluate(task, meta.Hooks)
	if len(matchedHooks) > 0 {
		hookResults, err := d.dispatchHooksLocked(ctx, task, matchedHooks, readonly)
		if err != nil {
			return nil, fmt.Errorf("hook dispatch: %w", err)
		}
		for _, hr := range hookResults {
			allResults = append(allResults, hr)
			firedEvents = append(firedEvents, buildFiredEvent(hr, "hook", string(task.Status), matchedHooks, nil))
			if err := checkExclusiveCollision(hr.PayloadPatch, hr.ID, exclusiveWriters); err != nil {
				return nil, err
			}
			if len(hr.PayloadPatch) > 0 && string(hr.PayloadPatch) != "{}" {
				hr.PayloadPatch = injectSourceState(hr.PayloadPatch, string(task.Status))
				merged, err := MergePayloadPatch(payload, hr.PayloadPatch, hr.ID, hr.allowedTraits(matchedHooks))
				if err != nil {
					slog.Warn("payload merge failed", "hook_id", hr.ID, "error", err)
					continue
				}
				payload = merged
			}
		}
	}

	// 2. Evaluate and dispatch gates (always parallel)
	// Use hook-updated payload so that traits produced by hooks are visible to gates.
	gateTask := *task
	gateTask.Payload = payload
	matchedGates := d.Evaluator.EvaluateGates(&gateTask, meta.Gates, GatePhaseExit)
	if len(matchedGates) > 0 {
		gateResults, err := d.dispatchGates(ctx, &gateTask, matchedGates)
		if err != nil {
			return nil, fmt.Errorf("gate dispatch: %w", err)
		}
		for _, gr := range gateResults {
			allResults = append(allResults, gr)
			firedEvents = append(firedEvents, buildFiredEvent(gr, "exit_gate", string(task.Status), nil, matchedGates))
			if err := checkExclusiveCollision(gr.PayloadPatch, gr.ID, exclusiveWriters); err != nil {
				return nil, err
			}
			if len(gr.PayloadPatch) > 0 && string(gr.PayloadPatch) != "{}" {
				gr.PayloadPatch = injectSourceState(gr.PayloadPatch, string(task.Status))
				merged, err := MergePayloadPatch(payload, gr.PayloadPatch, gr.ID, gr.allowedTraitsFromGates(matchedGates))
				if err != nil {
					slog.Warn("payload merge failed", "gate_id", gr.ID, "error", err)
					continue
				}
				payload = merged
			}
		}
	}

	// 3. executing 状態で hook が実際に実行された場合のみ execution_complete を注入する。
	// hook が1件も実行されなかった場合（hooks 未設定の behavior 等）は注入しない。
	// これにより成果物なし（artifact も tasks も書かれなかった）ケースの
	// executing → done 自動遷移が機能する。
	if task.Status == TaskStatusExecuting && hasHookResult(allResults) {
		payload = injectExecutionComplete(payload)
	}

	// 4. Evaluate auto-advance
	result := &DispatchResult{
		Results:      allResults,
		FiredEvents:  firedEvents,
		FinalPayload: payload,
	}

	advanceTask := *task
	advanceTask.Payload = payload
	if newTask, ok := sm.Advance(&advanceTask); ok {
		result.NewStatus = newTask.Status
	}

	return result, nil
}

// DispatchEntryGates runs entry-phase gates for the given task's current status.
// Unlike DispatchAndAdvance, this does NOT evaluate hooks/exit-gates or call sm.Advance.
// The returned result reflects only entry gate payload patches.
//
// executing 状態への入場時は execution_complete trait をクリアする。
// これにより将来的に executing を再入場するケースでも stale な値が残らない。
func (d *Coordinator) DispatchEntryGates(
	ctx context.Context,
	task *Task,
	meta *ProjectMeta,
) (*EntryGateResult, error) {
	// executing 入場時に execution_complete をリセットする
	payload := task.Payload
	if task.Status == TaskStatusExecuting {
		payload = clearTraitFromPayload(payload, "execution_complete")
	}

	matchedGates := d.Evaluator.EvaluateGates(task, meta.Gates, GatePhaseEntry)
	if len(matchedGates) == 0 {
		return &EntryGateResult{FinalPayload: payload}, nil
	}

	gateResults, err := d.dispatchGates(ctx, task, matchedGates)
	if err != nil {
		return nil, fmt.Errorf("entry gate dispatch: %w", err)
	}
	exclusiveWriters := map[string]string{}
	var firedEvents []FiredEvent
	for _, gr := range gateResults {
		firedEvents = append(firedEvents, buildFiredEvent(gr, "entry_gate", string(task.Status), nil, matchedGates))
		if err := checkExclusiveCollision(gr.PayloadPatch, gr.ID, exclusiveWriters); err != nil {
			return nil, err
		}
		if len(gr.PayloadPatch) > 0 && string(gr.PayloadPatch) != "{}" {
			gr.PayloadPatch = injectSourceState(gr.PayloadPatch, string(task.Status))
			merged, err := MergePayloadPatch(payload, gr.PayloadPatch, gr.ID, gr.allowedTraitsFromGates(matchedGates))
			if err != nil {
				slog.Warn("entry gate payload merge failed", "gate_id", gr.ID, "error", err)
				continue
			}
			payload = merged
		}
	}

	return &EntryGateResult{
		Results:      gateResults,
		FiredEvents:  firedEvents,
		FinalPayload: payload,
	}, nil
}

// dispatchHooksLocked wraps dispatchHooks with an optional worktree lock.
// The lock is acquired for non-readonly, non-worktree tasks and released
// via defer after dispatchHooks completes (gates are excluded from the lock scope).
func (d *Coordinator) dispatchHooksLocked(
	ctx context.Context,
	task *Task,
	hooks []Hook,
	readonly bool,
) ([]HandlerResult, error) {
	if d.Locker != nil && !readonly && !task.Worktree {
		release, err := d.Locker.Acquire(ctx, task.ProjectID)
		if err != nil {
			return nil, fmt.Errorf("worktree lock: %w", err)
		}
		defer release()
	}
	return d.dispatchHooks(ctx, task, hooks, readonly)
}

// dispatchHooks executes hooks, either in parallel (readonly) or sequentially.
func (d *Coordinator) dispatchHooks(
	ctx context.Context,
	task *Task,
	hooks []Hook,
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
	hooks []Hook,
) ([]HandlerResult, error) {
	var results []HandlerResult
	for _, h := range hooks {
		event := &HookFireEvent{
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

		results = append(results, parseHandlerResult(h.ID, RoleHook, completion))
	}
	return results, nil
}

func (d *Coordinator) dispatchParallel(
	ctx context.Context,
	task *Task,
	hooks []Hook,
) ([]HandlerResult, error) {
	type jobInfo struct {
		hookID string
		jobID  string
	}

	// Launch all hooks
	var jobs []jobInfo
	for _, h := range hooks {
		event := &HookFireEvent{
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
			results[idx] = parseHandlerResult(ji.hookID, RoleHook, completion)
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
	gates []Gate,
) ([]HandlerResult, error) {
	type jobInfo struct {
		gateID string
		jobID  string
	}

	var jobs []jobInfo
	for _, g := range gates {
		event := &GateFireEvent{
			EventID:         fmt.Sprintf("evt-%s-%s", task.ID[:8], g.ID),
			TaskID:          task.ID,
			ProjectID:       task.ProjectID,
			Gate:            g,
			TaskPayloadJSON: string(task.Payload),
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
			results[idx] = parseHandlerResult(ji.gateID, RoleGate, completion)
		}(i, j)
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	return results, nil
}

// buildFiredEvent constructs a FiredEvent from a HandlerResult.
// hooks is consulted for hook kind, gates for gate kinds; pass nil for the unused slice.
func buildFiredEvent(hr HandlerResult, kind string, sourceState string, hooks []Hook, gates []Gate) FiredEvent {
	kitID := ""
	for _, h := range hooks {
		if h.ID == hr.ID {
			kitID = h.Kit
			break
		}
	}
	for _, g := range gates {
		if g.ID == hr.ID {
			kitID = g.Kit
			break
		}
	}
	fe := FiredEvent{
		KitID:       kitID,
		HandlerID:   hr.ID,
		Kind:        kind,
		SourceState: sourceState,
		Success:     hr.ExitCode == 0,
	}
	if hr.ExitCode != 0 {
		fe.Error = fmt.Sprintf("exit code %d", hr.ExitCode)
	}
	return fe
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
		if TraitMergeMode(TraitType(key)) == MergeModeExclusive {
			if prev, exists := exclusiveWriters[key]; exists {
				return fmt.Errorf("exclusive trait %q written by both %q and %q", key, prev, writerID)
			}
			exclusiveWriters[key] = writerID
		}
	}
	return nil
}

// parseHandlerResult extracts payload_patch from job output.
func parseHandlerResult(id string, role Role, c JobCompletion) HandlerResult {
	hr := HandlerResult{
		ID:       id,
		Role:     role,
		ExitCode: c.ExitCode,
	}

	if c.Output == "" {
		return hr
	}

	// Parse payload_patch from YAML output (JSON is also accepted as valid YAML)
	var outputMap map[string]interface{}
	if err := yaml.Unmarshal([]byte(c.Output), &outputMap); err != nil {
		slog.Warn("failed to parse handler output", "id", id, "error", err)
		return hr
	}
	patchVal, ok := outputMap["payload_patch"]
	if !ok {
		return hr
	}
	patchJSON, err := json.Marshal(patchVal)
	if err != nil {
		slog.Warn("failed to marshal payload_patch", "id", id, "error", err)
		return hr
	}
	hr.PayloadPatch = patchJSON
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

// hasHookResult reports whether any HandlerResult with RoleHook is present.
// Used to gate execution_complete injection: only hooks (not gates) represent
// the main execution agent job.
func hasHookResult(results []HandlerResult) bool {
	for _, hr := range results {
		if hr.Role == RoleHook {
			return true
		}
	}
	return false
}

// injectExecutionComplete sets execution_complete=true in the payload.
// This is called by the coordinator after all hooks/gates complete successfully
// in executing state, signalling that the agent job finished without error.
func injectExecutionComplete(payload json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if len(payload) == 0 || string(payload) == "null" {
		m = make(map[string]json.RawMessage)
	} else if err := json.Unmarshal(payload, &m); err != nil {
		return payload
	}
	m["execution_complete"] = json.RawMessage("true")
	result, err := json.Marshal(m)
	if err != nil {
		return payload
	}
	return result
}

// clearTraitFromPayload removes the given trait key from the payload.
// Used to reset execution_complete when re-entering executing state.
func clearTraitFromPayload(payload json.RawMessage, trait string) json.RawMessage {
	if len(payload) == 0 || string(payload) == "{}" || string(payload) == "null" {
		return payload
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return payload
	}
	if _, ok := m[trait]; !ok {
		return payload
	}
	delete(m, trait)
	result, err := json.Marshal(m)
	if err != nil {
		return payload
	}
	return result
}

// allowedTraits returns the produces traits for this handler from the hook list.
func (hr *HandlerResult) allowedTraits(hooks []Hook) []TraitType {
	for _, h := range hooks {
		if h.ID == hr.ID {
			return h.Traits.Produces
		}
	}
	return nil
}

// allowedTraitsFromGates returns the produces traits for this handler from the gate list.
func (hr *HandlerResult) allowedTraitsFromGates(gates []Gate) []TraitType {
	for _, g := range gates {
		if g.ID == hr.ID {
			return g.Traits.Produces
		}
	}
	return nil
}
