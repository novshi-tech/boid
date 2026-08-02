package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/config"
	"github.com/novshi-tech/boid/internal/daemon"
	"github.com/novshi-tech/boid/internal/selfuser"
	"github.com/novshi-tech/boid/internal/server"
	"github.com/spf13/cobra"
)

const (
	// defaultStartHTTPAddr binds the Web UI / HTTP API to loopback only. The
	// data/control API is auth-gated over TCP (see auth.NewTCPAPIAuthMiddleware),
	// but binding to loopback keeps it off other interfaces by default; expose
	// it deliberately with `boid web set-addr`. Cloudflare Tunnel connects to
	// 127.0.0.1 so the documented tunnel flow is unaffected.
	//
	// This is the bare-host default only — see defaultContainerStartHTTPAddr
	// below for why the compose daemon needs a different one (穴 8,
	// docs/plans/release-onboarding.md).
	defaultStartHTTPAddr = "127.0.0.1:8080"

	// defaultContainerStartHTTPAddr is the fallback used instead of
	// defaultStartHTTPAddr when running under build/container/compose.yml
	// (穴 8 (a), docs/plans/release-onboarding.md). A fresh boid_state
	// volume has no web.http_addr in config.yaml, so a bare `boid start`
	// fell back to defaultStartHTTPAddr's 127.0.0.1 — binding the
	// CONTAINER's own loopback, which compose.yml's port publish
	// (`"${BOID_WEB_PORT:-8080}:8080"`) cannot make reachable from the
	// host: port publish only forwards to a bound INTERFACE inside the
	// container, and 127.0.0.1 there is not one. The result was a fresh
	// install's `http://localhost:8080` hanging with nothing listening on
	// the host-visible side at all. Binding all interfaces inside the
	// container is safe here the same way it already is for the CLIAddr
	// listener (BOID_CLI_TOKEN-gated) and the data/control API
	// (auth.NewTCPAPIAuthMiddleware): the container's own network
	// namespace is not directly host-reachable except through compose's
	// explicit port publish and the job-isolated boid_internal network,
	// so "all interfaces inside the container" is not "all interfaces on
	// the host". Selected only when runningUnderComposeContainer() is
	// true — see that function's own doc comment for the detection
	// story (Blocker, codex round-6 review: BOID_LOG_STDOUT alone is not
	// a safe container marker on its own).
	defaultContainerStartHTTPAddr = "0.0.0.0:8080"
)

// containerSentinelPaths are files a container RUNTIME itself creates
// inside the container's filesystem — /.dockerenv for Docker,
// /run/.containerenv for Podman (build/container/compose.yml's daemon
// service can run under either) — as opposed to anything an operator's
// own environment/config could set. Presence of either is treated as
// "this process's root filesystem really is a container", independent of
// what any env var claims.
var containerSentinelPaths = []string{"/.dockerenv", "/run/.containerenv"}

