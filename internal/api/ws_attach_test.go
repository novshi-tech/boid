//go:build linux

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/dispatcher"
)

// stubSubscriber is a fake RuntimeSubscriber for testing.
type stubSubscriber struct {
	snapshot []byte
	ch       chan []byte
	cancel   func()
	ok       bool
}

func (s *stubSubscriber) Subscribe(_ string) ([]byte, <-chan []byte, func(), bool) {
	cancelFn := s.cancel
	if cancelFn == nil {
		cancelFn = func() {}
	}
	return s.snapshot, s.ch, cancelFn, s.ok
}

// stubWriter is a fake RuntimeInputWriter for testing.
type stubWriter struct {
	mu          sync.Mutex
	inputCalls  []inputCall
	resizeCalls []resizeCall
}

type inputCall struct {
	jobID string
	data  []byte
}

type resizeCall struct {
	jobID string
	size  dispatcher.TerminalSize
}

func (s *stubWriter) WriteInput(jobID string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inputCalls = append(s.inputCalls, inputCall{jobID: jobID, data: append([]byte(nil), data...)})
	return nil
}

func (s *stubWriter) ResizeRuntime(jobID string, size dispatcher.TerminalSize) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resizeCalls = append(s.resizeCalls, resizeCall{jobID: jobID, size: size})
	return nil
}

func newWSTestServer(h *WSAttachHandler) *httptest.Server {
	r := chi.NewRouter()
	r.Get("/api/jobs/{id}/attach/ws", h.ServeHTTP)
	return httptest.NewServer(r)
}

func dialWS(t *testing.T, srv *httptest.Server, jobID string) *websocket.Conn {
	t.Helper()
	ctx := context.Background()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/jobs/" + jobID + "/attach/ws"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{srv.URL}},
	})
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	return conn
}

func readWSMsg(t *testing.T, conn *websocket.Conn) wsServerMsg {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	var msg wsServerMsg
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("ws msg unmarshal: %v", err)
	}
	return msg
}

func writeWSMsg(t *testing.T, conn *websocket.Conn, msg wsClientMsg) {
	t.Helper()
	b, _ := json.Marshal(msg)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("ws write: %v", err)
	}
}

