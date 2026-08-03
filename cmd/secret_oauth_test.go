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
