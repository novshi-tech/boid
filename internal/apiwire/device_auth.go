package apiwire

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// NormalizePublicURL parses raw as an HTTPS origin, strips path/query/fragment,
// lowercases and IPv6-brackets the host, and returns the canonical
// `https://host[:port]` form. It exists so wire.go can validate
// cfg.Web.PublicURL once at daemon startup and hand the canonical form to
// DeviceAuthHandler.PublicURL — the value that ends up in the POST
// /api/auth/device response, which the CLI (PR2) will byte-compare against
// its saved profile URL on every subsequent request. An un-normalized
// value (path, trailing slash, `HOST.EXAMPLE.COM`, plain http, empty host
// like `https://:443`) would either fail the CLI's origin-bind check
// later or, worse, get accepted and silently misroute future requests.
//
// This is also the exact same normalization applied to the request Host
// header fallback inside DeviceAuthHandler.canonicalURL — see the code
// path there for the "fallback must not bypass this validator" rule.
//
// Empty raw is not an error (PublicURL is optional at boot — the handler
// falls back to the request's Host header). A non-empty raw that fails
// validation is returned as an error so the operator sees the misconfig
// at startup instead of at the first pair attempt.
func NormalizePublicURL(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	// docs/plans/cli-remote-connection.md 決定事項 4 pins the whole remote
	// device-auth flow to https:// — plain http:// is unsupported (localhost
	// debug takes the unix socket instead), so a non-https public_url is a
	// misconfig, not a variant to accept.
	if u.Scheme != "https" {
		return "", fmt.Errorf("scheme %q, want https", u.Scheme)
	}
	if u.User != nil {
		return "", fmt.Errorf("userinfo not allowed in public_url")
	}
	// url.URL.Hostname/Port cannot be used to split u.Host: they treat
	// the LAST colon as the port separator, which mangles bracket-less
	// IPv6 literals like `2001:db8::1` (→ hostname="2001:db8:" port="1")
	// and multi-colon garbage like `example.com:80:443`
	// (→ hostname="example.com:80" port="443"). splitAuthority is the
	// strict version — it rejects both classes outright instead of
	// silently reshaping them into a bogus canonical origin.
	hostname, port, err := splitAuthority(u.Host)
	if err != nil {
		return "", err
	}
	hostname = strings.ToLower(hostname)
	if hostname == "" {
		// Catches "https://" alone, "https://:443", "https:///path", etc.
		return "", fmt.Errorf("missing host")
	}
	// A bracketed authority (`[...]`) in the original URL is the RFC 3986
	// form reserved for IPv6 literals. If the hostname pulled out of
	// splitAuthority is NOT an IPv6 literal (i.e. contains no colon),
	// the input was `[some-dns-name]` — a form Go 1.24's url.Parse
	// silently accepts as a bare DNS host but Go 1.25 rejects at parse
	// time. Reject it here so the CI (Go 1.24) and local (Go 1.25)
	// behave the same, and the canonical origin never contains a
	// bogusly-bracketed DNS name.
	if strings.HasPrefix(u.Host, "[") && !strings.ContainsRune(hostname, ':') {
		return "", fmt.Errorf("bracketed authority %q is not an IPv6 literal", u.Host)
	}
	// url.Parse only rejects a non-digit port; a numeric out-of-range
	// port like "65536" or "0" passes through as a string, and
	// splitAuthority propagates it verbatim. Reject those here so the
	// canonical origin cannot promise a TCP endpoint that could never
	// exist.
	if port != "" {
		n, perr := strconv.Atoi(port)
		if perr != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("port %q out of range 1..65535", port)
		}
	}
	// A hostname that survived splitAuthority yet still contains a colon
	// can only be an IPv6 literal (there is no other legal form). Validate
	// it as one before re-bracketing — a malformed literal must not slip
	// through as a canonical origin.
	if strings.ContainsRune(hostname, ':') {
		ip := net.ParseIP(hostname)
		if ip == nil || ip.To4() != nil {
			return "", fmt.Errorf("host %q is not a valid IPv6 literal", hostname)
		}
		hostname = "[" + hostname + "]"
	}
	hostport := hostname
	if port != "" {
		hostport += ":" + port
	}
	normalized := &url.URL{Scheme: "https", Host: hostport}
	return normalized.String(), nil
}

// splitAuthority splits an HTTP authority (`host`, `host:port`, `[ipv6]`,
// `[ipv6]:port`) into hostname and port. It reports an error for
// ambiguous forms that url.URL silently mangles — bracket-less IPv6
// (`2001:db8::1`) and multi-colon garbage (`example.com:80:443`). Port
// is returned as-is (no numeric validation — url.Parse already rejected
// non-digit ports upstream; range validation happens in the caller).
func splitAuthority(authority string) (host, port string, err error) {
	if h, p, e := net.SplitHostPort(authority); e == nil {
		return h, p, nil
	}
	// SplitHostPort failed — decide whether the input is a legal
	// port-less form or genuine malformed garbage.
	if strings.HasPrefix(authority, "[") && strings.HasSuffix(authority, "]") {
		// Bracketed IPv6 with no port, e.g. "[::1]".
		return authority[1 : len(authority)-1], "", nil
	}
	if strings.ContainsRune(authority, ':') {
		// Unbracketed colons + no successful split = ambiguous.
		return "", "", fmt.Errorf("authority %q: ambiguous host:port", authority)
	}
	// Plain DNS name with no port.
	return authority, "", nil
}
