package cmd

import (
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
// from the git URL's last path component (docs/plans/volume-only-daemon.md
// §論点a).
var projectAddName string

// --agent flag value for project init; empty falls back to initwizard.DefaultAgent.
var projectInitAgent string

var projectCmd = &cobra.Command{
	Use:   "project",
	Short: "Manage projects",
}

// projectAddCmd accepts ONLY a git remote URL: PR-4 (docs/plans/
// volume-only-daemon.md §論点e) removed the legacy host-directory
// registration form (`boid project add <dir>`) that PR-2a had restored
// side by side with the git-URL form — the manual migration window
// (`project rm` + `project add <url>`, one project at a time) that form
// existed to support has closed; the e2e fixtures that used to depend on
// it were themselves removed in the same PR (docs/plans/
// volume-only-daemon.md §論点e's e2e coverage audit — every one of them
// exercised the now-removed userns backend anyway).
//
// The daemon-side dir-based endpoint (POST /api/projects,
// internal/api.ProjectAppService.CreateProject) is NOT removed — this
// package's own test suite (and internal/server's) still uses it directly
// as a lightweight test-fixture registration primitive, a legitimate
// "repurposed, not CLI-exposed" use PR-4's own scope explicitly allows. Only
// this CLI command's dir-taking form is gone.
//
// looksLikeGitURL still gates the accept/reject decision, now purely to
// produce a clear, specific rejection message for an argument that is NOT a
// git URL, rather than to pick between two working code paths.
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
	// scopeLocal: [dir] is a local filesystem path the wizard writes
	// .boid/project.yaml under directly (docs/plans/release-onboarding.md
	// 穴 7/PR6 — this command no longer sends [dir] to the daemon at all;
	// it only scaffolds locally and prints git-URL registration guidance),
	// so it only makes sense resolved against whatever host this CLI
	// process itself is running on.
	//
	// annotationSkipAutostart=skip (Blocker, codex round-16 review): this
	// command's whole job used to require a running daemon (the
	// POST /api/projects registration call this PR removed), which is
	// exactly why cmd/root.go's PersistentPreRunE autostarts a bare-host
	// daemon for scopeLocal commands lacking this annotation by default —
	// but runProjectInit no longer talks to the daemon in ANY way. Left
	// unset, a user with no daemon running yet (or BOID_NO_AUTOSTART=1,
	// or a non-default socket) would have `project init` fail or spin up
	// an unwanted bare-host daemon before ever reaching the wizard, for a
	// command that no longer needs one at all.
	Args:        cobra.MaximumNArgs(1),
	Annotations: map[string]string{scopeAnnotationKey: scopeLocal, annotationSkipAutostart: "skip"},
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

// projectReloadCmd is scopeLocal, grouped with add/init in the plan doc's
// "境界越えで壊れる" row even though reload itself takes no path argument —
// runProjectReload re-reads every registered project's .boid/project.yaml
// from its stored WorkDir and re-captures each one's git origin remote, both
// of which only resolve correctly against the daemon's own host filesystem.
var projectReloadCmd = &cobra.Command{
	Use:         "reload",
	Short:       "Reload project.yaml for all registered projects",
	Annotations: map[string]string{scopeAnnotationKey: scopeLocal},
	RunE:        runProjectReload,
}

// projectShowExplain is the --explain flag for `boid project show`
// (docs/plans/workspace-default-project.md 論点e, PR6): prints, per field,
// whether the effective value came from project.yaml, the linked
// workspace's default project definition, or is unset. Without the flag,
// `project show` still prints a one-line indicator when the project is
// affected by the workspace default at all (the doc's stated minimum).
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
// project.yaml (docs/plans/volume-only-daemon.md §論点b fetch 経路) — a
// plain HTTP API operation, same as `project show`/`project list`.
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

	// Minor 2 (PR-2a codex round-1 review): the recovery guidance both this
	// file's own messages and docs/{en,ja}/reference/cli.md give
	// ("boid project rm <ref>") assumed an alias that never actually
	// existed — the command was scoped as "remove" only. Add it for real.
	projectRemoveCmd.Aliases = []string{"rm"}

	projectCmd.AddCommand(projectAddCmd, projectInitSubCmd, projectListCmd, projectRemoveCmd, projectReloadCmd, projectFetchCmd, projectShowCmd, projectBehaviorsCmd)
	rootCmd.AddCommand(projectCmd)
}

