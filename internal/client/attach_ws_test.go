package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// --- WS AttachJob (docs/plans/cli-remote-connection.md Phase 3 PR3:
// "WebSocket attach 一本化") ---
//
// These tests exercise AttachJob's wire-level behavior directly against a
// minimal hand-rolled WS server (using the same github.com/coder/websocket
// library internal/api/ws_attach.go's real handler uses), rather than the
// real WSAttachHandler — internal/api is off limits for internal/client
// (TestClientDoesNotDependOnBehavior), so wsAttachClientMsg/wsAttachServerMsg
// (client.go) are the wire contract under test here, mirrored independently
// on the server side of ws_attach.go. Reachability through the *real*
// server (auth wiring, wire.go's route mount point) is covered by
// TestServerJobRuntimeAttachAndResize (internal/server/server_phase3_test.go,
// unix profile) and TestTCPListener_WSAttach_ReachableViaBearerAndCookie
// (internal/server/ws_attach_wire_test.go, https-shaped TCP profile).

// newUnixWSServer starts an HTTP server listening on a fresh UNIX socket in
// t.TempDir(), serving handler, and returns a unix-scheme Client dialed
// against it plus a cleanup. Mirrors NewUnixClient's own DialContext
// wiring, so AttachJob's websocket.Dial(..., &websocket.DialOptions{
// HTTPClient: c.httpClient, ...}) exercises the exact unix-socket path
// production code takes.
func newUnixWSServer(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "boid.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() {
		srv.Close()
		_ = os.Remove(sockPath)
	})
	return NewUnixClient(sockPath)
}

// newWSHandler wraps fn as an http.Handler that accepts the WS upgrade (no
// origin restriction — see this file's package doc comment for why
// AttachJob's Origin header trivially satisfies the real server's check
// anyway) and hands the connection to fn.
func newWSHandler(t *testing.T, fn func(t *testing.T, conn *websocket.Conn, r *http.Request)) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		fn(t, conn, r)
	}
}

func readClientMsg(t *testing.T, conn *websocket.Conn, timeout time.Duration) wsAttachClientMsg {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("server read: %v", err)
	}
	var msg wsAttachClientMsg
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal client msg: %v", err)
	}
	return msg
}

func writeServerMsg(t *testing.T, conn *websocket.Conn, msg wsAttachServerMsg) {
	t.Helper()
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal server msg: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("server write: %v", err)
	}
}

// TestAttachJob_UnixSocket_FullFraming drives one AttachJob call through
// all four frame types (input, input_close, output, exit) over a real UNIX
// socket, matching how cmd/attach.go's attachLive actually calls AttachJob
// for a local profile.
func TestAttachJob_UnixSocket_FullFraming(t *testing.T) {
	var gotOrigin string
	c := newUnixWSServer(t, newWSHandler(t, func(t *testing.T, conn *websocket.Conn, r *http.Request) {
		gotOrigin = r.Header.Get("Origin")

		in := readClientMsg(t, conn, 3*time.Second)
		if in.Type != "input" {
			t.Errorf("first client frame type = %q, want %q", in.Type, "input")
		}
		data, err := base64.StdEncoding.DecodeString(in.Data)
		if err != nil {
			t.Fatalf("decode input data: %v", err)
		}
		if string(data) != "hello server" {
			t.Errorf("input data = %q, want %q", string(data), "hello server")
		}

		closeMsg := readClientMsg(t, conn, 3*time.Second)
		if closeMsg.Type != "input_close" {
			t.Errorf("second client frame type = %q, want %q", closeMsg.Type, "input_close")
		}

		writeServerMsg(t, conn, wsAttachServerMsg{Type: "output", Data: base64.StdEncoding.EncodeToString([]byte("hello client"))})
		writeServerMsg(t, conn, wsAttachServerMsg{Type: "exit", Code: 0})
	}))

	var stdout bytes.Buffer
	stdin := bytes.NewBufferString("hello server")

	if err := c.AttachJob("job-1", stdin, &stdout, AttachOptions{}); err != nil {
		t.Fatalf("AttachJob: %v", err)
	}
	if stdout.String() != "hello client" {
		t.Errorf("stdout = %q, want %q", stdout.String(), "hello client")
	}
	if gotOrigin == "" {
		t.Error("expected a non-empty Origin header on the WS handshake")
	}
}

