package gitgateway

import "fmt"

// Backend identifies which sandbox execution backend a set of
// sandbox-facing URLs (gateway, and eventually broker/dockerproxy) must be
// reachable from.
//
// Defined locally rather than pulled from internal/sandbox/backend: this
// package stays a leaf (no internal/... imports).
type Backend string

const (
	// BackendUserns identifies the former userns backend (pasta slirp NAT,
	// host loopback projected at 10.0.2.2). No longer used in production —
	// retained only as a test-DI seam / SandboxURLOptions.Backend's zero
	// value.
	BackendUserns Backend = "userns"
	// BackendContainer is the container backend, the sole backend used in
	// production: daemon and job share a compose network, so the gateway
	// is reached by its compose service name over DNS instead of a
	// loopback projection.
	BackendContainer Backend = "container"
)

// SandboxURLOptions carries the backend-specific addressing info needed to
// build a URL that reaches a daemon-side listener (e.g. the git gateway)
// from inside a sandbox.
type SandboxURLOptions struct {
	// Backend selects the addressing scheme. The zero value ("") behaves
	// like BackendUserns, so existing callers that don't set this field
	// keep today's http://10.0.2.2:<port> behavior unchanged.
	Backend Backend
	// Port is the TCP port the target listener is bound to. Consulted by
	// both backends.
	Port int
	// ServiceName is the compose network DNS name of the daemon's
	// service (e.g. "boid-daemon"). Only consulted when Backend ==
	// BackendContainer. Defaults to "boid-gateway" when left empty under
	// BackendContainer.
	ServiceName string
}

// SandboxURL builds the base URL a sandbox should use to reach a
// daemon-side listener, given opts.Backend. BackendUserns (and the zero
// value) reproduces the loopback-projection URL; BackendContainer produces
// a compose-service-name URL over TLS.
func SandboxURL(opts SandboxURLOptions) string {
	switch opts.Backend {
	case BackendContainer:
		name := opts.ServiceName
		if name == "" {
			name = "boid-gateway"
		}
		return fmt.Sprintf("https://%s:%d", name, opts.Port)
	default: // BackendUserns, and "" for callers that predate this field.
		return fmt.Sprintf("http://10.0.2.2:%d", opts.Port)
	}
}
