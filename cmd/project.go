package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/initwizard"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

// --workspace flag values for project add / project init.
var projectAddWorkspace string
var projectInitWorkspace string

// --name flag for project add: overrides the project name otherwise derived
// from the git URL's last path component.
var projectAddName string

// --agent flag value for project init; empty falls back to initwizard.DefaultAgent.
var projectInitAgent string

var projectCmd = &cobra.Command{
	Use:   "project",
	Short: "Manage projects",
}

// projectAddCmd accepts ONLY a git remote URL — the legacy host-directory
// registration form (`boid project add <dir>`) has been removed; migrate
// an existing one with `project rm` + `project add <url>`.
//
// The daemon-side dir-based endpoint (POST /api/projects,
// internal/api.ProjectAppService.CreateProject) is NOT removed — this
// package's own test suite (and internal/server's) still uses it directly
// as a lightweight test-fixture registration primitive. Only this CLI
// command's dir-taking form is gone.
//
// looksLikeGitURL gates the accept/reject decision, to produce a clear,
// specific rejection message for an argument that is NOT a git URL.
var projectAddCmd = &cobra.Command{
	Use:   "add <git-url>",
	Short: "Register a project from a git remote URL",
	Long: `Register a project from a git remote URL:

  boid project add <git-url> --workspace=<name> [--name=<project-name>]

Has the daemon clone the URL itself into a daemon-managed bare repository
(docs/plans/volume-only-daemon.md §論点a/b). --workspace is required;
--name overrides the project name otherwise derived from the URL's last
path component.

A git URL is recognized by an explicit scheme (https://, ssh://, git://,
...) or the scp-like form (user@host:path, e.g.
git@github.com:owner/repo.git). Anything else (including a bare relative or
absolute filesystem path) is rejected — host-directory registration was
removed; migrate an existing one with 'boid project rm <ref>' followed by
this command.
`,
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runProjectAdd,
}

var projectInitSubCmd = &cobra.Command{
	Use:   "init [dir]",
	Short: "Scaffold a new boid project and print the push-and-register next steps",
	Long: `Initialize a new boid project in the current directory (or [dir]).

Prompts for a project name, then writes .boid/project.yaml with the canonical
supervisor / executor task_behaviors (agent=claude-code by default).
project.yaml only holds id / name / task_behaviors / default_task_behavior —
runtime environment config (` + "`host_commands`" + ` / ` + "`env`" + ` / ` + "`additional_bindings`" + ` / ` + "`allowed_domains`" + `)
lives separately in a workspace; set it up with
` + "`boid workspace create/edit/import`" + `.

Does NOT register the project with the daemon — a fresh scaffold has not
been pushed anywhere yet, and registration only works from a git URL
(` + "`boid project add <git-url>`" + `, docs/plans/release-onboarding.md 穴 7). After the
scaffold is written, this command prints the exact next commands to run:
commit + push, then ` + "`boid project add <url> --workspace=<name>`" + `. --workspace here
only feeds that printed example command (defaulting to "default" when
omitted, so the printed command is directly runnable); it is NOT itself an
API call.

Example:
  boid project init                              # initialize in current dir
  boid project init ./my-project                 # initialize in ./my-project
  boid project init . --workspace main           # bake "--workspace=main" into the printed guidance
  boid project init . --agent codex              # bake a non-default agent
`,
	// scopeNeutral, not scopeLocal: runProjectInit never calls
	// client.FromContext or resolves a profile at all — it only
	// reads/writes [dir] on whatever host this CLI process itself runs on
	// and prints text. scopeLocal's own contract (root.go's
	// PersistentPreRunE, isLocalScope) is stricter than that: it
	// hard-rejects the command outright whenever the active profile is a
	// remote (https-scheme) one, BEFORE RunE ever runs — appropriate for a
	// command whose job genuinely depends on "this daemon, this host"
	// (start/stop/gc), but not for one that ignores the profile entirely.
	// scopeNeutral (the same classification login/logout use,
	// cmd/login.go) is the correct fit: "requires no profile resolution
	// at all."
	//
	// annotationSkipAutostart=skip: runProjectInit does not talk to the
	// daemon in ANY way. Left unset, a user with no daemon running yet (or
	// BOID_NO_AUTOSTART=1, or a non-default socket) would have `project
	// init` fail or spin up an unwanted bare-host daemon before ever
	// reaching the wizard, for a command that needs one for nothing.
	Args:        cobra.MaximumNArgs(1),
	Annotations: map[string]string{scopeAnnotationKey: scopeNeutral, annotationSkipAutostart: "skip"},
	RunE:        runProjectInit,
}

var projectListCmd = &cobra.Command{
	Use:         "list",
	Short:       "List registered projects",
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runProjectList,
}

var projectRemoveCmd = &cobra.Command{
	Use:               "remove <project-ref>",
	Short:             "Remove a project (id or name, partial match supported)",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeProjectRefs,
	Annotations:       map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:              runProjectRemove,
}

// runProjectReload re-reads every registered project's .boid/project.yaml
// from its stored WorkDir and re-captures each one's git origin remote,
// both of which only resolve correctly against the daemon's own host
// filesystem — but the command itself is a pure POST /api/projects/reload
// call (client.FromContext below), so it is scopeRemote: the daemon-side
// filesystem work only makes sense on the daemon's own host, not "the CLI
// does any of that work itself".
var projectReloadCmd = &cobra.Command{
	Use:         "reload",
	Short:       "Reload project.yaml for all registered projects",
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runProjectReload,
}

