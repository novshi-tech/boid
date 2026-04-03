package dispatcher

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
)

type Runner struct {
	DB            *sql.DB
	Runtime       JobRuntime
	Broker        CommandBroker // host command broker
	Sandbox       SandboxPreparer
	SecretStore   *SecretStore // secret store for resolving secret: env values
	tokenMu       sync.Mutex
	jobTokens     map[string]string // job ID -> broker token
	waiterMu      sync.Mutex
	jobWaiters    map[string]chan JobCompletionResult // job ID -> completion channel
	runtimeMu     sync.Mutex
	taskRuntimes  map[string]map[string]struct{} // task ID -> runtime IDs
	completedMu   sync.Mutex
	completedJobs map[string]JobCompletionResult
}

func (r *Runner) Dispatch(ctx context.Context, plan *DispatchPlan) (string, error) {
	if plan == nil {
		return "", fmt.Errorf("dispatch plan is required")
	}
	if plan.TaskID == "" || plan.ProjectID == "" || plan.HandlerID == "" || plan.Role == "" {
		return "", fmt.Errorf("dispatch plan is incomplete")
	}

	j := &Job{
		TaskID:      plan.TaskID,
		ProjectID:   plan.ProjectID,
		HandlerID:   plan.HandlerID,
		Role:        plan.Role,
		Interactive: false,
		TTY:         false,
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
		GatesDir:           plan.GatesDir,
		HookScript:         plan.HookScript,
		BoidBinary:         plan.BoidBinary,
		ServerSocket:       plan.ServerSocket,
		Env:                plan.Env,
		BuiltinCommands:    append([]string(nil), plan.BuiltinCommands...),
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
		InstructionsJSON:   plan.InstructionsJSON,
	}
	if spec.Role == "hook" || spec.Role == "gate" {
		spec.TTY = true
	}

	if r.Broker != nil {
		allowedProjectIDs := allowedProjectIDs(plan.ProjectID, plan.WorkspaceDirs)
		tokenCtx := BrokerContext{
			JobID:             j.ID,
			TaskID:            plan.TaskID,
			ProjectID:         plan.ProjectID,
			WorkspaceID:       plan.WorkspaceID,
			AllowedProjectIDs: allowedProjectIDs,
			Role:              plan.Role,
			ProjectDir:        plan.ProjectDir,
			WorktreeDir:       plan.WorktreeDir,
		}
		var resolve SecretResolver
		if r.SecretStore != nil {
			ns := plan.SecretNamespace
			if ns == "" {
				ns = "default"
			}
			resolve = func(key string) (string, error) {
				return r.SecretStore.Get(ns, key)
			}
		}
		spec.BrokerToken = r.Broker.RegisterCommands(plan.HostCommands, plan.BuiltinCommands, tokenCtx, resolve)
		spec.BrokerSocket = r.Broker.SocketPath()
		r.trackToken(j.ID, spec.BrokerToken)
	}

	return r.launchSandbox(ctx, j, spec)
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

func allowedProjectIDs(selfID string, workspaceDirs map[string]string) []string {
	seen := make(map[string]struct{})
	var ids []string

	if selfID != "" {
		seen[selfID] = struct{}{}
		ids = append(ids, selfID)
	}

	if len(workspaceDirs) == 0 {
		return ids
	}

	var peers []string
	for id := range workspaceDirs {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		peers = append(peers, id)
	}
	sort.Strings(peers)
	return append(ids, peers...)
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
	r.markJobCompleted(jobID, result)

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

// launchSandbox writes sandbox scripts and launches via the configured runtime. Returns job ID.
func (r *Runner) launchSandbox(ctx context.Context, job *Job, spec SandboxSpec) (string, error) {
	if job == nil {
		return "", fmt.Errorf("job is required")
	}
	if r.Sandbox == nil {
		return "", fmt.Errorf("sandbox preparer is required")
	}
	if r.Runtime == nil {
		return "", fmt.Errorf("job runtime is required")
	}

	prepared, err := r.Sandbox.PrepareSandbox(spec)
	if err != nil {
		return "", fmt.Errorf("prepare sandbox: %w", err)
	}
	if prepared == nil || prepared.OuterPath == "" {
		return "", fmt.Errorf("prepare sandbox: missing outer script path")
	}

	handle, err := r.Runtime.Start(ctx, RuntimeStartSpec{
		JobID:       job.ID,
		TaskID:      job.TaskID,
		ProjectID:   job.ProjectID,
		HandlerID:   job.HandlerID,
		Role:        job.Role,
		Command:     fmt.Sprintf("bash %s", prepared.OuterPath),
		Interactive: spec.TTY,
		TTY:         spec.TTY,
	})
	if err != nil {
		return "", fmt.Errorf("start runtime: %w", err)
	}

	job.RuntimeID = handle.ID
	job.Interactive = handle.Interactive
	job.TTY = handle.TTY
	if err := UpdateJob(r.DB, job); err != nil {
		_ = r.Runtime.Stop(context.Background(), handle.ID)
		return "", fmt.Errorf("persist job runtime metadata: %w", err)
	}

	r.trackTaskRuntime(job.TaskID, handle.ID)
	go r.watchRuntime(job.ID, handle.ID)
	slog.Info("job started", "job_id", job.ID, "runtime_id", handle.ID)
	return job.ID, nil
}

// CleanupTaskWindow stops all tracked runtimes associated with a task.
func (r *Runner) CleanupTaskWindow(taskID string) {
	if r.Runtime == nil {
		return
	}
	runtimeIDs := r.takeTaskRuntimes(taskID)
	for _, runtimeID := range runtimeIDs {
		if err := r.Runtime.Stop(context.Background(), runtimeID); err != nil {
			slog.Debug("cleanup task runtime", "task_id", taskID, "runtime_id", runtimeID, "error", err)
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

func (r *Runner) markJobCompleted(jobID string, result JobCompletionResult) {
	r.completedMu.Lock()
	defer r.completedMu.Unlock()

	if r.completedJobs == nil {
		r.completedJobs = make(map[string]JobCompletionResult)
	}
	r.completedJobs[jobID] = result
}

func (r *Runner) isJobCompleted(jobID string) bool {
	r.completedMu.Lock()
	defer r.completedMu.Unlock()

	_, ok := r.completedJobs[jobID]
	return ok
}

func (r *Runner) trackTaskRuntime(taskID, runtimeID string) {
	if taskID == "" || runtimeID == "" {
		return
	}

	r.runtimeMu.Lock()
	defer r.runtimeMu.Unlock()

	if r.taskRuntimes == nil {
		r.taskRuntimes = make(map[string]map[string]struct{})
	}
	if r.taskRuntimes[taskID] == nil {
		r.taskRuntimes[taskID] = make(map[string]struct{})
	}
	r.taskRuntimes[taskID][runtimeID] = struct{}{}
}

func (r *Runner) takeTaskRuntimes(taskID string) []string {
	r.runtimeMu.Lock()
	defer r.runtimeMu.Unlock()

	runtimes := r.taskRuntimes[taskID]
	if len(runtimes) == 0 {
		return nil
	}
	delete(r.taskRuntimes, taskID)

	out := make([]string, 0, len(runtimes))
	for runtimeID := range runtimes {
		out = append(out, runtimeID)
	}
	sort.Strings(out)
	return out
}

func (r *Runner) watchRuntime(jobID, runtimeID string) {
	if r.Runtime == nil || runtimeID == "" {
		return
	}

	result, err := r.Runtime.Wait(context.Background(), runtimeID)
	if err != nil {
		if errors.Is(err, ErrRuntimeUnsupported) {
			return
		}
		slog.Warn("runtime wait failed", "job_id", jobID, "runtime_id", runtimeID, "error", err)
		return
	}
	if r.isJobCompleted(jobID) {
		return
	}

	job, err := GetJob(r.DB, jobID)
	if err != nil {
		slog.Warn("runtime exited for unknown job", "job_id", jobID, "runtime_id", runtimeID, "error", err)
		return
	}
	if job.Status != JobStatusRunning {
		return
	}

	exitCode := result.ExitCode
	if exitCode == 0 {
		exitCode = 1
	}
	output := fmt.Sprintf("job runtime exited without boid job done (runtime_id=%s, exit_code=%d)", runtimeID, result.ExitCode)

	job.Status = JobStatusFailed
	job.ExitCode = exitCode
	job.Output = output
	if err := UpdateJob(r.DB, job); err != nil {
		slog.Warn("persist runtime exit failure state", "job_id", jobID, "runtime_id", runtimeID, "error", err)
		return
	}

	r.CompleteJob(jobID, JobCompletionResult{
		Output:   output,
		ExitCode: exitCode,
	})
	r.UnregisterJob(jobID)

	slog.Warn("runtime exited before boid job done", "job_id", jobID, "runtime_id", runtimeID, "exit_code", result.ExitCode)
}
