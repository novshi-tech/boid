package api

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type ProjectAppService struct {
	Projects ProjectRepository
	Meta     interface {
		Get(id string) (*orchestrator.ProjectMeta, bool)
		Remove(id string)
		LoadAll(projects []*orchestrator.Project) []error
		SetWorkspaceID(projectID, workspaceID string)
		Status(id string) orchestrator.ProjectStatus
		MarkDegraded(id, message string)
		SetSynthesizedMeta(id string, meta *orchestrator.ProjectMeta)
		// Explain returns id's field-provenance view — see
		// orchestrator.ProjectStore.Explain's own doc comment.
		Explain(id string) (*orchestrator.ProjectExplain, error)
	}
	CloneBareRepo      func(ctx context.Context, url, destPath, namespace string) error
	FetchBareRepo      func(ctx context.Context, bareRepoPath, namespace string) error
	DataDir            string
	Hydrator           orchestrator.MetaHydrator
	CaptureUpstreamURL func(workDir string) (string, error)
	Workspaces         WorkspaceStore
	HostCommands       func() map[string]orchestrator.HostCommandSpec
	WorkspacesConn     *sql.DB
	InitScripts        WorkspaceInitScriptStore
	mu                 sync.Mutex
	projectMu          sync.Mutex
}

func (s *ProjectAppService) hydrateProject(project *orchestrator.Project) *orchestrator.Project {
	if project == nil {
		return nil
	}
	if meta, ok := s.Meta.Get(project.ID); ok {
		project.Meta = *meta
	}
	return s.applyStatus(project)
}

func (s *ProjectAppService) applyStatus(project *orchestrator.Project) *orchestrator.Project {
	if project == nil {
		return nil
	}
	st := s.Meta.Status(project.ID)
	if st.State != orchestrator.StatusReady {
		project.Status = st.State
		project.StatusMessage = st.Message
	} else {
		project.Status = ""
		project.StatusMessage = ""
	}
	return project
}

func (s *ProjectAppService) removeCachedMetaUnlessRegistered(id string) {
	if existing, err := s.Projects.GetProject(id); err == nil && existing != nil {
		return
	}
	s.Meta.Remove(id)
}

func (s *ProjectAppService) hydrateProjectWithWorkspace(ctx context.Context, project *orchestrator.Project) *orchestrator.Project {
	if project == nil {
		return nil
	}
	if s.Hydrator != nil {
		meta, err := s.Hydrator.GetWithWorkspace(ctx, project.ID)
		if err == nil && meta != nil {
			project.Meta = *meta
			return s.applyStatus(project)
		}
		// Fall back to bare meta on hydration error so the API stays usable
		// even when workspace.yaml is malformed. The hydrator already logs.
		slog.Warn("project meta hydration failed; falling back to raw meta",
			"project_id", project.ID, "error", err)
	}
	return s.hydrateProject(project)
}

func (s *ProjectAppService) CreateProject(workDir string, requestedPerm string) (*orchestrator.Project, error) {
	return nil, &StatusError{Code: http.StatusBadRequest, Message: "filesystem registration has retired; use boid project add <git-url> --workspace=<name>"}
}

