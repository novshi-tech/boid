package dispatcher

import (
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// SessionJobInput carries the resolved data needed to build a Session
// (HarnessAdapter-backed, task-less) JobSpec, for both the daemon API
// (POST /sessions) and the `boid agent` CLI.
//
// session jobs inherit project-level traits only (env / host_commands /
// additional_bindings / secret_namespace). behavior-level traits are
// deliberately ignored — sessions are not driven by the task state machine
// and have no behavior context to resolve.
type SessionJobInput struct {
	// ProjectID and ProjectWorkDir locate the host filesystem the sandbox
	// will expose; ProjectWorkDir is the cwd seen by the agent.
	ProjectID      string
	ProjectWorkDir string

	// ProjectName is project.yaml's `meta.name` (see
	// orchestrator.Visibility.ProjectName's doc comment). The caller fills
	// this from the same workspace-hydrated ProjectMeta it already reads
	// Env/HostCommands/AdditionalBindings from.
	ProjectName string

	// HarnessType selects the agent adapter the runner-inner-child will
	// dispatch through. Must be one of "claude" / "codex" / "opencode" /
	// "shell" (validated by the caller; BuildSessionJobSpec does not police it).
	HarnessType string

	// Argv is the literal program + arguments the shell adapter consumes.
	// The claude / codex / opencode adapters ignore it (they build their
	// argv from CLI conventions). Required when HarnessType == "shell";
	// ignored otherwise.
	Argv []string

	// Instruction is the optional bootstrap prompt the agent should pick up
	// on launch (e.g. the `--instruction` flag of `boid agent`, or the
	// WebUI Session dialog's text field). When non-empty it is plumbed
	// through RunContext.UserAnswer so the adapter's existing "user reply"
	// path delivers it as the first turn of input. Empty leaves the adapter
	// to pick its default bootstrap (no positional for session mode on claude,
	// since the /boid-task skill is meaningless without a task.yaml).
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
	SecretNamespace    string
	DockerEnabled      bool

	// DisplayName is the human-readable label persisted to jobs.display_name
	// (and shown in the TUI / Web UI). Empty falls back to "<harness>
	// session" downstream.
	DisplayName string

	// ConnectorPolicy, when true, selects orchestrator.ConnectorBuiltinPolicies
	// instead of the general DefaultBuiltinPolicies set — only
	// signal_ingest/signal_cursor_get are allowed, no fetch builtin at all.
	// Set only by sessionDispatcherAdapter.StartExec (internal/server/wire.go)
	// for a connector-triggered exec; every other caller leaves this false.
	ConnectorPolicy bool

	// APIGatewayServices, when non-nil, overrides the dispatcher-resolved
	// (floor ∪ workspace) API gateway service allowlist for this job's token
	// — copied straight onto JobSpec.APIGatewayServices (see that field's own
	// doc comment for the full rationale and where it gets consumed).
	APIGatewayServices []string

	// SignalService / SignalConnector are copied straight onto
	// JobSpec.SignalService/SignalConnector — see that field's own doc
	// comment.
	SignalService   string
	SignalConnector string

	// CardID / CardRequestID are copied straight onto
	// JobSpec.CardID/CardRequestID when this session is a card command's
	// continuation (set only by sessionDispatcherAdapter.StartSession from
	// a StartSessionRequest carrying them — see that type's own doc
	// comment). Empty for every ordinary session.
	CardID        string
	CardRequestID string

	// JobID, when non-empty, is copied straight onto JobSpec.ID — see that
	// field's own doc comment. Set only for a card-command launcher exec,
	// whose job id must be known before Dispatch runs. Empty for every other
	// caller, leaving Dispatch's own fresh-uuid generation unchanged.
	JobID string
}

// BuildSessionJobSpec creates a task-less agent job using sandbox startup checkout.
func BuildSessionJobSpec(input SessionJobInput) (*orchestrator.JobSpec, error) {
	pctx := orchestrator.PolicyContext{ProjectDir: input.ProjectWorkDir}
	// A connector job gets the reduced, signal-ops-only policy — see
	// ConnectorPolicy's own doc comment. ConnectorBuiltinPolicies deliberately
	// returns no "fetch" entry at all.
	var builtinPolicies map[string]orchestrator.BuiltinPolicy
	if input.ConnectorPolicy {
		builtinPolicies = orchestrator.ConnectorBuiltinPolicies(pctx)
	} else {
		builtinPolicies = orchestrator.DefaultBuiltinPolicies(
			orchestrator.RoleHook,
			[]string{"boid", "fetch"},
			pctx,
		)
	}
	// A connector job must get ZERO host_commands entries regardless of what
	// the caller passed: host_commands is a completely separate
	// authorization path from BuiltinPolicies (internal/sandbox/broker.go's
	// Handle dispatches non-boid/non-fetch commands via entry.Commands), so
	// ConnectorBuiltinPolicies alone does not close this door — a project's
	// declared host_commands (e.g. a `gh` entry) would otherwise leak into a
	// connector job too.
	var hostCommands map[string]orchestrator.CommandDef
	if !input.ConnectorPolicy {
		hostCommands = orchestrator.HostCommands(input.HostCommands).ToCommandDefs()
	}

	env := map[string]string{}
	for k, v := range input.Env {
		env[k] = v
	}
	if input.Model != "" {
		env["BOID_MODEL"] = input.Model
	}

	displayName := input.DisplayName
	if displayName == "" {
		displayName = input.HarnessType + " session"
	}

	spec := &orchestrator.JobSpec{
		ID:                 input.JobID,
		ProjectID:          input.ProjectID,
		DisplayName:        displayName,
		DisplayNameDefault: input.DisplayName == "",
		Kind:               orchestrator.JobKindSession,
		HarnessType:        input.HarnessType,
		Argv:               input.Argv, // consumed by shell adapter only; agent adapters ignore it
		Visibility: orchestrator.Visibility{
			ProjectDir:         input.ProjectWorkDir,
			ProjectName:        input.ProjectName,
			AdditionalBindings: input.AdditionalBindings,
			Writable:           !input.Readonly,
			DockerEnabled:      input.DockerEnabled,
			Checkout:           input.ProjectWorkDir != "",
		},
		BuiltinPolicies:    builtinPolicies,
		HostCommands:       hostCommands,
		SecretNamespace:    input.SecretNamespace,
		Env:                env,
		Interactive:        true, // sessions are PTY-attached by definition
		APIGatewayServices: input.APIGatewayServices,
		SignalService:      input.SignalService,
		SignalConnector:    input.SignalConnector,
		CardID:             input.CardID,
		CardRequestID:      input.CardRequestID,
	}
	// Instruction is delivered through Env (BOID_USER_ANSWER), which the
	// runner-inner-child threads into RunContext.UserAnswer. For the claude
	// adapter this is the same path Q&A replies travel, so the first turn
	// receives the user text verbatim instead of the default skill bootstrap.
	if input.Instruction != "" {
		spec.Env["BOID_USER_ANSWER"] = input.Instruction
	}
	return spec, nil
}

func BuildExecJobSpec(input SessionJobInput, argv []string, interactive bool) (*orchestrator.JobSpec, error) {
	defaultName := input.DisplayName == ""
	input.HarnessType = "shell"
	if input.DisplayName == "" && len(argv) > 0 {
		input.DisplayName = argv[0]
	}
	spec, err := BuildSessionJobSpec(input)
	if err != nil {
		return nil, err
	}
	spec.DisplayNameDefault = defaultName
	spec.Kind = orchestrator.JobKindExec
	spec.Argv = argv
	spec.Interactive = interactive
	return spec, nil
}
