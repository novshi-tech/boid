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
		// scopeRemote (docs/plans/release-onboarding.md 決定2/scope 再分類
		// 表, PR5): gc's own work (POST /api/gc) is dispatched entirely
		// through the daemon's HTTP API, and pre-PR5 it only reached the
		// compose container daemon via an IMPLICIT channel — the
		// runtime-dir bind that happens to make the same unix socket path
		// resolve on both sides of the bind mount (§決定4's mutual-
		// exclusion contract). That implicit path was never guaranteed to
		// keep working (the plan doc's own words: "bind をいつか撤去したら
		// 壊れる"). Now that host mode (cmd/host.go) is the CLI's default
		// resolution path for every scope=remote command, gc goes through
		// the same explicit, authenticated CLI listener as everything
		// else instead of relying on that coincidence.
		// annotationSkipAutostart=skip still means "don't spin up a
		// daemon just to gc it" for the --profile-explicit fallback path
		// (a resolved unix profile skips EnsureRunningAt) — a different
		// axis from scopeAnnotationKey, see its own doc comment in
		// root.go. Host mode's own branch in root.go does not consult
		// this annotation (same as every other scopeRemote command).
		scopeAnnotationKey: scopeRemote,
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
		// WorkspaceHomes lists every workspace HOME VOLUME this install has
		// on the engine, with its size (docs/plans/home-workspace-volume.md
		// Phase 4 PR5, rewired onto the engine's volume API by 論点 a-2 / PR7
		// of docs/plans/workspace-home-volume-persistence.md) — visibility
		// only, GC never deletes a workspace home itself (`workspace remove`
		// does that). Comes back empty, with WorkspaceHomesListError set,
		// whenever no trustworthy listing could be produced.
		WorkspaceHomes []apiwire.WorkspaceHomeSize `json:"workspace_homes,omitempty"`
		// WorkspaceHomesListError is non-empty when the daemon could not
		// produce or trust the listing: the engine's volume enumeration
		// failed (PR7 round-2 codex review, Major 2), or the workspace lister
		// did, which makes orphan detection untrustworthy (codex PR #791
		// review, Should-fix #3). See printWorkspaceHomes.
		WorkspaceHomesListError string `json:"workspace_homes_list_error,omitempty"`
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

	printWorkspaceHomes(out, result.WorkspaceHomes, result.WorkspaceHomesListError)
	return nil
}

// printWorkspaceHomes renders `boid gc`'s workspace_homes listing
// (docs/plans/home-workspace-volume.md Phase 4 PR5, engine-backed since 論点
// a-2 / PR7 of docs/plans/workspace-home-volume-persistence.md): one line per
// workspace HOME VOLUME the daemon's engine reports for this install, an
// "(orphan) " prefix for any with no matching workspace row, and a total.
//
// A size failure renders as "?" rather than a bogus 0 B, and is excluded from
// the total (an unknown size must not silently understate it). Sizes come from
// one engine-wide disk-usage call, so this is normally all-or-nothing across
// the listing — a listing whose entries are all "?" means that one call failed,
// not that the volumes are unreadable one by one.
//
// No output at all when homes is empty and listErr is also empty — either the
// daemon was too old to report it, or no workspace has ever been dispatched
// into yet. A non-empty listErr reports a single warning line instead of a
// (necessarily empty) table, which covers both reasons the daemon withholds a
// listing: its engine could not enumerate the volumes (PR7 round-2 codex
// review, Major 2), or its workspace lister failed and orphan flags would
// therefore be untrustworthy (codex PR #791 review, Should-fix #3). Printing
// nothing in either case would be indistinguishable from a clean install with
// no homes, which is precisely what hid a wedged engine.
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
