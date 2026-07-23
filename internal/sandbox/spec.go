package sandbox

// Profile selects the sandbox filesystem layout strategy.
// The zero value (ProfileDefault) preserves the existing behaviour so callers
// that do not set the field are unaffected.
type Profile int

const (
	// ProfileDefault is the standard layout: a small set of host system dirs
	// (/bin, /sbin, /lib, /lib64, /usr, /etc) are ro-rbind-mounted plus
	// /dev, /proc, and a /tmp tmpfs. Broker socket and registration proceed
	// normally. This is the zero value; any Spec where Profile is not set
	// explicitly uses this layout.
	ProfileDefault Profile = iota

	// ProfileInit is the host-scan layout used by kit-init / workspace-configure
	// generation scripts. The entire host root (/) is ro-rbind-mounted so the
	// agent can discover installed tools without needing explicit allowlists.
	// Broker registration and the broker socket mount are both skipped because
	// init scripts do not call back into boid host-commands.
	ProfileInit
)

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
	// HarnessType, when non-empty, directs runner-inner-child to hand the
	// agent process off to the matching HarnessAdapter.Run() instead of
	// exec-ing Argv verbatim. Empty preserves the legacy exec path used by
	// boid exec jobs and any non-agent hook job. See sandbox.HarnessType.
	HarnessType HarnessType

	// UserAnswer carries the Q&A reply that should be threaded into the
	// adapter's RunContext.UserAnswer. Empty for fresh starts and for resumes
	// without a Q&A reply.
	UserAnswer string

	// Profile selects the filesystem layout strategy. Zero value (ProfileDefault)
	// preserves the existing behaviour. Set to ProfileInit for kit-init /
	// workspace-configure generation scripts that need to read the full host FS.
	Profile Profile

	// Clone declares the opt-in sandbox-internal clone + branch resolution
	// sequence runner-inner-child performs before handing off to the
	// harness (docs/plans/git-gateway-cutover.md PR5). Zero value
	// (Clone.Enabled == false) is a no-op — see CloneSpec's doc comment.
	Clone CloneSpec

	// ContainerImage is the container backend's image-selection input
	// (docs/plans/phase6-container-backend.md §PR5, §決定 2/11): the
	// workspace-level `orchestrator.WorkspaceMeta.ContainerImage` override,
	// threaded through unchanged from SandboxRuntimeInfo.ContainerImage by
	// BuildSandboxSpec. Empty means "use the backend's configured default
	// image" (the shared boid base image, §決定 2). The userns backend
	// never reads this field — it has no notion of images. When non-empty,
	// containerBackend.Launch treats it as a workspace override that must
	// pass the "derived from the boid base image" check (§決定 11) before
	// use: `docker inspect` the image and require the
	// `boid.runner_protocol` label to match, rejecting Launch otherwise.
	ContainerImage string
}
