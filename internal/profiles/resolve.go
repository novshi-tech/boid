package profiles

import (
	"fmt"
	"net/url"
	"os"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/spf13/cobra"
)

// ProfileFlagName is the --profile persistent flag cmd/root.go registers on
// rootCmd. Exported so cmd/root.go and this package agree on the flag name
// without either hard-coding a string literal the other could drift from.
const ProfileFlagName = "profile"

// BOIDProfileEnv is the environment variable checked between --profile and
// default_profile (decision 1, docs/plans/cli-remote-connection.md).
const BOIDProfileEnv = "BOID_PROFILE"

// Source values explain, for diagnostics, which precedence tier a
// ResolvedProfile came from.
const (
	SourceFlag           = "flag"
	SourceEnv            = "env"
	SourceDefaultProfile = "default_profile"
	// SourceUnixFallback names the terminal fallback tier when a local
	// UNIX socket is actually reachable at client.DefaultSocketPath()
	// (PR-3 codex round-1 review, "design pivot" — nose decision, 2026-07-25):
	// no --profile flag, no BOID_PROFILE, no default_profile in
	// config.yaml at all, AND the default socket file exists. This is the
	// pre-§論点c behavior, restored: the userns backend (bare `boid
	// start`, every Black-box E2E scenario) still has a host-visible
	// socket, so the CLI keeps dialing it directly with no TLS/token
	// machinery at all — see IsUnix()/Resolve's own no-token branch.
	SourceUnixFallback = "unix-fallback"
	// SourceTCPFallback names the OTHER half of the terminal fallback
	// tier — same "nothing configured at all" precondition as
	// SourceUnixFallback above, but client.DefaultSocketPath() has no
	// file on disk (a named-volume/compose daemon deployment, whose
	// filesystem the host cannot see — §論点c's own "問題" section — or a
	// bare-metal host where no daemon has ever been started yet). Falls
	// back to client.DefaultCLIAddr() over TCP+TLS instead — the "just
	// works, no `boid login` step" default §論点c's loopback-trust design
	// provides. A hand-configured "unix://" profile (named, via
	// default_profile/--profile/BOID_PROFILE) is unaffected by any of
	// this either way — see IsUnix().
	//
	// docs/plans/volume-only-daemon.md's original §論点c PR-3 shipped a
	// "full cutover" (terminal fallback ALWAYS TCP, unconditionally) —
	// codex round-1 review flagged this broke every Black-box E2E
	// scenario (userns backend, unix socket, no daemon-side TCP CA/token
	// bootstrap) and the container backend's own compose-published-port
	// loopback-trust story (Docker/Podman DNAT replaces RemoteAddr with
	// the bridge gateway, never genuine 127.0.0.1 — see
	// build/container/compose.yml's `network_mode: host` fix for that
	// half). nose's resulting design pivot: keep BOTH transports live,
	// auto-detected by which one is actually reachable, rather than a
	// one-way cutover.
	SourceTCPFallback = "tcp-fallback"
)

// ResolvedProfile is the outcome of Resolve: everything client.NewClient
// needs to build the actual transport, plus Name/Source for diagnostics
// and error messages.
type ResolvedProfile struct {
	// Name is the profile's slug, or "" for the terminal-fallback case (no
	// profile was ever selected by name).
	Name string
	// URL is always a "unix://" or "https://" url.NewClient can consume
	// directly.
	URL string
	// Token is the Bearer device token for an https-scheme profile, or ""
	// for a unix-scheme one (unix never needs a token — decision 4), the
	// TCP terminal fallback (SourceTCPFallback — loopback trust means no
	// token is needed either, docs/plans/volume-only-daemon.md §論点c), or
	// a NAMED loopback-hosted https profile with no matching token file on
	// disk (isLoopbackURL — same loopback-trust reasoning, best-effort
	// rather than required; see Resolve's own isLoopbackURL branch).
	Token string
	// CACert is an inline PEM cert (from the named profile's own CACert
	// field) or the well-known bootstrap file's contents
	// (client.DefaultCACertPath(), for SourceTCPFallback) to trust as an
	// ADDITIONAL root CA when dialing URL — see client.NewClientWithCACert.
	// Empty for a unix-scheme profile (never consulted) and for any
	// https-scheme profile that never set ca_cert (the original,
	// system-pool-only contract — docs/plans/cli-remote-connection.md's
	// remote daemons carry publicly-trusted certificates and need none of
	// this).
	CACert string
	// Source records which precedence tier decided Name (SourceFlag /
	// SourceEnv / SourceDefaultProfile / SourceUnixFallback /
	// SourceTCPFallback).
	Source string
}

