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

func (s stubProjectCatalog) ListProjects() ([]*Project, error) {
	return s.projects, nil
}

func TestDispatchPlannerCollectWorkspaceDirs_UsesProjectWorkspaceMembership(t *testing.T) {
	planner := &DispatchPlanner{
		Projects: stubProjectCatalog{
			projects: []*Project{
				{ID: "proj-1", WorkspaceID: "ws-1", WorkDir: "/workspace/proj-1"},
				{ID: "proj-2", WorkspaceID: "ws-1", WorkDir: "/workspace/proj-2"},
				{ID: "proj-3", WorkspaceID: "ws-2", WorkDir: "/workspace/proj-3"},
			},
		},
	}

	dirs, err := planner.collectWorkspaceDirs("ws-1", "proj-1")
	if err != nil {
		t.Fatalf("collectWorkspaceDirs: %v", err)
	}
	if len(dirs) != 1 {
		t.Fatalf("expected 1 peer workspace dir, got %d", len(dirs))
	}
	if dirs["proj-2"] != "/workspace/proj-2" {
		t.Fatalf("peer workspace dir = %#v", dirs)
	}
	if _, ok := dirs["proj-3"]; ok {
		t.Fatalf("unexpected workspace dir from another workspace: %#v", dirs)
	}
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

func TestDispatchPlannerAddsBoidAsBuiltinForHookAndGate(t *testing.T) {
	meta := &ProjectMeta{
		ID: "proj-1",
		TaskBehaviors: map[string]TaskBehavior{
			"dev": {
				Name:            "dev",
				BuiltinCommands: []string{"git"},
				HostCommands:    HostCommands{"git": {Path: "/usr/bin/git"}},
			},
		},
	}
	proj := &Project{ID: "proj-1", WorkDir: "/workspace/proj-1"}
	task := &Task{ID: "task-1", ProjectID: "proj-1", Behavior: "dev", Status: TaskStatusExecuting}

	planner := &DispatchPlanner{
		Meta:     stubMetaCache{meta: meta},
		Projects: stubProjectCatalog{projects: []*Project{proj}},
		Tasks:    stubTaskLookup{task: task},
	}

	hookReq, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    task.ID,
		ProjectID: proj.ID,
		Hook:      Hook{ID: "hook-1", ScriptPath: "/workspace/proj-1/.boid/hooks/hook-1.sh"},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}
	if len(hookReq.BuiltinPolicies) != 2 {
		t.Fatalf("hook builtin policies = %#v, want 2 entries (git, boid)", hookReq.BuiltinPolicies)
	}
	if _, ok := hookReq.BuiltinPolicies["git"]; !ok {
		t.Fatalf("hook builtin policies missing git: %#v", hookReq.BuiltinPolicies)
	}
	if _, ok := hookReq.BuiltinPolicies["boid"]; !ok {
		t.Fatalf("hook builtin policies missing boid: %#v", hookReq.BuiltinPolicies)
	}
	if hookReq.HostCommands != nil {
		t.Fatalf("hook host commands = %#v, want nil", hookReq.HostCommands)
	}

	gateReq, err := planner.PlanGate(&GateFireEvent{
		EventID:   "event-2",
		TaskID:    task.ID,
		ProjectID: proj.ID,
		Gate:      Gate{ID: "gate-1", ScriptPath: "/workspace/proj-1/.boid/gates/gate-1.sh"},
	})
	if err != nil {
		t.Fatalf("PlanGate: %v", err)
	}
	if len(gateReq.BuiltinPolicies) != 2 {
		t.Fatalf("gate builtin policies = %#v, want 2 entries (git, boid)", gateReq.BuiltinPolicies)
	}
	if _, ok := gateReq.BuiltinPolicies["git"]; !ok {
		t.Fatalf("gate builtin policies missing git: %#v", gateReq.BuiltinPolicies)
	}
	if _, ok := gateReq.BuiltinPolicies["boid"]; !ok {
		t.Fatalf("gate builtin policies missing boid: %#v", gateReq.BuiltinPolicies)
	}
	if _, ok := gateReq.HostCommands["boid"]; ok {
		t.Fatalf("gate host commands should not contain boid: %#v", gateReq.HostCommands)
	}
}

