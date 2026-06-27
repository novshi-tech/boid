package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/config"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/skills"
	"github.com/spf13/cobra"
)

var workspaceCmd = &cobra.Command{
	Use:   "workspace",
	Short: "Manage local workspace groupings",
}

var workspaceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List workspaces (yaml dir scan + DB scan union)",
	RunE:  runWorkspaceList,
}

var workspaceShowCmd = &cobra.Command{
	Use:   "show <slug>",
	Short: "Show workspace.yaml contents and assigned projects",
	Args:  cobra.ExactArgs(1),
	RunE:  runWorkspaceShow,
}

var workspaceAssignCmd = &cobra.Command{
	Use:   "assign <project-ref> <slug>",
	Short: "Assign a project to a workspace (get-or-create: creates DB row even for unknown slug)",
	Args:  cobra.ExactArgs(2),
	RunE:  runWorkspaceAssign,
}

var workspaceClearCmd = &cobra.Command{
	Use:   "clear <project-ref>",
	Short: "Reset a project's workspace assignment to the default workspace",
	Args:  cobra.ExactArgs(1),
	RunE:  runWorkspaceClear,
}

// workspaceConfigureExecFn runs the sandbox runner as a child process (fork+wait) and
// waits for it to complete. It is a package-level variable so tests can
// override it to intercept the launch without actually running a sandbox.
//
// Unlike syscall.Exec, this does NOT replace the current process — the caller
// regains control after the child exits, allowing post-run scanning logic to
// execute.
var workspaceConfigureExecFn = func(argv0 string, argv []string, envv []string) error {
	cmd := exec.Command(argv0, argv[1:]...)
	cmd.Env = envv
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// workspaceConfigureCmd launches a sandboxed agent session (ProfileInit) that
// reads the assigned projects and writes a workspace.yaml via the
// boid-workspace-configure skill. After the sandbox exits, the generated yaml
// is scanned for secrets; if any are found the original file is restored from
// backup and an error is returned.
//
// Unlike boid kit init, this command requires the daemon (workspace show + kit
// list are daemon API calls) and therefore does NOT carry annotationSkipAutostart.
var workspaceConfigureCmd = &cobra.Command{
	Use:   "configure <slug>",
	Short: "Configure a workspace via interactive agent session (boid-workspace-configure skill)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWorkspaceConfigure(cmd, args)
	},
}

var workspaceRemoveCmd = &cobra.Command{
	Use:   "remove <slug>",
	Short: "Remove a workspace (deletes workspace.yaml; errors if projects are still assigned)",
	Args:  cobra.ExactArgs(1),
	RunE:  runWorkspaceRemove,
}

func init() {
	workspaceCmd.AddCommand(
		workspaceListCmd,
		workspaceShowCmd,
		workspaceAssignCmd,
		workspaceClearCmd,
		workspaceConfigureCmd,
		workspaceRemoveCmd,
	)
	rootCmd.AddCommand(workspaceCmd)
}

// workspaceState classifies each slug in the union of yaml-dir and DB views.
type workspaceState string

const (
	workspaceStateReady        workspaceState = "ready"
	workspaceStateUnconfigured workspaceState = "unconfigured"
	workspaceStateEmpty        workspaceState = "empty"
)

type workspaceEntry struct {
	Slug         string         `json:"slug"`
	State        workspaceState `json:"state"`
	ProjectCount int            `json:"project_count"`
}

