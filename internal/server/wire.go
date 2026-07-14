package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/adapters/claude"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/api/auth"
	"github.com/novshi-tech/boid/internal/config"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/gitgateway"
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
	// resolver は KitResolver 型 (interface) を使い、 cfg.KitsDir 未設定時は
	// untyped nil interface を渡す。 *KitRegistry 型のローカル変数を経由すると
	// Go の typed-nil 罠で interface 値が non-nil (内部 type=*KitRegistry,
	// value=nil) となり、 spec_loader の `resolver == nil` check をすり抜けて
	// resolveKitRef → KitRegistry.Resolve で nil pointer dereference する。
	// testutil の Server fixture は KitsDir を渡さないので、 internal/api /
	// internal/server / cmd の rerun・hook・detail 系 test が main で軒並み
	// panic していた真因。
	var resolver orchestrator.KitResolver
	if cfg.KitsDir != "" {
		resolver = orchestrator.NewRegistry(cfg.KitsDir)
	}
	store := orchestrator.NewProjectStore(resolver)

	// Wire workspace store so GetWithWorkspace can hydrate workspace.yaml data
	// (capabilities, kits, env) at dispatch time.
	wsStore := orchestrator.NewWorkspaceStore("")
	store.SetWorkspaceStore(wsStore)

	// Ensure the implicit default workspace exists on disk before the
	// validation pass below scans the directory. The file is created empty;
	// users can later edit it via `boid workspace configure default`. We
	// log but do not block daemon startup on EnsureDefault failure since
	// the next Load attempt will surface the problem with a sharper error.
	if err := wsStore.EnsureDefault(); err != nil {
		slog.Warn("EnsureDefault failed (default workspace may be missing)",
			"error", err)
	}

	// Validate all workspace.yaml files at startup. ErrNotExist means no
	// workspace directory yet — that is the degraded window and is fine.
	// Any other error (parse failure, permission error) is a startup blocker.
	if slugs, err := wsStore.List(); err != nil {
		return nil, fmt.Errorf("daemon startup refused: list workspaces: %w", err)
	} else {
		var wsErrs []error
		for _, slug := range slugs {
			if _, err := wsStore.Load(slug); err != nil {
				wsErrs = append(wsErrs, err)
			}
		}
		if len(wsErrs) > 0 {
			var msg strings.Builder
			msg.WriteString("daemon startup refused: failed to load workspace metadata\n")
			for _, e := range wsErrs {
				msg.WriteString("  - ")
				msg.WriteString(e.Error())
				msg.WriteString("\n")
			}
			msg.WriteString("Run `boid workspace configure <slug>` to fix the affected workspace.\n")
			return nil, fmt.Errorf("%s", msg.String())
		}
	}

	// Migrate any legacy unlinked projects (no project_workspaces row) into
	// the default workspace so every project lives under exactly one
	// workspace from this point on. Idempotent: skips projects already
	// linked. A failure here is non-fatal — the runner.go fallback still
	// routes secrets to the default namespace.
	if n, err := projectRepo.AssignDefaultWorkspaceToUnlinked(orchestrator.DefaultWorkspaceSlug); err != nil {
		slog.Warn("AssignDefaultWorkspaceToUnlinked failed",
			"workspace_id", orchestrator.DefaultWorkspaceSlug, "error", err)
	} else if n > 0 {
		slog.Info("migrated unlinked projects into default workspace",
			"workspace_id", orchestrator.DefaultWorkspaceSlug, "count", n)
	}

	projects, err := projectRepo.ListProjects()
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	errs := store.LoadAll(projects)
	// project.yaml が無くなった project は「dir が物理削除された stale 登録」
	// と判定して DB から自動 prune + 起動を継続する。 schema migration error は
	// 従来通り fail-fast (--auto-migrate で auto-resolve)、 parse error 等の
	// 他の load 失敗も fail-fast (config bug を masking しないため)。
	//
	// daemon が起動失敗していると `boid project rm` が socket を叩けず詰むので、
	// ENOENT のときに限り auto-prune するのが詰み回避策。 project.yaml は
	// project の source of truth なので、 dir が消えた DB row を消すのは
	// データ破壊リスクなし。
	remaining := errs[:0]
	for _, e := range errs {
		var missErr *orchestrator.ProjectMissingError
		if errors.As(e, &missErr) {
			slog.Warn("project dir missing; auto-pruning stale DB row",
				"project_id", missErr.ProjectID, "dir", missErr.Dir)
			if delErr := projectRepo.DeleteProject(missErr.ProjectID); delErr != nil {
				// DB 削除自体に失敗したらそれは fail-fast 対象 (DB が壊れている
				// 可能性)。 元の missing error を fallthrough させる。
				slog.Error("failed to auto-prune stale project; falling back to startup failure",
					"project_id", missErr.ProjectID, "error", delErr)
				remaining = append(remaining, e)
			}
			continue
		}
		remaining = append(remaining, e)
	}
	if len(remaining) > 0 {
		return nil, buildProjectLoadStartupError(remaining)
	}

	backfillUpstreamURLs(projectRepo, projects)

	return store, nil
}

