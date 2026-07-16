package cmd

import (
	"fmt"
	"os"

	"github.com/novshi-tech/boid/internal/sandbox/runner"
	"github.com/spf13/cobra"
)

// The runner-* subcommands are the go-native sandbox launch chain (replacing
// the former outer.sh / setup.sh / inner.sh). They are internal plumbing: the
// daemon re-execs its own binary as `boid runner-outer`, which launches pasta →
// `boid runner-inner` → (clone) `boid runner-inner-child` → agent.
//
// They are hidden from help and never autostart the daemon (they ARE part of a
// sandbox the daemon launched). Each reads the JSON sandbox spec from --spec and
// appends diagnostics to --state, then exits with the sandbox's exit code.

func newRunnerCmd(use, short string, run func(specPath, statePath string) (int, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:           use,
		Short:         short,
		Hidden:        true,
		SilenceUsage:  true,
		SilenceErrors: true,
		Annotations: map[string]string{
			annotationSkipAutostart: "skip",
			// scopeLocal: these are the sandbox launch chain itself
			// (re-exec'd by the daemon), not a client calling the daemon's
			// API.
			scopeAnnotationKey: scopeLocal,
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			specPath, _ := cmd.Flags().GetString("spec")
			statePath, _ := cmd.Flags().GetString("state")
			if specPath == "" {
				return fmt.Errorf("%s: --spec is required", use)
			}
			code, err := run(specPath, statePath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[boid] %s: %v\n", use, err)
			}
			os.Exit(code)
			return nil
		},
	}
	cmd.Flags().String("spec", "", "path to the JSON sandbox spec")
	cmd.Flags().String("state", "", "path to the runner-state.json diagnostic file")
	return cmd
}

func init() {
	rootCmd.AddCommand(
		newRunnerCmd("runner-outer", "Internal: host-side sandbox launcher (pasta parent)", runner.RunOuter),
		newRunnerCmd("runner-inner", "Internal: sandbox runner inside pasta's namespace", runner.RunInner),
		newRunnerCmd("runner-inner-child", "Internal: sandbox runner inside the mount namespace", runner.RunInnerChild),
	)
}
