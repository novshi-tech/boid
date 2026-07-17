package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// Client is a boid daemon API client. It is transport-agnostic at the call
// site (Do/GetRaw/... all build requests against baseURL) — NewUnixClient
// and NewClient's "https" branch are the two constructors that decide what
// baseURL and httpClient actually mean underneath.
type Client struct {
	// socketPath is set only for a unix-scheme client (NewUnixClient / a
	// "unix://" NewClient url) and is empty for an https-scheme client.
	// Three callers actually inspect this: IsUnix() (the base
	// discriminator — every branch that switches on transport goes
	// through it), SocketPath() (root.PersistentPreRunE passes it to
	// client.EnsureRunningAt so autostart probes the same socket the CLI
	// is about to talk to), and ProbeAlive() (which uses it to skip the
	// TCP-dial branch on unix clients). AttachJob no longer touches it —
	// Phase 3 PR3's WebSocket unification routes attach through
	// httpClient's DialContext just like every other request.
	socketPath string
	// baseURL is the origin every Do*/GetRaw*/PostRaw/PutRaw* request is
	// built against: the fixed "http://boid" placeholder for a unix client
	// (the DialContext below ignores the request's host/port entirely and
	// always dials socketPath directly — only the scheme+host need to be
	// *present* so net/http's Transport accepts the request at all) or the
	// real "https://host[:port]" origin for a remote profile.
	baseURL    string
	httpClient *http.Client
}

var ErrAttachDetached = errors.New("attach detached")

func NewUnixClient(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
		baseURL:    "http://boid",
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", socketPath)
				},
			},
		},
	}
}

// NewClient builds a Client from a profile URL (docs/plans/
// cli-remote-connection.md "Transport 分岐"): the scheme decides transport.
//
//   - "unix://<path>" — a local UNIX socket, dialed exactly like
//     NewUnixClient(<path>). token is ignored (decision 4: a local socket
//     needs no Bearer auth — connecting to it already implies local user
//     trust).
//   - "https://<host>[:port]" — TCP + TLS, with token sent as
//     "Authorization: Bearer <token>" on every request (including same-
//     origin redirects; decision 7 rejects cross-origin ones outright — see
//     sameOriginCheckRedirect).
//   - anything else ("http://" included — decision 4 explicitly leaves it
//     unsupported; plain-HTTP remote daemons are not a supported
//     configuration) — a hard error.
func NewClient(rawURL, token string) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse client url %q: %w", rawURL, err)
	}
	switch u.Scheme {
	case "unix":
		path := unixSocketPathFromURL(u)
		// A "unix://" URL with no path (or one that resolves to just "/",
		// which url.Parse leaves for "unix:///" alone) is nonsense — every
		// downstream codepath (net.Dial("unix", ""), IsUnix() → false,
		// autostart's socket-path probe, dialing the filesystem root as
		// a socket) either errors indirectly or silently misbehaves.
		// Reject at construction so the diagnostic points at the actual
		// mistake (the URL) instead of a scattered side-effect further down.
		if path == "" || path == "/" {
			return nil, fmt.Errorf("unix client url %q: missing socket path", rawURL)
		}
		return NewUnixClient(path), nil
	case "https":
		return newHTTPSClient(u, token, nil)
	default:
		return nil, fmt.Errorf("unsupported client url scheme %q (want \"unix\" or \"https\"): %s", u.Scheme, rawURL)
	}
}

// SocketPath returns the UNIX socket path this Client was built to dial,
// or "" for an https-scheme Client. root.PersistentPreRunE passes this to
// EnsureRunningAt so the autostart probe hits the same socket the CLI is
// about to talk to (docs/plans/cli-remote-connection.md PR1 codex review).
func (c *Client) SocketPath() string { return c.socketPath }

