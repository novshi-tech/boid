package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/config"
	"github.com/novshi-tech/boid/internal/daemon"
	"github.com/novshi-tech/boid/internal/server"
	"github.com/spf13/cobra"
	"golang.org/x/term"
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
	startDBPath       string
	startSocketPath   string
	startKitsDir      string
	startKeyFilePath  string
	startAutoMigrate  bool
)

func init() {
	startCmd.Annotations = map[string]string{annotationSkipAutostart: "skip"}
	startCmd.Flags().StringVar(&startDBPath, "db-path", "", "Path to the SQLite database")
	startCmd.Flags().StringVar(&startSocketPath, "socket-path", "", "Path to the UNIX socket")
	startCmd.Flags().StringVar(&startKitsDir, "kits-dir", "", "Base directory for installed kits")
	startCmd.Flags().StringVar(&startKeyFilePath, "key-file-path", "", "Path to the secret encryption key file")
	startCmd.Flags().BoolVar(&startAutoMigrate, "auto-migrate", false,
		"When project.yaml schema migration is needed, run `boid project migrate <dir> --apply` for each affected project automatically and respawn the daemon (skips the confirmation prompt on TTY too)")
	rootCmd.AddCommand(startCmd)
}

func defaultAllowedDomains() []string {
	return []string{
		// AI agents
		".anthropic.com",
		".claude.ai",
		".claude.com",
		"api.openai.com",
		"auth.openai.com",
		"chatgpt.com",
		// Go
		"proxy.golang.org",
		"sum.golang.org",
		// Node
		"registry.npmjs.org",
		// .NET
		"api.nuget.org",
		// Python
		"pypi.org",
		"files.pythonhosted.org",
		// Docker
		".docker.io",
		"auth.docker.io",
	}
}

func defaultDBPath() string {
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		home, _ := os.UserHomeDir()
		dataDir = filepath.Join(home, ".local", "share")
	}
	dir := filepath.Join(dataDir, "boid")
	os.MkdirAll(dir, 0o755)
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

type startConfigOptions struct {
	DBPath      string
	SocketPath  string
	KitsDir     string
	KeyFilePath string
}

func buildStartConfig(opts startConfigOptions) (server.Config, error) {
	cfg := server.Config{
		DBPath:         opts.DBPath,
		SocketPath:     opts.SocketPath,
		KitsDir:        opts.KitsDir,
		KeyFilePath:    opts.KeyFilePath,
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
	})
	if err != nil {
		return err
	}

	if daemon.IsChild() {
		return runDaemonChild(cfg)
	}
	return runDaemonParent(cfg)
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
// It redirects stdin/stdout/stderr to the log file, detaches from the session,
// and runs the server until a termination signal arrives.
//
// On any startup failure the child writes a structured StartupStatus to
// fd 3 (the side-channel pipe set up by daemon.Spawn) before returning,
// so the parent can render a useful message or drive auto-migration. On
// successful startup the child closes fd 3 instead, signalling EOF to the
// parent. After srv.Start() returns, fd 3 is no longer touched.
func runDaemonChild(cfg server.Config) error {
	logPath := daemon.LogFilePath()
	if err := daemon.RedirectToLogRotating(logPath); err != nil {
		daemon.WriteStartupStatusOnFD3(err)
		return fmt.Errorf("redirect to log: %w", err)
	}

	if _, err := syscall.Setsid(); err != nil {
		daemon.WriteStartupStatusOnFD3(err)
		return fmt.Errorf("setsid: %w", err)
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
