package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/api/auth"
)

// TaskEvents streams Server-Sent Events for a specific task.
// Subscribes to the TaskEventHub and forwards events until the client disconnects
// or the device is revoked.
func (h *WebHandler) TaskEvents(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if h.Hub == nil {
		http.Error(w, "SSE not available", http.StatusServiceUnavailable)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := h.Hub.Subscribe(r.Context(), id)

	var revokeCh <-chan struct{}
	if h.Registry != nil {
		if deviceID, ok := auth.DeviceIDFromContext(r.Context()); ok {
			var release func()
			revokeCh, release = h.Registry.Register(deviceID)
			defer release()
		}
	}

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(ev.Payload)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Kind, data)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprintf(w, ":ping\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		case <-revokeCh:
			return
		}
	}
}
