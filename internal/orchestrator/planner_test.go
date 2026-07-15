package orchestrator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/adapters"
)

// stubHarnessAdapter is a test double matching the Phase 3-b two-method
// HarnessAdapter contract. planner currently only stores the adapter pointer
// (Phase 3-c is expected to expose harness capability hints again); the
// concrete behaviour of Run() is exercised end-to-end via runner-inner-child.
type stubHarnessAdapter struct{}

func (stubHarnessAdapter) Run(_ context.Context, _ adapters.RunContext) (adapters.Result, error) {
	return adapters.Result{}, nil
}
func (stubHarnessAdapter) Usage(_ context.Context, _ string) (adapters.Usage, error) {
	return adapters.Usage{}, nil
}
func (stubHarnessAdapter) Bindings(_ string) []adapters.BindMount { return nil }

type stubProjectCatalog struct {
	projects []*Project
}

func (s stubProjectCatalog) GetProject(id string) (*Project, error) {
	for _, project := range s.projects {
		if project.ID == id {
			return project, nil
		}
	}
	return nil, nil
}

type stubMetaCache struct {
	meta *ProjectMeta
}

func (s stubMetaCache) Get(id string) (*ProjectMeta, bool) {
	if s.meta == nil || s.meta.ID != id {
		return nil, false
	}
	return s.meta, true
}

type stubTaskLookup struct {
	task *Task
}

func (s stubTaskLookup) GetTask(id string) (*Task, error) {
	if s.task == nil || s.task.ID != id {
		return nil, nil
	}
	return s.task, nil
}

// Hooks include boid and fetch as builtin policies; host commands are propagated
// from behavior (nil when behavior has none).
func TestDispatchPlannerInjectsDefaultBuiltinsForHook(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".boid", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	planner := newPlannerForTest(&Project{ID: "proj-1", WorkDir: projectDir}, TaskBehavior{}, &Task{ID: "task-1", ProjectID: "proj-1", Behavior: "dev", Status: TaskStatusExecuting})

	hookReq, hookCleanup, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Hook: Hook{
			ID:         "hook-1",
			ScriptPath: filepath.Join(projectDir, ".boid/hooks", "hook-1.sh"),
		},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}
	if hookCleanup != nil {
		defer hookCleanup()
	}

	if len(hookReq.BuiltinPolicies) != 2 {
		t.Fatalf("hook builtin policies = %#v, want 2 (boid, fetch)", hookReq.BuiltinPolicies)
	}
	if _, ok := hookReq.BuiltinPolicies["fetch"]; !ok {
		t.Errorf("hook builtin policies missing \"fetch\": %#v", hookReq.BuiltinPolicies)
	}
	if hookReq.HostCommands != nil {
		t.Fatalf("hook host commands = %#v, want nil", hookReq.HostCommands)
	}
}

// PlanHook uses Hook.ScriptPath directly as Argv[0] and surfaces KitRoots
// from the behavior in Visibility.KitRoots. No staging directory is created.
func TestPlanHook_UsesScriptPathDirectlyAndSetsKitRoots(t *testing.T) {
	projectDir := t.TempDir()
	kitRoot := t.TempDir()
	kitHooksDir := filepath.Join(kitRoot, "hooks")
	if err := os.MkdirAll(kitHooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(kitHooksDir, "run-agent.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatalf("write kit hook: %v", err)
	}

	planner := newPlannerForTest(&Project{ID: "proj-1", WorkDir: projectDir}, TaskBehavior{
		KitRoots: []string{kitRoot},
	}, &Task{ID: "task-1", ProjectID: "proj-1", Behavior: "dev", Status: TaskStatusExecuting})

	req, cleanup, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Hook:      Hook{ID: "run-agent", ScriptPath: scriptPath},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}
	if cleanup != nil {
		t.Error("PlanHook should return nil cleanup (no staging dir)")
	}
	if len(req.Argv) == 0 || req.Argv[0] != scriptPath {
		t.Errorf("Argv[0] = %q, want %q", req.Argv[0], scriptPath)
	}
	if len(req.Visibility.KitRoots) != 1 || req.Visibility.KitRoots[0] != kitRoot {
		t.Errorf("KitRoots = %v, want [%s]", req.Visibility.KitRoots, kitRoot)
	}
}

