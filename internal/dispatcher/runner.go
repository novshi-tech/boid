package dispatcher

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
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
	completedJobs map[string]JobCompletionResult     // job ID -> result (guarded by waiterMu)
	runtimeMu     sync.Mutex
	taskRuntimes  map[string]map[string]struct{} // task ID -> runtime IDs
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
		HookFiles:          plan.HookFiles,
		GatesDir:           plan.GatesDir,
		HookScript:         plan.HookScript,
		BoidBinary:         plan.BoidBinary,
		ServerSocket:       plan.ServerSocket,
		Env:                plan.Env,
		BuiltinPolicies:    plan.BuiltinPolicies,
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
		TaskYAML:           plan.TaskYAML,
		EnvironmentYAML:    plan.EnvironmentYAML,
		Model:              plan.Model,
		InvokedRole:        plan.InvokedRole,
		InvokedName:        plan.InvokedName,
		InvokedType:        plan.InvokedType,
	}
	if spec.Role == "hook" || spec.Role == "gate" || plan.Interactive {
		spec.TTY = true
	}
	spec.Interactive = plan.Interactive

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
		spec.BrokerToken = r.Broker.RegisterCommands(plan.HostCommands, plan.BuiltinPolicies, tokenCtx, resolve)
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
// If the job has already completed, the result is sent immediately.
func (r *Runner) WaitForJob(jobID string) <-chan JobCompletionResult {
	r.waiterMu.Lock()
	defer r.waiterMu.Unlock()

	ch := make(chan JobCompletionResult, 1)

	// If already completed, deliver immediately without blocking.
	if result, ok := r.completedJobs[jobID]; ok {
		ch <- result
		return ch
	}

	if r.jobWaiters == nil {
		r.jobWaiters = make(map[string]chan JobCompletionResult)
	}
	r.jobWaiters[jobID] = ch
	return ch
}

// CompleteJob signals the waiting dispatcher that a job has completed.
func (r *Runner) CompleteJob(jobID string, result JobCompletionResult) {
	r.waiterMu.Lock()
	if r.completedJobs == nil {
		r.completedJobs = make(map[string]JobCompletionResult)
	}
	r.completedJobs[jobID] = result
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
		cleanupSandboxArtifacts(prepared)
		return "", fmt.Errorf("start runtime: %w", err)
	}

	job.RuntimeID = handle.ID
	job.Interactive = handle.Interactive
	job.TTY = handle.TTY
	if err := UpdateJob(r.DB, job); err != nil {
		_ = r.Runtime.Stop(context.Background(), handle.ID)
		cleanupSandboxArtifacts(prepared)
		return "", fmt.Errorf("persist job runtime metadata: %w", err)
	}

	r.trackTaskRuntime(job.TaskID, handle.ID)
	go r.watchRuntime(job.ID, handle.ID)
	go r.cleanupSandboxAfterWait(handle.ID, prepared)
	slog.Info("job started", "job_id", job.ID, "runtime_id", handle.ID)
	return job.ID, nil
}

// cleanupSandboxAfterWait blocks until the runtime exits, then removes sandbox
// temp artifacts (ROOT dir, generated scripts, gate staging dir). Safe to call
// alongside watchRuntime: both wait on the same runtime.done channel.
//
// IMPORTANT: we must wait for runtime exit before removing ROOT. Until the
// sandbox process is dead, bind mounts under ROOT may still be live, and
// os.RemoveAll could traverse into host filesystems.
func (r *Runner) cleanupSandboxAfterWait(runtimeID string, prepared *PreparedSandbox) {
	if r.Runtime == nil || runtimeID == "" || prepared == nil {
		return
	}
	if _, err := r.Runtime.Wait(context.Background(), runtimeID); err != nil {
		if errors.Is(err, ErrRuntimeUnsupported) {
			cleanupSandboxArtifacts(prepared)
			return
		}
		slog.Warn("skip sandbox cleanup: runtime wait failed", "runtime_id", runtimeID, "error", err)
		return
	}
	cleanupSandboxArtifacts(prepared)
}

func cleanupSandboxArtifacts(prepared *PreparedSandbox) {
	if prepared == nil {
		return
	}
	if prepared.RootDir != "" {
		if err := os.RemoveAll(prepared.RootDir); err != nil {
			slog.Warn("remove sandbox root", "path", prepared.RootDir, "error", err)
		}
	}
	for _, p := range prepared.ScriptPaths {
		if p == "" {
			continue
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			slog.Warn("remove sandbox script", "path", p, "error", err)
		}
	}
	if prepared.StagingDir != "" {
		if err := os.RemoveAll(prepared.StagingDir); err != nil {
			slog.Warn("remove sandbox staging dir", "path", prepared.StagingDir, "error", err)
		}
	}
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

func (r *Runner) isJobCompleted(jobID string) bool {
	r.waiterMu.Lock()
	defer r.waiterMu.Unlock()

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
