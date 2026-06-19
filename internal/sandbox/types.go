package sandbox

// MountType represents the type of filesystem mount.
type MountType string

const (
	MountBind  MountType = "bind"
	MountRBind MountType = "rbind"
	MountTmpfs MountType = "tmpfs"
)

// HarnessType identifies which HarnessAdapter the runner should hand the
// process off to via adapter.Run(). Phase 3-d (PR1) made this field
// invariant non-empty for every dispatched job — hook / session / exec
// all resolve to shell / claude / codex / opencode. The empty string is no
// longer a valid value and the runner-inner-child rejects it; the legacy
// runExecArgv fallback was retired in the same change.
type HarnessType string

const (
	// HarnessShell routes through internal/adapters/shell.Adapter.Run() and
	// is the fall-through for hooks without an `agent:` declaration, every
	// `boid exec` job, and the default for unknown agent values. Shell
	// adapter forwards SIGUSR1 / SIGWINCH and normalises stop-signal exits
	// like the agent adapters do but performs no session resolution.
	HarnessShell HarnessType = "shell"
	// HarnessClaude routes through internal/adapters/claude.Adapter.Run().
	HarnessClaude HarnessType = "claude"
	// HarnessCodex routes through internal/adapters/codex.Adapter.Run().
	// Added in Phase 3-c as a prototype to validate the HarnessAdapter
	// abstraction beyond claude. Minimum implementation: 1-turn launch with
	// signal forwarding; session resume / payload patch are deliberately
	// left as no-ops (see docs/plans/agent-aware-boid.md Phase 3-c).
	HarnessCodex HarnessType = "codex"
	// HarnessOpenCode routes through internal/adapters/opencode.Adapter.Run().
	// Phase 3-c prototype, same scope as HarnessCodex.
	HarnessOpenCode HarnessType = "opencode"
)

// BindMount is the dispatcher-facing DTO for arbitrary bind-mount requests.
// It is used by the dispatcher boundary (via SandboxSpec.AdditionalBindings)
// and is converted into Mount entries at the edge. The sandbox layer itself
// works in terms of Mount only.
type BindMount struct {
	Source   string
	Target   string // if empty, defaults to Source
	Mode     string // "rw" | "" (ro default)
	IsFile   bool   // if true, treat target as a file (touch before bind, skip type detection)
	Optional bool   // if true, skip mount when Source does not exist on the host
}

// Mount describes a single filesystem mount applied inside the sandbox.
// Types: bind, rbind, tmpfs. Flags cover read-only remount, file vs dir
// handling, slave propagation, guards, and required sub-directory creation.
type Mount struct {
	Source     string // host path (empty for tmpfs)
	Target     string // absolute path inside sandbox
	Type       MountType
	ReadOnly   bool
	Slave      bool     // mount --make-rslave after mounting
	IsFile     bool     // target is a file, not a directory
	DetectType bool     // detect file vs dir at runtime (if/elif)
	Guard      string   // shell test expression; if non-empty, wrap in if [ $Guard ]; then
	NeedsDirs  []string // subdirs to create under Target before ro remount
}

// FileWrite describes a file to materialize inside the sandbox. Content is
// written verbatim (shell-quoted at render time); the parent directory is
// created with mkdir -p beforehand.
type FileWrite struct {
	Path    string // absolute path inside sandbox
	Content string
}

// Symlink describes a symlink to create inside the sandbox.
type Symlink struct {
	LinkPath   string // absolute path inside sandbox (where the symlink is created)
	LinkTarget string // what the symlink points to (resolved relative to LinkPath)
}
