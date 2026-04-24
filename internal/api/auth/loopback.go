package auth

import (
	"net"
	"net/http"
)

// IsLoopback reports whether the request came from a loopback address.
// UNIX domain socket connections (RemoteAddr == "" or "@") are also considered
// loopback since they are only reachable from the local machine.
//
// If the request carries upstream-proxy headers (X-Forwarded-For,
// CF-Connecting-IP, Forwarded), the request is treated as NOT loopback even
// when RemoteAddr is 127.0.0.1 — because such a request arrived via a reverse
// proxy / tunnel (e.g. cloudflared forwarding to localhost:8080) and must not
// benefit from the bootstrap exemption granted to real local sessions.
// Header values themselves are not trusted (they can be spoofed); only the
// presence of the header is used as a "I came through a proxy" signal.
func IsLoopback(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-For") != "" ||
		r.Header.Get("CF-Connecting-IP") != "" ||
		r.Header.Get("Forwarded") != "" {
		return false
	}
	addr := r.RemoteAddr
	if addr == "" || addr == "@" {
		return true
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
