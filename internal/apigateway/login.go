package apigateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// This file implements `boid secret oauth login <service>` (docs/plans/
// api-gateway.md §7 "初回認証", PR3): the daemon-side half of the three-flow
// initial OAuth2 grant (device / loopback / manual). The CLI-facing HTTP
// handler lives in internal/api (this package stays free of any
// internal/api import — doc.go's "self-contained leaf package" rationale);
// LoginManager is what that handler drives.
//
// §7's role split — "CLI = ブラウザ側、daemon = Web サーバ側" — means every
// piece of state a grant needs while it is in flight (the PKCE verifier, the
// CSRF state, the RFC 8628 device_code) lives ONLY in LoginManager's
// in-process pendingLogin map, never on the CLI's side of the wire: the CLI
// only ever sees an opaque session id, a user-facing code/URL to display,
// and (for loopback) the redirect_uri IT is asking the daemon to bind the
// grant to (docs/plans/api-gateway.md §7: "デスクトップ機には平文トークンも
// client_secret も一度も存在しない").

// pendingLoginTTL bounds how long a loopback/manual pendingLogin survives
// before Status/CompleteLogin treat it as expired — these two flows have no
// provider-declared expiry the way device flow's own expires_in gives
// (startDevice below uses THAT instead of this constant). 10 minutes is
// comfortably longer than an ordinary "open the browser tab, click through
// consent" interaction, while still reaping an abandoned login promptly.
const pendingLoginTTL = 10 * time.Minute

// LoginStatus is the lifecycle state of one pendingLogin, exposed to the CLI
// via LoginManager.Status.
type LoginStatus string

const (
	LoginStatusPending  LoginStatus = "pending"
	LoginStatusComplete LoginStatus = "complete"
	LoginStatusFailed   LoginStatus = "failed"
	LoginStatusExpired  LoginStatus = "expired"
)

// pendingLogin is one in-flight `boid secret oauth login` session.
// Everything security-load-bearing (codeVerifier/state/deviceCode) never
// leaves this struct — LoginManager's exported methods only ever return the
// user-facing subset (authorize URL, user_code, status/error) a CLI needs.
type pendingLogin struct {
	id        string
	namespace string
	provider  string
	flow      LoginFlow
	createdAt time.Time
	expiresAt time.Time

	// loopback only.
	state        string
	codeVerifier string
	redirectURI  string

	// device only. cancelPoll stops pollDeviceGrant's background goroutine
	// early (used by tests; production code lets it run to its own natural
	// completion/expiry).
	deviceCode string
	cancelPoll context.CancelFunc

	mu     sync.Mutex
	status LoginStatus
	errMsg string
}

func (p *pendingLogin) snapshot() (LoginStatus, string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status, p.errMsg, true
}

func (p *pendingLogin) setResult(status LoginStatus, errMsg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// A session already COMPLETE or FAILED never regresses back to pending
	// or flips to a different terminal state — pollDeviceGrant's own
	// deadline-expiry goroutine and an in-flight CompleteLogin racing it
	// (or a caller invoking CompleteLogin twice) must not stomp on
	// whichever one actually won.
	if p.status != LoginStatusPending {
		return
	}
	p.status = status
	p.errMsg = errMsg
}

// LoginStart is StartLogin's result — everything the CLI needs to carry out
// its half of whichever flow the provider is configured for.
type LoginStart struct {
	SessionID string
	Flow      LoginFlow

	// loopback/manual only.
	AuthorizeURL string

	// device only.
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	IntervalSeconds         int
	ExpiresInSeconds        int
}

