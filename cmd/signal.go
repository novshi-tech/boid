package cmd

// docs/plans/signal-ingest-detailed-design.md §3.1 (PR-2): host-side CLI for
// the signal inbox — `boid signal list` (GET /api/signals) and `boid signal
// ack` (POST /api/signals/ack). Response/request shapes live in
// internal/apiwire (internal/apiwire/signal.go), not internal/api: this
// package talks to the daemon over HTTP only (client-type-import-ok-
// behavior-ng — see internal/apiwire/doc.go for why the split exists at
// all: importing internal/api here would drag internal/dispatcher,
// internal/db, internal/sandbox etc. behind it and break the
// GOOS=windows/darwin CLI build).

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/novshi-tech/boid/internal/apiwire"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

var signalCmd = &cobra.Command{
	Use:   "signal",
	Short: "Inspect and acknowledge the signal inbox",
}

var signalListCmd = &cobra.Command{
	Use:         "list",
	Short:       "List signals in the inbox",
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runSignalList,
}

var signalAckCmd = &cobra.Command{
	Use:         "ack <id>...",
	Short:       "Acknowledge one or more signals (idempotent)",
	Args:        cobra.MinimumNArgs(1),
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runSignalAck,
}

// registerSignalListFlags / registerSignalAckFlags are split out of init()
// so tests can re-register flags after cmd.ResetFlags() (package-level
// cobra.Command singletons otherwise leak flag state between test cases —
// see cmd/signal_test.go's newSignalCmdForTest).
func registerSignalListFlags(cmd *cobra.Command) {
	cmd.Flags().String("workspace", "", `Workspace slug (default: "default")`)
	cmd.Flags().String("source", "", "Filter by source, <pack>/<connector> (e.g. slack/mentions)")
	cmd.Flags().String("service", "", "Filter by service instance name")
	cmd.Flags().String("state", "", "Filter by state: pending|dead|acked|all (default: pending)")
	cmd.Flags().Int("limit", 0, "Max rows to return (default: server default)")
}

func registerSignalAckFlags(cmd *cobra.Command) {
	cmd.Flags().String("workspace", "", `Workspace slug (default: "default")`)
}

func init() {
	registerSignalListFlags(signalListCmd)
	registerSignalAckFlags(signalAckCmd)
	signalCmd.AddCommand(signalListCmd, signalAckCmd)
	rootCmd.AddCommand(signalCmd)
}

// resolveSignalWorkspaceFlag defaults to orchestrator.DefaultWorkspaceSlug
// ("default") when --workspace is omitted (signal-ingest-detailed-design.md
// §3.1). Uses the orchestrator constant directly rather than a local
// literal — cmd/ already imports internal/orchestrator in over 30 files
// (task.go, project.go, ...) for exactly this "shared type/constant, no
// behavior" reason (client-type-import-ok-behavior-ng), and it does not
// affect the GOOS=windows/darwin portable-client build (Opus review, PR
// #1011, F6: an earlier version of this file kept a duplicate local literal
// instead, on the mistaken assumption that importing orchestrator here was
// unsafe for that build).
func resolveSignalWorkspaceFlag(cmd *cobra.Command) string {
	ws, _ := cmd.Flags().GetString("workspace")
	if ws == "" {
		return orchestrator.DefaultWorkspaceSlug
	}
	return ws
}

func runSignalList(cmd *cobra.Command, args []string) error {
	workspace := resolveSignalWorkspaceFlag(cmd)
	source, _ := cmd.Flags().GetString("source")
	service, _ := cmd.Flags().GetString("service")
	state, _ := cmd.Flags().GetString("state")
	limit, _ := cmd.Flags().GetInt("limit")

	params := []string{"workspace_id=" + url.QueryEscape(workspace)}
	if source != "" {
		// SignalFilter.Connector matches the stored "<pack>/<connector>"
		// composite exactly — no split needed here (only the response
		// envelope's `source` block splits it back apart for display).
		params = append(params, "source="+url.QueryEscape(source))
	}
	if service != "" {
		params = append(params, "service="+url.QueryEscape(service))
	}
	if state != "" {
		params = append(params, "state="+url.QueryEscape(state))
	}
	if limit > 0 {
		params = append(params, "limit="+strconv.Itoa(limit))
	}
	path := "/api/signals?" + strings.Join(params, "&")

	c := client.FromContext(cmd.Context())
	var resp apiwire.ListSignalsResponse
	if err := c.Do("GET", path, nil, &resp); err != nil {
		return err
	}

	return renderOutput(cmd, resp.Signals, func() error {
		if len(resp.Signals) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "no signals")
			return nil
		}
		for _, s := range resp.Signals {
			src := s.Source.Pack + "/" + s.Source.Connector
			fmt.Fprintf(cmd.OutOrStdout(), "%-40s %-25s %-24s %s\n",
				s.ID, s.OccurredAt.Format(time.RFC3339), src, s.Title)
		}
		return nil
	})
}

func runSignalAck(cmd *cobra.Command, args []string) error {
	workspace := resolveSignalWorkspaceFlag(cmd)

	c := client.FromContext(cmd.Context())
	var resp apiwire.AckSignalsResponse
	req := apiwire.AckSignalsRequest{WorkspaceID: workspace, IDs: args}
	if err := c.Do("POST", "/api/signals/ack", req, &resp); err != nil {
		return err
	}

	return renderOutput(cmd, resp, func() error {
		fmt.Fprintf(cmd.OutOrStdout(), "acked %d signal(s)\n", len(resp.Acked))
		return nil
	})
}