// runningUnderComposeContainer reports whether this process is very
// likely the build/container/compose.yml daemon service specifically —
// used ONLY to pick buildStartConfig's HTTP bind fallback (穴 8 (a),
// docs/plans/release-onboarding.md), never for anything security-gating.
//
// Requires BOTH signals, not just one (Blocker, codex round-6 review of
// an earlier revision that used BOID_LOG_STDOUT alone):
//   - daemon.ShouldLogToStdout() (BOID_LOG_STDOUT=1): compose sets this,
//     but so could a bare-host systemd unit wanting the same "a
//     supervisor already owns my stdout/session lifecycle" behavior
//     (that flag's own doc comment) for reasons having nothing to do
//     with containers — on its own it is not a safe "flip the default
//     bind address to 0.0.0.0" trigger, since a host operator setting it
//     would unexpectedly expose a previously loopback-only listener to
//     every host interface.
//   - a container-runtime sentinel file (containerSentinelPaths): unlike
//     an env var, a bare-host process cannot set this — it is created by
//     the container runtime itself inside the container's OWN root
//     filesystem. (A prior codex review, internal/dispatcher/
//     daemon_state_volume.go's "Why this replaced the 'am I in a
//     container?' test" section, found a sentinel-file check alone
//     unreliable across runtimes for a DIFFERENT, security-relevant
//     decision — containerd/CRI and Kubernetes ship no such sentinel, so
//     a FALSE NEGATIVE there silently disabled a security protection.
//     The risk shape here is the opposite and much smaller: a false
//     negative just leaves this fallback at today's existing
//     loopback-only default, exactly the pre-PR6 status quo the
//     documented `boid config set web.http_addr` override already
//     handles — not a new failure mode.)
//
// Requiring both collapses the false-positive risk to needing an
// operator who is BOTH running under a real container runtime AND has
// manually exported BOID_LOG_STDOUT=1 for an unrelated reason — a
// combination compose's own daemon service already satisfies, but a bare
// host essentially cannot reach by accident.
func runningUnderComposeContainer() bool {
	if !daemon.ShouldLogToStdout() {
		return false
	}
	for _, p := range containerSentinelPaths {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the boid server",
	// Suppress cobra's auto-Usage block when RunE returns a non-nil error.
	// `boid start` errors (e.g. the migration block) are user-facing
	// remediation text and the usage dump just buries the actionable lines.
	SilenceUsage: true,
	// Suppress cobra's automatic `Error: <err>` line so main.go's stderr
	// print is the single source of the final error text. Otherwise
	// migration messages get duplicated (cobra + main both print).
	SilenceErrors: true,
	RunE:          runStart,
}

var (
	startDBPath      string
	startSocketPath  string
	startKitsDir     string
	startKeyFilePath string
	startCLIAddr     string
	startForeground  bool
)

func init() {
	startCmd.Annotations = map[string]string{
		annotationSkipAutostart: "skip",
		scopeAnnotationKey:      scopeLocal,
	}
	startCmd.Flags().StringVar(&startDBPath, "db-path", "", "Path to the SQLite database")
	startCmd.Flags().StringVar(&startSocketPath, "socket-path", "", "Path to the UNIX socket")
	startCmd.Flags().StringVar(&startKitsDir, "kits-dir", "", "Base directory for installed kits")
	startCmd.Flags().StringVar(&startKeyFilePath, "key-file-path", "", "Path to the secret encryption key file")
	startCmd.Flags().StringVar(&startCLIAddr, "cli-addr", "", "host:port for the dedicated CLI TCP listener (docs/plans/volume-only-daemon.md §論点c; only bound when BOID_CLI_TOKEN is also set; default: client.DefaultCLIAddr(), \"127.0.0.1:8442\")")
	// --auto-migrate (bare-metal double-fork respawn-after-migrate) is
	// removed as of docs/plans/release-onboarding.md 決定2/PR5 (codex
	// round-2 review Major 1): its only implementation, cmd/start_
	// automigrate.go's handleMigrationFailure, was driven exclusively by
	// the bare-metal parent/child fd3-status-pipe protocol
	// (runDaemonParent, removed in this same PR) — a mechanism with no
	// compose equivalent (a detached `compose up -d` container has no fd3
	// pipe back to this CLI process to read a structured startup failure
	// from at all), so there was no way to keep the flag wired to
	// anything. Silently accepting and ignoring it would have been worse
	// than removing it outright: an operator who typed --auto-migrate
	// expecting the old auto-respawn behavior deserves a clear "no such
	// flag" from cobra, not silent no-op behavior.
	startCmd.Flags().BoolVar(&startForeground, "foreground", false,
		"Run the daemon directly in this process, skipping the double-fork self-respawn — for a process supervisor (systemd Type=simple, a container entrypoint, ...) that already owns respawn/liveness. Equivalent to (and takes precedence over) setting BOID_DAEMON_CHILD=1, which remains supported for existing supervisor configs (build/container/compose.yml) that set the env var instead of passing this flag")
	rootCmd.AddCommand(startCmd)
}

// defaultAllowedDomains delegates to config.DefaultAllowedDomains — the
// literal itself moved there (BLOCKER 2 sibling fix, codex review round 1)
// so internal/server's config-hot-reload path can recompute the same floor
// without importing cmd. Kept here as a thin wrapper so buildStartConfig's
// call site below (and cmd/start_test.go) need no further change.
func defaultAllowedDomains() []string {
	return config.DefaultAllowedDomains()
}

func defaultDBPath() string {
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		home, _ := os.UserHomeDir()
		dataDir = filepath.Join(home, ".local", "share")
	}
	dir := filepath.Join(dataDir, "boid")
	_ = os.MkdirAll(dir, 0o755) // best-effort; a real failure surfaces at DB open
	return filepath.Join(dir, "boid.db")
}