// LoginManager orchestrates the pending-login-session lifecycle for `boid
// secret oauth login <service>` (docs/plans/api-gateway.md §7, PR3),
// reusing tokens (the daemon's single OAuth2TokenSource) for provider
// lookup and — critically — the exact same persistGrant persistence-order
// guarantee every refresh already relies on, so a login-obtained grant and
// a refresh-rotated one are indistinguishable to every downstream reader.
type LoginManager struct {
	tokens *OAuth2TokenSource

	// HTTPClient defaults to tokens.httpClient() (same anti-redirect
	// hardening — see OAuth2TokenSource.HTTPClient's own doc comment) when
	// nil; overridable by tests independently of tokens' own client.
	HTTPClient *http.Client
	// Now defaults to tokens.now() when nil.
	Now func() time.Time
	// SlowDownIncrement overrides DefaultDeviceSlowDownIncrement — see that
	// constant's own doc comment. Zero means "use the default".
	SlowDownIncrement time.Duration

	mu       sync.Mutex
	sessions map[string]*pendingLogin
}

// NewLoginManager builds a LoginManager sharing tokens' provider registry,
// secret resolver/writer, and persistGrant. tokens must not be nil.
func NewLoginManager(tokens *OAuth2TokenSource) *LoginManager {
	return &LoginManager{tokens: tokens, sessions: make(map[string]*pendingLogin)}
}

func (m *LoginManager) httpClient() *http.Client {
	if m.HTTPClient != nil {
		return m.HTTPClient
	}
	return m.tokens.httpClient()
}

func (m *LoginManager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return m.tokens.now()
}

func (m *LoginManager) provider(name string) (OAuthProviderConfig, bool) {
	cfg, ok := m.tokens.providers[name]
	return cfg, ok
}

func (m *LoginManager) put(p *pendingLogin) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions == nil {
		m.sessions = make(map[string]*pendingLogin)
	}
	m.sessions[p.id] = p
}

func (m *LoginManager) get(id string) (*pendingLogin, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.sessions[id]
	return p, ok
}

// StartLogin begins a new login session for (namespace, provider) using
// whichever LoginFlow that provider's config.yaml oauth_providers entry
// declares. redirectURI is the CLI's freshly-opened RFC 8252 §7.3 loopback
// listener's callback URL — always supplied by the CLI regardless of which
// flow it turns out to be (cheap to open a local port; StartLogin simply
// ignores it for device/manual) so a single request/response round trip
// suffices for every flow (no second "now tell me your redirect_uri" hop).
func (m *LoginManager) StartLogin(namespace, provider, redirectURI string) (*LoginStart, error) {
	// Normalized ONCE, here — see normalizeNamespace's own doc comment
	// (oauth2.go): this is one of exactly two entry points in the whole
	// package, the other being OAuth2TokenSource.AccessToken. Without this,
	// a caller passing "" (e.g. a hand-rolled HTTP request against
	// /api/oauth/login that skips the "" -> "default" defaulting
	// internal/api/oauth_login.go's Start handler and cmd/secret_oauth.go's
	// --namespace flag both already apply in every real call path) would
	// persist this login's grant through m.tokens' resolver/writer under the
	// literal namespace "" — harmless for the SECRET STORE row itself
	// (dispatcher.SecretStore.Get/Set independently normalize "" to
	// "default" internally), but NOT for this package's own in-process
	// memCache: a subsequent AccessToken("", provider) or AccessToken(
	// "default", provider) call normalizes to "default" before checking
	// memCache, so a login-obtained memCache entry stored under the
	// UNNORMALIZED "" key would simply never be found by it — forcing one
	// avoidable extra refresh immediately after a successful login. The
	// same class of bug codex review round 5 fixed for AccessToken itself
	// (oauth2.go's own doc comment on this constant).
	namespace = normalizeNamespace(namespace)

	cfg, ok := m.provider(provider)
	if !ok {
		return nil, fmt.Errorf("apigateway: oauth2 provider %q is not configured (oauth_providers: in config.yaml)", provider)
	}
	// Defensive guard (docs/plans/api-gateway.md §6-補): internal/config.
	// validateOAuthProviderConfig already rejects a config.yaml entry that
	// sets grant: client_credentials together with a flow at load time, so
	// cfg.Flow should always be "" here for a client_credentials provider —
	// this exists only to give a hand-built OAuthProviderConfig (a test, or
	// any future caller that skips config-load validation) a clear,
	// grant-specific error instead of either silently falling through to
	// the generic "no flow configured" message below (misleading — it
	// implies setting a flow would fix it, but client_credentials has no
	// flow concept at all to set) or, worse, StartLogin picking one of the
	// three login-flow branches by accident if cfg.Flow happened to be
	// non-empty despite the grant.
	if cfg.Grant == GrantClientCredentials {
		return nil, fmt.Errorf("apigateway: oauth2 provider %q uses the client_credentials grant (RFC 6749 §4.4) — this is a direct client_id/client_secret token-endpoint request with no user authorization step, so there is no login flow to start; seed or rotate client_secret via `boid secret set` instead", provider)
	}
	switch cfg.Flow {
	case LoginFlowDevice:
		return m.startDevice(namespace, cfg)
	case LoginFlowLoopback:
		return m.startLoopback(namespace, cfg, redirectURI)
	case LoginFlowManual:
		return m.startManual(namespace, cfg)
	default:
		return nil, fmt.Errorf("apigateway: oauth2 provider %q has no login flow configured (set oauth_providers.%s.flow to device/loopback/manual in config.yaml)", provider, provider)
	}
}

