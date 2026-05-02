package api

import (
	"bufio"
	"bytes"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/api/auth"
	"github.com/novshi-tech/boid/internal/dispatcher"
)

// JobLogSSEHandler streams live job output as Server-Sent Events on
// GET /{id}/log?follow=true.
type JobLogSSEHandler struct {
	Subscriber dispatcher.RuntimeSubscriber
	Registry   *auth.ConnectionRegistry
}

func (h *JobLogSSEHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("follow") != "true" {
		http.NotFound(w, r)
		return
	}

	jobID := chi.URLParam(r, "id")
	snapshot, ch, cancel, ok := h.Subscriber.Subscribe(jobID)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)

	flush := func() {
		if canFlush {
			flusher.Flush()
		}
	}

	sendLines := func(data []byte) {
		if len(data) == 0 {
			return
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		for sc.Scan() {
			fmt.Fprintf(w, "data: %s\n\n", sc.Text()) //nolint:errcheck
		}
		flush()
	}

	sendLines(snapshot)
	flush() // スナップショットが空でもヘッダーをクライアントに送信

	if !ok || ch == nil {
		return
	}

	var revokeCh <-chan struct{}
	if h.Registry != nil {
		if deviceID, ok := auth.DeviceIDFromContext(r.Context()); ok {
			var release func()
			revokeCh, release = h.Registry.Register(deviceID)
			defer release()
		}
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case chunk, more := <-ch:
			if !more {
				return
			}
			sendLines(chunk)
		case <-revokeCh:
			return
		}
	}
}
