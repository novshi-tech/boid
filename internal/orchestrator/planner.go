package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// collectHookFiles builds the list of hook files to bind-mount into the sandbox.
// Kit hooks are prefixed with "{consumer}--". Project hooks are added independently
// and cannot override kit hooks.
func collectHookFiles(projectHooksDir string, kitHooksDirs []KitHooksInfo) []HookFile {
	files := make(map[string]HookFile)

	for _, info := range kitHooksDirs {
		entries, err := os.ReadDir(info.HooksDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			ext := filepath.Ext(e.Name())
			if ext != ".sh" && ext != ".py" {
				continue
			}
			targetName := e.Name()
			if info.Consumer != "" {
				targetName = info.Consumer + "--" + e.Name()
			}
			files[targetName] = HookFile{
				Source:     filepath.Join(info.HooksDir, e.Name()),
				TargetName: targetName,
			}
		}
	}

	entries, err := os.ReadDir(projectHooksDir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			ext := filepath.Ext(e.Name())
			if ext != ".sh" && ext != ".py" {
				continue
			}
			if _, exists := files[e.Name()]; !exists {
				files[e.Name()] = HookFile{
					Source:     filepath.Join(projectHooksDir, e.Name()),
					TargetName: e.Name(),
				}
			}
		}
	}

	if len(files) == 0 {
		return nil
	}
	out := make([]HookFile, 0, len(files))
	for _, hf := range files {
		out = append(out, hf)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].TargetName < out[j].TargetName
	})
	return out
}

type MetaCache interface {
	Get(id string) (*ProjectMeta, bool)
}

type ProjectCatalog interface {
	GetProject(id string) (*Project, error)
	ListProjects() ([]*Project, error)
}

type TaskLookup interface {
	GetTask(id string) (*Task, error)
}

type WorktreePreparer interface {
	Prepare(task *Task, proj *Project) (string, error)
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

func (p *DispatchPlanner) PlanHook(event *HookFireEvent) (*DispatchRequest, error) {
	if event == nil {
		return nil, fmt.Errorf("hook event is required")
	}

	hookBase := filepath.Base(event.Hook.ScriptPath)
	if hookBase == "" || hookBase == "." {
		return nil, fmt.Errorf("hook %q: no script path resolved", event.Hook.ID)
	}
	hookFilename := hookBase
	if event.Hook.Kit != "" {
		hookFilename = event.Hook.Kit + "--" + hookBase
	}

	meta, proj, task, err := p.loadContext(event.ProjectID, event.TaskID)
	if err != nil {
		return nil, err
	}

	projectHooksDir := filepath.Join(proj.WorkDir, ".boid", "hooks")
	hookFiles := collectHookFiles(projectHooksDir, meta.KitHooksDirs)

	workspaceDirs, err := p.collectWorkspaceDirs(proj.WorkspaceID, event.ProjectID)
	if err != nil {
		return nil, err
	}
	worktreeDir, err := p.prepareWorktree(task, proj)
	if err != nil {
		return nil, err
	}

	payloadJSON := string(FilterPayloadByTraits(task.Payload, event.Hook.Traits.Consumes))

	var instructionsJSON string
	var interactive bool
	var model string
	instType := InstructionTypeForStatus(task.Status)
	myInstructions := FilterInstructions(task.Payload, instType, event.Hook.Consumer)
	if len(myInstructions) > 0 {
		if instJSON, err := json.Marshal(myInstructions); err == nil {
			instructionsJSON = string(instJSON)
		}
		interactive = myInstructions[0].Interactive
		model = myInstructions[0].Model
	}

	readonly := IsReadonly(task)
	taskYAML := buildTaskYAML(task)
	environmentYAML := buildEnvironmentYAML(readonly, worktreeDir != "", p.proxyPort() > 0, workspaceDirs, meta.BuiltinCommands)

	homeDir, _ := os.UserHomeDir()
	return &DispatchRequest{
		TaskID:             event.TaskID,
		ProjectID:          event.ProjectID,
		WorkspaceID:        proj.WorkspaceID,
		HandlerID:          event.Hook.ID,
		Role:               RoleHook,
		ProjectDir:         proj.WorkDir,
		HomeDir:            homeDir,
		HookFiles:          hookFiles,
		HookScript:         hookFilename,
		BoidBinary:         p.BoidBinary,
		ServerSocket:       p.ServerSocket,
		Env:                meta.Env,
		BuiltinPolicies:    DefaultBuiltinPolicies(RoleHook, mergeBuiltinCommands(meta.BuiltinCommands, []string{"boid"})),
		HostCommands:       nil,
		AdditionalBindings: meta.AdditionalBindings,
		SecretNamespace:    meta.SecretNamespace,
		WorkspaceDirs:      workspaceDirs,
		ProxyPort:          p.proxyPort(),
		WorktreeDir:        worktreeDir,
		PayloadJSON:        payloadJSON,
		Readonly:           readonly,
		Interactive:        interactive,
		InstructionsJSON:   instructionsJSON,
		TaskYAML:           taskYAML,
		EnvironmentYAML:    environmentYAML,
		Model:              model,
	}, nil
}

func (p *DispatchPlanner) PlanGate(event *GateFireEvent) (*DispatchRequest, error) {
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

	workspaceDirs, err := p.collectWorkspaceDirs(proj.WorkspaceID, event.ProjectID)
	if err != nil {
		return nil, err
	}

	projectGatesDir := filepath.Join(proj.WorkDir, ".boid", "gates")
	gatesDir := filepath.Dir(event.Gate.ScriptPath)
	var stagingDir string
	if len(meta.KitGatesDirs) > 0 {
		staged, _, err := StageGates(projectGatesDir, meta.KitGatesDirs, event.TaskID)
		if err != nil {
			return nil, fmt.Errorf("stage gates: %w", err)
		}
		gatesDir = staged
		stagingDir = staged
	}

	// Use hook-updated payload if provided. This value is the result of merging all
	// hook patches into the original payload inside DispatchAndAdvance, and may not
	// have been persisted to DB yet. It is always a superset of the DB payload at
	// this point in the dispatch cycle, so a direct replace (not merge) is correct.
	if event.TaskPayloadJSON != "" {
		task.Payload = json.RawMessage(event.TaskPayloadJSON)
	}

	taskJSON, err := json.Marshal(task)
	if err != nil {
		return nil, fmt.Errorf("marshal task: %w", err)
	}

	hostCommands := meta.HostCommands.ToCommandDefs()

	return &DispatchRequest{
		TaskID:          event.TaskID,
		ProjectID:       event.ProjectID,
		WorkspaceID:     proj.WorkspaceID,
		HandlerID:       event.Gate.ID,
		Role:            RoleGate,
		ProjectDir:      proj.WorkDir,
		GatesDir:        gatesDir,
		HookScript:      gateFilename,
		BoidBinary:      p.BoidBinary,
		ServerSocket:    p.ServerSocket,
		Env:             meta.Env,
		BuiltinPolicies: DefaultBuiltinPolicies(RoleGate, mergeBuiltinCommands(meta.BuiltinCommands, []string{"boid"})),
		HostCommands:    hostCommands,
		SecretNamespace: meta.SecretNamespace,
		WorkspaceDirs:   workspaceDirs,
		ProxyPort:       p.proxyPort(),
		StagingDir:      stagingDir,
		TaskJSON:        string(taskJSON),
	}, nil
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
		if candidate.WorkspaceID != workspaceID {
			continue
		}
		dirs[candidate.ID] = candidate.WorkDir
	}
	if len(dirs) == 0 {
		return nil, nil
	}
	return dirs, nil
}