// projectShowExplain is the --explain flag for `boid project show`: prints,
// per field, whether the effective value came from project.yaml, the
// linked workspace's default project definition, or is unset. Without the
// flag, `project show` still prints a one-line indicator when the project
// is affected by the workspace default at all.
var projectShowExplain bool

var projectShowCmd = &cobra.Command{
	Use:               "show <project-ref>",
	Short:             "Show project details (id or name, partial match supported)",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeProjectRefs,
	Annotations:       map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:              runProjectShow,
}

var projectBehaviorsCmd = &cobra.Command{
	Use:               "behaviors <project-ref>",
	Short:             "List task behaviors defined in the project (id or name, partial match supported)",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeProjectRefs,
	Annotations:       map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:              runProjectBehaviors,
}

// projectFetchCmd is scopeRemote: it runs `git fetch --all` inside a
// git-URL registered project's daemon-managed bare repo and reloads
// project.yaml — a plain HTTP API operation, same as `project
// show`/`project list`.
var projectFetchCmd = &cobra.Command{
	Use:               "fetch <project-ref>",
	Short:             "Fetch the latest git refs for a git-URL registered project and reload project.yaml",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeProjectRefs,
	Annotations:       map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:              runProjectFetch,
}

func init() {
	projectAddCmd.Flags().StringVar(&projectAddWorkspace, "workspace", "", "Workspace to register the project into (required for a git-URL <git-url>; optional, get-or-create, for a legacy <dir>)")
	projectAddCmd.Flags().StringVar(&projectAddName, "name", "", "Project name override, git-URL form only (default: derived from the URL's last path component)")
	projectInitSubCmd.Flags().StringVar(&projectInitWorkspace, "workspace", "", "Workspace name to bake into the printed `boid project add` guidance (no daemon call is made by project init itself)")
	projectInitSubCmd.Flags().StringVar(&projectInitAgent, "agent", "", "Harness agent baked into each behavior's default_instruction (default: claude-code)")
	projectShowCmd.Flags().BoolVar(&projectShowExplain, "explain", false, "Show field-by-field provenance (project.yaml vs workspace default vs unset)")

	// The recovery guidance both this file's own messages and
	// docs/{en,ja}/reference/cli.md give ("boid project rm <ref>") assumes
	// this alias.
	projectRemoveCmd.Aliases = []string{"rm"}

	projectCmd.AddCommand(projectAddCmd, projectInitSubCmd, projectListCmd, projectRemoveCmd, projectReloadCmd, projectFetchCmd, projectShowCmd, projectBehaviorsCmd)
	rootCmd.AddCommand(projectCmd)
}

// gitURLSchemePattern matches an explicit URL scheme ANCHORED at the start
// of the argument (https://, ssh://, git://, ...) — anchored so a plain
// relative path like "./https://repo" (a directory literally named
// "https:" one level down) is not misclassified as a URL just because
// "://" appears somewhere inside it.
var gitURLSchemePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)

// scpLikeGitURLPattern matches the scp-like SSH URL shape: an optional
// "user@" login prefix, a host, a literal ":", and then anything that is
// not itself a "/" — the "user@" prefix is optional since a bare
// "host:org/repo.git" is also a valid `git clone` source. Anchored at the
// start for the same reason as gitURLSchemePattern above.
var scpLikeGitURLPattern = regexp.MustCompile(`^([a-zA-Z0-9_.-]+@)?[a-zA-Z0-9_.-]+:[^/]`)

// looksLikeGitURL distinguishes a git remote URL from a host filesystem
// path BEFORE the CLI ever talks to the daemon, so `boid project add <dir>`
// fails fast with actionable guidance instead of a confusing daemon-side
// clone error (or, worse, git silently treating <dir> as a local clone
// source — a directory path IS technically a valid `git clone` source,
// which would register a filesystem path in an UpstreamURL field meant to
// hold a real remote). A URL is recognized by either an explicit scheme
// (`https://`, `ssh://`, ...) or the scp-like `[user@]host:path` form
// (`git@github.com:owner/repo.git`, or `github.com:owner/repo.git` with no
// explicit login user) — the two shapes the existing dispatcher URL parser
// (repoSlugFromOriginURL) already treats as the complete set of supported
// git URL forms.
func looksLikeGitURL(s string) bool {
	if gitURLSchemePattern.MatchString(s) {
		return true
	}
	return scpLikeGitURLPattern.MatchString(s)
}

// runProjectAdd rejects any argument that does not look like a git URL
// (looksLikeGitURL) — the legacy host-directory registration form this
// used to fall back to is gone (see projectAddCmd's own doc comment) —
// and otherwise registers it via runProjectAddGitURL.
func runProjectAdd(cmd *cobra.Command, args []string) error {
	arg := args[0]
	if !looksLikeGitURL(arg) {
		return fmt.Errorf("host directory registration was removed; use `boid project add <git-url> --workspace=<name>`")
	}
	return runProjectAddGitURL(cmd, arg)
}

