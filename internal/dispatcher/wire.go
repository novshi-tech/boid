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
	// AllowedDomains is the proxy egress allowlist. Plumbed through to
	// environment.yaml so agents can see which hosts the proxy will let
	// through without having to probe with a 403-burning fetch.
	AllowedDomains []string
	// RuntimesDir is the root directory where per-sandbox runtime directories
	// are created. When non-empty and DockerEnabled, the runner pre-allocates a
	// runtime directory here to host the per-sandbox docker proxy socket and
	// resource ledger.
	RuntimesDir string
	// AttachmentsRoot is the data-home directory under which per-task
	// attachments live (`<root>/tasks/<id>/attachments`). When non-empty the
	// runner threads it through SandboxRuntimeInfo so BuildSandboxSpec can
	// add the read-only bind to `~/.boid/attachments` for every harness.
	AttachmentsRoot string
}

func Wire(cfg WireConfig) *Runner {
	return &Runner{
		DB:              cfg.DB,
		Runtime:         cfg.Runtime,
		Broker:          cfg.Broker,
		Sandbox:         cfg.Sandbox,
		SecretStore:    cfg.SecretStore,
		Worktrees:       cfg.Worktrees,
		TaskLookup:      cfg.TaskLookup,
		Projects:        cfg.Projects,
		BoidBinary:      cfg.BoidBinary,
		ServerSocket:    cfg.ServerSocket,
		ProxyPort:       cfg.ProxyPort,
		AllowedDomains:  cfg.AllowedDomains,
		RuntimesDir:     cfg.RuntimesDir,
		AttachmentsRoot: cfg.AttachmentsRoot,
	}
}
