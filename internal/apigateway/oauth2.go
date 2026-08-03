package apigateway

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OAuthProviderConfig is the resolved shape of one config.yaml
// oauth_providers.<name> entry (internal/config.OAuthProviderConfig, which
// this package deliberately does not import — see doc.go's "self-contained
// leaf package" rationale) — the token-endpoint identity an
// OAuth2TokenSource needs to perform a refresh_token grant for that provider
// (docs/plans/api-gateway.md §論点4/§6: "config.yaml の oauth_providers:
// ブロック。client_secret のみ SecretStore 参照").
type OAuthProviderConfig struct {
	// Name is the provider's config.yaml key (oauth_providers.<Name>) — the
	// same string a services.*.auth.provider entry references.
	Name string
	// TokenEndpoint is the provider's OAuth2 token endpoint (RFC 6749 §3.2),
	// e.g. https://accounts.secure.freee.co.jp/public_api/token.
	TokenEndpoint string
	// ClientID is the OAuth2 client_id. Not a secret by RFC 6749 §2.2's own
	// classification (a public identifier), so — unlike a secret_key — it
	// travels as a plain string rather than a secret-store reference.
	ClientID string
	// ClientSecretKey is a secret-store key reference (never a plaintext
	// value) for a confidential client's client_secret (docs/plans/
	// api-gateway.md §7: "confidential client の client_secret を daemon 側
	// にのみ置く"). Empty for a public client (PKCE, no client_secret) —
	// callTokenEndpoint simply omits client_secret from the token request
	// when this resolves to "".
	ClientSecretKey string
	// Scopes is reserved for PR3's authorization-URL construction
	// (docs/plans/api-gateway.md §7 "flow の三段構え"). Never consumed by
	// this file's refresh_token grant — see callTokenEndpoint's own doc
	// comment for why.
	Scopes []string
}

// SecretWriter persists a plaintext secret-store value scoped to namespace
// — the write counterpart to SecretResolver. internal/server/wire.go adapts
// internal/dispatcher.SecretStore.Set to this exactly the way it already
// adapts SecretStore.Get to SecretResolver, keeping this leaf package free
// of any internal/dispatcher (and therefore internal/db) import — see
// SecretResolver's own doc comment for the identical rationale.
type SecretWriter func(namespace, key, value string) error

// OAuth2AccessTokenSource resolves a currently-valid access token for a
// (namespace, provider) pair, refreshing it first if needed. Implemented by
// *OAuth2TokenSource; CredentialProvider depends on this narrow interface —
// not the concrete type — so CredentialProvider's own tests can substitute
// a fake without exercising a real token endpoint (see
// CredentialProvider.SetOAuth2TokenSource).
type OAuth2AccessTokenSource interface {
	AccessToken(namespace, provider string) (string, error)
}

// oauthSecretField names the two pieces of per-(namespace, provider) OAuth2
// grant state an OAuth2TokenSource reads/writes through the secret store —
// never a plaintext config.yaml value (docs/plans/api-gateway.md §6:
// "SecretStore (workspace namespace 付き) に保持するもの: refresh_token、
// access_token、expires_at").
//
// access_token and expires_at are stored TOGETHER under one key
// (oauthFieldAccessTokenCache, a small JSON blob — see
// oauthAccessTokenCache) rather than as two independent secret-store keys
// (codex review round 3, Major finding): with two keys, a refresh that
// successfully persists access_token but then fails to persist expires_at
// (or vice versa) leaves a MISMATCHED pair durably on disk — a stale
// expires_at is silently believed to describe a brand-new access_token (or
// a stale access_token silently inherits a brand-new expires_at, the worse
// direction: it can make an already-invalid token look fresh for
// meaningfully longer than it actually is). A single key means the
// SecretStore's own per-key UPSERT is the only write involved, so this pair
// can never land partially — it is either the OLD (access_token, expires_at)
// pair, byte for byte, or the NEW one, never a hybrid of the two.
const (
	oauthFieldRefreshToken     = "refresh_token"
	oauthFieldAccessTokenCache = "access_token_cache"
)

// oauthAccessTokenCache is the JSON shape persisted under
// oauthFieldAccessTokenCache — see that constant's own doc comment for why
// access_token and expires_at are one key, not two.
type oauthAccessTokenCache struct {
	AccessToken string `json:"access_token"`
	ExpiresAt   int64  `json:"expires_at"` // Unix seconds
}