func runProjectAddGitURL(cmd *cobra.Command, gitURL string) error {
	if projectAddWorkspace == "" {
		return fmt.Errorf("--workspace is required: boid project add <git-url> --workspace=<name>")
	}
	if err := projectspec.ValidWorkspaceSlug(projectAddWorkspace); err != nil {
		return fmt.Errorf("invalid --workspace value: %w", err)
	}

	c := client.FromContext(cmd.Context())

	// get-or-create the workspace client-side (same UX as project init's
	// --workspace flag / `boid workspace assign`) so a typo'd-but-plausible
	// new slug does not require a separate `boid workspace create` round
	// trip. The server-side CreateProjectFromGitURL still validates the
	// workspace exists (404 otherwise) as a belt-and-suspenders check —
	// see that method's own doc comment for why it does not itself
	// get-or-create (a git-URL registration has no host-dir fallback to
	// eagerly default into like the legacy CreateProject flow does).
	if err := ensureWorkspaceExistsGetOrCreate(c, projectAddWorkspace, cmd.OutOrStdout()); err != nil {
		return fmt.Errorf("get-or-create workspace %q: %w", projectAddWorkspace, err)
	}

	body := map[string]string{"url": gitURL, "workspace": projectAddWorkspace}
	if projectAddName != "" {
		body["name"] = projectAddName
	}

	var p projectspec.Project
	if err := c.Do("POST", "/api/projects/git", body, &p); err != nil {
		return fmt.Errorf("register project: %w", err)
	}

	return renderOutput(cmd, &p, func() error {
		fmt.Fprintf(cmd.OutOrStdout(), "project registered: %s (%s)\n", p.ID, p.Meta.Name)
		fmt.Fprintf(cmd.OutOrStdout(), "  workspace: %s\n", p.WorkspaceID)
		fmt.Fprintf(cmd.OutOrStdout(), "  bare repo: %s\n", p.WorkDir)
		// Check hook requires
		for _, b := range p.Meta.TaskBehaviors {
			for _, h := range b.Hooks {
				for _, req := range h.Requires {
					if _, err := exec.LookPath(req); err != nil {
						fmt.Fprintf(cmd.OutOrStdout(), "  warning: hook %q requires %q but it's not found in PATH\n", h.ID, req)
					}
				}
			}
		}
		return nil
	})
}

// runProjectFetch handles `boid project fetch <project-ref>`.
func runProjectFetch(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())

	p, err := resolveProjectRef(c, os.Stdin, cmd.OutOrStdout(), args[0])
	if err != nil {
		return fmt.Errorf("resolve project: %w", err)
	}

	var updated projectspec.Project
	if err := c.Do("POST", "/api/projects/"+p.ID+"/fetch", nil, &updated); err != nil {
		return fmt.Errorf("fetch project: %w", err)
	}

	return renderOutput(cmd, &updated, func() error {
		fmt.Fprintf(cmd.OutOrStdout(), "project fetched: %s (%s)\n", updated.ID, updated.Meta.Name)
		return nil
	})
}

