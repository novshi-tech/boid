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
	"github.com/moby/moby/client"
	"github.com/novshi-tech/boid/internal/adapters/claude"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/api/auth"
	"github.com/novshi-tech/boid/internal/config"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/gitgateway"
	"github.com/novshi-tech/boid/internal/notify"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
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

func buildProjectStore(cfg Config, conn *sql.DB, projectRepo *orchestrator.ProjectRepository) (*orchestrator.ProjectStore, map[string]orchestrator.HostCommandSpec, error) {
	// The kit mechanism (orchestrator.KitRegistry / the KitResolver-driven
	// ws.Kits merge path) was retired in docs/plans/workspace-db-consolidation.md
	// Phase 2.5 PR6. KitResolver itself (and ProjectStore's resolver field
	// and NewProjectStore parameter) was removed outright in PR7 alongside
	// WorkspaceMeta.Kits.
	store := orchestrator.NewProjectStore()

	// Workspace DB cutover (docs/plans/workspace-db-consolidation.md PR3):
	// migrate any yaml-authority workspaces (DefaultWorkspaceDir()/*.yaml)
	// and kit host_commands (cfg.KitsDir) into the `workspaces` table before
	// anything below reads workspace data. Idempotent — a no-op once
	// already committed — and includes its own crash-recovery check, so
	// this is safe to call on every daemon startup. A migration failure
	// (corrupt yaml, a kit host_command name collision, a project
	// referencing an unresolvable workspace, or an unreconcilable crash
	// recovery mismatch) aborts daemon startup, same as the workspace
	// validation block below.
	if err := orchestrator.MigrateWorkspaceYAMLToDB(conn, "", cfg.KitsDir, projectRepo); err != nil {
		return nil, nil, fmt.Errorf("daemon startup refused: %w", err)
	}

	// Wire workspace store so GetWithWorkspace can hydrate workspace data
	// (capabilities, host_commands, env) at dispatch time. SetRepository
	// switches wsStore into DB mode (see WorkspaceStore's doc comment):
	// every Load/Save/Remove/List/EnsureDefault call below and at dispatch
	// time now routes through the workspaces table the migration above just
	// populated, instead of re-reading the yaml files (which remain on disk
	// only as a rollback/export shadow — decision 16).
	wsStore := orchestrator.NewWorkspaceStore("")
	wsStore.SetRepository(orchestrator.NewWorkspaceRepository(conn))
	store.SetWorkspaceStore(wsStore)

	// Ensure the implicit default workspace exists before the validation
	// pass below runs. MigrateWorkspaceYAMLToDB above already guarantees
	// this (it always ensures the default workspace row), so this call is a
	// belt-and-suspenders idempotent no-op in the normal case; it stays
	// here as defense-in-depth and because WorkspaceRepository.EnsureDefault
	// is safe to call unconditionally. We log but do not block daemon
	// startup on failure since the next Load attempt will surface the
	// problem with a sharper error.
	if err := wsStore.EnsureDefault(); err != nil {
		slog.Warn("EnsureDefault failed (default workspace may be missing)",
			"error", err)
	}

	// Validate every workspace row at startup (DB-backed since
	// SetRepository above). ErrNotExist from List means no rows yet — that
	// is the degraded window and is fine. Any other error (decode failure)
	// is a startup blocker.
	if slugs, err := wsStore.List(); err != nil {
		return nil, nil, fmt.Errorf("daemon startup refused: list workspaces: %w", err)
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
			// PR3 cutover 後、 workspace の権威は SQLite DB
			// (~/.local/share/boid/boid.db の workspaces テーブル)。
			// shadow yaml (~/.config/boid/workspaces/<slug>.yaml) を
			// 編集しても既に committed 済みの MigrateWorkspaceYAMLToDB
			// は再取込しないため、 shadow 編集単独では修復にならない。
			// daemon が起動拒否している (現状態) では workspace
			// edit/import CLI も使えないので、 現行の実質的な修復手段は
			// 以下いずれか:
			//   (a) SQLite CLI で workspaces テーブルの当該 slug 行を
			//       直接修正 or 削除 (削除後は daemon 再起動時に
			//       WorkspaceRepository.EnsureDefault が default を
			//       再生成するので、 default で足りる場合は削除で足りる)
			//   (b) schema_migrations テーブルから version=
			//       'workspace_db_consolidation' 行を削除 (state=staging
			//       にすると input_hash 再計算で不一致 abort、 完全削除
			//       すれば re-migrate される) + shadow yaml を修正、
			//       daemon 再起動で yaml から DB に再取込
			// PR7+ で offline 修復 CLI (`boid workspace repair` 相当) を
			// 検討する予定 (現状はまだ実装無し、 上記手動手順のみ)。
			msg.WriteString("Workspace metadata failed to decode from the DB (workspaces table). Recovery options:\n")
			msg.WriteString("  (a) delete the row directly via `sqlite3 ~/.local/share/boid/boid.db \"DELETE FROM workspaces WHERE slug='<slug>';\"` and restart the daemon (the default workspace is auto-recreated on boot)\n")
			msg.WriteString("  (b) fix the shadow yaml (~/.config/boid/workspaces/<slug>.yaml) AND clear the migration marker via `sqlite3 ~/.local/share/boid/boid.db \"DELETE FROM schema_migrations WHERE version='workspace_db_consolidation';\"`, then restart the daemon so the migration re-runs from yaml\n")
			return nil, nil, fmt.Errorf("%s", msg.String())
		}
	}

	// host_commands aggregation preflight (docs/plans/workspace-db-consolidation.md
	// PR2). hostCommandsPath is the aggregated config's on-disk location;
	// once written it is meant to be the authority (hand-editable — see the
	// plan doc), so MAJOR 3 (codex review) makes what follows conditional
	// on whether it already exists.
	hostCommandsPath, err := orchestrator.DefaultHostCommandsPath()
	if err != nil {
		return nil, nil, fmt.Errorf("daemon startup refused: resolve host_commands.yaml path: %w", err)
	}

	// MAJOR 3 (codex review): only aggregate-from-kits-and-rewrite when the
	// file does not exist yet. Before this fix, buildProjectStore
	// unconditionally re-aggregated every installed kit.yaml and rewrote
	// hostCommandsPath on *every* daemon startup — including every restart
	// long after MigrateWorkspaceYAMLToDB had already committed and written
	// it once. Since state=committed makes that migration a permanent no-op
	// (it never rewrites the file again), this unconditional rewrite was
	// the only thing still clobbering it: any hand edit to
	// ~/.config/boid/host_commands.yaml (the plan doc's documented way to
	// add/adjust a host_command without a kit) was silently reverted on the
	// very next `boid start`. The conditional below only regenerates when
	// the file is genuinely missing (fresh install racing ahead of the
	// migration in some test harness path, or a user manually deleting it)
	// — a self-healing fallback, not the steady-state path. A name
	// collision between two kits with differing definitions still aborts
	// daemon startup (decision 9 in the plan doc) whenever this fallback
	// does run.
	var hostCommands map[string]orchestrator.HostCommandSpec
	if _, statErr := os.Stat(hostCommandsPath); statErr == nil {
		hostCommands, err = orchestrator.LoadHostCommandsConfig(hostCommandsPath)
		if err != nil {
			return nil, nil, fmt.Errorf("daemon startup refused: load host_commands.yaml: %w", err)
		}
	} else if os.IsNotExist(statErr) {
		aggregatedHostCommands, err := orchestrator.LoadHostCommandsFromKits(cfg.KitsDir)
		if err != nil {
			return nil, nil, fmt.Errorf("daemon startup refused: %w", err)
		}
		if err := orchestrator.WriteHostCommandsConfig(hostCommandsPath, aggregatedHostCommands); err != nil {
			return nil, nil, fmt.Errorf("daemon startup refused: write host_commands.yaml: %w", err)
		}
		hostCommands, err = orchestrator.LoadHostCommandsConfig(hostCommandsPath)
		if err != nil {
			return nil, nil, fmt.Errorf("daemon startup refused: load host_commands.yaml: %w", err)
		}
	} else {
		return nil, nil, fmt.Errorf("daemon startup refused: stat host_commands.yaml: %w", statErr)
	}

	// Cutover (PR3): wire the aggregated host_commands map into the project
	// store so GetWithWorkspace can resolve workspace.HostCommands
	// (reference names) into full HostCommandSpec definitions at dispatch
	// time — see project_store.go's workspace host_commands merge block.
	// Symmetric with SetWorkspaceStore above.
	//
	// MAJOR 2 (codex review): hostCommands (loaded above, or returned to
	// this function's caller below) stays raw/unexpanded — that is the PR2
	// contract Server.HostCommands() exposes. But dispatch needs ${VAR}
	// placeholders (e.g. a kit's `path: ${HOME}/bin/gh`) resolved against
	// the daemon's own environment, or exec.LookPath fails on the literal
	// string. Wire an expanded *clone* into the dispatch-time store instead
	// of the raw map itself.
	store.SetHostCommands(orchestrator.ExpandHostCommandsForDispatch(hostCommands))

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
		return nil, nil, fmt.Errorf("list projects: %w", err)
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
		return nil, nil, buildProjectLoadStartupError(remaining)
	}

	backfillUpstreamURLs(projectRepo, projects)

	return store, hostCommands, nil
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

