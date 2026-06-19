package server

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/adapters/claude"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/api/auth"
	"github.com/novshi-tech/boid/internal/config"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/notify"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/dockerproxy"
	"github.com/novshi-tech/boid/web"
)

type appRuntime struct {
	projectRepo    api.ProjectRepository
	taskRepo       *orchestrator.TaskRepository
	jobStore       api.JobStore
	globalJobStore api.GlobalJobStore
	jobRuntime     dispatcher.JobRuntime
	runner         *dispatcher.Runner
	meta           api.MetaStore
	projectSvc     *api.ProjectAppService
	taskSvc        *api.TaskAppService
	webSvc         *api.WebAppService
	workflow       *api.TaskWorkflowService
	hub            *api.TaskEventHub
	authStore      *auth.Store
	sessionSigner  *auth.SessionSigner
	connRegistry   *auth.ConnectionRegistry
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

// runtimesDirFor returns the runtimes root directory for the given config.
func runtimesDirFor(cfg Config) string {
	if cfg.DBPath != "" && cfg.DBPath != ":memory:" {
		return filepath.Join(filepath.Dir(cfg.DBPath), "runtimes")
	}
	return filepath.Join(filepath.Dir(cfg.SocketPath), "runtimes")
}

// webSecretPathFor returns the path for the web session signing key.
func webSecretPathFor(cfg Config) string {
	if cfg.DBPath != "" && cfg.DBPath != ":memory:" {
		return filepath.Join(filepath.Dir(cfg.DBPath), "web_secret")
	}
	if cfg.SocketPath != "" {
		return filepath.Join(filepath.Dir(cfg.SocketPath), "web_secret")
	}
	return ""
}

func newJobRuntime(cfg Config) (dispatcher.JobRuntime, error) {
	if cfg.JobRuntime != nil {
		return cfg.JobRuntime, nil
	}

	rootDir := runtimesDirFor(cfg)
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir runtime root: %w", err)
	}
	return &dispatcher.LocalRuntime{RootDir: rootDir}, nil
}

// cleanOrphanRuntimes removes runtime directories that have no corresponding
// job row in the database. Call this on startup before MarkStaleJobsFailed
// so that only truly orphaned dirs (no DB row) are removed.
func cleanOrphanRuntimes(runtimesDir string, conn *sql.DB) {
	entries, err := os.ReadDir(runtimesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		slog.Warn("cleanup orphan runtimes: read dir failed", "error", err)
		return
	}

	removed := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runtimeID := entry.Name()
		var count int
		if err := conn.QueryRow(`SELECT COUNT(*) FROM jobs WHERE runtime_id = ?`, runtimeID).Scan(&count); err != nil {
			slog.Warn("cleanup orphan runtimes: query failed", "runtime_id", runtimeID, "error", err)
			continue
		}
		if count == 0 {
			dir := filepath.Join(runtimesDir, runtimeID)
			if err := os.RemoveAll(dir); err != nil {
				slog.Warn("cleanup orphan runtimes: remove failed", "runtime_id", runtimeID, "error", err)
				continue
			}
			removed++
		}
	}
	if removed > 0 {
		slog.Info("cleaned up orphan runtime dirs", "count", removed)
	}
}

