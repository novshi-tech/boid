package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/config"
	"github.com/novshi-tech/boid/internal/daemon"
	"github.com/novshi-tech/boid/internal/mtls"
	"github.com/novshi-tech/boid/internal/profiles"
	"github.com/novshi-tech/boid/internal/server"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

const (
	// defaultStartHTTPAddr binds the Web UI / HTTP API to loopback only. The
	// data/control API is auth-gated over TCP (see auth.NewTCPAPIAuthMiddleware),
	// but binding to loopback keeps it off other interfaces by default; expose
	// it deliberately with `boid web set-addr`. Cloudflare Tunnel connects to
	// 127.0.0.1 so the documented tunnel flow is unaffected.
	defaultStartHTTPAddr = "127.0.0.1:8080"
	daemonSocketTimeout  = 10 * time.Second
)

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
	startDBPath          string
	startSocketPath      string
	startKitsDir         string
	startKeyFilePath     string
	startCLIAddr         string
	startAutoMigrate     bool
	startForeground      bool
	startPrintCLIProfile bool
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
	startCmd.Flags().StringVar(&startCLIAddr, "cli-addr", "", "host:port for the dedicated CLI TCP(TLS) listener (docs/plans/volume-only-daemon.md §論点c; default: client.DefaultCLIAddr(), \"127.0.0.1:8442\")")
	startCmd.Flags().BoolVar(&startAutoMigrate, "auto-migrate", false,
		"When project.yaml schema migration is needed, run `boid project migrate <dir> --apply` for each affected project automatically and respawn the daemon (skips the confirmation prompt on TTY too)")
	startCmd.Flags().BoolVar(&startForeground, "foreground", false,
		"Run the daemon directly in this process, skipping the double-fork self-respawn — for a process supervisor (systemd Type=simple, a container entrypoint, ...) that already owns respawn/liveness. Equivalent to (and takes precedence over) setting BOID_DAEMON_CHILD=1, which remains supported for existing supervisor configs (build/container/compose.yml) that set the env var instead of passing this flag")
	startCmd.Flags().BoolVar(&startPrintCLIProfile, "print-cli-profile", false,
		"Print a bootstrap CLI connection profile (URL + daemon CA cert, docs/plans/volume-only-daemon.md §論点c) as YAML to stdout and exit immediately — does NOT start the daemon. For scripts/deploy-container.sh to seed the host's ~/.config/boid/config.yaml after the compose daemon has booted (a bare 'boid start' writes the equivalent bootstrap file itself on every boot — see client.DefaultCACertPath — so this flag exists mainly for the compose deployment, whose filesystem the host cannot see directly).")
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
	// CLIAddr overrides the dedicated CLI TCP(TLS) listener's bind address
	// (docs/plans/volume-only-daemon.md §論点c) — empty falls back to
	// client.DefaultCLIAddr() below, mirroring every other opts.* field's
	// "" -> default* helper pattern in this function.
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
	cfg.TLSDir = defaultTLSDir()
	cfg.InstallIDDir = defaultInstallIDDir()

	appCfg, err := config.Load()
	if err != nil {
		return server.Config{}, fmt.Errorf("load config: %w", err)
	}
	cfg.HTTPAddr = appCfg.Web.HTTPAddr
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = defaultStartHTTPAddr
	}
	cfg.AllowedDomains = append(cfg.AllowedDomains, appCfg.Sandbox.AllowedDomains...)

	return cfg, nil
}

func runStart(cmd *cobra.Command, args []string) error {
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

	if startPrintCLIProfile {
		return printCLIProfile(cmd, cfg)
	}

	if shouldRunForeground(startForeground) {
		return runDaemonChild(cfg)
	}
	return runDaemonParent(cfg)
}

// bootstrapProfileName is the profile name `--print-cli-profile` writes
// under (docs/plans/volume-only-daemon.md §論点c) — matches the
// conventional "default" name a fresh install's config.yaml would use for
// its one and only profile, so `default_profile: default` reads naturally
// alongside it in the printed YAML.
const bootstrapProfileName = "default"