// runProjectInit runs the interactive init wizard to scaffold
// .boid/project.yaml, then prints the "push it, then register the URL"
// three-step guidance.
//
// This does NOT also register the project with the daemon: under the
// volume-only compose daemon, a host-local projectDir does not exist inside
// the daemon's own container, so a work_dir-based registration call would
// always 400. The only registration path that actually works is POST
// /api/projects/git (CreateProjectFromGitURLRequest) via `boid project add
// <git-url>`, which requires the scaffold to already be pushed to a
// remote. So scaffolding and registration are two separate commands:
// project init's job ends at "write the scaffold, tell the user exactly
// what to run next", and registration is the explicit `project add` step
// the user runs after pushing.
func runProjectInit(cmd *cobra.Command, args []string) error {
	dir := "."
	if len(args) > 0 {
		dir = args[0]
	}

	projectDir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}

	// Validate --workspace BEFORE any side effect (scaffold write) below:
	// the printed guidance's `boid project add <url> --workspace=<slug>`
	// target itself requires ValidWorkspaceSlug too, so an invalid slug
	// like "Team_A" would let `project init` succeed and write the
	// scaffold, only for the exact command it just printed to
	// unconditionally fail.
	if projectInitWorkspace != "" {
		if err := projectspec.ValidWorkspaceSlug(projectInitWorkspace); err != nil {
			return fmt.Errorf("invalid --workspace value: %w", err)
		}
	}

	// Abort if project.yaml already exists.
	projectYAMLPath := filepath.Join(projectDir, ".boid", "project.yaml")
	if _, err := os.Stat(projectYAMLPath); err == nil {
		return fmt.Errorf(".boid/project.yaml already exists in %s; remove it first", projectDir)
	}

	// Reject projectDir being NESTED inside a DIFFERENT, already-existing
	// git repository: if projectDir has no .git of its own but sits below
	// some ANCESTOR directory's .git, the guidance's `git init` would
	// create a brand-new, unrelated repo nested inside that ancestor — a
	// push to the ancestor's existing remote either fails outright, or
	// ships a repo containing only the scaffold with none of the
	// ancestor's already-committed source. Caught before the scaffold is
	// even written.
	if err := rejectIfNestedInExistingRepo(projectDir); err != nil {
		return err
	}

	// Reject an already-existing repo currently in DETACHED HEAD state:
	// the printed guidance's `git push '<git-url>' HEAD` has no branch
	// name to infer a destination from when HEAD is detached, and the
	// earlier `git init` in that same chain does not check one out either
	// (a no-op on an already-initialized repo). Caught here, before the
	// scaffold commit, rather than leaving the user stuck mid-chain.
	if isDetachedHead(projectDir) {
		// A projectDir containing spaces or shell metacharacters would
		// otherwise break the suggested recovery command if copy-pasted
		// verbatim.
		return fmt.Errorf("%s is a git repository in detached HEAD state; check out (or create) a branch first — e.g. `git -C %s checkout -b <branch-name>` — then run `boid project init` again", projectDir, shellQuoteSingle(projectDir))
	}

	w := &initwizard.Wizard{
		In:    os.Stdin,
		Out:   cmd.OutOrStdout(),
		Agent: projectInitAgent,
	}

	if err := w.Run(projectDir); err != nil {
		return err
	}

	out := cmd.OutOrStdout()

	// Default to the "default" workspace when --workspace was not given,
	// so the printed command is directly runnable rather than a
	// placeholder like "--workspace=<workspace>" — --workspace is a
	// required flag on `project add`.
	workspaceSlug := "default"
	if projectInitWorkspace != "" {
		workspaceSlug = projectInitWorkspace
	}
	workspaceFlag := fmt.Sprintf("--workspace=%s", workspaceSlug)

	// cdPrefix makes every printed git command target projectDir
	// explicitly, regardless of the shell's own cwd when the user pastes
	// the guidance in — `[dir]` being a path other than "." would
	// otherwise mean the printed `git init`/`git add`/`git commit`
	// silently ran against the shell's cwd, not projectDir.
	cdPrefix := fmt.Sprintf("cd %s && ", shellQuoteSingle(projectDir))

	// A single, idempotent, one-shot chain — NOT a branch on whether
	// projectDir already has a repo/origin, and NOT touching the user's
	// `origin` remote configuration at all. The actual job here is
	// narrower than "reconcile the user's git remote setup": push the
	// scaffold to a URL, then tell the user to register that URL. A
	// one-shot `git push <url> HEAD` does exactly that without creating,
	// reading, or modifying any named remote — it works identically
	// whether `origin` is unset, already points elsewhere, or has any
	// pushurl overrides, since none of that is even consulted. Mutating or
	// depending on `origin` is deliberately avoided: it is PERSISTENT repo
	// state this command has no business assuming anything about (an
	// existing repository may deliberately have an HTTPS fetch / SSH push
	// split, multiple push mirrors, or simply an `origin` the user does
	// not want repointed just because they ran `project init`).
	//
	//   - `git init`: always safe to re-run ("Reinitialized existing Git
	//     repository..." if one already exists).
	//   - `git add .boid/project.yaml && (git diff --cached --quiet --
	//     .boid/project.yaml || git commit ... -- .boid/project.yaml)`:
	//     the pathspec is scoped to exactly the ONE file this command just
	//     wrote — a bare `.boid` pathspec would sweep in every OTHER file
	//     already under .boid too (this very repository's own .boid/ has
	//     files like run-e2e-scenario.sh alongside project.yaml). `git
	//     diff --cached --quiet -- .boid/project.yaml` is true exactly
	//     when there is nothing new to commit for that file, in which
	//     case the actual commit is skipped rather than run and rejected
	//     — if `git commit` DOES run and genuinely fails for a real
	//     reason (a rejecting pre-commit hook, no configured git
	//     identity, ...), that must stop the whole chain rather than
	//     pushing whatever HEAD already was.
	//   - `git push '<git-url>' HEAD`: a one-shot push straight to the
	//     URL, no `-u`/tracking, no named remote involved at all. Pushes
	//     the CURRENT branch under its own name — not a guess at "the
	//     remote's default branch", which `project init` has no way to
	//     know and no business overriding. If that branch is not what the
	//     remote treats as default, the daemon's own
	//     clone-and-read-project.yaml step reads .boid/project.yaml off
	//     the REMOTE'S default branch, not this one, and registration
	//     will miss the scaffold. There is no portable client-side git
	//     command that can query or set a remote's default branch/HEAD
	//     symref over the wire in the general case, so this is surfaced
	//     as an explicit precondition to verify (see the printed caveat
	//     below) rather than something silently handled here.
	//
	// Everything below `cd <dir> &&` is grouped in one `{ ...; }` block on
	// a SINGLE line, not multiple printed lines each expected to run in
	// projectDir: a bare `cd` on its own printed line only carries over to
	// LATER printed lines if the user pastes every line into the same
	// still-open shell — copied separately, re-run individually, or run
	// from a fresh terminal, a later `git push` on its own line would
	// silently target the wrong (or no) directory. Within the block,
	// EVERY step is joined with `&&`, not `;`: if `git init` itself
	// fails, falling through to `git add` anyway would let git's own
	// upward repository search silently operate on a PARENT repo instead.
	// The one exception is the intentionally SKIPPABLE "nothing to
	// commit" case, handled by the `git diff --cached --quiet ||` guard
	// itself exiting 0 (not by relaxing the outer `&&` chain) — an ACTUAL
	// commit failure still stops the chain before `git push` ever runs.
	//
	// '<git-url>' (single-quoted), not a bare <git-url>: this placeholder
	// is meant to be replaced in place by the user's real URL, but a bare
	// `<`/`>` is shell redirection syntax, and even a correctly
	// substituted URL can itself contain `&`, `;`, `#`, or other
	// characters a shell would otherwise split or reinterpret. Keeping
	// the quotes in the printed text means they naturally survive a
	// literal find-and-replace of the placeholder text with a real URL.
	// Known residual limitation: a URL that itself contains a literal
	// single quote could still break out of these quotes when
	// substituted in by hand — accepted, since no real git forge produces
	// a URL containing `'` in practice.
	//
	// Explicit "your actual code, not just the scaffold" caveat: the
	// chain below deliberately commits ONLY .boid/project.yaml — it does
	// NOT `git add .` the rest of the project. That is correct for a
	// genuinely brand-new, still-empty scaffold, but silently wrong for
	// `project init` run inside an existing, not-yet-pushed codebase: the
	// daemon clones whatever URL step 3 registers, and a remote
	// containing only .boid/project.yaml with no actual project source
	// leaves every agent dispatched against it with nothing to work on.
	// This can't safely be automated here (a blanket `git add .`
	// reintroduces the "sweeps in unrelated/sensitive staged content"
	// problem the file-scoped pathspec above avoids), so it is surfaced
	// as an explicit step instead.
	//
	// Push the CURRENT branch under its own name — NEVER force content
	// onto the remote's default branch directly: on an established
	// repository with branch protection, that either gets rejected
	// outright, or — worse, if it succeeds — ships every commit on the
	// current feature branch (not just the scaffold) straight onto the
	// protected default branch, bypassing PR review entirely. Landing the
	// scaffold on the actual default branch is the USER's call (a
	// PR/merge, exactly like any other change), not something this
	// command should force unilaterally.
	//
	// What this CAN safely do is tell the user whether they need to take
	// that extra step at all: `git ls-remote --symref <url> HEAD`
	// determines the remote's actual default branch (the same mechanism
	// internal/dispatcher/bare_repo.go already uses server-side) purely
	// to WARN, after the push, if the branch just pushed differs from it
	// — never to redirect the push itself.
	//
	// Checked AFTER the push, not before: a genuinely fresh, empty
	// repository on a REAL forge (GitHub, GitLab, ...) has its default
	// branch auto-set by the forge's own server-side logic essentially
	// immediately once the first push lands — so re-querying ls-remote
	// AFTER pushing, not just once beforehand, is what lets a real
	// forge's freshly-set default resolve at all. That same re-query is
	// also what catches a plain self-hosted bare git server (`git init
	// --bare` with no forge layered on top), which never auto-sets its
	// HEAD symref from a push — pushing "main" to one whose HEAD still
	// points at the never-created "master" leaves HEAD dangling even
	// after a successful push. An EMPTY $DEFAULT_REF after the push (as
	// opposed to a MISMATCHED one) means exactly this: not "a fresh repo
	// that will sort itself out", but a remote with no resolvable default
	// at all, which registration would otherwise silently fail against.
	fmt.Fprintln(out, "\nNext steps:")
	fmt.Fprintln(out, "  1. Make sure your project's actual source code — not just this scaffold — is already committed and pushed to your remote. The daemon clones whatever URL you register in step 3, and an agent dispatched against it needs real code to work with.")
	fmt.Fprintln(out, "  2. Commit the scaffold and push it to your remote (safe to run even if some of this is already done):")
	fmt.Fprintf(out, `       %s{ git init && git add .boid/project.yaml && (git diff --cached --quiet -- .boid/project.yaml || git commit -m 'add boid project scaffold' -- .boid/project.yaml) && git push '<git-url>' HEAD && { DEFAULT_REF=$(git ls-remote --symref '<git-url>' HEAD 2>/dev/null | awk '$1=="ref:"{print $2}'); CURRENT_REF="refs/heads/$(git symbolic-ref --short HEAD)"; if [ -z "$DEFAULT_REF" ]; then echo "WARNING: this remote has no resolvable default branch (HEAD) even after this push -- a real forge (GitHub/GitLab/...) auto-sets one from a first push to an empty repository, but a plain self-hosted bare git server does not. Set it there (e.g. git symbolic-ref HEAD refs/heads/<branch> run directly on the remote, or the forge's settings UI/API) before running step 3, or registration will fail." >&2; elif [ "$DEFAULT_REF" != "$CURRENT_REF" ]; then echo "WARNING: pushed to $CURRENT_REF, but this remote's default branch is $DEFAULT_REF -- merge/PR it there before running step 3, or the daemon will not see .boid/project.yaml" >&2; fi; }; }`+"\n", cdPrefix)
	fmt.Fprintln(out, "  3. Register the pushed URL with the running boid daemon:")
	fmt.Fprintf(out, "       boid project add '<git-url>' %s\n", workspaceFlag)

	return nil
}

