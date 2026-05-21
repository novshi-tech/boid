package server

import (
	"database/sql"
	"os/exec"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

type apiTxStore struct {
	tasks   *orchestrator.TaskRepository
	actions *orchestrator.TaskRepository
	jobs    *dispatcher.JobRepository
}

func (s apiTxStore) CreateTask(task *orchestrator.Task) error {
	return s.tasks.CreateTask(task)
}

func (s apiTxStore) GetTask(id string) (*orchestrator.Task, error) {
	return s.tasks.GetTask(id)
}

func (s apiTxStore) ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	return s.tasks.ListTasks(filter)
}

func (s apiTxStore) UpdateTask(task *orchestrator.Task) error {
	return s.tasks.UpdateTask(task)
}

func (s apiTxStore) DeleteTask(id string) error {
	return s.tasks.DeleteTask(id)
}

func (s apiTxStore) FindTaskByRemote(remoteID string) (*orchestrator.Task, error) {
	return s.tasks.FindTaskByRemote(remoteID)
}

func (s apiTxStore) FindTaskByRef(ref, parentID string) (*orchestrator.Task, error) {
	return s.tasks.FindTaskByRef(ref, parentID)
}

func (s apiTxStore) FindDependentTasks(taskID string) ([]*orchestrator.Task, error) {
	return s.tasks.FindDependentTasks(taskID)
}

func (s apiTxStore) CreateAction(action *orchestrator.Action) error {
	return s.actions.CreateAction(action)
}

func (s apiTxStore) ListActionsByTask(taskID string) ([]*orchestrator.Action, error) {
	return s.actions.ListActionsByTask(taskID)
}

func (s apiTxStore) GetJob(id string) (*api.Job, error) {
	job, err := s.jobs.GetJob(id)
	if err != nil {
		return nil, err
	}
	return toAPIJob(job), nil
}

func (s apiTxStore) ListJobsByTask(taskID string) ([]*api.Job, error) {
	jobs, err := s.jobs.ListJobsByTask(taskID)
	if err != nil {
		return nil, err
	}
	out := make([]*api.Job, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, toAPIJob(job))
	}
	return out, nil
}

func (s apiTxStore) UpdateJob(job *api.Job) error {
	return s.jobs.UpdateJob(toDispatcherJob(job))
}

type apiTransactor struct {
	db *sql.DB
}

func (t apiTransactor) WithinTx(fn func(api.TxStore) error) error {
	return db.InTxDB(t.db, func(tx db.DBTX) error {
		store := apiTxStore{
			tasks:   orchestrator.NewTaskRepository(tx),
			actions: orchestrator.NewTaskRepository(tx),
			jobs:    dispatcher.NewJobRepository(tx),
		}
		return fn(store)
	})
}

type brokerRegistry struct {
	broker      dispatcher.CommandBroker
	projects    api.ProjectRepository
	secretStore *dispatcher.SecretStore
}

func (r brokerRegistry) RegisterBrokerCommands(commands map[string]orchestrator.HostCommandSpec, builtinPolicies map[string]sandbox.BuiltinPolicy, projectID string) (*api.BrokerRegisterResponse, error) {
	if r.broker == nil {
		return nil, sql.ErrConnDone
	}
	if r.projects == nil {
		return nil, sql.ErrConnDone
	}
	project, err := r.projects.GetProject(projectID)
	if err != nil {
		return nil, err
	}
	allowedProjectIDs, err := r.allowedProjectIDs(project)
	if err != nil {
		return nil, err
	}

	ctx := sandbox.TokenContext{
		Role:              "gate",
		ProjectID:         project.ID,
		WorkspaceID:       project.WorkspaceID,
		AllowedProjectIDs: allowedProjectIDs,
		ProjectDir:        project.WorkDir,
	}
	defs := orchestrator.HostCommands(commands).ToCommandDefs()
	resolved, err := dispatcher.ResolveHostCommands(
		sortedBuiltinKeys(builtinPolicies),
		defs,
		project.WorkDir,
		exec.LookPath,
	)
	if err != nil {
		return nil, err
	}

	var resolve dispatcher.SecretResolver
	if r.secretStore != nil {
		resolve = func(key string) (string, error) {
			return r.secretStore.Get("default", key)
		}
	}
	token := r.broker.RegisterCommands(resolved, builtinPolicies, ctx, resolve)
	return &api.BrokerRegisterResponse{
		Token:                token,
		Socket:               r.broker.SocketPath(),
		ResolvedHostCommands: resolved,
	}, nil
}

func (r brokerRegistry) allowedProjectIDs(project *orchestrator.Project) ([]string, error) {
	if project == nil {
		return nil, sql.ErrNoRows
	}
	projects, err := r.projects.ListProjects()
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	allowed := []string{project.ID}
	seen[project.ID] = struct{}{}

	if project.WorkspaceID == "" {
		return allowed, nil
	}

	peers := make([]string, 0, len(projects))
	for _, candidate := range projects {
		if candidate == nil || candidate.ID == "" {
			continue
		}
		if candidate.ID == project.ID || candidate.WorkspaceID != project.WorkspaceID {
			continue
		}
		if _, ok := seen[candidate.ID]; ok {
			continue
		}
		peers = append(peers, candidate.ID)
		seen[candidate.ID] = struct{}{}
	}
	sort.Strings(peers)
	return append(allowed, peers...), nil
}

