package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/qrterm"
)

// setupSecretOAuthLoginCmd points the package-level secretOAuthLoginCmd at
// srv, sets namespace/timeout flags to test-friendly values, and restores
// everything (context, output sinks, flags) on cleanup — the same
// discipline runJobLogAgainst (job_test.go) and newInitScriptCmd
// (workspace_init_script_test.go) already document for these package-level
// cobra singletons.
func setupSecretOAuthLoginCmd(t *testing.T, srv *httptest.Server, timeout time.Duration, stdin string) *bytes.Buffer {
	t.Helper()
	c, err := client.NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("build client for %s: %v", srv.URL, err)
	}

	cmd := secretOAuthLoginCmd
	prevCtx := cmd.Context()
	prevNamespace, _ := cmd.Flags().GetString("namespace")
	prevTimeout, _ := cmd.Flags().GetDuration("timeout")
	t.Cleanup(func() {
		cmd.SetContext(prevCtx)
		cmd.SetOut(nil)
		cmd.SetErr(nil)
		cmd.SetIn(nil)
		_ = cmd.Flags().Set("namespace", prevNamespace)
		cmd.Flags().Set("timeout", prevTimeout.String())
	})

	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetContext(client.WithClient(context.Background(), c))
	if err := cmd.Flags().Set("namespace", "ws-a"); err != nil {
		t.Fatalf("set namespace flag: %v", err)
	}
	if err := cmd.Flags().Set("timeout", timeout.String()); err != nil {
		t.Fatalf("set timeout flag: %v", err)
	}
	return out
}

