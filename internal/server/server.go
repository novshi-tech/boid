package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/dispatcher"
	dtmux "github.com/novshi-tech/boid/internal/dispatcher/tmux"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/web"
)

type Config struct {
	DBPath         string
	SocketPath     string
	HTTPAddr       string
	TmuxSession    string
	KitsDir        string            // base dir for installed kit repos
	KeyFilePath    string            // path to secret encryption key file
	AllowedDomains []string          // proxy allowed domains
	Tmux           dtmux.TmuxManager // nil uses RealTmux
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
	mu          sync.Mutex
}

func New(cfg Config) (*Server, error) {
	d, err := db.Open(cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := d.Migrate(); err != nil {
		d.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	conn := d.Conn

	var registry *orchestrator.KitRegistry
	if cfg.KitsDir != "" {
		registry = orchestrator.NewRegistry(cfg.KitsDir)
	}
	store := orchestrator.NewProjectStore(registry)

	// Load meta for all registered projects
	projects, err := orchestrator.ListProjects(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("list projects: %w", err)
	}
	if errs := store.LoadAll(projects); len(errs) > 0 {
		for _, e := range errs {
			slog.Warn("failed to load project meta", "error", e)
		}
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

	r := chi.NewRouter()

	srv := &Server{
		cfg:         cfg,
		db:          conn,
		store:       store,
		broker:      broker,
		secretStore: secretStore,
		proxy:       sandbox.WireProxy(cfg.AllowedDomains),
		router:      r,
		httpServer: &http.Server{
			Handler: r,
		},
	}

	r.Get("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	r.Get("/api/proxy", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"port":%d}`, srv.proxyPort)
	})

	r.Get("/api/broker", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"socket":%q}`, srv.BrokerSocket())
	})

	r.Post("/api/broker/register", srv.handleBrokerRegister)

	// Secrets API
	if secretStore != nil {
		secretHandler := &api.SecretHandler{Store: secretStore}
		r.Mount("/api/secrets", secretHandler.Routes())
	}

	projectRepo := orchestrator.NewProjectRepository(conn)
	taskRepo := orchestrator.NewTaskRepository(conn)
	jobRepo := dispatcher.NewJobRepository(conn)
	tx := apiTransactor{db: conn}

	projectHandler := &api.ProjectHandler{Projects: projectRepo, Store: store}
	r.Mount("/api/projects", projectHandler.Routes())

	taskHandler := &api.TaskHandler{Tasks: taskRepo}
	r.Mount("/api/tasks", taskHandler.Routes())

	reg := orchestrator.NewDefaultRegistry()
	eval := &orchestrator.Evaluator{}

	// Build job runner and dispatcher
	tmuxMgr := cfg.Tmux
	if tmuxMgr == nil {
		tmuxMgr = &dtmux.RealTmux{}
	}
	tmuxSession := cfg.TmuxSession
	if tmuxSession == "" {
		tmuxSession = "boid"
	}
	boidBin, _ := os.Executable()

	// Worktree manager
	wtRootDir := filepath.Join(filepath.Dir(cfg.DBPath), "worktrees")
	os.MkdirAll(wtRootDir, 0o755)
	wtMgr := &dispatcher.WorktreeManager{RootDir: wtRootDir, DB: conn}

	runner := dispatcher.Wire(dispatcher.WireConfig{
		DB:          conn,
		Tmux:        tmuxMgr,
		TmuxSession: tmuxSession,
		Broker:      broker,
		SecretStore: secretStore,
	})
	planner := orchestrator.WireDispatchPlanner(orchestrator.PlannerWireConfig{
		Meta:         store,
		Projects:     orchestrator.DBProjectCatalog{DB: conn},
		Tasks:        orchestrator.DBTaskLookup{DB: conn},
		Worktrees:    worktreePreparer{manager: wtMgr},
		BoidBinary:   boidBin,
		ServerSocket: cfg.SocketPath,
		ProxyPort:    &srv.proxyPort,
	})
	adapter := orchestrator.NewDispatchAdapter(runner, planner)
	coordinator := &orchestrator.Coordinator{
		Evaluator:    eval,
		HookExecutor: adapter,
		GateExecutor: adapter,
		Waiter:       adapter,
		MaxDepth:     5,
	}

	actionHandler := &api.ActionHandler{Tasks: taskRepo, Actions: taskRepo, Projects: projectRepo, Tx: tx, Store: store, Registry: reg, Evaluator: eval, Coordinator: coordinator, Runner: runner, WorktreeMgr: wtMgr}
	r.Route("/api/tasks/{taskID}/actions", func(r chi.Router) {
		r.Mount("/", actionHandler.Routes())
	})

	jobHandler := &api.JobHandler{Jobs: jobRepo, Tasks: taskRepo, Actions: taskRepo, Projects: projectRepo, Tx: tx, Store: store, Registry: reg, Evaluator: eval, Runner: runner, Coordinator: coordinator, WorktreeMgr: wtMgr}
	r.Mount("/api/jobs", jobHandler.Routes())

	// Web UI
	webHandler := &api.WebHandler{Tasks: taskRepo, Actions: taskRepo, Jobs: jobRepo, Projects: projectRepo, Store: store}
	r.Mount("/", webHandler.Routes())

	// Static files
	staticFS, _ := fs.Sub(web.StaticFS, "static")
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

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

// Broker returns the sandbox broker.
func (s *Server) Broker() *sandbox.Broker {
	return s.broker
}

// SecretStore returns the secret store.
func (s *Server) SecretStore() *dispatcher.SecretStore {
	return s.secretStore
}

// TCPAddr returns the TCP listener address.
func (s *Server) TCPAddr() string {
	if s.tcpLn != nil {
		return s.tcpLn.Addr().String()
	}
	return ""
}

type brokerRegisterRequest struct {
	Commands map[string]sandbox.CommandDef `json:"commands"`
}

type brokerRegisterResponse struct {
	Token  string `json:"token"`
	Socket string `json:"socket"`
}

func (s *Server) handleBrokerRegister(w http.ResponseWriter, r *http.Request) {
	if s.broker == nil {
		http.Error(w, `{"error":"broker not available"}`, http.StatusServiceUnavailable)
		return
	}

	var req brokerRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	if len(req.Commands) == 0 {
		http.Error(w, `{"error":"no commands"}`, http.StatusBadRequest)
		return
	}

	// boid exec uses gate role for maximum access
	ctx := sandbox.TokenContext{Role: "gate"}
	var token string
	if s.secretStore != nil {
		token = s.broker.RegisterWithSecrets(req.Commands, ctx, s.secretStore.Get)
	} else {
		token = s.broker.Register(req.Commands, ctx)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(brokerRegisterResponse{
		Token:  token,
		Socket: s.broker.SocketPath,
	})
}