// IsUnix reports whether the resolved profile targets a UNIX socket (as
// opposed to an https-scheme remote daemon). Handy for callers that need
// this discrimination BEFORE a Token has been loaded — e.g. root's
// PersistentPreRunE checking scope=local rejection (decision 6) against
// a ResolveWithoutToken result, where a token-load failure on an
// unrelated https profile would otherwise pre-empt the intended
// "ローカル専用コマンドだよ" error.
func (rp *ResolvedProfile) IsUnix() bool {
	if rp == nil {
		return false
	}
	u, err := url.Parse(rp.URL)
	if err != nil {
		return false
	}
	return u.Scheme == "unix"
}

// Resolve determines which daemon this CLI invocation should talk to,
// applying the precedence chain from decision 1 (docs/plans/
// cli-remote-connection.md): --profile flag > BOID_PROFILE env >
// default_profile > terminal fallback (docs/plans/volume-only-daemon.md
// §論点c, auto-detected between unix://DefaultSocketPath() and
// https://client.DefaultCLIAddr() — see SourceUnixFallback/
// SourceTCPFallback's own doc comments for the detection rule). For a
// NAMED https-scheme profile it also loads and validates the Bearer device
// token (decision 9 origin-bind check) — best-effort (no hard requirement)
// for a loopback-hosted one (see the isLoopbackURL branch below), and
// skipped entirely for the terminal TCP fallback (Name == "", nothing to
// look a token file up under) same as a unix-scheme profile.
//
// cmd may be nil (unit tests exercising the env/default/fallback tiers
// without a real cobra invocation); a nil cmd is simply treated as
// "--profile was not passed".
//
// Callers that need the profile identity BEFORE running the token-load
// step — root's PersistentPreRunE, in particular, so a scope=local
// rejection (decision 6) fires *before* an unrelated
// missing/corrupt-token error preempts it — should call
// ResolveWithoutToken instead and only reach for the token when they've
// decided the invocation is actually going to proceed.
func Resolve(cmd *cobra.Command) (*ResolvedProfile, error) {
	resolved, err := ResolveWithoutToken(cmd)
	if err != nil {
		return nil, err
	}
	if resolved.IsUnix() {
		// decision 4: a unix-scheme profile never needs a token.
		return resolved, nil
	}
	if resolved.Source == SourceTCPFallback {
		// The terminal TCP fallback has no profile name to look up a token
		// file under (Name == "") and needs none anyway — its whole point
		// (docs/plans/volume-only-daemon.md §論点c) is loopback trust: a
		// same-host caller reaches the daemon's dedicated CLI TLS listener
		// with no Bearer/cookie required at all (internal/api/auth's
		// web.loopback_trust-gated NewTCPAPIAuthMiddleware). Falling
		// through to the LoadToken call below would look up a token file
		// for the empty profile name and always fail with a misleading
		// "run 'boid login' first" — this fallback is not something a user
		// ever logs into.
		return resolved, nil
	}
	if isLoopbackURL(resolved.URL) {
		// PR-3 codex round-1 review Blocker ("default_profile requires
		// device token"): a NAMED https profile pointed at a loopback host
		// — e.g. deploy-container.sh's compose-seeded default_profile,
		// `https://127.0.0.1:8442` — needs no device token any more than
		// the anonymous terminal TCP fallback above does. The daemon's own
		// loopback-trust middleware (auth.NewTCPAPIAuthMiddleware,
		// web.loopback_trust) authenticates by RemoteAddr alone once the
		// request actually arrives; requiring a LOCAL token FILE to exist
		// here — before ever attempting the request — just because this
		// profile happens to have a name (unlike the terminal fallback)
		// was the actual round-1 Blocker: a fresh compose deploy's first
		// `boid task list` failed with `no device token for profile
		// "default"` before making any request at all.
		//
		// Best-effort, not exclusive: if a token file for this profile DOES
		// exist (an operator explicitly ran `boid login` against it at some
		// point — e.g. to carry device identity across a future
		// `web.loopback_trust: false` toggle), still load and send it, the
		// same origin-bind check as the named-profile branch below. A
		// missing file or a URL mismatch is silently ignored here (not a
		// hard error) — unlike that branch — because loopback trust means
		// this profile works with no token at all; a stale/mismatched
		// token file must not turn a working, tokenless request into a
		// hard failure.
		if tok, err := LoadToken(resolved.Name); err == nil && tok.URL == resolved.URL {
			resolved.Token = tok.Token
		}
		return resolved, nil
	}
	tok, err := LoadToken(resolved.Name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no device token for profile %q; run 'boid login <url>' first", resolved.Name)
		}
		return nil, err
	}
	// decision 9: the token is strongly bound to the canonical origin it was
	// issued against. A mismatch — config.yaml's profile URL was edited, or
	// the daemon's canonical URL changed since login — is a hard error, not
	// a warning: silently sending a token for origin A to whatever origin B
	// config.yaml now names would be exactly the kind of cross-origin token
	// reuse decision 9 exists to rule out.
	if tok.URL != resolved.URL {
		return nil, fmt.Errorf("profile %q URL mismatch: config=%s, token=%s (re-login required)", resolved.Name, resolved.URL, tok.URL)
	}
	resolved.Token = tok.Token
	return resolved, nil
}