// shellQuoteSingle wraps s in single quotes for safe use in a POSIX shell
// command line, escaping any embedded single quote as '\” (close quote,
// escaped literal quote, reopen quote) — the standard technique, since a
// single-quoted string cannot itself contain an unescaped single quote.
func shellQuoteSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// gitTopLevel returns the resolved top-level directory of the git
// repository dir sits in (which may be dir itself, or an ANCESTOR of it —
// `git rev-parse --show-toplevel` walks upward same as any other git
// command run with cwd=dir), or an error if dir is not inside a git
// repository at all. The result is symlink-resolved, since git always
// reports a symlink-resolved toplevel — callers comparing this against
// dir should resolve dir the same way first (e.g. via filepath.
// EvalSymlinks), or an otherwise-matching dir could spuriously compare
// unequal to its own toplevel (a /tmp path on a system where /tmp itself
// is a symlink, for instance).
func gitTopLevel(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("not a git repository: %w", err)
	}
	topLevel := strings.TrimSpace(string(out))
	if resolved, err := filepath.EvalSymlinks(topLevel); err == nil {
		return resolved, nil
	}
	return topLevel, nil
}

// rejectIfNestedInExistingRepo returns an error if projectDir sits inside
// an existing, DIFFERENT git repository — see its only caller's own doc
// comment (runProjectInit) for why that matters.
//
// Handles projectDir NOT EXISTING YET: `git -C projectDir rev-parse
// --show-toplevel` simply errors when projectDir doesn't exist —
// indistinguishable from "not nested in any repo" — so this walks UP to
// the nearest ALREADY-EXISTING ancestor and runs the git check there
// instead: if that ancestor turns out to already be inside a repo, then
// creating projectDir underneath it necessarily nests inside that same
// repo too, regardless of how many not-yet-existing path segments sit in
// between.
func rejectIfNestedInExistingRepo(projectDir string) error {
	existingAncestor := projectDir
	for {
		if _, err := os.Stat(existingAncestor); err == nil {
			break
		}
		parent := filepath.Dir(existingAncestor)
		if parent == existingAncestor {
			break // reached the filesystem root without finding an existing dir
		}
		existingAncestor = parent
	}

	if existingAncestor == projectDir {
		// projectDir itself already exists: the one EXCEPTION is
		// projectDir being a git repo's own root (the "user already ran
		// git init themselves" starter scenario) — only a DIFFERENT,
		// enclosing repo is rejected.
		resolvedProjectDir := projectDir
		if resolved, err := filepath.EvalSymlinks(projectDir); err == nil {
			resolvedProjectDir = resolved
		}
		if enclosingRoot, err := gitTopLevel(projectDir); err == nil && enclosingRoot != resolvedProjectDir {
			return fmt.Errorf("%s is nested inside an existing git repository rooted at %s; run `boid project init` from that repository's root instead (or a location with no enclosing git repo at all)", projectDir, enclosingRoot)
		}
		return nil
	}

	// projectDir does not exist yet: it cannot possibly be a repo's own
	// root (nothing there to have a .git of its own), so ANY enclosing
	// repo found from its nearest existing ancestor means creating it
	// would nest inside that repo.
	if enclosingRoot, err := gitTopLevel(existingAncestor); err == nil {
		return fmt.Errorf("%s does not exist yet and would be nested inside an existing git repository rooted at %s; run `boid project init` from that repository's root instead (or a location with no enclosing git repo at all)", projectDir, enclosingRoot)
	}
	return nil
}

