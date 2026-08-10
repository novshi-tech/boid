//go:build linux

package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/api/auth"
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/dispatcher"
)

// stubSubscriber is a fake RuntimeSubscriber for testing. finished mirrors
// RuntimeSubscriber.Subscribe's own finished return (Opus review of PR
// #864, B2) — every pre-existing test literal below that sets ok:false
// also now sets finished explicitly (true for every one of them: none of
// this file's pre-B2 tests exercise the "still running, no stream" case),
// so this file's zero-value default of finished:false is never silently
// relied on by an existing test.
type stubSubscriber struct {
	snapshot []byte
	ch       chan []byte
	cancel   func()
	ok       bool
	finished bool
}

func (s *stubSubscriber) Subscribe(_ string) ([]byte, <-chan []byte, func(), bool, bool) {
	cancelFn := s.cancel
	if cancelFn == nil {
		cancelFn = func() {}
	}
	return s.snapshot, s.ch, cancelFn, s.ok, s.finished
}

// stubWriter is a fake RuntimeInputWriter for testing.
type stubWriter struct {
	mu          sync.Mutex
	inputCalls  []inputCall
	resizeCalls []resizeCall
	closeCalls  []string
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

func (s *stubWriter) CloseInput(jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeCalls = append(s.closeCalls, jobID)
	return nil
}

func newWSTestServer(h *WSAttachHandler) *httptest.Server {
	r := chi.NewRouter()
	r.Get("/api/jobs/{id}/attach/ws", h.ServeHTTP)
	return httptest.NewServer(r)
}

// dialWS opens an attach connection and consumes the leading "attach"
// frame every connection now begins with (WSAttachHandler.sendAttach), so
// the tests below keep reading the frame they actually care about first.
// Tests that assert on the attach frame itself use dialWSRaw.
func dialWS(t *testing.T, srv *httptest.Server, jobID string) *websocket.Conn {
	t.Helper()
	conn := dialWSRaw(t, srv, jobID, "")
	if msg := readWSMsg(t, conn); msg.Type != "attach" {
		t.Fatalf("first frame type = %q, want %q", msg.Type, "attach")
	}
	return conn
}

// dialWSRaw dials without consuming anything. query, when non-empty, is
// appended to the URL as-is (e.g. "replay_offset=5").
func dialWSRaw(t *testing.T, srv *httptest.Server, jobID, query string) *websocket.Conn {
	t.Helper()
	ctx := context.Background()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/jobs/" + jobID + "/attach/ws"
	if query != "" {
		wsURL += "?" + query
	}
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

func TestWSAttachHandler_InputCloseFrameForwardedToWriter(t *testing.T) {
	ch := make(chan []byte, 1)
	sub := &stubSubscriber{ch: ch, ok: true}
	writer := &stubWriter{}
	h := &WSAttachHandler{Subscriber: sub, Writer: writer}
	srv := newWSTestServer(h)
	defer srv.Close()

	conn := dialWS(t, srv, "job-input-close")
	defer conn.CloseNow()

	writeWSMsg(t, conn, wsClientMsg{Type: "input_close"})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		writer.mu.Lock()
		n := len(writer.closeCalls)
		writer.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	writer.mu.Lock()
	defer writer.mu.Unlock()
	if len(writer.closeCalls) == 0 {
		t.Fatal("CloseInput not called")
	}
	if writer.closeCalls[0] != "job-input-close" {
		t.Errorf("CloseInput jobID = %q, want %q", writer.closeCalls[0], "job-input-close")
	}
}

func TestWSAttachHandler_AlreadyFinished_ExitsImmediately(t *testing.T) {
	sub := &stubSubscriber{
		snapshot: []byte("done output"),
		ok:       false,
		finished: true,
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

// TestWSAttachHandler_StillRunningButUnavailable_SendsErrorNotExit pins B2
// from the Opus review of PR #864: when Subscribe reports ok=false but
// finished=false — the job is genuinely still running, it just has no live
// stream to offer right now (e.g. the container backend's adopt-time
// attach failed and hasn't been re-attached yet) — the handler must NOT
// send an "exit" frame. Sending "exit" here would be a false positive: the
// caller (internal/client's AttachJob, `boid job attach`) would report the
// still-running job as having finished successfully (exit code 0), which
// is a worse diagnostic than the dead-channel hang this whole fix started
// from — that hang at least signaled "something is wrong". This pins the
// opposite of TestWSAttachHandler_AlreadyFinished_ExitsImmediately just
// above, which pins the genuinely-finished (finished=true) case still
// getting "exit" unchanged.
func TestWSAttachHandler_StillRunningButUnavailable_SendsErrorNotExit(t *testing.T) {
	sub := &stubSubscriber{
		snapshot: []byte("partial output"),
		ok:       false,
		finished: false,
	}
	h := &WSAttachHandler{Subscriber: sub}
	srv := newWSTestServer(h)
	defer srv.Close()

	conn := dialWS(t, srv, "job-still-running")
	defer conn.CloseNow()

	msg := readWSMsg(t, conn)
	if msg.Type != "output" {
		t.Fatalf("expected output, got %q", msg.Type)
	}
	msg = readWSMsg(t, conn)
	if msg.Type != "error" {
		t.Fatalf("expected error (job still running, stream unavailable), got %q (%+v)", msg.Type, msg)
	}
	if msg.Message == "" {
		t.Error("error frame carried no message")
	}
}

func TestWSAttachHandler_RevokeClosesWS(t *testing.T) {
	ch := make(chan []byte)
	sub := &stubSubscriber{ch: ch, ok: true}
	reg := auth.NewConnectionRegistry()

	// Inject deviceID into request context via a middleware wrapper.
	r := chi.NewRouter()
	r.Get("/api/jobs/{id}/attach/ws", func(w http.ResponseWriter, req *http.Request) {
		ctx := auth.WithDeviceID(req.Context(), "ws-revoke-device")
		h := &WSAttachHandler{Subscriber: sub, Registry: reg}
		h.ServeHTTP(w, req.WithContext(ctx))
	})
	srv := httptest.NewServer(r)
	defer srv.Close()

	conn := dialWS(t, srv, "job-revoke")
	defer conn.CloseNow()

	// Give the handler time to register with the registry.
	time.Sleep(50 * time.Millisecond)
	reg.RevokeDevice("ws-revoke-device")

	// The connection should be closed by the server; Read should return an error.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if err == nil {
		t.Fatal("expected connection to be closed after RevokeDevice, but Read succeeded")
	}
}

// newTestAuthStoreForWS mirrors newTestAuthStore (web_management_test.go,
// same package) — a fresh migrated in-memory auth store — kept local to
// this file so the ws_attach tests below don't take on a cross-file
// dependency for a one-line helper.
func newTestAuthStoreForWS(t *testing.T) *auth.Store {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return auth.NewStore(d.Conn)
}

func TestWSAttachHandler_BearerAuth_RegistersWithConnectionRegistry(t *testing.T) {
	store := newTestAuthStoreForWS(t)
	token, err := auth.GenerateDeviceToken()
	if err != nil {
		t.Fatalf("GenerateDeviceToken: %v", err)
	}
	if err := store.InsertDeviceToken(context.Background(), "dev-ws-bearer", "cli", auth.HashToken(token)); err != nil {
		t.Fatalf("InsertDeviceToken: %v", err)
	}

	ch := make(chan []byte)
	sub := &stubSubscriber{ch: ch, ok: true}
	reg := auth.NewConnectionRegistry()
	h := &WSAttachHandler{Subscriber: sub, Registry: reg, Bearer: auth.NewBearerVerifier(store)}
	srv := newWSTestServer(h)
	defer srv.Close()

	ctx := context.Background()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/jobs/job-bearer/attach/ws"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Origin":        []string{srv.URL},
			"Authorization": []string{"Bearer " + token},
		},
	})
	if err != nil {
		t.Fatalf("ws dial with bearer token: %v", err)
	}
	defer conn.CloseNow()

	if msg := readWSMsg(t, conn); msg.Type != "attach" {
		t.Fatalf("first frame type = %q, want %q", msg.Type, "attach")
	}

	// Give the handler time to register with the registry, then revoke and
	// confirm the connection is torn down — this is only possible if the
	// handshake's Bearer token was actually resolved to "dev-ws-bearer" and
	// registered.
	time.Sleep(50 * time.Millisecond)
	reg.RevokeDevice("dev-ws-bearer")

	readCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := conn.Read(readCtx); err == nil {
		t.Fatal("expected connection to be closed after RevokeDevice, but Read succeeded")
	}
}

