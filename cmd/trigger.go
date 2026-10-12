package cmd

// `boid trigger run` はデバッグ用の手動 1 巡の口。daemon 内の
// TaskWorkflowService.RunTriggerNow を POST
// project または workspace の triggers/{name}/run 経由で叩く。
//
// every の経過チェックは無視するが single-flight は無視しない — 実行中の
// run がある場合は Skipped: true が返る。

import (
	"fmt"
	"net/url"
	"os"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/spf13/cobra"
)

var triggerCmd = &cobra.Command{
	Use:   "trigger",
	Short: "Manage project and workspace triggers",
}

var triggerRunProjectRef string
var triggerRunWorkspace string

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
	triggerRunCmd.Flags().StringVar(&triggerRunWorkspace, "workspace", "", "workspace slug (repository-free triggers)")
	_ = triggerRunCmd.RegisterFlagCompletionFunc("project", completeProjectRefs)
	triggerCmd.AddCommand(triggerRunCmd)
	rootCmd.AddCommand(triggerCmd)
}

func runTriggerRun(cobraCmd *cobra.Command, args []string) error {
	if (triggerRunProjectRef == "") == (triggerRunWorkspace == "") {
		return fmt.Errorf("exactly one of -p/--project or --workspace is required")
	}
	name := args[0]

	c := client.FromContext(cobraCmd.Context())
	var path string
	if triggerRunWorkspace != "" {
		path = "/api/workspaces/" + url.PathEscape(triggerRunWorkspace) + "/triggers/" + url.PathEscape(name) + "/run"
	} else {
		project, err := resolveProjectRef(c, os.Stdin, os.Stderr, triggerRunProjectRef)
		if err != nil {
			return fmt.Errorf("resolve project ref %q: %w", triggerRunProjectRef, err)
		}
		path = "/api/projects/" + project.ID + "/triggers/" + url.PathEscape(name) + "/run"
	}

	var result map[string]any
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
