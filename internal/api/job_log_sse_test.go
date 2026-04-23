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
)

type fakeRuntimeSubscriber struct {
	snapshot     []byte
	ch           chan []byte
	cancelCalled chan struct{}
	ok           bool
}

func (f *fakeRuntimeSubscriber) Subscribe(_ string) ([]byte, <-chan []byte, func(), bool) {
	cancel := func() {
		select {
		case f.cancelCalled <- struct{}{}:
		default:
		}
	}
	return f.snapshot, f.ch, cancel, f.ok
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
