package client

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// --- NewClient scheme dispatch (docs/plans/cli-remote-connection.md
// "NewClient の scheme 分岐") ---

func TestNewClient_UnixScheme(t *testing.T) {
	c, err := NewClient("unix:///run/user/1000/boid.sock", "ignored-token")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if !c.IsUnix() {
		t.Error("expected IsUnix() == true for a unix:// url")
	}
	if c.socketPath != "/run/user/1000/boid.sock" {
		t.Errorf("socketPath = %q, want %q", c.socketPath, "/run/user/1000/boid.sock")
	}
}

func TestNewClient_UnixScheme_TwoSlashTolerated(t *testing.T) {
	// A caller that typed "unix://foo/bar.sock" (two slashes, not three)
	// lands "foo" in Host and "/bar.sock" in Path under url.Parse;
	// unixSocketPathFromURL reassembles them instead of silently truncating
	// to "/bar.sock".
	c, err := NewClient("unix://relative/boid.sock", "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.socketPath != "relative/boid.sock" {
		t.Errorf("socketPath = %q, want %q", c.socketPath, "relative/boid.sock")
	}
}

func TestNewClient_UnixScheme_EmptyPath_Rejected(t *testing.T) {
	// A "unix://" URL with no path silently produced a Client whose
	// socketPath was "" and whose IsUnix() returned false — that meant
	// autostart's "skip if https" branch would apply, and the ensuing
	// net.Dial("unix", "") would only surface a confusing lower-level
	// error at first request time. Reject it at construction.
	for _, in := range []string{"unix://", "unix:///"} {
		if _, err := NewClient(in, ""); err == nil {
			t.Errorf("NewClient(%q) = nil error; want missing-socket-path rejection", in)
		}
	}
}

func TestClient_SocketPath_Unix(t *testing.T) {
	c, err := NewClient("unix:///tmp/probe.sock", "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if got := c.SocketPath(); got != "/tmp/probe.sock" {
		t.Errorf("SocketPath() = %q, want %q", got, "/tmp/probe.sock")
	}
}

func TestClient_SocketPath_HTTPS_Empty(t *testing.T) {
	c, err := NewClient("https://x.example", "tok")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if got := c.SocketPath(); got != "" {
		t.Errorf("SocketPath() = %q, want \"\" for https profile", got)
	}
}

func TestClient_ProbeAlive_UnixDead(t *testing.T) {
	c, err := NewClient("unix:///does/not/exist.sock", "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.ProbeAlive(50 * time.Millisecond) {
		t.Error("ProbeAlive on a missing unix socket returned true, want false")
	}
}

