package apigateway

import (
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
			name:        "tail with trailing slash",
			path:        "/api/tok123/myapp/v1/users/",
			wantToken:   "tok123",
			wantService: "myapp",
			wantPath:    "/v1/users",
		},
		{
			name:        "tail containing dots that are not traversal",
			path:        "/api/tok123/myapp/v1/a..b",
			wantToken:   "tok123",
			wantService: "myapp",
			wantPath:    "/v1/a..b",
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
// a request path containing ".." anywhere in the tail must never produce an
// upstream path that climbs above the service's own root. Go's net/http
// already percent-decodes %2e%2e into a literal ".." by the time it reaches
// r.URL.Path, so this test (and the real ServeHTTP caller) only ever sees
// the decoded form — this is what proves the encoded case is covered too.
func TestParsePath_TraversalCannotEscapeServiceRoot(t *testing.T) {
	cases := []struct {
		rawPath  string // as it would appear on the wire, before net/http's own percent-decoding
		wantPath string
	}{
		{"/api/tok123/myapp/..", "/"},
		{"/api/tok123/myapp/../../etc/passwd", "/etc/passwd"},
		{"/api/tok123/myapp/v1/../v2/users", "/v2/users"},
		// %2e%2e is the URL-encoded form of "..". net/url.Parse decodes it
		// into .Path automatically (exactly what net/http does before a
		// handler ever sees r.URL.Path) — decoding here through url.Parse,
		// rather than passing the literal "%2e%2e" text straight to
		// parsePath, is what actually exercises that encoded-input path.
		{"/api/tok123/myapp/%2e%2e/%2e%2e/secret", "/secret"},
	}
	for _, c := range cases {
		t.Run(c.rawPath, func(t *testing.T) {
			u, err := url.Parse(c.rawPath)
			if err != nil {
				t.Fatalf("url.Parse(%q): %v", c.rawPath, err)
			}
			rt, err := parsePath(u.Path)
			if err != nil {
				t.Fatalf("parsePath(%q): unexpected error: %v", u.Path, err)
			}
			if rt.path != c.wantPath {
				t.Errorf("parsePath(%q).path = %q, want %q (must never escape the service root)", u.Path, rt.path, c.wantPath)
			}
			for _, seg := range strings.Split(rt.path, "/") {
				if seg == ".." {
					t.Fatalf("parsePath(%q).path = %q contains a literal .. segment — traversal guard failed", u.Path, rt.path)
				}
			}
		})
	}
}
