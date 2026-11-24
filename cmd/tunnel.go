package cmd

import (
	"fmt"
	"io"
	"time"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/tunnel"
	"github.com/spf13/cobra"
)

var tunnelCmd = &cobra.Command{
	Use:   "tunnel",
	Short: "Connect boid to a Heddleway gateway as a local service",
}

var tunnelPairCmd = &cobra.Command{
	Use:   "pair <gateway URL>",
	Short: "Pair boid with your account at a Heddleway gateway",
	Long: `Pair boid with your account at a Heddleway gateway.

boid creates a key in its data home, asks the gateway for a code, and waits
for you to approve it at the gateway. Once approved, the daemon keeps the
tunnel open and reconnects by itself. Pairing again replaces the key.`,
	Args: cobra.ExactArgs(1),
	RunE: runTunnelPair,
}

var tunnelStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the tunnel connection",
	Args:  cobra.NoArgs,
	RunE:  runTunnelStatus,
}

var tunnelForgetCmd = &cobra.Command{
	Use:   "forget",
	Short: "Disconnect and delete the tunnel key",
	Long: `Disconnect and delete the tunnel key. The registration at the gateway
stays until you remove it there.`,
	Args: cobra.NoArgs,
	RunE: runTunnelForget,
}

func init() {
	for _, c := range []*cobra.Command{tunnelPairCmd, tunnelStatusCmd, tunnelForgetCmd} {
		c.Annotations = map[string]string{scopeAnnotationKey: scopeRemote}
		tunnelCmd.AddCommand(c)
	}
	tunnelPairCmd.Flags().String("name", "boid", "Service name to propose (the owner may change it when approving)")
	tunnelPairCmd.Flags().Bool("no-wait", false, "Print the code and return without waiting for approval")
	rootCmd.AddCommand(tunnelCmd)
}

func runTunnelPair(cmd *cobra.Command, args []string) error {
	name, _ := cmd.Flags().GetString("name")
	noWait, _ := cmd.Flags().GetBool("no-wait")
	c := client.FromContext(cmd.Context())
	var p tunnel.Pairing
	if err := c.Do("POST", tunnel.MountPath+"/pair", map[string]string{"gateway": args[0], "name": name}, &p); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Open %s\n", p.VerificationURI)
	fmt.Fprintf(out, "and approve the code %s (it expires at %s).\n", p.Code, p.ExpiresAt.Local().Format("15:04"))
	fmt.Fprintf(out, "Key thumbprint: %s\n", p.Thumbprint)
	if noWait {
		return nil
	}
	for time.Now().Before(p.ExpiresAt.Add(10 * time.Second)) {
		time.Sleep(2 * time.Second)
		var s tunnel.ManagerStatus
		if err := c.Do("GET", tunnel.MountPath, nil, &s); err != nil {
			return err
		}
		if s.Pairing != nil && s.Pairing.Code == p.Code {
			continue
		}
		if s.PairError != "" {
			return fmt.Errorf("%s", s.PairError)
		}
		if s.Connection != nil {
			fmt.Fprintf(out, "Paired as %q.\n", s.Connection.Name)
			return nil
		}
	}
	return fmt.Errorf("the code expired")
}

func runTunnelStatus(cmd *cobra.Command, _ []string) error {
	c := client.FromContext(cmd.Context())
	var s tunnel.ManagerStatus
	if err := c.Do("GET", tunnel.MountPath, nil, &s); err != nil {
		return err
	}
	printTunnelStatus(cmd.OutOrStdout(), s)
	return nil
}

func printTunnelStatus(out io.Writer, s tunnel.ManagerStatus) {
	if s.Pairing != nil {
		fmt.Fprintf(out, "pairing: waiting for approval of %s at %s\n", s.Pairing.Code, s.Pairing.VerificationURI)
	}
	if s.PairError != "" {
		fmt.Fprintf(out, "last pairing: %s\n", s.PairError)
	}
	if s.Connection == nil {
		fmt.Fprintln(out, "not paired (boid tunnel pair <gateway URL>)")
		return
	}
	conn := s.Connection
	fmt.Fprintf(out, "gateway:    %s\nname:       %s\nservice id: %s\n", conn.Gateway, conn.Name, conn.ServiceID)
	switch {
	case conn.Stopped != "":
		fmt.Fprintf(out, "state:      stopped (%s)\n", conn.Stopped)
	case conn.Connected:
		fmt.Fprintf(out, "state:      connected since %s\n", conn.ConnectedAt.Local().Format(time.DateTime))
	default:
		fmt.Fprintln(out, "state:      reconnecting")
	}
	if conn.LastError != "" && !conn.Connected {
		fmt.Fprintf(out, "last error: %s\n", conn.LastError)
	}
}

func runTunnelForget(cmd *cobra.Command, _ []string) error {
	c := client.FromContext(cmd.Context())
	if err := c.Do("DELETE", tunnel.MountPath, nil, nil); err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Deleted the tunnel key. Remove the registration at the gateway too.")
	return nil
}
