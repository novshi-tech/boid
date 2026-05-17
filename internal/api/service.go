package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/notify"
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

type TaskDetailView struct {
	Task              *orchestrator.Task
	Actions           []*orchestrator.Action
	Jobs              []*Job
	AvailableActions  []string             `json:"available_actions"`
	Dependents        []*orchestrator.Task `json:"dependents,omitempty"`
	DependsOnResolved []*orchestrator.Task `json:"depends_on_resolved,omitempty"`
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
		HostCommands:       map[string]orchestrator.HostCommandSpec(cmd.HostCommands),
		AdditionalBindings: cmd.AdditionalBindings,
		Readonly:           cmd.Readonly,
	}, nil
}

func (s *ProjectAppService) ListCommands(id string) ([]CommandSummary, error) {
	meta, ok := s.Meta.Get(id)
	if !ok {
		return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("project %q meta not loaded", id)}
	}
	summaries := make([]CommandSummary, 0, len(meta.Commands))
	for name, cmd := range meta.Commands {
		summaries = append(summaries, CommandSummary{Name: name, Command: cmd.ResolvedCommand, Readonly: cmd.Readonly})
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].Name < summaries[j].Name })
	return summaries, nil
}

func (s *TaskAppService) GetTaskBehaviorCommand(taskID, name string) (*CommandResponse, error) {
	task, err := s.GetTask(taskID)
	if err != nil {
		return nil, err
	}
	meta, ok := s.Meta.Get(task.ProjectID)
	if !ok {
		return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("project %q meta not loaded", task.ProjectID)}
	}
	behavior, _, ok := lookupBehaviorWithAlias(meta, task.Behavior)
	if !ok {
		return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("behavior %q not found", task.Behavior)}
	}
	cmd, ok := behavior.Commands[name]
	if !ok {
		return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("command %q not found", name)}
	}
	return &CommandResponse{
		Command:            cmd.ResolvedCommand,
		Env:                cmd.Env,
		HostCommands:       map[string]orchestrator.HostCommandSpec(cmd.HostCommands),
		AdditionalBindings: cmd.AdditionalBindings,
		Readonly:           cmd.Readonly,
	}, nil
}

func (s *TaskAppService) ListTaskBehaviorCommands(taskID string) ([]CommandSummary, error) {
	task, err := s.GetTask(taskID)
	if err != nil {
		return nil, err
	}
	meta, ok := s.Meta.Get(task.ProjectID)
	if !ok {
		return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("project %q meta not loaded", task.ProjectID)}
	}
	behavior, _, ok := lookupBehaviorWithAlias(meta, task.Behavior)
	if !ok {
		return []CommandSummary{}, nil
	}
	summaries := make([]CommandSummary, 0, len(behavior.Commands))
	for name, cmd := range behavior.Commands {
		summaries = append(summaries, CommandSummary{Name: name, Command: cmd.ResolvedCommand, Readonly: cmd.Readonly})
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
	Projects    ProjectWorkDirLookup
	RuntimesDir string
	Notify      Notifier
}

// Notifier sends an agent-driven notification for a task. Implementations
// typically exec a user-configured command. nil-safe at the call site:
// TaskAppService.NotifyTask returns an error when Notify is unset.
type Notifier interface {
	Notify(ctx context.Context, ev notify.Event) error
}

// enrichJob fills WorkspacePath from RuntimesDir and the job's RuntimeID.
// If either is empty the field is left unchanged (omitempty will omit it in JSON).
func enrichJob(runtimesDir string, job *Job) {
	if runtimesDir == "" || job.RuntimeID == "" {
		return
	}
	job.WorkspacePath = filepath.Join(runtimesDir, job.RuntimeID)
}

// enrichJobDisplayName sets job.DisplayName from the project meta's hook definitions
// when the job is a hook job and DisplayName is not yet set. This resolves the
// display name in-memory from the project meta store (no DB read needed).
func enrichJobDisplayName(job *Job, behavior string, meta MetaStore) {
	if job.DisplayName != "" || job.Role != "hook" || behavior == "" || meta == nil {
		return
	}
	projectMeta, ok := meta.Get(job.ProjectID)
	if !ok {
		return
	}
	tb, ok := projectMeta.TaskBehaviors[behavior]
	if !ok {
		return
	}
	for _, h := range tb.Hooks {
		if h.ID == job.HandlerID && h.Name != "" {
			job.DisplayName = h.Name
			return
		}
	}
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
	instructions orchestrator.Instructions
}

// DefaultBehavior is the reserved behavior name used when a CreateTaskRequest
// omits both behavior and behavior_spec. Projects are expected to define a
// behavior with this name in project.yaml's task_behaviors (typically with
// readonly: true) so that bare-task creation routes to a planning/triage step.
//
// Note: this is the canonical name; project.yaml files written with the
// legacy alias "plan" continue to work because spec_loader normalizes them
// to "supervisor" at load time (see BehaviorAliases).
const DefaultBehavior = "supervisor"

// lookupBehaviorWithAlias finds a TaskBehavior in meta.TaskBehaviors by name,
// being tolerant of the plan / dev → supervisor / executor rename. Lookup is
// tried in this order:
//
//  1. exact match against the requested name
//  2. if the request is a legacy alias, try the canonical name
//  3. if the request is a canonical name, try the legacy alias (handles
//     unnormalized in-memory ProjectMeta values that may exist in tests or
//     transitional code paths)
//
// When (2) or (3) hits, a deprecation warning is logged. The returned key
// is the map key that actually matched; callers may use it for further
// logging or store the canonical form on the task.
func lookupBehaviorWithAlias(meta *orchestrator.ProjectMeta, name string) (orchestrator.TaskBehavior, string, bool) {
	if b, ok := meta.TaskBehaviors[name]; ok {
		return b, name, true
	}
	if canonical, isAlias := orchestrator.CanonicalBehaviorName(name); isAlias {
		if b, ok := meta.TaskBehaviors[canonical]; ok {
			slog.Warn("task behavior name is deprecated; use canonical name instead",
				"scope", "CreateTask request",
				"deprecated", name,
				"canonical", canonical,
			)
			return b, canonical, true
		}
	}
	// Reverse: caller used the new canonical name, but meta still uses the
	// alias key (legacy in-memory meta, e.g. hand-built test fixtures).
	for alias, canonical := range orchestrator.BehaviorAliases {
		if canonical != name {
			continue
		}
		if b, ok := meta.TaskBehaviors[alias]; ok {
			slog.Warn("project meta uses deprecated behavior name; please regenerate via ReadProjectMetaWithKits",
				"scope", "CreateTask request",
				"deprecated", alias,
				"canonical", name,
			)
			return b, alias, true
		}
	}
	return orchestrator.TaskBehavior{}, "", false
}

