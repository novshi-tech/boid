package dispatcher

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"sync"
	"syscall"

	"github.com/google/uuid"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// TaskLookup mirrors the subset of orchestrator.TaskLookup the dispatcher
// needs for worktree resolution. Kept as an interface so tests can stub it.
type TaskLookup interface {
	GetTask(id string) (*orchestrator.Task, error)
}

// ProjectLookup lets dispatcher resolve ProjectID → WorkspaceID and enumerate
// workspace peers, so workspace-peer authorization and peer-visibility
// concerns stay inside dispatcher instead of leaking into JobSpec.
type ProjectLookup interface {
	GetProject(id string) (*orchestrator.Project, error)
	ListProjects() ([]*orchestrator.Project, error)
}

// JobEventSink lets the runner report job lifecycle events to a subscriber
// (typically the web SSE hub) without taking a hard dependency on it.
// All methods are best-effort: implementations should not block or fail
// the caller — they exist to push UI refresh hints.
type JobEventSink interface {
	JobCreated(taskID, jobID string)
}

type Runner struct {
	DB           *sql.DB
	Runtime      JobRuntime
	Broker       CommandBroker
	Sandbox      SandboxPreparer
	SecretStore  *SecretStore
	Worktrees    *WorktreeManager
	TaskLookup   TaskLookup
	Projects     ProjectLookup
	BoidBinary   string
	ServerSocket string
	ProxyPort    *int
	JobEvents    JobEventSink // optional; nil disables job lifecycle broadcasts

	tokenMu       sync.Mutex
	jobTokens     map[string]string
	waiterMu      sync.Mutex
	jobWaiters    map[string]chan JobCompletionResult
	completedJobs map[string]JobCompletionResult
	runtimeMu     sync.Mutex
	taskRuntimes  map[string]map[string]struct{}
}