func TestWSAttachHandler_InvalidBearer_HandshakeRejected(t *testing.T) {
	store := newTestAuthStoreForWS(t)
	sub := &stubSubscriber{ok: false}
	h := &WSAttachHandler{Subscriber: sub, Bearer: auth.NewBearerVerifier(store)}
	srv := newWSTestServer(h)
	defer srv.Close()

	ctx := context.Background()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/jobs/job-1/attach/ws"
	_, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Origin":        []string{srv.URL},
			"Authorization": []string{"Bearer boid_pat_does-not-exist"},
		},
	})
	if err == nil {
		t.Fatal("expected dial to fail for invalid bearer token")
	}
	if resp != nil && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestWSAttachHandler_NoBearerHeader_FallsBackToContextDeviceID(t *testing.T) {
	// Unchanged pre-PR0 behavior: with no Authorization header at all, the
	// handler must still honor whatever deviceID cookie-based middleware
	// upstream placed in the request context (see
	// TestWSAttachHandler_RevokeClosesWS for the pre-existing coverage of
	// this exact path — this test only additionally proves a Bearer field
	// on the handler doesn't change that when the header is absent).
	ch := make(chan []byte)
	sub := &stubSubscriber{ch: ch, ok: true}
	reg := auth.NewConnectionRegistry()
	store := newTestAuthStoreForWS(t)

	r := chi.NewRouter()
	r.Get("/api/jobs/{id}/attach/ws", func(w http.ResponseWriter, req *http.Request) {
		ctx := auth.WithDeviceID(req.Context(), "ws-context-device")
		h := &WSAttachHandler{Subscriber: sub, Registry: reg, Bearer: auth.NewBearerVerifier(store)}
		h.ServeHTTP(w, req.WithContext(ctx))
	})
	srv := httptest.NewServer(r)
	defer srv.Close()

	conn := dialWS(t, srv, "job-revoke-2")
	defer conn.CloseNow()

	time.Sleep(50 * time.Millisecond)
	reg.RevokeDevice("ws-context-device")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("expected connection to be closed after RevokeDevice, but Read succeeded")
	}
}

