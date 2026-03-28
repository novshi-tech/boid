package server

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/hook"
	"github.com/novshi-tech/boid/internal/job"
	"github.com/novshi-tech/boid/internal/project"
	"github.com/novshi-tech/boid/internal/reducer"
	"github.com/novshi-tech/boid/internal/tmux"
	"github.com/novshi-tech/boid/web"
)

type Config struct {
	DBPath      string
	SocketPath  string
	HTTPAddr    string
	TmuxSession string
	Tmux        tmux.TmuxManager // nil uses RealTmux
}

type Server struct {
	cfg        Config
	db         *db.DB
	store      *project.Store
	router     chi.Router
	unixLn     net.Listener
	tcpLn      net.Listener
	httpServer *http.Server
	mu         sync.Mutex
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

	store := project.NewStore()

	// Load meta for all registered projects
	projects, err := d.ListProjects()
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("list projects: %w", err)
	}
	if errs := store.LoadAll(projects); len(errs) > 0 {
		for _, e := range errs {
			slog.Warn("failed to load project meta", "error", e)
		}
	}

	r := chi.NewRouter()
	r.Get("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	projectHandler := &api.ProjectHandler{DB: d, Store: store}
	r.Mount("/api/projects", projectHandler.Routes())

	taskHandler := &api.TaskHandler{DB: d}
	r.Mount("/api/tasks", taskHandler.Routes())

	reg := reducer.NewDefaultRegistry()
	eval := &hook.Evaluator{}

	// Build job runner and dispatcher
	tmuxMgr := cfg.Tmux
	if tmuxMgr == nil {
		tmuxMgr = &tmux.RealTmux{}
	}
	tmuxSession := cfg.TmuxSession
	if tmuxSession == "" {
		tmuxSession = "boid"
	}
	boidBin, _ := os.Executable()
	runner := &job.Runner{
		DB:           d,
		Store:        store,
		Tmux:         tmuxMgr,
		TmuxSession:  tmuxSession,
		BoidBinary:   boidBin,
		ServerSocket: cfg.SocketPath,
	}
	dispatcher := &hook.Dispatcher{Runner: runner, MaxDepth: 3}

	actionHandler := &api.ActionHandler{DB: d, Store: store, Registry: reg, Evaluator: eval, Dispatcher: dispatcher}
	r.Route("/api/tasks/{taskID}/actions", func(r chi.Router) {
		r.Mount("/", actionHandler.Routes())
	})

	jobHandler := &api.JobHandler{DB: d, Store: store, Registry: reg, Evaluator: eval}
	r.Mount("/api/jobs", jobHandler.Routes())

	// Web UI
	webHandler := &api.WebHandler{DB: d, Store: store}
	r.Mount("/", webHandler.Routes())

	// Static files
	staticFS, _ := fs.Sub(web.StaticFS, "static")
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	return &Server{
		cfg:    cfg,
		db:     d,
		store:  store,
		router: r,
		httpServer: &http.Server{
			Handler: r,
		},
	}, nil
}

// DB returns the database instance.
func (s *Server) DB() *db.DB {
	return s.db
}

// Store returns the project store.
func (s *Server) Store() *project.Store {
	return s.store
}

// Router returns the chi router for registering additional routes.
func (s *Server) Router() chi.Router {
	return s.router
}

func (s *Server) Start(ctx context.Context) error {
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
	os.Remove(s.cfg.SocketPath)

	if len(errs) > 0 {
		return fmt.Errorf("stop errors: %v", errs)
	}
	return nil
}

// SocketPath returns the UNIX socket path.
func (s *Server) SocketPath() string {
	return s.cfg.SocketPath
}

// TCPAddr returns the TCP listener address.
func (s *Server) TCPAddr() string {
	if s.tcpLn != nil {
		return s.tcpLn.Addr().String()
	}
	return ""
}
