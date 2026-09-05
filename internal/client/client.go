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
	"github.com/novshi-tech/boid/internal/apiwire"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// Client is a boid daemon API client, transport-agnostic at the call site
// (Do/GetRaw/... all build requests against baseURL).
type Client struct {
	// socketPath is set only for a unix-scheme client and empty for an
	// https-scheme client; it is the base discriminator IsUnix() checks.
	socketPath string
	// baseURL is the origin every Do*/GetRaw*/PostRaw/PutRaw* request is
	// built against: a fixed placeholder for a unix client (the DialContext
	// below dials socketPath directly regardless of host/port) or the real
	// "https://host[:port]" origin for a remote profile.
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

// NewClient builds a Client from a profile URL; the scheme decides transport:
//
//   - "unix://<path>" — a local UNIX socket, dialed like NewUnixClient.
//     token is ignored (a local socket already implies local user trust).
//   - "https://<host>[:port]" — TCP + TLS, with token sent as
//     "Authorization: Bearer <token>" on every request (including same-
//     origin redirects; see sameOriginCheckRedirect for cross-origin ones).
//   - "http://<loopback-host>[:port]" — used by the CLI's own host-mode
//     orchestration to reach the container-backend daemon's CLI listener
//     with a shared-secret Bearer token instead of TLS. Restricted to a
//     loopback hostname (see newHTTPClient) so a Bearer token can never be
//     sent in cleartext to a genuinely remote host.
//   - anything else — a hard error.
func NewClient(rawURL, token string) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse client url %q: %w", rawURL, err)
	}
	switch u.Scheme {
	case "unix":
		path := unixSocketPathFromURL(u)
		// An empty or "/" path is nonsense here; reject at construction so
		// the error points at the URL rather than a scattered side effect.
		if path == "" || path == "/" {
			return nil, fmt.Errorf("unix client url %q: missing socket path", rawURL)
		}
		return NewUnixClient(path), nil
	case "https":
		return newHTTPSClient(u, token, nil)
	case "http":
		return newHTTPClient(u, token, nil)
	default:
		return nil, fmt.Errorf("unsupported client url scheme %q (want \"unix\", \"https\", or loopback \"http\"): %s", u.Scheme, rawURL)
	}
}

// SocketPath returns the UNIX socket path this Client was built to dial,
// or "" for an https-scheme Client.
func (c *Client) SocketPath() string { return c.socketPath }

// ProbeAlive reports whether the daemon behind this client is reachable
// within timeout, at the transport layer only (no auth, no request body).
// For an https client this is a bare TCP connect, not a TLS handshake — a
// TLS failure means the daemon IS up and a follow-up API request will
// surface the real error; only a transport-level failure means no daemon.
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
// baseURL. Uses Hostname()+Port()+JoinHostPort rather than naive colon
// splitting so IPv6 literals like "[::1]" rebuild correctly.
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

// unixSocketPathFromURL recovers the filesystem path from a "unix://" URL,
// tolerating a two-slash typo ("unix://relative/path", which url.Parse
// lands in Host rather than Path) by reassembling Host+Path.
func unixSocketPathFromURL(u *url.URL) string {
	if u.Host != "" {
		return u.Host + u.Path
	}
	return u.Path
}

// IsUnix reports whether c dials a local UNIX socket rather than a remote
// HTTPS origin. Used to decide whether daemon autostart applies.
func (c *Client) IsUnix() bool {
	return c.socketPath != ""
}

// newHTTPSClient builds an https-scheme Client. transport, when nil,
// defaults to http.DefaultTransport at request time; tests pass a
// transport pinned to a test server's certificate so TLS verification
// doesn't need to be disabled process-wide.
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

// newHTTPClient builds an http (no TLS) scheme Client for the CLI's
// host-mode orchestration, which already owns the daemon container's
// lifecycle end to end and so has no independent trust decision left for a
// certificate to make — the Bearer token is the only credential. Rejects a
// non-loopback host as cheap insurance against sending that token in
// cleartext to a genuinely remote host. transport, when nil, defaults to
// http.DefaultTransport at request time; tests pass one pinned to a test
// server's client instead of a real loopback listener.
func newHTTPClient(u *url.URL, token string, transport http.RoundTripper) (*Client, error) {
	if u.Host == "" {
		return nil, fmt.Errorf("http client url %q: missing host", u.String())
	}
	switch u.Hostname() {
	case "127.0.0.1", "::1", "localhost":
		// supported
	default:
		return nil, fmt.Errorf("http client url %q: only a loopback host (127.0.0.1/::1/localhost) is supported for the unencrypted http scheme; use https:// for a remote daemon", u.String())
	}
	origin := (&url.URL{Scheme: "http", Host: u.Host}).String()
	return &Client{
		baseURL: origin,
		httpClient: &http.Client{
			Transport:     &bearerTransport{token: token, base: transport},
			CheckRedirect: sameOriginCheckRedirect,
		},
	}, nil
}

