package server

import "testing"

// This file pins [Blocker 2, PR7 codex review]: the git gateway's
// sandbox-facing URL and TCP(mTLS) listener bind address must route by
// which sandbox backend is selected — a container-backend job has no
// 10.0.2.2 loopback projection at all (that address is a pasta/slirp
// userns artifact this backend's own Launch never creates), so it must
// reach the gateway via the compose service DNS name over mTLS instead.
// gatewayURLFor/gatewayBindHost are pure functions (no Server/listener
// state) so the routing decision itself is testable without a live TLS
// listener.

func TestGatewayBindHost_Userns_ReturnsLoopback(t *testing.T) {
	if got, want := gatewayBindHost(false), "127.0.0.1"; got != want {
		t.Errorf("gatewayBindHost(false) = %q, want %q (byte-for-byte unchanged from the pre-PR7 literal)", got, want)
	}
}

func TestGatewayBindHost_Container_ReturnsAllInterfaces(t *testing.T) {
	if got, want := gatewayBindHost(true), composeBindHost; got != want {
		t.Errorf("gatewayBindHost(true) = %q, want %q (composeBindHost — a sibling job container cannot reach a loopback-bound listener)", got, want)
	}
}

func TestGatewayURLFor_Userns_ReturnsLoopbackProjection(t *testing.T) {
	got := gatewayURLFor(false, 4242, 9999)
	want := "http://10.0.2.2:4242"
	if got != want {
		t.Errorf("gatewayURLFor(false, 4242, 9999) = %q, want %q (plainPort, BackendUserns — byte-for-byte the pre-PR7 literal)", got, want)
	}
}

func TestGatewayURLFor_Container_ReturnsComposeServiceURL(t *testing.T) {
	got := gatewayURLFor(true, 4242, 9999)
	want := "https://boid-gateway:9999"
	if got != want {
		t.Errorf("gatewayURLFor(true, 4242, 9999) = %q, want %q (tlsPort, BackendContainer, compose service DNS name)", got, want)
	}
}