// normalizeNamespace mirrors internal/dispatcher.SecretStore's own private
// normalizeNamespace exactly (an empty namespace means "default"),
// duplicated here — not imported — to keep this leaf package free of
// internal/dispatcher (this package's own established pattern; see
// token.go's GenerateToken and singleflight.go's own doc comment for the
// identical rationale). AccessToken is the ONLY call site: see its own doc
// comment for why applying this exactly once, at that single entry point,
// is what keeps every downstream key (singleflight, memCache) and every
// resolver/writer call consistent with what the SecretStore itself treats
// as the same row.
func normalizeNamespace(ns string) string {
	if ns == "" {
		return "default"
	}
	return ns
}

// OAuthSecretKey returns the secret-store key an OAuth2TokenSource reads or
// writes for one (provider, field) pair. Exported so an operator's `boid
// secret set` invocation (docs/plans/api-gateway.md PR2 scope note: "初回
// grant は当面 boid secret set で refresh_token を手動投入して動作確認でき
// る形にする") and this package's own tests share the exact same naming
// convention, rather than two independently-typed-out string literals
// silently drifting apart. The "oauth2:" prefix keeps this namespaced apart
// from any other secret-store key a service's static auth kinds
// (bearer/basic/header/query) might use in the same secret-store namespace.
func OAuthSecretKey(provider, field string) string {
	return "oauth2:" + provider + ":" + field
}

// DefaultOAuth2RefreshMargin is how far before expires_at an
// OAuth2TokenSource proactively refreshes an access token, absent an
// explicit override (docs/plans/api-gateway.md §6: "expires_at より margin
// 手前で更新"). Comfortably covers the gateway's own request latency plus
// ordinary clock skew, while staying well inside every provider's shortest
// observed access-token lifetime (freee: 6h; Google/GitHub/Microsoft: ~1h).
const DefaultOAuth2RefreshMargin = 5 * time.Minute

// fallbackAccessTokenLifetime is used only when a token endpoint response
// omits expires_in entirely (RFC 6749 §5.1 marks it OPTIONAL, though every
// provider docs/plans/api-gateway.md targets does send it in practice). A
// conservative, deliberately short default: without it, a provider omitting
// expires_in would compute expiresAt == refresh time, making EVERY
// subsequent AccessToken call refresh again (DefaultOAuth2RefreshMargin
// would always be past a zero-length "valid" window) — a refresh storm
// against that provider's token endpoint. This value trades a few
// unnecessary refreshes (harmless — refresh_token grants have no
// single-use restriction) for never hammering the endpoint on every single
// gateway request.
//
// Deliberately SHORT (codex review round 3, Major finding), not a
// generous "typical OAuth2 token lifetime" guess like 55 minutes: PR2 has
// no reactive refresh-on-401 path at all (docs/plans/api-gateway.md §6:
// out of scope until buffered-retry is designed), so an assumed lifetime
// that overshoots a provider's REAL (but unstated) token lifetime means
// every request in that gap gets a genuinely-expired access token and a
// guaranteed upstream 401 — for as long as the wrong assumption holds. A
// short fallback bounds that window tightly at the cost of occasional
// extra (harmless) refreshes if the real, unstated lifetime happens to be
// long; combined with effectiveMargin's own halving-clamp (refresh's own
// doc comment), a token cached under this fallback re-checks itself well
// before this constant's own duration elapses, not after.
const fallbackAccessTokenLifetime = 10 * time.Minute

