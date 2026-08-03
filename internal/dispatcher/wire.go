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
	// fallback when ProxyAllocator is not wired (or fails) for a NAMED
	// workspace's dispatch. Sandboxes linked to a workspace get a
	// per-workspace port via ProxyAllocator.
	ProxyPort *int
	// NoWorkspaceProxyPort points at the dedicated no-workspace proxy port
	// — see Runner.NoWorkspaceProxyPort's own doc comment. Deliberately
	// separate from ProxyPort: resolveWorkspaceProxy's empty-workspaceID
	// path must never be able to fall back to the real default workspace's
	// own (editable) listener.
	NoWorkspaceProxyPort *int
	// AllowedDomains is the daemon-wide proxy egress allowlist floor
	// (config.yaml sandbox.allowed_domains + boid built-in defaults),
	// captured once when Wire builds the Runner — see Runner.AllowedDomains's
	// own doc comment for why this is a plain []string, not a getter
	// (sandbox.allowed_domains is ReloadRestartRequired: PR #830 round-4
	// simplification, nose directive). Workspaces add entries on top via
	// workspace.yaml; they cannot remove floor entries
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
	// straight to Runner.DataHomeDir (see its doc comment). server/wire.go
	// passes dataHomeFor(cfg) — the same value it already gives
	// projectSvc.DataDir, the attachments root and the repos root, so
	// workspace home init bookkeeping lands in the `boid_state` volume with
	// the rest of the daemon's durable state rather than on tmpfs
	// (docs/plans/workspace-home-volume-persistence.md 論点b, PR2). Empty
	// falls back to $XDG_DATA_HOME/boid rather than disabling anything —
	// unlike dataHomeFor's other consumers, which do read "" as "feature
	// disabled".
	DataHomeDir string
	// ReservedVolumeNames is threaded straight to Runner.ReservedVolumeNames
	// (see its doc comment): the docker volume names a sandboxed docker
	// client may not create, delete or mount, discovered at daemon startup by
	// inspecting the daemon's own container. server/wire.go passes
	// dispatcher.DetectDaemonStateVolumes' result. nil — every non-container
	// deployment and all test wiring — leaves the dockerproxy policy exactly
	// as it was before this field existed.
	ReservedVolumeNames []string
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
	// disables CA propagation — the plaintext gateway URL of the (now
	// removed, PR-4) userns backend never needed it either way.
	GatewayCAPEM *[]byte
	// APIGateway is the API gateway's job-token registry (docs/plans/
	// api-gateway.md PR1). nil disables gateway token registration
	// entirely.
	APIGateway *apigateway.Registry
	// APIGatewayServicesFloor is the daemon-wide set of API gateway service
	// names enabled for every workspace (config.yaml services_floor),
	// captured once when Wire builds the Runner — see Runner.
	// APIGatewayServicesFloor's own doc comment.
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
