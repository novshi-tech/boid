package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/novshi-tech/boid/internal/client"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

var checkCmd = &cobra.Command{
	Use:          "check",
	Short:        "Check host prerequisites and hook dependencies",
	SilenceUsage: true,
	RunE:         runCheck,
}

func init() {
	checkCmd.Annotations = map[string]string{
		annotationSkipAutostart: "skip",
		// scopeLocal (codex review round 2, docs/plans/cli-remote-connection.md
		// classification table groups check with start/stop under "daemon
		// 生殺与奪"): check was scopeNeutral until this fix, on the reasoning
		// that it works standalone and only opportunistically queries the
		// daemon. That reasoning covers whether a daemon is *required*, but
		// under Phase 3's remote-profile model "local" is about a different
		// axis — whether the command's result is only meaningful on the same
		// host the daemon (and therefore the sandbox) actually runs on.
		// check's exec.LookPath/unshare probes inspect binaries and kernel
		// features on the machine running the CLI process itself; against a
		// future https:// (remote daemon) profile those would report on the
		// wrong host entirely, since sandboxes execute wherever the daemon
		// is, not wherever the CLI happens to run. See cmd/scope_annotations_test.go's
		// expectedScopeAnnotations table for the full cross-check against
		// the plan doc.
		scopeAnnotationKey: scopeLocal,
	}
	rootCmd.AddCommand(checkCmd)
}

var hostRequiredTools = []string{"passt"}

func runCheck(cmd *cobra.Command, args []string) error {
	allOK := true

	fmt.Println("=== Host required tools ===")
	for _, tool := range hostRequiredTools {
		if _, err := exec.LookPath(tool); err != nil {
			fmt.Printf("  MISSING: %s\n", tool)
			allOK = false
		} else {
			fmt.Printf("  OK: %s\n", tool)
		}
	}

	// Check unprivileged user namespaces (AppArmor restriction on Ubuntu 24.04+)
	fmt.Println("\n=== Sandbox prerequisites ===")
	if err := exec.Command("unshare", "--user", "--mount", "--map-root-user", "--", "true").Run(); err != nil {
		fmt.Println("  ERROR: unprivileged user namespaces not available")
		fmt.Println("         sandbox will not work")
		if data, err := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns"); err == nil {
			if strings.TrimSpace(string(data)) == "1" {
				fmt.Println("         AppArmor restricts unprivileged userns (kernel.apparmor_restrict_unprivileged_userns=1)")
				fmt.Println("         fix: sudo sysctl kernel.apparmor_restrict_unprivileged_userns=0")
			}
		}
		allOK = false
	} else {
		fmt.Println("  OK: unprivileged user namespaces")
	}

	// Check hook requires for registered projects
	c := client.FromContext(cmd.Context())
	var projects []projectspec.Project
	if err := c.Do("GET", "/api/projects", nil, &projects); err != nil {
		fmt.Printf("\n(server not running, skipping project hook checks)\n")
		if !allOK {
			return fmt.Errorf("some required tools are missing")
		}
		return nil
	}

	if len(projects) > 0 {
		fmt.Println("\n=== Hook dependencies ===")
		for _, p := range projects {
			for _, b := range p.Meta.TaskBehaviors {
				for _, h := range b.Hooks {
					for _, req := range h.Requires {
						if _, err := exec.LookPath(req); err != nil {
							fmt.Printf("  MISSING: %s (project: %s, hook: %s)\n", req, p.ID, h.ID)
							allOK = false
						} else {
							fmt.Printf("  OK: %s (project: %s, hook: %s)\n", req, p.ID, h.ID)
						}
					}
				}
			}
		}
	}

	if !allOK {
		return fmt.Errorf("some required tools are missing")
	}
	fmt.Println("\nAll checks passed.")
	return nil
}