// OAuth2TokenSource is the daemon's single OAuth2 refresher (docs/plans/
// api-gateway.md §6 "daemon 単一リフレッシャの原則"): every oauth2-kind
// service's Resolve/Inject call funnels through the one instance
// internal/server/wire.go constructs, so two sandboxed jobs — or two
// concurrent requests from the same job — hitting the same (namespace,
// provider) grant at once are coalesced into a single token-endpoint round
// trip (singleflightGroup) rather than racing two independent refreshes:
// exactly the race a rotating-refresh-token provider (freee) cannot
// survive, since the loser's refresh_token has already been invalidated by
// the winner's token-endpoint call by the time it would try to use it.
type OAuth2TokenSource struct {
	providers map[string]OAuthProviderConfig
	resolver  SecretResolver
	writer    SecretWriter

	// RefreshMargin/Now/HTTPClient are exported so tests can override them
	// on an otherwise-normally-constructed *OAuth2TokenSource, rather than a
	// parallel constructor-with-options surface — mirrors this package's
	// existing preference for plain constructors (NewCredentialProvider,
	// NewServer) over an options struct. NewOAuth2TokenSource sets
	// production-sane defaults for all three; nothing outside this
	// package's own tests needs to touch them.
	RefreshMargin time.Duration
	Now           func() time.Time
	HTTPClient    *http.Client

	sf singleflightGroup[string]

	// memCache is a same-process, best-effort cache of the access token a
	// refresh most recently obtained, keyed by "namespace\x00provider" —
	// populated unconditionally on every successful refresh (codex review
	// round 1, Major finding), independent of whether the secret-store
	// access_token/expires_at persist (refresh's own "Phase 2") succeeded.
	// It exists to close two narrow-but-real gaps a secret-store-only cache
	// has:
	//
	//  1. Resolve/Inject (credentials.go) each call AccessToken
	//     independently for the SAME request (Resolve's own doc comment:
	//     "Inject re-resolves... cheap in the common case" — true for the
	//     static auth kinds' plain secret-store read, but NOT necessarily
	//     true here: if a refresh Resolve triggered hit Phase 2's cache-
	//     write failure, Inject's own subsequent AccessToken call would find
	//     no persisted access_token and would trigger a SECOND network round
	//     trip to the token endpoint — which, if it also fails, 502s a
	//     request despite a perfectly valid access token having just been
	//     obtained. memCache means Inject's call finds the freshly-obtained
	//     token without ever needing Phase 2's persist to have succeeded.
	//  2. A caller that arrives strictly AFTER another goroutine's refresh
	//     for the same (namespace, provider) already completed (and
	//     therefore missed singleflight's in-flight coalescing window
	//     entirely — singleflightGroup only coalesces OVERLAPPING calls, by
	//     design, the same as golang.org/x/sync/singleflight) still hits
	//     this cache instead of unconditionally re-refreshing, since the
	//     memCache entry and the just-computed expiresAt are already valid.
	//
	// Never treated as authoritative across a process restart (memCache
	// starts empty every time the daemon starts; cachedAccessToken falls
	// back to the secret store, which IS the cross-restart source of
	// truth) — this is purely a same-process optimization layered on top of
	// the persisted cache, never a substitute for it.
	memMu    sync.Mutex
	memCache map[string]oauthMemCacheEntry
}

// oauthMemCacheEntry is one OAuth2TokenSource.memCache entry. margin is the
// EFFECTIVE refresh margin for this specific token, computed once by
// refresh (at the moment expiresAt itself was computed) rather than
// re-derived from however much of the token's life happens to remain at
// whatever later moment cachedAccessToken is called — see refresh's own
// comment on why it must be computed that way, not derived from decaying
// "remaining time" at check time.
type oauthMemCacheEntry struct {
	accessToken string
	expiresAt   time.Time
	margin      time.Duration
}

