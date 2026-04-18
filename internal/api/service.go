package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string {
	return e.Message
}

type ActionApplication struct {
	Task         *orchestrator.Task   `json:"task"`
	Action       *orchestrator.Action `json:"action"`
	MatchedHooks []string             `json:"matched_hooks,omitempty"`
}

type ProjectReloadResult struct {
	Status string   `json:"status"`
	Errors []string `json:"errors,omitempty"`
}

// TaskNode represents a node in the recursive dependency tree.
type TaskNode struct {
	Task     *orchestrator.Task `json:"task"`
	Children []*TaskNode        `json:"children,omitempty"`
}

type TaskDetailView struct {
	Task              *orchestrator.Task
	Actions           []*orchestrator.Action
	Jobs              []*Job
	AvailableActions  []string             `json:"available_actions"`
	Dependents        []*orchestrator.Task `json:"dependents,omitempty"`
	DependsOnResolved []*orchestrator.Task `json:"depends_on_resolved,omitempty"`
	DependsOnTree     []*TaskNode          `json:"depends_on_tree,omitempty"`
	DependentsTree    []*TaskNode          `json:"dependents_tree,omitempty"`
}

// buildDependsOnTree recursively builds the upstream (DependsOn) tree.
// visited prevents infinite loops on circular dependencies.
func buildDependsOnTree(store TaskStore, ids []string, visited map[string]bool) []*TaskNode {
	var nodes []*TaskNode
	for _, id := range ids {
		if visited[id] {
			continue
		}
		visited[id] = true
		task, err := store.GetTask(id)
		if err != nil {
			continue
		}
		children := buildDependsOnTree(store, task.DependsOn, visited)
		nodes = append(nodes, &TaskNode{Task: task, Children: children})
	}
	return nodes
}

// buildDependentsTree recursively builds the downstream (Dependents) tree.
// visited prevents infinite loops on circular dependencies.
func buildDependentsTree(store TaskStore, taskID string, visited map[string]bool) []*TaskNode {
	dependents, err := store.FindDependentTasks(taskID)
	if err != nil || len(dependents) == 0 {
		return nil
	}
	var nodes []*TaskNode
	for _, t := range dependents {
		if visited[t.ID] {
			continue
		}
		visited[t.ID] = true
		children := buildDependentsTree(store, t.ID, visited)
		nodes = append(nodes, &TaskNode{Task: t, Children: children})
	}
	return nodes
}

type ProjectAppService struct {
	Projects ProjectRepository
	Meta     interface {
		Load(workDir string) (*orchestrator.ProjectMeta, error)
		Get(id string) (*orchestrator.ProjectMeta, bool)
		Remove(id string)
		LoadAll(projects []*orchestrator.Project) []error
	}
}

func (s *ProjectAppService) hydrateProject(project *orchestrator.Project) *orchestrator.Project {
	if project == nil {
		return nil
	}
	if meta, ok := s.Meta.Get(project.ID); ok {
		project.Meta = *meta
	}
	return project
}

func (s *ProjectAppService) CreateProject(workDir string) (*orchestrator.Project, error) {
	meta, err := s.Meta.Load(workDir)
	if err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}

	project := &orchestrator.Project{
		ID:      meta.ID,
		WorkDir: workDir,
	}
	if err := s.Projects.CreateProject(project); err != nil {
		s.Meta.Remove(meta.ID)
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	project.Meta = *meta
	return project, nil
}

func (s *ProjectAppService) ListProjects(workspaceID string) ([]*orchestrator.Project, error) {
	projects, err := s.Projects.ListProjects()
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	var result []*orchestrator.Project
	for _, project := range projects {
		s.hydrateProject(project)
		if workspaceID != "" && project.WorkspaceID != workspaceID {
			continue
		}
		result = append(result, project)
	}
	if result == nil {
		result = []*orchestrator.Project{}
	}
	return result, nil
}

func (s *ProjectAppService) GetProject(id string) (*orchestrator.Project, error) {
	project, err := s.Projects.GetProject(id)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	return s.hydrateProject(project), nil
}

func (s *ProjectAppService) SetProjectWorkspace(id, workspaceID string) (*orchestrator.Project, error) {
	project, err := s.Projects.GetProject(id)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	if err := s.Projects.SetProjectWorkspace(id, workspaceID); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	project.WorkspaceID = workspaceID
	return s.hydrateProject(project), nil
}

func (s *ProjectAppService) ListWorkspaces() ([]*orchestrator.WorkspaceSummary, error) {
	workspaces, err := s.Projects.ListWorkspaces()
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	if workspaces == nil {
		workspaces = []*orchestrator.WorkspaceSummary{}
	}
	return workspaces, nil
}

