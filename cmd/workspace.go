package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
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
	Short: "Clear a project's workspace assignment",
	Args:  cobra.ExactArgs(1),
	RunE:  runWorkspaceClear,
}

// workspaceConfigureCmd is a stub for the workspace configure command. The
// full implementation (project scan + kit matching) will be added in a later PR.
//
// When the full implementation dispatches a sandbox-based configuration script,
// the JobSpec must set:
//   SandboxProfile: int(sandbox.ProfileInit)
// This causes BuildPlan to mount the entire host root read-only (so the
// configuration script can detect installed tools) and skips broker registration
// / socket mount (configure scripts do not invoke boid host-commands).
var workspaceConfigureCmd = &cobra.Command{
	Use:   "configure <slug>",
	Short: "Configure a workspace (scan projects + kit matching — stub, full implementation in a later PR)",
	Args:  cobra.ExactArgs(1),
	RunE:  runWorkspaceConfigure,
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
	c := client.NewUnixClient(client.DefaultSocketPath())

	p, err := resolveProjectRef(c, os.Stdin, cmd.OutOrStdout(), args[0])
	if err != nil {
		return fmt.Errorf("resolve project: %w", err)
	}

	// get-or-create: PUT creates the DB row for the slug even if it's unknown.
	var project orchestrator.Project
	if err := c.Do("PUT", "/api/projects/"+p.ID+"/workspace", map[string]string{"workspace_id": args[1]}, &project); err != nil {
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

	var project orchestrator.Project
	if err := c.Do("PUT", "/api/projects/"+p.ID+"/workspace", map[string]string{"workspace_id": ""}, &project); err != nil {
		return fmt.Errorf("clear workspace: %w", err)
	}

	return renderOutput(cmd, &project, func() error {
		fmt.Fprintf(cmd.OutOrStdout(), "workspace cleared: %s\n", project.ID)
		return nil
	})
}

func runWorkspaceConfigure(cmd *cobra.Command, args []string) error {
	slug := args[0]
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return err
	}

	c := client.NewUnixClient(client.DefaultSocketPath())
	out := cmd.OutOrStdout()

	// Show assigned projects.
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

	// Create skeleton workspace.yaml if missing.
	store := orchestrator.NewWorkspaceStore("")
	_, err := store.Load(slug)
	if errors.Is(err, os.ErrNotExist) || (err != nil && strings.Contains(err.Error(), os.ErrNotExist.Error())) {
		empty := &orchestrator.WorkspaceMeta{}
		if saveErr := store.Save(slug, empty); saveErr != nil {
			return fmt.Errorf("create workspace.yaml: %w", saveErr)
		}
		fmt.Fprintf(out, "Created skeleton workspace.yaml for %q.\n", slug)
	}

	fmt.Fprintln(out, "Note: full configure (project scan + kit matching) will be available in a future PR.")
	fmt.Fprintf(out, "Edit the workspace.yaml manually or wait for `boid workspace configure` to be fully implemented.\n")
	return nil
}

func runWorkspaceRemove(cmd *cobra.Command, args []string) error {
	slug := args[0]
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return err
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
