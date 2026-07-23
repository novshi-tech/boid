package dockerproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// --- test helpers ---

// fakeUpstream runs a minimal Docker-API-compatible HTTP server on a Unix socket.
type fakeUpstream struct {
	sockPath  string
	listener  net.Listener
	requests  chan capturedRequest
	handler   http.HandlerFunc
	closeOnce func()
}

type capturedRequest struct {
	Method string
	Path   string
	Body   []byte
}

func newFakeUpstream(t *testing.T, handler http.HandlerFunc) *fakeUpstream {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "upstream.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal("fakeUpstream listen:", err)
	}
	f := &fakeUpstream{
		sockPath: sock,
		listener: ln,
		requests: make(chan capturedRequest, 32),
		handler:  handler,
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			f.requests <- capturedRequest{Method: r.Method, Path: r.URL.Path, Body: body}
			f.handler(w, r)
		}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.closeOnce = cancel
	go func() {
		srv.Serve(ln) //nolint:errcheck
	}()
	go func() {
		<-ctx.Done()
		srv.Close()
		ln.Close()
	}()
	t.Cleanup(f.Close)
	return f
}

func (f *fakeUpstream) Close() {
	if f.closeOnce != nil {
		f.closeOnce()
	}
}

// newProxy starts a proxy Server on a temp Unix socket pointing at upstreamSock.
func newProxy(t *testing.T, upstreamSock string) (sockPath string) {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "proxy.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal("proxy listen:", err)
	}
	srv := New(upstreamSock)
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() {
		srv.Close()
		ln.Close()
	})
	return sock
}

// newProxyWithWorkspaceNetwork is newProxy plus Server.SetWorkspaceNetwork
// (§決定5's forced sibling network injection).
func newProxyWithWorkspaceNetwork(t *testing.T, upstreamSock, network string) (sockPath string) {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "proxy.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal("proxy listen:", err)
	}
	srv := New(upstreamSock)
	srv.SetWorkspaceNetwork(network)
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() {
		srv.Close()
		ln.Close()
	})
	return sock
}

// httpClientForSocket returns an http.Client that dials the given Unix socket.
func httpClientForSocket(sockPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
			DisableKeepAlives: true,
		},
	}
}

// doProxyRequest sends a request to the proxy and returns the response.
func doProxyRequest(t *testing.T, proxySock, method, path string, body []byte) *http.Response {
	t.Helper()
	client := httpClientForSocket(proxySock)
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://docker"+path, bodyReader)
	if err != nil {
		t.Fatal("building request:", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal("do request:", err)
	}
	return resp
}

// --- deny tests ---

func TestProxy_deny_dangerousCreate_noUpstreamHit(t *testing.T) {
	var upstreamHits int
	upstream := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusCreated)
	})

	proxySock := newProxy(t, upstream.sockPath)

	body := mustJSON(map[string]interface{}{
		"HostConfig": map[string]interface{}{"Privileged": true},
	})
	resp := doProxyRequest(t, proxySock, "POST", "/containers/create", body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}
	if upstreamHits != 0 {
		t.Errorf("upstream received %d requests; expected 0", upstreamHits)
	}
}

// TestProxy_deny_publishAllPorts_noUpstreamHit pins Blocker 1 (PR6 codex
// review) at the Server level: PublishAllPorts must be rejected the same
// way TestProxy_deny_dangerousCreate_noUpstreamHit already pins for
// Privileged — never reaching upstream.
func TestProxy_deny_publishAllPorts_noUpstreamHit(t *testing.T) {
	var upstreamHits int
	upstream := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusCreated)
	})

	proxySock := newProxy(t, upstream.sockPath)

	body := mustJSON(map[string]interface{}{
		"HostConfig": map[string]interface{}{"PublishAllPorts": true},
	})
	resp := doProxyRequest(t, proxySock, "POST", "/containers/create", body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}
	if upstreamHits != 0 {
		t.Errorf("upstream received %d requests; expected 0", upstreamHits)
	}
}

func TestProxy_deny_imageBuild_403(t *testing.T) {
	upstream := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	proxySock := newProxy(t, upstream.sockPath)

	for _, path := range []string{"/build", "/session", "/v1.43/build"} {
		resp := doProxyRequest(t, proxySock, "POST", path, nil)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s: expected 403, got %d", path, resp.StatusCode)
		}
	}
}