func runWorkspaceList(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())

	// DB scan — workspaces that have at least one assigned project.
	var dbWorkspaces []*orchestrator.WorkspaceSummary
	if err := c.Do("GET", "/api/workspaces", nil, &dbWorkspaces); err != nil {
		return fmt.Errorf("list workspaces (db): %w", err)
	}
	dbMap := make(map[string]int, len(dbWorkspaces))
	for _, ws := range dbWorkspaces {
		dbMap[ws.ID] = ws.ProjectCount
	}

	// yaml dir scan — workspaces with a workspace.yaml file.
	store := orchestrator.NewWorkspaceStore("")
	yamlSlugs, err := store.List()
	if err != nil {
		return fmt.Errorf("list workspaces (yaml): %w", err)
	}
	yamlSet := make(map[string]struct{}, len(yamlSlugs))
	for _, s := range yamlSlugs {
		yamlSet[s] = struct{}{}
	}

	// Union.
	allSlugs := make(map[string]struct{})
	for slug := range dbMap {
		allSlugs[slug] = struct{}{}
	}
	for slug := range yamlSet {
		allSlugs[slug] = struct{}{}
	}

	sorted := make([]string, 0, len(allSlugs))
	for slug := range allSlugs {
		sorted = append(sorted, slug)
	}
	sort.Strings(sorted)

	entries := make([]workspaceEntry, 0, len(sorted))
	for _, slug := range sorted {
		_, hasYAML := yamlSet[slug]
		count, hasDB := dbMap[slug]
		var state workspaceState
		switch {
		case hasYAML && hasDB:
			state = workspaceStateReady
		case !hasYAML && hasDB:
			state = workspaceStateUnconfigured
		default: // hasYAML && !hasDB
			state = workspaceStateEmpty
		}
		entries = append(entries, workspaceEntry{
			Slug:         slug,
			State:        state,
			ProjectCount: count,
		})
	}

	return renderOutput(cmd, entries, func() error {
		out := cmd.OutOrStdout()
		if len(entries) == 0 {
			fmt.Fprintln(out, "no workspaces configured")
			return nil
		}
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "SLUG\tSTATE\tPROJECTS")
		for _, e := range entries {
			fmt.Fprintf(tw, "%s\t%s\t%d\n", e.Slug, e.State, e.ProjectCount)
		}
		return tw.Flush()
	})
}

// workspaceShowView is the JSON shape for workspace show output.
type workspaceShowView struct {
	Slug     string                   `json:"slug"`
	Meta     *orchestrator.WorkspaceMeta `json:"meta,omitempty"`
	Projects []*orchestrator.Project  `json:"projects"`
	Warnings []string                 `json:"warnings,omitempty"`
}

func runWorkspaceShow(cmd *cobra.Command, args []string) error {
	slug := args[0]
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return err
	}

	c := client.NewUnixClient(client.DefaultSocketPath())

	// Load workspace.yaml (machine-local, CLI reads directly).
	store := orchestrator.NewWorkspaceStore("")
	meta, yamlErr := store.Load(slug)

	// Fetch assigned projects from daemon.
	var projects []*orchestrator.Project
	apiErr := c.Do("GET", "/api/projects?workspace_id="+slug, nil, &projects)
	if apiErr != nil {
		return fmt.Errorf("list projects: %w", apiErr)
	}
	if projects == nil {
		projects = []*orchestrator.Project{}
	}

	view := workspaceShowView{
		Slug:     slug,
		Meta:     meta,
		Projects: projects,
	}
	if errors.Is(yamlErr, os.ErrNotExist) || (yamlErr != nil && strings.Contains(yamlErr.Error(), os.ErrNotExist.Error())) {
		view.Warnings = append(view.Warnings,
			fmt.Sprintf("workspace.yaml: not found — run `boid workspace configure %s` to create", slug),
		)
	} else if yamlErr != nil {
		view.Warnings = append(view.Warnings, fmt.Sprintf("workspace.yaml: read error: %v", yamlErr))
	}
	// Mirror the yaml-side warning for the other half of the union (DB-side
	// empty). Plan: "片方欠落時に欠落側を明示" — the `empty` state (yaml is
	// present but no project is assigned) should be surfaced symmetrically to
	// the `unconfigured` state above.
	if meta != nil && len(projects) == 0 {
		view.Warnings = append(view.Warnings,
			fmt.Sprintf("project assignments: none — workspace is empty (run `boid workspace remove %s` to delete, or `boid workspace assign <project> %s` to add)", slug, slug),
		)
	}

	return renderOutput(cmd, view, func() error {
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Workspace: %s\n", slug)

		if len(view.Warnings) > 0 {
			for _, w := range view.Warnings {
				fmt.Fprintf(out, "  warning: %s\n", w)
			}
			fmt.Fprintln(out)
		}

		if meta != nil {
			fmt.Fprintf(out, "kits: %s\n", formatStringSlice(meta.Kits))
			if len(meta.Env) > 0 {
				envKeys := make([]string, 0, len(meta.Env))
				for k := range meta.Env {
					envKeys = append(envKeys, k)
				}
				sort.Strings(envKeys)
				fmt.Fprintln(out, "env:")
				for _, k := range envKeys {
					fmt.Fprintf(out, "  %s: %s\n", k, meta.Env[k])
				}
			}
			if meta.Capabilities.Docker != nil {
				fmt.Fprintf(out, "capabilities: docker=enabled\n")
			}
			fmt.Fprintln(out)
		}

		if len(projects) == 0 {
			fmt.Fprintf(out, "projects: (none) — `boid workspace remove %s` で削除可能\n", slug)
		} else {
			fmt.Fprintf(out, "projects (%d):\n", len(projects))
			for _, p := range projects {
				fmt.Fprintf(out, "  %-36s  %s\n", p.ID, filepath.Base(p.WorkDir))
			}
		}
		return nil
	})
}