// bearerTransport injects "Authorization: Bearer <token>" into every
// outgoing request. It applies the header fresh on every RoundTrip call
// rather than relying on net/http's redirect header-copying, so it
// naturally re-applies on a same-origin redirect.
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

// sameOriginCheckRedirect rejects a redirect to a different origin
// (scheme+host) than the request that triggered it, so a compromised or
// misconfigured remote daemon can't redirect this CLI's Bearer token to an
// arbitrary third-party host. Same-origin redirects still work like
// net/http's own default policy, including the 10-hop cap.
func sameOriginCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		// Defensive: net/http always populates via before calling this.
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

// defaultCLIAddrHost is the loopback literal DefaultCLIAddr always binds/
// dials — mirrors newHTTPClient's own loopback-only restriction.
const defaultCLIAddrHost = "127.0.0.1"

// DefaultCLIAddr resolves the "host:port" the container-backend daemon's
// dedicated CLI TCP listener binds/is published on, dialed by the CLI's
// host-mode orchestration once the daemon container is confirmed healthy.
// cmd/start.go's buildStartConfig uses this same value to bind
// server.Config.CLIAddr, and build/container/compose.yml publishes the
// identical port — all three must be kept in lockstep if this literal
// ever changes; there is no runtime override.
func DefaultCLIAddr() string {
	return defaultCLIAddrHost + ":8442"
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
func (c *Client) ListJobs(filter apiwire.JobListFilter) ([]apiwire.JobWithContext, error) {
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

	var jobs []apiwire.JobWithContext
	if err := c.Do("GET", path, nil, &jobs); err != nil {
		return nil, err
	}
	return jobs, nil
}

// wsAttachClientMsg / wsAttachServerMsg mirror the wire format
// internal/api/ws_attach.go's wsClientMsg/wsServerMsg define. Kept as an
// independent copy rather than an import since the JSON wire contract, not
// internal/api's Go types, is the sharing boundary with the server — keep
// both structs' field sets in sync by hand if the frame shape ever changes.
//
// input_close has no counterpart in wsServerMsg — it is a client→server-only
// frame type (see attachSendInputClose).
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
	// Offset is meaningful only on the "attach" frame — the transcript
	// byte position the replay that follows starts at. See
	// ws_attach.go's sendAttach for the server side.
	Offset int64 `json:"offset,omitempty"`
	// Rendered is meaningful only on the "attach" frame — true means the
	// replay that follows is a resolved screen dump rather than raw
	// transcript bytes, and must be painted onto a cleared screen. See
	// ws_attach.go's resolveReplay for when the server chooses it.
	Rendered bool `json:"rendered,omitempty"`
}

// AttachJob opens a live interactive attach to jobID over WebSocket and
// blocks until the job's process exits (the server sends an "exit" frame,
// or the socket closes normally) or the caller detaches (stdin.Read
// returning ErrAttachDetached — see cmd/attach.go's detachReader). Works
// identically for a unix-scheme Client and an https-scheme one, since both
// route through c.httpClient (its DialContext for unix, its
// bearerTransport for the Bearer header) rather than a separate transport.
//
// stdin may be nil (no input forwarding) — used by
// TestServerJobRuntimeAttachAndResize (internal/server/server_phase3_test.go),
// which only cares about output replay.
// AttachOptions tunes how AttachJobWithOptions behaves when the connection
// drops without the server having said the job was over.
type AttachOptions struct {
	// Notify, when non-nil, receives one short status line per reconnect
	// event ("connection lost", "reconnected", "giving up"). Lines are
	// CRLF-terminated because the caller's terminal is in raw mode during
	// an interactive attach — a bare LF would stair-step. nil silences
	// them entirely, which is what a non-interactive caller wants.
	Notify io.Writer

	// ReconnectWindow bounds the total time spent retrying after a drop
	// (the clock starts at the first failure and is NOT reset by a
	// successful reconnect that later drops again). Zero means
	// defaultAttachReconnectWindow. Negative disables reconnect entirely,
	// restoring the pre-reconnect behavior of returning the drop as an
	// error immediately.
	ReconnectWindow time.Duration
}