// startLoopback implements docs/plans/api-gateway.md §7's loopback flow:
// PKCE verifier/challenge (RFC 7636) + CSRF state, both held only here
// (never sent to the CLI) — the CLI receives just the finished authorize
// URL to open in a browser.
func (m *LoginManager) startLoopback(namespace string, cfg OAuthProviderConfig, redirectURI string) (*LoginStart, error) {
	if redirectURI == "" {
		return nil, fmt.Errorf("apigateway: oauth2 provider %q uses the loopback flow, which requires a redirect_uri (the CLI's local listener callback URL)", cfg.Name)
	}
	verifier, err := generateCodeVerifier()
	if err != nil {
		return nil, fmt.Errorf("apigateway: generate PKCE code_verifier: %w", err)
	}
	// GenerateToken (token.go) panics rather than returning an error on a
	// crypto/rand failure — an acceptable pre-existing convention for a
	// job/registry token, reused here verbatim for the CSRF state value
	// (same randomness requirement, no separate helper needed).
	state := GenerateToken()

	authURL, err := buildAuthorizeURL(cfg, redirectURI, state, codeChallengeS256(verifier))
	if err != nil {
		return nil, err
	}

	p := &pendingLogin{
		id:           GenerateToken(),
		namespace:    namespace,
		provider:     cfg.Name,
		flow:         LoginFlowLoopback,
		createdAt:    m.now(),
		expiresAt:    m.now().Add(pendingLoginTTL),
		state:        state,
		codeVerifier: verifier,
		redirectURI:  redirectURI,
		status:       LoginStatusPending,
	}
	m.put(p)

	return &LoginStart{SessionID: p.id, Flow: LoginFlowLoopback, AuthorizeURL: authURL}, nil
}

// startManual implements docs/plans/api-gateway.md §7's manual/OOB flow: no
// PKCE (freee — the flow's motivating provider — does not support it), no
// state (there is no redirect to carry it back on; the provider displays
// the code directly in the browser and the user copies it by hand), the
// OOB redirect_uri urn:ietf:wg:oauth:2.0:oob.
func (m *LoginManager) startManual(namespace string, cfg OAuthProviderConfig) (*LoginStart, error) {
	authURL, err := buildAuthorizeURL(cfg, "urn:ietf:wg:oauth:2.0:oob", "", "")
	if err != nil {
		return nil, err
	}

	p := &pendingLogin{
		id:        GenerateToken(),
		namespace: namespace,
		provider:  cfg.Name,
		flow:      LoginFlowManual,
		createdAt: m.now(),
		expiresAt: m.now().Add(pendingLoginTTL),
		status:    LoginStatusPending,
	}
	m.put(p)

	return &LoginStart{SessionID: p.id, Flow: LoginFlowManual, AuthorizeURL: authURL}, nil
}

