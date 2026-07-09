package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

type ProjectAppService struct {
	Projects ProjectRepository
	Meta     interface {
		Load(workDir string) (*orchestrator.ProjectMeta, error)
		Get(id string) (*orchestrator.ProjectMeta, bool)
		Remove(id string)
		LoadAll(projects []*orchestrator.Project) []error
		SetWorkspaceID(projectID, workspaceID string)
	}
	// Hydrator is optional. When set, callers that need fully-resolved meta
	// (workspace-level capabilities / kits / env merged in + SecretNamespace
	// injected) go through GetWithWorkspace instead of the bare Meta.Get path.
	// Wired in internal/server/wire.go alongside the workspace store.
	Hydrator orchestrator.MetaHydrator
	// CaptureUpstreamURL reads a project work_dir's git origin remote and
	// normalizes it to HTTPS (docs/plans/git-gateway-cutover.md PR2). Wired
	// in internal/server/wire.go to dispatcher.CaptureUpstreamURL. When nil
	// (e.g. in tests that do not exercise this path), upstream_url capture
	// is skipped entirely rather than panicking.
	CaptureUpstreamURL func(workDir string) (string, error)
}

// hydrateProject fills project.Meta from the cached raw meta. Use this when
// the caller only needs project-yaml-level fields (e.g. Name for ref
// matching) and the extra workspace.yaml read would be wasted.
func (s *ProjectAppService) hydrateProject(project *orchestrator.Project) *orchestrator.Project {
	if project == nil {
		return nil
	}
	if meta, ok := s.Meta.Get(project.ID); ok {
		project.Meta = *meta
	}
	return project
}

// hydrateProjectWithWorkspace fills project.Meta with workspace-aware
// hydration when a Hydrator is wired; otherwise it falls back to the bare
// cache. Use this for paths that build sandbox specs (session / exec /
// hook), where workspace Capabilities / Env / SecretNamespace must take
// effect.
func (s *ProjectAppService) hydrateProjectWithWorkspace(ctx context.Context, project *orchestrator.Project) *orchestrator.Project {
	if project == nil {
		return nil
	}
	if s.Hydrator != nil {
		meta, err := s.Hydrator.GetWithWorkspace(ctx, project.ID)
		if err == nil && meta != nil {
			project.Meta = *meta
			return project
		}
		// Fall back to bare meta on hydration error so the API stays usable
		// even when workspace.yaml is malformed. The hydrator already logs.
		slog.Warn("project meta hydration failed; falling back to raw meta",
			"project_id", project.ID, "error", err)
	}
	return s.hydrateProject(project)
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

	// Capture the project's git origin remote as upstream_url (SSH → HTTPS
	// normalized). docs/plans/git-gateway-cutover.md PR2 makes this mandatory:
	// a project with no remote cannot be registered ("新しい意味論"). Reject
	// with a clear remediation message rather than silently registering a
	// project that will need a remote before it can ever dispatch under the
	// eventual gateway cutover.
	if s.CaptureUpstreamURL != nil {
		upstreamURL, err := s.CaptureUpstreamURL(workDir)
		if err != nil {
			s.Meta.Remove(meta.ID)
			return nil, &StatusError{
				Code: http.StatusBadRequest,
				Message: fmt.Sprintf(
					"project %q has no git remote configured (%v); add one (e.g. `git remote add origin <url>`) and re-run `boid project add`",
					meta.ID, err,
				),
			}
		}
		project.UpstreamURL = upstreamURL
	}

	if err := s.Projects.CreateProject(project); err != nil {
		s.Meta.Remove(meta.ID)
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	// Assign to the default workspace eagerly so the project is always
	// linked. The caller may overwrite this with `boid workspace assign`
	// (or the --workspace flag on `project add`) immediately after. A
	// failure here is non-fatal: daemon startup runs the same migration
	// idempotently, so the project will be linked on next boot.
	if err := s.Projects.SetProjectWorkspace(project.ID, orchestrator.DefaultWorkspaceSlug); err != nil {
		slog.Warn("CreateProject: default workspace assignment failed (project will be linked on next daemon start)",
			"project_id", project.ID, "error", err)
	} else {
		project.WorkspaceID = orchestrator.DefaultWorkspaceSlug
		s.Meta.SetWorkspaceID(project.ID, orchestrator.DefaultWorkspaceSlug)
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
	// GET /api/projects/{id} is the canonical "give me a sandbox-ready
	// snapshot" endpoint — `cmd/exec.go` and any future client builds a
	// SessionJobInput straight from the returned Meta. Hydrate with
	// workspace so Capabilities / Env / SecretNamespace are populated.
	return s.hydrateProjectWithWorkspace(context.Background(), project), nil
}

func (s *ProjectAppService) SetProjectWorkspace(id, workspaceID string) (*orchestrator.Project, error) {
	// API-layer validation per plan (3-layer defense). Empty string clears the
	// assignment and is handled at the repository layer; non-empty values must
	// satisfy ValidWorkspaceSlug or we return 400 Bad Request early.
	if workspaceID != "" {
		if err := orchestrator.ValidWorkspaceSlug(workspaceID); err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
		}
	}
	project, err := s.Projects.GetProject(id)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	if err := s.Projects.SetProjectWorkspace(id, workspaceID); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	// Propagate the workspace association into the in-memory ProjectStore so
	// the next GetWithWorkspace call sees the new value (otherwise the stale
	// empty workspaceID stays cached until daemon restart).
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

func (s *ProjectAppService) ReloadProjects() (*ProjectReloadResult, error) {
	projects, err := s.Projects.ListProjects()
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	errs := s.Meta.LoadAll(projects)

	var messages []string
	for _, err := range errs {
		messages = append(messages, err.Error())
	}

	// Re-capture upstream_url for every project on reload (docs/plans/
	// git-gateway-cutover.md PR2: "capture タイミング: project add / project
	// reload 時"). This both keeps upstream_url in sync with a project's
	// current origin remote and heals rows left NULL by the startup backfill
	// (e.g. a remote added after registration). Capture failures are logged
	// and surfaced as reload warnings, never fatal to the reload as a whole.
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
