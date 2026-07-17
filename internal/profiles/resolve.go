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
	SourceUnixFallback   = "unix-fallback"
)

// ResolvedProfile is the outcome of Resolve: everything client.NewClient
// needs to build the actual transport, plus Name/Source for diagnostics
// and error messages.
type ResolvedProfile struct {
	// Name is the profile's slug, or "" for the unix-fallback case (no
	// profile was ever selected by name).
	Name string
	// URL is always a "unix://" or "https://" url.NewClient can consume
	// directly.
	URL string
	// Token is the Bearer device token for an https-scheme profile, or ""
	// for a unix-scheme one (unix never needs a token — decision 4).
	Token string
	// Source records which precedence tier decided Name (SourceFlag /
	// SourceEnv / SourceDefaultProfile / SourceUnixFallback).
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
// default_profile > 現行互換 (unix://DefaultSocketPath()). For https-scheme
// profiles it also loads and validates the Bearer device token
// (decision 9 origin-bind check).
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
			// a silent request to fall back to the unix default. Falling
			// back would swallow the caller's mistake AND skip slug
			// validation, so hard-fail instead.
			return nil, fmt.Errorf("--profile requires a non-empty value")
		}
		return &ResolvedProfile{
			URL:    (&url.URL{Scheme: "unix", Path: client.DefaultSocketPath()}).String(),
			Source: SourceUnixFallback,
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

	return &ResolvedProfile{Name: name, URL: prof.URL, Source: source}, nil
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
