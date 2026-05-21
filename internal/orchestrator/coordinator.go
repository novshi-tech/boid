package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"gopkg.in/yaml.v3"
)

// lookupBehavior returns the TaskBehavior that matches task.Behavior. Returns
// false if the project has no matching behavior; callers should treat that as
// "no hooks" rather than an error.
func lookupBehavior(meta *ProjectMeta, task *Task) (TaskBehavior, bool) {
	if meta == nil || task == nil {
		return TaskBehavior{}, false
	}
	b, ok := meta.TaskBehaviors[task.Behavior]
	return b, ok
}

// Coordinator orchestrates the hook → advance flow.
//
// Locking is owned by the workflow service for the entire executing lifetime
// of each task (see internal/api/service.go). The coordinator dispatches hooks
// under the assumption that the branch lock is already held by the caller.
type Coordinator struct {
	Evaluator      *Evaluator
	HookExecutor   HookExecutor
	Waiter         JobWaiter
	MaxDepth       int
	LifecycleStore LifecycleStore // optional; nil skips rework_count/abort derivation
}

// DispatchAndAdvance runs the full dispatch cycle:
// 1. Evaluate and execute hooks (parallel if readonly, sequential otherwise)
// 2. Merge hook payload patches
// 3. Evaluate condition-based auto-advance via state machine
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

	hookExclusiveWriters := map[string]string{}

	// 1. Evaluate and dispatch hooks
	behavior, hasBehavior := lookupBehavior(meta, task)
	var behaviorHooks []Hook
	if hasBehavior {
		behaviorHooks = behavior.Hooks
	}
	matchedHooks := d.Evaluator.Evaluate(task, behaviorHooks)
	if len(matchedHooks) > 0 {
		hookResults, err := d.dispatchHooks(ctx, task, matchedHooks, readonly)
		// Always record FiredEvents for hooks that ran — even on error the
		// partial results let the caller persist hook_fired actions, which is
		// what makes failed runs visible in the UI timeline.
		for _, hr := range hookResults {
			firedEvents = append(firedEvents, buildFiredEvent(hr, "hook", string(task.Status), matchedHooks))
		}
		if err != nil {
			return &DispatchResult{FiredEvents: firedEvents}, fmt.Errorf("hook dispatch: %w", err)
		}
		for _, hr := range hookResults {
			allResults = append(allResults, hr)
			if err := checkExclusiveCollision(hr.PayloadPatch, hr.ID, hookExclusiveWriters); err != nil {
				return &DispatchResult{FiredEvents: firedEvents}, err
			}
			if len(hr.PayloadPatch) > 0 && string(hr.PayloadPatch) != "{}" {
				merged, err := MergePayloadPatch(payload, hr.PayloadPatch, hr.ID, hr.allowedTraits(matchedHooks))
				if err != nil {
					slog.Warn("payload merge failed", "hook_id", hr.ID, "error", err)
					continue
				}
				payload = merged
			}
		}
	}

	// 2. Lifecycle derivation + auto-advance.
	hookRan := task.Status == TaskStatusExecuting && hasHookResult(allResults)
	finalPayload, newStatus, actionPayload, err := d.evaluateAndAdvance(ctx, task, meta, sm, payload, hookRan)
	if err != nil {
		return &DispatchResult{FiredEvents: firedEvents}, err
	}

	return &DispatchResult{
		Results:       allResults,
		FiredEvents:   firedEvents,
		FinalPayload:  finalPayload,
		NewStatus:     newStatus,
		ActionPayload: actionPayload,
	}, nil
}