// backfillUpstreamURLs captures upstream_url for any project registered
// before PR2 (docs/plans/git-gateway-cutover.md) added the column. Idempotent
// — projects that already have a value are skipped — so it is safe to run on
// every daemon startup. Capture failures (no git repo / no origin remote) are
// logged as warnings, never fatal to startup: the project keeps dispatching
// exactly as it did before this column existed until a remote is added and
// `boid project reload` (or the next startup) captures it.
func backfillUpstreamURLs(projectRepo *orchestrator.ProjectRepository, projects []*orchestrator.Project) {
	for _, p := range projects {
		if p.UpstreamURL != "" {
			continue
		}
		url, err := dispatcher.CaptureUpstreamURL(p.WorkDir)
		if err != nil {
			slog.Warn("project has no upstream_url and none could be captured; add a git remote and run `boid project reload`",
				"project_id", p.ID, "work_dir", p.WorkDir, "error", err)
			continue
		}
		if err := projectRepo.SetProjectUpstreamURL(p.ID, url); err != nil {
			slog.Warn("failed to persist backfilled upstream_url", "project_id", p.ID, "error", err)
			continue
		}
		slog.Info("backfilled project upstream_url", "project_id", p.ID, "upstream_url", url)
	}
}

// startupError holds the human-readable aggregate startup error text while
// also exposing its causes via Unwrap() []error so that callers (e.g. the
// boid start parent) can errors.As a *orchestrator.ProjectMigrationError
// out of it and drive auto-migration without parsing strings.
type startupError struct {
	aggregate string
	causes    []error
}

func (e *startupError) Error() string  { return e.aggregate }
func (e *startupError) Unwrap() []error { return e.causes }

// buildProjectLoadStartupError renders the legacy multi-line error message
// (byte-identical to the pre-typed-error version) while attaching the
// per-project causes so callers can errors.As the typed migration error.
//
// Per-project text lines retain the historical `  - <err.Error()>\n` shape;
// because *ProjectMigrationError formats with the `project "<id>": ...`
// prefix when ProjectID is set (via FormatMigrationIssue), the rendered
// output matches what users have been seeing in boid.log.
func buildProjectLoadStartupError(errs []error) error {
	var msg strings.Builder
	msg.WriteString("daemon startup refused: failed to load project metadata\n")
	migAgg := &orchestrator.ProjectMigrationError{}
	causes := make([]error, 0, len(errs)+1)
	for _, e := range errs {
		msg.WriteString("  - ")
		msg.WriteString(e.Error())
		msg.WriteString("\n")

		var migErr *orchestrator.ProjectMigrationError
		if errors.As(e, &migErr) {
			migAgg.Projects = append(migAgg.Projects, migErr.Projects...)
		} else {
			causes = append(causes, e)
		}
	}
	// migration ヒント行は実際に migration error が混じっているときだけ出す。
	// schema migration じゃない load 失敗 (parse error 等) に対して
	// 「Run boid project migrate <dir>」 を表示するのは misleading で、
	// --auto-migrate も migration error 以外には効かない。
	if len(migAgg.Projects) > 0 {
		msg.WriteString("Run `boid project migrate <dir>` for each affected project to migrate to the new schema.\n")
		// Put migration error first so errors.As walks find it quickly.
		causes = append([]error{migAgg}, causes...)
	}
	return &startupError{aggregate: msg.String(), causes: causes}
}

