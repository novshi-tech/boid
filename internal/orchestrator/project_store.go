package orchestrator

import (
	"fmt"
	"sync"

	"github.com/novshi-tech/boid/internal/project"
)

// KitResolver resolves a kit reference string to a filesystem directory.
type KitResolver = project.KitResolver

// ProjectStore holds project metadata in memory, loaded from project.yaml files.
type ProjectStore struct {
	mu       sync.RWMutex
	metas    map[string]*project.ProjectMeta
	resolver KitResolver
}

// NewProjectStore creates a new store. If resolver is non-nil, kit references
// in project.yaml files will be resolved and merged at load time.
func NewProjectStore(resolver KitResolver) *ProjectStore {
	return &ProjectStore{
		metas:    make(map[string]*project.ProjectMeta),
		resolver: resolver,
	}
}

// Load reads project.yaml from the work_dir and stores the meta in memory.
func (s *ProjectStore) Load(workDir string) (*project.ProjectMeta, error) {
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
func (s *ProjectStore) Get(id string) (*project.ProjectMeta, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	meta, ok := s.metas[id]
	return meta, ok
}

// Set stores meta directly.
func (s *ProjectStore) Set(id string, meta *project.ProjectMeta) {
	s.mu.Lock()
	s.metas[id] = meta
	s.mu.Unlock()
}

// Remove deletes a project's meta from the store.
func (s *ProjectStore) Remove(id string) {
	s.mu.Lock()
	delete(s.metas, id)
	s.mu.Unlock()
}

// LoadAll reads project.yaml for each registered project.
func (s *ProjectStore) LoadAll(projects []*project.Project) []error {
	var errs []error
	for _, candidate := range projects {
		if _, err := s.Load(candidate.WorkDir); err != nil {
			errs = append(errs, fmt.Errorf("project %q: %w", candidate.ID, err))
		}
	}
	return errs
}
