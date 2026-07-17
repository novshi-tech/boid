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
	gcCmd.Annotations = map[string]string{
		annotationSkipAutostart: "skip",
		// scopeLocal (codex review round 2, docs/plans/cli-remote-connection.md
		// classification table: "local | gc | ローカル runtime dir 削除").
		// gc was scopeRemote until this fix, on the reasoning that its
		// actual work (POST /api/gc) is entirely dispatched through the
		// daemon's HTTP API — that reasoning is not wrong mechanically (gc
		// would in fact work correctly against a remote daemon too, since
		// everything it deletes lives on whichever host the daemon itself
		// runs on), but this pins gc to the plan doc's classification for
		// consistency with the rest of the "daemon lifecycle machinery"
		// grouping (start/stop/gc/init) rather than re-litigating the doc's
		// own call here — see the codex review round 2 report for the full
		// discussion of this judgment call. annotationSkipAutostart=skip
		// only means "don't launch one just for this" — a different axis,
		// see scopeAnnotationKey's doc comment in root.go.
		scopeAnnotationKey: scopeLocal,
	}
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

	c := client.FromContext(cmd.Context())

	var result struct {
		Tasks      int64 `json:"tasks"`
		Jobs       int64 `json:"jobs"`
		Actions    int64 `json:"actions"`
		Runtimes   int64 `json:"runtimes"`
		SandboxTmp int64 `json:"sandbox_tmp"`
	}
	if err := c.Do("POST", "/api/gc", body, &result); err != nil {
		return err
	}

	if dryRun {
		fmt.Printf("dry run: would delete %d tasks, %d jobs, %d actions, %d runtimes, %d sandbox tmp entries\n",
			result.Tasks, result.Jobs, result.Actions, result.Runtimes, result.SandboxTmp)
	} else {
		fmt.Printf("deleted: %d tasks, %d jobs, %d actions, %d runtimes, %d sandbox tmp entries\n",
			result.Tasks, result.Jobs, result.Actions, result.Runtimes, result.SandboxTmp)
	}
	return nil
}