func TestProxy_deny_unknownMutating_failClosed(t *testing.T) {
	upstream := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	proxySock := newProxy(t, upstream.sockPath)

	resp := doProxyRequest(t, proxySock, "POST", "/some/new/api", nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for unknown mutating, got %d", resp.StatusCode)
	}
}

// --- transfer (passthrough) tests ---

func TestProxy_transfer_getVersion(t *testing.T) {
	upstream := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"Version":"24.0.0"}`)
	})
	proxySock := newProxy(t, upstream.sockPath)

	resp := doProxyRequest(t, proxySock, "GET", "/version", nil)
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("24.0.0")) {
		t.Errorf("expected version in body, got %q", body)
	}

	// Verify the request reached upstream.
	select {
	case req := <-upstream.requests:
		if req.Method != "GET" || req.Path != "/version" {
			t.Errorf("upstream got %s %s", req.Method, req.Path)
		}
	case <-time.After(time.Second):
		t.Error("upstream did not receive request")
	}
}

func TestProxy_transfer_versionPrefixPreserved(t *testing.T) {
	upstream := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		// Echo the path so we can verify it was forwarded verbatim.
		fmt.Fprint(w, r.URL.Path)
	})
	proxySock := newProxy(t, upstream.sockPath)

	resp := doProxyRequest(t, proxySock, "GET", "/v1.43/containers/json", nil)
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "/v1.43/containers/json" {
		t.Errorf("upstream received path %q; expected /v1.43/containers/json", body)
	}
}

func TestProxy_transfer_safeContainersCreate_reaches_upstream(t *testing.T) {
	upstream := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"Id":"abc123"}`)
	})
	proxySock := newProxy(t, upstream.sockPath)

	body := mustJSON(map[string]interface{}{
		"Image": "alpine:latest",
		"HostConfig": map[string]interface{}{
			"NetworkMode": "bridge",
		},
	})
	resp := doProxyRequest(t, proxySock, "POST", "/containers/create", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("expected 201, got %d", resp.StatusCode)
	}
}

// TestProxy_workspaceNetwork_forcesInjection pins §決定5's "sibling の
// workspace network 強制注入" (docs/plans/phase6-container-backend.md §PR6):
// when a workspace network is configured, the upstream Docker daemon must
// see the job's workspace network — NOT whatever network the sandboxed
// client itself requested — in NetworkingConfig.EndpointsConfig, while
// every other field of the client's original body survives unchanged.
func TestProxy_workspaceNetwork_forcesInjection(t *testing.T) {
	upstream := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"Id":"abc123"}`)
	})
	proxySock := newProxyWithWorkspaceNetwork(t, upstream.sockPath, "boid-ws-myworkspace")

	body := mustJSON(map[string]interface{}{
		"Image": "alpine:latest",
		"NetworkingConfig": map[string]interface{}{
			"EndpointsConfig": map[string]interface{}{
				"attacker-chosen-network": map[string]interface{}{},
			},
		},
	})
	resp := doProxyRequest(t, proxySock, "POST", "/containers/create", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	var received capturedRequest
	select {
	case received = <-upstream.requests:
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive request")
	}

	var got struct {
		Image            string `json:"Image"`
		NetworkingConfig struct {
			EndpointsConfig map[string]json.RawMessage `json:"EndpointsConfig"`
		} `json:"NetworkingConfig"`
	}
	if err := json.Unmarshal(received.Body, &got); err != nil {
		t.Fatalf("unmarshal upstream body: %v (body: %s)", err, received.Body)
	}
	if got.Image != "alpine:latest" {
		t.Errorf("Image = %q, want alpine:latest (non-network fields must survive)", got.Image)
	}
	if _, ok := got.NetworkingConfig.EndpointsConfig["boid-ws-myworkspace"]; !ok {
		t.Errorf("EndpointsConfig = %v, want the workspace network present", got.NetworkingConfig.EndpointsConfig)
	}
	if _, ok := got.NetworkingConfig.EndpointsConfig["attacker-chosen-network"]; ok {
		t.Errorf("EndpointsConfig = %v, client-requested network must be discarded, not merged", got.NetworkingConfig.EndpointsConfig)
	}
	if len(got.NetworkingConfig.EndpointsConfig) != 1 {
		t.Errorf("EndpointsConfig = %v, want exactly one (forced) network", got.NetworkingConfig.EndpointsConfig)
	}
}

// TestProxy_workspaceNetwork_unsetLeavesBodyUnchanged verifies the
// pre-existing behavior (no Server.SetWorkspaceNetwork call — every
// existing e2e/unit test's baseline) is untouched: the client's body
// reaches upstream byte-for-byte, matching
// TestProxy_rawBodyForwarding_bytesUnchanged's own guarantee extended to a
// body that itself carries a NetworkingConfig.
func TestProxy_workspaceNetwork_unsetLeavesBodyUnchanged(t *testing.T) {
	var receivedBody []byte
	upstream := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"Id":"abc123"}`)
	})
	proxySock := newProxy(t, upstream.sockPath) // no SetWorkspaceNetwork call

	body := mustJSON(map[string]interface{}{
		"Image": "alpine:latest",
		"NetworkingConfig": map[string]interface{}{
			"EndpointsConfig": map[string]interface{}{
				"whatever-the-client-asked-for": map[string]interface{}{},
			},
		},
	})
	resp := doProxyRequest(t, proxySock, "POST", "/containers/create", body)
	defer resp.Body.Close()

	select {
	case req := <-upstream.requests:
		receivedBody = req.Body
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive request")
	}
	if !bytes.Equal(receivedBody, body) {
		t.Errorf("upstream body = %s, want unchanged %s (workspace network unset)", receivedBody, body)
	}
}

