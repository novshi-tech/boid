package api

import (
	"log/slog"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func cleanupWorktree(projects ProjectRepository, mgr *dispatcher.WorktreeManager, taskID, projectID string, status orchestrator.TaskStatus) {
	if projects == nil || mgr == nil || projectID == "" {
		return
	}

	project, err := projects.GetProject(projectID)
	if err != nil {
		slog.Warn("worktree cleanup project lookup failed", "task_id", taskID, "project_id", projectID, "error", err)
		return
	}
	if err := mgr.CleanupForTask(taskID, project.WorkDir, string(status)); err != nil {
		slog.Warn("worktree cleanup failed", "task_id", taskID, "project_id", projectID, "error", err)
	}
}