// Reconnect pacing. These are vars only so tests can shrink them.
var (
	// defaultAttachReconnectWindow covers a dropped-but-healthy connection
	// (e.g. an intermediary like Cloudflare Tunnel or NAT reaping an idle
	// attach); those recover in seconds, and a few minutes also covers a
	// tunnel process being restarted underneath a long-lived session.
	defaultAttachReconnectWindow = 3 * time.Minute
	attachReconnectInitialWait   = 500 * time.Millisecond
	attachReconnectMaxWait       = 5 * time.Second

	// attachKeepalivePeriod mirrors ws_attach.go's wsKeepalivePeriod — both
	// ends ping idle connections, since an intermediary that meters the two
	// directions separately can otherwise leave one direction unreaped.
	attachKeepalivePeriod = 30 * time.Second
	attachPingTimeout     = 10 * time.Second

	// attachDialTimeout bounds one WS handshake. Without it, a pooled idle
	// connection whose peer is gone (e.g. a laptop resuming from sleep) can
	// leave a dial neither completing nor erroring, parking the reconnect
	// loop forever; cancelling also evicts that dead connection from
	// net/http's idle pool so the next attempt dials fresh.
	attachDialTimeout = 20 * time.Second
)

// On an unexpected drop (the connection breaking without the server
// having said the job was over) this redials with backoff and resumes the
// transcript from where it left off (the ?replay_offset= / "attach" frame
// pair, see ws_attach.go), so the user's screen picks up mid-stream
// instead of repainting the whole session. An "exit" frame, an "error"
// frame, a clean WS close, or a local detach (Ctrl-]) all return straight
// away instead of reconnecting.
func (c *Client) AttachJob(jobID string, stdin io.Reader, stdout io.Writer, opts AttachOptions) error {
	if stdout == nil {
		stdout = io.Discard
	}
	ctx := context.Background()

	st := &attachState{jobID: jobID, stdout: stdout}
	if stdin != nil {
		st.pump = startStdinPump(stdin)
		defer st.pump.stop()
	}

	window := opts.ReconnectWindow
	if window == 0 {
		window = defaultAttachReconnectWindow
	}

	wait := attachReconnectInitialWait
	var deadline time.Time
	for {
		done, err := c.attachOnce(ctx, st)
		if done {
			return err
		}
		// The connection went away mid-session. Reconnect unless the
		// caller opted out, or we never got a connection in the first
		// place (a failed FIRST dial is a real error — a bad job id, a
		// daemon that isn't listening, a rejected token — and retrying it
		// would just hide the diagnostic behind a timeout).
		if window < 0 || !st.everConnected {
			return err
		}
		if deadline.IsZero() {
			deadline = time.Now().Add(window)
		}
		if time.Now().After(deadline) {
			attachNotifyf(opts.Notify, "connection lost and could not be restored: %v", err)
			attachNotifyf(opts.Notify, "reattach with: boid attach %s", jobID)
			return err
		}
		if !st.notifiedDrop {
			// Only the first failure of this outage gets a notice; attachOnce
			// clears the flag once a connection comes back up, so the next
			// outage still gets its own.
			attachNotifyTransientf(opts.Notify, "connection lost (%v); reconnecting...", err)
			st.notifiedDrop = true
		}
		if st.waitBeforeReconnect(wait) {
			// stdin ended (EOF or Ctrl-] detach) while we were waiting.
			return nil
		}
		if wait *= 2; wait > attachReconnectMaxWait {
			wait = attachReconnectMaxWait
		}
	}
}

