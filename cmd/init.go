package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/initwizard"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init [dir]",
	Short: "Initialize a new boid project interactively",
	Long: `Initialize a new boid project in the current directory (or [dir]).

This command runs an interactive wizard to:
  1. Set the project name
  2. Select applicable kits from the installed kit registry
  3. Choose a task behavior provider
  4. Validate host command requirements
  5. Generate .boid/project.yaml

Example:
  boid init                # initialize in the current directory
  boid init ./my-project   # initialize in ./my-project
`,
	Args: cobra.MaximumNArgs(1),
	RunE: runInit,
}

func init() {
	initCmd.Annotations = map[string]string{annotationSkipAutostart: "skip"}
	rootCmd.AddCommand(initCmd)
}

func runInit(cmd *cobra.Command, args []string) error {
	dir := "."
	if len(args) > 0 {
		dir = args[0]
	}

	projectDir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}

	// [5] Abort if project.yaml already exists
	projectYAMLPath := filepath.Join(projectDir, ".boid", "project.yaml")
	if _, err := os.Stat(projectYAMLPath); err == nil {
		return fmt.Errorf(".boid/project.yaml already exists in %s; remove it first", projectDir)
	}

	w := &initwizard.Wizard{
		In:      os.Stdin,
		Out:     os.Stdout,
		KitsDir: defaultKitsDir(),
	}

	if err := w.Run(projectDir); err != nil {
		return err
	}

	// [8] Register project with the boid server
	c := client.NewUnixClient(client.DefaultSocketPath())
	var p projectspec.Project
	if err := c.Do("POST", "/api/projects", map[string]string{"work_dir": projectDir}, &p); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not register project with boid server: %v\n", err)
		fmt.Fprintln(os.Stderr, "Run 'boid project add .' once the server is running.")
		return nil
	}

	fmt.Printf("project registered: %s (%s)\n", p.ID, p.Meta.Name)
	return nil
}
