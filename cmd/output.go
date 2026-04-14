package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// getOutputFormat returns the --output flag value from the root command.
// Defaults to "plain" if the flag is not registered or the value is empty.
func getOutputFormat(cmd *cobra.Command) string {
	f, err := cmd.Root().PersistentFlags().GetString("output")
	if err != nil || f == "" {
		return "plain"
	}
	return f
}

// renderOutput renders v using the format specified by the --output root flag.
// For plain output, plainFn is called.
// For json/yaml output, v is marshaled and written to cmd.OutOrStdout().
func renderOutput(cmd *cobra.Command, v any, plainFn func() error) error {
	switch getOutputFormat(cmd) {
	case "json":
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s\n", b)
		return nil
	case "yaml":
		b, err := yaml.Marshal(v)
		if err != nil {
			return fmt.Errorf("marshal yaml: %w", err)
		}
		fmt.Fprint(cmd.OutOrStdout(), string(b))
		return nil
	default:
		return plainFn()
	}
}
