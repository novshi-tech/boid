package api

import (
	"net/http"
	"sort"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func (s *WebAppService) ListCardProjects() ([]*orchestrator.Project, error) {
	if s.EnsureCardProjects != nil {
		if err := s.EnsureCardProjects(); err != nil {
			return nil, err
		}
	}
	projects, err := s.ListProjects()
	if err != nil {
		return nil, err
	}
	return cardProjects(projects), nil
}

func cardProjects(projects []*orchestrator.Project) []*orchestrator.Project {
	var result []*orchestrator.Project
	for _, p := range projects {
		if p.Status != orchestrator.StatusDegraded && orchestrator.IsCardProject(&p.Meta) {
			result = append(result, p)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].WorkspaceID != result[j].WorkspaceID {
			return result[i].WorkspaceID < result[j].WorkspaceID
		}
		return orchestrator.IsDefaultMetaproject(result[i]) && !orchestrator.IsDefaultMetaproject(result[j])
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
		projectID = orchestrator.DefaultMetaprojectID(workspace)
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
