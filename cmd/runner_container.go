//go:build linux

package cmd

import (
	"fmt"
	"os"

	"github.com/novshi-tech/boid/internal/sandbox/runner"
	"github.com/novshi-tech/boid/internal/selfuser"
	"github.com/spf13/cobra"
)

// runner-container is the container-backend entry point: a job container's
// ENTRYPOINT execs the image-baked boid binary as `boid runner-container
// --spec ... --state ...` directly. It is internal plumbing: hidden from
// help and never autostarts the daemon. It reads the JSON sandbox spec from
// --spec, appends diagnostics to --state, then exits with the sandbox's
// exit code. Sole `boid runner-*` subcommand (docs/plans/volume-only-daemon.md §論点e).
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
			// Registers this container's arbitrary --user uid/gid so
			// passwd/id lookups (ssh, git credential helpers) succeed;
			// both calls are best-effort/non-fatal.
			selfuser.EnsureRuntimeUserRegistered()
			selfuser.ApplyGroupWritableUmask()
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