func defaultKitsDir() string {
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		home, _ := os.UserHomeDir()
		dataDir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataDir, "boid", "kits")
}

func defaultKeyFilePath() string {
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		home, _ := os.UserHomeDir()
		dataDir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataDir, "boid", "secret.key")
}

// defaultTLSDir returns the directory holding (or to generate) the
// per-daemon internal CA used to secure the broker/git-gateway TCP(mTLS)
// listeners (docs/plans/phase6-container-backend.md §PR4/§決定5) — same
// XDG data-dir convention as the other default*Path helpers above, and the
// same "web_secret"-style file-in-data-dir layout the plan doc calls for
// (~/.local/share/boid/tls/ca.crt + ca.key).
func defaultTLSDir() string {
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		home, _ := os.UserHomeDir()
		dataDir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataDir, "boid", "tls")
}

// defaultInstallIDDir returns the directory holding (or to generate) this
// installation's plain-UUID install_id file (internal/install.LoadOrCreate
// — docs/plans/phase6-container-backend.md §PR6/§決定6). Same XDG data-dir
// convention as the other default*Path/Dir helpers above, and the same
// "web_secret"-style file-directly-in-data-dir layout §決定6 calls for
// (~/.local/share/boid/install_id, alongside boid.db and web_secret — NOT
// nested under its own subdirectory the way tls/ is, since it is a single
// file, not a CA's cert+key pair).
func defaultInstallIDDir() string {
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		home, _ := os.UserHomeDir()
		dataDir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataDir, "boid")
}

type startConfigOptions struct {
	DBPath      string
	SocketPath  string
	KitsDir     string
	KeyFilePath string
	// CLIAddr overrides the dedicated CLI TCP listener's bind address
	// (docs/plans/volume-only-daemon.md §論点c, PR-3 Option 4 host-mode
	// redesign) — empty falls back to client.DefaultCLIAddr() below,
	// mirroring every other opts.* field's "" -> default* helper pattern
	// in this function.
	CLIAddr string
}

func buildStartConfig(opts startConfigOptions) (server.Config, error) {
	cfg := server.Config{
		DBPath:         opts.DBPath,
		SocketPath:     opts.SocketPath,
		KitsDir:        opts.KitsDir,
		KeyFilePath:    opts.KeyFilePath,
		CLIAddr:        opts.CLIAddr,
		AllowedDomains: defaultAllowedDomains(),
	}

	if cfg.DBPath == "" {
		cfg.DBPath = defaultDBPath()
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = client.DefaultSocketPath()
	}
	if cfg.KitsDir == "" {
		cfg.KitsDir = defaultKitsDir()
	}
	if cfg.KeyFilePath == "" {
		cfg.KeyFilePath = defaultKeyFilePath()
	}
	if cfg.CLIAddr == "" {
		cfg.CLIAddr = client.DefaultCLIAddr()
	}
	// BOID_CLI_TOKEN (PR-3 Option 4 host-mode redesign): the shared secret
	// `boid`'s own host-mode orchestration (cmd/host.go) generates/reads
	// from ~/.config/boid/cli-token and passes to the daemon container via
	// build/container/compose.yml's `environment:` block. Empty for a bare
	// `boid start` invoked directly on the host (not through host mode) —
	// Server.Start skips binding the CLIAddr listener whenever CLIToken is
	// empty (Config.CLIAddr's own doc comment), so this is a pure no-op
	// there, not a behavior change.
	cfg.CLIToken = os.Getenv("BOID_CLI_TOKEN")
	cfg.TLSDir = defaultTLSDir()
	cfg.InstallIDDir = defaultInstallIDDir()

	appCfg, err := config.Load()
	if err != nil {
		return server.Config{}, fmt.Errorf("load config: %w", err)
	}
	cfg.HTTPAddr = appCfg.Web.HTTPAddr
	if cfg.HTTPAddr == "" {
		// 穴 8 (a): default to 0.0.0.0 only under compose's OWN container,
		// never on a bare host — see runningUnderComposeContainer's own
		// doc comment for why a single env var was not enough (Blocker,
		// codex round-6 review).
		if runningUnderComposeContainer() {
			cfg.HTTPAddr = defaultContainerStartHTTPAddr
		} else {
			cfg.HTTPAddr = defaultStartHTTPAddr
		}
	}
	cfg.AllowedDomains = append(cfg.AllowedDomains, appCfg.Sandbox.AllowedDomains...)
	cfg.LogLevel = appCfg.Log.Level

	return cfg, nil
}

