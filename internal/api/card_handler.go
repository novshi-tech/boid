package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// CardReadService is the read surface CardHandler needs — narrowed from
// *TaskWorkflowService so the handler can be tested against a fake.
type CardReadService interface {
	GetCard(taskID string) (*CardView, error)
	ListCards(filter orchestrator.TaskFilter) ([]*CardView, error)
}

// CardHandler serves the card read surface (docs/plans/
// cross-project-issue-triage.md Phase 1 PR-5a).
//
// Mounted at its own /api/cards root (renamed from /api/triage by
// docs/plans/card-model-cleanup.md PR-3 §4) rather than as
// /api/tasks/{id}/cards + a sibling list route: the listing needs a
// collection endpoint of its own, and hanging it off /api/tasks would put a
// static segment in the same position as the {id} wildcard.
type CardHandler struct {
	Service CardReadService
}

func (h *CardHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Get("/{id}", h.Get)
	return r
}

// Get returns one triage task's full projection (stored columns + the
// actions-derived parked_from + the opaque detail blob).
func (h *CardHandler) Get(w http.ResponseWriter, r *http.Request) {
	view, err := h.Service.GetCard(chi.URLParam(r, "id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// List returns the projections of the triage tasks matching the query
// filters. project_id and status are passed straight through to the same
// orchestrator.TaskFilter the task listing uses, so "triage"/"queue_next"
// and any concrete status value all work here too ("queue", the old broad
// pre-execution-status superset, was removed in PR-2 — docs/plans/
// suggestion-as-state-transition-impl.md §4.1).
func (h *CardHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	views, err := h.Service.ListCards(orchestrator.TaskFilter{
		ProjectID:   q.Get("project_id"),
		WorkspaceID: q.Get("workspace_id"),
		Status:      q.Get("status"),
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}
