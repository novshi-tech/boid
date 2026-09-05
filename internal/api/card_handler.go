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

// CardHandler serves the card read surface.
//
// Mounted at its own /api/cards root rather than as /api/tasks/{id}/cards +
// a sibling list route: the listing needs a collection endpoint of its own,
// and hanging it off /api/tasks would put a static segment in the same
// position as the {id} wildcard.
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
// orchestrator.TaskFilter the task listing uses, so "cards_live" ("triage"
// is still accepted as a compatibility alias) and any concrete status value
// all work here too. "queue_next" is ALSO still a valid string to pass, but
// it has no special membership predicate anymore — it falls through to a
// literal `t.status = 'queue_next'` match, which can never match a real
// row, so it deterministically returns an empty list rather than an error
// (store.go's own doc comment).
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
