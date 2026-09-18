package apigateway

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type redirectRoundTripFunc func(*http.Request) (*http.Response, error)

func (f redirectRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func redirectResponse(r *http.Request, status int, location, body string) *http.Response {
	h := make(http.Header)
	if location != "" {
		h.Set("Location", location)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: r, ContentLength: int64(len(body))}
}

func TestServer_Redirect(t *testing.T) {
	registry := NewRegistry()
	token := registry.Register([]string{"github"}, "ws", "task", true)
	creds := NewCredentialProvider([]ServiceConfig{{Name: "github", Redirects: testRedirectPolicy(), BaseURL: "https://api.github.com", Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "pat"}}}, stubResolver(map[string]string{"ws/pat": "secret"}))
	rec := &recordingRecorder{}
	srv := NewServer(registry, creds, nil, rec.record)
	srv.proxy.Transport = redirectRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatal("missing API credential")
		}
		return redirectResponse(r, 302, "https://logs.blob.core.windows.net/log.zip?sig=signed", ""), nil
	})
	calls := 0
	srv.redirectTransport = redirectRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if len(r.Header) != 0 || r.Method != "GET" {
			t.Fatalf("download inherited request data: %v", r.Header)
		}
		if calls == 1 {
			if r.URL.RawQuery != "sig=signed" {
				t.Fatal("signed query changed")
			}
			return redirectResponse(r, 307, "https://other.blob.core.windows.net/log.zip?sig=second", ""), nil
		}
		resp := redirectResponse(r, 200, "", "log contents")
		resp.Header.Set("Connection", "X-Hop-Header")
		resp.Header.Set("X-Hop-Header", "private")
		resp.Header.Set("Set-Cookie", "session=secret")
		return resp, nil
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/"+token+"/github/repos/org/repo/actions/runs/123/logs", nil)
	req.Header.Set("X-Private-Key", "caller-secret")
	req.Header.Set("Cookie", "secret=cookie")
	srv.ServeHTTP(w, req)
	if w.Code != 200 || w.Body.String() != "log contents" || calls != 2 {
		t.Fatalf("response %d %q; calls %d", w.Code, w.Body.String(), calls)
	}
	if w.Header().Get("Connection") != "" || w.Header().Get("X-Hop-Header") != "" || w.Header().Get("Set-Cookie") != "" {
		t.Fatal("download response leaked hop headers/cookies")
	}
	if rec.count() != 1 || rec.last().status != 200 || rec.last().taskID != "task" {
		t.Fatalf("record: %+v", rec.last())
	}
}

func testRedirectPolicy() RedirectPolicy {
	return RedirectPolicy{AllowedHosts: []string{"*.blob.core.windows.net", "*.actions.githubusercontent.com"}}
}

func redirectTestServer() *Server {
	return &Server{credentials: NewCredentialProvider([]ServiceConfig{{Name: "svc", BaseURL: "https://api.example.com", Redirects: testRedirectPolicy()}}, nil)}
}

func redirectTestRequest() *http.Request {
	r, _ := http.NewRequest("GET", "https://api.example.com/files/1", nil)
	return r.WithContext(context.WithValue(r.Context(), routeInfoKey{}, routeInfo{service: "svc"}))
}

func TestRedirectDefaultAndMethodScope(t *testing.T) {
	for _, method := range []string{"GET", "HEAD", "POST", "PUT", "DELETE"} {
		for _, enabled := range []bool{true, false} {
			srv := redirectTestServer()
			if !enabled {
				srv.credentials.services["svc"] = resolvedService{}
			}
			calls := 0
			srv.redirectTransport = redirectRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != method {
					t.Fatalf("method changed to %s", r.Method)
				}
				return redirectResponse(r, 200, "", "ok"), nil
			})
			r := redirectTestRequest()
			r.Method = method
			resp := redirectResponse(r, 302, "https://logs.blob.core.windows.net/file", "")
			if err := srv.followRedirect(resp); err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			want := 0
			if enabled && (method == "GET" || method == "HEAD") {
				want = 1
			}
			if calls != want {
				t.Fatalf("method %s enabled %v calls %d", method, enabled, calls)
			}
			if want == 0 && resp.StatusCode != 302 {
				t.Fatal("changed passthrough response")
			}
		}
	}
}

func TestRedirectRejected(t *testing.T) {
	for _, target := range []string{
		"https://evil.example/log?sig=secret", "http://logs.blob.core.windows.net/log",
		"https://logs.blob.core.windows.net.evil.example/log", "https://user:pass@logs.blob.core.windows.net/log",
		"https://logs.blob.core.windows.net:8443/log", "https://127.0.0.1/log", "https://[::1]/log",
	} {
		t.Run(target, func(t *testing.T) {
			r := redirectTestRequest()
			srv := redirectTestServer()
			srv.redirectTransport = redirectRoundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("contacted rejected target"); return nil, nil })
			err := srv.followRedirect(redirectResponse(r, 302, target, ""))
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("error: %v", err)
			}
		})
	}
}