// isDetachedHead reports whether dir is a git repository currently
// checked out in detached HEAD state (HEAD points directly at a commit,
// not a branch ref) — see its only caller's own doc comment
// (runProjectInit) for why that matters. Returns false for anything else,
// including dir not being a git repository at all (that's the
// not-yet-`git init`'d starter scenario, an entirely different — and
// harmless — case).
func isDetachedHead(dir string) bool {
	err := exec.Command("git", "-C", dir, "symbolic-ref", "-q", "HEAD").Run()
	if err == nil {
		return false // HEAD resolves to a branch ref: not detached.
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		// `git symbolic-ref -q` exits 1 specifically when HEAD is NOT a
		// symbolic ref (detached) — any other error (not a repo, git not
		// found, ...) is a different situation entirely and reported as
		// "not detached" here since detecting it is not this function's
		// job.
		return true
	}
	return false
}

func runProjectList(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())

	var projects []projectspec.Project
	if err := c.Do("GET", "/api/projects", nil, &projects); err != nil {
		return err
	}

	return renderOutput(cmd, projects, func() error {
		if len(projects) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "no projects registered")
			return nil
		}
		for _, p := range projects {
			status := p.Status
			if status == "" {
				status = "ready"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%-20s %-9s %s  (%s)", p.ID, status, p.Meta.Name, p.WorkDir)
			if p.UpstreamURL != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "  upstream=%s", p.UpstreamURL)
			}
			fmt.Fprintln(cmd.OutOrStdout())
			if p.Status != "" && p.StatusMessage != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", p.StatusMessage)
			}
		}
		return nil
	})
}

func runProjectRemove(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())

	p, err := resolveProjectRef(c, os.Stdin, cmd.OutOrStdout(), args[0])
	if err != nil {
		return fmt.Errorf("resolve project: %w", err)
	}

	var result map[string]string
	if err := c.Do("DELETE", "/api/projects/"+p.ID, nil, &result); err != nil {
		return fmt.Errorf("remove project: %w", err)
	}

	return renderOutput(cmd, map[string]any{"id": p.ID, "removed": true}, func() error {
		fmt.Fprintf(cmd.OutOrStdout(), "project removed: %s\n", p.ID)
		return nil
	})
}

func runProjectReload(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())

	var result map[string]any
	if err := c.Do("POST", "/api/projects/reload", nil, &result); err != nil {
		return fmt.Errorf("reload: %w", err)
	}

	return renderOutput(cmd, result, func() error {
		status := result["status"]
		fmt.Fprintf(cmd.OutOrStdout(), "reload: %s\n", status)
		if errs, ok := result["errors"]; ok {
			for _, e := range errs.([]any) {
				fmt.Fprintf(cmd.OutOrStdout(), "  error: %s\n", e)
			}
		}
		return nil
	})
}