func (s *ProjectAppService) DeleteProject(id string) error {
	if err := s.Projects.DeleteProject(id); err != nil {
		return &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	s.Meta.Remove(id)
	return nil
}

// ResolveProjectRef resolves ref to matching projects with the following priority:
//  1. id exact match (returns immediately on first hit)
//  2. name exact match (all projects with that name)
//  3. name substring match, case-insensitive
//
// Returns a single-element slice on unambiguous match, a multi-element slice on
// ambiguous match, or StatusError{404} when nothing matches.
func (s *ProjectAppService) ResolveProjectRef(ref string) ([]*orchestrator.Project, error) {
	projects, err := s.Projects.ListProjects()
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	// Hydrate all projects so Meta.Name is available for name matching.
	for _, p := range projects {
		s.hydrateProject(p)
	}

	// 1. id exact match — highest priority, return immediately.
	for _, p := range projects {
		if p.ID == ref {
			return []*orchestrator.Project{p}, nil
		}
	}

	// 2. name exact match.
	var nameExact []*orchestrator.Project
	for _, p := range projects {
		if p.Meta.Name == ref {
			nameExact = append(nameExact, p)
		}
	}
	if len(nameExact) > 0 {
		return nameExact, nil
	}

	// 3. name substring match (case-insensitive).
	refLower := strings.ToLower(ref)
	var namePartial []*orchestrator.Project
	for _, p := range projects {
		if strings.Contains(strings.ToLower(p.Meta.Name), refLower) {
			namePartial = append(namePartial, p)
		}
	}
	if len(namePartial) > 0 {
		return namePartial, nil
	}

	return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("no project matches ref %q", ref)}
}

func (s *ProjectAppService) GetCommand(id, name string) (*CommandResponse, error) {
	meta, ok := s.Meta.Get(id)
	if !ok {
		return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("project %q meta not loaded", id)}
	}
	cmd, ok := meta.Commands[name]
	if !ok {
		return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("command %q not found", name)}
	}
	return &CommandResponse{
		Command:            cmd.ResolvedCommand,
		Env:                cmd.Env,
		BuiltinCommands:    cmd.BuiltinCommands,
		HostCommands:       map[string]orchestrator.HostCommandSpec(cmd.HostCommands),
		AdditionalBindings: cmd.AdditionalBindings,
	}, nil
}

func (s *ProjectAppService) ListCommands(id string) ([]CommandSummary, error) {
	meta, ok := s.Meta.Get(id)
	if !ok {
		return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("project %q meta not loaded", id)}
	}
	summaries := make([]CommandSummary, 0, len(meta.Commands))
	for name, cmd := range meta.Commands {
		summaries = append(summaries, CommandSummary{Name: name, Command: cmd.ResolvedCommand})
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].Name < summaries[j].Name })
	return summaries, nil
}

func (s *ProjectAppService) ReloadProjects() (*ProjectReloadResult, error) {
	projects, err := s.Projects.ListProjects()
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	errs := s.Meta.LoadAll(projects)
	if len(errs) == 0 {
		return &ProjectReloadResult{Status: "ok"}, nil
	}

	messages := make([]string, 0, len(errs))
	for _, err := range errs {
		messages = append(messages, err.Error())
	}
	return &ProjectReloadResult{
		Status: "partial",
		Errors: messages,
	}, nil
}

type TaskAppService struct {
	Tasks       TaskStore
	Actions     ActionStore
	Jobs        JobStore
	Meta        MetaStore
	Workflow    WorkflowService
	RuntimesDir string
}

// enrichJob fills WorkspacePath from RuntimesDir and the job's RuntimeID.
// If either is empty the field is left unchanged (omitempty will omit it in JSON).
func enrichJob(runtimesDir string, job *Job) {
	if runtimesDir == "" || job.RuntimeID == "" {
		return
	}
	job.WorkspacePath = filepath.Join(runtimesDir, job.RuntimeID)
}

// behaviorResolution holds the resolved behavior fields after processing either
// a named behavior or an inline behavior_spec.
type behaviorResolution struct {
	behaviorName string
	traits       []string
	readonly     bool
	worktree     bool
	branchPrefix string
	baseBranch   string
	payload      json.RawMessage
	instructions map[string]orchestrator.Instruction
}