func TestRedirectLoopAndSecondHopValidation(t *testing.T) {
	for _, target := range []string{"https://logs.blob.core.windows.net/log", "https://evil.example/log"} {
		r := redirectTestRequest()
		calls := 0
		srv := redirectTestServer()
		srv.redirectTransport = redirectRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return redirectResponse(r, 302, target, ""), nil
		})
		if err := srv.followRedirect(redirectResponse(r, 302, "https://logs.blob.core.windows.net/log", "")); err == nil {
			t.Fatal("accepted loop or unsafe second hop")
		}
		want := 3
		if strings.Contains(target, "evil") {
			want = 1
		}
		if calls != want {
			t.Fatalf("calls %d, want %d", calls, want)
		}
	}
}

func TestRedirectDownloadPublicAddresses(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "::1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254", "fc00::1", "fe80::1", "100.100.100.200", "::ffff:127.0.0.1", "0.0.0.0", "224.0.0.1"} {
		if publicRedirectIP(net.ParseIP(addr)) {
			t.Errorf("accepted %s", addr)
		}
	}
	if !publicRedirectIP(net.ParseIP("20.1.2.3")) {
		t.Fatal("rejected public IP")
	}
	for _, raw := range []string{"https://logs.blob.core.windows.net/log?sig=x", "https://results-receiver.actions.githubusercontent.com/log"} {
		u, _ := url.Parse(raw)
		if !testRedirectPolicy().allows(u) {
			t.Errorf("rejected %s", raw)
		}
	}
}

func TestRedirectDownloadBodyLimit(t *testing.T) {
	canceled := false
	b := &redirectBody{ReadCloser: io.NopCloser(strings.NewReader("12345")), remaining: 4, cancel: func() { canceled = true }}
	data, err := io.ReadAll(b)
	if err == nil || string(data) != "1234" {
		t.Fatalf("read %q, %v", data, err)
	}
	b.Close()
	if !canceled {
		t.Fatal("close did not cancel")
	}
}

func TestRedirectPolicyPatterns(t *testing.T) {
	for _, pattern := range []string{"*", "*.com/evil", "https://example.com", "example.com:443", "127.0.0.1", "*.127.0.0.1", "[::1]", " example.com", "example.com.", "example..com", "foo.*.com", "-foo.example", "foo_.example"} {
		if err := (RedirectPolicy{AllowedHosts: []string{pattern}}).Validate(); err == nil {
			t.Errorf("accepted %q", pattern)
		}
	}
	p := RedirectPolicy{AllowedHosts: []string{"cdn.example.com", "*.storage.example.com"}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"cdn.example.com", true}, {"CDN.EXAMPLE.COM", true}, {"child.cdn.example.com", false},
		{"storage.example.com", false}, {"a.storage.example.com", true}, {"a.b.storage.example.com", true},
		{"storage.example.com.evil.example", false},
	} {
		u, _ := url.Parse("https://" + tc.host + "/file")
		if p.allows(u) != tc.want {
			t.Errorf("host %s", tc.host)
		}
	}
}

func TestRedirectStatusAndRelativeLocation(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		srv := redirectTestServer()
		srv.credentials.services["svc"] = resolvedService{redirects: RedirectPolicy{AllowedHosts: []string{"api.example.com"}}}
		srv.redirectTransport = redirectRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() != "https://api.example.com/download?sig=new" || r.URL.Query().Has("api_key") || len(r.Header) != 0 {
				t.Fatalf("redirect request: %s %v", r.URL, r.Header)
			}
			return redirectResponse(r, 404, "", "expired"), nil
		})
		r := redirectTestRequest()
		r.URL.RawQuery = "api_key=secret"
		r.Header.Set("X-Api-Key", "secret")
		resp := redirectResponse(r, status, "/download?sig=new", "")
		if err := srv.followRedirect(resp); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatalf("target status lost: %d", resp.StatusCode)
		}
	}
}

func TestRedirectTransportRejectsInternalIP(t *testing.T) {
	for _, address := range []string{"127.0.0.1:443", "[::1]:443", "169.254.169.254:443"} {
		conn, err := dialRedirect(context.Background(), "tcp", address)
		if err == nil {
			conn.Close()
			t.Fatalf("accepted %s", address)
		}
	}
}

func TestRedirectResponseSizeAndCancellation(t *testing.T) {
	srv := redirectTestServer()
	var download *http.Request
	srv.redirectTransport = redirectRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		download = r
		resp := redirectResponse(r, 200, "", "")
		resp.ContentLength = maxRedirectBytes + 1
		return resp, nil
	})
	r := redirectTestRequest()
	if err := srv.followRedirect(redirectResponse(r, 302, "https://logs.blob.core.windows.net/file", "")); err == nil {
		t.Fatal("accepted oversized body")
	}
	if download.Context().Err() == nil {
		t.Fatal("download not canceled")
	}
}
