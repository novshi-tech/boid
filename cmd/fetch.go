package cmd

import (
	"fmt"

	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/spf13/cobra"
)

var fetchCmd = &cobra.Command{
	Use:   "fetch <url>",
	Short: "Fetch a URL and print its content as markdown",
	Args:  cobra.ExactArgs(1),
	Annotations: map[string]string{
		annotationSkipAutostart: "skip",
		scopeAnnotationKey:      scopeLocal,
	},
	DisableFlagsInUseLine: true,
	RunE:                  runFetch,
}

func init() {
	rootCmd.AddCommand(fetchCmd)
}

func runFetch(cmd *cobra.Command, args []string) error {
	req := &sandbox.FetchRequest{URL: args[0]}
	resp := sandbox.ExecFetch(req)
	if resp.Stdout != "" {
		fmt.Print(resp.Stdout)
	}
	if resp.Stderr != "" {
		fmt.Fprint(cmd.ErrOrStderr(), resp.Stderr)
		fmt.Fprintln(cmd.ErrOrStderr())
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("fetch failed")
	}
	return nil
}