// gitURLSchemePattern matches an explicit URL scheme ANCHORED at the start
// of the argument (https://, ssh://, git://, ...) — Minor 1, PR-2a codex
// round-2 review: the pre-fix strings.Contains(s, "://") searched anywhere
// in the string, so a plain relative path like "./https://repo" (a
// directory literally named "https:" one level down — an edge case, but a
// real one) was misclassified as a URL just because "://" appeared
// somewhere inside it.
var gitURLSchemePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)

// scpLikeGitURLPattern matches the scp-like SSH URL shape: an optional
// "user@" login prefix, a host, a literal ":", and then anything that is
// not itself a "/" (Minor 1, PR-2a codex round-2 review: the pre-fix check
// required an explicit "user@" prefix, so a bare "host:org/repo.git" — a
// perfectly valid `git clone` source with no explicit login user — was
// misclassified as a directory). Anchored at the start for the same reason
// as gitURLSchemePattern above: "./git@host:repo" (a directory) must not
// match just because a "user@host:" shape appears somewhere past the
// leading "./".
var scpLikeGitURLPattern = regexp.MustCompile(`^([a-zA-Z0-9_.-]+@)?[a-zA-Z0-9_.-]+:[^/]`)

// looksLikeGitURL is the "clear error" heuristic docs/plans/
// volume-only-daemon.md §論点a asks for: distinguish a git remote URL from
// a host filesystem path BEFORE the CLI ever talks to the daemon, so
// `boid project add <dir>` fails fast with actionable guidance instead of a
// confusing daemon-side clone error (or, worse, git silently treating <dir>
// as a local clone source — a directory path IS technically a valid `git
// clone` source, which would register a filesystem path in an UpstreamURL
// field meant to hold a real remote). A URL is recognized by either an
// explicit scheme (`https://`, `ssh://`, ...) or the scp-like
// `[user@]host:path` form (`git@github.com:owner/repo.git`, or
// `github.com:owner/repo.git` with no explicit login user) — the two
// shapes docs/plans/volume-only-daemon.md and the existing dispatcher URL
// parser (repoSlugFromOriginURL) both already treat as the complete set of
// supported git URL forms.
func looksLikeGitURL(s string) bool {
	if gitURLSchemePattern.MatchString(s) {
		return true
	}
	return scpLikeGitURLPattern.MatchString(s)
}

// runProjectAdd rejects any argument that does not look like a git URL
// (looksLikeGitURL) — PR-4 removed the legacy host-directory registration
// form this used to fall back to (see projectAddCmd's own doc comment) —
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