// printCLIProfile implements `boid start --print-cli-profile`
// (docs/plans/volume-only-daemon.md §論点c "profile bootstrapping"): loads
// (or, on a truly first-ever invocation, generates — mtls.LoadOrCreate is
// idempotent, the exact same call Server.New itself makes) the daemon's
// internal CA from cfg.TLSDir, and prints a profiles.Config-shaped YAML
// document naming a "default" profile at cfg.CLIAddr with the CA's own
// certificate embedded inline (profiles.Profile.CACert) — everything a
// host's ~/.config/boid/config.yaml needs to reach this daemon's dedicated
// CLI TLS listener with no `boid login`/device-pair step (loopback trust,
// web.loopback_trust default true).
//
// This does NOT start the daemon, bind any listener, or touch config.yaml
// itself — it only prints to stdout. scripts/deploy-container.sh is the
// intended caller for the compose deployment (`docker compose exec daemon
// boid start --print-cli-profile`, run once after the daemon container has
// booted, so cfg.TLSDir already has a CA to load rather than this call
// racing the daemon's own first-boot generation): the compose daemon's
// filesystem is a named volume the host cannot see directly, so unlike a
// bare `boid start` (which writes client.DefaultCACertPath() straight to
// the host's own ~/.config/boid — see runDaemonChild), there is no other
// way for the host's config.yaml to learn this CA's certificate.
func printCLIProfile(cmd *cobra.Command, cfg server.Config) error {
	ca, err := mtls.LoadOrCreate(cfg.TLSDir)
	if err != nil {
		return fmt.Errorf("load or create tls ca: %w", err)
	}

	doc := profiles.Config{
		DefaultProfile: bootstrapProfileName,
		Profiles: map[string]profiles.Profile{
			bootstrapProfileName: {
				URL:    (&url.URL{Scheme: "https", Host: cfg.CLIAddr}).String(),
				CACert: string(ca.CertPEM()),
			},
		},
	}
	data, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("marshal cli profile: %w", err)
	}
	_, err = cmd.OutOrStdout().Write(data)
	return err
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

// printBareStartDeprecationNotice is a documentation-only nudge (Phase 6
// PR9 deprecation skeleton, docs/plans/phase6-cutover-followups.md §「host
// daemon 起動経路撤去」) printed after a successful bare `boid start`
// double-fork (runDaemonParent's own success branch — never on the
// --foreground/BOID_DAEMON_CHILD path a supervisor like build/container/
// compose.yml's `daemon` service uses, which has already opted into the
// newer deployment shape). It changes nothing about startup itself: no
// behavior, exit code, or return value is affected — this is purely
// informational, matching the plan doc's explicit "実撤去は禁止...
// deprecation warning は追加可" scope for PR9. The bare host-daemon +
// userns deployment model remains fully supported until the followups
// doc's own retirement PRs actually land.
func printBareStartDeprecationNotice() {
	fmt.Println("note: this bare `boid start` uses the host daemon + userns deployment model.")
	fmt.Println("      the container backend (`sandbox.backend: container` + `scripts/deploy-container.sh`,")
	fmt.Println("      docs/plans/phase6-container-backend.md) is now the recommended deployment path;")
	fmt.Println("      see docs/plans/phase6-cutover-followups.md for the retirement timeline.")
}

