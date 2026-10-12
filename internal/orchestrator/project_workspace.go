package orchestrator

import (
	"fmt"
	"github.com/novshi-tech/boid/internal/db"
)

// ProjectWorkspaceID resolves repository membership, falling back to the default workspace.
func ProjectWorkspaceID(dbtx db.DBTX, projectID string) (string, error) {
	var workspaceID string
	err := dbtx.QueryRow(`SELECT COALESCE((SELECT NULLIF(workspace_id, '') FROM project_workspaces WHERE project_id = ?), ?)`, projectID, DefaultWorkspaceSlug).Scan(&workspaceID)
	if err != nil {
		return "", fmt.Errorf("resolve project workspace: %w", err)
	}
	return workspaceID, nil
}