// resolveBehavior validates and resolves behavior fields from a CreateTaskRequest.
// It handles both the named behavior path (meta lookup) and the inline behavior_spec path.
func resolveBehavior(meta *orchestrator.ProjectMeta, req CreateTaskRequest) (*behaviorResolution, error) {
	if req.Behavior == "" && req.BehaviorSpec == nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "either behavior or behavior_spec is required"}
	}
	if req.Behavior != "" && req.BehaviorSpec != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "behavior and behavior_spec are mutually exclusive"}
	}

	res := &behaviorResolution{payload: req.Payload}

	if req.BehaviorSpec != nil {
		spec := req.BehaviorSpec
		if spec.Name == "" {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: "behavior_spec.name is required"}
		}
		if err := orchestrator.ValidateDefaultPayloadNoInstructions(spec.DefaultPayload); err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: "behavior_spec.default_payload: " + err.Error()}
		}
		res.behaviorName = spec.Name
		res.traits = spec.Traits
		res.readonly = spec.Readonly
		res.worktree = spec.Worktree
		res.branchPrefix = spec.BranchPrefix
		res.baseBranch = spec.BaseBranch
		if len(spec.DefaultPayload) > 0 {
			merged, err := orchestrator.MergeDefaultPayload(spec.DefaultPayload.RawMessage(), req.Payload)
			if err != nil {
				return nil, &StatusError{Code: http.StatusBadRequest, Message: "payload merge: " + err.Error()}
			}
			res.payload = merged
		}
		mergedInstructions, err := orchestrator.MergeDefaultInstructions(spec.DefaultInstructions, req.Instructions)
		if err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: "instructions merge: " + err.Error()}
		}
		res.instructions = mergedInstructions
		return res, nil
	}

	// Named behavior path (existing logic).
	res.behaviorName = req.Behavior
	if meta != nil {
		behavior, ok := meta.TaskBehaviors[req.Behavior]
		if !ok {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("behavior %q not found", req.Behavior)}
		}
		res.traits = behavior.Traits
		res.readonly = behavior.Readonly
		res.worktree = behavior.Worktree
		res.branchPrefix = behavior.BranchPrefix
		res.baseBranch = behavior.BaseBranch
		if len(behavior.DefaultPayload) > 0 {
			merged, err := orchestrator.MergeDefaultPayload(behavior.DefaultPayload.RawMessage(), req.Payload)
			if err != nil {
				return nil, &StatusError{Code: http.StatusBadRequest, Message: "payload merge: " + err.Error()}
			}
			res.payload = merged
		}
		mergedInstructions, err := orchestrator.MergeDefaultInstructions(behavior.DefaultInstructions, req.Instructions)
		if err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: "instructions merge: " + err.Error()}
		}
		res.instructions = mergedInstructions
	} else if len(req.Instructions) > 0 {
		mergedInstructions, err := orchestrator.MergeDefaultInstructions(nil, req.Instructions)
		if err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: "instructions merge: " + err.Error()}
		}
		res.instructions = mergedInstructions
	}
	return res, nil
}

func (s *TaskAppService) CreateTask(req CreateTaskRequest) (*orchestrator.Task, error) {
	var meta *orchestrator.ProjectMeta
	if s.Meta != nil {
		if m, ok := s.Meta.Get(req.ProjectID); ok {
			meta = m
		}
	}

	res, err := resolveBehavior(meta, req)
	if err != nil {
		return nil, err
	}

	traits := res.traits
	readonly := res.readonly
	worktree := res.worktree
	branchPrefix := res.branchPrefix
	baseBranch := res.baseBranch
	payload := res.payload

	if req.Traits != nil {
		traits = req.Traits
	}
	if req.Readonly != nil {
		readonly = *req.Readonly
	}
	if req.Worktree != nil {
		worktree = *req.Worktree
	}
	if req.BranchPrefix != nil {
		branchPrefix = *req.BranchPrefix
	}
	if req.BaseBranch != nil {
		baseBranch = *req.BaseBranch
	}

	var resolvedDeps []string
	for _, dep := range req.DependsOn {
		t, err := s.Tasks.FindTaskByRef(dep, req.ParentID)
		if err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("depends_on: ref %q lookup failed: %v", dep, err)}
		}
		if t == nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("depends_on: ref %q not found (parent_id: %s)", dep, req.ParentID)}
		}
		resolvedDeps = append(resolvedDeps, t.ID)
	}

	task := &orchestrator.Task{
		ID:               req.ID,
		ProjectID:        req.ProjectID,
		Title:            req.Title,
		Description:      req.Description,
		Behavior:         res.behaviorName,
		Traits:           traits,
		Readonly:         readonly,
		Worktree:         worktree,
		BranchPrefix:     branchPrefix,
		BaseBranch:       baseBranch,
		RemoteID:         req.RemoteID,
		DataSourceID:     req.DataSourceID,
		Payload:          payload,
		Instructions:     res.instructions,
		AutoStart:        req.AutoStart,
		DependsOn:        resolvedDeps,
		DependsOnPayload: req.DependsOnPayload,
		Ref:              req.Ref,
		ParentID:         req.ParentID,
	}
	if err := s.Tasks.CreateTask(task); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	if req.AutoStart && s.Workflow != nil {
		result, err := s.Workflow.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "start"})
		if err != nil {
			slog.Error("auto_start: failed to apply start action", "task_id", task.ID, "error", err)
		} else {
			task = result.Task
		}
	}
	return task, nil
}

func (s *TaskAppService) ImportTasks(reqs []CreateTaskRequest) (*ImportResult, error) {
	result := &ImportResult{Errors: []ImportError{}}
	for i, req := range reqs {
		if req.RemoteID == "" && req.DataSourceID == "" {
			result.Errors = append(result.Errors, ImportError{
				Line:     i + 1,
				RemoteID: req.RemoteID,
				Error:    "remote_id and datasource_id are required",
			})
			continue
		}

		existing, err := s.Tasks.FindTaskByRemote(req.RemoteID, req.DataSourceID)
		if err != nil {
			result.Errors = append(result.Errors, ImportError{Line: i + 1, RemoteID: req.RemoteID, Error: err.Error()})
			continue
		}
		if existing != nil {
			result.Skipped++
			continue
		}

		if _, err := s.CreateTask(req); err != nil {
			msg := err.Error()
			if se, ok := err.(*StatusError); ok {
				msg = se.Message
			}
			result.Errors = append(result.Errors, ImportError{Line: i + 1, RemoteID: req.RemoteID, Error: msg})
			continue
		}
		result.Created++
	}
	return result, nil
}

