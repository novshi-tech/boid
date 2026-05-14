package orchestrator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

// Hooks include boid and git as builtin policies; host commands are propagated
// from behavior (nil when behavior has none). Gates run directly on the host
// and have no builtin policies or host commands (no broker is involved).
func TestDispatchPlannerInjectsDefaultBuiltinsForHookAndGate(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".boid", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectDir, ".boid", "gates"), 0o755); err != nil {
		t.Fatal(err)
	}
	planner := newPlannerForTest(&Project{ID: "proj-1", WorkDir: projectDir}, TaskBehavior{
		Name: "dev",
	}, &Task{ID: "task-1", ProjectID: "proj-1", Behavior: "dev", Status: TaskStatusExecuting})

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
		t.Fatalf("hook builtin policies = %#v, want 2 (git, boid)", hookReq.BuiltinPolicies)
	}
	if hookReq.HostCommands != nil {
		t.Fatalf("hook host commands = %#v, want nil", hookReq.HostCommands)
	}

	gateReq, gateCleanup, err := planner.PlanGate(&GateFireEvent{
		EventID:   "event-2",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Gate: Gate{
			ID:         "gate-1",
			ScriptPath: filepath.Join(projectDir, ".boid/gates", "gate-1.sh"),
		},
	})
	if err != nil {
		t.Fatalf("PlanGate: %v", err)
	}
	if gateCleanup != nil {
		defer gateCleanup()
	}

	// Gates run directly on the host; no broker policies are needed.
	if len(gateReq.BuiltinPolicies) != 0 {
		t.Fatalf("gate builtin policies = %#v, want nil (gates use host-direct, no broker)", gateReq.BuiltinPolicies)
	}
	if len(gateReq.HostCommands) != 0 {
		t.Fatalf("gate host commands = %#v, want nil (gates use host-direct, no broker)", gateReq.HostCommands)
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
		Name:     "dev",
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

// Hook jobs always request an interactive PTY: agent runners (claude code etc.)
// are launched via real PTY sessions and rely on daemon-side SIGTERM (on
// `boid task notify --ask` or `boid job done`) to terminate. Gate jobs run on
// the host directly and have no use for the flag.
func TestPlanHook_AlwaysInteractive(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".boid", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	planner := newPlannerForTest(&Project{ID: "proj-1", WorkDir: projectDir}, TaskBehavior{
		Name: "executor",
	}, &Task{ID: "task-1", ProjectID: "proj-1", Behavior: "executor", Status: TaskStatusExecuting})

	hookReq, cleanup, err := planner.PlanHook(&HookFireEvent{
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
	if !hookReq.Interactive {
		t.Errorf("PlanHook spec.Interactive = false, want true (hook jobs must allocate a PTY)")
	}

	// Sanity: gate jobs do NOT inherit Interactive — they run on host and have
	// no PTY to drive.
	if err := os.MkdirAll(filepath.Join(projectDir, ".boid", "gates"), 0o755); err != nil {
		t.Fatal(err)
	}
	gateReq, gateCleanup, err := planner.PlanGate(&GateFireEvent{
		EventID:   "event-2",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Gate: Gate{
			ID:         "gate-1",
			ScriptPath: filepath.Join(projectDir, ".boid/gates", "gate-1.sh"),
		},
	})
	if err != nil {
		t.Fatalf("PlanGate: %v", err)
	}
	if gateCleanup != nil {
		defer gateCleanup()
	}
	if gateReq.Interactive {
		t.Errorf("PlanGate spec.Interactive = true, want false (gates run host-direct without a PTY)")
	}
}

// PlanGate uses Gate.ScriptPath directly as Argv[0]. No staging directory is
// created. Gates run on the host directly, so Visibility (including KitRoots)
// is not populated.
func TestPlanGate_UsesScriptPathDirectly(t *testing.T) {
	projectDir := t.TempDir()
	kitRoot := t.TempDir()
	kitGatesDir := filepath.Join(kitRoot, "gates")
	if err := os.MkdirAll(kitGatesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(kitGatesDir, "gate-1.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatalf("write kit gate: %v", err)
	}

	planner := newPlannerForTest(&Project{ID: "proj-1", WorkDir: projectDir}, TaskBehavior{
		Name:     "dev",
		KitRoots: []string{kitRoot},
	}, &Task{ID: "task-1", ProjectID: "proj-1", Behavior: "dev", Status: TaskStatusExecuting})

	req, cleanup, err := planner.PlanGate(&GateFireEvent{
		EventID:   "event-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Gate:      Gate{ID: "gate-1", ScriptPath: scriptPath},
	})
	if err != nil {
		t.Fatalf("PlanGate: %v", err)
	}
	if cleanup != nil {
		t.Error("PlanGate should return nil cleanup (no staging dir)")
	}
	if len(req.Argv) == 0 || req.Argv[0] != scriptPath {
		t.Errorf("Argv[0] = %q, want %q", req.Argv[0], scriptPath)
	}
	// Gates run on the host directly; Visibility (including KitRoots) is not
	// populated — the host gate wrapper handles env/cwd directly.
	if len(req.Visibility.KitRoots) != 0 {
		t.Errorf("KitRoots = %v, want empty (gates use host-direct, no sandbox visibility)", req.Visibility.KitRoots)
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
			{Type: InstructionTypeExecution, Agent: "claude-code", Message: "do X"},
		},
	}
	planner := newPlannerForTest(&Project{ID: "proj-1", WorkDir: projectDir}, TaskBehavior{Name: "dev"}, task)

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
	planner := newPlannerForTest(&Project{ID: "proj-1", WorkDir: projectDir}, TaskBehavior{Name: "dev"}, task)

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
	planner := newPlannerForTest(&Project{ID: "proj-1", WorkDir: projectDir}, TaskBehavior{Name: "dev"}, task)

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

// PlanGate feeds the full task (including payload) through PrimaryInput and
// leaves Task nil (no task.yaml context file).
func TestPlanGate_PrimaryInputIsFullTaskJSON(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".boid", "gates"), 0o755); err != nil {
		t.Fatal(err)
	}
	task := &Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Status:    TaskStatusExecuting,
		Behavior:  "dev",
		Payload:   json.RawMessage(`{"verification":{"findings":[]}}`),
	}
	planner := newPlannerForTest(&Project{ID: "proj-1", WorkDir: projectDir}, TaskBehavior{Name: "dev"}, task)

	req, gateCleanup, err := planner.PlanGate(&GateFireEvent{
		EventID:   "event-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Gate: Gate{
			ID:         "gate-1",
			ScriptPath: filepath.Join(projectDir, ".boid/gates", "gate-1.sh"),
		},
	})
	if err != nil {
		t.Fatalf("PlanGate: %v", err)
	}
	if gateCleanup != nil {
		defer gateCleanup()
	}

	if req.Task != nil {
		t.Errorf("gate should not emit task.yaml: %#v", req.Task)
	}
	if !strings.Contains(string(req.PrimaryInput), "\"verification\"") {
		t.Errorf("gate PrimaryInput missing payload data: %s", req.PrimaryInput)
	}
	if req.Visibility.ProjectDir != "" {
		t.Errorf("gate Visibility.ProjectDir = %q, want empty", req.Visibility.ProjectDir)
	}
}