// ResolveWithoutToken performs the profile-selection half of Resolve (config
// load, precedence chain, slug validation, URL scheme validation) but
// deliberately stops SHORT of loading the Bearer device token. The
// returned ResolvedProfile has Token == "" even for an https-scheme
// profile that would normally need one.
//
// This exists so root.PersistentPreRunE can enforce decision 6
// (scope=local + non-unix profile → hard error) *before* a
// scope-independent failure — a missing/corrupt token file on the
// resolved https profile — has a chance to preempt it with a "run 'boid
// login' first" error that would mislead the caller into thinking token
// state is the actual problem.
func ResolveWithoutToken(cmd *cobra.Command) (*ResolvedProfile, error) {
	cfgPath, err := ConfigPath()
	if err != nil {
		return nil, err
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		return nil, err
	}

	name, source := selectProfileName(cmd, cfg)
	if name == "" {
		if source == SourceFlag {
			// An explicit `--profile=` (empty value) is a caller error, not
			// a silent request to fall back to the terminal default.
			// Falling back would swallow the caller's mistake AND skip
			// slug validation, so hard-fail instead.
			return nil, fmt.Errorf("--profile requires a non-empty value")
		}
		// Terminal fallback: no --profile, no BOID_PROFILE, no
		// default_profile in config.yaml at all. nose's PR-3 codex
		// round-1 design pivot (see SourceUnixFallback/SourceTCPFallback's
		// own doc comments above): auto-detect which transport is
		// actually reachable rather than a one-way TCP cutover — a
		// client.DefaultSocketPath() file on disk means a same-host
		// daemon (userns backend, bare `boid start`) is (or recently was)
		// listening there, exactly the pre-§論点c unix:// default; its
		// absence means either a named-volume/compose daemon (whose
		// filesystem the host cannot see at all) or a bare-metal host
		// that has simply never started a daemon yet — both fall back to
		// client.DefaultCLIAddr() over TCP+TLS, same as before this
		// pivot.
		//
		// This is a plain os.Stat, not a liveness probe: a STALE socket
		// file left behind by a crashed daemon still resolves to unix
		// here (matching pre-§論点c behavior byte-for-byte) — the actual
		// liveness/autostart decision is client.EnsureRunningAt's job
		// (root's PersistentPreRunE calls it once IsUnix() is true),
		// which already probes-then-autostarts on a genuinely dead
		// socket. Duplicating a liveness dial here would only race that
		// same logic for no benefit.
		if socketPath := client.DefaultSocketPath(); unixSocketFileExists(socketPath) {
			return &ResolvedProfile{
				URL:    (&url.URL{Scheme: "unix", Path: socketPath}).String(),
				Source: SourceUnixFallback,
			}, nil
		}
		// CACert is loaded best-effort from client.DefaultCACertPath() (a
		// bare `boid start` writes it on every boot; `boid start
		// --print-cli-profile` + deploy-container.sh seed it for the
		// compose deployment) — a missing file just leaves CACert empty,
		// which client.NewClientWithCACert treats as "system pool only",
		// failing the eventual TLS handshake with an ordinary (if less
		// specific) certificate-verification error rather than refusing
		// to even attempt the dial here.
		caCert := ""
		if caCertPath, pathErr := client.DefaultCACertPath(); pathErr == nil {
			if data, readErr := os.ReadFile(caCertPath); readErr == nil {
				caCert = string(data)
			}
		}
		return &ResolvedProfile{
			URL:    (&url.URL{Scheme: "https", Host: client.DefaultCLIAddr()}).String(),
			CACert: caCert,
			Source: SourceTCPFallback,
		}, nil
	}

	if err := ValidateSlug(name); err != nil {
		return nil, err
	}

	prof, ok := cfg.Profiles[name]
	if !ok {
		return nil, fmt.Errorf("profile %q is not defined in %s", name, cfgPath)
	}

	u, err := url.Parse(prof.URL)
	if err != nil {
		return nil, fmt.Errorf("profile %q: invalid url %q: %w", name, prof.URL, err)
	}
	// Reject unsupported schemes now — running the scope=local check on
	// an ftp:// / http:// profile would still return the wrong diagnostic
	// (a scope-based rejection for a shape that fundamentally cannot be
	// a boid daemon URL at all — decision 4).
	switch u.Scheme {
	case "unix", "https":
		// supported
	default:
		return nil, fmt.Errorf("profile %q: unsupported url scheme %q (want \"unix\" or \"https\")", name, u.Scheme)
	}

	return &ResolvedProfile{Name: name, URL: prof.URL, CACert: prof.CACert, Source: source}, nil
}