// attachState is the part of an attach that OUTLIVES any one connection,
// so a reconnect can pick up where the previous one stopped.
type attachState struct {
	jobID  string
	stdout io.Writer
	pump   *stdinPump

	// offset counts transcript bytes already written to stdout, and is
	// what the next connection sends as ?replay_offset=.
	offset int64
	// everConnected records that at least one WS handshake succeeded, so
	// a first-dial failure stays a hard error.
	everConnected bool
	// notifiedDrop records that the CURRENT outage already got its
	// "connection lost; reconnecting" notice, so a stubborn network
	// retrying under backoff doesn't re-flash it on every attempt.
	// attachOnce clears this the moment a dial actually succeeds.
	notifiedDrop bool
	// stdinClosed records that stdin already hit EOF, so every subsequent
	// connection re-announces it (the server tracks this per connection).
	stdinClosed bool
}

// waitBeforeReconnect sleeps for d, returning early with true if stdin
// terminated meanwhile — the user pressing Ctrl-] during a reconnect gap
// should get out immediately, not after the backoff elapses.
func (st *attachState) waitBeforeReconnect(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	var closed <-chan struct{}
	if st.pump != nil {
		closed = st.pump.closed
	}
	select {
	case <-timer.C:
		return false
	case <-closed:
		// A clean EOF is not a reason to abandon the session (a piped
		// stdin just ran out); a detach is.
		return errors.Is(st.pump.err, ErrAttachDetached)
	}
}

func attachNotifyf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, "\r\n[boid] "+format+"\r\n", args...)
}

// attachNotifyTransientf is attachNotifyf for a notice that fires WHILE the
// job may still be producing output on the same terminal. Unlike
// attachNotifyf's plain "\r\n...\r\n" (which would permanently
// scroll/displace whatever a full-screen program has drawn), this stays on
// the current row: \r + \x1b[2K clear it in place, and \x1b[s/\x1b[u save
// and restore the cursor so the job's next write lands where it left off.
func attachNotifyTransientf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, "\x1b[s\r\x1b[2K[boid] "+format+"\x1b[u", args...)
}

// stdinPump reads the caller's stdin exactly once for the whole attach,
// handing chunks to whichever connection is current. Re-reading stdin per
// connection (the shape before reconnect existed) would race: a read
// already in flight when a connection dies owns bytes the next connection
// would never see.
type stdinPump struct {
	chunks  chan []byte
	closed  chan struct{}
	stopped chan struct{}
	// err is the terminal read error (io.EOF, ErrAttachDetached, or a
	// genuine failure). Written before closed is closed, so any reader
	// that observed closed may read it without further synchronization.
	err error
}

func startStdinPump(stdin io.Reader) *stdinPump {
	p := &stdinPump{
		chunks:  make(chan []byte),
		closed:  make(chan struct{}),
		stopped: make(chan struct{}),
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				select {
				case p.chunks <- append([]byte(nil), buf[:n]...):
				case <-p.stopped:
					return
				}
			}
			if err != nil {
				p.err = err
				close(p.closed)
				return
			}
		}
	}()
	return p
}

// stop releases the pump goroutine if it is parked handing over a chunk.
// It cannot interrupt a blocked stdin.Read — nothing can — but that
// goroutine holds no resources beyond itself and dies with the process.
func (p *stdinPump) stop() { close(p.stopped) }