// Hook / gate jobs must receive task.BaseBranch via BOID_BASE_BRANCH so kits
// like git-auto-merge can identify the merge target without inspecting the
// worktree (gate sandboxes hide the project filesystem).
func TestDispatchPlanner_PropagatesBaseBranchEnv(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".boid", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectDir, ".boid", "gates"), 0o755); err != nil {
		t.Fatal(err)
	}

	behavior := TaskBehavior{
		Name: "dev",
		Env:  map[string]string{"KIT_VAR": "kit-value"},
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

	gateReq, _, err := planner.PlanGate(&GateFireEvent{
		EventID:   "event-2",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Gate: Gate{
			ID:         "gate-1",
			ScriptPath: filepath.Join(projectDir, ".boid/gates", "gate-1.sh"),
		},
	})
	if err != nil {
		t.Fatalf("PlanGate: %v", err)
	}
	if got := gateReq.Env["BOID_BASE_BRANCH"]; got != "feature/BGO-170" {
		t.Errorf("gate BOID_BASE_BRANCH = %q, want feature/BGO-170", got)
	}
	if got := gateReq.Env["KIT_VAR"]; got != "kit-value" {
		t.Errorf("gate KIT_VAR = %q, want kit-value", got)
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
		Name: "dev",
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
			planner := newPlannerForTest(&Project{ID: "proj-1", WorkDir: projectDir}, TaskBehavior{Name: "dev"}, task)
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

// CommandSpec.Readonly drives Visibility.Writable for exec jobs, mirroring the
// hook behavior. task.readonly is the sole arbiter in both cases.
func TestPlanExec_WritableControlledByCommandReadonly(t *testing.T) {
	cases := []struct {
		name     string
		readonly bool
		want     bool
	}{
		{"exec + readonly=false", false, true},
		{"exec + readonly=true", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			projectDir := t.TempDir()
			planner := newPlannerForTest(&Project{ID: "proj-1", WorkDir: projectDir}, TaskBehavior{Name: "dev"},
				&Task{ID: "task-1", ProjectID: "proj-1", Behavior: "dev", Status: TaskStatusExecuting})
			req, cleanup, err := planner.PlanExec(&ExecFireEvent{
				ProjectID: "proj-1",
				Command: CommandSpec{
					ResolvedCommand: []string{"bash"},
					Readonly:        tc.readonly,
				},
			})
			if err != nil {
				t.Fatalf("PlanExec: %v", err)
			}
			if cleanup != nil {
				defer cleanup()
			}
			if req.Visibility.Writable != tc.want {
				t.Errorf("Writable = %v, want %v (readonly=%v)", req.Visibility.Writable, tc.want, tc.readonly)
			}
			if req.Visibility.ProjectDir != projectDir {
				t.Errorf("ProjectDir = %q, want %q", req.Visibility.ProjectDir, projectDir)
			}
			if req.Kind != JobKindExec {
				t.Errorf("Kind = %q, want exec", req.Kind)
			}
		})
	}
}

// When a task has an awaiting trait with session_id / pending_answer /
// question_id, PlanHook must surface them as BOID_AGENT_SESSION_ID,
// BOID_USER_ANSWER, and BOID_QUESTION_ID so the kit can resume the session.
// For a plain initial-start (no awaiting payload) the vars must be absent.
func TestDispatchPlanner_PropagatesAwaitingEnv(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".boid", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(projectDir, ".boid/hooks", "hook-1.sh")

	awaitingPayload := json.RawMessage(`{"awaiting":{"session_id":"sess-xyz","question":"ok?","question_id":"q-1","pending_answer":"yes"}}`)
	task := &Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Behavior:  "dev",
		Status:    TaskStatusExecuting,
		Payload:   awaitingPayload,
	}
	planner := newPlannerForTest(&Project{ID: "proj-1", WorkDir: projectDir}, TaskBehavior{Name: "dev"}, task)

	req, _, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Hook:      Hook{ID: "hook-1", ScriptPath: scriptPath},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}

	if got := req.Env["BOID_AGENT_SESSION_ID"]; got != "sess-xyz" {
		t.Errorf("BOID_AGENT_SESSION_ID = %q, want sess-xyz", got)
	}
	if got := req.Env["BOID_USER_ANSWER"]; got != "yes" {
		t.Errorf("BOID_USER_ANSWER = %q, want yes", got)
	}
	if got := req.Env["BOID_QUESTION_ID"]; got != "q-1" {
		t.Errorf("BOID_QUESTION_ID = %q, want q-1", got)
	}

	// Initial-start task (no awaiting payload): env vars must be absent.
	task.Payload = nil
	plainPlanner := newPlannerForTest(&Project{ID: "proj-1", WorkDir: projectDir}, TaskBehavior{Name: "dev"}, task)
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

// --- test helpers ---

func newPlannerForTest(proj *Project, behavior TaskBehavior, task *Task) *DispatchPlanner {
	meta := &ProjectMeta{
		ID:            proj.ID,
		TaskBehaviors: map[string]TaskBehavior{behavior.Name: behavior},
	}
	return &DispatchPlanner{
		Meta:     stubMetaCache{meta: meta},
		Projects: stubProjectCatalog{projects: []*Project{proj}},
		Tasks:    stubTaskLookup{task: task},
	}
}