func (p *DispatchPlanner) prepareWorktree(task *Task, proj *Project) (string, error) {
	if p.Worktrees == nil || !task.Worktree {
		return "", nil
	}
	worktreeDir, err := p.Worktrees.Prepare(task, proj)
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

func cloneStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

// buildTaskYAML serializes task metadata for context/task.yaml.
func buildTaskYAML(task *Task) string {
	m := map[string]string{
		"id":       task.ID,
		"title":    task.Title,
		"status":   string(task.Status),
		"behavior": task.Behavior,
	}
	if task.Description != "" {
		m["description"] = task.Description
	}
	out, _ := yaml.Marshal(m)
	return string(out)
}

type workspaceProject struct {
	Path string `yaml:"path"`
	Name string `yaml:"name"`
}

type environmentData struct {
	Readonly          bool               `yaml:"readonly"`
	Worktree          bool               `yaml:"worktree"`
	Network           map[string]bool    `yaml:"network"`
	Tools             []string           `yaml:"tools,omitempty"`
	WorkspaceProjects []workspaceProject `yaml:"workspace_projects,omitempty"`
}

// buildEnvironmentYAML serializes sandbox constraints for context/environment.yaml.
func buildEnvironmentYAML(readonly, worktree, networkRestricted bool, workspaceDirs map[string]string, builtinCommands []string) string {
	env := environmentData{
		Readonly: readonly,
		Worktree: worktree,
		Network:  map[string]bool{"restricted": networkRestricted},
		Tools:    builtinTools(builtinCommands),
	}

	if len(workspaceDirs) > 0 {
		// Sort for deterministic output
		ids := make([]string, 0, len(workspaceDirs))
		for id := range workspaceDirs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			dir := workspaceDirs[id]
			env.WorkspaceProjects = append(env.WorkspaceProjects, workspaceProject{
				Path: dir,
				Name: filepath.Base(dir),
			})
		}
	}

	out, _ := yaml.Marshal(env)
	return string(out)
}

// builtinTools returns the list of tools available in the sandbox.
// Always includes "git"; adds other builtin commands that are not internal.
func builtinTools(builtinCommands []string) []string {
	tools := []string{"git"}
	for _, cmd := range builtinCommands {
		if cmd == "boid" || cmd == "git" {
			continue
		}
		tools = append(tools, cmd)
	}
	return tools
}
