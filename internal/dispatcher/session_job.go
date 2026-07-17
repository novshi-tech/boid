package dispatcher

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// SessionJobInput carries the resolved data needed to build a Session
// (HarnessAdapter-backed, task-less) JobSpec. Phase 3-d (PR1) introduced
// it as the input shape for both the daemon API (POST /sessions) and the
// `boid agent` CLI.
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
}

// BuildSessionJobSpec converts a resolved SessionJobInput into a JobSpec
// (JobKindSession, adapter-bound HarnessType). The result is fed straight to
// dispatcher.Runner which builds the sandbox and hands the agent process to
// adapter.Run().
//
// Returns an error when the sandbox-internal clone declaration cannot be
// built for a non-empty ProjectWorkDir — see buildSessionCloneDeclaration's
// doc comment. The caller (WebUI POST /sessions, `boid exec` CLI) must
// surface that as a user-visible error rather than silently degrading, since
// the cutover contract (docs/plans/git-gateway-cutover.md PR6) requires a
// clone-based dispatch for every project-visible job.
func BuildSessionJobSpec(input SessionJobInput) (*orchestrator.JobSpec, error) {
	builtinPolicies := orchestrator.DefaultBuiltinPolicies(
		orchestrator.RoleHook,
		[]string{"boid", "fetch"},
		orchestrator.PolicyContext{ProjectDir: input.ProjectWorkDir},
	)
	hostCommands := orchestrator.HostCommands(input.HostCommands).ToCommandDefs()

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

	// Clone (docs/plans/git-gateway-cutover.md PR6 cutover): sessions and
	// exec have no Task, so there is no explicit base_branch to declare —
	// clone the project's current default branch with no branch created
	// (CheckoutOnly). A resolution failure is a hard error, not a silent
	// fallback: see buildSessionCloneDeclaration.
	cloneDecl, err := buildSessionCloneDeclaration(input.ProjectWorkDir)
	if err != nil {
		return nil, err
	}

	spec := &orchestrator.JobSpec{
		ProjectID:   input.ProjectID,
		DisplayName: displayName,
		Kind:        orchestrator.JobKindSession,
		HarnessType: input.HarnessType,
		Argv:        input.Argv, // consumed by shell adapter only; agent adapters ignore it
		Visibility: orchestrator.Visibility{
			ProjectDir:         input.ProjectWorkDir,
			ProjectName:        input.ProjectName,
			AdditionalBindings: input.AdditionalBindings,
			Writable:           !input.Readonly,
			DockerEnabled:      input.DockerEnabled,
			Clone:              cloneDecl,
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
	return spec, nil
}

// resolveSessionBaseBranchFn is the injection point for
// buildSessionCloneDeclaration's HEAD resolver. Production points at
// resolveSessionBaseBranch (which shells out to real git); tests override it
// with a stub so the field-passthrough contracts don't have to bring up a
// real git repo per test. Reassigned only from tests, non-parallel — the
// session_job_test.go stubSessionBaseBranch helper documents the discipline.
var resolveSessionBaseBranchFn = resolveSessionBaseBranch

// buildSessionCloneDeclaration returns the Clone declaration for a task-less
// job (`boid agent` / `boid exec`): CheckoutOnly on whatever branch the host
// project directory's HEAD currently resolves to.
//
// Returns (nil, nil) only when projectWorkDir is empty — the "no project
// visible" state, in which BuildSandboxSpec's own "no project visible"
// branch takes over cleanly. When projectWorkDir is set but HEAD cannot be
// resolved (detached HEAD, corrupted repo, git absent), this returns an
// error rather than silently degrading to nil.
//
// Rationale: pre-cutover, a nil Clone from this function silently fell
// through to the projectVisibilityMounts branch (a live host RW bind of
// ProjectDir plus the git shim overlay) — that path is now retired
// (docs/plans/git-gateway-cutover.md PR6 cutover) and re-entering it would
// bypass RequireUpstreamURL, gateway auth, and the whole clone-mode
// contract. Detached HEAD is a real state that happens on real repos
// outside hand-built test fixtures (mid-rebase, bisect, or someone
// checked out a tag or a raw SHA), so degrading here would be a cutover
// bypass in exactly the situations it's most important to catch.
func buildSessionCloneDeclaration(projectWorkDir string) (*orchestrator.CloneDeclaration, error) {
	if projectWorkDir == "" {
		return nil, nil
	}
	branch := resolveSessionBaseBranchFn(projectWorkDir)
	if branch == "" {
		return nil, fmt.Errorf(
			"cannot resolve default branch of project dir %q for sandbox-internal clone "+
				"(detached HEAD, not a git repository, or a corrupted checkout); "+
				"check out a branch (`git checkout <branch>`) or verify the project dir "+
				"is a valid clone with a default branch before starting a session/exec",
			projectWorkDir,
		)
	}
	return &orchestrator.CloneDeclaration{
		Branch:       branch,
		BaseBranch:   branch,
		CheckoutOnly: true,
	}, nil
}

// resolveSessionBaseBranch resolves the branch a task-less job should clone
// and check out, since (unlike a hook/task job) there is no Task.BaseBranch
// to declare. Reads the host project dir's current HEAD. Tolerates both real
// git's `--short` output ("main") and a detached/prefixed form
// ("refs/heads/main") defensively, since e2e's fake host git shim
// (e2e/fixtures/hostbin/git) does not honour --short. Returns "" when HEAD
// cannot be resolved at all (caller buildSessionCloneDeclaration turns that
// into a hard error).
func resolveSessionBaseBranch(projectWorkDir string) string {
	if projectWorkDir == "" {
		return ""
	}
	out, err := exec.Command("git", "-C", projectWorkDir, "symbolic-ref", "--quiet", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	branch := strings.TrimSpace(string(out))
	return strings.TrimPrefix(branch, "refs/heads/")
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
// Propagates any error from BuildSessionJobSpec (in particular a session
// clone-declaration failure — see buildSessionCloneDeclaration).
func BuildExecJobSpec(input SessionJobInput, argv []string, interactive bool) (*orchestrator.JobSpec, error) {
	input.HarnessType = "shell"
	if input.DisplayName == "" && len(argv) > 0 {
		input.DisplayName = argv[0]
	}
	spec, err := BuildSessionJobSpec(input)
	if err != nil {
		return nil, err
	}
	spec.Kind = orchestrator.JobKindExec
	spec.Argv = argv
	spec.Interactive = interactive
	return spec, nil
}
