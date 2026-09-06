package cmd

import (
	"fmt"

	"github.com/novshi-tech/boid/internal/apiwire"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/spf13/cobra"
)

// taskReleaseCardRequestCmd is the operator escape hatch for a card whose
// single execution slot is stuck launching/attached with no
// continuation ever going to terminate on its own — sibling to
// `boid task diagnose-cards` rather than folded into it, since diagnose-cards
// is read-only and this mutates.
var taskReleaseCardRequestCmd = &cobra.Command{
	Use:   "release-card-request <request-id>",
	Short: "Force-release a stuck card_requests execution slot (launching/attached -> failed, retry-able)",
	Long: "card の単一実行枠 (card_requests) が launching/attached のまま\n" +
		"継続先が二度と終端しない状態で詰まったときの、運用者向けの\n" +
		"強制解除口。継続先の生存確認をスキップして failed (retry 可能) に\n" +
		"直接落とす — 通常は `boid task diagnose-cards` や継続先照合の\n" +
		"reconcile ループが自律的に解放するので、それらが効かない\n" +
		"詰まりにのみ使うこと。\n\n" +
		"注意: これは枠を解放するだけで、継続先 (session/task) 自体は\n" +
		"止めない。解放対象の行に継続先が記録されていた場合、または\n" +
		"launching のまま (launcher job がまだ動いている可能性がある)\n" +
		"場合は、コマンドが warning を出す — どちらも「生存を確認した」\n" +
		"わけではなく「まだ動いている可能性がある」という注意喚起なので、\n" +
		"本当に止めたいなら別途手動で確認・対処すること。",
	Args: cobra.ExactArgs(1),
	RunE: runTaskReleaseCardRequest,
}

var taskReleaseCardRequestReason string

func init() {
	taskReleaseCardRequestCmd.Flags().StringVar(&taskReleaseCardRequestReason, "reason", "", "operator-supplied reason recorded on the request's error field")
	taskReleaseCardRequestCmd.Annotations = map[string]string{scopeAnnotationKey: scopeRemote}
	taskCmd.AddCommand(taskReleaseCardRequestCmd)
}

func runTaskReleaseCardRequest(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())
	requestID := args[0]

	var result apiwire.ReleaseResult
	if err := c.Do("POST", fmt.Sprintf("/api/card-requests/%s/release", requestID), map[string]string{"reason": taskReleaseCardRequestReason}, &result); err != nil {
		return fmt.Errorf("release card request: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "card request %s released\n", requestID)
	if result.OperatorNotice != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "warning: %s\n", result.OperatorNotice)
		if result.LauncherJobID != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "  launcher_job_id: %s\n", result.LauncherJobID)
		}
	}
	return nil
}
