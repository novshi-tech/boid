package server

import (
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/dispatcher"
	dtmux "github.com/novshi-tech/boid/internal/dispatcher/tmux"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/web"
)

type appRuntime struct {
	projectRepo *orchestrator.ProjectRepository
	taskRepo    *orchestrator.TaskRepository
	jobRepo     *dispatcher.JobRepository
	workflow    *api.TaskWorkflowService
}

func buildProjectStore(cfg Config, projectRepo *orchestrator.ProjectRepository) (*orchestrator.ProjectStore, error) {
	var registry *orchestrator.KitRegistry
	if cfg.KitsDir != "" {
		registry = orchestrator.NewRegistry(cfg.KitsDir)
	}
	store := orchestrator.NewProjectStore(registry)

	projects, err := projectRepo.ListProjects()
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	if errs := store.LoadAll(projects); len(errs) > 0 {
		for _, e := range errs {
			slog.Warn("failed to load project meta", "error", e)
		}
	}
	return store, nil
}

func newTmuxManager(cfg Config) dtmux.TmuxManager {
	if cfg.Tmux != nil {
		return cfg.Tmux
	}
	return &dtmux.RealTmux{}
}

func newTmuxSession(cfg Config) string {
	if cfg.TmuxSession != "" {
		return cfg.TmuxSession
	}
	return "boid"
}

func buildRuntime(srv *Server, cfg Config, store *orchestrator.ProjectStore, broker *sandbox.Broker, secretStore *dispatcher.SecretStore) (*appRuntime, error) {
	projectRepo := orchestrator.NewProjectRepository(srv.db)
	taskRepo := orchestrator.NewTaskRepository(srv.db)
	jobRepo := dispatcher.NewJobRepository(srv.db)
	tx := apiTransactor{db: srv.db}

	wtRootDir := filepath.Join(filepath.Dir(cfg.DBPath), "worktrees")
	if err := os.MkdirAll(wtRootDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir worktrees: %w", err)
	}
	wtMgr := &dispatcher.WorktreeManager{RootDir: wtRootDir, DB: srv.db}

	runner := dispatcher.Wire(dispatcher.WireConfig{
		DB:          srv.db,
		Tmux:        newTmuxManager(cfg),
		TmuxSession: newTmuxSession(cfg),
		Broker:      broker,
		SecretStore: secretStore,
	})

	boidBin, _ := os.Executable()
	planner := orchestrator.WireDispatchPlanner(orchestrator.PlannerWireConfig{
		Meta:         store,
		Projects:     orchestrator.DBProjectCatalog{DB: srv.db},
		Tasks:        orchestrator.DBTaskLookup{DB: srv.db},
		Worktrees:    worktreePreparer{manager: wtMgr},
		BoidBinary:   boidBin,
		ServerSocket: cfg.SocketPath,
		ProxyPort:    &srv.proxyPort,
	})
	adapter := orchestrator.NewDispatchAdapter(runner, planner)
	workflow := &api.TaskWorkflowService{
		Tasks:       taskRepo,
		Jobs:        jobRepo,
		Projects:    projectRepo,
		Tx:          tx,
		Meta:        store,
		Resolver:    orchestrator.NewDefaultRegistry(),
		Coordinator: &orchestrator.Coordinator{Evaluator: &orchestrator.Evaluator{}, HookExecutor: adapter, GateExecutor: adapter, Waiter: adapter, MaxDepth: 5},
		Lifecycle:   runner,
		Worktrees:   wtMgr,
	}

	return &appRuntime{
		projectRepo: projectRepo,
		taskRepo:    taskRepo,
		jobRepo:     jobRepo,
		workflow:    workflow,
	}, nil
}

func mountRoutes(srv *Server, runtime *appRuntime) error {
	r := srv.router

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

	if srv.secretStore != nil {
		secretHandler := &api.SecretHandler{Store: srv.secretStore}
		r.Mount("/api/secrets", secretHandler.Routes())
	}

	projectHandler := &api.ProjectHandler{Projects: runtime.projectRepo, Store: srv.store}
	r.Mount("/api/projects", projectHandler.Routes())

	taskHandler := &api.TaskHandler{Tasks: runtime.taskRepo}
	r.Mount("/api/tasks", taskHandler.Routes())

	actionHandler := &api.ActionHandler{Service: runtime.workflow}
	r.Route("/api/tasks/{taskID}/actions", func(r chi.Router) {
		r.Mount("/", actionHandler.Routes())
	})

	jobHandler := &api.JobHandler{Jobs: runtime.jobRepo, Service: runtime.workflow}
	r.Mount("/api/jobs", jobHandler.Routes())

	webHandler := &api.WebHandler{Tasks: runtime.taskRepo, Actions: runtime.taskRepo, Jobs: runtime.jobRepo, Projects: runtime.projectRepo, Store: srv.store}
	r.Mount("/", webHandler.Routes())

	staticFS, err := fs.Sub(web.StaticFS, "static")
	if err != nil {
		return fmt.Errorf("sub static fs: %w", err)
	}
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	return nil
}