// TestAttachJob_HTTPS_SendsBearerHeaderOnHandshake proves the https-scheme
// path carries the same Authorization: Bearer header on the WS handshake
// that bearerTransport injects on every plain HTTP request — this is the
// whole point of reusing c.httpClient as websocket.Dial's HTTPClient
// (AttachJob's own doc comment) rather than building a fresh one.
func TestAttachJob_HTTPS_SendsBearerHeaderOnHandshake(t *testing.T) {
	var gotAuth string
	handler := newWSHandler(t, func(t *testing.T, conn *websocket.Conn, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		writeServerMsg(t, conn, wsAttachServerMsg{Type: "exit"})
	})

	c, _ := newTestTLSHTTPSClient(t, handler, "tk_attach_bearer")

	if err := c.AttachJob("job-1", nil, nil, AttachOptions{}); err != nil {
		t.Fatalf("AttachJob: %v", err)
	}
	if gotAuth != "Bearer tk_attach_bearer" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer tk_attach_bearer")
	}
}

// TestAttachJob_ServerErrorFrame_ReturnsError proves a server-sent "error"
// frame (WSAttachHandler.sendError — e.g. "subscriber not configured")
// surfaces as AttachJob's returned error instead of being silently dropped.
func TestAttachJob_ServerErrorFrame_ReturnsError(t *testing.T) {
	c := newUnixWSServer(t, newWSHandler(t, func(t *testing.T, conn *websocket.Conn, r *http.Request) {
		writeServerMsg(t, conn, wsAttachServerMsg{Type: "error", Message: "boom: subscriber not configured"})
	}))

	err := c.AttachJob("job-1", nil, nil, AttachOptions{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := err.Error(); got != "boom: subscriber not configured" {
		t.Errorf("error = %q, want %q", got, "boom: subscriber not configured")
	}
}

// TestAttachJob_DetachKey_ReturnsNilPromptly proves that when stdin's Read
// reports ErrAttachDetached (cmd/attach.go's detachReader on Ctrl-]),
// AttachJob returns nil immediately rather than waiting for the server to
// close its side — matching the pre-PR3 raw-transport semantics (detach
// abandons the connection outright).
func TestAttachJob_DetachKey_ReturnsNilPromptly(t *testing.T) {
	unblock := make(chan struct{})
	c := newUnixWSServer(t, newWSHandler(t, func(t *testing.T, conn *websocket.Conn, r *http.Request) {
		// Block on a read that never comes from a well-behaved client;
		// AttachJob detaching should close the connection out from under
		// this, unblocking it with an error so the handler goroutine (and
		// therefore the test process) doesn't leak past the subtest.
		_, _, _ = conn.Read(context.Background())
		close(unblock)
	}))

	stdin := detachingReader{}
	done := make(chan error, 1)
	go func() { done <- c.AttachJob("job-1", stdin, nil, AttachOptions{}) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("AttachJob: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for AttachJob to return after detach")
	}

	select {
	case <-unblock:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for server-side read to unblock after client detach")
	}
}

type detachingReader struct{}

func (detachingReader) Read([]byte) (int, error) { return 0, ErrAttachDetached }

// TestAttachJob_HandshakeRejected_SurfacesServerErrorMessage proves a
// pre-upgrade failure (e.g. WSAttachHandler's 401 "unauthorized" JSON body)
// is surfaced through AttachJob's returned error rather than a generic
// "dial attach websocket" message with no detail.
func TestAttachJob_HandshakeRejected_SurfacesServerErrorMessage(t *testing.T) {
	c := newUnixWSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
	}))

	err := c.AttachJob("job-1", nil, nil, AttachOptions{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := err.Error(); !strings.Contains(got, "unauthorized") {
		t.Errorf("error = %q, want it to contain %q", got, "unauthorized")
	}
}

// TestAttachJob_LargeOutputFrame_ExceedsDefaultReadLimit pins the fix for
// the Windows `boid attach` failure "websocket: message too big: read
// limited at 32769 bytes": WSAttachHandler replays a job's whole
// accumulated transcript as its first output frame, and that transcript is
// unbounded — any job that has printed more than ~24 KB (32768 / 4 * 3,
// the base64 inflation of the frame's payload) produced a frame larger than
// coder/websocket's 32768-byte default read limit, so the attach died at
// the very first Read instead of showing output. AttachJob must accept
// server frames of arbitrary size; the daemon-side chunking added
// alongside this only bounds NEW daemons' frames, and a new CLI still has
// to attach to an already-running old daemon.
func TestAttachJob_LargeOutputFrame_ExceedsDefaultReadLimit(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 256*1024)
	c := newUnixWSServer(t, newWSHandler(t, func(t *testing.T, conn *websocket.Conn, r *http.Request) {
		writeServerMsg(t, conn, wsAttachServerMsg{Type: "output", Data: base64.StdEncoding.EncodeToString(payload)})
		writeServerMsg(t, conn, wsAttachServerMsg{Type: "exit", Code: 0})
	}))

	var stdout bytes.Buffer
	if err := c.AttachJob("job-1", nil, &stdout, AttachOptions{}); err != nil {
		t.Fatalf("AttachJob: %v", err)
	}
	if got := stdout.Len(); got != len(payload) {
		t.Errorf("stdout length = %d, want %d", got, len(payload))
	}
}