func runWorkspaceAssign(cmd *cobra.Command, args []string) error {
	// CLI entry-point validation per plan (3-layer defense). Early error gives
	// a better UX than a 400 from the daemon.
	slug := args[1]
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return err
	}

	c := client.NewUnixClient(client.DefaultSocketPath())

	p, err := resolveProjectRef(c, os.Stdin, cmd.OutOrStdout(), args[0])
	if err != nil {
		return fmt.Errorf("resolve project: %w", err)
	}

	// get-or-create: PUT creates the DB row for the slug even if it's unknown.
	var project orchestrator.Project
	if err := c.Do("PUT", "/api/projects/"+p.ID+"/workspace", map[string]string{"workspace_id": slug}, &project); err != nil {
		return fmt.Errorf("assign workspace: %w", err)
	}

	return renderOutput(cmd, &project, func() error {
		fmt.Fprintf(cmd.OutOrStdout(), "workspace assigned: %s -> %s\n", project.ID, project.WorkspaceID)
		return nil
	})
}

func runWorkspaceClear(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())

	p, err := resolveProjectRef(c, os.Stdin, cmd.OutOrStdout(), args[0])
	if err != nil {
		return fmt.Errorf("resolve project: %w", err)
	}

	// "Clear" now resets to the default workspace rather than removing the
	// project_workspaces row. Every project belongs to exactly one workspace
	// — "unassigned" is no longer a representable state.
	var project orchestrator.Project
	if err := c.Do("PUT", "/api/projects/"+p.ID+"/workspace",
		map[string]string{"workspace_id": orchestrator.DefaultWorkspaceSlug}, &project); err != nil {
		return fmt.Errorf("clear workspace: %w", err)
	}

	return renderOutput(cmd, &project, func() error {
		fmt.Fprintf(cmd.OutOrStdout(), "workspace reset to %s: %s\n", orchestrator.DefaultWorkspaceSlug, project.ID)
		return nil
	})
}

