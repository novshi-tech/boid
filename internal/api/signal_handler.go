package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// SignalHandler serves the host API: GET /api/signals (list) and
// POST /api/signals/ack. Store is SignalStore (internal/api/store.go);
// wire.go mounts this handler directly against runtime.taskRepo, which
// already implements it — no separate service type is needed here, unlike
// CardHandler's CardReadService.
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
// workspace_id defaults to orchestrator.DefaultWorkspaceSlug when omitted,
// so any direct API caller gets the same behavior as the CLI. source maps
// to SignalFilter.Connector unchanged (the stored composite
// "<pack>/<connector>" string) — only the response envelope splits it back
// apart, in toWireSignals below.
func (h *SignalHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	workspaceID := q.Get("workspace_id")
	if workspaceID == "" {
		workspaceID = orchestrator.DefaultWorkspaceSlug
	}

	state := orchestrator.SignalState(q.Get("state"))
	switch state {
	case "", orchestrator.SignalStatePending, orchestrator.SignalStateDead, orchestrator.SignalStateAcked, orchestrator.SignalStateAll:
		// valid (empty defaults to "pending" in ListSignals)
	default:
		// Validated here so an invalid --state maps to 400 like an invalid
		// --limit does, rather than ListSignals' own plain-error 500.
		writeError(w, http.StatusBadRequest, "invalid state: "+string(state))
		return
	}

	filter := orchestrator.SignalFilter{
		WorkspaceID: workspaceID,
		Service:     q.Get("service"),
		Connector:   q.Get("source"),
		State:       state,
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
// AckSignals is itself idempotent (re-acking an id is a no-op, not an
// error), so this handler adds no extra dedup check of its own. The only
// validation here is "ids must not be empty".
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
		// AckSignals' only error path is "unknown id(s)" — a client input
		// problem, so this maps to 400 rather than writeServiceError's 500.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// req.IDs may contain repeats; AckSignalsResponse.Acked promises the
	// de-duplicated ids, so echo them back deduped rather than verbatim.
	writeJSON(w, http.StatusOK, AckSignalsResponse{Acked: dedupeStrings(req.IDs)})
}

// dedupeStrings returns ss with duplicates removed, keeping first-seen
// order. Small local helper rather than reusing orchestrator's own
// dedupeStringsPreserveOrder (signal_store.go), which is unexported.
func dedupeStrings(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// toWireSignals shapes store rows into the wire envelope, splitting each
// row's stored "<pack>/<connector>" composite Connector back into the
// envelope's separate source.pack/source.connector fields.
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