// evaluateAndAdvance derives lifecycle traits and evaluates sm.Advance.
// It is shared by DispatchAndAdvance and ReplayHook so that both paths
// apply identical post-hook logic.
//
// hookRan must be true when a hook actually executed in this dispatch cycle
// (used for lifecycle.executed derivation). payload is the hook-merged payload
// to feed into the state machine.
//
// Returns: (finalPayload, newStatus, actionPayload, error).
// newStatus is empty when no advance occurred. lifecycle is NOT included in
// finalPayload — it is transient and must not be persisted.
func (d *Coordinator) evaluateAndAdvance(
	ctx context.Context,
	task *Task,
	meta *ProjectMeta,
	sm *StateMachine,
	payload json.RawMessage,
	hookRan bool,
) (json.RawMessage, TaskStatus, json.RawMessage, error) {
	// Derive lifecycle traits (transient — not persisted).
	lc, err := DeriveLifecycle(ctx, task.ID, d.LifecycleStore, hookRan)
	if err != nil {
		slog.Warn("lifecycle derivation failed", "task_id", task.ID, "error", err)
		lc = Lifecycle{Executed: hookRan}
	}
	payloadForSM := injectLifecycle(payload, lc)

	// Evaluate auto-advance.
	var newStatus TaskStatus
	var actionPayload json.RawMessage
	advanceTask := *task
	advanceTask.Payload = payloadForSM
	if outcome := sm.AdvanceFull(&advanceTask); outcome != nil {
		newStatus = outcome.Task.Status
		actionPayload = outcome.ActionPayload
	}

	return payload, newStatus, actionPayload, nil
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

		hr := parseHandlerResult(h.ID, RoleHook, completion)
		results = append(results, hr)

		// Stop after a failed hook: subsequent hooks on a non-readonly task
		// often depend on the prior hook's payload_patch, so running them on
		// stale state is unlikely to help and may mask the real failure.
		// The partial results are still returned so the caller can persist
		// FiredEvents for every hook that actually ran (incl. the failing one).
		if hr.ExitCode != 0 {
			return results, fmt.Errorf("hook %q failed: exit code %d", h.ID, hr.ExitCode)
		}
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
	// Aggregate exit codes: a hook that exited non-zero must surface as an
	// error so DispatchAndAdvance early-returns before lifecycle.executed=true
	// is derived. Without this, a failed hook would be treated as a successful
	// run and a readonly task could auto-advance to done. Mirrors the
	// dispatchSequential contract — the partial results are returned so the
	// caller can persist FiredEvents for every hook that ran (incl. failures).
	for _, hr := range results {
		if hr.ExitCode != 0 {
			return results, fmt.Errorf("hook %q failed: exit code %d", hr.ID, hr.ExitCode)
		}
	}
	return results, nil
}