func writeJSONResponse(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func TestSecretOAuthLogin_ManualFlow_Success(t *testing.T) {
	var gotNamespace, gotService string
	var gotCompleteCode, gotCompleteState string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/oauth/login":
			var req oauthLoginStartRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			gotNamespace, gotService = req.Namespace, req.Service
			writeJSONResponse(t, w, oauthLoginStartResponse{
				SessionID: "sess-1", Flow: "manual", AuthorizeURL: "https://accounts.secure.freee.co.jp/public_api/authorize",
			})
		case r.Method == "POST" && r.URL.Path == "/api/oauth/login/sess-1/complete":
			var req oauthLoginCompleteRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			gotCompleteCode, gotCompleteState = req.Code, req.State
			writeJSONResponse(t, w, map[string]string{"status": "complete"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	out := setupSecretOAuthLoginCmd(t, srv, time.Second, "OOB-CODE\n")
	if err := runSecretOAuthLogin(secretOAuthLoginCmd, []string{"freee"}); err != nil {
		t.Fatalf("runSecretOAuthLogin: %v", err)
	}
	if gotNamespace != "ws-a" || gotService != "freee" {
		t.Errorf("start request = (namespace=%q, service=%q), want (ws-a, freee)", gotNamespace, gotService)
	}
	if gotCompleteCode != "OOB-CODE" {
		t.Errorf("complete code = %q, want OOB-CODE", gotCompleteCode)
	}
	if gotCompleteState != "" {
		t.Errorf("complete state = %q, want empty (manual flow has no state)", gotCompleteState)
	}
	if !strings.Contains(out.String(), "Login complete.") {
		t.Errorf("output = %q, want it to mention Login complete.", out.String())
	}
	if !strings.Contains(out.String(), "accounts.secure.freee.co.jp") {
		t.Errorf("output = %q, want it to show the authorize URL", out.String())
	}
}

// TestSecretOAuthLogin_ManualFlow_OmitsQRCode pins the rule that now holds
// for every `boid secret oauth login` flow: no terminal QR code. The person
// running the command is already sitting at the terminal the authorize URL
// is printed on, so the QR only adds screens of noise to scroll past — see
// printAuthorizeURL's doc comment. (`boid web pair` keeps its QR: there the
// whole point is enrolling a phone.)
func TestSecretOAuthLogin_ManualFlow_OmitsQRCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/oauth/login":
			writeJSONResponse(t, w, oauthLoginStartResponse{
				SessionID: "sess-1", Flow: "manual", AuthorizeURL: "https://accounts.secure.freee.co.jp/public_api/authorize",
			})
		case r.Method == "POST" && r.URL.Path == "/api/oauth/login/sess-1/complete":
			writeJSONResponse(t, w, map[string]string{"status": "complete"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	out := setupSecretOAuthLoginCmd(t, srv, time.Second, "PASTED-CODE\n")
	if err := runSecretOAuthLogin(secretOAuthLoginCmd, []string{"freee"}); err != nil {
		t.Fatalf("runSecretOAuthLogin: %v", err)
	}
	if !strings.Contains(out.String(), "https://accounts.secure.freee.co.jp/public_api/authorize") {
		t.Errorf("output = %q, want it to show the authorize URL", out.String())
	}
	qr, err := qrterm.Encode("https://accounts.secure.freee.co.jp/public_api/authorize", false)
	if err != nil {
		t.Fatalf("qrterm.Encode: %v", err)
	}
	if strings.Contains(out.String(), qr) {
		t.Error("manual flow printed a QR code; oauth login no longer offers one")
	}
}

func TestSecretOAuthLogin_ManualFlow_EmptyCodeRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/oauth/login" {
			writeJSONResponse(t, w, oauthLoginStartResponse{SessionID: "sess-1", Flow: "manual", AuthorizeURL: "https://example.com/authorize"})
			return
		}
		t.Errorf("unexpected request %s %s (complete should never be called)", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	setupSecretOAuthLoginCmd(t, srv, time.Second, "\n")
	if err := runSecretOAuthLogin(secretOAuthLoginCmd, []string{"freee"}); err == nil {
		t.Fatal("want error for an empty pasted code, got nil")
	}
}

func TestSecretOAuthLogin_LoopbackFlow_Success(t *testing.T) {
	var gotRedirectURI string
	var gotCompleteCode, gotCompleteState string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/oauth/login":
			var req oauthLoginStartRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			gotRedirectURI = req.RedirectURI
			writeJSONResponse(t, w, oauthLoginStartResponse{
				SessionID: "sess-1", Flow: "loopback", AuthorizeURL: "https://accounts.google.com/o/oauth2/v2/auth?state=xyz",
			})
			// Fire the simulated browser redirect once we know the actual
			// ephemeral port the CLI bound (redirect_uri is only known
			// after the CLI opened its listener, which happens before this
			// very request is sent).
			go func(redirectURI string) {
				time.Sleep(50 * time.Millisecond)
				resp, err := http.Get(redirectURI + "?code=AUTH-CODE&state=xyz")
				if err == nil {
					resp.Body.Close()
				}
			}(gotRedirectURI)
		case r.Method == "POST" && r.URL.Path == "/api/oauth/login/sess-1/complete":
			var req oauthLoginCompleteRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			gotCompleteCode, gotCompleteState = req.Code, req.State
			writeJSONResponse(t, w, map[string]string{"status": "complete"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	out := setupSecretOAuthLoginCmd(t, srv, 5*time.Second, "")
	if err := runSecretOAuthLogin(secretOAuthLoginCmd, []string{"google"}); err != nil {
		t.Fatalf("runSecretOAuthLogin: %v", err)
	}
	if gotCompleteCode != "AUTH-CODE" || gotCompleteState != "xyz" {
		t.Errorf("complete request = (code=%q, state=%q), want (AUTH-CODE, xyz)", gotCompleteCode, gotCompleteState)
	}
	if !strings.Contains(out.String(), "Login complete.") {
		t.Errorf("output = %q, want it to mention Login complete.", out.String())
	}
	// The loopback flow must NOT offer a QR code. The authorize URL
	// redirects to http://127.0.0.1:<port>, a port only THIS process is
	// listening on — scanning it sends a phone to its own localhost, where
	// nothing answers, and the resulting failure looks like the provider
	// rejecting the login. See printAuthorizeURL's doc comment.
	if qr, err := qrterm.Encode("https://accounts.google.com/o/oauth2/v2/auth?state=xyz", false); err == nil {
		if strings.Contains(out.String(), qr) {
			t.Error("loopback flow printed a QR code; the callback is only reachable on this machine")
		}
	}
}

func TestSecretOAuthLogin_LoopbackFlow_TimesOutWithoutCallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/oauth/login" {
			writeJSONResponse(t, w, oauthLoginStartResponse{SessionID: "sess-1", Flow: "loopback", AuthorizeURL: "https://accounts.google.com/o/oauth2/v2/auth"})
			return
		}
		t.Errorf("unexpected request %s %s (no callback ever arrives in this test)", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	setupSecretOAuthLoginCmd(t, srv, 150*time.Millisecond, "")
	err := runSecretOAuthLogin(secretOAuthLoginCmd, []string{"google"})
	if err == nil {
		t.Fatal("want a timeout error when the browser never calls back, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want it to mention a timeout", err.Error())
	}
}

func TestSecretOAuthLogin_DeviceFlow_Success(t *testing.T) {
	restore := overrideOAuthLoginPollSleep(t)
	defer restore()

	var pollCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/oauth/login":
			writeJSONResponse(t, w, oauthLoginStartResponse{
				SessionID: "sess-1", Flow: "device", UserCode: "USER-CODE",
				VerificationURI: "https://github.com/login/device", IntervalSeconds: 1, ExpiresInSeconds: 60,
			})
		case r.Method == "GET" && r.URL.Path == "/api/oauth/login/sess-1":
			pollCount++
			status := "pending"
			if pollCount >= 3 {
				status = "complete"
			}
			writeJSONResponse(t, w, oauthLoginStatusResponse{Status: status})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	out := setupSecretOAuthLoginCmd(t, srv, 5*time.Second, "")
	if err := runSecretOAuthLogin(secretOAuthLoginCmd, []string{"github"}); err != nil {
		t.Fatalf("runSecretOAuthLogin: %v", err)
	}
	if pollCount < 3 {
		t.Errorf("poll count = %d, want at least 3", pollCount)
	}
	if !strings.Contains(out.String(), "USER-CODE") {
		t.Errorf("output = %q, want it to show the user_code", out.String())
	}
	if !strings.Contains(out.String(), "Login complete.") {
		t.Errorf("output = %q, want it to mention Login complete.", out.String())
	}
}

// TestSecretOAuthLogin_DeviceFlow_OmitsQRCode is the device-flow half of
// TestSecretOAuthLogin_ManualFlow_OmitsQRCode. The device flow used to render
// verification_uri_complete as a QR — the one case where scanning it with a
// phone genuinely worked — but the rule is now uniform across the command:
// print the URL, never the QR.
func TestSecretOAuthLogin_DeviceFlow_OmitsQRCode(t *testing.T) {
	restore := overrideOAuthLoginPollSleep(t)
	defer restore()

	const complete = "https://github.com/login/device?user_code=USER-CODE"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/oauth/login":
			writeJSONResponse(t, w, oauthLoginStartResponse{
				SessionID: "sess-1", Flow: "device", UserCode: "USER-CODE",
				VerificationURI: "https://github.com/login/device", VerificationURIComplete: complete,
				IntervalSeconds: 1, ExpiresInSeconds: 60,
			})
		case r.Method == "GET" && r.URL.Path == "/api/oauth/login/sess-1":
			writeJSONResponse(t, w, oauthLoginStatusResponse{Status: "complete"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	out := setupSecretOAuthLoginCmd(t, srv, 5*time.Second, "")
	if err := runSecretOAuthLogin(secretOAuthLoginCmd, []string{"github"}); err != nil {
		t.Fatalf("runSecretOAuthLogin: %v", err)
	}
	qr, err := qrterm.Encode(complete, false)
	if err != nil {
		t.Fatalf("qrterm.Encode: %v", err)
	}
	if strings.Contains(out.String(), qr) {
		t.Error("device flow printed a QR code; oauth login no longer offers one")
	}
}

func TestSecretOAuthLogin_DeviceFlow_FailedStatusReturnsError(t *testing.T) {
	restore := overrideOAuthLoginPollSleep(t)
	defer restore()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/oauth/login":
			writeJSONResponse(t, w, oauthLoginStartResponse{
				SessionID: "sess-1", Flow: "device", UserCode: "USER-CODE",
				VerificationURI: "https://github.com/login/device", IntervalSeconds: 1, ExpiresInSeconds: 60,
			})
		case r.Method == "GET" && r.URL.Path == "/api/oauth/login/sess-1":
			writeJSONResponse(t, w, oauthLoginStatusResponse{Status: "failed", Error: "access_denied"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	setupSecretOAuthLoginCmd(t, srv, 5*time.Second, "")
	err := runSecretOAuthLogin(secretOAuthLoginCmd, []string{"github"})
	if err == nil {
		t.Fatal("want error for a failed device login, got nil")
	}
	if !strings.Contains(err.Error(), "access_denied") {
		t.Errorf("error = %q, want it to mention access_denied", err.Error())
	}
}

func TestSecretOAuthLogin_StartError_ServiceNotConfigured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		writeJSONResponse(t, w, map[string]string{"error": "service nope is not configured with auth.kind: oauth2"})
	}))
	defer srv.Close()

	setupSecretOAuthLoginCmd(t, srv, time.Second, "")
	err := runSecretOAuthLogin(secretOAuthLoginCmd, []string{"nope"})
	if err == nil {
		t.Fatal("want error for an unconfigured service, got nil")
	}
	if !strings.Contains(err.Error(), "not configured with auth.kind: oauth2") {
		t.Errorf("error = %q, want it to relay the daemon's message", err.Error())
	}
}

func TestSecretOAuthLogin_UnrecognizedFlowReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(t, w, oauthLoginStartResponse{SessionID: "sess-1", Flow: "telepathy"})
	}))
	defer srv.Close()

	setupSecretOAuthLoginCmd(t, srv, time.Second, "")
	err := runSecretOAuthLogin(secretOAuthLoginCmd, []string{"freee"})
	if err == nil {
		t.Fatal("want error for an unrecognized flow, got nil")
	}
}

// overrideOAuthLoginPollSleep replaces oauthLoginPollSleep with a fast,
// duration-independent sleep for the device-flow poll loop tests above, so
// they don't wait out a real IntervalSeconds cadence. Restored on cleanup —
// the returned func is also callable directly via `defer restore()` for
// symmetry with the rest of this file's setup helpers.
func overrideOAuthLoginPollSleep(t *testing.T) func() {
	t.Helper()
	prev := oauthLoginPollSleep
	oauthLoginPollSleep = func(time.Duration) { time.Sleep(time.Millisecond) }
	restore := func() { oauthLoginPollSleep = prev }
	t.Cleanup(restore)
	return restore
}
