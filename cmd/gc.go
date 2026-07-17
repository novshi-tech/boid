package cmd

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/humanize"
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
		// WorkspaceHomes lists every workspace home directory's on-disk size
		// (docs/plans/home-workspace-volume.md Phase 4 PR5) — visibility
		// only, GC never deletes a home directory itself (`workspace
		// remove` does that).
		WorkspaceHomes []api.WorkspaceHomeSize `json:"workspace_homes,omitempty"`
	}
	if err := c.Do("POST", "/api/gc", body, &result); err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if dryRun {
		fmt.Fprintf(out, "dry run: would delete %d tasks, %d jobs, %d actions, %d runtimes, %d sandbox tmp entries\n",
			result.Tasks, result.Jobs, result.Actions, result.Runtimes, result.SandboxTmp)
	} else {
		fmt.Fprintf(out, "deleted: %d tasks, %d jobs, %d actions, %d runtimes, %d sandbox tmp entries\n",
			result.Tasks, result.Jobs, result.Actions, result.Runtimes, result.SandboxTmp)
	}

	printWorkspaceHomes(out, result.WorkspaceHomes)
	return nil
}

// printWorkspaceHomes renders `boid gc`'s workspace_homes listing
// (docs/plans/home-workspace-volume.md Phase 4 PR5): one line per workspace
// home directory found on disk, an "(orphan) " prefix for any with no
// matching workspace row, and a total. A size computation failure renders
// as "?" rather than a bogus 0 B, and is excluded from the total (an
// unknown size must not silently understate it). No output at all when
// homes is empty — either the daemon was too old to report it, or no
// workspace has ever been dispatched into yet.
func printWorkspaceHomes(out io.Writer, homes []api.WorkspaceHomeSize) {
	if len(homes) == 0 {
		return
	}
	fmt.Fprintln(out, "workspace homes:")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	var total int64
	for _, h := range homes {
		label := h.Slug + ":"
		if h.Orphan {
			label = "(orphan) " + label
		}
		size := "?"
		if h.SizeError == "" {
			size = humanize.FormatBytes(h.Bytes)
			total += h.Bytes
		}
		fmt.Fprintf(tw, "  %s\t%s\n", label, size)
	}
	fmt.Fprintf(tw, "  %s\t%s\n", "total:", humanize.FormatBytes(total))
	_ = tw.Flush()
}
