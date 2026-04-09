package cmd

import (
	"fmt"
	"strings"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

var scriptCmd = &cobra.Command{
	Use:   "script",
	Short: "Manage scripts",
}

var scriptListCmd = &cobra.Command{
	Use:   "list",
	Short: "List available scripts for a project",
	RunE:  runScriptList,
}

var scriptRunCmd = &cobra.Command{
	Use:   "run <kit>/<script-id>",
	Short: "Run a script manually",
	Args:  cobra.ExactArgs(1),
	RunE:  runScriptRun,
}

func init() {
	scriptListCmd.Flags().String("project", "", "Project ID (default: current directory's project)")
	scriptRunCmd.Flags().String("project", "", "Project ID (default: current directory's project)")
	scriptCmd.AddCommand(scriptListCmd, scriptRunCmd)
	rootCmd.AddCommand(scriptCmd)
}

func resolveScriptProjectID(cmd *cobra.Command) (string, error) {
	projectID, _ := cmd.Flags().GetString("project")
	if projectID != "" {
		return projectID, nil
	}
	projectDir, err := resolveProjectRoot("")
	if err != nil {
		return "", fmt.Errorf("could not determine project (use --project): %w", err)
	}
	meta, err := orchestrator.ReadProjectMeta(projectDir)
	if err != nil {
		return "", fmt.Errorf("read project meta: %w", err)
	}
	return meta.ID, nil
}

func runScriptList(cmd *cobra.Command, args []string) error {
	projectID, err := resolveScriptProjectID(cmd)
	if err != nil {
		return err
	}

	c := client.NewUnixClient(client.DefaultSocketPath())
	scripts, err := c.ListScripts(projectID)
	if err != nil {
		return err
	}

	if len(scripts) == 0 {
		fmt.Println("no scripts")
		return nil
	}

	for _, s := range scripts {
		triggers := make([]string, len(s.On))
		for i, t := range s.On {
			triggers[i] = string(t)
		}
		on := strings.Join(triggers, ",")
		fmt.Printf("%-30s %-40s %s\n", s.Kit+"/"+s.ID, s.Description, on)
	}
	return nil
}

func runScriptRun(cmd *cobra.Command, args []string) error {
	projectID, err := resolveScriptProjectID(cmd)
	if err != nil {
		return err
	}

	ref := args[0]
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("invalid script ref %q: must be <kit>/<script-id>", ref)
	}
	kit, scriptID := parts[0], parts[1]

	c := client.NewUnixClient(client.DefaultSocketPath())
	task, err := c.RunScript(projectID, kit, scriptID)
	if err != nil {
		return err
	}

	fmt.Printf("task created: %s (%s)\n", task.ID, task.Status)
	return nil
}
