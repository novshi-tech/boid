package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ProjectStore caches registered identities and resolves their workspace settings.
type ProjectStore struct {
	mu             sync.RWMutex
	metas          map[string]*ProjectMeta
	workspaceIDs   map[string]string // projectID → workspaceID (empty if unlinked)
	workspaceStore *WorkspaceStore
	hostCommands   map[string]HostCommandSpec
	statuses       map[string]ProjectStatus
}

// ProjectStatus is a project's in-memory health snapshot. See
// Status/MarkDegraded/MarkReady.
type ProjectStatus struct {
	// State is StatusReady or StatusDegraded.
	State string
	// Message explains why State is StatusDegraded (empty for StatusReady).
	Message string
}

const (
	StatusReady    = "ready"
	StatusDegraded = "degraded"
)

// NewProjectStore creates a new store.
func NewProjectStore() *ProjectStore {
	return &ProjectStore{
		metas:        make(map[string]*ProjectMeta),
		workspaceIDs: make(map[string]string),
	}
}

func (s *ProjectStore) MarkDegraded(projectID, message string) {
	s.mu.Lock()
	if s.statuses == nil {
		s.statuses = make(map[string]ProjectStatus)
	}
	s.statuses[projectID] = ProjectStatus{State: StatusDegraded, Message: message}
	s.mu.Unlock()
}

func (s *ProjectStore) MarkReady(projectID string) {
	s.mu.Lock()
	delete(s.statuses, projectID)
	s.mu.Unlock()
}

func (s *ProjectStore) Status(projectID string) ProjectStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if st, ok := s.statuses[projectID]; ok {
		return st
	}
	return ProjectStatus{State: StatusReady}
}

// SetWorkspaceStore configures the workspace store used by GetWithWorkspace.
// Call this before LoadAll when workspace hydration is desired.
func (s *ProjectStore) SetWorkspaceStore(ws *WorkspaceStore) {
	s.workspaceStore = ws
}

func (s *ProjectStore) WorkspaceStore() *WorkspaceStore {
	return s.workspaceStore
}

func (s *ProjectStore) SetHostCommands(hostCommands map[string]HostCommandSpec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hostCommands = hostCommands
}

func (s *ProjectStore) hostCommandSpec(name string) (HostCommandSpec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	spec, ok := s.hostCommands[name]
	return spec, ok
}

// Get returns the cached meta for a project.
func (s *ProjectStore) Get(id string) (*ProjectMeta, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	meta, ok := s.metas[id]
	return meta, ok
}

func (s *ProjectStore) GetWithWorkspace(_ context.Context, projectID string) (*ProjectMeta, error) {
	s.mu.RLock()
	meta, ok := s.metas[projectID]
	workspaceID := s.workspaceIDs[projectID]
	s.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("project %q: meta not loaded", projectID)
	}
	if workspaceID == "" {
		return &ProjectMeta{ID: meta.ID, Name: meta.Name, NameSource: meta.NameSource}, nil
	}

	return s.hydrateWorkspace(meta, projectID, workspaceID)
}

// GetWorkspaceMeta resolves runtime definitions without a primary repository.
func (s *ProjectStore) GetWorkspaceMeta(ctx context.Context, workspaceID string) (*ProjectMeta, error) {
	if err := ValidWorkspaceSlug(workspaceID); err != nil {
		return nil, err
	}
	if s.workspaceStore == nil {
		return nil, fmt.Errorf("workspace store not configured")
	}
	meta := WorkspaceDefaults()
	meta.Name = workspaceID
	meta.DefaultTaskBehavior = ""
	return s.hydrateWorkspace(meta, "", workspaceID)
}

