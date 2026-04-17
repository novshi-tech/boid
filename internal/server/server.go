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
	WebEnabled     bool // enable Web UI (default: false)
}

type Server struct {
	cfg         Config
	db          *sql.DB
	store       *orchestrator.ProjectStore
	broker      *sandbox.Broker
	secretStore *dispatcher.SecretStore
	proxy       *sandbox.Proxy
	proxyPort   int
	router      chi.Router
	unixLn      net.Listener
	tcpLn       net.Listener
	httpServer  *http.Server
	gcLoop      *orchestrator.GCLoop // nil if GC is disabled
	workflow    *api.TaskWorkflowService
	mu          sync.Mutex
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
		cfg:         cfg,
		db:          conn,
		store:       store,
		broker:      broker,
		secretStore: secretStore,
		proxy:       sandbox.WireProxy(cfg.AllowedDomains),
		router:      chi.NewRouter(),
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

	// Start proxy
	if s.proxy != nil {
		port, err := s.proxy.Start(ctx)
		if err != nil {
			return fmt.Errorf("start proxy: %w", err)
		}
		s.proxyPort = port
		slog.Info("proxy started", "port", port)
	}

	// Remove stale socket
	os.Remove(s.cfg.SocketPath)

	unixLn, err := net.Listen("unix", s.cfg.SocketPath)
	if err != nil {
		return fmt.Errorf("listen unix: %w", err)
	}
	s.unixLn = unixLn

	tcpLn, err := net.Listen("tcp", s.cfg.HTTPAddr)
	if err != nil {
		unixLn.Close()
		return fmt.Errorf("listen tcp: %w", err)
	}
	s.tcpLn = tcpLn

	go s.httpServer.Serve(unixLn)
	go s.httpServer.Serve(tcpLn)

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
	if s.proxy != nil {
		s.proxy.Stop()
	}
	if s.broker != nil {
		s.broker.Stop()
	}
	os.Remove(s.cfg.SocketPath)

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
