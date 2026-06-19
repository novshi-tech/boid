package dispatcher

import "github.com/novshi-tech/boid/internal/orchestrator"

// SessionJobInput carries the resolved data needed to build a Session
// (HarnessAdapter-backed, task-less) JobSpec. Phase 3-d (PR1) introduced
// it as the input shape for both the daemon API (POST /sessions) and the
// `boid agent` CLI.
//
// session jobs inherit project-level traits only (env / host_commands /
// additional_bindings / kit_roots / secret_namespace). behavior-level traits
// are deliberately ignored — sessions are not driven by the task state
// machine and have no behavior context to resolve.
type SessionJobInput struct {
	// ProjectID and ProjectWorkDir locate the host filesystem the sandbox
	// will expose; ProjectWorkDir is the cwd seen by the agent.
	ProjectID      string
	ProjectWorkDir string

	// HarnessType selects the agent adapter the runner-inner-child will
	// dispatch through. Must be one of "claude" / "codex" / "opencode"
	// (validated by the caller; BuildSessionJobSpec does not police it).
	HarnessType string

	// SessionID is the resume target. Empty starts a fresh session and the
	// adapter generates a new id.
	SessionID string

	// Instruction is the optional bootstrap prompt the agent should pick up
	// on launch (e.g. the `--instruction` flag of `boid agent`, or the
	// WebUI Session dialog's text field). When non-empty it is plumbed
	// through RunContext.UserAnswer so the adapter's existing "user reply"
	// path delivers it as the first turn of input. Empty leaves the adapter
	// to pick its default bootstrap (e.g. /boid-sandbox skill for claude).
	Instruction string

	// Readonly controls Visibility.Writable. Sessions default to writable
	// (interactive use prioritises developer ergonomics over fail-safety)
	// so callers must opt into a read-only session explicitly.
	Readonly bool

	// Model overrides the harness binary's default model selection.
	Model string

	// Project trait overlay (the session has no behavior to resolve from,
	// so the caller fills these directly from ProjectMeta).
	Env                map[string]string
	HostCommands       map[string]orchestrator.HostCommandSpec
	AdditionalBindings []orchestrator.BindMount
	KitRoots           []string
	SecretNamespace    string
	DockerEnabled      bool

	// DisplayName is the human-readable label persisted to jobs.display_name
	// (and shown in the TUI / Web UI). Empty falls back to "<harness>
	// session" downstream.
	DisplayName string
}

// BuildSessionJobSpec converts a resolved SessionJobInput into a JobSpec
// (JobKindSession, adapter-bound HarnessType). The result is fed straight to
// dispatcher.Runner which builds the sandbox and hands the agent process to
// adapter.Run().
func BuildSessionJobSpec(input SessionJobInput) *orchestrator.JobSpec {
	builtinPolicies := orchestrator.DefaultBuiltinPolicies(
		orchestrator.RoleHook,
		[]string{"boid", "git", "fetch"},
		orchestrator.PolicyContext{ProjectDir: input.ProjectWorkDir},
	)
	hostCommands := orchestrator.HostCommands(input.HostCommands).ToCommandDefs()

	env := map[string]string{}
	for k, v := range input.Env {
		env[k] = v
	}
	if input.SessionID != "" {
		env["BOID_AGENT_SESSION_ID"] = input.SessionID
	}
	if input.Model != "" {
		env["BOID_MODEL"] = input.Model
	}

	displayName := input.DisplayName
	if displayName == "" {
		displayName = input.HarnessType + " session"
	}

	spec := &orchestrator.JobSpec{
		ProjectID:   input.ProjectID,
		DisplayName: displayName,
		Kind:        orchestrator.JobKindSession,
		HarnessType: input.HarnessType,
		Visibility: orchestrator.Visibility{
			ProjectDir:         input.ProjectWorkDir,
			UseWorktree:        false,
			AdditionalBindings: input.AdditionalBindings,
			Writable:           !input.Readonly,
			KitRoots:           input.KitRoots,
			DockerEnabled:      input.DockerEnabled,
		},
		BuiltinPolicies: builtinPolicies,
		HostCommands:    hostCommands,
		SecretNamespace: input.SecretNamespace,
		Env:             env,
		Interactive:     true, // sessions are PTY-attached by definition
	}
	// Instruction is delivered through Env (BOID_USER_ANSWER), which the
	// runner-inner-child threads into RunContext.UserAnswer. For the claude
	// adapter this is the same path Q&A replies travel, so the first turn
	// receives the user text verbatim instead of the default skill bootstrap.
	if input.Instruction != "" {
		spec.Env["BOID_USER_ANSWER"] = input.Instruction
	}
	return spec
}

// BuildExecJobSpec is the shell-harness variant of BuildSessionJobSpec used by
// `boid exec` to run a user-supplied argv inside the project sandbox. It reuses
// BuildSessionJobSpec for project trait inheritance and overrides the result:
//
//   - Kind = JobKindExec (TUI displays an "exec" badge instead of "session")
//   - Argv = the user's argv (runner-inner-child hands this to the shell adapter)
//   - Interactive = caller's tty detection (sessions are always PTY-attached;
//     exec may be piped from a non-TTY stdin)
//   - DisplayName falls back to argv[0] when the caller leaves it empty
//
// HarnessType in input is ignored and forced to "shell"; argv must be non-empty.
func BuildExecJobSpec(input SessionJobInput, argv []string, interactive bool) *orchestrator.JobSpec {
	input.HarnessType = "shell"
	if input.DisplayName == "" && len(argv) > 0 {
		input.DisplayName = argv[0]
	}
	spec := BuildSessionJobSpec(input)
	spec.Kind = orchestrator.JobKindExec
	spec.Argv = argv
	spec.Interactive = interactive
	return spec
}