// ProbeAlive reports whether the daemon behind this client is reachable
// within timeout, at the transport layer only (no auth, no request body).
// Used by cmd/completion.go so shell TAB completion can skip a daemon that
// is not going to answer without blocking the shell on a full API request.
//
// The probe is scheme-aware:
//   - unix: net.DialTimeout("unix", ...) as before
//   - https: net.DialTimeout("tcp", host[:port default 443], ...) — just a
//     TCP connect, not a TLS handshake, since the point is "is anyone
//     listening on that port at all" not "is the cert valid" (a TLS-level
//     failure means the daemon IS up and the follow-up API request will
//     surface the real error to the user; a transport-level connect
//     failure means no daemon).
func (c *Client) ProbeAlive(timeout time.Duration) bool {
	if c.IsUnix() {
		conn, err := net.DialTimeout("unix", c.socketPath, timeout)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}
	addr, ok := c.probeDialAddress()
	if !ok {
		return false
	}
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// probeDialAddress rebuilds the "host:port" address ProbeAlive dials for
// an https-scheme Client, or ("", false) when the client has no usable
// baseURL. Split from ProbeAlive so a unit test can assert the address
// construction (in particular the IPv6 case where a naive
// `strings.Contains(":")` on u.Host would leave `[::1]` port-less) —
// without a live listener to actually dial.
//
// It uses Hostname()+Port()+JoinHostPort so IPv6 literals rebuild
// correctly: `https://[::1]` would land in u.Host as "[::1]" which
// naive colon inspection misclassifies as "has a port" and leaves us
// dialing a bracketed-but-portless address. Hostname() strips the
// brackets, Port() gives us just the port (or "" so we fall back to the
// https default), and JoinHostPort re-brackets the IPv6 hostname before
// pasting the port back on.
func (c *Client) probeDialAddress() (string, bool) {
	if c.baseURL == "" {
		return "", false
	}
	u, err := url.Parse(c.baseURL)
	if err != nil || u.Host == "" {
		return "", false
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(u.Hostname(), port), true
}

// unixSocketPathFromURL recovers the filesystem path from a "unix://" URL.
// The documented config schema (docs/plans/cli-remote-connection.md) always
// writes a triple-slash absolute path ("unix:///run/user/1000/boid.sock"),
// which url.Parse puts entirely into Path with an empty Host. A caller that
// only types two slashes ("unix://relative/path") would instead land the
// first path segment in Host — reassembling Host+Path here tolerates that
// typo instead of silently truncating the path to what followed the first
// "/".
func unixSocketPathFromURL(u *url.URL) string {
	if u.Host != "" {
		return u.Host + u.Path
	}
	return u.Path
}

// IsUnix reports whether c dials a local UNIX socket (NewUnixClient, or
// NewClient given a "unix://" url) rather than a remote HTTPS origin. root's
// PersistentPreRunE uses this to decide whether daemon autostart applies
// (decision 6: autostart only ever makes sense for a daemon this same host
// can spawn).
func (c *Client) IsUnix() bool {
	return c.socketPath != ""
}

// newHTTPSClient builds an https-scheme Client. transport, when nil,
// defaults to http.DefaultTransport at request time (bearerTransport.base);
// tests pass a transport pinned to a httptest.NewTLSServer's certificate
// (via that server's own Client().Transport) so the Bearer-header and
// same-origin-redirect behavior can be exercised without disabling TLS
// verification process-wide — production callers (NewClient) always pass
// nil and get the system cert store.
func newHTTPSClient(u *url.URL, token string, transport http.RoundTripper) (*Client, error) {
	if u.Host == "" {
		return nil, fmt.Errorf("https client url %q: missing host", u.String())
	}
	origin := (&url.URL{Scheme: "https", Host: u.Host}).String()
	return &Client{
		baseURL: origin,
		httpClient: &http.Client{
			Transport:     &bearerTransport{token: token, base: transport},
			CheckRedirect: sameOriginCheckRedirect,
		},
	}, nil
}

// bearerTransport injects "Authorization: Bearer <token>" into every
// outgoing request (RFC 6750; matches internal/api/auth/bearer_verifier.go's
// case-insensitive scheme parsing on the server side). It applies the
// header fresh on every RoundTrip call rather than relying on net/http's
// own "copy headers to the redirected request" behavior, so it naturally
// re-applies on a same-origin redirect and is never even asked to apply to
// a cross-origin one — sameOriginCheckRedirect (below) rejects that hop
// before net/http builds the request this RoundTripper would see.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if t.token == "" {
		return base.RoundTrip(req)
	}
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.token)
	return base.RoundTrip(req)
}

