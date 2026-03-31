package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/kit"
	"github.com/novshi-tech/boid/internal/projectspec"
)

type MetaCache interface {
	Get(id string) (*projectspec.ProjectMeta, bool)
}

type ProjectCatalog interface {
	GetProject(id string) (*projectspec.Project, error)
	ListProjects() ([]*projectspec.Project, error)
}

type TaskLookup interface {
	GetTask(id string) (*Task, error)
}

type WorktreePreparer interface {
	Prepare(task *Task, proj *projectspec.Project, behavior *projectspec.TaskBehavior) (string, error)
}

type DispatchPlanner struct {
	Meta         MetaCache
	Projects     ProjectCatalog
	Tasks        TaskLookup
	Worktrees    WorktreePreparer
	BoidBinary   string
	ServerSocket string
	ProxyPort    *int
}

func (p *DispatchPlanner) PlanHook(event *projectspec.HookFireEvent) (*dispatcher.DispatchPlan, error) {
	if event == nil {
		return nil, fmt.Errorf("hook event is required")
	}

	hookFilename := filepath.Base(event.Hook.ScriptPath)
	if hookFilename == "" || hookFilename == "." {
		return nil, fmt.Errorf("hook %q: no script path resolved", event.Hook.ID)
	}

	meta, proj, task, err := p.loadContext(event.ProjectID, event.TaskID)
	if err != nil {
		return nil, err
	}

	projectHooksDir := filepath.Join(proj.WorkDir, ".boid", "hooks")
	hooksDir := projectHooksDir
	var stagingDir string
	if len(meta.KitHooksDirs) > 0 {
		staged, _, err := kit.StageHooks(projectHooksDir, meta.KitHooksDirs, event.TaskID)
		if err != nil {
			return nil, fmt.Errorf("stage hooks: %w", err)
		}
		hooksDir = staged
		stagingDir = staged
	}

	behavior, _ := meta.TaskBehaviors[task.Behavior]
	workspaceDirs, err := p.collectWorkspaceDirs(meta.WorkspaceID, event.ProjectID)
	if err != nil {
		return nil, err
	}
	worktreeDir, err := p.prepareWorktree(task, proj, &behavior)
	if err != nil {
		return nil, err
	}

	payloadJSON := string(task.Payload)
	if payloadJSON == "" {
		payloadJSON = "{}"
	}

	homeDir, _ := os.UserHomeDir()
	return &dispatcher.DispatchPlan{
		TaskID:             event.TaskID,
		ProjectID:          event.ProjectID,
		HandlerID:          event.Hook.ID,
		Role:               string(projectspec.RoleHook),
		ProjectDir:         proj.WorkDir,
		HomeDir:            homeDir,
		HooksDir:           hooksDir,
		HookScript:         hookFilename,
		BoidBinary:         p.BoidBinary,
		ServerSocket:       p.ServerSocket,
		Env:                meta.Env,
		HostCommands:       map[string]dispatcher.CommandDef{"boid": {Name: "boid"}},
		AdditionalBindings: toDispatcherBindings(meta.AdditionalBindings),
		WorkspaceDirs:      workspaceDirs,
		ProxyPort:          p.proxyPort(),
		StagingDir:         stagingDir,
		WorktreeDir:        worktreeDir,
		PayloadJSON:        payloadJSON,
		Readonly:           IsReadonly(&behavior, task.Status),
	}, nil
}

func (p *DispatchPlanner) PlanGate(event *projectspec.GateFireEvent) (*dispatcher.DispatchPlan, error) {
	if event == nil {
		return nil, fmt.Errorf("gate event is required")
	}

	gateFilename := filepath.Base(event.Gate.ScriptPath)
	if gateFilename == "" || gateFilename == "." {
		return nil, fmt.Errorf("gate %q: no script path resolved", event.Gate.ID)
	}

	meta, proj, task, err := p.loadContext(event.ProjectID, event.TaskID)
	if err != nil {
		return nil, err
	}

	taskJSON, err := json.Marshal(task)
	if err != nil {
		return nil, fmt.Errorf("marshal task: %w", err)
	}

	hostCommands := toDispatcherCommands(meta.HostCommands)
	hostCommands["boid"] = dispatcher.CommandDef{Name: "boid"}

	return &dispatcher.DispatchPlan{
		TaskID:       event.TaskID,
		ProjectID:    event.ProjectID,
		HandlerID:    event.Gate.ID,
		Role:         string(projectspec.RoleGate),
		ProjectDir:   proj.WorkDir,
		HookScript:   gateFilename,
		BoidBinary:   p.BoidBinary,
		ServerSocket: p.ServerSocket,
		Env:          meta.Env,
		HostCommands: hostCommands,
		ProxyPort:    p.proxyPort(),
		TaskJSON:     string(taskJSON),
	}, nil
}

func (p *DispatchPlanner) loadContext(projectID, taskID string) (*projectspec.ProjectMeta, *projectspec.Project, *Task, error) {
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

func (p *DispatchPlanner) collectWorkspaceDirs(workspaceID, selfID string) (map[string]string, error) {
	if workspaceID == "" {
		return nil, nil
	}
	projects, err := p.Projects.ListProjects()
	if err != nil {
		return nil, fmt.Errorf("list projects for workspace: %w", err)
	}

	dirs := make(map[string]string)
	for _, candidate := range projects {
		if candidate.ID == selfID {
			continue
		}
		meta, ok := p.Meta.Get(candidate.ID)
		if !ok || meta.WorkspaceID != workspaceID {
			continue
		}
		dirs[candidate.ID] = candidate.WorkDir
	}
	if len(dirs) == 0 {
		return nil, nil
	}
	return dirs, nil
}

func (p *DispatchPlanner) prepareWorktree(task *Task, proj *projectspec.Project, behavior *projectspec.TaskBehavior) (string, error) {
	if p.Worktrees == nil || behavior == nil || !behavior.Worktree {
		return "", nil
	}
	worktreeDir, err := p.Worktrees.Prepare(task, proj, behavior)
	if err != nil {
		return "", fmt.Errorf("resolve worktree: %w", err)
	}
	return worktreeDir, nil
}

func (p *DispatchPlanner) proxyPort() int {
	if p.ProxyPort == nil {
		return 0
	}
	return *p.ProxyPort
}

func toDispatcherBindings(bindings []projectspec.BindMount) []dispatcher.BindMount {
	if len(bindings) == 0 {
		return nil
	}
	out := make([]dispatcher.BindMount, 0, len(bindings))
	for _, binding := range bindings {
		out = append(out, dispatcher.BindMount{
			Source: binding.Source,
			Mode:   binding.Mode,
		})
	}
	return out
}

func toDispatcherCommands(cmds map[string]projectspec.CommandDef) map[string]dispatcher.CommandDef {
	if len(cmds) == 0 {
		return nil
	}
	out := make(map[string]dispatcher.CommandDef, len(cmds))
	for name, def := range cmds {
		out[name] = dispatcher.CommandDef(def)
	}
	return out
}