func (s *ProjectAppService) CreateProjectFromGitURL(ctx context.Context, gitURL, workspaceSlug, nameOverride string, requestedPerm string) (*orchestrator.Project, error) {
	perm, err := registrationPerm(requestedPerm)
	if err != nil {
		return nil, err
	}
	gitURL = strings.TrimSpace(gitURL)
	if gitURL == "" {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "url is required"}
	}
	normalizedURL, normErr := dispatcher.NormalizeOriginURL(gitURL)
	if normErr != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("unrecognized or credential-embedded git URL %q: %v", dispatcher.SanitizeURLForLogging(gitURL), normErr)}
	}
	gitURL = normalizedURL
	if strings.TrimSpace(workspaceSlug) == "" {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "workspace is required (git-URL registration has no host directory to eagerly default a workspace assignment from — unlike the now-removed legacy dir-based CreateProject; `boid project init` itself no longer registers a project at all, docs/plans/release-onboarding.md 穴 7)"}
	}
	if err := orchestrator.ValidWorkspaceSlug(workspaceSlug); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	if s.CloneBareRepo == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "git clone not wired"}
	}
	if s.DataDir == "" {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "daemon data directory not wired"}
	}

	name := strings.TrimSpace(nameOverride)
	nameWasExplicit := name != ""
	if name == "" {
		derived, err := orchestrator.DeriveProjectNameFromURL(gitURL)
		if err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
		}
		name = derived
	}

	exists, err := s.Projects.WorkspaceExists(workspaceSlug)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	if !exists {
		return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("workspace %q not found; create it first with `boid workspace create %s`", workspaceSlug, workspaceSlug)}
	}

	bareRepoPath, err := orchestrator.SafeBareRepoPath(s.DataDir, workspaceSlug, name)
	if err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}

	s.projectMu.Lock()
	defer s.projectMu.Unlock()

	if _, statErr := os.Stat(bareRepoPath); statErr == nil {
		return nil, &StatusError{Code: http.StatusConflict, Message: fmt.Sprintf("a project is already registered at %s; remove it first (`boid project rm`) or choose a different --name", bareRepoPath)}
	}

	if err := s.CloneBareRepo(ctx, gitURL, bareRepoPath, workspaceSlug); err != nil {
		return nil, &StatusError{Code: http.StatusBadGateway, Message: fmt.Sprintf("clone %q: %v", gitURL, err)}
	}

	meta, err := s.synthesizeProjectMeta(gitURL, name, workspaceSlug, nameWasExplicit)
	if err != nil {
		if rmErr := os.RemoveAll(bareRepoPath); rmErr != nil {
			slog.Warn("failed to roll back bare repo", "error", rmErr)
		}
		return nil, err
	}

	project := &orchestrator.Project{
		ID:          meta.ID,
		Name:        meta.Name,
		Perm:        perm,
		WorkDir:     bareRepoPath,
		UpstreamURL: gitURL,
	}

	if err := s.Projects.CreateProject(project); err != nil {
		s.removeCachedMetaUnlessRegistered(meta.ID)
		if rmErr := os.RemoveAll(bareRepoPath); rmErr != nil {
			slog.Warn("CreateProjectFromGitURL: failed to roll back bare repo after DB insert failure",
				"path", bareRepoPath, "error", rmErr)
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	s.Meta.SetSynthesizedMeta(meta.ID, meta)

	s.mu.Lock()
	if err := s.Projects.AssignWorkspaceIfExists(project.ID, workspaceSlug); err != nil {
		s.mu.Unlock()
		if rmErr := os.RemoveAll(bareRepoPath); rmErr != nil {
			slog.Error("CreateProjectFromGitURL: workspace assignment failed AND bare-repo rollback removal also failed; leaving the project row and its cached meta in place so the bare repo can still be removed on retry",
				"project_id", project.ID, "workspace", workspaceSlug, "path", bareRepoPath, "assign_error", err, "remove_error", rmErr)
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: fmt.Sprintf(
				"assign workspace %q failed (%v) and automatic bare-repo rollback also failed (%v); project %q was left registered — fix the underlying issue and retry `boid project rm %s` to finish cleanup, or `boid project show %s` to inspect its state",
				workspaceSlug, err, rmErr, project.ID, project.ID, project.ID,
			)}
		}
		if delErr := s.Projects.DeleteProject(project.ID); delErr != nil {
			slog.Error("CreateProjectFromGitURL: workspace assignment failed; bare repo was rolled back but DB row deletion also failed; retry `boid project rm` to finish removing the dangling row",
				"project_id", project.ID, "workspace", workspaceSlug, "assign_error", err, "delete_error", delErr)
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: fmt.Sprintf(
				"assign workspace %q failed (%v) and DB row cleanup also failed (%v); the bare repo has already been removed — retry `boid project rm %s` to finish removing the dangling row",
				workspaceSlug, err, delErr, project.ID,
			)}
		}
		s.Meta.Remove(meta.ID)
		if errors.Is(err, os.ErrNotExist) {
			return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf(
				"workspace %q was removed while this registration was in progress; retry `boid project add` once the workspace exists again", workspaceSlug,
			)}
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: fmt.Sprintf("assign workspace %q: %v", workspaceSlug, err)}
	}
	project.WorkspaceID = workspaceSlug
	s.Meta.SetWorkspaceID(project.ID, workspaceSlug)
	s.mu.Unlock()

	project.Meta = *meta
	return s.applyStatus(project), nil
}

