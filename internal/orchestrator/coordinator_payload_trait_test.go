package orchestrator_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

// TestCoordinator_HookAndExitGateBothProduceArtifact pins down the fix for the
// dispatch_error trap where a hook (claude-code/run-agent) and an exit gate
// (github-auto-merge/auto-merge) both wrote disjoint sub-keys under `artifact`
// and triggered a spurious "exclusive trait collision". The two should run in
// separate collision domains, and their sub-keys must deep-merge so neither
// writer's contribution is lost.
func TestCoordinator_HookAndExitGateBothProduceArtifact(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("run-agent",
		`{"payload_patch":{"artifact":{"claude_code":{"sessions":[{"id":"sess-1"}]}}}}`, 0)
	mock.setGateCompletion("auto-merge",
		`{"payload_patch":{"artifact":{"auto-merge":{"merged":true}}}}`, 0)

	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
	}
	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
		Payload:   json.RawMessage(`{}`),
	}
	meta := metaWithBehavior(
		[]projectspec.Hook{{
			ID: "run-agent",
			Traits: projectspec.HandlerTraits{
				Produces: []projectspec.TraitType{projectspec.TraitArtifact},
			},
		}},
		[]projectspec.Gate{{
			ID:    "auto-merge",
			Phase: projectspec.GatePhaseExit,
			Traits: projectspec.HandlerTraits{
				Produces: []projectspec.TraitType{projectspec.TraitArtifact},
			},
		}},
	)
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("hook+exit-gate writing disjoint artifact sub-keys must not collide: %v", err)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(result.FinalPayload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	artifactRaw, ok := payload["artifact"]
	if !ok {
		t.Fatal("expected artifact in final payload")
	}
	var artifact map[string]json.RawMessage
	if err := json.Unmarshal(artifactRaw, &artifact); err != nil {
		t.Fatalf("unmarshal artifact: %v", err)
	}
	if _, ok := artifact["claude_code"]; !ok {
		t.Errorf("hook contribution artifact.claude_code missing; got %v", artifact)
	}
	if _, ok := artifact["auto-merge"]; !ok {
		t.Errorf("exit gate contribution artifact.auto-merge missing; got %v", artifact)
	}
}

func TestCoordinator_DispatchAndAdvance_ExclusiveTraitCollision(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("hook-a", `{"payload_patch":{"prompt":"from-a"}}`, 0)
	mock.setHookCompletion("hook-b", `{"payload_patch":{"prompt":"from-b"}}`, 0)

	eval := &orchestrator.Evaluator{}
	coord := &orchestrator.Coordinator{
		Evaluator:    eval,
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
		Payload:   json.RawMessage(`{}`),
	}
	meta := metaWithBehavior([]projectspec.Hook{
		{ID: "hook-a"},
		{ID: "hook-b"},
	}, nil)
	sm := simpleStateMachine()

	_, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err == nil {
		t.Fatal("expected error for exclusive trait collision")
	}
}

func TestCoordinator_DispatchAndAdvance_SharedTraitNoCollision(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("hook-a", `{"payload_patch":{"verification":{"findings":[{"message":"ok","status":"resolved"}]}}}`, 0)
	mock.setHookCompletion("hook-b", `{"payload_patch":{"verification":{"findings":[{"message":"bug","status":"open"}]}}}`, 0)

	eval := &orchestrator.Evaluator{}
	coord := &orchestrator.Coordinator{
		Evaluator:    eval,
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
		Payload:   json.RawMessage(`{}`),
	}
	meta := metaWithBehavior([]projectspec.Hook{
		{ID: "hook-a"},
		{ID: "hook-b"},
	}, nil)
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("shared trait should not collide: %v", err)
	}

	var payload map[string]json.RawMessage
	json.Unmarshal(result.FinalPayload, &payload)
	var verification map[string]json.RawMessage
	json.Unmarshal(payload["verification"], &verification)
	if _, ok := verification["hook-a"]; !ok {
		t.Error("expected hook-a sub-key in verification")
	}
	if _, ok := verification["hook-b"]; !ok {
		t.Error("expected hook-b sub-key in verification")
	}

	// source_state is no longer injected by the coordinator (verification.findings廃止)
	_ = verification
}

