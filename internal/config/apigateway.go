package config

import (
	"fmt"
	"net/url"
	"sort"

	"github.com/novshi-tech/boid/internal/apigateway"
)

// ServiceAuthConfig configures how the API gateway authenticates requests
// to one service. docs/plans/api-gateway.md §2 ("service registry
// (config.yaml)") / §4 ("credential 注入"). Which fields are required
// depends on Kind — see validateServiceConfig.
type ServiceAuthConfig struct {
	// Kind selects the injection strategy: "bearer" / "basic" / "header" /
	// "query" (implemented, PR1) or "oauth2" (reserved — schema only, PR2
	// adds the actual TokenSource; docs/plans/api-gateway.md PR1 scope note).
	Kind string `yaml:"kind"`
	// SecretKey is a reference into the secret store
	// (internal/dispatcher/secret_store.go); never a plaintext value.
	// Required for bearer/basic/header/query; unused for oauth2.
	SecretKey string `yaml:"secret_key,omitempty"`
	// Username is the Basic-auth username. Required (and only meaningful)
	// for kind: basic.
	Username string `yaml:"username,omitempty"`
	// Header is the header name to set. Required (and only meaningful) for
	// kind: header.
	Header string `yaml:"header,omitempty"`
	// Query is the query parameter name to set. Required (and only
	// meaningful) for kind: query.
	Query string `yaml:"query,omitempty"`
	// Provider names the OAuth2 provider (config.yaml oauth_providers:
	// block, PR2 — docs/plans/api-gateway.md §論点4). Required (and only
	// meaningful) for kind: oauth2; unused and unvalidated in PR1 beyond
	// "must be non-empty".
	Provider string `yaml:"provider,omitempty"`
}

// ServiceConfig declares one logical service the API gateway can proxy job
// requests to. docs/plans/api-gateway.md §2.
type ServiceConfig struct {
	// BaseURL is the upstream base URL (scheme + host, optionally a path
	// prefix) — never exposed to the sandbox; only the logical service name
	// in the route path is (docs/plans/api-gateway.md §1).
	BaseURL string            `yaml:"base_url"`
	Auth    ServiceAuthConfig `yaml:"auth"`
}

// validServiceAuthKinds is the initial AuthKind set docs/plans/api-gateway.md
// §2 declares: "auth.kind の初期セット: bearer / basic / header / query /
// oauth2". oauth2 validates only that Provider is set — everything else
// about it is PR2's concern.
var validServiceAuthKinds = map[string]bool{
	string(apigateway.AuthBearer): true,
	string(apigateway.AuthBasic):  true,
	string(apigateway.AuthHeader): true,
	string(apigateway.AuthQuery):  true,
	string(apigateway.AuthOAuth2): true,
}

// validateServiceConfig validates one services.<name> entry, returning a
// descriptive error naming both the service and the missing/invalid field —
// the same "fail loud with the offending name" posture
// resolveForgeConfig/GatewayConfig's validation uses.
func validateServiceConfig(name string, sc ServiceConfig) error {
	if sc.BaseURL == "" {
		return fmt.Errorf("services[%q]: missing required \"base_url\" field", name)
	}
	// Parsed and scheme/host-checked here, at config-load time, rather than
	// left for apigateway.NewCredentialProvider's own defensive parse to
	// discover later (internal/server/wire.go). That constructor's doc
	// comment states an "already validated by config load" invariant for
	// exactly this reason — a malformed base_url should fail `boid start`
	// loudly (the same posture gateway.forges.*.host gets), not silently
	// vanish from the gateway's service registry with only a log warning
	// once the daemon is already running.
	if u, err := url.Parse(sc.BaseURL); err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("services[%q]: \"base_url\" must be an absolute URL with a scheme and host (got %q)", name, sc.BaseURL)
	}
	if !validServiceAuthKinds[sc.Auth.Kind] {
		return fmt.Errorf("services[%q]: auth.kind: unrecognized %q (want one of bearer, basic, header, query, oauth2)", name, sc.Auth.Kind)
	}
	switch apigateway.AuthKind(sc.Auth.Kind) {
	case apigateway.AuthBearer:
		if sc.Auth.SecretKey == "" {
			return fmt.Errorf("services[%q]: auth.kind bearer requires \"secret_key\"", name)
		}
	case apigateway.AuthBasic:
		if sc.Auth.Username == "" {
			return fmt.Errorf("services[%q]: auth.kind basic requires \"username\"", name)
		}
		if sc.Auth.SecretKey == "" {
			return fmt.Errorf("services[%q]: auth.kind basic requires \"secret_key\"", name)
		}
	case apigateway.AuthHeader:
		if sc.Auth.Header == "" {
			return fmt.Errorf("services[%q]: auth.kind header requires \"header\" (the header name to set)", name)
		}
		if sc.Auth.SecretKey == "" {
			return fmt.Errorf("services[%q]: auth.kind header requires \"secret_key\"", name)
		}
	case apigateway.AuthQuery:
		if sc.Auth.Query == "" {
			return fmt.Errorf("services[%q]: auth.kind query requires \"query\" (the query parameter name to set)", name)
		}
		if sc.Auth.SecretKey == "" {
			return fmt.Errorf("services[%q]: auth.kind query requires \"secret_key\"", name)
		}
	case apigateway.AuthOAuth2:
		if sc.Auth.Provider == "" {
			return fmt.Errorf("services[%q]: auth.kind oauth2 requires \"provider\" (reserved for PR2 — docs/plans/api-gateway.md §論点4)", name)
		}
	}
	return nil
}

// APIGatewayServices resolves c.Services into the flat
// []apigateway.ServiceConfig list apigateway.NewCredentialProvider consumes
// (internal/server/wire.go), sorted by name for determinism. Mirrors
// GatewayConfig.HostConfigs's own shape and its "already validated by
// Config.UnmarshalYAML" invariant — an entry that fails validateServiceConfig
// here (a hand-built Config that skipped validation) is skipped silently
// rather than surfaced as an error, since this method has no error return
// and never should have one under that invariant.
func (c Config) APIGatewayServices() []apigateway.ServiceConfig {
	names := make([]string, 0, len(c.Services))
	for name := range c.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]apigateway.ServiceConfig, 0, len(names))
	for _, name := range names {
		sc := c.Services[name]
		if err := validateServiceConfig(name, sc); err != nil {
			continue
		}
		out = append(out, apigateway.ServiceConfig{
			Name:    name,
			BaseURL: sc.BaseURL,
			Auth: apigateway.ServiceAuth{
				Kind:      apigateway.AuthKind(sc.Auth.Kind),
				SecretKey: sc.Auth.SecretKey,
				Username:  sc.Auth.Username,
				Header:    sc.Auth.Header,
				Query:     sc.Auth.Query,
				Provider:  sc.Auth.Provider,
			},
		})
	}
	return out
}