// TestPlanHook_SetsVisibilityProjectNameFromMeta is the workspace 親化リファ
// クタリング (nose 2026-07-13 decision) regression guard for the self-project
// half of the sandbox-internal /workspace/<name> clone dir: PlanHook must
// thread project.yaml's `meta.name` through to Visibility.ProjectName so
// dispatcher can derive the name-scoped clone dir directly from JobSpec,
// without a second (and — see dispatcher.cloneDirNameForVisibility's doc
// comment — unreliable) Projects lookup.
func TestPlanHook_SetsVisibilityProjectNameFromMeta(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".boid", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	proj := &Project{ID: "proj-1", WorkDir: projectDir}
	task := &Task{ID: "task-1", ProjectID: "proj-1", Behavior: "dev", Status: TaskStatusExecuting}
	meta := &ProjectMeta{
		ID:            proj.ID,
		Name:          "bm-next",
		TaskBehaviors: map[string]TaskBehavior{task.Behavior: {}},
	}
	planner := &DispatchPlanner{
		Meta:     stubMetaCache{meta: meta},
		Projects: stubProjectCatalog{projects: []*Project{proj}},
		Tasks:    stubTaskLookup{task: task},
		Adapter:  stubHarnessAdapter{},
	}

	req, cleanup, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Hook:      Hook{ID: "hook-1", ScriptPath: filepath.Join(projectDir, ".boid/hooks", "hook-1.sh")},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	if req.Visibility.ProjectName != "bm-next" {
		t.Errorf("Visibility.ProjectName = %q, want %q", req.Visibility.ProjectName, "bm-next")
	}
}

// PlanHook must carry behavior.AdditionalBindings through to
// Visibility.AdditionalBindings. This is the task-hook counterpart of the
// session path's binding passthrough (dispatcher.TestBindingPassthrough_*):
// a workspace-kit binding merged into the behavior would silently vanish from
// every task hook if the planner dropped it here, which is the exact shape of
// the 2026-06-29 regression on the hook side. KitRoots is covered above; this
// pins the sibling field.
func TestPlanHook_CarriesAdditionalBindings(t *testing.T) {
	projectDir := t.TempDir()
	kitHooksDir := filepath.Join(projectDir, "hooks")
	if err := os.MkdirAll(kitHooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(kitHooksDir, "run.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	binding := BindMount{Source: "/opt/volta", Target: "/opt/volta", Mode: "rw"}
	planner := newPlannerForTest(&Project{ID: "proj-1", WorkDir: projectDir}, TaskBehavior{
		AdditionalBindings: []BindMount{binding},
	}, &Task{ID: "task-1", ProjectID: "proj-1", Behavior: "dev", Status: TaskStatusExecuting})

	req, _, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Hook:      Hook{ID: "run", ScriptPath: scriptPath},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}
	if len(req.Visibility.AdditionalBindings) != 1 || req.Visibility.AdditionalBindings[0] != binding {
		t.Errorf("Visibility.AdditionalBindings = %+v, want [%+v]", req.Visibility.AdditionalBindings, binding)
	}
}

// Agent-bearing hooks (HarnessType != "") request an interactive PTY:
// agent runners (claude code etc.) are launched via real PTY sessions and
// rely on daemon-side SIGUSR1 (on `boid task notify --ask` or `boid job
// done`) to terminate.
//
// Phase 3-d table-extended: hook.Agent → HarnessType mapping. Known agents
// are routed to their adapter; an unknown agent (including hooks without
// `agent:` declared) falls through to the shell adapter so every job flows
// through the adapter pipeline. HarnessType is invariant non-empty from
// Phase 3-d onward.
func TestPlanHook_AgentHookInteractive(t *testing.T) {
	cases := []struct {
		agent       string
		wantHarness string
	}{
		{"claude-code", "claude"},
		{"codex", "codex"},
		{"opencode", "opencode"},
		// Unknown agent: shell adapter takes over and execs the hook
		// script's argv directly.
		{"some-future-agent", "shell"},
	}
	for _, tc := range cases {
		t.Run(tc.agent, func(t *testing.T) {
			projectDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(projectDir, ".boid", "hooks"), 0o755); err != nil {
				t.Fatal(err)
			}
			task := &Task{
				ID:        "task-1",
				ProjectID: "proj-1",
				Behavior:  "executor",
				Status:    TaskStatusExecuting,
				Instructions: Instructions{{
					Agent: tc.agent,
				}},
			}
			planner := newPlannerForTest(&Project{ID: "proj-1", WorkDir: projectDir}, TaskBehavior{}, task)

			hookReq, cleanup, err := planner.PlanHook(&HookFireEvent{
				EventID:   "event-1",
				TaskID:    "task-1",
				ProjectID: "proj-1",
				Hook: Hook{
					ID:         "hook-1",
					ScriptPath: filepath.Join(projectDir, ".boid/hooks", "hook-1.sh"),
					Agent:      tc.agent,
				},
			})
			if err != nil {
				t.Fatalf("PlanHook: %v", err)
			}
			if cleanup != nil {
				defer cleanup()
			}
			if hookReq.HarnessType != tc.wantHarness {
				t.Errorf("PlanHook agent=%q: HarnessType = %q, want %q", tc.agent, hookReq.HarnessType, tc.wantHarness)
			}
			if !hookReq.Interactive {
				t.Errorf("PlanHook agent=%q: Interactive = false, want true (all agent-bearing hooks allocate a PTY)", tc.agent)
			}
		})
	}
}

