package cmd

import (
	"bufio"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/spf13/cobra"
)

var secretCmd = &cobra.Command{
	Use:   "secret",
	Short: "Manage secrets",
}

var secretSetCmd = &cobra.Command{
	Use:         "set <key>",
	Short:       "Set a secret (reads value from stdin or prompts)",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runSecretSet,
}

var secretGetCmd = &cobra.Command{
	Use:         "get <key>",
	Short:       "Get a secret value",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runSecretGet,
}

var secretListCmd = &cobra.Command{
	Use:         "list",
	Short:       "List secret keys",
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runSecretList,
}

var secretDeleteCmd = &cobra.Command{
	Use:         "delete <key>",
	Short:       "Delete a secret",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runSecretDelete,
}

func init() {
	for _, c := range []*cobra.Command{secretSetCmd, secretGetCmd, secretListCmd, secretDeleteCmd} {
		c.Flags().StringP("namespace", "n", "default", "Secret namespace")
	}
	secretSetCmd.Flags().Bool("raw", false, "Store stdin verbatim, keeping the trailing newline (default strips one)")
	secretCmd.AddCommand(secretSetCmd, secretGetCmd, secretListCmd, secretDeleteCmd)
	rootCmd.AddCommand(secretCmd)
}

// secretStdinIsInteractive reports whether stdin is a terminal, and so
// whether `boid secret set` should prompt rather than read the stream.
//
// The test is "is stdin a character device", not "is stdin a pipe". Through
// 2026-08-07 this asked os.ModeNamedPipe instead, which recognised
// `printf ... | boid secret set K` but NOT `boid secret set K < file` — a
// redirected regular file matches neither ModeNamedPipe nor ModeCharDevice,
// so the redirect fell through to the interactive branch and the command sat
// waiting for a human that a restore script does not have. Asking about
// ModeCharDevice inverts the question so that every non-terminal stdin
// (pipe, regular file, /dev/null, a closed fd) is read as a stream, which is
// what a caller who redirected anything at all meant.
func secretStdinIsInteractive(stat os.FileInfo) bool {
	return stat.Mode()&os.ModeCharDevice != 0
}

// readPipedSecretValue reads all of r as a secret value.
//
// Unless raw is set, exactly ONE trailing newline is stripped — the one a
// shell here-string, `echo`, or a text file's final line terminator adds,
// which is never part of the value the caller meant. Everything else is
// preserved byte for byte. The pre-2026-08-07 implementation read through a
// bufio.Scanner and rejoined the lines with "\n", which could not represent
// a value whose trailing newlines were significant (they were all dropped)
// and quietly rewrote any \r\n input; reading the stream whole avoids both.
func readPipedSecretValue(r io.Reader, raw bool) (string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	value := string(b)
	if !raw {
		value = strings.TrimSuffix(value, "\n")
	}
	return value, nil
}

func runSecretSet(cmd *cobra.Command, args []string) error {
	key := args[0]
	namespace, _ := cmd.Flags().GetString("namespace")
	raw, _ := cmd.Flags().GetBool("raw")

	// Read value from stdin
	var value string
	stat, statErr := os.Stdin.Stat()
	// A stdin that cannot even be stat'd is not a terminal to prompt at, so
	// treat it as a stream and let the read below report the real problem.
	if statErr == nil && secretStdinIsInteractive(stat) {
		if raw {
			return fmt.Errorf("--raw needs a value on stdin (pipe or redirect one in); it cannot be combined with the interactive prompt")
		}
		fmt.Fprintf(os.Stderr, "Enter value for %q: ", key)
		scanner := bufio.NewScanner(os.Stdin)
		if scanner.Scan() {
			value = scanner.Text()
		}
	} else {
		v, err := readPipedSecretValue(os.Stdin, raw)
		if err != nil {
			return fmt.Errorf("read value from stdin: %w", err)
		}
		value = v
	}

	if value == "" {
		return fmt.Errorf("empty value")
	}

	c := client.FromContext(cmd.Context())
	req := map[string]string{"namespace": namespace, "key": key, "value": value}
	if err := c.Do("POST", "/api/secrets", req, nil); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "secret %q saved (namespace: %s)\n", key, namespace)
	return nil
}

func runSecretGet(cmd *cobra.Command, args []string) error {
	namespace, _ := cmd.Flags().GetString("namespace")
	c := client.FromContext(cmd.Context())
	var resp struct {
		Value string `json:"value"`
	}
	path := "/api/secrets/" + url.PathEscape(args[0]) + "/value?namespace=" + url.QueryEscape(namespace)
	if err := c.Do("GET", path, nil, &resp); err != nil {
		return err
	}
	fmt.Print(resp.Value)
	return nil
}

func runSecretList(cmd *cobra.Command, args []string) error {
	namespace, _ := cmd.Flags().GetString("namespace")
	c := client.FromContext(cmd.Context())
	var keys []string
	path := "/api/secrets?namespace=" + url.QueryEscape(namespace)
	if err := c.Do("GET", path, nil, &keys); err != nil {
		return err
	}
	for _, k := range keys {
		fmt.Println(k)
	}
	return nil
}

func runSecretDelete(cmd *cobra.Command, args []string) error {
	namespace, _ := cmd.Flags().GetString("namespace")
	c := client.FromContext(cmd.Context())
	path := "/api/secrets/" + url.PathEscape(args[0]) + "?namespace=" + url.QueryEscape(namespace)
	if err := c.Do("DELETE", path, nil, nil); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "secret %q deleted (namespace: %s)\n", args[0], namespace)
	return nil
}