// selectProfileName applies the --profile / BOID_PROFILE / default_profile
// tiers (decision 1's first three precedence levels — the fourth, 現行互換,
// is Resolve's own empty-name fallback). Returns ("", "") when none of the
// three apply. When --profile was passed but with an empty value, returns
// ("", SourceFlag) so Resolve can distinguish it from the fallback case
// and hard-error instead of silently downgrading to the unix default.
func selectProfileName(cmd *cobra.Command, cfg *Config) (name, source string) {
	if cmd != nil {
		if f := cmd.Flags().Lookup(ProfileFlagName); f != nil && f.Changed {
			return f.Value.String(), SourceFlag
		}
	}
	if v := os.Getenv(BOIDProfileEnv); v != "" {
		return v, SourceEnv
	}
	if cfg.DefaultProfile != "" {
		return cfg.DefaultProfile, SourceDefaultProfile
	}
	return "", ""
}

// unixSocketFileExists reports whether socketPath names an existing
// filesystem entry — used by ResolveWithoutToken's terminal-fallback
// auto-detect (SourceUnixFallback vs SourceTCPFallback) to decide which
// transport a same-host daemon is actually reachable over. Deliberately a
// plain os.Stat, not a liveness dial: see the terminal-fallback branch's
// own doc comment for why a stale file still counts as "unix" here.
func unixSocketFileExists(socketPath string) bool {
	_, err := os.Stat(socketPath)
	return err == nil
}

// isLoopbackURL reports whether rawURL's hostname is one of the literal
// loopback names/addresses the daemon's own loopback-trust middleware
// (auth.IsLoopback, internal/api/auth/api_middleware.go) and CLI TLS
// listener cert SANs recognize (internal/server's ServerOnlyTLSConfig("
// 127.0.0.1", "localhost") call) — used by Resolve's NAMED-profile branch
// to decide whether a token is required at all (see its own call site's
// doc comment). A parse failure or any other host is "not loopback", the
// safe default (falls through to requiring a token as before).
func isLoopbackURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "127.0.0.1", "::1", "localhost":
		return true
	default:
		return false
	}
}