// Phase 3-e fallback: PlanHook must accept an agent-kind Hook with empty
// ScriptPath — that's the shape the Evaluator synthesizes when the behavior
// declares no hook of its own and the active instruction targets a known
// harness. The resulting JobSpec carries an empty Argv (the HarnessAdapter
// builds its own argv from CLI conventions) but a populated HarnessType so
// the runner-inner-child hands the job off to the right adapter.
func TestPlanHook_AcceptsScriptlessAgentHook(t *testing.T) {
	projectDir := t.TempDir()
	task := &Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Behavior:  "executor",
		Status:    TaskStatusExecuting,
		Instructions: Instructions{{
			Agent:   "claude-code",
			Message: "do stuff",
		}},
	}
	planner := newPlannerForTest(&Project{ID: "proj-1", WorkDir: projectDir}, TaskBehavior{}, task)

	req, cleanup, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Hook: Hook{
			ID:    "agent:claude-code",
			Kind:  HandlerKindAgent,
			Agent: "claude-code",
			// ScriptPath intentionally empty.
		},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	if req.HarnessType != "claude" {
		t.Errorf("HarnessType = %q, want claude", req.HarnessType)
	}
	if len(req.Argv) != 0 {
		t.Errorf("Argv = %v, want empty (agent adapter ignores Argv)", req.Argv)
	}
	if !req.Interactive {
		t.Error("Interactive = false, want true (agent hooks always allocate a PTY)")
	}
	if req.Instruction == nil {
		t.Fatal("Instruction = nil, want routed instruction for claude-code agent")
	}
	if req.Instruction.Agent != "claude-code" {
		t.Errorf("Instruction.Agent = %q, want claude-code", req.Instruction.Agent)
	}
}

// Non-agent hooks (Kind == "") still require a Command or ScriptPath: the
// shell adapter has no way to build an Argv on their behalf.
func TestPlanHook_RejectsScriptlessNonAgentHook(t *testing.T) {
	projectDir := t.TempDir()
	planner := newPlannerForTest(
		&Project{ID: "proj-1", WorkDir: projectDir},
		TaskBehavior{},
		&Task{ID: "task-1", ProjectID: "proj-1", Behavior: "executor", Status: TaskStatusExecuting},
	)

	_, _, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Hook: Hook{
			ID:   "no-script",
			Kind: "", // non-agent
		},
	})
	if err == nil {
		t.Fatal("PlanHook accepted a non-agent hook with empty Command and ScriptPath; want error")
	}
	if !strings.Contains(err.Error(), "no command or script path resolved") {
		t.Errorf("error = %v, want one mentioning 'no command or script path resolved'", err)
	}
}

