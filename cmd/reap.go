package cmd

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/moby/moby/client"
	"github.com/spf13/cobra"

	"github.com/novshi-tech/boid/internal/install"
	"github.com/novshi-tech/boid/internal/reap"
)

// reapInstallIDOverride lets an operator (or a test harness) point `boid
// reap` at a specific install_id rather than the one this host's
// ~/.local/share/boid/install_id resolves to — useful for the
// deploy-level-rollback scenario docs/plans/phase6-container-backend.md
// describes, where the reaper may need to run from a location whose
// default data dir differs from the compose daemon's.
var reapInstallIDOverride string

var reapCmd = &cobra.Command{
	Use:   "reap",
	Short: "Stop and remove every docker resource this boid install created (daemon-independent)",
	// docs/plans/phase6-container-backend.md §PR6/§決定6: `boid reap` is the
	// "deploy-level reaper" the plan's rollback contract requires — it must
	// work even when the compose daemon it is cleaning up after is down or
	// unreachable, so unlike almost every other command in this tree it
	// talks to the docker engine directly rather than through the boid
	// daemon's own HTTP API.
	Long: `boid reap stops and removes every docker container, network, and volume
belonging to this installation, found as the UNION of:

  - live docker resources carrying the boid.install_id=<id> label
    (containers/networks/volumes the daemon creates directly)
  - the per-job docker-resources.jsonl ledger under each job's runtime
    directory (sibling resources dockerproxy's client created — these carry
    no boid label at all, so the label query alone would miss them)

This talks to the docker engine directly (DOCKER_HOST / the platform
default socket) and does NOT require the boid daemon to be running — it is
meant to work as the deploy-level rollback reaper from host daemon +
userns "旧デプロイ" back after the container (compose) deploy, per
docs/plans/phase6-container-backend.md's rollback contract.`,
	SilenceUsage: true,
	RunE:         runReap,
}

func init() {
	reapCmd.Annotations = map[string]string{
		// Daemon-independent by design (see the Long description above) —
		// must never try to autostart a daemon, and is not itself daemon
		// lifecycle machinery the way start/stop/gc are, but the "never
		// talks to a remote profile" axis is identical, so it is classified
		// scopeLocal alongside them (cmd/root.go's scopeAnnotationKey doc
		// comment).
		annotationSkipAutostart: "skip",
		scopeAnnotationKey:      scopeLocal,
	}
	reapCmd.Flags().StringVar(&reapInstallIDOverride, "install-id", "", "Reap this install_id instead of the local ~/.local/share/boid/install_id")
	rootCmd.AddCommand(reapCmd)
}

func runReap(cmd *cobra.Command, args []string) error {
	dataDir := defaultInstallIDDir() // ~/.local/share/boid — same dir install_id/boid.db/web_secret live in

	installID := reapInstallIDOverride
	if installID == "" {
		id, err := install.LoadOrCreate(dataDir)
		if err != nil {
			return fmt.Errorf("resolve install id: %w", err)
		}
		installID = id
	}

	runtimesDir := filepath.Join(dataDir, "runtimes")

	// API-version negotiation is enabled by default in this SDK version
	// (client.WithAPIVersionNegotiation is a deprecated no-op) — client.FromEnv
	// alone is enough to pick up DOCKER_HOST/DOCKER_API_VERSION/etc.
	dockerClient, err := client.New(client.FromEnv)
	if err != nil {
		return fmt.Errorf("connect to docker: %w", err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "reaping install_id=%s (runtimes dir: %s)\n", installID, runtimesDir)

	report, err := reap.Run(context.Background(), dockerClient, installID, runtimesDir)
	if err != nil {
		return fmt.Errorf("reap: %w", err)
	}

	if report.Empty() {
		fmt.Fprintln(out, "nothing to reap")
		return nil
	}
	for _, id := range report.DestroyedContainers {
		fmt.Fprintf(out, "  destroyed container %s\n", id)
	}
	for _, id := range report.DestroyedNetworks {
		fmt.Fprintf(out, "  destroyed network %s\n", id)
	}
	for _, id := range report.DestroyedVolumes {
		fmt.Fprintf(out, "  destroyed volume %s\n", id)
	}
	for _, e := range report.Errors {
		fmt.Fprintf(out, "  ERROR: %s\n", e)
	}
	fmt.Fprintf(out, "reaped: %d containers, %d networks, %d volumes (%d errors)\n",
		len(report.DestroyedContainers), len(report.DestroyedNetworks), len(report.DestroyedVolumes), len(report.Errors))
	if len(report.Errors) > 0 {
		return fmt.Errorf("reap completed with %d error(s); see above", len(report.Errors))
	}
	return nil
}
