package server

import (
	"context"
	"crypto/tls"
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/apigateway"
	"github.com/novshi-tech/boid/internal/config"
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/gitgateway"
	"github.com/novshi-tech/boid/internal/install"
	"github.com/novshi-tech/boid/internal/mtls"
	"github.com/novshi-tech/boid/internal/notify"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// gatewayTLSShutdownTimeout bounds how long Stop waits for the git
// gateway's TLS listener to drain in-flight requests before giving up.
// Matches the daemon's other best-effort shutdown waits in shape — short
// enough that a stuck request can't hang `boid stop` indefinitely.
const gatewayTLSShutdownTimeout = 5 * time.Second

// composeBrokerServiceName and composeGatewayServiceName are the compose
// network DNS names the broker's and git gateway's TCP(mTLS) listener
// certs advertise as SANs, alongside the loopback names every caller
// actually dials today. The SAN has to be on the cert from the moment the
// CA issues it (once per daemon lifetime), not retrofitted once a real
// caller resolves it. composeGatewayServiceName intentionally matches
// gitgateway.SandboxURLOptions.ServiceName's own BackendContainer default
// ("boid-gateway", see internal/gitgateway/sandbox_url.go) rather than
// inventing a second name for the same service.
//
// The egress proxy's own compose-network DNS name ("boid-egress") is
// declared as dispatcher.composeEgressServiceName, not duplicated here:
// internal/server already imports internal/dispatcher (the reverse would
// cycle), and dispatcher.SandboxRuntimeInfo.ProxyHost is the only place
// that literal is actually consumed — see its own doc comment. It must
// still match this file's composeBindHost value and
// build/container/compose.yml's own "boid-egress" alias.
//
// composeBindHost is the listen address the git gateway's TCP(mTLS)
// listener and the default-workspace egress proxy listener bind to when
// the container backend is selected, instead of the loopback-only
// "127.0.0.1" every non-container-backend deployment uses. A sibling job
// container reaches this daemon over the shared compose network by its
// own container IP, not 127.0.0.1 — a loopback-bound listener is
// unreachable from outside this daemon's own container entirely, so a job
// could dial the DNS-resolved address and still get connection-refused.
// "0.0.0.0" (all interfaces) is simplest and matches how every other
// example containerized Go service exposes a port to its own docker
// network; the daemon container's own network boundary (boid_internal
// being `internal: true`) is what actually scopes reachability, not the
// bind address itself. The plaintext (non-TLS) git gateway HTTP listener
// is deliberately NOT included here — it stays loopback-only regardless of
// backend (never expose plaintext off-box); only the already-mTLS-protected
// TCP listeners move.
const (
	composeBrokerServiceName  = "boid-broker"
	composeGatewayServiceName = "boid-gateway"
	composeBindHost           = "0.0.0.0"
)

type Config struct {
	DBPath         string
	SocketPath     string
	HTTPAddr       string
	KitsDir        string   // base dir for installed kit repos
	KeyFilePath    string   // path to secret encryption key file
	AllowedDomains []string // proxy allowed domains
	// EgressProxyPortLow/High bound the band the egress proxy allocates
	// stable per-workspace ports from (config.yaml
	// sandbox.egress_proxy_port_low/high). Zero means "use
	// sandbox.DefaultProxyPortRange{Low,High}"; the default literal
	// deliberately lives in internal/sandbox only. Captured once at
	// startup, ReloadRestartRequired, exactly like AllowedDomains — a
	// listener cannot move to a different band without rebinding anyway.
	EgressProxyPortLow  int
	EgressProxyPortHigh int
	// ServicesFloor is the daemon-wide API gateway service allowlist floor
	// (config.yaml services_floor), captured once here exactly like
	// AllowedDomains is for the proxy egress allowlist: cmd/start.go's
	// buildStartConfig reads it from its own config.Load() call and passes
	// it in, so a later `boid config set services_floor ...` only takes
	// effect on the next daemon restart, same ReloadRestartRequired
	// posture as sandbox.allowed_domains.
	ServicesFloor []string
	// Backend, when non-nil, overrides buildRuntime's default construction
	// of a real containerBackend (sandboxBackendForConfig) — a test/DI seam
	// so tests can exercise daemon startup / the HTTP API / attach-resize
	// wiring against a fake backend.SandboxBackend without a live docker
	// daemon.
	Backend backend.SandboxBackend
	// TLSDir, when non-empty, is the directory holding (or to generate)
	// the per-daemon internal CA (ca.crt/ca.key) used to secure the
	// broker/git-gateway TCP(mTLS) listeners. Empty (the zero value)
	// skips binding those listeners entirely — only the pre-existing UNIX
	// socket / plaintext loopback listeners are bound. cmd/start.go sets
	// a real default (~/.local/share/boid/tls) so a live daemon actually
	// binds them.
	TLSDir string
	// InstallIDDir, when non-empty, is the directory holding (or to
	// generate) this installation's plain-UUID install_id file
	// (internal/install.LoadOrCreate). Empty (the zero value) skips this
	// entirely — New leaves Server.installID at "" and nothing changes.
	// cmd/start.go sets a real default (~/.local/share/boid, the same dir
	// web_secret and boid.db live in) so a live daemon actually has one.
	InstallIDDir string
	// CLIAddr, when non-empty, is the "host:port" the dedicated CLI TCP
	// listener binds. Only the PORT is actually load-bearing: Start()
	// ignores the host half and binds gatewayBindHost(s.usingContainerBackend)
	// instead — "127.0.0.1" for the userns backend (this listener is
	// reached exclusively by the `boid` CLI's own host-mode orchestration,
	// cmd/host.go, dialing the docker-published port on the SAME host,
	// never by a remote caller or a sibling job container) or
	// composeBindHost ("0.0.0.0") for the container backend: under
	// compose, this daemon IS the container being dialed — docker's
	// bridge-mode port publish DNATs the host's own 127.0.0.1 to the
	// container's bridge IP, never the container's loopback interface, so
	// a loopback-only bind there is unreachable from the host at all; see
	// gatewayBindHost's own doc comment for the identical routing already
	// used by the git gateway/broker TLS listeners. Separate from
	// HTTPAddr (the Web UI's own TCP listener, default :8080) so a CLI
	// session keeps working even if an operator firewalls off or disables
	// the Web UI port.
	//
	// Binding is further gated on CLIToken being non-empty (see that
	// field's own doc comment) — CLIAddr alone is not enough, unlike every
	// other listener in this Config, because this listener has no
	// TLS/loopback-trust/cookie fallback: an unset token would otherwise
	// mean an open, unauthenticated TCP port.
	CLIAddr string
	// CLIToken, when non-empty, is the shared secret
	// (auth.NewCLITokenAuthMiddleware) the dedicated CLI TCP listener
	// (CLIAddr) requires on every request's `Authorization: Bearer`
	// header. Sourced from the BOID_CLI_TOKEN env var (cmd/start.go's
	// buildStartConfig), which `boid`'s own host-mode orchestration
	// (cmd/host.go) generates/reads from ~/.config/boid/cli-token and
	// passes to the daemon container via build/container/compose.yml's
	// `environment:` block — the same value on both ends is what
	// authenticates a host-mode CLI dispatch with no TLS and no
	// device-pairing ceremony. Empty (the zero value) skips binding the
	// CLIAddr listener entirely, same as CLIAddr=="" — see Start()'s own
	// bind condition.
	CLIToken string
	// LogLevel is config.yaml's log.level (internal/config.LogConfig.Level),
	// copied here by cmd/start.go's buildStartConfig purely so it can travel
	// alongside the rest of this struct from the parent's config.Load() call
	// to runDaemonChild — unlike every other field above, Server itself
	// never reads LogLevel; runDaemonChild applies it directly
	// (daemon.ApplyLogLevel(cfg.LogLevel)), before New/Start are even
	// called, since it is a process-wide slog concern with nothing to do
	// with the HTTP/dispatch server New builds. Empty (the zero value, and
	// every pre-this-field caller/test) is a no-op — see
	// internal/config.LogConfig and internal/daemon.ApplyLogLevel's own doc
	// comments for the full contract.
	LogLevel string
}

type Server struct {
	cfg          Config
	db           *sql.DB
	store        *orchestrator.ProjectStore
	broker       *sandbox.Broker
	secretStore  *dispatcher.SecretStore
	proxyManager *sandbox.ProxyManager
	proxyPort    int // port of the default-workspace listener (back-compat /api/proxy surface)
	// noWorkspaceProxyPort is the port of the dedicated egress proxy
	// listener bound at Start (keyed by dispatcher.NoWorkspaceProxyKey) for
	// jobs with no workspace at all — see Runner.NoWorkspaceProxyPort's own
	// doc comment. Same late-binding-via-pointer pattern as proxyPort;
	// deliberately a separate field so it can never be confused with (or
	// fall back to) proxyPort's own, editable-default-workspace value.
	noWorkspaceProxyPort int
	router               chi.Router
	unixLn               net.Listener
	tcpLn                net.Listener
	httpServer           *http.Server
	// tcpHandler wraps the router with transport-aware API auth and is served
	// to the TCP listener only. The UNIX socket is served the bare router
	// (trusted CLI/agent transport). Set by mountRoutes.
	tcpHandler http.Handler
	tcpServer  *http.Server
	// cliLn/cliServer/cliHandler are the dedicated CLI TCP listener. Bound
	// in Start only when both cfg.CLIAddr and cfg.CLIToken are set (see
	// Config.CLIAddr's own doc comment for why token presence gates the
	// bind, not just the address). cliHandler wraps the same bare router
	// mountRoutes builds tcpHandler from, but with
	// auth.NewCLITokenAuthMiddleware(cfg.CLIToken) instead of
	// NewTCPAPIAuthMiddleware — a single shared-secret Bearer token, no
	// TLS, no cookie/loopback-trust fallback. nil whenever the listener
	// was never bound.
	cliLn          net.Listener
	cliServer      *http.Server
	cliHandler     http.Handler
	gcLoop         *orchestrator.GCLoop // nil if GC is disabled
	queueSweepLoop *api.QueueSweepLoop  // queue の決定論的評価: wake 評価 rule
	triggerLoop    *api.TriggerLoop     // トリガのスケジュール/single-flight/実行記録
	workflow       *api.TaskWorkflowService

	// hostCommands is the aggregated host_commands config assembled by
	// buildProjectStore's preflight: every installed kit.yaml's
	// host_commands, deduped by identical definition, name-collision-
	// checked, and mirrored to orchestrator.DefaultHostCommandsPath() on
	// disk. workspace.HostCommands ([]string reference names) resolves
	// against this map in ProjectStore.GetWithWorkspace.
	hostCommands map[string]orchestrator.HostCommandSpec

	// gitgateway: the authenticating reverse proxy sandboxes clone through,
	// with env-var-advertised credentials.
	//
	// gatewayRegistry is constructed early in New() (buildRuntime) and shared
	// with dispatcher.Runner so Dispatch/UnregisterJob can
	// Register/Unregister job tokens; gatewayHTTPServer wraps
	// combinedGatewayHandler (built once config + notify are available) and
	// is only bound to a listener in Start().
	gatewayRegistry   *gitgateway.Registry
	gatewayHTTPServer *http.Server
	gatewayLn         net.Listener
	// apiGatewayRegistry is the API gateway's job-token registry — the
	// exact same role gatewayRegistry plays for the git gateway, shared
	// with dispatcher.Runner so Dispatch/UnregisterJob can
	// Register/Unregister API gateway tokens too.
	apiGatewayRegistry *apigateway.Registry
	// combinedGatewayHandler dispatches a single shared listener's requests
	// to either the git gateway or the API gateway by path prefix (the
	// listener is shared: path prefix /j/ vs /api/). Built by wire.go once
	// both underlying Server handlers exist; gatewayHTTPServer.Handler is
	// set to it directly (the plaintext loopback listener needs no further
	// wiring here), and Start reads this field again to build the
	// TLS(mTLS) listener's own *http.Server — see gatewayTLSHTTPServer's
	// own doc comment for why that can't reuse gitgateway.Server.ListenTLS
	// (that method hardcodes its OWN ServeHTTP as the Handler, not this
	// combined one). nil until wire.go constructs it.
	combinedGatewayHandler *combinedGatewayHandler
	// gatewayTLSLn is the git/API gateway shared TCP(mTLS) listener, bound in
	// Start only when cfg.TLSDir is set. nil otherwise.
	gatewayTLSLn net.Listener
	// gatewayTLSHTTPServer is the *http.Server serving gatewayTLSLn with
	// combinedGatewayHandler. gitgateway.Server.ListenTLS's own internally
	// constructed *http.Server (Handler: the gitgateway.Server itself) no
	// longer works once a second gateway shares the listener, since
	// ListenTLS has no way to accept an external Handler. Start instead
	// calls tls.Listen directly and constructs this *http.Server itself,
	// with combinedGatewayHandler as its Handler; Stop calls Shutdown on it
	// (graceful — closes idle keep-alives too). nil until Start binds it
	// (cfg.TLSDir unset, or Start hasn't run).
	gatewayTLSHTTPServer *http.Server
	// gatewayURL is the sandbox-facing base URL (http://10.0.2.2:<port>),
	// populated by Start() once the listener is bound. Empty before Start
	// completes. Runner holds a pointer to this string (WireConfig.GatewayURL)
	// so SandboxRuntimeInfo.GatewayURL reflects it at dispatch time — the
	// same late-binding-via-pointer trick as proxyPort. Shared verbatim by
	// the API gateway (same listener, same URL) — dispatcher.WireConfig.
	// GatewayURL is the single pointer both gateways' env-var advertise
	// reads.
	gatewayURL string
	// gatewayCAPEM is the daemon's internal CA's own certificate
	// (mtls.CA.CertPEM), PEM-encoded, populated by Start() alongside
	// daemonCA whenever cfg.TLSDir is set. Empty when TLS isn't configured.
	// This is the client-side half of
	// the shared gateway TLS listener's trust: a container-backend sandbox
	// needs this CA's public cert to verify the gateway's server
	// certificate (the tlsCfg Start's TLS block builds, below) — non-secret,
	// so no per-job materialization/rotation is needed, unlike a client
	// certificate would be. Runner holds a pointer to this slice
	// (WireConfig.GatewayCAPEM), the same late-binding-via-pointer pattern
	// gatewayURL already uses, so SandboxRuntimeInfo.GatewayCAPEM reflects
	// it at dispatch time even though it (like gatewayURL) is only known
	// once Start has run.
	gatewayCAPEM []byte

	// daemonCA is the per-daemon internal CA, loaded (or generated) once
	// in New() when cfg.TLSDir is set. Empty (nil) otherwise — every call
	// site that consumes it already treats a nil CA as "TLS not
	// configured, skip the additive TCP(mTLS) listener" (Start's own
	// `if daemonCA != nil` guards).
	//
	// Loaded here (in New(), before buildRuntime runs) rather than lazily
	// inside Start, because buildRuntime's sandboxBackendForConfig call
	// (internal/server/wire.go) constructs the container backend's
	// ContainerBackendOptions — which needs this exact CA to issue
	// per-job broker client certs — strictly BEFORE Start ever runs.
	// Start() below reads this field instead of loading its own second
	// (content-identical, since mtls.LoadOrCreate is idempotent) copy.
	daemonCA *mtls.CA

	// brokerTLSSandboxAddr is the compose-network `host:port`
	// (composeBrokerServiceName + the broker's actual bound TLS port, e.g.
	// "boid-broker:54321") a container-backend job's BOID_BROKER_TLS_ADDR
	// env should point at, populated by Start() once s.broker.Start has
	// bound the TLS listener. Empty before Start completes, and empty for
	// the entire life of the process whenever cfg.TLSDir is unset (no TLS
	// listener at all). sandboxBackendForConfig (called from buildRuntime,
	// itself called from New() — strictly before Start ever runs) is
	// handed a POINTER to this field (dispatcher.ContainerBackendOptions.
	// BrokerTLSAddr), the same late-binding-via-pointer pattern gatewayURL
	// uses one layer up in this same file, so containerBackend.Launch
	// (which only ever runs once real job dispatch begins, long after
	// Start has completed) dereferences the real value instead of the
	// empty string this field starts at.
	//
	// Deliberately a SEPARATE field from the existing BrokerTLSAddr()
	// accessor's own return value (s.broker.TLSListenAddr(), e.g.
	// "0.0.0.0:54321" — the RAW listen address, used by tests that dial
	// the listener directly): a job container cannot dial "0.0.0.0" at
	// all (that is a bind wildcard, not a routable peer address) — it must
	// dial the compose service DNS name instead, which is what this field
	// holds.
	brokerTLSSandboxAddr string

	// installID is this installation's plain-UUID identity, loaded (or
	// generated) once in New() when cfg.InstallIDDir is set. Empty
	// otherwise — see InstallIDDir's doc comment.
	installID string

	// usingContainerBackend records whether buildRuntime selected the
	// container backend for this daemon's dispatcher.Runner. Backend
	// selection is GLOBAL for the whole daemon, not per-job, so this is
	// computed once in New() and never changes for the life of the
	// process. Start() reads it to decide which addressing scheme
	// (loopback 10.0.2.2 projection vs. compose service DNS + TLS) the
	// git gateway's sandbox-facing URL and listener bind address use, and
	// which host the egress proxy env advertises to job containers — see
	// gatewayURLFor/gatewayBindHost and ProxyManager.BindHost's own doc
	// comments.
	usingContainerBackend bool

	mu sync.Mutex

	// configMu serializes read-modify-write config.yaml mutations: set and
	// apply must be serialized so two concurrent sets cannot interleave
	// and produce a torn config. Deliberately a separate lock from mu
	// (hostCommands' own lock) — config-apply's validate+write+
	// hot-apply critical section can run long enough (disk I/O) that
	// sharing mu would add unrelated contention against hostCommands
	// reads. See internal/server/config_edit.go for every method that
	// takes it.
	configMu sync.Mutex
	// liveConfig is the daemon's current effective *config.Config,
	// populated once by buildRuntime (wire.go) and swapped, under
	// configMu, by ApplyConfigYAML on every successful apply. nil only
	// before buildRuntime runs — every production and test path (New())
	// populates it before mountRoutes ever exposes GET/POST /api/config.
	liveConfig *config.Config
	// configPath is the on-disk config.yaml path ApplyConfigYAML persists
	// to — config.DefaultPath(), the exact path config.Load() itself
	// reads, set once by buildRuntime. A daemon restart therefore always
	// reads back exactly what the live daemon's last successful apply
	// wrote.
	configPath string
	// notifySvc is the same *notify.Service instance wire.go wires into
	// TaskAppService.Notify and the git gateway's notifier, stored here
	// too so ApplyConfigYAML can hot-swap its Command/PublicURL live
	// (notify.command/web.public_url are both "dynamic" reload-class
	// keys). nil in any test
	// that builds a *Server without going through buildRuntime's notifySvc
	// wiring — ApplyConfigYAML treats that as "no live notify service to
	// update", not an error.
	notifySvc *notify.Service
}

func New(cfg Config) (*Server, error) {
	d, err := db.Open(cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := migrate.Apply(d.Conn); err != nil {
		d.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	// No embedded-skill deployment here: the embedded set is baked into the
	// runner image at build time (build/container/Dockerfile) and
	// symlinked into each workspace home by the init container. Pinned by
	// TestNew_DoesNotDeploySkillsBesideTheDB. Whatever an existing
	// installation already has under <dataHome>/skills is left alone.

	// Install identity: loaded (or generated once) here, alongside the
	// other on-disk daemon-identity artifacts New() already establishes
	// (install_id, migrations). Empty cfg.InstallIDDir skips this —
	// installID stays "".
	//
	// Advisory, not blocking: install_id is a container-backend concept —
	// its only consumers are containerBackend's boid.install_id resource
	// label and `boid reap`'s label filter, neither of which the userns
	// backend touches at all. Failing New() outright over a LoadOrCreate
	// error (e.g. cfg.InstallIDDir root-owned from a prior run under a
	// different uid) would refuse to start a userns daemon over a value
	// it never uses — so a failure here is logged and installID stays ""
	// instead. containerBackend's own ReapOrphans doc comment already
	// documents its pre-install_id "global (not install_id-scoped) label
	// filter" fallback for exactly this empty-installID case.
	var installID string
	if cfg.InstallIDDir != "" {
		id, err := install.LoadOrCreate(cfg.InstallIDDir)
		if err != nil {
			slog.Warn("install id load/create failed; continuing with an empty install id (container-backend resource labeling/reap scoping degrades to the pre-install_id global filter — the userns backend is unaffected)",
				"dir", cfg.InstallIDDir, "err", err)
		} else {
			installID = id
		}
	}

	conn := d.Conn

	projectRepo := orchestrator.NewProjectRepository(conn)
	store, hostCommands, err := buildProjectStore(cfg, conn, projectRepo)
	if err != nil {
		conn.Close()
		return nil, err
	}

	// host_commands provisioning check: resolve every configured host
	// command against this daemon's own host now, at boot, instead of only
	// discovering a gap the first time an affected project dispatches.
	// Advisory only — a missing command is warned about, not fatal; see
	// ValidateHostCommandsInstalled's doc comment for why.
	for _, name := range orchestrator.ValidateHostCommandsInstalled(hostCommands, exec.LookPath) {
		slog.Warn("host command not found on daemon host; broker exec will fail for jobs using it (provision it in build/container/Dockerfile once the compose deploy is in use)",
			"command", name)
	}

	brokerSocket := filepath.Join(filepath.Dir(cfg.SocketPath), "boid-broker.sock")
	broker := &sandbox.Broker{SocketPath: brokerSocket}

	// Secret store
	var secretStore *dispatcher.SecretStore
	if cfg.KeyFilePath != "" {
		key, err := dispatcher.LoadOrCreateKey(cfg.KeyFilePath)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("load secret key: %w", err)
		}
		secretStore, err = dispatcher.NewSecretStore(conn, key)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("secret store: %w", err)
		}
	}

	srv := &Server{
		cfg:          cfg,
		db:           conn,
		store:        store,
		broker:       broker,
		secretStore:  secretStore,
		proxyManager: sandbox.NewProxyManager(),
		router:       chi.NewRouter(),
		httpServer: &http.Server{
			Handler: nil,
		},
		hostCommands: hostCommands,
		installID:    installID,
	}

	// Per-daemon internal CA: hoisted here — before buildRuntime runs —
	// rather than lazily loaded inside Start. See Server.daemonCA's own
	// doc comment for why: buildRuntime's sandboxBackendForConfig call
	// needs this CA to configure the
	// container backend's per-job broker client cert issuance, and that
	// call happens strictly before Start ever runs. cfg.TLSDir empty
	// leaves srv.daemonCA/gatewayCAPEM at their zero values, so this hoist
	// changes nothing for any deployment that doesn't configure TLS.
	if cfg.TLSDir != "" {
		ca, err := mtls.LoadOrCreate(cfg.TLSDir)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("load or create tls ca: %w", err)
		}
		srv.daemonCA = ca
		srv.gatewayCAPEM = ca.CertPEM()
	}

	runtime, err := buildRuntime(srv, cfg, store, newCommandBroker(broker), secretStore)
	if err != nil {
		conn.Close()
		return nil, err
	}
	// Computed once here, right after buildRuntime has resolved
	// runner.Backend from config (global-not-per-job selection) — see
	// usingContainerBackend's own doc comment for what Start()/the egress
	// proxy do with it.
	srv.usingContainerBackend = dispatcher.IsContainerBackend(runtime.runner.Backend)
	if srv.usingContainerBackend && srv.proxyManager != nil {
		srv.proxyManager.BindHost = composeBindHost
	}
	if srv.proxyManager != nil {
		// Make each egress proxy listener come back on the same port after
		// a daemon restart. Wired regardless of backend: nothing about port
		// stability is container-specific, and a job-side config baking a
		// proxy URL is just as stale-able either way.
		srv.proxyManager.PortStore = orchestrator.NewEgressPortStore(conn)
		srv.proxyManager.PortRangeLow = cfg.EgressProxyPortLow
		srv.proxyManager.PortRangeHigh = cfg.EgressProxyPortHigh
	}
	if err := mountRoutes(srv, runtime); err != nil {
		conn.Close()
		return nil, err
	}
	srv.httpServer.Handler = srv.router
	srv.workflow = runtime.workflow

	return srv, nil
}