// refuseRootUID rejects uid 0 (codex review of PR2, Blocker): the
// arbitrary-uid redesign (docs/plans/release-onboarding.md 決定1) makes
// gid 0 + `g=u` permissions do the work non-root uids used to need a
// fixed, baked passwd entry for — but nothing before this check stopped
// compose's `user: "${BOID_UID:-1000}:0"` from resolving to "0:0" if an
// operator sets BOID_UID=0, or scripts/deploy-container.sh's
// `: "${BOID_UID:=$(id -u)}"` from picking up uid 0 when the deploy
// script itself is run as root. internal/dispatcher.NewContainerBackend's
// own uid-0 guard (§決定4) only ever protected the JOB containers this
// daemon creates — a root DAEMON would still pass os.Getuid()==0 through
// internal/server/wire.go's sandboxBackendForConfig, tripping that
// guard's "reject and fall back to 1000:0" path and silently mismatching
// the workspace HOME ownership §決定4's own wire.go comment describes (a
// HOME the root daemon created is 0700-owned by uid 0, unreadable to a
// 1000:0 job container). Refusing outright, here, is cheaper and clearer
// than letting that mismatch surface later as a confusing per-job
// permission error.
//
// Takes the uid as a plain int (rather than calling os.Getuid() itself)
// so a test can exercise both branches without needing to actually run
// this process as root, and because which uid is "the one that matters"
// differs by call site (codex round-9 review of PR5, Major): on the
// --foreground/daemon-child path THIS process is (or becomes) the
// daemon, so callers must pass os.Getuid() — BOID_UID has no bearing on
// what uid this process runs as there, since compose.yml's `user:`
// substitution is never consulted on that branch at all. On the
// non-foreground "just run compose up" path, callers must pass
// effectiveBoidUID() instead, since BOID_UID (when set) is what actually
// ends up substituted into compose.yml's `user: "${BOID_UID}:0"`. See
// runStart's own call sites for exactly where each applies.
func refuseRootUID(uid int) error {
	if uid != 0 {
		return nil
	}
	return fmt.Errorf("boid start: refusing to run as uid 0 (root) — §決定1/§決定4 (docs/plans/release-onboarding.md) require a non-root uid with supplementary group 0; set BOID_UID to a non-zero uid (build/container/compose.yml's `user: \"${BOID_UID}:0\"`) or invoke this process as a non-root user")
}

