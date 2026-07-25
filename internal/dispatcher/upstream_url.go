package dispatcher

import (
	"fmt"
	"strings"
)

// NormalizeOriginURL converts a git remote origin URL into the HTTPS form
// used as a project's upstream_url (docs/plans/git-gateway-cutover.md PR2:
// "project → 上流 URL の明示マッピング"). HTTPS URLs are returned unchanged
// ("既に HTTPS URL ならそのまま"); scp-like SSH (`git@host:owner/repo.git`)
// and `ssh://` URLs are rewritten to `https://host/owner/repo.git`
// (`http://` is likewise upgraded to `https://`, reusing the same host/path
// extraction). Returns an error for an empty or unrecognized URL form.
//
// This is a pure function so it can be unit tested without a real git
// repository; CaptureUpstreamURL below composes it with the actual
// `git config` read.
func NormalizeOriginURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("empty origin url")
	}
	// MAJOR 5 (PR-2a codex round-1 review): reject an http(s) URL with
	// userinfo embedded (https://user:pat@host/org/repo.git) outright,
	// checked BEFORE the https:// passthrough / repoSlugFromOriginURL
	// upgrade below — accepting one would store the credential verbatim in
	// three places: the daemon-managed bare repo's own remote.origin.url
	// (dispatcher.CloneBareRepo stores rawURL as-is into the clone
	// destination, unlike credentialGitArgs' transient, process-local
	// header injection), the projects.upstream_url DB column, and every API
	// response / `boid project list` line that echoes UpstreamURL back.
	// Forge authentication is meant to flow through gateway.forges
	// configured server-side, never embedded in the URL string itself.
	if hasHTTPUserinfo(raw) {
		return "", fmt.Errorf("credential-embedded URL not supported; use gateway.forges configuration for authentication instead of embedding a token in the URL")
	}
	if strings.HasPrefix(raw, "https://") {
		return raw, nil
	}
	// file:// is passed through unchanged too, alongside https:// — unlike
	// the scp-like/ssh/http forms below, there is no "canonical https form"
	// to convert it to (it names a local filesystem path, not a forge
	// host), so the only sensible normalization is none. Primarily useful
	// for daemon-side testing of the git-URL project model (docs/plans/
	// volume-only-daemon.md §論点a: CreateProjectFromGitURL normalizes via
	// this function before cloning) against a local fixture repo instead of
	// a real forge, but also a legitimate git URL scheme in its own right
	// (e.g. an NFS-shared bare repo mirror).
	if strings.HasPrefix(raw, "file://") {
		return raw, nil
	}
	slug, err := repoSlugFromOriginURL(raw)
	if err != nil {
		return "", fmt.Errorf("normalize origin url %q: %w", raw, err)
	}
	return "https://" + slug + ".git", nil
}

// hasHTTPUserinfo reports whether rawURL is an http(s) URL with userinfo
// embedded before the host (scheme://user[:pass]@host/...) — MAJOR 5,
// PR-2a codex round-1 review. Only http(s) is checked: the scp-like
// (git@host:owner/repo.git) and ssh:// forms also carry a "user@" prefix,
// but repoSlugFromOriginURL already strips it unconditionally as the SSH
// login user (conventionally "git", never a secret) before the https://
// rewrite — see that function's ssh:// and scp-like cases — so those two
// never reach here carrying anything through to reject.
func hasHTTPUserinfo(rawURL string) bool {
	var rest string
	switch {
	case strings.HasPrefix(rawURL, "https://"):
		rest = strings.TrimPrefix(rawURL, "https://")
	case strings.HasPrefix(rawURL, "http://"):
		rest = strings.TrimPrefix(rawURL, "http://")
	default:
		return false
	}
	hostPart, _, _ := strings.Cut(rest, "/")
	return strings.Contains(hostPart, "@")
}

// CaptureUpstreamURL reads dir's `git config --get remote.origin.url` and
// normalizes it to an HTTPS URL suitable for a project's upstream_url.
// Returns an error if dir has no git repository, no origin remote is
// configured, or the origin URL is in an unrecognized form — the caller
// (project registration / reload / startup backfill) decides how to react.
func CaptureUpstreamURL(dir string) (string, error) {
	raw, err := GitOriginURL(dir)
	if err != nil {
		return "", err
	}
	return NormalizeOriginURL(raw)
}