// DB returns the database instance.
func (s *Server) DB() *sql.DB {
	return s.db
}

// Store returns the project store.
func (s *Server) Store() *orchestrator.ProjectStore {
	return s.store
}

// InstallID returns this installation's plain-UUID identity, or "" when
// cfg.InstallIDDir was empty (New never generated one). Exists so `boid
// reap` and containerBackend wiring have a single, already-tested source
// for the value rather than each re-implementing the load.
func (s *Server) InstallID() string {
	return s.installID
}

// KitsDir returns this daemon's effective base directory for installed
// kits. s.cfg is set once in New() and never mutated afterward, so no
// locking is needed, unlike the mutable s.hostCommands snapshot below.
// Backs GET /api/config/kits-dir (api.ConfigService) so a CLI client-side
// helper can resolve a legacy workspace.yaml's `kits:` references against
// the running daemon's actual kits directory, including one overridden by
// `boid start --kits-dir <custom>`.
func (s *Server) KitsDir() string {
	// Normalize to an absolute path before returning it over the wire
	// (codex PR7 review, round 3): `--kits-dir some/relative/path` gets
	// stored verbatim in s.cfg.KitsDir, so a CLI running in a different
	// cwd from the daemon would resolve that relative path against its
	// own cwd — pointing at an entirely different (possibly nonexistent)
	// kits directory and silently materializing kits from the wrong
	// place. filepath.Abs is stable (uses the daemon's cwd at the moment
	// of the call, but s.cfg.KitsDir is set once at startup and this
	// process's cwd never changes), so the result is deterministic per
	// daemon boot. If Abs fails for any reason (should never in practice,
	// since the value is a filesystem path we already use elsewhere), we
	// fall back to the raw value — better than dropping the endpoint
	// entirely and forcing the CLI into a hard error.
	if s.cfg.KitsDir == "" {
		return ""
	}
	if abs, err := filepath.Abs(s.cfg.KitsDir); err == nil {
		return abs
	}
	return s.cfg.KitsDir
}

