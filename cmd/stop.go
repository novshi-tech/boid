//go:build linux

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
		// scopeLocal: stop is daemon lifecycle management, classified
		// alongside start/gc (docs/plans/release-onboarding.md).
		scopeAnnotationKey: scopeLocal,
	}
	rootCmd.AddCommand(stopCmd)
}

// runStop implements `boid stop` as a `compose down` (docs/plans/
// release-onboarding.md 決定2), guarded by the same withHostModeLock flock
// ensureHostModeDaemon's autostart path holds so a stop can't race a
// concurrent autostart's `compose up`.
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

// runComposeDownScript invokes <root>/scripts/deploy-container.sh --down,
// sharing runDeployScript's (cmd/host.go) engine-detection/podman-overlay
// logic so `compose down` resolves the same project `compose up` used.
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
