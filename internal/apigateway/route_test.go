package apigateway

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestParsePath_Valid(t *testing.T) {
	cases := []struct {
		name        string
		path        string
		wantToken   string
		wantService string
		wantAccount string
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
		// --- account qualifier (docs/plans/api-gateway-credential-accounts.md D1/D2) ---
		{
			name:        "service with account qualifier",
			path:        "/api/tok123/freee@ubs/api/1/deals",
			wantToken:   "tok123",
			wantService: "freee",
			wantAccount: "ubs",
			wantPath:    "/api/1/deals",
		},
		{
			name:        "account qualifier with digits, hyphen and underscore",
			path:        "/api/tok123/freee@ubs-2_test/v1",
			wantToken:   "tok123",
			wantService: "freee",
			wantAccount: "ubs-2_test",
			wantPath:    "/v1",
		},
		{
			name:        "account qualifier on root path (no tail)",
			path:        "/api/tok123/freee@ubs",
			wantToken:   "tok123",
			wantService: "freee",
			wantAccount: "ubs",
			wantPath:    "/",
		},
		{
			// "@" is a pchar (RFC 3986) so it does not need percent-encoding
			// on the wire (D1) — this pins that an operator who percent-
			// encodes it anyway ("%40") gets the exact same split. Both
			// forms become the identical string "freee@ubs" once
			// url.PathUnescape runs on the segment, and splitServiceAccount
			// operates on that already-unescaped string — see its own doc
			// comment for why there's no reason to treat the two forms
			// differently here (unlike route.path, which never unescapes at
			// all).
			name:        "percent-encoded %40 splits the same as a literal @",
			path:        "/api/tok123/freee%40ubs/v1",
			wantToken:   "tok123",
			wantService: "freee",
			wantAccount: "ubs",
			wantPath:    "/v1",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt, err := parsePath(c.path)
			if err != nil {
				t.Fatalf("parsePath(%q): unexpected error: %v", c.path, err)
			}
			if rt.token != c.wantToken || rt.service != c.wantService || rt.account != c.wantAccount || rt.path != c.wantPath {
				t.Errorf("parsePath(%q) = {token:%q service:%q account:%q path:%q}, want {token:%q service:%q account:%q path:%q}",
					c.path, rt.token, rt.service, rt.account, rt.path, c.wantToken, c.wantService, c.wantAccount, c.wantPath)
			}
		})
	}
}

// TestParsePath_InvalidAccount pins D11's account-name validation and the
// malformed-service-segment shapes ("@" appearing more than once, nothing
// before/after it) — every case here must be classified as errInvalidAccount
// (Server.ServeHTTP maps this to 400), unlike every other parsePath failure
// (404) — see TestParsePath_Invalid and errInvalidAccount's own doc comment.
func TestParsePath_InvalidAccount(t *testing.T) {
	cases := []string{
		"/api/tok123/freee@/v1",                                // empty account after "@"
		"/api/tok123/@ubs/v1",                                  // empty base service name before "@"
		"/api/tok123/freee@ubs@nvt/v1",                         // two "@" in the segment
		"/api/tok123/freee@ub.s/v1",                            // "." not in [A-Za-z0-9_-]
		"/api/tok123/freee@ub%2Fs/v1",                          // percent-encoded "/" inside the account: decodes to "ub/s", "/" not allowed
		"/api/tok123/freee@ub:s/v1",                            // ":" not in [A-Za-z0-9_-]
		"/api/tok123/freee@" + strings.Repeat("a", 65) + "/v1", // 65 chars, over the 64 limit
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			_, err := parsePath(p)
			if err == nil {
				t.Fatalf("parsePath(%q): want error, got nil", p)
			}
			if !errors.Is(err, errInvalidAccount) {
				t.Errorf("parsePath(%q) error = %v, want it to wrap errInvalidAccount", p, err)
			}
		})
	}
}

// TestParsePath_AccountExactly64CharsAccepted pins D11's boundary: exactly
// 64 characters is still valid (the limit is "at most 64", not "fewer than
// 64").
func TestParsePath_AccountExactly64CharsAccepted(t *testing.T) {
	account := strings.Repeat("a", 64)
	rt, err := parsePath("/api/tok123/freee@" + account + "/v1")
	if err != nil {
		t.Fatalf("parsePath: unexpected error for a 64-character account: %v", err)
	}
	if rt.account != account {
		t.Errorf("account = %q, want the 64-character name", rt.account)
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