// HostCommands returns a deep-copy snapshot of the aggregated host_commands
// config assembled at startup from every installed kit.yaml. A snapshot
// (rather than the live internal map) is returned so a caller mutating the
// result can never corrupt daemon state, and so ReloadHostCommands can
// safely swap s.hostCommands without racing against an in-flight caller.
func (s *Server) HostCommands() map[string]orchestrator.HostCommandSpec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return orchestrator.CloneHostCommandsMap(s.hostCommands)
}

// ReloadHostCommands re-reads the aggregated host_commands.yaml config from
// disk and swaps in both the raw snapshot (what HostCommands() hands out)
// and the dispatch-facing expanded copy wired into the ProjectStore. There
// is no create/edit API for individual host_command entries — only this
// reload of a hand-edited ~/.config/boid/host_commands.yaml.
//
// A parse error (a typo introduced by the hand edit) is returned as-is and
// leaves the daemon's live config untouched — s.hostCommands and the
// store's copy are only swapped after LoadHostCommandsConfig succeeds, so a
// bad reload never degrades an already-running daemon.
func (s *Server) ReloadHostCommands() error {
	path, err := orchestrator.DefaultHostCommandsPath()
	if err != nil {
		return fmt.Errorf("reload host_commands: resolve path: %w", err)
	}
	raw, err := orchestrator.LoadHostCommandsConfig(path)
	if err != nil {
		return fmt.Errorf("reload host_commands: %w", err)
	}

	s.mu.Lock()
	s.hostCommands = raw
	s.mu.Unlock()

	s.store.SetHostCommands(orchestrator.ExpandHostCommandsForDispatch(raw))
	return nil
}