// NewOAuth2TokenSource builds an OAuth2TokenSource from the daemon's
// configured OAuth2 providers and the resolver/writer pair used to read and
// persist each (namespace, provider)'s refresh_token/access_token/
// expires_at secret-store state. resolver is also used to look up a
// provider's ClientSecretKey (when set) — the same resolver
// CredentialProvider itself uses for the static auth kinds' secrets.
func NewOAuth2TokenSource(providers []OAuthProviderConfig, resolver SecretResolver, writer SecretWriter) *OAuth2TokenSource {
	m := make(map[string]OAuthProviderConfig, len(providers))
	for _, p := range providers {
		m[p.Name] = p
	}
	return &OAuth2TokenSource{
		providers:     m,
		resolver:      resolver,
		writer:        writer,
		RefreshMargin: DefaultOAuth2RefreshMargin,
		Now:           time.Now,
		memCache:      make(map[string]oauthMemCacheEntry),
		HTTPClient: &http.Client{
			Timeout: 20 * time.Second,
			// codex review round 1, Major finding: without an explicit
			// CheckRedirect, Go's default http.Client redirect policy
			// follows a 307/308 response (preserving method AND body, per
			// RFC 7538/7231) to WHATEVER scheme the Location header names —
			// including a downgrade from the config-validated https
			// token_endpoint to a plain-http URL. That would repost this
			// request's body (containing refresh_token and, for a
			// confidential client, client_secret) in cleartext to wherever
			// the redirect points, silently defeating
			// validateOAuthProviderConfig's own "token_endpoint must be
			// https" config-load guarantee (internal/config/apigateway.go)
			// the moment the token endpoint itself (or an on-path attacker
			// between this daemon and it) issues a redirect. Refusing every
			// redirect outright — an OAuth2 token endpoint has no
			// legitimate reason to redirect a POST at all — closes this
			// off entirely rather than trying to allow-list "same-scheme"
			// redirects. http.ErrUseLastResponse makes the Client return
			// the 3xx response itself as the final answer (no request ever
			// sent to the Location target), which callTokenEndpoint's
			// ordinary non-200 handling below already treats as a failure.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// AccessToken returns a currently-valid access token for (namespace,
// provider), proactively refreshing it first when there is no cached
// access_token yet, the cached expires_at cannot be parsed, or the cached
// token is within RefreshMargin of expiring (docs/plans/api-gateway.md §6
// "先回りリフレッシュ"). "No cached access_token yet" is the expected PR2
// dogfood starting state right after an operator seeds only a refresh_token
// via `boid secret set` (docs/plans/api-gateway.md PR分割 PR2) — not an
// error.
//
// Concurrent callers for the SAME (namespace, provider) that all decide a
// refresh is needed are coalesced into a single token-endpoint round trip
// via t.sf — see refresh's own doc comment for why only one of them actually
// performs it. The cache is re-checked a SECOND time once inside t.sf.Do,
// immediately before calling refresh (codex review round 2, Major finding):
// a caller can decide "needs refresh" from its OWN cache read, then stall
// (goroutine scheduling) long enough for a DIFFERENT caller's refresh for
// the same key to complete and release the singleflight slot entirely —
// singleflightGroup, like golang.org/x/sync/singleflight, only coalesces
// calls that overlap in time, never calls arriving strictly after a prior
// one already finished. Re-checking right before the network call closes
// that window down to the ordinary, industry-standard residual TOCTOU any
// check-then-act pattern has (not a new one this package introduces), and
// in particular avoids the double-refresh-within-one-request shape entirely
// for the common case: Resolve's own refresh already left a fresh entry in
// memCache by the time Inject's call reaches this same recheck.
func (t *OAuth2TokenSource) AccessToken(namespace, provider string) (string, error) {
	if t == nil {
		return "", fmt.Errorf("apigateway: no OAuth2 token source configured")
	}
	cfg, ok := t.providers[provider]
	if !ok {
		return "", fmt.Errorf("apigateway: oauth2 provider %q is not configured (oauth_providers: in config.yaml)", provider)
	}
	if t.resolver == nil {
		return "", fmt.Errorf("apigateway: oauth2 provider %q: no secret resolver configured", provider)
	}

	// Normalized ONCE, here, at the sole entry point every other method in
	// this file receives namespace from (codex review round 5, Major
	// finding): internal/dispatcher.SecretStore.normalizeNamespace treats ""
	// and "default" as the exact same underlying row, but this package's own
	// singleflight/memCache keys (namespace+"\x00"+provider) did not apply
	// that same normalization — so a caller passing "" (Registry.Entry.
	// Namespace's own documented shape for a workspace-unlinked project) and
	// a caller passing "default" (an explicit default-workspace-linked
	// project) were treated as two DIFFERENT keys despite reading/writing
	// the identical secret-store row, letting both trigger independent
	// concurrent refreshes against the same refresh_token — precisely the
	// rotation race the whole singleflight design exists to prevent.
	// Normalizing here means cachedAccessToken/refreshWithRecheck/refresh/
	// the singleflight key below, and the resolver/writer calls threaded
	// through them, all agree with the SecretStore's own notion of which
	// row they're touching.
	namespace = normalizeNamespace(namespace)

	if accessToken, expiresAt, margin, ok := t.cachedAccessToken(namespace, provider); ok {
		if freshEnough(t.now(), expiresAt, margin) {
			return accessToken, nil
		}
	}

	key := namespace + "\x00" + provider
	return t.sf.Do(key, func() (string, error) {
		return t.refreshWithRecheck(namespace, cfg)
	})
}

// refreshWithRecheck is the body of AccessToken's t.sf.Do closure, factored
// out so it can be exercised directly by a test that simulates "another
// goroutine's refresh already completed and released the singleflight slot
// before this call ever entered it" without needing to fabricate a genuine
// goroutine-scheduling race. Re-checks the cache (see AccessToken's own doc
// comment for why) and only calls refresh if it's still actually needed.
func (t *OAuth2TokenSource) refreshWithRecheck(namespace string, cfg OAuthProviderConfig) (string, error) {
	if accessToken, expiresAt, margin, ok := t.cachedAccessToken(namespace, cfg.Name); ok {
		if freshEnough(t.now(), expiresAt, margin) {
			return accessToken, nil
		}
	}
	return t.refresh(namespace, cfg)
}

func (t *OAuth2TokenSource) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t *OAuth2TokenSource) margin() time.Duration {
	if t.RefreshMargin > 0 {
		return t.RefreshMargin
	}
	return DefaultOAuth2RefreshMargin
}

// freshEnough reports whether a token expiring at expiresAt should still be
// treated as valid, given margin (this token's EFFECTIVE refresh margin —
// see oauthMemCacheEntry.margin's own doc comment for why the caller
// supplies this per-entry rather than freshEnough always using
// t.margin() directly). A pure function (no receiver) so it is trivially
// unit-testable on its own.
func freshEnough(now, expiresAt time.Time, margin time.Duration) bool {
	return now.Add(margin).Before(expiresAt)
}

func (t *OAuth2TokenSource) httpClient() *http.Client {
	if t.HTTPClient != nil {
		return t.HTTPClient
	}
	return http.DefaultClient
}

// cachedAccessToken returns a currently-cached access_token/expires_at (and
// the margin to check it against — see the returned value's own doc
// comment below) for (namespace, provider), checking the in-process
// memCache first (populated by refresh on every successful token-endpoint
// round trip, regardless of whether the secret-store persist below
// succeeded — see memCache's own doc comment) and falling back to the
// secret store (the cross-process-restart source of truth) when memCache
// has no entry. ok is false whenever neither source has a usable value or
// expires_at fails to parse as a Unix-seconds integer — both cases mean
// "treat as expired, refresh now" to AccessToken's caller, never a hard
// error.
//
// margin is t.margin() (the plain configured value) for a value read from
// the secret store — this fallback path has no way to know the token's
// original declared lifetime (the persisted cache holds access_token/
// expires_at, never expires_in itself), so it cannot apply
// oauthMemCacheEntry.margin's clamp; this only affects the FIRST check
// after a daemon restart for a genuinely short-lived-token provider (a
// narrow, self-correcting gap — see refresh's own doc comment on where the
// clamped margin actually gets computed and cached going forward).
func (t *OAuth2TokenSource) cachedAccessToken(namespace, provider string) (accessToken string, expiresAt time.Time, margin time.Duration, ok bool) {
	t.memMu.Lock()
	entry, found := t.memCache[namespace+"\x00"+provider]
	t.memMu.Unlock()
	if found {
		return entry.accessToken, entry.expiresAt, entry.margin, true
	}

	raw, err := t.resolver(namespace, OAuthSecretKey(provider, oauthFieldAccessTokenCache))
	if err != nil || raw == "" {
		return "", time.Time{}, 0, false
	}
	var cache oauthAccessTokenCache
	if err := json.Unmarshal([]byte(raw), &cache); err != nil || cache.AccessToken == "" {
		return "", time.Time{}, 0, false
	}
	return cache.AccessToken, time.Unix(cache.ExpiresAt, 0), t.margin(), true
}

// refresh performs exactly one refresh_token grant round trip for
// (namespace, cfg.Name) — never called directly by AccessToken; always
// through t.sf.Do, which coalesces every concurrent caller that decided a
// refresh was needed for the same (namespace, provider) key onto whichever
// goroutine's call actually runs this function. That IS docs/plans/
// api-gateway.md §6's singleflight requirement in full: this function has
// no locking of its own, because by the time it runs it is already the only
// in-flight refresh for this key.
//
// PERSISTENCE ORDER (docs/plans/api-gateway.md §6, load-bearing — do not
// reorder): once the token endpoint responds, the (possibly rotated)
// refresh_token is written to the secret store FIRST, and only once that
// write has succeeded does this function persist access_token/expires_at
// and return the access token for use. A provider that rotates
// refresh_token on every use (freee) invalidates the OLD refresh_token the
// instant it issues the new one — if the daemon crashed (or this write
// failed) between receiving the response and persisting the new
// refresh_token, the grant would be unrecoverable (the old value in the
// secret store no longer works upstream, and the new one was never saved)
// short of a full re-login. Persisting it first — and treating a failure to
// do so as fatal to this refresh attempt, even though the access token sits
// right there in the response — is what keeps that window as small as a
// single synchronous secret-store write, instead of spanning the network
// round trip AND every subsequent step of this function.
func (t *OAuth2TokenSource) refresh(namespace string, cfg OAuthProviderConfig) (string, error) {
	refreshToken, err := t.resolver(namespace, OAuthSecretKey(cfg.Name, oauthFieldRefreshToken))
	if err != nil || refreshToken == "" {
		return "", fmt.Errorf("apigateway: oauth2 provider %q: no refresh_token configured for namespace %q — run `boid secret set` (docs/plans/api-gateway.md PR2: manual grant) or complete login (PR3)", cfg.Name, namespace)
	}

	var clientSecret string
	if cfg.ClientSecretKey != "" {
		clientSecret, err = t.resolver(namespace, cfg.ClientSecretKey)
		if err != nil {
			return "", fmt.Errorf("apigateway: oauth2 provider %q: resolve client_secret_key %q (namespace %q): %w", cfg.Name, cfg.ClientSecretKey, namespace, err)
		}
	}

	resp, err := t.callTokenEndpoint(cfg, refreshToken, clientSecret)
	if err != nil {
		return "", fmt.Errorf("apigateway: oauth2 provider %q: refresh_token grant: %w", cfg.Name, err)
	}

	// Phase 1 (see doc comment above): refresh_token first, and fatal if it
	// fails — even though the new access_token is sitting right here in
	// `resp`, using (or persisting) it would mean handing out a token this
	// function could never reconstruct again after a crash, for a grant
	// whose recovery now depends entirely on this one write.
	//
	// Only actually WRITE when the provider genuinely rotated (resp.
	// RefreshToken non-empty AND different from what we already have) —
	// codex review round 3, Major finding: not every provider rotates
	// refresh_token on refresh (Google never does; freee always does — RFC
	// 6749 §5.1 makes the response field OPTIONAL specifically so a client
	// can tell "unchanged" from "rotated" apart), and an EARLIER version of
	// this function re-persisted the same, unchanged value unconditionally
	// on every refresh, treating THAT redundant write's failure as fatal
	// too. For a non-rotating provider, the value already durably stored is
	// still perfectly valid upstream — nothing about the grant is at risk —
	// so failing to re-persist an UNCHANGED value must never discard an
	// otherwise-successful refresh (a real access token in hand) and 502 the
	// request. Skipping the write entirely when unrotated also removes an
	// unnecessary secret-store write on every single refresh for the common
	// (non-rotating) case.
	if resp.RefreshToken != "" && resp.RefreshToken != refreshToken {
		if err := t.writer(namespace, OAuthSecretKey(cfg.Name, oauthFieldRefreshToken), resp.RefreshToken); err != nil {
			slog.Error("apigateway: oauth2 refresh_token persist failed after a successful token-endpoint round trip — the grant may now be UNRECOVERABLE if the provider rotates refresh_token on use; re-run login/`boid secret set` if subsequent refreshes fail",
				"provider", cfg.Name, "namespace", namespace, "error", err)
			return "", fmt.Errorf("apigateway: oauth2 provider %q: persist refreshed refresh_token: %w", cfg.Name, err)
		}
	}

	// Phase 2: access_token/expires_at are a cache, not the source of
	// truth — refresh_token above is already durably safe, so a failure
	// here is logged but not fatal to this call: this function still
	// returns the freshly obtained access token for THIS caller's immediate
	// use, and the next AccessToken() call simply finds no (or a stale)
	// cached access_token and refreshes again.
	expiresIn := time.Duration(resp.ExpiresIn) * time.Second
	if resp.ExpiresIn <= 0 {
		slog.Warn("apigateway: oauth2 token endpoint response omitted expires_in; falling back to a conservative default lifetime",
			"provider", cfg.Name, "namespace", namespace, "fallback", fallbackAccessTokenLifetime)
		expiresIn = fallbackAccessTokenLifetime
	}
	expiresAt := t.now().Add(expiresIn)

	// effectiveMargin (codex review round 2, Major finding): t.margin()
	// clamped to at most half of THIS token's own declared lifetime
	// (expiresIn), computed exactly once, here, at the moment expiresIn is
	// known — never re-derived later from however much of that lifetime
	// happens to remain at some future check (that alternative was tried
	// and reverted: it broke the ordinary case, since a NORMAL long-lived
	// token that has simply decayed down near t.margin() would then also
	// get its margin shrunk, defeating proactive refresh entirely — see
	// TestOAuth2TokenSource_WithinMargin_Refreshes). Computed once here,
	// this only ever shrinks the margin for a provider whose access-token
	// lifetime is itself shorter than (or comparable to) t.margin() (5
	// minutes by default — none of PR2's targeted providers are actually
	// this short-lived: freee is ~6h, Google/GitHub/Microsoft ~1h), so a
	// freshly-obtained such token is immediately usable rather than being
	// judged "already needs refresh" the instant it's minted — which would
	// otherwise force every single AccessToken call to refresh again (a
	// live-lock), and reproduce the same-request double-refresh shape
	// (Resolve then Inject, docs/plans/api-gateway.md §6) for exactly the
	// tokens short-lived enough to trigger it. For every normal-lifetime
	// token, expiresIn/2 is comfortably larger than t.margin(), so this is
	// a no-op and effectiveMargin == t.margin(), unchanged from before this
	// fix.
	effectiveMargin := t.margin()
	if halfLife := expiresIn / 2; halfLife < effectiveMargin {
		effectiveMargin = halfLife
	}

	// memCache is populated UNCONDITIONALLY here — before either
	// secret-store write below is even attempted, and regardless of whether
	// they succeed — so a Phase-2 persist failure never forces a same-
	// process caller (e.g. this same request's own Inject call, right after
	// Resolve's precheck triggered this very refresh) into a second,
	// possibly-failing network round trip for a token this process already
	// has in hand. See memCache's own doc comment (codex review round 1,
	// Major finding) for the full rationale; this is purely a same-process
	// optimization layered on top of the secret-store persistence below,
	// never a substitute for it — Phase 1 (refresh_token) above is still
	// what makes the grant itself durably safe.
	t.memMu.Lock()
	if t.memCache == nil {
		t.memCache = make(map[string]oauthMemCacheEntry)
	}
	t.memCache[namespace+"\x00"+cfg.Name] = oauthMemCacheEntry{accessToken: resp.AccessToken, expiresAt: expiresAt, margin: effectiveMargin}
	t.memMu.Unlock()

	// Single write, single key (oauthFieldAccessTokenCache's own doc
	// comment): access_token and expires_at can never land partially —
	// either this whole JSON blob is persisted, or the previous one (still
	// internally consistent) is left untouched.
	cacheJSON, err := json.Marshal(oauthAccessTokenCache{AccessToken: resp.AccessToken, ExpiresAt: expiresAt.Unix()})
	if err != nil {
		// Unreachable in practice (marshaling two plain fields never fails)
		// — defensive only, matching this package's other "should never
		// happen" fallbacks.
		slog.Warn("apigateway: oauth2 access_token cache marshal failed (refresh_token already safely persisted; not fatal — an in-process cache still has the fresh token)",
			"provider", cfg.Name, "namespace", namespace, "error", err)
	} else if err := t.writer(namespace, OAuthSecretKey(cfg.Name, oauthFieldAccessTokenCache), string(cacheJSON)); err != nil {
		slog.Warn("apigateway: oauth2 access_token cache persist failed (refresh_token already safely persisted; not fatal — an in-process cache still has the fresh token)",
			"provider", cfg.Name, "namespace", namespace, "error", err)
	}

	return resp.AccessToken, nil
}

// flexibleInt accepts a JSON number OR a JSON string containing digits for
// one field (expires_in): RFC 6749 §5.1 specifies a bare NUMBER, but some
// real-world token endpoints have been observed to quote it. Tolerating
// both here means a provider's exact encoding choice for this one field
// never turns into a decode error.
type flexibleInt int64

func (f *flexibleInt) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid expires_in %q: %w", s, err)
	}
	*f = flexibleInt(n)
	return nil
}

