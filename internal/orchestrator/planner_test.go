package orchestrator

import (
	"os"
	"path/filepath"
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
