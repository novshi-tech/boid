package apigateway

import (
	"net/url"
	"testing"
)

func TestParsePath_Valid(t *testing.T) {
	cases := []struct {
		name        string
		path        string
		wantToken   string
		wantService string
		wantPath    string
	}{
		{
			name:        "root path (no tail)",
			path:        "/api/tok123/myapp",
			wantToken:   "tok123",
			wantService: "myapp",
			wantPath:    "/",
		},
		{
			name:        "trailing slash only",
			path:        "/api/tok123/myapp/",
			wantToken:   "tok123",
			wantService: "myapp",
			wantPath:    "/",
		},
		{
			name:        "simple tail",
			path:        "/api/tok123/myapp/v1/users",
			wantToken:   "tok123",
			wantService: "myapp",
			wantPath:    "/v1/users",
		},
		{
			// A trailing slash on the request tail must be preserved, not
			// silently stripped: some REST APIs treat "/hooks/" and
			// "/hooks" as different routes, and this gateway is meant to
			// forward arbitrary caller-chosen paths byte-for-byte.
			name:        "tail with trailing slash is preserved",
			path:        "/api/tok123/myapp/v1/users/",
			wantToken:   "tok123",
			wantService: "myapp",
			wantPath:    "/v1/users/",
		},
		{
			name:        "tail containing dots that are not traversal",
			path:        "/api/tok123/myapp/v1/a..b",
			wantToken:   "tok123",
			wantService: "myapp",
			wantPath:    "/v1/a..b",
		},
		{
			// A "%2F"-encoded slash INSIDE a single path segment (common for
			// REST APIs whose resource keys themselves contain "/", e.g.
			// object storage / registry APIs) must survive as part of that
			// one segment, not be treated as an extra path separator.
			// parsePath is fed r.URL.EscapedPath() in real use (never
			// r.URL.Path, which net/http would have already decoded %2F
			// into a literal "/"), so this test passes the escaped form
			// directly, matching that real call site.
			name:        "percent-encoded slash within one segment is preserved",
			path:        "/api/tok123/myapp/objects/a%2Fb",
			wantToken:   "tok123",
			wantService: "myapp",
			wantPath:    "/objects/a%2Fb",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt, err := parsePath(c.path)
			if err != nil {
				t.Fatalf("parsePath(%q): unexpected error: %v", c.path, err)
			}
			if rt.token != c.wantToken || rt.service != c.wantService || rt.path != c.wantPath {
				t.Errorf("parsePath(%q) = {token:%q service:%q path:%q}, want {token:%q service:%q path:%q}",
					c.path, rt.token, rt.service, rt.path, c.wantToken, c.wantService, c.wantPath)
			}
		})
	}
}

func TestParsePath_Invalid(t *testing.T) {
	cases := []string{
		"/",
		"/api",
		"/api/",
		"/api/tok123",        // missing service segment
		"/api/tok123/",       // missing service segment
		"/j/tok123/myapp/v1", // wrong prefix (git gateway's, not ours)
		"/other/tok123/myapp",
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			if _, err := parsePath(p); err == nil {
				t.Errorf("parsePath(%q): want error, got nil", p)
			}
		})
	}
}

// TestParsePath_TraversalCannotEscapeServiceRoot pins §5 of
// docs/plans/api-gateway.md ("path 正規化 — ".." / 絶対URL混入で base_url の
// 外に出られないことを parse 層で保証する (URLエンコード済み %2e%2e 含む)"):
// a request path containing any ".." segment — literal or percent-encoded —
// is rejected OUTRIGHT (an error, never a resolved/clamped path). This is a
// deliberate design choice over gitgateway's sibling guard: resolving ".."
// (path.Clean-style) would require operating on the DECODED path, which in
// turn would silently turn a legitimate "%2F"-encoded slash inside one
// segment into an extra path separator (see TestParsePath_Valid's
// percent-encoded-slash case) — rejecting outright avoids that tension
// entirely while still making escape impossible.
func TestParsePath_TraversalCannotEscapeServiceRoot(t *testing.T) {
	cases := []string{
		"/api/tok123/myapp/..",
		"/api/tok123/myapp/../../etc/passwd",
		"/api/tok123/myapp/v1/../v2/users",
		// %2e%2e is the URL-encoded form of "..". parsePath is fed the
		// ESCAPED path (r.URL.EscapedPath() in real use), so this is
		// exercised directly here as the literal on-the-wire text, not
		// pre-decoded through url.Parse first.
		"/api/tok123/myapp/%2e%2e/%2e%2e/secret",
		// A bare single "." segment is also rejected (not itself a way to
		// escape the root, but there is no legitimate reason a caller needs
		// it, and treating it identically to ".." keeps the guard simple).
		"/api/tok123/myapp/./secret",
		// codex review round 2 finding: "%2e%2e%2fadmin" has no LITERAL "/"
		// in it, so the outer split (on literal "/") treats it as one raw
		// segment — but it decodes to "../admin", which itself contains a
		// "/" and hides a ".." sub-segment. Some upstreams decode "%2F"
		// themselves before routing, so forwarding this intact could still
		// resolve to a real traversal on the upstream side.
		"/api/tok123/myapp/%2e%2e%2fadmin",
		"/api/tok123/myapp/v1/%2e%2e%2f%2e%2e%2fadmin",
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			if _, err := parsePath(p); err == nil {
				t.Errorf("parsePath(%q): want error (traversal attempt), got nil", p)
			}
		})
	}
}

// TestParsePath_UsesEscapedPathNotDecodedPath is a sanity check that the
// exact string parsePath is documented to expect (r.URL.EscapedPath()) does
// what this package's Server.ServeHTTP relies on: for a wire path containing
// "%2e%2e", EscapedPath preserves the percent-encoding (unlike r.URL.Path,
// which net/http decodes eagerly) — parsePath's own per-segment
// url.PathUnescape check is what still catches it as a traversal attempt
// either way.
func TestParsePath_UsesEscapedPathNotDecodedPath(t *testing.T) {
	u, err := url.Parse("/api/tok123/myapp/%2e%2e/secret")
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	if u.EscapedPath() == u.Path {
		t.Fatalf("test setup invalid: EscapedPath() and Path must differ here (got both %q)", u.Path)
	}
	if _, err := parsePath(u.EscapedPath()); err == nil {
		t.Error("parsePath(EscapedPath()): want traversal error, got nil")
	}
}
