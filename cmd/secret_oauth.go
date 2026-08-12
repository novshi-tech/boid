package cmd

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/spf13/cobra"
)

// This file implements `boid secret oauth login <service>` (docs/plans/
// api-gateway.md §7 "初回認証", PR3): the CLI's half of the three-flow
// initial OAuth2 grant the daemon's internal/apigateway.LoginManager
// orchestrates. Per §7's role split ("CLI = ブラウザ側、daemon = Web サーバ
// 側"), this file's job is narrow: talk to the daemon's /api/oauth/login*
// endpoints, display whatever the user needs to see (an authorize URL, a
// device user_code, a QR code), and forward whatever comes back from the
// browser/device flow to the daemon — it never touches a PKCE verifier,
// client_secret, or refresh_token itself.

// oauthLoginRequestTimeout bounds each individual daemon round trip
// (POST .../login, GET .../login/{id}, POST .../login/{id}/complete) —
// distinct from --timeout below, which bounds the WHOLE interactive wait
// for the user to finish authorizing in a browser or on another device.
const oauthLoginRequestTimeout = 30 * time.Second

// oauthLoginPollSleep/oauthLoginNow are indirections over time.Sleep/
// time.Now so tests can drive the device-flow poll loop without a real
// wall-clock wait — overridden only by this file's own tests, always the
// real functions in production (mirrors internal/apigateway.OAuth2TokenSource.
// Now's identical override convention, applied here to a plain package var
// since there is no single receiver struct instance to hang a field on).
var (
	oauthLoginPollSleep = time.Sleep
	oauthLoginNow       = time.Now
)

// Wire DTOs mirroring internal/api/oauth_login.go's unexported request/
// response shapes — duplicated here rather than imported, the same
// established pattern cmd/login.go's deviceAuthRequest/deviceAuthResponse
// already follow for POST /api/auth/device (see that file's own doc
// comment for the full rationale: those server-side types are deliberately
// unexported, since internal/api/oauth_login.go is a server-internal
// handler, not a shared client/server contract package).
type oauthLoginStartRequest struct {
	Service     string `json:"service"`
	Namespace   string `json:"namespace"`
	RedirectURI string `json:"redirect_uri,omitempty"`
}