// Dispatch launches a sandbox for the given JobSpec. The optional cleanup
// callback (typically provided by orchestrator's PlanHook for
// staging dir teardown) runs after the sandbox process has exited.
func (r *Runner) Dispatch(ctx context.Context, spec *orchestrator.JobSpec, cleanup orchestrator.CleanupFunc) (string, error) {
	if spec == nil {
		return "", fmt.Errorf("job spec is required")
	}
	if spec.ProjectID == "" {
		return "", fmt.Errorf("job spec is missing project id")
	}
	if len(spec.Argv) == 0 {
		return "", fmt.Errorf("job spec is missing argv")
	}

	j := &Job{
		TaskID:      spec.TaskID,
		ProjectID:   spec.ProjectID,
		HandlerID:   spec.HandlerID,
		DisplayName: spec.DisplayName,
		// Role は DB ラベル / TUI 表示のみに使われる。sandbox 構築側は
		// 一切これを読まない。
		Role:           string(spec.Kind),
		ExecutionState: spec.ExecutionState,
	}
	j.ID = uuid.New().String()

	// Phase 2-2 case 1 HEAD guard: supervisors running in the project dir
	// (worktree=false) must find the project HEAD still on the base_branch
	// they were created for. If the user (or a parallel process) moved the
	// branch between task creation and now, refuse to dispatch — running a
	// supervisor against an unexpected branch is the precise foot-gun the
	// Phase 2-2 design exists to prevent. The check is best-effort: failing
	// to look up the task or the project (test wiring without stubs, etc.)
	// degrades to a no-op rather than blocking the dispatch.
	if err := r.enforceCaseOneHeadInvariant(spec); err != nil {
		if cleanup != nil {
			cleanup()
		}
		return "", err
	}

	// Resolve the worktree path before sandbox construction, so the mount
	// layout sees the correct project root.
	worktreePath, err := r.resolveWorktree(spec)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return "", err
	}
	// resolvedWorktreePath passes the existing worktree path to the broker
	// TokenContext.WorktreeDir without allocating a new one.
	resolvedWorktreePath := worktreePath
	if resolvedWorktreePath == "" {
		resolvedWorktreePath = r.existingWorktreePath(spec)
	}

	if err := CreateJob(r.DB, j); err != nil {
		if cleanup != nil {
			cleanup()
		}
		return "", fmt.Errorf("create job: %w", err)
	}

	// Notify the web SSE hub (via the optional JobEvents sink) so task detail
	// timelines refresh as soon as a running job row exists, not only after
	// it completes. Without this the UI sits idle during the whole hook run.
	if r.JobEvents != nil && j.TaskID != "" {
		r.JobEvents.JobCreated(j.TaskID, j.ID)
	}

	workspaceID, projectWorkDir, _ := r.resolveProjectRuntime(spec.ProjectID)
	workspacePeers := r.resolveWorkspacePeers(workspaceID, spec.ProjectID)

	var resolvedHostCommands map[string]orchestrator.CommandDef
	if len(spec.HostCommands) > 0 || len(spec.BuiltinPolicies) > 0 {
		var err error
		resolvedHostCommands, err = ResolveHostCommands(
			sortedKeys(spec.BuiltinPolicies),
			spec.HostCommands,
			projectWorkDir,
			exec.LookPath,
		)
		if err != nil {
			r.failJob(j, err)
			if cleanup != nil {
				cleanup()
			}
			return "", err
		}
	}

	var brokerSocket, brokerToken string
	if r.Broker != nil && (len(spec.BuiltinPolicies) > 0 || len(resolvedHostCommands) > 0) {
		tokenCtx := sandbox.TokenContext{
			JobID:             j.ID,
			TaskID:            spec.TaskID,
			ProjectID:         spec.ProjectID,
			WorkspaceID:       workspaceID,
			AllowedProjectIDs: allowedProjectIDs(spec.ProjectID, workspacePeers),
			Role:              j.Role,
			ProjectDir:  projectWorkDir,
			WorktreeDir: resolvedWorktreePath,
		}
		var resolve SecretResolver
		if r.SecretStore != nil {
			ns := spec.SecretNamespace
			if ns == "" {
				ns = "default"
			}
			resolve = func(key string) (string, error) {
				return r.SecretStore.Get(ns, key)
			}
		}
		brokerToken = r.Broker.RegisterCommands(
			resolvedHostCommands,
			PoliciesToSandbox(spec.BuiltinPolicies),
			tokenCtx,
			resolve,
		)
		brokerSocket = r.Broker.SocketPath()
		r.trackToken(j.ID, brokerToken)
	}

	rtInfo := SandboxRuntimeInfo{
		JobID:                j.ID,
		BoidBinary:           r.BoidBinary,
		ServerSocket:         r.ServerSocket,
		ProxyPort:            r.proxyPort(),
		BrokerSocket:         brokerSocket,
		BrokerToken:          brokerToken,
		WorktreeDir:          worktreePath,
		WorkspacePeers:       workspacePeers,
		ResolvedHostCommands: resolvedHostCommands,
		DockerEnabled:        spec.Visibility.DockerEnabled,
	}
	// Server socket is only exposed to jobs that have no broker policies
	// attached — i.e. boid exec invocations that need to talk to the daemon
	// directly. For hook/gate jobs the daemon conversation goes through the
	// broker socket above.
	if brokerToken != "" {
		rtInfo.ServerSocket = ""
	}

	sbSpec, err := BuildSandboxSpec(spec, rtInfo)
	if err != nil {
		r.failJob(j, err)
		if cleanup != nil {
			cleanup()
		}
		return "", err
	}
	return r.launchSandbox(ctx, j, sbSpec, cleanup)
}

