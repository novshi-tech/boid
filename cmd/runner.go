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
// `boid runner-inner` → (clone CLONE_NEWNS) `boid runner-mount` →
// (clone CLONE_NEWUSER + chroot) `boid runner-inner-child` → agent.
//
// They are hidden from help and never autostart the daemon (they ARE part of a
// sandbox the daemon launched). runner-{outer,inner,mount} read the JSON sandbox
// spec from --spec and append diagnostics to --state; runner-inner-child is
// chrooted and instead reads the spec / state from inherited fds 3 / 4.

func newRunnerCmd(use, short string, run func(specPath, statePath string) (int, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:           use,
		Short:         short,
		Hidden:        true,
		SilenceUsage:  true,
		SilenceErrors: true,
		Annotations:   map[string]string{annotationSkipAutostart: "skip"},
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

// runnerInnerChildCmd is special: it runs already chrooted, so it takes no path
// flags and reads the spec / runner-state from inherited fds.
var runnerInnerChildCmd = &cobra.Command{
	Use:           "runner-inner-child",
	Short:         "Internal: sandbox agent host inside the chrooted user namespace",
	Hidden:        true,
	SilenceUsage:  true,
	SilenceErrors: true,
	Annotations:   map[string]string{annotationSkipAutostart: "skip"},
	RunE: func(cmd *cobra.Command, _ []string) error {
		code, err := runner.RunInnerChild()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[boid] runner-inner-child: %v\n", err)
		}
		os.Exit(code)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(
		newRunnerCmd("runner-outer", "Internal: host-side sandbox launcher (pasta parent)", runner.RunOuter),
		newRunnerCmd("runner-inner", "Internal: sandbox runner inside pasta's namespace", runner.RunInner),
		newRunnerCmd("runner-mount", "Internal: sandbox mount-namespace setup", runner.RunMount),
		runnerInnerChildCmd,
	)
}
