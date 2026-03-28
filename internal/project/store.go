package project

import (
	"fmt"
	"sync"

	"github.com/novshi-tech/boid/internal/model"
)

// Store holds project metadata in memory, loaded from project.yaml files.
type Store struct {
	mu    sync.RWMutex
	metas map[string]*model.ProjectMeta // keyed by project ID
}

func NewStore() *Store {
	return &Store{metas: make(map[string]*model.ProjectMeta)}
}

// Load reads project.yaml from the work_dir and stores the meta in memory.
func (s *Store) Load(workDir string) (*model.ProjectMeta, error) {
	meta, err := ReadMeta(workDir)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.metas[meta.ID] = meta
	s.mu.Unlock()
	return meta, nil
}

// Get returns the cached meta for a project.
func (s *Store) Get(id string) (*model.ProjectMeta, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.metas[id]
	return m, ok
}

// Set stores meta directly (useful for tests).
func (s *Store) Set(id string, meta *model.ProjectMeta) {
	s.mu.Lock()
	s.metas[id] = meta
	s.mu.Unlock()
}

// Remove deletes a project's meta from the store.
func (s *Store) Remove(id string) {
	s.mu.Lock()
	delete(s.metas, id)
	s.mu.Unlock()
}

// LoadAll reads project.yaml for each registered project.
// Returns errors for projects that failed to load (but continues loading others).
func (s *Store) LoadAll(projects []*model.Project) []error {
	var errs []error
	for _, p := range projects {
		if _, err := s.Load(p.WorkDir); err != nil {
			errs = append(errs, fmt.Errorf("project %q: %w", p.ID, err))
		}
	}
	return errs
}
