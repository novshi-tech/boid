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
// "no hooks, no gates" rather than an error.
func lookupBehavior(meta *ProjectMeta, task *Task) (TaskBehavior, bool) {
	if meta == nil || task == nil {
		return TaskBehavior{}, false
	}
	b, ok := meta.TaskBehaviors[task.Behavior]
	return b, ok
}

// Coordinator orchestrates the hook → gate → advance flow.
type Coordinator struct {
	Evaluator      *Evaluator
	HookExecutor   HookExecutor
	GateExecutor   GateExecutor
	Waiter         JobWaiter
	MaxDepth       int
	Locker         WorktreeLocker // optional; nil skips locking
	LifecycleStore LifecycleStore // optional; nil skips rework_count/abort derivation
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

	// hook と exit gate は順次処理される別フェーズで、 collision 検査は各フェーズ内に
	// 閉じる。 フェーズ跨ぎで同じ exclusive trait に書き込むケースは、 patch 値の
	// sub-key deep merge (MergePayloadPatch) によって両者の寄与を保持する。
	hookExclusiveWriters := map[string]string{}
	gateExclusiveWriters := map[string]string{}

	// 1. Evaluate and dispatch hooks
	behavior, hasBehavior := lookupBehavior(meta, task)
	var behaviorHooks []Hook
	var behaviorGates []Gate
	if hasBehavior {
		behaviorHooks = behavior.Hooks
		behaviorGates = behavior.Gates
	}
	matchedHooks := d.Evaluator.Evaluate(task, behaviorHooks)
	if len(matchedHooks) > 0 {
		hookResults, err := d.dispatchHooksLocked(ctx, task, matchedHooks, readonly)
		// Always record FiredEvents for hooks that ran — even on error the
		// partial results let the caller persist hook_fired actions, which is
		// what makes failed runs visible in the UI timeline.
		for _, hr := range hookResults {
			firedEvents = append(firedEvents, buildFiredEvent(hr, "hook", string(task.Status), matchedHooks, nil))
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

	// 2. Evaluate and dispatch task.exit gates (always parallel)
	// Use hook-updated payload so that traits produced by hooks are visible to gates.
	gateTask := *task
	gateTask.Payload = payload
	matchedGates := d.Evaluator.EvaluateGates(&gateTask, behaviorGates, GatePhaseExit)
	if len(matchedGates) > 0 {
		gateResults, err := d.dispatchGates(ctx, &gateTask, matchedGates)
		for _, gr := range gateResults {
			firedEvents = append(firedEvents, buildFiredEvent(gr, "exit_gate", string(task.Status), nil, matchedGates))
		}
		if err != nil {
			return &DispatchResult{FiredEvents: firedEvents}, fmt.Errorf("gate dispatch: %w", err)
		}
		for _, gr := range gateResults {
			allResults = append(allResults, gr)
			if err := checkExclusiveCollision(gr.PayloadPatch, gr.ID, gateExclusiveWriters); err != nil {
				return &DispatchResult{FiredEvents: firedEvents}, err
			}
			if len(gr.PayloadPatch) > 0 && string(gr.PayloadPatch) != "{}" {
				merged, err := MergePayloadPatch(payload, gr.PayloadPatch, gr.ID, gr.allowedTraitsFromGates(matchedGates))
				if err != nil {
					slog.Warn("payload merge failed", "gate_id", gr.ID, "error", err)
					continue
				}
				payload = merged
			}
		}
	}

	// 3. Derive lifecycle traits and inject into a transient payload copy for
	// state-machine evaluation. lifecycle is NOT included in FinalPayload so it
	// is never persisted to the DB.
	hookRan := task.Status == TaskStatusExecuting && hasHookResult(allResults)
	lc, err := DeriveLifecycle(ctx, task.ID, d.LifecycleStore, hookRan)
	if err != nil {
		slog.Warn("lifecycle derivation failed", "task_id", task.ID, "error", err)
		lc = Lifecycle{Executed: hookRan}
	}
	payloadForSM := injectLifecycle(payload, lc)

	// 4. Evaluate auto-advance
	result := &DispatchResult{
		Results:      allResults,
		FiredEvents:  firedEvents,
		FinalPayload: payload, // lifecycle excluded — not persisted
	}

	advanceTask := *task
	advanceTask.Payload = payloadForSM
	if outcome := sm.AdvanceFull(&advanceTask); outcome != nil {
		result.NewStatus = outcome.Task.Status
		result.ActionPayload = outcome.ActionPayload
	}

	return result, nil
}

// DispatchEntryGates runs entry-phase gates for the given task's current status.
// Unlike DispatchAndAdvance, this does NOT evaluate hooks/exit-gates or call sm.Advance.
// The returned result reflects only entry gate payload patches.
func (d *Coordinator) DispatchEntryGates(
	ctx context.Context,
	task *Task,
	meta *ProjectMeta,
) (*EntryGateResult, error) {
	payload := task.Payload

	var behaviorGates []Gate
	if behavior, ok := lookupBehavior(meta, task); ok {
		behaviorGates = behavior.Gates
	}
	matchedGates := d.Evaluator.EvaluateGates(task, behaviorGates, GatePhaseEntry)
	if len(matchedGates) == 0 {
		return &EntryGateResult{FinalPayload: payload}, nil
	}

	gateResults, dispatchErr := d.dispatchGates(ctx, task, matchedGates)
	var firedEvents []FiredEvent
	for _, gr := range gateResults {
		firedEvents = append(firedEvents, buildFiredEvent(gr, "entry_gate", string(task.Status), nil, matchedGates))
	}
	if dispatchErr != nil {
		return &EntryGateResult{FiredEvents: firedEvents}, fmt.Errorf("entry gate dispatch: %w", dispatchErr)
	}
	exclusiveWriters := map[string]string{}
	for _, gr := range gateResults {
		if err := checkExclusiveCollision(gr.PayloadPatch, gr.ID, exclusiveWriters); err != nil {
			return &EntryGateResult{FiredEvents: firedEvents}, err
		}
		if len(gr.PayloadPatch) > 0 && string(gr.PayloadPatch) != "{}" {
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
// Used by the coordinator to detect that a hook actually ran in this dispatch
// cycle (independent of gate-only dispatches).
func hasHookResult(results []HandlerResult) bool {
	for _, hr := range results {
		if hr.Role == RoleHook {
			return true
		}
	}
	return false
}

// ReplayResult is the result of a single-gate replay operation.
type ReplayResult struct {
	Result       HandlerResult
	FinalPayload json.RawMessage
	NewStatus    TaskStatus
	FiredEvents  []FiredEvent
}

// ReplayGate executes a single named gate in isolation against the task's
// current state. It is used by "boid task gate replay" to re-run a specific
// gate without triggering hooks or entry-gate chains.
//
// If the gate is an exit gate, sm.Advance is evaluated after the run and the
// new status is reported in ReplayResult.NewStatus (but NOT persisted — that
// is the caller's responsibility). Entry gates never trigger an advance.
//
// Returns an error if the gate ID is not found in the behavior or if the gate
// is not matched for the current status (400-class caller error).
func (d *Coordinator) ReplayGate(
	ctx context.Context,
	task *Task,
	meta *ProjectMeta,
	sm *StateMachine,
	gateID string,
) (*ReplayResult, error) {
	behavior, ok := lookupBehavior(meta, task)
	if !ok {
		return nil, fmt.Errorf("behavior %q not found in project meta", task.Behavior)
	}

	// Find the gate by ID.
	var found *Gate
	for i := range behavior.Gates {
		if behavior.Gates[i].ID == gateID {
			g := behavior.Gates[i]
			found = &g
			break
		}
	}
	if found == nil {
		return nil, fmt.Errorf("gate %q not found in behavior %q", gateID, task.Behavior)
	}

	// Determine phase (default exit when unset).
	phase := found.Phase
	if phase == "" {
		phase = GatePhaseExit
	}

	// Check the gate matches current status.
	matched := d.Evaluator.EvaluateGates(task, []Gate{*found}, phase)
	if len(matched) == 0 {
		return nil, fmt.Errorf("gate %q does not match current status %q", gateID, task.Status)
	}

	gateResults, err := d.dispatchGates(ctx, task, matched)
	if err != nil {
		return nil, fmt.Errorf("gate replay: %w", err)
	}

	payload := task.Payload
	var result HandlerResult
	var firedEvents []FiredEvent

	for _, gr := range gateResults {
		result = gr
		kind := "gate_replay"
		firedEvents = append(firedEvents, buildFiredEvent(gr, kind, string(task.Status), nil, matched))
		if len(gr.PayloadPatch) > 0 && string(gr.PayloadPatch) != "{}" {
			merged, err := MergePayloadPatch(payload, gr.PayloadPatch, gr.ID, gr.allowedTraitsFromGates(matched))
			if err != nil {
				slog.Warn("gate replay payload merge failed", "gate_id", gr.ID, "error", err)
			} else {
				payload = merged
			}
		}
	}

	replay := &ReplayResult{
		Result:       result,
		FinalPayload: payload,
		FiredEvents:  firedEvents,
	}

	// Only evaluate advance for exit gates.
	if phase == GatePhaseExit {
		advanceTask := *task
		advanceTask.Payload = payload
		if newTask, ok := sm.Advance(&advanceTask); ok {
			replay.NewStatus = newTask.Status
		}
	}

	return replay, nil
}

// ListGatesForStatus returns gates from the behavior that would match the
// given status for either phase. Used by "boid task gate list".
func ListGatesForStatus(meta *ProjectMeta, task *Task, status TaskStatus) []Gate {
	behavior, ok := lookupBehavior(meta, task)
	if !ok {
		return nil
	}
	eval := &Evaluator{}
	probe := *task
	probe.Status = status

	var result []Gate
	for _, phase := range []GatePhase{GatePhaseEntry, GatePhaseExit} {
		matched := eval.EvaluateGates(&probe, behavior.Gates, phase)
		result = append(result, matched...)
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
