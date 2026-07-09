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
	if strings.HasPrefix(raw, "https://") {
		return raw, nil
	}
	slug, err := repoSlugFromOriginURL(raw)
	if err != nil {
		return "", fmt.Errorf("normalize origin url %q: %w", raw, err)
	}
	return "https://" + slug + ".git", nil
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
