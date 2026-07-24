package server

import (
	"context"
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
	"github.com/novshi-tech/boid/internal/skills"
)

// gatewayTLSShutdownTimeout bounds how long Stop waits for the git
// gateway's TLS listener to drain in-flight requests before giving up
// (codex review [Minor 4] on docs/plans/phase6-container-backend.md
// §PR4). Matches the daemon's other best-effort shutdown waits in shape —
// short enough that a stuck request can't hang `boid stop` indefinitely.
const gatewayTLSShutdownTimeout = 5 * time.Second

// composeBrokerServiceName and composeGatewayServiceName are the compose
// network DNS names the broker's and git gateway's TCP(mTLS) listener
// certs advertise as SANs, alongside the loopback names every PR4 caller
// actually dials (codex review [Major 2] on
// docs/plans/phase6-container-backend.md §PR4). No PR4 caller resolves
// these names yet — the container backend that will is PR5+ — but the
// SAN has to be on the cert from the moment the CA issues it (once per
// daemon lifetime), not retrofitted once a real caller shows up.
// composeGatewayServiceName intentionally matches
// gitgateway.SandboxURLOptions.ServiceName's own BackendContainer default
// ("boid-gateway", see internal/gitgateway/sandbox_url.go) rather than
// inventing a second name for the same service.
//
// The egress proxy's own compose-network DNS name ("boid-egress" — [Blocker
// 2, PR7 codex review]) is declared as dispatcher.composeEgressServiceName,
// not duplicated here: internal/server already imports internal/dispatcher
// (the reverse would cycle), and dispatcher.SandboxRuntimeInfo.ProxyHost is
// the only place that literal is actually consumed — see its own doc
// comment. It must still match this file's composeBindHost value and
// build/container/compose.yml's own "boid-egress" alias.
//
// composeBindHost is the listen address the git gateway's TCP(mTLS)
// listener and the default-workspace egress proxy listener bind to when
// the container backend is selected, instead of the loopback-only
// "127.0.0.1" every pre-PR7 (and every non-container-backend) deployment
// uses. A sibling job container reaches this daemon over the shared
// compose network by its own container IP, not 127.0.0.1 — a
// loopback-bound listener is unreachable from outside this daemon's own
// container entirely, so a job could dial the DNS-resolved address and
// still get connection-refused. "0.0.0.0" (all interfaces) is simplest and
// matches how every other example containerized Go service exposes a port
// to its own docker network; the daemon container's own network
// boundary (boid_internal being `internal: true`, §決定5) is what actually
// scopes reachability, not the bind address itself. The plaintext
// (non-TLS) git gateway HTTP listener is deliberately NOT included here —
// it stays loopback-only regardless of backend (never expose plaintext off
// -box); only the already-mTLS-protected TCP listeners move.
const (
	composeBrokerServiceName  = "boid-broker"
	composeGatewayServiceName = "boid-gateway"
	composeBindHost           = "0.0.0.0"
)

type Config struct {
	DBPath         string
	SocketPath     string
	HTTPAddr       string
	KitsDir        string                // base dir for installed kit repos
	KeyFilePath    string                // path to secret encryption key file
	AllowedDomains []string              // proxy allowed domains
	JobRuntime     dispatcher.JobRuntime //nolint:staticcheck // SA1019: JobRuntime's Deprecated marker (Phase 6 PR9 skeleton) flags its own type, not a call to remove now — usernsBackend (this field's only production use) is still the default backend; actual retirement is a follow-up PR (docs/plans/phase6-cutover-followups.md).
	// TLSDir, when non-empty, is the directory holding (or to generate)
	// the per-daemon internal CA (ca.crt/ca.key) used to secure the
	// broker/git-gateway TCP(mTLS) listeners added in
	// docs/plans/phase6-container-backend.md §PR4/§決定5. Empty (the
	// zero value, and every pre-PR4 caller/test) skips binding those
	// listeners entirely — only the pre-existing UNIX socket / plaintext
	// loopback listeners are bound, i.e. today's behavior is unchanged.
	// cmd/start.go sets a real default (~/.local/share/boid/tls) so a
	// live daemon actually binds them; nothing dispatches through them
	// yet (the container backend that will is PR5).
	TLSDir string
	// InstallIDDir, when non-empty, is the directory holding (or to
	// generate) this installation's plain-UUID install_id file
	// (internal/install.LoadOrCreate — docs/plans/
	// phase6-container-backend.md §PR6/§決定6: "daemon が作る全
	// container/network/volume に boid.install_id... label を付与"). Empty
	// (the zero value, and every pre-PR6 test) skips this entirely — New
	// leaves Server.installID at "" and nothing changes. cmd/start.go sets
	// a real default (~/.local/share/boid, the same dir web_secret and
	// boid.db live in) so a live daemon actually has one; nothing consumes
	// it in production dispatch yet (containerBackend construction is
	// still test/DI-only — see NewContainerBackend's doc comment — the
	// same "config 非公開" scope PR5 shipped under).
	InstallIDDir string
}