// resolveBehavior validates and resolves behavior fields from a CreateTaskRequest.
// It handles both the named behavior path (meta lookup) and the inline behavior_spec path.
// When both behavior and behavior_spec are empty, the request is routed to DefaultBehavior.
func resolveBehavior(meta *orchestrator.ProjectMeta, req CreateTaskRequest) (*behaviorResolution, error) {
	if req.Behavior != "" && req.BehaviorSpec != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "behavior and behavior_spec are mutually exclusive"}
	}
	if req.Behavior == "" && req.BehaviorSpec == nil {
		req.Behavior = DefaultBehavior
	}

	res := &behaviorResolution{payload: req.Payload}

	if req.BehaviorSpec != nil {
		spec := req.BehaviorSpec
		if spec.Name == "" {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: "behavior_spec.name is required"}
		}
		res.behaviorName = spec.Name
		res.traits = spec.Traits
		// Phase 3-1: behavior-level readonly/worktree/branch_prefix/base_branch
		// and default_payload are gone. Inline specs receive the canonical
		// readonly/worktree treatment along with named behaviors below — set
		// here from project-top defaults (worktree only) and finalised by
		// applyCanonicalBehaviorOverrides.
		if meta != nil {
			res.worktree = meta.Worktree
			res.baseBranch = meta.BaseBranch
		}
		applyCanonicalBehaviorOverrides(res, meta)
		mergedInstructions, err := orchestrator.MergeDefaultInstructions(spec.DefaultInstruction, req.Instructions)
		if err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: "instructions merge: " + err.Error()}
		}
		res.instructions = mergedInstructions
		return res, nil
	}

	// Named behavior path (existing logic).
	res.behaviorName = req.Behavior
	if meta != nil {
		behavior, lookupKey, ok := lookupBehaviorWithAlias(meta, req.Behavior)
		if !ok {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("behavior %q not found", req.Behavior)}
		}
		// When alias resolution kicked in (the meta key we matched differs
		// from what the caller asked for), persist the canonical form on
		// the task so rows converge regardless of which alias the caller
		// or the meta used. Exact matches are preserved verbatim to keep
		// legacy callers / fixtures stable until Phase 5.
		if lookupKey != req.Behavior {
			canonical, _ := orchestrator.CanonicalBehaviorName(req.Behavior)
			res.behaviorName = canonical
		}
		res.traits = behavior.Traits
		// Phase 3-1: behavior-level readonly / worktree / branch_prefix /
		// base_branch / default_payload are deleted. readonly comes from the
		// behavior name (supervisor / executor) via
		// applyCanonicalBehaviorOverrides; worktree and base_branch come
		// from project-top fields.
		res.worktree = meta.Worktree
		res.baseBranch = meta.BaseBranch
		mergedInstructions, err := orchestrator.MergeDefaultInstructions(behavior.DefaultInstruction, req.Instructions)
		if err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: "instructions merge: " + err.Error()}
		}
		res.instructions = mergedInstructions

		applyCanonicalBehaviorOverrides(res, meta)
	} else if len(req.Instructions) > 0 {
		mergedInstructions, err := orchestrator.MergeDefaultInstructions(nil, req.Instructions)
		if err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: "instructions merge: " + err.Error()}
		}
		res.instructions = mergedInstructions
	}
	return res, nil
}

// applyCanonicalBehaviorOverrides enforces the Phase 3-1 readonly/worktree
// rules. After the behavior-level fields were removed, readonly is decided
// entirely by the canonical behavior name (supervisor=true, executor=false);
// non-canonical behaviors get readonly=false (the legacy zero-value default).
// worktree is taken from the project-top setting verbatim.
//
// meta may be nil when behavior_spec is in use without a project meta; the
// only effect of nil is that res.worktree stays at its caller-supplied value
// (typically the bool zero) which mirrors the pre-Phase-3-1 behavior of
// inline specs.
func applyCanonicalBehaviorOverrides(res *behaviorResolution, meta *orchestrator.ProjectMeta) {
	switch res.behaviorName {
	case "supervisor":
		res.readonly = true
	case "executor":
		res.readonly = false
	default:
		// Non-canonical behavior: readonly stays at the zero value (false).
		res.readonly = false
	}
	if meta != nil {
		res.worktree = meta.Worktree
	}
}

