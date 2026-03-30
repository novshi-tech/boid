package job

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/hostcmd"
	"github.com/novshi-tech/boid/internal/secret"
	"github.com/novshi-tech/boid/internal/kit"
	"github.com/novshi-tech/boid/internal/model"
	"github.com/novshi-tech/boid/internal/project"
	"github.com/novshi-tech/boid/internal/tmux"
)

type Runner struct {
	DB           *db.DB
	Store        *project.Store
	Tmux         tmux.TmuxManager
	TmuxSession  string           // defaults to "boid"
	BoidBinary   string           // host-side path to boid binary
	ServerSocket string           // host-side server socket path
	ProxyPort    *int             // pointer to server's proxy port (populated after Start)
	Broker       *hostcmd.Broker  // host command broker
	SecretStore  *secret.Store    // secret store for resolving secret: env values
	tokenMu      sync.Mutex
	jobTokens    map[string]string // job ID -> broker token
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
	projects, err := r.DB.ListProjects()
	if err != nil {
		slog.Warn("list projects for workspace", "error", err)
		return nil
	}
	dirs := make(map[string]string)
	for _, p := range projects {
		if p.ID == selfID {
			continue
		}
		m, ok := r.Store.Get(p.ID)
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
func (r *Runner) Execute(ctx context.Context, event *model.HookFireEvent) error {
	slog.Info("executing hook", "hook_id", event.Hook.ID, "task_id", event.TaskID)

	hookFilename := filepath.Base(event.Hook.ScriptPath)
	if hookFilename == "" || hookFilename == "." {
		return fmt.Errorf("hook %q: no script path resolved", event.Hook.ID)
	}

	// Get project info
	meta, ok := r.Store.Get(event.ProjectID)
	if !ok {
		return fmt.Errorf("project %q: meta not loaded", event.ProjectID)
	}

	proj, err := r.DB.GetProject(event.ProjectID)
	if err != nil {
		return fmt.Errorf("get project: %w", err)
	}

	j := &model.Job{
		TaskID:    event.TaskID,
		ProjectID: event.ProjectID,
		HookID:    event.Hook.ID,
	}

	if err := r.DB.CreateJob(j); err != nil {
		return fmt.Errorf("create job: %w", err)
	}

	// Determine hooks directory: stage if kits provide hooks, otherwise use project dir
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

	// Collect workspace peer projects (read-only mounts)
	workspaceDirs := r.collectWorkspaceDirs(meta.WorkspaceID, event.ProjectID)

	var proxyPort int
	if r.ProxyPort != nil {
		proxyPort = *r.ProxyPort
	}

	// Register host commands with broker
	var brokerSocket, brokerToken string
	if r.Broker != nil && len(meta.HostCommands) > 0 {
		if r.SecretStore != nil {
			brokerToken = r.Broker.RegisterWithSecrets(meta.HostCommands, r.SecretStore.Get)
		} else {
			brokerToken = r.Broker.Register(meta.HostCommands)
		}
		brokerSocket = r.Broker.SocketPath
		r.trackToken(j.ID, brokerToken)
	}

	homeDir, _ := os.UserHomeDir()

	cfg := WrapperConfig{
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
	}

	outerPath, err := WriteSandboxScripts(cfg)
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

func hostCommandNames(cmds map[string]hostcmd.CommandDef) []string {
	names := make([]string, 0, len(cmds))
	for name := range cmds {
		names = append(names, name)
	}
	return names
}

func (r *Runner) trackToken(jobID, token string) {
	r.tokenMu.Lock()
	defer r.tokenMu.Unlock()
	if r.jobTokens == nil {
		r.jobTokens = make(map[string]string)
	}
	r.jobTokens[jobID] = token
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