// sameOriginCheckRedirect implements decision 7 (docs/plans/
// cli-remote-connection.md): an https-scheme Client must never follow a
// redirect to a different origin (scheme+host) than the request that
// triggered it — a compromised or merely misconfigured remote daemon must
// not be able to redirect this CLI's Bearer token to an arbitrary
// third-party host. Same-origin redirects (e.g. path canonicalization)
// still work exactly like net/http's own default policy, including the
// same 10-hop cap.
func sameOriginCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		// net/http only invokes CheckRedirect once a redirect has actually
		// happened, always with the triggering request already in via — this
		// guards a hypothetical empty via defensively rather than relying on
		// that invariant to avoid an index panic.
		return nil
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	first := via[0]
	if req.URL.Scheme != first.URL.Scheme || req.URL.Host != first.URL.Host {
		return fmt.Errorf("refusing cross-origin redirect from %s://%s to %s://%s",
			first.URL.Scheme, first.URL.Host, req.URL.Scheme, req.URL.Host)
	}
	return nil
}

func DefaultSocketPath() string {
	if s := os.Getenv("BOID_SOCKET"); s != "" {
		return s
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "boid.sock")
	}
	uid := strconv.Itoa(os.Getuid())
	runDir := filepath.Join("/run/user", uid)
	if _, err := os.Stat(runDir); err == nil {
		return filepath.Join(runDir, "boid.sock")
	}
	return fmt.Sprintf("/tmp/boid-%s.sock", uid)
}

// Do issues an HTTP request with no deadline. Suitable for foreground CLI
// commands where the user explicitly waits for a response. For latency-
// bounded callers (shell completion, health probes) use DoContext with a
// bounded context.Context so a slow / hung daemon never blocks the user's
// shell indefinitely.
func (c *Client) Do(method, path string, body any, result any) error {
	return c.DoContext(context.Background(), method, path, body, result)
}