func (s *ProjectAppService) FetchProject(ctx context.Context, id string) (*orchestrator.Project, error) {
	s.projectMu.Lock()
	defer s.projectMu.Unlock()

	project, err := s.Projects.GetProject(id)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}

	if s.FetchBareRepo == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "git fetch not wired"}
	}
	if !s.isManagedBareRepoPath(project.WorkDir) {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("project %q is not a git-URL registered project (legacy host-dir project; re-add via `boid project add <git-url> --workspace=<name>`)", id)}
	}

	if err := s.FetchBareRepo(ctx, project.WorkDir, project.WorkspaceID); err != nil {
		msg := fmt.Sprintf("fetch failed: %v", err)
		s.Meta.MarkDegraded(id, msg)
		return nil, &StatusError{Code: http.StatusBadGateway, Message: msg}
	}

	if errs := s.Meta.LoadAll([]*orchestrator.Project{project}); len(errs) > 0 {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: errs[0].Error()}
	}

	return s.hydrateProjectWithWorkspace(ctx, project), nil
}

// WithProjectLock serializes project registry reads with lifecycle mutations.
func (s *ProjectAppService) WithProjectLock(fn func() error) error {
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	return fn()
}

func (s *ProjectAppService) isManagedBareRepoPath(workDir string) bool {
	if s.DataDir == "" {
		return orchestrator.IsBareRepoDir(workDir)
	}
	underRoot, err := orchestrator.PathIsUnderResolved(orchestrator.ReposRoot(s.DataDir), workDir)
	if err != nil {
		slog.Warn("isManagedBareRepoPath: symlink resolution failed; treating as NOT daemon-managed (fail closed)",
			"work_dir", workDir, "error", err)
		return false
	}
	return underRoot
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
	return s.hydrateProjectWithWorkspace(context.Background(), project), nil
}

func (s *ProjectAppService) ExplainProject(id string) (*orchestrator.ProjectExplain, error) {
	if _, err := s.Projects.GetProject(id); err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	explain, err := s.Meta.Explain(id)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	return explain, nil
}

func (s *ProjectAppService) SetProjectWorkspace(id, workspaceID string) (*orchestrator.Project, error) {
	if workspaceID != "" {
		if err := orchestrator.ValidWorkspaceSlug(workspaceID); err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
		}
	}
	project, err := s.Projects.GetProject(id)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.Projects.AssignWorkspaceIfExists(id, workspaceID); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("workspace %q not found", workspaceID)}
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	s.Meta.SetWorkspaceID(id, workspaceID)
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

func (s *ProjectAppService) CreateWorkspace(slug string, meta *orchestrator.WorkspaceMeta) (*WorkspaceDetail, error) {
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	if s.Workspaces == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "workspace store not wired"}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.validateHostCommandRefs(meta.HostCommands); err != nil {
		return nil, err
	}
	if err := s.Workspaces.Create(slug, meta); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, &StatusError{Code: http.StatusConflict, Message: fmt.Sprintf("workspace %q already exists", slug)}
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	return s.buildWorkspaceDetail(slug, meta)
}

func (s *ProjectAppService) GetWorkspace(slug string) (*WorkspaceDetail, error) {
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	if s.Workspaces == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "workspace store not wired"}
	}
	meta, revision, err := s.Workspaces.LoadWithRevision(slug)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("workspace %q not found", slug)}
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	return s.buildWorkspaceDetailFromRevision(slug, meta, revision)
}