func (s *TaskAppService) ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	tasks, err := s.Tasks.ListTasks(filter)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	if tasks == nil {
		tasks = []*orchestrator.Task{}
	}
	return tasks, nil
}

func (s *TaskAppService) GetTask(id string) (*orchestrator.Task, error) {
	task, err := s.Tasks.GetTask(id)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	return task, nil
}

func (s *TaskAppService) UpdateTask(id string, req UpdateTaskRequest) (*orchestrator.Task, error) {
	task, err := s.Tasks.GetTask(id)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	if req.Title != "" {
		task.Title = req.Title
	}
	if req.Description != "" {
		task.Description = req.Description
	}
	payloadUpdated := false
	if len(req.Payload) > 0 {
		if err := rejectPayloadInstructions(req.Payload); err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
		}
		if err := orchestrator.RejectReservedPayloadKeys(req.Payload); err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
		}
		// 案 B: artifact.<handler-role> が別 top-level キーになるため、
		// top-level shallow merge で handler 間の書き込みが衝突しない。
		// null は削除。instructions の特別扱いは不要。
		var base map[string]json.RawMessage
		if len(task.Payload) > 0 && string(task.Payload) != "null" {
			if err := json.Unmarshal(task.Payload, &base); err != nil {
				return nil, &StatusError{Code: http.StatusBadRequest, Message: "payload parse: " + err.Error()}
			}
		}
		if base == nil {
			base = make(map[string]json.RawMessage)
		}
		var override map[string]json.RawMessage
		if err := json.Unmarshal(req.Payload, &override); err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: "payload merge: " + err.Error()}
		}
		for k, v := range override {
			if string(v) == "null" {
				delete(base, k)
			} else {
				base[k] = v
			}
		}
		merged, err := json.Marshal(base)
		if err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: "payload merge: " + err.Error()}
		}
		task.Payload = merged
		payloadUpdated = true
	}
	var instructionsBefore map[string]orchestrator.Instruction
	if len(req.Instructions) > 0 {
		if !isInstructionsEditable(task.Status) {
			return nil, &StatusError{
				Code:    http.StatusConflict,
				Message: fmt.Sprintf("cannot edit instructions while task is running (status: %s)", task.Status),
			}
		}
		instructionsBefore = cloneInstructions(task.Instructions)
		merged, err := orchestrator.MergeDefaultInstructions(task.Instructions, req.Instructions)
		if err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: "instructions merge: " + err.Error()}
		}
		task.Instructions = merged
	}
	if err := s.Tasks.UpdateTask(task); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	if instructionsBefore != nil {
		s.auditInstructionsChange(task.ID, instructionsBefore, task.Instructions)
	}
	if payloadUpdated && s.Workflow != nil {
		go s.Workflow.TriggerDependents(context.Background(), id)
	}
	return task, nil
}

// isInstructionsEditable reports whether a task's instructions can be edited
// in its current status. Editing is only allowed while the task is stopped
// (pending/done/aborted) to avoid racing with in-flight handlers.
func isInstructionsEditable(status orchestrator.TaskStatus) bool {
	switch status {
	case orchestrator.TaskStatusPending,
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusAborted:
		return true
	}
	return false
}

// rejectPayloadInstructions is the local shim around orchestrator's validation
// so that API layer can report 400 on payload containing "instructions" key.
func rejectPayloadInstructions(payload json.RawMessage) error {
	return orchestrator.RejectPayloadInstructions(payload)
}

func (s *TaskAppService) DeleteTask(id string, force bool) error {
	task, err := s.Tasks.GetTask(id)
	if err != nil {
		return &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	if !force {
		switch task.Status {
		case orchestrator.TaskStatusExecuting,
			orchestrator.TaskStatusReworking,
			orchestrator.TaskStatusVerifying:
			return &StatusError{
				Code:    http.StatusConflict,
				Message: "task is active (status: " + string(task.Status) + "); use --force to delete",
			}
		}
	}
	if err := s.Tasks.DeleteTask(id); err != nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	return nil
}

// computeAvailableActions returns the list of manual actions applicable to the task's current status.
func computeAvailableActions(task *orchestrator.Task) []string {
	return orchestrator.DefaultMachine().AvailableActions(task.Status)
}

func (s *TaskAppService) DuplicateTask(sourceID string, autoStart bool) (*orchestrator.Task, error) {
	source, err := s.GetTask(sourceID)
	if err != nil {
		return nil, err
	}
	req := CreateTaskRequest{
		ProjectID:   source.ProjectID,
		Title:       source.Title,
		Description: source.Description,
		Behavior:    source.Behavior,
		AutoStart:   autoStart,
	}
	return s.CreateTask(req)
}

func (s *TaskAppService) RerunTask(id string, req RerunTaskRequest) (*orchestrator.Task, error) {
	task, err := s.Tasks.GetTask(id)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	if task.Status != orchestrator.TaskStatusDone && task.Status != orchestrator.TaskStatusAborted {
		return nil, &StatusError{
			Code:    http.StatusConflict,
			Message: fmt.Sprintf("task is not in a rerun-able state (status: %s)", task.Status),
		}
	}

	var instructionsBefore map[string]orchestrator.Instruction
	if len(req.InstructionsOverride) > 0 && string(req.InstructionsOverride) != "null" {
		instructionsBefore = cloneInstructions(task.Instructions)
		merged, err := orchestrator.MergeDefaultInstructions(task.Instructions, req.InstructionsOverride)
		if err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: "instructions merge: " + err.Error()}
		}
		task.Instructions = merged
	}

	task.Status = orchestrator.TaskStatusPending
	task.Payload = json.RawMessage("{}")
	if err := s.Tasks.UpdateTask(task); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	if instructionsBefore != nil {
		s.auditInstructionsChange(task.ID, instructionsBefore, task.Instructions)
	}

	if req.AutoStart && s.Workflow != nil {
		result, err := s.Workflow.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "start"})
		if err != nil {
			slog.Error("rerun auto_start: failed to apply start action", "task_id", task.ID, "error", err)
		} else {
			task = result.Task
		}
	}

	return task, nil
}