// buildAuthorizeURL builds the RFC 6749 §3.1 authorization request URL
// shared by the loopback and manual flows. state/codeChallenge are both ""
// for manual (see startManual's own doc comment).
func buildAuthorizeURL(cfg OAuthProviderConfig, redirectURI, state, codeChallenge string) (string, error) {
	base, err := url.Parse(cfg.AuthorizationEndpoint)
	if err != nil || base.Scheme == "" || base.Host == "" {
		// Defensive: internal/config.validateOAuthProviderConfig already
		// validates authorization_endpoint at config-load time for any
		// provider whose flow requires it — see that function's own
		// "already validated" invariant note (mirrors
		// NewCredentialProvider's identical convention for base_url).
		return "", fmt.Errorf("apigateway: oauth2 provider %q has an invalid authorization_endpoint %q", cfg.Name, cfg.AuthorizationEndpoint)
	}
	q := base.Query()
	q.Set("response_type", "code")
	q.Set("client_id", cfg.ClientID)
	q.Set("redirect_uri", redirectURI)
	if len(cfg.Scopes) > 0 {
		q.Set("scope", strings.Join(cfg.Scopes, " "))
	}
	if state != "" {
		q.Set("state", state)
	}
	if codeChallenge != "" {
		q.Set("code_challenge", codeChallenge)
		q.Set("code_challenge_method", "S256")
	}
	for k, v := range cfg.AuthorizeParams {
		q.Set(k, v)
	}
	base.RawQuery = q.Encode()
	return base.String(), nil
}

// CompleteLogin finishes a loopback or manual pendingLogin: exchanges code
// (RFC 6749 §4.1.3 authorization_code grant) for tokens and persists the
// grant through tokens.persistGrant — the SAME durable-write-ordering
// contract a refresh gets. state is required (and checked) for loopback
// (docs/plans/api-gateway.md §7: "state 検証は daemon 側の pending login
// session と突き合わせる (CSRF 対策)"); ignored for manual, which never had
// one to check (no redirect occurs for OOB — see startManual).
//
// A session is single-shot: once it leaves LoginStatusPending (success OR
// failure), every subsequent CompleteLogin call for the same sessionID is
// rejected outright rather than retried — RFC 6749 §4.1.2 authorization
// codes are single-use, so a failed exchange attempt has, in general,
// already burned the code upstream (whether the provider actually
// invalidated it or not is provider-specific and not worth guessing at);
// the correct recovery is always a fresh `boid secret oauth login`, not a
// retry against the same session.
func (m *LoginManager) CompleteLogin(sessionID, code, state string) error {
	p, ok := m.get(sessionID)
	if !ok {
		return fmt.Errorf("apigateway: oauth2 login session %q not found or expired", sessionID)
	}
	if p.flow != LoginFlowLoopback && p.flow != LoginFlowManual {
		return fmt.Errorf("apigateway: oauth2 login session %q is a %q flow — device flow completes on its own via daemon polling, not CompleteLogin", sessionID, p.flow)
	}
	if m.now().After(p.expiresAt) {
		p.setResult(LoginStatusExpired, "login session expired")
		return fmt.Errorf("apigateway: oauth2 login session %q expired", sessionID)
	}
	if status, errMsg, _ := p.snapshot(); status != LoginStatusPending {
		if status == LoginStatusComplete {
			return fmt.Errorf("apigateway: oauth2 login session %q already completed", sessionID)
		}
		return fmt.Errorf("apigateway: oauth2 login session %q already failed: %s", sessionID, errMsg)
	}
	if code == "" {
		return fmt.Errorf("apigateway: oauth2 login session %q: code is required", sessionID)
	}
	if p.flow == LoginFlowLoopback && state != p.state {
		p.setResult(LoginStatusFailed, "state mismatch")
		return fmt.Errorf("apigateway: oauth2 login session %q: state mismatch (possible CSRF)", sessionID)
	}

	cfg, ok := m.provider(p.provider)
	if !ok {
		p.setResult(LoginStatusFailed, "provider no longer configured")
		return fmt.Errorf("apigateway: oauth2 provider %q is no longer configured", p.provider)
	}

	if err := m.exchangeAndPersist(p, cfg, code); err != nil {
		p.setResult(LoginStatusFailed, err.Error())
		return err
	}
	p.setResult(LoginStatusComplete, "")
	return nil
}

