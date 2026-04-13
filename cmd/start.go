package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/daemon"
	"github.com/novshi-tech/boid/internal/server"
	"github.com/spf13/cobra"
)

const (
	defaultStartHTTPAddr  = ":8080"
	daemonSocketTimeout   = 10 * time.Second
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the boid server",
	RunE:  runStart,
}

var (
	startDBPath      string
	startSocketPath  string
	startHTTPAddr    string
	startKitsDir     string
	startKeyFilePath string
)

func init() {
	startCmd.Flags().StringVar(&startDBPath, "db-path", "", "Path to the SQLite database")
	startCmd.Flags().StringVar(&startSocketPath, "socket-path", "", "Path to the UNIX socket")
	startCmd.Flags().StringVar(&startHTTPAddr, "http-addr", "", "HTTP listen address")
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

type startConfigOptions struct {
	DBPath      string
	SocketPath  string
	HTTPAddr    string
	KitsDir     string
	KeyFilePath string
}

func buildStartConfig(opts startConfigOptions) server.Config {
	cfg := server.Config{
		DBPath:         opts.DBPath,
		SocketPath:     opts.SocketPath,
		HTTPAddr:       opts.HTTPAddr,
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
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = defaultStartHTTPAddr
	}
	if cfg.KitsDir == "" {
		cfg.KitsDir = defaultKitsDir()
	}

	return cfg
}

func runStart(cmd *cobra.Command, args []string) error {
	cfg := buildStartConfig(startConfigOptions{
		DBPath:      startDBPath,
		SocketPath:  startSocketPath,
		HTTPAddr:    startHTTPAddr,
		KitsDir:     startKitsDir,
		KeyFilePath: startKeyFilePath,
	})

	if daemon.IsChild() {
		return runDaemonChild(cfg)
	}
	return runDaemonParent(cfg)
}

// runDaemonParent spawns the daemon child, waits for the UNIX socket to become
// ready, prints a status line, and exits.
func runDaemonParent(cfg server.Config) error {
	pid, err := daemon.Spawn(os.Args)
	if err != nil {
		return fmt.Errorf("spawn daemon: %w", err)
	}

	logPath := daemon.LogFilePath()
	if err := daemon.WaitForSocket(cfg.SocketPath, daemonSocketTimeout); err != nil {
		return fmt.Errorf("daemon did not start (pid: %d); check logs at %s: %w", pid, logPath, err)
	}

	fmt.Printf("boid server started (pid: %d, socket: %s, http: %s)\n", pid, cfg.SocketPath, cfg.HTTPAddr)
	return nil
}

// runDaemonChild is executed by the daemon child process (BOID_DAEMON_CHILD=1).
// It redirects stdin/stdout/stderr to the log file, detaches from the session,
// manages the PID file, and runs the server until a termination signal arrives.
func runDaemonChild(cfg server.Config) error {
	logPath := daemon.LogFilePath()
	if err := daemon.RedirectToLogRotating(logPath); err != nil {
		return fmt.Errorf("redirect to log: %w", err)
	}

	if _, err := syscall.Setsid(); err != nil {
		return fmt.Errorf("setsid: %w", err)
	}

	pidPath := daemon.PIDFilePath()
	if err := daemon.CheckNotRunning(pidPath); err != nil {
		return err
	}
	if err := daemon.WritePID(pidPath, os.Getpid()); err != nil {
		return fmt.Errorf("write pid file: %w", err)
	}
	defer daemon.RemovePID(pidPath)

	srv, err := server.New(cfg)
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		return fmt.Errorf("start server: %w", err)
	}

	slog.Info("boid server started", "socket", cfg.SocketPath, "http", cfg.HTTPAddr)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	slog.Info("shutting down...")
	return srv.Stop()
}
