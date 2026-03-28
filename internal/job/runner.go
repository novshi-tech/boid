package job

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/model"
	"github.com/novshi-tech/boid/internal/project"
	"github.com/novshi-tech/boid/internal/tmux"
)

type Runner struct {
	DB           *db.DB
	Store        *project.Store
	Tmux         tmux.TmuxManager
	TmuxSession  string // defaults to "boid"
	BoidBinary   string // host-side path to boid binary
	ServerSocket string // host-side server socket path
}

func (r *Runner) session() string {
	if r.TmuxSession != "" {
		return r.TmuxSession
	}
	return "boid"
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

	cfg := WrapperConfig{
		JobID:        j.ID,
		ProjectID:    meta.ID,
		ProjectDir:   proj.WorkDir,
		HooksDir:     filepath.Join(proj.WorkDir, ".boid", "hooks"),
		HookScript:   hookFilename,
		BoidBinary:   r.BoidBinary,
		ServerSocket: r.ServerSocket,
		Env:          meta.Env,
		HostCommands: meta.HostCommands,
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
		if err := r.Tmux.NewWindow(session, windowName); err != nil {
			return fmt.Errorf("new window: %w", err)
		}

		cmd := fmt.Sprintf("bash %s", outerPath)
		if err := r.Tmux.SendKeys(session, windowName, cmd); err != nil {
			return fmt.Errorf("send keys: %w", err)
		}
	}

	slog.Info("job started", "job_id", j.ID, "window", windowName)
	return nil
}
