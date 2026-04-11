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
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/web"
)

type appRuntime struct {
	projectRepo    api.ProjectRepository
	taskRepo       *orchestrator.TaskRepository
	jobStore       api.JobStore
	globalJobStore api.GlobalJobStore
	jobRuntime     dispatcher.JobRuntime
	meta           api.MetaStore
	projectSvc     *api.ProjectAppService
	taskSvc        *api.TaskAppService
	webSvc         *api.WebAppService
	workflow       *api.TaskWorkflowService
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

func newJobRuntime(cfg Config) (dispatcher.JobRuntime, error) {
	if cfg.JobRuntime != nil {
		return cfg.JobRuntime, nil
	}

	rootDir := filepath.Join(filepath.Dir(cfg.SocketPath), "runtimes")
	if cfg.DBPath != "" && cfg.DBPath != ":memory:" {
		rootDir = filepath.Join(filepath.Dir(cfg.DBPath), "runtimes")
	}
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir runtime root: %w", err)
	}
	return &dispatcher.LocalRuntime{RootDir: rootDir}, nil
}

func buildRuntime(srv *Server, cfg Config, store *orchestrator.ProjectStore, broker dispatcher.CommandBroker, secretStore *dispatcher.SecretStore) (*appRuntime, error) {
	// Clean up jobs left in running state from a previous crash or restart.
	if err := dispatcher.MarkStaleJobsFailed(srv.db); err != nil {
		slog.Warn("failed to mark stale jobs as failed", "error", err)
	}

	projectRepo := orchestrator.NewProjectRepository(srv.db)
	taskRepo := orchestrator.NewTaskRepository(srv.db)
	jobRepo := dispatcher.NewJobRepository(srv.db)
	jobStore := jobStoreAdapter{repo: jobRepo}
	tx := apiTransactor{db: srv.db}

	wtRootDir := filepath.Join(filepath.Dir(cfg.DBPath), "worktrees")
	if err := os.MkdirAll(wtRootDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir worktrees: %w", err)
	}
	wtMgr := &dispatcher.WorktreeManager{RootDir: wtRootDir, DB: srv.db}

	if err := wtMgr.CleanOrphaned(func(taskID, projectID string) (string, string, error) {
		task, err := taskRepo.GetTask(taskID)
		if err != nil || task == nil {
			return "", "", err
		}
		proj, err := projectRepo.GetProject(projectID)
		if err != nil || proj == nil {
			return "", "", err
		}
		return string(task.Status), proj.WorkDir, nil
	}); err != nil {
		slog.Warn("orphan worktree cleanup failed", "error", err)
	}

	jobRuntime, err := newJobRuntime(cfg)
	if err != nil {
		return nil, err
	}

	runner := dispatcher.Wire(dispatcher.WireConfig{
		DB:          srv.db,
		Runtime:     jobRuntime,
		Broker:      broker,
		Sandbox:     sandbox.NewDispatcherPreparer(),
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
	adapter := dispatcher.NewOrchestratorAdapter(runner, planner)
	workflow := &api.TaskWorkflowService{
		Tasks:       taskRepo,
		Jobs:        jobStore,
		Projects:    projectRepo,
		Tx:          tx,
		Meta:        store,
		Resolver:    orchestrator.NewDefaultRegistry(),
		Coordinator: &orchestrator.Coordinator{Evaluator: &orchestrator.Evaluator{}, HookExecutor: adapter, GateExecutor: adapter, Waiter: adapter, MaxDepth: 5, Locker: orchestrator.NewInMemoryWorktreeLockManager()},
		Lifecycle:   jobLifecycleAdapter{runner: runner},
		Worktrees:   wtMgr,
	}
	projectSvc := &api.ProjectAppService{
		Projects: projectRepo,
		Meta:     store,
	}
	taskSvc := &api.TaskAppService{
		Tasks:    taskRepo,
		Actions:  taskRepo,
		Jobs:     jobStore,
		Meta:     store,
		Workflow: workflow,
	}
	if srv.broker != nil {
		srv.broker.BoidExecutor = newBoidBuiltinExecutor(workflow, taskSvc)
	}
	globalJobSvc := &globalJobStore{
		jobs:     jobRepo,
		tasks:    taskRepo,
		projects: projectRepo,
	}
	webSvc := &api.WebAppService{
		Tasks:      taskRepo,
		Actions:    taskRepo,
		Jobs:       jobStore,
		GlobalJobs: globalJobSvc,
		Projects:   projectRepo,
		Meta:       store,
		Workflow:   workflow,
	}

	return &appRuntime{
		projectRepo:    projectRepo,
		taskRepo:       taskRepo,
		jobStore:       jobStore,
		globalJobStore: globalJobSvc,
		jobRuntime:     jobRuntime,
		meta:           store,
		projectSvc:     projectSvc,
		taskSvc:        taskSvc,
		webSvc:         webSvc,
		workflow:       workflow,
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

	brokerHandler := &api.BrokerHandler{
		Registry: brokerRegistry{broker: newCommandBroker(srv.broker), projects: runtime.projectRepo, secretStore: srv.secretStore},
	}
	r.Mount("/api/broker", brokerHandler.Routes())

	if srv.secretStore != nil {
		secretHandler := &api.SecretHandler{Store: srv.secretStore}
		r.Mount("/api/secrets", secretHandler.Routes())
	}

	projectHandler := &api.ProjectHandler{Service: runtime.projectSvc}
	r.Mount("/api/projects", projectHandler.Routes())

	workspaceHandler := &api.WorkspaceHandler{Service: runtime.projectSvc}
	r.Mount("/api/workspaces", workspaceHandler.Routes())

	taskHandler := &api.TaskHandler{Service: runtime.taskSvc}
	r.Mount("/api/tasks", taskHandler.Routes())

	gcStore := orchestrator.NewTaskGCStoreWithWorktree(
		srv.db,
		func(projectID string) (string, error) {
			proj, err := orchestrator.GetProject(srv.db, projectID)
			if err != nil {
				return "", err
			}
			return proj.WorkDir, nil
		},
		"",
	)
	gcHandler := &api.GCHandler{Service: &api.GCAppService{Store: gcStore}}
	r.Mount("/api/gc", gcHandler.Routes())

	scriptHandler := &api.ScriptHandler{Service: &api.ScriptAppService{
		Meta:     runtime.meta,
		Tasks:    runtime.taskRepo,
		Workflow: runtime.workflow,
	}}
	r.Route("/api/projects/{id}/scripts", func(r chi.Router) {
		r.Mount("/", scriptHandler.Routes())
	})

	actionHandler := &api.ActionHandler{Service: runtime.workflow}
	r.Route("/api/tasks/{taskID}/actions", func(r chi.Router) {
		r.Mount("/", actionHandler.Routes())
	})

	jobHandler := &api.JobHandler{Jobs: runtime.jobStore, Global: runtime.globalJobStore, Service: runtime.workflow}
	r.Mount("/api/jobs", jobHandler.Routes())
	mountJobRuntimeRoutes(r, runtime)

	webHandler := &api.WebHandler{Service: runtime.webSvc}
	r.Mount("/", webHandler.Routes())

	staticFS, err := fs.Sub(web.StaticFS, "static")
	if err != nil {
		return fmt.Errorf("sub static fs: %w", err)
	}
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	return nil
}
