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
		HostCommands: HostCommands{
			"git": {Path: "/usr/bin/git"},
		},
		BuiltinCommands: []string{"git"},
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
	if len(hookReq.BuiltinCommands) != 2 || hookReq.BuiltinCommands[0] != "git" || hookReq.BuiltinCommands[1] != "boid" {
		t.Fatalf("hook builtin commands = %#v, want [git boid]", hookReq.BuiltinCommands)
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
	if len(gateReq.BuiltinCommands) != 2 || gateReq.BuiltinCommands[0] != "git" || gateReq.BuiltinCommands[1] != "boid" {
		t.Fatalf("gate builtin commands = %#v, want [git boid]", gateReq.BuiltinCommands)
	}
	if _, ok := gateReq.HostCommands["boid"]; ok {
		t.Fatalf("gate host commands should not contain boid: %#v", gateReq.HostCommands)
	}
}

func TestDispatchPlannerPlanGateStagesKitGates(t *testing.T) {
	projectDir := t.TempDir()
	kitGatesDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(kitGatesDir, "gate-1.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatalf("write kit gate: %v", err)
	}

	meta := &ProjectMeta{
		ID:           "proj-1",
		KitGatesDirs: []KitGatesInfo{{GatesDir: kitGatesDir, GateIDs: []string{"gate-1"}}},
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

	if req.GatesDir == "" || req.StagingDir == "" {
		t.Fatalf("expected staged gates dir, got GatesDir=%q StagingDir=%q", req.GatesDir, req.StagingDir)
	}
	if req.GatesDir != req.StagingDir {
		t.Fatalf("expected GatesDir and StagingDir to match, got %q vs %q", req.GatesDir, req.StagingDir)
	}
	if req.GatesDir == filepath.Join(projectDir, ".boid", "gates") {
		t.Fatalf("expected kit gates to be staged, got project gates dir %q", req.GatesDir)
	}
	if _, err := os.Stat(filepath.Join(req.GatesDir, "gate-1.sh")); err != nil {
		t.Fatalf("expected staged gate script: %v", err)
	}
}

func TestPlanHook_InstructionsJSON_MatchingConsumer(t *testing.T) {
	payload := json.RawMessage(`{
		"instructions": {
			"reviewer": {"type":"verification","consumer":"claude-code","message":"check formatting"},
			"executor": {"type":"execution","consumer":"claude-code","message":"run tests"}
		}
	}`)
	meta := &ProjectMeta{
		ID: "proj-1",
	}
	proj := &Project{ID: "proj-1", WorkDir: t.TempDir()}
	task := &Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Behavior:  "dev",
		Status:    TaskStatusVerifying,
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
	payload := json.RawMessage(`{
		"instructions": {
			"executor": {"type":"execution","consumer":"other-agent","message":"run tests"}
		}
	}`)
	meta := &ProjectMeta{
		ID: "proj-1",
	}
	proj := &Project{ID: "proj-1", WorkDir: t.TempDir()}
	task := &Task{
		ID:        "task-3",
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
	payload := json.RawMessage(`{
		"instructions": {
			"zebra": {"type":"execution","consumer":"claude-code","message":"last"},
			"alpha": {"type":"execution","consumer":"claude-code","message":"first"},
			"middle": {"type":"execution","consumer":"claude-code","message":"middle"}
		}
	}`)
	meta := &ProjectMeta{
		ID: "proj-1",
	}
	proj := &Project{ID: "proj-1", WorkDir: t.TempDir()}
	task := &Task{
		ID:        "task-4",
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

func TestPlanHook_InstructionsJSON_ReworkingStatus(t *testing.T) {
	payload := json.RawMessage(`{
		"instructions": {
			"main":   {"type":"execution","consumer":"claude-code","message":"implement the feature"},
			"rework": {"type":"rework","consumer":"claude-code","message":"CI \u5931\u6557\u3092\u4fee\u6b63\u3057\u3066\u304f\u3060\u3055\u3044"}
		}
	}`)
	meta := &ProjectMeta{
		ID: "proj-1",
	}
	proj := &Project{ID: "proj-1", WorkDir: t.TempDir()}
	task := &Task{
		ID:        "task-rework-1",
		ProjectID: "proj-1",
		Behavior:  "dev",
		Status:    TaskStatusReworking,
		Payload:   payload,
	}

	planner := &DispatchPlanner{
		Meta:     stubMetaCache{meta: meta},
		Projects: stubProjectCatalog{projects: []*Project{proj}},
		Tasks:    stubTaskLookup{task: task},
	}

	req, err := planner.PlanHook(&HookFireEvent{
		EventID:   "event-rework-1",
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
		t.Fatal("expected InstructionsJSON to be set for reworking state")
	}

	var instructions []RoutedInstruction
	if err := json.Unmarshal([]byte(req.InstructionsJSON), &instructions); err != nil {
		t.Fatalf("unmarshal InstructionsJSON: %v", err)
	}
	// reworking \u72b6\u614b\u3067\u306f rework \u578b instruction \u306e\u307f\u304c\u30eb\u30fc\u30c6\u30a3\u30f3\u30b0\u3055\u308c\u308b
	if len(instructions) != 1 {
		t.Fatalf("expected 1 instruction (rework type only), got %d", len(instructions))
	}
	if instructions[0].Role != "rework" {
		t.Errorf("expected role=rework, got %q", instructions[0].Role)
	}
	if instructions[0].Type != InstructionTypeRework {
		t.Errorf("expected type=rework, got %q", instructions[0].Type)
	}
	if instructions[0].Consumer != "claude-code" {
		t.Errorf("expected consumer=claude-code, got %q", instructions[0].Consumer)
	}
}

func TestPlanHook_Interactive_PropagatedToDispatchRequest(t *testing.T) {
	payload := json.RawMessage(`{
		"instructions":{
			"executor":{"type":"execution","consumer":"claude-code","message":"run it","interactive":true}
		}
	}`)
	meta := &ProjectMeta{ID: "proj-1"}
	proj := &Project{ID: "proj-1", WorkDir: t.TempDir()}
	task := &Task{
		ID:        "task-interactive-1",
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
	payload := json.RawMessage(`{
		"instructions":{
			"executor":{"type":"execution","consumer":"claude-code","message":"run it"}
		}
	}`)
	meta := &ProjectMeta{ID: "proj-1"}
	proj := &Project{ID: "proj-1", WorkDir: t.TempDir()}
	task := &Task{
		ID:        "task-interactive-2",
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
	if req.Interactive {
		t.Fatal("expected Interactive=false, got true")
	}
}

func TestPlanHook_EnvironmentYAML(t *testing.T) {
	meta := &ProjectMeta{
		ID:              "proj-1",
		BuiltinCommands: []string{"git", "python3"},
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