func toAPIJob(job *dispatcher.Job) *api.Job {
	if job == nil {
		return nil
	}
	return &api.Job{
		ID:             job.ID,
		TaskID:         job.TaskID,
		ProjectID:      job.ProjectID,
		HandlerID:      job.HandlerID,
		DisplayName:    job.DisplayName,
		Role:           job.Role,
		RuntimeID:      job.RuntimeID,
		Interactive:    job.Interactive,
		TTY:            job.TTY,
		Status:         api.JobStatus(job.Status),
		ExitCode:       job.ExitCode,
		Output:         job.Output,
		ExecutionState: job.ExecutionState,
		CreatedAt:      job.CreatedAt,
		UpdatedAt:      job.UpdatedAt,
	}
}

func toDispatcherJob(job *api.Job) *dispatcher.Job {
	if job == nil {
		return nil
	}
	return &dispatcher.Job{
		ID:             job.ID,
		TaskID:         job.TaskID,
		ProjectID:      job.ProjectID,
		HandlerID:      job.HandlerID,
		DisplayName:    job.DisplayName,
		Role:           job.Role,
		RuntimeID:      job.RuntimeID,
		Interactive:    job.Interactive,
		TTY:            job.TTY,
		Status:         dispatcher.JobStatus(job.Status),
		ExitCode:       job.ExitCode,
		Output:         job.Output,
		ExecutionState: job.ExecutionState,
		CreatedAt:      job.CreatedAt,
		UpdatedAt:      job.UpdatedAt,
	}
}

type jobStoreAdapter struct {
	repo *dispatcher.JobRepository
}

func (a jobStoreAdapter) GetJob(id string) (*api.Job, error) {
	job, err := a.repo.GetJob(id)
	if err != nil {
		return nil, err
	}
	return toAPIJob(job), nil
}

func (a jobStoreAdapter) ListJobsByTask(taskID string) ([]*api.Job, error) {
	jobs, err := a.repo.ListJobsByTask(taskID)
	if err != nil {
		return nil, err
	}
	out := make([]*api.Job, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, toAPIJob(job))
	}
	return out, nil
}

func (a jobStoreAdapter) UpdateJob(job *api.Job) error {
	return a.repo.UpdateJob(toDispatcherJob(job))
}

// globalJobStore implements api.GlobalJobStore for cross-task job listing with context.
type globalJobStore struct {
	jobs     *dispatcher.JobRepository
	tasks    *orchestrator.TaskRepository
	projects *orchestrator.ProjectRepository
}

func (s *globalJobStore) ListJobsWithContext(filter api.JobListFilter) ([]api.JobWithContext, error) {
	jobs, err := s.jobs.ListJobsFiltered(dispatcher.JobFilter{
		Status:      filter.Status,
		Interactive: filter.Interactive,
	})
	if err != nil {
		return nil, err
	}

	result := make([]api.JobWithContext, 0, len(jobs))
	for _, job := range jobs {
		jwc := api.JobWithContext{Job: *toAPIJob(job)}
		if task, err := s.tasks.GetTask(job.TaskID); err == nil {
			jwc.TaskTitle = task.Title
		}
		if project, err := s.projects.GetProject(job.ProjectID); err == nil {
			jwc.ProjectName = filepath.Base(project.WorkDir)
		}
		result = append(result, jwc)
	}
	return result, nil
}

// transcriptLogReader implements api.JobLogReader by reading transcript files from disk.
type transcriptLogReader struct {
	rootDir string
}

func (r transcriptLogReader) ReadJobLog(runtimeID string) ([]byte, error) {
	return dispatcher.ReadTranscript(r.rootDir, runtimeID)
}

func (r transcriptLogReader) StatJobLog(runtimeID string) (int64, time.Time, error) {
	fi, err := dispatcher.StatTranscript(r.rootDir, runtimeID)
	if err != nil {
		return 0, time.Time{}, err
	}
	return fi.Size(), fi.ModTime(), nil
}

type jobLifecycleAdapter struct {
	runner *dispatcher.Runner
}

func (a jobLifecycleAdapter) CompleteJob(jobID string, result api.JobCompletion) {
	a.runner.CompleteJob(jobID, dispatcher.JobCompletionResult{
		Output:   result.Output,
		ExitCode: result.ExitCode,
	})
}

func (a jobLifecycleAdapter) UnregisterJob(jobID string) {
	a.runner.UnregisterJob(jobID)
}

func (a jobLifecycleAdapter) CleanupTaskWindow(taskID string) {
	a.runner.CleanupTaskWindow(taskID)
}

func (a jobLifecycleAdapter) StopJobRuntime(runtimeID string) {
	a.runner.StopJobRuntime(runtimeID)
}

func (a jobLifecycleAdapter) SignalJobRuntime(runtimeID string, sig syscall.Signal) {
	a.runner.SignalJobRuntime(runtimeID, sig)
}

// hubJobEventSink lets the dispatcher runner push job-created events into
// the web SSE hub. Kept tiny — it only exists to decouple dispatcher from
// internal/api imports while letting the timeline refresh live.
type hubJobEventSink struct {
	hub *api.TaskEventHub
}

func sortedBuiltinKeys(m map[string]sandbox.BuiltinPolicy) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (s hubJobEventSink) JobCreated(taskID, jobID string) {
	if s.hub == nil || taskID == "" {
		return
	}
	s.hub.Broadcast(taskID, api.TaskEvent{
		Kind: "job",
		Payload: map[string]any{
			"job_id":     jobID,
			"new_status": "running",
		},
	})
}