// --- raw body forwarding ---

func TestProxy_rawBodyForwarding_bytesUnchanged(t *testing.T) {
	var receivedBody []byte
	upstream := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"Id":"xyz"}`)
	})
	proxySock := newProxy(t, upstream.sockPath)

	// Build a valid (allow) body with unusual but harmless fields.
	sentBody := []byte(`{"Image":"alpine","HostConfig":{"NetworkMode":"none"},"Cmd":["sh","-c","echo hello"]}`)

	resp := doProxyRequest(t, proxySock, "POST", "/containers/create", sentBody)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("expected 201, got %d", resp.StatusCode)
	}

	select {
	case req := <-upstream.requests:
		receivedBody = req.Body
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive request")
	}

	if !bytes.Equal(sentBody, receivedBody) {
		t.Errorf("body mismatch:\n sent:     %q\n received: %q", sentBody, receivedBody)
	}
}

// --- hijack streaming ---

// fakeHijackUpstream is a raw Unix socket server for hijack testing.
// It reads the HTTP request, sends a hijack response, then exchanges raw data.
type fakeHijackUpstream struct {
	sockPath string
	listener net.Listener
	// fromClient receives bytes the upstream reads from the client after the response.
	fromClient chan []byte
	// toClient is data the upstream will send after the response headers.
	toClient []byte
}

func newFakeHijackUpstream(t *testing.T, toClient []byte) *fakeHijackUpstream {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "hijack-upstream.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal("hijack upstream listen:", err)
	}
	f := &fakeHijackUpstream{
		sockPath:   sock,
		listener:   ln,
		fromClient: make(chan []byte, 1),
		toClient:   toClient,
	}
	t.Cleanup(func() { ln.Close() })
	return f
}

// serveOne accepts a single connection, serves the hijack protocol, and returns.
func (f *fakeHijackUpstream) serveOne() {
	conn, err := f.listener.Accept()
	if err != nil {
		return
	}
	defer conn.Close()

	// Read HTTP request headers (and small body).
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	io.ReadAll(req.Body) //nolint:errcheck
	req.Body.Close()

	// Send hijack response.
	conn.Write([]byte( //nolint:errcheck
		"HTTP/1.1 200 OK\r\n" +
			"Content-Type: application/vnd.docker.raw-stream\r\n" +
			"\r\n",
	))

	// Send upstream→client data.
	if len(f.toClient) > 0 {
		conn.Write(f.toClient) //nolint:errcheck
	}

	// Read client→upstream data (drain whatever arrives, up to 1 read).
	buf := make([]byte, 256)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	n, _ := conn.Read(buf)
	if n > 0 {
		cp := make([]byte, n)
		copy(cp, buf[:n])
		select {
		case f.fromClient <- cp:
		default:
		}
	}
}

func TestProxy_hijack_execStart_responseForwarded(t *testing.T) {
	fakeUp := newFakeHijackUpstream(t, []byte("stream_data_from_upstream"))
	go fakeUp.serveOne()

	proxySock := newProxy(t, fakeUp.sockPath)

	// Dial proxy with raw connection to simulate a Docker client.
	clientConn, err := net.Dial("unix", proxySock)
	if err != nil {
		t.Fatal("dial proxy:", err)
	}
	defer clientConn.Close()

	// Send exec/start request.
	reqBody := `{"Detach":false}`
	fmt.Fprintf(clientConn,
		"POST /exec/abc123/start HTTP/1.1\r\nHost: docker\r\nContent-Length: %d\r\n\r\n%s",
		len(reqBody), reqBody,
	)

	// Read the HTTP response headers.
	br := bufio.NewReader(clientConn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal("reading response:", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "docker.raw-stream") {
		t.Errorf("expected docker.raw-stream content-type, got %q", ct)
	}

	// Read stream data forwarded from upstream.
	buf := make([]byte, 64)
	clientConn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	n, _ := br.Read(buf)
	if !bytes.Contains(buf[:n], []byte("stream_data_from_upstream")) {
		t.Errorf("expected stream data, got %q", buf[:n])
	}
}

func TestProxy_hijack_bidirectional(t *testing.T) {
	fakeUp := newFakeHijackUpstream(t, []byte("UP_TO_CLIENT"))
	go fakeUp.serveOne()

	proxySock := newProxy(t, fakeUp.sockPath)

	clientConn, err := net.Dial("unix", proxySock)
	if err != nil {
		t.Fatal("dial proxy:", err)
	}
	defer clientConn.Close()

	reqBody := `{"Detach":false}`
	fmt.Fprintf(clientConn,
		"POST /exec/abc123/start HTTP/1.1\r\nHost: docker\r\nContent-Length: %d\r\n\r\n%s",
		len(reqBody), reqBody,
	)

	br := bufio.NewReader(clientConn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal("reading response:", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Read UP_TO_CLIENT from upstream.
	buf := make([]byte, 64)
	clientConn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	n, _ := br.Read(buf)
	if !bytes.Contains(buf[:n], []byte("UP_TO_CLIENT")) {
		t.Errorf("expected UP_TO_CLIENT, got %q", buf[:n])
	}

	// Send CLIENT_TO_UP toward the upstream.
	clientConn.SetWriteDeadline(time.Now().Add(time.Second)) //nolint:errcheck
	clientConn.Write([]byte("CLIENT_TO_UP"))                  //nolint:errcheck

	// Verify upstream received it.
	select {
	case received := <-fakeUp.fromClient:
		if !bytes.Contains(received, []byte("CLIENT_TO_UP")) {
			t.Errorf("upstream expected CLIENT_TO_UP, got %q", received)
		}
	case <-time.After(3 * time.Second):
		t.Error("timeout waiting for upstream to receive client data")
	}
}

func TestProxy_hijack_goroutineCleanup(t *testing.T) {
	// Record goroutine count before the test.
	runtime.GC()
	time.Sleep(10 * time.Millisecond)
	before := runtime.NumGoroutine()

	fakeUp := newFakeHijackUpstream(t, nil)
	go fakeUp.serveOne()

	proxySock := newProxy(t, fakeUp.sockPath)

	clientConn, err := net.Dial("unix", proxySock)
	if err != nil {
		t.Fatal("dial proxy:", err)
	}

	reqBody := `{"Detach":false}`
	fmt.Fprintf(clientConn,
		"POST /exec/abc123/start HTTP/1.1\r\nHost: docker\r\nContent-Length: %d\r\n\r\n%s",
		len(reqBody), reqBody,
	)

	// Read response headers so the hijack bridge is established.
	br := bufio.NewReader(clientConn)
	http.ReadResponse(br, nil) //nolint:errcheck

	// Close client — this should cause both bridge goroutines to exit.
	clientConn.Close()

	// Wait up to 2 seconds for goroutines to settle.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		after := runtime.NumGoroutine()
		// Allow small fluctuation (test runner goroutines, GC, etc.).
		if after <= before+4 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	after := runtime.NumGoroutine()
	if after > before+4 {
		t.Errorf("possible goroutine leak: before=%d after=%d", before, after)
	}
}

func TestProxy_hijack_containersAttach(t *testing.T) {
	fakeUp := newFakeHijackUpstream(t, []byte("attach_data"))
	go fakeUp.serveOne()

	proxySock := newProxy(t, fakeUp.sockPath)

	clientConn, err := net.Dial("unix", proxySock)
	if err != nil {
		t.Fatal("dial proxy:", err)
	}
	defer clientConn.Close()

	// containers/attach has no body.
	fmt.Fprintf(clientConn,
		"POST /containers/abc123/attach?stdin=1&stdout=1 HTTP/1.1\r\nHost: docker\r\nContent-Length: 0\r\n\r\n",
	)

	br := bufio.NewReader(clientConn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal("reading response:", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	buf := make([]byte, 64)
	clientConn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	n, _ := br.Read(buf)
	if !bytes.Contains(buf[:n], []byte("attach_data")) {
		t.Errorf("expected attach_data, got %q", buf[:n])
	}
}

// --- upstream resolution tests ---

func TestResolveUpstream_explicit(t *testing.T) {
	got, err := resolveUpstream("/custom/docker.sock", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/custom/docker.sock" {
		t.Errorf("expected /custom/docker.sock, got %q", got)
	}
}

func TestResolveUpstream_dockerHostUnix(t *testing.T) {
	got, err := resolveUpstream("", "unix:///run/user/1000/docker.sock", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/run/user/1000/docker.sock" {
		t.Errorf("expected /run/user/1000/docker.sock, got %q", got)
	}
}

func TestResolveUpstream_dockerHostTCP_error(t *testing.T) {
	_, err := resolveUpstream("", "tcp://localhost:2375", nil)
	if err == nil {
		t.Error("expected error for TCP DOCKER_HOST")
	}
	if !strings.Contains(err.Error(), "TCP") {
		t.Errorf("error should mention TCP, got: %v", err)
	}
}

func TestResolveUpstream_dockerHostEmptyUnixPath_error(t *testing.T) {
	_, err := resolveUpstream("", "unix://", nil)
	if err == nil {
		t.Error("expected error for empty unix:// path")
	}
}

func TestResolveUpstream_candidates_firstExisting(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "docker.sock")
	if err := os.WriteFile(present, nil, 0600); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(dir, "nosuch.sock")

	got, err := resolveUpstream("", "", []string{absent, present})
	if err != nil {
		t.Fatal(err)
	}
	if got != present {
		t.Errorf("expected %q, got %q", present, got)
	}
}

func TestResolveUpstream_noneFound_error(t *testing.T) {
	_, err := resolveUpstream("", "", []string{"/no/such/path.sock", "/also/not/here.sock"})
	if err == nil {
		t.Error("expected error when no socket found")
	}
	if !strings.Contains(err.Error(), "no Docker socket") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestResolveUpstream_explicit_takes_priority(t *testing.T) {
	// Even if DOCKER_HOST or candidates exist, explicit wins.
	dir := t.TempDir()
	sock := filepath.Join(dir, "real.sock")
	os.WriteFile(sock, nil, 0600) //nolint:errcheck

	got, err := resolveUpstream("/explicit.sock", "unix://"+sock, []string{sock})
	if err != nil {
		t.Fatal(err)
	}
	if got != "/explicit.sock" {
		t.Errorf("expected /explicit.sock, got %q", got)
	}
}

func TestResolveUpstream_dockerHost_priority_over_candidates(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "candidate.sock")
	os.WriteFile(sock, nil, 0600) //nolint:errcheck

	got, err := resolveUpstream("", "unix:///from/docker_host.sock", []string{sock})
	if err != nil {
		t.Fatal(err)
	}
	if got != "/from/docker_host.sock" {
		t.Errorf("expected /from/docker_host.sock, got %q", got)
	}
}

// TestResolveUpstream_picksPodmanSocketWhenDockerAbsent verifies the
// public ResolveUpstream entry point falls back to the rootless podman socket
// at $XDG_RUNTIME_DIR/podman/podman.sock when no docker.sock candidate exists.
// This is the common case on hosts that ship podman by default (Fedora,
// Ubuntu with podman installed) and have no docker daemon.
func TestResolveUpstream_picksPodmanSocketWhenDockerAbsent(t *testing.T) {
	dir := t.TempDir()
	// Create the rootless podman socket layout under a fake XDG_RUNTIME_DIR
	// but deliberately omit docker.sock so the resolver must fall through.
	podmanDir := filepath.Join(dir, "podman")
	if err := os.MkdirAll(podmanDir, 0o755); err != nil {
		t.Fatal(err)
	}
	podmanSock := filepath.Join(podmanDir, "podman.sock")
	if err := os.WriteFile(podmanSock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("DOCKER_HOST", "")

	got, err := ResolveUpstream("")
	if err != nil {
		t.Fatalf("ResolveUpstream: %v", err)
	}
	if got != podmanSock {
		t.Errorf("expected %q, got %q", podmanSock, got)
	}
}

// TestResolveUpstream_prefersDockerOverPodman verifies docker.sock keeps
// its existing priority when both docker and podman sockets are present
// — adding podman fallback must not regress hosts that intentionally use
// the docker daemon.
func TestResolveUpstream_prefersDockerOverPodman(t *testing.T) {
	dir := t.TempDir()
	dockerSock := filepath.Join(dir, "docker.sock")
	if err := os.WriteFile(dockerSock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	podmanDir := filepath.Join(dir, "podman")
	if err := os.MkdirAll(podmanDir, 0o755); err != nil {
		t.Fatal(err)
	}
	podmanSock := filepath.Join(podmanDir, "podman.sock")
	if err := os.WriteFile(podmanSock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("DOCKER_HOST", "")

	got, err := ResolveUpstream("")
	if err != nil {
		t.Fatalf("ResolveUpstream: %v", err)
	}
	if got != dockerSock {
		t.Errorf("expected docker.sock %q to win over podman, got %q", dockerSock, got)
	}
}

// --- helpers for ledger-aware proxy tests ---

// newProxyWithLedger starts a proxy with an attached ledger and returns its socket path.
func newProxyWithLedger(t *testing.T, upstreamSock string, l *Ledger) string {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "proxy.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal("proxy listen:", err)
	}
	srv := NewWithLedger(upstreamSock, l)
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() {
		srv.Close()
		ln.Close()
	})
	return sock
}

func newTempLedger(t *testing.T) *Ledger {
	t.Helper()
	return NewLedger(filepath.Join(t.TempDir(), "docker-resources.jsonl"))
}

// --- id scope check tests ---

func TestScope_unknownID_returns404_noUpstreamHit(t *testing.T) {
	var hits int
	upstream := newFakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusNoContent)
	})

	l := newTempLedger(t)
	proxySock := newProxyWithLedger(t, upstream.sockPath, l)

	// Container ID not in ledger — scope check should 404.
	resp := doProxyRequest(t, proxySock, "POST", "/containers/unknown123/stop", nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
	if hits != 0 {
		t.Errorf("upstream received %d requests; expected 0", hits)
	}
}

func TestScope_knownID_transparent(t *testing.T) {
	upstream := newFakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	l := newTempLedger(t)
	_ = l.Append(ResourceEntry{Type: "container", ID: "known123"})

	proxySock := newProxyWithLedger(t, upstream.sockPath, l)

	resp := doProxyRequest(t, proxySock, "POST", "/containers/known123/stop", nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("expected 204, got %d", resp.StatusCode)
	}
}

func TestScope_networkID_unknownReturns404(t *testing.T) {
	var hits int
	upstream := newFakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	})

	l := newTempLedger(t)
	proxySock := newProxyWithLedger(t, upstream.sockPath, l)

	resp := doProxyRequest(t, proxySock, "DELETE", "/networks/foreign-net", nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
	if hits != 0 {
		t.Errorf("upstream received %d requests; expected 0", hits)
	}
}

func TestScope_execID_unknownReturns404(t *testing.T) {
	var hits int
	upstream := newFakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	})

	l := newTempLedger(t)
	proxySock := newProxyWithLedger(t, upstream.sockPath, l)

	// exec/{id}/start — ID not in ledger
	clientConn, err := net.Dial("unix", proxySock)
	if err != nil {
		t.Fatal("dial proxy:", err)
	}
	defer clientConn.Close()

	reqBody := `{"Detach":false}`
	fmt.Fprintf(clientConn,
		"POST /exec/unknownExec/start HTTP/1.1\r\nHost: docker\r\nContent-Length: %d\r\n\r\n%s",
		len(reqBody), reqBody,
	)
	br := bufio.NewReader(clientConn)
	httpResp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal("reading response:", err)
	}
	io.Copy(io.Discard, httpResp.Body) //nolint:errcheck
	httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", httpResp.StatusCode)
	}
	if hits != 0 {
		t.Errorf("upstream received %d requests; expected 0", hits)
	}
}

// TestScope_noLedger verifies that without a ledger the proxy passes all IDs through.
func TestScope_noLedger_passesAll(t *testing.T) {
	upstream := newFakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	proxySock := newProxy(t, upstream.sockPath) // no ledger

	resp := doProxyRequest(t, proxySock, "POST", "/containers/anyid/stop", nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("expected 204 without ledger, got %d", resp.StatusCode)
	}
}

// --- ID recording tests ---

func TestIDRecord_containerCreate_recordedBeforeResponse(t *testing.T) {
	const fixedID = "deadbeef0123456789abcdef"

	upstream := newFakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"Id":%q,"Warnings":[]}`, fixedID)
	})

	l := newTempLedger(t)
	proxySock := newProxyWithLedger(t, upstream.sockPath, l)

	body := mustJSON(map[string]interface{}{"Image": "alpine:latest"})
	resp := doProxyRequest(t, proxySock, "POST", "/containers/create", body)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	// By the time the response is returned the ID must already be in the ledger.
	ok, err := l.Contains("container", fixedID)
	if err != nil || !ok {
		t.Errorf("container ID not in ledger after response: ok=%v err=%v", ok, err)
	}
}