// attachOnce runs a single WS connection to completion. done=true means
// the attach is over for good (job exited, server error frame, local
// detach); done=false means the connection dropped and a reconnect is
// warranted. st is updated in place with everything the next connection
// needs.
func (c *Client) attachOnce(ctx context.Context, st *attachState) (done bool, err error) {
	url := c.baseURL + "/api/jobs/" + st.jobID + "/attach/ws"
	if st.offset > 0 {
		url += "?replay_offset=" + strconv.FormatInt(st.offset, 10)
	}
	// The dial gets its own bounded context (see attachDialTimeout); the
	// established connection does NOT inherit it — net/http clears the
	// request's canceler once it sees the protocol-switched response body,
	// so cancelling after a successful Dial cannot close the attach itself.
	dialCtx, cancelDial := context.WithTimeout(ctx, attachDialTimeout)
	defer cancelDial()
	conn, resp, err := websocket.Dial(dialCtx, url, &websocket.DialOptions{
		HTTPClient: c.httpClient,
		// Origin mirrors c.baseURL: a direct Go http.Client dial's Origin
		// and Host are the same origin by construction, so the server's
		// origin check always allows it regardless of its pattern list.
		HTTPHeader: http.Header{"Origin": []string{c.baseURL}},
	})
	if err != nil {
		return false, fmt.Errorf("dial attach websocket: %w", attachDialError(resp, err))
	}
	defer conn.CloseNow()
	st.everConnected = true
	st.notifiedDrop = false

	// Disable coder/websocket's 32768-byte default read limit: the daemon
	// replays a job's whole accumulated transcript on connect, which is
	// unbounded, and the client cannot assume the daemon chunks it. No cap
	// is substituted for the default — any finite ceiling just moves the
	// same failure to a longer job.
	conn.SetReadLimit(-1)

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type readResult struct {
		offset int64
		done   bool
		err    error
	}
	outputCh := make(chan readResult, 1)
	go func() {
		offset, done, err := attachReadOutput(connCtx, conn, st.stdout, st.offset)
		outputCh <- readResult{offset: offset, done: done, err: err}
	}()
	// Every return path below goes through finish, so the reader goroutine
	// is always joined and st.offset always reflects what actually reached
	// stdout on this connection.
	finish := func(overrideDone bool, overrideErr error, useOverride bool) (bool, error) {
		res := <-outputCh
		st.offset = res.offset
		if useOverride {
			return overrideDone, overrideErr
		}
		return res.done, res.err
	}

	if st.stdinClosed {
		// A previous connection already saw stdin end; the server tracks
		// that per connection, so say it again on this one.
		_ = attachSendInputClose(connCtx, conn)
	}

	var chunks <-chan []byte
	var stdinClosedCh <-chan struct{}
	if st.pump != nil {
		chunks = st.pump.chunks
		stdinClosedCh = st.pump.closed
	}
	if st.stdinClosed {
		chunks, stdinClosedCh = nil, nil
	}

	ping := time.NewTicker(attachKeepalivePeriod)
	defer ping.Stop()

	for {
		select {
		case res := <-outputCh:
			// The common ending: the server spoke last (exit frame, error
			// frame, clean close) or the wire broke under the reader.
			st.offset = res.offset
			return res.done, res.err
		case chunk := <-chunks:
			msg := wsAttachClientMsg{Type: "input", Data: base64.StdEncoding.EncodeToString(chunk)}
			b, marshalErr := json.Marshal(msg)
			if marshalErr != nil {
				return finish(true, marshalErr, true)
			}
			if writeErr := conn.Write(connCtx, websocket.MessageText, b); writeErr != nil {
				// A failing write is USUALLY a symptom of the server
				// having closed the connection because the job's process
				// exited (`yes | boid exec -- head -n1`: head exits,
				// server sends exit and closes, our next write races into
				// the just-closed conn). Let the output side speak: an
				// "exit" frame that arrived microseconds earlier is the
				// real story, and reporting the write error would
				// misreport a clean process exit as an attach failure.
				return finish(false, nil, false)
			}
		case <-stdinClosedCh:
			if errors.Is(st.pump.err, ErrAttachDetached) {
				// The user asked to leave, not to let the remote command
				// finish reading from a pipe that isn't closing. CloseNow
				// (not Close) is what makes it immediate: Close would
				// block up to websocket.Conn's 5-second close handshake.
				_ = conn.CloseNow()
				return finish(true, nil, true)
			}
			// stdin closed cleanly — tell the server so a non-interactive
			// StdinForward job sees a real EOF (see
			// dispatcher.LocalRuntime.CloseInputRuntime's doc comment),
			// then keep waiting on output/exit.
			st.stdinClosed = true
			_ = attachSendInputClose(connCtx, conn)
			chunks, stdinClosedCh = nil, nil
		case <-ping.C:
			pingCtx, cancelPing := context.WithTimeout(connCtx, attachPingTimeout)
			pingErr := conn.Ping(pingCtx)
			cancelPing()
			if pingErr != nil {
				// The peer stopped answering: tear the connection down so
				// the reader unblocks, and let the reconnect loop decide.
				_ = conn.CloseNow()
				return finish(false, pingErr, true)
			}
		}
	}
}

// attachDialError extracts a server-provided error message from a failed
// WS handshake response's {"error": "..."} body. Falls back to err
// unchanged when there is no response body or it isn't the expected shape.
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