// effectiveBoidUID returns the uid that will actually end up running the
// compose daemon CONTAINER on the non-foreground "just run compose up"
// path: BOID_UID, when set (docs/plans/release-onboarding.md — scripts/
// deploy-container.sh's own `: "${BOID_UID:=$(id -u)}"` default —
// compose's `user: "${BOID_UID:-1000}:0"` substitutes it at compose-parse
// time on the HOST, not as a container environment variable, so an
// operator exporting BOID_UID=0 changes what uid the CONTAINER runs as
// without changing THIS PROCESS's own os.Getuid() at all), or
// os.Getuid() otherwise.
//
// codex round-7 review of PR5, Major: refuseRootUID(os.Getuid()) alone
// only ever inspected the CLI process's own uid — a non-root operator
// running `BOID_UID=0 boid start` sailed straight through it, since the
// check had nothing to do with the value BOID_UID ultimately resolves
// to. scripts/deploy-container.sh gained an equivalent guard at the
// actual enforcement point in an earlier round of this same review; this
// closes the matching gap on the Go CLI side so the refusal fires as
// early as possible too, before ever invoking that script.
//
// NOT safe to reuse for the --foreground/daemon-child path (codex
// round-9 review of PR5, Major, reverting an earlier round's claim to
// the contrary): that path's caller must pass refuseRootUID(os.Getuid())
// directly instead, because there THIS process itself is (or becomes)
// the daemon and compose.yml is never consulted at all — checking
// effectiveBoidUID() there let a root operator bypass the refusal
// entirely via `BOID_UID=1000 boid start --foreground`, since BOID_UID
// has no bearing on what uid this process actually runs as on that
// branch. See refuseRootUID's own doc comment and runStart's call sites.
//
// An unparseable BOID_UID is not this function's problem to diagnose —
// it returns os.Getuid() unchanged in that case, and compose/docker will
// surface their own clear error against the malformed value when they
// try to use it.
func effectiveBoidUID() int {
	if v := os.Getenv("BOID_UID"); v != "" {
		if uid, err := strconv.Atoi(v); err == nil {
			return uid
		}
	}
	return os.Getuid()
}

// runStart implements `boid start`. docs/plans/release-onboarding.md
// 決定2/PR5 redefines the ordinary invocation (an operator typing
// `boid start` on their own host) to mean "bring the compose daemon stack
// up" — compose is the only daemon shape boid supports now, so there is
// no bare-metal daemon left for a plain `boid start` to spawn directly.
//
// The foreground/daemon-child carve-out (shouldRunForeground) is the one
// path this redefinition must NOT touch (docs/plans/
// release-onboarding.md「決定 2 の事前確認」, PR5's stated must-have):
// build/container/compose.yml's OWN daemon service entrypoint is
// `["/usr/local/bin/boid"]` + `command: ["start"]` with
// BOID_DAEMON_CHILD=1 set — that invocation, running INSIDE the
// container, must become the real daemon process (runDaemonChild)
// exactly as before. If it instead fell into the compose-up branch
// below, the compose daemon's own `boid start` would recurse into
// trying to `compose up` itself from inside its own already-running
// container.
func runStart(cmd *cobra.Command, args []string) error {
	// codex round-9 review of PR5, Major: on the --foreground/daemon-child
	// path this process itself IS (or becomes) the daemon — no compose
	// substitution happens, so the uid that actually matters is THIS
	// process's own os.Getuid(), not effectiveBoidUID(). Checking
	// effectiveBoidUID() here let a root operator bypass the refusal with
	// `BOID_UID=1000 boid start --foreground`: BOID_UID has no bearing on
	// what uid this process runs as outside of compose's own `user:`
	// substitution (compose.yml is never consulted on this branch at
	// all), so the check would pass while the daemon kept running as root.
	if shouldRunForeground(startForeground) {
		if err := refuseRootUID(os.Getuid()); err != nil {
			return err
		}
		// Arbitrary-uid self-registration + group-writable umask
		// (docs/plans/release-onboarding.md 決定1, PR2): under the
		// compose daemon service this process's uid may have no
		// /etc/passwd entry at all (the image no longer bakes one —
		// build/container/Dockerfile), and runtime-generated files
		// (secret.key, boid.db, ...) need the group-write bit so a
		// later run under a DIFFERENT uid (still group 0) stays working
		// (§決定1 実装形2, 未決6's "uid is fixed per install" contract).
		// Both calls are best-effort/non-fatal and equally harmless for
		// a --foreground supervisor on bare metal, where the running
		// uid almost always already has a real passwd entry (a no-op)
		// — see their own doc comments.
		selfuser.EnsureRuntimeUserRegistered()
		selfuser.ApplyGroupWritableUmask()

		cfg, err := buildStartConfig(startConfigOptions{
			DBPath:      startDBPath,
			SocketPath:  startSocketPath,
			KitsDir:     startKitsDir,
			KeyFilePath: startKeyFilePath,
			CLIAddr:     startCLIAddr,
		})
		if err != nil {
			return err
		}
		return runDaemonChild(cfg)
	}

	// codex round-3 review of PR5, Major 2: --db-path/--socket-path/
	// --kits-dir/--key-file-path/--cli-addr only ever configure the
	// ACTUAL daemon process (buildStartConfig, read above only on the
	// --foreground/daemon-child branch) — the compose stack's own
	// daemon config comes from build/container/compose.yml's own env
	// wiring instead, which this non-foreground "just run compose up"
	// path has no way to forward these flags into at all. Silently
	// accepting and ignoring them would be actively misleading: a script
	// that used to pass e.g. --db-path against a bare-metal `boid start`
	// would now appear to succeed while operating on a compose stack
	// that never saw that flag, silently touching the WRONG database.
	// Fail loudly instead.
	if err := refuseDaemonConfigFlagsWithoutForeground(cmd); err != nil {
		return err
	}

	// codex round-7 review of PR5, Major (re-scoped by round-9): on this
	// non-foreground "just run compose up" path, scripts/
	// deploy-container.sh's `: "${BOID_UID:=$(id -u)}"` defaults BOID_UID
	// to whatever uid ran `boid start` when it is not set explicitly, but
	// an operator MAY set BOID_UID explicitly to something other than
	// their own uid — effectiveBoidUID() (not os.Getuid()) is the value
	// that actually ends up substituted into compose.yml's
	// `user: "${BOID_UID}:0"`, so it is the one this check must inspect
	// on this branch to rule out §決定1/§決定4's root-daemon outcome.
	if err := refuseRootUID(effectiveBoidUID()); err != nil {
		return err
	}

	return runComposeUp(cmd.Context(), client.DefaultCLIAddr())
}