// TestWSAttachHandler_CLITokenAuthenticated_BypassesDeviceBearerCheck pins
// the round-2 codex review Blocker 1 fix: a request already authenticated
// by auth.NewCLITokenAuthMiddleware (context marked via
// auth.WithCLITokenAuthenticated) must not have its Bearer header
// re-verified as a device-pair token here — BOID_CLI_TOKEN is never a value
// InsertDeviceToken wrote to the auth store, so h.Bearer.Verify would
// always reject it, breaking `boid attach`/`exec` end to end over host
// mode. A Bearer header carrying an arbitrary (non-device) token is
// deliberately present on the request below, mirroring exactly what the
// real CLI-token listener forwards downstream — the handshake must still
// succeed.
func TestWSAttachHandler_CLITokenAuthenticated_BypassesDeviceBearerCheck(t *testing.T) {
	store := newTestAuthStoreForWS(t)
	ch := make(chan []byte)
	sub := &stubSubscriber{ch: ch, ok: true}
	h := &WSAttachHandler{Subscriber: sub, Bearer: auth.NewBearerVerifier(store)}

	r := chi.NewRouter()
	r.Get("/api/jobs/{id}/attach/ws", func(w http.ResponseWriter, req *http.Request) {
		ctx := auth.WithCLITokenAuthenticated(req.Context())
		h.ServeHTTP(w, req.WithContext(ctx))
	})
	srv := httptest.NewServer(r)
	defer srv.Close()

	ctx := context.Background()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/jobs/job-cli-token/attach/ws"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Origin":        []string{srv.URL},
			"Authorization": []string{"Bearer some-cli-token-not-a-device-token"},
		},
	})
	if err != nil {
		t.Fatalf("ws dial with CLI-token-authenticated context should succeed, got: %v", err)
	}
	defer conn.CloseNow()
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