func cloneInstructions(src map[string]orchestrator.Instruction) map[string]orchestrator.Instruction {
	if src == nil {
		return nil
	}
	out := make(map[string]orchestrator.Instruction, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// auditInstructionsChange records an instructions change as an Action so that
// the reason behind rerun-over-rerun outcome differences can be traced.
func (s *TaskAppService) auditInstructionsChange(taskID string, before, after map[string]orchestrator.Instruction) {
	if s.Actions == nil {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"before": before,
		"after":  after,
	})
	if err != nil {
		slog.Error("audit instructions change: marshal", "task_id", taskID, "error", err)
		return
	}
	action := &orchestrator.Action{
		TaskID:  taskID,
		Type:    "update_instructions",
		Payload: payload,
	}
	if err := s.Actions.CreateAction(action); err != nil {
		slog.Error("audit instructions change: create action", "task_id", taskID, "error", err)
	}
}

func (s *TaskAppService) GetTaskDetail(id string) (*TaskDetailView, error) {
	task, err := s.GetTask(id)
	if err != nil {
		return nil, err
	}

	actions, err := s.Actions.ListActionsByTask(task.ID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	jobs, err := s.Jobs.ListJobsByTask(task.ID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	for _, j := range jobs {
		enrichJob(s.RuntimesDir, j)
	}

	dependents, err := s.Tasks.FindDependentTasks(task.ID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	var dependsOnResolved []*orchestrator.Task
	for _, depID := range task.DependsOn {
		dep, err := s.Tasks.GetTask(depID)
		if err != nil {
			continue
		}
		dependsOnResolved = append(dependsOnResolved, dep)
	}

	visitedUp := map[string]bool{task.ID: true}
	dependsOnTree := buildDependsOnTree(s.Tasks, task.DependsOn, visitedUp)

	visitedDown := map[string]bool{task.ID: true}
	dependentsTree := buildDependentsTree(s.Tasks, task.ID, visitedDown)

	return &TaskDetailView{
		Task:              task,
		Actions:           actions,
		Jobs:              jobs,
		AvailableActions:  computeAvailableActions(task),
		Dependents:        dependents,
		DependsOnResolved: dependsOnResolved,
		DependsOnTree:     dependsOnTree,
		DependentsTree:    dependentsTree,
	}, nil
}

type WebAppService struct {
	Tasks      TaskStore
	Actions    ActionStore
	Jobs       JobStore
	GlobalJobs GlobalJobStore
	Projects   ProjectRepository
	Meta       MetaStore
	Workflow   WorkflowService
}

func (s *WebAppService) ListTasks(status string) ([]*orchestrator.Task, error) {
	return s.Tasks.ListTasks(orchestrator.TaskFilter{Status: status})
}

func (s *WebAppService) GetTaskDetail(id string) (*TaskDetailView, error) {
	task, err := s.Tasks.GetTask(id)
	if err != nil {
		return nil, err
	}

	actions, _ := s.Actions.ListActionsByTask(task.ID)
	jobs, _ := s.Jobs.ListJobsByTask(task.ID)
	dependents, _ := s.Tasks.FindDependentTasks(task.ID)

	var dependsOnResolved []*orchestrator.Task
	for _, depID := range task.DependsOn {
		dep, err := s.Tasks.GetTask(depID)
		if err != nil {
			continue
		}
		dependsOnResolved = append(dependsOnResolved, dep)
	}

	visitedUp := map[string]bool{task.ID: true}
	dependsOnTree := buildDependsOnTree(s.Tasks, task.DependsOn, visitedUp)

	visitedDown := map[string]bool{task.ID: true}
	dependentsTree := buildDependentsTree(s.Tasks, task.ID, visitedDown)

	return &TaskDetailView{
		Task:              task,
		Actions:           actions,
		Jobs:              jobs,
		AvailableActions:  computeAvailableActions(task),
		Dependents:        dependents,
		DependsOnResolved: dependsOnResolved,
		DependsOnTree:     dependsOnTree,
		DependentsTree:    dependentsTree,
	}, nil
}

func (s *WebAppService) ListProjects() ([]*orchestrator.Project, error) {
	projects, err := s.Projects.ListProjects()
	if err != nil {
		return nil, err
	}
	for _, project := range projects {
		if meta, ok := s.Meta.Get(project.ID); ok {
			project.Meta = *meta
		}
	}
	return projects, nil
}

func (s *WebAppService) DuplicateTask(id string) (string, error) {
	task, err := s.Tasks.GetTask(id)
	if err != nil {
		return "", &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	newTask := &orchestrator.Task{
		ProjectID:    task.ProjectID,
		Title:        task.Title,
		Description:  task.Description,
		Behavior:     task.Behavior,
		Traits:       task.Traits,
		Readonly:     task.Readonly,
		Worktree:     task.Worktree,
		BranchPrefix: task.BranchPrefix,
		BaseBranch:   task.BaseBranch,
		Payload:      task.Payload,
	}
	if err := s.Tasks.CreateTask(newTask); err != nil {
		return "", &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	return newTask.ID, nil
}

func (s *WebAppService) ApplyAction(taskID string, actionType string) error {
	if s.Workflow == nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: "workflow service not configured"}
	}
	_, err := s.Workflow.ApplyAction(context.Background(), taskID, ApplyActionRequest{Type: actionType})
	return err
}

func (s *WebAppService) ListJobs(status string) ([]JobWithContext, error) {
	jobs, err := s.GlobalJobs.ListJobsWithContext(JobListFilter{Status: status})
	if err != nil {
		return nil, err
	}
	if jobs == nil {
		jobs = []JobWithContext{}
	}
	return jobs, nil
}

func (s *WebAppService) GetJob(id string) (*JobWithContext, error) {
	job, err := s.Jobs.GetJob(id)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	result := &JobWithContext{Job: *job}
	if task, err := s.Tasks.GetTask(job.TaskID); err == nil {
		result.TaskTitle = task.Title
	}
	return result, nil
}

type TaskWorkflowService struct {
	Tasks       TaskStore
	Jobs        JobStore
	Projects    ProjectRepository
	Tx          Transactor
	Meta        MetaStore
	Coordinator DispatchCoordinator
	Lifecycle   JobLifecycle
	Worktrees   WorktreeCleaner

	dispatchCtx    context.Context
	dispatchCancel context.CancelFunc
	dispatchWG     sync.WaitGroup
}

// InitDispatch initialises the lifecycle context used by dispatch-loop
// goroutines. Must be called before the first action is applied. The returned
// cancel is stored internally; call Shutdown to invoke it.
func (s *TaskWorkflowService) InitDispatch(ctx context.Context) {
	s.dispatchCtx, s.dispatchCancel = context.WithCancel(ctx)
}

// Shutdown cancels the dispatch context and blocks until all in-flight dispatch
// loops have returned. Call this before closing the database.
func (s *TaskWorkflowService) Shutdown() {
	if s.dispatchCancel != nil {
		s.dispatchCancel()
	}
	s.dispatchWG.Wait()
}

func (s *TaskWorkflowService) ApplyAction(ctx context.Context, taskID string, req ApplyActionRequest) (*ActionApplication, error) {
	task, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}

	meta, ok := s.Meta.Get(task.ProjectID)
	if !ok {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "project meta not loaded: " + task.ProjectID}
	}

	sm := orchestrator.DefaultMachine()

	if req.Type == "start" {
		if err := checkDependencies(task, s.Tasks.GetTask); err != nil {
			return nil, &StatusError{Code: http.StatusConflict, Message: "dependency not satisfied: " + err.Error()}
		}
	}

	fromStatus := task.Status
	action := &orchestrator.Action{
		TaskID:  task.ID,
		Type:    req.Type,
		Payload: req.Payload,
	}
	newTask, err := sm.Apply(task, action)
	if err != nil {
		return nil, &StatusError{Code: http.StatusConflict, Message: err.Error()}
	}
	action.FromStatus = fromStatus
	action.ToStatus = newTask.Status

	merged, err := orchestrator.MergePayload(task.Payload, action.Payload)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "payload merge: " + err.Error()}
	}
	newTask.Payload = merged

	if err := s.Tx.WithinTx(func(tx TxStore) error {
		if err := tx.UpdateTask(newTask); err != nil {
			return err
		}
		return tx.CreateAction(action)
	}); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	s.cleanupWorktree(newTask.ID, task.ProjectID, newTask.Status)

	if s.Coordinator != nil {
		dispatchCtx := s.dispatchCtx
		if dispatchCtx == nil {
			dispatchCtx = context.Background()
		}
		s.dispatchWG.Add(1)
		go func() {
			defer s.dispatchWG.Done()
			s.runDispatchLoop(dispatchCtx, newTask, meta, sm)
		}()
	}

	var matchedHooks []string
	if s.Coordinator != nil {
		if coord, ok := s.Coordinator.(*orchestrator.Coordinator); ok && coord.Evaluator != nil {
			if behavior, found := meta.TaskBehaviors[newTask.Behavior]; found {
				for _, hook := range coord.Evaluator.Evaluate(newTask, behavior.Hooks) {
					matchedHooks = append(matchedHooks, hook.ID)
				}
			}
		}
	}

	return &ActionApplication{
		Task:         newTask,
		Action:       action,
		MatchedHooks: matchedHooks,
	}, nil
}