// refuseDaemonConfigFlagsWithoutForeground reports an error naming every
// explicitly-set daemon-process-config flag when startForeground is
// false — see runStart's own call site for the full rationale (codex
// round-3 review of PR5, Major 2).
func refuseDaemonConfigFlagsWithoutForeground(cmd *cobra.Command) error {
	var set []string
	for _, name := range []string{"db-path", "socket-path", "kits-dir", "key-file-path", "cli-addr"} {
		if f := cmd.Flags().Lookup(name); f != nil && f.Changed {
			set = append(set, "--"+name)
		}
	}
	if len(set) == 0 {
		return nil
	}
	return fmt.Errorf(
		"boid start: %s only configure the actual daemon process (compose.yml's own env wiring does that for the compose stack) and have no effect on a plain `boid start` (which now just runs compose up) — pass --foreground (or set BOID_DAEMON_CHILD=1) if you are directly supervising the daemon process yourself, or drop %s and rely on compose.yml's config instead",
		strings.Join(set, ", "), strings.Join(set, ", "))
}

// shouldRunForeground reports whether runStart should skip the
// parent/child double-fork and run the daemon directly in the current
// process (Major 6, PR6 codex review): the pre-fix code only ever checked
// daemon.IsChild() (BOID_DAEMON_CHILD=1), which build/container/compose.yml
// sets but a bare userns process supervisor (systemd Type=simple, runit,
// ...) generally has no reason to know about or set — so a userns startup
// under a supervisor still double-forked, the supervisor's tracked process
// exited once the parent confirmed the detached child's socket was up, and
// the supervisor treated that exit as a crash and restart-looped.
//
// --foreground (the foregroundFlag arg, wired from startForeground) is the
// primary, discoverable seam for this: any supervisor config, not just
// compose's, can pass it. daemon.IsChild() (BOID_DAEMON_CHILD=1) remains
// supported so compose.yml's existing env-var-based config keeps working
// unchanged — both routes converge on the exact same runDaemonChild call,
// so there is exactly one foreground code path for any caller to reach.
func shouldRunForeground(foregroundFlag bool) bool {
	return foregroundFlag || daemon.IsChild()
}