// script-hook-removal PR1 (docs/plans/script-hook-removal.md): Hook.Command
// is an inline shell command. PlanHook must wrap it as `sh -c <command>` so
// the shell adapter execs it directly, with no on-disk script involved.
func TestPlanHook_UsesCommandBuildsShArgv(t *testing.T) {
	projectDir := t.TempDir()
	planner := newPlannerForTest(
		&Project{ID: "proj-1", WorkDir: projectDir},
		TaskBehavior{},
		&Task{ID: "task-1", ProjectID: "proj-1", Behavior: "executor", Status: TaskStatusExecuting},
	)

	req, cleanup, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Hook: Hook{
			ID:      "assert-clone-cwd",
			Command: "echo hi",
		},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	wantArgv := []string{"sh", "-c", "echo hi"}
	if len(req.Argv) != len(wantArgv) {
		t.Fatalf("Argv = %v, want %v", req.Argv, wantArgv)
	}
	for i := range wantArgv {
		if req.Argv[i] != wantArgv[i] {
			t.Errorf("Argv[%d] = %q, want %q", i, req.Argv[i], wantArgv[i])
		}
	}
}

// Command and ScriptPath are mutually exclusive: double-specifying both argv
// sources on the same hook is rejected rather than silently preferring one.
func TestPlanHook_RejectsCommandAndScriptPathTogether(t *testing.T) {
	projectDir := t.TempDir()
	scriptPath := filepath.Join(projectDir, ".boid", "hooks", "hook-1.sh")
	planner := newPlannerForTest(
		&Project{ID: "proj-1", WorkDir: projectDir},
		TaskBehavior{},
		&Task{ID: "task-1", ProjectID: "proj-1", Behavior: "executor", Status: TaskStatusExecuting},
	)

	_, _, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Hook: Hook{
			ID:         "double-specified",
			ScriptPath: scriptPath,
			Command:    "echo hi",
		},
	})
	if err == nil {
		t.Fatal("PlanHook accepted a hook with both ScriptPath and Command set; want error")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %v, want one mentioning 'mutually exclusive'", err)
	}
}

// Agent and Command are mutually exclusive: an agent-routed hook and an
// inline-command hook are different dispatch shapes (this is distinct from
// the Kind == HandlerKindAgent + Command case below — a hook can declare
// Agent without declaring kind: agent, e.g. a hook that has drifted out of
// sync with its kind).
func TestPlanHook_RejectsCommandAndAgentTogether(t *testing.T) {
	projectDir := t.TempDir()
	planner := newPlannerForTest(
		&Project{ID: "proj-1", WorkDir: projectDir},
		TaskBehavior{},
		&Task{ID: "task-1", ProjectID: "proj-1", Behavior: "executor", Status: TaskStatusExecuting},
	)

	_, _, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Hook: Hook{
			ID:      "agent-and-command",
			Agent:   "claude-code",
			Command: "echo hi",
		},
	})
	if err == nil {
		t.Fatal("PlanHook accepted a hook with both Agent and Command set; want error")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %v, want one mentioning 'mutually exclusive'", err)
	}
}

// Agent-kind hooks are dispatched to a HarnessAdapter, which builds its own
// argv — they must not also declare an inline Command.
func TestPlanHook_RejectsAgentKindHookWithCommand(t *testing.T) {
	projectDir := t.TempDir()
	planner := newPlannerForTest(
		&Project{ID: "proj-1", WorkDir: projectDir},
		TaskBehavior{},
		&Task{ID: "task-1", ProjectID: "proj-1", Behavior: "executor", Status: TaskStatusExecuting},
	)

	_, _, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Hook: Hook{
			ID:      "agent-kind-with-command",
			Kind:    HandlerKindAgent,
			Command: "echo hi",
			// Agent intentionally empty, to isolate this from the
			// Agent+Command exclusivity case above.
		},
	})
	if err == nil {
		t.Fatal("PlanHook accepted an agent-kind hook with Command set; want error")
	}
	if !strings.Contains(err.Error(), "agent-kind hooks do not take") {
		t.Errorf("error = %v, want one mentioning agent-kind hooks rejecting command", err)
	}
}