// oauthTokenResponse is the RFC 6749 §5.1 successful token response shape,
// restricted to the fields this package actually reads.
type oauthTokenResponse struct {
	AccessToken  string      `json:"access_token"`
	RefreshToken string      `json:"refresh_token,omitempty"`
	ExpiresIn    flexibleInt `json:"expires_in"`
	TokenType    string      `json:"token_type,omitempty"`
}

// oauthErrorResponse is the RFC 6749 §5.2 error response shape.
type oauthErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// rfc6749TokenErrorCodes is RFC 6749 §5.2's complete, fixed "error" enum for
// a token endpoint error response. callTokenEndpoint only ever echoes
// oerr.Error back to its caller (and, from there, potentially into
// Server.ServeHTTP's sandbox-facing 502 body — see callTokenEndpoint's own
// doc comment) when the value is a MEMBER of this exact set (codex review
// round 5, Major finding): oerr.Error is otherwise an arbitrary,
// unvalidated string straight from the response body — a non-compliant or
// compromised token endpoint (an external threat surface even in boid's
// single-user threat model: the attacker here is the remote OAuth2
// provider or an on-path party, never another local process) could stuff
// it with a hostname, PII, or anything else, and round 4's fix (dropping
// ErrorDescription) alone would not have caught THAT field carrying the
// same risk.
var rfc6749TokenErrorCodes = map[string]bool{
	"invalid_request":        true,
	"invalid_client":         true,
	"invalid_grant":          true,
	"unauthorized_client":    true,
	"unsupported_grant_type": true,
	"invalid_scope":          true,
}