func TestPlanHook_CollectsKitAndProjectHookFiles(t *testing.T) {
	projectDir := t.TempDir()
	kitHooksDir := t.TempDir()

	// kit hook
	if err := os.MkdirAll(kitHooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kitHooksDir, "run-agent.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatalf("write kit hook: %v", err)
	}

	// project hook
	projHooksDir := filepath.Join(projectDir, ".boid", "hooks")
	if err := os.MkdirAll(projHooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projHooksDir, "local.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatalf("write project hook: %v", err)
	}

	meta := &ProjectMeta{
		ID: "proj-1",
		TaskBehaviors: map[string]TaskBehavior{
			"dev": {
				Name: "dev",
				KitHooksDirs: []KitHooksInfo{
					{HooksDir: kitHooksDir, Consumer: "claude-code"},
				},
			},
		},
	}
	proj := &Project{ID: "proj-1", WorkDir: projectDir}
	task := &Task{ID: "task-1", ProjectID: "proj-1", Behavior: "dev", Status: TaskStatusExecuting}

	planner := &DispatchPlanner{
		Meta:     stubMetaCache{meta: meta},
		Projects: stubProjectCatalog{projects: []*Project{proj}},
		Tasks:    stubTaskLookup{task: task},
	}

	req, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    task.ID,
		ProjectID: proj.ID,
		Hook:      Hook{ID: "hook-1", ScriptPath: filepath.Join(projHooksDir, "local.sh")},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}

	byTarget := make(map[string]HookFile)
	for _, hf := range req.HookFiles {
		byTarget[hf.TargetName] = hf
	}

	// Kit hook should be prefixed with consumer name
	kitHook, ok := byTarget["claude-code--run-agent.sh"]
	if !ok {
		t.Errorf("kit hook missing: want target=claude-code--run-agent.sh, got %v", byTarget)
	} else if kitHook.Source != filepath.Join(kitHooksDir, "run-agent.sh") {
		t.Errorf("kit hook source = %q, want %q", kitHook.Source, filepath.Join(kitHooksDir, "run-agent.sh"))
	}

	// Project hook should not be prefixed
	projHook, ok := byTarget["local.sh"]
	if !ok {
		t.Errorf("project hook missing: want target=local.sh, got %v", byTarget)
	} else if projHook.Source != filepath.Join(projHooksDir, "local.sh") {
		t.Errorf("project hook source = %q, want %q", projHook.Source, filepath.Join(projHooksDir, "local.sh"))
	}

	// No staging dir for hooks
	if req.StagingDir != "" {
		t.Errorf("expected no StagingDir for hooks, got %q", req.StagingDir)
	}
}

func TestDispatchPlannerPlanGatePassesKitGatesThrough(t *testing.T) {
	projectDir := t.TempDir()
	kitGatesDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(kitGatesDir, "gate-1.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatalf("write kit gate: %v", err)
	}

	kitGatesDirs := []KitGatesInfo{{GatesDir: kitGatesDir, GateIDs: []string{"gate-1"}}}
	meta := &ProjectMeta{
		ID: "proj-1",
		TaskBehaviors: map[string]TaskBehavior{
			"dev": {
				Name:         "dev",
				KitGatesDirs: kitGatesDirs,
			},
		},
	}
	proj := &Project{ID: "proj-1", WorkDir: projectDir}
	task := &Task{ID: "task-1", ProjectID: "proj-1", Behavior: "dev", Status: TaskStatusExecuting}

	planner := &DispatchPlanner{
		Meta:     stubMetaCache{meta: meta},
		Projects: stubProjectCatalog{projects: []*Project{proj}},
		Tasks:    stubTaskLookup{task: task},
	}

	req, err := planner.PlanGate(&GateFireEvent{
		EventID:   "event-1",
		TaskID:    task.ID,
		ProjectID: proj.ID,
		Gate:      Gate{ID: "gate-1", ScriptPath: filepath.Join(kitGatesDir, "gate-1.sh")},
	})
	if err != nil {
		t.Fatalf("PlanGate: %v", err)
	}

	// Staging を planner 側では行わない: StagingDir は空のまま。
	if req.StagingDir != "" {
		t.Fatalf("expected empty StagingDir (dispatcher-owned), got %q", req.StagingDir)
	}
	if req.ProjectGatesDir != filepath.Join(projectDir, ".boid", "gates") {
		t.Fatalf("ProjectGatesDir = %q", req.ProjectGatesDir)
	}
	if len(req.KitGatesDirs) != 1 || req.KitGatesDirs[0].GatesDir != kitGatesDir {
		t.Fatalf("KitGatesDirs = %#v", req.KitGatesDirs)
	}
}

