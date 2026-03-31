package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/novshi-tech/boid/internal/db"
	dtmux "github.com/novshi-tech/boid/internal/dispatcher/tmux"
	"github.com/novshi-tech/boid/internal/hostcmd"
	"github.com/novshi-tech/boid/internal/kit"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/project"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/secret"
	"github.com/novshi-tech/boid/internal/worktree"
)

type Runner struct {
	DB           *db.DB
	Meta         MetaCache
	Tmux         dtmux.TmuxManager
	TmuxSession  string            // defaults to "boid"
	BoidBinary   string            // host-side path to boid binary
	ServerSocket string            // host-side server socket path
	ProxyPort    *int              // pointer to server's proxy port (populated after Start)
	Broker       *hostcmd.Broker   // host command broker
	SecretStore  *secret.Store     // secret store for resolving secret: env values
	WorktreeMgr  *worktree.Manager // optional worktree manager
	tokenMu      sync.Mutex
	jobTokens    map[string]string // job ID -> broker token
	waiterMu     sync.Mutex
	jobWaiters   map[string]chan JobCompletionResult // job ID -> completion channel
}

func (r *Runner) session() string {
	if r.TmuxSession != "" {
		return r.TmuxSession
	}
	return "boid"
}

// collectWorkspaceDirs returns the WorkDir of peer projects sharing the same workspace.
func (r *Runner) collectWorkspaceDirs(workspaceID, selfID string) map[string]string {
	if workspaceID == "" {
		return nil
	}
	projects, err := project.ListProjects(r.DB.Conn)
	if err != nil {
		slog.Warn("list projects for workspace", "error", err)
		return nil
	}
	dirs := make(map[string]string)
	for _, p := range projects {
		if p.ID == selfID {
			continue
		}
		m, ok := r.Meta.Get(p.ID)
		if !ok || m.WorkspaceID != workspaceID {
			continue
		}
		dirs[p.ID] = p.WorkDir
	}
	if len(dirs) == 0 {
		return nil
	}
	return dirs
}

// Execute creates a job and runs the hook script in a sandboxed tmux window.
func (r *Runner) Execute(ctx context.Context, event *project.HookFireEvent) error {
	slog.Info("executing hook", "hook_id", event.Hook.ID, "task_id", event.TaskID)

	hookFilename := filepath.Base(event.Hook.ScriptPath)
	if hookFilename == "" || hookFilename == "." {
		return fmt.Errorf("hook %q: no script path resolved", event.Hook.ID)
	}

	meta, ok := r.Meta.Get(event.ProjectID)
	if !ok {
		return fmt.Errorf("project %q: meta not loaded", event.ProjectID)
	}

	proj, err := project.GetProject(r.DB.Conn, event.ProjectID)
	if err != nil {
		return fmt.Errorf("get project: %w", err)
	}

	j := &Job{
		TaskID:    event.TaskID,
		ProjectID: event.ProjectID,
		HandlerID: event.Hook.ID,
		Role:      string(project.RoleHook),
	}

	if err := CreateJob(r.DB.Conn, j); err != nil {
		return fmt.Errorf("create job: %w", err)
	}

	projectHooksDir := filepath.Join(proj.WorkDir, ".boid", "hooks")
	hooksDir := projectHooksDir
	var stagingDir string

	if len(meta.KitHooksDirs) > 0 {
		staged, _, err := kit.StageHooks(projectHooksDir, meta.KitHooksDirs, j.ID)
		if err != nil {
			return fmt.Errorf("stage hooks: %w", err)
		}
		hooksDir = staged
		stagingDir = staged
	}

	workspaceDirs := r.collectWorkspaceDirs(meta.WorkspaceID, event.ProjectID)

	var proxyPort int
	if r.ProxyPort != nil {
		proxyPort = *r.ProxyPort
	}

	var brokerSocket, brokerToken string
	if r.Broker != nil && len(meta.HostCommands) > 0 {
		tokenCtx := hostcmd.TokenContext{
			JobID:     j.ID,
			TaskID:    event.TaskID,
			ProjectID: event.ProjectID,
			Role:      string(project.RoleHook),
		}
		if r.SecretStore != nil {
			brokerToken = r.Broker.RegisterWithSecrets(hostCommandDefs(meta.HostCommands), tokenCtx, r.SecretStore.Get)
		} else {
			brokerToken = r.Broker.Register(hostCommandDefs(meta.HostCommands), tokenCtx)
		}
		brokerSocket = r.Broker.SocketPath
		r.trackToken(j.ID, brokerToken)
	}

	homeDir, _ := os.UserHomeDir()

	var worktreeDir string
	if r.WorktreeMgr != nil {
		worktreeDir, err = r.resolveWorktree(event, meta, proj)
		if err != nil {
			return fmt.Errorf("resolve worktree: %w", err)
		}
	}

	cfg := sandbox.WrapperConfig{
		JobID:              j.ID,
		TaskID:             event.TaskID,
		ProjectID:          meta.ID,
		ProjectDir:         proj.WorkDir,
		HomeDir:            homeDir,
		HooksDir:           hooksDir,
		HookScript:         hookFilename,
		BoidBinary:         r.BoidBinary,
		ServerSocket:       r.ServerSocket,
		BrokerSocket:       brokerSocket,
		BrokerToken:        brokerToken,
		Env:                meta.Env,
		HostCommands:       hostCommandNames(meta.HostCommands),
		AdditionalBindings: meta.AdditionalBindings,
		WorkspaceDirs:      workspaceDirs,
		ProxyPort:          proxyPort,
		StagingDir:         stagingDir,
		WorktreeDir:        worktreeDir,
	}

	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		return fmt.Errorf("write sandbox scripts: %w", err)
	}

	session := r.session()
	windowName := fmt.Sprintf("hook-%s-%s", event.TaskID[:8], event.Hook.ID)

	if r.Tmux != nil {
		if err := r.Tmux.EnsureSession(session); err != nil {
			return fmt.Errorf("ensure session: %w", err)
		}

		cmd := fmt.Sprintf("bash %s", outerPath)
		if err := r.Tmux.RunInWindow(session, windowName, cmd); err != nil {
			return fmt.Errorf("run in window: %w", err)
		}
	}

	slog.Info("job started", "job_id", j.ID, "window", windowName)
	return nil
}