// exchangeAndPersist performs the authorization_code grant for p and
// persists the result via m.tokens.persistGrant. Split out of CompleteLogin
// so both the loopback and manual arms below can share the "resolve
// client_secret, exchange, persist" sequence — the only difference between
// the two is whether a code_verifier/redirect_uri accompanies the request.
func (m *LoginManager) exchangeAndPersist(p *pendingLogin, cfg OAuthProviderConfig, code string) error {
	var clientSecret string
	if cfg.ClientSecretKey != "" {
		var err error
		clientSecret, err = m.tokens.resolver(p.namespace, cfg.ClientSecretKey)
		if err != nil {
			return fmt.Errorf("apigateway: oauth2 provider %q: resolve client_secret_key %q (namespace %q): %w", cfg.Name, cfg.ClientSecretKey, p.namespace, err)
		}
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	if cfg.ClientID != "" {
		form.Set("client_id", cfg.ClientID)
	}
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}
	// redirect_uri/code_verifier only apply to the loopback flow (RFC 6749
	// §4.1.3 requires redirect_uri to be repeated here IF it was present in
	// the authorization request, which it always is for loopback; RFC 7636
	// §4.5 requires code_verifier for a PKCE-using request). The manual/OOB
	// flow sent neither at authorize time (startManual), so neither is
	// repeated here — see LoginFlowManual's own doc comment for why PKCE
	// does not apply to it at all.
	if p.flow == LoginFlowLoopback {
		form.Set("redirect_uri", p.redirectURI)
		form.Set("code_verifier", p.codeVerifier)
	}

	resp, _, err := m.tokens.postFormToTokenEndpoint(cfg.TokenEndpoint, form, cfg.Name, rfc6749TokenErrorCodes)
	if err != nil {
		return fmt.Errorf("apigateway: oauth2 provider %q: authorization_code grant: %w", cfg.Name, err)
	}

	// priorRefreshToken: best-effort read of whatever the secret store
	// already held (empty for a genuine first-ever login) — see
	// persistGrant's own doc comment for why this is the correct value to
	// diff resp.RefreshToken against rather than always treating it as "".
	// A resolver error here is NOT fatal to the login itself (an unset key
	// is the overwhelmingly common case and resolver's own error for that
	// is indistinguishable from a genuine transient failure at this
	// layer) — worst case, a genuinely rotated refresh_token that happens
	// to collide byte-for-byte with a stale unreadable value gets an
	// extra, harmless re-write.
	priorRefreshToken, _ := m.tokens.resolver(p.namespace, OAuthSecretKey(cfg.Name, oauthFieldRefreshToken))
	if _, err := m.tokens.persistGrant(p.namespace, cfg, resp, priorRefreshToken, true); err != nil {
		return err
	}
	return requireRefreshToken(cfg.Name, resp.RefreshToken, priorRefreshToken)
}

// requireRefreshToken reports an error when a login's grant produced no
// usable refresh_token at all — neither a freshly-issued one
// (freshRefreshToken, resp.RefreshToken) nor one already on file
// (priorRefreshToken). This is deliberately checked as a LOGIN-specific
// failure (never for an ordinary refresh — refresh() calls persistGrant
// directly, not through this function): the entire point of `boid secret
// oauth login` is to hand the daemon's single OAuth2TokenSource something
// it can refresh with going forward (docs/plans/api-gateway.md §6 "daemon
// 単一リフレッシャの原則"). A login that only obtains an access_token
// (no refresh_token, and none already stored) would silently "succeed" —
// the access_token works until it expires — and then fail confusingly on
// the very first proactive refresh afterward, with no obvious link back to
// this login having been incomplete. The most common real cause is a
// provider (Google is the canonical example) that only issues a
// refresh_token on the FIRST consent, or when explicitly asked via
// provider-specific authorize request parameters — see
// OAuthProviderConfig.AuthorizeParams' own doc comment.
func requireRefreshToken(providerName, freshRefreshToken, priorRefreshToken string) error {
	if freshRefreshToken != "" || priorRefreshToken != "" {
		return nil
	}
	return fmt.Errorf("apigateway: oauth2 provider %q: login succeeded in obtaining an access_token, but the provider returned no refresh_token and none was already on file — the daemon cannot refresh this grant later; check the provider's authorize_params (e.g. Google needs access_type=offline, and prompt=consent if you have already granted consent once before)", providerName)
}