// Router returns the chi router for registering additional routes.
func (s *Server) Router() chi.Router {
	return s.router
}

func (s *Server) Start(ctx context.Context) error {
	if s.installID != "" {
		slog.Info("install id", "id", s.installID)
	}

	// Start GC loop goroutine if configured.
	if s.gcLoop != nil {
		go s.gcLoop.Run(ctx)
	}

	// Start the queue wake-evaluation sweep loop (queue の決定論的評価:
	// wake 評価 rule).
	if s.queueSweepLoop != nil {
		go s.queueSweepLoop.Run(ctx)
	}

	// Start the trigger sweep loop (トリガのスケジュール/single-flight/実行記録).
	if s.triggerLoop != nil {
		go s.triggerLoop.Run(ctx)
	}

	// Per-daemon internal CA: loaded (or generated) in New(), not here —
	// see Server.daemonCA's own doc comment for why the load had to move
	// earlier (sandboxBackendForConfig needs it before Start ever runs).
	// s.daemonCA is nil whenever cfg.TLSDir is empty.
	daemonCA := s.daemonCA

	// Start broker
	if s.broker != nil {
		if daemonCA != nil {
			// composeBrokerServiceName is included as a SAN alongside the
			// loopback names below: the container backend dials the broker
			// by its compose service DNS name, not 127.0.0.1/localhost, so
			// hostname verification would otherwise fail. The SAN must be
			// present on the cert from day one since the CA/cert are
			// generated once per daemon lifetime, not per-caller.
			tlsCfg, err := daemonCA.ServerTLSConfig("127.0.0.1", "localhost", composeBrokerServiceName)
			if err != nil {
				return fmt.Errorf("broker tls config: %w", err)
			}
			// Bind host follows gatewayBindHost(s.usingContainerBackend),
			// the exact same helper the git gateway's own TLS listener
			// already uses just below — "0.0.0.0" (composeBindHost) when
			// the container backend is selected (a sibling job container
			// cannot reach a listener bound to THIS container's own
			// loopback interface), "127.0.0.1" otherwise. Unlike the git
			// gateway, the broker KEEPS mTLS (ServerTLSConfig above, not
			// ServerOnlyTLSConfig) — see mtls.CA.ServerOnlyTLSConfig's own
			// doc comment: the broker is an arbitrary-RPC endpoint, not a
			// single-purpose per-job-token-authorized clone endpoint like
			// the gateway, so per-connection client identity binding is
			// worth keeping — the per-job client cert
			// containerBackend.materializeBrokerClientCert issues is what
			// makes that requirement satisfiable by a real job container.
			s.broker.TLSAddr = gatewayBindHost(s.usingContainerBackend) + ":0"
			s.broker.TLSConfig = tlsCfg
		}
		if err := s.broker.Start(ctx); err != nil {
			return fmt.Errorf("start broker: %w", err)
		}
		slog.Info("broker started", "socket", s.broker.SocketPath)
		if addr := s.broker.TLSListenAddr(); addr != "" {
			slog.Info("broker tls listener started", "addr", addr)
			// brokerTLSSandboxAddr: the compose-service-DNS address a job
			// container's BOID_BROKER_TLS_ADDR env should dial —
			// "0.0.0.0:<port>" (the raw bind address above) is not itself
			// dialable by a peer, so this re-composes the SAME port with
			// composeBrokerServiceName as the host, mirroring
			// gatewayURLFor's own tlsPort extraction just below.
			// sandboxBackendForConfig already holds a pointer to this
			// exact field (srv.brokerTLSSandboxAddr, wired in buildRuntime
			// before Start ever ran) — see Server.brokerTLSSandboxAddr's
			// own doc comment — so writing it here is sufficient to make
			// containerBackend.Launch observe the real value on every
			// subsequent job dispatch, with no
			// further plumbing needed at this call site.
			if _, port, splitErr := net.SplitHostPort(addr); splitErr == nil {
				s.brokerTLSSandboxAddr = composeBrokerServiceName + ":" + port
			} else {
				slog.Warn("broker tls listener addr did not parse as host:port; BOID_BROKER_TLS_ADDR propagation into container-backend jobs will be empty",
					"addr", addr, "error", splitErr)
			}
		}
	}

	// Start proxy manager and the default-workspace listener. The default
	// listener's port is exposed via /api/proxy (back-compat) and used by
	// CLI flows that do not flow through dispatch (e.g. `boid exec`,
	// ProfileInit sandboxes). Per-workspace listeners are lazily allocated
	// at dispatch time — see Runner.Dispatch.
	if s.proxyManager != nil {
		s.proxyManager.Start(ctx)
		port, err := s.proxyManager.GetOrCreate(orchestrator.DefaultWorkspaceSlug, s.cfg.AllowedDomains)
		if err != nil {
			return fmt.Errorf("start default proxy: %w", err)
		}
		s.proxyPort = port
		slog.Info("proxy started", "port", port, "workspace", orchestrator.DefaultWorkspaceSlug)

		// Dedicated listener for jobs with no workspace at all (`boid exec` /
		// ProfileInit against an unlinked project) — bound once here, under
		// dispatcher.NoWorkspaceProxyKey, a key DISTINCT from
		// orchestrator.DefaultWorkspaceSlug — see Runner.NoWorkspaceProxyPort's
		// own doc comment for the aliasing bug this distinctness prevents.
		// Bound once, never refreshed per-dispatch — sandbox.allowed_domains
		// is ReloadRestartRequired, so a restart is exactly what's needed to
		// pick up a changed floor here too, same as the default listener
		// above.
		noWSPort, err := s.proxyManager.GetOrCreate(dispatcher.NoWorkspaceProxyKey, s.cfg.AllowedDomains)
		if err != nil {
			return fmt.Errorf("start no-workspace proxy: %w", err)
		}
		s.noWorkspaceProxyPort = noWSPort
		slog.Info("proxy started", "port", noWSPort, "workspace", "<no-workspace>")
	}

	// git gateway: bind its listener before the UNIX/TCP listeners below so
	// srv.gatewayURL (and, via the WireConfig.GatewayURL pointer,
	// SandboxRuntimeInfo.GatewayURL) is populated by the time the first job
	// dispatches. The plaintext (non-TLS) listener stays loopback-only
	// (127.0.0.1) regardless of backend — never expose plaintext off-box —
	// and userns sandboxes reach it via the slirp-provided 10.0.2.2 alias,
	// so the URL exposed to dispatch uses that address instead of
	// 127.0.0.1 for that backend.
	if s.gatewayHTTPServer != nil {
		gwLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return fmt.Errorf("listen git gateway: %w", err)
		}
		s.gatewayLn = gwLn
		port := gwLn.Addr().(*net.TCPAddr).Port
		// gatewayURLFor's BackendUserns branch reproduces
		// fmt.Sprintf("http://10.0.2.2:%d", port) literally for every
		// deployment that has not opted into sandbox.backend: container —
		// see its own doc comment. When s.usingContainerBackend is true,
		// this initial value is OVERWRITTEN below once the TLS listener is
		// bound (a container-backend job never reaches this plaintext
		// loopback listener at all — only the compose service DNS name
		// over mTLS).
		s.gatewayURL = gatewayURLFor(false, port, 0)
		go func() { _ = s.gatewayHTTPServer.Serve(gwLn) }() // returns ErrServerClosed on Stop
		slog.Info("git gateway started", "addr", gwLn.Addr().String(), "sandbox_url", s.gatewayURL)

		// git gateway TCP(mTLS) listener: additive alongside the plaintext
		// loopback listener above.
		//
		// composeGatewayServiceName is included as a SAN for the same
		// reason as the broker's above: it matches SandboxURL's own
		// BackendContainer default ("boid-gateway", see sandbox_url.go) so
		// a container-backend caller that leaves
		// SandboxURLOptions.ServiceName unset gets a URL whose hostname
		// this cert already covers.
		//
		// The listener bind address is gatewayBindHost(s.usingContainerBackend)
		// — "0.0.0.0" instead of loopback-only "127.0.0.1" when the
		// container backend is selected, since a sibling job container on
		// the shared compose network cannot reach a listener bound to THIS
		// container's own loopback interface. See composeBindHost's own
		// doc comment for why this is safe (the compose network boundary,
		// not the bind address, is what scopes reachability).
		if daemonCA != nil && s.combinedGatewayHandler != nil {
			bindHost := gatewayBindHost(s.usingContainerBackend)
			// ServerOnlyTLSConfig, not ServerTLSConfig: the git gateway's
			// own per-job Registry token (URL-path-embedded, verified by
			// gatewayHandler's ServeHTTP) already fully authorizes every
			// request — a required client certificate would add no
			// per-job authorization on top of that, and no client cert
			// issuance/delivery was ever wired for this listener (see
			// ServerOnlyTLSConfig's own doc comment). Requiring one
			// anyway makes the listener unusable by any real sandbox
			// (every clone attempt fails the TLS handshake with "tls:
			// client didn't provide a certificate"). The API gateway's
			// own per-job Registry token is authorized the identical way,
			// so this reasoning covers both gateways sharing this
			// listener.
			tlsCfg, err := daemonCA.ServerOnlyTLSConfig("127.0.0.1", "localhost", composeGatewayServiceName)
			if err != nil {
				return fmt.Errorf("git gateway tls config: %w", err)
			}
			// tls.Listen + s.combinedGatewayHandler directly, rather than
			// gitgateway.Server.ListenTLS (which hardcodes its OWN
			// ServeHTTP as the *http.Server's Handler — see
			// gatewayTLSHTTPServer's own doc comment for why that no
			// longer fits once the API gateway shares this listener).
			gwTLSLn, err := tls.Listen("tcp", bindHost+":0", tlsCfg)
			if err != nil {
				return fmt.Errorf("listen git gateway tls: %w", err)
			}
			s.gatewayTLSLn = gwTLSLn
			tlsSrv := &http.Server{Handler: s.combinedGatewayHandler}
			s.gatewayTLSHTTPServer = tlsSrv
			go func() { _ = tlsSrv.Serve(gwTLSLn) }() // returns ErrServerClosed on Stop
			slog.Info("git gateway tls listener started", "addr", gwTLSLn.Addr().String())

			// Once the container backend is selected AND the TLS listener
			// is actually up, gatewayURL is replaced with the
			// compose-service-DNS + mTLS URL — the ONLY address a sibling
			// job container can reach this daemon at (`10.0.2.2` is a
			// pasta/slirp userns artifact with no docker equivalent).
			// Falls through to the plaintext BackendUserns URL set above
			// when usingContainerBackend is false — gatewayURLFor's own
			// default branch, unreachable in production since the userns
			// backend was removed (see gatewayURLFor's own doc comment).
			if s.usingContainerBackend {
				tlsPort := gwTLSLn.Addr().(*net.TCPAddr).Port
				s.gatewayURL = gatewayURLFor(true, port, tlsPort)
				slog.Info("git gateway sandbox url (container backend)", "sandbox_url", s.gatewayURL)
			}
		} else if s.usingContainerBackend {
			// The container backend REQUIRES the mTLS listener (it never
			// reaches the plaintext loopback listener at all — see above);
			// without cfg.TLSDir configured (daemonCA nil) or the combined
			// gateway handler unset, gatewayURL is left at the useless
			// plaintext-loopback value computed above, which no job
			// container could ever dial. Surfaced loudly so an operator
			// running sandbox.backend: container without cfg.TLSDir set
			// (cmd/start.go's default IS to set it, but any custom wiring
			// that doesn't would land here) finds out from the log rather
			// than from every job's git clone (or API gateway request)
			// timing out.
			slog.Error("sandbox.backend: container is selected but no TLS CA is configured (cfg.TLSDir unset) or the gateway handler is missing; job containers will not be able to reach the git/API gateway at all (docs/plans/phase6-container-backend.md §決定5)")
		}
	}

	// Remove stale socket
	os.Remove(s.cfg.SocketPath)

	unixLn, err := net.Listen("unix", s.cfg.SocketPath)
	if err != nil {
		return fmt.Errorf("listen unix: %w", err)
	}
	s.unixLn = unixLn
	go func() { _ = s.httpServer.Serve(unixLn) }() // returns ErrServerClosed on Stop

	tcpLn, err := net.Listen("tcp", s.cfg.HTTPAddr)
	if err != nil {
		unixLn.Close()
		return fmt.Errorf("listen tcp: %w", err)
	}
	s.tcpLn = tcpLn

	// The TCP listener is potentially externally exposed (direct bind, tunnel,
	// shared-host loopback), so it is served the auth-wrapped handler rather
	// than the bare router. mountRoutes always sets tcpHandler; the fallback
	// only guards against a misconstructed Server in tests.
	tcpHandler := s.tcpHandler
	if tcpHandler == nil {
		tcpHandler = s.router
	}
	s.tcpServer = &http.Server{Handler: tcpHandler}
	go func() { _ = s.tcpServer.Serve(tcpLn) }() // returns ErrServerClosed on Stop

	// Dedicated CLI TCP listener: additive alongside tcpLn/tcpServer above,
	// on its own port — see Config.CLIAddr's own doc comment for
	// why a separate listener. Gated on BOTH cfg.CLIAddr and cfg.CLIToken
	// being set — unlike tcpLn (which always binds), this listener has no
	// fallback auth path at all (no TLS, no cookie, no loopback trust), so
	// an unset token must skip binding entirely rather than open an
	// unauthenticated port.
	if s.cfg.CLIAddr != "" && s.cfg.CLIToken != "" {
		_, cliPort, splitErr := net.SplitHostPort(s.cfg.CLIAddr)
		if splitErr != nil {
			return fmt.Errorf("parse cli addr %q: %w", s.cfg.CLIAddr, splitErr)
		}
		// gatewayBindHost(s.usingContainerBackend) — the same
		// container-backend-aware routing the git gateway/broker TLS
		// listeners already use (wire.go), not a hardcoded "127.0.0.1":
		// under the container backend, this daemon runs inside a compose
		// container
		// reached via docker's bridge-mode port publish
		// (`127.0.0.1:8442:8442` in build/container/compose.yml) — the
		// HOST-side 127.0.0.1 is DNAT'd to the CONTAINER'S bridge-network
		// IP, never the container's own loopback interface, so a listener
		// bound to the container's 127.0.0.1 is unreachable from the host
		// (container-backend E2E's `GET http://127.0.0.1:8442/api/health =
		// 000`). The userns backend keeps the pre-existing loopback-only
		// bind unchanged — see Config.CLIAddr's own doc comment for why
		// this listener still needs no TLS/loopback-trust fallback: the
		// host-side docker publish is itself loopback-only, and
		// auth.NewCLITokenAuthMiddleware's Bearer token is the only trust
		// mechanism regardless of which interface the container-side
		// listener binds.
		cliBindAddr := gatewayBindHost(s.usingContainerBackend) + ":" + cliPort
		cliLn, err := net.Listen("tcp", cliBindAddr)
		if err != nil {
			return fmt.Errorf("listen cli: %w", err)
		}
		s.cliLn = cliLn
		cliHandler := s.cliHandler
		if cliHandler == nil {
			cliHandler = s.router
		}
		s.cliServer = &http.Server{Handler: cliHandler}
		go func() { _ = s.cliServer.Serve(cliLn) }() // returns ErrServerClosed on Stop
		slog.Info("cli listener started", "addr", cliLn.Addr().String())
	}

	return nil
}