func (s *TaskWorkflowService) CompleteJob(_ context.Context, jobID string, req JobDoneRequest) (*Job, error) {
	job, err := s.Jobs.GetJob(jobID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}

	if req.ExitCode == 0 {
		job.Status = JobStatusCompleted
	} else {
		job.Status = JobStatusFailed
	}
	job.ExitCode = req.ExitCode
	job.Output = req.Output

	finalize := func() {
		if s.Lifecycle == nil {
			return
		}
		s.Lifecycle.CompleteJob(job.ID, JobCompletion{
			Output:   req.Output,
			ExitCode: req.ExitCode,
		})
		s.Lifecycle.UnregisterJob(job.ID)
	}

	if err := s.Jobs.UpdateJob(job); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	defer finalize()

	// Successful job completion: no state transition here.
	// The runDispatchLoop (hooks → gates → auto-advance) is responsible for
	// evaluating conditions and advancing the task state once all handlers
	// have completed. Transitioning in CompleteJob would race with the gate
	// execution and clean up the worktree before gates can run.
	if req.ExitCode == 0 {
		return job, nil
	}

	// Failed job: apply job_failed → aborted.
	task, err := s.Tasks.GetTask(job.TaskID)
	if err != nil {
		slog.Error("job done: task not found", "task_id", job.TaskID)
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "task not found: " + err.Error()}
	}

	if _, ok := s.Meta.Get(job.ProjectID); !ok {
		slog.Error("job done: project meta not loaded", "project_id", job.ProjectID)
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "project meta not loaded: " + job.ProjectID}
	}

	sm := orchestrator.DefaultMachine()

	jobFailedFrom := task.Status
	action := &orchestrator.Action{TaskID: task.ID, Type: "job_failed"}
	newTask, err := sm.Apply(task, action)
	if err != nil {
		slog.Warn("job done: job_failed transition not applicable", "error", err)
		return job, nil
	}
	action.FromStatus = jobFailedFrom
	action.ToStatus = newTask.Status

	if err := s.Tx.WithinTx(func(tx TxStore) error {
		if err := tx.UpdateTask(newTask); err != nil {
			return err
		}
		return tx.CreateAction(action)
	}); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	slog.Info("job done: job_failed applied", "job_id", job.ID, "new_status", newTask.Status)
	s.cleanupWorktree(newTask.ID, job.ProjectID, newTask.Status)
	return job, nil
}