// classifyAndApplyBaseBranchCase performs the Phase 2-2 supervisor 3-case
// classification and adjusts task.Worktree based on the result. It is also
// where the "parent-less executor pointed at a non-existent base" error is
// surfaced — a child executor with a parent inherits the parent's already-
// resolved base (which by construction exists or will be created when the
// parent runs) and thus skips classification entirely.
//
// Returns the updated worktree flag (the task field) and a *StatusError on
// validation failure. The function is conservative: when classification
// itself fails (e.g. detached HEAD, project lookup unwired) it surfaces the
// error so callers cannot silently fall through to a broken supervisor run.
//
// Rationale for living on the service (rather than orchestrator pkg): the
// decision combines task-row metadata (behaviorName, parent), project meta
// (workdir lookup), and orchestrator primitives. Pushing it into orchestrator
// would require importing the ProjectWorkDirLookup interface back, which is
// the wrong direction for the layer boundary (orchestrator → api is forbidden
// per feedback_layer_boundary_enforcement). Service is the right join point.
func (s *TaskAppService) classifyAndApplyBaseBranchCase(req CreateTaskRequest, behaviorName, baseBranch string, worktree, inheritedFromParent bool) (bool, error) {
	// Inherited base branches were already validated when the parent was
	// scheduled; re-checking here would either double-trip (parent already
	// exists → case 2) or fight the parent's case-3 promise. Same reasoning
	// as the inheritance branch in CreateTask.
	if inheritedFromParent {
		return worktree, nil
	}
	if behaviorName != "supervisor" && behaviorName != "executor" {
		// Non-canonical behaviors keep the existing semantics (P3-1 will
		// remove the divergence entirely).
		return worktree, nil
	}
	if s.Projects == nil {
		// No project workdir lookup available (e.g. test wiring without a
		// Projects stub). Without it we cannot classify; leave the worktree
		// decision untouched and skip the check. CreateTask paths that need
		// the classification wire the Projects field — silent skipping here
		// matches the legacy behavior of the base_branch expander.
		return worktree, nil
	}
	proj, projErr := s.Projects.GetProject(req.ProjectID)
	if projErr != nil {
		return worktree, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("project lookup failed: %v", projErr)}
	}
	if proj == nil || proj.WorkDir == "" {
		return worktree, nil
	}

	state, err := orchestrator.ClassifyBaseBranch(proj.WorkDir, baseBranch)
	if err != nil {
		return worktree, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("classify base_branch %q: %v", baseBranch, err)}
	}

	switch behaviorName {
	case "supervisor":
		// Supervisor case routing:
		//   case 1 → worktree=false (run in project dir)
		//   case 2 → worktree=true  (check out the existing base in a worktree)
		//   case 3 → worktree=true  (worktree manager will create the base)
		switch state {
		case orchestrator.Case1HeadMatches:
			worktree = false
		case orchestrator.Case2ExistsButNotCheckedOut, orchestrator.Case3NotFound:
			worktree = true
		}
	case "executor":
		// Executor never runs in the project dir, so case 1 / case 2 are both
		// fine. Case 3 with no parent is an error: a child executor inherits
		// its parent's base_branch (so its presence is the parent's
		// responsibility), but a parent-less executor has nobody to create
		// the missing base.
		if state == orchestrator.Case3NotFound && req.ParentID == "" {
			return worktree, &StatusError{
				Code: http.StatusBadRequest,
				Message: fmt.Sprintf("executor base_branch %q does not exist locally or on origin, and the task has no parent supervisor to create it", baseBranch),
			}
		}
	}
	return worktree, nil
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
	// Phase 2-3: task-row level overrides for readonly / worktree / base_branch
	// / branch_prefix have been removed. Values come from the resolved behavior
	// (and project-level defaults for worktree / base_branch).

	// Phase 1-3: parent-child base_branch inheritance.
	//
	// If the new task has a parent, inherit the parent's already-resolved
	// BaseBranch verbatim and skip both expanders. The parent's BaseBranch
	// was expanded when the parent was created, so re-expansion (especially
	// of ${TASK_REMOTE_ID}) on the child would diverge from the parent's
	// branch and break the worktree assumption that all children of a
	// task share its base.
	//
	// Static base_branch values (e.g. "main") on the child are also discarded
	// in favor of the parent's value: "parent's resolved base wins" is the
	// invariant. See README of Phase 1-3 for the rationale.
	//
	// Parent-not-found is logged and we fall through to behavior-level
	// expansion: legacy callers that pre-date strict parent validation
	// (and many existing tests) wire parent_ids that aren't real rows.
	// Phase 2 will tighten this once those callers are migrated.
	inheritedFromParent := false
	if req.ParentID != "" {
		parent, parentErr := s.Tasks.GetTask(req.ParentID)
		if parentErr != nil || parent == nil {
			slog.Warn("parent task not found for base_branch inheritance; falling back to behavior-level resolution",
				"scope", "CreateTask",
				"task_id", req.ID,
				"parent_id", req.ParentID,
				"error", parentErr,
			)
		} else {
			baseBranch = parent.BaseBranch
			inheritedFromParent = true
		}
	}

	if !inheritedFromParent && baseBranch != "" {
		// Phase 1-3: expand ${TASK_REMOTE_ID} first so a missing remote_id
		// errors out before we touch the project working directory.
		expanded, err := orchestrator.ExpandTaskBaseBranch(baseBranch, req.RemoteID)
		if err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
		}
		baseBranch = expanded

		if s.Projects != nil {
			proj, projErr := s.Projects.GetProject(req.ProjectID)
			if projErr != nil {
				return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("project lookup failed: %v", projErr)}
			}
			expanded, err := orchestrator.ExpandBaseBranch(baseBranch, proj.WorkDir)
			if err != nil {
				return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
			}
			baseBranch = expanded
		}
	}

	// Phase 2-2: supervisor 3-case execution location decision + executor base
	// existence check. classifyAndApplyBaseBranchCase mutates worktree (and
	// short-circuits with an error in the "executor + case 3 + no parent" case);
	// inheritedFromParent skips the classify entirely because the parent's
	// base must already exist (case 1/2) or have been created (case 3) when
	// the parent itself was scheduled.
	worktree, err = s.classifyAndApplyBaseBranchCase(req, res.behaviorName, baseBranch, worktree, inheritedFromParent)
	if err != nil {
		return nil, err
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

// GetTaskField resolves a dotted field path against the task. See
// ResolveTaskField for the path syntax (top-level fields, payload traits,
// computed lifecycle).
func (s *TaskAppService) GetTaskField(id, path string) (string, error) {
	if path == "" {
		return "", &StatusError{Code: http.StatusBadRequest, Message: "field path is required"}
	}
	task, err := s.Tasks.GetTask(id)
	if err != nil {
		return "", &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	value, err := ResolveTaskField(task, s.Actions, path)
	if err != nil {
		return "", &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	return value, nil
}

// NotifyTask invokes the configured notify command for the given task.
// Returns 501 when no notifier is wired and ask is empty (notifications disabled in config).
// When ask is non-empty the task is transitioned to awaiting; the notification is
// best-effort and skipped if no notifier is configured. questionID identifies the
// Q&A turn (generated when empty).
// When progress is non-empty (progress mode), no hook fires and no state transition
// occurs — only a progress Action is written to the timeline.
// ask and progress are mutually exclusive.
func (s *TaskAppService) NotifyTask(ctx context.Context, taskID, message, ask, questionID, sessionID, progress, done, fail string) error {
	// ask / progress / done / fail are mutually exclusive: each represents a
	// distinct lifecycle signal (Q&A pause, FYI-only progress, success
	// self-report, failure self-report). Allowing more than one would
	// ambiguate which state transition (if any) to fire.
	modes := 0
	for _, m := range []string{ask, progress, done, fail} {
		if m != "" {
			modes++
		}
	}
	if modes > 1 {
		return &StatusError{Code: http.StatusBadRequest, Message: "--ask, --progress, --done, --fail are mutually exclusive"}
	}
	if message == "" && progress == "" {
		return &StatusError{Code: http.StatusBadRequest, Message: "message is required"}
	}

	// Progress mode: write a timeline Action directly, skip hook firing entirely.
	// Progress is a pure observability event with no user-facing surface, so the
	// parent_id gate below does not apply — both root and child tasks can record
	// progress without further checks.
	if progress != "" {
		task, err := s.Tasks.GetTask(taskID)
		if err != nil {
			return &StatusError{Code: http.StatusNotFound, Message: err.Error()}
		}
		payload, err := json.Marshal(map[string]string{"message": progress})
		if err != nil {
			return &StatusError{Code: http.StatusInternalServerError, Message: "encode progress payload: " + err.Error()}
		}
		action := &orchestrator.Action{
			TaskID:     taskID,
			Type:       "progress",
			FromStatus: task.Status,
			ToStatus:   task.Status,
			Payload:    payload,
		}
		if err := s.Actions.CreateAction(action); err != nil {
			return &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
		}
		return nil
	}
	task, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}

	// Lifecycle-accountability hard gate: only root tasks (parent_id == "")
	// fire user-facing notify hooks. Child tasks signal their parent supervisor
	// via the awaiting state transition (for ask mode) or are silently dropped
	// (for FYI mode) — the supervisor's monitoring loop is the canonical
	// delivery path. This is a daemon-level invariant rather than a project.yaml
	// hook expression, so child tasks cannot accidentally page the user when a
	// project author forgets the condition. See docs/plans/lifecycle-accountability.md.
	fireUserNotify := task.ParentID == ""

	// Without ask, a working notifier is required to surface the FYI — but only
	// when we would actually fire the hook. Child tasks skip the hook unconditionally,
	// so a missing notifier is fine.
	if s.Notify == nil && ask == "" && fireUserNotify {
		return &StatusError{Code: http.StatusNotImplemented, Message: "notify is not configured"}
	}

	// Lifecycle signal modes (ask / done / fail) all advance the task state
	// machine and SIGTERM running hook runtimes. Plain FYI notify (none of
	// those flags) only fires the user-notify hook for root tasks.
	signalsTransition := ask != "" || done != "" || fail != ""

	ev := notify.Event{
		TaskID:    taskID,
		TaskTitle: task.Title,
		ProjectID: task.ProjectID,
		Message:   message,
	}
	// Deep-link target depends on mode:
	//   ask  → Q&A turn page (reply form)
	//   done → task detail (success outcome to inspect)
	//   fail → task detail (failure outcome to inspect / decide reopen)
	//   FYI  → most recent interactive running job (live session attach)
	switch {
	case ask != "":
		if questionID == "" {
			questionID = newQuestionID()
		}
		ev.URLPath = "/tasks/" + taskID + "/questions/" + questionID
	case done != "" || fail != "":
		ev.URLPath = "/tasks/" + taskID
	}
	// Project name is best-effort: omit silently if Projects lookup fails or is unwired.
	if s.Projects != nil {
		if proj, lookupErr := s.Projects.GetProject(task.ProjectID); lookupErr == nil && proj != nil {
			ev.ProjectName = proj.Meta.Name
		}
	}
	// FYI mode only: find the most recent interactive running job so the
	// notification deep-links to the live session. ask/done/fail set
	// URLPath above to a more specific destination.
	if !signalsTransition && s.Jobs != nil {
		if jobs, jobsErr := s.Jobs.ListJobsByTask(taskID); jobsErr == nil {
			for i := len(jobs) - 1; i >= 0; i-- {
				j := jobs[i]
				if j.Status == JobStatusRunning && j.Interactive {
					ev.JobID = j.ID
					break
				}
			}
		}
	}
	if fireUserNotify && s.Notify != nil {
		if err := s.Notify.Notify(ctx, ev); err != nil {
			if !signalsTransition {
				return &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
			}
			slog.Warn("notify: notification failed in signal mode, continuing with state transition", "error", err, "mode", notifyModeName(ask, done, fail))
		}
	} else if !fireUserNotify {
		slog.Debug("notify: skipped user-facing hook (child task, owner is parent supervisor)",
			"task_id", taskID, "parent_id", task.ParentID, "mode", notifyModeName(ask, done, fail))
	}

	if !signalsTransition {
		return nil
	}

	// Lifecycle signal: persist the agent's intent + SIGUSR1 the running jobs.
	//
	// --ask still goes through ApplyAction(ask): the awaiting transition is
	// synchronous (the agent expects the task to be visibly in `awaiting`
	// immediately after the call returns so the parent supervisor's polling
	// loop sees it).
	//
	// --done / --fail record a `done_request` / `fail_request` action
	// directly WITHOUT calling ApplyAction. The state transition fires later
	// via the condition-based auto rule (`lifecycle.executed && lifecycle.done`
	// → done; ditto for fail), which only kicks in after the runtime has
	// cleanly exited and bash's EXIT trap has called `boid job done`. This
	// preserves the agent's payload_patch (session id) and avoids the race
	// where ApplyAction(done)'s spawned dispatch loop SIGTERM'd the still-
	// running runtime, leaving the job marked failed.
	if s.Workflow == nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: "workflow service not configured"}
	}
	if ask != "" {
		ap := orchestrator.AwaitingPayload{
			SessionID:  sessionID,
			Question:   ask,
			QuestionID: questionID,
		}
		apJSON, err := json.Marshal(ap)
		if err != nil {
			return &StatusError{Code: http.StatusInternalServerError, Message: "encode awaiting payload: " + err.Error()}
		}
		askPayload, err := json.Marshal(map[string]json.RawMessage{string(orchestrator.TraitAwaiting): apJSON})
		if err != nil {
			return &StatusError{Code: http.StatusInternalServerError, Message: "encode action payload: " + err.Error()}
		}
		if _, err := s.Workflow.ApplyAction(ctx, taskID, ApplyActionRequest{
			Type:    "ask",
			Payload: askPayload,
		}); err != nil {
			return err
		}
	} else {
		// done / fail: record the intent as a non-transitioning action.
		// The dispatch loop's auto-advance picks it up once the runtime exits.
		if s.Actions == nil {
			return &StatusError{Code: http.StatusInternalServerError, Message: "action store not configured"}
		}
		if task.Status != orchestrator.TaskStatusExecuting {
			return &StatusError{Code: http.StatusConflict, Message: fmt.Sprintf("task is not executing (status: %s); cannot record %s_request", task.Status, notifyModeName(ask, done, fail))}
		}
		var actionType, msg string
		if done != "" {
			actionType, msg = "done_request", done
		} else {
			actionType, msg = "fail_request", fail
		}
		payload, err := json.Marshal(map[string]string{"message": msg})
		if err != nil {
			return &StatusError{Code: http.StatusInternalServerError, Message: "encode " + actionType + " payload: " + err.Error()}
		}
		action := &orchestrator.Action{
			TaskID:     taskID,
			Type:       actionType,
			FromStatus: task.Status,
			ToStatus:   task.Status,
			Payload:    payload,
		}
		if err := s.Actions.CreateAction(action); err != nil {
			return &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
		}
	}

	// Ask the agent (claude) of each running hook job to terminate via a
	// SIGUSR1 routed to run-agent.py. This leaves bash and the EXIT trap
	// alive: bash receives SIGUSR1 too but ignores it via `trap '' USR1`
	// (SIG_IGN propagates across execve to pasta/unshare/inner bash); only
	// run-agent.py's Python handler reacts, forwarding SIGTERM to the
	// claude process (which it launched in its own session via
	// start_new_session=True so it doesn't receive the group signal).
	//
	// Crucially, we do NOT call CompleteJob preemptively here. CompleteJob's
	// finalize releases the broker token, which would reject the bash EXIT
	// trap's follow-up `boid job done --output-file payload_patch.json` as
	// "invalid token" — silently dropping the agent's session id and
	// breaking the next hook's resume. By letting the EXIT trap be the sole
	// CompleteJob caller (through the broker), the standard completion path
	// runs with the agent's payload_patch intact.
	if s.Jobs != nil {
		jobs, err := s.Jobs.ListJobsByTask(taskID)
		if err == nil {
			for _, j := range jobs {
				if j.Status != JobStatusRunning {
					continue
				}
				if j.RuntimeID == "" {
					continue
				}
				s.Workflow.StopAgent(j.RuntimeID)
			}
		} else {
			slog.Warn("notify: list running jobs failed", "task_id", taskID, "mode", notifyModeName(ask, done, fail), "error", err)
		}
	}
	return nil
}

