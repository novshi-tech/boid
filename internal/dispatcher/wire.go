package dispatcher

import (
	"database/sql"
)

type WireConfig struct {
	DB          *sql.DB
	Runtime     JobRuntime
	Broker      CommandBroker
	Sandbox     SandboxPreparer
	SecretStore *SecretStore

	// Worktrees resolves per-task git worktrees when a JobSpec declares
	// Visibility.UseWorktree. Pass nil to disable worktree-backed jobs.
	Worktrees  *WorktreeManager
	TaskLookup TaskLookup
	Projects   ProjectLookup

	// BoidBinary is the host path to the boid executable that should be
	// bind-mounted into sandboxes.
	BoidBinary string
	// ServerSocket is the host path to the daemon UNIX socket (for boid exec
	// jobs that talk to boid over HTTP from inside the sandbox).
	ServerSocket string
	// ProxyPort, when non-zero, enables HTTP(S) proxy environment variables
	// pointing at host-gateway:<ProxyPort>.
	ProxyPort *int
}

func Wire(cfg WireConfig) *Runner {
	return &Runner{
		DB:           cfg.DB,
		Runtime:      cfg.Runtime,
		Broker:       cfg.Broker,
		Sandbox:      cfg.Sandbox,
		SecretStore:  cfg.SecretStore,
		Worktrees:    cfg.Worktrees,
		TaskLookup:   cfg.TaskLookup,
		Projects:     cfg.Projects,
		BoidBinary:   cfg.BoidBinary,
		ServerSocket: cfg.ServerSocket,
		ProxyPort:    cfg.ProxyPort,
	}
}