func (s *ProjectAppService) UpdateWorkspace(slug string, meta *orchestrator.WorkspaceMeta, ifMatch string, force bool) (*WorkspaceDetail, error) {
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	if s.Workspaces == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "workspace store not wired"}
	}
	if !force && ifMatch == "" {
		return nil, &StatusError{Code: http.StatusPreconditionRequired, Message: "If-Match header is required (or pass ?force=true)"}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Same validation as CreateWorkspace — see that call site's comment.
	if err := s.validateHostCommandRefs(meta.HostCommands); err != nil {
		return nil, err
	}

	if force {
		if _, err := s.Workspaces.Load(slug); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("workspace %q not found", slug)}
			}
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
		}
		if err := s.Workspaces.Save(slug, meta); err != nil {
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
		}
		return s.buildWorkspaceDetail(slug, meta)
	}

	newRevision, matched, err := s.Workspaces.UpdateIfRevisionMatches(slug, ifMatch, meta)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	if !matched {
		if _, existErr := s.Workspaces.Load(slug); existErr != nil {
			if errors.Is(existErr, os.ErrNotExist) {
				return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("workspace %q not found", slug)}
			}
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: existErr.Error()}
		}
		return nil, &StatusError{Code: http.StatusPreconditionFailed, Message: fmt.Sprintf("revision mismatch: If-Match %q does not match the current revision", ifMatch)}
	}
	return s.buildWorkspaceDetailFromRevision(slug, meta, newRevision)
}

func (s *ProjectAppService) validateHostCommandRefs(refs []string) error {
	if s.HostCommands == nil || len(refs) == 0 {
		return nil
	}
	snapshot := s.HostCommands()
	var unknown []string
	for _, name := range refs {
		if _, ok := snapshot[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return &StatusError{
		Code:    http.StatusBadRequest,
		Message: fmt.Sprintf("unknown host_commands reference(s): %s (run `boid host-commands list` to see known names)", strings.Join(unknown, ", ")),
	}
}

func (s *ProjectAppService) RemoveWorkspace(slug string) (*WorkspaceRemoval, error) {
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	if slug == orchestrator.DefaultWorkspaceSlug {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("workspace %q is reserved and cannot be removed", slug)}
	}
	if s.Workspaces == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "workspace store not wired"}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	assigned, err := s.ListProjects(slug)
	if err != nil {
		return nil, err
	}

	if err := s.Workspaces.Remove(slug); err != nil {
		if errors.Is(err, orchestrator.ErrWorkspaceHasTasks) {
			return nil, &StatusError{Code: http.StatusConflict, Message: err.Error()}
		}
		if errors.Is(err, os.ErrNotExist) {
			return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("workspace %q not found", slug)}
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	for _, p := range assigned {
		s.Meta.SetWorkspaceID(p.ID, orchestrator.DefaultWorkspaceSlug)
	}

	removal := &WorkspaceRemoval{}
	removal.InitScriptDeleted, removal.InitScriptError = deleteWorkspaceInitScript(s.InitScripts, slug)
	return removal, nil
}

func (s *ProjectAppService) buildWorkspaceDetail(slug string, meta *orchestrator.WorkspaceMeta) (*WorkspaceDetail, error) {
	summary, err := s.Projects.GetWorkspaceSummary(slug)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	return s.buildWorkspaceDetailFromRevision(slug, meta, summary.Revision)
}

func (s *ProjectAppService) buildWorkspaceDetailFromRevision(slug string, meta *orchestrator.WorkspaceMeta, revision string) (*WorkspaceDetail, error) {
	projects, err := s.ListProjects(slug)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(projects))
	for _, p := range projects {
		ids = append(ids, p.ID)
	}
	return &WorkspaceDetail{
		Slug:             slug,
		Meta:             meta,
		Revision:         revision,
		ProjectCount:     len(ids),
		AssignedProjects: ids,
	}, nil
}