func TestPlanHook_InstructionsJSON_MatchingConsumer(t *testing.T) {
	meta := &ProjectMeta{
		ID: "proj-1",
	}
	proj := &Project{ID: "proj-1", WorkDir: t.TempDir()}
	task := &Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Behavior:  "dev",
		Status:    TaskStatusVerifying,
		Instructions: map[string]Instruction{
			"reviewer": {Type: InstructionTypeVerification, Consumer: "claude-code", Message: "check formatting"},
			"executor": {Type: InstructionTypeExecution, Consumer: "claude-code", Message: "run tests"},
		},
	}

	planner := &DispatchPlanner{
		Meta:     stubMetaCache{meta: meta},
		Projects: stubProjectCatalog{projects: []*Project{proj}},
		Tasks:    stubTaskLookup{task: task},
	}

	req, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    task.ID,
		ProjectID: proj.ID,
		Hook: Hook{
			ID:         "hook-1",
			ScriptPath: filepath.Join(proj.WorkDir, ".boid", "hooks", "hook-1.sh"),
			Consumer:   "claude-code",
		},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}

	if req.InstructionsJSON == "" {
		t.Fatal("expected InstructionsJSON to be set")
	}

	var instructions []RoutedInstruction
	if err := json.Unmarshal([]byte(req.InstructionsJSON), &instructions); err != nil {
		t.Fatalf("unmarshal InstructionsJSON: %v", err)
	}
	if len(instructions) != 1 {
		t.Fatalf("expected 1 instruction (only verification type for verifying status), got %d", len(instructions))
	}
	if instructions[0].Role != "reviewer" {
		t.Errorf("expected role=reviewer, got %q", instructions[0].Role)
	}
	if instructions[0].Consumer != "claude-code" {
		t.Errorf("expected consumer=claude-code, got %q", instructions[0].Consumer)
	}
	if instructions[0].Message != "check formatting" {
		t.Errorf("expected message=%q, got %q", "check formatting", instructions[0].Message)
	}
}

func TestPlanHook_InstructionsJSON_NoInstructions(t *testing.T) {
	payload := json.RawMessage(`{"prompt":"do stuff"}`)
	meta := &ProjectMeta{
		ID: "proj-1",
	}
	proj := &Project{ID: "proj-1", WorkDir: t.TempDir()}
	task := &Task{
		ID:        "task-2",
		ProjectID: "proj-1",
		Behavior:  "dev",
		Status:    TaskStatusExecuting,
		Payload:   payload,
	}

	planner := &DispatchPlanner{
		Meta:     stubMetaCache{meta: meta},
		Projects: stubProjectCatalog{projects: []*Project{proj}},
		Tasks:    stubTaskLookup{task: task},
	}

	req, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-2",
		TaskID:    task.ID,
		ProjectID: proj.ID,
		Hook: Hook{
			ID:         "hook-1",
			ScriptPath: filepath.Join(proj.WorkDir, ".boid", "hooks", "hook-1.sh"),
			Consumer:   "claude-code",
		},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}

	if req.InstructionsJSON != "" {
		t.Errorf("expected InstructionsJSON to be empty, got %q", req.InstructionsJSON)
	}
}

func TestPlanHook_InstructionsJSON_ConsumerMismatch(t *testing.T) {
	meta := &ProjectMeta{
		ID: "proj-1",
	}
	proj := &Project{ID: "proj-1", WorkDir: t.TempDir()}
	task := &Task{
		ID:        "task-3",
		ProjectID: "proj-1",
		Behavior:  "dev",
		Status:    TaskStatusExecuting,
		Instructions: map[string]Instruction{
			"executor": {Type: InstructionTypeExecution, Consumer: "other-agent", Message: "run tests"},
		},
	}

	planner := &DispatchPlanner{
		Meta:     stubMetaCache{meta: meta},
		Projects: stubProjectCatalog{projects: []*Project{proj}},
		Tasks:    stubTaskLookup{task: task},
	}

	req, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-3",
		TaskID:    task.ID,
		ProjectID: proj.ID,
		Hook: Hook{
			ID:         "hook-1",
			ScriptPath: filepath.Join(proj.WorkDir, ".boid", "hooks", "hook-1.sh"),
			Consumer:   "claude-code",
		},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}

	if req.InstructionsJSON != "" {
		t.Errorf("expected InstructionsJSON to be empty when consumer mismatches, got %q", req.InstructionsJSON)
	}
}

