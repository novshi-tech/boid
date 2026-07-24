package dispatcher

import (
	"database/sql"

	"github.com/novshi-tech/boid/internal/gitgateway"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type WireConfig struct {
	DB          *sql.DB
	Runtime     JobRuntime
	Broker      CommandBroker
	Sandbox     SandboxPreparer
	SecretStore *SecretStore

	Projects ProjectLookup
	// Hydrator is optional workspace-hydrated ProjectMeta lookup, threaded
	// straight to Runner.Hydrator (see its doc comment). nil disables
	// workspace-peer meta.name resolution; buildPeerAdvertise falls back to
	// filepath.Base(WorkDir).
	Hydrator orchestrator.MetaHydrator

	// BoidBinary is the host path to the boid executable that should be
	// bind-mounted into sandboxes.
	BoidBinary string
	// ServerSocket is the host path to the daemon UNIX socket (for boid exec
	// jobs that talk to boid over HTTP from inside the sandbox).
	ServerSocket string
	// ProxyPort points at the default-workspace proxy port. Used as the
	// fallback when ProxyAllocator is not wired (or fails). Sandboxes
	// linked to a workspace get a per-workspace port via ProxyAllocator.
	ProxyPort *int
	// AllowedDomains is a getter for the daemon-wide proxy egress allowlist
	// floor (config.yaml sandbox.allowed_domains + boid built-in
	// defaults), read fresh on every dispatch — see Runner.AllowedDomains's
	// own doc comment for why this is a func() []string and not a captured
	// []string (BLOCKER 2, codex review round 1). Workspaces add entries on
	// top via workspace.yaml; they cannot remove floor entries
	// (orchestrator.ResolveAllowedDomains enforces this).
	AllowedDomains func() []string
	// Workspaces is the WorkspaceLookup used at dispatch time to discover
	// each workspace's AllowedDomains overrides. nil disables workspace
	// hydration and the runner stays on the floor only.
	Workspaces WorkspaceLookup
	// ProxyAllocator is the per-workspace proxy listener registry. nil
	// disables workspace-scoped proxy allocation and the runner serves
	// every sandbox via the default-workspace listener.
	ProxyAllocator ProxyAllocator
	// RuntimesDir is the root directory where per-sandbox runtime directories
	// are created. When non-empty and DockerEnabled, the runner pre-allocates a
	// runtime directory here to host the per-sandbox docker proxy socket and
	// resource ledger.
	RuntimesDir string
	// GitGateway is the git gateway's job-token registry
	// (docs/plans/git-gateway-cutover.md PR4). nil disables gateway token
	// registration entirely.
	GitGateway *gitgateway.Registry
	// GatewayURL points at the daemon's own gateway listener address string,
	// filled in by Server.Start once the gateway's TCP listener is bound
	// (same late-binding pattern as ProxyPort). nil disables gateway URL
	// propagation into SandboxRuntimeInfo.
	GatewayURL *string
	// GatewayCAPEM points at the daemon's internal CA's own PEM-encoded
	// certificate, filled in by Server.Start alongside GatewayURL (same
	// late-binding-via-pointer pattern). A container-backend clone-mode
	// job needs this to verify the git gateway's TLS server certificate
	// (see SandboxRuntimeInfo.GatewayCAPEM's own doc comment). nil
	// disables CA propagation — the userns backend's plaintext gateway URL
	// never needs it either way.
	GatewayCAPEM *[]byte
}

func Wire(cfg WireConfig) *Runner {
	return &Runner{
		DB:             cfg.DB,
		Runtime:        cfg.Runtime,
		Broker:         cfg.Broker,
		Sandbox:        cfg.Sandbox,
		SecretStore:    cfg.SecretStore,
		Projects:       cfg.Projects,
		Hydrator:       cfg.Hydrator,
		Workspaces:     cfg.Workspaces,
		ProxyAllocator: cfg.ProxyAllocator,
		BoidBinary:     cfg.BoidBinary,
		ServerSocket:   cfg.ServerSocket,
		ProxyPort:      cfg.ProxyPort,
		AllowedDomains: cfg.AllowedDomains,
		RuntimesDir:    cfg.RuntimesDir,
		GitGateway:     cfg.GitGateway,
		GatewayURL:     cfg.GatewayURL,
		GatewayCAPEM:   cfg.GatewayCAPEM,
	}
}
