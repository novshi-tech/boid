package cmd

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	webauth "github.com/novshi-tech/boid/internal/api/auth"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/qrterm"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var webCmd = &cobra.Command{
	Use:   "web",
	Short: "Manage web access and paired devices",
}

var webPairCmd = &cobra.Command{
	Use:   "pair",
	Short: "Issue a pairing code for a new web device",
	RunE:  runWebPair,
}

var webDevicesCmd = &cobra.Command{
	Use:   "devices",
	Short: "List paired web devices",
	RunE:  runWebDevices,
}

var webRevokeCmd = &cobra.Command{
	Use:   "revoke <id>",
	Short: "Revoke a specific web device",
	Args:  cobra.ExactArgs(1),
	RunE:  runWebRevoke,
}

var webRevokeAllCmd = &cobra.Command{
	Use:   "revoke-all",
	Short: "Revoke all web devices",
	RunE:  runWebRevokeAll,
}

var webSetURLCmd = &cobra.Command{
	Use:         "set-url <URL>",
	Short:       "Set the public URL in config.yaml",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{annotationSkipAutostart: "skip"},
	RunE:        runWebSetURL,
}

var webSetAddrCmd = &cobra.Command{
	Use:         "set-addr <addr>",
	Short:       "Set the HTTP listen address in config.yaml",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{annotationSkipAutostart: "skip"},
	RunE:        runWebSetAddr,
}

var webPairLabel string

func init() {
	webPairCmd.Flags().StringVar(&webPairLabel, "label", "", "Label for the device")
	webCmd.AddCommand(webPairCmd, webDevicesCmd, webRevokeCmd, webRevokeAllCmd, webSetURLCmd, webSetAddrCmd)
	rootCmd.AddCommand(webCmd)
}

func runWebPair(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())

	req := webauth.PairRequest{Label: webPairLabel}
	var resp webauth.PairResponse
	if err := c.Do("POST", "/api/web/pair", req, &resp); err != nil {
		return fmt.Errorf("pair: %w", err)
	}

	return renderOutput(cmd, &resp, func() error {
		fmt.Fprintf(cmd.OutOrStdout(), "Pairing code: %s\n", resp.Code)

		if resp.URL == "" {
			fmt.Fprintln(cmd.OutOrStdout(), "boid web set-url <URL> で設定してください")
			return nil
		}

		fmt.Fprintf(cmd.OutOrStdout(), "URL: %s\n", resp.URL)

		remaining := time.Until(resp.ExpiresAt)
		if remaining > 0 {
			mins := int(math.Ceil(remaining.Minutes()))
			fmt.Fprintf(cmd.OutOrStdout(), "Expires in: %d minutes\n", mins)
		}

		qr, err := qrterm.Encode(resp.URL, false)
		if err != nil {
			return fmt.Errorf("qr: %w", err)
		}
		fmt.Fprint(cmd.OutOrStdout(), qr)
		return nil
	})
}

func runWebDevices(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())

	var devices []webauth.DeviceInfo
	if err := c.Do("GET", "/api/web/devices", nil, &devices); err != nil {
		return fmt.Errorf("list devices: %w", err)
	}

	return renderOutput(cmd, devices, func() error {
		if len(devices) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "no devices")
			return nil
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tLABEL\tLAST SEEN\tCREATED")
		for _, d := range devices {
			id := d.ID
			if len(id) > 8 {
				id = id[:8]
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
				id, d.Label,
				formatTime(d.LastSeenAt),
				formatTime(d.CreatedAt),
			)
		}
		return w.Flush()
	})
}

func runWebRevoke(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())
	if err := c.Do("DELETE", "/api/web/devices/"+args[0], nil, nil); err != nil {
		return fmt.Errorf("revoke device: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "revoked: %s\n", args[0])
	return nil
}

func runWebRevokeAll(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())
	if err := c.Do("DELETE", "/api/web/devices", nil, nil); err != nil {
		return fmt.Errorf("revoke all devices: %w", err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "all devices revoked")
	return nil
}

func runWebSetURL(cmd *cobra.Command, args []string) error {
	url := args[0]

	configDir, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("get config dir: %w", err)
	}
	configPath := filepath.Join(configDir, "boid", "config.yaml")

	var root map[string]any
	data, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read config: %w", err)
	}
	if err == nil {
		if unmarshalErr := yaml.Unmarshal(data, &root); unmarshalErr != nil {
			return fmt.Errorf("parse config: %w", unmarshalErr)
		}
	}
	if root == nil {
		root = make(map[string]any)
	}

	web, _ := root["web"].(map[string]any)
	if web == nil {
		web = make(map[string]any)
	}
	web["public_url"] = url
	root["web"] = web

	out, err := yaml.Marshal(root)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(configPath, out, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "web.public_url = %s\n", url)
	return nil
}

func runWebSetAddr(cmd *cobra.Command, args []string) error {
	addr := args[0]

	configDir, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("get config dir: %w", err)
	}
	configPath := filepath.Join(configDir, "boid", "config.yaml")

	var root map[string]any
	data, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read config: %w", err)
	}
	if err == nil {
		if unmarshalErr := yaml.Unmarshal(data, &root); unmarshalErr != nil {
			return fmt.Errorf("parse config: %w", unmarshalErr)
		}
	}
	if root == nil {
		root = make(map[string]any)
	}

	web, _ := root["web"].(map[string]any)
	if web == nil {
		web = make(map[string]any)
	}
	web["http_addr"] = addr
	root["web"] = web

	out, err := yaml.Marshal(root)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(configPath, out, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "web.http_addr = %s\n", addr)
	return nil
}