// runComposeUp is `boid start`'s non-foreground path (docs/plans/
// release-onboarding.md 決定2/PR5): bring the compose daemon stack up.
// Shares cmd/host.go's on-demand daemon-lifecycle machinery
// (loadOrCreateCLIToken, findComposeRoot/deployFromCheckout/
// deployFromEmbeddedAssets, withHostModeLock) rather than reimplementing
// it — a scope=remote command that happens to need the daemon running
// (e.g. `boid task list`) and an explicit `boid start` both ultimately
// need "the compose stack is up, at this addr, authenticated with this
// token" and should agree on exactly how that gets accomplished.
//
// Deliberately unconditional, unlike host mode's ensureHostModeDaemon
// (which short-circuits when the stack already answers healthy, since it
// is a lazy autostart hook riding along on an unrelated command): `boid
// start` is itself the explicit "make sure this is running" request, so
// it always runs the deploy script's own down+up cycle — a no-op-ish
// `compose up -d` against an already-current stack, or a real rebuild/
// restart when local code or BOID_IMAGE moved on. It also does not
// consult client.NoAutostartEnv (BOID_NO_AUTOSTART) — that knob exists to
// stop an UNRELATED command from silently spinning up a daemon as a
// side effect, not to make `boid start` itself a no-op.
//
// addr takes the CLI listener address as a parameter (production always
// passes client.DefaultCLIAddr(), the fixed "127.0.0.1:8442" — see that
// function's own doc comment for why it isn't independently configurable)
// rather than calling client.DefaultCLIAddr() internally, mirroring
// ensureHostModeDaemon's identical parameterization in cmd/host.go: a test
// can point it at an httptest server instead of the real fixed port.
func runComposeUp(ctx context.Context, addr string) error {
	token, err := loadOrCreateCLIToken()
	if err != nil {
		return fmt.Errorf("boid start: %w", err)
	}

	err = withHostModeLock(func() error {
		if root, ferr := findComposeRoot(); ferr == nil {
			return deployFromCheckout(ctx, root, token, addr)
		}
		return deployFromEmbeddedAssets(ctx, token, addr)
	})
	if err != nil {
		return fmt.Errorf("boid start: %w", err)
	}
	fmt.Printf("boid server started (compose, cli: http://%s)\n", addr)
	return nil
}

// The bare-metal double-fork daemon spawn (parent/child fd3-status-pipe
// protocol, migration-retry-on-respawn loop) that used to live here —
// runDaemonParent/spawnAndWaitForStartup/formatNonMigrationFailure — is
// removed as of docs/plans/release-onboarding.md 決定2/PR5: compose is the
// only daemon shape now, and runStart's non-foreground branch calls
// runComposeUp instead. cmd/start_automigrate.go (handleMigrationFailure
// and its helpers) is removed in the SAME PR (codex round-2 review Major
// 1) rather than left behind as unreachable dead code with a broken
// guidance string: its entire mechanism depended on reading a structured
// startup-failure status back from a bare-metal-forked child over fd3 — a
// pipe a detached `compose up -d` container has no equivalent of at all,
// so there was no way to keep it wired to anything once runDaemonParent
// (its sole caller) was gone. There is no fully automated or fully
// general by-hand recovery path yet for a schema issue a compose daemon
// reports at startup (internal/orchestrator/spec_loader.go's
// migrationGuidance and cmd/project_migrate.go's guardApply, both
// updated across several review rounds of this PR, explain the current,
// deliberately non-prescriptive guidance and its known limitations — see
// docs/ja/guide/migration.md). If a future PR wants a `boid start
// --bare-metal` escape hatch back, git history has the removed
// implementation (this same file, pre-PR5).

