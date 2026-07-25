package cmd

import (
	"fmt"
	"os"

	"github.com/novshi-tech/boid/internal/sandbox/runner"
	"github.com/spf13/cobra"
)

// runner-container is the container-backend entry point
// (docs/plans/phase6-container-backend.md §PR2): a job container's
// ENTRYPOINT execs the image-baked boid binary as
// `boid runner-container --spec ... --state ...` directly (no shim, no
// pasta relay). It is internal plumbing: hidden from help and never
// autostarts the daemon (it IS part of a sandbox the daemon launched). It
// reads the JSON sandbox spec from --spec and appends diagnostics to
// --state, then exits with the sandbox's exit code.
//
// PR-4 (docs/plans/volume-only-daemon.md §論点e) removed the userns launch
// chain this used to run alongside (`boid runner-outer`/`runner-inner`/
// `runner-inner-child`, cmd/runner.go — unshare/pivot_root/pasta) along
// with the newRunnerCmd helper those three shared; `boid runner-container`
// is now the sole `boid runner-*` subcommand, so it builds its own minimal
// cobra.Command directly rather than sharing a factory with call sites that
// no longer exist.
func init() {
	cmd := &cobra.Command{
		Use:           "runner-container",
		Short:         "Internal: container entrypoint (the sole sandbox launch path since PR-4)",
		Hidden:        true,
		SilenceUsage:  true,
		SilenceErrors: true,
		Annotations: map[string]string{
			annotationSkipAutostart: "skip",
			// scopeLocal: this is the sandbox launch chain itself (the job
			// container's own ENTRYPOINT), not a client calling the
			// daemon's API.
			scopeAnnotationKey: scopeLocal,
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			specPath, _ := cmd.Flags().GetString("spec")
			statePath, _ := cmd.Flags().GetString("state")
			if specPath == "" {
				return fmt.Errorf("runner-container: --spec is required")
			}
			code, err := runner.RunContainer(specPath, statePath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[boid] runner-container: %v\n", err)
			}
			os.Exit(code)
			return nil
		},
	}
	cmd.Flags().String("spec", "", "path to the JSON sandbox spec")
	cmd.Flags().String("state", "", "path to the runner-state.json diagnostic file")
	rootCmd.AddCommand(cmd)
}
