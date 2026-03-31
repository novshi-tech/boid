package dispatcher

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/novshi-tech/boid/internal/db"
	dtmux "github.com/novshi-tech/boid/internal/dispatcher/tmux"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/secret"
)

type Runner struct {
	DB          *db.DB
	Tmux        dtmux.TmuxManager
	TmuxSession string          // defaults to "boid"
	Broker      *sandbox.Broker // host command broker
	SecretStore *secret.Store   // secret store for resolving secret: env values
	tokenMu     sync.Mutex
	jobTokens   map[string]string // job ID -> broker token
	waiterMu    sync.Mutex
	jobWaiters  map[string]chan JobCompletionResult // job ID -> completion channel
}

func (r *Runner) session() string {
	if r.TmuxSession != "" {
		return r.TmuxSession
	}
	return "boid"
}

func (r *Runner) Dispatch(ctx context.Context, plan *DispatchPlan) (string, error) {
	if plan == nil {
		return "", fmt.Errorf("dispatch plan is required")
	}
	if plan.TaskID == "" || plan.ProjectID == "" || plan.HandlerID == "" || plan.Role == "" {
		return "", fmt.Errorf("dispatch plan is incomplete")
	}

	j := &Job{
		TaskID:    plan.TaskID,
		ProjectID: plan.ProjectID,
		HandlerID: plan.HandlerID,
		Role:      plan.Role,
	}
	if err := CreateJob(r.DB.Conn, j); err != nil {
		return "", fmt.Errorf("create job: %w", err)
	}

	cfg := sandbox.WrapperConfig{
		JobID:              j.ID,
		TaskID:             plan.TaskID,
		ProjectID:          plan.ProjectID,
		ProjectDir:         plan.ProjectDir,
		HomeDir:            plan.HomeDir,
		HooksDir:           plan.HooksDir,
		HookScript:         plan.HookScript,
		BoidBinary:         plan.BoidBinary,
		ServerSocket:       plan.ServerSocket,
		Env:                plan.Env,
		HostCommands:       hostCommandNames(plan.HostCommands),
		AdditionalBindings: toSandboxBindings(plan.AdditionalBindings),
		WorkspaceDirs:      plan.WorkspaceDirs,
		ProxyPort:          plan.ProxyPort,
		StagingDir:         plan.StagingDir,
		WorktreeDir:        plan.WorktreeDir,
		Role:               plan.Role,
		PayloadJSON:        plan.PayloadJSON,
		TaskJSON:           plan.TaskJSON,
		Readonly:           plan.Readonly,
	}

	if r.Broker != nil {
		tokenCtx := sandbox.TokenContext{
			JobID:     j.ID,
			TaskID:    plan.TaskID,
			ProjectID: plan.ProjectID,
			Role:      plan.Role,
		}
		if r.SecretStore != nil {
			cfg.BrokerToken = r.Broker.RegisterWithSecrets(hostCommandDefs(plan.HostCommands), tokenCtx, r.SecretStore.Get)
		} else {
			cfg.BrokerToken = r.Broker.Register(hostCommandDefs(plan.HostCommands), tokenCtx)
		}
		cfg.BrokerSocket = r.Broker.SocketPath
		r.trackToken(j.ID, cfg.BrokerToken)
	}

	return r.launchSandbox(j.ID, plan.TaskID, plan.HandlerID, cfg)
}

func hostCommandNames(cmds map[string]CommandDef) []string {
	if len(cmds) == 0 {
		return nil
	}
	names := make([]string, 0, len(cmds))
	for name := range cmds {
		names = append(names, name)
	}
	return names
}

func hostCommandDefs(cmds map[string]CommandDef) map[string]sandbox.CommandDef {
	if len(cmds) == 0 {
		return nil
	}
	out := make(map[string]sandbox.CommandDef, len(cmds))
	for name, def := range cmds {
		out[name] = sandbox.CommandDef(def)
	}
	return out
}

func toSandboxBindings(bindings []BindMount) []sandbox.BindMount {
	if len(bindings) == 0 {
		return nil
	}
	out := make([]sandbox.BindMount, 0, len(bindings))
	for _, binding := range bindings {
		out = append(out, sandbox.BindMount{
			Source: binding.Source,
			Mode:   binding.Mode,
		})
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
		r.Tmux.KillWindow(session, windowName)
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

// WaitForJobCtx waits for job completion with context cancellation.
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