func TestIDRecord_networkCreate(t *testing.T) {
	const fixedID = "net-id-aabbcc"

	upstream := newFakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"Id":%q}`, fixedID)
	})

	l := newTempLedger(t)
	proxySock := newProxyWithLedger(t, upstream.sockPath, l)

	resp := doProxyRequest(t, proxySock, "POST", "/networks/create",
		mustJSON(map[string]interface{}{"Name": "mynet"}))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	ok, err := l.Contains("network", fixedID)
	if err != nil || !ok {
		t.Errorf("network ID not in ledger: ok=%v err=%v", ok, err)
	}
}

func TestIDRecord_volumeCreate_usesName(t *testing.T) {
	const volName = "my-volume"

	upstream := newFakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"Name":%q,"Driver":"local"}`, volName)
	})

	l := newTempLedger(t)
	proxySock := newProxyWithLedger(t, upstream.sockPath, l)

	resp := doProxyRequest(t, proxySock, "POST", "/volumes/create",
		mustJSON(map[string]interface{}{"Name": volName}))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	ok, err := l.Contains("volume", volName)
	if err != nil || !ok {
		t.Errorf("volume Name not in ledger: ok=%v err=%v", ok, err)
	}
}

func TestIDRecord_execCreate_recordsExecID(t *testing.T) {
	const containerID = "ctr-abc"
	const execID = "exec-xyz"

	upstream := newFakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"Id":%q}`, execID)
	})

	l := newTempLedger(t)
	// Pre-register the container so scope check passes.
	_ = l.Append(ResourceEntry{Type: "container", ID: containerID})
	proxySock := newProxyWithLedger(t, upstream.sockPath, l)

	resp := doProxyRequest(t, proxySock, "POST", "/containers/"+containerID+"/exec",
		mustJSON(map[string]interface{}{"Cmd": []string{"ls"}}))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	ok, err := l.Contains("exec", execID)
	if err != nil || !ok {
		t.Errorf("exec ID not in ledger: ok=%v err=%v", ok, err)
	}
}

// TestIDRecord_responseBodyUnmodified verifies the response body reaches the
// client byte-for-byte unchanged even after ID extraction.
func TestIDRecord_responseBodyUnmodified(t *testing.T) {
	const rawResponse = `{"Id":"cafebabe1234","Warnings":[]}`

	upstream := newFakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, rawResponse)
	})

	l := newTempLedger(t)
	proxySock := newProxyWithLedger(t, upstream.sockPath, l)

	resp := doProxyRequest(t, proxySock, "POST", "/containers/create",
		mustJSON(map[string]interface{}{"Image": "alpine"}))
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)

	if string(got) != rawResponse {
		t.Errorf("response body modified:\n got:  %q\n want: %q", got, rawResponse)
	}
}

// TestIDRecord_versionPrefixedPath verifies recording works with /v1.43/ prefix.
func TestIDRecord_versionPrefixedPath(t *testing.T) {
	const fixedID = "versioned-id-001"

	upstream := newFakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"Id":%q}`, fixedID)
	})

	l := newTempLedger(t)
	proxySock := newProxyWithLedger(t, upstream.sockPath, l)

	resp := doProxyRequest(t, proxySock, "POST", "/v1.43/containers/create",
		mustJSON(map[string]interface{}{"Image": "alpine"}))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	ok, err := l.Contains("container", fixedID)
	if err != nil || !ok {
		t.Errorf("versioned path: container ID not in ledger: ok=%v err=%v", ok, err)
	}
}

