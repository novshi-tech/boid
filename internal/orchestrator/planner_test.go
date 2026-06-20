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

// Hooks include boid and git as builtin policies; host commands are propagated
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

	if len(hookReq.BuiltinPolicies) != 3 {
		t.Fatalf("hook builtin policies = %#v, want 3 (git, boid, fetch)", hookReq.BuiltinPolicies)
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

// BOID_PARENT_BRANCH is set to the parent's HEAD branch for child tasks (P3).
// Root tasks must NOT have BOID_PARENT_BRANCH.
func TestDispatchPlanner_PropagatesParentBranchEnv(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".boid", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(projectDir, ".boid/hooks", "hook-1.sh")

	cases := []struct {
		name           string
		parent         *Task
		task           *Task
		wantParentBranch string
		wantAbsent     bool
	}{
		{
			name: "child of root task — parent is root so BOID_PARENT_BRANCH = parent.BaseBranch",
			parent: &Task{
				ID:         "root00001234567",
				ProjectID:  "proj-1",
				BaseBranch: "main",
				// ParentID == "" → root
			},
			task: &Task{
				ID:         "child0001234567",
				ProjectID:  "proj-1",
				Behavior:   "executor",
				Status:     TaskStatusExecuting,
				BaseBranch: "main",
				ParentID:   "root00001234567",
			},
			wantParentBranch: "main",
		},
		{
			name: "child of child — parent is itself a child so BOID_PARENT_BRANCH = boid/<parent_id8>",
			parent: &Task{
				ID:        "parentab00000000",
				ProjectID: "proj-1",
				ParentID:  "grandparent-root",
			},
			task: &Task{
				ID:         "childabc00000000",
				ProjectID:  "proj-1",
				Behavior:   "executor",
				Status:     TaskStatusExecuting,
				BaseBranch: "main",
				ParentID:   "parentab00000000",
			},
			wantParentBranch: "boid/parentab", // parentab00000000[:8] = "parentab"
		},
		{
			name: "root task — BOID_PARENT_BRANCH must be absent",
			parent: nil,
			task: &Task{
				ID:         "roottask00000000",
				ProjectID:  "proj-1",
				Behavior:   "executor",
				Status:     TaskStatusExecuting,
				BaseBranch: "main",
				// ParentID == ""
			},
			wantAbsent: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			planner := newPlannerForTestWithParent(
				&Project{ID: "proj-1", WorkDir: projectDir},
				TaskBehavior{},
				tc.task,
				tc.parent,
			)
			req, _, err := planner.PlanHook(&HookFireEvent{
				EventID:   "event-1",
				TaskID:    tc.task.ID,
				ProjectID: "proj-1",
				Hook:      Hook{ID: "hook-1", ScriptPath: scriptPath},
			})
			if err != nil {
				t.Fatalf("PlanHook: %v", err)
			}
			if tc.wantAbsent {
				if _, ok := req.Env["BOID_PARENT_BRANCH"]; ok {
					t.Errorf("root task must not have BOID_PARENT_BRANCH, got %q", req.Env["BOID_PARENT_BRANCH"])
				}
			} else {
				if got := req.Env["BOID_PARENT_BRANCH"]; got != tc.wantParentBranch {
					t.Errorf("BOID_PARENT_BRANCH = %q, want %q", got, tc.wantParentBranch)
				}
			}
		})
	}
}

// Existing BOID_BASE_BRANCH must still be propagated unchanged (P3 retention test).
func TestDispatchPlanner_BaseBranchEnvRetained_WithParent(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".boid", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	parentTask := &Task{
		ID:         "parent1234567890",
		ProjectID:  "proj-1",
		BaseBranch: "main",
	}
	childTask := &Task{
		ID:         "child12345678901",
		ProjectID:  "proj-1",
		Behavior:   "executor",
		Status:     TaskStatusExecuting,
		BaseBranch: "feature/BGO-999",
		ParentID:   "parent1234567890",
	}
	planner := newPlannerForTestWithParent(
		&Project{ID: "proj-1", WorkDir: projectDir},
		TaskBehavior{},
		childTask,
		parentTask,
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

type stubMultiTaskLookup struct {
	tasks map[string]*Task
}

func (s stubMultiTaskLookup) GetTask(id string) (*Task, error) {
	t, ok := s.tasks[id]
	if !ok {
		return nil, nil
	}
	return t, nil
}

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

func newPlannerForTestWithParent(proj *Project, behavior TaskBehavior, task *Task, parent *Task) *DispatchPlanner {
	meta := &ProjectMeta{
		ID:            proj.ID,
		TaskBehaviors: map[string]TaskBehavior{task.Behavior: behavior},
	}
	tasks := map[string]*Task{task.ID: task}
	if parent != nil {
		tasks[parent.ID] = parent
	}
	return &DispatchPlanner{
		Meta:     stubMetaCache{meta: meta},
		Projects: stubProjectCatalog{projects: []*Project{proj}},
		Tasks:    stubMultiTaskLookup{tasks: tasks},
		Adapter:  stubHarnessAdapter{},
	}
}