// DoContext is Do with a caller-supplied context — the request is canceled
// (and any in-flight IO unblocked) when ctx is Done, so completion and
// probe callers can enforce a wall-clock bound on the daemon round trip.
// Behaviorally identical to Do at the API surface (same URL construction,
// same headers, same status-code handling); the sole difference is the
// context propagation.
func (c *Client) DoContext(ctx context.Context, method, path string, body any, result any) error {
	var reqBody *bytes.Buffer
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}
		reqBody = bytes.NewBuffer(data)
	} else {
		reqBody = &bytes.Buffer{}
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var errResp map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&errResp) // best-effort; fall back to HTTP status below
		if msg, ok := errResp["error"]; ok {
			return fmt.Errorf("%s", msg)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// DoWithContentType performs an HTTP request with a custom Content-Type and raw body.
func (c *Client) DoWithContentType(method, path, contentType string, body []byte, result any) error {
	var reqBody *bytes.Buffer
	if body != nil {
		reqBody = bytes.NewBuffer(body)
	} else {
		reqBody = &bytes.Buffer{}
	}

	req, err := http.NewRequest(method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var errResp map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&errResp) // best-effort; fall back to HTTP status below
		if msg, ok := errResp["error"]; ok {
			return fmt.Errorf("%s", msg)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// ListJobs - フィルタ付きで全プロジェクト横断のジョブ一覧を取得
func (c *Client) ListJobs(filter api.JobListFilter) ([]api.JobWithContext, error) {
	path := "/api/jobs"
	var params []byte
	if filter.Status != "" {
		params = append(params, ("status=" + filter.Status)...)
	}
	if filter.Interactive != nil {
		sep := ""
		if len(params) > 0 {
			sep = "&"
		}
		if *filter.Interactive {
			params = append(params, (sep + "interactive=true")...)
		} else {
			params = append(params, (sep + "interactive=false")...)
		}
	}
	if len(params) > 0 {
		path += "?" + string(params)
	}

	var jobs []api.JobWithContext
	if err := c.Do("GET", path, nil, &jobs); err != nil {
		return nil, err
	}
	return jobs, nil
}

// wsAttachClientMsg / wsAttachServerMsg mirror the wire format
// internal/api/ws_attach.go's wsClientMsg/wsServerMsg define (docs/plans/
// cli-remote-connection.md Phase 3 PR3: "フレーム構成 (既存 ws_attach.go の
// 仕様に準拠)"). Kept as an independent copy rather than an import — those
// server-side types are unexported, and internal/client's own architecture
// rule (TestClientDoesNotDependOnBehavior in architecture_test.go) treats
// the JSON wire contract, not internal/api's Go types, as the sharing
// boundary with the server. Keep both structs' field sets in sync by hand
// if ws_attach.go's frame shape ever changes.
//
// input_close has no counterpart in wsServerMsg — it is a client→server-only
// frame type (see attachSendInputClose) that ws_attach.go's read loop
// switches on alongside "input"/"resize".
type wsAttachClientMsg struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"` // base64-encoded, "input" only
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

type wsAttachServerMsg struct {
	Type    string `json:"type"`
	Data    string `json:"data,omitempty"`
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// AttachJob opens a live interactive attach to jobID over WebSocket
// (docs/plans/cli-remote-connection.md Phase 3 PR3: "WebSocket attach
// 一本化") and blocks until the job's process exits (the server sends an
// "exit" frame, or the socket closes normally) or the caller detaches
// (stdin.Read returning ErrAttachDetached — see cmd/attach.go's
// detachReader). Works identically for a unix-scheme Client (the WS
// handshake dials over the same UNIX socket via c.httpClient's
// DialContext) and an https-scheme one (the handshake request carries the
// same Authorization: Bearer header every other request on c.httpClient
// does, via bearerTransport) — reusing c.httpClient rather than building a
// separate one is what gives WS attach Bearer auth for free, and is the
// point of "一本化": one transport, one auth path, for both local and
// remote profiles alike.
//
// This replaces the old raw net.Dial("unix", ...) + hand-written
// `Upgrade: boid-attach` implementation, which only ever worked for a unix
// socket Client — an https-scheme Client had no socketPath to dial at all,
// so remote attach previously just returned a "not yet supported" error —
// and duplicated a second attach transport alongside the Web UI's existing
// WebSocket one for no reason beyond history (decision 5, docs/plans/
// cli-remote-connection.md: two attach transports serving the same purpose
// is a maintenance burden the plan explicitly rejects).
//
// stdin may be nil (no input forwarding) — used by
// TestServerJobRuntimeAttachAndResize (internal/server/server_phase3_test.go),
// which only cares about output replay.
func (c *Client) AttachJob(jobID string, stdin io.Reader, stdout io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}

	ctx := context.Background()
	conn, resp, err := websocket.Dial(ctx, c.baseURL+"/api/jobs/"+jobID+"/attach/ws", &websocket.DialOptions{
		HTTPClient: c.httpClient,
		// Origin deliberately mirrors c.baseURL itself rather than one of
		// the server's allowedOrigins() patterns (localhost/127.0.0.1/
		// [::1]/public_url — those exist for a *browser*'s cross-origin WS
		// connect; see ws_attach.go's allowedOrigins doc comment). A direct
		// Go http.Client dial's Origin and Host are the same origin by
		// construction, and websocket.Accept's authenticateOrigin already
		// short-circuits to "allowed" whenever Origin's host equals the
		// request Host — true here unconditionally — before it ever
		// consults the pattern list. Sending it at all (rather than
		// omitting the header entirely, which authenticateOrigin also
		// allows) is just explicit-over-implicit: it documents at the wire
		// level which origin this connection claims to be from.
		HTTPHeader: http.Header{"Origin": []string{c.baseURL}},
	})
	if err != nil {
		return fmt.Errorf("dial attach websocket: %w", attachDialError(resp, err))
	}
	defer conn.CloseNow()

	outputErrCh := make(chan error, 1)
	go func() {
		outputErrCh <- attachReadOutput(ctx, conn, stdout)
	}()

	if stdin == nil {
		return <-outputErrCh
	}

	inputErrCh := make(chan error, 1)
	go func() {
		inputErrCh <- attachSendInput(ctx, conn, stdin)
	}()

	for {
		select {
		case err := <-outputErrCh:
			return err
		case err := <-inputErrCh:
			switch {
			case errors.Is(err, ErrAttachDetached):
				// Mirrors the pre-PR3 behavior: a detach abandons the whole
				// connection immediately rather than politely signaling
				// end-of-input — the user asked to leave, not to let the
				// remote command finish reading from a pipe that isn't
				// closing. CloseNow (not Close) is what makes it immediate:
				// Close would block up to websocket.Conn's 5-second close
				// handshake, which the old raw-hijack transport never did.
				_ = conn.CloseNow()
				return nil
			case err == nil || errors.Is(err, io.EOF):
				// stdin closed cleanly (not a detach) — tell the server so
				// a non-interactive StdinForward job sees a real EOF (see
				// dispatcher.LocalRuntime.CloseInputRuntime's doc comment),
				// mirroring the old raw-transport's closeConnWrite
				// half-close. Then keep waiting on output/exit; never
				// select on inputErrCh again (a nil channel blocks forever
				// in select, which is exactly "don't send more input").
				_ = attachSendInputClose(ctx, conn)
				inputErrCh = nil
			default:
				// Any other input-side failure is USUALLY a symptom of the
				// server having closed the connection because the job's
				// process exited (`yes | boid exec -- head -n1`: head
				// exits, server sends exit and closes, our next stdin write
				// races into a net.ErrClosed / websocket.CloseError from
				// the just-closed conn). Wait for the output side to speak
				// before deciding — an "exit" frame that arrived
				// microseconds earlier is the real story, and returning
				// the input-side write error would misreport a clean
				// process exit as an attach failure. If the output side
				// itself fails or is silent, its error (or nil for a
				// clean close, which normalizeAttachWSError handles) is
				// what we want to surface.
				return <-outputErrCh
			}
		}
	}
}

// attachDialError extracts a server-provided error message from a failed
// WS handshake response, mirroring the old raw-Upgrade AttachJob's
// {"error": "..."} decoding (WSAttachHandler.ServeHTTP's 401 response, and
// any other pre-upgrade failure, writes that same JSON shape). Falls back
// to err unchanged when there is no response body or it isn't the expected
// shape — websocket.Dial's own error already describes the underlying
// failure either way.
func attachDialError(resp *http.Response, err error) error {
	if resp == nil || resp.Body == nil {
		return err
	}
	defer resp.Body.Close()
	var errResp map[string]string
	if decodeErr := json.NewDecoder(resp.Body).Decode(&errResp); decodeErr == nil {
		if msg, ok := errResp["error"]; ok {
			return errors.New(msg)
		}
	}
	return err
}

// attachReadOutput reads server frames from conn until an "exit" frame, an
// "error" frame, or the connection closes, decoding each "output" frame's
// base64 payload straight to stdout. A clean WS close (StatusNormalClosure
// / StatusGoingAway — every send-loop exit path in ws_attach.go's
// ServeHTTP closes with one of these two codes) is treated the same as an
// explicit "exit" frame: nil, not an error.
func attachReadOutput(ctx context.Context, conn *websocket.Conn, stdout io.Writer) error {
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return normalizeAttachWSError(err)
		}
		var msg wsAttachServerMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue // tolerate a malformed frame rather than aborting the attach
		}
		switch msg.Type {
		case "output":
			data, err := base64.StdEncoding.DecodeString(msg.Data)
			if err != nil || len(data) == 0 {
				continue
			}
			if _, err := stdout.Write(data); err != nil {
				return err
			}
		case "exit":
			// The exit frame's Code field is a spec-reserved slot
			// (docs/plans/cli-remote-connection.md "フレーム構成":
			// `exit: {code}`). Today the server always sends 0 there —
			// exit codes are actually surfaced via a separate REST call
			// (cmd/exec.go's fetchExecExitCode), NOT through this frame,
			// so AttachJob has no need to return one. Wiring an actual
			// exit code through this path (server-side capture + a
			// method-signature bump on Client.AttachJob) is tracked as
			// an unresolved point in the plan doc; deliberately not
			// silently returning msg.Code today would mislead callers
			// into using a value that is always 0.
			return nil
		case "error":
			return errors.New(msg.Message)
		}
	}
}

