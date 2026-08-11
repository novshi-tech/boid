package sandbox

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
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

	// BindHost, when non-empty, overrides Start's default loopback-only
	// bind ("127.0.0.1") — see ProxyManager.BindHost's own doc comment for
	// the container-backend rationale ([Blocker 2, PR7 codex review]). Must
	// be set before Start is called; changing it afterward has no effect on
	// an already-bound listener.
	BindHost string

	// DesiredPort, when non-zero, is the port Start binds to instead of
	// letting the kernel pick an ephemeral one (`:0`). Start returns the
	// bind error unchanged when the port is unavailable — deciding whether
	// that is fatal or worth retrying on another port belongs to the
	// caller, not here (ProxyManager treats it as retryable; see
	// docs/plans/egress-proxy-stable-port.md). Must be set before Start.
	DesiredPort int

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
	host := p.BindHost
	if host == "" {
		host = "127.0.0.1"
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(p.DesiredPort)))
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

// Stop closes the listener and every connection served through it.
//
// Both the server AND the listener are closed, which looks redundant but is
// not: Start hands the listener to http.Server.Serve on a NEW GOROUTINE, and
// if Stop's Close lands before that goroutine gets scheduled, Serve's own
// trackListener call sees the already-shutting-down server, returns
// ErrServerClosed immediately, and never closes the listener it was handed.
// The socket then stays bound for the life of the process.
//
// Harmless while ports were ephemeral — nothing ever asked for that number
// again. Not harmless now that a port is meant to be reused across restarts:
// found by TestProxyManager_ReusesPersistedPort failing on CI (a loaded
// machine loses that race far more often than a developer's), where the
// leaked listener kept the port and the "restarted" manager had to
// reallocate — i.e. exactly the port drift this feature exists to prevent.
func (p *Proxy) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.server != nil {
		p.server.Close()
	}
	if p.listener != nil {
		// No-op (returns an already-closed error we ignore) in the normal
		// case where Serve did close it.
		_ = p.listener.Close()
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

	if isRefusedDotlessTarget(host) {
		slog.Warn("proxy CONNECT refused: dotless hostname", "host", r.Host)
		http.Error(w, "dotless hostname not routable via egress proxy (workspace network isolation, docs/plans/phase6-cutover-followups.md §⓪-b)", http.StatusBadGateway)
		return
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
	if isRefusedDotlessTarget(host) {
		slog.Warn("proxy HTTP refused: dotless hostname", "host", r.URL.Host)
		http.Error(w, "dotless hostname not routable via egress proxy (workspace network isolation, docs/plans/phase6-cutover-followups.md §⓪-b)", http.StatusBadGateway)
		return
	}
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

// isRefusedDotlessTarget returns true when host is a single-label DNS name
// (no ".", not an IPv6 literal) and is not one of the boid infrastructure
// names a job legitimately reaches through boid's own env plumbing —
// docs/plans/phase6-cutover-followups.md §⓪-b.
//
// Why: the egress proxy sits inside the daemon container, which is
// self-connected to every currently-active workspace network at once
// (§決定5's job-workspace network isolation is implemented by joining the
// daemon to each per-workspace network so the gateway/broker/egress-proxy
// services HOSTED on the daemon are reachable from a confined job — see
// containerBackend.ensureWorkspaceNetwork). That means the proxy's own
// DNS view (docker embedded DNS, aggregated across every network it is
// currently connected to) can resolve a name like "sib-b" — declared on
// workspace B's network by a concurrently dispatched job — from a
// workspace A job's CONNECT request, and would happily open the tunnel
// once the allowlist permits it (allowed_domains: ["sib-b"] would let a
// workspace A job reach a workspace B sibling). Application-layer
// allowlist enforcement thus does not, on its own, satisfy the network-
// layer isolation contract §決定5 declares — it only closes the specific
// hole when every workspace's allowed_domains list is disjoint by
// construction, which the config type does not require or enforce.
//
// The fix is a boid convention: the egress proxy refuses to relay any
// bare-hostname target that isn't a boid infrastructure name a job would
// dial by convention. Every real cross-workspace destination a job might
// pick a bare hostname for (a sibling container `docker run` created on
// another workspace's network) hits this refusal before the CONNECT
// dial ever leaves the daemon. Every dotted destination (github.com,
// npmjs.org, ...) — the overwhelming majority of legitimate egress
// traffic — falls through to the existing isDomainAllowed check
// unchanged. The whitelist is defensive: those names never actually
// reach the proxy in practice, because applyProxyEnv already puts them
// in no_proxy (dispatcher/sandbox_builder.go), but a misconfiguration of
// no_proxy — e.g. a job override with an incomplete list — must not
// silently break broker RPC or git-gateway clones.
func isRefusedDotlessTarget(host string) bool {
	if host == "" {
		return false
	}
	if strings.Contains(host, ".") || strings.Contains(host, ":") {
		return false
	}
	switch strings.ToLower(host) {
	case "localhost", "boid-broker", "boid-gateway", "boid-egress":
		return false
	}
	return true
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