// TestPlanHook_DockerEnabled verifies that capabilities.docker in ProjectMeta
// flows through to Visibility.DockerEnabled on the resulting JobSpec.
func TestPlanHook_DockerEnabled_WhenCapabilitySet(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".boid", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	dockerCap := &DockerCapability{}
	planner := newPlannerWithCapabilities(
		&Project{ID: "proj-1", WorkDir: projectDir},
		TaskBehavior{},
		&Task{ID: "task-1", ProjectID: "proj-1", Behavior: "executor", Status: TaskStatusExecuting},
		Capabilities{Docker: dockerCap},
	)
	req, cleanup, err := planner.PlanHook(&HookFireEvent{
		EventID:   "ev-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Hook:      Hook{ID: "h-1", ScriptPath: filepath.Join(projectDir, ".boid/hooks/h-1.sh")},
	})
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}
	if !req.Visibility.DockerEnabled {
		t.Error("Visibility.DockerEnabled should be true when capabilities.docker is declared")
	}
}

func TestPlanHook_DockerEnabled_WhenCapabilityNotSet(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".boid", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	planner := newPlannerWithCapabilities(
		&Project{ID: "proj-1", WorkDir: projectDir},
		TaskBehavior{},
		&Task{ID: "task-1", ProjectID: "proj-1", Behavior: "executor", Status: TaskStatusExecuting},
		Capabilities{},
	)
	req, cleanup, err := planner.PlanHook(&HookFireEvent{
		EventID:   "ev-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Hook:      Hook{ID: "h-1", ScriptPath: filepath.Join(projectDir, ".boid/hooks/h-1.sh")},
	})
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}
	if req.Visibility.DockerEnabled {
		t.Error("Visibility.DockerEnabled should be false when capabilities.docker is not declared")
	}
}

// FilterInstructions picks a matching agent; planner surfaces exactly one
// RoutedInstruction on JobSpec.
func TestPlanHook_Instruction_MatchingAgent(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".boid", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	task := &Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Behavior:  "dev",
		Status:    TaskStatusExecuting,
		Instructions: Instructions{
			{Agent: "claude-code", Message: "do X"},
		},
	}
	planner := newPlannerForTest(&Project{ID: "proj-1", WorkDir: projectDir}, TaskBehavior{}, task)

	req, hookCleanup, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Hook: Hook{
			ID:         "hook-1",
			ScriptPath: filepath.Join(projectDir, ".boid/hooks", "hook-1.sh"),
			Agent:      "claude-code",
		},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}
	if hookCleanup != nil {
		defer hookCleanup()
	}

	if req.Instruction == nil {
		t.Fatal("expected Instruction, got nil")
	}
	if req.Instruction.Agent != "claude-code" {
		t.Errorf("Instruction.Agent = %q, want claude-code", req.Instruction.Agent)
	}
	if req.Instruction.Message != "do X" {
		t.Errorf("Instruction.Message = %q", req.Instruction.Message)
	}
}

// TaskSnapshot carries the same business metadata as the old buildTaskYAML
// output.
func TestPlanHook_TaskSnapshot(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".boid", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	task := &Task{
		ID:          "task-1",
		ProjectID:   "proj-1",
		Title:       "Hello",
		Status:      TaskStatusExecuting,
		Behavior:    "dev",
		Description: "short desc",
	}
	planner := newPlannerForTest(&Project{ID: "proj-1", WorkDir: projectDir}, TaskBehavior{}, task)

	req, hookCleanup, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Hook: Hook{
			ID:         "hook-1",
			ScriptPath: filepath.Join(projectDir, ".boid/hooks", "hook-1.sh"),
		},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}
	if hookCleanup != nil {
		defer hookCleanup()
	}

	if req.Task == nil {
		t.Fatal("expected Task snapshot")
	}
	if req.Task.ID != "task-1" || req.Task.Title != "Hello" {
		t.Errorf("TaskSnapshot = %#v", req.Task)
	}
}

