package dockerproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Server is a Docker API proxy that filters and forwards requests to an upstream
// Docker daemon via a Unix socket.
type Server struct {
	upstream  string
	transport *http.Transport
	srv       *http.Server
	ledger    *Ledger
}

// New creates a Server that forwards to the given upstream Unix socket path.
func New(upstreamSocket string) *Server {
	return NewWithLedger(upstreamSocket, nil)
}

// NewWithLedger creates a Server with an attached resource ledger.
// The ledger enables id scope checks (404 for IDs not owned by this sandbox)
// and write-ahead ID recording from creation responses.
func NewWithLedger(upstreamSocket string, l *Ledger) *Server {
	s := &Server{upstream: upstreamSocket, ledger: l}
	s.transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", upstreamSocket)
		},
		DisableKeepAlives: true,
	}
	s.srv = &http.Server{Handler: http.HandlerFunc(s.serveHTTP)}
	return s
}

// Serve begins accepting connections on ln. It blocks until Close is called.
func (s *Server) Serve(ln net.Listener) error {
	return s.srv.Serve(ln)
}

// Close shuts down the server.
func (s *Server) Close() error {
	return s.srv.Close()
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	method := strings.ToUpper(r.Method)

	// Read and buffer the body for policy inspection and raw forwarding.
	var bodyBytes []byte
	if r.Body != nil && r.Body != http.NoBody {
		var err error
		bodyBytes, err = io.ReadAll(io.LimitReader(r.Body, int64(MaxBodyBytes+1)))
		r.Body.Close()
		if err != nil {
			http.Error(w, "reading request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if len(bodyBytes) > MaxBodyBytes {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
	}

	bare := stripVersion(r.URL.Path)

	// Scope check: when a ledger is attached, reject operations on resource IDs
	// not owned by this sandbox with 404 (behave as if the resource doesn't exist).
	if s.ledger != nil {
		resType, id := scopeTarget(bare)
		if resType != "" {
			ok, err := s.ledger.Contains(resType, id)
			if err != nil {
				slog.Warn("docker proxy: scope check error", "err", err)
				http.Error(w, "scope check: "+err.Error(), http.StatusInternalServerError)
				return
			}
			if !ok {
				slog.Info("docker proxy: SCOPE DENY", "method", method, "path", r.URL.Path, "type", resType, "id", id)
				http.NotFound(w, r)
				return
			}
		}
	}

	verdict := CheckRequest(method, r.URL.Path, bodyBytes)
	if !verdict.Allow {
		slog.Warn("docker proxy: DENY", "method", method, "path", r.URL.Path, "reason", verdict.Reason)
		http.Error(w, "forbidden: "+verdict.Reason, http.StatusForbidden)
		return
	}

	slog.Debug("docker proxy: ALLOW", "method", method, "path", r.URL.Path)

	if isHijackEndpoint(method, bare) {
		s.serveHijack(w, r, bodyBytes)
		return
	}
	s.serveForward(w, r, bodyBytes)
}

// isHijackEndpoint returns true for endpoints that use HTTP connection hijacking
// (exec start and container attach produce raw bidirectional streams).
func isHijackEndpoint(method, bare string) bool {
	if method != "POST" {
		return false
	}
	return matchesPattern(bare, "/exec/*/start") || matchesPattern(bare, "/containers/*/attach")
}

// serveForward proxies a standard request/response to the upstream daemon.
// The original URL (including any API version prefix) is sent verbatim.
// When a ledger is attached and this is a creation endpoint, the new resource
// ID is recorded in the ledger (with fsync) before the response is returned to
// the client, ensuring "every ID the client knows is already in the ledger".
func (s *Server) serveForward(w http.ResponseWriter, r *http.Request, bodyBytes []byte) {
	upURL := &url.URL{
		Scheme:   "http",
		Host:     "docker", // arbitrary; DialContext routes to the Unix socket
		Path:     r.URL.Path,
		RawQuery: r.URL.RawQuery,
	}

	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, upURL.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		http.Error(w, "building upstream request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	copyRequestHeaders(upReq.Header, r.Header)
	upReq.ContentLength = int64(len(bodyBytes))

	resp, err := s.transport.RoundTrip(upReq)
	if err != nil {
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// For creation endpoints with a ledger: buffer the response body, record
	// the new resource ID (fsync), then forward the unmodified body to the client.
	if s.ledger != nil {
		bare := stripVersion(r.URL.Path)
		resType, idField := creationResourceType(strings.ToUpper(r.Method), bare)
		if resType != "" && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(MaxBodyBytes+1)))
			if readErr == nil && len(respBody) <= MaxBodyBytes {
				if id := extractCreationID(respBody, idField); id != "" {
					if err := s.ledger.Append(ResourceEntry{Type: resType, ID: id}); err != nil {
						slog.Warn("docker proxy: ledger append", "type", resType, "id", id, "err", err)
					}
				}
			}
			// Forward headers and the buffered (unmodified) body.
			for k, vv := range resp.Header {
				for _, v := range vv {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(resp.StatusCode)
			w.Write(respBody) //nolint:errcheck
			return
		}
	}

	// Standard streaming path (no ID recording needed).
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body) //nolint:errcheck
}

// extractCreationID parses the upstream creation response body and returns the
// value of the given JSON field (e.g. "Id" or "Name").
func extractCreationID(body []byte, field string) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	raw, ok := m[field]
	if !ok {
		return ""
	}
	var id string
	if err := json.Unmarshal(raw, &id); err != nil {
		return ""
	}
	return id
}

// serveHijack handles endpoints that switch to raw bidirectional streaming
// (exec/start and containers/attach). It dials upstream directly, forwards
// the request, reads the response headers, then bridges both connections
// as raw TCP streams with guaranteed cleanup.
func (s *Server) serveHijack(w http.ResponseWriter, r *http.Request, bodyBytes []byte) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}

	upConn, err := net.Dial("unix", s.upstream)
	if err != nil {
		http.Error(w, "upstream dial failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Send the original request (verbatim path + headers + body) to upstream.
	upReq, err := http.NewRequest(r.Method, r.URL.RequestURI(), bytes.NewReader(bodyBytes))
	if err != nil {
		upConn.Close()
		http.Error(w, "building upstream request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	copyRequestHeaders(upReq.Header, r.Header)
	upReq.Host = "docker"
	upReq.ContentLength = int64(len(bodyBytes))

	if err := upReq.Write(upConn); err != nil {
		upConn.Close()
		http.Error(w, "writing to upstream: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Read upstream response headers (leaves body/stream in upBR).
	upBR := bufio.NewReader(upConn)
	upResp, err := http.ReadResponse(upBR, upReq)
	if err != nil {
		upConn.Close()
		http.Error(w, "reading upstream response: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Hijack client connection before sending the response.
	clientConn, clientBW, err := hijacker.Hijack()
	if err != nil {
		upResp.Body.Close()
		upConn.Close()
		return
	}

	// Forward upstream response headers to the client.
	fmt.Fprintf(clientBW, "HTTP/1.1 %s\r\n", upResp.Status)
	upResp.Header.Write(clientBW) //nolint:errcheck
	fmt.Fprint(clientBW, "\r\n")
	clientBW.Flush() //nolint:errcheck

	// Bridge both directions; when either side closes, both are force-closed
	// to prevent goroutine leaks. upBR is used for the upstream read direction
	// to drain any bytes already buffered after the response headers.
	bidirectionalBridge(clientConn, upConn, upBR)
}

// bidirectionalBridge copies between clientConn and upConn in both directions
// concurrently. upReader wraps upConn (may buffer post-header bytes from the
// upstream response). When either goroutine finishes, both connections are
// closed, causing the other goroutine to exit. Returns only after both
// goroutines have fully exited.
func bidirectionalBridge(clientConn, upConn net.Conn, upReader io.Reader) {
	done := make(chan struct{}, 2)

	go func() {
		defer func() { done <- struct{}{} }()
		io.Copy(upConn, clientConn) //nolint:errcheck
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		io.Copy(clientConn, upReader) //nolint:errcheck
	}()

	<-done
	clientConn.Close()
	upConn.Close()
	<-done
}

// hop-by-hop headers must not be forwarded upstream.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailers":            true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

func copyRequestHeaders(dst, src http.Header) {
	for k, vv := range src {
		if hopByHopHeaders[k] {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}
