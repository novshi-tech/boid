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
	snapshot, ch, cancel, ok, finished := h.Subscriber.Subscribe(jobID)
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
		if !finished {
			// The job is still genuinely running but has no live stream
			// to offer right now (Opus review of PR #857, B2) — same
			// underlying condition as WSAttachHandler's own !finished
			// branch, see its doc comment for the full rationale. A bare
			// return here (this branch's entire behavior before B2)
			// silently ends the SSE stream with no signal at all — the
			// Web UI's EventSource just stops receiving further "message"
			// events and never learns whether the job actually finished
			// or the stream merely dropped. A distinctly-named SSE event
			// (not the default "message" event sendLines' own `data:`
			// lines use, so this never gets mistaken for ordinary log
			// output by the existing job-log EventSource listener) lets
			// the client tell the two apart — web/templates/jobs.templ's
			// own `boid-stream-unavailable` listener renders a notice
			// instead of silently going quiet.
			fmt.Fprintf(w, "event: boid-stream-unavailable\ndata: job is still running but has no live output stream right now\n\n") //nolint:errcheck
			flush()
		}
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
