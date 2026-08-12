package gwtransport

import (
	"net/http"
	"testing"
)

// TestNew_EvictsIdleConnections pins the first half of the 2026-08-11
// production wedge (see the package doc comment): an outbound Transport
// with IdleConnTimeout left at zero keeps a pooled connection whose peer
// silently vanished forever, and hands it back out to every later request.
func TestNew_EvictsIdleConnections(t *testing.T) {
	tr := New()
	if tr.IdleConnTimeout == 0 {
		t.Fatal("IdleConnTimeout is 0: idle pooled connections never expire, so a connection whose peer silently vanished is reused forever")
	}
	if tr.IdleConnTimeout != IdleConnTimeout {
		t.Errorf("IdleConnTimeout = %v, want %v (http.DefaultTransport parity)", tr.IdleConnTimeout, IdleConnTimeout)
	}
}

// TestNew_PreservesExpectContinueTimeout guards the one field both gateways
// did set before this package existed: dropping it would silently stop
// honoring a client's "Expect: 100-continue".
func TestNew_PreservesExpectContinueTimeout(t *testing.T) {
	if got := New().ExpectContinueTimeout; got != ExpectContinueTimeout {
		t.Errorf("ExpectContinueTimeout = %v, want %v", got, ExpectContinueTimeout)
	}
}

// TestNew_ConfiguresHTTP2KeepAlive pins the second — and, for the observed
// incident, decisive — half: HTTP/2 was already being negotiated
// implicitly, but http.HTTP2Config's own docs say a zero SendPingTimeout
// performs no health check, so a half-dead h2 connection is never
// detected. Unlike HTTP/1.1 it also survives its own canceled streams, so
// every subsequent request piles onto the same wedge.
func TestNew_ConfiguresHTTP2KeepAlive(t *testing.T) {
	tr := New()
	if !tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 is false: HTTP/2 would be left to net/http's implicit auto-upgrade, which any later TLSClientConfig/Dial hook silently disables")
	}
	if tr.HTTP2 == nil {
		t.Fatal("HTTP2 is nil: no keep-alive ping is configured, so a half-dead h2 connection is never evicted")
	}
	if tr.HTTP2.SendPingTimeout == 0 {
		t.Fatal("HTTP2.SendPingTimeout is 0: net/http performs no health check at all in that state")
	}
	if tr.HTTP2.SendPingTimeout != SendPingTimeout {
		t.Errorf("HTTP2.SendPingTimeout = %v, want %v", tr.HTTP2.SendPingTimeout, SendPingTimeout)
	}
	if tr.HTTP2.PingTimeout != PingTimeout {
		t.Errorf("HTTP2.PingTimeout = %v, want %v", tr.HTTP2.PingTimeout, PingTimeout)
	}
}

// TestNew_LeavesProxyAndResponseHeaderTimeoutUnset documents the two
// http.DefaultTransport-ish knobs this package deliberately does NOT adopt,
// so a later "just use DefaultTransport" simplification has to confront the
// reasons first (see New's own doc comment): honoring proxy env vars would
// change the daemon's egress path, and ResponseHeaderTimeout would cap
// legitimately slow upstreams such as git-upload-pack's pack computation.
func TestNew_LeavesProxyAndResponseHeaderTimeoutUnset(t *testing.T) {
	tr := New()
	if tr.Proxy != nil {
		t.Error("Proxy is set: gateways dial upstreams directly; honoring proxy env vars is a behavior change, not a bug fix")
	}
	if tr.ResponseHeaderTimeout != 0 {
		t.Errorf("ResponseHeaderTimeout = %v, want 0: it is h1-only and would cap slow upstreams like git-upload-pack", tr.ResponseHeaderTimeout)
	}
}

// TestNew_ReturnsIndependentTransports guards against a package-level
// singleton creeping in: each gateway must own its own connection pool, so
// one wedged upstream cannot be shared across gateways.
func TestNew_ReturnsIndependentTransports(t *testing.T) {
	a, b := New(), New()
	if a == b {
		t.Fatal("New returned the same *http.Transport twice: the gateways would share one connection pool")
	}
	if a.HTTP2 == b.HTTP2 {
		t.Error("New returned a shared *http.HTTP2Config: mutating one gateway's h2 settings would silently retune the other")
	}
	var _ http.RoundTripper = a
}
