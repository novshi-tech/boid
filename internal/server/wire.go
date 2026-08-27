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
	"github.com/novshi-tech/boid/internal/apigateway"
	"github.com/novshi-tech/boid/internal/apiwire"
	"github.com/novshi-tech/boid/internal/config"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/gitgateway"
	"github.com/novshi-tech/boid/internal/integrationpack"
	"github.com/novshi-tech/boid/internal/mtls"
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
	// workspaceHomes is the engine-backed view of workspace HOME volumes
	// (docs/plans/workspace-home-volume-persistence.md 論点 a-2, PR7), built
	// in buildRuntime from the one docker client it already has and consumed
	// by mountRoutes' WorkspaceHandler/GCHandler. nil when there is no engine
	// (cfg.Backend injected — the test/DI seam), which is the feature's off
	// switch: see api.WorkspaceHomeStore's doc comment for why the engine
	// handle replaced the old RuntimesDir gate.
	workspaceHomes api.WorkspaceHomeStore
	// oauthLogin backs `boid secret oauth login <service>` (docs/plans/
	// api-gateway.md §7, PR3) — nil exactly when the API gateway's OAuth2
	// TokenSource itself is unwired (no secretStore configured), the same
	// "feature's off switch is an absent dependency" convention
	// workspaceHomes above already follows. mountRoutes only mounts the
	// route when this is non-nil.
	oauthLogin api.OAuthLoginService
	// packs is the daemon's loaded Integration Pack registry (docs/plans/
	// signal-ingest-detailed-design.md §6.2/§5.2, PR-5), resolved once at
	// buildRuntime startup (integrationpack.LoadPacks). Consumed by
	// mountRoutes to construct sessionDispatcherAdapter, which resolves a
	// signal-derived trigger's connector reference against it at StartExec
	// time. nil is a legitimate value (integrations.dir does not exist —
	// no Packs installed) — every connector-trigger StartExec then fails
	// with a clear "pack not installed" error rather than a nil panic.
	packs []*integrationpack.Pack
}

