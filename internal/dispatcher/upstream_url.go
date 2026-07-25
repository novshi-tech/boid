package dispatcher

import (
	"fmt"
	"regexp"
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
	var result string
	switch {
	case strings.HasPrefix(raw, "https://"):
		result = raw
	// file:// is passed through unchanged too, alongside https:// — unlike
	// the scp-like/ssh/http forms below, there is no "canonical https form"
	// to convert it to (it names a local filesystem path, not a forge
	// host), so the only sensible normalization is none. Primarily useful
	// for daemon-side testing of the git-URL project model (docs/plans/
	// volume-only-daemon.md §論点a: CreateProjectFromGitURL normalizes via
	// this function before cloning) against a local fixture repo instead of
	// a real forge, but also a legitimate git URL scheme in its own right
	// (e.g. an NFS-shared bare repo mirror).
	case strings.HasPrefix(raw, "file://"):
		result = raw
	default:
		slug, err := repoSlugFromOriginURL(raw)
		if err != nil {
			return "", fmt.Errorf("normalize origin url %q: %w", SanitizeURLForLogging(raw), err)
		}
		result = "https://" + slug + ".git"
	}
	// BLOCKER 1 (PR-2a codex round-2 review): hasHTTPUserinfo above only
	// ever inspected the authority component (everything up to the first
	// "/" after the scheme) of the RAW input — it never saw a credential
	// smuggled in as a query parameter (?access_token=...), nor one placed
	// past the first path separator (e.g.
	// "https://host/x-oauth-basic:TOKEN@repo.git", where the "@" lands
	// inside what hasHTTPUserinfo already believed was the path). Both
	// bypassed registration outright: the credential-bearing URL got
	// clone-attempted (leaking it into the clone command's argv on
	// failure) and, on success, stored verbatim into remote.origin.url AND
	// projects.upstream_url. validateURLForClone runs against the FINAL
	// https:// or file:// form (after any scp-like/ssh login-user stripping
	// above), so a legitimate ssh "git@host:..." login is never
	// misclassified — only a credential surviving all the way to what will
	// actually be stored/cloned is rejected.
	if err := validateURLForClone(result); err != nil {
		return "", err
	}
	return result, nil
}

// credentialQueryParamPattern matches "<name>=<value>" query parameters
// whose name commonly carries a secret across various forge URL
// conventions (BLOCKER 1, PR-2a codex round-2 review — "URL query" bypass:
// "https://host/repo.git?access_token=SECRET" sails straight through
// hasHTTPUserinfo, which never looks at the query string at all). The
// captured group 1 keeps the "?"/"&" + key + "=" prefix intact so
// SanitizeURLForLogging can splice a placeholder in for the value alone.
var credentialQueryParamPattern = regexp.MustCompile(`(?i)([?&](?:access[_-]?token|token|password|oauth[_-]?token|api[_-]?key|client[_-]?secret|auth|secret|pat)=)[^&#\s]*`)

// urlCredentialUserinfoPattern matches a "user:pass@" or "user@" credential
// shape ANYWHERE it appears in a URL string — not just immediately after a
// scheme:// (BLOCKER 1's "URL path" bypass:
// "https://host/x-oauth-basic:TOKEN@repo.git" places the "@" past the
// first "/", exactly where hasHTTPUserinfo already stopped looking, since
// it cuts the authority off at the first "/" and never re-inspects
// anything after it). Deliberately broad — any run of non-slash/non-@
// characters immediately followed by "@" — since this is only ever run
// against the FINAL https:// or file:// form a legitimate scp-like/ssh
// login prefix has already been stripped from (see NormalizeOriginURL's
// call site), so there is no legitimate reason for one of those forms to
// contain this shape anywhere at all.
var urlCredentialUserinfoPattern = regexp.MustCompile(`[^\s/@]+@`)

// SanitizeURLForLogging returns a copy of raw with every recognized
// credential shape (userinfo anywhere in the string, and any known
// credential-carrying query parameter) replaced with a redacted
// placeholder — safe to embed in an error message, log line, or persisted
// StatusMessage even when raw itself might carry a secret (PR-2a codex
// round-2 Blocker 1 sibling sweep: every site that echoes a caller-supplied
// or not-yet-validated URL back into an error/log routes through this
// instead of interpolating the raw string directly). Deliberately
// over-redacts rather than under-redacts — e.g. a legitimate scp-like
// "git@host:path" login prefix reads as "REDACTED@host:path" when run
// through this on a raw, not-yet-normalized string — since this function's
// only job is "never let a real secret through," not "produce a pretty
// error message."
func SanitizeURLForLogging(raw string) string {
	out := urlCredentialUserinfoPattern.ReplaceAllString(raw, "REDACTED@")
	out = credentialQueryParamPattern.ReplaceAllString(out, "${1}REDACTED")
	return out
}

// validateURLForClone rejects u — which by the time this runs is ALWAYS
// the final https:// or file:// form NormalizeOriginURL is about to return,
// never raw caller input — if it still carries a credential in any form
// (BLOCKER 1, PR-2a codex round-2 review). Rather than maintaining a
// growing enumeration of every known encoding trick, this re-runs
// SanitizeURLForLogging (the exact same redaction every logging/error site
// below relies on) and rejects outright the moment the sanitized form
// differs from the input at all — "this looks credential-shaped in some
// way we did not specifically anticipate" is treated identically to "this
// is definitely a credential," per codex round-2's explicit request for
// defense against unknown/future encoding forms rather than only the ones
// enumerated so far.
//
// file:// is exempt: it names a local filesystem path, not a forge host —
// there is no credential surface to protect there, and rejecting a path
// that happens to contain "@" (an unusual but legitimate directory/user
// name) would be a pure false positive.
func validateURLForClone(u string) error {
	if strings.HasPrefix(u, "file://") {
		return nil
	}
	if sanitized := SanitizeURLForLogging(u); sanitized != u {
		return fmt.Errorf("credential-embedded URL not supported (%s); use gateway.forges configuration for authentication instead of embedding a token in the URL", sanitized)
	}
	return nil
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