// resolveWorktree checks if the task's behavior enables worktree isolation.
func (r *Runner) resolveWorktree(event *project.HookFireEvent, meta *project.ProjectMeta, proj *project.Project) (string, error) {
	task, err := orchestrator.GetTask(r.DB.Conn, event.TaskID)
	if err != nil {
		return "", fmt.Errorf("get task: %w", err)
	}

	behavior, ok := meta.TaskBehaviors[task.Behavior]
	if !ok || !behavior.Worktree {
		return "", nil
	}

	existing, err := r.WorktreeMgr.Get(event.TaskID)
	if err != nil {
		return "", fmt.Errorf("get worktree: %w", err)
	}
	if existing != nil && existing.CleanedAt == nil {
		return existing.Path, nil
	}

	w, err := r.WorktreeMgr.Create(
		proj.WorkDir,
		event.ProjectID,
		event.TaskID,
		behavior.BranchPrefix,
		behavior.BaseBranch,
	)
	if err != nil {
		return "", err
	}
	return w.Path, nil
}

func hostCommandNames(cmds map[string]project.CommandDef) []string {
	names := make([]string, 0, len(cmds))
	for name := range cmds {
		names = append(names, name)
	}
	return names
}

func hostCommandDefs(cmds map[string]project.CommandDef) map[string]hostcmd.CommandDef {
	if len(cmds) == 0 {
		return nil
	}
	out := make(map[string]hostcmd.CommandDef, len(cmds))
	for name, def := range cmds {
		out[name] = hostcmd.CommandDef(def)
	}
	return out
}

func (r *Runner) trackToken(jobID, token string) {
	r.tokenMu.Lock()
	defer r.tokenMu.Unlock()
	if r.jobTokens == nil {
		r.jobTokens = make(map[string]string)
	}
	r.jobTokens[jobID] = token
}

// WaitForJob registers a channel that will receive the job completion result.
func (r *Runner) WaitForJob(jobID string) <-chan JobCompletionResult {
	r.waiterMu.Lock()
	defer r.waiterMu.Unlock()
	if r.jobWaiters == nil {
		r.jobWaiters = make(map[string]chan JobCompletionResult)
	}
	ch := make(chan JobCompletionResult, 1)
	r.jobWaiters[jobID] = ch
	return ch
}

// CompleteJob signals the waiting dispatcher that a job has completed.
func (r *Runner) CompleteJob(jobID string, result JobCompletionResult) {
	r.waiterMu.Lock()
	ch, ok := r.jobWaiters[jobID]
	if ok {
		delete(r.jobWaiters, jobID)
	}
	r.waiterMu.Unlock()

	if ok {
		ch <- result
	}
}