func buildRuntime(srv *Server, cfg Config, store *orchestrator.ProjectStore, broker dispatcher.CommandBroker, secretStore *dispatcher.SecretStore) (*appRuntime, error) {
	// Clean up runtime dirs that have no corresponding job rows (must run before
	// MarkStaleJobsFailed so we only remove truly orphaned dirs).
	cleanOrphanRuntimes(runtimesDirFor(cfg), srv.db)

	// Clean up jobs left in running state from a previous crash or restart.
	if err := dispatcher.MarkStaleJobsFailed(srv.db); err != nil {
		slog.Warn("failed to mark stale jobs as failed", "error", err)
	}

	// Abort tasks left in executing state from a previous crash or restart.
	if n, err := dispatcher.MarkStaleExecutingTasksAborted(srv.db); err != nil {
		slog.Warn("failed to abort stale executing tasks", "error", err)
	} else if n > 0 {
		slog.Info("aborted stale executing tasks on startup", "count", n)
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

	jobRuntime, err := newJobRuntime(cfg)
	if err != nil {
		return nil, err
	}

	boidBin, _ := os.Executable()
	projectCatalog := orchestrator.DBProjectCatalog{DB: srv.db}
	taskLookup := orchestrator.DBTaskLookup{DB: srv.db}
	runner := dispatcher.Wire(dispatcher.WireConfig{
		DB:           srv.db,
		Runtime:      jobRuntime,
		Broker:       broker,
		Sandbox:      dispatcher.NewSandboxPreparer(),
		SecretStore:  secretStore,
		Worktrees:    wtMgr,
		TaskLookup:   taskLookup,
		Projects:     projectCatalog,
		BoidBinary:   boidBin,
		ServerSocket: cfg.SocketPath,
		ProxyPort:    &srv.proxyPort,
		RuntimesDir:  runtimesDirFor(cfg),
	})

	lifecycle := jobLifecycleAdapter{runner: runner}
	claudeAdapter := claude.New()
	planner := orchestrator.WireDispatchPlanner(orchestrator.PlannerWireConfig{
		Meta:     store,
		Projects: projectCatalog,
		Tasks:    taskLookup,
		Adapter:  claudeAdapter,
	})
	adapter := dispatcher.NewOrchestratorAdapter(runner, planner)
	hub := api.NewTaskEventHub()
	// Wire the runner's job-event sink to the web SSE hub so job creations
	// surface in task timelines without polling. Completion broadcasts live
	// in TaskWorkflowService.CompleteJob (where exit-code semantics are known).
	runner.JobEvents = hubJobEventSink{hub: hub}
	// Branch lock — held by the workflow service for the full executing
	// lifetime of each task. Root tasks on the same base_branch serialize;
	// child tasks (boid/<id8>) always run in parallel.
	projectLocks := orchestrator.NewBranchLockManager(orchestrator.NewInMemoryWorktreeLockManager())
	workflow := &api.TaskWorkflowService{
		Tasks:       taskRepo,
		Jobs:        jobStore,
		Projects:    projectRepo,
		Tx:          tx,
		Meta:        store,
		Coordinator: &orchestrator.Coordinator{Evaluator: &orchestrator.Evaluator{}, HookExecutor: adapter, Waiter: adapter, MaxDepth: 5, LifecycleStore: taskRepo},
		Lifecycle:   lifecycle,
		Worktrees:   wtMgr,
		Hub:         hub,
		Locks:       projectLocks,
		Adapter:     claudeAdapter,
	}
	workflow.InitDispatch(context.Background())

	// Auto-reopen tasks that were interrupted by the previous daemon shutdown.
	// These tasks were aborted with code=daemon_shutdown either by
	// abortOnDispatchError (hook in flight when SIGTERM fired) or by
	// MarkStaleExecutingTasksAborted above (executing-state remnants from a
	// crash that bypassed the dispatch loop). Both paths set the same code
	// so a single startup query covers them.
	if shutdownIDs, err := dispatcher.FindDaemonShutdownAbortedTasks(srv.db); err != nil {
		slog.Warn("failed to query daemon_shutdown aborted tasks", "error", err)
	} else {
		for _, id := range shutdownIDs {
			if _, err := workflow.ApplyAction(context.Background(), id, api.ApplyActionRequest{Type: "reopen"}); err != nil {
				slog.Warn("auto-reopen on startup failed", "task_id", id, "error", err)
				continue
			}
			slog.Info("auto-reopened task interrupted by daemon shutdown", "task_id", id)
		}
	}
	projectSvc := &api.ProjectAppService{
		Projects: projectRepo,
		Meta:     store,
	}
	boidCfg, err := config.Load()
	if err != nil {
		slog.Warn("failed to load boid config, using defaults", "error", err)
		boidCfg = config.DefaultConfig()
	}
	notifySvc := &notify.Service{
		Command:   boidCfg.Notify.Command,
		PublicURL: boidCfg.Web.PublicURL,
	}

	taskSvc := &api.TaskAppService{
		Tasks:       taskRepo,
		Actions:     taskRepo,
		Jobs:        jobStore,
		Meta:        store,
		Workflow:    workflow,
		Projects:    projectRepo,
		RuntimesDir: runtimesDirFor(cfg),
		Notify:      notifySvc,
	}
	if srv.broker != nil {
		srv.broker.BoidExecutor = newBoidBuiltinExecutor(workflow, taskSvc, jobStore, transcriptLogReader{rootDir: runtimesDirFor(cfg)})
		srv.broker.ProjectResolver = projectResolverFor(projectSvc)
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
		TaskSvc:    taskSvc,
		Hooks:      workflow,
		Answerer:   taskSvc,
	}

	authStore := auth.NewStore(srv.db)
	var sessionSigner *auth.SessionSigner
	if webSecretPath := webSecretPathFor(cfg); webSecretPath != "" {
		webSecret, err := dispatcher.LoadOrCreateKey(webSecretPath)
		if err != nil {
			return nil, fmt.Errorf("load web secret: %w", err)
		}
		sessionSigner = auth.NewSessionSigner(webSecret, authStore)
	}

	connRegistry := auth.NewConnectionRegistry()

	return &appRuntime{
		projectRepo:    projectRepo,
		taskRepo:       taskRepo,
		jobStore:       jobStore,
		globalJobStore: globalJobSvc,
		jobRuntime:     jobRuntime,
		runner:         runner,
		meta:           store,
		projectSvc:     projectSvc,
		taskSvc:        taskSvc,
		webSvc:         webSvc,
		workflow:       workflow,
		hub:            hub,
		authStore:      authStore,
		sessionSigner:  sessionSigner,
		connRegistry:   connRegistry,
	}, nil
}

// makeDockerRuntimeReaper returns a GC runtime-reaper function that checks each
// runtime directory for a docker-resources.jsonl ledger and, when found, calls
// dockerproxy.Reap to clean up any Docker resources that weren't cleaned up when
// the sandbox exited (safety net for daemon-restart scenarios).
func makeDockerRuntimeReaper() func(runtimeDir string) error {
	return func(runtimeDir string) error {
		ledgerPath := filepath.Join(runtimeDir, "docker-resources.jsonl")
		if _, err := os.Stat(ledgerPath); err != nil {
			if os.IsNotExist(err) {
				return nil // no ledger → no docker resources to reap
			}
			return err
		}
		upstream, err := dockerproxy.ResolveUpstream("")
		if err != nil {
			// No docker socket available: log at debug and skip. This is
			// expected when the machine has no docker daemon.
			slog.Debug("docker gc reap: no upstream socket, skipping", "runtime_dir", runtimeDir, "err", err)
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		ledger := dockerproxy.NewLedger(ledgerPath)
		if err := dockerproxy.Reap(ctx, upstream, ledger); err != nil {
			slog.Warn("docker gc reap failed", "runtime_dir", runtimeDir, "error", err)
			// Non-fatal: let GC continue to remove the directory.
		}
		return nil
	}
}

// projectResolverFor adapts ProjectAppService.ResolveProjectRef into the
// sandbox.ProjectResolver contract: a single UUID or a hard error. Unlike the
// HTTP-facing caller (cmd/project_ref.go), sandbox callers have no TTY, so
// ambiguous matches fail instead of prompting.
func projectResolverFor(svc *api.ProjectAppService) sandbox.ProjectResolver {
	return func(ref string) (string, error) {
		projects, err := svc.ResolveProjectRef(ref)
		if err != nil {
			return "", err
		}
		if len(projects) > 1 {
			return "", fmt.Errorf("ambiguous project ref %q (%d matches)", ref, len(projects))
		}
		return projects[0].ID, nil
	}
}

// jobDispatcher abstracts the Dispatch method of *dispatcher.Runner for testability.
type jobDispatcher interface {
	Dispatch(ctx context.Context, spec *orchestrator.JobSpec, cleanup orchestrator.CleanupFunc) (string, error)
}

// sessionDispatcherAdapter implements api.SessionDispatcher by translating
// StartSessionRequest into a SessionJobInput and handing it to the runner.
// Phase 3-d (PR1) wired this in alongside the legacy ExecuteCommand path so
// the new entry can coexist with the existing Commands buttons until PR2
// removes them.
type sessionDispatcherAdapter struct {
	service *api.ProjectAppService
	runner  *dispatcher.Runner
}

func (a *sessionDispatcherAdapter) StartSession(ctx context.Context, req api.StartSessionRequest) (*api.StartSessionResult, error) {
	project, err := a.service.GetProject(req.ProjectID)
	if err != nil {
		return nil, err
	}
	meta := project.Meta
	// shell sessions get a hard-coded interactive bash. Agent harnesses
	// (claude / codex / opencode) build their own argv from CLI conventions
	// and ignore SessionJobInput.Argv entirely.
	var argv []string
	if req.HarnessType == "shell" {
		argv = []string{"/bin/bash"}
	}
	spec := dispatcher.BuildSessionJobSpec(dispatcher.SessionJobInput{
		ProjectID:          project.ID,
		ProjectWorkDir:     project.WorkDir,
		HarnessType:        req.HarnessType,
		Argv:               argv,
		SessionID:          req.SessionID,
		Instruction:        req.Instruction,
		Readonly:           req.Readonly,
		Model:              req.Model,
		DisplayName:        req.DisplayName,
		Env:                meta.Env,
		HostCommands:       meta.HostCommands,
		AdditionalBindings: meta.AdditionalBindings,
		SecretNamespace:    meta.SecretNamespace,
		DockerEnabled:      meta.Capabilities.Docker != nil,
	})
	jobID, err := a.runner.Dispatch(ctx, spec, nil)
	if err != nil {
		return nil, &api.StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	return &api.StartSessionResult{
		JobID:     jobID,
		AttachURL: fmt.Sprintf("/jobs/%s", jobID),
	}, nil
}

func mountRoutes(srv *Server, runtime *appRuntime) error {
	r := srv.router

	// CSRF middleware must be registered before any routes (chi requirement).
	// The middleware exempts /api/* and /auth paths, so existing API routes are unaffected.
	r.Use(auth.CSRFMiddleware)

	r.Get("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if addr := srv.TCPAddr(); addr != "" {
			fmt.Fprintf(w, `{"status":"ok","http_addr":%q}`, addr)
		} else {
			w.Write([]byte(`{"status":"ok"}`))
		}
	})

	r.Post("/api/shutdown", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		go func() {
			// レスポンスがクライアントに届く前にプロセスが死なないよう少し待つ。
			time.Sleep(50 * time.Millisecond)
			// 自プロセスに SIGTERM を送り、daemon child の signal handler
			// (runDaemonChild) に srv.Stop() とプロセス終了を任せる。ここで
			// srv.Stop() を直接呼ぶとプロセス本体が終了せず、次回 boid start が
			// 生存中の socket/listen を検知できなくなる。
			if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
				slog.Error("shutdown: send SIGTERM", "error", err)
			}
		}()
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

	sessionAdapter := &sessionDispatcherAdapter{service: runtime.projectSvc, runner: runtime.runner}
	projectHandler := &api.ProjectHandler{
		Service:           runtime.projectSvc,
		SessionDispatcher: sessionAdapter,
	}
	r.Mount("/api/projects", projectHandler.Routes())

	sessionHandler := &api.SessionHandler{
		Service:    runtime.projectSvc,
		Dispatcher: sessionAdapter,
	}
	r.Mount("/api/sessions", sessionHandler.Routes())

	workspaceHandler := &api.WorkspaceHandler{Service: runtime.projectSvc}
	r.Mount("/api/workspaces", workspaceHandler.Routes())

	taskHandler := &api.TaskHandler{Service: runtime.taskSvc, Hooks: runtime.workflow, Notifier: runtime.taskSvc, Answerer: runtime.taskSvc}
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
		runtimesDirFor(srv.cfg),
	).WithSandboxTmpDir(os.TempDir()).WithRuntimeReaper(makeDockerRuntimeReaper())
	gcAppService := &api.GCAppService{Store: gcStore, DeviceStore: runtime.authStore}
	gcHandler := &api.GCHandler{Service: gcAppService}
	r.Mount("/api/gc", gcHandler.Routes())

	// Wire up the periodic GC loop.
	gcCfg, err := config.Load()
	if err != nil {
		slog.Warn("failed to load boid config, using defaults", "error", err)
		gcCfg = config.DefaultConfig()
	}
	if gcCfg.GC.Enabled {
		srv.gcLoop = &orchestrator.GCLoop{
			Store:        gcAppService,
			Interval:     gcCfg.GC.Interval,
			OlderThan:    gcCfg.GC.OlderThan,
			InitialDelay: 10 * time.Second,
		}
	}

	actionHandler := &api.ActionHandler{Service: runtime.workflow}
	r.Route("/api/tasks/{taskID}/actions", func(r chi.Router) {
		r.Mount("/", actionHandler.Routes())
	})

	jobHandler := &api.JobHandler{
		Jobs:      runtime.jobStore,
		Global:    runtime.globalJobStore,
		Service:   runtime.workflow,
		LogReader: transcriptLogReader{rootDir: runtimesDirFor(srv.cfg)},
		SSEHandler: &api.JobLogSSEHandler{
			Subscriber: runtime.runner,
			Registry:   runtime.connRegistry,
		},
	}
	r.Mount("/api/jobs", jobHandler.Routes())
	mountJobRuntimeRoutes(r, runtime)

	staticFS, err := fs.Sub(web.StaticFS, "static")
	if err != nil {
		return fmt.Errorf("sub static fs: %w", err)
	}

	// Static files are served unauthenticated.
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	// Management API — accessible via UNIX socket (CLI only), no session auth.
	webMgmt := &api.WebManagementHandler{
		Pairing:  auth.NewPairingManager(runtime.authStore),
		Store:    runtime.authStore,
		PublicURL: gcCfg.Web.PublicURL,
		Registry: runtime.connRegistry,
	}
	r.Mount("/api/web", webMgmt.Routes())

	// Login/auth routes (exempted by WebAuthMiddleware and CSRFMiddleware).
	loginHandler := &api.LoginHandler{
		Pairing: auth.NewPairingManager(runtime.authStore),
		Store:   runtime.authStore,
		Limiter: auth.NewRateLimiter(nil),
	}
	if runtime.sessionSigner != nil {
		loginHandler.Signer = runtime.sessionSigner
	}
	r.Get("/login", loginHandler.GetLogin)
	r.Post("/login", loginHandler.PostLogin)
	r.Get("/auth", loginHandler.GetAuth)

	// Web UI routes protected by session auth.
	r.Group(func(r chi.Router) {
		r.Use(auth.NewWebAuthMiddleware(runtime.sessionSigner, runtime.authStore))
		webHandler := &api.WebHandler{
			Service:           runtime.webSvc,
			Hub:               runtime.hub,
			SessionDispatcher: sessionAdapter,
			Registry:          runtime.connRegistry,
		}
		r.Get("/api/tasks/{id}/events", webHandler.TaskEvents)
		r.Get("/api/jobs/{id}/attach/ws", (&api.WSAttachHandler{
			Subscriber: runtime.runner,
			Writer:     runtime.runner,
			PublicURL:  gcCfg.Web.PublicURL,
			Registry:   runtime.connRegistry,
		}).ServeHTTP)
		r.Mount("/", webHandler.Routes())
	})

	// The router above is served as-is to the UNIX socket (trusted CLI/agent
	// transport). The TCP listener — which may be exposed directly, via a
	// tunnel, or to other local users on the shared loopback — is served the
	// same router wrapped with transport-aware API auth, so the data/control
	// /api/* surface requires a session over TCP. See Server.Start.
	srv.tcpHandler = auth.NewTCPAPIAuthMiddleware(runtime.sessionSigner, runtime.authStore)(r)
	return nil
}
