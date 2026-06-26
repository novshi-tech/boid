package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/config"
	"github.com/novshi-tech/boid/internal/daemon"
	"github.com/novshi-tech/boid/internal/server"
	"github.com/spf13/cobra"
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
	RunE:         runStart,
}

var (
	startDBPath      string
	startSocketPath  string
	startKitsDir     string
	startKeyFilePath string
)

func init() {
	startCmd.Annotations = map[string]string{annotationSkipAutostart: "skip"}
	startCmd.Flags().StringVar(&startDBPath, "db-path", "", "Path to the SQLite database")
	startCmd.Flags().StringVar(&startSocketPath, "socket-path", "", "Path to the UNIX socket")
	startCmd.Flags().StringVar(&startKitsDir, "kits-dir", "", "Base directory for installed kits")
	startCmd.Flags().StringVar(&startKeyFilePath, "key-file-path", "", "Path to the secret encryption key file")
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
// The outer timeout (daemonSocketTimeout + grace) prevents the loop from
// blocking forever if all three signals stall. Structured migration
// failures are surfaced to the user with project paths and the canonical
// `boid project migrate <dir> --apply` remediation; the user never has to
// grep boid.log to find the cause.
func runDaemonParent(cfg server.Config) error {
	// 既存サーバが生きていれば二重起動を拒否する。socket ファイルが残って
	// いるだけ (ECONNREFUSED) の場合は stale とみなし、子プロセスに clean up
	// を任せる。
	if daemon.IsSocketAlive(cfg.SocketPath, 500*time.Millisecond) {
		return fmt.Errorf("boid server already running (socket: %s)", cfg.SocketPath)
	}

	pid, statusR, err := daemon.Spawn(os.Args)
	if err != nil {
		return fmt.Errorf("spawn daemon: %w", err)
	}
	defer statusR.Close()

	logPath := daemon.LogFilePath()

	// Channel for socket-readiness polling.
	socketCh := make(chan error, 1)
	go func() {
		socketCh <- daemon.WaitForSocket(cfg.SocketPath, daemonSocketTimeout)
	}()

	// Channel for the structured startup status (fd 3 pipe).
	type startupResult struct {
		status *daemon.StartupStatus // non-nil = structured failure
		err    error                 // non-nil = decode error (rare)
	}
	resCh := make(chan startupResult, 1)
	go func() {
		s, err := daemon.ReadStartupStatus(statusR)
		switch {
		case errors.Is(err, daemon.ErrStartupOK):
			// EOF without payload — startup succeeded.
			resCh <- startupResult{}
		case err != nil:
			resCh <- startupResult{err: err}
		default:
			resCh <- startupResult{status: s}
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

	// Outer timeout — should not fire if the socket / status / dead
	// signals work correctly, but acts as a backstop.
	timeoutCh := time.After(daemonSocketTimeout + 5*time.Second)

	for socketCh != nil || resCh != nil {
		select {
		case err := <-socketCh:
			socketCh = nil
			if err == nil {
				fmt.Printf("boid server started (pid: %d, socket: %s, http: %s)\n",
					pid, cfg.SocketPath, cfg.HTTPAddr)
				return nil
			}
			// socket polling timed out (10s); keep waiting for the status
			// pipe or liveness signal to give a more specific cause.
		case res := <-resCh:
			resCh = nil
			if res.err != nil {
				return fmt.Errorf("daemon startup status decode failed: %w (logs: %s)",
					res.err, logPath)
			}
			if res.status != nil {
				return formatStartupFailure(res.status, logPath)
			}
			// EOF = startup OK; keep waiting for socket to confirm.
		case <-deadCh:
			return fmt.Errorf("daemon process exited unexpectedly (pid: %d); check logs at %s",
				pid, logPath)
		case <-timeoutCh:
			return fmt.Errorf("daemon did not start within %s (pid: %d); check logs at %s",
				daemonSocketTimeout+5*time.Second, pid, logPath)
		}
	}

	// Both signals reported "fine" but the socket never came up; treat as
	// a soft failure with log fallback.
	return fmt.Errorf("daemon startup completed but socket %s never became reachable; check logs at %s",
		cfg.SocketPath, logPath)
}

// formatStartupFailure renders a user-facing message for a structured
// startup failure. Migration cases include the per-project remediation
// command line so the user can copy-paste; other failures echo the
// daemon's error text and point at the log file.
func formatStartupFailure(s *daemon.StartupStatus, logPath string) error {
	switch s.Kind {
	case daemon.StartupKindMigration:
		return formatMigrationFailure(s, logPath)
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

// formatMigrationFailure builds the human-facing migration error for the
// boid start parent. The body lists every affected project and prints the
// exact `boid project migrate <dir> --apply` lines the user needs to run.
//
// The output target on auto-mode (Phase A) is "actionable without grep":
// the user sees the dirs and the remediation in one block.
func formatMigrationFailure(s *daemon.StartupStatus, logPath string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "boid start: %d project(s) need migration to the new project.yaml schema.\n\n",
		len(s.Projects))
	for _, p := range s.Projects {
		if p.ID != "" {
			fmt.Fprintf(&b, "  - %s\n    %s\n", p.Dir, p.ID)
		} else {
			fmt.Fprintf(&b, "  - %s\n", p.Dir)
		}
		for _, m := range p.Messages {
			fmt.Fprintf(&b, "      %s\n", m)
		}
	}
	b.WriteString("\nRun the following to migrate, then re-run `boid start`:\n\n")
	for _, p := range s.Projects {
		fmt.Fprintf(&b, "  boid project migrate %s --apply\n", p.Dir)
	}
	b.WriteString("\nDry-run first (without --apply) to inspect the plan.\n")
	b.WriteString("Full daemon log: " + logPath + "\n")
	return errors.New(b.String())
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