func TestPlanHook_InstructionsJSON_MultipleRolesSorted(t *testing.T) {
	meta := &ProjectMeta{
		ID: "proj-1",
	}
	proj := &Project{ID: "proj-1", WorkDir: t.TempDir()}
	task := &Task{
		ID:        "task-4",
		ProjectID: "proj-1",
		Behavior:  "dev",
		Status:    TaskStatusExecuting,
		Instructions: map[string]Instruction{
			"zebra":  {Type: InstructionTypeExecution, Consumer: "claude-code", Message: "last"},
			"alpha":  {Type: InstructionTypeExecution, Consumer: "claude-code", Message: "first"},
			"middle": {Type: InstructionTypeExecution, Consumer: "claude-code", Message: "middle"},
		},
	}

	planner := &DispatchPlanner{
		Meta:     stubMetaCache{meta: meta},
		Projects: stubProjectCatalog{projects: []*Project{proj}},
		Tasks:    stubTaskLookup{task: task},
	}

	req, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-4",
		TaskID:    task.ID,
		ProjectID: proj.ID,
		Hook: Hook{
			ID:         "hook-1",
			ScriptPath: filepath.Join(proj.WorkDir, ".boid", "hooks", "hook-1.sh"),
			Consumer:   "claude-code",
		},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}

	if req.InstructionsJSON == "" {
		t.Fatal("expected InstructionsJSON to be set")
	}

	var instructions []RoutedInstruction
	if err := json.Unmarshal([]byte(req.InstructionsJSON), &instructions); err != nil {
		t.Fatalf("unmarshal InstructionsJSON: %v", err)
	}
	if len(instructions) != 3 {
		t.Fatalf("expected 3 instructions, got %d", len(instructions))
	}

	// Verify sorted order
	roles := make([]string, len(instructions))
	for i, inst := range instructions {
		roles[i] = inst.Role
	}
	expected := []string{"alpha", "middle", "zebra"}
	for i, want := range expected {
		if roles[i] != want {
			t.Errorf("instruction[%d].Role = %q, want %q", i, roles[i], want)
		}
	}
	_ = strings.Join(roles, ",") // use the strings import
}

func TestPlanHook_TaskYAML(t *testing.T) {
	meta := &ProjectMeta{ID: "proj-1"}
	proj := &Project{ID: "proj-1", WorkDir: t.TempDir()}
	task := &Task{
		ID:          "task-yaml-1",
		ProjectID:   "proj-1",
		Title:       "Implement OAuth",
		Description: "Add OAuth2 login",
		Behavior:    "impl",
		Status:      TaskStatusExecuting,
		Payload:     json.RawMessage(`{}`),
	}

	planner := &DispatchPlanner{
		Meta:     stubMetaCache{meta: meta},
		Projects: stubProjectCatalog{projects: []*Project{proj}},
		Tasks:    stubTaskLookup{task: task},
	}

	req, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    task.ID,
		ProjectID: proj.ID,
		Hook:      Hook{ID: "hook-1", ScriptPath: filepath.Join(proj.WorkDir, ".boid", "hooks", "hook-1.sh")},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}

	if req.TaskYAML == "" {
		t.Fatal("expected TaskYAML to be set")
	}
	if !strings.Contains(req.TaskYAML, "task-yaml-1") {
		t.Error("TaskYAML missing task ID")
	}
	if !strings.Contains(req.TaskYAML, "Implement OAuth") {
		t.Error("TaskYAML missing title")
	}
	if !strings.Contains(req.TaskYAML, "executing") {
		t.Error("TaskYAML missing status")
	}
	if !strings.Contains(req.TaskYAML, "impl") {
		t.Error("TaskYAML missing behavior")
	}
	if !strings.Contains(req.TaskYAML, "Add OAuth2 login") {
		t.Error("TaskYAML missing description")
	}
}