func buildProjectStore(cfg Config, conn *sql.DB, projectRepo *orchestrator.ProjectRepository) (*orchestrator.ProjectStore, map[string]orchestrator.HostCommandSpec, error) {
	// The kit mechanism (orchestrator.KitRegistry / the KitResolver-driven
	// ws.Kits merge path) was retired in docs/plans/workspace-db-consolidation.md
	// Phase 2.5 PR6. KitResolver itself (and ProjectStore's resolver field
	// and NewProjectStore parameter) was removed outright in PR7 alongside
	// WorkspaceMeta.Kits.
	store := orchestrator.NewProjectStore()

	// docs/plans/volume-only-daemon.md §論点b: the daemon-managed bare-repo
	// storage root (<data_dir>/repos/<workspace>/<project>.git). Wired here
	// (not just at CreateProjectFromGitURL's DataDir field below) so
	// LoadAll's degraded-status message can recognize a project whose
	// WorkDir falls outside this root as a pre-cutover, host-filesystem
	// registration — see ProjectStore.SetReposRoot's own doc comment.
	store.SetReposRoot(orchestrator.ReposRoot(dataHomeFor(cfg)))

	// docs/plans/workspace-default-project.md 論点h 案1 (PR7 round-3 Major
	// fix): reconcileExpectedProjectID needs to verify that a url-derived
	// expectedID was ACTUALLY derived from the project's current
	// UpstreamURL (not merely sharing URLDerivedProjectIDPrefix with one) —
	// see ProjectStore.SetDeriveProjectIDFunc's doc comment for why this is
	// wired in from here rather than called directly.
	store.SetDeriveProjectIDFunc(api.DeriveProjectIDFromURL)

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

	// INVARIANT (docs/plans/volume-only-daemon.md §論点a "on-startup
	// auto-prune 撤去"): filesystem/remote observations never justify a
	// hard delete of a project's DB row, at any point — not at startup, not
	// at dispatch, not at `boid project fetch`, not at GC. This block used
	// to auto-prune a project's DB row the moment its project.yaml could
	// not be read (reasoning: "ENOENT == the dir was physically removed,
	// safe to delete"). That exact shortcut is what turned a transient
	// "container can't see the host filesystem" observation into a
	// mass-deletion of 18 projects / 479 tasks / 816 jobs during the
	// 2026-07-24 dogfood session that prompted this doc's pivot — a
	// perfectly healthy DB row was indistinguishable, from inside that
	// shortcut, from a genuinely stale one.
	//
	// store.LoadAll (project_store.go) already implements the replacement:
	// every per-project load failure — missing dir, YAML parse error,
	// corrupt or unreachable bare repo, project.yaml missing from a bare
	// repo's HEAD — is recorded via ProjectStore.MarkDegraded (StatusDegraded)
	// and the DB row is left completely untouched. Nothing below this
	// point deletes anything. The one remaining fail-fast case is a schema
	// migration requirement (*ProjectMigrationError, resolved via
	// `boid project migrate <dir>` + `boid workspace create/edit
	// --from-file` — docs/plans/release-onboarding.md 決定2/PR5 removed the
	// bare-metal-only `boid start --auto-migrate` respawn path):
	// unlike the conditions above, that
	// signals a config SHAPE the daemon has no safe default interpretation
	// for, not an observational/transient failure, so refusing to start is
	// still the right call.
	//
	// The only two entry points that remove a project's DB row are now
	// `boid project rm <id>` and `boid workspace delete <name>` — both
	// explicit operator actions, never an inference from what the daemon
	// happened to observe on disk or over the network at some point in time.
	var migrationErrs []error
	for _, e := range errs {
		var migErr *orchestrator.ProjectMigrationError
		if errors.As(e, &migErr) {
			migrationErrs = append(migrationErrs, e)
			continue
		}
		slog.Warn("project failed to load; marked degraded, DB row preserved (docs/plans/volume-only-daemon.md §論点a)",
			"error", e)
	}
	if len(migrationErrs) > 0 {
		return nil, nil, buildProjectLoadStartupError(migrationErrs)
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
	// codex round-9 review of PR5, Blocker 1: this used to append one
	// more generic "Run `boid project migrate <dir>`" hint line here,
	// with a literal "<dir>" placeholder — redundant with, AND less
	// accurate than, what is already there: each `e.Error()` written
	// into msg above (for an `e` that IS a *ProjectMigrationError) is
	// FormatMigrationIssue's own per-project text, which already embeds
	// migrationGuidance(p.Dir, p.IsBareRepo) — the real path (or, for a
	// git-URL-registered project, the correct "this is a bare-repo path,
	// use your own clone instead" guidance). Nothing more to add here;
	// only the errors.As wiring (causes) is still needed.
	if len(migAgg.Projects) > 0 {
		// Put migration error first so errors.As walks find it quickly.
		causes = append([]error{migAgg}, causes...)
	}
	return &startupError{aggregate: msg.String(), causes: causes}
}

// sandboxBackendForConfig unconditionally constructs the containerBackend
// Runner.Backend is wired to (docs/plans/volume-only-daemon.md §論点e,
// PR-4): container is the only sandbox backend now — the userns backend and
// the sandbox.backend config option that used to select between the two
// were removed outright, so there is no branch left here, only construction.
//
// dockerClient is passed IN rather than built here, and the difference is
// load-bearing rather than stylistic [round 2 Major 2, codex review
// 2026-07-26]. The daemon-state-volume self-inspection
// (dispatcher.DetectDaemonStateVolumes) needs a docker client too, and when it
// built its own there were two constructions on the startup path. Two costs
// followed: a self-inspect that could fail on its own on a deployment whose
// backend is perfectly healthy — reported as an error the caller then had to
// be careful not to drop — and, less obviously, a second SYNCHRONOUS read of
// DOCKER_CERT_PATH's ca.pem/cert.pem/key.pem, which client.New(client.FromEnv)
// performs eagerly (moby client_options.go's WithTLSClientConfigFromEnv). That
// read is not interruptible by any context, so a wedged filesystem behind
// DOCKER_CERT_PATH would hang `boid start` inside a feature whose entire
// design is "warn and continue". buildRuntime now makes exactly one client and
// hands it to both, which puts that read back where it already was: a
// precondition of the container backend itself, which every release since the
// volume-only cutover has had.
//
// The client is real (github.com/moby/moby/client —
// client.New(client.FromEnv) does not dial the docker daemon eagerly; it
// only resolves DOCKER_HOST/DOCKER_* env and builds the HTTP client
// config, so constructing it never fails just because docker is unreachable at
// daemon-boot time — the same lazy-connect behavior cmd/reap.go's
// runReap already relies on). It carries this
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
//
// [Major 7, PR7 codex review]: DiagnosticsCollector is now wired to
// dispatcher.NewDefaultDiagnosticsCollector(dockerClient, runtimeDir) —
// before this fix it was left nil in every production path (see
// NewContainerBackend's own doc comment: "PR5 leaves this nil (no consumer
// yet)"), so an OOM-killed or setup-failure job container was removed with
// no diagnostic capture beyond whatever the attach-stream transcript spool
// happened to catch (exit code 137 and an empty log were often the only
// signal). NewDefaultDiagnosticsCollector runs only on an abnormal exit
// (ExitCode != 0) and degrades gracefully on its own inspect/logs failures
// — see its own doc comment for the full contract.
// brokerTLSCA/brokerTLSAddr thread the broker's mTLS trust material into the
// containerBackend this function may construct (docs/plans/
// phase6-cutover-followups.md §⓪ "broker TCP wire completion"):
// brokerTLSCA is the daemon's already-loaded internal CA (srv.daemonCA,
// hoisted to New() specifically so it exists before buildRuntime calls this
// function — see Server.daemonCA's own doc comment for why that hoist was
// needed), passed by value since the CA itself never changes for the life
// of the process. brokerTLSAddr is a pointer (srv's own
// &srv.brokerTLSSandboxAddr field) rather than a resolved string: the
// broker's TLS listener binds an OS-assigned ephemeral port only known once
// Start has run — strictly after this function returns — so the pointer is
// handed to ContainerBackendOptions.BrokerTLSAddr and dereferenced fresh by
// containerBackend.Launch on every call, the same late-binding-via-pointer
// pattern WireConfig.GatewayURL already uses one layer up. Both are nil for
// every caller that does not thread them (every test below except the ones
// added for this feature) — ContainerBackendOptions.BrokerTLSCA nil is the
// existing "feature disabled" contract (see its own doc comment), so this
// is a pure additive parameter, not a behavior change for anyone who leaves
// it nil.
func sandboxBackendForConfig(dockerClient *client.Client, installID, runtimeDir, transcriptDir string, brokerTLSCA *mtls.CA, brokerTLSAddr *string) backend.SandboxBackend {
	// UID/GID (PR9, §決定4 — "job container は `--user <daemon uid>:<gid>`
	// で非 root 起動"): os.Getuid()/os.Getgid() are the DAEMON's own actual
	// runtime uid/gid — under compose these are whatever `user:
	// "${BOID_UID}:0"` set them to (build/container/compose.yml, gid
	// hardcoded to 0 as of docs/plans/release-onboarding.md 決定1/PR2's
	// arbitrary-uid redesign), not necessarily ContainerBackendOptions'
	// own 1000:1000 default. Before this fix, sandboxBackendForConfig
	// never populated UID/GID at all, so every job container silently ran
	// as 1000:1000 regardless of the daemon's actual uid — harmless only
	// when an operator's host uid happens to also be 1000. Real impact
	// found via docs/plans/phase6-cutover-followups.md's e2e-container job
	// debugging trail: the workspace home directory (host-created by the
	// daemon process, mode 0700, owned by the daemon's real uid) became
	// unreadable ("permission denied") to a job container running under a
	// DIFFERENT uid whenever the two diverged (e.g. GH Actions runners
	// default to uid 1001, not 1000). Using the daemon's own
	// os.Getuid()/os.Getgid() here — rather than yet another config knob —
	// keeps this correct by construction: whatever uid the daemon
	// container actually runs as is exactly the uid its own bind-mounted
	// files (workspace homes included) are owned by, and now exactly the
	// uid job containers run as too.
	//
	// os.Getgid() legitimately resolves to 0 under the arbitrary-uid
	// compose config above — dispatcher.NewContainerBackend's gid-0-aware
	// guard (docs/plans/release-onboarding.md 決定1, PR2) accepts that
	// straight through rather than rejecting it as a root-resolving pair
	// the way it used to (see ContainerBackendOptions.UID's doc comment);
	// only a uid of 0 is still rejected.
	uid, gid := os.Getuid(), os.Getgid()
	return dispatcher.NewContainerBackend(dockerClient, dispatcher.ContainerBackendOptions{
		InstallID:     installID,
		RuntimeDir:    runtimeDir,
		TranscriptDir: transcriptDir,
		UID:           &uid,
		GID:           &gid,
		// DefaultImage (docs/plans/release-onboarding.md 穴4/PR4 codex
		// review, Blocker): BOID_IMAGE, when set, names the EXACT image ref
		// this daemon process itself was launched from — build/container/
		// compose.yml's daemon service now maps that same compose-level
		// variable into the container's own environment (its own comment
		// there explains why), so a compose deploy's daemon always sees it.
		// Without this, job containers would fall back to
		// version.DefaultContainerImage() evaluated INSIDE the daemon
		// process — for any non-exact-release build (the common case: a
		// GHCR ":latest" pull from main, not a tagged release) that
		// evaluates to the bare, registry-less "boid-runner:latest", the
		// exact 穴4 bug this PR exists to fix. A daemon that successfully
		// pulled its own ghcr.io/.../boid-runner:latest image would then
		// have every job dispatch try to pull an unrelated, nonexistent
		// docker.io/library/boid-runner:latest instead — "the daemon's pull
		// succeeds but the job runner still uses the old ref" is exactly
		// the inconsistency 穴4's table warns about, just one hop later
		// (daemon-vs-job instead of fallback-vs-primary). Empty (bare
		// `boid start` outside compose, or any deploy that hasn't set it)
		// leaves NewContainerBackend's own version.DefaultContainerImage()
		// fallback in charge, unchanged from before this fix.
		DefaultImage: os.Getenv("BOID_IMAGE"),
		// diagnostics.json follows the same TranscriptDir-else-RuntimeDir
		// fallback openTranscriptSpool applies internally (see
		// ContainerBackendOptions.TranscriptDir's doc comment) — computed
		// here rather than inside NewDefaultDiagnosticsCollector since that
		// free function only takes one directory string.
		DiagnosticsCollector: dispatcher.NewDefaultDiagnosticsCollector(dockerClient, firstNonEmpty(transcriptDir, runtimeDir)),
		// SelfContainerID (PR9, §決定5): $HOSTNAME is docker's own default
		// container hostname (the short container ID) unless a service
		// overrides it — build/container/compose.yml's `daemon` service
		// does not, so inside a real compose deploy this resolves to the
		// daemon's own container ID, letting
		// containerBackend.ensureWorkspaceNetwork connect this same
		// container to each per-workspace network it creates (see
		// ContainerBackendOptions.SelfContainerID's own doc comment for
		// why that is required, not optional, once workspace network
		// isolation is enabled). Outside a container (bare `boid start`
		// with sandbox.backend: container manually forced, or any non-
		// compose test/DI usage) $HOSTNAME is just the host's own
		// hostname — NetworkConnect against that bogus value fails
		// harmlessly (ensureWorkspaceNetwork's self-connect step is
		// best-effort, logged, non-fatal), not a new failure mode this
		// wiring introduces.
		SelfContainerID: os.Getenv("HOSTNAME"),
		BrokerTLSCA:     brokerTLSCA,
		BrokerTLSAddr:   brokerTLSAddr,
	})
}

// newWorkspaceHomeStoreFn is the package-level indirection that lets the
// workspace-HOME wiring be exercised end to end — buildRuntime through
// mountRoutes into a live HTTP response — without an engine, the same DI
// shape as detectDaemonStateVolumesFn below. Production never reassigns it.
//
// It takes the engine handle as dispatcher.WorkspaceHomeVolumeAPI rather than
// *client.Client for Go's typed-nil trap: a nil *client.Client assigned into
// an interface is a NON-nil interface, and the whole point of the nil check at
// the call site is that api.WorkspaceHandler.Homes / api.GCHandler.Homes being
// nil is what turns the feature off (see api.WorkspaceHomeStore's doc
// comment). buildRuntime therefore decides nil-ness on the CONCRETE
// *client.Client, before anything reaches an interface.
var newWorkspaceHomeStoreFn = func(engine dispatcher.WorkspaceHomeVolumeAPI, installID string) api.WorkspaceHomeStore {
	return &dispatcher.WorkspaceHomeVolumeStore{API: engine, InstallID: installID}
}

// detectDaemonStateVolumesFn is the package-level indirection that lets
// reservedDaemonStateVolumes' CALLER be tested without a container or an
// engine — the same DI shape as internal/dispatcher's daemonUID. The detection internals
// have their own seams one layer down (dispatcher's readSelfMountInfo /
// readSelfCgroup). Production never reassigns it.
//
// It is a bare alias for dispatcher.DetectDaemonStateVolumes and deliberately
// carries no logic of its own [round 2 Major 2, codex review 2026-07-26]. It
// used to construct a docker client here first, which put an engine-shaped
// failure IN FRONT of the engine-independent classification: a client that
// failed to build returned a zero DaemonStateVolumes — StateVolumeExpected
// false, i.e. the one value that means "nothing to protect, stay silent" —
// alongside a real error the caller then read second and discarded. A daemon
// with an unreadable DOCKER_CERT_PATH or a malformed DOCKER_HOST started with
// containment off and logged nothing at all, which is precisely the failure
// mode this whole feature exists to eliminate. buildRuntime now builds the one
// docker client (shared with the container backend, see
// sandboxBackendForConfig) and passes it in, so nothing between the caller and
// the classification can fail.
var detectDaemonStateVolumesFn = dispatcher.DetectDaemonStateVolumes

// reservedDaemonStateVolumes returns the docker volume names a sandboxed
// docker client must not be allowed to create, delete, or mount because THIS
// daemon's own persistent state lives in them
// (docs/plans/workspace-home-volume-persistence.md, the "未解決" entry this
// PR resolves: under the compose deploy that is `boid_boid_state`, holding
// boid.db / secret.key / tls/ca.key / web_secret / install_id).
//
// Never fatal, and the outcomes are deliberately logged differently. Silence
// is reserved for the ONE case that is a positive finding rather than a
// missing answer — that distinction is the whole of [Major 1] and is pinned by
// TestReservedDaemonStateVolumes_warnContract:
//
//   - No volume can be holding the daemon's state — its data root is an
//     ordinary directory on the root filesystem, i.e. bare `boid start`:
//     silent. Nothing is unprotected, and a warning would be pure noise on a
//     perfectly correct deployment.
//   - Detection failed (unknown id, ambiguous id, inspect error, inspect
//     timeout): ONE warning at startup, saying what is unprotected and how to
//     check it by hand. Refusing to boot would trade an exposure every release
//     so far has shipped with for a daemon that does not run at all on any
//     runtime this detection does not understand — a far worse deal.
//   - Detected: an info line naming the volumes, plus a warning if the
//     daemon's own data root turns out not to be inside any of them (every
//     step succeeded, yet the thing worth protecting is not covered).
//
// The warning text has to serve two very different readers, because the
// classification is deliberately biased toward warning: an operator running
// the compose/k8s deploy, for whom it means "your secrets are reachable from a
// job", and an operator running `boid start` on a host that merely keeps /home
// on its own filesystem, for whom it means "nothing to do". It therefore names
// the condition that produced it rather than asserting a breach.
//
// api is the docker client buildRuntime built for the container backend, or
// nil when there is none to share (cfg.Backend overridden — the test/DI seam).
// Nil is handled by the detection itself, as a failure that warns rather than
// as an answer.
func reservedDaemonStateVolumes(ctx context.Context, api dispatcher.SelfContainerInspector, dataHome string) []string {
	res, err := detectDaemonStateVolumesFn(ctx, api, dataHome)
	// ERROR FIRST [round 2 Major 2, codex review 2026-07-26]. This used to
	// branch on res.StateVolumeExpected before looking at err, so any failure
	// that came back with a zero result took the "nothing to protect" exit and
	// the error was never read. DetectDaemonStateVolumes now guarantees the
	// complementary half — every error return it makes carries
	// StateVolumeExpected=true — but the ordering here is what makes the two
	// halves independent: a future detection path that forgets the invariant
	// gets a spurious warning, not silence.
	if err != nil {
		slog.Warn("daemon state volume containment is INACTIVE: a named volume may be holding this daemon's own "+
			"persistent state, and no volume name could be reserved because the check did not complete. "+
			"If this daemon runs in a container, a job with capabilities.docker can mount the volume holding "+
			"boid.db and secret.key — find it with `docker inspect <this container>` / `docker volume ls` and see "+
			"docs/plans/workspace-home-volume-persistence.md. "+
			"If this daemon runs directly on a host that simply keeps its data root on a separate filesystem, "+
			"there is nothing to protect and nothing to do",
			"error", err, "data_home", dataHome)
		return nil
	}
	if !res.StateVolumeExpected {
		// A positive "nothing to protect". Detection reports this only when it
		// actually established that the data root is on the root mount, never
		// as the result of a check it could not complete.
		return nil
	}
	if len(res.Names) == 0 {
		slog.Warn("daemon state volume containment reserved nothing: this daemon's container has no named volume mounted. "+
			"If its persistent state is on a bind mount that is already covered (dockerproxy denies bind mounts outright); "+
			"if it is on a volume, the sandbox docker policy is not protecting it",
			"container_id", res.ContainerID, "data_home", dataHome)
		return nil
	}
	slog.Info("daemon state volume containment active",
		"container_id", res.ContainerID, "volumes", res.Names, "data_home_covered", res.DataHomeCovered)
	// Reported separately from the INACTIVE warning above [round 3 Minor 1,
	// codex review]: containment IS active here — the volumes were named and
	// reserved — but data_home_covered was computed against a data root the
	// daemon could not normalize, so that one field is not trustworthy. This
	// used to be dropped entirely, which meant a broken symlink in the data
	// root produced a completely clean startup log.
	if res.DataHomeUnresolved != nil {
		slog.Warn("daemon data root could not be normalized for the coverage check; "+
			"the volumes above ARE reserved, but data_home_covered was computed against an unresolved path and may be wrong",
			"data_home", dataHome, "error", res.DataHomeUnresolved)
	}
	// Gated on DataHomeUnresolved being nil: this line states as FACT the very
	// thing the warning above disclaims, so emitting both turns a real signal
	// into a contradiction an operator cannot act on. When resolution failed,
	// DataHomeCovered was computed against an unresolved path, so its false is
	// not evidence of anything — the warning above already says coverage is
	// unknown, so nothing goes unreported.
	if dataHome != "" && !res.DataHomeCovered && res.DataHomeUnresolved == nil {
		slog.Warn("daemon data root is not inside any of the reserved volumes; "+
			"name-based reservation is not protecting boid.db / secret.key",
			"data_home", dataHome, "volumes", res.Names)
	}
	return res.Names
}

// gatewayBindHost [Blocker 2, PR7 codex review] returns the listen address
// the git gateway's TCP(mTLS) listener binds to: "0.0.0.0" (composeBindHost)
// when the container backend is selected, "127.0.0.1" (the pre-PR7
// literal, byte-for-byte unchanged) otherwise. The container backend is the
// only one selected in production since PR-4's volume-only cutover removed
// the userns backend; the false branch remains a test-DI seam. A pure
// function — no Server state — so it is independently unit-testable
// without a live listener; see wire_backend_test.go.
func gatewayBindHost(usingContainerBackend bool) string {
	if usingContainerBackend {
		return composeBindHost
	}
	return "127.0.0.1"
}

// gatewayURLFor [Blocker 2, PR7 codex review] computes the git gateway's
// sandbox-facing base URL for the currently-selected backend:
//
//   - usingContainerBackend == false: gitgateway.BackendUserns's
//     http://10.0.2.2:<plainPort> — byte-for-byte the pre-PR7 literal
//     (docs/plans/phase6-container-backend.md §PR4: "既存 (10.0.2.2) を
//     無条件で切り替える禁止"). PR-4 (volume-only cutover) removed the
//     userns backend entirely, so this branch is unreachable in
//     production — `dispatcher.IsContainerBackend(runtime.runner.Backend)`
//     is always true now — and survives only as a test-DI seam
//     (wire_backend_test.go exercises both branches directly).
//   - usingContainerBackend == true: gitgateway.BackendContainer's
//     https://boid-gateway:<tlsPort> — a sibling job container has no
//     10.0.2.2 loopback projection at all (that address was a pasta/slirp
//     userns artifact with no docker equivalent), so it MUST use the
//     compose service DNS name over the mTLS listener instead.
//
// tlsPort is only consulted in the container-backend branch; plainPort only
// in the userns branch — Start's own call sites pass 0 for whichever one
// isn't relevant to a given call, matching SandboxURL's own "Port is
// backend-specific" contract. A pure function — no Server/listener state —
// so it is independently unit-testable; see wire_backend_test.go.
func gatewayURLFor(usingContainerBackend bool, plainPort, tlsPort int) string {
	if usingContainerBackend {
		return gitgateway.SandboxURL(gitgateway.SandboxURLOptions{
			Backend:     gitgateway.BackendContainer,
			Port:        tlsPort,
			ServiceName: composeGatewayServiceName,
		})
	}
	return gitgateway.SandboxURL(gitgateway.SandboxURLOptions{Backend: gitgateway.BackendUserns, Port: plainPort})
}

// runtimesDirFor returns the runtimes root directory for the given config.
func runtimesDirFor(cfg Config) string {
	if cfg.DBPath != "" && cfg.DBPath != ":memory:" {
		return filepath.Join(filepath.Dir(cfg.DBPath), "runtimes")
	}
	return filepath.Join(filepath.Dir(cfg.SocketPath), "runtimes")
}

// hostVisibleRuntimesDirFor returns a runtimes root a docker-out-of-docker
// (DooD) sibling container's bind-mount SOURCE can actually resolve on the
// HOST filesystem — the fix for the gap build/container/compose.yml's own
// "KNOWN GAP" comment flags (docs/plans/volume-only-daemon.md): under the
// volume-only compose deploy, runtimesDirFor(cfg) above prefers
// filepath.Dir(cfg.DBPath) — BOID_DATA_DIR, now the `boid_state` NAMED
// VOLUME, invisible to the daemon container's OWN filesystem view from the
// perspective of the HOST docker engine a sibling container's bind source
// must resolve against (§論点i's "DooD path 境界", realization.go's own
// MountSourceHostPath doc comment).
//
// cfg.SocketPath, by contrast, resolves under BOID_RUNTIME_DIR — a HOST
// BIND mount, source == target, deliberately left untouched by the
// volume-only pivot's compose.yml (see that file's own "Persistence"
// header comment: "BOID_RUNTIME_DIR remains a host BIND mount... this PR
// is scoped to daemon *data* persistence... this bind mount is still
// load-bearing"). filepath.Dir(cfg.SocketPath) is therefore exactly the
// host-visible root every DooD sibling-mount-source construction site
// needs: containerBackend's per-job dockerTLSDir/brokerTLSDir/spec.json/
// state.json/transcript-spool material (ContainerBackendOptions.RuntimeDir
// — sandboxBackendForConfig's own call site below), Runner.RuntimesDir's
// own downstream uses (workspace HOME — dispatcher.WorkspaceHomesDir — and
// the PR-2b per-job clone staging area, dispatcher.PrepareJobCheckout), and
// the matching job-log read paths (transcriptLogReader) that must agree
// with wherever those writes actually landed.
//
// Falls back to runtimesDirFor(cfg) when cfg.SocketPath is itself empty (an
// exotic config with neither a real DB path nor a socket path — matches
// runtimesDirFor's own last-resort fallback rather than returning "").
//
// Trade-off (flagged, not fully resolved by this PR — see compose.yml's
// own "KNOWN GAP" comment for the longer-term fix; codex round-1, PR834
// Minor 2 — this comment previously named only workspace HOME, understating
// the trade-off's actual scope): BOID_RUNTIME_DIR is host XDG_RUNTIME_DIR,
// typically `/run/user/<uid>` — tmpfs, cleared on host reboot (not on
// container restart). EVERY surface this function's own doc comment above
// lists therefore does NOT survive a host reboot under container backend,
// not just workspace HOME:
//   - workspace HOME content (dispatcher.WorkspaceHomesDir) — the one
//     home-workspace-volume.md Phase 4 intends to survive reboots for the
//     userns backend.
//   - job transcripts (containerBackend's transcript-spool material) and
//     runner-state.json diagnostics (spec.json/state.json) — `boid job log`
//     and post-mortem failure diagnosis both lose their on-disk backing the
//     moment the host reboots, for every job that ever ran under container
//     backend, not merely ones still in flight.
//   - the per-job dockerTLSDir/brokerTLSDir TLS material — inert after a
//     reboot regardless (per-job, reissued on the next dispatch), so this
//     one is harmless, but named here for completeness since it shares the
//     same root.
//   - the PR-2b per-job clone staging area (dispatcher.PrepareJobCheckout)
//     — also harmless by the same reasoning (re-cloned fresh per dispatch,
//     never expected to survive between jobs let alone a reboot).
//
// This is strictly better than today's total breakage (container CREATE
// fails outright — the bind source does not exist on the host at all,
// since it's the OLD, non-host-visible root), not the final answer; a
// docker-managed named volume (with per-workspace Subpath mounts) is the
// likely follow-up.
//
// Every call site of this function is conditional on the container backend
// actually being selected (see each call site's own comment) — a userns
// deployment (the overwhelming majority of pre-volume-only installs) never
// calls this function at all, so its own runtimesDirFor(cfg)-based
// persistence contract (CLAUDE.md's documented 30-day daemon GC, Phase 4's
// reboot-durable workspace HOME) is completely unaffected.
func hostVisibleRuntimesDirFor(cfg Config) string {
	if cfg.SocketPath == "" {
		return runtimesDirFor(cfg)
	}
	return filepath.Join(filepath.Dir(cfg.SocketPath), "runtimes")
}

// dataHomeFor returns the per-installation data root (typically
// ~/.local/share/boid). It is the parent of runtimesDirFor and the place
// where per-task data (e.g. tasks/<id>/attachments), the bare-repo storage
// root (orchestrator.ReposRoot) and the workspace home init bookkeeping
// (dispatcher's homes-meta/) live.
//
// It is NOT simply "the directory boid.db lives in", despite that being the
// only case that occurs in a real deployment. The full contract, in order:
//
//  1. filepath.Dir(cfg.DBPath) when the DB is a real file. This is the
//     production answer, and under the volume-only compose deploy it is the
//     `boid_state` named volume (build/container/compose.yml) — the same
//     directory as web_secret / install_id / secret.key / tls/ / kits/.
//  2. filepath.Dir(cfg.SocketPath) when cfg.DBPath is empty or ":memory:".
//     Test wiring, chiefly: an in-memory DB has no directory of its own, and
//     the socket's parent is the only other on-disk root such a config
//     names. Note that this is a DIFFERENT root from case 1 under the
//     container deploy (it resolves under BOID_RUNTIME_DIR — see
//     hostVisibleRuntimesDirFor), so a config that hits this branch does not
//     get the persistence properties case 1 has.
//  3. "" when neither is configured.
//
// Callers do not all read "" the same way, and both readings are deliberate:
//
//   - projectSvc.DataDir (attachments), webSecretPathFor's sibling logic and
//     orchestrator.ReposRoot treat "" as "feature disabled" — there is no
//     path to put the data at, so the feature simply does not operate.
//   - dispatcher.WireConfig.DataHomeDir does not. dispatcher's
//     workspaceHomeMetaDir falls back to the $XDG_DATA_HOME/ ~/.local/share
//     convention when it receives "" (see its own doc comment), because a
//     bare &dispatcher.Runner{} — the standard minimal test wiring — must
//     still resolve marker/lock somewhere rather than failing dispatch. That
//     fallback is why every package whose tests can reach Runner.Dispatch
//     installs testutil/homeenv's TestMain isolation.
func dataHomeFor(cfg Config) string {
	if cfg.DBPath != "" && cfg.DBPath != ":memory:" {
		return filepath.Dir(cfg.DBPath)
	}
	if cfg.SocketPath != "" {
		return filepath.Dir(cfg.SocketPath)
	}
	return ""
}

// transcriptsDirFor returns the persistent root for daemon-authored,
// per-job diagnostic artifacts (transcript.log, diagnostics.json) under a
// container-backend deploy: <dataHomeFor(cfg)>/runtime-transcripts, a
// sibling of boid.db under the boid_state named volume.
//
// This is distinct from hostVisibleRuntimesDirFor, which MUST resolve to a
// host-visible tmpfs path because a DooD sibling container's own bind
// mounts (spec.json/state.json/TLS material — writeContainerSpec) are
// sourced from it. transcript.log/diagnostics.json carry no such
// requirement: containerBackend writes both itself, from its own
// docker-attach/inspect/logs calls, never via a bind mount into the job
// container (see ContainerBackendOptions.TranscriptDir's doc comment) — so
// they can live on a real persistent volume instead of the host tmpfs that
// hostVisibleRuntimesDirFor resolves to, which is wiped on every host
// reboot (that function's own doc comment). Parking them here means a
// completed job's `boid job log` output and post-mortem diagnostics survive
// a reboot exactly like boid.db already does.
//
// Deliberately a NEW path, not a reuse of runtimesDirFor's existing value
// (which happens to compute the same parent directory when DBPath is set):
// runtimesDirFor is pinned by TestRuntimesDirFor_UnaffectedByHostVisibleVariant
// as the historical (pre-PR-4, now-removed userns backend) LocalRuntime
// root — reusing it here for an unrelated, container-backend-only purpose
// would conflate two different meanings under one function/directory.
//
// Empty when dataHomeFor(cfg) is empty (the same exotic-config case
// dataHomeFor itself documents) — every call site treats "" as "disable
// this persistence layer, fall back to RuntimeDir", not a hard error.
func transcriptsDirFor(cfg Config) string {
	home := dataHomeFor(cfg)
	if home == "" {
		return ""
	}
	return filepath.Join(home, "runtime-transcripts")
}

// firstNonEmpty returns a, or b if a is empty.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
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

// reapOrphansBeforeReopen calls runner.ReapOrphans (docs/plans/
// phase6-container-backend.md §PR7 / §決定 6) and translates its
// job-scoped ReapReport into (a) the set of TASK IDs the caller's
// subsequent daemon_shutdown auto-reopen sweep must skip, and (b) whether
// startup reap failed so completely that EVERY daemon_shutdown-aborted task
// must be held back this boot (see blockAllReopen below — [Blocker 4, PR7
// codex review]).
//
// ReapOrphans reasons about jobs (a backend's resource label is
// boid.job_id, not task_id — a task can have had several jobs across its
// lifetime), while FindDaemonShutdownAbortedTasks/auto-reopen reasons
// about tasks — this resolves FailedJobIDs back to their owning task via
// dispatcher.GetJob so the two layers can talk to each other. A job whose
// row can no longer be found (or that was never linked to a task, e.g. a
// taskless `boid exec`) is skipped rather than erroring: there is no task
// to protect from a double-reopen race in that case.
//
// [Blocker 4, PR7 codex review]: a non-nil return error or a non-nil
// ReapReport.GlobalError (e.g. the docker API was entirely unreachable —
// the userns backend, which never returns either, is unaffected) sets
// blockAllReopen=true. Before this fix, a global reap failure was only
// logged; the skip set stayed exactly whatever FailedJobIDs already
// contributed (§決定6's job-level granularity has no visibility into jobs a
// total listing failure never got to enumerate at all) — meaning a global
// error, however severe (docker itself down, or transiently unavailable
// right at daemon boot), silently degraded to "skip nothing" and every
// daemon_shutdown-aborted task auto-reopened anyway. §決定6's own rule is
// "reap は auto-reopen より前に完了させ、reap 失敗 task は reopen しない" — a
// GlobalError means reap did NOT complete for ANY job (the listing call
// itself never returned), so the caller has no way to know which tasks are
// actually safe: the only rule-consistent behavior is to treat every
// pending daemon_shutdown-aborted task as unreconciled and hold all of them
// back, not silently reopen every one of them as if reap had never run at
// all. This does not abort daemon startup outright (a global reap failure
// is scoped to sandbox-resource reconciliation, not e.g. the HTTP API or
// userns-backed dispatch) — it only withholds the auto-reopen sweep for
// this boot; a subsequent successful `boid start` (once docker recovers) or
// a manual `boid task action reopen` unblocks the affected tasks.
func reapOrphansBeforeReopen(ctx context.Context, runner *dispatcher.Runner, conn *sql.DB) (skip map[string]bool, blockAllReopen bool) {
	report, err := runner.ReapOrphans(ctx)
	if err != nil {
		slog.Error("startup reap failed; blocking ALL daemon_shutdown auto-reopen this boot to avoid a double-execution race (docs/plans/phase6-container-backend.md §決定6)",
			"error", err)
		blockAllReopen = true
	}
	if report.GlobalError != nil {
		slog.Error("startup reap: backend reported a global error; blocking ALL daemon_shutdown auto-reopen this boot to avoid a double-execution race (docs/plans/phase6-container-backend.md §決定6)",
			"error", report.GlobalError)
		blockAllReopen = true
	}
	if len(report.ReapedJobIDs) > 0 {
		slog.Info("startup reap: reconciled orphaned sandbox resources", "reaped_job_count", len(report.ReapedJobIDs))
	}

	skip = make(map[string]bool, len(report.FailedJobIDs))
	for _, jobID := range report.FailedJobIDs {
		job, jerr := dispatcher.GetJob(conn, jobID)
		if jerr != nil || job == nil || job.TaskID == "" {
			slog.Warn("startup reap: could not resolve a failed job to its task; unable to skip auto-reopen for it",
				"job_id", jobID, "error", jerr)
			continue
		}
		skip[job.TaskID] = true
	}
	return skip, blockAllReopen
}

func buildRuntime(srv *Server, cfg Config, store *orchestrator.ProjectStore, broker dispatcher.CommandBroker, secretStore *dispatcher.SecretStore) (*appRuntime, error) {
	// [Major 11, PR7 codex review, still relevant post-PR-4]: a fail-hard
	// config.Load() at the very top of buildRuntime — before even the
	// startup orphan-runtime cleanup immediately below — so a config.yaml
	// that becomes unreadable at reload time (a torn write from a
	// concurrent `boid` CLI edit, a permissions change, disk corruption —
	// anything short of ENOENT, which config.Load's own loadFromPath
	// already treats as "use defaults" and returns a nil error for) refuses
	// daemon startup outright rather than silently proceeding on defaults,
	// the same way every other daemon startup precondition in this file
	// does (buildProjectStore's own "daemon startup refused" errors above).
	// The value itself is discarded now: PR-4 (docs/plans/
	// volume-only-daemon.md §論点e) removed sandbox.backend and the
	// userns/container branch this load used to gate — container is the
	// only sandbox backend, so there is nothing left to select — but the
	// "config.yaml must actually be loadable at boot" invariant this check
	// also enforced stands on its own regardless.
	if _, err := config.Load(); err != nil {
		return nil, fmt.Errorf("daemon startup refused: load boid config: %w", err)
	}

	// Host-visible runtimes root (docs/plans/volume-only-daemon.md — see
	// hostVisibleRuntimesDirFor's own doc comment for the full rationale):
	// runner.RuntimesDir below, sandboxBackendForConfig's runtimeDir
	// argument further down, the startup orphan-runtime cleanup immediately
	// below, and the matching job-log read path (transcriptLogReader) all
	// use this SAME value, so `boid job log` / workspace HOME / the PR-2b
	// per-job clone staging area never disagree about where their own data
	// actually landed. Unconditional now (PR-4): every deployment uses the
	// container backend, so this is no longer choosing between two roots.
	runtimesRoot := hostVisibleRuntimesDirFor(cfg)

	// Persistent transcript/diagnostics root (transcriptsDirFor's own doc
	// comment) — daemon-authored artifacts that don't need runtimesRoot's
	// DooD host-visibility, so they're parked on the same persistent volume
	// as boid.db instead of the tmpfs runtimesRoot loses on host reboot.
	transcriptsRoot := transcriptsDirFor(cfg)

	// Clean up runtime dirs that have no corresponding job rows (must run before
	// MarkStaleJobsFailed so we only remove truly orphaned dirs).
	cleanOrphanRuntimes(runtimesRoot, srv.db)
	// Same orphan sweep for the transcripts root: a job row deleted by some
	// path other than GC (GC itself removes both roots together, see
	// TaskGCStore.cleanRuntimes) would otherwise leak a transcript dir
	// forever now that it survives reboots.
	cleanOrphanRuntimes(transcriptsRoot, srv.db)

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
	// API gateway registry (docs/plans/api-gateway.md PR1): same role and
	// same "built early, shared with the runner" timing as gatewayRegistry
	// above — the two gateways are otherwise fully independent, right down
	// to their own Registry types, and only share a listener (§論点1) at
	// the very end of this function.
	srv.apiGatewayRegistry = apigateway.NewRegistry()

	// THE docker client — singular, and that is the point [round 2 Major 2,
	// codex review 2026-07-26]. Two consumers need one: the container backend
	// (sandboxBackendForConfig, further down) and the daemon-state-volume
	// self-inspection (reservedDaemonStateVolumes, just below). Each used to
	// build its own; see sandboxBackendForConfig's doc comment for why the
	// duplicate was not merely redundant but a startup-hang and a
	// silent-failure surface of its own.
	//
	// Constructed here, before either consumer, because a failure is fatal to
	// the same thing in both cases: without a docker client there is no
	// sandbox backend, and PR-4 (docs/plans/volume-only-daemon.md §論点e) made
	// the container backend the only one. Refusing startup is what the daemon
	// already did — this only moves the refusal a few lines earlier and gives
	// it one call site instead of two. client.New(client.FromEnv) does not
	// dial (it resolves DOCKER_HOST/DOCKER_* and builds an HTTP client config;
	// API version negotiation is deferred to the first request), so this does
	// not require a reachable engine, only a coherent configuration.
	//
	// cfg.Backend set means a test/DI backend was injected and no engine is
	// involved at all, so no client is built. selfInspector is declared
	// separately rather than passing dockerClient directly for Go's typed-nil
	// trap: a nil *client.Client assigned into a SelfContainerInspector
	// parameter is a NON-nil interface that panics on first use, exactly the
	// hazard wsLookup above is guarded for. Detection handles a genuinely nil
	// interface as a failure that warns.
	var dockerClient *client.Client
	var selfInspector dispatcher.SelfContainerInspector
	if cfg.Backend == nil {
		var derr error
		dockerClient, derr = client.New(client.FromEnv)
		if derr != nil {
			return nil, fmt.Errorf("daemon startup refused: connect to docker: %w", derr)
		}
		selfInspector = dockerClient
	}

	// backendCfg/usingContainerBackend/runtimesRoot were already resolved at
	// the very top of this function (see that block's own doc comment for
	// why it moved there — PR834 PR-2b round-2 codex review Major 2) —
	// reused here rather than a second config.Load().
	runner := dispatcher.Wire(dispatcher.WireConfig{
		DB:                   srv.db,
		Broker:               broker,
		SecretStore:          secretStore,
		Projects:             projectCatalog,
		Hydrator:             store, // workspace-aware hydration for peer meta.name (buildPeerAdvertise)
		Workspaces:           wsLookup,
		ProxyAllocator:       srv.proxyManager,
		BoidBinary:           boidBin,
		ServerSocket:         cfg.SocketPath,
		ProxyPort:            &srv.proxyPort,
		NoWorkspaceProxyPort: &srv.noWorkspaceProxyPort,
		// AllowedDomains: captured once here, not a getter (PR #830 round-4
		// simplification, nose directive — sandbox.allowed_domains is
		// ReloadRestartRequired: see Runner.AllowedDomains's own doc
		// comment, internal/dispatcher/runner.go, and ReloadDynamic's own
		// doc comment, internal/config/schema.go). A later
		// `boid config set sandbox.allowed_domains ...` persists to
		// config.yaml immediately but only reaches this Runner on the next
		// daemon restart, when buildRuntime runs again from the
		// freshly-loaded config.
		AllowedDomains: cfg.AllowedDomains,
		RuntimesDir:    runtimesRoot,
		// DataHomeDir: deliberately dataHomeFor(cfg), NOT runtimesRoot
		// (docs/plans/workspace-home-volume-persistence.md 論点b, PR2). The
		// two are different roots since the volume-only pivot: runtimesRoot
		// is host-visible but lives under BOID_RUNTIME_DIR (tmpfs — see
		// hostVisibleRuntimesDirFor's own doc comment), whereas
		// dataHomeFor(cfg) resolves — in a real deployment, where cfg.DBPath
		// is a real file — to the `boid_state` volume that already holds
		// boid.db / web_secret / install_id (see dataHomeFor's own doc
		// comment for the in-memory-DB and empty cases, which only arise in
		// test wiring). The workspace home init marker and lock are daemon
		// bookkeeping that must outlive a host reboot and, from PR6 on, must
		// stay reachable by ordinary file I/O once the homes themselves
		// become named volumes — so they follow the DB, not the runtime dir.
		// Same value passed to projectSvc.DataDir, the attachments root and
		// orchestrator.ReposRoot elsewhere in this file.
		//
		// Giving the marker a different lifetime from the home it describes
		// is precisely why resolveWorkspaceHome also stamps an identity into
		// the home and refuses to trust a marker whose identity no longer
		// matches — see workspaceHomeMarker.HomeID.
		DataHomeDir: dataHomeFor(cfg),
		// ReservedVolumeNames: the volumes this daemon is itself mounted on,
		// which a sandboxed docker client must not be able to mount into a
		// sibling container. Resolved by asking the engine about THIS
		// container at startup — see reservedDaemonStateVolumes, and
		// dispatcher.DetectDaemonStateVolumes for why the name cannot simply
		// be pattern-matched. Empty when the daemon's data root is a plain
		// directory on the root filesystem (nothing to protect), and empty
		// with a warning if detection failed; never fatal, and bounded by
		// dispatcher's selfInspectTimeout so a wedged engine socket cannot
		// stall startup here.
		ReservedVolumeNames:     reservedDaemonStateVolumes(context.Background(), selfInspector, dataHomeFor(cfg)),
		GitGateway:              srv.gatewayRegistry,
		GatewayURL:              &srv.gatewayURL,
		GatewayCAPEM:            &srv.gatewayCAPEM,
		APIGateway:              srv.apiGatewayRegistry,
		APIGatewayServicesFloor: cfg.ServicesFloor,
	})

	// Sandbox backend construction (docs/plans/volume-only-daemon.md
	// §論点e, PR-4): unconditional now — container is the only sandbox
	// backend, so runner.Backend is always set here, never left nil.
	// runtimesRoot was already resolved at the very top of this function.
	// cfg.Backend, when set, overrides construction entirely — the test/DI
	// seam Config.Backend's own doc comment describes (server.go), so a
	// test can exercise daemon startup against a fake backend.SandboxBackend
	// without a live docker daemon. dockerClient is the one built above and
	// already handed to the self-inspection — the same handle, deliberately
	// (see sandboxBackendForConfig's doc comment); it is non-nil exactly when
	// cfg.Backend is nil, which is exactly this branch.
	sandboxBackend := cfg.Backend
	if sandboxBackend == nil {
		sandboxBackend = sandboxBackendForConfig(dockerClient, srv.installID, runtimesRoot, transcriptsRoot, srv.daemonCA, &srv.brokerTLSSandboxAddr)
	}
	runner.Backend = sandboxBackend
	// InstallID (PR9, §決定5): threaded onto Runner too, alongside
	// Backend — startDockerProxy's per-job SetWorkspaceNetwork call
	// needs it to compute the exact same containerWorkspaceNetworkName
	// value ContainerBackendOptions.InstallID (set two lines above, via
	// sandboxBackendForConfig) gave the container backend itself. See
	// Runner.InstallID's own doc comment.
	runner.InstallID = srv.installID
	slog.Info("sandbox backend: container (docker)")

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
		// TaskTriage: taskRepo already implements CardStore (see
		// internal/orchestrator/repository.go) — same object as Tasks above,
		// viewed through a narrower interface for TaskWorkflowService.Dispatch's
		// pre-tx children read (docs/plans/cross-project-issue-triage.md Phase
		// 1 PR-2).
		TaskTriage: taskRepo,
		// Actions: taskRepo already implements ActionListStore (see
		// internal/orchestrator/repository.go's ListActionsSince) — same
		// object as Tasks/TaskTriage above, viewed through a narrower
		// interface for docs/plans/ingestion-identity.md PR-3 (B-3)'s
		// BoidOpActionList read.
		Actions: taskRepo,
		// Triggers: taskRepo already implements TriggerRunStore (see
		// internal/orchestrator/repository.go's CreateTriggerRun /
		// CompleteTriggerRun / ListInFlightTriggerRuns / LatestTriggerRun) —
		// same object as Tasks/TaskTriage/Actions above, viewed through a
		// narrower interface for docs/plans/ingestion-identity.md PR-4
		// (B-5)'s trigger_runs single-flight/execution-record read+write.
		// Exec (api.ExecDispatcher) is deliberately NOT set here — see the
		// assignment at sessionAdapter's construction below (mountRoutes)
		// for why it can't be until then.
		Triggers: taskRepo,
		// Signals: taskRepo already implements api.SignalStore (see
		// internal/orchestrator/signal_store.go's IngestSignals /
		// GetSignalCursor / ListSignals / ClaimSignals / AckSignals /
		// HasPendingSignals, and internal/api/store.go's `var _ SignalStore
		// = (*orchestrator.TaskRepository)(nil)` assertion) — same object as
		// Tasks/TaskTriage/Actions/Triggers above, viewed through a narrower
		// interface. docs/plans/signal-ingest-detailed-design.md §4.2 (PR-6):
		// SweepTriggers' on:signals due predicate reads this via
		// signalsPendingForTrigger (trigger_loop.go).
		Signals: taskRepo,
		// TaskCreator is wired below, once taskSvc (*api.TaskAppService) is
		// constructed — see the comment there for why this can't be set here.
	}
	workflow.InitDispatch(context.Background())

	// Startup reap (docs/plans/phase6-container-backend.md §PR7 / §決定6):
	// reconcile orphaned sandbox resources — real work only for the
	// container backend; the userns backend's ReapOrphans is a permanent
	// no-op stub — BEFORE auto-reopening any daemon_shutdown-aborted task
	// below. A docker container does not die when this daemon process
	// restarts the way a userns child process does (MarkStaleJobsFailed
	// above only updates the DB row; it does not kill anything), so
	// auto-reopening before reap could dispatch a fresh agent against a
	// task whose previous job container is still alive — two agents
	// mutating the same $HOME/task RPC concurrently. reapOrphansBeforeReopen
	// returns the set of task IDs to skip reopening (every task with at
	// least one job ReapOrphans failed to reconcile). blockAllReopen
	// (Blocker 4, PR7 codex review) is set when reap failed so globally
	// (docker unreachable, listing itself errored) that no per-job
	// FailedJobIDs information exists at all — every daemon_shutdown-aborted
	// task is then treated as unreconciled, not just the ones ReapOrphans
	// happened to enumerate.
	reapSkipTasks, reapBlockAllReopen := reapOrphansBeforeReopen(context.Background(), runner, srv.db)

	// Startup sweep of interrupted workspace-home migrations (PR8 round-2 codex
	// review Major 2, docs/plans/workspace-home-volume-persistence.md 論点 f).
	//
	// Separate from the reap above and not foldable into it: that one reaps
	// CONTAINERS by label, and what an interrupted migration leaves behind is a
	// VOLUME — a workspace HOME holding whatever crossed before the daemon was
	// killed, with its completion marker already deleted. Left in place, the next
	// dispatch runs init.sh against it, and an init.sh that probes for what it
	// installs concludes the toolchain is already there and lets the daemon write
	// a fresh completion marker over a broken home.
	//
	// Placed HERE — after reap, before the auto-reopen sweep below — because
	// auto-reopen is the first thing in daemon startup that can dispatch, and the
	// discard has to be finished before any job can mount one of these volumes.
	//
	// A failure does not abort startup, for the same reason a reap failure does
	// not: it is scoped to resource reconciliation. The records stay on disk, so
	// the next start retries.
	//
	// context.Background() is correct here and not an oversight: this call is not
	// inside a request and has nothing better to derive from. The BOUND lives in
	// the sweep itself (workspaceHomeMigrationSweepTimeout, codex round 4 Major),
	// because "startup cannot park forever inside this step" is a property of the
	// step rather than of who calls it — an engine socket that accepts the
	// connection and then answers nothing would otherwise stop the daemon here,
	// before auto-reopen and before the API listener.
	//
	// The count is RECORDS RESOLVED, of which only some involved destroying
	// anything: a leftover record whose migration finished, or was refused before
	// it changed anything, is resolved by deleting the record alone. Which kind
	// each was is in the sweep's own per-record log line.
	if resolved, err := runner.SweepInterruptedWorkspaceHomeMigrations(context.Background()); err != nil {
		slog.Error("startup: could not resolve the leftover record(s) of an interrupted `boid workspace import-home`; they are left in place and a later start will retry (docs/plans/workspace-home-volume-persistence.md 論点 f)",
			"error", err)
	} else if resolved > 0 {
		slog.Warn("startup: resolved leftover `boid workspace import-home` record(s); any whose migration had actually destroyed a home have had that home discarded and will be initialized from scratch on their next dispatch",
			"count", resolved)
	}

	// Auto-reopen tasks that were interrupted by the previous daemon shutdown.
	// These tasks were aborted with code=daemon_shutdown either by
	// abortOnDispatchError (hook in flight when SIGTERM fired) or by
	// MarkStaleExecutingTasksAborted above (executing-state remnants from a
	// crash that bypassed the dispatch loop). Both paths set the same code
	// so a single startup query covers them.
	if shutdownIDs, err := dispatcher.FindDaemonShutdownAbortedTasks(srv.db); err != nil {
		slog.Warn("failed to query daemon_shutdown aborted tasks", "error", err)
	} else {
		if reapBlockAllReopen && len(shutdownIDs) > 0 {
			slog.Error("startup reap failed globally; withholding auto-reopen for every daemon_shutdown-aborted task this boot (docs/plans/phase6-container-backend.md §決定6) — reopen manually once sandbox resources are confirmed reconciled",
				"task_count", len(shutdownIDs))
		}
		for _, id := range shutdownIDs {
			switch {
			case reapBlockAllReopen:
				slog.Warn("skip auto-reopen: startup reap failed globally (docs/plans/phase6-container-backend.md §決定6); leaving it aborted to avoid a double-execution race",
					"task_id", id)
				continue
			case reapSkipTasks[id]:
				slog.Warn("skip auto-reopen: startup reap failed to reconcile a sandbox resource for this task (docs/plans/phase6-container-backend.md §決定6); leaving it aborted to avoid a double-execution race",
					"task_id", id)
				continue
			}
			if _, err := workflow.ApplyAction(orchestrator.WithActor(context.Background(), orchestrator.ActorDaemon), id, api.ApplyActionRequest{Type: "reopen"}); err != nil {
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
		// WorkspacesConn backs ApplyWorkspace/ExportWorkspaceEnvelopes
		// (docs/plans/volume-only-daemon.md PR-1d codex round-1 Blocker 2/3):
		// both need the daemon's raw *sql.DB handle to open their own single
		// transaction spanning workspace-meta + project-assignment
		// reads/writes. srv.db is the exact same connection projectRepo
		// above was built from.
		WorkspacesConn: srv.db,
		// InitScripts backs GET/PUT /api/workspaces/{slug}/init-script and the
		// spec.init_script half of export/apply (PR9 of
		// docs/plans/workspace-home-volume-persistence.md, 論点 d).
		//
		// The zero value is the whole wiring: the store deliberately has no
		// configurable root, because it must resolve the SAME path
		// resolveWorkspaceHome reads the script from — which comes from the
		// daemon's own os.UserConfigDir. Under the container deploy that is
		// inside the daemon's state volume, which is exactly why an HTTP
		// surface had to exist at all.
		//
		// Unconditional, unlike Homes/HomeImporter below: this store needs no
		// engine handle and no container backend, so there is no environment
		// where the daemon can serve workspaces but not their init scripts.
		InitScripts: dispatcher.WorkspaceInitScriptStore{},
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

	// Integration Pack registry (docs/plans/signal-ingest-detailed-design.md
	// §6.2, PR-4/PR-5): enumerate every installed Pack under
	// integrations.dir ONCE at daemon startup — the same "daemon 起動時
	// (wire.go の gateway 配線点)" point §6.2 names. A missing directory is
	// NOT an error (LoadPacks' own doc comment: no Packs installed yet is a
	// legitimate, common deployment state); every OTHER failure (a
	// malformed manifest, a pack/version directory mismatch, ...) fails
	// daemon startup outright (§6.2 item 3's "起動エラー" posture — the same
	// eager-validation contract config.yaml's own services: entries already
	// get, see config.Load's callers). packs feeds two independent
	// consumers below: apiGwCreds' service registry (via ResolveServices,
	// replacing the old services-only-minus-uses view) and
	// sessionDispatcherAdapter's connector-trigger resolution (mountRoutes).
	packs, err := integrationpack.LoadPacks(boidCfg.Integrations.Dir)
	if err != nil {
		return nil, fmt.Errorf("daemon startup refused: load integration packs: %w", err)
	}

	// Daemon-side config-editing surface (docs/plans/volume-only-daemon.md
	// §論点 f: `boid config get/set/unset/apply/edit`). verifyRestartExtractorCoverage
	// (BLOCKER, codex review round 3) runs FIRST, before srv.liveConfig or
	// srv.configPath are set — i.e. before this surface can accept a single
	// config mutation. Pre-fix, the equivalent exhaustiveness check lived
	// only inside applyDynamicConfigLocked's own loop, which runs AFTER
	// applyConfigYAMLLocked has already written the new document to disk
	// and swapped s.liveConfig (see that function's own doc comment) — so
	// an incomplete-coverage bug (a future ReloadRestartRequired schema leaf
	// added without a matching restartFieldExtractors/
	// restartFieldExtractorExemptions entry) would durably persist a config
	// mutation and only THEN panic and fail the HTTP request that caused
	// it, leaving config.yaml and the daemon's in-memory belief about it
	// out of sync going into the next restart. Panicking here instead,
	// before mountRoutes ever registers a route, converts "incomplete
	// coverage" into "the daemon refuses to start" — no config mutation can
	// ever reach applyConfigYAMLLocked while coverage is incomplete.
	verifyRestartExtractorCoverage()

	// srv.notifySvc is the SAME *notify.Service instance built above (and
	// wired into TaskAppService.Notify / the git gateway's notifier below)
	// — stored here so ApplyConfigYAML (internal/server/config_edit.go) can
	// hot-swap its Command/PublicURL live when notify.command/web.public_url
	// change. configPath resolution failing (os.UserConfigDir() erroring — the
	// same condition config.Load() above already tolerated by falling
	// back to DefaultConfig()) leaves srv.configPath empty; both
	// ConfigYAML and ApplyConfigYAML (internal/server/config_edit.go)
	// treat that as a hard error rather than guessing a path, so
	// GET/POST /api/config (`boid config get/set/unset/apply/edit`) fail
	// cleanly instead of reading/writing somewhere unexpected. Every other
	// daemon feature is unaffected — os.UserConfigDir() failing at all is
	// already an exotic (effectively Linux-only-with-no-$HOME) situation.
	srv.liveConfig = boidCfg
	srv.notifySvc = notifySvc
	// queue の決定論的評価 節 rule 4 (notify) — PR-3 wires the SAME notifySvc
	// instance workflow already shares with TaskAppService.Notify (see the
	// comment above) into TaskWorkflowService.Notifier, so `notify.command`
	// hot-reload (ApplyConfigYAML -> notifySvc.Update) also covers queue
	// entry notifications with no extra wiring.
	workflow.Notifier = notifySvc
	// queue の決定論的評価 節 rule 1 (wake 評価) — periodic sweep, mirroring the
	// GC loop's shape below. 1 分間隔は PR-3 の初期値 (config 化はしない —
	// GC 同様 config.yaml に足す価値は運用実績を見てから判断)。
	srv.queueSweepLoop = &api.QueueSweepLoop{
		Store:        workflow,
		Interval:     1 * time.Minute,
		InitialDelay: 20 * time.Second,
	}
	// No revision counter to seed here (BLOCKER, codex review round 2):
	// GET /api/config's ETag is now derived from config.yaml's own bytes
	// (internal/server/config_edit.go's computeRevision), computed fresh on
	// every read — there is no in-process baseline to initialize.
	if p, pathErr := config.DefaultPath(); pathErr == nil {
		srv.configPath = p
	} else {
		slog.Warn("failed to resolve config.yaml path; POST /api/config (`boid config set/unset/apply/edit`) will be unavailable", "error", pathErr)
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

	// API gateway HTTP handler (docs/plans/api-gateway.md PR1) — the same
	// resolver-nil-means-unconfigured convention as gwResolver above (its
	// own comment applies here verbatim, substituting "API gateway" for
	// "git gateway"). apiGwCreds' service registry is the FULL resolved
	// list — free-form services: entries (config.Config.APIGatewayServices,
	// validated at config load time) PLUS every `uses:` entry desugared
	// against packs (integrationpack.ResolveServices, docs/plans/
	// signal-ingest-detailed-design.md §6.2 item 2/PR-5's wiring — this
	// replaces the PR-4-era boidCfg.APIGatewayServices() call, which
	// deliberately excluded uses: entries; see that method's own doc
	// comment). A uses: entry that fails to desugar (pack/profile/
	// credential-slot mismatch) fails daemon startup outright, matching
	// LoadPacks' own posture just above.
	var apiGwResolver apigateway.SecretResolver
	if secretStore != nil {
		apiGwResolver = func(namespace, key string) (string, error) {
			return secretStore.Get(namespace, key)
		}
	}
	apiGwServices, err := integrationpack.ResolveServices(boidCfg, packs)
	if err != nil {
		return nil, fmt.Errorf("daemon startup refused: resolve integration pack services: %w", err)
	}
	apiGwCreds := apigateway.NewCredentialProvider(apiGwServices, apiGwResolver)

	// OAuth2 TokenSource (docs/plans/api-gateway.md §6, PR2): wired with the
	// same resolver as apiGwResolver above, plus a writer closure so a
	// refresh can persist the (possibly rotated) refresh_token/access_token/
	// expires_at back to the secret store — see
	// apigateway.OAuth2TokenSource's own doc comment for the load-bearing
	// persistence-order contract that writer participates in. Left unwired
	// (apiGwCreds keeps oauth == nil) when secretStore itself is
	// unconfigured, the same "resolver nil means unconfigured" convention
	// apiGwResolver/gwResolver above already follow — Resolve/Inject then
	// surface a clear "no OAuth2 TokenSource configured" error for any
	// oauth2-kind service instead of ever reaching a nil-deref.
	// oauthTokenSource is kept as its own named variable (rather than only
	// living inline inside SetOAuth2TokenSource's argument, as PR2 left it)
	// so runtime.oauthLogin below can share the EXACT SAME instance —
	// docs/plans/api-gateway.md §7, PR3: a login-obtained grant should be
	// immediately usable by the very next gateway request through this same
	// process's memCache/singleflight, not just eventually consistent via
	// the secret store once something else triggers a fresh refresh.
	var oauthTokenSource *apigateway.OAuth2TokenSource
	if secretStore != nil {
		apiGwWriter := func(namespace, key, value string) error {
			return secretStore.Set(namespace, key, value)
		}
		oauthTokenSource = apigateway.NewOAuth2TokenSource(boidCfg.APIGatewayOAuthProviders(), apiGwResolver, apiGwWriter)
		apiGwCreds.SetOAuth2TokenSource(oauthTokenSource)
	}

	apiGwHandler := apigateway.NewServer(srv.apiGatewayRegistry, apiGwCreds, apiGatewayNotifier{notify: notifySvc}, newAPIGatewayRecorder(taskRepo))

	// `boid secret oauth login <service>` (docs/plans/api-gateway.md §7,
	// PR3): apiGatewayLoginAdapter (apigateway_login.go) is api.
	// OAuthLoginHandler's OAuthLoginService, built only when oauthTokenSource
	// itself was (secretStore configured) — mountRoutes leaves the route
	// unmounted otherwise, the same "feature's off switch is an absent
	// dependency, not a nil-checked handler" convention gwCreds/apiGwCreds
	// above already follow for their own oauth2 wiring.
	var oauthLoginSvc api.OAuthLoginService
	if oauthTokenSource != nil {
		oauthLoginSvc = &apiGatewayLoginAdapter{creds: apiGwCreds, logins: apigateway.NewLoginManager(oauthTokenSource)}
	}

	// Shared listener (docs/plans/api-gateway.md 論点1: "同居 — path prefix
	// /j/ と /api/ で分岐"): combinedGatewayHandler dispatches by path
	// prefix to whichever gateway a request targets. gatewayHTTPServer
	// (the plaintext loopback listener, bound in Start) is served this
	// combined handler directly; Start's TLS block reads
	// srv.combinedGatewayHandler again to build the TCP(mTLS) listener's
	// own *http.Server, since neither gateway's ListenTLS-style helper can
	// serve a handler it didn't construct itself.
	combined := &combinedGatewayHandler{git: gwHandler, api: apiGwHandler}
	srv.gatewayHTTPServer = &http.Server{Handler: combined}
	srv.combinedGatewayHandler = combined

	// docs/plans/volume-only-daemon.md §論点a/b: `boid project add <git-url>`
	// / `boid project fetch <id>` need an authenticated bare clone/fetch —
	// wired here (rather than inline in the projectSvc literal above)
	// because both need gwCreds, which is only built once boidCfg is loaded
	// a few lines up. Reuses the exact same CredentialProvider the sandbox
	// git gateway reverse proxy authenticates job clones with — see
	// dispatcher.CloneBareRepo/FetchBareRepo's own doc comments for why this
	// is a direct CredentialProvider.Resolve call rather than a round trip
	// through the HTTP reverse proxy (there is no job token / sandbox
	// involved; this runs in the daemon process itself).
	projectSvc.CloneBareRepo = func(ctx context.Context, url, dest, namespace string) error {
		return dispatcher.CloneBareRepo(ctx, url, dest, gwCreds, namespace)
	}
	projectSvc.FetchBareRepo = func(ctx context.Context, bareRepoPath, namespace string) error {
		return dispatcher.FetchBareRepo(ctx, bareRepoPath, gwCreds, namespace)
	}
	projectSvc.DataDir = dataHomeFor(cfg)

	// docs/plans/volume-only-daemon.md §論点b, PR-2b: Runner.Dispatch's own
	// per-job-clone step (runner.go) calls dispatcher.FetchBareRepo directly
	// against a git-URL-registered project's bare repo before staging a
	// per-job checkout off of it — reusing this exact same gwCreds the two
	// closures immediately above already wire into ProjectAppService, for
	// the identical reason their own doc comment gives (no job token or
	// sandbox involved; this runs in the daemon process itself).
	runner.GatewayCredentials = gwCreds
	// WithProjectLock (PR834 PR-2b round-2 codex review Major 1): shares
	// projectSvc's own projectMu with the per-job-clone dispatch step above,
	// so it serializes against a concurrent `project rm` + re-add at the
	// same managed path — see ProjectAppService.WithProjectLock's own doc
	// comment for why this is a plain function value rather than a direct
	// import (internal/dispatcher cannot import internal/api — this package
	// already imports internal/dispatcher for wiring).
	runner.WithProjectLock = projectSvc.WithProjectLock
	// ConfirmWorkspaceExists (PR8 round-2 codex review Major 1): closes over the
	// same projectSvc so Runner.ImportWorkspaceHome can re-check the workspace
	// ROW while holding its migration registration. Without it, a
	// `boid workspace remove` that completes between the API handler's own
	// pre-check and the migration's first engine call leaves the migration
	// creating a HOME volume for a row that is gone — credentials in a volume no
	// dispatch can mount and `workspace remove` can no longer delete. A plain
	// function value for the same reason WithProjectLock above is one.
	//
	// The service's error is passed through unchanged (an *api.StatusError, i.e.
	// a 404 for a workspace that is gone) because the handler maps it back to a
	// status — see WorkspaceHandler.ImportHome's errors.As branch.
	runner.ConfirmWorkspaceExists = func(slug string) error {
		_, err := projectSvc.GetWorkspace(slug)
		return err
	}

	taskSvc := &api.TaskAppService{
		Tasks:    taskRepo,
		Actions:  taskRepo,
		Jobs:     jobStore,
		Meta:     store,
		Workflow: workflow,
		Projects: projectRepo,
		// Identities backs the brokered task_identity_link/_unlink/_resolve
		// ops (docs/plans/ingestion-identity.md PR-1). Same repo again —
		// taskRepo already implements api.TaskIdentityStore (see
		// internal/orchestrator/repository.go).
		Identities: taskRepo,
		// runtimesRoot (not a fresh runtimesDirFor(cfg) — codex round-1,
		// PR834 Minor 1): must agree with runner's own RuntimesDir and
		// transcriptLogReader.rootDir just below, both already using
		// runtimesRoot for exactly this reason (see runtimesRoot's own
		// comment above). Under the container backend, runtimesDirFor(cfg)
		// resolves under the boid_state NAMED VOLUME (invisible on the
		// host), while task-detail's own workspace_path — sourced from this
		// RuntimesDir — needs the SAME host-visible root every other
		// per-job artifact (transcript, runner-state.json, the PR-2b clone
		// staging area) actually landed under, or `boid task get`/the web
		// UI's task detail would report a workspace_path nothing on the
		// host can resolve.
		RuntimesDir:        runtimesRoot,
		Notify:             notifySvc,
		BlockingAsk:        api.NewBlockingAskRegistry(),
		AskDisconnectGrace: boidCfg.TaskAsk.DisconnectGrace,
	}
	// Late-bind workflow.TaskCreator to taskSvc now that taskSvc exists
	// (docs/plans/cross-project-issue-triage.md Phase 1 PR-2): workflow is
	// constructed before taskSvc (taskSvc.Workflow needs workflow to already
	// exist), so this is the same late-binding idiom as workflow.InitDispatch
	// above, just for a field instead of a method call. See
	// api.TaskCreator's own doc comment for why this can't be a literal
	// struct-construction cycle instead.
	workflow.TaskCreator = taskSvc
	if srv.broker != nil {
		// runner (*dispatcher.Runner) satisfies jobContextProvider structurally
		// (its JobContext method backs the Phase 5b PR1 `boid task env` /
		// `boid task payload` RPCs — docs/plans/phase5-shim-and-task-context.md).
		// dataHomeFor(cfg) is the same value passed to
		// api.WebHandler.AttachmentsRoot (below) — the Phase 5b PR2 attachments
		// RPCs (`boid task attachments list|get`) must read from the identical
		// directory the upload path writes to (wiring-seams.md #15).
		// transcriptLogReader.rootDir uses transcriptsRoot (not runtimesRoot)
		// — under container backend this must be the SAME persistent root
		// containerBackend's own transcript spool actually writes to now
		// (openTranscriptSpool, sandboxBackendForConfig's transcriptDir
		// argument above — see transcriptsDirFor's own doc comment), or
		// `boid job log`/`boid task env`-adjacent broker RPCs would read
		// from the wrong directory. fallbackRootDir: runtimesRoot covers
		// jobs whose transcript was written under the pre-migration tmpfs
		// root by a daemon build that predates this PR (codex review round
		// 1 — see transcriptLogReader's own doc comment).
		// taskRepo (in scope above) already implements api.SignalStore
		// (PR-1's var _ assertion, internal/api/store.go) — passed
		// explicitly rather than relying on a runtime interface check
		// against workflow (docs/plans/signal-ingest-detailed-design.md
		// §3.2, PR-3; M1 of PR #1014's review: workflow's concrete type
		// *api.TaskWorkflowService does not implement api.SignalStore, so
		// the earlier runtime-check version of this wiring silently left
		// every signal op unavailable in production).
		srv.broker.BoidExecutor = newBoidBuiltinExecutor(workflow, taskSvc, jobStore, transcriptLogReader{rootDir: transcriptsRoot, fallbackRootDir: runtimesRoot}, runner, dataHomeFor(cfg), projectSvc, taskRepo)
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
		// workspaceHomes: the SAME dockerClient the container backend and the
		// startup self-inspection got (see sandboxBackendForConfig's doc
		// comment on why there is exactly one), so `workspace show`/`workspace
		// remove`/`boid gc` talk to the engine that actually holds the
		// volumes. Left nil when dockerClient is nil, which happens exactly
		// when cfg.Backend injected a test/DI backend — nil on the CONCRETE
		// pointer, decided here, because a typed-nil handed through the
		// interface would be non-nil as an interface and turn the feature
		// silently ON against a client that panics on first use.
		workspaceHomes: workspaceHomeStore(dockerClient, srv.installID),
		oauthLogin:     oauthLoginSvc,
		packs:          packs,
	}, nil
}

// workspaceHomeStore builds the workspace-HOME volume store from the daemon's
// one docker client, or returns a nil api.WorkspaceHomeStore when there is no
// client to build it from. The nil-ness decision lives here, on the concrete
// *client.Client, for the typed-nil reason newWorkspaceHomeStoreFn's doc
// comment gives; every caller downstream only ever sees the interface.
func workspaceHomeStore(dockerClient *client.Client, installID string) api.WorkspaceHomeStore {
	if dockerClient == nil {
		return nil
	}
	return newWorkspaceHomeStoreFn(dockerClient, installID)
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
	// packs is the daemon's loaded Integration Pack registry (docs/plans/
	// signal-ingest-detailed-design.md §5.2, PR-5), resolved once at
	// buildRuntime startup and reused for every StartExec call whose
	// StartExecRequest carries a Connector reference (fireTrigger's
	// derived-trigger dispatch, internal/api/trigger_loop.go). nil-safe:
	// resolveConnectorExec's own findInstalledPack fails loudly ("pack not
	// installed") against a nil/empty packs rather than panicking. Every
	// StartExec call with req.Connector == nil never consults this field.
	packs []*integrationpack.Pack
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

	input := dispatcher.SessionJobInput{
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
	}

	// docs/plans/signal-ingest-detailed-design.md §5.2 (PR-5): a
	// signal-derived trigger's connector job carries req.Connector — the
	// ONLY caller that ever sets it is fireTrigger (internal/api/
	// trigger_loop.go), firing a hydrate-derived Trigger.Connector != nil.
	// Resolving it here (rather than at fireTrigger, which must not import
	// internal/integrationpack — see TriggerConnector's own doc comment) is
	// where the daemon's loaded Pack registry (a.packs) actually lives.
	// A resolution failure (pack not installed, connector unknown, config
	// schema violation) fails StartExec outright — fireTrigger's existing
	// fail-open retry (dispatch failed this tick, retried next tick once
	// `every` elapses again) is the SAME mechanism an ordinary dispatch
	// failure already gets; no separate error path needed.
	if req.Connector != nil {
		resolved, rerr := resolveConnectorExec(a.packs, *req.Connector)
		if rerr != nil {
			return nil, &api.StatusError{Code: http.StatusBadRequest, Message: rerr.Error()}
		}
		// env (§5.2 item 1): merged on top of the project's own Env — a
		// connector job still inherits ordinary project.yaml env, but the 4
		// signal vars always win on a name collision (there should never be
		// one in practice, but "the connector's own identity wins" is the
		// safer direction if there is).
		env := make(map[string]string, len(input.Env)+len(resolved.Env))
		for k, v := range input.Env {
			env[k] = v
		}
		for k, v := range resolved.Env {
			env[k] = v
		}
		input.Env = env
		// No bind mount here (§5.2 item 2, revised 2026-08-27): daemon and
		// job container share the same base image, so BOID_CONNECTOR_EXEC
		// (set above via resolved.Env) already points at a path that exists
		// inside the job container's own filesystem — see
		// resolveConnectorExec's (connector_exec.go) own doc comment for why
		// a bind mount would in fact be WRONG under DooD.
		// 縮小 policy (§5.2 item 3, Q27): ConnectorBuiltinPolicies instead of
		// the general set — see SessionJobInput.ConnectorPolicy's own doc
		// comment.
		input.ConnectorPolicy = true
		// API gateway token service allowlist (§5.2 item 4): restrict to
		// exactly the declared service.
		input.APIGatewayServices = resolved.APIGatewayServices
		// TokenContext.Service/Connector (§3.2/§5.2): the broker-side
		// authorization for signal_ingest/signal_cursor_get.
		input.SignalService = req.Connector.Service
		input.SignalConnector = req.Connector.Pack + "/" + req.Connector.ConnectorName
	}

	spec, err := dispatcher.BuildExecJobSpec(input, req.Argv, req.Interactive)
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

	// Host-visible runtimes root (docs/plans/volume-only-daemon.md — see
	// hostVisibleRuntimesDirFor's own doc comment): must agree with
	// buildRuntime's own runtimesRoot determination (runner.RuntimesDir,
	// sandboxBackendForConfig's runtimeDir argument) or the read paths
	// wired below (`boid gc`'s runtime-directory sweep, the job-log REST
	// endpoint) would look in the wrong directory for a container-backend
	// deploy. Workspace-home size/delete and GC's workspace_homes listing
	// USED to be on that list; PR7 of
	// docs/plans/workspace-home-volume-persistence.md took them off it —
	// they read the engine's volume API now and never resolve a host path.
	// dispatcher.
	// IsContainerBackend(runtime.runner.Backend) is the cheap, already-
	// resolved signal buildRuntime itself left behind (runtime.runner.
	// Backend is only non-nil when sandboxBackendForConfig actually
	// constructed a container backend) — reusing it here avoids a second,
	// independent config.Load() that could theoretically disagree.
	usingContainerBackend := dispatcher.IsContainerBackend(runtime.runner.Backend)
	runtimesRoot := runtimesDirFor(srv.cfg)
	if usingContainerBackend {
		runtimesRoot = hostVisibleRuntimesDirFor(srv.cfg)
	}
	// Persistent transcript/diagnostics root — must agree with buildRuntime's
	// own transcriptsRoot (sandboxBackendForConfig's transcriptDir argument),
	// same reasoning as runtimesRoot above. A pure function of srv.cfg, so
	// recomputing here (rather than threading it through appRuntime) cannot
	// drift from buildRuntime's value.
	transcriptsRoot := transcriptsDirFor(srv.cfg)

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

	// GET /api/cli-token-check (round-2 codex review Blocker 2, PR-3 Option
	// 4 host-mode redesign): an authenticated no-op host mode's readiness
	// probe (cmd/host.go's hostModeHealthy) hits INSTEAD of the public
	// /api/health above — proving the CONFIGURED BOID_CLI_TOKEN actually
	// works against the running daemon container, not merely that some
	// process answers on the port. /api/health alone cannot catch a token
	// mismatch: e.g. ~/.config/boid/cli-token deleted and regenerated on
	// the host while the container keeps running with the OLD value still
	// baked into its process environment — health would keep returning 200
	// forever while every real request 401s indefinitely. Deliberately NOT
	// added to auth.apiAuthRequired's exemption list, so both the CLI TCP
	// listener (auth.NewCLITokenAuthMiddleware) and the Web UI's TCP
	// listener (auth.NewTCPAPIAuthMiddleware) gate it exactly like any
	// other /api/* route — reusing the SAME auth machinery a readiness
	// probe needs to prove is actually working, rather than hand-rolling a
	// parallel token comparison here.
	r.Get("/api/cli-token-check", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
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

	// `boid secret oauth login <service>` (docs/plans/api-gateway.md §7,
	// PR3) — mounted only when runtime.oauthLogin is wired (buildRuntime's
	// own "feature's off switch is an absent dependency" note on that
	// field). A request against /api/oauth/* while this is unmounted 404s
	// at the router level, same as any other absent route.
	if runtime.oauthLogin != nil {
		oauthLoginHandler := &api.OAuthLoginHandler{Service: runtime.oauthLogin}
		r.Mount("/api/oauth", oauthLoginHandler.Routes())
	}

	sessionAdapter := &sessionDispatcherAdapter{service: runtime.projectSvc, runner: runtime.runner, packs: runtime.packs}
	// docs/plans/ingestion-identity.md PR-4 (B-5): the trigger sweep loop's
	// "実行" section — daemon dispatches a due trigger's exec job through
	// the SAME api.ExecDispatcher.StartExec `boid exec` uses. This can only
	// be wired here, not alongside workflow's other fields above
	// (buildRuntime), because sessionAdapter does not exist until this
	// point — see TaskWorkflowService.Exec's own doc comment.
	runtime.workflow.Exec = sessionAdapter
	projectHandler := &api.ProjectHandler{
		Service:           runtime.projectSvc,
		SessionDispatcher: sessionAdapter,
		ExecDispatcher:    sessionAdapter,
		// TriggerRunner backs 12 節「手動 1 巡の口」(`boid trigger run`,
		// cmd/trigger.go) — runtime.workflow satisfies api.TriggerRunner via
		// RunTriggerNow (trigger_loop.go).
		TriggerRunner: runtime.workflow,
	}
	r.Mount("/api/projects", projectHandler.Routes())

	// Wire up the periodic trigger sweep loop (docs/plans/
	// ingestion-identity.md PR-4, B-5's「スケジューラ」section — same
	// InitialDelay-then-Interval idiom as srv.queueSweepLoop (buildRuntime)
	// and srv.gcLoop (below), just constructed here instead because it
	// needs runtime.workflow.Exec, only just assigned above. InitialDelay
	// is staggered after gcLoop's 10s and queueSweepLoop's 20s so all three
	// startup sweeps don't contend for the single DB connection at once
	// (db.go's SetMaxOpenConns(1)).
	srv.triggerLoop = &api.TriggerLoop{
		Store: runtime.workflow,
		// N-6 (Opus review): tied to orchestrator.TriggerSweepResolution,
		// not a separate literal — ValidateTriggers rejects any `every`
		// below that same constant at project.yaml load time specifically
		// because it assumes this IS the sweep loop's actual Interval. If
		// the two drifted apart, ValidateTriggers' floor would stop
		// describing reality.
		Interval:     orchestrator.TriggerSweepResolution,
		InitialDelay: 30 * time.Second,
		Notifier:     srv.notifySvc,
	}

	sessionHandler := &api.SessionHandler{
		Service:    runtime.projectSvc,
		Dispatcher: sessionAdapter,
	}
	r.Mount("/api/sessions", sessionHandler.Routes())

	workspaceHandler := &api.WorkspaceHandler{
		Service: runtime.projectSvc,
		// Workspace HOME size reporting (Show) + deletion (Remove),
		// docs/plans/home-workspace-volume.md Phase 4 PR5, rewired onto the
		// engine's volume API by 論点 a-2 / PR7 of
		// docs/plans/workspace-home-volume-persistence.md. This deliberately
		// no longer takes runtimesRoot: the home is a named volume
		// (dockerres.WorkspaceHomeVolumeName) and there is no host path left
		// to resolve. nil here — no docker client — is the feature's off
		// switch, see api.WorkspaceHomeStore's doc comment.
		Homes: runtime.workspaceHomes,
		// PR8 of the same plan doc (論点 f): the migration of a pre-PR6 host
		// home directory into the workspace's HOME volume. The Runner is the
		// implementation because everything the migration has to keep
		// consistent is the Runner's — the completion marker under
		// <dataHome>/homes-meta/, the per-workspace init flock, and the
		// identity label it mints — see dispatcher.Runner.ImportWorkspaceHome's
		// D1 note for why this is not a CLI that drives the engine directly the
		// way `boid reap` does.
		//
		// Unlike Homes above, this does NOT gate on a docker client: the
		// capability is discovered on the BACKEND by type assertion
		// (dispatcher.WorkspaceHomeImporter), so a DI backend that implements it
		// serves the route and one that does not fails with a message naming
		// itself. Gating on the engine handle here would additionally turn off
		// the route for exactly the backends that can serve it in tests.
		HomeImporter: runtime.runner,
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
		WithRuntimesDir(runtimesRoot).
		WithTranscriptsDir(transcriptsRoot).
		WithSandboxTmpDir(os.TempDir()).
		WithRuntimeReaper(makeDockerRuntimeReaper()).
		WithAttachmentsRoot(dataHomeFor(srv.cfg))
	gcAppService := &api.GCAppService{Store: gcStore, DeviceStore: runtime.authStore}
	gcHandler := &api.GCHandler{
		Service: gcAppService,
		// workspace_homes size listing (docs/plans/home-workspace-volume.md
		// Phase 4 PR5, on the engine's volume API since 論点 a-2 / PR7 of
		// docs/plans/workspace-home-volume-persistence.md) — visibility only,
		// GC never deletes a workspace home itself (that's `workspace
		// remove`'s job). Workspaces flags orphan homes (a HOME volume with
		// no matching workspace row) in the listing.
		Homes:      runtime.workspaceHomes,
		Workspaces: runtime.projectSvc,
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

	// card read surface (docs/plans/cross-project-issue-triage.md Phase
	// 1 PR-5a; mount renamed from /api/triage by docs/plans/
	// card-model-cleanup.md PR-3 §4). Mounted at its own root rather than
	// under /api/tasks — see api.CardHandler's doc comment.
	r.Mount("/api/cards", (&api.CardHandler{Service: runtime.workflow}).Routes())

	// signal inbox read/ack surface (docs/plans/signal-ingest-detailed-
	// design.md §3.1, PR-2). taskRepo already implements api.SignalStore
	// (PR-1's var _ assertion, internal/api/store.go) — same "mount the
	// handler directly against taskRepo, no wrapper service" shape as
	// TaskTriage/Actions/Triggers above use runtime.workflow's fields for.
	r.Mount("/api/signals", (&api.SignalHandler{Store: runtime.taskRepo}).Routes())

	actionHandler := &api.ActionHandler{Service: runtime.workflow}
	r.Route("/api/tasks/{taskID}/actions", func(r chi.Router) {
		r.Mount("/", actionHandler.Routes())
	})

	jobHandler := &api.JobHandler{
		Jobs:    runtime.jobStore,
		Global:  runtime.globalJobStore,
		Service: runtime.workflow,
		// fallbackRootDir: runtimesRoot — see the boid_executor wiring's
		// identical comment above (codex review round 1).
		LogReader: transcriptLogReader{rootDir: transcriptsRoot, fallbackRootDir: runtimesRoot},
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
	// GET /auth only renders the confirmation page; POST /auth is what
	// spends the one-time pairing code (see LoginHandler.GetAuth for why the
	// redemption had to move off the GET).
	r.Get("/auth", loginHandler.GetAuth)
	r.Post("/auth", loginHandler.PostAuth)

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
	deviceAuthPublicURL, publicURLErr := apiwire.NormalizePublicURL(gcCfg.Web.PublicURL)
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
			// GET /settings (docs/plans/volume-only-daemon.md §論点 f):
			// srv itself satisfies api.SettingsConfigService directly
			// (ConfigYAML() already matches that shape — the same method
			// configHandler above uses), mirroring configHandler's own
			// `Service: srv` wiring just above.
			ConfigService: srv,
			// List row suggestion/summary enrichment (cross-project-issue-triage
			// Phase 1 PR-3; restated for the single flat list by docs/plans/
			// webui-detail-list-redesign.md PR-4 — the pre-PR-4 queue_next/
			// Parked tabs this originally backed are gone) — same taskRepo
			// instance already wired as TaskWorkflowService.TaskTriage above.
			TaskTriage: runtime.taskRepo,
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

	// The dedicated CLI TCP listener (Config.CLIAddr/CLIToken, PR-3 Option 4
	// host-mode redesign, docs/plans/volume-only-daemon.md §論点c) is served
	// the same router wrapped with a DIFFERENT auth layer: a single shared
	// BOID_CLI_TOKEN secret, not the Web UI's session/device-pair/loopback-
	// trust machinery — see auth.NewCLITokenAuthMiddleware's own doc
	// comment. Built unconditionally here (byte-for-byte cheap when
	// cfg.CLIToken==""; NewCLITokenAuthMiddleware rejects every request in
	// that case) — Server.Start only actually SERVES it when cfg.CLIAddr is
	// also set.
	srv.cliHandler = auth.NewCLITokenAuthMiddleware(srv.cfg.CLIToken)(r)
	return nil
}
