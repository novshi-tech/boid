package orchestrator

import (
	"encoding/json"
	"fmt"
	"path/filepath"
)

type MetaCache interface {
	Get(id string) (*ProjectMeta, bool)
}

type ProjectCatalog interface {
	GetProject(id string) (*Project, error)
}

type TaskLookup interface {
	GetTask(id string) (*Task, error)
}

// DispatchPlanner turns state-machine-driven hook / gate fire events into a
// sandbox-agnostic JobSpec. All sandbox construction concerns (mounts, env,
// proxy wiring, exit scripts, worktree recreation) live in dispatcher.
type DispatchPlanner struct {
	Meta     MetaCache
	Projects ProjectCatalog
	Tasks    TaskLookup
}

// PlanHook renders a hook fire event into a JobSpec. The returned cleanup
// callback must be invoked by the caller (dispatcher) once the sandbox
// process has exited — it removes the temporary hook staging directory.
func (p *DispatchPlanner) PlanHook(event *HookFireEvent) (*JobSpec, CleanupFunc, error) {
	if event == nil {
		return nil, nil, fmt.Errorf("hook event is required")
	}
	hookBase := filepath.Base(event.Hook.ScriptPath)
	if hookBase == "" || hookBase == "." {
		return nil, nil, fmt.Errorf("hook %q: no script path resolved", event.Hook.ID)
	}
	hookFilename := hookBase
	if event.Hook.Kit != "" {
		hookFilename = event.Hook.Kit + "--" + hookBase
	}

	meta, proj, task, err := p.loadContext(event.ProjectID, event.TaskID)
	if err != nil {
		return nil, nil, err
	}

	behavior, _ := lookupBehavior(meta, task)

	// Stage kit + project hook files under a single temp directory so the
	// entry script can source sibling helpers via a consistent path.
	projectHooksDir := filepath.Join(proj.WorkDir, ".boid", "hooks")
	stagingDir, cleanup, err := StageHooks(projectHooksDir, behavior.KitHooksDirs, event.EventID)
	if err != nil {
		return nil, nil, err
	}
	entryPath := filepath.Join(stagingDir, hookFilename)

	// Business payload filter: limit task.payload to the traits this hook declares.
	payload := FilterPayloadByTraits(task.Payload, event.Hook.Traits.Consumes)

	// 1 hook = 1 routed instruction. If multiple candidates match (same phase
	// and consumer), take the first after filtering.
	instruction := selectInstruction(task, event.Hook.Consumer)

	spec := &JobSpec{
		TaskID:       event.TaskID,
		ProjectID:    event.ProjectID,
		HandlerID:    event.Hook.ID,
		Kind:         JobKindHook,
		Argv:         []string{entryPath},
		Instruction:  instruction,
		Task:         snapshotTask(task),
		PrimaryInput: payload,
		Visibility: Visibility{
			ProjectDir:         proj.WorkDir,
			UseWorktree:        task.Worktree,
			AdditionalBindings: behavior.AdditionalBindings,
			Writable:           !IsReadonly(task),
		},
		BuiltinPolicies: DefaultBuiltinPolicies(
			RoleHook,
			[]string{"boid", "git"},
			PolicyContext{ProjectDir: proj.WorkDir, HomeDir: sandboxHomeDir()},
		),
		HostCommands:    nil, // hooks never get broker-mediated host commands
		SecretNamespace: meta.SecretNamespace,
		Env:             behavior.Env,
	}
	return spec, cleanup, nil
}

// PlanGate renders a gate fire event into a JobSpec. The returned cleanup
// callback releases the gate staging directory.
func (p *DispatchPlanner) PlanGate(event *GateFireEvent) (*JobSpec, CleanupFunc, error) {
	if event == nil {
		return nil, nil, fmt.Errorf("gate event is required")
	}
	gateFilename := filepath.Base(event.Gate.ScriptPath)
	if gateFilename == "" || gateFilename == "." {
		return nil, nil, fmt.Errorf("gate %q: no script path resolved", event.Gate.ID)
	}

	meta, proj, task, err := p.loadContext(event.ProjectID, event.TaskID)
	if err != nil {
		return nil, nil, err
	}

	behavior, _ := lookupBehavior(meta, task)

	// hook-updated payload overrides the DB value for this gate's task snapshot.
	if event.TaskPayloadJSON != "" {
		task.Payload = json.RawMessage(event.TaskPayloadJSON)
	}

	// gate scripts read the full task snapshot (including payload) from stdin.
	taskJSON, err := json.Marshal(task)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal task: %w", err)
	}

	projectGatesDir := filepath.Join(proj.WorkDir, ".boid", "gates")
	stagingDir, cleanup, err := StageGates(projectGatesDir, behavior.KitGatesDirs, event.EventID)
	if err != nil {
		return nil, nil, err
	}
	entryPath := filepath.Join(stagingDir, gateFilename)

	spec := &JobSpec{
		TaskID:       event.TaskID,
		ProjectID:    event.ProjectID,
		HandlerID:    event.Gate.ID,
		Kind:         JobKindGate,
		Argv:         []string{entryPath},
		Instruction:  nil,
		Task:         nil, // gate gets task data via stdin rather than context file
		PrimaryInput: taskJSON,
		Visibility: Visibility{
			// Project filesystem is intentionally not visible to gates.
			ProjectDir:  "",
			UseWorktree: false,
			// Gates never pass through kit CLIs; they only see their own tmpfs.
			AdditionalBindings: nil,
			Writable:           false,
		},
		BuiltinPolicies: DefaultBuiltinPolicies(
			RoleGate,
			[]string{"boid", "git"},
			PolicyContext{ProjectDir: proj.WorkDir, HomeDir: sandboxHomeDir()},
		),
		HostCommands:    behavior.HostCommands.ToCommandDefs(),
		SecretNamespace: meta.SecretNamespace,
		Env:             behavior.Env,
	}
	return spec, cleanup, nil
}

func (p *DispatchPlanner) loadContext(projectID, taskID string) (*ProjectMeta, *Project, *Task, error) {
	if p.Meta == nil || p.Projects == nil || p.Tasks == nil {
		return nil, nil, nil, fmt.Errorf("dispatch planner is not fully configured")
	}

	meta, ok := p.Meta.Get(projectID)
	if !ok {
		return nil, nil, nil, fmt.Errorf("project %q: meta not loaded", projectID)
	}
	proj, err := p.Projects.GetProject(projectID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get project: %w", err)
	}
	task, err := p.Tasks.GetTask(taskID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get task: %w", err)
	}
	return meta, proj, task, nil
}

func selectInstruction(task *Task, consumer string) *RoutedInstruction {
	instType := InstructionTypeForStatus(task.Status)
	routed := FilterInstructions(task.Instructions, instType, consumer)
	if len(routed) == 0 {
		return nil
	}
	// 1 ジョブ = 1 routed instruction。複数候補があれば先頭を採用する。
	selected := routed[0]
	return &selected
}

func snapshotTask(task *Task) *TaskSnapshot {
	if task == nil {
		return nil
	}
	return &TaskSnapshot{
		ID:          task.ID,
		Title:       task.Title,
		Status:      string(task.Status),
		Behavior:    task.Behavior,
		Description: task.Description,
	}
}
