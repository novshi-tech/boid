package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/config"
	orchestrator "github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

var kitCmd = &cobra.Command{
	Use:   "kit",
	Short: "Manage kits",
}

// kitInitCmd is a stub for the kit init command. The full implementation
// (environment scan + kit.yaml generation) will be added in a subsequent PR.
//
// When the full implementation dispatches a sandbox-based generation script,
// the JobSpec must set:
//   SandboxProfile: int(sandbox.ProfileInit)
// This causes BuildPlan to mount the entire host root read-only (so the
// generation script can detect installed tools) and skips broker registration
// / socket mount (init scripts do not invoke boid host-commands).
//
// The `boid.autostart=skip` annotation opts this command out of the root
// PersistentPreRunE EnsureRunning hook so the first-time onboarding flow can
// run before a daemon exists. PR3 will add the sandboxed generation step;
// for now PR2 only resolves and persists default_harness.
var kitInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Generate kit.yaml for this machine (stub — full implementation in a future PR)",
	Args:  cobra.NoArgs,
	Annotations: map[string]string{
		annotationSkipAutostart: "skip",
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		return runKitInit(cmd.InOrStdin(), cmd.OutOrStdout())
	},
}

// runKitInit resolves the default harness (prompting the user on first run)
// and prints a stub line indicating where PR3 will pick up. It writes prompts
// to out and reads the user's response from in, so tests can drive it
// without touching the real terminal.
func runKitInit(in io.Reader, out io.Writer) error {
	harness, err := config.DefaultHarness()
	switch {
	case err == nil:
		// already configured
	case errors.Is(err, config.ErrDefaultHarnessNotSet):
		harness, err = promptDefaultHarness(in, out)
		if err != nil {
			return err
		}
		if err := config.SetDefaultHarness(harness); err != nil {
			return fmt.Errorf("save default harness: %w", err)
		}
		fmt.Fprintf(out, "saved default harness: %s\n", harness)
	default:
		return fmt.Errorf("resolve default harness: %w", err)
	}

	fmt.Fprintf(out, "default harness: %s\n", harness)
	fmt.Fprintln(out, "boid kit init: 生成スキルは今後の PR で実装予定です")
	return nil
}

// promptDefaultHarness reads a harness identifier from in, re-prompting on
// invalid input. It returns an error if in closes before a valid answer is
// given (non-TTY pipelines should set BOID_DEFAULT_HARNESS instead).
//
// Suggested choices are listed in the prompt but the input is not enum-checked
// beyond ValidateHarnessName — so locally-named harnesses (forks) work too.
func promptDefaultHarness(in io.Reader, out io.Writer) (string, error) {
	fmt.Fprintln(out, "No default harness configured.")
	fmt.Fprintln(out, "Choose the agent harness to use for boid generation skills.")
	fmt.Fprintln(out, "Suggested: claude, codex, opencode")

	scanner := bufio.NewScanner(in)
	const maxAttempts = 3
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		fmt.Fprint(out, "default harness> ")
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return "", fmt.Errorf("read default harness: %w", err)
			}
			return "", fmt.Errorf("no default harness provided (set %s to skip the prompt)", config.EnvDefaultHarness)
		}
		answer := strings.TrimSpace(scanner.Text())
		if err := config.ValidateHarnessName(answer); err != nil {
			fmt.Fprintf(out, "  %v\n", err)
			continue
		}
		return answer, nil
	}
	return "", fmt.Errorf("default harness not provided after %d attempts", maxAttempts)
}

var kitListCmd = &cobra.Command{
	Use:   "list",
	Short: "List installed kits",
	RunE: func(cmd *cobra.Command, args []string) error {
		reg := orchestrator.NewRegistry(defaultKitsDir())
		names, err := reg.List()
		if err != nil {
			return err
		}
		if len(names) == 0 {
			fmt.Println("no kits installed")
			return nil
		}
		for _, n := range names {
			fmt.Println(n)
		}
		return nil
	},
}

var kitRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove an installed kit",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		// Early validation, consistent with the workspace slug 3-layer
		// defense: fail fast on path-traversal / invalid characters so we
		// never filepath.Join an attacker-controlled value.
		if err := orchestrator.ValidKitName(name); err != nil {
			return err
		}

		// Check if any workspace references this kit.
		wsStore := orchestrator.NewWorkspaceStore("")
		slug, checkErr := workspacesReferencingKit(wsStore, name)
		if checkErr != nil {
			return fmt.Errorf("check workspace references: %w", checkErr)
		}
		if len(slug) > 0 {
			return fmt.Errorf("kit %q is referenced by workspace(s): %s\nRemove the kit from those workspaces first", name, strings.Join(slug, ", "))
		}

		reg := orchestrator.NewRegistry(defaultKitsDir())
		if err := reg.Remove(name); err != nil {
			return err
		}
		fmt.Printf("removed: %s\n", name)
		return nil
	},
}

// workspacesReferencingKit returns the slugs of workspaces whose Kits field
// contains the given kit name.
func workspacesReferencingKit(store *orchestrator.WorkspaceStore, kitName string) ([]string, error) {
	slugs, err := store.List()
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, slug := range slugs {
		ws, err := store.Load(slug)
		if err != nil {
			continue // skip unloadable workspaces
		}
		for _, k := range ws.Kits {
			if k == kitName {
				refs = append(refs, slug)
				break
			}
		}
	}
	return refs, nil
}

func reloadProjects() {
	c := client.NewUnixClient(client.DefaultSocketPath())
	if err := c.Do("POST", "/api/projects/reload", nil, nil); err != nil {
		return
	}
	fmt.Println("projects reloaded")
}

func init() {
	kitCmd.AddCommand(kitInitCmd, kitListCmd, kitRemoveCmd)
	rootCmd.AddCommand(kitCmd)
}