// --- reconnect (2026-08-10) ---

// shortenAttachReconnectWaits makes the backoff schedule test-speed and
// restores it afterwards.
func shortenAttachReconnectWaits(t *testing.T) {
	t.Helper()
	origInitial, origMax := attachReconnectInitialWait, attachReconnectMaxWait
	attachReconnectInitialWait = time.Millisecond
	attachReconnectMaxWait = 2 * time.Millisecond
	t.Cleanup(func() {
		attachReconnectInitialWait, attachReconnectMaxWait = origInitial, origMax
	})
}

// TestAttachJob_ReconnectsAfterAbnormalDrop is the CLI half of the original
// report: an idle attach from Windows over a Cloudflare Tunnel got reaped by
// the network, which the client used to report as a finished job. It must
// instead redial, tell the server how much transcript it already has, and
// splice the rest onto the same stdout.
func TestAttachJob_ReconnectsAfterAbnormalDrop(t *testing.T) {
	shortenAttachReconnectWaits(t)

	var mu sync.Mutex
	var seenOffsets []string
	attempt := 0

	c := newUnixWSServer(t, newWSHandler(t, func(t *testing.T, conn *websocket.Conn, r *http.Request) {
		mu.Lock()
		n := attempt
		attempt++
		seenOffsets = append(seenOffsets, r.URL.Query().Get("replay_offset"))
		mu.Unlock()

		offset, _ := strconv.ParseInt(r.URL.Query().Get("replay_offset"), 10, 64)
		writeServerMsg(t, conn, wsAttachServerMsg{Type: "attach", Offset: offset})
		if n == 0 {
			writeServerMsg(t, conn, wsAttachServerMsg{Type: "output", Data: base64.StdEncoding.EncodeToString([]byte("hello"))})
			// Vanish without a close frame — exactly what an intermediary
			// reaping an idle connection looks like from here.
			conn.CloseNow()
			return
		}
		writeServerMsg(t, conn, wsAttachServerMsg{Type: "output", Data: base64.StdEncoding.EncodeToString([]byte(" world"))})
		writeServerMsg(t, conn, wsAttachServerMsg{Type: "exit", Code: 0})
	}))

	var stdout, notices bytes.Buffer
	if err := c.AttachJob("job-reconnect", nil, &stdout, AttachOptions{Notify: &notices}); err != nil {
		t.Fatalf("AttachJobWithOptions: %v", err)
	}

	if stdout.String() != "hello world" {
		t.Errorf("stdout = %q, want %q", stdout.String(), "hello world")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seenOffsets) != 2 {
		t.Fatalf("server saw %d connections, want 2 (%v)", len(seenOffsets), seenOffsets)
	}
	if seenOffsets[0] != "" {
		t.Errorf("first connection sent replay_offset=%q, want it absent", seenOffsets[0])
	}
	if seenOffsets[1] != "5" {
		t.Errorf("reconnect sent replay_offset=%q, want %q", seenOffsets[1], "5")
	}
	if !strings.Contains(notices.String(), "reconnecting") {
		t.Errorf("notices = %q, want a reconnect notice", notices.String())
	}
}

