package server

import (
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/project"
	"github.com/novshi-tech/boid/internal/worktree"
)

type worktreePreparer struct {
	manager *worktree.Manager
}

func (p worktreePreparer) Prepare(task *orchestrator.Task, proj *project.Project, behavior *project.TaskBehavior) (string, error) {
	if p.manager == nil {
		return "", nil
	}

	existing, err := p.manager.Get(task.ID)
	if err != nil {
		return "", err
	}
	if existing != nil && existing.CleanedAt == nil {
		return existing.Path, nil
	}

	w, err := p.manager.Create(
		proj.WorkDir,
		proj.ID,
		task.ID,
		behavior.BranchPrefix,
		behavior.BaseBranch,
	)
	if err != nil {
		return "", err
	}
	return w.Path, nil
}