// notifyModeName returns a short label identifying which lifecycle signal
// (if any) was supplied to NotifyTask. Used only for slog context.
func notifyModeName(ask, done, fail string) string {
	switch {
	case ask != "":
		return "ask"
	case done != "":
		return "done"
	case fail != "":
		return "fail"
	default:
		return "fyi"
	}
}

// AnswerTask saves the user's reply and transitions the task awaiting → executing.
func (s *TaskAppService) AnswerTask(ctx context.Context, taskID, questionID, answer string) error {
	if questionID == "" {
		return &StatusError{Code: http.StatusBadRequest, Message: "question_id is required"}
	}
	if answer == "" {
		return &StatusError{Code: http.StatusBadRequest, Message: "answer is required"}
	}
	task, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	if task.Status != orchestrator.TaskStatusAwaiting {
		return &StatusError{
			Code:    http.StatusConflict,
			Message: fmt.Sprintf("task is not awaiting (status: %s)", task.Status),
		}
	}
	if s.Workflow == nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: "workflow service not configured"}
	}

	// Merge pending_answer into the existing awaiting trait.
	existing := orchestrator.GetAwaitingPayload(task.Payload)
	existing.PendingAnswer = answer
	apJSON, err := json.Marshal(existing)
	if err != nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: "encode awaiting payload: " + err.Error()}
	}
	answerPayload, err := json.Marshal(map[string]json.RawMessage{string(orchestrator.TraitAwaiting): apJSON})
	if err != nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: "encode action payload: " + err.Error()}
	}
	if _, err := s.Workflow.ApplyAction(ctx, taskID, ApplyActionRequest{
		Type:    "answer",
		Payload: answerPayload,
	}); err != nil {
		return err
	}
	return nil
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
	if req.DependsOn != nil {
		for _, depID := range req.DependsOn {
			dep, err := s.Tasks.GetTask(depID)
			if err != nil || dep == nil {
				return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("depends_on: task %q not found", depID)}
			}
		}
		if hasCycleInUpdate(id, req.DependsOn, s.Tasks.GetTask) {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: "depends_on: circular dependency detected"}
		}
		task.DependsOn = req.DependsOn
	}
	if req.DependsOnPayload != nil {
		task.DependsOnPayload = *req.DependsOnPayload
	}
	if req.ParentID != nil {
		task.ParentID = *req.ParentID
	}
	// Phase 2-3: task-row level base_branch / branch_prefix / worktree updates
	// have been removed. These values are determined at create time from the
	// behavior type and project-level defaults, and are no longer mutable.
	var instructionsBefore orchestrator.Instructions
	if len(req.Instructions) > 0 {
		if !isInstructionsEditable(task.Status) {
			return nil, &StatusError{
				Code:    http.StatusConflict,
				Message: fmt.Sprintf("cannot edit instructions while task is running (status: %s)", task.Status),
			}
		}
		instructionsBefore = cloneInstructions(task.Instructions)
		var override orchestrator.Instructions
		if err := json.Unmarshal(req.Instructions, &override); err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: "instructions parse: " + err.Error()}
		}
		task.Instructions = override
	}
	if req.AutoStart != nil {
		task.AutoStart = *req.AutoStart
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
	if req.AutoStart != nil && *req.AutoStart && task.Status == orchestrator.TaskStatusPending && s.Workflow != nil {
		result, err := s.Workflow.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "start"})
		if err != nil {
			slog.Error("auto_start: update: failed to apply start action", "task_id", task.ID, "error", err)
		} else {
			task = result.Task
		}
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
		if task.Status == orchestrator.TaskStatusExecuting {
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

	var instructionsBefore orchestrator.Instructions
	if len(req.InstructionsOverride) > 0 && string(req.InstructionsOverride) != "null" {
		instructionsBefore = cloneInstructions(task.Instructions)
		var override orchestrator.Instructions
		if err := json.Unmarshal(req.InstructionsOverride, &override); err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: "instructions parse: " + err.Error()}
		}
		task.Instructions = override
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