type Server struct {
	cfg          Config
	db           *sql.DB
	store        *orchestrator.ProjectStore
	broker       *sandbox.Broker
	secretStore  *dispatcher.SecretStore
	proxyManager *sandbox.ProxyManager
	proxyPort    int // port of the default-workspace listener (back-compat /api/proxy surface)
	router       chi.Router
	unixLn       net.Listener
	tcpLn        net.Listener
	httpServer   *http.Server
	// tcpHandler wraps the router with transport-aware API auth and is served
	// to the TCP listener only. The UNIX socket is served the bare router
	// (trusted CLI/agent transport). Set by mountRoutes.
	tcpHandler http.Handler
	tcpServer  *http.Server
	gcLoop     *orchestrator.GCLoop // nil if GC is disabled
	workflow   *api.TaskWorkflowService

	// hostCommands is the aggregated host_commands config assembled by
	// buildProjectStore's preflight (docs/plans/workspace-db-consolidation.md
	// PR2): every installed kit.yaml's host_commands, deduped by identical
	// definition, name-collision-checked, and mirrored to
	// orchestrator.DefaultHostCommandsPath() on disk. This is the live
	// dispatch route as of PR3's cutover — workspace.HostCommands ([]string
	// reference names) resolves against this map in ProjectStore.
	// GetWithWorkspace. The former per-kit resolver path (KitResolver in
	// buildProjectStore, kept only for the PR2/PR3 parity-verification
	// window) was removed in PR6 (kit mechanism retirement).
	hostCommands map[string]orchestrator.HostCommandSpec

	// gitgateway 4-point set (docs/plans/git-gateway-cutover.md PR4): the
	// authenticating reverse proxy sandboxes will eventually clone through
	// (PR5) and cutover env-var-advertise (PR6). Inert in this PR — nothing
	// dispatches through it yet, but the daemon builds, listens, and tears it
	// down like every other subserver.
	//
	// gatewayRegistry is constructed early in New() (buildRuntime) and shared
	// with dispatcher.Runner so Dispatch/UnregisterJob can
	// Register/Unregister job tokens; gatewayHTTPServer wraps the
	// gitgateway.Server handler (built once config + notify are available)
	// and is only bound to a listener in Start().
	gatewayRegistry   *gitgateway.Registry
	gatewayHTTPServer *http.Server
	gatewayLn         net.Listener
	// gatewayHandler is the same *gitgateway.Server wrapped by
	// gatewayHTTPServer.Handler, kept as its own field (rather than a
	// type assertion at Start time) so Start can additionally bind the
	// TCP(mTLS) listener via gatewayHandler.ListenTLS
	// (docs/plans/phase6-container-backend.md §PR4). nil until wire.go
	// constructs it (mirrors gatewayHTTPServer's construction timing).
	gatewayHandler *gitgateway.Server
	// gatewayTLSLn is the git gateway's TCP(mTLS) listener, bound in
	// Start only when cfg.TLSDir is set. nil otherwise.
	gatewayTLSLn net.Listener
	// gatewayURL is the sandbox-facing base URL (http://10.0.2.2:<port>),
	// populated by Start() once the listener is bound. Empty before Start
	// completes. Runner holds a pointer to this string (WireConfig.GatewayURL)
	// so SandboxRuntimeInfo.GatewayURL reflects it at dispatch time — the
	// same late-binding-via-pointer trick as proxyPort.
	gatewayURL string
	// gatewayCAPEM is the daemon's internal CA's own certificate
	// (mtls.CA.CertPEM), PEM-encoded, populated by Start() alongside
	// daemonCA whenever cfg.TLSDir is set. Empty when TLS isn't configured
	// (every pre-PR9-fix caller/test). This is the client-side half of
	// the git gateway TLS listener's trust: a container-backend sandbox
	// needs this CA's public cert to verify the gateway's server
	// certificate (gatewayHandler.ListenTLS's tlsCfg, below) — non-secret,
	// so no per-job materialization/rotation is needed, unlike a client
	// certificate would be. Runner holds a pointer to this slice
	// (WireConfig.GatewayCAPEM), the same late-binding-via-pointer pattern
	// gatewayURL already uses, so SandboxRuntimeInfo.GatewayCAPEM reflects
	// it at dispatch time even though it (like gatewayURL) is only known
	// once Start has run.
	gatewayCAPEM []byte

	// daemonCA is the per-daemon internal CA (docs/plans/
	// phase6-container-backend.md §PR4/§決定5), loaded (or generated) once
	// in New() when cfg.TLSDir is set. Empty (nil) otherwise — every call
	// site that consumes it already treats a nil CA as "TLS not
	// configured, skip the additive TCP(mTLS) listener" (Start's own
	// `if daemonCA != nil` guards), so this is the exact same contract the
	// pre-broker-TCP-wire local `var daemonCA *mtls.CA` inside Start used
	// to have.
	//
	// Loaded here (in New(), before buildRuntime runs) rather than lazily
	// inside Start the way every pre-this-PR version did, because
	// buildRuntime's sandboxBackendForConfig call (internal/server/
	// wire.go) constructs the container backend's ContainerBackendOptions
	// — which now needs this exact CA to issue per-job broker client
	// certs, docs/plans/phase6-cutover-followups.md §⓪ — strictly BEFORE
	// Start ever runs. Start() below reads this field instead of loading
	// its own second (content-identical, since mtls.LoadOrCreate is
	// idempotent) copy.
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

	// installID is this installation's plain-UUID identity (§決定6), loaded
	// (or generated) once in New() when cfg.InstallIDDir is set. Empty
	// otherwise — see InstallIDDir's doc comment.
	installID string

	// usingContainerBackend records whether buildRuntime selected the
	// container backend for this daemon's dispatcher.Runner ([Blocker 2,
	// PR7 codex review] — docs/plans/phase6-container-backend.md §決定11:
	// backend selection is GLOBAL for the whole daemon, not per-job, so
	// this is computed once in New() and never changes for the life of the
	// process. Start() reads it to decide which addressing scheme
	// (loopback 10.0.2.2 projection vs. compose service DNS + TLS) the
	// git gateway's sandbox-facing URL and listener bind address use, and
	// which host the egress proxy env advertises to job containers — see
	// gatewayURLFor/gatewayBindHost and ProxyManager.BindHost's own doc
	// comments.
	usingContainerBackend bool

	mu sync.Mutex

	// configMu serializes read-modify-write config.yaml mutations
	// (docs/plans/volume-only-daemon.md §論点 f, concurrency decision:
	// "set and apply should be serialized... two concurrent sets must not
	// interleave and produce a torn config"). Deliberately a separate lock
	// from mu (hostCommands' own lock) — config-apply's validate+write+
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
	// keys per docs/plans/volume-only-daemon.md §論点 f). nil in any test
	// that builds a *Server without going through buildRuntime's notifySvc
	// wiring — ApplyConfigYAML treats that as "no live notify service to
	// update", not an error.
	notifySvc *notify.Service
	// configRevision is a monotonic, process-lifetime-scoped concurrency
	// token for config.yaml (BLOCKER 1, codex review round 1: the
	// ETag/If-Match convention `boid workspace edit` already established —
	// internal/api/project_service.go's UpdateWorkspace,
	// cmd/workspace.go's runWorkspaceEdit — extended to `boid config
	// edit`/`apply -f`). Every successful ApplyConfigYAML/MutateConfig
	// write bumps it under configMu (the same lock already serializing
	// every config.yaml read-modify-write, so no separate lock is needed).
	// GET /api/config exposes the current value as an ETag; POST
	// /api/config's optional If-Match is checked against it before
	// applying. Unlike workspace revisions (DB-row updated_at timestamps),
	// config.yaml has no DB row of its own, so this is a plain in-process
	// counter — it only needs to detect "did another write happen since my
	// GET", not survive a daemon restart the way a DB-backed revision
	// would (a restart already invalidates every other piece of ephemeral
	// client-side CLI state). Starts at 1 once buildRuntime populates
	// liveConfig; 0 is never a valid revision (only the pre-buildRuntime,
	// defensive-only zero value of *Server).
	configRevision uint64
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

	if cfg.DBPath != "" && cfg.DBPath != ":memory:" {
		skillsDir := filepath.Join(filepath.Dir(cfg.DBPath), "skills")
		if err := skills.DeployAll(skillsDir); err != nil {
			d.Close()
			return nil, fmt.Errorf("deploy skills: %w", err)
		}
	}

	// Install identity (docs/plans/phase6-container-backend.md §PR6/§決定6):
	// loaded (or generated once) here, alongside the other on-disk
	// daemon-identity artifacts New() already establishes (skills,
	// migrations). Empty cfg.InstallIDDir (every pre-PR6 caller/test) skips
	// this — installID stays "".
	//
	// Advisory, not blocking (Major 5, PR6 codex review): install_id is a
	// container-backend concept — its only consumers are containerBackend's
	// boid.install_id resource label (§決定6) and `boid reap`'s label
	// filter, neither of which the userns backend touches at all. Failing
	// New() outright over a LoadOrCreate error (e.g. cfg.InstallIDDir
	// root-owned from a prior run under a different uid) would refuse to
	// start a userns daemon over a value it never uses — so a failure here
	// is logged and installID stays "" instead. containerBackend's own
	// ReapOrphans doc comment already documents its pre-install_id "global
	// (not install_id-scoped) label filter" fallback for exactly this
	// empty-installID case, so nothing downstream needs to special-case
	// this: it is the same path New() has always taken when
	// cfg.InstallIDDir itself was empty.
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

	// host_commands provisioning check (docs/plans/phase6-container-backend.md
	// §PR6/§決定4): resolve every configured host command against this
	// daemon's own host now, at boot, instead of only discovering a gap the
	// first time an affected project dispatches. Advisory only (skeleton
	// level) — a missing command is warned about, not fatal; see
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

	// Per-daemon internal CA (docs/plans/phase6-container-backend.md
	// §PR4/§決定5): hoisted here — before buildRuntime runs — rather than
	// lazily loaded inside Start the way every pre-broker-TCP-wire version
	// did. See Server.daemonCA's own doc comment for why: buildRuntime's
	// sandboxBackendForConfig call needs this CA to configure the
	// container backend's per-job broker client cert issuance
	// (docs/plans/phase6-cutover-followups.md §⓪), and that call happens
	// strictly before Start ever runs. cfg.TLSDir empty (every pre-PR4
	// caller/test) leaves srv.daemonCA/gatewayCAPEM at their zero values,
	// so this hoist changes nothing for any deployment that doesn't
	// configure TLS.
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
	// [Blocker 2, PR7 codex review]: computed once here, right after
	// buildRuntime has resolved runner.Backend from config (§決定11's
	// global-not-per-job selection) — see usingContainerBackend's own doc
	// comment for what Start()/the egress proxy do with it.
	srv.usingContainerBackend = dispatcher.IsContainerBackend(runtime.runner.Backend)
	if srv.usingContainerBackend && srv.proxyManager != nil {
		srv.proxyManager.BindHost = composeBindHost
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

// InstallID returns this installation's plain-UUID identity (§決定6), or
// "" when cfg.InstallIDDir was empty (New never generated one). Nothing in
// production dispatch consumes this yet as of PR6 (containerBackend
// construction is still test/DI-only) — it exists so `boid reap` and a
// future PR7 containerBackend wiring have a single, already-tested source
// for the value rather than each re-implementing the load.
func (s *Server) InstallID() string {
	return s.installID
}

// KitsDir returns this daemon's effective base directory for installed kits
// (MAJOR 1, codex review round 1, docs/plans/workspace-db-consolidation.md
// Phase 2.5 PR7): s.cfg is set once in New() and never mutated afterward, so
// no locking is needed, unlike the mutable s.hostCommands snapshot below.
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

// AllowedDomains returns a snapshot of the daemon's current sandbox egress
// allowlist (s.cfg.AllowedDomains) — the exact value sandbox.ProxyManager.
// GetOrCreate reads fresh on every dispatch. Exposed primarily so
// ApplyConfigYAML's `sandbox.allowed_domains` hot-reload (internal/server/
// config_edit.go) is externally observable/testable without a full sandbox
// dispatch, mirroring KitsDir()'s existing "expose an effective daemon
// setting read-only" convention.
func (s *Server) AllowedDomains() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cfg.AllowedDomains...)
}

