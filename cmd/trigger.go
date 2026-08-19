package cmd

// docs/plans/ingestion-identity.md PR-4 (B-5) 12 節「手動 1 巡の口」:
// `boid trigger run` はデバッグ用の手動 1 巡の口。daemon 内の
// TaskWorkflowService.RunTriggerNow (internal/api/trigger_loop.go) を
// POST /api/projects/{id}/triggers/{name}/run 経由で叩く、ホスト側 CLI。
//
// every の経過チェックは無視するが single-flight は無視しない — 実行中の
// run がある場合は Skipped: true が返る (強制二重起動の口ではない)。

import (
	"fmt"
	"net/url"
	"os"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/spf13/cobra"
)

var triggerCmd = &cobra.Command{
	Use:   "trigger",
	Short: "Manage project.yaml triggers",
}

var triggerRunProjectRef string

var triggerRunCmd = &cobra.Command{
	Use:           "run <name>",
	Short:         "Fire one trigger immediately (ignores `every`, still respects single-flight)",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args:          cobra.ExactArgs(1),
	Annotations:   map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:          runTriggerRun,
}

func init() {
	triggerRunCmd.Flags().StringVarP(&triggerRunProjectRef, "project", "p", "", "project ref (id or name, partial match supported)")
	_ = triggerRunCmd.RegisterFlagCompletionFunc("project", completeProjectRefs)
	triggerCmd.AddCommand(triggerRunCmd)
	rootCmd.AddCommand(triggerCmd)
}

func runTriggerRun(cobraCmd *cobra.Command, args []string) error {
	if triggerRunProjectRef == "" {
		return fmt.Errorf("-p/--project is required")
	}
	name := args[0]

	c := client.FromContext(cobraCmd.Context())
	project, err := resolveProjectRef(c, os.Stdin, os.Stderr, triggerRunProjectRef)
	if err != nil {
		return fmt.Errorf("resolve project ref %q: %w", triggerRunProjectRef, err)
	}

	var result map[string]any
	path := "/api/projects/" + project.ID + "/triggers/" + url.PathEscape(name) + "/run"
	if err := c.Do("POST", path, nil, &result); err != nil {
		return fmt.Errorf("run trigger: %w", err)
	}

	return renderOutput(cobraCmd, result, func() error {
		if skipped, _ := result["skipped"].(bool); skipped {
			reason, _ := result["reason"].(string)
			fmt.Fprintf(cobraCmd.OutOrStdout(), "skipped: %s\n", reason)
			return nil
		}
		jobID, _ := result["job_id"].(string)
		fmt.Fprintf(cobraCmd.OutOrStdout(), "fired: job %s\n", jobID)
		return nil
	})
}