func cloneInstructions(src orchestrator.Instructions) orchestrator.Instructions {
	if src == nil {
		return nil
	}
	out := make(orchestrator.Instructions, len(src))
	copy(out, src)
	return out
}

// auditInstructionsChange records an instructions change as an Action so that
// the reason behind rerun-over-rerun outcome differences can be traced.
func (s *TaskAppService) auditInstructionsChange(taskID string, before, after orchestrator.Instructions) {
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
		enrichJobDisplayName(j, task.Behavior, s.Meta)
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

	return &TaskDetailView{
		Task:              task,
		Actions:           actions,
		Jobs:              jobs,
		AvailableActions:  computeAvailableActions(task),
		Dependents:        dependents,
		DependsOnResolved: dependsOnResolved,
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
	TaskSvc    TaskService
	Gates      GateService
	Hooks      HookService
	Answerer   TaskAnswerService // optional: enables POST /tasks/{id}/answer
}

func (s *WebAppService) CreateTask(req CreateTaskRequest) (*orchestrator.Task, error) {
	if s.TaskSvc == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "task service not configured"}
	}
	return s.TaskSvc.CreateTask(req)
}

func (s *WebAppService) UpdateTask(id string, req UpdateTaskRequest) error {
	if s.TaskSvc == nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: "task service not configured"}
	}
	_, err := s.TaskSvc.UpdateTask(id, req)
	return err
}

func (s *WebAppService) ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	return s.Tasks.ListTasks(filter)
}

func (s *WebAppService) ListBehaviors() ([]string, error) {
	tasks, err := s.Tasks.ListTasks(orchestrator.TaskFilter{})
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var behaviors []string
	for _, t := range tasks {
		if t.Behavior != "" && !seen[t.Behavior] {
			seen[t.Behavior] = true
			behaviors = append(behaviors, t.Behavior)
		}
	}
	sort.Strings(behaviors)
	return behaviors, nil
}

func (s *WebAppService) ListWorkspaces() ([]*orchestrator.WorkspaceSummary, error) {
	return s.Projects.ListWorkspaces()
}

func (s *WebAppService) GetTaskDetail(id string) (*TaskDetailView, error) {
	task, err := s.Tasks.GetTask(id)
	if err != nil {
		return nil, err
	}

	actions, _ := s.Actions.ListActionsByTask(task.ID)
	rawJobs, _ := s.Jobs.ListJobsByTask(task.ID)
	for _, j := range rawJobs {
		enrichJobDisplayName(j, task.Behavior, s.Meta)
	}
	jobs := rawJobs
	dependents, _ := s.Tasks.FindDependentTasks(task.ID)

	var dependsOnResolved []*orchestrator.Task
	for _, depID := range task.DependsOn {
		dep, err := s.Tasks.GetTask(depID)
		if err != nil {
			continue
		}
		dependsOnResolved = append(dependsOnResolved, dep)
	}

	return &TaskDetailView{
		Task:              task,
		Actions:           actions,
		Jobs:              jobs,
		AvailableActions:  computeAvailableActions(task),
		Dependents:        dependents,
		DependsOnResolved: dependsOnResolved,
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

// DuplicateTask delegates to the shared TaskService so the Web UI uses the
// same duplication semantics as the JSON API: a fresh task is created via
// CreateTask + resolveBehavior so that Instructions and Payload come from
// the behavior's DefaultInstruction / DefaultPayload, not from the source
// task's runtime state. Without this delegation the duplicate inherited
// the source's runtime payload (claude_code.sessions, awaiting trait) and
// missing Instructions caused the hook evaluator to skip the agent hook,
// so no hook fired on Start.
//
// The Web UI button does not auto-start the duplicate; the user clicks
// Start separately.
func (s *WebAppService) DuplicateTask(id string) (string, error) {
	if s.TaskSvc == nil {
		return "", &StatusError{Code: http.StatusInternalServerError, Message: "task service not configured"}
	}
	task, err := s.TaskSvc.DuplicateTask(id, false)
	if err != nil {
		return "", err
	}
	return task.ID, nil
}

// DeleteTask delegates to the shared TaskService so the web UI uses the
// same delete semantics as the JSON API and TUI.
func (s *WebAppService) DeleteTask(id string, force bool) error {
	if s.TaskSvc == nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: "task service not configured"}
	}
	return s.TaskSvc.DeleteTask(id, force)
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
		enrichJobDisplayName(&result.Job, task.Behavior, s.Meta)
	}
	return result, nil
}

func (s *WebAppService) RerunTask(id string, req RerunTaskRequest) error {
	if s.TaskSvc == nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: "task service not configured"}
	}
	_, err := s.TaskSvc.RerunTask(id, req)
	return err
}

type ReopenTaskRequest struct {
	Message string `json:"message,omitempty"`
}

func (s *WebAppService) ReopenTask(id string, req ReopenTaskRequest) error {
	if s.Workflow == nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: "workflow service not configured"}
	}
	var payload json.RawMessage
	if req.Message != "" {
		b, err := json.Marshal(map[string]any{
			"instruction": map[string]any{"message": req.Message},
		})
		if err != nil {
			return &StatusError{Code: http.StatusInternalServerError, Message: "payload encode: " + err.Error()}
		}
		payload = b
	}
	_, err := s.Workflow.ApplyAction(context.Background(), id, ApplyActionRequest{Type: "reopen", Payload: payload})
	return err
}

func (s *WebAppService) ListGatesForStatus(taskID, status string) ([]orchestrator.Gate, error) {
	if s.Gates == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "gate service not configured"}
	}
	return s.Gates.ListGatesForStatus(taskID, status)
}

func (s *WebAppService) ReplayGate(ctx context.Context, taskID string, req ReplayGateRequest) (*ReplayGateResult, error) {
	if s.Gates == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "gate service not configured"}
	}
	return s.Gates.ReplayGate(ctx, taskID, req)
}

func (s *WebAppService) ListHooksForStatus(taskID, status string) ([]orchestrator.Hook, error) {
	if s.Hooks == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "hook service not configured"}
	}
	return s.Hooks.ListHooksForStatus(taskID, status)
}

func (s *WebAppService) ReplayHook(ctx context.Context, taskID string, req ReplayHookRequest) (*ReplayHookResult, error) {
	if s.Hooks == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "hook service not configured"}
	}
	return s.Hooks.ReplayHook(ctx, taskID, req)
}

func (s *WebAppService) GetProjectByID(id string) (*orchestrator.Project, error) {
	project, err := s.Projects.GetProject(id)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	if meta, ok := s.Meta.Get(id); ok {
		project.Meta = *meta
	}
	return project, nil
}