func TestPlanHook_PayloadJSON_FilteredByConsumes(t *testing.T) {
	payload := json.RawMessage(`{
		"artifact": "http://example.com/artifact",
		"instructions": {
			"exec": {"type":"execution","consumer":"claude-code","message":"run tests"}
		}
	}`)
	meta := &ProjectMeta{ID: "proj-1"}
	proj := &Project{ID: "proj-1", WorkDir: t.TempDir()}
	task := &Task{
		ID:        "task-payload-filter-1",
		ProjectID: "proj-1",
		Behavior:  "dev",
		Status:    TaskStatusExecuting,
		Payload:   payload,
	}

	planner := &DispatchPlanner{
		Meta:     stubMetaCache{meta: meta},
		Projects: stubProjectCatalog{projects: []*Project{proj}},
		Tasks:    stubTaskLookup{task: task},
	}

	req, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    task.ID,
		ProjectID: proj.ID,
		Hook: Hook{
			ID:         "hook-1",
			ScriptPath: filepath.Join(proj.WorkDir, ".boid", "hooks", "hook-1.sh"),
			Consumer:   "claude-code",
			Traits:     HandlerTraits{Consumes: []TraitType{TraitArtifact}},
		},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(req.PayloadJSON), &m); err != nil {
		t.Fatalf("unmarshal PayloadJSON: %v", err)
	}
	if _, ok := m["artifact"]; !ok {
		t.Error("expected artifact key in PayloadJSON")
	}
	if _, ok := m["instructions"]; ok {
		t.Error("instructions should be filtered out of PayloadJSON when not in Consumes")
	}
}

func TestPlanHook_PayloadJSON_EmptyConsumes(t *testing.T) {
	payload := json.RawMessage(`{"artifact":"url"}`)
	meta := &ProjectMeta{ID: "proj-1"}
	proj := &Project{ID: "proj-1", WorkDir: t.TempDir()}
	task := &Task{
		ID:        "task-payload-filter-2",
		ProjectID: "proj-1",
		Behavior:  "dev",
		Status:    TaskStatusExecuting,
		Payload:   payload,
	}

	planner := &DispatchPlanner{
		Meta:     stubMetaCache{meta: meta},
		Projects: stubProjectCatalog{projects: []*Project{proj}},
		Tasks:    stubTaskLookup{task: task},
	}

	req, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    task.ID,
		ProjectID: proj.ID,
		Hook: Hook{
			ID:         "hook-1",
			ScriptPath: filepath.Join(proj.WorkDir, ".boid", "hooks", "hook-1.sh"),
			Traits:     HandlerTraits{Consumes: nil},
		},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}
	if req.PayloadJSON != "{}" {
		t.Fatalf("expected empty payload when no consumes, got %q", req.PayloadJSON)
	}
}

func TestPlanHook_Interactive_PropagatedToJobSpec(t *testing.T) {
	meta := &ProjectMeta{ID: "proj-1"}
	proj := &Project{ID: "proj-1", WorkDir: t.TempDir()}
	task := &Task{
		ID:        "task-interactive-1",
		ProjectID: "proj-1",
		Behavior:  "dev",
		Status:    TaskStatusExecuting,
		Instructions: map[string]Instruction{
			"executor": {Type: InstructionTypeExecution, Consumer: "claude-code", Message: "run it", Interactive: true},
		},
	}

	planner := &DispatchPlanner{
		Meta:     stubMetaCache{meta: meta},
		Projects: stubProjectCatalog{projects: []*Project{proj}},
		Tasks:    stubTaskLookup{task: task},
	}

	req, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    task.ID,
		ProjectID: proj.ID,
		Hook: Hook{
			ID:         "hook-1",
			ScriptPath: filepath.Join(proj.WorkDir, ".boid", "hooks", "hook-1.sh"),
			Consumer:   "claude-code",
		},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}
	if !req.Interactive {
		t.Fatal("expected Interactive=true, got false")
	}
}