// TestWSAttachHandler_LargeSnapshotSplitIntoFrames pins the daemon side of
// the "websocket: message too big: read limited at 32769 bytes" attach
// failure: a job's replayed transcript is unbounded, and sending it as one
// frame blew past coder/websocket's 32768-byte DEFAULT read limit on the
// other end (browsers have no such limit, but every Go client — `boid
// attach`, `boid exec` — did). sendOutput must split anything oversized
// into frames that stay under that default, so an old CLI can still attach
// to a new daemon, while the concatenated payload is byte-identical.
func TestWSAttachHandler_LargeSnapshotSplitIntoFrames(t *testing.T) {
	snapshot := make([]byte, 200*1024)
	for i := range snapshot {
		snapshot[i] = byte('a' + i%26)
	}
	sub := &stubSubscriber{snapshot: snapshot, ch: make(chan []byte, 1), ok: true}
	h := &WSAttachHandler{Subscriber: sub}
	srv := newWSTestServer(h)
	defer srv.Close()

	conn := dialWS(t, srv, "job-1")
	defer conn.CloseNow()

	var got []byte
	for len(got) < len(snapshot) {
		msg := readWSMsg(t, conn)
		if msg.Type != "output" {
			t.Fatalf("frame type = %q, want output (got %d of %d bytes so far)", msg.Type, len(got), len(snapshot))
		}
		raw, err := base64.StdEncoding.DecodeString(msg.Data)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		// The wire frame is the JSON envelope around the base64 payload;
		// it is what the peer's read limit actually measures.
		if frameLen := len(msg.Data) + 32; frameLen > 32768 {
			t.Errorf("frame size ~%d bytes exceeds the 32768-byte default WS read limit", frameLen)
		}
		got = append(got, raw...)
	}
	if !bytes.Equal(got, snapshot) {
		t.Errorf("reassembled snapshot differs from the original (%d vs %d bytes)", len(got), len(snapshot))
	}
}

// --- reconnect support: replay offset + keepalive ping ---

