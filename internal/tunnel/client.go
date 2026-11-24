package tunnel

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	maxBodyBytes      = 8 << 20
	maxFrameBytes     = 16 << 20
	maxInflightCalls  = 8
	requestTimeout    = 25 * time.Second
	callTimeout       = 60 * time.Second
	pingInterval      = 30 * time.Second
	minReconnectDelay = time.Second
	maxReconnectDelay = 60 * time.Second

	closeReplaced = 4001
	closeRemoved  = 4003
)

// ErrNotConnected is returned by Call while the tunnel is down.
var ErrNotConnected = errors.New("tunnel: not connected to the gateway")

// ErrTooManyCalls is returned by Call when maxInflightCalls are pending.
var ErrTooManyCalls = errors.New("tunnel: too many calls in flight")

// inFrame is any frame the gateway sends.
type inFrame struct {
	Type      string      `json:"type"`
	ID        string      `json:"id"`
	Method    string      `json:"method"`
	Path      string      `json:"path"`
	Headers   [][2]string `json:"headers"`
	Body      *string     `json:"body"`
	Status    int         `json:"status"`
	ServiceID string      `json:"service_id"`
	Name      string      `json:"name"`
	Message   string      `json:"message"`
}

type responseFrame struct {
	Type    string      `json:"type"`
	ID      string      `json:"id"`
	Status  int         `json:"status"`
	Headers [][2]string `json:"headers"`
	Body    *string     `json:"body"`
}

type callFrame struct {
	Type    string      `json:"type"`
	ID      string      `json:"id"`
	Method  string      `json:"method"`
	Service string      `json:"service"`
	Path    string      `json:"path"`
	Headers [][2]string `json:"headers"`
	Body    *string     `json:"body"`
}

type helloFrame struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
	Version     string `json:"version,omitempty"`
}

// CallResult is the gateway's answer to a call back.
type CallResult struct {
	Status  int
	Headers [][2]string
	Body    []byte
}

// Status is a snapshot of the connection for `boid tunnel status`.
type Status struct {
	Gateway     string    `json:"gateway,omitempty"`
	ServiceID   string    `json:"service_id,omitempty"`
	Name        string    `json:"name,omitempty"`
	Connected   bool      `json:"connected"`
	ConnectedAt time.Time `json:"connected_at,omitzero"`
	LastError   string    `json:"last_error,omitempty"`
	Stopped     string    `json:"stopped,omitempty"`
}

// Client keeps one paired local service connected and relays both ways.
type Client struct {
	Identity    *Identity
	Handler     http.Handler
	Description string
	Version     string
	HTTPClient  *http.Client
	Now         func() time.Time

	mu      sync.Mutex
	conn    *websocket.Conn
	waiting map[string]chan inFrame
	status  Status
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Status returns the current connection state.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.status
	s.Gateway, s.ServiceID, s.Name = c.Identity.Gateway, c.Identity.ServiceID, c.Identity.Name
	return s
}

func (c *Client) setError(err error) {
	c.mu.Lock()
	c.status.LastError = err.Error()
	c.mu.Unlock()
}

// Run connects and reconnects until ctx ends or the gateway tells this
// client to stop (registration removed, or replaced by a newer connection).
func (c *Client) Run(ctx context.Context) error {
	delay := minReconnectDelay
	for {
		opened, err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		switch websocket.CloseStatus(err) {
		case closeRemoved:
			c.stop("the registration was removed at the gateway; pair again")
			return err
		case closeReplaced:
			c.stop("another connection with the same key replaced this one")
			return err
		}
		if err != nil {
			c.setError(err)
			slog.Warn("tunnel: disconnected", "gateway", c.Identity.Gateway, "err", err)
		}
		if opened {
			delay = minReconnectDelay
		} else {
			delay = min(delay*2, maxReconnectDelay)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

func (c *Client) stop(reason string) {
	c.mu.Lock()
	c.status.Stopped = reason
	c.mu.Unlock()
	slog.Warn("tunnel: stopped", "gateway", c.Identity.Gateway, "reason", reason)
}

func (c *Client) runOnce(ctx context.Context) (opened bool, err error) {
	var challenge struct {
		Challenge string `json:"challenge"`
	}
	status, err := postJSON(ctx, c.httpClient(), c.Identity.Gateway+"/tunnel/challenge", struct{}{}, &challenge)
	if err != nil {
		return false, fmt.Errorf("challenge: %w", err)
	}
	if status != http.StatusOK || challenge.Challenge == "" {
		return false, fmt.Errorf("challenge: status %d", status)
	}
	wsURL := "ws" + strings.TrimPrefix(c.Identity.Gateway, "http") + "/tunnel/connect"
	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.Identity.assertion(challenge.Challenge, c.now()))
	header.Set("User-Agent", "boid-tunnel")
	conn, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: c.httpClient(), HTTPHeader: header})
	if err != nil {
		if resp != nil {
			return false, fmt.Errorf("connect: status %d", resp.StatusCode)
		}
		return false, fmt.Errorf("connect: %w", err)
	}
	conn.SetReadLimit(maxFrameBytes)
	defer conn.CloseNow()

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	c.mu.Lock()
	c.conn = conn
	c.waiting = make(map[string]chan inFrame)
	c.status = Status{Connected: true, ConnectedAt: c.now()}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.conn = nil
		for _, ch := range c.waiting {
			close(ch)
		}
		c.waiting = nil
		c.status.Connected = false
		c.mu.Unlock()
	}()
	slog.Info("tunnel: connected", "gateway", c.Identity.Gateway, "name", c.Identity.Name)

	if err := c.writeJSON(connCtx, conn, helloFrame{Type: "hello", Description: c.Description, Version: c.Version}); err != nil {
		return true, err
	}
	go func() {
		t := time.NewTicker(pingInterval)
		defer t.Stop()
		for {
			select {
			case <-connCtx.Done():
				return
			case <-t.C:
				_ = conn.Write(connCtx, websocket.MessageText, []byte("ping"))
			}
		}
	}()

	for {
		typ, data, err := conn.Read(connCtx)
		if err != nil {
			return true, err
		}
		if typ != websocket.MessageText || string(data) == "pong" {
			continue
		}
		var f inFrame
		if err := json.Unmarshal(data, &f); err != nil {
			slog.Warn("tunnel: unreadable frame from the gateway", "err", err)
			continue
		}
		switch f.Type {
		case "request":
			go c.serve(connCtx, conn, f)
		case "call_result":
			c.mu.Lock()
			ch := c.waiting[f.ID]
			delete(c.waiting, f.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- f
			}
		case "error":
			slog.Warn("tunnel: gateway reported an error", "message", f.Message)
		}
	}
}

