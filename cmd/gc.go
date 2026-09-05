package cmd

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/novshi-tech/boid/internal/apiwire"
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
		scopeAnnotationKey:      scopeRemote,
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
		// TriggerRuns is the count of finished trigger_runs rows deleted.
		TriggerRuns int64 `json:"trigger_runs"`
		// Signals is the count of signals rows deleted.
		Signals int64 `json:"signals"`
		// WorkspaceHomes lists every workspace HOME VOLUME this install has
		// on the engine, with its size — visibility only, GC never deletes a
		// workspace home itself (`workspace remove` does that). Comes back
		// empty, with WorkspaceHomesListError set, whenever no trustworthy
		// listing could be produced.
		WorkspaceHomes []apiwire.WorkspaceHomeSize `json:"workspace_homes,omitempty"`
		// WorkspaceHomesListError is non-empty when the daemon could not
		// produce or trust the listing (engine volume enumeration failed, or
		// the workspace lister failed, making orphan detection untrustworthy).
		// See printWorkspaceHomes.
		WorkspaceHomesListError string `json:"workspace_homes_list_error,omitempty"`
	}
	if err := c.Do("POST", "/api/gc", body, &result); err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if dryRun {
		fmt.Fprintf(out, "dry run: would delete %d tasks, %d jobs, %d actions, %d runtimes, %d sandbox tmp entries, %d trigger runs, %d signals\n",
			result.Tasks, result.Jobs, result.Actions, result.Runtimes, result.SandboxTmp, result.TriggerRuns, result.Signals)
	} else {
		fmt.Fprintf(out, "deleted: %d tasks, %d jobs, %d actions, %d runtimes, %d sandbox tmp entries, %d trigger runs, %d signals\n",
			result.Tasks, result.Jobs, result.Actions, result.Runtimes, result.SandboxTmp, result.TriggerRuns, result.Signals)
	}

	printWorkspaceHomes(out, result.WorkspaceHomes, result.WorkspaceHomesListError)
	return nil
}

// printWorkspaceHomes renders `boid gc`'s workspace_homes listing: one line
// per workspace HOME VOLUME the daemon's engine reports for this install, an
// "(orphan) " prefix for any with no matching workspace row, and a total.
//
// A size failure renders as "?" rather than a bogus 0 B and is excluded from
// the total. A non-empty listErr reports a single warning line instead of a
// (necessarily empty) table — printing nothing would be indistinguishable
// from a clean install with no homes, which would hide a wedged engine.
func printWorkspaceHomes(out io.Writer, homes []apiwire.WorkspaceHomeSize, listErr string) {
	if listErr != "" {
		fmt.Fprintf(out, "workspace homes: listing unavailable (%s)\n", listErr)
	}
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