// TestCoordinator_DispatchAndAdvance_DropsUnknownTraitAndMergesArtifact
// reproduces at coordinator level the silent data-loss bug (task 089373ac /
// job 61b4d77f) where an agent wrote an undefined top-level key (e.g. `status`)
// alongside a valid `artifact` trait. Previously MergePayloadPatch rejected the
// whole patch for "trait status not in produces", dropping artifact too and
// stalling downstream gates. After the fix, unknown keys are skipped and
// valid traits still merge into task.payload.
func TestCoordinator_DispatchAndAdvance_DropsUnknownTraitAndMergesArtifact(t *testing.T) {
	mock := newMockExecutorWaiter()
	// Agent emits both an undefined trait ("status") and a valid one ("artifact").
	mock.setHookCompletion("main-hook",
		`{"payload_patch":{"status":"done","artifact":{"commit":"abc1234"}}}`, 0)

	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
		Payload:   json.RawMessage(`{}`),
	}
	meta := metaWithBehavior([]projectspec.Hook{
		{
			ID: "main-hook",
			Traits: projectspec.HandlerTraits{
				Produces: []projectspec.TraitType{projectspec.TraitArtifact},
			},
		},
	}, nil)
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("dispatch should not fail on unknown trait: %v", err)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(result.FinalPayload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if _, ok := payload["status"]; ok {
		t.Error("unknown trait \"status\" should have been dropped from payload")
	}
	if got, ok := payload["artifact"]; !ok {
		t.Error("valid trait \"artifact\" should have been merged into payload")
	} else if string(got) != `{"commit":"abc1234"}` {
		t.Errorf("artifact value mismatch: %s", got)
	}
}

// TestCoordinator_GateFiresOnlyWhenConsumedTraitPresent verifies the core
// "seam" between hook merge and gate firing: an exit gate that declares
// `consumes: [artifact]` must fire iff a prior hook successfully merged
// an `artifact` trait into the payload. The silent data-loss bug surfaced
// here — when merge dropped artifact, this gate silently did not fire,
// which was indistinguishable from the legitimate "no artifact produced"
// case. Covering both branches pins down the expected behavior.
func TestCoordinator_GateFiresOnlyWhenConsumedTraitPresent(t *testing.T) {
	artifactConsumer := projectspec.Gate{
		ID:    "post-artifact-gate",
		Phase: projectspec.GatePhaseExit,
		Traits: projectspec.HandlerTraits{
			Consumes: []projectspec.TraitType{projectspec.TraitArtifact},
		},
	}

	t.Run("hook produces artifact -> gate fires", func(t *testing.T) {
		mock := newMockExecutorWaiter()
		mock.setHookCompletion("producer",
			`{"payload_patch":{"artifact":{"commit":"abc1234"}}}`, 0)

		coord := &orchestrator.Coordinator{
			Evaluator:    &orchestrator.Evaluator{},
			HookExecutor: mock,
			GateExecutor: mock,
			Waiter:       mock,
			MaxDepth:     5,
		}
		task := &orchestrator.Task{
			ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
			ProjectID: "proj-1",
			Status:    orchestrator.TaskStatusExecuting,
			Behavior:  "dev",
			Payload:   json.RawMessage(`{}`),
		}
		meta := metaWithBehavior(
			[]projectspec.Hook{{
				ID: "producer",
				Traits: projectspec.HandlerTraits{
					Produces: []projectspec.TraitType{projectspec.TraitArtifact},
				},
			}},
			[]projectspec.Gate{artifactConsumer},
		)
		sm := simpleStateMachine()

		if _, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm); err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if len(mock.gateCalls) != 1 {
			t.Fatalf("gate fire count = %d, want 1 (artifact present -> gate should fire)", len(mock.gateCalls))
		}
		if mock.gateCalls[0].Gate.ID != "post-artifact-gate" {
			t.Errorf("unexpected gate fired: %q", mock.gateCalls[0].Gate.ID)
		}
	})

	t.Run("hook produces nothing -> gate does not fire", func(t *testing.T) {
		mock := newMockExecutorWaiter()
		mock.setHookCompletion("noop-producer", `{"payload_patch":{}}`, 0)

		coord := &orchestrator.Coordinator{
			Evaluator:    &orchestrator.Evaluator{},
			HookExecutor: mock,
			GateExecutor: mock,
			Waiter:       mock,
			MaxDepth:     5,
		}
		task := &orchestrator.Task{
			ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
			ProjectID: "proj-1",
			Status:    orchestrator.TaskStatusExecuting,
			Behavior:  "dev",
			Payload:   json.RawMessage(`{}`),
		}
		meta := metaWithBehavior(
			[]projectspec.Hook{{
				ID: "noop-producer",
				Traits: projectspec.HandlerTraits{
					Produces: []projectspec.TraitType{projectspec.TraitArtifact},
				},
			}},
			[]projectspec.Gate{artifactConsumer},
		)
		sm := simpleStateMachine()

		if _, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm); err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if len(mock.gateCalls) != 0 {
			t.Fatalf("gate fire count = %d, want 0 (no artifact -> gate must not fire)", len(mock.gateCalls))
		}
	})
}

