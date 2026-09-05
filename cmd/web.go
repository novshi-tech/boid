package cmd

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"

	webauth "github.com/novshi-tech/boid/internal/api/auth"
	"github.com/novshi-tech/boid/internal/apiwire"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/qrterm"
	"github.com/spf13/cobra"
)

var webCmd = &cobra.Command{
	Use:   "web",
	Short: "Manage web access and paired devices",
}

var webPairCmd = &cobra.Command{
	Use:         "pair",
	Short:       "Issue a pairing code for a new web device",
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runWebPair,
}

var webDevicesCmd = &cobra.Command{
	Use:         "devices",
	Short:       "List paired web devices",
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runWebDevices,
}

var webRevokeCmd = &cobra.Command{
	Use:         "revoke <id>",
	Short:       "Revoke a specific web device",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runWebRevoke,
}

var webRevokeAllCmd = &cobra.Command{
	Use:         "revoke-all",
	Short:       "Revoke all web devices",
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runWebRevokeAll,
}

// webSetURLCmd / webSetAddrCmd are scopeRemote: they go through the same
// `boid config set` machinery (POST /api/config/mutate, cmd/config.go) so
// the daemon performs the read-modify-write on config.yaml, wherever it
// actually runs (docs/plans/release-onboarding.md 穴8 (b)).
var webSetURLCmd = &cobra.Command{
	Use:         "set-url <URL>",
	Short:       "Set the public URL in config.yaml (web.public_url, via `config set`)",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runWebSetURL,
}

var webSetAddrCmd = &cobra.Command{
	Use:         "set-addr <addr>",
	Short:       "Set the HTTP listen address in config.yaml (web.http_addr, via `config set`)",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runWebSetAddr,
}

var webPairLabel string

func init() {
	webPairCmd.Flags().StringVar(&webPairLabel, "label", "", "Label for the device")
	webCmd.AddCommand(webPairCmd, webDevicesCmd, webRevokeCmd, webRevokeAllCmd, webSetURLCmd, webSetAddrCmd)
	rootCmd.AddCommand(webCmd)
}

func runWebPair(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())

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
	c := client.FromContext(cmd.Context())

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
	c := client.FromContext(cmd.Context())
	id, err := resolveDeviceID(c, args[0])
	if err != nil {
		return err
	}
	if err := c.Do("DELETE", "/api/web/devices/"+id, nil, nil); err != nil {
		return fmt.Errorf("revoke device: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "revoked: %s\n", id)
	return nil
}

// resolveDeviceID accepts either a full device UUID or a unique prefix
// (matching what `boid web devices` displays) and resolves it to the full ID
// by listing devices and finding a unique prefix match.
func resolveDeviceID(c *client.Client, idOrPrefix string) (string, error) {
	var devices []webauth.DeviceInfo
	if err := c.Do("GET", "/api/web/devices", nil, &devices); err != nil {
		return "", fmt.Errorf("list devices: %w", err)
	}
	var matches []string
	for _, d := range devices {
		if strings.HasPrefix(d.ID, idOrPrefix) {
			matches = append(matches, d.ID)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no device with id or prefix %q (run 'boid web devices' to list)", idOrPrefix)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("prefix %q is ambiguous (matches %d devices: %s)",
			idOrPrefix, len(matches), strings.Join(matches, ", "))
	}
}

func runWebRevokeAll(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())
	if err := c.Do("DELETE", "/api/web/devices", nil, nil); err != nil {
		return fmt.Errorf("revoke all devices: %w", err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "all devices revoked")
	return nil
}

func runWebSetURL(cmd *cobra.Command, args []string) error {
	return webConfigMutateSet(cmd, "web.public_url", args[0])
}

func runWebSetAddr(cmd *cobra.Command, args []string) error {
	return webConfigMutateSet(cmd, "web.http_addr", args[0])
}

// webConfigMutateSet is `boid web set-url`/`set-addr`'s shared
// implementation: a single POST /api/config/mutate call, identical in
// shape to cmd/config.go's runConfigSet.
func webConfigMutateSet(cmd *cobra.Command, key, value string) error {
	c := client.FromContext(cmd.Context())
	var result apiwire.ConfigMutateResult
	if err := c.Do(http.MethodPost, "/api/config/mutate", apiwire.ConfigMutateRequest{
		Op:    apiwire.ConfigMutateSet,
		Key:   key,
		Value: []string{value},
	}, &result); err != nil {
		return fmt.Errorf("set %s: %w", key, err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%s = %s\n", key, value)
	for _, w := range result.Warnings {
		fmt.Fprintln(out, w)
	}
	return nil
}
