package cmd

import (
	"bufio"
	"fmt"
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
	secretCmd.AddCommand(secretSetCmd, secretGetCmd, secretListCmd, secretDeleteCmd)
	rootCmd.AddCommand(secretCmd)
}

func runSecretSet(cmd *cobra.Command, args []string) error {
	key := args[0]
	namespace, _ := cmd.Flags().GetString("namespace")

	// Read value from stdin
	var value string
	stat, _ := os.Stdin.Stat()
	if stat.Mode()&os.ModeNamedPipe != 0 {
		// Piped input
		scanner := bufio.NewScanner(os.Stdin)
		var lines []string
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		value = strings.Join(lines, "\n")
	} else {
		// Interactive prompt
		fmt.Fprintf(os.Stderr, "Enter value for %q: ", key)
		scanner := bufio.NewScanner(os.Stdin)
		if scanner.Scan() {
			value = scanner.Text()
		}
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