// PrimaryInput gets filtered by the hook's declared trait consumption.
func TestPlanHook_PrimaryInput_FilteredByConsumes(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".boid", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	task := &Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Status:    TaskStatusExecuting,
		Behavior:  "dev",
		Payload: json.RawMessage(`{
			"artifact": {"file": "foo.go"},
			"verification": {"findings": []}
		}`),
	}
	planner := newPlannerForTest(&Project{ID: "proj-1", WorkDir: projectDir}, TaskBehavior{}, task)

	req, hookCleanup, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Hook: Hook{
			ID:         "hook-1",
			ScriptPath: filepath.Join(projectDir, ".boid/hooks", "hook-1.sh"),
			Traits:     HandlerTraits{Consumes: []TraitType{TraitArtifact}},
		},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}
	if hookCleanup != nil {
		defer hookCleanup()
	}

	if !strings.Contains(string(req.PrimaryInput), "\"artifact\"") {
		t.Errorf("PrimaryInput missing artifact: %s", req.PrimaryInput)
	}
	if strings.Contains(string(req.PrimaryInput), "\"verification\"") {
		t.Errorf("PrimaryInput should not carry verification: %s", req.PrimaryInput)
	}
}


// Hook jobs must receive task.BaseBranch via BOID_BASE_BRANCH so kits
// like git-auto-merge can identify the merge target without inspecting the
// worktree.
func TestDispatchPlanner_PropagatesBaseBranchEnv(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".boid", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}

	behavior := TaskBehavior{
		Env: map[string]string{"KIT_VAR": "kit-value"},
	}
	task := &Task{
		ID:         "task-1",
		ProjectID:  "proj-1",
		Behavior:   "dev",
		Status:     TaskStatusExecuting,
		BaseBranch: "feature/BGO-170",
	}
	planner := newPlannerForTest(&Project{ID: "proj-1", WorkDir: projectDir}, behavior, task)

	hookReq, _, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Hook: Hook{
			ID:         "hook-1",
			ScriptPath: filepath.Join(projectDir, ".boid/hooks", "hook-1.sh"),
		},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}
	if got := hookReq.Env["BOID_BASE_BRANCH"]; got != "feature/BGO-170" {
		t.Errorf("hook BOID_BASE_BRANCH = %q, want feature/BGO-170", got)
	}
	if got := hookReq.Env["KIT_VAR"]; got != "kit-value" {
		t.Errorf("hook KIT_VAR = %q, want kit-value (behavior env must be preserved)", got)
	}

	// Tasks without a base branch should not surface an empty BOID_BASE_BRANCH:
	// kit detection (`-n "${BOID_BASE_BRANCH:-}"`) treats empty and unset alike,
	// but leaving the var absent keeps env diagnostics clean.
	task.BaseBranch = ""
	emptyReq, _, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-3",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Hook: Hook{
			ID:         "hook-2",
			ScriptPath: filepath.Join(projectDir, ".boid/hooks", "hook-1.sh"),
		},
	})
	if err != nil {
		t.Fatalf("PlanHook (empty base): %v", err)
	}
	if _, ok := emptyReq.Env["BOID_BASE_BRANCH"]; ok {
		t.Errorf("hook env should not include BOID_BASE_BRANCH when task.BaseBranch is empty, got %#v", emptyReq.Env)
	}
}

// PlanHook propagates behavior.HostCommands into JobSpec.HostCommands.
func TestPlanHook_PropagatesHostCommands(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".boid", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	behavior := TaskBehavior{
		HostCommands: HostCommands{
			"gh": {Allow: []string{"pr", "issue"}},
			"jq": {},
		},
	}
	planner := newPlannerForTest(&Project{ID: "proj-1", WorkDir: projectDir}, behavior,
		&Task{ID: "task-1", ProjectID: "proj-1", Behavior: "dev", Status: TaskStatusExecuting})

	req, cleanup, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Hook: Hook{
			ID:         "hook-1",
			ScriptPath: filepath.Join(projectDir, ".boid/hooks", "hook-1.sh"),
		},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	if len(req.HostCommands) != 2 {
		t.Fatalf("HostCommands = %v, want 2 entries (gh, jq)", req.HostCommands)
	}
	if _, ok := req.HostCommands["gh"]; !ok {
		t.Error("HostCommands missing gh")
	}
	if _, ok := req.HostCommands["jq"]; !ok {
		t.Error("HostCommands missing jq")
	}
}