func (s *TaskWorkflowService) runDispatchLoop(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine) {
	const maxCycles = 10
	current := task

	for cycle := 0; cycle < maxCycles; cycle++ {
		result, err := s.Coordinator.DispatchAndAdvance(ctx, current, meta, sm)
		if err != nil {
			slog.Error("dispatch loop error", "task_id", current.ID, "cycle", cycle, "error", err)
			s.recordDispatchError(current.ID, current.Status, err)
			return
		}

		s.persistFiredEvents(current.ID, current.Status, result.FiredEvents)

		// Persist hook + exit gate payload
		if len(result.FinalPayload) > 0 {
			var persisted *orchestrator.Task
			if err := s.Tx.WithinTx(func(tx TxStore) error {
				latest, err := tx.GetTask(current.ID)
				if err != nil {
					return err
				}
				latest.Payload = result.FinalPayload
				if err := tx.UpdateTask(latest); err != nil {
					return err
				}
				persisted = latest
				return nil
			}); err != nil {
				slog.Error("persist payload failed", "task_id", current.ID, "error", err)
				return
			}
			current = persisted
		}

		if result.NewStatus == "" {
			// No transition this cycle. Finalize if terminal.
			if current.Status == orchestrator.TaskStatusDone || current.Status == orchestrator.TaskStatusAborted {
				s.cleanupWorktree(current.ID, current.ProjectID, current.Status)
				if s.Lifecycle != nil {
					s.Lifecycle.CleanupTaskWindow(current.ID)
				}
				if current.Status == orchestrator.TaskStatusDone {
					s.triggerDependentTasks(ctx, current.ID)
				}
				if current.ParentID != "" {
					s.triggerDependentTasks(ctx, current.ParentID)
				}
			}
			return
		}

		prevStatus := current.Status
		action := &orchestrator.Action{
			TaskID:     current.ID,
			Type:       "auto_advance",
			FromStatus: prevStatus,
			ToStatus:   result.NewStatus,
		}
		current.Status = result.NewStatus
		if err := s.Tx.WithinTx(func(tx TxStore) error {
			if err := tx.UpdateTask(current); err != nil {
				return err
			}
			return tx.CreateAction(action)
		}); err != nil {
			slog.Error("auto-advance persist failed", "task_id", current.ID, "error", err)
			return
		}

		slog.Info("auto-advanced", "task_id", current.ID, "new_status", current.Status, "cycle", cycle)

		// Run entry gates on the new state (skip for self-loops)
		if prevStatus != current.Status {
			entryResult, err := s.Coordinator.DispatchEntryGates(ctx, current, meta)
			if err != nil {
				slog.Error("entry gate dispatch failed", "task_id", current.ID, "error", err)
				s.recordDispatchError(current.ID, current.Status, err)
				return
			}
			s.persistFiredEvents(current.ID, current.Status, entryResult.FiredEvents)
			if len(entryResult.FinalPayload) > 0 {
				current.Payload = entryResult.FinalPayload
				if err := s.Tx.WithinTx(func(tx TxStore) error {
					return tx.UpdateTask(current)
				}); err != nil {
					slog.Error("persist entry gate payload failed", "task_id", current.ID, "error", err)
					return
				}
			}
		}

		s.cleanupWorktree(current.ID, current.ProjectID, current.Status)

		if current.Status == orchestrator.TaskStatusDone || current.Status == orchestrator.TaskStatusAborted {
			if s.Lifecycle != nil {
				s.Lifecycle.CleanupTaskWindow(current.ID)
			}
			if current.Status == orchestrator.TaskStatusDone {
				s.triggerDependentTasks(ctx, current.ID)
			}
			if current.ParentID != "" {
				s.triggerDependentTasks(ctx, current.ParentID)
			}
			return
		}
	}

	slog.Warn("dispatch loop max cycles reached", "task_id", current.ID, "max", maxCycles)
}

