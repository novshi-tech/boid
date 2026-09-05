package apigateway

import (
	"fmt"
	"log/slog"
	"net/url"
)

// ValidateBaseURL validates that rawURL is safe to use as a service's
// outbound upstream address: an absolute URL with scheme and host, no query
// string or fragment, https unless allowInsecure is set, and only
// http/https schemes even then.
//
// Shared by config.ValidateServiceURL (config-load time) and
// CredentialProvider.BaseURLFor (request time, for a secret-backed
// base_url); keep both call sites in sync if this changes.
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