func (s *WebAppService) ListProjectCommands(projectID string) ([]CommandSummary, error) {
	meta, ok := s.Meta.Get(projectID)
	if !ok {
		return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("project %q meta not loaded", projectID)}
	}
	summaries := make([]CommandSummary, 0, len(meta.Commands))
	for name, cmd := range meta.Commands {
		summaries = append(summaries, CommandSummary{Name: name, Command: cmd.ResolvedCommand, Readonly: cmd.Readonly})
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].Name < summaries[j].Name })
	return summaries, nil
}

func (s *WebAppService) ListTaskBehaviorCommands(taskID string) ([]CommandSummary, error) {
	task, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	meta, ok := s.Meta.Get(task.ProjectID)
	if !ok {
		return []CommandSummary{}, nil
	}
	behavior, _, ok := lookupBehaviorWithAlias(meta, task.Behavior)
	if !ok {
		return []CommandSummary{}, nil
	}
	summaries := make([]CommandSummary, 0, len(behavior.Commands))
	for name, cmd := range behavior.Commands {
		summaries = append(summaries, CommandSummary{Name: name, Command: cmd.ResolvedCommand, Readonly: cmd.Readonly})
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].Name < summaries[j].Name })
	return summaries, nil
}

func (s *WebAppService) AnswerTask(ctx context.Context, taskID, questionID, answer string) error {
	if s.Answerer == nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: "answer service not configured"}
	}
	return s.Answerer.AnswerTask(ctx, taskID, questionID, answer)
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
	Hub         *TaskEventHub
	// Locks pins the project-level worktree lock to the executing lifetime of
	// each task. Optional: when nil, no project locking is performed (matches
	// pre-P0-2 behaviour for tests that don't exercise concurrency).
	Locks *orchestrator.ProjectLockManager

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

// shouldHoldProjectLock reports whether the given task needs an exclusive
// project worktree lock while executing. Mirrors the legacy condition that
// guarded dispatchHooksLocked: readonly tasks (no writes) and worktree=true
// tasks (private working directory) bypass the lock and can run in parallel.
func shouldHoldProjectLock(task *orchestrator.Task) bool {
	if task == nil {
		return false
	}
	return !task.Readonly && !task.Worktree
}

