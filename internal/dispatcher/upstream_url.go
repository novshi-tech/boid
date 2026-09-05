package dispatcher

import (
	"fmt"
	"regexp"
	"strings"
)

// NormalizeOriginURL converts a git remote origin URL into the HTTPS form
// used as a project's upstream_url. HTTPS URLs are returned unchanged;
// scp-like SSH (`git@host:owner/repo.git`) and `ssh://` URLs are rewritten
// to `https://host/owner/repo.git` (`http://` is likewise upgraded to
// `https://`, reusing the same host/path extraction). Returns an error for
// an empty or unrecognized URL form.
//
// This is a pure function so it can be unit tested without a real git
// repository; CaptureUpstreamURL below composes it with the actual
// `git config` read.
func NormalizeOriginURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("empty origin url")
	}
	// Reject a URL with credentials embedded before it can be stored
	// verbatim into remote.origin.url / projects.upstream_url and echoed
	// back by every API response that reads UpstreamURL. Forge auth flows
	// through gateway.forges instead.
	if hasHTTPUserinfo(raw) {
		return "", fmt.Errorf("credential-embedded URL not supported; use gateway.forges configuration for authentication instead of embedding a token in the URL")
	}
	var result string
	switch {
	case strings.HasPrefix(raw, "https://"):
		result = raw
	// file:// is passed through unchanged too, alongside https:// — it
	// names a local filesystem path, not a forge host, so there is no
	// canonical https form to convert it to.
	case strings.HasPrefix(raw, "file://"):
		result = raw
	default:
		slug, err := repoSlugFromOriginURL(raw)
		if err != nil {
			return "", fmt.Errorf("normalize origin url %q: %w", SanitizeURLForLogging(raw), err)
		}
		result = "https://" + slug + ".git"
	}
	// hasHTTPUserinfo only inspects the raw input's authority component, so
	// it misses a credential smuggled into a query param or past the first
	// path separator; validateURLForClone catches those against the final
	// https:// or file:// form, after any scp-like/ssh login-user stripping,
	// so a legitimate ssh "git@host:..." login is never misclassified.
	if err := validateURLForClone(result); err != nil {
		return "", err
	}
	return result, nil
}

// credentialQueryParamPattern matches "<name>=<value>" query parameters
// whose name commonly carries a secret across various forge URL
// conventions (e.g. "?access_token=SECRET"). The captured group 1 keeps
// the "?"/"&" + key + "=" prefix intact so SanitizeURLForLogging can splice
// a placeholder in for the value alone.
var credentialQueryParamPattern = regexp.MustCompile(`(?i)([?&](?:access[_-]?token|token|password|oauth[_-]?token|api[_-]?key|client[_-]?secret|auth|secret|pat)=)[^&#\s]*`)

// urlCredentialUserinfoPattern matches a "user:pass@" or "user@" credential
// shape anywhere it appears in a URL string, not just immediately after a
// scheme://. Deliberately broad — any run of non-slash/non-@ characters
// immediately followed by "@" — since this only ever runs against the
// final https:// or file:// form a legitimate scp-like/ssh login prefix has
// already been stripped from.
var urlCredentialUserinfoPattern = regexp.MustCompile(`[^\s/@]+@`)

// SanitizeURLForLogging returns a copy of raw with every recognized
// credential shape (userinfo anywhere in the string, and any known
// credential-carrying query parameter) replaced with a redacted
// placeholder — safe to embed in an error message, log line, or persisted
// StatusMessage even when raw itself might carry a secret. Deliberately
// over-redacts rather than under-redacts — e.g. a legitimate scp-like
// "git@host:path" login prefix reads as "REDACTED@host:path" when run
// through this on a raw, not-yet-normalized string.
func SanitizeURLForLogging(raw string) string {
	out := urlCredentialUserinfoPattern.ReplaceAllString(raw, "REDACTED@")
	out = credentialQueryParamPattern.ReplaceAllString(out, "${1}REDACTED")
	return out
}

// validateURLForClone rejects u — always the final https:// or file:// form
// NormalizeOriginURL is about to return, never raw caller input — if it
// still carries a credential in any form. Rather than maintaining a growing
// enumeration of every known encoding trick, this re-runs
// SanitizeURLForLogging and rejects outright the moment the sanitized form
// differs from the input at all: anything credential-shaped is treated as
// a credential, even a form not specifically anticipated.
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
// embedded before the host (scheme://user[:pass]@host/...). Only http(s) is
// checked: the scp-like (git@host:owner/repo.git) and ssh:// forms also
// carry a "user@" prefix, but repoSlugFromOriginURL already strips it
// unconditionally as the SSH login user (conventionally "git", never a
// secret) before the https:// rewrite, so those two never reach here.
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
