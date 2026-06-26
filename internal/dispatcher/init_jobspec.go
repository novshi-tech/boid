package dispatcher

import (
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// InitJobInput carries the resolved data needed to build a sandbox JobSpec for
// init-style commands (boid kit init, boid workspace configure) that scan the
// host filesystem and write machine-local yaml files without going through the
// task state machine or daemon broker.
//
// The shape mirrors SessionJobInput but is distinct by design so exec / session
// jobs are never accidentally given ProfileInit semantics and vice versa.
type InitJobInput struct {
	// Profile selects the sandbox filesystem layout. Must be sandbox.ProfileInit
	// for kit-init / workspace-configure (host root ro-rbind, broker skipped).
	Profile sandbox.Profile

	// WritableDirs is the list of host directories to bind read-write into the
	// sandbox so the agent can write generated yaml files. Each entry must be an
	// absolute host path; it is bind-mounted at the same path inside the
	// sandbox. The directory must already exist on the host before dispatch
	// (caller is responsible for mkdir).
	WritableDirs []string

	// PreCreateFiles is the list of host file paths that the caller has already
	// created (touch) and should be exposed read-write inside the sandbox. Used
	// by workspace-configure to expose a single writable <slug>.yaml without
	// making the entire parent directory writable.
	PreCreateFiles []string

	// ReadOnlyBinds is additional host paths to bind read-only into the
	// sandbox. Used by workspace-configure to give the agent access to linked
	// project directories (package.json / go.mod / hook scripts) without
	// granting write access.
	ReadOnlyBinds []string

	// Argv is the literal program + arguments to exec inside the sandbox.
	// For agent harnesses (claude / codex / opencode) the adapter builds its
	// own argv from its CLI conventions and may ignore this; it is still
	// required so the shell adapter fall-through path works and so the runner
	// can record a meaningful command in diagnostics.
	Argv []string

	// DisplayName is the human-readable label shown in the TUI / Web UI.
	DisplayName string

	// Env carries additional environment variables to inject into the sandbox
	// on top of the standard HOME / PATH / TERM set. Used to pass context like
	// BOID_WORKSPACE_SLUG to the skill.
	Env map[string]string

	// Instruction is the optional bootstrap prompt the agent should pick up
	// on launch. Mirrors SessionJobInput.Instruction: when non-empty it is
	// delivered through Env (BOID_USER_ANSWER) so the harness adapter
	// receives it as the first turn of user input — for ProfileInit jobs
	// this is how `boid kit init` / `boid workspace configure` kicks the
	// embedded skill ("boid kit init を実行して" etc.) without making the
	// user type anything after the harness opens.
	Instruction string

	// HarnessType selects the agent adapter. Must be one of "claude" /
	// "codex" / "opencode" / "shell". Validated by the caller.
	HarnessType string
}

// BuildInitJobSpec converts an InitJobInput into a JobSpec suitable for
// ProfileInit sandbox dispatch. It does not touch broker state, host-command
// registration, or the task state machine — those are all skipped for
// init-style jobs (see sandbox_builder.go:257-264 for the ServerSocket guard,
// and runner.go:183 for the broker-registration ProfileInit guard).
//
// The returned JobSpec is passed to BuildSandboxSpec + NewSandboxPreparer to
// produce the launch artefacts, then handed to runner-outer via syscall.Exec
// (foreground mode, same as boid exec).
func BuildInitJobSpec(in InitJobInput) *orchestrator.JobSpec {
	env := cloneStringMap(in.Env)
	if env == nil {
		env = map[string]string{}
	}

	// Instruction is delivered through Env (BOID_USER_ANSWER), mirroring
	// the SessionJobInput plumbing. The runner-inner-child threads
	// BOID_USER_ANSWER into RunContext.UserAnswer so the harness adapter
	// uses it as the first turn of input instead of opening to an empty
	// prompt — the only way kit init / workspace configure skills can
	// auto-trigger without the user having to know what to type.
	if in.Instruction != "" {
		env["BOID_USER_ANSWER"] = in.Instruction
	}

	// Build the additional bindings from the caller-supplied writable dirs,
	// pre-created files, and read-only extra binds.
	var bindings []orchestrator.BindMount
	for _, dir := range in.WritableDirs {
		bindings = append(bindings, orchestrator.BindMount{
			Source: dir,
			Target: dir,
			Mode:   "rw",
		})
	}
	for _, file := range in.PreCreateFiles {
		bindings = append(bindings, orchestrator.BindMount{
			Source: file,
			Target: file,
			Mode:   "rw",
			IsFile: true,
		})
	}
	for _, path := range in.ReadOnlyBinds {
		bindings = append(bindings, orchestrator.BindMount{
			Source: path,
			Target: path,
			// Mode "" → read-only (default)
		})
	}

	displayName := in.DisplayName
	if displayName == "" {
		displayName = "boid init"
	}

	return &orchestrator.JobSpec{
		DisplayName: displayName,
		Kind:        orchestrator.JobKindExec,
		HarnessType: in.HarnessType,
		Argv:        in.Argv,
		// No TaskID / ProjectID / HandlerID — init jobs are not tied to the
		// task state machine.
		Visibility: orchestrator.Visibility{
			// No ProjectDir: ProfileInit scans the full host root via the plan's
			// ro-rbind, not a project dir. The builder layers a tmpfs over
			// HOME/.boid only (not over HOME) so kit init / workspace configure
			// can still see ~/.volta, ~/.local/bin and other host tooling that
			// lives under HOME — see BuildSandboxSpec's ProfileInit branch.
			Writable:           false,
			AdditionalBindings: bindings,
		},
		// No BuiltinPolicies / HostCommands / SecretNamespace — broker is
		// skipped for ProfileInit jobs (runner.go:183 guard).
		Env:            env,
		Interactive:    true,          // foreground TTY
		SandboxProfile: int(in.Profile), // sandbox.Profile — int to avoid circular import
	}
}
