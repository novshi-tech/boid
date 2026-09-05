package dispatcher

import (
	"database/sql"

	"github.com/novshi-tech/boid/internal/apigateway"
	"github.com/novshi-tech/boid/internal/gitgateway"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type WireConfig struct {
	DB          *sql.DB
	Broker      CommandBroker
	SecretStore *SecretStore

	Projects ProjectLookup
	// Hydrator is optional workspace-hydrated ProjectMeta lookup, threaded
	// straight to Runner.Hydrator. nil disables workspace-peer meta.name
	// resolution; buildPeerAdvertise falls back to filepath.Base(WorkDir).
	Hydrator orchestrator.MetaHydrator

	// BoidBinary is the host path to the boid executable that should be
	// bind-mounted into sandboxes.
	BoidBinary string
	// ServerSocket is the host path to the daemon UNIX socket (for boid exec
	// jobs that talk to boid over HTTP from inside the sandbox).
	ServerSocket string
	// ProxyPort points at the default-workspace proxy port. Used as the
	// fallback when ProxyAllocator is not wired (or fails) for a NAMED
	// workspace's dispatch. Sandboxes linked to a workspace get a
	// per-workspace port via ProxyAllocator.
	ProxyPort *int
	// NoWorkspaceProxyPort points at the dedicated no-workspace proxy port.
	// Deliberately separate from ProxyPort: resolveWorkspaceProxy's
	// empty-workspaceID path must never fall back to the default
	// workspace's own (editable) listener.
	NoWorkspaceProxyPort *int
	// AllowedDomains is the daemon-wide proxy egress allowlist floor
	// (config.yaml sandbox.allowed_domains + boid built-in defaults),
	// captured once when Wire builds the Runner. Workspaces add entries on
	// top via workspace.yaml; they cannot remove floor entries
	// (orchestrator.ResolveAllowedDomains enforces this).
	AllowedDomains []string
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
	// DataHomeDir is the daemon's own persistent data root, threaded
	// straight to Runner.DataHomeDir. Empty falls back to
	// $XDG_DATA_HOME/boid rather than disabling anything.
	DataHomeDir string
	// ReservedVolumeNames is threaded straight to Runner.ReservedVolumeNames:
	// the docker volume names a sandboxed docker client may not create,
	// delete or mount, discovered at daemon startup by inspecting the
	// daemon's own container. nil leaves the dockerproxy policy unrestricted
	// by this field.
	ReservedVolumeNames []string
	// GitGateway is the git gateway's job-token registry. nil disables
	// gateway token registration entirely.
	GitGateway *gitgateway.Registry
	// GatewayURL points at the daemon's own gateway listener address string,
	// filled in by Server.Start once the gateway's TCP listener is bound
	// (same late-binding pattern as ProxyPort). nil disables gateway URL
	// propagation into SandboxRuntimeInfo.
	GatewayURL *string
	// GatewayCAPEM points at the daemon's internal CA's own PEM-encoded
	// certificate, filled in by Server.Start alongside GatewayURL. A
	// container-backend clone-mode job needs this to verify the git
	// gateway's TLS server certificate. nil disables CA propagation.
	GatewayCAPEM *[]byte
	// APIGateway is the API gateway's job-token registry. nil disables
	// gateway token registration entirely.
	APIGateway *apigateway.Registry
	// APIGatewayServicesFloor is the daemon-wide set of API gateway service
	// names enabled for every workspace (config.yaml services_floor),
	// captured once when Wire builds the Runner.
	APIGatewayServicesFloor []string
}

func Wire(cfg WireConfig) *Runner {
	return &Runner{
		DB:                      cfg.DB,
		Broker:                  cfg.Broker,
		SecretStore:             cfg.SecretStore,
		Projects:                cfg.Projects,
		Hydrator:                cfg.Hydrator,
		Workspaces:              cfg.Workspaces,
		ProxyAllocator:          cfg.ProxyAllocator,
		BoidBinary:              cfg.BoidBinary,
		ServerSocket:            cfg.ServerSocket,
		ProxyPort:               cfg.ProxyPort,
		NoWorkspaceProxyPort:    cfg.NoWorkspaceProxyPort,
		AllowedDomains:          cfg.AllowedDomains,
		RuntimesDir:             cfg.RuntimesDir,
		DataHomeDir:             cfg.DataHomeDir,
		ReservedVolumeNames:     cfg.ReservedVolumeNames,
		GitGateway:              cfg.GitGateway,
		GatewayURL:              cfg.GatewayURL,
		GatewayCAPEM:            cfg.GatewayCAPEM,
		APIGateway:              cfg.APIGateway,
		APIGatewayServicesFloor: cfg.APIGatewayServicesFloor,
	}
}