// task.readonly (and verifying status) drives Visibility.Writable for hook jobs.
// This is the canonical single-source-of-truth for the hook sandbox write permission.
func TestPlanHook_WritableControlledByTaskReadonly(t *testing.T) {
	cases := []struct {
		name     string
		readonly bool
		status   TaskStatus
		want     bool
	}{
		{"hook + readonly=false", false, TaskStatusExecuting, true},
		{"hook + readonly=true", true, TaskStatusExecuting, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			projectDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(projectDir, ".boid", "hooks"), 0o755); err != nil {
				t.Fatal(err)
			}
			task := &Task{
				ID:        "task-1",
				ProjectID: "proj-1",
				Behavior:  "dev",
				Readonly:  tc.readonly,
				Status:    tc.status,
			}
			planner := newPlannerForTest(&Project{ID: "proj-1", WorkDir: projectDir}, TaskBehavior{}, task)
			req, cleanup, err := planner.PlanHook(&HookFireEvent{
				EventID:   "event-1",
				TaskID:    "task-1",
				ProjectID: "proj-1",
				Hook: Hook{
					ID:         "hook-1",
					ScriptPath: filepath.Join(projectDir, ".boid/hooks", "hook-1.sh"),
				},
			})
			if err != nil {
				t.Fatalf("PlanHook: %v", err)
			}
			if cleanup != nil {
				defer cleanup()
			}
			if req.Visibility.Writable != tc.want {
				t.Errorf("Writable = %v, want %v (readonly=%v, status=%v)", req.Visibility.Writable, tc.want, tc.readonly, tc.status)
			}
		})
	}
}

// When a task has an awaiting trait with pending_answer / question_id,
// PlanHook surfaces them as BOID_USER_ANSWER / BOID_QUESTION_ID so the kit
// can read the prior reply on the next invocation. BOID_AGENT_SESSION_ID is
// no longer emitted — the session-id resume path was removed repo-wide, so
// even legacy persisted records carrying session_id leave the env vars
// alone. For a plain initial-start (no awaiting payload) every related var
// must be absent.
func TestDispatchPlanner_PropagatesAwaitingEnv(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".boid", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(projectDir, ".boid/hooks", "hook-1.sh")

	// Legacy records still hold a session_id; deserialisation must skip it
	// silently rather than fail, and PlanHook must not surface it as env.
	awaitingPayload := json.RawMessage(`{"awaiting":{"session_id":"sess-xyz","question":"ok?","question_id":"q-1","pending_answer":"yes"}}`)
	task := &Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Behavior:  "dev",
		Status:    TaskStatusExecuting,
		Payload:   awaitingPayload,
	}
	planner := newPlannerForTest(&Project{ID: "proj-1", WorkDir: projectDir}, TaskBehavior{}, task)

	req, _, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Hook:      Hook{ID: "hook-1", ScriptPath: scriptPath},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}

	if _, ok := req.Env["BOID_AGENT_SESSION_ID"]; ok {
		t.Errorf("BOID_AGENT_SESSION_ID must not be set anymore, got %q", req.Env["BOID_AGENT_SESSION_ID"])
	}
	if got := req.Env["BOID_USER_ANSWER"]; got != "yes" {
		t.Errorf("BOID_USER_ANSWER = %q, want yes", got)
	}
	if got := req.Env["BOID_QUESTION_ID"]; got != "q-1" {
		t.Errorf("BOID_QUESTION_ID = %q, want q-1", got)
	}

	// Initial-start task (no awaiting payload): env vars must be absent.
	task.Payload = nil
	plainPlanner := newPlannerForTest(&Project{ID: "proj-1", WorkDir: projectDir}, TaskBehavior{}, task)
	plainReq, _, err := plainPlanner.PlanHook(&HookFireEvent{
		EventID:   "event-2",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Hook:      Hook{ID: "hook-1", ScriptPath: scriptPath},
	})
	if err != nil {
		t.Fatalf("PlanHook (plain): %v", err)
	}
	for _, key := range []string{"BOID_AGENT_SESSION_ID", "BOID_USER_ANSWER", "BOID_QUESTION_ID"} {
		if _, ok := plainReq.Env[key]; ok {
			t.Errorf("plain start should not set %s, got %q", key, plainReq.Env[key])
		}
	}
}