// TestWSAttachHandler_ReplayOffsetResumesMidTranscript pins the server half
// of CLI/Web auto-reconnect: a client that already rendered the first N
// bytes of a job's transcript asks for the rest, and gets exactly the rest —
// not the whole transcript repainted from the top.
func TestWSAttachHandler_ReplayOffsetResumesMidTranscript(t *testing.T) {
	sub := &stubSubscriber{snapshot: []byte("hello snapshot"), ch: make(chan []byte, 1), ok: true}
	h := &WSAttachHandler{Subscriber: sub}
	srv := newWSTestServer(h)
	defer srv.Close()

	conn := dialWSRaw(t, srv, "job-1", "replay_offset=5")
	defer conn.CloseNow()

	attach := readWSMsg(t, conn)
	if attach.Type != "attach" {
		t.Fatalf("first frame type = %q, want attach", attach.Type)
	}
	if attach.Offset != 5 {
		t.Errorf("attach offset = %d, want 5", attach.Offset)
	}

	out := readWSMsg(t, conn)
	data, err := base64.StdEncoding.DecodeString(out.Data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(data) != " snapshot" {
		t.Errorf("replayed %q, want %q", string(data), " snapshot")
	}
}

// TestWSAttachHandler_ReplayOffsetAtEnd_SendsNoReplay covers the common
// reconnect case: nothing new was printed while the client was away, so the
// next frame it sees must be live output, not a repaint.
func TestWSAttachHandler_ReplayOffsetAtEnd_SendsNoReplay(t *testing.T) {
	ch := make(chan []byte, 1)
	sub := &stubSubscriber{snapshot: []byte("hello snapshot"), ch: ch, ok: true}
	h := &WSAttachHandler{Subscriber: sub}
	srv := newWSTestServer(h)
	defer srv.Close()

	conn := dialWSRaw(t, srv, "job-1", "replay_offset=14")
	defer conn.CloseNow()

	if attach := readWSMsg(t, conn); attach.Type != "attach" || attach.Offset != 14 {
		t.Fatalf("attach frame = %+v, want type=attach offset=14", attach)
	}

	ch <- []byte("live")
	out := readWSMsg(t, conn)
	data, _ := base64.StdEncoding.DecodeString(out.Data)
	if string(data) != "live" {
		t.Errorf("next frame payload = %q, want %q (no replay expected)", string(data), "live")
	}
}

// TestWSAttachHandler_ReplayOffsetBeyondSnapshot_ReplaysAll covers a daemon
// that restarted and rebuilt a shorter transcript under the client's feet.
// Sending nothing would leave the user staring at a blank terminal, so the
// server replays from the top and says so in the attach frame.
func TestWSAttachHandler_ReplayOffsetBeyondSnapshot_ReplaysAll(t *testing.T) {
	sub := &stubSubscriber{snapshot: []byte("short"), ch: make(chan []byte, 1), ok: true}
	h := &WSAttachHandler{Subscriber: sub}
	srv := newWSTestServer(h)
	defer srv.Close()

	conn := dialWSRaw(t, srv, "job-1", "replay_offset=9999")
	defer conn.CloseNow()

	attach := readWSMsg(t, conn)
	if attach.Offset != 0 {
		t.Errorf("attach offset = %d, want 0 (replay from the top)", attach.Offset)
	}
	out := readWSMsg(t, conn)
	data, _ := base64.StdEncoding.DecodeString(out.Data)
	if string(data) != "short" {
		t.Errorf("replayed %q, want %q", string(data), "short")
	}
}

// TestWSAttachHandler_PingsIdleConnection pins the fix for the original
// report: an attach left idle from Windows (over a Cloudflare Tunnel) got
// dropped by the network because boid put no traffic on the wire at all.
//
// The assertion is made against a hand-rolled raw WS client rather than a
// coder/websocket one, because that library answers pings transparently
// deep inside Read — a normal client conn gives the test no way to observe
// that a ping ever arrived.
func TestWSAttachHandler_PingsIdleConnection(t *testing.T) {
	sub := &stubSubscriber{ch: make(chan []byte), ok: true}
	h := &WSAttachHandler{Subscriber: sub, KeepalivePeriod: 20 * time.Millisecond}
	srv := newWSTestServer(h)
	defer srv.Close()

	conn := dialRawWS(t, srv, "job-idle")
	defer conn.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		opcode, _ := readRawWSFrame(t, conn)
		if opcode == rawWSOpcodePing {
			return
		}
	}
	t.Fatal("no keepalive ping arrived on an idle attach connection")
}

const rawWSOpcodePing = 0x9

// dialRawWS performs the WebSocket handshake by hand and returns the naked
// TCP connection, so a test can inspect the control frames coder/websocket
// would otherwise consume on its own.
func dialRawWS(t *testing.T, srv *httptest.Server, jobID string) net.Conn {
	t.Helper()
	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("raw dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	req := "GET /api/jobs/" + jobID + "/attach/ws HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: AAAAAAAAAAAAAAAAAAAAAA==\r\n" +
		"Origin: " + srv.URL + "\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("raw handshake write: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("raw handshake read: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("raw handshake status = %d, want 101", resp.StatusCode)
	}
	if br.Buffered() > 0 {
		t.Fatalf("unexpected %d bytes buffered after handshake", br.Buffered())
	}
	return conn
}

// readRawWSFrame reads one server→client frame. Server frames are never
// masked, so the payload follows the length header directly.
func readRawWSFrame(t *testing.T, conn net.Conn) (opcode byte, payload []byte) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	var head [2]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		t.Fatalf("read frame header: %v", err)
	}
	opcode = head[0] & 0x0f
	size := uint64(head[1] & 0x7f)
	switch size {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(conn, ext[:]); err != nil {
			t.Fatalf("read 16-bit length: %v", err)
		}
		size = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(conn, ext[:]); err != nil {
			t.Fatalf("read 64-bit length: %v", err)
		}
		size = binary.BigEndian.Uint64(ext[:])
	}
	payload = make([]byte, size)
	if _, err := io.ReadFull(conn, payload); err != nil {
		t.Fatalf("read frame payload: %v", err)
	}
	return opcode, payload
}