// enforceCaseOneHeadInvariant verifies the Phase 2-2 case 1 HEAD guard:
// when a supervisor task is scheduled to run in the project dir (no worktree)
// the project HEAD must still match the task's BaseBranch. The guard is
// degraded to a no-op when dependencies are unwired (TaskLookup or Projects
// absent — typical in unit tests that only exercise Dispatch's argv plumbing)
// so the existing test suite stays untouched.
//
// Returns a non-nil error only when the invariant is genuinely violated. The
// caller fails the job dispatch with the message so the orchestrator surfaces
// "project dir was moved out from under us" to the user instead of silently
// running against the wrong branch.
func (r *Runner) enforceCaseOneHeadInvariant(spec *orchestrator.JobSpec) error {
	if spec == nil || spec.Visibility.UseWorktree {
		return nil
	}
	if spec.TaskID == "" || r.TaskLookup == nil || r.Projects == nil || r.Worktrees == nil {
		return nil
	}
	task, err := r.TaskLookup.GetTask(spec.TaskID)
	if err != nil || task == nil {
		// Task not found mid-flight: not our error to surface here. The
		// downstream sandbox build will catch the inconsistency.
		return nil
	}
	canonical, _ := orchestrator.CanonicalBehaviorName(task.Behavior)
	if canonical != "supervisor" {
		return nil
	}
	if task.BaseBranch == "" {
		return nil
	}
	proj, err := r.Projects.GetProject(spec.ProjectID)
	if err != nil || proj == nil || proj.WorkDir == "" {
		return nil
	}
	if err := r.Worktrees.EnforceHeadOnBaseBranch(proj.WorkDir, task.BaseBranch); err != nil {
		slog.Warn("supervisor HEAD guard rejected dispatch",
			"task_id", spec.TaskID,
			"project_dir", proj.WorkDir,
			"base_branch", task.BaseBranch,
			"error", err,
		)
		return err
	}
	return nil
}

func (r *Runner) resolveProjectRuntime(projectID string) (string, string, error) {
	if r.Projects == nil || projectID == "" {
		return "", "", nil
	}
	proj, err := r.Projects.GetProject(projectID)
	if err != nil || proj == nil {
		return "", "", err
	}
	return proj.WorkspaceID, proj.WorkDir, nil
}

// resolveWorkspacePeers enumerates projects sharing workspaceID other than
// selfID, returning a peer-id → host-path map suitable for both broker
// authorization (AllowedProjectIDs) and sandbox FS mounting. Returns nil when
// workspaceID is empty, Projects is unset, or the lookup fails — callers treat
// nil as "no peers" and a solo-project allowlist.
func (r *Runner) resolveWorkspacePeers(workspaceID, selfID string) map[string]string {
	if r.Projects == nil || workspaceID == "" {
		return nil
	}
	projects, err := r.Projects.ListProjects()
	if err != nil {
		return nil
	}
	peers := make(map[string]string)
	for _, p := range projects {
		if p == nil || p.ID == "" || p.ID == selfID {
			continue
		}
		if p.WorkspaceID != workspaceID {
			continue
		}
		peers[p.ID] = p.WorkDir
	}
	if len(peers) == 0 {
		return nil
	}
	return peers
}

func (r *Runner) proxyPort() int {
	if r.ProxyPort == nil {
		return 0
	}
	return *r.ProxyPort
}

