package auth

import (
	"net"
	"net/http"
)

// IsLoopback reports whether the request came from a loopback address.
// UNIX domain socket connections (RemoteAddr == "" or "@") are also considered
// loopback since they are only reachable from the local machine.
// X-Forwarded-For is intentionally ignored to prevent spoofing.
func IsLoopback(r *http.Request) bool {
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