func TestPlanHook_Interactive_FalseWhenNotSet(t *testing.T) {
	meta := &ProjectMeta{ID: "proj-1"}
	proj := &Project{ID: "proj-1", WorkDir: t.TempDir()}
	task := &Task{
		ID:        "task-interactive-2",
		ProjectID: "proj-1",
		Behavior:  "dev",
		Status:    TaskStatusExecuting,
		Instructions: map[string]Instruction{
			"executor": {Type: InstructionTypeExecution, Consumer: "claude-code", Message: "run it"},
		},
	}

	planner := &DispatchPlanner{
		Meta:     stubMetaCache{meta: meta},
		Projects: stubProjectCatalog{projects: []*Project{proj}},
		Tasks:    stubTaskLookup{task: task},
	}

	req, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-2",
		TaskID:    task.ID,
		ProjectID: proj.ID,
		Hook: Hook{
			ID:         "hook-1",
			ScriptPath: filepath.Join(proj.WorkDir, ".boid", "hooks", "hook-1.sh"),
			Consumer:   "claude-code",
		},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}
	if req.Interactive {
		t.Fatal("expected Interactive=false, got true")
	}
}

func TestPlanHook_Model_PropagatedFromInstruction(t *testing.T) {
	meta := &ProjectMeta{ID: "proj-1"}
	proj := &Project{ID: "proj-1", WorkDir: t.TempDir()}
	task := &Task{
		ID:        "task-model-1",
		ProjectID: "proj-1",
		Behavior:  "dev",
		Status:    TaskStatusExecuting,
		Instructions: map[string]Instruction{
			"executor": {Type: InstructionTypeExecution, Consumer: "claude-code", Message: "run it", Model: "claude-opus-4-6"},
		},
	}

	planner := &DispatchPlanner{
		Meta:     stubMetaCache{meta: meta},
		Projects: stubProjectCatalog{projects: []*Project{proj}},
		Tasks:    stubTaskLookup{task: task},
	}

	req, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-model-1",
		TaskID:    task.ID,
		ProjectID: proj.ID,
		Hook: Hook{
			ID:         "hook-1",
			ScriptPath: filepath.Join(proj.WorkDir, ".boid", "hooks", "hook-1.sh"),
			Consumer:   "claude-code",
		},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}
	if req.Model != "claude-opus-4-6" {
		t.Errorf("expected Model=%q, got %q", "claude-opus-4-6", req.Model)
	}
}

func TestPlanHook_Model_EmptyWhenNotSet(t *testing.T) {
	meta := &ProjectMeta{ID: "proj-1"}
	proj := &Project{ID: "proj-1", WorkDir: t.TempDir()}
	task := &Task{
		ID:        "task-model-2",
		ProjectID: "proj-1",
		Behavior:  "dev",
		Status:    TaskStatusExecuting,
		Instructions: map[string]Instruction{
			"executor": {Type: InstructionTypeExecution, Consumer: "claude-code", Message: "run it"},
		},
	}

	planner := &DispatchPlanner{
		Meta:     stubMetaCache{meta: meta},
		Projects: stubProjectCatalog{projects: []*Project{proj}},
		Tasks:    stubTaskLookup{task: task},
	}

	req, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-model-2",
		TaskID:    task.ID,
		ProjectID: proj.ID,
		Hook: Hook{
			ID:         "hook-1",
			ScriptPath: filepath.Join(proj.WorkDir, ".boid", "hooks", "hook-1.sh"),
			Consumer:   "claude-code",
		},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}
	if req.Model != "" {
		t.Errorf("expected Model empty, got %q", req.Model)
	}
}

func TestPlanHook_PayloadJSON_OptionalVerificationIncludedDuringRework(t *testing.T) {
	payload := json.RawMessage(`{
		"artifact": {"summary":"initial impl"},
		"verification": {"pr-verify": {"findings": [{"message":"CI failed","status":"open"}]}}
	}`)
	meta := &ProjectMeta{ID: "proj-1"}
	proj := &Project{ID: "proj-1", WorkDir: t.TempDir()}
	task := &Task{
		ID:        "task-rework-verify-1",
		ProjectID: "proj-1",
		Behavior:  "dev",
		Status:    TaskStatusReworking,
		Payload:   payload,
		Instructions: map[string]Instruction{
			"rework": {Type: InstructionTypeRework, Consumer: "claude-code", Message: "fix findings"},
		},
	}

	planner := &DispatchPlanner{
		Meta:     stubMetaCache{meta: meta},
		Projects: stubProjectCatalog{projects: []*Project{proj}},
		Tasks:    stubTaskLookup{task: task},
	}

	// Agent hook that declares consumes: [verification?]
	req, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    task.ID,
		ProjectID: proj.ID,
		Hook: Hook{
			ID:         "hook-1",
			ScriptPath: filepath.Join(proj.WorkDir, ".boid", "hooks", "hook-1.sh"),
			Kind:       HandlerKindAgent,
			Consumer:   "claude-code",
			Traits:     HandlerTraits{Consumes: []TraitType{"verification?"}},
		},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(req.PayloadJSON), &m); err != nil {
		t.Fatalf("unmarshal PayloadJSON: %v", err)
	}
	if _, ok := m["verification"]; !ok {
		t.Error("expected verification in PayloadJSON (optional trait present)")
	}
	if _, ok := m["artifact"]; ok {
		t.Error("artifact should be filtered out since not in consumes")
	}
}

