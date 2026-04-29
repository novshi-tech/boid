package dispatcher

import "github.com/novshi-tech/boid/internal/orchestrator"

// CommandJobInput carries the resolved data from a project command definition
// needed to build an orchestrator.JobSpec. It is shared between the CLI exec
// path (cmd/exec.go) and the daemon API path (POST /api/projects/.../execute).
type CommandJobInput struct {
	ProjectID          string
	ProjectWorkDir     string
	Argv               []string
	Env                map[string]string
	HostCommands       map[string]orchestrator.HostCommandSpec
	AdditionalBindings []orchestrator.BindMount
	Readonly           bool
	// Interactive forces TTY allocation when dispatched via the daemon runtime.
	// The CLI exec path leaves this false and overrides TTY after BuildSandboxSpec
	// based on the real terminal state.
	Interactive bool
}

// BuildCommandJobSpec converts a resolved CommandJobInput into a JobSpec.
// It is the canonical place for CommandSpec → JobSpec translation: builtin
// policy construction, host command conversion, and visibility derivation.
// Neither sandbox construction nor broker registration is performed here.
func BuildCommandJobSpec(input CommandJobInput) *orchestrator.JobSpec {
	builtinPolicies := orchestrator.DefaultBuiltinPolicies(
		orchestrator.RoleHook,
		[]string{"boid", "git"},
		orchestrator.PolicyContext{ProjectDir: input.ProjectWorkDir},
	)
	hostCommands := orchestrator.HostCommands(input.HostCommands).ToCommandDefs()

	return &orchestrator.JobSpec{
		ProjectID: input.ProjectID,
		HandlerID: "",
		Kind:      orchestrator.JobKindExec,
		Argv:      input.Argv,
		Visibility: orchestrator.Visibility{
			ProjectDir:         input.ProjectWorkDir,
			UseWorktree:        false,
			AdditionalBindings: input.AdditionalBindings,
			Writable:           !input.Readonly,
		},
		BuiltinPolicies: builtinPolicies,
		HostCommands:    hostCommands,
		Env:             input.Env,
		Interactive:     input.Interactive,
	}
}
