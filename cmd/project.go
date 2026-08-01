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
	"github.com/novshi-tech/boid/internal/dispatcher"
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
	Short: "Initialize a new boid project interactively and register it",
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
only feeds that printed example command; it is NOT itself an API call.

Example:
  boid project init                              # initialize in current dir
  boid project init ./my-project                 # initialize in ./my-project
  boid project init . --workspace main           # bake "--workspace=main" into the printed guidance
  boid project init . --agent codex              # bake a non-default agent
`,
	// scopeLocal — same "境界越えで壊れる" rationale as projectAddCmd above:
	// [dir] is a local filesystem path the daemon resolves against its own
	// host.
	Args:        cobra.MaximumNArgs(1),
	Annotations: map[string]string{scopeAnnotationKey: scopeLocal},
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

	// Abort if project.yaml already exists.
	projectYAMLPath := filepath.Join(projectDir, ".boid", "project.yaml")
	if _, err := os.Stat(projectYAMLPath); err == nil {
		return fmt.Errorf(".boid/project.yaml already exists in %s; remove it first", projectDir)
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

	workspaceFlag := "--workspace=<workspace>"
	if projectInitWorkspace != "" {
		workspaceFlag = fmt.Sprintf("--workspace=%s", projectInitWorkspace)
	}

	// If projectDir is already a git repo with an `origin` remote (the user
	// ran `git init`/`git remote add` themselves before `project init`),
	// use that known URL directly instead of a <git-url> placeholder — one
	// less thing for the user to fill in by hand.
	fmt.Fprintln(out, "\nNext steps:")
	if originURL, err := dispatcher.CaptureUpstreamURL(projectDir); err == nil && originURL != "" {
		fmt.Fprintln(out, "  1. Commit the scaffold and push it to the remote:")
		fmt.Fprintln(out, "       git add .boid && git commit -m 'add boid project scaffold' && git push")
		fmt.Fprintln(out, "  2. Register the pushed URL with the running boid daemon:")
		fmt.Fprintf(out, "       boid project add %s %s\n", originURL, workspaceFlag)
	} else {
		fmt.Fprintln(out, "  1. Initialize git and push this project to a remote (skip what's already done):")
		fmt.Fprintln(out, "       git init && git add . && git commit -m 'initial commit'")
		fmt.Fprintln(out, "       git remote add origin <git-url> && git push -u origin HEAD")
		fmt.Fprintln(out, "  2. Register the pushed URL with the running boid daemon:")
		fmt.Fprintf(out, "       boid project add <git-url> %s\n", workspaceFlag)
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
