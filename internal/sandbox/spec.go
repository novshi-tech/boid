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
	// Broker socket path and token are carried in Env (BOID_BROKER_SOCKET /
	// BOID_BROKER_TOKEN).
	//
	// Phase 6 PR8 (docs/plans/phase6-container-backend.md §決定 9) retired the
	// sibling PayloadPatchPath field: agents / hook scripts now apply their
	// payload patch immediately via the broker's `boid task update
	// --payload-patch` RPC instead of writing a well-known
	// $HOME/.boid/output/payload_patch.json the runner used to read back as
	// the job-done output — see runner.resolveJobOutput's doc comment.
	Foreground bool

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

	// HomeSkeletonRoot is the absolute in-sandbox path the workspace HOME is
	// mounted at, i.e. $HOME. It is the root HomeSkeletonDirs' entries are
	// relative to, and — because the HOME mount is the whole of the workspace
	// home volume — it is also the point that separates an in-container path
	// from a path INSIDE the volume.
	//
	// That second role is why it is carried at all rather than being implied.
	// The only repair for what HomeSkeletonDirs detects is deleting the
	// offending directory from the host side (`docker volume inspect`'s
	// Mountpoint, under `podman unshare` or `sudo`), and the path to give the
	// operator there is <Mountpoint>/<the entry>, NOT <Mountpoint>/<the
	// in-container absolute path>. Those differ by exactly this prefix, and
	// naming the wrong one sends the operator to a path that does not exist:
	// they delete nothing, the uid-0 directory survives, and the next job fails
	// the same check — a loop the error itself caused (codex review of PR6,
	// Major 2).
	//
	// Empty when HomeSkeletonDirs is empty. A non-empty skeleton with no root
	// is a wiring bug and the runner fails the job rather than resolving the
	// entries against whatever the process's cwd happens to be.
	HomeSkeletonRoot string

	// HomeSkeletonDirs are directories, RELATIVE to HomeSkeletonRoot, that
	// must already exist and be owned by the uid the sandbox runs as before
	// the harness starts. The container runner checks them and fails the job
	// when one is not (internal/sandbox/runner's verifyHomeSkeleton).
	//
	// Relative rather than absolute so that they are the same values the rest
	// of the mechanism already speaks in: dispatcher.workspaceHomeSkeletonDirs
	// produces them relative to the home, the init container's prelude mkdir's
	// them relative to the home, and the completion marker records them
	// relative to the home. An absolute spelling here would be a fourth
	// representation, convertible back only by string surgery at the point
	// where it matters most (see HomeSkeletonRoot).
	//
	// They are the workspace HOME's bind-target ancestors — .claude and
	// .claude/skills. The check exists because an ABSENT bind target is created
	// by the container ENGINE, at container start, as uid 0: measured on podman
	// 4.9.3 with a named-volume home and keep-id, ~/.claude comes out
	// root-owned and the uid-1000 harness can no longer write
	// ~/.claude/.credentials.json, which under rootless podman neither it nor
	// the daemon can repair (the host-side owner is a subuid). The symptom
	// without this check is not an error but a workspace whose authentication
	// silently stops persisting.
	//
	// # Why the per-skill LEAVES are not in here
	//
	// The set the prelude creates also has one entry per embedded skill
	// (.claude/skills/<name>), and those are deliberately excluded: each is
	// itself a read-only bind target in this same Spec, so by the time this
	// process runs, os.Stat on one reads the BIND SOURCE — the daemon-side
	// materialized skill directory — not the directory inside the volume. A
	// check that cannot observe its subject is not a weaker check, it is a
	// different one, and dispatcher.homeSkeletonDirs drops any entry a mount
	// covers rather than pretending otherwise. See its doc comment for what
	// that leaves exposed (a stale, empty, root-owned leaf survives in the
	// volume, harmlessly, until a release stops shipping that skill).
	//
	// Verification lives HERE rather than on the daemon side because PR6 of
	// docs/plans/workspace-home-volume-persistence.md made the home a docker
	// named volume: the daemon can neither stat nor repair anything inside it.
	// 論点 e-2 requires the check to sit outside whatever creates these
	// directories (the init container's prelude does), and the job container
	// satisfies that while also being the only place that observes the state
	// the harness will actually meet — after the engine's auto-creation, as the
	// harness's own uid.
	//
	// Empty means "nothing to check": test wiring, and any sandbox with no
	// workspace home (the tmpfs-HOME degrade in dispatcher.homeMounts).
	HomeSkeletonDirs []string
}
