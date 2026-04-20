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

// Hook / gate dispatches both include boid and git as builtin policies, and
// hooks never receive host commands.
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

	if len(gateReq.BuiltinPolicies) != 2 {
		t.Fatalf("gate builtin policies = %#v, want 2 (git, boid)", gateReq.BuiltinPolicies)
	}
	if _, ok := gateReq.HostCommands["boid"]; ok {
		t.Fatalf("gate host commands should not contain boid: %#v", gateReq.HostCommands)
	}
}

// PlanHook stages kit + project hook files into a single temp dir and the
// returned cleanup callback removes it.
func TestPlanHook_StagesHookFilesAndReturnsCleanup(t *testing.T) {
	projectDir := t.TempDir()
	projHooksDir := filepath.Join(projectDir, ".boid", "hooks")
	if err := os.MkdirAll(projHooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projHooksDir, "local.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatalf("write project hook: %v", err)
	}
	kitHooksDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(kitHooksDir, "run-agent.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatalf("write kit hook: %v", err)
	}

	planner := newPlannerForTest(&Project{ID: "proj-1", WorkDir: projectDir}, TaskBehavior{
		Name: "dev",
		KitHooksDirs: []KitHooksInfo{
			{HooksDir: kitHooksDir, Consumer: "claude-code"},
		},
	}, &Task{ID: "task-1", ProjectID: "proj-1", Behavior: "dev", Status: TaskStatusExecuting})

	req, hookCleanup, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Hook:      Hook{ID: "hook-1", ScriptPath: filepath.Join(projHooksDir, "local.sh")},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}
	if hookCleanup == nil {
		t.Fatal("PlanHook returned nil cleanup")
	}

	if len(req.Argv) == 0 {
		t.Fatal("JobSpec.Argv should have at least the entry script")
	}
	stagingDir := filepath.Dir(req.Argv[0])
	if _, err := os.Stat(filepath.Join(stagingDir, "local.sh")); err != nil {
		t.Errorf("project hook missing from staging dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stagingDir, "claude-code--run-agent.sh")); err != nil {
		t.Errorf("kit hook missing from staging dir: %v", err)
	}

	hookCleanup()
	if _, err := os.Stat(stagingDir); !os.IsNotExist(err) {
		t.Errorf("staging dir still present after cleanup: err=%v", err)
	}
}

// PlanGate stages kit gate scripts into a temp dir and returns a cleanup
// callback; JobSpec itself carries no KitGatesDirs / ProjectGatesDir fields.
func TestPlanGate_StagesGateScriptsAndReturnsCleanup(t *testing.T) {
	projectDir := t.TempDir()
	projGatesDir := filepath.Join(projectDir, ".boid", "gates")
	if err := os.MkdirAll(projGatesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	kitGatesDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(kitGatesDir, "gate-1.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatalf("write kit gate: %v", err)
	}

	planner := newPlannerForTest(&Project{ID: "proj-1", WorkDir: projectDir}, TaskBehavior{
		Name:         "dev",
		KitGatesDirs: []KitGatesInfo{{GatesDir: kitGatesDir, GateIDs: []string{"gate-1"}}},
	}, &Task{ID: "task-1", ProjectID: "proj-1", Behavior: "dev", Status: TaskStatusExecuting})

	req, gateCleanup, err := planner.PlanGate(&GateFireEvent{
		EventID:   "event-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Gate:      Gate{ID: "gate-1", ScriptPath: filepath.Join(kitGatesDir, "gate-1.sh")},
	})
	if err != nil {
		t.Fatalf("PlanGate: %v", err)
	}
	if gateCleanup == nil {
		t.Fatal("PlanGate returned nil cleanup")
	}

	if len(req.Argv) == 0 || filepath.Base(req.Argv[0]) != "gate-1.sh" {
		t.Fatalf("gate Argv[0] = %v, want staged gate-1.sh", req.Argv)
	}
	stagingDir := filepath.Dir(req.Argv[0])
	if _, err := os.Stat(filepath.Join(stagingDir, "gate-1.sh")); err != nil {
		t.Errorf("kit gate missing from staging dir: %v", err)
	}

	gateCleanup()
	if _, err := os.Stat(stagingDir); !os.IsNotExist(err) {
		t.Errorf("staging dir still present after cleanup: err=%v", err)
	}
}

// FilterInstructions picks a matching consumer; planner surfaces exactly one
// RoutedInstruction on JobSpec.
func TestPlanHook_Instruction_MatchingConsumer(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".boid", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	task := &Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Behavior:  "dev",
		Status:    TaskStatusExecuting,
		Instructions: map[string]Instruction{
			"main":   {Type: InstructionTypeExecution, Consumer: "claude-code", Message: "do X"},
			"review": {Type: InstructionTypeVerification, Consumer: "reviewer", Message: "check"},
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
			Consumer:   "claude-code",
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
	if req.Instruction.Consumer != "claude-code" {
		t.Errorf("Instruction.Consumer = %q, want claude-code", req.Instruction.Consumer)
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
			"verification": {"findings": []},
			"tasks": []
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
		Status:    TaskStatusVerifying,
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