// runProjectFetch handles `boid project fetch <project-ref>` (docs/plans/
// volume-only-daemon.md §論点b fetch 経路).
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
// three-step guidance the plan doc's 目標オンボーディングフロー spells out
// (docs/plans/release-onboarding.md 穴 7 / PR6).
//
// This used to also register the project with the daemon, POSTing the
// host-local projectDir straight to POST /api/projects (the work_dir-based
// CreateProject). Under the volume-only compose daemon that directory does
// not exist inside the daemon's own container, so the call always 400'd —
// and the error-recovery message it printed on failure ("boid project add
// .") pointed at a form `project add` no longer even accepts (PR-4 removed
// host-directory registration entirely). The only registration path left
// that actually works is POST /api/projects/git
// (CreateProjectFromGitURLRequest) via `boid project add <git-url>`, and
// that requires the scaffold to already be pushed to a remote. So
// scaffolding and registration can no longer be one command: project init's
// job now ends at "write the scaffold, tell the user exactly what to run
// next", and registration itself is the separate, explicit `project add`
// step the user runs after pushing.
func runProjectInit(cmd *cobra.Command, args []string) error {
	dir := "."
	if len(args) > 0 {
		dir = args[0]
	}

	projectDir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}

	// Validate --workspace BEFORE any side effect (scaffold write) below
	// (Major, codex round-2 review of this PR): the printed guidance's
	// `boid project add <url> --workspace=<slug>` target itself requires
	// ValidWorkspaceSlug (runProjectAddGitURL, cmd/project.go's own
	// --workspace validation) — an invalid slug like "Team_A" would let
	// `project init` succeed and write the scaffold, only for the exact
	// command it just printed to unconditionally fail. Fail fast here
	// instead, before touching the filesystem at all.
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
	// git repository (Major, codex round-15 review): if projectDir has no
	// .git of its own but sits below some ANCESTOR directory's .git, the
	// guidance's `git init` would create a brand-new, unrelated repo
	// nested inside that ancestor — its own commit/push history has
	// nothing to do with the ancestor repo's, a push to the ancestor's
	// existing remote either fails outright (non-fast-forward/unrelated
	// histories) or, on a DIFFERENT remote, ships a repo containing only
	// the scaffold with none of the ancestor's already-committed source
	// (the same "no real code for an agent to work on" problem the
	// step-1 caveat above warns about, but silently self-inflicted
	// here). Caught before the scaffold is even written.
	if err := rejectIfNestedInExistingRepo(projectDir); err != nil {
		return err
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

	// The 目標オンボーディングフロー step 5 example registers into the
	// "default" workspace; do the same here when --workspace was not
	// given so the printed command is directly runnable (Major, codex
	// round-1 review of this PR: a placeholder like "--workspace=<workspace>"
	// is not "the exact next command" the help text promises, since
	// --workspace is a required flag on `project add`).
	workspaceSlug := "default"
	if projectInitWorkspace != "" {
		workspaceSlug = projectInitWorkspace
	}
	workspaceFlag := fmt.Sprintf("--workspace=%s", workspaceSlug)

	// cdPrefix makes every printed git command target projectDir
	// explicitly, regardless of the shell's own cwd when the user pastes
	// the guidance in (Blocker, codex round-1 review: `[dir]` being a
	// path other than "." meant the printed `git init`/`git add`/`git
	// commit` silently ran against the shell's cwd, not projectDir).
	cdPrefix := fmt.Sprintf("cd %s && ", shellQuoteSingle(projectDir))

	// A single, idempotent, one-shot chain — NOT a branch on whether
	// projectDir already has a repo/origin, and NOT touching the user's
	// `origin` remote configuration at all.
	//
	// Two earlier revisions of this guidance tried progressively more
	// clever things with git remote state: first detecting an existing
	// `origin` and printing its URL back (four rounds of review found
	// shell-injection, parent-repo misattribution, and file://-as-a-
	// real-remote bugs in that), then unconditionally creating/repointing
	// `origin` itself via `git remote add`/`set-url` plus normalizing
	// `remote.origin.pushurl` (two MORE rounds found a stale separate
	// push URL, then multiple stale push URLs, still reaching the wrong
	// remote). Every one of those bugs came from the same root cause:
	// mutating or depending on `origin`, a piece of PERSISTENT repo state
	// this command has no business assuming anything about, let alone
	// overwriting — an existing repository may deliberately have an HTTPS
	// fetch / SSH push split, multiple push mirrors, or simply an
	// `origin` the user does not want repointed just because they ran
	// `project init` (Major, codex round-12 review of the remote-
	// mutating design).
	//
	// The actual job here is narrower than "reconcile the user's git
	// remote setup": push the scaffold to a URL, then tell the user to
	// register that URL. A one-shot `git push <url> HEAD` does exactly
	// that without creating, reading, or modifying any named remote at
	// all — verified empirically to work identically whether `origin` is
	// unset, already points elsewhere, or has any pushurl overrides,
	// since none of that is even consulted.
	//
	//   - `git init`: always safe to re-run ("Reinitialized existing Git
	//     repository..." if one already exists).
	//   - `git add .boid/project.yaml && (git diff --cached --quiet --
	//     .boid/project.yaml || git commit ... -- .boid/project.yaml)`:
	//     the pathspec (Major, codex round-4 review, NARROWED to the
	//     single file in round-9 review — a bare `.boid` pathspec sweeps
	//     in every OTHER file already under .boid too, and an existing
	//     repository is not guaranteed to have only scaffold content
	//     there: this very repository's own .boid/ has files like
	//     run-e2e-scenario.sh alongside project.yaml) scopes the commit
	//     to exactly the ONE file this command just wrote, so it neither
	//     sweeps in unrelated already-staged changes (anywhere in the
	//     repo, or elsewhere under .boid) nor tries to commit when that
	//     one file has no staged changes at all (the idempotent-rerun
	//     case). `git diff --cached --quiet -- .boid/project.yaml` is
	//     true (no output, exit 0) exactly when there is nothing new to
	//     commit for that file — in that case the `||`'s right side (the
	//     actual commit) is skipped entirely, rather than run and
	//     rejected. Skipping vs. attempting-and-failing matters (Blocker,
	//     codex round-6 review): if `git commit` DOES run and genuinely
	//     fails for a real reason (a rejecting pre-commit hook, no
	//     configured git identity, a required GPG signature that can't
	//     be produced, ...), that must stop the whole chain — pushing
	//     whatever HEAD already was would silently register a project
	//     missing the scaffold this command just wrote. See the
	//     all-`&&` note below for how that's wired.
	//   - `git push '<git-url>' HEAD`: a one-shot push straight to the
	//     URL, no `-u`/tracking, no named remote involved at all (Major,
	//     codex round-12 review, replacing the previous origin-mutating
	//     design described above). Pushes the CURRENT branch under its
	//     own name — not a guess at "the remote's default branch", which
	//     `project init` has no way to know and no business overriding.
	//     Scope boundary (Blocker, codex round-6/round-8 review): if that
	//     branch is not what the remote treats as default, the daemon's
	//     own clone-and-read-project.yaml step (dispatcher.CloneBareRepo,
	//     project_bare_repo.go) reads .boid/project.yaml off the
	//     REMOTE'S default branch, not this one, and registration will
	//     miss the scaffold — regardless of WHY that branch differs from
	//     default (a fresh remote that never auto-set one, or an
	//     established remote whose default is simply a different branch
	//     than the one currently checked out). There is no portable
	//     client-side git command that can query OR set a remote's
	//     default branch/HEAD symref over the wire in the general case —
	//     only something running ON that remote (a forge's own UI/API,
	//     or direct server access) can, so this is surfaced as an
	//     explicit precondition to verify (see the printed caveat below)
	//     rather than something silently handled here.
	//
	// Everything below `cd <dir> &&` is grouped in one `{ ...; }` block on
	// a SINGLE line, not multiple printed lines each expected to run in
	// projectDir (Blocker, codex round-3 review of an earlier revision):
	// a bare `cd` on its own printed line only carries over to LATER
	// printed lines if the user happens to paste every line into the
	// same still-open shell — copied separately, re-run individually, or
	// run from a fresh terminal, a later `git push` on its own line would
	// silently target the wrong (or no) directory. Within the block,
	// EVERY step is joined with `&&`, not `;` (Blocker, codex round-10
	// review of an earlier revision that `;`-separated `git init` from
	// the rest): if `git init` itself fails — projectDir sits inside an
	// unrelated parent repository and permissions/git config make a
	// nested repo impossible, say — falling through to `git add` anyway
	// would let git's own upward repository search silently operate on
	// that PARENT repo instead, the exact "wrong repo" failure mode
	// Blocker 1 (round-1 review) already fixed once for the origin-URL
	// misattribution case. A genuine failure at ANY step here must stop
	// the whole chain. The one exception remains the intentionally
	// SKIPPABLE "nothing to commit" case, which is handled by the `git
	// diff --cached --quiet ||` guard itself making that inner group
	// exit 0 (not by relaxing the outer `&&` chain) — so an ACTUAL commit
	// failure (a rejecting hook, no git identity, ...) still stops the
	// chain before `git push` ever runs.
	// '<git-url>' (single-quoted), not a bare <git-url> (Major, codex
	// round-7 review): this placeholder is meant to be replaced in place
	// by the user's real URL, but left as-is or replaced carelessly a
	// bare `<`/`>` is shell redirection syntax, and even a correctly
	// substituted URL can itself contain `&`, `;`, `#`, or other
	// characters a shell would otherwise split or reinterpret. Keeping
	// the quotes in the printed text means they naturally survive a
	// literal find-and-replace of the placeholder text with a real URL.
	// Explicit "your actual code, not just the scaffold" caveat (Major,
	// codex round-13 review): the chain below deliberately commits ONLY
	// .boid/project.yaml (the file-scoped pathspec fix from round-9,
	// still load-bearing to avoid sweeping in unrelated staged changes)
	// — it does NOT `git add .` the rest of the project. That is
	// correct for a genuinely brand-new, still-empty scaffold (nothing
	// else exists yet to accidentally leave behind), but silently wrong
	// for `project init` run inside an existing, not-yet-pushed
	// codebase: the daemon clones whatever URL step 3 registers, and a
	// remote containing only .boid/project.yaml with no actual project
	// source leaves every agent dispatched against it with nothing to
	// work on. This can't safely be automated here (a blanket `git add
	// .` reintroduces exactly the "sweeps in unrelated/sensitive staged
	// content" problem round 4/9 fixed for the scaffold's own commit),
	// so it is surfaced as an explicit step instead.
	// Push the CURRENT branch under its own name — NEVER force content
	// onto the remote's default branch directly (Blocker, codex
	// round-16 review of an earlier revision that pushed `HEAD:
	// $DEFAULT_REF` unconditionally): on an established repository with
	// branch protection, that either gets rejected outright, or — worse,
	// if it succeeds — ships every commit on the current feature branch
	// (not just the scaffold) straight onto the protected default
	// branch, bypassing PR review entirely. Landing the scaffold on the
	// actual default branch is the USER's call (a PR/merge, exactly like
	// any other change), not something this command should force
	// unilaterally.
	//
	// What this CAN safely do is tell the user whether they need to take
	// that extra step at all: `git ls-remote --symref <url> HEAD`
	// determines the remote's actual default branch (round-15 review
	// corrected an earlier claim that no portable client-side git
	// command could do this — this is the same mechanism internal/
	// dispatcher/bare_repo.go already uses server-side) purely to WARN,
	// after the push, if the branch just pushed differs from it — never
	// to redirect the push itself. Verified empirically: a brand-new
	// empty remote (no resolvable HEAD yet) prints no warning at all; an
	// established repo with the local checkout on a different feature
	// branch prints the warning while leaving the remote's actual
	// default branch completely untouched.
	fmt.Fprintln(out, "\nNext steps:")
	fmt.Fprintln(out, "  1. Make sure your project's actual source code — not just this scaffold — is already committed and pushed to your remote. The daemon clones whatever URL you register in step 3, and an agent dispatched against it needs real code to work with.")
	fmt.Fprintln(out, "  2. Commit the scaffold and push it to your remote (safe to run even if some of this is already done):")
	fmt.Fprintf(out, `       %s{ git init && git add .boid/project.yaml && (git diff --cached --quiet -- .boid/project.yaml || git commit -m 'add boid project scaffold' -- .boid/project.yaml) && git push '<git-url>' HEAD && { DEFAULT_REF=$(git ls-remote --symref '<git-url>' HEAD 2>/dev/null | awk '$1=="ref:"{print $2}'); CURRENT_REF="refs/heads/$(git symbolic-ref --short HEAD)"; if [ -n "$DEFAULT_REF" ] && [ "$DEFAULT_REF" != "$CURRENT_REF" ]; then echo "WARNING: pushed to $CURRENT_REF, but this remote's default branch is $DEFAULT_REF -- merge/PR it there before running step 3, or the daemon will not see .boid/project.yaml" >&2; fi; }; }`+"\n", cdPrefix)
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
// Handles projectDir NOT EXISTING YET (Major, codex round-16 review of an
// earlier revision): `git -C projectDir rev-parse --show-toplevel` simply
// errors when projectDir doesn't exist — indistinguishable from "not
// nested in any repo" — so `boid project init ./brand-new-subdir` inside
// an existing repo sailed through this check uncaught (Wizard.Run creates
// the directory itself via MkdirAll, after this check already passed).
// Fixed by walking UP to the nearest ALREADY-EXISTING ancestor and running
// the git check there instead: if that ancestor turns out to already be
// inside a repo, then creating projectDir underneath it necessarily nests
// inside that same repo too, regardless of how many not-yet-existing path
// segments sit in between.
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

	// Always fetch the explain view (docs/plans/workspace-default-project.md
	// 論点e, PR6): even without --explain, the doc requires at least a
	// one-line indicator when the project is affected by the workspace
	// default at all. Fetch failure here (e.g. talking to a pre-PR6 daemon
	// that has no /explain route) must not break plain `project show` — it
	// just means the indicator/--explain output is silently skipped.
	var explain *projectspec.ProjectExplain
	if e, eerr := fetchProjectExplain(c, p.ID); eerr == nil {
		explain = e
	} else if projectShowExplain {
		return fmt.Errorf("get project explain: %w", eerr)
	}

	// Codex review round 1 Major / round 2 Major: structured output (--format
	// json/yaml) must reflect BOTH tiers plain-text output has — the
	// always-on one-line-indicator tier (WorkspaceDefaultApplied /
	// NoYAMLProject, set whenever the fetch above succeeded) and the
	// --explain full-detail tier (Explain, set ONLY when projectShowExplain
	// is true — round 2 caught that the first fix always populated Explain
	// regardless of the flag, so structured output no longer differed with
	// vs. without --explain). out embeds *projectspec.Project so its fields
	// are still promoted to the top level for existing `project show -o
	// json/yaml` consumers.
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
// --explain — the full field-provenance view (docs/plans/
// workspace-default-project.md 論点e, PR6).
//
// Project is embedded with `yaml:",inline"` so YAML marshaling promotes its
// fields to the top level: gopkg.in/yaml.v3, unlike encoding/json, does NOT
// inline an anonymous struct field by default — omitting this tag was a
// round 2 Codex review Major (it silently changed `project show -o yaml`'s
// existing top-level project shape into a nested `project:` key). JSON needs
// no equivalent tag; encoding/json promotes anonymous struct fields
// unconditionally.
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
	// --explain was passed (round 2 Codex review Major: the original fix set
	// this unconditionally, so structured output did not actually differ
	// with vs. without --explain).
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
		// Codex review round 2 Major: a real workspace-load failure must be
		// visible even when project.yaml supplies every scalar field itself
		// (so AnyWorkspaceDefaultApplied is false and none of the per-field
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
		// Codex review round 2 Major: surface this explicitly (not just via
		// individual field values reading "workspace unavailable" below) so
		// it can't be missed when project.yaml happens to supply every
		// scalar field itself.
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
	// Project-level base_branch (Phase 3-1: behavior-level readonly / worktree /
	// branch_prefix / base_branch are gone; branch-policy-simplification Phase 2
	// additionally retired the project-top 'worktree' field, so only
	// base_branch remains to display).
	if p.Meta.BaseBranch != "" {
		fmt.Printf("\nbase_branch: %s\n", p.Meta.BaseBranch)
	}
}
