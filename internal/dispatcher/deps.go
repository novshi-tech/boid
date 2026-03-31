package dispatcher

import "github.com/novshi-tech/boid/internal/project"

// MetaCache is a consumer-side interface for project meta lookup.
// project.Store satisfies this interface.
type MetaCache interface {
	Get(id string) (*project.ProjectMeta, bool)
}
