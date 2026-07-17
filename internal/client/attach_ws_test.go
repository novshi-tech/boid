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
	"strings"
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

	if err := c.AttachJob("job-1", stdin, &stdout); err != nil {
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

	if err := c.AttachJob("job-1", nil, nil); err != nil {
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

	err := c.AttachJob("job-1", nil, nil)
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
	go func() { done <- c.AttachJob("job-1", stdin, nil) }()

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

	err := c.AttachJob("job-1", nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := err.Error(); !strings.Contains(got, "unauthorized") {
		t.Errorf("error = %q, want it to contain %q", got, "unauthorized")
	}
}