func runWorkspaceConfigure(cmd *cobra.Command, args []string) error {
	slug := args[0]
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	c := client.NewUnixClient(client.DefaultSocketPath())

	// 1. Resolve default harness (workspace configure requires one; kit init sets
	//    it on first run, so by the time configure is called it should be set).
	harness, err := config.DefaultHarness()
	if err != nil {
		if errors.Is(err, config.ErrDefaultHarnessNotSet) {
			return fmt.Errorf("default harness not configured — run `boid kit init` first to set it")
		}
		return fmt.Errorf("resolve default harness: %w", err)
	}
	fmt.Fprintf(out, "default harness: %s\n", harness)

	// 2. Deploy embedded skills to the host (idempotent) so the adapter can
	//    bind-mount them into the sandbox.
	skillsDir := defaultSkillsDir()
	if err := skills.DeployAll(skillsDir); err != nil {
		return fmt.Errorf("deploy skills: %w", err)
	}

	// 3. Fetch assigned projects from the daemon to gather their WorkDirs for
	//    read-only bind mounts so the skill can read package.json / go.mod etc.
	var projects []*orchestrator.Project
	if err := c.Do("GET", "/api/projects?workspace_id="+slug, nil, &projects); err != nil {
		return fmt.Errorf("list projects: %w", err)
	}

	fmt.Fprintf(out, "Workspace: %s\n", slug)
	if len(projects) == 0 {
		fmt.Fprintln(out, "  (no projects assigned — use `boid workspace assign <project> "+slug+"` first)")
	} else {
		fmt.Fprintf(out, "Assigned projects (%d):\n", len(projects))
		for _, p := range projects {
			fmt.Fprintf(out, "  %s  %s\n", p.ID, p.WorkDir)
		}
	}
	fmt.Fprintln(out)

	// 4. Determine the workspace.yaml path and ensure the parent dir exists.
	store := orchestrator.NewWorkspaceStore("")
	wsDir, err := orchestrator.DefaultWorkspaceDir()
	if err != nil {
		return fmt.Errorf("workspace dir: %w", err)
	}
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		return fmt.Errorf("create workspace dir: %w", err)
	}
	wsYAML := filepath.Join(wsDir, slug+".yaml")

	// 5. Backup existing workspace.yaml (if any) and ensure the file exists
	//    (touch) with mode 0o600 so the post-sandbox secret scan always has a
	//    file to read and so the agent's umask cannot widen the mode. The
	//    binding itself is now on the parent dir (WritableDirs), not the file
	//    — Write/Edit's atomic rename needs the parent writable, and a
	//    single-file IsFile bind would block it with EROFS.
	bakPath, err := backupWorkspaceYAML(wsYAML)
	if err != nil {
		return fmt.Errorf("backup workspace.yaml: %w", err)
	}

	// 6. Resolve the boid binary path (runner-outer is re-exec'd via this path).
	boidBinary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve boid binary: %w", err)
	}

	// 7. Collect WorkDirs from the assigned projects as read-only binds so the
	//    skill can read package.json / go.mod / hook scripts without write access.
	var roBinds []string
	for _, p := range projects {
		if p.WorkDir != "" {
			roBinds = append(roBinds, p.WorkDir)
		}
	}

	// 8. Build the JobSpec via BuildInitJobSpec.
	jobID := fmt.Sprintf("workspace-configure-%s", randomJobSuffix())
	// The parent dir (~/.config/boid/workspaces/) is bind-mounted rw rather
	// than just the target <slug>.yaml. A single-file IsFile bind would block
	// the atomic write pattern (write to <name>.tmp.<pid>.<rand> in the parent
	// dir, then rename) used by harness-side file editors with EROFS, which
	// pinned the skill to shell-only writes and broke harness-agnosticism.
	// The blast radius is contained: this dir only holds per-slug workspace
	// yamls, and the post-sandbox secret scan still vetoes any leaked secret.
	spec := dispatcher.BuildInitJobSpec(dispatcher.InitJobInput{
		Profile:       sandbox.ProfileInit,
		WritableDirs:  []string{wsDir},
		ReadOnlyBinds: roBinds,
		Argv:          []string{"boid-workspace-configure"},
		DisplayName:   "boid workspace configure " + slug,
		HarnessType:   harness,
		Env: map[string]string{
			"BOID_WORKSPACE_SLUG": slug,
		},
		// Bootstrap prompt — matches a trigger phrase from
		// boid-workspace-configure SKILL.md frontmatter. Embedding the slug
		// lets the skill skip the "which workspace" question and dive
		// straight into the project scan, mirroring how `boid kit init`
		// kicks the kit-init skill on launch.
		Instruction: fmt.Sprintf("workspace %q の boid workspace configure を実行して", slug),
	})

	// 9. Build the SandboxRuntimeInfo.
	//    ServerSocket is set so the skill can call daemon APIs (boid workspace
	//    show, boid kit list, etc.).
	//
	//    ProxyPort must be wired the same as `boid kit init` — ProfileInit
	//    sandboxes can only egress through the daemon proxy port; without
	//    HTTPS_PROXY, AI agent harnesses fail with FailedToOpenSocket and
	//    nothing reaches api.anthropic.com.
	rt := dispatcher.SandboxRuntimeInfo{
		JobID:        jobID,
		BoidBinary:   boidBinary,
		ServerSocket: client.DefaultSocketPath(), // daemon API required
		ProxyPort:    resolveDaemonProxyPort(out),
		Foreground:   true,
	}

	sbSpec, err := dispatcher.BuildSandboxSpec(spec, rt)
	if err != nil {
		restoreWorkspaceYAML(wsYAML, bakPath)
		return fmt.Errorf("build sandbox spec: %w", err)
	}

	sb, err := dispatcher.NewSandboxPreparer().PrepareSandbox(sbSpec)
	if err != nil {
		restoreWorkspaceYAML(wsYAML, bakPath)
		return fmt.Errorf("prepare sandbox: %w", err)
	}
	if sb == nil || sb.SpecPath == "" {
		restoreWorkspaceYAML(wsYAML, bakPath)
		return fmt.Errorf("prepare sandbox: missing spec path")
	}

	// 10. Run the runner-outer as a child process and wait for completion.
	runnerArgs := []string{boidBinary, "runner-outer", "--spec", sb.SpecPath, "--state", sb.StatePath}
	if execErr := workspaceConfigureExecFn(boidBinary, runnerArgs, os.Environ()); execErr != nil {
		// Sandbox failed — restore backup.
		restoreWorkspaceYAML(wsYAML, bakPath)
		return execErr
	}

	// 11. Scan the written workspace.yaml for secrets.
	//     On finding(s): restore backup + return error.
	//     On clean: delete backup, print summary, reload projects.
	if err := scanWorkspaceYAML(wsYAML, bakPath, out, store, slug); err != nil {
		return err
	}

	// 12. Reload projects so the daemon picks up kit changes in workspace.yaml.
	reloadProjects()
	return nil
}

