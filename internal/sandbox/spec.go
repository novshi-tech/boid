package sandbox

// Spec describes a sandbox invocation in primitives only. The sandbox layer
// knows nothing about Role / Task / Job / Broker / Hook — all of those are the
// caller's concern. Everything needed to build and run the sandbox must already
// be present as mounts, files, env, argv, etc.
//
// Spec is JSON-serialized by the dispatcher to /tmp/boid-<ID>-runner-spec.json
// and read back by the go-native runner subcommands (runner-outer /
// runner-inner / runner-inner-child). All fields are exported so the default
// encoding/json round-trip is exact.
type Spec struct {
	// ID is the job id; it namespaces the generated /tmp/boid-<ID>-* files.
	ID string

	// --- Filesystem primitives ---
	// Mounts are applied in order to compose the sandbox root filesystem.
	Mounts []Mount
	// Files are materialized inside the sandbox (after pivot_root) before the
	// entry command runs.
	Files []FileWrite
	// Symlinks are created inside the sandbox.
	Symlinks []Symlink

	// --- Network ---
	// ProxyPort, when > 0, engages the nft drop policy + HTTP proxy env vars.
	ProxyPort int

	// --- Process ---
	// Argv is the program and arguments to invoke (POSIX argv).
	Argv []string
	// WorkDir is the cwd for the entry process. Also used as the cwd reported to
	// the broker for the job-done call (must be within the project/worktree root).
	WorkDir string
	// Env is the environment for the entry process.
	Env map[string]string
	// StdinBytes, when non-empty, is piped into the entry's stdin.
	StdinBytes []byte
	// StdoutCaptureFile, when non-empty, redirects stdout to that sandbox-internal
	// path. Doubles as the job-done output fallback when no payload patch exists.
	StdoutCaptureFile string
	// TTY, when true, the entry process inherits the caller's PTY on stdio.
	TTY bool

	// --- Completion / broker ---
	// Foreground indicates a user-facing job (boid exec): stdout/stderr are
	// inherited and no broker job-done callback fires on exit. Hook jobs leave
	// this false so the runner posts `boid job done` through the broker.
	Foreground bool
	// PayloadPatchPath is the sandbox-internal path the agent writes its result
	// patch to (HOME/.boid/output/payload_patch.json). The runner reads it as the
	// job-done output. Broker socket path and token are carried in Env
	// (BOID_BROKER_SOCKET / BOID_BROKER_TOKEN).
	PayloadPatchPath string

	// --- Bookkeeping ---
	// RootDir, if non-empty, is the host directory used as the sandbox ROOT (a
	// tmpfs is mounted on it). Go-side cleanup removes it after exit.
	RootDir string
	// CleanupPaths are removed by runner-outer after the sandbox process tree
	// exits (used for staging dirs). Removal runs in the host mount namespace,
	// where the sandbox's bind mounts are already gone.
	CleanupPaths []string
	// StopSignalName is the harness agent-stop signal name (e.g. "USR1"). The
	// runner subcommands set this signal to SIG_IGN so they survive a
	// process-group signal while the harness runner (run-agent.py) acts on it.
	// Defaults to "USR1" when empty.
	StopSignalName string
}
