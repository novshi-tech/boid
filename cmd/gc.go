package cmd

import (
	"fmt"
	"time"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/spf13/cobra"
)

var gcCmd = &cobra.Command{
	Use:   "gc",
	Short: "Garbage collect done and aborted tasks older than a given duration",
	RunE:  runGC,
}

func init() {
	gcCmd.Annotations = map[string]string{annotationSkipAutostart: "skip"}
	gcCmd.Flags().Duration("older-than", 30*24*time.Hour, "Delete tasks older than this duration")
	gcCmd.Flags().Bool("dry-run", false, "Show what would be deleted without actually deleting")
	rootCmd.AddCommand(gcCmd)
}

func runGC(cmd *cobra.Command, args []string) error {
	olderThan, _ := cmd.Flags().GetDuration("older-than")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	body := map[string]any{
		"older_than": olderThan.String(),
		"dry_run":    dryRun,
	}

	c := client.NewUnixClient(client.DefaultSocketPath())

	var result struct {
		Tasks     int64 `json:"tasks"`
		Jobs      int64 `json:"jobs"`
		Actions   int64 `json:"actions"`
		Worktrees int64 `json:"worktrees"`
		Runtimes  int64 `json:"runtimes"`
	}
	if err := c.Do("POST", "/api/gc", body, &result); err != nil {
		return err
	}

	if dryRun {
		fmt.Printf("dry run: would delete %d tasks, %d jobs, %d actions, %d worktrees, %d runtimes\n",
			result.Tasks, result.Jobs, result.Actions, result.Worktrees, result.Runtimes)
	} else {
		fmt.Printf("deleted: %d tasks, %d jobs, %d actions, %d worktrees, %d runtimes\n",
			result.Tasks, result.Jobs, result.Actions, result.Worktrees, result.Runtimes)
	}
	return nil
}