// TestCoordinator_GateConsumesSharedVerificationFromMultipleHooks covers the
// shared-trait merge path (verification) at the seam between hook output and
// gate firing. Two hooks each produce verification findings under their own
// handlerID sub-key; MergePayloadPatch combines them under `verification`,
// and an exit gate that consumes verification fires once with both entries
// visible in the passed payload. This guards the merge + fire interaction
// for shared traits, complementing the exclusive (artifact) case.
func TestCoordinator_GateConsumesSharedVerificationFromMultipleHooks(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("hook-a",
		`{"payload_patch":{"verification":{"findings":[{"message":"ok","status":"resolved"}]}}}`, 0)
	mock.setHookCompletion("hook-b",
		`{"payload_patch":{"verification":{"findings":[{"message":"fix","status":"resolved"}]}}}`, 0)

	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
		Payload:   json.RawMessage(`{}`),
	}
	meta := metaWithBehavior(
		[]projectspec.Hook{
			{ID: "hook-a", Traits: projectspec.HandlerTraits{
				Produces: []projectspec.TraitType{projectspec.TraitVerification},
			}},
			{ID: "hook-b", Traits: projectspec.HandlerTraits{
				Produces: []projectspec.TraitType{projectspec.TraitVerification},
			}},
		},
		[]projectspec.Gate{{
			ID:    "verification-consumer",
			Phase: projectspec.GatePhaseExit,
			Traits: projectspec.HandlerTraits{
				Consumes: []projectspec.TraitType{projectspec.TraitVerification},
			},
		}},
	)
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if len(mock.gateCalls) != 1 {
		t.Fatalf("gate fire count = %d, want 1", len(mock.gateCalls))
	}
	gateEvt := mock.gateCalls[0]

	var gatePayload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(gateEvt.TaskPayloadJSON), &gatePayload); err != nil {
		t.Fatalf("unmarshal gate input payload: %v", err)
	}
	verRaw, ok := gatePayload["verification"]
	if !ok {
		t.Fatal("gate should see verification trait in payload")
	}
	var verification map[string]json.RawMessage
	if err := json.Unmarshal(verRaw, &verification); err != nil {
		t.Fatalf("unmarshal verification: %v", err)
	}
	for _, key := range []string{"hook-a", "hook-b"} {
		if _, ok := verification[key]; !ok {
			t.Errorf("gate should see merged verification entry for %q; got keys %v", key, verification)
		}
	}

	// Final payload should also reflect the shared merge.
	var finalPayload map[string]json.RawMessage
	if err := json.Unmarshal(result.FinalPayload, &finalPayload); err != nil {
		t.Fatalf("unmarshal final payload: %v", err)
	}
	if _, ok := finalPayload["verification"]; !ok {
		t.Error("final payload should retain merged verification trait")
	}
}