// attachSendInput reads stdin and forwards each chunk as a base64-encoded
// "input" frame, until stdin.Read returns a non-nil error — io.EOF on a
// clean close, ErrAttachDetached on Ctrl-] (cmd/attach.go's detachReader),
// or a genuine read error. All three are returned as-is for AttachJob's
// select loop to interpret; this function itself never inspects err beyond
// "is it nil".
func attachSendInput(ctx context.Context, conn *websocket.Conn, stdin io.Reader) error {
	buf := make([]byte, 4096)
	for {
		n, readErr := stdin.Read(buf)
		if n > 0 {
			msg := wsAttachClientMsg{Type: "input", Data: base64.StdEncoding.EncodeToString(buf[:n])}
			b, marshalErr := json.Marshal(msg)
			if marshalErr != nil {
				return marshalErr
			}
			if writeErr := conn.Write(ctx, websocket.MessageText, b); writeErr != nil {
				return writeErr
			}
		}
		if readErr != nil {
			return readErr
		}
	}
}

// attachSendInputClose sends the "input_close" frame (see
// dispatcher.RuntimeInputWriter.CloseInput's doc comment for the server
// side) telling the server this attach will send no further input.
// Best-effort: called after stdin already hit EOF, with the output side of
// the same connection still in use — a write failure here just means the
// connection is already on its way down, which the caller (AttachJob)
// observes directly via the output side instead.
func attachSendInputClose(ctx context.Context, conn *websocket.Conn) error {
	b, err := json.Marshal(wsAttachClientMsg{Type: "input_close"})
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, b)
}