// runtimesDirFor returns the runtimes root directory for the given config.
func runtimesDirFor(cfg Config) string {
	if cfg.DBPath != "" && cfg.DBPath != ":memory:" {
		return filepath.Join(filepath.Dir(cfg.DBPath), "runtimes")
	}
	return filepath.Join(filepath.Dir(cfg.SocketPath), "runtimes")
}

// dataHomeFor returns the per-installation data root (typically
// ~/.local/share/boid). It is the parent of runtimesDirFor and the place
// where per-task data (e.g. tasks/<id>/attachments) lives. Empty when no
// suitable on-disk path can be derived (DB is in-memory and no socket path
// is configured) — callers should treat that as "feature disabled".
func dataHomeFor(cfg Config) string {
	if cfg.DBPath != "" && cfg.DBPath != ":memory:" {
		return filepath.Dir(cfg.DBPath)
	}
	if cfg.SocketPath != "" {
		return filepath.Dir(cfg.SocketPath)
	}
	return ""
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

	// Abort tasks left in awaiting state too: after a restart no agent is parked
	// in the in-memory BlockingAskRegistry, so an awaiting task is a zombie with
	// no live agent behind it. Same daemon_shutdown code → auto-reopened below.
	if n, err := dispatcher.MarkStaleAwaitingTasksAborted(srv.db); err != nil {
		slog.Warn("failed to abort stale awaiting tasks", "error", err)
	} else if n > 0 {
		slog.Info("aborted stale awaiting tasks on startup", "count", n)
	}

	projectRepo := orchestrator.NewProjectRepository(srv.db)
	taskRepo := orchestrator.NewTaskRepository(srv.db)
	jobRepo := dispatcher.NewJobRepository(srv.db)
	jobStore := jobStoreAdapter{repo: jobRepo}
	tx := apiTransactor{db: srv.db}

	jobRuntime, err := newJobRuntime(cfg)
	if err != nil {
		return nil, err
	}

	boidBin, _ := os.Executable()
	projectCatalog := orchestrator.DBProjectCatalog{DB: srv.db}
	taskLookup := orchestrator.DBTaskLookup{DB: srv.db}
	// Workspace lookup is plumbed through the interface field on
	// dispatcher.WireConfig (WorkspaceLookup). Go's typed-nil trap would
	// turn a nil *orchestrator.WorkspaceStore into a non-nil interface
	// value that panics on the first Load call — guard explicitly.
	var wsLookup dispatcher.WorkspaceLookup
	if ws := store.WorkspaceStore(); ws != nil {
		wsLookup = ws
	}

	// git gateway registry (docs/plans/git-gateway-cutover.md PR4): built
	// early and shared with the runner so Dispatch/UnregisterJob can
	// Register/Unregister job tokens. The gateway's HTTP handler (which
	// needs boidCfg + notifySvc, built further down) shares this same
	// Registry — see the gitgateway.NewServer(...) call below.
	srv.gatewayRegistry = gitgateway.NewRegistry()

	runner := dispatcher.Wire(dispatcher.WireConfig{
		DB:              srv.db,
		Runtime:         jobRuntime,
		Broker:          broker,
		Sandbox:         dispatcher.NewSandboxPreparer(),
		SecretStore:     secretStore,
		Projects:        projectCatalog,
		Workspaces:      wsLookup,
		ProxyAllocator:  srv.proxyManager,
		BoidBinary:      boidBin,
		ServerSocket:    cfg.SocketPath,
		ProxyPort:       &srv.proxyPort,
		AllowedDomains:  cfg.AllowedDomains,
		RuntimesDir:     runtimesDirFor(cfg),
		AttachmentsRoot: dataHomeFor(cfg),
		GitGateway:      srv.gatewayRegistry,
		GatewayURL:      &srv.gatewayURL,
	})

	lifecycle := jobLifecycleAdapter{runner: runner}
	claudeAdapter := claude.New()
	planner := orchestrator.WireDispatchPlanner(orchestrator.PlannerWireConfig{
		Meta:     store,
		Hydrator: store, // workspace-aware hydration at dispatch time
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
	workflow := &api.TaskWorkflowService{
		Tasks:       taskRepo,
		Jobs:        jobStore,
		Projects:    projectRepo,
		Tx:          tx,
		Meta:        store,
		Coordinator: &orchestrator.Coordinator{Evaluator: &orchestrator.Evaluator{}, HookExecutor: adapter, Waiter: adapter, MaxDepth: 5, LifecycleStore: taskRepo},
		Lifecycle:   lifecycle,
		Hub:         hub,
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
		Hydrator: store, // workspace-aware hydration for GET /api/projects/{id}
		// upstream_url capture (docs/plans/git-gateway-cutover.md PR2):
		// `project add` rejects projects with no git origin remote, and
		// `project reload` re-captures on every call.
		CaptureUpstreamURL: dispatcher.CaptureUpstreamURL,
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

	// git gateway HTTP handler (docs/plans/git-gateway-cutover.md PR4). Only
	// the listener bind (127.0.0.1:0) is deferred to Server.Start — the
	// handler itself, and the Registry it shares with the runner above, are
	// ready as soon as config + notifySvc are. The secret resolver closure
	// keeps internal/gitgateway free of any internal/dispatcher (and
	// therefore internal/db) import, per that package's own layering rule
	// (scripts/check-internal-architecture.sh).
	//
	// gwResolver is deliberately left nil (rather than a closure that always
	// errors) when secretStore itself is unconfigured (KeyFilePath unset):
	// CredentialProvider.Configured() reports false in that case, and
	// Server.ServeHTTP rejects gateway requests outright without ever
	// calling Inject or the notifier — see that method's doc comment
	// (docs/plans/git-gateway-cutover.md PR5 review: 「KeyFilePath 未設定時
	// の CredentialError 抑制」, distinct from an ordinary per-key miss on an
	// otherwise-configured store, which still fails open + notifies as
	// before).
	var gwResolver gitgateway.SecretResolver
	if secretStore != nil {
		gwResolver = func(key string) (string, error) {
			return secretStore.Get("default", key)
		}
	}
	gwCreds := gitgateway.NewCredentialProvider(boidCfg.Gateway.Hosts, gwResolver)
	gwHandler := gitgateway.NewServer(srv.gatewayRegistry, gwCreds, gatewayNotifier{notify: notifySvc})
	srv.gatewayHTTPServer = &http.Server{Handler: gwHandler}

	taskSvc := &api.TaskAppService{
		Tasks:              taskRepo,
		Actions:            taskRepo,
		Jobs:               jobStore,
		Meta:               store,
		Workflow:           workflow,
		Projects:           projectRepo,
		RuntimesDir:        runtimesDirFor(cfg),
		Notify:             notifySvc,
		BlockingAsk:        api.NewBlockingAskRegistry(),
		AskDisconnectGrace: boidCfg.TaskAsk.DisconnectGrace,
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

// sessionDispatcherAdapter implements api.SessionDispatcher (and, since the
// git gateway cutover's exec-via-Dispatch PR, api.ExecDispatcher too) by
// translating the request into a SessionJobInput and handing it to the
// runner. Phase 3-d (PR1) wired the session half in alongside the legacy
// ExecuteCommand path so the new entry can coexist with the existing
// Commands buttons until PR2 removes them; StartExec reuses the same struct
// because both entry points share identical project-hydration + Dispatch()
// plumbing — see StartExec's doc comment for why exec needed this at all.
type sessionDispatcherAdapter struct {
	service *api.ProjectAppService
	runner  *dispatcher.Runner
}

func (a *sessionDispatcherAdapter) StartSession(ctx context.Context, req api.StartSessionRequest) (*api.StartSessionResult, error) {
	project, err := a.service.GetProject(req.ProjectID)
	if err != nil {
		return nil, err
	}
	// project.Meta is workspace-hydrated by GetProject (see ProjectAppService
	// .hydrateProjectWithWorkspace) so Capabilities / Env / SecretNamespace
	// reflect the linked workspace.yaml.
	meta := project.Meta
	// shell sessions get a hard-coded interactive bash. Agent harnesses
	// (claude / codex / opencode) build their own argv from CLI conventions
	// and ignore SessionJobInput.Argv entirely.
	var argv []string
	if req.HarnessType == "shell" {
		argv = []string{"/bin/bash"}
	}
	spec, err := dispatcher.BuildSessionJobSpec(dispatcher.SessionJobInput{
		ProjectID:          project.ID,
		ProjectWorkDir:     project.WorkDir,
		ProjectName:        meta.Name,
		HarnessType:        req.HarnessType,
		Argv:               argv,
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
	if err != nil {
		return nil, &api.StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	jobID, err := a.runner.Dispatch(ctx, spec, nil)
	if err != nil {
		return nil, &api.StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	return &api.StartSessionResult{
		JobID:     jobID,
		AttachURL: fmt.Sprintf("/jobs/%s", jobID),
	}, nil
}

// StartExec implements api.ExecDispatcher: it builds an exec JobSpec
// (dispatcher.BuildExecJobSpec — HarnessType forced to "shell", Argv the
// caller's literal argv) and hands it to the same Runner.Dispatch() every
// session goes through. This is the git gateway cutover fix: routing exec
// through Dispatch means registerGatewayToken / buildGatewayCloneURL /
// RequireUpstreamURL all run automatically, exactly as they do for a
// session — no separate wiring for exec to fall out of sync with (the bug
// this PR fixes: `boid exec` never picked up the PR6 gateway wiring because
// it bypassed Dispatch() entirely).
//
// Unlike StartSession, no host_commands / broker registration happens
// client-side here — Dispatch() handles broker registration internally, so
// the old cmd/exec.go's manual POST /api/broker/register call (and the
// project-fixed, non-unique "exec-<project-id>" job id that leaked broker
// tokens across invocations) is gone: every exec now gets Dispatch()'s
// normal fresh UUID job id and its normal UnregisterJob cleanup.
func (a *sessionDispatcherAdapter) StartExec(ctx context.Context, req api.StartExecRequest) (*api.StartExecResult, error) {
	project, err := a.service.GetProject(req.ProjectID)
	if err != nil {
		return nil, err
	}
	meta := project.Meta

	spec, err := dispatcher.BuildExecJobSpec(dispatcher.SessionJobInput{
		ProjectID:          project.ID,
		ProjectWorkDir:     project.WorkDir,
		ProjectName:        meta.Name,
		Readonly:           req.Readonly,
		DisplayName:        req.DisplayName,
		Env:                meta.Env,
		HostCommands:       meta.HostCommands,
		AdditionalBindings: meta.AdditionalBindings,
		SecretNamespace:    meta.SecretNamespace,
		DockerEnabled:      meta.Capabilities.Docker != nil,
	}, req.Argv, req.Interactive)
	if err != nil {
		return nil, &api.StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	jobID, err := a.runner.Dispatch(ctx, spec, nil)
	if err != nil {
		return nil, &api.StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	return &api.StartExecResult{
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
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		}
	})

	r.Post("/api/shutdown", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
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
		Registry: brokerRegistry{
			broker:      newCommandBroker(srv.broker),
			projects:    runtime.projectRepo,
			metaStore:   srv.Store(),
			secretStore: srv.secretStore,
		},
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
		ExecDispatcher:    sessionAdapter,
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

	gcStore := orchestrator.NewTaskGCStore(srv.db).
		WithRuntimesDir(runtimesDirFor(srv.cfg)).
		WithSandboxTmpDir(os.TempDir()).
		WithRuntimeReaper(makeDockerRuntimeReaper()).
		WithAttachmentsRoot(dataHomeFor(srv.cfg))
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
		Pairing:   auth.NewPairingManager(runtime.authStore),
		Store:     runtime.authStore,
		PublicURL: gcCfg.Web.PublicURL,
		Registry:  runtime.connRegistry,
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
			AttachmentsRoot:   dataHomeFor(srv.cfg),
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