// callTokenEndpoint performs the RFC 6749 §6 refresh_token grant: a POST to
// cfg.TokenEndpoint with an application/x-www-form-urlencoded body carrying
// grant_type=refresh_token, refresh_token, client_id, and (when configured)
// client_secret — the "client_secret_post"-style client authentication the
// providers docs/plans/api-gateway.md targets (freee, Google, GitHub,
// Microsoft) all accept for this grant, rather than HTTP Basic auth (RFC
// 6749 §2.3.1's alternative): keeping this package to the one shape avoids
// a per-provider auth-method config knob PR2 does not need yet.
//
// Deliberately never sends `scope`: RFC 6749 §6 allows a refresh request to
// narrow an already-granted scope, but doing so correctly is
// provider-specific territory neither of PR2's targeted providers needs
// (freee has no scope concept on refresh at all). cfg.Scopes is
// parsed-and-stored for PR3's authorization-URL construction only — see
// OAuthProviderConfig.Scopes' own doc comment.
//
// Every error path below is deliberately SANITIZED before it is returned
// (codex review round 4, Major finding): a raw Go net/http transport error
// (from httpClient().Do failing — DNS lookup, connection refused, TLS
// handshake, ...) typically embeds cfg.TokenEndpoint's real host:port
// verbatim in its error string, and this function's caller chain
// (refresh -> AccessToken -> CredentialProvider.Resolve/Inject) is exactly
// what Server.ServeHTTP's pre-existing fail-fast precheck echoes into the
// SANDBOX-facing 502 response body (`"bad gateway: ...: " + err.Error()`,
// internal/apigateway/server.go — a path PR1 already hardened against this
// exact leak for its own ReverseProxy.ErrorHandler, see
// TestServer_TransportErrorDoesNotLeakUpstreamHostToSandbox's own doc
// comment, but did not need to harden here since no static auth kind's
// Resolve error had ever contained an upstream hostname before oauth2
// existed). Directly contradicts docs/plans/api-gateway.md §1's "upstream
// の実 URL は sandbox からは見えない (見せる必要もない)" design principle.
// Every branch therefore logs the FULL raw detail via slog.Warn (server-side
// diagnosis only) and returns a generic, hostname-free message upward — the
// one exception is oerr.Error (RFC 6749 §5.2's fixed, non-secret error-code
// enum: invalid_grant, invalid_client, ...), kept in the returned message
// since it is genuinely useful to the caller and never contains a hostname
// or credential; oerr.ErrorDescription (a provider-authored free-text field
// with no such guarantee) is logged but never returned.
func (t *OAuth2TokenSource) callTokenEndpoint(cfg OAuthProviderConfig, refreshToken, clientSecret string) (oauthTokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	if cfg.ClientID != "" {
		form.Set("client_id", cfg.ClientID)
	}
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}

	req, err := http.NewRequest(http.MethodPost, cfg.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		slog.Warn("apigateway: oauth2 failed to build token endpoint request", "provider", cfg.Name, "error", err)
		return oauthTokenResponse{}, fmt.Errorf("build token request failed (see daemon log for details)")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := t.httpClient().Do(req)
	if err != nil {
		slog.Warn("apigateway: oauth2 token endpoint request failed", "provider", cfg.Name, "error", err)
		return oauthTokenResponse{}, fmt.Errorf("token endpoint request failed (see daemon log for details)")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		slog.Warn("apigateway: oauth2 failed to read token endpoint response", "provider", cfg.Name, "error", err)
		return oauthTokenResponse{}, fmt.Errorf("read token endpoint response failed (see daemon log for details)")
	}

	if resp.StatusCode != http.StatusOK {
		var oerr oauthErrorResponse
		if jsonErr := json.Unmarshal(body, &oerr); jsonErr == nil && oerr.Error != "" {
			slog.Warn("apigateway: oauth2 token endpoint returned an error response",
				"provider", cfg.Name, "status", resp.StatusCode, "error", oerr.Error, "error_description", oerr.ErrorDescription)
			// Only echo oerr.Error upward when it is a genuine RFC 6749
			// §5.2 enum member (rfc6749TokenErrorCodes' own doc comment) —
			// anything else (a non-compliant/compromised endpoint's
			// arbitrary string) falls through to the same generic message
			// the "no parseable error body" case gets, exactly as if this
			// field had been empty.
			if rfc6749TokenErrorCodes[oerr.Error] {
				return oauthTokenResponse{}, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, oerr.Error)
			}
			return oauthTokenResponse{}, fmt.Errorf("token endpoint returned %d", resp.StatusCode)
		}
		slog.Warn("apigateway: oauth2 token endpoint returned a non-200 status with no parseable error body",
			"provider", cfg.Name, "status", resp.StatusCode)
		return oauthTokenResponse{}, fmt.Errorf("token endpoint returned %d", resp.StatusCode)
	}

	var tokResp oauthTokenResponse
	if err := json.Unmarshal(body, &tokResp); err != nil {
		slog.Warn("apigateway: oauth2 failed to decode token endpoint response", "provider", cfg.Name, "error", err)
		return oauthTokenResponse{}, fmt.Errorf("decode token endpoint response failed (see daemon log for details)")
	}
	if tokResp.AccessToken == "" {
		return oauthTokenResponse{}, fmt.Errorf("token endpoint response has no access_token")
	}
	return tokResp, nil
}
