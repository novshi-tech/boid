package api

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/api/auth"
)

type fakeRuntimeSubscriber struct {
	snapshot     []byte
	ch           chan []byte
	cancelCalled chan struct{}
	ok           bool
	// finished mirrors RuntimeSubscriber.Subscribe's own finished return
	// (Opus review of PR #864, B2). None of this file's ok:false tests
	// reach the code path that reads it (TestJobLogSSEHandler_NonFollow
	// returns 404 before ever calling Subscribe), so the zero value
	// (false) is never relied on for those; see
	// TestJobLogSSEHandler_StillRunningButUnavailable_SendsNamedSSEEvent
	// and TestJobLogSSEHandler_Finished_EndsStreamSilently for the two
	// cases that DO exercise it explicitly.
	finished bool
}

func (f *fakeRuntimeSubscriber) Subscribe(_ string) ([]byte, <-chan []byte, func(), bool, bool) {
	cancel := func() {
		select {
		case f.cancelCalled <- struct{}{}:
		default:
		}
	}
	return f.snapshot, f.ch, cancel, f.ok, f.finished
}

func newSSETestServer(sub *fakeRuntimeSubscriber) *httptest.Server {
	h := &JobLogSSEHandler{Subscriber: sub}
	r := chi.NewRouter()
	r.Get("/{id}/log", h.ServeHTTP)
	return httptest.NewServer(r)
}

func TestJobLogSSEHandler_ThreeChunks(t *testing.T) {
	ch := make(chan []byte, 10)
	sub := &fakeRuntimeSubscriber{
		snapshot:     []byte("snapshot line\n"),
		ch:           ch,
		cancelCalled: make(chan struct{}, 1),
		ok:           true,
	}
	srv := newSSETestServer(sub)
	defer srv.Close()

	go func() {
		ch <- []byte("chunk1\n")
		ch <- []byte("chunk2\n")
		ch <- []byte("chunk3\n")
		close(ch)
	}()

	resp, err := http.Get(srv.URL + "/job-1/log?follow=true")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	var events []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			events = append(events, strings.TrimPrefix(line, "data: "))
		}
	}

	want := []string{"snapshot line", "chunk1", "chunk2", "chunk3"}
	if len(events) != len(want) {
		t.Fatalf("got %d events %v, want %d %v", len(events), events, len(want), want)
	}
	for i, e := range events {
		if e != want[i] {
			t.Errorf("event[%d] = %q, want %q", i, e, want[i])
		}
	}
}

func TestJobLogSSEHandler_CancelOnDisconnect(t *testing.T) {
	ch := make(chan []byte)
	sub := &fakeRuntimeSubscriber{
		ch:           ch,
		cancelCalled: make(chan struct{}, 1),
		ok:           true,
	}
	srv := newSSETestServer(sub)
	defer srv.Close()

	ctx, ctxCancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/job-1/log?follow=true", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	ctxCancel()

	select {
	case <-sub.cancelCalled:
		// subscriber cancel が呼ばれた
	case <-time.After(2 * time.Second):
		t.Error("subscriber cancel was not called after client disconnect")
	}
}

func TestJobLogSSEHandler_SnapshotFirst(t *testing.T) {
	ch := make(chan []byte, 1)
	sub := &fakeRuntimeSubscriber{
		snapshot:     []byte("initial line\n"),
		ch:           ch,
		cancelCalled: make(chan struct{}, 1),
		ok:           true,
	}
	srv := newSSETestServer(sub)
	defer srv.Close()

	close(ch)

	resp, err := http.Get(srv.URL + "/job-1/log?follow=true")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	var first string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			first = strings.TrimPrefix(line, "data: ")
			break
		}
	}
	if first != "initial line" {
		t.Errorf("first SSE event = %q, want %q", first, "initial line")
	}
}