// HostCommands returns a deep-copy snapshot of the aggregated host_commands
// config assembled at startup from every installed kit.yaml
// (docs/plans/workspace-db-consolidation.md PR2). It is read-only reference
// data during PR2/PR3's parity-verification window — nothing dispatches
// through it yet. A snapshot (rather than the live internal map) is returned
// so a caller mutating the result can never corrupt daemon state, and so a
// future reload API (PR4) can safely swap s.hostCommands without racing
// against an in-flight caller.
func (s *Server) HostCommands() map[string]orchestrator.HostCommandSpec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return orchestrator.CloneHostCommandsMap(s.hostCommands)
}

// ReloadHostCommands re-reads the aggregated host_commands.yaml config from
// disk and swaps in both the raw snapshot (what HostCommands() hands out)
// and the dispatch-facing expanded copy wired into the ProjectStore
// (docs/plans/workspace-db-consolidation.md PR4 Step G). This is the
// daemon-side half of the plan doc's documented hand-edit path
// ("host_commands 実定義の集約先": 手で ~/.config/boid/host_commands.yaml を編集
// → boid host-commands reload で daemon に読み直させる) — there is no
// create/edit API for individual host_command entries, only this reload.
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

	// Per-daemon internal CA (docs/plans/phase6-container-backend.md
	// §PR4/§決定5): loaded (or generated) in New(), not here — see
	// Server.daemonCA's own doc comment for why the load had to move
	// earlier (the broker TCP wire followup's sandboxBackendForConfig
	// needs it before Start ever runs). s.daemonCA is nil whenever
	// cfg.TLSDir is empty (every pre-PR4 caller/test), so Start's behavior
	// below is byte-for-byte unchanged from before this hoist — the two
	// `if daemonCA != nil` blocks are the only places this affects
	// anything.
	daemonCA := s.daemonCA

	// Start broker
	if s.broker != nil {
		if daemonCA != nil {
			// composeBrokerServiceName is included as a SAN alongside the
			// loopback names below (codex review [Major 2] on PR4): the
			// container backend (PR5+) dials the broker by its compose
			// service DNS name, not 127.0.0.1/localhost, so hostname
			// verification would otherwise fail once that caller exists.
			// PR4 itself only ever dials this listener via 127.0.0.1
			// (see TestServer's own tests) — the extra SAN is inert until
			// PR5 but must be present on the cert from day one since the
			// CA/cert are generated once per daemon lifetime, not
			// per-caller.
			tlsCfg, err := daemonCA.ServerTLSConfig("127.0.0.1", "localhost", composeBrokerServiceName)
			if err != nil {
				return fmt.Errorf("broker tls config: %w", err)
			}
			// broker TCP wire completion (docs/plans/
			// phase6-cutover-followups.md §⓪): bind host now follows
			// gatewayBindHost(s.usingContainerBackend), the exact same
			// helper the git gateway's own TLS listener already uses just
			// below — "0.0.0.0" (composeBindHost) when the container
			// backend is selected (a sibling job container cannot reach a
			// listener bound to THIS container's own loopback interface),
			// "127.0.0.1" (byte-for-byte the pre-fix hardcoded literal)
			// otherwise. Unlike the git gateway, the broker KEEPS mTLS
			// (ServerTLSConfig above, not ServerOnlyTLSConfig) — see
			// mtls.CA.ServerOnlyTLSConfig's own doc comment and
			// ContainerBackendOptions.BrokerTLSCA's for the design
			// decision (broker is an arbitrary-RPC endpoint, not a
			// single-purpose per-job-token-authorized clone endpoint like
			// the gateway, so per-connection client identity binding is
			// worth keeping) — the per-job client cert
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
			// brokerTLSSandboxAddr (broker TCP wire completion): the
			// compose-service-DNS address a job container's
			// BOID_BROKER_TLS_ADDR env should dial — "0.0.0.0:<port>" (the
			// raw bind address above) is not itself dialable by a peer, so
			// this re-composes the SAME port with composeBrokerServiceName
			// as the host, mirroring gatewayURLFor's own tlsPort
			// extraction just below. sandboxBackendForConfig already holds
			// a pointer to this exact field (srv.brokerTLSSandboxAddr,
			// wired in buildRuntime before Start ever ran) — see
			// Server.brokerTLSSandboxAddr's own doc comment — so writing it
			// here is sufficient to make containerBackend.Launch observe
			// the real value on every subsequent job dispatch, with no
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
		// [Blocker 2, PR7 codex review]: gatewayURLFor's BackendUserns
		// branch reproduces the pre-PR7 fmt.Sprintf("http://10.0.2.2:%d",
		// port) literally (docs/plans/phase6-container-backend.md §PR4:
		// "既存 (10.0.2.2) を無条件で切り替える禁止") for every deployment that
		// has not opted into sandbox.backend: container — see its own doc
		// comment. When s.usingContainerBackend is true, this initial value
		// is OVERWRITTEN below once the TLS listener is bound (a container
		// -backend job never reaches this plaintext loopback listener at
		// all — only the compose service DNS name over mTLS).
		s.gatewayURL = gatewayURLFor(false, port, 0)
		go func() { _ = s.gatewayHTTPServer.Serve(gwLn) }() // returns ErrServerClosed on Stop
		slog.Info("git gateway started", "addr", gwLn.Addr().String(), "sandbox_url", s.gatewayURL)

		// git gateway TCP(mTLS) listener: additive alongside the
		// plaintext loopback listener above (§PR4/§決定5).
		//
		// composeGatewayServiceName is included as a SAN for the same
		// reason as the broker's above (codex review [Major 2] on PR4):
		// it matches SandboxURL's own BackendContainer default
		// ("boid-gateway", see sandbox_url.go) so a container-backend
		// caller that leaves SandboxURLOptions.ServiceName unset gets a URL
		// whose hostname this cert already covers.
		//
		// [Blocker 2, PR7 codex review]: the listener bind address is now
		// gatewayBindHost(s.usingContainerBackend) — "0.0.0.0" instead of
		// loopback-only "127.0.0.1" when the container backend is
		// selected, since a sibling job container on the shared compose
		// network cannot reach a listener bound to THIS container's own
		// loopback interface. See composeBindHost's own doc comment for
		// why this is safe (the compose network boundary, not the bind
		// address, is what scopes reachability). userns deployments are
		// byte-for-byte unaffected: gatewayBindHost(false) returns
		// "127.0.0.1", identical to the pre-fix hardcoded literal.
		if daemonCA != nil && s.gatewayHandler != nil {
			bindHost := gatewayBindHost(s.usingContainerBackend)
			// ServerOnlyTLSConfig (PR9 e2e-container fix), not
			// ServerTLSConfig: the git gateway's own per-job Registry
			// token (URL-path-embedded, verified by gatewayHandler's
			// ServeHTTP) already fully authorizes every request — a
			// required client certificate would add no per-job
			// authorization on top of that, and no PR ever wired
			// per-job client cert issuance/delivery for this listener in
			// the first place (see ServerOnlyTLSConfig's own doc
			// comment). Requiring one anyway made the listener unusable
			// by any real sandbox: PR9's real-docker e2e-container CI
			// job's every sandbox-internal clone attempt failed the TLS
			// handshake outright ("tls: client didn't provide a
			// certificate").
			tlsCfg, err := daemonCA.ServerOnlyTLSConfig("127.0.0.1", "localhost", composeGatewayServiceName)
			if err != nil {
				return fmt.Errorf("git gateway tls config: %w", err)
			}
			gwTLSLn, err := s.gatewayHandler.ListenTLS(bindHost+":0", tlsCfg)
			if err != nil {
				return fmt.Errorf("listen git gateway tls: %w", err)
			}
			s.gatewayTLSLn = gwTLSLn
			slog.Info("git gateway tls listener started", "addr", gwTLSLn.Addr().String())

			// [Blocker 2, PR7 codex review]: once the container backend is
			// selected AND the TLS listener is actually up, gatewayURL is
			// replaced with the compose-service-DNS + mTLS URL — the ONLY
			// address a sibling job container can reach this daemon at
			// (BackendUserns' http://10.0.2.2 loopback projection does not
			// exist between sibling docker containers, per this PR's own
			// Blocker 2 evidence: `10.0.2.2` is a pasta/slirp userns
			// artifact with no docker equivalent). Falls through to the
			// plaintext BackendUserns URL set above when
			// usingContainerBackend is false (every pre-PR7 deployment) —
			// gatewayURLFor's own default branch.
			if s.usingContainerBackend {
				tlsPort := gwTLSLn.Addr().(*net.TCPAddr).Port
				s.gatewayURL = gatewayURLFor(true, port, tlsPort)
				slog.Info("git gateway sandbox url (container backend)", "sandbox_url", s.gatewayURL)
			}
		} else if s.usingContainerBackend {
			// The container backend REQUIRES the mTLS listener (it never
			// reaches the plaintext loopback listener at all — see above);
			// without cfg.TLSDir configured (daemonCA nil) or gatewayHandler
			// unset, gatewayURL is left at the useless plaintext-loopback
			// value computed above, which no job container could ever
			// dial. Surfaced loudly so an operator running
			// sandbox.backend: container without cfg.TLSDir set (cmd/
			// start.go's default IS to set it, but any custom wiring that
			// doesn't would land here) finds out from the log rather than
			// from every job's git clone timing out.
			slog.Error("sandbox.backend: container is selected but no TLS CA is configured (cfg.TLSDir unset) or the git gateway handler is missing; job containers will not be able to reach the git gateway at all (docs/plans/phase6-container-backend.md §決定5)")
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
	if s.gatewayHTTPServer != nil {
		if err := s.gatewayHTTPServer.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.gatewayTLSLn != nil {
		// gatewayHandler.CloseTLS (rather than s.gatewayTLSLn.Close()) also
		// closes idle keep-alive connections already accepted on the TLS
		// listener — a bare listener Close only stops new connections
		// from being accepted (codex review [Minor 4] on
		// docs/plans/phase6-container-backend.md §PR4). gatewayHandler is
		// always non-nil here: gatewayTLSLn is only ever set inside the
		// `s.gatewayHandler != nil` guard in Start.
		ctx, cancel := context.WithTimeout(context.Background(), gatewayTLSShutdownTimeout)
		if err := s.gatewayHandler.CloseTLS(ctx); err != nil {
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

// BrokerTLSAddr returns the broker's TCP(mTLS) listener address
// (docs/plans/phase6-container-backend.md §PR4), or "" when cfg.TLSDir was
// unset (no TLS listener bound) or before Start has bound it.
func (s *Server) BrokerTLSAddr() string {
	if s.broker != nil {
		return s.broker.TLSListenAddr()
	}
	return ""
}

// GatewayTLSAddr returns the git gateway's TCP(mTLS) listener address
// (docs/plans/phase6-container-backend.md §PR4), or "" when cfg.TLSDir was
// unset (no TLS listener bound) or before Start has bound it.
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

// GatewayURL returns the git gateway's sandbox-facing base URL
// (http://10.0.2.2:<port>), or "" before Start has bound its listener.
// docs/plans/git-gateway-cutover.md PR4 wires this into
// SandboxRuntimeInfo.GatewayURL (via dispatcher.WireConfig.GatewayURL), but
// nothing consumes it yet — the sandbox env var advertise and the runner
// clone sequence are PR6/PR5.
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