// startDevice implements docs/plans/api-gateway.md §7's device flow (RFC
// 8628): a device authorization request against
// cfg.DeviceAuthorizationEndpoint, then a background goroutine that polls
// cfg.TokenEndpoint on the provider's own declared interval until success,
// denial, or expiry — "device flow の polling は daemon 側で行う" (§7). The
// CLI never talks to either endpoint directly; it only displays
// user_code/verification_uri and polls THIS package's own Status (a local,
// cheap daemon-session check, decoupled from whatever cadence the poller
// below actually uses against the real provider).
func (m *LoginManager) startDevice(namespace string, cfg OAuthProviderConfig) (*LoginStart, error) {
	if cfg.DeviceAuthorizationEndpoint == "" {
		return nil, fmt.Errorf("apigateway: oauth2 provider %q uses the device flow but has no device_authorization_endpoint configured", cfg.Name)
	}

	var clientSecret string
	if cfg.ClientSecretKey != "" {
		var err error
		clientSecret, err = m.tokens.resolver(namespace, cfg.ClientSecretKey)
		if err != nil {
			return nil, fmt.Errorf("apigateway: oauth2 provider %q: resolve client_secret_key %q (namespace %q): %w", cfg.Name, cfg.ClientSecretKey, namespace, err)
		}
	}

	dresp, err := m.postDeviceAuthorizationRequest(cfg, clientSecret)
	if err != nil {
		return nil, fmt.Errorf("apigateway: oauth2 provider %q: device authorization request: %w", cfg.Name, err)
	}
	if dresp.DeviceCode == "" || dresp.UserCode == "" || dresp.VerificationURI == "" {
		return nil, fmt.Errorf("apigateway: oauth2 provider %q: device authorization response missing a required field", cfg.Name)
	}

	interval := time.Duration(dresp.Interval) * time.Second
	if interval <= 0 {
		// RFC 8628 §3.2: interval is OPTIONAL, default 5 seconds.
		interval = 5 * time.Second
	}
	expiresIn := time.Duration(dresp.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		// RFC 8628 §3.2 marks expires_in REQUIRED, but every field this
		// package reads gets a defensive fallback rather than trusting a
		// non-compliant provider — 10 minutes matches pendingLoginTTL's own
		// "long enough for a real interaction, short enough to reap
		// promptly" reasoning.
		expiresIn = pendingLoginTTL
	}

	ctx, cancel := context.WithTimeout(context.Background(), expiresIn)
	p := &pendingLogin{
		id:         GenerateToken(),
		namespace:  namespace,
		provider:   cfg.Name,
		flow:       LoginFlowDevice,
		createdAt:  m.now(),
		expiresAt:  m.now().Add(expiresIn),
		deviceCode: dresp.DeviceCode,
		cancelPoll: cancel,
		status:     LoginStatusPending,
	}
	m.put(p)

	go m.pollDeviceGrant(ctx, cancel, p, cfg, clientSecret, interval)

	return &LoginStart{
		SessionID:               p.id,
		Flow:                    LoginFlowDevice,
		UserCode:                dresp.UserCode,
		VerificationURI:         dresp.VerificationURI,
		VerificationURIComplete: dresp.VerificationURIComplete,
		IntervalSeconds:         int(interval / time.Second),
		ExpiresInSeconds:        int(expiresIn / time.Second),
	}, nil
}

// DefaultDeviceSlowDownIncrement is RFC 8628 §3.5's mandated backoff on a
// "slow_down" response: "the client MUST increase its polling interval by
// 5 seconds for each subsequent request". Overridable via
// LoginManager.SlowDownIncrement — tests only (mirroring
// OAuth2TokenSource.RefreshMargin's own override convention, so a
// slow_down test doesn't have to wait out a real 5-second backoff);
// production callers always get the spec value.
const DefaultDeviceSlowDownIncrement = 5 * time.Second

