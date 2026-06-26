package cmd

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/config"
	"github.com/novshi-tech/boid/internal/dispatcher"
	orchestrator "github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/skills"
	"github.com/spf13/cobra"
)

var kitCmd = &cobra.Command{
	Use:   "kit",
	Short: "Manage kits",
}

// kitInitCmd generates kit.yaml files for this machine by launching a sandboxed
// agent session in ProfileInit mode. The agent scans the host filesystem and
// writes generated kit.yaml files to ~/.local/share/boid/kits/.
//
// The command opts out of the root PersistentPreRunE EnsureRunning hook so
// first-time onboarding works without a running daemon (daemon 未起動な初手
// オンボーディングでも動く).
var kitInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Generate kit.yaml for this machine",
	Args:  cobra.NoArgs,
	Annotations: map[string]string{
		annotationSkipAutostart: "skip",
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		return runKitInit(cmd.InOrStdin(), cmd.OutOrStdout())
	},
}

// runKitInit resolves the default harness, deploys embedded skills to the host,
// and launches a sandboxed agent session (ProfileInit) that writes kit.yaml
// files to ~/.local/share/boid/kits/. The sandbox has read-only access to the
// full host root and read-write access to the kits directory.
func runKitInit(in io.Reader, out io.Writer) error {
	// 1. Resolve the default harness (prompting on first run).
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

	// 2. Deploy embedded skills to the host so the adapter can bind-mount them
	//    into the sandbox even when the daemon has never been started.
	skillsDir := defaultSkillsDir()
	if err := skills.DeployAll(skillsDir); err != nil {
		return fmt.Errorf("deploy skills: %w", err)
	}

	// 3. Ensure the kits directory exists so we can bind-mount it.
	kitsDir := defaultKitsDir()
	if err := os.MkdirAll(kitsDir, 0o755); err != nil {
		return fmt.Errorf("create kits dir: %w", err)
	}

	// 4. Resolve the boid binary path (runner-outer is re-exec'd via this path).
	boidBinary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve boid binary: %w", err)
	}

	// 5. Build the JobSpec via BuildInitJobSpec.
	//    The harness adapter (claude/codex/opencode) ignores Argv and builds its
	//    own; we pass a no-op placeholder so the shell fall-through has something
	//    meaningful to log. The skill prompt itself is delivered through
	//    BOID_USER_ANSWER (runner-inner-child threads it into RunContext.UserAnswer)
	//    which we do not set here — the agent uses the SKILL.md default bootstrap
	//    once PR4 fills in the boid-kit-init SKILL.md.
	jobID := fmt.Sprintf("kit-init-%s", randomJobSuffix())
	spec := dispatcher.BuildInitJobSpec(dispatcher.InitJobInput{
		Profile:     sandbox.ProfileInit,
		WritableDirs: []string{kitsDir},
		Argv:        []string{"boid-kit-init"},
		DisplayName: "boid kit init",
		HarnessType: harness,
	})

	// 6. Build the SandboxRuntimeInfo.
	//    ServerSocket is intentionally empty: kit init does not need daemon API
	//    access (it is designed to run before the daemon exists). The broker
	//    socket is also empty — runner.go:183 skips registration for ProfileInit.
	//    Adapter bindings (claude/codex/opencode each declare ~/.claude etc.) are
	//    resolved inside BuildSandboxSpec via registry.For(spec.HarnessType).Bindings().
	rt := dispatcher.SandboxRuntimeInfo{
		JobID:        jobID,
		BoidBinary:   boidBinary,
		ServerSocket: "", // daemon not required for kit init
		Foreground:   true,
	}

	sbSpec, err := dispatcher.BuildSandboxSpec(spec, rt)
	if err != nil {
		return fmt.Errorf("build sandbox spec: %w", err)
	}

	sb, err := dispatcher.NewSandboxPreparer().PrepareSandbox(sbSpec)
	if err != nil {
		return fmt.Errorf("prepare sandbox: %w", err)
	}
	if sb == nil || sb.SpecPath == "" {
		return fmt.Errorf("prepare sandbox: missing spec path")
	}

	// 7. Exec the runner-outer in place of this process (foreground mode).
	runnerArgs := []string{boidBinary, "runner-outer", "--spec", sb.SpecPath, "--state", sb.StatePath}
	return syscall.Exec(boidBinary, runnerArgs, os.Environ())
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

// defaultSkillsDir returns the host path where embedded skills are deployed.
// This mirrors the path used by the daemon server (internal/server/server.go)
// which derives it as filepath.Dir(cfg.DBPath) + "/skills", and defaultDBPath()
// places the DB at ~/.local/share/boid/boid.db.
func defaultSkillsDir() string {
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		home, _ := os.UserHomeDir()
		dataDir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataDir, "boid", "skills")
}

// randomJobSuffix returns a short random-enough suffix for use in job IDs.
// It reads 4 bytes from /dev/urandom and encodes them as hex; on any error
// it falls back to a fixed placeholder (collisions are harmless for ephemeral
// foreground jobs).
func randomJobSuffix() string {
	f, err := os.Open("/dev/urandom")
	if err != nil {
		return "0000"
	}
	defer f.Close()
	b := make([]byte, 4)
	if _, err := io.ReadFull(f, b); err != nil {
		return "0000"
	}
	return hex.EncodeToString(b)
}

func init() {
	kitCmd.AddCommand(kitInitCmd, kitListCmd, kitRemoveCmd)
	rootCmd.AddCommand(kitCmd)
}
