package cmd

// cmd/host.go implements "host mode" (nose directive, PR-3 Option 4
// redesign of #835, docs/plans/volume-only-daemon.md §論点c, 2026-07-25):
// the `boid` CLI itself becomes the outer wrapper responsible for the
// container-backend daemon's lifecycle, mirroring the shape of the
// pre-existing bare-metal on-demand daemon start UX (cmd/root.go's
// autostart, client.EnsureRunningAt) but for a `docker/podman compose`
// stack instead of a single host process.
//
// Opt-in via BOID_MODE=container (checked by hostModeEnabled) — the
// default remains today's profile-based resolution (bare-metal unix
// socket, or a named remote https:// profile), completely untouched by
// anything in this file. Nose configures BOID_MODE once per shell
// environment rather than the CLI trying to auto-detect it.
//
// On every scope=remote invocation (isRemoteScope — the commands that
// actually talk to the daemon's HTTP API; scope=local/neutral commands
// like `start`/`stop`/`login` fall through to the ordinary path unchanged,
// since they either are daemon-lifecycle machinery for the BARE-METAL
// daemon or don't need a daemon at all):
//
//  1. Read (or, on first use, generate) a persistent shared-secret token
//     from ~/.config/boid/cli-token (loadOrCreateCLIToken).
//  2. Confirm the daemon container answers GET /api/health on
//     client.DefaultCLIAddr() (127.0.0.1:8442); if not, serialize behind
//     an flock (~/.config/boid/cli-lock) and invoke
//     scripts/deploy-container.sh (build image if needed, `compose up -d`)
//     with BOID_CLI_TOKEN set to the token from step 1, then poll health
//     until it answers or a deadline passes.
//  3. Build a *client.Client against "http://<DefaultCLIAddr>" with the
//     token as its Bearer credential and inject it into cmd's context,
//     exactly like the ordinary path's resolveClient does.
//
// Deliberate simplification vs. a literal reading of the original
// go:embed suggestion: build/container/Dockerfile's build context is
// `COPY . .` — the ENTIRE go source tree, not just the compose/Dockerfile
// assets — so embedding what the image build actually needs into the very
// binary being built would mean embedding a full second copy of this
// repository's source inside itself. Impractical (and circular) at this
// project's current dogfooding stage — real third-party distribution
// packaging is separate, not-yet-started work. Host mode instead LOCATES
// the already-checked-out repo (findComposeRoot: BOID_COMPOSE_ROOT env
// override, or walking up from cwd looking for scripts/deploy-container.sh
// — nose's actual workflow always runs `boid` from within, or below, this
// checkout) and invokes the existing, already-tested deploy-container.sh
// directly — the "invoke from CLI" alternative the brief explicitly
// permitted alongside "extract". This also gets engine detection
// (docker/podman), the podman preflight, and cache-cheap re-builds for
// free, with no separate Go reimplementation to keep in sync.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/novshi-tech/boid/internal/atomicfile"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/spf13/cobra"
)

// boidModeEnv / boidModeContainer: BOID_MODE=container opts a shell
// environment into host mode for every subsequent `boid` invocation. Any
// other value (including unset, the default) leaves the ordinary
// profiles.Resolve-based path in cmd/root.go completely unaffected.
const (
	boidModeEnv       = "BOID_MODE"
	boidModeContainer = "container"
)

// cliTokenFileName / cliLockFileName live under the same ~/.config/boid
// directory profiles.ConfigPath() (config.yaml) uses — os.UserConfigDir()'s
// "boid" subdirectory.
const (
	cliTokenFileName = "cli-token"
	cliLockFileName  = "cli-lock"
)

// hostModeStartTimeout bounds how long ensureHostModeDaemon waits for the
// daemon container to answer its health endpoint after invoking
// deploy-container.sh — generous because a cold first run has to build the
// image from scratch (go build + apt installs, easily a minute-plus on a
// slow connection/host); a warm re-run (cached image layers, container
// already built once before) is seconds.
const hostModeStartTimeout = 5 * time.Minute

// hostModeHealthPollInterval is how often ensureHostModeDaemon re-checks
// health while waiting out hostModeStartTimeout.
const hostModeHealthPollInterval = 500 * time.Millisecond

// hostModeProbeTimeout bounds a single health-check HTTP round trip — kept
// short so a genuinely-not-listening daemon fails the "is it already up"
// check quickly rather than stalling every single `boid` invocation.
const hostModeProbeTimeout = 1500 * time.Millisecond

func hostModeEnabled() bool {
	return os.Getenv(boidModeEnv) == boidModeContainer
}

// isRemoteScope reports whether cmd is annotated boid.scope=remote — the
// commands host mode actually needs to intercept (they talk to the
// daemon's HTTP API). scope=local (start/stop/gc/...) and scope=neutral
// (login/logout) commands fall through to the ordinary
// profiles.Resolve-based path unchanged regardless of BOID_MODE: they
// either are bare-metal daemon lifecycle machinery this file has no
// opinion about, or need no daemon connection at all.
func isRemoteScope(cmd *cobra.Command) bool {
	return cmd.Annotations[scopeAnnotationKey] == scopeRemote
}

// hostModeConfigDir returns (creating if necessary) the ~/.config/boid
// directory the CLI token and lock files live under.
func hostModeConfigDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	d := filepath.Join(dir, "boid")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", d, err)
	}
	return d, nil
}