func (m *LoginManager) slowDownIncrement() time.Duration {
	if m.SlowDownIncrement > 0 {
		return m.SlowDownIncrement
	}
	return DefaultDeviceSlowDownIncrement
}

// pollDeviceGrant repeatedly polls cfg.TokenEndpoint (RFC 8628 §3.4) on
// interval until ctx is done (the device_authorization response's own
// expires_in elapsed — startDevice's ctx), the provider reports a terminal
// error ("expired_token"/"access_denied"/anything outside the
// "keep waiting" set), or the grant succeeds. Runs in its own goroutine,
// entirely decoupled from any CLI request/response — the CLI learns the
// outcome by polling Status, not by staying connected to this goroutine.
//
// cancel is startDevice's own context.WithTimeout cancel func, called via
// defer on every exit path (not just the ctx.Done() one) so the underlying
// timer is released the moment polling concludes for ANY reason, rather
// than lingering until expiresIn's full duration elapses regardless of how
// quickly the grant actually succeeded or failed.
func (m *LoginManager) pollDeviceGrant(ctx context.Context, cancel context.CancelFunc, p *pendingLogin, cfg OAuthProviderConfig, clientSecret string, interval time.Duration) {
	defer cancel()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			p.setResult(LoginStatusExpired, "device code expired before the user completed authorization")
			return
		case <-ticker.C:
		}

		form := url.Values{}
		form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
		form.Set("device_code", p.deviceCode)
		if cfg.ClientID != "" {
			form.Set("client_id", cfg.ClientID)
		}
		if clientSecret != "" {
			form.Set("client_secret", clientSecret)
		}

		resp, errCode, err := m.tokens.postFormToTokenEndpoint(cfg.TokenEndpoint, form, cfg.Name, rfc8628DeviceTokenErrorCodes)
		switch {
		case err == nil:
			priorRefreshToken, _ := m.tokens.resolver(p.namespace, OAuthSecretKey(cfg.Name, oauthFieldRefreshToken))
			if _, persistErr := m.tokens.persistGrant(p.namespace, cfg, resp, priorRefreshToken, true); persistErr != nil {
				slog.Error("apigateway: oauth2 device login grant obtained but persistence failed — the grant is lost; re-run `boid secret oauth login`",
					"provider", cfg.Name, "namespace", p.namespace, "error", persistErr)
				p.setResult(LoginStatusFailed, persistErr.Error())
				return
			}
			if reqErr := requireRefreshToken(cfg.Name, resp.RefreshToken, priorRefreshToken); reqErr != nil {
				p.setResult(LoginStatusFailed, reqErr.Error())
				return
			}
			p.setResult(LoginStatusComplete, "")
			return
		case errCode == "authorization_pending":
			// RFC 8628 §3.5: normal — the user has not finished yet. Keep
			// polling at the same interval.
			continue
		case errCode == "slow_down":
			// RFC 8628 §3.5: "the client MUST increase its polling
			// interval by 5 seconds". Applied to this ticker's own period
			// for every SUBSEQUENT tick — not just once.
			interval += m.slowDownIncrement()
			ticker.Reset(interval)
			continue
		default:
			// expired_token / access_denied / any standard RFC 6749 error /
			// an unrecognized code (errCode == "" — postFormToTokenEndpoint
			// already sanitized it) — all terminal.
			p.setResult(LoginStatusFailed, err.Error())
			return
		}
	}
}

// deviceAuthorizationResponse is RFC 8628 §3.2's device authorization
// response shape.
type deviceAuthorizationResponse struct {
	DeviceCode              string      `json:"device_code"`
	UserCode                string      `json:"user_code"`
	VerificationURI         string      `json:"verification_uri"`
	VerificationURIComplete string      `json:"verification_uri_complete,omitempty"`
	ExpiresIn               flexibleInt `json:"expires_in"`
	Interval                flexibleInt `json:"interval,omitempty"`
}