func runProjectShow(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())

	p, err := resolveProjectRef(c, os.Stdin, cmd.OutOrStdout(), args[0])
	if err != nil {
		return fmt.Errorf("get project: %w", err)
	}

	// Always fetch the explain view: even without --explain, at least a
	// one-line indicator is shown when the project is affected by the
	// workspace default at all. Fetch failure here (e.g. talking to an
	// older daemon that has no /explain route) must not break plain
	// `project show` — it just means the indicator/--explain output is
	// silently skipped.
	var explain *projectspec.ProjectExplain
	if e, eerr := fetchProjectExplain(c, p.ID); eerr == nil {
		explain = e
	} else if projectShowExplain {
		return fmt.Errorf("get project explain: %w", eerr)
	}

	// Structured output (--format json/yaml) must reflect BOTH tiers
	// plain-text output has — the always-on one-line-indicator tier
	// (WorkspaceDefaultApplied / NoYAMLProject, set whenever the fetch
	// above succeeded) and the --explain full-detail tier (Explain, set
	// ONLY when projectShowExplain is true, so structured output actually
	// differs with vs. without --explain). out embeds *projectspec.Project
	// so its fields are still promoted to the top level for existing
	// `project show -o json/yaml` consumers.
	out := &projectShowOutput{Project: p}
	if explain != nil {
		out.WorkspaceDefaultApplied = explain.AnyWorkspaceDefaultApplied
		out.NoYAMLProject = explain.IsNoYAMLProject
		if projectShowExplain {
			out.Explain = explain
		}
	}

	return renderOutput(cmd, out, func() error {
		renderProjectDetail(p)
		if explain != nil {
			renderProjectExplainSummary(explain)
			if projectShowExplain {
				renderProjectExplainDetail(explain)
			}
		}
		return nil
	})
}

// projectShowOutput is `project show`'s structured-output (--format
// json/yaml) shape: the project itself, the two always-on summary
// indicators (mirroring the plain-text one-liner), and — only with
// --explain — the full field-provenance view.
//
// Project is embedded with `yaml:",inline"` so YAML marshaling promotes its
// fields to the top level: gopkg.in/yaml.v3, unlike encoding/json, does NOT
// inline an anonymous struct field by default — omitting this tag would
// silently change `project show -o yaml`'s top-level project shape into a
// nested `project:` key. JSON needs no equivalent tag; encoding/json
// promotes anonymous struct fields unconditionally.
type projectShowOutput struct {
	*projectspec.Project `yaml:",inline"`

	// WorkspaceDefaultApplied / NoYAMLProject mirror
	// ProjectExplain.AnyWorkspaceDefaultApplied /.IsNoYAMLProject — set
	// whenever the /explain fetch succeeded, regardless of --explain, so a
	// scripted caller gets the same minimum signal the plain-text one-liner
	// conveys without needing --explain.
	WorkspaceDefaultApplied bool `json:"workspace_default_applied,omitempty" yaml:"workspace_default_applied,omitempty"`
	NoYAMLProject           bool `json:"no_yaml_project,omitempty" yaml:"no_yaml_project,omitempty"`

	// Explain is the full per-field provenance view — set ONLY when
	// --explain was passed, so structured output actually differs with vs.
	// without --explain.
	Explain *projectspec.ProjectExplain `json:"explain,omitempty" yaml:"explain,omitempty"`
}

func fetchProjectExplain(c *client.Client, projectID string) (*projectspec.ProjectExplain, error) {
	var explain projectspec.ProjectExplain
	if err := c.Do("GET", "/api/projects/"+projectID+"/explain", nil, &explain); err != nil {
		return nil, err
	}
	return &explain, nil
}

// renderProjectExplainSummary prints the doc's required minimum: a single
// line, shown even without --explain, when the project is affected by the
// workspace default at all.
func renderProjectExplainSummary(e *projectspec.ProjectExplain) {
	if e.AnyWorkspaceDefaultApplied {
		fmt.Println("Note:        one or more fields are inherited from the workspace default (see `project show --explain`)")
	}
	if e.WorkspaceUnavailable {
		// A real workspace-load failure must be visible even when
		// project.yaml supplies every scalar field itself (so
		// AnyWorkspaceDefaultApplied is false and none of the per-field
		// provenance lines would otherwise mention it) — otherwise the
		// operator sees an apparently-complete, apparently-healthy
		// explanation despite the daemon having failed to read the
		// workspace at all.
		fmt.Println("Note:        the linked workspace's default project definition could not be read (see `project show --explain`)")
	}
	if e.IsNoYAMLProject {
		fmt.Println("Note:        this project has no project.yaml — its id was derived from the git URL, not read from a committed `id:` field")
		nameSource := e.NameSource
		if nameSource == "" {
			nameSource = "(could not be recovered)"
		}
		fmt.Printf("Note:        its name source is %q (see `project show --explain` for detail)\n", nameSource)
	}
}