// TestAttachJob_ExitFrameDoesNotReconnect guards the other side of the
// split: a job that genuinely ended must not be redialed.
func TestAttachJob_ExitFrameDoesNotReconnect(t *testing.T) {
	shortenAttachReconnectWaits(t)

	var mu sync.Mutex
	connections := 0
	c := newUnixWSServer(t, newWSHandler(t, func(t *testing.T, conn *websocket.Conn, _ *http.Request) {
		mu.Lock()
		connections++
		mu.Unlock()
		writeServerMsg(t, conn, wsAttachServerMsg{Type: "exit", Code: 0})
	}))

	var stdout bytes.Buffer
	if err := c.AttachJob("job-exit", nil, &stdout, AttachOptions{}); err != nil {
		t.Fatalf("AttachJobWithOptions: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if connections != 1 {
		t.Errorf("server saw %d connections, want 1", connections)
	}
}

// TestAttachJob_ReconnectGivesUpAndPointsAtReattach pins what the user sees
// when the network never comes back: a real error, plus the job id they need
// to get back in by hand (`boid agent claude` never prints it otherwise).
func TestAttachJob_ReconnectGivesUpAndPointsAtReattach(t *testing.T) {
	shortenAttachReconnectWaits(t)

	c := newUnixWSServer(t, newWSHandler(t, func(t *testing.T, conn *websocket.Conn, _ *http.Request) {
		writeServerMsg(t, conn, wsAttachServerMsg{Type: "attach"})
		conn.CloseNow()
	}))

	var stdout, notices bytes.Buffer
	err := c.AttachJob("job-gone", nil, &stdout, AttachOptions{
		Notify:          &notices,
		ReconnectWindow: 30 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected an error once the reconnect window elapsed")
	}
	if !strings.Contains(notices.String(), "boid attach job-gone") {
		t.Errorf("notices = %q, want a `boid attach job-gone` hint", notices.String())
	}
}

// TestAttachJob_ReconnectDisabled keeps the pre-reconnect contract available
// for callers that want a drop reported immediately.
func TestAttachJob_ReconnectDisabled(t *testing.T) {
	var mu sync.Mutex
	connections := 0
	c := newUnixWSServer(t, newWSHandler(t, func(t *testing.T, conn *websocket.Conn, _ *http.Request) {
		mu.Lock()
		connections++
		mu.Unlock()
		writeServerMsg(t, conn, wsAttachServerMsg{Type: "attach"})
		conn.CloseNow()
	}))

	var stdout bytes.Buffer
	err := c.AttachJob("job-nodial", nil, &stdout, AttachOptions{ReconnectWindow: -1})
	if err == nil {
		t.Fatal("expected the drop to be reported as an error")
	}

	mu.Lock()
	defer mu.Unlock()
	if connections != 1 {
		t.Errorf("server saw %d connections, want 1 (reconnect disabled)", connections)
	}
}

// TestAttachJob_FirstDialFailureIsNotRetried keeps a bad job id / down
// daemon a fast, honest error instead of a timeout.
func TestAttachJob_FirstDialFailureIsNotRetried(t *testing.T) {
	shortenAttachReconnectWaits(t)

	var mu sync.Mutex
	requests := 0
	c := newUnixWSServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"no such job"}`)) //nolint:errcheck
	}))

	var stdout bytes.Buffer
	err := c.AttachJob("job-missing", nil, &stdout, AttachOptions{})
	if err == nil || !strings.Contains(err.Error(), "no such job") {
		t.Fatalf("error = %v, want the server's own message", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if requests != 1 {
		t.Errorf("server saw %d handshake attempts, want 1", requests)
	}
}

// TestAttachJob_WedgedReconnectDialTimesOut pins the third failure mode
// behind the Windows-sleep report. On resume, the pooled TCP connection the
// reconnect dial reuses can be black-holed rather than reset: the peer is
// gone but nothing ever says so, so the WS handshake neither succeeds nor
// fails. Without a bound on the dial, attachOnce parks inside websocket.Dial
// forever, the reconnect loop never gets back to its deadline check, and the
// session is lost with no message at all — the same wedge class as the
// gateway's half-dead upstream connections.
//
// The server here accepts the first connection and drops it abnormally, then
// stops answering entirely. AttachJob must still come back, with the
// give-up notice, well inside the test's own patience.
func TestAttachJob_WedgedReconnectDialTimesOut(t *testing.T) {
	shortenAttachReconnectWaits(t)

	origDialTimeout := attachDialTimeout
	attachDialTimeout = 50 * time.Millisecond
	t.Cleanup(func() { attachDialTimeout = origDialTimeout })

	var mu sync.Mutex
	attempt := 0
	c := newUnixWSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := attempt
		attempt++
		mu.Unlock()

		if n == 0 {
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			writeServerMsg(t, conn, wsAttachServerMsg{Type: "attach"})
			conn.CloseNow()
			return
		}
		// Never respond: the handshake hangs exactly like a dial onto a
		// black-holed connection.
		<-r.Context().Done()
	}))

	done := make(chan error, 1)
	go func() {
		var stdout, notices bytes.Buffer
		err := c.AttachJob("job-wedged", nil, &stdout, AttachOptions{
			Notify:          &notices,
			ReconnectWindow: 100 * time.Millisecond,
		})
		if !strings.Contains(notices.String(), "boid attach job-wedged") {
			t.Errorf("notices = %q, want a `boid attach job-wedged` hint", notices.String())
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error once the reconnect window elapsed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("AttachJob wedged inside a dial that never completes")
	}
}