func (s *ProjectStore) hydrateWorkspace(meta *ProjectMeta, projectID, workspaceID string) (*ProjectMeta, error) {

	out := WorkspaceDefaults()
	out.ID, out.Name, out.NameSource = meta.ID, meta.Name, meta.NameSource
	out.DefaultTaskBehavior = ""

	// Always inject SecretNamespace, even in the degraded (workspace.yaml
	// missing) window, so secret routing is stable regardless of disk state.
	out.SecretNamespace = workspaceID

	if s.workspaceStore == nil {
		slog.Warn("workspace store not configured; skipping workspace hydration",
			"project_id", projectID, "workspace_id", workspaceID)
		return out, nil
	}

	ws, err := s.workspaceStore.Load(workspaceID)
	if err != nil {
		if projectID != "" && errors.Is(err, os.ErrNotExist) {
			slog.Warn("workspace.yaml not found; running in degraded mode (capabilities/kits/env not injected)",
				"project_id", projectID, "workspace_id", workspaceID)
			return out, nil
		}
		return nil, fmt.Errorf("project %q: load workspace %q: %w", projectID, workspaceID, err)
	}

	ws = expandWorkspaceRuntimeForDispatch(ws)

	if ws.Capabilities.Docker != nil {
		out.Capabilities = ws.Capabilities
	}

	if len(ws.TaskBehaviors) > 0 {
		if out.TaskBehaviors == nil {
			out.TaskBehaviors = make(map[string]TaskBehavior)
		}
		for name, behavior := range ws.TaskBehaviors {

			out.TaskBehaviors[name] = behavior
		}
	}

	if len(ws.HostCommands) > 0 {
		resolved := make(HostCommands, len(ws.HostCommands))
		for _, name := range ws.HostCommands {
			spec, ok := s.hostCommandSpec(name)
			if !ok {
				slog.Warn("workspace host_commands reference unresolved; skipping",
					"project_id", projectID, "workspace_id", workspaceID, "name", name)
				continue
			}
			resolved[name] = spec
		}
		if len(resolved) > 0 {
			out.HostCommands = mergeHostCommands(resolved, out.HostCommands)
			if err := validateBuiltinHostConflict("workspace host_commands", out.HostCommands); err != nil {
				return nil, fmt.Errorf("project %q: %w", projectID, err)
			}
			if err := validateRejectRules(out.HostCommands); err != nil {
				return nil, fmt.Errorf("project %q: workspace host_commands: %w", projectID, err)
			}

			if out.TaskBehaviors == nil {
				out.TaskBehaviors = make(map[string]TaskBehavior)
			}
			for name, behavior := range out.TaskBehaviors {
				behavior.HostCommands = mergeHostCommands(resolved, behavior.HostCommands)
				out.TaskBehaviors[name] = behavior
			}
		}
	}

	if len(ws.Env) > 0 {
		out.Env = mergeStringMaps(ws.Env, out.Env)
		// Workspace env must also reach each behavior's Env so the planner's
		// PlanHook (which only reads behavior.Env, not meta.Env) picks it up.
		for name, behavior := range out.TaskBehaviors {
			behavior.Env = mergeStringMaps(ws.Env, behavior.Env)
			out.TaskBehaviors[name] = behavior
		}
	}

	out.BaseBranch = ws.BaseBranch
	out.ForkPoint = ws.ForkPoint
	out.DefaultTaskBehavior = ws.DefaultTaskBehavior

	if err := s.mergeWorkspaceAutomation(out, ws, workspaceID); err != nil {
		return nil, err
	}
	warnSignalConnectorServicesNotEnabled(projectID, workspaceID, out.Triggers, ws.Services)

	return out, nil
}

func (s *ProjectStore) Explain(projectID string) (*ProjectExplain, error) {
	s.mu.RLock()
	meta, ok := s.metas[projectID]
	workspaceID := s.workspaceIDs[projectID]
	s.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("project %q: meta not loaded", projectID)
	}

	var wsMeta *WorkspaceMeta
	workspaceUnavailable := false
	if workspaceID != "" && s.workspaceStore != nil {
		if loaded, err := s.workspaceStore.Load(workspaceID); err == nil {
			wsMeta = loaded
		} else if errors.Is(err, os.ErrNotExist) {
			slog.Warn("project explain: workspace.yaml not found; reporting as no workspace default applied",
				"project_id", projectID, "workspace_id", workspaceID)
		} else {
			workspaceUnavailable = true
			slog.Warn("project explain: workspace load failed; reporting affected fields as unavailable rather than unset",
				"project_id", projectID, "workspace_id", workspaceID, "error", err)
		}
	}

	return ComputeProjectExplain(projectID, workspaceID, meta, wsMeta, workspaceUnavailable), nil
}