func (s *ProjectAppService) ApplyWorkspace(apply *orchestrator.WorkspaceEnvelopeApply, dryRun bool) (*orchestrator.WorkspaceApplyResult, error) {
	slug := apply.Envelope.Metadata.Name
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	if s.WorkspacesConn == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "workspace db connection not wired"}
	}

	if apply.FieldsPresent["host_commands"] {
		if err := s.validateHostCommandRefs(apply.Envelope.Spec.HostCommands); err != nil {
			return nil, err
		}
	}
	if err := validateWorkspaceApplyInitScript(apply); err != nil {
		return nil, err
	}

	var projectIDsByName map[string]string
	var missing []string
	if apply.FieldsPresent["projects"] {
		var resolveErr error
		projectIDsByName, missing, resolveErr = s.resolveWorkspaceApplyProjectNames(apply.Envelope.Spec.Projects)
		if resolveErr != nil {
			return nil, resolveErr
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := orchestrator.ApplyWorkspaceEnvelope(s.WorkspacesConn, apply, projectIDsByName, dryRun)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	result.MissingProjects = missing

	if !dryRun {
		for _, id := range result.AttachedProjects {
			s.Meta.SetWorkspaceID(id, slug)
		}
		for _, id := range result.DetachedProjects {
			s.Meta.SetWorkspaceID(id, orchestrator.DefaultWorkspaceSlug)
		}
	}

	action, warning, err := s.applyWorkspaceInitScript(apply, dryRun)
	if err != nil {
		return nil, fmt.Errorf(
			"workspace %q: metadata and project assignments were applied, but spec.init_script was not: %w", slug, err)
	}
	result.InitScriptAction = action
	if warning != "" {
		slog.Warn("workspace apply: spec.init_script could not be applied", "workspace", slug, "reason", warning)
		result.Warnings = append(result.Warnings, warning)
	}
	return result, nil
}

func (s *ProjectAppService) resolveWorkspaceApplyProjectNames(projects []orchestrator.WorkspaceEnvelopeProject) (ids map[string]string, missing []string, err error) {
	snapshot, err := s.snapshotRegisteredProjects()
	if err != nil {
		return nil, nil, err
	}

	if err := refuseIfAnyRegisteredProjectNameUnavailable(snapshot); err != nil {
		return nil, nil, err
	}

	ids = make(map[string]string, len(projects))
	for _, ep := range projects {
		if strings.TrimSpace(ep.Name) == "" {
			missing = append(missing, ep.Name)
			continue
		}
		matches := resolveProjectByNameExact(snapshot, ep.Name)
		if len(matches) == 0 {
			missing = append(missing, ep.Name)
			continue
		}
		if len(matches) > 1 {
			matchIDs := make([]string, len(matches))
			for i, m := range matches {
				matchIDs[i] = m.ID
			}
			return nil, nil, &StatusError{
				Code: http.StatusConflict,
				Message: fmt.Sprintf(
					"project name %q is ambiguous: it matches %d registered projects (%s) — apply refused to avoid detaching the wrong one(s); re-register these projects with distinct `boid project add --name` values, or drop spec.projects from this document and manage the assignment via `boid project assign` directly",
					ep.Name, len(matches), strings.Join(matchIDs, ", "),
				),
			}
		}
		ids[ep.Name] = matches[0].ID
	}
	return ids, missing, nil
}

func (s *ProjectAppService) snapshotRegisteredProjects() ([]*orchestrator.Project, error) {
	projects, err := s.Projects.ListProjects()
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	for _, p := range projects {
		s.hydrateProject(p)
	}
	return projects, nil
}

func refuseIfAnyRegisteredProjectNameUnavailable(snapshot []*orchestrator.Project) error {
	for _, p := range snapshot {
		if p.Meta.Name == "" {
			return &StatusError{
				Code: http.StatusConflict,
				Message: fmt.Sprintf(
					"project %q in the workspace's target has no authoritative name; cannot resolve safely — its registered name is unavailable, re-register it with `boid project add --name` before applying",
					p.ID,
				),
			}
		}
	}
	return nil
}

func resolveProjectByNameExact(snapshot []*orchestrator.Project, name string) []*orchestrator.Project {
	var matches []*orchestrator.Project
	for _, p := range snapshot {
		if p.Meta.Name == name {
			matches = append(matches, p)
		}
	}
	return matches
}

func (s *ProjectAppService) ExportWorkspaceEnvelopes(slugs []string) ([]byte, error) {
	if s.WorkspacesConn == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "workspace db connection not wired"}
	}

	snapshots, allProjects, err := orchestrator.SnapshotWorkspacesForExport(s.WorkspacesConn, slugs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	duplicateNames, nameCollidesWithOtherID, effectiveNames := s.projectCollisions(allProjects)

	docs := make([][]byte, 0, len(snapshots))
	for _, snap := range snapshots {
		envProjects := make([]orchestrator.WorkspaceEnvelopeProject, 0, len(snap.ProjectIDs))
		for _, id := range snap.ProjectIDs {
			p, ok := snap.Projects[id]
			if !ok || p == nil {
				return nil, &StatusError{Code: http.StatusInternalServerError, Message: fmt.Sprintf("workspace %q: project %q is assigned but missing from the export snapshot (data inconsistency)", snap.Slug, id)}
			}

			name := effectiveNames[id]
			if strings.TrimSpace(name) == "" {
				return nil, &StatusError{Code: http.StatusConflict, Message: fmt.Sprintf(
					"project %q has no authoritative name (registered name unavailable or whitespace-only); re-register it with `boid project add --name` before exporting",
					id,
				)}
			}
			if collidingIDs, dup := duplicateNames[name]; dup {
				return nil, &StatusError{Code: http.StatusConflict, Message: fmt.Sprintf(
					"workspace %q: project name %q is ambiguous across %d registered projects (%s) — export refused because a later `apply` could not unambiguously resolve it back; re-register these projects with distinct `boid project add --name` values",
					snap.Slug, name, len(collidingIDs), strings.Join(collidingIDs, ", "),
				)}
			}
			if collidingIDs, dup := nameCollidesWithOtherID[name]; dup {
				return nil, &StatusError{Code: http.StatusConflict, Message: fmt.Sprintf(
					"workspace %q: project %q's name %q collides with another registered project's id (%s) — export refused because applying this document could attach/detach the wrong project; re-register %q using `boid project add --name` with a name that does not equal another project's id",
					snap.Slug, id, name, strings.Join(collidingIDs, ", "), id,
				)}
			}
			envProjects = append(envProjects, orchestrator.WorkspaceEnvelopeProject{
				Name: name,
				URL:  p.UpstreamURL,
			})
		}
		initScript, err := s.exportWorkspaceInitScript(snap.Slug)
		if err != nil {
			return nil, err
		}
		envelope := orchestrator.NewWorkspaceEnvelopeFromMeta(snap.Slug, snap.Meta, envProjects, initScript)
		data, err := orchestrator.MarshalWorkspaceEnvelope(envelope)
		if err != nil {
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: fmt.Sprintf("marshal workspace %q: %v", snap.Slug, err)}
		}
		if err := checkWorkspaceEnvelopeIsApplicable(snap.Slug, data, len(initScript)); err != nil {
			return nil, err
		}
		docs = append(docs, data)
	}
	return bytes.Join(docs, []byte("---\n")), nil
}

