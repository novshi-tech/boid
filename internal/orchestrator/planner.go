package orchestrator

import (
	"fmt"

	"github.com/novshi-tech/boid/internal/adapters"
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

// DispatchPlanner turns state-machine-driven hook fire events into a
// sandbox-agnostic JobSpec. All sandbox construction concerns (mounts, env,
// proxy wiring, exit scripts, worktree recreation) live in dispatcher.
type DispatchPlanner struct {
	Meta     MetaCache
	Projects ProjectCatalog
	Tasks    TaskLookup
	Adapter  adapters.HarnessAdapter
}

// PlanHook renders a hook fire event into a JobSpec.
func (p *DispatchPlanner) PlanHook(event *HookFireEvent) (*JobSpec, CleanupFunc, error) {
	if event == nil {
		return nil, nil, fmt.Errorf("hook event is required")
	}
	if event.Hook.ScriptPath == "" {
		return nil, nil, fmt.Errorf("hook %q: no script path resolved", event.Hook.ID)
	}

	meta, proj, task, err := p.loadContext(event.ProjectID, event.TaskID)
	if err != nil {
		return nil, nil, err
	}

	parent, err := p.lookupParent(task)
	if err != nil {
		return nil, nil, err
	}

	behavior, _ := lookupBehavior(meta, task)

	// Business payload filter: limit task.payload to the traits this hook declares.
	payload := FilterPayloadByTraits(task.Payload, event.Hook.Traits.Consumes)

	// 1 hook = 1 routed instruction. If multiple candidates match (same phase
	// and agent), take the first after filtering.
	instruction := selectInstruction(task, event.Hook.Agent)

	spec := &JobSpec{
		TaskID:      event.TaskID,
		ProjectID:   event.ProjectID,
		HandlerID:   event.Hook.ID,
		DisplayName: event.Hook.Name,
		Kind:        JobKindHook,
		Argv:         []string{event.Hook.ScriptPath},
		Instruction:  instruction,
		Task:         snapshotTask(task),
		PrimaryInput: payload,
		Visibility: Visibility{
			ProjectDir:         proj.WorkDir,
			UseWorktree:        task.Worktree,
			AdditionalBindings: behavior.AdditionalBindings,
			Writable:           !IsReadonly(task),
			KitRoots:           behavior.KitRoots,
			ForkPoint:          meta.ForkPoint,
			DockerEnabled:      meta.Capabilities.Docker != nil,
		},
		BuiltinPolicies: DefaultBuiltinPolicies(
			RoleHook,
			[]string{"boid", "git", "fetch"},
			PolicyContext{ProjectDir: proj.WorkDir, HomeDir: sandboxHomeDir()},
		),
		HostCommands: behavior.HostCommands.ToCommandDefs(),
		SecretNamespace: meta.SecretNamespace,
		Env:             mergeStringMaps(mergeStringMaps(behavior.Env, taskBusinessEnv(task, parent)), p.resumeEnv(task)),
		ExecutionState:  string(task.Status),
		// Hook jobs run an agent session that requires a real PTY. The
		// adapter decides whether a PTY is needed; the EXIT trap added by
		// dispatcher (Foreground=false) still fires `boid job done` on
		// process exit so the daemon learns when the agent finishes.
		Interactive: p.interactive(),
	}
	return spec, nil, nil
}

// ExecFireEvent carries the data needed to plan a boid-exec job.
// Command must be the fully resolved CommandSpec (ResolvedCommand populated).
type ExecFireEvent struct {
	ProjectID string
	Command   CommandSpec
}

// PlanExec renders an exec fire event into a JobSpec.
// Visibility.Writable is driven by Command.Readonly, mirroring how PlanHook
// derives it from IsReadonly(task) — task.readonly is the sole arbiter.
func (p *DispatchPlanner) PlanExec(event *ExecFireEvent) (*JobSpec, CleanupFunc, error) {
	if event == nil {
		return nil, nil, fmt.Errorf("exec event is required")
	}
	if len(event.Command.ResolvedCommand) == 0 {
		return nil, nil, fmt.Errorf("exec event: no command resolved")
	}
	if p.Meta == nil || p.Projects == nil {
		return nil, nil, fmt.Errorf("dispatch planner is not fully configured")
	}

	proj, err := p.Projects.GetProject(event.ProjectID)
	if err != nil {
		return nil, nil, fmt.Errorf("get project: %w", err)
	}

	var secretNS string
	var dockerEnabled bool
	if meta, ok := p.Meta.Get(event.ProjectID); ok {
		secretNS = meta.SecretNamespace
		dockerEnabled = meta.Capabilities.Docker != nil
	}

	spec := &JobSpec{
		ProjectID: event.ProjectID,
		Kind:      JobKindExec,
		Argv:      event.Command.ResolvedCommand,
		Visibility: Visibility{
			ProjectDir:         proj.WorkDir,
			UseWorktree:        false,
			AdditionalBindings: event.Command.AdditionalBindings,
			Writable:           !event.Command.Readonly,
			DockerEnabled:      dockerEnabled,
		},
		BuiltinPolicies: DefaultBuiltinPolicies(
			RoleHook,
			[]string{"boid", "git", "fetch"},
			PolicyContext{ProjectDir: proj.WorkDir, HomeDir: sandboxHomeDir()},
		),
		HostCommands:    event.Command.HostCommands.ToCommandDefs(),
		SecretNamespace: secretNS,
		Env:             event.Command.Env,
	}
	return spec, nil, nil
}

// lookupParent returns the parent task when task.ParentID is set, or nil for
// root tasks. Used to propagate BOID_PARENT_BRANCH into the job environment.
func (p *DispatchPlanner) lookupParent(task *Task) (*Task, error) {
	if task == nil || task.ParentID == "" || p.Tasks == nil {
		return nil, nil
	}
	parent, err := p.Tasks.GetTask(task.ParentID)
	if err != nil {
		return nil, fmt.Errorf("lookup parent task %q: %w", task.ParentID, err)
	}
	return parent, nil
}

// interactive returns whether the harness requires a PTY. Defaults to true
// (claude behaviour) when no adapter is configured.
func (p *DispatchPlanner) interactive() bool {
	if p.Adapter == nil {
		return true
	}
	return p.Adapter.Interactive()
}

// resumeEnv returns the harness-specific env vars to set when resuming an
// awaiting task (session ID key name is owned by the adapter). Returns nil
// when no adapter is configured or the task has no awaiting session ID.
func (p *DispatchPlanner) resumeEnv(task *Task) map[string]string {
	if p.Adapter == nil {
		return nil
	}
	ap := GetAwaitingPayload(task.Payload)
	if ap.SessionID == "" {
		return nil
	}
	_, env := p.Adapter.ResumePayload(ap.SessionID)
	return env
}

// taskBusinessEnv returns env vars derived from business-level task fields
// that hook scripts may need at runtime. Surfaces the task's base branch,
// the parent task's HEAD branch (BOID_PARENT_BRANCH), and, when the task has
// an awaiting trait, the user answer and question ID. The harness-specific
// session ID env var is produced separately via DispatchPlanner.resumeEnv.
//
// parent is nil for root tasks; when set, BOID_PARENT_BRANCH is emitted.
func taskBusinessEnv(task *Task, parent *Task) map[string]string {
	if task == nil {
		return nil
	}
	out := map[string]string{}
	if task.BaseBranch != "" {
		out["BOID_BASE_BRANCH"] = task.BaseBranch
	}
	if parent != nil {
		if pb := ComputeHeadBranch(parent); pb != "" {
			out["BOID_PARENT_BRANCH"] = pb
		}
	}
	ap := GetAwaitingPayload(task.Payload)
	if ap.PendingAnswer != "" {
		out["BOID_USER_ANSWER"] = ap.PendingAnswer
	}
	if ap.QuestionID != "" {
		out["BOID_QUESTION_ID"] = ap.QuestionID
	}
	if len(out) == 0 {
		return nil
	}
	return out
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

func selectInstruction(task *Task, agent string) *RoutedInstruction {
	// Instructions only drive dispatch while the task is executing; other
	// statuses carry no live instruction. This guard makes that explicit —
	// it was previously enforced indirectly through the instruction phase.
	if task.Status != TaskStatusExecuting {
		return nil
	}
	routed := FilterInstructions(task.Instructions, agent)
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