// ExecuteHook implements orchestrator.HookExecutor.
func (r *Runner) ExecuteHook(ctx context.Context, event *project.HookFireEvent) (string, error) {
	slog.Info("executing hook (advanced)", "hook_id", event.Hook.ID, "task_id", event.TaskID)

	hookFilename := filepath.Base(event.Hook.ScriptPath)
	if hookFilename == "" || hookFilename == "." {
		return "", fmt.Errorf("hook %q: no script path resolved", event.Hook.ID)
	}

	meta, ok := r.Meta.Get(event.ProjectID)
	if !ok {
		return "", fmt.Errorf("project %q: meta not loaded", event.ProjectID)
	}
	proj, err := project.GetProject(r.DB.Conn, event.ProjectID)
	if err != nil {
		return "", fmt.Errorf("get project: %w", err)
	}

	task, err := orchestrator.GetTask(r.DB.Conn, event.TaskID)
	if err != nil {
		return "", fmt.Errorf("get task: %w", err)
	}

	j := &Job{
		TaskID:    event.TaskID,
		ProjectID: event.ProjectID,
		HandlerID: event.Hook.ID,
		Role:      string(project.RoleHook),
	}
	if err := CreateJob(r.DB.Conn, j); err != nil {
		return "", fmt.Errorf("create job: %w", err)
	}

	projectHooksDir := filepath.Join(proj.WorkDir, ".boid", "hooks")
	hooksDir := projectHooksDir
	var stagingDir string
	if len(meta.KitHooksDirs) > 0 {
		staged, _, err := kit.StageHooks(projectHooksDir, meta.KitHooksDirs, j.ID)
		if err != nil {
			return "", fmt.Errorf("stage hooks: %w", err)
		}
		hooksDir = staged
		stagingDir = staged
	}

	workspaceDirs := r.collectWorkspaceDirs(meta.WorkspaceID, event.ProjectID)

	var proxyPort int
	if r.ProxyPort != nil {
		proxyPort = *r.ProxyPort
	}

	var brokerSocket, brokerToken string
	if r.Broker != nil {
		tokenCtx := hostcmd.TokenContext{
			JobID: j.ID, TaskID: event.TaskID, ProjectID: event.ProjectID,
			Role: string(project.RoleHook),
		}
		r.Broker.Register(nil, tokenCtx) // no project commands for hooks
		brokerToken = r.Broker.Register(nil, tokenCtx)
		brokerSocket = r.Broker.SocketPath
		r.trackToken(j.ID, brokerToken)
	}

	behavior, _ := meta.TaskBehaviors[task.Behavior]
	readonly := behavior.Readonly ||
		task.Status == orchestrator.TaskStatusVerifying ||
		task.Status == orchestrator.TaskStatusInReview

	homeDir, _ := os.UserHomeDir()
	var worktreeDir string
	if r.WorktreeMgr != nil {
		worktreeDir, _ = r.resolveWorktree(event, meta, proj)
	}

	payloadJSON := string(task.Payload)
	if payloadJSON == "" {
		payloadJSON = "{}"
	}

	cfg := sandbox.WrapperConfig{
		JobID:              j.ID,
		TaskID:             event.TaskID,
		ProjectID:          meta.ID,
		ProjectDir:         proj.WorkDir,
		HomeDir:            homeDir,
		HooksDir:           hooksDir,
		HookScript:         hookFilename,
		BoidBinary:         r.BoidBinary,
		ServerSocket:       r.ServerSocket,
		BrokerSocket:       brokerSocket,
		BrokerToken:        brokerToken,
		Env:                meta.Env,
		HostCommands:       []string{"boid"},
		AdditionalBindings: meta.AdditionalBindings,
		WorkspaceDirs:      workspaceDirs,
		ProxyPort:          proxyPort,
		StagingDir:         stagingDir,
		WorktreeDir:        worktreeDir,
		Role:               "hook",
		PayloadJSON:        payloadJSON,
		Readonly:           readonly,
	}

	return r.launchSandbox(j.ID, event.TaskID, event.Hook.ID, cfg)
}

