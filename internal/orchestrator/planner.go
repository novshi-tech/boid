package orchestrator

import (
	"context"
	"fmt"

	"github.com/novshi-tech/boid/internal/adapters"
)

type MetaCache interface {
	Get(id string) (*ProjectMeta, bool)
}

// MetaHydrator extends MetaCache with workspace-aware hydration. When a
// ProjectStore has a WorkspaceStore configured, it implements this interface
// and returns a ProjectMeta enriched with workspace capabilities/kits/env.
type MetaHydrator interface {
	GetWithWorkspace(ctx context.Context, projectID string) (*ProjectMeta, error)
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
	Hydrator MetaHydrator // optional; when set, loadContext uses GetWithWorkspace instead of Get
	Projects ProjectCatalog
	Tasks    TaskLookup
	Adapter  adapters.HarnessAdapter
}

// PlanHook renders a hook fire event into a JobSpec.
//
// Agent-kind hooks (Hook.Kind == HandlerKindAgent) may omit ScriptPath — the
// HarnessAdapter builds its own argv from CLI conventions, so an empty Argv
// flows through fine. Non-agent hooks (shell-bound) still require a resolved
// script path. The Evaluator may synthesize a script-less agent hook when the
// behavior declares none of its own (Phase 3-e kit-retirement fallback); the
// relaxed validation here is what makes those virtual hooks dispatch-ready.
func (p *DispatchPlanner) PlanHook(event *HookFireEvent) (*JobSpec, CleanupFunc, error) {
	if event == nil {
		return nil, nil, fmt.Errorf("hook event is required")
	}
	if event.Hook.ScriptPath == "" && event.Hook.Kind != HandlerKindAgent {
		return nil, nil, fmt.Errorf("hook %q: no script path resolved", event.Hook.ID)
	}

	meta, proj, task, err := p.loadContext(event.ProjectID, event.TaskID)
	if err != nil {
		return nil, nil, err
	}

	behavior, _ := lookupBehavior(meta, task)

	// Business payload filter: limit task.payload to the traits this hook declares.
	payload := FilterPayloadByTraits(task.Payload, event.Hook.Traits.Consumes)

	// 1 hook = 1 routed instruction. If multiple candidates match (same phase
	// and agent), take the first after filtering.
	instruction := selectInstruction(task, event.Hook.Agent)

	// Phase 3-d: every hook flows through a HarnessAdapter. The mapping
	// resolves recognised agents (claude-code / codex / opencode) to their
	// dedicated adapter; everything else — including hooks with no `agent:`
	// declaration and instruction-less hooks — falls through to the shell
	// adapter, which forwards the hook script's argv straight to exec.
	harnessType := harnessTypeForAgent(event.Hook.Agent)

	var argv []string
	if event.Hook.ScriptPath != "" {
		argv = []string{event.Hook.ScriptPath}
	}

	spec := &JobSpec{
		TaskID:       event.TaskID,
		ProjectID:    event.ProjectID,
		HandlerID:    event.Hook.ID,
		DisplayName:  event.Hook.Name,
		Kind:         JobKindHook,
		HarnessType:  harnessType,
		Argv:         argv,
		Instruction:  instruction,
		Task:         snapshotTask(task),
		PrimaryInput: payload,
		Visibility: Visibility{
			ProjectDir:         proj.WorkDir,
			ProjectName:        meta.Name,
			AdditionalBindings: behavior.AdditionalBindings,
			Writable:           !IsReadonly(task),
			KitRoots:           behavior.KitRoots,
			DockerEnabled:      meta.Capabilities.Docker != nil,
			// docs/plans/git-gateway-cutover.md PR6 cutover: dispatcher no
			// longer resolves a host-repo worktree, it clones inside the
			// sandbox and resolves the declared branch there.
			Clone: BuildCloneDeclaration(task, meta.ForkPoint),
		},
		BuiltinPolicies: DefaultBuiltinPolicies(
			RoleHook,
			[]string{"boid", "fetch"},
			PolicyContext{ProjectDir: proj.WorkDir, HomeDir: sandboxHomeDir()},
		),
		HostCommands:    behavior.HostCommands.ToCommandDefs(),
		SecretNamespace: meta.SecretNamespace,
		Env:             mergeStringMaps(behavior.Env, taskBusinessEnv(task)),
		ExecutionState:  string(task.Status),
		// All hook jobs allocate a PTY: agent hooks (HarnessType="claude")
		// need it so the harness' TUI behaves correctly, and pure shell hooks
		// rely on it for live stdout streaming to the Web UI's WebSocket
		// attach endpoint (see e2e/scenarios/hook-attach-smoke). Phase 3-c
		// can revisit per-harness PTY hints if a non-PTY harness lands.
		Interactive: true,
	}
	return spec, nil, nil
}

// taskBusinessEnv returns env vars derived from business-level task fields
// that hook scripts may need at runtime. Surfaces the task's base branch
// and, when the task has an awaiting trait, the user answer and question ID.
//
// Note: session-id resume has been removed (task-ask-rpc / reopen session id
// removal). Every dispatch is a fresh agent process; harness-specific session
// resume env vars are no longer surfaced.
//
// BOID_PARENT_BRANCH was removed in
// docs/plans/branch-policy-simplification.md Phase 1: the per-task
// "boid/<id8>" branch it exposed no longer exists, and grep across
// production project.yaml / e2e scripts found zero real use of the env var
// (nose 2026-07-15 decision) — so rather than redefine it to parent.BaseBranch
// (a weaker, largely redundant signal now that clone-mode child tasks share
// no branch machinery with their parent), it is dropped entirely.
func taskBusinessEnv(task *Task) map[string]string {
	if task == nil {
		return nil
	}
	out := map[string]string{}
	if task.BaseBranch != "" {
		out["BOID_BASE_BRANCH"] = task.BaseBranch
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

	var meta *ProjectMeta
	if p.Hydrator != nil {
		var err error
		meta, err = p.Hydrator.GetWithWorkspace(context.Background(), projectID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("project %q: hydrate meta: %w", projectID, err)
		}
	} else {
		ok := false
		meta, ok = p.Meta.Get(projectID)
		if !ok {
			return nil, nil, nil, fmt.Errorf("project %q: meta not loaded", projectID)
		}
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

// harnessTypeForAgent maps a hook's agent declaration (project.yaml
// `hooks: agent: ...`) to the HarnessType the runner should hand it off to.
// Phase 3-d makes the resolver total: anything that does not match a known
// agent (including an empty string when no `agent:` is set) resolves to
// "shell" so the hook still runs under the adapter pipeline. The mapping
// must stay in sync with the runner-inner-child adapter switch in
// internal/sandbox/runner/runner_linux.go.
func harnessTypeForAgent(agent string) string {
	switch agent {
	case "claude-code":
		return "claude"
	case "codex":
		return "codex"
	case "opencode":
		return "opencode"
	default:
		return "shell"
	}
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