// TestDispatchPlanner_NoParentBranchEnv pins that BOID_PARENT_BRANCH is never
// emitted (docs/plans/branch-policy-simplification.md Phase 1, nose
// 2026-07-15 decision: removed entirely rather than redefined, since a grep
// across production project.yaml / e2e scripts found zero real use). The
// parent task doesn't need to exist in the lookup — Phase 1 also removed
// planner.lookupParent, so a child task with a ParentID set never triggers
// a parent read anymore.
func TestDispatchPlanner_NoParentBranchEnv(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".boid", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(projectDir, ".boid/hooks", "hook-1.sh")

	task := &Task{
		ID:         "child0001234567",
		ProjectID:  "proj-1",
		Behavior:   "executor",
		Status:     TaskStatusExecuting,
		BaseBranch: "main",
		ParentID:   "root00001234567",
	}
	planner := newPlannerForTest(
		&Project{ID: "proj-1", WorkDir: projectDir},
		TaskBehavior{},
		task,
	)
	req, _, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    task.ID,
		ProjectID: "proj-1",
		Hook:      Hook{ID: "hook-1", ScriptPath: scriptPath},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}
	if _, ok := req.Env["BOID_PARENT_BRANCH"]; ok {
		t.Errorf("BOID_PARENT_BRANCH must not be set anymore, got %q", req.Env["BOID_PARENT_BRANCH"])
	}
}

// Existing BOID_BASE_BRANCH must still be propagated unchanged (P3 retention test).
func TestDispatchPlanner_BaseBranchEnvRetained_WithParent(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".boid", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	childTask := &Task{
		ID:         "child12345678901",
		ProjectID:  "proj-1",
		Behavior:   "executor",
		Status:     TaskStatusExecuting,
		BaseBranch: "feature/BGO-999",
		ParentID:   "parent1234567890",
	}
	planner := newPlannerForTest(
		&Project{ID: "proj-1", WorkDir: projectDir},
		TaskBehavior{},
		childTask,
	)
	req, _, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    "child12345678901",
		ProjectID: "proj-1",
		Hook:      Hook{ID: "hook-1", ScriptPath: filepath.Join(projectDir, ".boid/hooks", "hook-1.sh")},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}
	if got := req.Env["BOID_BASE_BRANCH"]; got != "feature/BGO-999" {
		t.Errorf("BOID_BASE_BRANCH = %q, want feature/BGO-999", got)
	}
}

// --- test helpers ---

func newPlannerForTest(proj *Project, behavior TaskBehavior, task *Task) *DispatchPlanner {
	meta := &ProjectMeta{
		ID:            proj.ID,
		TaskBehaviors: map[string]TaskBehavior{task.Behavior: behavior},
	}
	return &DispatchPlanner{
		Meta:     stubMetaCache{meta: meta},
		Projects: stubProjectCatalog{projects: []*Project{proj}},
		Tasks:    stubTaskLookup{task: task},
		Adapter:  stubHarnessAdapter{},
	}
}

func newPlannerWithCapabilities(proj *Project, behavior TaskBehavior, task *Task, caps Capabilities) *DispatchPlanner {
	meta := &ProjectMeta{
		ID:            proj.ID,
		TaskBehaviors: map[string]TaskBehavior{task.Behavior: behavior},
		Capabilities:  caps,
	}
	return &DispatchPlanner{
		Meta:     stubMetaCache{meta: meta},
		Projects: stubProjectCatalog{projects: []*Project{proj}},
		Tasks:    stubTaskLookup{task: task},
		Adapter:  stubHarnessAdapter{},
	}
}

