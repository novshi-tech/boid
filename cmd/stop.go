package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the boid server",
	RunE:  runStop,
}

func init() {
	stopCmd.Annotations = map[string]string{
		annotationSkipAutostart: "skip",
		// scopeLocal (per plan doc guidance): stop is daemon lifecycle
		// management, classified alongside start/gc even though its RunE
		// no longer talks to the daemon's HTTP API at all (see runStop's
		// own doc comment) — it drives compose directly, the same as
		// `boid start`'s own scopeLocal classification.
		scopeAnnotationKey: scopeLocal,
	}
	rootCmd.AddCommand(stopCmd)
}

// runStop implements `boid stop`. docs/plans/release-onboarding.md
// 決定2/PR5 redefines this from "POST /api/shutdown over the daemon's own
// socket" (which only ever stopped the daemon PROCESS, leaving compose's
// containers/networks/volumes and restart policy in place — a half-torn-
// down stack inconsistent with `boid start` now meaning "compose up") to
// an actual `compose down`, mirroring runComposeUp's own
// findComposeRoot-or-embedded-assets shape (cmd/host.go, cmd/start.go).
//
// codex round-12 review of PR5, Major: this used to run entirely without
// withHostModeLock (cmd/host.go) — the SAME flock ensureHostModeDaemon's
// own autostart path holds while bringing the compose stack up. Without
// it, a `boid stop` racing a concurrent autostart (another `boid`
// invocation noticing the daemon unreachable at the same moment) could
// read scripts/deploy-container.sh's engine-state file, run its own
// `compose down`, and finish AFTER the autostart's `compose up`
// completes — reporting "stack stopped" while a stack it just raced past
// keeps running. Sharing the one lock ensureHostModeDaemon already uses
// serializes stop against every autostart/explicit-start path instead.
func runStop(cmd *cobra.Command, args []string) error {
	return withHostModeLock(func() error {
		root, err := findComposeRoot()
		if err != nil {
			root, err = extractComposeAssets()
			if err != nil {
				return fmt.Errorf("boid stop: %w", err)
			}
		}
		if err := runComposeDownScript(cmd.Context(), root); err != nil {
			return fmt.Errorf("boid stop: %w", err)
		}
		fmt.Println("compose stack stopped")
		return nil
	})
}

// runComposeDownScript invokes <root>/scripts/deploy-container.sh --down
// — see runDeployScript's own doc comment (cmd/host.go) for why tearing
// the stack down shares that script's engine-detection/podman-overlay
// logic rather than reimplementing it here: `compose down` has to resolve
// the SAME compose project (engine, override files) `compose up` used, or
// it can silently target the wrong stack.
func runComposeDownScript(ctx context.Context, root string) error {
	scriptPath := filepath.Join(root, "scripts", "deploy-container.sh")
	deployCmd := exec.CommandContext(ctx, scriptPath, "--down") //nolint:gosec // scriptPath is built from a fixed, repo-relative suffix under either a located checkout root or this process's own extraction dir — not attacker-controlled input, same as runDeployScript's identical case
	deployCmd.Dir = root
	deployCmd.Stdout = os.Stderr
	deployCmd.Stderr = os.Stderr
	if err := deployCmd.Run(); err != nil {
		return fmt.Errorf("%s --down failed: %w", scriptPath, err)
	}
	return nil
}
