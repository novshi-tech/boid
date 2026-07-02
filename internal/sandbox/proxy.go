package sandbox

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
)

type Proxy struct {
	// allowedDomains is the egress allowlist consulted by isDomainAllowed.
	// It is mutated via SetAllowed at sandbox-dispatch time so per-workspace
	// allowlists can be applied to a long-lived listener. Reads from inside
	// CONNECT/HTTP handlers are guarded by allowedMu (RLock); writes are
	// guarded by allowedMu (Lock). Already-established CONNECT tunnels are
	// hijacked TCP connections that bypass this list once allowed, so
	// SetAllowed never tears down an in-flight connection.
	allowedDomains []string
	allowedMu      sync.RWMutex

	listener net.Listener
	server   *http.Server
	mu       sync.Mutex
}

func NewProxy(allowedDomains []string) *Proxy {
	return &Proxy{allowedDomains: append([]string(nil), allowedDomains...)}
}

// SetAllowed replaces the proxy egress allowlist atomically. Subsequent
// CONNECT and HTTP-forward requests use the new list. Existing CONNECT
// tunnels (hijacked TCP connections) are unaffected — that is intentional:
// once an egress decision has been made for a long-lived TLS connection,
// pulling its destination out of the allowlist mid-stream would surface as
// an opaque socket close to the sandbox. Sandboxes that reconnect after
// SetAllowed will be re-validated against the new list.
func (p *Proxy) SetAllowed(domains []string) {
	p.allowedMu.Lock()
	p.allowedDomains = append([]string(nil), domains...)
	p.allowedMu.Unlock()
}

// snapshotAllowed returns a private copy of the current allowlist for the
// duration of a single request check. The slice never escapes the calling
// goroutine, so callers can iterate without holding the lock.
func (p *Proxy) snapshotAllowed() []string {
	p.allowedMu.RLock()
	defer p.allowedMu.RUnlock()
	out := make([]string, len(p.allowedDomains))
	copy(out, p.allowedDomains)
	return out
}

func (p *Proxy) Start(ctx context.Context) (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("listen: %w", err)
	}
	p.listener = ln

	p.server = &http.Server{
		Handler: http.HandlerFunc(p.serveHTTP),
	}

	go func() { _ = p.server.Serve(ln) }() // returns ErrServerClosed on Stop
	go func() {
		<-ctx.Done()
		p.Stop()
	}()

	return ln.Addr().(*net.TCPAddr).Port, nil
}

func (p *Proxy) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.server != nil {
		p.server.Close()
	}
}

func (p *Proxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handleHTTP(w, r)
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	if !p.isDomainAllowed(host) {
		slog.Warn("proxy CONNECT blocked", "host", r.Host)
		http.Error(w, "domain not allowed", http.StatusForbidden)
		return
	}

	slog.Debug("proxy CONNECT", "host", r.Host)
	targetConn, err := net.Dial("tcp", r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		targetConn.Close()
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}

	clientConn, buf, err := hijacker.Hijack()
	if err != nil {
		targetConn.Close()
		return
	}

	_, _ = buf.WriteString("HTTP/1.1 200 Connection established\r\n\r\n")
	buf.Flush()

	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(targetConn, clientConn) // best-effort pump; either side may close
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(clientConn, targetConn) // best-effort pump; either side may close
		done <- struct{}{}
	}()

	<-done
	clientConn.Close()
	targetConn.Close()
}

func (p *Proxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Host == "" {
		http.Error(w, "missing host", http.StatusBadRequest)
		return
	}

	host := r.URL.Hostname()
	if !p.isDomainAllowed(host) {
		http.Error(w, "domain not allowed", http.StatusForbidden)
		return
	}

	r.RequestURI = ""
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body) // best-effort; client may have disconnected
}

// isDomainAllowed checks if a domain matches the allowed list.
// Entries starting with "." match as a suffix (e.g. ".docker.io" matches "registry-1.docker.io").
func (p *Proxy) isDomainAllowed(domain string) bool {
	domain = strings.ToLower(domain)
	for _, d := range p.snapshotAllowed() {
		d = strings.ToLower(d)
		if strings.HasPrefix(d, ".") {
			if strings.HasSuffix(domain, d) || domain == d[1:] {
				return true
			}
		} else if d == domain {
			return true
		}
	}
	return false
}