type oauthLoginStartResponse struct {
	SessionID               string `json:"session_id"`
	Flow                    string `json:"flow"`
	AuthorizeURL            string `json:"authorize_url,omitempty"`
	UserCode                string `json:"user_code,omitempty"`
	VerificationURI         string `json:"verification_uri,omitempty"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	IntervalSeconds         int    `json:"interval_seconds,omitempty"`
	ExpiresInSeconds        int    `json:"expires_in_seconds,omitempty"`
}

type oauthLoginStatusResponse struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type oauthLoginCompleteRequest struct {
	Code  string `json:"code"`
	State string `json:"state,omitempty"`
}

var secretOAuthCmd = &cobra.Command{
	Use:   "oauth",
	Short: "Manage OAuth2 grants for API gateway services",
}

var secretOAuthLoginCmd = &cobra.Command{
	Use:   "login <service|provider>",
	Short: "Obtain an initial OAuth2 grant (device/loopback/manual)",
	Long: "Starts `boid secret oauth login <service|provider>`'s three-flow initial grant\n" +
		"(docs/plans/api-gateway.md §7): device (Microsoft/GitHub — displays a\n" +
		"user_code, the daemon polls in the background), loopback (Google/\n" +
		"Atlassian — opens a local listener and waits for the browser redirect),\n" +
		"or manual (freee and other OOB-only providers — paste the code shown\n" +
		"after authorizing). Which flow runs is decided entirely by the\n" +
		"service's oauth_providers.<name>.flow config; this command adapts to\n" +
		"whichever one the daemon reports.\n\n" +
		"The argument names either a services.<name> entry (auth.kind: oauth2)\n" +
		"or an oauth_providers.<name> entry directly. The grant belongs to the\n" +
		"PROVIDER, so one login covers every service pointing at it — with six\n" +
		"google-backed services configured, `login google` and `login gmail-api`\n" +
		"do exactly the same thing and both unlock all six.",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runSecretOAuthLogin,
}

func init() {
	secretOAuthLoginCmd.Flags().StringP("namespace", "n", "default", "Secret namespace (workspace)")
	secretOAuthLoginCmd.Flags().Duration("timeout", 5*time.Minute, "How long to wait for the user to finish authorizing")
	secretOAuthCmd.AddCommand(secretOAuthLoginCmd)
	secretCmd.AddCommand(secretOAuthCmd)
}

func runSecretOAuthLogin(cmd *cobra.Command, args []string) error {
	service := args[0]
	namespace, err := cmd.Flags().GetString("namespace")
	if err != nil {
		return fmt.Errorf("secret oauth login: %w", err)
	}
	timeout, err := cmd.Flags().GetDuration("timeout")
	if err != nil {
		return fmt.Errorf("secret oauth login: %w", err)
	}

	c := client.FromContext(cmd.Context())

	// Always open a local RFC 8252 §7.3 loopback listener up front,
	// regardless of which flow the service turns out to use — opening a
	// port is cheap, and it lets a single request/response round trip with
	// the daemon suffice for every flow instead of a "tell me the flow
	// first, then maybe open a listener" second hop (see
	// apigateway.LoginManager.StartLogin's own doc comment). Closed
	// immediately below for device/manual, whose redirect_uri is never the
	// loopback one.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("secret oauth login: open local listener: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	ctx, cancel := context.WithTimeout(cmd.Context(), oauthLoginRequestTimeout)
	defer cancel()
	req := oauthLoginStartRequest{Service: service, Namespace: namespace, RedirectURI: redirectURI}
	var start oauthLoginStartResponse
	if err := c.DoContext(ctx, "POST", "/api/oauth/login", req, &start); err != nil {
		_ = ln.Close()
		return fmt.Errorf("secret oauth login: %w", err)
	}

	switch start.Flow {
	case "device":
		_ = ln.Close()
		return runOAuthDeviceFlow(cmd, c, start, timeout)
	case "loopback":
		return runOAuthLoopbackFlow(cmd, c, ln, start, timeout)
	case "manual":
		_ = ln.Close()
		return runOAuthManualFlow(cmd, c, start)
	default:
		_ = ln.Close()
		return fmt.Errorf("secret oauth login: daemon returned an unrecognized flow %q", start.Flow)
	}
}

// printAuthorizeURL prints an authorize/verification URL for the user to
// open.
//
// No terminal QR code is rendered for any flow. Earlier revisions offered
// one for the device and manual/OOB flows on the theory that a QR is an
// invitation to finish the flow ON ANOTHER DEVICE — technically true for
// those two, but not how this command is actually used: whoever runs
// `boid secret oauth login` is already at the terminal the URL is printed
// on, and opening it there is strictly less work than scanning. What the
// QR did deliver was screens of block characters to scroll past before the
// prompt. (`boid web pair` still renders one — enrolling a phone is that
// command's entire purpose, so there the other device is the point.)
func printAuthorizeURL(cmd *cobra.Command, label, rawURL string) {
	fmt.Fprintf(cmd.ErrOrStderr(), "%s:\n\n  %s\n\n", label, rawURL)
}

// completeOAuthLogin POSTs /api/oauth/login/{sessionID}/complete and prints
// the standard success line — shared by the loopback and manual flows
// below (device never calls this; its completion is entirely daemon-side —
// see runOAuthDeviceFlow's own doc comment).
func completeOAuthLogin(cmd *cobra.Command, c *client.Client, sessionID, code, state string) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), oauthLoginRequestTimeout)
	defer cancel()
	req := oauthLoginCompleteRequest{Code: code, State: state}
	if err := c.DoContext(ctx, "POST", "/api/oauth/login/"+sessionID+"/complete", req, nil); err != nil {
		return fmt.Errorf("secret oauth login: %w", err)
	}
	fmt.Fprintln(cmd.ErrOrStderr(), "Login complete.")
	return nil
}

// runOAuthManualFlow implements the manual/OOB flow (docs/plans/
// api-gateway.md §7: freee and any other provider whose only redirect_uri
// is urn:ietf:wg:oauth:2.0:oob) — the provider displays the code directly
// in the browser after consent; there is no redirect for this CLI to
// intercept, so the user copies it by hand.
func runOAuthManualFlow(cmd *cobra.Command, c *client.Client, start oauthLoginStartResponse) error {
	printAuthorizeURL(cmd, "Open this URL in a browser to authorize", start.AuthorizeURL)
	fmt.Fprint(cmd.ErrOrStderr(), "Paste the code shown after authorizing: ")
	code, err := readLine(cmd.InOrStdin())
	if err != nil {
		return fmt.Errorf("secret oauth login: read code: %w", err)
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return fmt.Errorf("secret oauth login: code is required")
	}
	return completeOAuthLogin(cmd, c, start.SessionID, code, "")
}

// oauthCallbackResult is what runOAuthLoopbackFlow's local HTTP server
// hands back once the browser redirect lands (or the provider redirected
// with an error instead of a code — RFC 6749 §4.1.2.1).
type oauthCallbackResult struct {
	code, state string
	err         error
}

// runOAuthLoopbackFlow implements the loopback flow (docs/plans/
// api-gateway.md §7, RFC 8252 §7.3): serves a one-shot HTTP handler on the
// listener StartLogin's redirect_uri already named, waits for the browser
// to land on it (or the caller-supplied timeout to elapse), and forwards
// whatever code/state arrived to CompleteLogin — the daemon is the one that
// actually checks state against the PKCE session it holds (this function
// never sees the verifier at all).
func runOAuthLoopbackFlow(cmd *cobra.Command, c *client.Client, ln net.Listener, start oauthLoginStartResponse, timeout time.Duration) error {
	// "on THIS machine" is load-bearing: the callback lands on this
	// machine's own 127.0.0.1 listener, so the browser must be here.
	printAuthorizeURL(cmd, "Open this URL in a browser on THIS machine to authorize", start.AuthorizeURL)

	resultCh := make(chan oauthCallbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if errParam := q.Get("error"); errParam != "" {
			fmt.Fprintln(w, "Authorization failed. You can close this tab and return to the terminal.")
			select {
			case resultCh <- oauthCallbackResult{err: fmt.Errorf("authorization server returned error: %s", errParam)}:
			default:
			}
			return
		}
		fmt.Fprintln(w, "Authorization received. You can close this tab and return to the terminal.")
		select {
		case resultCh <- oauthCallbackResult{code: q.Get("code"), state: q.Get("state")}:
		default:
		}
	})
	srv := &http.Server{Handler: mux}
	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- srv.Serve(ln) }()
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	select {
	case res := <-resultCh:
		if res.err != nil {
			return fmt.Errorf("secret oauth login: %w", res.err)
		}
		if res.code == "" {
			return fmt.Errorf("secret oauth login: browser callback did not include a code parameter")
		}
		return completeOAuthLogin(cmd, c, start.SessionID, res.code, res.state)
	case err := <-serveErrCh:
		if err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("secret oauth login: local callback listener failed: %w", err)
		}
		return fmt.Errorf("secret oauth login: local callback listener stopped before receiving a callback")
	case <-time.After(timeout):
		return fmt.Errorf("secret oauth login: timed out after %s waiting for the browser callback", timeout)
	}
}

// runOAuthDeviceFlow implements the device flow (docs/plans/api-gateway.md
// §7, RFC 8628): displays the user_code/verification URI and polls the
// daemon's own session status — NOT the provider directly; the daemon's
// LoginManager already polls the provider's token endpoint in the
// background at its own cadence (docs/plans/api-gateway.md §7: "device
// flow の polling は daemon 側で行う"), so this loop is just checking in on
// that, decoupled from whatever backoff (RFC 8628 §3.5 slow_down) the
// daemon's own poll is doing against the real provider.
func runOAuthDeviceFlow(cmd *cobra.Command, c *client.Client, start oauthLoginStartResponse, timeout time.Duration) error {
	fmt.Fprintf(cmd.ErrOrStderr(), "Go to: %s\n", start.VerificationURI)
	fmt.Fprintf(cmd.ErrOrStderr(), "Enter code: %s\n\n", start.UserCode)
	if start.VerificationURIComplete != "" {
		// RFC 8628 §3.3.1: the same page with the user_code pre-filled, so
		// opening it skips the manual code entry above. Printed as plain
		// text — see printAuthorizeURL on why there is no QR here.
		fmt.Fprintf(cmd.ErrOrStderr(), "Or open this URL to skip entering the code:\n\n  %s\n\n", start.VerificationURIComplete)
	}

	interval := time.Duration(start.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := oauthLoginNow().Add(timeout)

	for {
		oauthLoginPollSleep(interval)
		if oauthLoginNow().After(deadline) {
			return fmt.Errorf("secret oauth login: timed out after %s waiting for device authorization", timeout)
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), oauthLoginRequestTimeout)
		var status oauthLoginStatusResponse
		err := c.DoContext(ctx, "GET", "/api/oauth/login/"+start.SessionID, nil, &status)
		cancel()
		if err != nil {
			return fmt.Errorf("secret oauth login: check status: %w", err)
		}

		switch status.Status {
		case "complete":
			fmt.Fprintln(cmd.ErrOrStderr(), "Login complete.")
			return nil
		case "failed":
			return fmt.Errorf("secret oauth login: %s", status.Error)
		case "expired":
			return fmt.Errorf("secret oauth login: session expired before authorization completed")
		}
		// "pending" — keep polling.
	}
}
