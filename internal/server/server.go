package server

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/gitgateway"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/skills"
)

type Config struct {
	DBPath         string
	SocketPath     string
	HTTPAddr       string
	KitsDir        string   // base dir for installed kit repos
	KeyFilePath    string   // path to secret encryption key file
	AllowedDomains []string // proxy allowed domains
	JobRuntime     dispatcher.JobRuntime
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
	unixLn      net.Listener
	tcpLn       net.Listener
	httpServer  *http.Server
	// tcpHandler wraps the router with transport-aware API auth and is served
	// to the TCP listener only. The UNIX socket is served the bare router
	// (trusted CLI/agent transport). Set by mountRoutes.
	tcpHandler http.Handler
	tcpServer  *http.Server
	gcLoop     *orchestrator.GCLoop // nil if GC is disabled
	workflow   *api.TaskWorkflowService

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
	// gatewayURL is the sandbox-facing base URL (http://10.0.2.2:<port>),
	// populated by Start() once the listener is bound. Empty before Start
	// completes. Runner holds a pointer to this string (WireConfig.GatewayURL)
	// so SandboxRuntimeInfo.GatewayURL reflects it at dispatch time — the
	// same late-binding-via-pointer trick as proxyPort.
	gatewayURL string

	mu sync.Mutex
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

	conn := d.Conn

	projectRepo := orchestrator.NewProjectRepository(conn)
	store, err := buildProjectStore(cfg, projectRepo)
	if err != nil {
		conn.Close()
		return nil, err
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
	}
	runtime, err := buildRuntime(srv, cfg, store, newCommandBroker(broker), secretStore)
	if err != nil {
		conn.Close()
		return nil, err
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

// Router returns the chi router for registering additional routes.
func (s *Server) Router() chi.Router {
	return s.router
}

func (s *Server) Start(ctx context.Context) error {
	// Start GC loop goroutine if configured.
	if s.gcLoop != nil {
		go s.gcLoop.Run(ctx)
	}

	// Start broker
	if s.broker != nil {
		if err := s.broker.Start(ctx); err != nil {
			return fmt.Errorf("start broker: %w", err)
		}
		slog.Info("broker started", "socket", s.broker.SocketPath)
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
	// dispatches. Bound on 127.0.0.1 like the egress proxy
	// (internal/sandbox/proxy.go) — sandboxes reach the host loopback via
	// the slirp-provided 10.0.2.2 alias, so the URL exposed to dispatch uses
	// that address instead of 127.0.0.1. Nothing inside the sandbox talks to
	// this yet (PR4 is inert — see docs/plans/git-gateway-cutover.md PR4);
	// the runner clone sequence (PR5) and env var advertise (PR6) are future
	// work.
	if s.gatewayHTTPServer != nil {
		gwLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return fmt.Errorf("listen git gateway: %w", err)
		}
		s.gatewayLn = gwLn
		port := gwLn.Addr().(*net.TCPAddr).Port
		s.gatewayURL = fmt.Sprintf("http://10.0.2.2:%d", port)
		go func() { _ = s.gatewayHTTPServer.Serve(gwLn) }() // returns ErrServerClosed on Stop
		slog.Info("git gateway started", "addr", gwLn.Addr().String(), "sandbox_url", s.gatewayURL)
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