// ExecuteGate implements orchestrator.GateExecutor.
func (r *Runner) ExecuteGate(ctx context.Context, event *project.GateFireEvent) (string, error) {
	slog.Info("executing gate", "gate_id", event.Gate.ID, "task_id", event.TaskID)

	gateFilename := filepath.Base(event.Gate.ScriptPath)
	if gateFilename == "" || gateFilename == "." {
		return "", fmt.Errorf("gate %q: no script path resolved", event.Gate.ID)
	}

	meta, ok := r.Meta.Get(event.ProjectID)
	if !ok {
		return "", fmt.Errorf("project %q: meta not loaded", event.ProjectID)
	}
	proj, err := project.GetProject(r.DB.Conn, event.ProjectID)
	if err != nil {
		return "", fmt.Errorf("get project: %w", err)
	}

	task, err := orchestrator.GetTask(r.DB.Conn, event.TaskID)
	if err != nil {
		return "", fmt.Errorf("get task: %w", err)
	}

	j := &Job{
		TaskID:    event.TaskID,
		ProjectID: event.ProjectID,
		HandlerID: event.Gate.ID,
		Role:      string(project.RoleGate),
	}
	if err := CreateJob(r.DB.Conn, j); err != nil {
		return "", fmt.Errorf("create job: %w", err)
	}

	var proxyPort int
	if r.ProxyPort != nil {
		proxyPort = *r.ProxyPort
	}

	var brokerSocket, brokerToken string
	if r.Broker != nil {
		tokenCtx := hostcmd.TokenContext{
			JobID: j.ID, TaskID: event.TaskID, ProjectID: event.ProjectID,
			Role: string(project.RoleGate),
		}
		if r.SecretStore != nil {
			brokerToken = r.Broker.RegisterWithSecrets(hostCommandDefs(meta.HostCommands), tokenCtx, r.SecretStore.Get)
		} else {
			brokerToken = r.Broker.Register(hostCommandDefs(meta.HostCommands), tokenCtx)
		}
		brokerSocket = r.Broker.SocketPath
		r.trackToken(j.ID, brokerToken)
	}

	taskJSON, _ := json.Marshal(task)

	cfg := sandbox.WrapperConfig{
		JobID:        j.ID,
		TaskID:       event.TaskID,
		ProjectID:    meta.ID,
		ProjectDir:   proj.WorkDir,
		HookScript:   gateFilename,
		BoidBinary:   r.BoidBinary,
		ServerSocket: r.ServerSocket,
		BrokerSocket: brokerSocket,
		BrokerToken:  brokerToken,
		Env:          meta.Env,
		HostCommands: append(hostCommandNames(meta.HostCommands), "boid"),
		ProxyPort:    proxyPort,
		Role:         "gate",
		TaskJSON:     string(taskJSON),
	}

	return r.launchSandbox(j.ID, event.TaskID, event.Gate.ID, cfg)
}

// launchSandbox writes sandbox scripts and launches in tmux. Returns job ID.
func (r *Runner) launchSandbox(jobID, taskID, handlerID string, cfg sandbox.WrapperConfig) (string, error) {
	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		return "", fmt.Errorf("write sandbox scripts: %w", err)
	}

	session := r.session()

	var windowName string
	if cfg.Role == "hook" || cfg.Role == "gate" {
		windowName = fmt.Sprintf("task-%s", taskID[:min(8, len(taskID))])
	} else {
		windowName = fmt.Sprintf("hook-%s-%s", taskID[:min(8, len(taskID))], handlerID)
	}

	if r.Tmux != nil {
		if err := r.Tmux.EnsureSession(session); err != nil {
			return "", fmt.Errorf("ensure session: %w", err)
		}
		r.Tmux.KillWindow(session, windowName) // ignore error (window may not exist)
		cmd := fmt.Sprintf("bash %s", outerPath)
		if err := r.Tmux.RunInWindow(session, windowName, cmd); err != nil {
			return "", fmt.Errorf("run in window: %w", err)
		}
	}

	slog.Info("job started", "job_id", jobID, "window", windowName)
	return jobID, nil
}

// CleanupTaskWindow kills the tmux window associated with a task.
func (r *Runner) CleanupTaskWindow(taskID string) {
	if r.Tmux == nil {
		return
	}
	session := r.session()
	windowName := fmt.Sprintf("task-%s", taskID[:min(8, len(taskID))])
	if err := r.Tmux.KillWindow(session, windowName); err != nil {
		slog.Debug("cleanup task window", "task_id", taskID, "error", err)
	}
}

// WaitForJobCtx implements orchestrator.JobWaiter.
func (r *Runner) WaitForJobCtx(ctx context.Context, jobID string) (JobCompletionResult, error) {
	ch := r.WaitForJob(jobID)
	select {
	case result := <-ch:
		if result.ExitCode != 0 {
			return result, fmt.Errorf("job %s failed with exit code %d", jobID, result.ExitCode)
		}
		return result, nil
	case <-ctx.Done():
		return JobCompletionResult{}, fmt.Errorf("wait for job %s: %w", jobID, ctx.Err())
	}
}

// UnregisterJob removes the broker token associated with the given job.
func (r *Runner) UnregisterJob(jobID string) {
	r.tokenMu.Lock()
	token, ok := r.jobTokens[jobID]
	if ok {
		delete(r.jobTokens, jobID)
	}
	r.tokenMu.Unlock()

	if ok && r.Broker != nil {
		r.Broker.Unregister(token)
		slog.Info("unregistered broker token", "job_id", jobID)
	}
}
