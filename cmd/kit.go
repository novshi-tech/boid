package cmd

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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

// kitInitExecFn runs the sandbox runner as a child process (fork+wait) and
// waits for it to complete. It is a package-level variable so tests can
// override it to intercept the launch without actually running a sandbox.
//
// Unlike syscall.Exec, this does NOT replace the current process — the caller
// regains control after the child exits, allowing post-run scanning logic to
// execute.
var kitInitExecFn = func(argv0 string, argv []string, envv []string) error {
	cmd := exec.Command(argv0, argv[1:]...)
	cmd.Env = envv
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
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
	//    own; we pass a placeholder so the shell fall-through has something
	//    meaningful to log. The skill prompt is delivered through the adapter's
	//    default SKILL.md bootstrap (PR4 fills in the boid-kit-init SKILL.md).
	//
	//    XDG_DATA_HOME is forwarded into the sandbox so the skill agent (and the
	//    shell-adapter fake in e2e) can locate the kits directory without
	//    assuming ~/.local/share when XDG_DATA_HOME is overridden (e.g. in the
	//    E2E test harness).
	jobID := fmt.Sprintf("kit-init-%s", randomJobSuffix())
	sandboxEnv := map[string]string{}
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		sandboxEnv["XDG_DATA_HOME"] = xdg
	}
	spec := dispatcher.BuildInitJobSpec(dispatcher.InitJobInput{
		Profile:      sandbox.ProfileInit,
		WritableDirs: []string{kitsDir},
		Argv:         []string{"boid-kit-init"},
		DisplayName:  "boid kit init",
		HarnessType:  harness,
		Env:          sandboxEnv,
		// Bootstrap prompt — matches a trigger phrase from boid-kit-init
		// SKILL.md frontmatter so the embedded skill auto-loads the moment
		// the harness opens, instead of leaving the user staring at an empty
		// prompt with no clue what to type next.
		Instruction: "boid kit init を実行して",
	})

	// 6. Build the SandboxRuntimeInfo.
	//    ServerSocket is intentionally empty: kit init does not need daemon API
	//    access (it is designed to run before the daemon exists). The broker
	//    socket is also empty — runner.go:183 skips registration for ProfileInit.
	//    Adapter bindings (claude/codex/opencode each declare ~/.claude etc.) are
	//    resolved inside BuildSandboxSpec via registry.For(spec.HarnessType).Bindings().
	//
	//    ProxyPort, if any, must be wired so the in-sandbox HTTPS_PROXY env vars
	//    point at the daemon's egress proxy. ProfileInit puts the sandbox in a
	//    fresh netns whose nftables rules only permit CONNECT to the proxy port,
	//    so without this the AI agent harnesses fail to open any socket and
	//    surface `FailedToOpenSocket` instead of reaching api.anthropic.com.
	rt := dispatcher.SandboxRuntimeInfo{
		JobID:        jobID,
		BoidBinary:   boidBinary,
		ServerSocket: "", // daemon not required for kit init
		ProxyPort:    resolveDaemonProxyPort(out),
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

	// 7. Snapshot the kit dirs that already exist before the sandbox runs.
	//    We use this to identify which directories are new (generated by this
	//    invocation) when scanning for secrets after the sandbox exits.
	existingKitDirs, err := listKitDirs(kitsDir)
	if err != nil {
		return fmt.Errorf("snapshot kits dir: %w", err)
	}

	// 8. Run the runner-outer as a child process (fork+wait) and wait for
	//    completion. Using exec.Command (not syscall.Exec) preserves the current
	//    process so that the post-run secret scan below can execute.
	runnerArgs := []string{boidBinary, "runner-outer", "--spec", sb.SpecPath, "--state", sb.StatePath}
	if err := kitInitExecFn(boidBinary, runnerArgs, os.Environ()); err != nil {
		return err
	}

	// 9. Scan YAML files in any newly generated kit directories for secrets.
	//    If findings are detected the generated directories are removed and an
	//    error is returned so the caller sees a clear failure message.
	if err := scanNewKitDirs(kitsDir, existingKitDirs, out); err != nil {
		return err
	}

	// 10. Apply the legacy-* cleanup the skill recorded inside kitsDir, if any.
	//     This rewrites workspace.kits references for any legacy kit the skill
	//     renamed or deleted — work the sandbox cannot do itself because
	//     workspace.yaml lives outside its writable bind. A missing result file
	//     is fine (the skill performed no cleanup); the file is consumed on
	//     success so a second run does not re-apply the same mapping.
	return applyKitCleanupResult(kitsDir, "", out)
}

// listKitDirs returns the set of direct subdirectory names that exist under
// kitsDir. The returned map is used as a snapshot so callers can identify
// directories added by a subsequent sandbox run.
func listKitDirs(kitsDir string) (map[string]struct{}, error) {
	entries, err := os.ReadDir(kitsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]struct{}{}, nil
		}
		return nil, err
	}
	existing := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			existing[e.Name()] = struct{}{}
		}
	}
	return existing, nil
}

// scanNewKitDirs scans every *.yaml file inside directories that were created
// under kitsDir after the baseline snapshot was taken (i.e. dirs whose names
// are not in existing). If any secret-like pattern is detected the new
// directories are deleted and an error listing the (redacted) findings is
// returned. On success it prints a summary of generated kit names to out.
//
// This function is intentionally exported as a free function (not a method) so
// PR6 (workspace configure) can reuse the same logic with a different base
// directory.
func scanNewKitDirs(kitsDir string, existing map[string]struct{}, out io.Writer) error {
	entries, err := os.ReadDir(kitsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Nothing written — fine, just report zero kits.
			fmt.Fprintln(out, "generated kits: (none)")
			return nil
		}
		return fmt.Errorf("read kits dir after sandbox: %w", err)
	}

	var newDirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, seen := existing[e.Name()]; !seen {
			newDirs = append(newDirs, e.Name())
		}
	}

	if len(newDirs) == 0 {
		fmt.Fprintln(out, "generated kits: (none)")
		return nil
	}

	// Scan all *.yaml files in each new kit directory.
	var allFindings []orchestrator.SecretFinding
	for _, name := range newDirs {
		dirPath := filepath.Join(kitsDir, name)
		yamlFiles, walkErr := filepath.Glob(filepath.Join(dirPath, "*.yaml"))
		if walkErr != nil {
			return fmt.Errorf("glob yaml in %s: %w", dirPath, walkErr)
		}
		for _, yamlPath := range yamlFiles {
			findings, scanErr := orchestrator.ScanSecretsFile(yamlPath)
			if scanErr != nil {
				return fmt.Errorf("scan %s: %w", yamlPath, scanErr)
			}
			allFindings = append(allFindings, findings...)
		}
	}

	if len(allFindings) > 0 {
		// Rollback: remove all newly generated kit directories.
		for _, name := range newDirs {
			_ = os.RemoveAll(filepath.Join(kitsDir, name))
		}
		var sb strings.Builder
		sb.WriteString("secret scan: suspicious values detected in generated kit yaml — rolled back\n")
		for _, f := range allFindings {
			sb.WriteString("  ")
			sb.WriteString(f.String())
			sb.WriteString("\n")
		}
		return errors.New(strings.TrimRight(sb.String(), "\n"))
	}

	// Success: report generated kit names.
	fmt.Fprintf(out, "generated kits: %s\n", strings.Join(newDirs, ", "))
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