// attachReadOutput reads server frames until the attach is over or the
// connection breaks, decoding each "output" frame's base64 payload straight
// to stdout.
//
// offset in / newOffset out is the transcript byte position: it starts at
// whatever the previous connection reached, is re-based by the server's
// leading "attach" frame (the server may replay from further back than
// asked — see clampReplayOffset), and advances by every payload byte
// written. attachOnce hands it to the next connection as ?replay_offset=.
//
// done separates "the attach is genuinely over" (an "exit" frame, an
// "error" frame, a clean WS close, a cancelled context) from "the wire
// broke" (everything else, notably the EOF/1006 an intermediary produces
// when it reaps an idle connection). Only the latter is worth reconnecting
// on, and conflating the two is precisely what left a dropped session
// looking like a finished one before reconnect existed.
func attachReadOutput(ctx context.Context, conn *websocket.Conn, stdout io.Writer, offset int64) (newOffset int64, done bool, err error) {
	for {
		_, raw, readErr := conn.Read(ctx)
		if readErr != nil {
			finished, normalized := classifyAttachWSError(readErr)
			return offset, finished, normalized
		}
		var msg wsAttachServerMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue // tolerate a malformed frame rather than aborting the attach
		}
		switch msg.Type {
		case "attach":
			offset = msg.Offset
			if msg.Rendered {
				// A rendered screen dump uses absolute positioning, so it
				// must land on a cleared screen. ED 2 + CUP home rather
				// than a full RIS, since a reset would also drop scrollback
				// the user may still want.
				if _, writeErr := stdout.Write([]byte("\x1b[2J\x1b[H")); writeErr != nil {
					return offset, true, writeErr
				}
			}
		case "output":
			data, decodeErr := base64.StdEncoding.DecodeString(msg.Data)
			if decodeErr != nil || len(data) == 0 {
				continue
			}
			if _, writeErr := stdout.Write(data); writeErr != nil {
				return offset, true, writeErr
			}
			offset += int64(len(data))
		case "exit":
			// msg.Code is always 0 today — exit codes are surfaced via a
			// separate REST call (cmd/exec.go's fetchExecExitCode), not
			// this frame — so it is deliberately not returned here.
			return offset, true, nil
		case "error":
			// Terminal by choice: the server states a reason (bad job,
			// no live stream yet), and redialing into the same reason in
			// a backoff loop would bury that message under a timeout.
			return offset, true, errors.New(msg.Message)
		}
	}
}

// attachSendInputClose sends the "input_close" frame (see
// dispatcher.RuntimeInputWriter.CloseInput's doc comment for the server
// side) telling the server this connection will send no further input.
// Best-effort: called after stdin already hit EOF, with the output side of
// the same connection still in use — a write failure here just means the
// connection is already on its way down, which the caller (attachOnce)
// observes directly via the output side instead.
func attachSendInputClose(ctx context.Context, conn *websocket.Conn) error {
	b, err := json.Marshal(wsAttachClientMsg{Type: "input_close"})
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, b)
}

// classifyAttachWSError decides whether a failed Read means the attach is
// over (finished=true, with the error normalized to nil when it is merely
// how a clean teardown surfaces) or merely that the connection broke
// (finished=false, error preserved for the reconnect loop to report) — the
// split matters most for io.EOF/net.ErrClosed, which an intermediary
// reaping an idle connection can also produce.
func classifyAttachWSError(err error) (finished bool, normalized error) {
	if err == nil {
		return true, nil
	}
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway:
		// Every send-loop exit path in ws_attach.go's ServeHTTP closes
		// with one of these two codes, so this is the server saying the
		// attach is over — same as an explicit "exit" frame.
		return true, nil
	}
	if errors.Is(err, context.Canceled) {
		// Our own teardown (attachOnce's deferred cancel).
		return true, nil
	}
	return false, err
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
func (c *Client) GetTaskDetail(id string) (*apiwire.TaskDetailView, error) {
	var detail apiwire.TaskDetailView
	if err := c.Do("GET", "/api/tasks/"+id+"/detail", nil, &detail); err != nil {
		return nil, err
	}
	return &detail, nil
}

