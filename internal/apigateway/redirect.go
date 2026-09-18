package apigateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/novshi-tech/boid/internal/gwtransport"
)

// RedirectPolicy opts one service into credential-free GET/HEAD redirects.
// Empty AllowedHosts preserves the upstream redirect response unchanged.
type RedirectPolicy struct {
	AllowedHosts []string `yaml:"allowed_hosts,omitempty"`
}

// Validate rejects ambiguous patterns: entries are DNS names or *.DNS names,
// never URLs, IP literals, ports, or unrestricted wildcards.
func (p RedirectPolicy) Validate() error {
	for _, pattern := range p.AllowedHosts {
		host := strings.TrimPrefix(pattern, "*.")
		if len(host) > 253 || !strings.Contains(host, ".") || net.ParseIP(host) != nil {
			return fmt.Errorf("invalid redirect host pattern %q", pattern)
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return fmt.Errorf("invalid redirect host pattern %q", pattern)
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
					return fmt.Errorf("invalid redirect host pattern %q", pattern)
				}
			}
		}
	}
	return nil
}

const maxRedirectBytes int64 = 512 << 20

func (p RedirectPolicy) allows(u *url.URL) bool {
	if u.Scheme != "https" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, pattern := range p.AllowedHosts {
		pattern = strings.ToLower(pattern)
		if strings.HasPrefix(pattern, "*.") {
			if strings.HasSuffix(host, pattern[1:]) {
				return true
			}
		} else if host == pattern {
			return true
		}
	}
	return false
}

func redirectStatus(status int) bool {
	switch status {
	case 301, 302, 303, 307, 308:
		return true
	}
	return false
}

func publicRedirectIP(ip net.IP) bool {
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() &&
		!sharedRedirectNetwork.Contains(ip)
}

var sharedRedirectNetwork = func() *net.IPNet { _, n, _ := net.ParseCIDR("100.64.0.0/10"); return n }()

func newRedirectTransport() *http.Transport {
	t := gwtransport.New()
	t.TLSHandshakeTimeout = 10 * time.Second
	t.ResponseHeaderTimeout = 30 * time.Second
	t.DialContext = dialRedirect
	return t
}

// Dial the validated IP itself: a second DNS lookup must not permit rebinding
// into a workspace, localhost, or a cloud metadata endpoint. TLS still checks
// the original hostname. This transport deliberately does not use env proxies.
func dialRedirect(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("invalid redirect download address")
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("redirect download DNS lookup failed")
	}
	for _, ip := range ips {
		if !publicRedirectIP(ip.IP) {
			return nil, errors.New("non-public redirect download address")
		}
	}
	dialer := net.Dialer{Timeout: 10 * time.Second}
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return conn, nil
		}
	}
	return nil, errors.New("redirect download connection failed")
}

// Follow only locations supplied by an authenticated upstream response, never
// a caller-provided URL. Fresh requests carry no API credentials or caller
// headers, even when another redirect stays on the same download host.
func (s *Server) followRedirect(resp *http.Response) error {
	info, _ := resp.Request.Context().Value(routeInfoKey{}).(routeInfo)
	if s.credentials == nil {
		return nil
	}
	policy := s.credentials.services[info.service].redirects
	if len(policy.AllowedHosts) == 0 || !isSafeMethod(resp.Request.Method) || !redirectStatus(resp.StatusCode) {
		return nil
	}
	original := resp.Request
	ctx, cancel := context.WithTimeout(original.Context(), 2*time.Minute)
	current := resp
	for hop := 0; ; hop++ {
		target, err := current.Location()
		current.Body.Close()
		if err != nil || !policy.allows(target) || hop >= 3 {
			cancel()
			return errors.New("redirect target rejected")
		}
		req, err := http.NewRequestWithContext(ctx, original.Method, target.String(), nil)
		if err != nil {
			cancel()
			return errors.New("invalid redirect download request")
		}
		next, err := s.redirectTransport.RoundTrip(req)
		if err != nil {
			cancel()
			// Transport errors can embed the signed URL; do not return them to logging.
			return errors.New("redirect download request failed")
		}
		if redirectStatus(next.StatusCode) {
			current = next
			continue
		}
		if next.StatusCode == http.StatusSwitchingProtocols || (original.Method != http.MethodHead && next.ContentLength > maxRedirectBytes) {
			next.Body.Close()
			cancel()
			return errors.New("redirect download response rejected")
		}
		next.Body = &redirectBody{ReadCloser: next.Body, remaining: maxRedirectBytes, cancel: cancel}
		next.Request = original // Preserve gateway authorization/recording context.
		// ReverseProxy stripped hop-by-hop headers before ModifyResponse;
		// this replacement response needs the same cleanup.
		for _, value := range next.Header.Values("Connection") {
			for _, name := range strings.Split(value, ",") {
				next.Header.Del(strings.TrimSpace(name))
			}
		}
		for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
			next.Header.Del(name)
		}
		next.Header.Del("Set-Cookie")
		next.Header.Del("Location")
		*resp = *next
		return nil
	}
}

type redirectBody struct {
	io.ReadCloser
	remaining int64
	cancel    context.CancelFunc
}

func (b *redirectBody) Read(p []byte) (int, error) {
	if int64(len(p)) > b.remaining+1 {
		p = p[:b.remaining+1]
	}
	n, err := b.ReadCloser.Read(p)
	if int64(n) > b.remaining {
		n = int(b.remaining)
		b.remaining = 0
		return n, errors.New("redirect download size limit exceeded")
	}
	b.remaining -= int64(n)
	if err != nil && err != io.EOF {
		return n, errors.New("redirect download stream failed")
	}
	return n, err
}

func (b *redirectBody) Close() error { b.cancel(); return b.ReadCloser.Close() }