// buildFiredEvent constructs a FiredEvent from a HandlerResult.
func buildFiredEvent(hr HandlerResult, kind string, sourceState string, hooks []Hook) FiredEvent {
	kitID := ""
	for _, h := range hooks {
		if h.ID == hr.ID {
			kitID = h.Kit
			break
		}
	}
	fe := FiredEvent{
		KitID:       kitID,
		HandlerID:   hr.ID,
		JobID:       hr.JobID,
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
		JobID:    c.JobID,
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
	// yaml.v3 は非 string キー (bool/int/null/float) を含む内側 map を
	// map[interface{}]interface{} で返すため、そのままでは json.Marshal が落ちる。
	// 過去事例: agent が `on: verifying` と書いた YAML が PyYAML の round-trip で
	// `true: verifying` に化け、Layer 2 がないと payload_patch がまるごと silent drop した。
	patchVal = normalizeYAMLKeys(patchVal)
	patchJSON, err := json.Marshal(patchVal)
	if err != nil {
		slog.Warn("failed to marshal payload_patch", "id", id, "error", err)
		return hr
	}
	hr.PayloadPatch = patchJSON
	return hr
}

// normalizeYAMLKeys は yaml.v3 が非 string キーで decode した
// map[interface{}]interface{} を再帰的に map[string]interface{} に正規化する。
// 非 string キーは fmt.Sprint で stringify する (true→"true"、42→"42"、nil→"<nil>")。
// 既に map[string]interface{} の枝も再帰で下る。
func normalizeYAMLKeys(v interface{}) interface{} {
	switch x := v.(type) {
	case map[interface{}]interface{}:
		m := make(map[string]interface{}, len(x))
		for k, val := range x {
			ks, ok := k.(string)
			if !ok {
				ks = fmt.Sprint(k)
			}
			m[ks] = normalizeYAMLKeys(val)
		}
		return m
	case map[string]interface{}:
		for k, val := range x {
			x[k] = normalizeYAMLKeys(val)
		}
		return x
	case []interface{}:
		for i, val := range x {
			x[i] = normalizeYAMLKeys(val)
		}
		return x
	default:
		return v
	}
}

// hasHookResult reports whether any HandlerResult with RoleHook is present.
// Used by the coordinator to detect that a hook actually ran in this dispatch cycle.
func hasHookResult(results []HandlerResult) bool {
	for _, hr := range results {
		if hr.Role == RoleHook {
			return true
		}
	}
	return false
}

// ReplayResult is the result of a single-hook replay operation.
type ReplayResult struct {
	Result       HandlerResult
	FinalPayload json.RawMessage
	NewStatus    TaskStatus
	FiredEvents  []FiredEvent
}

// ReplayHook executes a single named hook in isolation against the task's
// current state. After the hook completes, lifecycle is derived and sm.Advance
// is applied — identical to the post-hook phase of DispatchAndAdvance.
//
// Returns an error if the hook ID is not found in the behavior or if the hook
// does not match the current status (e.g. task not in executing state).
func (d *Coordinator) ReplayHook(
	ctx context.Context,
	task *Task,
	meta *ProjectMeta,
	sm *StateMachine,
	hookID string,
) (*ReplayResult, error) {
	behavior, ok := lookupBehavior(meta, task)
	if !ok {
		return nil, fmt.Errorf("behavior %q not found in project meta", task.Behavior)
	}

	// Find hook by ID.
	var found *Hook
	for i := range behavior.Hooks {
		if behavior.Hooks[i].ID == hookID {
			h := behavior.Hooks[i]
			found = &h
			break
		}
	}
	if found == nil {
		return nil, fmt.Errorf("hook %q not found in behavior %q", hookID, task.Behavior)
	}

	// Check hook matches current status.
	matched := d.Evaluator.Evaluate(task, []Hook{*found})
	if len(matched) == 0 {
		return nil, fmt.Errorf("hook %q does not match current status %q", hookID, task.Status)
	}

	// Execute the hook. Project-level locking is owned by the workflow service
	// (executing-lifetime lock); the replay path here runs in-line with the
	// caller and assumes the surrounding task is currently held.
	readonly := IsReadonly(task)
	hookResults, err := d.dispatchHooks(ctx, task, matched, readonly)

	payload := task.Payload
	var result HandlerResult
	var firedEvents []FiredEvent

	for _, hr := range hookResults {
		result = hr
		firedEvents = append(firedEvents, buildFiredEvent(hr, "hook_replay", string(task.Status), matched))
		if len(hr.PayloadPatch) > 0 && string(hr.PayloadPatch) != "{}" {
			merged, mergeErr := MergePayloadPatch(payload, hr.PayloadPatch, hr.ID, hr.allowedTraits(matched))
			if mergeErr != nil {
				slog.Warn("hook replay payload merge failed", "hook_id", hr.ID, "error", mergeErr)
			} else {
				payload = merged
			}
		}
	}

	if err != nil {
		return nil, fmt.Errorf("hook replay: %w", err)
	}

	// Evaluate lifecycle + advance (same as DispatchAndAdvance post-hook phase).
	finalPayload, newStatus, _, exitErr := d.evaluateAndAdvance(ctx, task, meta, sm, payload, true)
	if exitErr != nil {
		return nil, fmt.Errorf("hook replay advance: %w", exitErr)
	}

	return &ReplayResult{
		Result:       result,
		FinalPayload: finalPayload,
		NewStatus:    newStatus,
		FiredEvents:  firedEvents,
	}, nil
}

// ListHooksForStatus returns hooks from the behavior that would match the given
// status. Since hooks only fire during executing, only executing status yields
// results. Used by "boid task hook list".
func ListHooksForStatus(meta *ProjectMeta, task *Task, status TaskStatus) []Hook {
	behavior, ok := lookupBehavior(meta, task)
	if !ok {
		return nil
	}
	eval := &Evaluator{}
	probe := *task
	probe.Status = status
	return eval.Evaluate(&probe, behavior.Hooks)
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