func (c *Client) writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, raw)
}

// serve answers one owner request with the local handler.
func (c *Client) serve(ctx context.Context, conn *websocket.Conn, f inFrame) {
	resp := c.handle(ctx, f)
	resp.Type, resp.ID = "response", f.ID
	if err := c.writeJSON(ctx, conn, resp); err != nil {
		slog.Warn("tunnel: could not send a response", "err", err)
	}
}

func (c *Client) handle(ctx context.Context, f inFrame) responseFrame {
	if c.Handler == nil {
		return errorResponse(http.StatusServiceUnavailable, "boid is starting")
	}
	var body io.Reader = http.NoBody
	if f.Body != nil {
		raw, err := base64.StdEncoding.DecodeString(*f.Body)
		if err != nil {
			return errorResponse(http.StatusBadRequest, "malformed request body")
		}
		body = bytes.NewReader(raw)
	}
	path := f.Path
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, f.Method, "http://tunnel.local"+path, body)
	if err != nil {
		return errorResponse(http.StatusBadRequest, "malformed request")
	}
	for _, h := range f.Headers {
		req.Header.Add(h[0], h[1])
	}
	req.RemoteAddr = "tunnel"
	rec := newRecorder()
	c.Handler.ServeHTTP(rec, req)
	if rec.overflow {
		return errorResponse(http.StatusBadGateway, "the response is larger than the tunnel allows")
	}
	return responseFrame{Status: rec.code(), Headers: outgoingHeaders(rec.header), Body: encodeBody(rec.body.Bytes())}
}

// Call sends a request to the gateway's /api/<service>/<path> as the owner.
func (c *Client) Call(ctx context.Context, method, service, path string, headers [][2]string, body []byte) (*CallResult, error) {
	id := newID()
	ch := make(chan inFrame, 1)
	c.mu.Lock()
	conn := c.conn
	if conn == nil {
		c.mu.Unlock()
		return nil, ErrNotConnected
	}
	if len(c.waiting) >= maxInflightCalls {
		c.mu.Unlock()
		return nil, ErrTooManyCalls
	}
	c.waiting[id] = ch
	c.mu.Unlock()
	drop := func() {
		c.mu.Lock()
		if c.waiting != nil {
			delete(c.waiting, id)
		}
		c.mu.Unlock()
	}

	if err := c.writeJSON(ctx, conn, callFrame{Type: "call", ID: id, Method: method, Service: service, Path: path, Headers: headers, Body: encodeBody(body)}); err != nil {
		drop()
		return nil, ErrNotConnected
	}
	timer := time.NewTimer(callTimeout)
	defer timer.Stop()
	select {
	case f, ok := <-ch:
		if !ok {
			return nil, ErrNotConnected
		}
		out := &CallResult{Status: f.Status, Headers: f.Headers}
		if f.Body != nil {
			raw, err := base64.StdEncoding.DecodeString(*f.Body)
			if err != nil {
				return nil, fmt.Errorf("tunnel: malformed call_result body")
			}
			out.Body = raw
		}
		return out, nil
	case <-timer.C:
		drop()
		return &CallResult{Status: http.StatusGatewayTimeout}, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func encodeBody(b []byte) *string {
	if len(b) == 0 {
		return nil
	}
	s := base64.StdEncoding.EncodeToString(b)
	return &s
}

func errorResponse(status int, message string) responseFrame {
	raw, _ := json.Marshal(map[string]string{"error": message})
	return responseFrame{Status: status, Headers: [][2]string{{"content-type", "application/json"}}, Body: encodeBody(raw)}
}

var hopByHop = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-connection": true, "transfer-encoding": true,
	"upgrade": true, "te": true, "trailer": true, "content-length": true, "set-cookie": true,
}

func outgoingHeaders(h http.Header) [][2]string {
	out := [][2]string{}
	for name, values := range h {
		lower := strings.ToLower(name)
		if hopByHop[lower] {
			continue
		}
		for _, v := range values {
			out = append(out, [2]string{lower, v})
		}
	}
	return out
}

// recorder buffers a handler's response up to maxBodyBytes.
type recorder struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	overflow bool
}

func newRecorder() *recorder { return &recorder{header: http.Header{}} }

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
}

func (r *recorder) Write(p []byte) (int, error) {
	r.WriteHeader(http.StatusOK)
	if r.body.Len()+len(p) > maxBodyBytes {
		r.overflow = true
		return 0, errors.New("tunnel: response too large")
	}
	return r.body.Write(p)
}

func (r *recorder) code() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}