// backupWorkspaceYAML ensures the workspace yaml file exists (creating it if
// absent with mode 0o600) and writes a backup copy to <path>.bak.<unixtime>
// if the file already had content. Returns the backup path (may be empty if
// the original did not exist) and any error.
func backupWorkspaceYAML(wsYAML string) (bakPath string, err error) {
	existing, readErr := os.ReadFile(wsYAML)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return "", fmt.Errorf("read workspace.yaml: %w", readErr)
	}

	if !errors.Is(readErr, os.ErrNotExist) && len(existing) > 0 {
		// File exists with content — create a backup.
		bakPath = fmt.Sprintf("%s.bak.%d", wsYAML, os.Getpid())
		if err := os.WriteFile(bakPath, existing, 0o600); err != nil {
			return "", fmt.Errorf("write backup: %w", err)
		}
	}

	// Touch the file (create empty or leave existing in place — truncate only
	// if it did not exist yet so the sandbox can overwrite via RW bind).
	if errors.Is(readErr, os.ErrNotExist) {
		if err := os.WriteFile(wsYAML, []byte{}, 0o600); err != nil {
			return bakPath, fmt.Errorf("touch workspace.yaml: %w", err)
		}
	}
	return bakPath, nil
}

// restoreWorkspaceYAML restores wsYAML from bakPath (if a backup exists) or
// removes wsYAML if there was no backup (the file was newly created by touch).
// Errors are swallowed — this is best-effort rollback called on failure paths.
func restoreWorkspaceYAML(wsYAML, bakPath string) {
	if bakPath == "" {
		// No pre-existing content — remove the touch-created file.
		_ = os.Remove(wsYAML)
		return
	}
	bak, err := os.ReadFile(bakPath)
	if err != nil {
		return
	}
	_ = os.WriteFile(wsYAML, bak, 0o600)
	_ = os.Remove(bakPath)
}