func TestClient_ProbeDialAddress(t *testing.T) {
	// Direct assertion on the address probeDialAddress builds — the
	// pre-fix `strings.Contains(u.Host, ":")` branch would return
	// "[::1]" (bracketed but portless) for the IPv6-no-port case,
	// which then fails at the dialer with a misleading error. Splitting
	// address construction from the live dial lets this test check the
	// exact string net.DialTimeout would receive.
	cases := []struct {
		name    string
		url     string
		want    string
		wantErr bool
	}{
		{"host with explicit port", "https://work.example.com:8443", "work.example.com:8443", false},
		{"host without port defaults to 443", "https://work.example.com", "work.example.com:443", false},
		{"IPv6 with explicit port keeps brackets", "https://[::1]:8443", "[::1]:8443", false},
		{"IPv6 without port gets bracket + default 443", "https://[::1]", "[::1]:443", false},
		{"IPv6 with default zone port", "https://[fe80::1]:9000", "[fe80::1]:9000", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewClient(tc.url, "tok")
			if err != nil {
				t.Fatalf("NewClient(%q): %v", tc.url, err)
			}
			got, ok := c.probeDialAddress()
			if !ok {
				t.Fatalf("probeDialAddress: not ok for %q", tc.url)
			}
			if got != tc.want {
				t.Errorf("probeDialAddress(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

func TestClient_ProbeAlive_HTTPSIPv6NoPort_UsesDefault443(t *testing.T) {
	// End-to-end regression that ProbeAlive on `https://[::1]` returns
	// false without misbehaving on the mangled-address path. Address
	// construction correctness is directly asserted in
	// TestClient_ProbeDialAddress above.
	c, err := NewClient("https://[::1]", "tok")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.ProbeAlive(200 * time.Millisecond) {
		t.Error("ProbeAlive on an unused loopback IPv6 returned true, want false")
	}
}

func TestClient_ProbeAlive_HTTPSUnreachable(t *testing.T) {
	// 0 port on loopback: nothing is listening, TCP connect must fail.
	// Bounded timeout guarantees the test itself does not hang if the
	// probe logic ever regresses.
	c, err := NewClient("https://127.0.0.1:0", "tok")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.ProbeAlive(200 * time.Millisecond) {
		t.Error("ProbeAlive to a non-listening https origin returned true, want false")
	}
}

// TestNewClient_HTTPScheme_NonLoopback_Rejected pins decision 4's original
// verdict for anything that ISN'T loopback: a plain-HTTP remote daemon
// stays unsupported (would send BOID_CLI_TOKEN/any Bearer token in
// cleartext over the network). Host mode's own loopback exception (see the
// next test) does not widen this.
func TestNewClient_HTTPScheme_NonLoopback_Rejected(t *testing.T) {
	_, err := NewClient("http://example.com", "tok")
	if err == nil {
		t.Fatal("expected an error for a non-loopback http:// scheme")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Errorf("error should mention 'loopback', got %q", err.Error())
	}
}

// TestNewClient_HTTPScheme_Loopback_Accepted pins the PR-3 Option 4
// host-mode redesign (docs/plans/volume-only-daemon.md §論点c): an
// http://127.0.0.1[:port] (or ::1/localhost) url is now a supported
// scheme — cmd/host.go's own dispatch client for the container-backend
// daemon's dedicated CLI TCP listener.
func TestNewClient_HTTPScheme_Loopback_Accepted(t *testing.T) {
	for _, host := range []string{"127.0.0.1:8442", "localhost:8442", "[::1]:8442"} {
		c, err := NewClient("http://"+host, "tok")
		if err != nil {
			t.Fatalf("NewClient(http://%s): %v", host, err)
		}
		if c.IsUnix() {
			t.Errorf("http://%s: expected IsUnix() == false", host)
		}
	}
}

func TestNewClient_HTTPScheme_MissingHost_Rejected(t *testing.T) {
	_, err := NewClient("http://", "tok")
	if err == nil {
		t.Fatal("expected an error for an http:// url with no host")
	}
}

func TestNewClient_UnknownScheme_Rejected(t *testing.T) {
	_, err := NewClient("ftp://example.com", "tok")
	if err == nil {
		t.Fatal("expected an error for an unrecognized scheme")
	}
}

func TestNewClient_InvalidURL_Rejected(t *testing.T) {
	_, err := NewClient("://not a url", "tok")
	if err == nil {
		t.Fatal("expected an error for an unparseable url")
	}
}

func TestNewClient_HTTPSScheme_IsNotUnix(t *testing.T) {
	c, err := NewClient("https://work.example.com", "tok")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.IsUnix() {
		t.Error("expected IsUnix() == false for an https:// url")
	}
	if c.baseURL != "https://work.example.com" {
		t.Errorf("baseURL = %q, want %q", c.baseURL, "https://work.example.com")
	}
}

func TestNewClient_HTTPSScheme_MissingHost_Rejected(t *testing.T) {
	_, err := NewClient("https://", "tok")
	if err == nil {
		t.Fatal("expected an error for an https:// url with no host")
	}
}

// --- newHTTPClient (PR-3 Option 4 host-mode redesign, docs/plans/
// volume-only-daemon.md §論点c) — mirrors the https Bearer-header test
// below, minus TLS. httptest.NewServer's URL is always loopback
// ("127.0.0.1:<port>"), so newHTTPClient's own loopback-host check passes
// unremarkably here. ---

func TestHTTPClient_SendsAuthorizationBearerHeader(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	c, err := newHTTPClient(u, "cli_tok_abc", nil)
	if err != nil {
		t.Fatalf("newHTTPClient: %v", err)
	}
	if err := c.Do("GET", "/api/tasks", nil, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotAuth != "Bearer cli_tok_abc" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer cli_tok_abc")
	}
}

// --- Authorization: Bearer header injection + cross-origin redirect
// (docs/plans/cli-remote-connection.md 決定事項 7/9) ---

func newTestTLSHTTPSClient(t *testing.T, handler http.Handler, token string) (*Client, *httptest.Server) {
	t.Helper()
	ts := httptest.NewTLSServer(handler)
	t.Cleanup(ts.Close)
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	c, err := newHTTPSClient(u, token, ts.Client().Transport)
	if err != nil {
		t.Fatalf("newHTTPSClient: %v", err)
	}
	return c, ts
}

func TestHTTPSClient_SendsAuthorizationBearerHeader(t *testing.T) {
	var gotAuth string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	})
	c, _ := newTestTLSHTTPSClient(t, handler, "tk_abc123")

	if err := c.Do("GET", "/api/tasks", nil, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotAuth != "Bearer tk_abc123" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer tk_abc123")
	}
}

func TestHTTPSClient_EmptyToken_NoAuthorizationHeader(t *testing.T) {
	var gotAuth string
	sawRequest := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRequest = true
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	})
	c, _ := newTestTLSHTTPSClient(t, handler, "")

	if err := c.Do("GET", "/api/tasks", nil, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !sawRequest {
		t.Fatal("handler never saw the request")
	}
	if gotAuth != "" {
		t.Errorf("Authorization header = %q, want empty (no token configured)", gotAuth)
	}
}

func TestHTTPSClient_SameOriginRedirect_KeepsAuthHeader(t *testing.T) {
	var finalAuth string
	var redirected bool
	mux := http.NewServeMux()
	mux.HandleFunc("/old", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/new", http.StatusFound)
	})
	mux.HandleFunc("/new", func(w http.ResponseWriter, r *http.Request) {
		redirected = true
		finalAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	})
	c, _ := newTestTLSHTTPSClient(t, mux, "tk_same_origin")

	if err := c.Do("GET", "/old", nil, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !redirected {
		t.Fatal("expected the same-origin redirect to be followed")
	}
	if finalAuth != "Bearer tk_same_origin" {
		t.Errorf("Authorization header after same-origin redirect = %q, want %q", finalAuth, "Bearer tk_same_origin")
	}
}

func TestHTTPSClient_CrossOriginRedirect_Rejected(t *testing.T) {
	// evilTS is the redirect *target* — a different origin than c is
	// configured against. If cross-origin redirects were ever followed, this
	// handler would observe the Bearer token; it must never be reached with
	// the token.
	var evilSawAuth string
	evilSawRequest := false
	evilTS := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		evilSawRequest = true
		evilSawAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(evilTS.Close)

	mainTS := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evilTS.URL+"/steal", http.StatusFound)
	}))
	t.Cleanup(mainTS.Close)

	u, err := url.Parse(mainTS.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	c, err := newHTTPSClient(u, "tk_cross_origin", mainTS.Client().Transport)
	if err != nil {
		t.Fatalf("newHTTPSClient: %v", err)
	}

	err = c.Do("GET", "/redirect-me", nil, nil)
	if err == nil {
		t.Fatal("expected an error for a cross-origin redirect, got nil")
	}
	if !strings.Contains(err.Error(), "cross-origin") {
		t.Errorf("error should mention cross-origin rejection, got %q", err.Error())
	}
	if evilSawRequest {
		t.Errorf("the cross-origin target must never have been reached at all (Authorization header %q would have leaked)", evilSawAuth)
	}
}

// --- DefaultCLIAddr (PR-3 Option 4 host-mode redesign, docs/plans/
// volume-only-daemon.md §論点c) ---

func TestDefaultCLIAddr_NoOverride(t *testing.T) {
	t.Setenv("BOID_CLI_ADDR", "")
	if got := DefaultCLIAddr(); got != "127.0.0.1:8442" {
		t.Errorf("DefaultCLIAddr() = %q, want %q", got, "127.0.0.1:8442")
	}
}

func TestDefaultCLIAddr_PortOverride(t *testing.T) {
	t.Setenv("BOID_CLI_ADDR", "0.0.0.0:19999")
	if got := DefaultCLIAddr(); got != "127.0.0.1:19999" {
		t.Errorf("DefaultCLIAddr() = %q, want %q (host override ignored, port honored)", got, "127.0.0.1:19999")
	}
}

func TestDefaultCLIAddr_MalformedOverride_Ignored(t *testing.T) {
	t.Setenv("BOID_CLI_ADDR", "not-a-host-port")
	if got := DefaultCLIAddr(); got != "127.0.0.1:8442" {
		t.Errorf("DefaultCLIAddr() = %q, want the default %q for a malformed override", got, "127.0.0.1:8442")
	}
}