func (s *Server) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var errs []error
	if s.httpServer != nil {
		if err := s.httpServer.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.tcpServer != nil {
		if err := s.tcpServer.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.cliServer != nil {
		if err := s.cliServer.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.gatewayHTTPServer != nil {
		if err := s.gatewayHTTPServer.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.gatewayTLSHTTPServer != nil {
		// gatewayTLSHTTPServer.Shutdown (rather than s.gatewayTLSLn.Close())
		// also closes idle keep-alive connections already accepted on the
		// TLS listener — a bare listener Close only stops new connections
		// from being accepted. gatewayTLSHTTPServer is
		// always non-nil here whenever gatewayTLSLn is: both are set
		// together inside the `s.combinedGatewayHandler != nil` guard in
		// Start.
		ctx, cancel := context.WithTimeout(context.Background(), gatewayTLSShutdownTimeout)
		if err := s.gatewayTLSHTTPServer.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
		cancel()
	}
	// Cancel dispatch-loop context and wait for all goroutines to finish
	// before closing the database; otherwise in-flight loops hit "db closed".
	if s.workflow != nil {
		s.workflow.Shutdown()
	}
	if s.db != nil {
		if err := s.db.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.proxyManager != nil {
		s.proxyManager.StopAll()
	}
	if s.broker != nil {
		s.broker.Stop()
	}
	// Do NOT os.Remove(s.cfg.SocketPath) here. The UNIX socket file was already
	// unlinked by httpServer.Close() above (net.UnixListener.Close unlinks its
	// own socket exactly once via the fd it owns). A blind path-based removal is
	// unsafe across a fast restart: `httpServer.Close()` unlinks our socket early,
	// so a successor daemon can create a brand-new socket at the same path (tmpfs
	// even reuses the inode number) while this Stop() is still draining
	// workflow.Shutdown()/db.Close() — a variable-length wait gated on killing
	// in-flight hooks. If we then removed the path, we would delete the *successor's*
	// live socket, leaving clients with ENOENT. That is the daemon-restart-resume
	// flake. Any stale socket from an unclean crash is cleared by Start's
	// os.Remove(s.cfg.SocketPath) before it re-listens.

	if len(errs) > 0 {
		return fmt.Errorf("stop errors: %v", errs)
	}
	return nil
}

// ProxyPort returns the proxy listening port.
func (s *Server) ProxyPort() int {
	return s.proxyPort
}

// SocketPath returns the UNIX socket path.
func (s *Server) SocketPath() string {
	return s.cfg.SocketPath
}

// BrokerSocket returns the broker UNIX socket path.
func (s *Server) BrokerSocket() string {
	if s.broker != nil {
		return s.broker.SocketPath
	}
	return ""
}

// BrokerTLSAddr returns the broker's TCP(mTLS) listener address, or "" when
// cfg.TLSDir was unset (no TLS listener bound) or before Start has bound
// it.
func (s *Server) BrokerTLSAddr() string {
	if s.broker != nil {
		return s.broker.TLSListenAddr()
	}
	return ""
}

// GatewayTLSAddr returns the git gateway's TCP(mTLS) listener address, or
// "" when cfg.TLSDir was unset (no TLS listener bound) or before Start has
// bound it.
func (s *Server) GatewayTLSAddr() string {
	if s.gatewayTLSLn != nil {
		return s.gatewayTLSLn.Addr().String()
	}
	return ""
}

// TCPAddr returns the TCP listener address.
func (s *Server) TCPAddr() string {
	if s.tcpLn != nil {
		return s.tcpLn.Addr().String()
	}
	return ""
}

// CLIListenAddr returns the dedicated CLI TCP listener's bound address, or
// "" when it was never bound (cfg.CLIAddr=="" or cfg.CLIToken=="" — see
// Config.CLIAddr's own doc comment) or before Start has run.
func (s *Server) CLIListenAddr() string {
	if s.cliLn != nil {
		return s.cliLn.Addr().String()
	}
	return ""
}

// GatewayURL returns the git gateway's sandbox-facing base URL
// (http://10.0.2.2:<port>), or "" before Start has bound its listener.
// Wired into SandboxRuntimeInfo.GatewayURL via
// dispatcher.WireConfig.GatewayURL.
func (s *Server) GatewayURL() string {
	return s.gatewayURL
}

// GatewayCAPEM returns the daemon's internal CA's own PEM-encoded
// certificate (see the gatewayCAPEM field's own doc comment), or nil
// before Start has loaded/created it (cfg.TLSDir unset, or Start hasn't
// run). Exposed for internal/dispatcher.WireConfig.GatewayCAPEM
// (via internal/server/wire.go) and for tests — see
// server_container_backend_gateway_test.go.
func (s *Server) GatewayCAPEM() []byte {
	return s.gatewayCAPEM
}
