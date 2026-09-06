package cmd

import (
	"fmt"
	"net/url"
	"time"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

// cardCmd groups the thin CLI wrappers a human operator needs around the
// manual card-command launcher (card_command_launcher.go) and its
// card_requests ledger, so starting or inspecting one doesn't need
// hand-rolled curl against the unauthenticated UNIX socket.
var cardCmd = &cobra.Command{
	Use:   "card",
	Short: "Run card commands and inspect card_requests",
}

func init() {
	rootCmd.AddCommand(cardCmd)
}

// cardRunResult mirrors api.RunCardCommandResult's wire shape — only the
// fields this command renders.
type cardRunResult struct {
	Occupied      bool   `json:"occupied"`
	RequestID     string `json:"request_id,omitempty"`
	LauncherJobID string `json:"launcher_job_id,omitempty"`
	TargetKind    string `json:"target_kind,omitempty"`
	TargetID      string `json:"target_id,omitempty"`
	Instruction   string `json:"instruction,omitempty"`
}

var cardRunInstruction string

var cardRunCmd = &cobra.Command{
	Use:   "run <card-id> <key>",
	Short: "Run a project.yaml card_commands entry as a human command launcher",
	Long: "card 直下の project.yaml `card_commands.<key>` を人発コマンドとして起動する\n" +
		"(POST /api/cards/{id}/commands/{key} の薄いラッパー)。\n" +
		"card の単一実行枠が既に占有中なら起動せず、現在の実行 (target_kind/target_id)\n" +
		"へのリンクを返す — 入力した --instruction はそのまま応答に含まれるので\n" +
		"呼び出し側で保持できる。",
	Args: cobra.ExactArgs(2),
	RunE: runCardRun,
}

func init() {
	cardRunCmd.Flags().StringVar(&cardRunInstruction, "instruction", "", "free-text instruction carried into the launched command's context")
	cardRunCmd.Annotations = map[string]string{scopeAnnotationKey: scopeRemote}
	cardCmd.AddCommand(cardRunCmd)
}

func runCardRun(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())
	cardID, key := args[0], args[1]

	var result cardRunResult
	body := map[string]string{"instruction": cardRunInstruction}
	path := fmt.Sprintf("/api/cards/%s/commands/%s", url.PathEscape(cardID), url.PathEscape(key))
	if err := c.Do("POST", path, body, &result); err != nil {
		return fmt.Errorf("card run: %w", err)
	}

	return renderOutput(cmd, result, func() error {
		if result.Occupied {
			fmt.Fprintf(cmd.OutOrStdout(), "occupied: card %s's execution slot is already held", cardID)
			if result.TargetKind != "" && result.TargetID != "" {
				fmt.Fprintf(cmd.OutOrStdout(), " by %s %s", result.TargetKind, result.TargetID)
			}
			fmt.Fprintln(cmd.OutOrStdout())
			if result.Instruction != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "not dispatched — keep your instruction for retry: %s\n", result.Instruction)
			}
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "launched: request_id=%s launcher_job_id=%s\n", result.RequestID, result.LauncherJobID)
		return nil
	})
}

// cardRequestEntry mirrors api.cardRequestView's wire shape — only the
// fields this command renders.
type cardRequestEntry struct {
	ID            string    `json:"id"`
	CardID        string    `json:"card_id"`
	CommandKey    string    `json:"command_key"`
	CauseID       string    `json:"cause_id,omitempty"`
	Status        string    `json:"status"`
	Instruction   string    `json:"instruction,omitempty"`
	LauncherJobID string    `json:"launcher_job_id,omitempty"`
	TargetKind    string    `json:"target_kind,omitempty"`
	TargetID      string    `json:"target_id,omitempty"`
	Error         string    `json:"error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

var cardRequestsCmd = &cobra.Command{
	Use:   "requests <card-id>",
	Short: "List a card's card_requests rows, oldest first",
	Long: "GET /api/card-requests?card_id=<id> の薄いラッパー。\n" +
		"queued/launching/attached/folded/finished/failed の全行を古い順に表示する。\n" +
		"詰まった行の強制解除は `boid task release-card-request <request_id>`。",
	Args: cobra.ExactArgs(1),
	RunE: runCardRequests,
}

func init() {
	cardRequestsCmd.Annotations = map[string]string{scopeAnnotationKey: scopeRemote}
	cardCmd.AddCommand(cardRequestsCmd)
}

func runCardRequests(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())
	cardID := args[0]

	var rows []cardRequestEntry
	if err := c.Do("GET", "/api/card-requests?card_id="+url.QueryEscape(cardID), nil, &rows); err != nil {
		return fmt.Errorf("list card requests: %w", err)
	}

	return renderOutput(cmd, rows, func() error {
		if len(rows) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "no card_requests for this card")
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%-36s %-11s %-14s %-38s %s\n", "ID", "STATUS", "COMMAND KEY", "TARGET", "CREATED")
		for _, r := range rows {
			target := ""
			if r.TargetKind != "" {
				target = r.TargetKind + ":" + r.TargetID
			}
			cmdKey := r.CommandKey
			if cmdKey == orchestrator.CardRequestCommandKeyGo {
				cmdKey = "(go)"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%-36s %-11s %-14s %-38s %s\n", r.ID, r.Status, cmdKey, target, r.CreatedAt.Format(time.RFC3339))
		}
		return nil
	})
}