// releaseProjectLock drops the executing-lifetime project lock for the given
// task. Safe to call multiple times; safe when the task never acquired the
// lock (e.g. readonly/worktree tasks).
func (s *TaskWorkflowService) releaseProjectLock(taskID string) {
	if s.Locks == nil || taskID == "" {
		return
	}
	s.Locks.ReleaseForTask(taskID)
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

	// reopen carries an optional `{"instruction": {...}}` payload that appends a
	// new entry to the task's instruction history. The instruction is recorded
	// only on the action (audit trail) and not merged into task.payload.
	var reopenPayloadConsumed bool
	if req.Type == "reopen" && len(req.Payload) > 0 {
		var p struct {
			Instruction *orchestrator.Instruction `json:"instruction,omitempty"`
		}
		if err := json.Unmarshal(req.Payload, &p); err == nil && p.Instruction != nil {
			inst := *p.Instruction
			if inst.Type == "" {
				inst.Type = orchestrator.InstructionTypeExecution
			}
			if active := task.Instructions.Active(); active != nil {
				if inst.Agent == "" {
					inst.Agent = active.Agent
				}
				if inst.Model == "" {
					inst.Model = active.Model
				}
			}
			newTask.Instructions = orchestrator.AppendInstruction(task.Instructions, inst)
			reopenPayloadConsumed = true
		}
	}

	if !reopenPayloadConsumed {
		merged, err := orchestrator.MergePayload(task.Payload, action.Payload)
		if err != nil {
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: "payload merge: " + err.Error()}
		}
		newTask.Payload = merged
	}

	if err := s.Tx.WithinTx(func(tx TxStore) error {
		if err := tx.UpdateTask(newTask); err != nil {
			return err
		}
		return tx.CreateAction(action)
	}); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	// Release the project lock whenever the action moves the task out of
	// executing (ask, done, abort, ...). Idempotent — safe when the task did
	// not hold the lock (e.g. readonly/worktree tasks, or repeated abort).
	if newTask.Status != orchestrator.TaskStatusExecuting {
		s.releaseProjectLock(newTask.ID)
	}

	if s.Hub != nil {
		s.Hub.Broadcast(newTask.ID, TaskEvent{
			Kind: "action",
			Payload: map[string]any{
				"action_id":  action.ID,
				"new_status": string(action.ToStatus),
			},
		})
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
			if behavior, _, found := lookupBehaviorWithAlias(meta, newTask.Behavior); found {
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

// StopAgent asynchronously delivers a SIGUSR1 to the runtime's process group.
// The agent runner (run-agent.py) catches this and SIGTERMs claude only —
// bash stays alive so the EXIT trap can fire `boid job done --output-file
// payload_patch.json` through the broker normally. See WorkflowService
// interface doc for the full lifecycle rationale.
func (s *TaskWorkflowService) StopAgent(runtimeID string) {
	if runtimeID == "" || s.Lifecycle == nil {
		return
	}
	go s.Lifecycle.SignalJobRuntime(runtimeID, syscall.SIGUSR1)
}

func (s *TaskWorkflowService) CompleteJob(_ context.Context, jobID string, req JobDoneRequest) (*Job, error) {
	job, err := s.Jobs.GetJob(jobID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}

	// Idempotency: a second CompleteJob call (e.g. EXIT trap after agent-driven
	// SIGTERM) must not corrupt the already-terminal job or re-fire lifecycle events.
	if job.Status == JobStatusCompleted || job.Status == JobStatusFailed {
		return job, nil
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

	// Stop the runtime so the agent process receives SIGTERM immediately after
	// calling `boid job done` explicitly, rather than waiting for natural bash
	// exit. The EXIT trap that fires afterward is absorbed by the idempotency
	// guard above. A no-op when RuntimeID is unset or the process has already
	// exited (LocalRuntime.Stop handles that gracefully).
	if job.RuntimeID != "" && s.Lifecycle != nil {
		runtimeID := job.RuntimeID
		go s.Lifecycle.StopJobRuntime(runtimeID)
	}

	// Successful job completion: no state transition here.
	// The runDispatchLoop (hooks → gates → auto-advance) is responsible for
	// evaluating conditions and advancing the task state once all handlers
	// have completed. Transitioning in CompleteJob would race with the gate
	// execution and clean up the worktree before gates can run.
	//
	// Broadcast the running→completed transition so the web timeline can
	// recolor the marker (green) immediately — without waiting for the
	// downstream hook_fired action to land later. The failure path below
	// gets its own broadcast alongside the job_failed action (task-status
	// transition is a separate visual signal).
	if req.ExitCode == 0 {
		if s.Hub != nil {
			s.Hub.Broadcast(job.TaskID, TaskEvent{
				Kind: "job",
				Payload: map[string]any{
					"job_id":     job.ID,
					"new_status": string(job.Status),
				},
			})
		}
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

	if s.Hub != nil {
		s.Hub.Broadcast(job.TaskID, TaskEvent{
			Kind: "job",
			Payload: map[string]any{
				"job_id":    job.ID,
				"new_state": string(newTask.Status),
			},
		})
	}

	// job_failed moves the task out of executing — release the project lock so
	// queued tasks on the same project can advance. Idempotent.
	if newTask.Status != orchestrator.TaskStatusExecuting {
		s.releaseProjectLock(newTask.ID)
	}

	slog.Info("job done: job_failed applied", "job_id", job.ID, "new_status", newTask.Status)
	s.cleanupWorktree(newTask.ID, job.ProjectID, newTask.Status)
	return job, nil
}

func (s *TaskWorkflowService) runDispatchLoop(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine) {
	const maxCycles = 10
	current := task

	// Project-level worktree lock — held for the entire executing lifetime so
	// concurrent tasks on the same project don't race over the working tree.
	// Idempotent: re-spawned dispatch loops for an already-locked task no-op.
	//
	// Eligibility:
	//   - task.Status == executing (no-op when dispatching a terminal task,
	//     e.g. a `done` ApplyAction that spawns the loop solely to fire
	//     finalizeTerminal → triggerDependentTasks)
	//   - !readonly && !worktree (matches the legacy dispatchHooksLocked gate)
	//   - the behavior actually declares hooks that would fire in executing
	//     (a hook-less behavior leaves the project working tree untouched, so
	//     locking it would serialize unrelated tasks for no reason — this
	//     was the regression that broke auto-start-deps in CI)
	if s.Locks != nil && current.Status == orchestrator.TaskStatusExecuting && shouldHoldProjectLock(current) {
		if len(orchestrator.ListHooksForStatus(meta, current, orchestrator.TaskStatusExecuting)) > 0 {
			if err := s.Locks.AcquireForTask(ctx, current.ProjectID, current.ID); err != nil {
				slog.Warn("dispatch loop: project lock acquire failed",
					"task_id", current.ID, "project_id", current.ProjectID, "error", err)
				// Lock was never acquired, so no release needed. terminateForDispatchError
				// calls finalizeTerminal which calls releaseProjectLock idempotently (no-op).
				s.terminateForDispatchError(ctx, current, fmt.Errorf("project lock: %w", err))
				return
			}
		}
	}

	for cycle := 0; cycle < maxCycles; cycle++ {
		result, err := s.Coordinator.DispatchAndAdvance(ctx, current, meta, sm)
		if err != nil {
			// Persist any partial FiredEvents first so the failing hook/gate
			// remains visible in the timeline; recordDispatchError then logs
			// the dispatcher-level error for context.
			if result != nil {
				s.persistFiredEvents(current.ID, current.Status, result.FiredEvents)
			}
			slog.Error("dispatch loop error", "task_id", current.ID, "cycle", cycle, "error", err)
			s.terminateForDispatchError(ctx, current, err)
			return
		}

		s.persistFiredEvents(current.ID, current.Status, result.FiredEvents)

		// The awaiting trait is owned exclusively by ApplyAction("ask"/"answer")
		// and is persisted to the DB inline as those actions run. The coordinator's
		// FinalPayload, however, derives from a snapshot of task.Payload taken
		// BEFORE the hook executed, so any awaiting value it carries is necessarily
		// stale: if the hook itself called `boid task notify --ask` mid-flight, the
		// fresh awaiting trait is already in the DB and the snapshot's awaiting
		// would clobber it on top-level merge. Strip awaiting from FinalPayload
		// before the merge and apply pending_answer clearing to the DB-fresh row
		// instead.
		result.FinalPayload = orchestrator.StripAwaitingTrait(result.FinalPayload)

		// Persist hook + exit gate payload. Always refresh the task row so we
		// can detect concurrent terminal transitions (abort/done) and pick up
		// any awaiting trait written by an ApplyAction("ask") that fired during
		// the hook.
		var persisted *orchestrator.Task
		if err := s.Tx.WithinTx(func(tx TxStore) error {
			latest, err := tx.GetTask(current.ID)
			if err != nil {
				return err
			}
			// Clear pending_answer from the (DB-fresh) awaiting trait now that
			// the hook has been spawned and consumed it. session_id, question,
			// and question_id are preserved so the task can be resumed again
			// if the kit emits another ask.
			latest.Payload = orchestrator.ClearPendingAnswer(latest.Payload)
			if len(result.FinalPayload) > 0 {
				merged, mergeErr := orchestrator.MergePayload(latest.Payload, result.FinalPayload)
				if mergeErr != nil {
					return mergeErr
				}
				latest.Payload = merged
			}
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

		// Drop any would-be auto-advance if the task was terminated
		// concurrently (e.g. user abort while a hook was in flight). Finalize
		// here so the caller that set the terminal status does not have to
		// race with us on cleanup.
		if current.Status == orchestrator.TaskStatusDone || current.Status == orchestrator.TaskStatusAborted {
			slog.Info("dispatch loop: task reached terminal concurrently, skipping advance",
				"task_id", current.ID, "status", current.Status, "would_advance_to", result.NewStatus)
			s.finalizeTerminal(ctx, current)
			return
		}

		// If a hook called boid task notify --ask during this cycle, the task
		// transitioned to awaiting. The lifecycle.executed signal computed from
		// the hook exit is stale — do not auto-advance to done. The dispatch
		// loop will re-fire (via AnswerTask → ApplyAction("answer")) once the
		// user replies.
		if current.Status == orchestrator.TaskStatusAwaiting {
			slog.Info("dispatch loop: task is awaiting user answer, skipping auto-advance",
				"task_id", current.ID, "would_advance_to", result.NewStatus)
			// awaiting means the task left executing — release the project
			// lock so other tasks can run. answer will re-acquire on resume.
			s.releaseProjectLock(current.ID)
			return
		}

		if result.NewStatus == "" {
			// No transition this cycle. Finalize if terminal.
			s.finalizeTerminal(ctx, current)
			return
		}

		prevStatus := current.Status
		action := &orchestrator.Action{
			TaskID:     current.ID,
			Type:       "auto_advance",
			FromStatus: prevStatus,
			ToStatus:   result.NewStatus,
			Payload:    result.ActionPayload,
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
				if entryResult != nil {
					s.persistFiredEvents(current.ID, current.Status, entryResult.FiredEvents)
				}
				slog.Error("entry gate dispatch failed", "task_id", current.ID, "error", err)
				s.terminateForDispatchError(ctx, current, err)
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

		if current.Status == orchestrator.TaskStatusDone || current.Status == orchestrator.TaskStatusAborted {
			s.finalizeTerminal(ctx, current)
			return
		}
	}

	slog.Warn("dispatch loop max cycles reached", "task_id", current.ID, "max", maxCycles)
}

// TriggerDependents は taskID に依存する pending タスクを評価し、
// auto_start=true かつ依存条件が満たされた場合に自動 start する。
// auto_start=false のタスクは依存解決しても pending のまま残り、
// ユーザが手動で start するまで待機する。
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
		if !dep.AutoStart {
			continue
		}
		if err := checkDependencies(dep, s.Tasks.GetTask); err != nil {
			continue
		}
		if _, err := s.ApplyAction(ctx, dep.ID, ApplyActionRequest{Type: "start"}); err != nil {
			slog.Warn("trigger dependent tasks: start failed", "dependent_id", dep.ID, "error", err)
		}
	}
}

// terminateForDispatchError transitions the task to aborted, records a
// dispatch_error action with aborted_reason and optional cause fields, then
// calls finalizeTerminal to release the project lock, clean up any worktree,
// and trigger dependent tasks.
//
// It is safe to call this even when the project lock was never acquired
// (lock acquire failure path): releaseProjectLock is idempotent.
func (s *TaskWorkflowService) terminateForDispatchError(ctx context.Context, task *orchestrator.Task, err error) {
	payloadData := map[string]any{
		"error":          err.Error(),
		"aborted_reason": "dispatch_error",
	}
	var wse *dispatcher.WorktreeSetupError
	if errors.As(err, &wse) {
		payloadData["cause"] = wse.Cause
	}
	payload, marshalErr := json.Marshal(payloadData)
	if marshalErr != nil {
		slog.Error("terminate for dispatch error: marshal payload failed", "task_id", task.ID, "error", marshalErr)
		return
	}

	sm := orchestrator.DefaultMachine()
	abortedTask, smErr := sm.Apply(task, &orchestrator.Action{Type: "abort"})
	if smErr != nil {
		// Abort is a wildcard transition; this should not happen. Fall back to
		// just recording the error without a state transition.
		slog.Error("terminate for dispatch error: abort transition failed",
			"task_id", task.ID, "status", task.Status, "error", smErr)
		s.recordDispatchError(task.ID, task.Status, err)
		return
	}

	action := &orchestrator.Action{
		TaskID:     task.ID,
		Type:       "dispatch_error",
		Payload:    payload,
		FromStatus: task.Status,
		ToStatus:   abortedTask.Status,
	}
	if s.Tx != nil {
		if txErr := s.Tx.WithinTx(func(tx TxStore) error {
			if err := tx.UpdateTask(abortedTask); err != nil {
				return err
			}
			return tx.CreateAction(action)
		}); txErr != nil {
			slog.Error("terminate for dispatch error: persist failed", "task_id", task.ID, "error", txErr)
			return
		}
	}

	s.finalizeTerminal(ctx, abortedTask)
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
				"job_id":       fe.JobID,
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
		return
	}

	if s.Hub != nil {
		for _, fe := range events {
			s.Hub.Broadcast(taskID, TaskEvent{
				Kind: "fired_event",
				Payload: map[string]any{
					"event_name": fe.Kind + "_fired",
					"role":       fe.HandlerID,
					"kit_id":     fe.KitID,
					"success":    fe.Success,
				},
			})
		}
	}
}

// ReplayGate replays a single gate for the given task. If req.Status is non-empty
// the task's status is overwritten before dispatch (allows recovery from terminal
// states). Running jobs on the same task cause a 409 Conflict.
func (s *TaskWorkflowService) ReplayGate(ctx context.Context, taskID string, req ReplayGateRequest) (*ReplayGateResult, error) {
	task, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}

	meta, ok := s.Meta.Get(task.ProjectID)
	if !ok {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "project meta not loaded: " + task.ProjectID}
	}

	// Check for running jobs.
	jobs, err := s.Jobs.ListJobsByTask(taskID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	for _, j := range jobs {
		if j.Status == JobStatusRunning {
			return nil, &StatusError{Code: http.StatusConflict, Message: "task has a running job; wait for it to complete before replaying"}
		}
	}

	// Optional status override.
	if req.Status != "" {
		task.Status = orchestrator.TaskStatus(req.Status)
		if err := s.Tasks.UpdateTask(task); err != nil {
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
		}
	}

	sm := orchestrator.DefaultMachine()
	replay, err := s.Coordinator.ReplayGate(ctx, task, meta, sm, req.GateID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}

	// Persist payload and optional status advance.
	if err := s.Tx.WithinTx(func(tx TxStore) error {
		latest, err := tx.GetTask(taskID)
		if err != nil {
			return err
		}
		latest.Payload = replay.FinalPayload
		if replay.NewStatus != "" {
			latest.Status = replay.NewStatus
		}
		return tx.UpdateTask(latest)
	}); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	s.persistFiredEvents(taskID, task.Status, replay.FiredEvents)

	// Re-fetch to return the persisted state.
	updated, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	return &ReplayGateResult{Task: updated, FiredEvents: replay.FiredEvents}, nil
}

// ListGatesForStatus returns gates that match the given status for the task.
// If status is empty, the task's current status is used.
func (s *TaskWorkflowService) ListGatesForStatus(taskID, status string) ([]orchestrator.Gate, error) {
	task, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	meta, ok := s.Meta.Get(task.ProjectID)
	if !ok {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "project meta not loaded: " + task.ProjectID}
	}
	effectiveStatus := task.Status
	if status != "" {
		effectiveStatus = orchestrator.TaskStatus(status)
	}
	gates := orchestrator.ListGatesForStatus(meta, task, effectiveStatus)
	if gates == nil {
		gates = []orchestrator.Gate{}
	}
	return gates, nil
}

// ReplayHook replays a single hook for the given task. If req.Status is non-empty
// the task's status is overwritten before dispatch. Running jobs on the same task
// cause a 409 Conflict.
func (s *TaskWorkflowService) ReplayHook(ctx context.Context, taskID string, req ReplayHookRequest) (*ReplayHookResult, error) {
	task, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}

	meta, ok := s.Meta.Get(task.ProjectID)
	if !ok {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "project meta not loaded: " + task.ProjectID}
	}

	// Check for running jobs.
	jobs, err := s.Jobs.ListJobsByTask(taskID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	for _, j := range jobs {
		if j.Status == JobStatusRunning {
			return nil, &StatusError{Code: http.StatusConflict, Message: "task has a running job; wait for it to complete before replaying"}
		}
	}

	// Optional status override.
	if req.Status != "" {
		task.Status = orchestrator.TaskStatus(req.Status)
		if err := s.Tasks.UpdateTask(task); err != nil {
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
		}
	}

	sm := orchestrator.DefaultMachine()
	replay, err := s.Coordinator.ReplayHook(ctx, task, meta, sm, req.HookID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}

	// Persist payload and optional status advance.
	if err := s.Tx.WithinTx(func(tx TxStore) error {
		latest, err := tx.GetTask(taskID)
		if err != nil {
			return err
		}
		latest.Payload = replay.FinalPayload
		if replay.NewStatus != "" {
			latest.Status = replay.NewStatus
		}
		return tx.UpdateTask(latest)
	}); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	s.persistFiredEvents(taskID, task.Status, replay.FiredEvents)

	// Re-fetch to return the persisted state.
	updated, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	return &ReplayHookResult{Task: updated, FiredEvents: replay.FiredEvents}, nil
}

// ListHooksForStatus returns hooks that match the given status for the task.
// If status is empty, the task's current status is used.
func (s *TaskWorkflowService) ListHooksForStatus(taskID, status string) ([]orchestrator.Hook, error) {
	task, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	meta, ok := s.Meta.Get(task.ProjectID)
	if !ok {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "project meta not loaded: " + task.ProjectID}
	}
	effectiveStatus := task.Status
	if status != "" {
		effectiveStatus = orchestrator.TaskStatus(status)
	}
	hooks := orchestrator.ListHooksForStatus(meta, task, effectiveStatus)
	if hooks == nil {
		hooks = []orchestrator.Hook{}
	}
	return hooks, nil
}

// finalizeTerminal runs the per-task cleanup required once a task has reached
// a terminal status. No-op for non-terminal tasks. Safe to call multiple
// times: cleanupWorktree skips already-removed worktrees and
// CleanupTaskWindow atomically drains runtimes.
func (s *TaskWorkflowService) finalizeTerminal(ctx context.Context, task *orchestrator.Task) {
	if task.Status != orchestrator.TaskStatusDone && task.Status != orchestrator.TaskStatusAborted {
		return
	}
	// Release the executing-lifetime project lock first so a queued waiter on
	// the same project can acquire while the cleanup below is still in flight.
	// Idempotent — safe if the task never acquired the lock.
	s.releaseProjectLock(task.ID)
	s.cleanupWorktree(task.ID, task.ProjectID, task.Status)
	if s.Lifecycle != nil {
		s.Lifecycle.CleanupTaskWindow(task.ID)
	}
	if task.Status == orchestrator.TaskStatusDone {
		s.triggerDependentTasks(ctx, task.ID)
	}
	if task.ParentID != "" {
		s.triggerDependentTasks(ctx, task.ParentID)
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

// newQuestionID generates a random hex identifier for a Q&A turn.
func newQuestionID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