// runDaemonParent spawns the daemon child and waits on three concurrent
// signals via a select loop:
//
//  1. socket up         — daemon listening, startup succeeded
//  2. fd 3 status pipe  — EOF (= success) or structured JSON (= failure)
//  3. child liveness    — child exited without writing fd 3 (crash)
//
// On structured migration failure, the parent invokes
// handleMigrationFailure which (subject to --auto-migrate or TTY prompt)
// runs `boid project migrate <dir> --apply` in-process for each project
// and respawns the daemon at most once. On any other failure (or if
// migrate auto-resolution declines or fails), the cause is surfaced
// directly to the user — no boid.log grep needed.
func runDaemonParent(cfg server.Config) error {
	// 既存サーバが生きていれば二重起動を拒否する。socket ファイルが残って
	// いるだけ (ECONNREFUSED) の場合は stale とみなし、子プロセスに clean up
	// を任せる。
	if daemon.IsSocketAlive(cfg.SocketPath, 500*time.Millisecond) {
		return fmt.Errorf("boid server already running (socket: %s)", cfg.SocketPath)
	}

	logPath := daemon.LogFilePath()
	retries := 0
	for {
		result, err := spawnAndWaitForStartup(cfg, logPath)
		if err != nil {
			return err
		}
		if result.success {
			fmt.Printf("boid server started (pid: %d, socket: %s, http: %s)\n",
				result.pid, cfg.SocketPath, cfg.HTTPAddr)
			printBareStartDeprecationNotice()
			return nil
		}
		// userErr non-nil = no structured status; report and exit.
		if result.userErr != nil {
			return result.userErr
		}
		// Structured failure; branch on kind.
		if result.status.Kind != daemon.StartupKindMigration {
			return formatNonMigrationFailure(result.status, logPath)
		}
		// Migration failure → try to auto-resolve.
		retry, herr := handleMigrationFailure(
			os.Stderr,
			os.Stdin,
			result.status,
			logPath,
			startAutoMigrate,
			term.IsTerminal(int(os.Stdin.Fd())),
			defaultMigratePrompter,
			MigrateProject,
		)
		if !retry {
			return herr
		}
		if retries >= 1 {
			return fmt.Errorf("daemon still failing after auto-migrate; check logs at %s", logPath)
		}
		retries++
		// Loop back to respawn.
	}
}

// startupResult is the outcome of one spawn → wait cycle.
type startupResult struct {
	success bool
	pid     int
	status  *daemon.StartupStatus // non-nil when the child wrote fd 3
	userErr error                 // non-nil for environment-level failures (timeout, crash without status)
}

// spawnAndWaitForStartup spawns the daemon child and runs the four-way
// select on socket / status / liveness / outer-timeout. Returns once one
// of the wait paths resolves; cleans up its goroutines via the deferred
// context cancel and statusR close.
func spawnAndWaitForStartup(cfg server.Config, logPath string) (*startupResult, error) {
	pid, statusR, err := daemon.Spawn(os.Args)
	if err != nil {
		return nil, fmt.Errorf("spawn daemon: %w", err)
	}
	defer statusR.Close()

	// Channel for socket-readiness polling.
	socketCh := make(chan error, 1)
	go func() {
		socketCh <- daemon.WaitForSocket(cfg.SocketPath, daemonSocketTimeout)
	}()

	// Channel for the structured startup status (fd 3 pipe).
	type statusResult struct {
		status *daemon.StartupStatus
		err    error
	}
	resCh := make(chan statusResult, 1)
	go func() {
		s, err := daemon.ReadStartupStatus(statusR)
		switch {
		case errors.Is(err, daemon.ErrStartupOK):
			resCh <- statusResult{}
		case err != nil:
			resCh <- statusResult{err: err}
		default:
			resCh <- statusResult{status: s}
		}
	}()

	// Liveness probe: kill(pid, 0) returns ESRCH once the child exits.
	livenessCtx, livenessCancel := context.WithCancel(context.Background())
	defer livenessCancel()
	deadCh := make(chan struct{}, 1)
	go func() {
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-livenessCtx.Done():
				return
			case <-t.C:
				proc, err := os.FindProcess(pid)
				if err != nil {
					deadCh <- struct{}{}
					return
				}
				if err := proc.Signal(syscall.Signal(0)); err != nil {
					deadCh <- struct{}{}
					return
				}
			}
		}
	}()

	// Outer timeout — backstop in case all three signals stall.
	timeoutCh := time.After(daemonSocketTimeout + 5*time.Second)

	for socketCh != nil || resCh != nil {
		select {
		case err := <-socketCh:
			socketCh = nil
			if err == nil {
				return &startupResult{success: true, pid: pid}, nil
			}
			// socket polling timed out; wait for status/dead to give a
			// more specific cause.
		case res := <-resCh:
			resCh = nil
			if res.err != nil {
				return &startupResult{
					pid: pid,
					userErr: fmt.Errorf("daemon startup status decode failed: %w (logs: %s)",
						res.err, logPath),
				}, nil
			}
			if res.status != nil {
				return &startupResult{pid: pid, status: res.status}, nil
			}
			// EOF without payload → success; keep waiting on socket.
		case <-deadCh:
			return &startupResult{
				pid: pid,
				userErr: fmt.Errorf("daemon process exited unexpectedly (pid: %d); check logs at %s",
					pid, logPath),
			}, nil
		case <-timeoutCh:
			return &startupResult{
				pid: pid,
				userErr: fmt.Errorf("daemon did not start within %s (pid: %d); check logs at %s",
					daemonSocketTimeout+5*time.Second, pid, logPath),
			}, nil
		}
	}

	return &startupResult{
		pid: pid,
		userErr: fmt.Errorf("daemon startup completed but socket %s never became reachable; check logs at %s",
			cfg.SocketPath, logPath),
	}, nil
}