func TestJobLogSSEHandler_RevokeClosesSSE(t *testing.T) {
	ch := make(chan []byte) // never sends
	sub := &fakeRuntimeSubscriber{
		ch:           ch,
		cancelCalled: make(chan struct{}, 1),
		ok:           true,
	}
	reg := auth.NewConnectionRegistry()

	r := chi.NewRouter()
	r.Get("/{id}/log", func(w http.ResponseWriter, req *http.Request) {
		ctx := auth.WithDeviceID(req.Context(), "sse-revoke-device")
		h := &JobLogSSEHandler{Subscriber: sub, Registry: reg}
		h.ServeHTTP(w, req.WithContext(ctx))
	})
	srv := httptest.NewServer(r)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/job-revoke/log?follow=true", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Give the handler time to register.
	time.Sleep(50 * time.Millisecond)
	reg.RevokeDevice("sse-revoke-device")

	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 1)
		resp.Body.Read(buf) //nolint:errcheck
	}()

	select {
	case <-done:
		// handler exited after revoke
	case <-time.After(3 * time.Second):
		t.Fatal("JobLogSSE handler did not return after RevokeDevice")
	}
}

// TestJobLogSSEHandler_Finished_EndsStreamSilently pins the pre-existing,
// unchanged behavior for a genuinely finished job (ok=false, finished=true)
// — a regression guard alongside
// TestJobLogSSEHandler_StillRunningButUnavailable_SendsNamedSSEEvent below,
// which pins the NEW behavior for the opposite (finished=false) case (Opus
// review of PR #864, B2). A finished job's SSE stream must still just end
// after the snapshot, with no named event of any kind.
func TestJobLogSSEHandler_Finished_EndsStreamSilently(t *testing.T) {
	sub := &fakeRuntimeSubscriber{
		snapshot:     []byte("final line\n"),
		cancelCalled: make(chan struct{}, 1),
		ok:           false,
		finished:     true,
	}
	srv := newSSETestServer(sub)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/job-1/log?follow=true")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, "data: final line") {
		t.Errorf("body = %q, want it to contain the snapshot line", got)
	}
	if strings.Contains(got, "event:") {
		t.Errorf("body = %q, a genuinely finished job must not emit any named SSE event", got)
	}
}

// TestJobLogSSEHandler_StillRunningButUnavailable_SendsNamedSSEEvent pins
// B2 from the Opus review of PR #864: when Subscribe reports ok=false but
// finished=false (the job is genuinely still running, it just has no live
// stream to offer right now), the handler must emit a distinctly-named SSE
// event rather than silently ending the stream — the same symptom as
// ws_attach.go's exit-vs-error distinction, adapted to SSE's own protocol
// (there is no single "exit" frame type here; a bare return, this branch's
// entire behavior before B2, is indistinguishable from a genuinely
// finished job to any client watching the raw byte stream). The event
// name is deliberately NOT "message" (the unnamed default sendLines' own
// `data:` lines use) so an existing EventSource onmessage listener (e.g.
// web/templates/jobs.templ's job-log follow script) never mistakes it for
// ordinary log output.
func TestJobLogSSEHandler_StillRunningButUnavailable_SendsNamedSSEEvent(t *testing.T) {
	sub := &fakeRuntimeSubscriber{
		snapshot:     []byte("partial line\n"),
		cancelCalled: make(chan struct{}, 1),
		ok:           false,
		finished:     false,
	}
	srv := newSSETestServer(sub)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/job-1/log?follow=true")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, "data: partial line") {
		t.Errorf("body = %q, want it to contain the snapshot line", got)
	}
	if !strings.Contains(got, "event: boid-stream-unavailable") {
		t.Errorf("body = %q, want a named boid-stream-unavailable SSE event when the job is still running but has no live stream", got)
	}
}

func TestJobLogSSEHandler_NonFollow(t *testing.T) {
	sub := &fakeRuntimeSubscriber{
		cancelCalled: make(chan struct{}, 1),
	}
	srv := newSSETestServer(sub)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/job-1/log")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