// postDeviceAuthorizationRequest performs the RFC 8628 §3.1 device
// authorization request: a POST to cfg.DeviceAuthorizationEndpoint with
// client_id (+ client_secret, if cfg is a confidential client) + scope +
// any configured AuthorizeParams. Error handling mirrors
// postFormToTokenEndpoint's own sanitization posture (never leak
// cfg.DeviceAuthorizationEndpoint's host into an error a sandbox-facing
// response might eventually echo), though this endpoint's response shape
// (deviceAuthorizationResponse, not oauthTokenResponse) is different enough
// that it is not worth threading through that function's generic parse —
// this is a small, independent implementation instead.
func (m *LoginManager) postDeviceAuthorizationRequest(cfg OAuthProviderConfig, clientSecret string) (deviceAuthorizationResponse, error) {
	form := url.Values{}
	if cfg.ClientID != "" {
		form.Set("client_id", cfg.ClientID)
	}
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}
	if len(cfg.Scopes) > 0 {
		form.Set("scope", strings.Join(cfg.Scopes, " "))
	}
	for k, v := range cfg.AuthorizeParams {
		form.Set(k, v)
	}

	req, err := http.NewRequest(http.MethodPost, cfg.DeviceAuthorizationEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		slog.Warn("apigateway: oauth2 failed to build device authorization request", "provider", cfg.Name, "error", err)
		return deviceAuthorizationResponse{}, fmt.Errorf("build device authorization request failed (see daemon log for details)")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := m.httpClient().Do(req)
	if err != nil {
		slog.Warn("apigateway: oauth2 device authorization request failed", "provider", cfg.Name, "error", err)
		return deviceAuthorizationResponse{}, fmt.Errorf("device authorization request failed (see daemon log for details)")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		slog.Warn("apigateway: oauth2 failed to read device authorization response", "provider", cfg.Name, "error", err)
		return deviceAuthorizationResponse{}, fmt.Errorf("read device authorization response failed (see daemon log for details)")
	}

	if resp.StatusCode != http.StatusOK {
		slog.Warn("apigateway: oauth2 device authorization endpoint returned a non-200 status",
			"provider", cfg.Name, "status", resp.StatusCode)
		return deviceAuthorizationResponse{}, fmt.Errorf("device authorization endpoint returned %d", resp.StatusCode)
	}

	var dresp deviceAuthorizationResponse
	if err := json.Unmarshal(body, &dresp); err != nil {
		slog.Warn("apigateway: oauth2 failed to decode device authorization response", "provider", cfg.Name, "error", err)
		return deviceAuthorizationResponse{}, fmt.Errorf("decode device authorization response failed (see daemon log for details)")
	}
	return dresp, nil
}

// Status returns the current lifecycle state of sessionID, for the CLI's
// device-flow poll (loopback/manual complete synchronously via
// CompleteLogin's own return value and rarely need this, but it works for
// them too). ok is false when sessionID is unknown — never registered, or
// already reaped (this package does not currently reap completed sessions
// itself; they simply live until process restart, which is an acceptable
// bound for a single-user personal daemon's in-memory session count).
func (m *LoginManager) Status(sessionID string) (status LoginStatus, errMsg string, ok bool) {
	p, found := m.get(sessionID)
	if !found {
		return "", "", false
	}
	if m.now().After(p.expiresAt) {
		p.setResult(LoginStatusExpired, "login session expired")
	}
	status, errMsg, _ = p.snapshot()
	return status, errMsg, true
}

// generateCodeVerifier returns a fresh RFC 7636 §4.1 PKCE code_verifier: 32
// random bytes, base64url-no-padding-encoded (43 characters — within the
// RFC's required 43-128 character range using only its "unreserved"
// character set, which base64url's alphabet is a subset of).
func generateCodeVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// codeChallengeS256 computes the RFC 7636 §4.2 S256 code_challenge for
// verifier: BASE64URL-ENCODE(SHA256(ASCII(verifier))), no padding.
func codeChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