// formatNonMigrationFailure renders a user-facing message for structured
// startup failures that are NOT migration-related. Migration failures go
// through handleMigrationFailure instead.
func formatNonMigrationFailure(s *daemon.StartupStatus, logPath string) error {
	switch s.Kind {
	case daemon.StartupKindOther:
		if s.Message == "" {
			return fmt.Errorf("daemon startup failed (no detail); check logs at %s", logPath)
		}
		return fmt.Errorf("daemon startup failed: %s\nFull log: %s", s.Message, logPath)
	default:
		return fmt.Errorf("daemon reported unknown startup status kind %q; check logs at %s",
			s.Kind, logPath)
	}
}

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

	// Bootstrap CA-trust file (docs/plans/volume-only-daemon.md §論点c): a
	// bare `boid start` runs directly on the host, so — unlike the compose
	// deployment (whose filesystem is a named volume the host cannot see,
	// requiring the separate `boid start --print-cli-profile` +
	// deploy-container.sh seeding flow) — it can just write the daemon's CA
	// cert straight to client.DefaultCACertPath() itself, on every boot.
	// This is what makes the CLI's TCP terminal fallback
	// (profiles.ResolveWithoutToken's SourceTCPFallback) work out of the
	// box against a bare host daemon with no `boid start --print-cli-profile`
	// step at all. Idempotent and safe to overwrite unconditionally — a CA
	// certificate is public, non-secret material (srv.GatewayCAPEM() is the
	// exact same CA the dedicated CLI TLS listener's own cert is issued
	// from, reused here rather than duplicating a second load of
	// cfg.TLSDir). Best-effort: a write failure here must not take down an
	// otherwise-healthy daemon — logged and otherwise ignored.
	if caCertPath, err := client.DefaultCACertPath(); err != nil {
		slog.Warn("could not resolve daemon CA bootstrap file path; CLI TCP terminal fallback will not trust this daemon's self-signed cert automatically", "error", err)
	} else if caPEM := srv.GatewayCAPEM(); len(caPEM) > 0 {
		if err := os.MkdirAll(filepath.Dir(caCertPath), 0o755); err != nil {
			slog.Warn("could not create daemon CA bootstrap file directory", "path", caCertPath, "error", err)
		} else if err := os.WriteFile(caCertPath, caPEM, 0o644); err != nil {
			slog.Warn("could not write daemon CA bootstrap file", "path", caCertPath, "error", err)
		} else {
			slog.Info("wrote cli daemon-ca bootstrap file", "path", caCertPath)
		}
	}

	slog.Info("boid server started", "socket", cfg.SocketPath, "http", cfg.HTTPAddr)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	slog.Info("shutting down...")
	return srv.Stop()
}
