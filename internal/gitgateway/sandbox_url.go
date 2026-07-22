package gitgateway

import "fmt"

// Backend identifies which sandbox execution backend a set of
// sandbox-facing URLs (gateway, and eventually broker/dockerproxy) must be
// reachable from (docs/plans/phase6-container-backend.md §決定5: "gateway
// / broker / dockerproxy はサービス名 (DNS) + TCP (mTLS) で到達する。旧版の
// loopback bind + 10.0.2.2 投影... は丸ごと不要").
//
// gitgateway stays a leaf package (no internal/... imports) — Backend is
// defined locally rather than pulled from internal/sandbox/backend so this
// package's dependency-free property is preserved; internal/server, which
// already imports both packages, is where a real backend selection
// (should one ever need to vary per-daemon rather than per-deploy — see
// the plan doc's §決定11) would be bridged in.
type Backend string

const (
	// BackendUserns is the current (and, until Phase 6 PR5, only) sandbox
	// backend: pasta provides a slirp NAT with the host loopback projected
	// into the sandbox at 10.0.2.2.
	BackendUserns Backend = "userns"
	// BackendContainer is the Phase 6 container backend (PR5+): daemon
	// and job share a compose network, so the gateway is reached by its
	// compose service name over DNS instead of a loopback projection.
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
	// BackendContainer; PR4 does not run any container backend
	// deployment, so no real caller sets this yet — PR5/PR6 own actually
	// populating it from compose service discovery. Defaults to
	// "boid-gateway" when left empty under BackendContainer.
	ServiceName string
}

// SandboxURL builds the base URL a sandbox should use to reach a
// daemon-side listener, given opts.Backend
// (docs/plans/phase6-container-backend.md §PR4: "gateway の sandbox 向け
// URL 生成を backend 別に"). BackendUserns (and the zero value) reproduces
// today's loopback-projection URL byte-for-byte; BackendContainer produces
// a compose-service-name URL over TLS, for PR5's container backend to
// consume once it exists.
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
