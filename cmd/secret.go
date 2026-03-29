package cmd

import (
	"bufio"
	"fmt"
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
	Use:   "set <key>",
	Short: "Set a secret (reads value from stdin or prompts)",
	Args:  cobra.ExactArgs(1),
	RunE:  runSecretSet,
}

var secretGetCmd = &cobra.Command{
	Use:   "get <key>",
	Short: "Get a secret value",
	Args:  cobra.ExactArgs(1),
	RunE:  runSecretGet,
}

var secretListCmd = &cobra.Command{
	Use:   "list",
	Short: "List secret keys",
	RunE:  runSecretList,
}

var secretDeleteCmd = &cobra.Command{
	Use:   "delete <key>",
	Short: "Delete a secret",
	Args:  cobra.ExactArgs(1),
	RunE:  runSecretDelete,
}

func init() {
	secretCmd.AddCommand(secretSetCmd, secretGetCmd, secretListCmd, secretDeleteCmd)
	rootCmd.AddCommand(secretCmd)
}

func runSecretSet(cmd *cobra.Command, args []string) error {
	key := args[0]

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

	c := client.NewUnixClient(client.DefaultSocketPath())
	req := map[string]string{"key": key, "value": value}
	if err := c.Do("POST", "/api/secrets", req, nil); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "secret %q saved\n", key)
	return nil
}

func runSecretGet(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())
	var resp struct {
		Value string `json:"value"`
	}
	if err := c.Do("GET", "/api/secrets/value?key="+args[0], nil, &resp); err != nil {
		return err
	}
	fmt.Print(resp.Value)
	return nil
}

func runSecretList(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())
	var keys []string
	if err := c.Do("GET", "/api/secrets", nil, &keys); err != nil {
		return err
	}
	for _, k := range keys {
		fmt.Println(k)
	}
	return nil
}

func runSecretDelete(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())
	if err := c.Do("DELETE", "/api/secrets?key="+args[0], nil, nil); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "secret %q deleted\n", args[0])
	return nil
}
