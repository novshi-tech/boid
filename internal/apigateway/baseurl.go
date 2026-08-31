package apigateway

import (
	"fmt"
	"log/slog"
	"net/url"
)

// ValidateBaseURL validates rawURL for the safety properties every service's
// outbound upstream address must have (docs/plans/api-gateway.md §1/§2):
//   - an absolute URL with an explicit scheme and host
//   - no query string or fragment (Server always forwards the INBOUND
//     request's own RawQuery verbatim and never merges in anything from
//     this URL, and an HTTP fragment is never sent to a server at all —
//     RFC 3986 §3.5)
//   - https, unless allowInsecure is true (a plain-http scheme means an
//     injected credential crosses the network to the upstream in
//     cleartext — allowInsecure moves that acknowledgment into the config
//     document itself rather than a log line that may never be read)
//   - only http/https schemes even with allowInsecure set (this package's
//     outbound Transport only ever speaks http/https regardless of this
//     flag — "ftp"/"ws"/anything else would validate cleanly yet fail
//     every request at 502 once dispatched)
//
// This is the single implementation both call sites share:
// config.ValidateServiceURL (config-load time, for a literal base_url/
// endpoint — see that function's own doc comment, now a thin wrapper around
// this one) and CredentialProvider.BaseURLFor (request time, for a
// BaseURLSecretKey-backed base_url, docs/plans/api-gateway-credential-
// accounts.md D12 — a secret-store value cannot be checked until it is
// resolved, so this exact check runs once per resolution instead of once at
// `boid start`). Moved here (rather than staying config-only) because
// internal/apigateway cannot import internal/config (the dependency runs
// the other way — internal/config already imports internal/apigateway for
// AuthKind etc.), and duplicating slightly-different logic in two packages
// is worse than one implementation with two callers.
func ValidateBaseURL(serviceName, fieldName, rawURL string, allowInsecure bool) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("services[%q]: %q must be an absolute URL with a scheme and host (got %q)", serviceName, fieldName, rawURL)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("services[%q]: %q must not include a query string or fragment (got %q) — the sandbox's own query string is always forwarded as-is; use auth.kind: query for a fixed, injected query parameter instead", serviceName, fieldName, rawURL)
	}
	if u.Scheme != "https" && !allowInsecure {
		return fmt.Errorf(
			"services[%q]: %q scheme %q is not https, so this service's injected credential would cross the network in cleartext — "+
				"set \"allow_insecure: true\" on this service if that is intentional (e.g. an internal test/staging API with no TLS yet)",
			serviceName, fieldName, u.Scheme)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("services[%q]: %q scheme %q is not supported — the gateway only ever speaks http or https to the upstream, regardless of \"allow_insecure\"", serviceName, fieldName, u.Scheme)
	}
	if u.Scheme != "https" {
		slog.Warn("apigateway: URL does not use https — this service's injected credential will cross the network to the upstream in cleartext",
			"service", serviceName, "field", fieldName, "url", rawURL, "scheme", u.Scheme)
	}
	return nil
}