func (s *ProjectAppService) projectCollisions(allProjects map[string]*orchestrator.Project) (byName map[string][]string, nameCollidesWithOtherID map[string][]string, effectiveNames map[string]string) {
	for _, p := range allProjects {
		s.hydrateProject(p)
	}

	effectiveNames = make(map[string]string, len(allProjects))
	byNameIDs := map[string][]string{}
	for id, p := range allProjects {
		effectiveNames[id] = p.Meta.Name
		if p.Meta.Name == "" {
			continue
		}
		byNameIDs[p.Meta.Name] = append(byNameIDs[p.Meta.Name], id)
	}

	byName = map[string][]string{}
	for name, ids := range byNameIDs {
		if len(ids) > 1 {
			byName[name] = ids
		}
	}

	nameCollidesWithOtherID = map[string][]string{}
	for name, ids := range byNameIDs {
		other, exists := allProjects[name]
		if !exists {
			continue
		}
		if len(ids) == 1 && ids[0] == other.ID {
			continue
		}
		nameCollidesWithOtherID[name] = []string{other.ID}
	}
	return byName, nameCollidesWithOtherID, effectiveNames
}

func (s *ProjectAppService) DeleteProject(id string) error {
	s.projectMu.Lock()
	defer s.projectMu.Unlock()

	project, getErr := s.Projects.GetProject(id)
	if getErr != nil {
		return &StatusError{Code: http.StatusNotFound, Message: getErr.Error()}
	}

	if s.isManagedBareRepoPath(project.WorkDir) {
		if rmErr := os.RemoveAll(project.WorkDir); rmErr != nil {
			return &StatusError{Code: http.StatusInternalServerError, Message: fmt.Sprintf(
				"remove daemon-managed bare repo at %s: %v; project %q was NOT removed — fix the underlying issue and retry `boid project rm`",
				project.WorkDir, rmErr, id,
			)}
		}
	}

	if err := s.Projects.DeleteProject(id); err != nil {
		return &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	s.Meta.Remove(id)
	return nil
}

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

func (s *ProjectAppService) ReloadProjects() (*ProjectReloadResult, error) {
	s.mu.Lock()
	projects, err := s.Projects.ListProjects()
	if err != nil {
		s.mu.Unlock()
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	errs := s.Meta.LoadAll(projects)
	s.mu.Unlock()

	var messages []string
	for _, err := range errs {
		messages = append(messages, err.Error())
	}

	if s.CaptureUpstreamURL != nil {
		for _, p := range projects {
			upstreamURL, err := s.CaptureUpstreamURL(p.WorkDir)
			if err != nil {
				slog.Warn("project reload: could not capture upstream_url; add a git remote and reload again",
					"project_id", p.ID, "work_dir", p.WorkDir, "error", err)
				messages = append(messages, fmt.Sprintf("project %q: upstream_url not captured: %v", p.ID, err))
				continue
			}
			if upstreamURL == p.UpstreamURL {
				continue
			}
			if err := s.Projects.SetProjectUpstreamURL(p.ID, upstreamURL); err != nil {
				slog.Warn("project reload: failed to persist captured upstream_url",
					"project_id", p.ID, "error", err)
				messages = append(messages, fmt.Sprintf("project %q: failed to persist upstream_url: %v", p.ID, err))
			}
		}
	}

	if len(messages) == 0 {
		return &ProjectReloadResult{Status: "ok"}, nil
	}
	return &ProjectReloadResult{
		Status: "partial",
		Errors: messages,
	}, nil
}

// registrationPerm defaults omitted permissions to push and validates explicit values.
func registrationPerm(requestedPerm string) (string, error) {
	perm := "push"
	if requestedPerm != "" {
		perm = requestedPerm
	}
	if err := orchestrator.ValidateProjectPerm(perm); err != nil {
		return "", &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	return perm, nil
}

// SetProjectPerm changes a repository's permission for subsequent task creation and dispatch.
func (s *ProjectAppService) SetProjectPerm(id, perm string) (*orchestrator.Project, error) {
	if err := orchestrator.ValidateProjectPerm(perm); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	_, err := s.Projects.GetProject(id)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	if err := s.Projects.SetProjectPerm(id, perm); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	updated, err := s.Projects.GetProject(id)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	return s.hydrateProject(updated), nil
}

// GetWorkspaceRuntimeMeta resolves repository-free exec configuration.
func (s *ProjectAppService) GetWorkspaceRuntimeMeta(ctx context.Context, workspaceID string) (*orchestrator.ProjectMeta, error) {
	source, ok := s.Meta.(interface {
		GetWorkspaceMeta(context.Context, string) (*orchestrator.ProjectMeta, error)
	})
	if !ok {
		return nil, fmt.Errorf("workspace meta unavailable")
	}
	return source.GetWorkspaceMeta(ctx, workspaceID)
}
