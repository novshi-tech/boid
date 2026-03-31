package orchestrator

import "github.com/novshi-tech/boid/internal/db"

type DBProjectCatalog struct {
	DB db.DBTX
}

func (c DBProjectCatalog) GetProject(id string) (*Project, error) {
	return GetProject(c.DB, id)
}

func (c DBProjectCatalog) ListProjects() ([]*Project, error) {
	return ListProjects(c.DB)
}

type DBTaskLookup struct {
	DB db.DBTX
}

func (l DBTaskLookup) GetTask(id string) (*Task, error) {
	return GetTask(l.DB, id)
}