func TestWSAttachHandler_SnapshotDelivered(t *testing.T) {
	ch := make(chan []byte, 1)
	sub := &stubSubscriber{
		snapshot: []byte("hello snapshot"),
		ch:       ch,
		ok:       true,
	}
	h := &WSAttachHandler{Subscriber: sub}
	srv := newWSTestServer(h)
	defer srv.Close()

	conn := dialWS(t, srv, "job-1")
	defer conn.CloseNow()

	msg := readWSMsg(t, conn)
	if msg.Type != "output" {
		t.Fatalf("expected output, got %q", msg.Type)
	}
	data, err := base64.StdEncoding.DecodeString(msg.Data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(data) != "hello snapshot" {
		t.Errorf("snapshot = %q, want %q", string(data), "hello snapshot")
	}
}

func TestWSAttachHandler_LiveChunkDelivered(t *testing.T) {
	ch := make(chan []byte, 2)
	sub := &stubSubscriber{
		ch: ch,
		ok: true,
	}
	h := &WSAttachHandler{Subscriber: sub}
	srv := newWSTestServer(h)
	defer srv.Close()

	conn := dialWS(t, srv, "job-1")
	defer conn.CloseNow()

	ch <- []byte("live chunk")
	msg := readWSMsg(t, conn)
	if msg.Type != "output" {
		t.Fatalf("expected output, got %q", msg.Type)
	}
	data, _ := base64.StdEncoding.DecodeString(msg.Data)
	if string(data) != "live chunk" {
		t.Errorf("chunk = %q, want %q", string(data), "live chunk")
	}
}

func TestWSAttachHandler_ChannelCloseTriggersExit(t *testing.T) {
	ch := make(chan []byte)
	sub := &stubSubscriber{
		ch: ch,
		ok: true,
	}
	h := &WSAttachHandler{Subscriber: sub}
	srv := newWSTestServer(h)
	defer srv.Close()

	conn := dialWS(t, srv, "job-1")
	defer conn.CloseNow()

	close(ch)

	msg := readWSMsg(t, conn)
	if msg.Type != "exit" {
		t.Fatalf("expected exit, got %q", msg.Type)
	}
}

func TestWSAttachHandler_InputFrameForwardedToWriter(t *testing.T) {
	ch := make(chan []byte, 1)
	sub := &stubSubscriber{ch: ch, ok: true}
	writer := &stubWriter{}
	h := &WSAttachHandler{Subscriber: sub, Writer: writer}
	srv := newWSTestServer(h)
	defer srv.Close()

	conn := dialWS(t, srv, "job-42")
	defer conn.CloseNow()

	payload := base64.StdEncoding.EncodeToString([]byte("ls\n"))
	writeWSMsg(t, conn, wsClientMsg{Type: "input", Data: payload})

	// Wait for the write to arrive in the stub.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		writer.mu.Lock()
		n := len(writer.inputCalls)
		writer.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	writer.mu.Lock()
	defer writer.mu.Unlock()
	if len(writer.inputCalls) == 0 {
		t.Fatal("WriteInput not called")
	}
	if string(writer.inputCalls[0].data) != "ls\n" {
		t.Errorf("WriteInput data = %q, want %q", writer.inputCalls[0].data, "ls\n")
	}
	if writer.inputCalls[0].jobID != "job-42" {
		t.Errorf("WriteInput jobID = %q, want %q", writer.inputCalls[0].jobID, "job-42")
	}
}

func TestWSAttachHandler_ResizeFrameForwardedToWriter(t *testing.T) {
	ch := make(chan []byte, 1)
	sub := &stubSubscriber{ch: ch, ok: true}
	writer := &stubWriter{}
	h := &WSAttachHandler{Subscriber: sub, Writer: writer}
	srv := newWSTestServer(h)
	defer srv.Close()

	conn := dialWS(t, srv, "job-resize")
	defer conn.CloseNow()

	writeWSMsg(t, conn, wsClientMsg{Type: "resize", Cols: 120, Rows: 40})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		writer.mu.Lock()
		n := len(writer.resizeCalls)
		writer.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	writer.mu.Lock()
	defer writer.mu.Unlock()
	if len(writer.resizeCalls) == 0 {
		t.Fatal("ResizeRuntime not called")
	}
	if writer.resizeCalls[0].size.Cols != 120 || writer.resizeCalls[0].size.Rows != 40 {
		t.Errorf("resize = %+v, want {Cols:120,Rows:40}", writer.resizeCalls[0].size)
	}
}

func TestWSAttachHandler_AlreadyFinished_ExitsImmediately(t *testing.T) {
	sub := &stubSubscriber{
		snapshot: []byte("done output"),
		ok:       false,
	}
	h := &WSAttachHandler{Subscriber: sub}
	srv := newWSTestServer(h)
	defer srv.Close()

	conn := dialWS(t, srv, "job-done")
	defer conn.CloseNow()

	// Should receive snapshot output then exit.
	msg := readWSMsg(t, conn)
	if msg.Type != "output" {
		t.Fatalf("expected output, got %q", msg.Type)
	}
	msg = readWSMsg(t, conn)
	if msg.Type != "exit" {
		t.Fatalf("expected exit, got %q", msg.Type)
	}
}

func TestWSAttachHandler_OriginRejected(t *testing.T) {
	sub := &stubSubscriber{ok: false}
	h := &WSAttachHandler{Subscriber: sub, PublicURL: "https://example.com"}
	srv := newWSTestServer(h)
	defer srv.Close()

	ctx := context.Background()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/jobs/job-1/attach/ws"
	// Use a disallowed origin.
	_, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"https://evil.com"}},
	})
	if err == nil {
		t.Fatal("expected dial to fail for disallowed origin")
	}
	if resp != nil && resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatal("server should have rejected the origin")
	}
}
