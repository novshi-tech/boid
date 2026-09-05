// Package gwtransport builds the outbound (gateway → upstream)
// http.Transport shared by internal/gitgateway and internal/apigateway.
//
// New restores IdleConnTimeout and configures an HTTP/2 keep-alive ping on
// top of an otherwise zero-value http.Transport, so a connection whose peer
// silently vanishes (still looks ESTABLISHED to the local kernel) gets
// evicted from the pool instead of wedging every subsequent request against
// it indefinitely.
package gwtransport

import (
	"net/http"
	"time"
)

const (
	// ExpectContinueTimeout makes the outbound transport actually wait for
	// the upstream's 100-continue before streaming the body, rather than
	// silently ignoring the client's "Expect: 100-continue" header.
	ExpectContinueTimeout = 5 * time.Second

	// IdleConnTimeout matches http.DefaultTransport's own value. It bounds
	// how long a pooled connection that nobody is using survives, which is
	// what eventually retires a connection whose peer vanished while the
	// gateway was idle.
	IdleConnTimeout = 90 * time.Second

	// SendPingTimeout is how long an HTTP/2 connection may go without
	// receiving a frame before the transport sends a keep-alive PING. Zero
	// — net/http's own default — means no health check at all, which is
	// precisely the gap that let a half-dead h2 connection stay in the pool
	// indefinitely.
	SendPingTimeout = 30 * time.Second

	// PingTimeout is how long that PING may go unanswered before the
	// connection is closed and evicted from the pool, so the next request
	// dials a fresh one instead of joining the wedge. Set explicitly rather
	// than relying on net/http's 15s default, so the pairing with
	// SendPingTimeout stays visible at the call site.
	PingTimeout = 15 * time.Second
)

// New returns the outbound Transport both gateways proxy through. Each call
// builds a fresh Transport: a gateway must own its connection pool, so one
// wedged upstream can never be shared across gateways.
//
// Deliberately NOT set, so the change stays scoped to connection liveness:
//
//   - Proxy stays nil (not http.ProxyFromEnvironment, which is what
//     http.DefaultTransport uses). The gateways dial their upstreams
//     directly today; honoring proxy env vars would be a behavior change
//     to the daemon's egress path, not a bug fix.
//   - ResponseHeaderTimeout stays zero. It is an HTTP/1.1-only backstop
//     (the HTTP/2 path does not honor it), and on the h1 path a canceled
//     request already tears its connection down, so h1 self-heals after a
//     single failure without it. Setting it would instead risk breaking
//     legitimately slow upstreams — notably git-upload-pack, which can
//     spend a long time computing a pack before it sends the first
//     response header.
//   - Every body-streaming-relevant field (DisableCompression,
//     WriteBufferSize, ...) stays at its zero value, preserving the
//     unbuffered streaming semantics both gateways rely on.
func New() *http.Transport {
	return &http.Transport{
		ExpectContinueTimeout: ExpectContinueTimeout,
		IdleConnTimeout:       IdleConnTimeout,
		// Both gateways already got HTTP/2 implicitly, from net/http's
		// auto-upgrade of a Transport with no TLSClientConfig and no Dial
		// hooks. Stating it explicitly keeps HTTP2 below meaningful if
		// either of those is ever set here, instead of silently
		// downgrading the upstream leg to HTTP/1.1 along with it.
		ForceAttemptHTTP2: true,
		HTTP2: &http.HTTP2Config{
			SendPingTimeout: SendPingTimeout,
			PingTimeout:     PingTimeout,
		},
	}
}