// normalizeAttachWSError collapses every "the attach is over, and that's
// fine" signal — a clean WS close, a context cancellation, or the
// lower-level net.ErrClosed/io.EOF a Read can still surface depending on
// exactly where the connection was torn down — into nil, mirroring the old
// raw-transport normalizeAttachIOError's contract. Any other error
// (protocol violation, network failure mid-stream, ...) passes through
// unchanged.
func normalizeAttachWSError(err error) error {
	if err == nil {
		return nil
	}
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway:
		return nil
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// TaskListFilter holds filters for listing tasks.
type TaskListFilter struct {
	Status    string
	ProjectID string
}

// ListTasks fetches tasks with optional status and project filters.
func (c *Client) ListTasks(filter TaskListFilter) ([]*orchestrator.Task, error) {
	path := "/api/tasks"
	var params []string
	if filter.Status != "" {
		params = append(params, "status="+filter.Status)
	}
	if filter.ProjectID != "" {
		params = append(params, "project_id="+filter.ProjectID)
	}
	if len(params) > 0 {
		path += "?" + strings.Join(params, "&")
	}

	var tasks []*orchestrator.Task
	if err := c.Do("GET", path, nil, &tasks); err != nil {
		return nil, err
	}
	return tasks, nil
}

// ListProjects fetches all projects.
func (c *Client) ListProjects() ([]*orchestrator.Project, error) {
	var projects []*orchestrator.Project
	if err := c.Do("GET", "/api/projects", nil, &projects); err != nil {
		return nil, err
	}
	return projects, nil
}

// ListWorkspaces fetches all workspaces.
func (c *Client) ListWorkspaces() ([]*orchestrator.WorkspaceSummary, error) {
	var workspaces []*orchestrator.WorkspaceSummary
	if err := c.Do("GET", "/api/workspaces", nil, &workspaces); err != nil {
		return nil, err
	}
	return workspaces, nil
}

// GetTaskDetail fetches task metadata + actions + jobs for a given task ID.
func (c *Client) GetTaskDetail(id string) (*api.TaskDetailView, error) {
	var detail api.TaskDetailView
	if err := c.Do("GET", "/api/tasks/"+id+"/detail", nil, &detail); err != nil {
		return nil, err
	}
	return &detail, nil
}

// CreateTask creates a new task via POST /api/tasks.
func (c *Client) CreateTask(req api.CreateTaskRequest) (*orchestrator.Task, error) {
	var task orchestrator.Task
	if err := c.Do("POST", "/api/tasks", req, &task); err != nil {
		return nil, err
	}
	return &task, nil
}

// GetProject fetches a single project by ID via GET /api/projects/{id}.
func (c *Client) GetProject(id string) (*orchestrator.Project, error) {
	var project orchestrator.Project
	if err := c.Do("GET", "/api/projects/"+id, nil, &project); err != nil {
		return nil, err
	}
	return &project, nil
}

// UpdateTask updates the title and description of a task via PATCH /api/tasks/{id}.
func (c *Client) UpdateTask(id string, req api.UpdateTaskRequest) (*orchestrator.Task, error) {
	var task orchestrator.Task
	if err := c.Do("PATCH", "/api/tasks/"+id, req, &task); err != nil {
		return nil, err
	}
	return &task, nil
}

// DeleteTask deletes a task via DELETE /api/tasks/{id}.
func (c *Client) DeleteTask(id string) error {
	return c.Do("DELETE", "/api/tasks/"+id, nil, nil)
}

// DuplicateTask duplicates a task via POST /api/tasks/{id}/duplicate.
func (c *Client) DuplicateTask(id string) (*orchestrator.Task, error) {
	req := api.DuplicateTaskRequest{AutoStart: false}
	var task orchestrator.Task
	if err := c.Do("POST", "/api/tasks/"+id+"/duplicate", req, &task); err != nil {
		return nil, err
	}
	return &task, nil
}

// RerunTask resets a done/aborted task to pending via POST /api/tasks/{id}/rerun.
func (c *Client) RerunTask(id string, autoStart bool) (*orchestrator.Task, error) {
	req := api.RerunTaskRequest{AutoStart: autoStart}
	var task orchestrator.Task
	if err := c.Do("POST", "/api/tasks/"+id+"/rerun", req, &task); err != nil {
		return nil, err
	}
	return &task, nil
}

// AnswerTask submits an answer for an awaiting task via POST /api/tasks/{id}/answer.
func (c *Client) AnswerTask(taskID, questionID, answer string) error {
	req := api.AnswerTaskRequest{QuestionID: questionID, Answer: answer}
	return c.Do("POST", "/api/tasks/"+taskID+"/answer", req, nil)
}

// ApplyAction sends an action to POST /api/tasks/{taskID}/actions.
func (c *Client) ApplyAction(taskID string, req api.ApplyActionRequest) (*api.ActionApplication, error) {
	var result api.ActionApplication
	if err := c.Do("POST", "/api/tasks/"+taskID+"/actions", req, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetRaw performs a GET request and returns the raw response body and status code.
func (c *Client) GetRaw(path string) (statusCode int, body []byte, err error) {
	req, err := http.NewRequest("GET", c.baseURL+path, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read body: %w", err)
	}
	return resp.StatusCode, data, nil
}

// GetRawWithAccept performs a GET request with a custom Accept header,
// returning the raw response body and status code regardless of status
// (mirrors GetRaw) — used by `boid workspace export`
// (docs/plans/workspace-db-consolidation.md PR5 Step D) to explicitly
// request the yaml export body, even though the server today always
// responds with application/yaml regardless of Accept.
func (c *Client) GetRawWithAccept(path, accept string) (statusCode int, body []byte, err error) {
	req, err := http.NewRequest("GET", c.baseURL+path, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("create request: %w", err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read body: %w", err)
	}
	return resp.StatusCode, data, nil
}

// PostRaw performs a POST request with a custom Content-Type and raw body,
// returning the raw response status code and body regardless of status
// (mirrors PutRawWithIfMatch's rationale) — used by `boid workspace import`
// (docs/plans/workspace-db-consolidation.md PR5 Step E) so the CLI can
// distinguish 409 (create-only conflict against an existing slug) from 400
// (bad mode/host_commands reference) from 200 (success) instead of losing
// that distinction to a single generic error string.
func (c *Client) PostRaw(path, contentType string, body []byte) (statusCode int, respBody []byte, err error) {
	req, err := http.NewRequest(http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read body: %w", err)
	}
	return resp.StatusCode, data, nil
}

// PutRawWithIfMatch performs a PUT request with a custom Content-Type and
// (optional) If-Match header, returning the raw response status code and
// body regardless of status — unlike Do/DoWithContentType, which collapse
// every 4xx/5xx into a generic error. Used by `boid workspace edit`
// (docs/plans/workspace-db-consolidation.md PR4 Step E/H) so the CLI can
// distinguish 412 (stale revision) from 428 (missing If-Match) from 200
// (success) instead of losing that distinction to a single error string.
func (c *Client) PutRawWithIfMatch(path, contentType string, body []byte, ifMatch string) (statusCode int, respBody []byte, err error) {
	req, err := http.NewRequest(http.MethodPut, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read body: %w", err)
	}
	return resp.StatusCode, data, nil
}

func (c *Client) ResizeJob(jobID string, rows, cols int) error {
	return c.Do("POST", "/api/jobs/"+jobID+"/resize", map[string]int{
		"rows": rows,
		"cols": cols,
	}, nil)
}