// CreateTask creates a new task via POST /api/tasks.
func (c *Client) CreateTask(req apiwire.CreateTaskRequest) (*orchestrator.Task, error) {
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
func (c *Client) UpdateTask(id string, req apiwire.UpdateTaskRequest) (*orchestrator.Task, error) {
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
	req := apiwire.DuplicateTaskRequest{AutoStart: false}
	var task orchestrator.Task
	if err := c.Do("POST", "/api/tasks/"+id+"/duplicate", req, &task); err != nil {
		return nil, err
	}
	return &task, nil
}

// RerunTask resets a done/aborted task to pending via POST /api/tasks/{id}/rerun.
func (c *Client) RerunTask(id string, autoStart bool) (*orchestrator.Task, error) {
	req := apiwire.RerunTaskRequest{AutoStart: autoStart}
	var task orchestrator.Task
	if err := c.Do("POST", "/api/tasks/"+id+"/rerun", req, &task); err != nil {
		return nil, err
	}
	return &task, nil
}

// AnswerTask submits an answer for an awaiting task via POST /api/tasks/{id}/answer.
func (c *Client) AnswerTask(taskID, questionID, answer string) error {
	req := apiwire.AnswerTaskRequest{QuestionID: questionID, Answer: answer}
	return c.Do("POST", "/api/tasks/"+taskID+"/answer", req, nil)
}

// ApplyAction sends an action to POST /api/tasks/{taskID}/actions.
func (c *Client) ApplyAction(taskID string, req apiwire.ApplyActionRequest) (*apiwire.ActionApplication, error) {
	var result apiwire.ActionApplication
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

// GetRawWithAcceptAndRevision performs a GET request with a custom Accept
// header, returning the raw response body/status alongside the response's
// ETag header value VERBATIM, quotes included — used by `boid config
// get`/`apply -f`/`edit` to capture the daemon's current config.yaml
// revision for a later POST's If-Match. config.yaml's GET response body is
// raw YAML, not JSON, so the ETag header is the only place the revision
// exists on the wire. The quotes are kept (rather than stripped here) so
// the value stays standard entity-tag syntax (RFC 7232 §2.3) when it is
// round-tripped verbatim into a later If-Match header.
func (c *Client) GetRawWithAcceptAndRevision(path, accept string) (statusCode int, body []byte, revision string, err error) {
	req, err := http.NewRequest("GET", c.baseURL+path, nil)
	if err != nil {
		return 0, nil, "", fmt.Errorf("create request: %w", err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, "", fmt.Errorf("read body: %w", err)
	}
	return resp.StatusCode, data, resp.Header.Get("ETag"), nil
}

// PostStream performs a POST request whose body is STREAMED from body rather
// than buffered, returning the raw response status code and body regardless of
// status. It exists for `boid workspace import-home`, whose body is a tar of
// an entire workspace home — every other method here takes a []byte, which
// for a payload that size would mean holding the whole archive in the CLI's
// heap on its way to a local socket.
//
// Two consequences of streaming, both deliberate:
//
//   - The request is sent with chunked transfer encoding (no Content-Length):
//     the size is not knowable without walking the tree twice, and the walk is
//     the expensive part.
//   - The server can answer BEFORE the body has finished uploading, and
//     net/http's transport surfaces that response rather than the resulting
//     write error. That is precisely what this caller wants: the daemon refuses
//     a workspace with a running job at the very start of the request, and an
//     operator should be told so in the first second rather than after
//     streaming multiple gigabytes.
//
// ctx is honored for the whole exchange, so a caller can abort a transfer.
func (c *Client) PostStream(ctx context.Context, path, contentType string, body io.Reader) (statusCode int, respBody []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, body)
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

// PostRaw performs a POST request with a custom Content-Type and raw body,
// returning the raw response status code and body regardless of status —
// used by `boid workspace apply` and `boid project migrate`'s daemon push
// so each caller can distinguish the status codes its own body can provoke
// (409 conflict, 400 bad field/reference, 200 success) instead of losing
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
// every 4xx/5xx into a generic error. Used by `boid workspace edit` so the
// CLI can distinguish 412 (stale revision) from 428 (missing If-Match)
// from 200 (success) instead of losing that distinction to a single error
// string.
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

// PostRawWithIfMatch performs a POST request with a custom Content-Type and
// (optional) If-Match header, returning the raw response status code and
// body regardless of status — the POST counterpart of PutRawWithIfMatch,
// used by `boid config apply -f`/`edit` so the CLI can distinguish 412
// (stale revision) from 428 (missing If-Match) from 200 (success) instead
// of losing that distinction to a single error string.
func (c *Client) PostRawWithIfMatch(path, contentType string, body []byte, ifMatch string) (statusCode int, respBody []byte, err error) {
	req, err := http.NewRequest(http.MethodPost, c.baseURL+path, bytes.NewReader(body))
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