// Set stores meta directly.
func (s *ProjectStore) Set(id string, meta *ProjectMeta) {
	s.mu.Lock()
	s.metas[id] = meta
	s.mu.Unlock()
}

func (s *ProjectStore) SetSynthesizedMeta(id string, meta *ProjectMeta) {
	s.mu.Lock()
	s.metas[id] = meta
	s.mu.Unlock()
	s.MarkReady(id)
}

func (s *ProjectStore) synthesizeMetaForReload(candidate *Project) *ProjectMeta {
	s.mu.RLock()
	prev, hadPrev := s.metas[candidate.ID]
	s.mu.RUnlock()

	name := ""
	nameSource := ""
	switch {
	case candidate.Name != "":
		name = candidate.Name
		nameSource = "database"
	case hadPrev && prev.Name != "":
		name = prev.Name
		nameSource = "cached"
	case candidate.WorkDir != "":
		base := filepath.Base(candidate.WorkDir)
		name = strings.TrimSuffix(base, ".git")
		nameSource = "basename"
	case candidate.UpstreamURL != "":
		if derived, err := DeriveProjectNameFromURL(candidate.UpstreamURL); err == nil {
			name = derived
			nameSource = "url"
		}
	}
	return &ProjectMeta{ID: candidate.ID, Name: name, NameSource: nameSource}
}

func (s *ProjectStore) CardEventCommand(projectID string) (string, bool) {
	meta, err := s.GetWithWorkspace(context.Background(), projectID)
	if err != nil {
		return "", false
	}
	return meta.CardEvents.Command, meta.CardEvents.Command != ""
}

func (s *ProjectStore) SetWorkspaceID(projectID, workspaceID string) {
	s.mu.Lock()
	if workspaceID == "" {
		delete(s.workspaceIDs, projectID)
	} else {
		s.workspaceIDs[projectID] = workspaceID
	}
	s.mu.Unlock()
}

// Remove deletes a project's meta from the store.
func (s *ProjectStore) Remove(id string) {
	s.mu.Lock()
	delete(s.metas, id)
	delete(s.workspaceIDs, id)
	s.mu.Unlock()
}

// LoadAll builds cached repository identities from registered database rows.
func (s *ProjectStore) LoadAll(projects []*Project) []error {
	for _, project := range projects {
		meta := s.synthesizeMetaForReload(project)
		s.SetSynthesizedMeta(project.ID, meta)
		s.SetWorkspaceID(project.ID, project.WorkspaceID)
	}
	return nil
}

func PathIsUnder(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func PathIsUnderResolved(root, path string) (bool, error) {
	resolvedRoot, err := resolveExistingAncestor(root)
	if err != nil {
		return false, fmt.Errorf("resolve %q: %w", root, err)
	}
	resolvedPath, err := resolveExistingAncestor(path)
	if err != nil {
		return false, fmt.Errorf("resolve %q: %w", path, err)
	}
	return PathIsUnder(resolvedRoot, resolvedPath), nil
}

func resolveExistingAncestor(p string) (string, error) {
	clean := filepath.Clean(p)
	var missingSuffix []string
	cur := clean
	for {
		if _, err := os.Lstat(cur); err == nil {
			resolved, evalErr := filepath.EvalSymlinks(cur)
			if evalErr != nil {
				return "", evalErr
			}
			for i := len(missingSuffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missingSuffix[i])
			}
			return resolved, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return clean, nil
		}
		missingSuffix = append(missingSuffix, filepath.Base(cur))
		cur = parent
	}
}
