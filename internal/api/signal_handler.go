package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// SignalHandler serves docs/plans/signal-ingest-detailed-design.md §3.1
// (PR-2)'s host API: GET /api/signals (list) and POST /api/signals/ack.
//
// Store is SignalStore (internal/api/store.go, added by PR-1) — wire.go
// mounts this handler directly against runtime.taskRepo, which already
// implements SignalStore (see that interface's own doc comment). No new
// service type is needed the way CardHandler needs CardReadService: the
// store interface PR-1 shipped is already narrow enough to hand straight to
// a handler, following the same "thin handler over a narrow store
// interface" shape CardHandler uses.
type SignalHandler struct {
	Store SignalStore
}

func (h *SignalHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Post("/ack", h.Ack)
	return r
}

// List handles GET /api/signals?workspace_id=&source=&service=&state=&limit=.
//
// workspace_id defaults to orchestrator.DefaultWorkspaceSlug ("default")
// when omitted — the CLI's --workspace flag already resolves this default
// before sending the request (cmd/signal.go), but the handler defaults it
// too so any direct API caller (curl, a future non-CLI client) gets the
// same behavior rather than SignalFilter's "workspace id must not be empty"
// error.
//
// source maps to SignalFilter.Connector UNCHANGED — the signals table
// stores connector as the composite "<pack>/<connector>" string
// (signal-ingest-detailed-design.md §2), so --source slack/mentions and the
// stored value match exactly with no split needed for filtering (only the
// response envelope's `source` block splits it back apart, in
// toWireSignals below).
func (h *SignalHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	workspaceID := q.Get("workspace_id")
	if workspaceID == "" {
		workspaceID = orchestrator.DefaultWorkspaceSlug
	}

	filter := orchestrator.SignalFilter{
		WorkspaceID: workspaceID,
		Service:     q.Get("service"),
		Connector:   q.Get("source"),
		State:       orchestrator.SignalState(q.Get("state")),
	}

	if limitStr := q.Get("limit"); limitStr != "" {
		limit, err := strconv.Atoi(limitStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid limit: "+limitStr)
			return
		}
		filter.Limit = limit
	}

	signals, err := h.Store.ListSignals(filter)
	if err != nil {
		writeServiceError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, ListSignalsResponse{Signals: toWireSignals(signals)})
}

// Ack handles POST /api/signals/ack. Body: AckSignalsRequest
// ({"workspace_id": "...", "ids": [...]}); workspace_id defaults the same
// way List's query param does.
//
// AckSignals (internal/orchestrator/signal_store.go) is itself idempotent —
// re-acking an already-acked id is a no-op, not an error (signal-driven-
// review.md §14 Q14) — so this handler adds no extra dedup/already-acked
// check of its own; doing so would risk diverging from the store's own
// idempotency contract. The only validation here is "ids must not be
// empty", which is a client-input bug worth rejecting up front rather than
// forwarding a request AckSignals would silently no-op on.
func (h *SignalHandler) Ack(w http.ResponseWriter, r *http.Request) {
	var req AckSignalsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "ids must not be empty")
		return
	}

	workspaceID := req.WorkspaceID
	if workspaceID == "" {
		workspaceID = orchestrator.DefaultWorkspaceSlug
	}

	if err := h.Store.AckSignals(workspaceID, req.IDs); err != nil {
		// AckSignals' only error path is "unknown id(s)" (typo detection,
		// signal-ingest-detailed-design.md §2) — a client input problem, so
		// this maps to 400 rather than writeServiceError's 500 default.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, AckSignalsResponse{Acked: req.IDs})
}

// toWireSignals shapes store rows into the v0 envelope
// (signal-driven-review.md §5.2), splitting each row's stored
// "<pack>/<connector>" composite Connector back into the envelope's
// separate source.pack/source.connector fields.
func toWireSignals(signals []*orchestrator.Signal) []Signal {
	out := make([]Signal, 0, len(signals))
	for _, s := range signals {
		pack, connector := splitPackConnector(s.Connector)
		out = append(out, Signal{
			ID:         s.ID,
			OccurredAt: s.OccurredAt,
			Source: SignalSource{
				Pack:      pack,
				Connector: connector,
				Service:   s.Service,
			},
			Identity:   s.Identity,
			URL:        s.URL,
			Author:     s.Author,
			Title:      s.Title,
			ReceivedAt: s.ReceivedAt,
			Attempts:   s.Attempts,
			AckedAt:    s.AckedAt,
		})
	}
	return out
}

// splitPackConnector splits a stored "<pack>/<connector>" composite into its
// two halves. A value with no "/" (should never happen for a real
// connector-produced row, but the wire layer must not panic on it) comes
// back as an empty Pack with the whole string as Connector.
func splitPackConnector(composite string) (pack, connector string) {
	pack, connector, ok := strings.Cut(composite, "/")
	if !ok {
		return "", composite
	}
	return pack, connector
}
