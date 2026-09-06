package cmd

import (
	"fmt"
	"net/url"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

// diagnoseCardsRow is one flagged card in `boid task diagnose-cards`'
// output: a card carrying more than one unresolved child, OR a card an
// active card_requests row occupies while showing zero unresolved children
// (a card mid-command or mid-Go-dispatch reads as "free" by child count
// alone). Read-only — it never stops, deletes, or otherwise touches any
// running work.
type diagnoseCardsRow struct {
	TaskID             string   `json:"task_id"`
	Title              string   `json:"title"`
	Status             string   `json:"status"`
	UnresolvedChildren int      `json:"unresolved_children"`
	OpenChildTaskCount int      `json:"open_task_rows"`
	UnresolvedChildIDs []string `json:"unresolved_child_ids,omitempty"`
	ActiveCardRequest  string   `json:"active_card_request_id,omitempty"`
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

	// One bulk call instead of one GET per card (GET /api/card-requests with
	// no card_id lists every currently active row across every card). A
	// failure here must not degrade into "no cards violate the invariant" —
	// this column exists specifically to catch a card whose only problem
	// is a stuck card_request with zero unresolved children, so silently
	// treating every card as having no active request would hide exactly
	// that case.
	activeByCard, err := activeCardRequestIDsByCard(c)
	if err != nil {
		return fmt.Errorf("list active card_requests: %w", err)
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
		var liveChildren []orchestrator.Task
		if err := c.Do("GET", "/api/tasks?parent_id="+url.QueryEscape(t.ID), nil, &liveChildren); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: task %s: list children: %v\n", t.ID, err)
			continue
		}
		liveChildPtrs := make([]*orchestrator.Task, len(liveChildren))
		for i := range liveChildren {
			liveChildPtrs[i] = &liveChildren[i]
		}
		count, err := orchestrator.CountUnresolvedChildren(t.Card.Detail, liveChildPtrs)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: task %s: count unresolved children: %v\n", t.ID, err)
			continue
		}

		// A child count of 0/1 alone reads as "free", but an active
		// (launching/attached) card_requests row occupies the SAME shared
		// slot without ever showing up as a child — surface it too, or an
		// operator sees a "free" card that a retry would still reject.
		activeRequestID := activeByCard[t.ID]

		if count <= 1 && activeRequestID == "" {
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
			ActiveCardRequest:  activeRequestID,
		})
	}

	return renderOutput(cmd, rows, func() error {
		if len(rows) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "no cards violate the single-work-slot invariant")
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%-36s %-9s %-6s %-10s %-38s %s\n", "TASK ID", "STATUS", "COUNT", "LIVE ROWS", "ACTIVE CARD REQUEST", "OPEN/SPECCED CHILD IDS")
		for _, r := range rows {
			fmt.Fprintf(cmd.OutOrStdout(), "%-36s %-9s %-6d %-10d %-38s %v\n", r.TaskID, r.Status, r.UnresolvedChildren, r.OpenChildTaskCount, r.ActiveCardRequest, r.UnresolvedChildIDs)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "\nresolve extra children by dropping all but one: boid action send --task <task_id> --type child_dropped --payload '{\"id\":\"<child_id>\",\"reason\":\"...\"}'")
		fmt.Fprintln(cmd.OutOrStdout(), "resolve a stuck card_request: boid task release-card-request <request_id>")
		return nil
	})
}

// cardRequestListEntry mirrors api.cardRequestView's wire shape — only the
// fields this command reads.
type cardRequestListEntry struct {
	ID     string `json:"id"`
	CardID string `json:"card_id"`
	Status string `json:"status"`
}

// activeCardRequestIDsByCard returns, for every card with a currently
// launching/attached card_requests row, that row's id — one bulk
// GET /api/card-requests (no card_id) instead of one GET per card.
func activeCardRequestIDsByCard(c *client.Client) (map[string]string, error) {
	var rows []cardRequestListEntry
	if err := c.Do("GET", "/api/card-requests", nil, &rows); err != nil {
		return nil, err
	}
	byCard := make(map[string]string, len(rows))
	for _, r := range rows {
		if r.Status == "launching" || r.Status == "attached" {
			byCard[r.CardID] = r.ID
		}
	}
	return byCard, nil
}