// runDaemonChild is executed by the daemon child process (BOID_DAEMON_CHILD=1).
// It redirects stdin/stdout/stderr to the log file and detaches from the
// session via Setsid (unless daemon.ShouldLogToStdout() opts both out —
// PR9, container-mode caller), then runs the server until a termination
// signal arrives.
//
// On any startup failure the child writes a structured StartupStatus to
// fd 3 (the side-channel pipe set up by daemon.Spawn) before returning,
// so the parent can render a useful message or drive auto-migration. On
// successful startup the child closes fd 3 instead, signalling EOF to the
// parent. After srv.Start() returns, fd 3 is no longer touched.
func runDaemonChild(cfg server.Config) error {
	// BOID_LOG_STDOUT (PR9, daemon.ShouldLogToStdout's own doc comment):
	// skip both the log-file redirect and Setsid entirely when a
	// supervisor already owns stdout capture and process/session
	// lifecycle (build/container/compose.yml's daemon service) — every
	// other caller (bare `boid start`, no supervisor already doing this)
	// keeps today's redirect-to-rotating-file + Setsid behavior unchanged.
	// Setsid specifically is not just skippable but actively WRONG to
	// call here: it fails with EPERM when the caller is already a process
	// group leader — true of a container's entrypoint process (tini's
	// direct child) — which is docs/plans/phase6-cutover-followups.md's
	// actual root cause for the e2e-container job's original startup
	// crash (see logStdoutEnvKey's doc comment for the full story).
	if !daemon.ShouldLogToStdout() {
		logPath := daemon.LogFilePath()
		if err := daemon.RedirectToLogRotating(logPath); err != nil {
			daemon.WriteStartupStatusOnFD3(err)
			return fmt.Errorf("redirect to log: %w", err)
		}

		if _, err := syscall.Setsid(); err != nil {
			daemon.WriteStartupStatusOnFD3(err)
			return fmt.Errorf("setsid: %w", err)
		}
	}

	// log.level (docs/ja/reference/config-yaml.md "log" section): applied
	// unconditionally, regardless of ShouldLogToStdout()'s branch above —
	// the level gates what slog emits either way, whether the destination
	// is the rotating log file or a container runtime's own stdout capture.
	// Deliberately the first slog-affecting call WITHIN this function
	// (before the CLIToken warning below, which itself calls slog.Warn), so
	// everything runDaemonChild itself logs from here on observes the
	// configured level. Not quite "the first thing this whole process ever
	// logs", though: buildStartConfig's own config.Load() call (runStart,
	// just above this function on the call stack, same process) can already
	// have emitted a couple of slog.Warn lines of its own (deprecated
	// sandbox.backend/gateway.hosts config, internal/config/config.go) —
	// see ApplyLogLevel's own doc comment for why that gap is cosmetic, not
	// a correctness concern.
	daemon.ApplyLogLevel(cfg.LogLevel)

	// BOID_CLI_TOKEN misconfiguration warning (PR-3 Option 4 host-mode
	// redesign, docs/plans/volume-only-daemon.md §論点c): daemon.
	// ShouldLogToStdout() is the exact same "are we running as the
	// compose/container daemon" signal BOID_LOG_STDOUT's own block above
	// already uses — reused here rather than inventing a second env var.
	// A bare `boid start` on the host (ShouldLogToStdout()==false) never
	// warns: cfg.CLIToken is expected to be empty there (host mode's own
	// dedicated CLI listener has no reason to exist outside the container
	// deployment its BOID_CLI_TOKEN passthrough targets), and
	// Server.Start already no-ops the listener bind in that case — see
	// Config.CLIAddr's own doc comment. Under the container supervisor,
	// though, an unset token silently leaves host-mode CLI dispatch
	// unreachable (no listener bound at all) with no other symptom until
	// someone notices `boid` commands hang waiting on a connection that
	// never completes — surfaced loudly here instead.
	if cfg.CLIToken == "" && daemon.ShouldLogToStdout() {
		slog.Warn("BOID_CLI_TOKEN is unset; the dedicated CLI TCP listener will not be bound — host-mode `boid` CLI dispatch (cmd/host.go) will not be able to reach this daemon (docs/plans/volume-only-daemon.md §論点c)")
	}

	srv, err := server.New(cfg)
	if err != nil {
		daemon.WriteStartupStatusOnFD3(err)
		return fmt.Errorf("create server: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		daemon.WriteStartupStatusOnFD3(err)
		return fmt.Errorf("start server: %w", err)
	}

	// Startup succeeded. EOF on the parent's read-end means OK; do not
	// touch fd 3 after this point.
	daemon.CloseStartupFD3()

	slog.Info("boid server started", "socket", cfg.SocketPath, "http", cfg.HTTPAddr)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	slog.Info("shutting down...")
	return srv.Stop()
}