func allowedProjectIDs(selfID string, workspacePeers map[string]string) []string {
	seen := make(map[string]struct{})
	var ids []string
	if selfID != "" {
		seen[selfID] = struct{}{}
		ids = append(ids, selfID)
	}
	if len(workspacePeers) == 0 {
		return ids
	}
	var peers []string
	for id := range workspacePeers {
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

// failJob marks j as failed in the DB. Used for errors that occur after
// CreateJob but before the sandbox is launched, so orphan running rows do not
// accumulate in the jobs table.
func (r *Runner) failJob(j *Job, cause error) {
	j.Status = JobStatusFailed
	j.Output = cause.Error()
	if err := UpdateJob(r.DB, j); err != nil {
		slog.Warn("persist pre-launch job failure", "job_id", j.ID, "error", err)
	}
}

// WaitForJob registers a channel that will receive the job completion result.
func (r *Runner) WaitForJob(jobID string) <-chan JobCompletionResult {
	r.waiterMu.Lock()
	defer r.waiterMu.Unlock()

	ch := make(chan JobCompletionResult, 1)
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

// launchSandbox writes sandbox scripts and launches via the configured runtime.
func (r *Runner) launchSandbox(ctx context.Context, job *Job, spec sandbox.Spec, cleanup orchestrator.CleanupFunc) (string, error) {
	if job == nil {
		return "", fmt.Errorf("job is required")
	}
	if r.Sandbox == nil {
		if cleanup != nil {
			cleanup()
		}
		return "", fmt.Errorf("sandbox preparer is required")
	}
	if r.Runtime == nil {
		if cleanup != nil {
			cleanup()
		}
		return "", fmt.Errorf("job runtime is required")
	}

	prepared, err := r.Sandbox.PrepareSandbox(spec)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return "", fmt.Errorf("prepare sandbox: %w", err)
	}
	if prepared == nil || prepared.OuterPath == "" {
		if cleanup != nil {
			cleanup()
		}
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
		if cleanup != nil {
			cleanup()
		}
		return "", fmt.Errorf("start runtime: %w", err)
	}

	job.RuntimeID = handle.ID
	job.Interactive = handle.Interactive
	job.TTY = handle.TTY
	if err := UpdateJob(r.DB, job); err != nil {
		_ = r.Runtime.Stop(context.Background(), handle.ID)
		cleanupSandboxArtifacts(prepared)
		if cleanup != nil {
			cleanup()
		}
		return "", fmt.Errorf("persist job runtime metadata: %w", err)
	}

	r.trackTaskRuntime(job.TaskID, handle.ID)
	go r.watchRuntime(job.ID, handle.ID)
	go r.cleanupSandboxAfterWait(handle.ID, prepared, cleanup)
	slog.Info("job started", "job_id", job.ID, "runtime_id", handle.ID)
	return job.ID, nil
}

func (r *Runner) cleanupSandboxAfterWait(runtimeID string, prepared *PreparedSandbox, extra orchestrator.CleanupFunc) {
	defer func() {
		if extra != nil {
			extra()
		}
	}()
	if r.Runtime == nil || runtimeID == "" || prepared == nil {
		return
	}
	result, err := r.Runtime.Wait(context.Background(), runtimeID)
	if err != nil {
		if errors.Is(err, ErrRuntimeUnsupported) {
			cleanupSandboxArtifacts(prepared)
			return
		}
		slog.Warn("skip sandbox cleanup: runtime wait failed", "runtime_id", runtimeID, "error", err)
		return
	}
	// Scaffolding (RootDir, StagingDir) は outer.sh が常に削除するので、
	// ここでは保険として idempotent に rm するだけ。 exit_code に関わらず実行。
	cleanupSandboxScaffolding(prepared)
	if result.ExitCode != 0 {
		// silent な exit_code != 0 ケースの事後解析を可能にするため、 script
		// ファイルだけ保全する。 transcript.log が 0 byte で daemon log にも
		// 有用情報が無い場合、 outer.sh / setup.sh / inner.sh の中身がほぼ唯一の
		// 手がかりになる。 GC や手動削除に任せる。
		slog.Warn("retained sandbox scripts for diagnosis (exit_code!=0)",
			"runtime_id", runtimeID,
			"exit_code", result.ExitCode,
			"scripts", prepared.ScriptPaths,
		)
		return
	}
	cleanupSandboxScripts(prepared)
}

// cleanupSandboxArtifacts removes every sandbox artifact (scaffolding +
// scripts). Used by runtime-unsupported paths and tests.
func cleanupSandboxArtifacts(prepared *PreparedSandbox) {
	cleanupSandboxScaffolding(prepared)
	cleanupSandboxScripts(prepared)
}

// cleanupSandboxScaffolding removes the sandbox ROOT directory and the staging
// dir. Both are normally rm'd by outer.sh; this call is a best-effort safety
// net for the case where outer.sh was killed before its cleanup ran.
func cleanupSandboxScaffolding(prepared *PreparedSandbox) {
	if prepared == nil {
		return
	}
	if prepared.RootDir != "" {
		if err := os.RemoveAll(prepared.RootDir); err != nil {
			slog.Warn("remove sandbox root", "path", prepared.RootDir, "error", err)
		}
	}
	if prepared.StagingDir != "" {
		if err := os.RemoveAll(prepared.StagingDir); err != nil {
			slog.Warn("remove sandbox staging dir", "path", prepared.StagingDir, "error", err)
		}
	}
}

// cleanupSandboxScripts removes the generated outer/setup/inner scripts. These
// are deliberately retained on exit_code != 0 for post-hoc diagnosis.
func cleanupSandboxScripts(prepared *PreparedSandbox) {
	if prepared == nil {
		return
	}
	for _, p := range prepared.ScriptPaths {
		if p == "" {
			continue
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			slog.Warn("remove sandbox script", "path", p, "error", err)
		}
	}
}

// StopJobRuntime stops the runtime identified by runtimeID.
// It is a best-effort operation: errors are logged at debug level only.
func (r *Runner) StopJobRuntime(runtimeID string) {
	if r.Runtime == nil || runtimeID == "" {
		return
	}
	if err := r.Runtime.Stop(context.Background(), runtimeID); err != nil {
		slog.Debug("stop job runtime", "runtime_id", runtimeID, "error", err)
	}
}

// SignalJobRuntime delivers a single signal to the runtime's process group
// without any SIGKILL follow-up. NotifyTask uses this for SIGUSR1 to ask the
// agent (run-agent.py) to stop the claude session gracefully — bash and the
// EXIT trap stay alive (via `trap '' USR1` propagated as SIG_IGN across
// execve), so payload_patch capture and `boid job done --output-file`
// continue through the normal completion path. Best-effort: errors at debug
// level only.
func (r *Runner) SignalJobRuntime(runtimeID string, sig syscall.Signal) {
	if r.Runtime == nil || runtimeID == "" {
		return
	}
	if err := r.Runtime.Signal(context.Background(), runtimeID, sig); err != nil {
		slog.Debug("signal job runtime", "runtime_id", runtimeID, "signal", sig, "error", err)
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
//
// A non-zero exit is NOT reported as an error — the caller inspects
// result.ExitCode. Only true wait-machinery failures (ctx cancel) produce a
// non-nil error. This lets the orchestrator record `hook_fired` actions for
// failing hooks the same way as successful ones; prior behavior discarded
// the partial FiredEvents when any hook exited non-zero.
func (r *Runner) WaitForJobCtx(ctx context.Context, jobID string) (JobCompletionResult, error) {
	ch := r.WaitForJob(jobID)
	select {
	case result := <-ch:
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

	// transcript size を一緒に出すと、 0 byte なら「子プロセスが PTY に何も書け
	// ずに死んだ silent failure」と即時に判別できる。 transcript path は
	// retainSandboxArtifacts と合わせて事後解析の起点になる。
	transcriptSize, transcriptErr := transcriptSizeBytes(result.TranscriptPath)
	slog.Warn("runtime exited before boid job done",
		"job_id", jobID,
		"runtime_id", runtimeID,
		"exit_code", result.ExitCode,
		"transcript_path", result.TranscriptPath,
		"transcript_size", transcriptSize,
		"transcript_stat_error", transcriptErr,
	)
}

// transcriptSizeBytes は transcript.log のサイズを返す。 path が空 / stat 失敗
// の場合は (-1, error message) を返す。 watchRuntime の log で silent failure
// を判別するために使う。
func transcriptSizeBytes(path string) (int64, string) {
	if path == "" {
		return -1, "no transcript path"
	}
	info, err := os.Stat(path)
	if err != nil {
		return -1, err.Error()
	}
	return info.Size(), ""
}