// renderProjectExplainDetail prints the full --explain breakdown: per-field
// provenance (project.yaml / workspace default / unset), the base_branch
// snapshot-timing caveat, and (for a no-YAML project) the name recovery
// source.
func renderProjectExplainDetail(e *projectspec.ProjectExplain) {
	fmt.Println("\nExplain:")
	if e.WorkspaceUnavailable {
		// Surface this explicitly (not just via individual field values
		// reading "workspace unavailable" below) so it can't be missed when
		// project.yaml happens to supply every scalar field itself.
		fmt.Println("  WARNING: the linked workspace's default project definition could not be read; any field below reading \"workspace unavailable\" means this function genuinely does not know whether a workspace default would apply — not that none does.")
	}
	fmt.Printf("  base_branch:           %s\n", e.BaseBranch)
	fmt.Printf("  fork_point:            %s\n", e.ForkPoint)
	fmt.Printf("  default_task_behavior: %s\n", e.DefaultTaskBehavior)
	if len(e.TaskBehaviors) > 0 {
		fmt.Println("  task_behaviors:")
		for _, name := range e.TaskBehaviorNames() {
			fmt.Printf("    %-20s %s\n", name, e.TaskBehaviors[name])
		}
	}
	if e.IsNoYAMLProject {
		nameSource := e.NameSource
		if nameSource == "" {
			nameSource = "(could not be recovered)"
		}
		fmt.Printf("  id source:             derived from git URL (not project.yaml)\n")
		fmt.Printf("  name source:           %s\n", nameSource)
	}
	fmt.Printf("\n  %s\n", e.BaseBranchSnapshotNote)
}

func runProjectBehaviors(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())

	p, err := resolveProjectRef(c, os.Stdin, cmd.OutOrStdout(), args[0])
	if err != nil {
		return fmt.Errorf("get project: %w", err)
	}

	return renderOutput(cmd, p.Meta.TaskBehaviors, func() error {
		renderProjectBehaviors(p)
		return nil
	})
}

func renderProjectDetail(p *projectspec.Project) {
	fmt.Printf("ID:          %s\n", p.ID)
	fmt.Printf("Name:        %s\n", p.Meta.Name)
	fmt.Printf("WorkDir:     %s\n", p.WorkDir)
	fmt.Printf("WorkspaceID: %s\n", p.WorkspaceID)
	if p.UpstreamURL != "" {
		fmt.Printf("UpstreamURL: %s\n", p.UpstreamURL)
	} else {
		fmt.Printf("UpstreamURL: (none — add a git remote and run `boid project reload`)\n")
	}
	fmt.Printf("CreatedAt:   %s\n", formatTime(p.CreatedAt))
	fmt.Printf("UpdatedAt:   %s\n", formatTime(p.UpdatedAt))
	if p.Status != "" {
		// Status is omitted from the API response (and thus zero here) for
		// the common "ready" case — see orchestrator.Project.Status's doc
		// comment — so only print the line when there is something to say.
		fmt.Printf("Status:      %s\n", p.Status)
		if p.StatusMessage != "" {
			fmt.Printf("             %s\n", p.StatusMessage)
		}
	}

	m := p.Meta

	if len(m.TaskBehaviors) > 0 {
		fmt.Println("TaskBehaviors:")
		keys := make([]string, 0, len(m.TaskBehaviors))
		for k := range m.TaskBehaviors {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b := m.TaskBehaviors[k]
			fmt.Printf("  %-20s\n", k)
			for _, h := range b.Hooks {
				requires := ""
				if len(h.Requires) > 0 {
					requires = "  requires=[" + strings.Join(h.Requires, ",") + "]"
				}
				fmt.Printf("    hook: %s%s\n", h.ID, requires)
			}
		}
	}

	if len(m.HostCommands) > 0 {
		fmt.Println("HostCommands:")
		hcKeys := make([]string, 0, len(m.HostCommands))
		for k := range m.HostCommands {
			hcKeys = append(hcKeys, k)
		}
		sort.Strings(hcKeys)
		for _, k := range hcKeys {
			fmt.Printf("  %s\n", k)
		}
	}

	if len(m.AdditionalBindings) > 0 {
		fmt.Println("AdditionalBindings:")
		for _, b := range m.AdditionalBindings {
			fmt.Printf("  %s  (%s)\n", b.Source, b.Mode)
		}
	}

	if len(m.Env) > 0 {
		fmt.Println("Env:")
		envKeys := make([]string, 0, len(m.Env))
		for k := range m.Env {
			envKeys = append(envKeys, k)
		}
		sort.Strings(envKeys)
		for _, k := range envKeys {
			fmt.Printf("  %s\n", k)
		}
	}

	if m.SecretNamespace != "" {
		fmt.Printf("SecretNamespace: %s\n", m.SecretNamespace)
	}
}

func renderProjectBehaviors(p *projectspec.Project) {
	if len(p.Meta.TaskBehaviors) == 0 {
		fmt.Println("no behaviors defined")
		return
	}

	keys := make([]string, 0, len(p.Meta.TaskBehaviors))
	for k := range p.Meta.TaskBehaviors {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		b := p.Meta.TaskBehaviors[k]
		fmt.Printf("%-20s\n", k)
		if len(b.Traits) > 0 {
			fmt.Printf("  traits: %s\n", strings.Join(b.Traits, ", "))
		}
	}
	// Project-level base_branch: behavior-level readonly / worktree /
	// branch_prefix / base_branch and the project-top 'worktree' field are
	// gone, so only base_branch remains to display.
	if p.Meta.BaseBranch != "" {
		fmt.Printf("\nbase_branch: %s\n", p.Meta.BaseBranch)
	}
}