func TestPlanHook_EnvironmentYAML(t *testing.T) {
	meta := &ProjectMeta{
		ID: "proj-1",
		TaskBehaviors: map[string]TaskBehavior{
			"dev": {
				Name:            "dev",
				BuiltinCommands: []string{"git", "python3"},
			},
		},
	}
	proj := &Project{ID: "proj-1", WorkDir: t.TempDir(), WorkspaceID: "ws-1"}
	peer := &Project{ID: "proj-2", WorkDir: "/workspace/peer", WorkspaceID: "ws-1"}
	task := &Task{
		ID:        "task-env-1",
		ProjectID: "proj-1",
		Behavior:  "dev",
		Status:    TaskStatusVerifying,
		Payload:   json.RawMessage(`{}`),
	}

	planner := &DispatchPlanner{
		Meta:     stubMetaCache{meta: meta},
		Projects: stubProjectCatalog{projects: []*Project{proj, peer}},
		Tasks:    stubTaskLookup{task: task},
	}

	req, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-1",
		TaskID:    task.ID,
		ProjectID: proj.ID,
		Hook:      Hook{ID: "hook-1", ScriptPath: filepath.Join(proj.WorkDir, ".boid", "hooks", "hook-1.sh")},
	})
	if err != nil {
		t.Fatalf("PlanHook: %v", err)
	}

	if req.EnvironmentYAML == "" {
		t.Fatal("expected EnvironmentYAML to be set")
	}
	// verifying status → readonly should be true
	if !strings.Contains(req.EnvironmentYAML, "readonly: true") {
		t.Error("EnvironmentYAML should have readonly: true for verifying status")
	}
	if !strings.Contains(req.EnvironmentYAML, "restricted") {
		t.Error("EnvironmentYAML missing network info")
	}
	if !strings.Contains(req.EnvironmentYAML, "git") {
		t.Error("EnvironmentYAML missing git tool")
	}
	if !strings.Contains(req.EnvironmentYAML, "python3") {
		t.Error("EnvironmentYAML missing python3 tool")
	}
	if !strings.Contains(req.EnvironmentYAML, "/workspace/peer") {
		t.Error("EnvironmentYAML missing workspace project")
	}
}

func TestPlanGate_WorkspaceDirsPopulated(t *testing.T) {
	meta := &ProjectMeta{ID: "proj-1"}
	proj := &Project{ID: "proj-1", WorkDir: t.TempDir(), WorkspaceID: "ws-1"}
	peer := &Project{ID: "proj-2", WorkDir: "/workspace/peer", WorkspaceID: "ws-1"}
	task := &Task{
		ID:        "task-gate-ws-1",
		ProjectID: "proj-1",
		Behavior:  "dev",
		Status:    TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}

	planner := &DispatchPlanner{
		Meta:     stubMetaCache{meta: meta},
		Projects: stubProjectCatalog{projects: []*Project{proj, peer}},
		Tasks:    stubTaskLookup{task: task},
	}

	req, err := planner.PlanGate(&GateFireEvent{
		EventID:   "event-gate-1",
		TaskID:    task.ID,
		ProjectID: proj.ID,
		Gate:      Gate{ID: "gate-1", ScriptPath: filepath.Join(proj.WorkDir, ".boid", "gates", "gate-1.sh")},
	})
	if err != nil {
		t.Fatalf("PlanGate: %v", err)
	}

	if req.WorkspaceDirs["proj-2"] != "/workspace/peer" {
		t.Errorf("WorkspaceDirs[proj-2] = %q, want %q", req.WorkspaceDirs["proj-2"], "/workspace/peer")
	}
}
