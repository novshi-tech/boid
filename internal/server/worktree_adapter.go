package server

import (
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type worktreePreparer struct {
	manager *dispatcher.WorktreeManager
}

func (p worktreePreparer) Prepare(task *orchestrator.Task, proj *orchestrator.Project) (string, error) {
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
		task.BranchPrefix,
		task.BaseBranch,
	)
	if err != nil {
		return "", err
	}
	return w.Path, nil
}
