package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/api/auth"
)

func newTestEventsRouter(hub *TaskEventHub) http.Handler {
	h := &WebHandler{Hub: hub}
	r := chi.NewRouter()
	r.Get("/api/tasks/{id}/events", h.TaskEvents)
	return r
}

// TestTaskEvents_StreamsEvents connects via real HTTP and reads 2-3 events.
func TestTaskEvents_StreamsEvents(t *testing.T) {
	hub := NewTaskEventHub()
	srv := httptest.NewServer(newTestEventsRouter(hub))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/tasks/task-1/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// Broadcast 3 events after a short delay to ensure subscription is active.
	go func() {
		time.Sleep(20 * time.Millisecond)
		hub.Broadcast("task-1", TaskEvent{Kind: "action", Payload: map[string]any{"new_status": "executing"}})
		hub.Broadcast("task-1", TaskEvent{Kind: "job", Payload: map[string]any{"job_id": "j-1"}})
		hub.Broadcast("task-1", TaskEvent{Kind: "fired_event", Payload: map[string]any{"event_name": "hook_fired"}})
	}()

	scanner := bufio.NewScanner(resp.Body)
	var eventKinds []string
	for scanner.Scan() {
		line := scanner.Text()
		if kind, ok := strings.CutPrefix(line, "event: "); ok {
			eventKinds = append(eventKinds, kind)
		}
		if len(eventKinds) >= 2 {
			break
		}
	}
	cancel() // close the connection

	if len(eventKinds) < 2 {
		t.Fatalf("received %d events, want at least 2: %v", len(eventKinds), eventKinds)
	}
	if eventKinds[0] != "action" {
		t.Errorf("events[0] = %q, want %q", eventKinds[0], "action")
	}
	if eventKinds[1] != "job" {
		t.Errorf("events[1] = %q, want %q", eventKinds[1], "job")
	}
}

// TestTaskEvents_ContextCancelUnsubscribes verifies that cancelling the request
// context causes the handler to exit and removes the subscriber from the hub.
func TestTaskEvents_ContextCancelUnsubscribes(t *testing.T) {
	hub := NewTaskEventHub()
	srv := httptest.NewServer(newTestEventsRouter(hub))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/tasks/task-2/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}

	// Broadcast one event to confirm subscription is active, then read it.
	go func() {
		time.Sleep(20 * time.Millisecond)
		hub.Broadcast("task-2", TaskEvent{Kind: "action", Payload: nil})
	}()

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "event:") {
			break
		}
	}

	// Cancel request context → server handler exits via r.Context().Done().
	cancel()
	resp.Body.Close()

	// Wait for server-side subscriber cleanup.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		hub.mu.Lock()
		_, exists := hub.subs["task-2"]
		hub.mu.Unlock()
		if !exists {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("subscriber not removed from hub after context cancel")
}

// TestTaskEvents_RevokeClosesSSE verifies that revoking the device causes the
// SSE handler to return even while the hub subscription is still active.
func TestTaskEvents_RevokeClosesSSE(t *testing.T) {
	hub := NewTaskEventHub()
	reg := auth.NewConnectionRegistry()
	h := &WebHandler{Hub: hub, Registry: reg}

	// Middleware is bypassed; inject deviceID directly into the request context.
	r := chi.NewRouter()
	r.Get("/api/tasks/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		ctx := auth.WithDeviceID(r.Context(), "device-revoke-test")
		h.TaskEvents(w, r.WithContext(ctx))
	})
	srv := httptest.NewServer(r)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/tasks/task-revoke/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	// Confirm the connection is open by reading the first ping or event.
	// Then revoke the device and verify the body closes.
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 1)
		resp.Body.Read(buf) //nolint:errcheck — just wait for any byte or EOF
	}()

	// Give the handler time to register with the registry.
	time.Sleep(50 * time.Millisecond)
	reg.RevokeDevice("device-revoke-test")

	select {
	case <-done:
		// handler exited after revoke
	case <-time.After(3 * time.Second):
		t.Fatal("SSE handler did not return after RevokeDevice")
	}
}

// TestTaskEvents_NoHub returns 503 when Hub is nil.
func TestTaskEvents_NoHub(t *testing.T) {
	h := &WebHandler{}
	r := chi.NewRouter()
	r.Get("/api/tasks/{id}/events", h.TaskEvents)
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/tasks/task-1/events")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}