// TriggerDependents は taskID に依存する pending タスクを評価し、
// 依存条件が満たされた場合に自動 start する。
func (s *TaskWorkflowService) TriggerDependents(ctx context.Context, taskID string) {
	s.triggerDependentTasks(ctx, taskID)
}

func (s *TaskWorkflowService) triggerDependentTasks(ctx context.Context, taskID string) {
	if s.Tasks == nil {
		return
	}
	dependents, err := s.Tasks.FindDependentTasks(taskID)
	if err != nil {
		slog.Error("trigger dependent tasks: find dependents", "task_id", taskID, "error", err)
		return
	}
	for _, dep := range dependents {
		if err := checkDependencies(dep, s.Tasks.GetTask); err != nil {
			continue
		}
		if _, err := s.ApplyAction(ctx, dep.ID, ApplyActionRequest{Type: "start"}); err != nil {
			slog.Warn("trigger dependent tasks: start failed", "dependent_id", dep.ID, "error", err)
		}
	}
}

func (s *TaskWorkflowService) recordDispatchError(taskID string, taskStatus orchestrator.TaskStatus, err error) {
	if s.Tx == nil || taskID == "" || err == nil {
		return
	}

	payload, marshalErr := json.Marshal(map[string]string{"error": err.Error()})
	if marshalErr != nil {
		slog.Error("marshal dispatch error payload failed", "task_id", taskID, "error", marshalErr)
		return
	}

	// dispatch_error は状態遷移を伴わないため from_status = to_status = 現在のステータス
	action := &orchestrator.Action{
		TaskID:     taskID,
		Type:       "dispatch_error",
		Payload:    payload,
		FromStatus: taskStatus,
		ToStatus:   taskStatus,
	}
	if txErr := s.Tx.WithinTx(func(tx TxStore) error {
		return tx.CreateAction(action)
	}); txErr != nil {
		slog.Error("persist dispatch error failed", "task_id", taskID, "error", txErr)
	}
}

func (s *TaskWorkflowService) persistFiredEvents(taskID string, status orchestrator.TaskStatus, events []orchestrator.FiredEvent) {
	if len(events) == 0 || s.Tx == nil {
		return
	}
	if err := s.Tx.WithinTx(func(tx TxStore) error {
		for _, fe := range events {
			payload, _ := json.Marshal(map[string]any{
				"kit_id":       fe.KitID,
				"hook_id":      fe.HandlerID,
				"source_state": fe.SourceState,
				"success":      fe.Success,
				"error":        fe.Error,
			})
			action := &orchestrator.Action{
				TaskID:     taskID,
				Type:       fe.Kind + "_fired",
				Payload:    payload,
				FromStatus: status,
				ToStatus:   status,
			}
			if err := tx.CreateAction(action); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		slog.Warn("persist fired events failed", "task_id", taskID, "error", err)
	}
}

func (s *TaskWorkflowService) cleanupWorktree(taskID, projectID string, status orchestrator.TaskStatus) {
	if s.Projects == nil || s.Worktrees == nil || projectID == "" {
		return
	}

	project, err := s.Projects.GetProject(projectID)
	if err != nil {
		slog.Warn("worktree cleanup project lookup failed", "task_id", taskID, "project_id", projectID, "error", err)
		return
	}
	if err := s.Worktrees.CleanupForTask(taskID, project.WorkDir, string(status)); err != nil {
		slog.Warn("worktree cleanup failed", "task_id", taskID, "project_id", projectID, "error", err)
	}
}
