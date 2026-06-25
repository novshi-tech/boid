package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
)

// ProjectStore holds project metadata in memory, loaded from project.yaml files.
type ProjectStore struct {
	mu             sync.RWMutex
	metas          map[string]*ProjectMeta
	workspaceIDs   map[string]string // projectID → workspaceID (empty if unlinked)
	resolver       KitResolver
	workspaceStore *WorkspaceStore
}

// NewProjectStore creates a new store. If resolver is non-nil, kit references
// in project.yaml files will be resolved and merged at load time.
func NewProjectStore(resolver KitResolver) *ProjectStore {
	return &ProjectStore{
		metas:        make(map[string]*ProjectMeta),
		workspaceIDs: make(map[string]string),
		resolver:     resolver,
	}
}

// SetWorkspaceStore configures the workspace store used by GetWithWorkspace.
// Call this before LoadAll when workspace hydration is desired.
func (s *ProjectStore) SetWorkspaceStore(ws *WorkspaceStore) {
	s.workspaceStore = ws
}

// Load reads project.yaml from the work_dir and stores the meta in memory.
func (s *ProjectStore) Load(workDir string) (*ProjectMeta, error) {
	meta, err := ReadProjectMetaWithKits(workDir, s.resolver)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.metas[meta.ID] = meta
	s.mu.Unlock()
	return meta, nil
}

// Get returns the cached meta for a project.
func (s *ProjectStore) Get(id string) (*ProjectMeta, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	meta, ok := s.metas[id]
	return meta, ok
}

// GetWithWorkspace returns a ProjectMeta hydrated with workspace-level
// capabilities, kits, env, and SecretNamespace injection.
//
// Hydration rules:
//   - If the project has no linked workspace, returns the cached meta unchanged.
//   - If linked: always injects meta.SecretNamespace = workspaceID.
//   - On workspace.yaml load success: merges Capabilities, kits, and Env.
//   - On os.ErrNotExist (degraded window): logs a warning, returns meta with
//     only SecretNamespace injected (no error).
//   - On other errors: returns nil and the error.
//
// The returned *ProjectMeta is a fresh copy when hydration occurs; callers
// must not mutate the value returned when workspaceID is empty (it is the
// cached pointer).
func (s *ProjectStore) GetWithWorkspace(_ context.Context, projectID string) (*ProjectMeta, error) {
	s.mu.RLock()
	meta, ok := s.metas[projectID]
	workspaceID := s.workspaceIDs[projectID]
	s.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("project %q: meta not loaded", projectID)
	}
	if workspaceID == "" {
		return meta, nil
	}

	// Shallow-clone meta so we can mutate runtime-only fields without
	// corrupting the shared cached copy.
	out := cloneProjectMeta(meta)

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
		if errors.Is(err, os.ErrNotExist) {
			slog.Warn("workspace.yaml not found; running in degraded mode (capabilities/kits/env not injected)",
				"project_id", projectID, "workspace_id", workspaceID)
			return out, nil
		}
		return nil, fmt.Errorf("project %q: load workspace %q: %w", projectID, workspaceID, err)
	}

	// Capabilities: workspace overrides project (e.g. enables docker proxy).
	if ws.Capabilities.Docker != nil {
		out.Capabilities = ws.Capabilities
	}

	// Workspace kits are resolved and merged into top-level runtime fields
	// (HostCommands, AdditionalBindings, Env) and into each TaskBehavior's
	// Hooks / KitRoots / Env / HostCommands / AdditionalBindings. They act at
	// lower priority than project.yaml entries — project wins on conflict.
	if len(ws.Kits) > 0 {
		var wsKitMetas []*KitMeta
		var wsAgents []string
		for _, ref := range ws.Kits {
			kRef := KitRef{Ref: ref}
			kitDir, err := resolveKitRef(kRef.Ref, "", s.resolver)
			if err != nil {
				slog.Warn("workspace kit unresolved; ignoring",
					"workspace_id", workspaceID, "ref", ref, "error", err.Error())
				continue
			}
			km, err := ReadKitMeta(kitDir)
			if err != nil {
				return nil, fmt.Errorf("project %q: workspace kit %q: %w", projectID, ref, err)
			}
			wsKitMetas = append(wsKitMetas, km)
			wsAgents = append(wsAgents, ResolveKitAgent(kRef))
		}

		if len(wsKitMetas) > 0 {
			rt, err := MergeKitRuntime(wsKitMetas, wsAgents)
			if err != nil {
				return nil, fmt.Errorf("project %q: workspace kit merge: %w", projectID, err)
			}
			// project.yaml top-level overrides workspace kits.
			out.HostCommands = mergeHostCommands(rt.HostCommands, out.HostCommands)
			out.AdditionalBindings = mergeBindMounts(rt.AdditionalBindings, out.AdditionalBindings)
			out.Env = mergeStringMaps(rt.Env, out.Env)
			if err := validateBuiltinHostConflict("workspace kits", out.HostCommands); err != nil {
				return nil, fmt.Errorf("project %q: %w", projectID, err)
			}

			// Merge workspace kits into each TaskBehavior so kit-provided
			// hooks / env / bindings / host_commands surface at dispatch
			// time. This mirrors the per-behavior merge that ReadProjectMetaWithKits
			// used to do for project-level kits.
			if out.TaskBehaviors == nil {
				out.TaskBehaviors = make(map[string]TaskBehavior)
			}
			// Strip alias mirrors so each canonical behavior is only merged once;
			// re-add them after.
			out.TaskBehaviors = stripAliasMirrors(out.TaskBehaviors)
			for name, behavior := range out.TaskBehaviors {
				if err := MergeKitMetaIntoBehavior(&behavior, wsKitMetas, wsAgents); err != nil {
					return nil, fmt.Errorf("project %q: behavior %q: workspace kit merge: %w", projectID, name, err)
				}
				out.TaskBehaviors[name] = behavior
			}
			out.TaskBehaviors = addAliasMirrors(out.TaskBehaviors)
		}
	}

	// workspace.Env is applied on top of kit env but below project.yaml env.
	// The merge above (mergeStringMaps(rt.Env, out.Env)) has already placed
	// project env in out.Env; applying workspace env as the new base preserves
	// that precedence: mergeStringMaps(ws.Env, out.Env) → out.Env wins.
	if len(ws.Env) > 0 {
		out.Env = mergeStringMaps(ws.Env, out.Env)
	}

	return out, nil
}

// Set stores meta directly.
func (s *ProjectStore) Set(id string, meta *ProjectMeta) {
	s.mu.Lock()
	s.metas[id] = meta
	s.mu.Unlock()
}

// Remove deletes a project's meta from the store.
func (s *ProjectStore) Remove(id string) {
	s.mu.Lock()
	delete(s.metas, id)
	delete(s.workspaceIDs, id)
	s.mu.Unlock()
}

// LoadAll reads project.yaml for each registered project and records each
// project's workspaceID so that GetWithWorkspace can hydrate at call time.
func (s *ProjectStore) LoadAll(projects []*Project) []error {
	var errs []error
	for _, candidate := range projects {
		if _, err := s.Load(candidate.WorkDir); err != nil {
			s.Remove(candidate.ID)
			errs = append(errs, fmt.Errorf("project %q: %w", candidate.ID, err))
			continue
		}
		// Record workspace association (empty for unlinked projects).
		s.mu.Lock()
		s.workspaceIDs[candidate.ID] = candidate.WorkspaceID
		s.mu.Unlock()
	}
	return errs
}
