package dispatcher

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	dtmux "github.com/novshi-tech/boid/internal/dispatcher/tmux"
)

type Runner struct {
	DB          *sql.DB
	Tmux        dtmux.TmuxManager
	TmuxSession string        // defaults to "boid"
	Broker      CommandBroker // host command broker
	Sandbox     SandboxPreparer
	SecretStore *SecretStore // secret store for resolving secret: env values
	tokenMu     sync.Mutex
	jobTokens   map[string]string // job ID -> broker token
	waiterMu    sync.Mutex
	jobWaiters  map[string]chan JobCompletionResult // job ID -> completion channel
	windowMu    sync.Mutex
	taskWindows map[string]map[string]struct{} // task ID -> tmux window names
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
	if err := CreateJob(r.DB, j); err != nil {
		return "", fmt.Errorf("create job: %w", err)
	}

	spec := SandboxSpec{
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
		AdditionalBindings: plan.AdditionalBindings,
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
		tokenCtx := BrokerContext{
			JobID:     j.ID,
			TaskID:    plan.TaskID,
			ProjectID: plan.ProjectID,
			Role:      plan.Role,
		}
		var resolve SecretResolver
		if r.SecretStore != nil {
			resolve = r.SecretStore.Get
		}
		spec.BrokerToken = r.Broker.RegisterCommands(plan.HostCommands, tokenCtx, resolve)
		spec.BrokerSocket = r.Broker.SocketPath()
		r.trackToken(j.ID, spec.BrokerToken)
	}

	return r.launchSandbox(j.ID, plan.TaskID, plan.HandlerID, spec)
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
func (r *Runner) launchSandbox(jobID, taskID, handlerID string, spec SandboxSpec) (string, error) {
	if r.Sandbox == nil {
		return "", fmt.Errorf("sandbox preparer is required")
	}

	prepared, err := r.Sandbox.PrepareSandbox(spec)
	if err != nil {
		return "", fmt.Errorf("prepare sandbox: %w", err)
	}
	if prepared == nil || prepared.OuterPath == "" {
		return "", fmt.Errorf("prepare sandbox: missing outer script path")
	}

	session := r.session()

	var windowName string
	if spec.Role == "hook" || spec.Role == "gate" {
		windowName = fmt.Sprintf("job-%s-%s", shortID(taskID), shortID(jobID))
	} else {
		windowName = fmt.Sprintf("hook-%s-%s", shortID(taskID), handlerID)
	}

	if r.Tmux != nil {
		if err := r.Tmux.EnsureSession(session); err != nil {
			return "", fmt.Errorf("ensure session: %w", err)
		}
		r.Tmux.KillWindow(session, windowName)
		cmd := fmt.Sprintf("bash %s", prepared.OuterPath)
		if err := r.Tmux.RunInWindow(session, windowName, cmd); err != nil {
			return "", fmt.Errorf("run in window: %w", err)
		}
		r.trackTaskWindow(taskID, windowName)
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
	windows := r.takeTaskWindows(taskID)
	if len(windows) == 0 {
		windows = []string{fmt.Sprintf("task-%s", shortID(taskID))}
	}
	for _, windowName := range windows {
		if err := r.Tmux.KillWindow(session, windowName); err != nil {
			slog.Debug("cleanup task window", "task_id", taskID, "window", windowName, "error", err)
		}
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
		r.Broker.UnregisterCommandToken(token)
		slog.Info("unregistered broker token", "job_id", jobID)
	}
}

func shortID(id string) string {
	return id[:min(8, len(id))]
}

func (r *Runner) trackTaskWindow(taskID, windowName string) {
	if taskID == "" || windowName == "" {
		return
	}

	r.windowMu.Lock()
	defer r.windowMu.Unlock()

	if r.taskWindows == nil {
		r.taskWindows = make(map[string]map[string]struct{})
	}
	if r.taskWindows[taskID] == nil {
		r.taskWindows[taskID] = make(map[string]struct{})
	}
	r.taskWindows[taskID][windowName] = struct{}{}
}

func (r *Runner) takeTaskWindows(taskID string) []string {
	r.windowMu.Lock()
	defer r.windowMu.Unlock()

	windows := r.taskWindows[taskID]
	if len(windows) == 0 {
		return nil
	}
	delete(r.taskWindows, taskID)

	out := make([]string, 0, len(windows))
	for windowName := range windows {
		out = append(out, windowName)
	}
	sort.Strings(out)
	return out
}