// scanWorkspaceYAML scans the single workspace.yaml file for secrets.
// On clean: deletes backup and prints a kit summary.
// On findings: restores backup from bakPath and returns an error.
func scanWorkspaceYAML(wsYAML, bakPath string, out interface{ Write([]byte) (int, error) }, store *orchestrator.WorkspaceStore, slug string) error {
	findings, err := orchestrator.ScanSecretsFile(wsYAML)
	if err != nil {
		restoreWorkspaceYAML(wsYAML, bakPath)
		return fmt.Errorf("secret scan: %w", err)
	}

	if len(findings) > 0 {
		restoreWorkspaceYAML(wsYAML, bakPath)
		var sb strings.Builder
		sb.WriteString("secret scan: suspicious values detected in generated workspace.yaml — rolled back\n")
		for _, f := range findings {
			sb.WriteString("  ")
			sb.WriteString(f.String())
			sb.WriteString("\n")
		}
		return errors.New(strings.TrimRight(sb.String(), "\n"))
	}

	// Clean — remove backup.
	if bakPath != "" {
		_ = os.Remove(bakPath)
	}

	// Print kit summary from the written workspace.yaml.
	meta, loadErr := store.Load(slug)
	if loadErr == nil && meta != nil {
		fmt.Fprintf(out, "kits: %s\n", formatStringSlice(meta.Kits))
	}
	return nil
}

func runWorkspaceRemove(cmd *cobra.Command, args []string) error {
	slug := args[0]
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return err
	}

	// CLI entry-point guard (3-layer defense; the WorkspaceStore.Remove
	// domain layer enforces the same rule). The reserved default workspace
	// cannot be removed because every project would otherwise become
	// unlinked.
	if slug == orchestrator.DefaultWorkspaceSlug {
		return fmt.Errorf("workspace %q is reserved and cannot be removed", slug)
	}

	c := client.NewUnixClient(client.DefaultSocketPath())
	out := cmd.OutOrStdout()

	// Check for assigned projects first.
	var projects []*orchestrator.Project
	if err := c.Do("GET", "/api/projects?workspace_id="+slug, nil, &projects); err != nil {
		return fmt.Errorf("list projects: %w", err)
	}
	if len(projects) > 0 {
		fmt.Fprintf(out, "error: workspace %q still has %d assigned project(s):\n", slug, len(projects))
		for _, p := range projects {
			fmt.Fprintf(out, "  %s  %s\n", p.ID, filepath.Base(p.WorkDir))
		}
		fmt.Fprintln(out, "Run `boid workspace clear <project>` for each project first.")
		return fmt.Errorf("workspace %q has assigned projects", slug)
	}

	// Delete workspace.yaml.
	store := orchestrator.NewWorkspaceStore("")
	if err := store.Remove(slug); err != nil {
		if errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), os.ErrNotExist.Error()) {
			// yaml was already gone — not a hard error; the DB side was already clean.
			fmt.Fprintf(out, "workspace.yaml for %q not found (already removed).\n", slug)
		} else {
			return fmt.Errorf("remove workspace.yaml: %w", err)
		}
	} else {
		fmt.Fprintf(out, "workspace %q removed.\n", slug)
	}

	return nil
}

// formatStringSlice formats a slice for display: "(none)" when empty, or comma-joined.
func formatStringSlice(ss []string) string {
	if len(ss) == 0 {
		return "(none)"
	}
	return strings.Join(ss, ", ")
}