func (e *startupError) Error() string   { return e.aggregate }
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

// sandboxBackendForConfig selects the SandboxBackend Runner.Backend should
// be overridden to based on cfg.Sandbox.Backend (docs/plans/
// phase6-container-backend.md §PR7 cutover, §決定11). Returns (nil, nil)
// for the default "userns" (or an unset cfg — every pre-PR7 caller):
// leaving Runner.Backend nil is what makes Runner.sandboxBackend() keep
// constructing its own usernsBackend, so this is a true no-op for every
// deployment that hasn't opted in.
//
// "container" wires a real docker client (github.com/moby/moby/client —
// client.New(client.FromEnv) does not dial the docker daemon eagerly; it
// only resolves DOCKER_HOST/DOCKER_* env and builds the HTTP client
// config, so this never fails just because docker is unreachable at
// daemon-boot time — the same lazy-connect behavior cmd/reap.go's
// runReap already relies on) into a fresh containerBackend, carrying this
// installation's install_id (§決定6 resource labeling) and the
// host-visible runtimes directory (so `boid job log`'s transcript spool —
// §PR7's transcript persistence — and the per-job dockerproxy TLS
// materialize dir land under the same bind-mounted path a sibling docker
// daemon can actually reach, matching ContainerBackendOptions.RuntimeDir's
// own doc comment).
//
// installID and runtimeDir are threaded as plain values (not read from cfg
// itself) so this stays independently unit-testable without a live
// Server/DB — see wire_backend_test.go.
func sandboxBackendForConfig(cfg *config.Config, installID, runtimeDir string) (backend.SandboxBackend, error) {
	if cfg == nil || cfg.Sandbox.Backend != config.SandboxBackendContainer {
		return nil, nil
	}
	dockerClient, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("sandbox.backend: container: connect to docker: %w", err)
	}
	return dispatcher.NewContainerBackend(dockerClient, dispatcher.ContainerBackendOptions{
		InstallID:  installID,
		RuntimeDir: runtimeDir,
	}), nil
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
		DB:             srv.db,
		Runtime:        jobRuntime,
		Broker:         broker,
		Sandbox:        dispatcher.NewSandboxPreparer(),
		SecretStore:    secretStore,
		Projects:       projectCatalog,
		Hydrator:       store, // workspace-aware hydration for peer meta.name (buildPeerAdvertise)
		Workspaces:     wsLookup,
		ProxyAllocator: srv.proxyManager,
		BoidBinary:     boidBin,
		ServerSocket:   cfg.SocketPath,
		ProxyPort:      &srv.proxyPort,
		AllowedDomains: cfg.AllowedDomains,
		RuntimesDir:    runtimesDirFor(cfg),
		GitGateway:     srv.gatewayRegistry,
		GatewayURL:     &srv.gatewayURL,
	})

	// sandbox backend selection (docs/plans/phase6-container-backend.md
	// §PR7 cutover, §決定11): config-driven, global (not per-workspace).
	// sandboxBackendForConfig returns nil for the default/unset "userns"
	// case, leaving runner.Backend nil — Runner.sandboxBackend() then
	// keeps constructing the pre-Phase-6 usernsBackend on every call
	// exactly as before this PR, so every existing deployment (no
	// sandbox.backend key in config.yaml) is byte-for-byte unaffected.
	// "container" is the opt-in cutover this PR exposes; the plan doc's
	// own cutover gate (container e2e green + rollback rehearsal) is an
	// operational precondition on actually setting it in a real deploy,
	// not something enforced here.
	backendCfg, err := config.Load()
	if err != nil {
		slog.Warn("failed to load boid config for sandbox backend selection; defaulting to userns", "error", err)
	} else {
		sandboxBackend, berr := sandboxBackendForConfig(backendCfg, srv.installID, runtimesDirFor(cfg))
		if berr != nil {
			return nil, fmt.Errorf("daemon startup refused: %w", berr)
		}
		if sandboxBackend != nil {
			runner.Backend = sandboxBackend
			slog.Info("sandbox backend: container (docker) — cutover config (docs/plans/phase6-container-backend.md §PR7)")
		}
	}

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
		// Workspace CRUD (docs/plans/workspace-db-consolidation.md PR4):
		// store.WorkspaceStore() is the same DB-backed *orchestrator.WorkspaceStore
		// buildProjectStore wired via SetRepository above, so create/show/
		// update/remove operate on the exact same workspaces-table rows that
		// GetWithWorkspace hydration reads at dispatch time.
		Workspaces: store.WorkspaceStore(),
		// HostCommands lets CreateWorkspace/UpdateWorkspace validate every
		// meta.HostCommands reference against the daemon's live aggregated
		// snapshot (docs/plans/workspace-db-consolidation.md MAJOR 2, codex
		// review). srv.HostCommands already returns exactly this shape (see
		// its own doc comment) — the same method HostCommandsHandler uses
		// for GET /api/host_commands below.
		HostCommands: srv.HostCommands,
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
	//
	// The namespace parameter (post-cutover 改善 §1 workspace-scoped PAT
	// namespace) is passed straight through to secretStore.Get, which
	// already normalizes "" to "default" (SecretStore.normalizeNamespace),
	// so a job token registered with no SecretNamespace (workspace-unlinked
	// project) keeps resolving against the pre-namespacing "default"
	// namespace exactly as before this change.
	var gwResolver gitgateway.SecretResolver
	if secretStore != nil {
		gwResolver = func(namespace, key string) (string, error) {
			return secretStore.Get(namespace, key)
		}
	}
	gwCreds := gitgateway.NewCredentialProvider(boidCfg.Gateway.HostConfigs(), gwResolver)
	gwHandler := gitgateway.NewServer(srv.gatewayRegistry, gwCreds, gatewayNotifier{notify: notifySvc})
	srv.gatewayHTTPServer = &http.Server{Handler: gwHandler}
	// Kept alongside gatewayHTTPServer so Start can additionally bind the
	// TCP(mTLS) listener via gatewayHandler.ListenTLS
	// (docs/plans/phase6-container-backend.md §PR4).
	srv.gatewayHandler = gwHandler

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
		// runner (*dispatcher.Runner) satisfies jobContextProvider structurally
		// (its JobContext method backs the Phase 5b PR1 `boid task env` /
		// `boid task payload` RPCs — docs/plans/phase5-shim-and-task-context.md).
		// dataHomeFor(cfg) is the same value passed to
		// api.WebHandler.AttachmentsRoot (below) — the Phase 5b PR2 attachments
		// RPCs (`boid task attachments list|get`) must read from the identical
		// directory the upload path writes to (wiring-seams.md #15).
		srv.broker.BoidExecutor = newBoidBuiltinExecutor(workflow, taskSvc, jobStore, transcriptLogReader{rootDir: runtimesDirFor(cfg)}, runner, dataHomeFor(cfg))
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
	// HarnessType validation happens up at the HTTP handlers (see
	// api.validateHarnessType in session.go, called by SessionHandler /
	// ProjectHandler / WebHandler before dispatch), so by the time execution
	// reaches here it is already one of claude / codex / opencode. The old
	// `boid agent shell` session variant that forced argv=/bin/bash was
	// retired — `boid exec -p <project> -- bash` runs the shell adapter
	// through the same Runner.Dispatch() with an interactive PTY, so there
	// is no use case left for a session-mode shell. SessionJobInput.Argv is
	// left nil for the agent adapters (they build their own argv from CLI
	// conventions and ignore the field entirely — see the field's doc).
	spec, err := dispatcher.BuildSessionJobSpec(dispatcher.SessionJobInput{
		ProjectID:          project.ID,
		ProjectWorkDir:     project.WorkDir,
		ProjectName:        meta.Name,
		HarnessType:        req.HarnessType,
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

	workspaceHandler := &api.WorkspaceHandler{
		Service: runtime.projectSvc,
		// Home directory size reporting (Show) + deletion (Remove),
		// docs/plans/home-workspace-volume.md Phase 4 PR5. Same
		// runtimesDirFor(cfg) value the dispatcher itself resolves
		// homes/ from (dispatcher.WorkspaceHomesDir).
		RuntimesDir: runtimesDirFor(srv.cfg),
	}
	r.Mount("/api/workspaces", workspaceHandler.Routes())

	// host_commands read/reload (docs/plans/workspace-db-consolidation.md
	// PR4 Step G). srv itself satisfies api.HostCommandsService directly
	// (HostCommands()/ReloadHostCommands() already match that shape).
	hostCommandsHandler := &api.HostCommandsHandler{Service: srv}
	r.Mount("/api/host_commands", hostCommandsHandler.Routes())

	// Daemon config read surface (MAJOR 1, codex review round 1,
	// docs/plans/workspace-db-consolidation.md Phase 2.5 PR7): srv itself
	// satisfies api.ConfigService directly (KitsDir() already matches that
	// shape). Currently just GET /api/config/kits-dir; add further read-only
	// config fields here as they need a client-visible surface.
	configHandler := &api.ConfigHandler{Service: srv}
	r.Mount("/api/config", configHandler.Routes())

	taskHandler := &api.TaskHandler{Service: runtime.taskSvc, Hooks: runtime.workflow, Notifier: runtime.taskSvc, Answerer: runtime.taskSvc}
	r.Mount("/api/tasks", taskHandler.Routes())

	gcStore := orchestrator.NewTaskGCStore(srv.db).
		WithRuntimesDir(runtimesDirFor(srv.cfg)).
		WithSandboxTmpDir(os.TempDir()).
		WithRuntimeReaper(makeDockerRuntimeReaper()).
		WithAttachmentsRoot(dataHomeFor(srv.cfg))
	gcAppService := &api.GCAppService{Store: gcStore, DeviceStore: runtime.authStore}
	gcHandler := &api.GCHandler{
		Service: gcAppService,
		// workspace_homes size listing (docs/plans/home-workspace-volume.md
		// Phase 4 PR5) — visibility only, GC never deletes home directories
		// itself (that's `workspace remove`'s job). Workspaces flags orphan
		// home dirs (no matching workspace row) in the listing.
		RuntimesDir: runtimesDirFor(srv.cfg),
		Workspaces:  runtime.projectSvc,
	}
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

	// WebSocket attach (docs/plans/cli-remote-connection.md Phase 3 PR3:
	// "WebSocket attach 一本化"). Deliberately mounted at the top level of
	// r — NOT inside the cookie-only WebAuthMiddleware Group below (unlike
	// its pre-PR3 position) — for the same reason the Bearer device-auth
	// routes above are: over TCP this path must be gated solely by the
	// (Bearer-aware) TCPAPIAuthMiddleware wrapping the whole router
	// (auth.NewTCPAPIAuthMiddleware, applied to srv.tcpHandler at the
	// bottom of this function), not by the Group's cookie-only check. This
	// is also what makes the CLI's new WS-based AttachJob work at all over
	// the UNIX socket: the bare router (no auth middleware whatsoever, see
	// the "UNIX socket は trusted transport" comment below) is what the CLI
	// dials, so a route left inside the Group would 302-redirect-to-/login
	// a bare `net.Dial("unix", ...)` WS handshake that carries no cookie.
	//
	// The WSAttachHandler.Bearer field (Phase 3 PR0) only ever provided the
	// primitive; wiring the route to actually be Bearer-reachable over TCP
	// was explicitly left to this PR — see WSAttachHandler's own doc
	// comment, which predates this move.
	//
	// Cookie-based Web UI attach keeps working unchanged: WSAttachHandler.
	// authenticateDevice falls back to auth.DeviceIDFromContext when no
	// Authorization header is present, and TCPAPIAuthMiddleware still runs
	// the cookie check (via SessionSigner) for any TCP request without a
	// Bearer header, populating that same context key before the request
	// reaches this handler. /api/tasks/{id}/events (SSE) stays inside the
	// Group below unchanged — cookie-only is fine there, nothing needs it
	// reachable from a bare UNIX-socket CLI client the way attach does.
	r.Get("/api/jobs/{id}/attach/ws", (&api.WSAttachHandler{
		Subscriber: runtime.runner,
		Writer:     runtime.runner,
		PublicURL:  gcCfg.Web.PublicURL,
		Registry:   runtime.connRegistry,
		Bearer:     auth.NewBearerVerifier(runtime.authStore),
	}).ServeHTTP)

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

	// Shared rate limiter for every publicly-reachable pairing-code-redeem
	// entry point (docs/plans/cli-remote-connection.md Phase 3 PR0 決定事項:
	// device auth's rate limiting "既存 ratelimit.go を再利用、pair redeem と
	// 同じ枠に乗せて良い" — a bad actor guessing codes against /login, /auth,
	// and /api/auth/device all draws down the same per-IP bucket instead of
	// getting three independent five-attempt budgets).
	authRateLimiter := auth.NewRateLimiter(nil)

	// Login/auth routes (exempted by WebAuthMiddleware and CSRFMiddleware).
	loginHandler := &api.LoginHandler{
		Pairing: auth.NewPairingManager(runtime.authStore),
		Store:   runtime.authStore,
		Limiter: authRateLimiter,
	}
	if runtime.sessionSigner != nil {
		loginHandler.Signer = runtime.sessionSigner
	}
	r.Get("/login", loginHandler.GetLogin)
	r.Post("/login", loginHandler.PostLogin)
	r.Get("/auth", loginHandler.GetAuth)

	// Bearer device-auth routes (docs/plans/cli-remote-connection.md Phase 3
	// PR0): POST /api/auth/device is public (see apiAuthRequired's exemption
	// — rate-limited here instead), DELETE /api/auth/devices/{id} requires
	// Bearer auth like any other /api/* route. Deliberately mounted at the
	// same level as the other r.Mount("/api/...") calls above — NOT inside
	// the WebAuthMiddleware Group below — so that over TCP it is gated
	// solely by the (now Bearer-aware) TCPAPIAuthMiddleware wrapping the
	// whole router, not by that Group's cookie-only check.
	//
	// PublicURL is validated + normalized here at startup rather than in
	// PostDevice on every request — a misconfigured public_url is an
	// operator mistake (wrong scheme, path suffix, trailing slash, plain
	// http://) and should surface as one log line at boot, not as a
	// silent misbind of every future device token. An unusable value is
	// downgraded to the empty string so the handler falls back to the
	// request Host header (still HTTPS-forced by canonicalURL).
	deviceAuthPublicURL, publicURLErr := api.NormalizePublicURL(gcCfg.Web.PublicURL)
	if publicURLErr != nil {
		slog.Warn("web.public_url is invalid; canonical_url in device auth will fall back to request Host header",
			"value", gcCfg.Web.PublicURL, "error", publicURLErr)
		deviceAuthPublicURL = ""
	}
	deviceAuthHandler := &api.DeviceAuthHandler{
		Pairing:   auth.NewPairingManager(runtime.authStore),
		Store:     runtime.authStore,
		Limiter:   authRateLimiter,
		Registry:  runtime.connRegistry,
		PublicURL: deviceAuthPublicURL,
	}
	r.Mount("/api/auth", deviceAuthHandler.Routes())

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