// loadOrCreateCLIToken reads ~/.config/boid/cli-token, generating a fresh
// 32-byte (hex-encoded) random token on first use. Uses
// atomicfile.PublishIfAbsent (PR-1a's existing "generate once, race-safe,
// read back whichever writer actually won" primitive — the exact
// semantics a persistent shared secret needs, unlike a value meant to be
// regenerated every call) rather than inventing a second file-generation
// pattern.
func loadOrCreateCLIToken() (string, error) {
	dir, err := hostModeConfigDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, cliTokenFileName)

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate cli token: %w", err)
	}
	candidate := []byte(hex.EncodeToString(raw))

	data, err := atomicfile.PublishIfAbsent(path, 0o600, candidate)
	if err != nil {
		return "", fmt.Errorf("load or create %s: %w", path, err)
	}
	return string(data), nil
}

// hostModeHealthy reports whether the daemon container's dedicated CLI
// listener answers GET /api/health within hostModeProbeTimeout.
// /api/health is intentionally public (auth.apiAuthRequired) — no token
// needed for this probe, so it works even before loadOrCreateCLIToken has
// run (not that any current caller orders it that way, but the health
// check itself has no reason to depend on the token).
func hostModeHealthy(ctx context.Context, addr string) bool {
	reqCtx, cancel := context.WithTimeout(ctx, hostModeProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "http://"+addr+"/api/health", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// findComposeRoot locates the boid repo checkout containing
// scripts/deploy-container.sh + build/container/{Dockerfile,compose.yml}
// — see this file's own header comment for why host mode invokes that
// script rather than embedding its assets. BOID_COMPOSE_ROOT, when set,
// wins outright (explicit override — e2e/run-container.sh's own host-mode
// wiring uses this so it never depends on the invoking shell's cwd).
// Otherwise walks up from the current working directory looking for
// scripts/deploy-container.sh, mirroring how `git`/similar tools locate a
// repo root from any subdirectory — nose's actual workflow always runs
// `boid` from within, or below, this checkout.
func findComposeRoot() (string, error) {
	if v := os.Getenv("BOID_COMPOSE_ROOT"); v != "" {
		return v, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "scripts", "deploy-container.sh")); statErr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf(
		"host mode: could not locate scripts/deploy-container.sh by walking up from the current directory; " +
			"run `boid` from within the boid repo checkout, or set BOID_COMPOSE_ROOT to its root")
}

// withHostModeLock serializes concurrent daemon-start attempts (multiple
// `boid` invocations racing to notice "daemon not running" at once) behind
// an flock on ~/.config/boid/cli-lock, so at most one deploy-container.sh
// invocation runs at a time. Linux-only (CLAUDE.md) — syscall.Flock is a
// direct, dependency-free fit; no need for a cross-platform library here.
func withHostModeLock(fn func() error) error {
	dir, err := hostModeConfigDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, cliLockFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open cli lock file %s: %w", path, err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock %s: %w", path, err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck // best-effort unlock; the fd close below also releases it
	return fn()
}

// ensureHostModeDaemon confirms the daemon container is reachable at
// client.DefaultCLIAddr(), starting it via scripts/deploy-container.sh
// (with BOID_CLI_TOKEN=token in its environment) if it is not.
func ensureHostModeDaemon(ctx context.Context, token string) error {
	addr := client.DefaultCLIAddr()
	if hostModeHealthy(ctx, addr) {
		return nil
	}
	return withHostModeLock(func() error {
		// Double-checked after acquiring the lock: another concurrent CLI
		// invocation may have already finished starting the daemon while
		// this one waited on the flock.
		if hostModeHealthy(ctx, addr) {
			return nil
		}

		root, err := findComposeRoot()
		if err != nil {
			return err
		}

		fmt.Fprintln(os.Stderr, "boid: daemon container not reachable; starting it now (this can take a while on first run)...")
		deployCmd := exec.CommandContext(ctx, filepath.Join(root, "scripts", "deploy-container.sh")) //nolint:gosec // fixed, repo-relative script path — not attacker-controlled input
		deployCmd.Dir = root
		deployCmd.Env = append(os.Environ(), "BOID_CLI_TOKEN="+token)
		// Progress goes to stderr, keeping stdout clean for the eventual
		// subcommand's own output (e.g. `boid task list -o json`).
		deployCmd.Stdout = os.Stderr
		deployCmd.Stderr = os.Stderr
		if err := deployCmd.Run(); err != nil {
			return fmt.Errorf("scripts/deploy-container.sh failed: %w", err)
		}

		deadline := time.Now().Add(hostModeStartTimeout)
		for time.Now().Before(deadline) {
			if hostModeHealthy(ctx, addr) {
				fmt.Fprintln(os.Stderr, "boid: daemon container is up")
				return nil
			}
			time.Sleep(hostModeHealthPollInterval)
		}
		return fmt.Errorf("daemon container did not become healthy within %s after deploy-container.sh (addr=%s)", hostModeStartTimeout, addr)
	})
}

// resolveHostModeClient implements BOID_MODE=container's connection
// resolution end to end: token management, daemon lifecycle, and building
// the resulting *client.Client — entirely independent of
// profiles.Resolve/ResolveWithoutToken, which remain the bare-metal/
// remote-profile resolution path untouched by this file.
func resolveHostModeClient(ctx context.Context) (*client.Client, error) {
	token, err := loadOrCreateCLIToken()
	if err != nil {
		return nil, fmt.Errorf("host mode: %w", err)
	}
	if err := ensureHostModeDaemon(ctx, token); err != nil {
		return nil, fmt.Errorf("host mode: %w", err)
	}
	c, err := client.NewClient("http://"+client.DefaultCLIAddr(), token)
	if err != nil {
		return nil, fmt.Errorf("host mode: build client: %w", err)
	}
	return c, nil
}
