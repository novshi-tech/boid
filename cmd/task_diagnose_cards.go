package cmd

import (
	"fmt"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

// diagnoseCardsRow is one flagged card in `boid task diagnose-cards`'
// output: a card carrying more than one unresolved child. Read-only — it
// never stops, deletes, or otherwise touches any running work.
type diagnoseCardsRow struct {
	TaskID             string   `json:"task_id"`
	Title              string   `json:"title"`
	Status             string   `json:"status"`
	UnresolvedChildren int      `json:"unresolved_children"`
	OpenChildTaskCount int      `json:"open_task_rows"`
	UnresolvedChildIDs []string `json:"unresolved_child_ids,omitempty"`
}

var taskDiagnoseCardsCmd = &cobra.Command{
	Use:   "diagnose-cards",
	Short: "List cards violating the single-work-slot invariant (more than one unresolved child)",
	Long: "card 直下の未実行/実行中の子は合わせて最大1件、という不変条件に\n" +
		"違反している card (子が2件以上) を列挙する。読み取り専用 —\n" +
		"実行中の仕事を停止・削除しない。解消は人が既存の\n" +
		"`boid action send --type child_dropped` で残りの子から選ぶ。",
	Args: cobra.NoArgs,
	RunE: runTaskDiagnoseCards,
}

func init() {
	taskDiagnoseCardsCmd.Annotations = map[string]string{scopeAnnotationKey: scopeRemote}
	taskCmd.AddCommand(taskDiagnoseCardsCmd)
}

func runTaskDiagnoseCards(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())
	var tasks []orchestrator.Task
	if err := c.Do("GET", "/api/tasks?status=cards_live", nil, &tasks); err != nil {
		return fmt.Errorf("list cards: %w", err)
	}

	var rows []diagnoseCardsRow
	for _, t := range tasks {
		if t.Card == nil {
			continue
		}
		children, err := orchestrator.DetailChildren(t.Card.Detail)
		if err != nil {
			// A malformed detail blob is itself worth flagging, not silently
			// skipping — surface it as a zero-child row so it isn't invisible.
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: task %s: parse children: %v\n", t.ID, err)
			continue
		}
		count, err := orchestrator.CountUnresolvedChildren(t.Card.Detail, t.OpenChildCount)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: task %s: count unresolved children: %v\n", t.ID, err)
			continue
		}
		if count <= 1 {
			continue
		}
		var unresolvedIDs []string
		for _, ch := range children {
			if ch.Status == orchestrator.TaskTriageChildStatusOpen || ch.Status == orchestrator.TaskTriageChildStatusSpecced {
				unresolvedIDs = append(unresolvedIDs, ch.ID)
			}
		}
		rows = append(rows, diagnoseCardsRow{
			TaskID:             t.ID,
			Title:              t.Title,
			Status:             string(t.Status),
			UnresolvedChildren: count,
			OpenChildTaskCount: t.OpenChildCount,
			UnresolvedChildIDs: unresolvedIDs,
		})
	}

	return renderOutput(cmd, rows, func() error {
		if len(rows) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "no cards violate the single-work-slot invariant")
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%-36s %-9s %-6s %-10s %s\n", "TASK ID", "STATUS", "COUNT", "LIVE ROWS", "OPEN/SPECCED CHILD IDS")
		for _, r := range rows {
			fmt.Fprintf(cmd.OutOrStdout(), "%-36s %-9s %-6d %-10d %v\n", r.TaskID, r.Status, r.UnresolvedChildren, r.OpenChildTaskCount, r.UnresolvedChildIDs)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "\nresolve by dropping all but one child: boid action send --task <task_id> --type child_dropped --payload '{\"id\":\"<child_id>\",\"reason\":\"...\"}'")
		return nil
	})
}
