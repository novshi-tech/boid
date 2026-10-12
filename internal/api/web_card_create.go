package api

import (
	"net/http"
	"sort"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func (s *WebAppService) ListCardProjects() ([]*orchestrator.Project, error) {
	projects, err := s.ListProjects()
	if err != nil {
		return nil, err
	}
	return cardProjects(projects), nil
}

func cardProjects(projects []*orchestrator.Project) []*orchestrator.Project {
	var result []*orchestrator.Project
	for _, p := range projects {
		_, hasCommand := p.Meta.CardCommands[p.Meta.CardEvents.Command]
		if p.Perm != "fetch" && p.Status != orchestrator.StatusDegraded && p.Meta.CardEvents.Command != "" && hasCommand {
			result = append(result, p)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].WorkspaceID != result[j].WorkspaceID {
			return result[i].WorkspaceID < result[j].WorkspaceID
		}
		return result[i].ID < result[j].ID
	})
	return result
}

func (h *WebHandler) listCardProjects() ([]*orchestrator.Project, error) {
	if svc, ok := h.Service.(interface {
		ListCardProjects() ([]*orchestrator.Project, error)
	}); ok {
		return svc.ListCardProjects()
	}
	projects, err := h.Service.ListProjects()
	return cardProjects(projects), err
}

func (h *WebHandler) resolveCardProject(workspace, projectID string) (string, error) {
	if workspace == "" {
		workspace = orchestrator.DefaultWorkspaceSlug
	}
	if projectID == "" {
		workspaces, err := h.Service.ListWorkspaces()
		if err != nil {
			return "", err
		}
		for _, ws := range workspaces {
			if ws.ID == workspace {
				return "", nil
			}
		}
		return "", &StatusError{Code: http.StatusBadRequest, Message: "このワークスペースは存在しません"}
	}
	projects, err := h.listCardProjects()
	if err != nil {
		return "", err
	}
	for _, p := range projects {
		if p.ID == projectID && p.WorkspaceID == workspace {
			return projectID, nil
		}
	}
	return "", &StatusError{Code: http.StatusBadRequest, Message: "このワークスペースのメタプロジェクトを選択してください"}
}

func (h *WebHandler) cardWorkspaceSlugs() []string {
	workspaces, err := h.Service.ListWorkspaces()
	if err != nil {
		return nil
	}
	slugs := make([]string, 0, len(workspaces))
	for _, ws := range workspaces {
		slugs = append(slugs, ws.ID)
	}
	return slugs
}
