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

// Resolve determines which daemon this CLI invocation should talk to,
// applying the precedence chain from decision 1 (docs/plans/
// cli-remote-connection.md): --profile flag > BOID_PROFILE env >
// default_profile > 現行互換 (unix://DefaultSocketPath()).
//
// cmd may be nil (unit tests exercising the env/default/fallback tiers
// without a real cobra invocation); a nil cmd is simply treated as
// "--profile was not passed".
func Resolve(cmd *cobra.Command) (*ResolvedProfile, error) {
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

	resolved := &ResolvedProfile{Name: name, URL: prof.URL, Source: source}

	u, err := url.Parse(prof.URL)
	if err != nil {
		return nil, fmt.Errorf("profile %q: invalid url %q: %w", name, prof.URL, err)
	}
	// Reject unsupported schemes BEFORE reaching LoadToken: otherwise an
	// `http://...` or `ftp://...` profile with no token file would surface
	// the confusing "run 'boid login <url>' first" message instead of the
	// real cause (scheme unsupported at all — decision 4). Same for any
	// other unknown scheme.
	switch u.Scheme {
	case "unix":
		// decision 4: a unix-scheme profile never needs a token.
		return resolved, nil
	case "https":
		// fall through to token loading below.
	default:
		return nil, fmt.Errorf("profile %q: unsupported url scheme %q (want \"unix\" or \"https\")", name, u.Scheme)
	}

	tok, err := LoadToken(name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no device token for profile %q; run 'boid login <url>' first", name)
		}
		return nil, err
	}
	// decision 9: the token is strongly bound to the canonical origin it was
	// issued against. A mismatch — config.yaml's profile URL was edited, or
	// the daemon's canonical URL changed since login — is a hard error, not
	// a warning: silently sending a token for origin A to whatever origin B
	// config.yaml now names would be exactly the kind of cross-origin token
	// reuse decision 9 exists to rule out.
	if tok.URL != prof.URL {
		return nil, fmt.Errorf("profile %q URL mismatch: config=%s, token=%s (re-login required)", name, prof.URL, tok.URL)
	}
	resolved.Token = tok.Token
	return resolved, nil
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
