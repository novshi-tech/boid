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
// Both paths below invoke the SAME scripts/deploy-container.sh — single
// source of truth for what "deploy the container backend" means (round-3
// codex review: an earlier revision of the fallback path ran a bare
// `compose up -d` directly, silently skipping that script's config-seed +
// effective-backend validation, BOID_UID/BOID_GID/DOCKER_GID setup,
// runtime-dir provisioning, and podman socket preflight — a fresh
// no-checkout deploy would start the daemon in userns mode instead of
// container mode, with the CLI listener unreachable). They differ only in
// which ROOT directory the script runs from, and whether that root is
// allowed to BUILD a fresh image from it.
//
// PRIMARY path (findComposeRoot found a checkout — BOID_COMPOSE_ROOT env
// override, or walking up from cwd looking for scripts/deploy-container.sh
// — nose's actual dev workflow always runs `boid` from within, or below,
// this checkout): host mode invokes deploy-container.sh directly out of
// that checkout (deployFromCheckout), letting it build a fresh image when
// needed. build/container/Dockerfile's build context is `COPY . .` — the
// ENTIRE go source tree — so this path is the only one that can ever BUILD
// a fresh image; there is no way around needing a real checkout for that
// (embedding what an image build needs into the very binary being built
// from that same source would mean embedding a full second copy of the
// repository inside itself — impractical and circular).
//
// FALLBACK path (round-2 codex review Major 1 — "host mode cannot autostart
// from an installed standalone CLI"; no checkout discoverable, e.g.
// /usr/local/bin/boid invoked from an ordinary project directory):
// deployFromEmbeddedAssets extracts scripts/deploy-container.sh (go:embed'd
// into its own package, boidscripts.Assets — a separate package from
// compose.yml/Dockerfile below since go:embed cannot cross a directory
// boundary with "..") ALONGSIDE build/container/{compose.yml,Dockerfile}
// (go:embed'd into build/container's own package, boidcontainer.Assets)
// into a directory tree that mirrors this repo's own layout (so the
// script's own `ROOT_DIR="$(dirname "${BASH_SOURCE[0]}")/.."`-relative
// path computation resolves exactly as it would from a real checkout), then
// runs that extracted script with DEPLOY_CONTAINER_SKIP_BUILD=1 — which
// requires a "boid-runner:latest" image to already exist locally (built at
// some earlier point from within a real checkout — the common case for
// repeat invocations from an installed CLI on a host that has run host mode
// at least once before from a checkout) rather than attempting to build one
// (Dockerfile's `COPY . .` context is not present in an extracted asset
// directory — see above). No image and no checkout is a genuine dead end
// (nothing to build from, nothing to run) and fails with a clear, specific
// message before ever invoking the script, rather than either the old
// "could not locate scripts/deploy-container.sh" error (which fired even
// when a perfectly usable pre-built image already existed) or a confusing
// `docker build`/`git rev-parse` failure against a source-less extracted
// context.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	boidcontainer "github.com/novshi-tech/boid/build/container"
	"github.com/novshi-tech/boid/internal/atomicfile"
	"github.com/novshi-tech/boid/internal/client"
	boidscripts "github.com/novshi-tech/boid/scripts"
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

// cliTokenFileName names the persistent shared-secret file under
// hostModeConfigDir() (~/.config/boid — profiles.ConfigPath()'s
// os.UserConfigDir()-based "boid" subdirectory). cliLockFileName names the
// flock file under hostModeAssetsDir() instead (XDG_STATE_HOME — round-3
// codex review Minor 2, withHostModeLock's own doc comment explains why it
// moved out of the config dir).
const (
	cliTokenFileName = "cli-token"
	cliLockFileName  = "cli-lock"
)

// minCLITokenLen is validateCLIToken's floor for "this doesn't look like a
// truncated/corrupt file" — loadOrCreateCLIToken's own generated tokens are
// always exactly 64 hex characters (32 random bytes), so anything shorter
// than half that is implausible as a genuine token and almost certainly a
// mistake (partial copy-paste, truncated restore, ...).
const minCLITokenLen = 32

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
	return validateCLIToken(path, data)
}

// validateCLIToken trims surrounding whitespace from raw and validates the
// result is a plausible token (round-2 codex review Minor 2). A manually
// restored token file commonly carries a trailing newline (e.g. `echo
// $TOKEN > cli-token` instead of `printf`), which — left untrimmed — would
// flow straight into an `Authorization: Bearer <token>\n` header value and
// fail deep inside net/http with a cryptic "invalid header field value"
// nowhere near path, the actual file at fault. Rejecting anything
// implausibly short or still containing embedded whitespace after trimming
// catches other corruption shapes (partial copy-paste, truncated restore)
// the same way, with an error that names path directly.
func validateCLIToken(path string, raw []byte) (string, error) {
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("cli token file %s is empty (after trimming whitespace) — remove it to regenerate, or restore a valid one", path)
	}
	if len(token) < minCLITokenLen {
		return "", fmt.Errorf("cli token file %s holds a token shorter than %d characters (got %d) — looks truncated or corrupt; remove it to regenerate, or restore a valid one", path, minCLITokenLen, len(token))
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return "", fmt.Errorf("cli token file %s contains embedded whitespace — looks corrupt; remove it to regenerate, or restore a valid one", path)
	}
	return token, nil
}

// hostModeProbeResult is probeHostMode's outcome — distinguishes "not
// reachable yet, keep polling" from "reachable but will never succeed, stop
// polling" (round-3 codex review Regression coverage item 3).
type hostModeProbeResult int

const (
	// hostModeUnreachable covers everything retry-worthy: connection
	// refused/timeout (daemon still starting), and any non-2xx/non-404
	// status (e.g. 401 for a wrong/stale token — see
	// TestHostModeHealthy_WrongToken_ReportsUnhealthy's own comment for why
	// that specific case is deliberately treated as retryable here rather
	// than a third terminal outcome).
	hostModeUnreachable hostModeProbeResult = iota
	hostModeHealthyResult
	// hostModeStaleImage means the daemon answered HTTP at all but returned
	// 404 for /api/cli-token-check specifically — the endpoint genuinely
	// does not exist in whatever image is running, not "not authenticated
	// yet" or "still starting". That only happens with a boid-runner image
	// built before this CLI-listener endpoint existed. Waiting out the full
	// hostModeStartTimeout for a condition that can never resolve on its
	// own wastes 5 minutes on every single invocation; ensureHostModeDaemon
	// already has everything it needs to detect this immediately.
	hostModeStaleImage
)

// probeHostMode issues one GET /api/cli-token-check against addr — an
// AUTHENTICATED no-op route (internal/server/wire.go) — using token as the
// Authorization: Bearer credential, and classifies the result. See
// hostModeHealthy (kept as the simple bool wrapper most callers want) and
// waitForHealthy (which needs the finer hostModeStaleImage distinction) for
// how the two outcomes besides "healthy" are used.
//
// round-2 codex review Blocker 2: this used to hit the public /api/health
// instead, which proves only that some process is listening, not that the
// token host mode is about to use for every real request actually works.
// Concrete failure that fix closed: ~/.config/boid/cli-token gets
// deleted/regenerated on the host (e.g. manually, or by wiping
// ~/.config/boid) while the daemon container keeps running with the OLD
// BOID_CLI_TOKEN baked into its process environment — /api/health would
// keep reporting 200 forever, so ensureHostModeDaemon would never notice
// and never redeploy, and every subsequent CLI command would 401
// indefinitely. Checking the SAME token this invocation is about to
// dispatch its real request with, against the SAME auth middleware that
// gates every other /api/* route, closes that gap directly instead of
// reimplementing a parallel comparison here.
func probeHostMode(ctx context.Context, addr, token string) hostModeProbeResult {
	reqCtx, cancel := context.WithTimeout(ctx, hostModeProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "http://"+addr+"/api/cli-token-check", nil)
	if err != nil {
		return hostModeUnreachable
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return hostModeUnreachable
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return hostModeHealthyResult
	case resp.StatusCode == http.StatusNotFound:
		return hostModeStaleImage
	default:
		return hostModeUnreachable
	}
}

func hostModeHealthy(ctx context.Context, addr, token string) bool {
	return probeHostMode(ctx, addr, token) == hostModeHealthyResult
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

// composeImageTag is the sole tag build/container/compose.yml's `daemon`
// service references (it has no `build:` section of its own — see that
// file's own header comment) and the STABLE second tag
// scripts/deploy-container.sh's own `docker build ... -t "$IMAGE_TAG" -t
// boid-runner:latest` step always applies, regardless of which git commit
// built the image. deployFromEmbeddedAssets checks for this exact tag to
// decide whether the "compose up an already-built image, no checkout
// needed" fallback is even possible.
const composeImageTag = "boid-runner:latest"

// hostModeAssetsDir returns (creating if necessary) the stable directory
// deployFromEmbeddedAssets extracts its embedded assets into —
// $XDG_STATE_HOME/boid/compose (or ~/.local/state/boid/compose), mirroring
// internal/client's autostartLogPath's own XDG_STATE_HOME fallback
// convention. State-dir, not config-dir (hostModeConfigDir): these are
// regenerable, binary-version-derived artifacts, not a standalone secret
// like cli-token.
//
// This directory doubles as an emulated boid-repo ROOT for
// scripts/deploy-container.sh's own purposes (extractComposeAssets writes
// scripts/deploy-container.sh and build/container/{compose.yml,Dockerfile}
// underneath it, mirroring this repo's own layout) — the returned path is
// what deployFromEmbeddedAssets passes as ROOT to runDeployScript, exactly
// like findComposeRoot's checkout root is for deployFromCheckout.
func hostModeAssetsDir() (string, error) {
	stateDir := os.Getenv("XDG_STATE_HOME")
	if stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		stateDir = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(stateDir, "boid", "compose")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return dir, nil
}

// extractComposeAssets writes the embedded scripts/deploy-container.sh
// (boidscripts.Assets, scripts/embed.go) and compose.yml/Dockerfile
// (boidcontainer.Assets, build/container/assets.go) into a directory tree
// rooted at hostModeAssetsDir() that mirrors this repo's own layout —
// <root>/scripts/deploy-container.sh and
// <root>/build/container/{compose.yml,Dockerfile} — so the extracted
// script's own `ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"`
// relative-path computation resolves exactly as it would from a real
// checkout (round-3 codex review: unify the checkout and embedded-assets
// host-mode paths onto the SAME script, rather than reimplementing a
// second, narrower copy of its config-seed/UID-GID/podman-preflight logic
// directly in Go). Returns <root>.
//
// Unconditionally overwrites on every call via atomicfile.WriteAtomic
// (write-temp + rename) — cheap, keeps the extracted copy from ever
// drifting stale against whichever `boid` binary version is running, and
// closes a real hazard a plain os.WriteFile would leave open (round-3
// codex review Minor 2): a concurrent reader (this repo's own
// deploy-container.sh, invoked by a second `boid` process) could otherwise
// observe a truncated/partially-written file mid-extraction. This is
// independent, defense-in-depth hardening alongside withHostModeLock's own
// fix (its own doc comment) for the specific race that motivated it —
// different XDG_CONFIG_HOME values resolving to different lock files
// while sharing the same XDG_STATE_HOME.
func extractComposeAssets() (string, error) {
	root, err := hostModeAssetsDir()
	if err != nil {
		return "", err
	}

	script, err := boidscripts.Assets.ReadFile("deploy-container.sh")
	if err != nil {
		return "", fmt.Errorf("read embedded deploy-container.sh: %w", err)
	}
	compose, err := boidcontainer.Assets.ReadFile("compose.yml")
	if err != nil {
		return "", fmt.Errorf("read embedded compose.yml: %w", err)
	}
	dockerfile, err := boidcontainer.Assets.ReadFile("Dockerfile")
	if err != nil {
		return "", fmt.Errorf("read embedded Dockerfile: %w", err)
	}

	scriptsDir := filepath.Join(root, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", scriptsDir, err)
	}
	buildDir := filepath.Join(root, "build", "container")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", buildDir, err)
	}

	// 0755: the script must be executable (runDeployScript execs it
	// directly, not via `sh scripts/deploy-container.sh`).
	if err := atomicfile.WriteAtomic(filepath.Join(scriptsDir, "deploy-container.sh"), 0o755, script); err != nil {
		return "", fmt.Errorf("extract deploy-container.sh: %w", err)
	}
	if err := atomicfile.WriteAtomic(filepath.Join(buildDir, "compose.yml"), 0o644, compose); err != nil {
		return "", fmt.Errorf("extract compose.yml: %w", err)
	}
	if err := atomicfile.WriteAtomic(filepath.Join(buildDir, "Dockerfile"), 0o644, dockerfile); err != nil {
		return "", fmt.Errorf("extract Dockerfile: %w", err)
	}
	return root, nil
}

// usableEngine reports whether name (docker or podman) is not just present
// on PATH but actually answers `<name> version` — mirrors
// scripts/deploy-container.sh's own usable() helper (round-2 codex review
// Major 4): a CLI binary can be installed with no reachable daemon/socket
// behind it, and presence-only detection would wrongly prefer that over a
// genuinely working second engine.
func usableEngine(ctx context.Context, name string) bool {
	if _, err := exec.LookPath(name); err != nil {
		return false
	}
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return exec.CommandContext(checkCtx, name, "version").Run() == nil
}

// dockerComposeUsable reports whether `docker compose version` succeeds —
// distinct from usableEngine(ctx, "docker"), which only proves the docker
// ENGINE is reachable (`docker version`), not that the compose v2 PLUGIN
// this whole file's compose invocations depend on is installed alongside
// it (round-3 codex review Minor 1). A docker install with a reachable
// daemon but no compose plugin would otherwise be preferred over a
// genuinely usable podman+podman-compose and only fail much later, at the
// actual `up` call.
func dockerComposeUsable(ctx context.Context) bool {
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return exec.CommandContext(checkCtx, "docker", "compose", "version").Run() == nil
}

// detectComposeEngine picks a usable engine + compose invocation, the same
// preference order as scripts/deploy-container.sh's own engine selection
// (docker first, podman via podman-compose second) — kept independent of
// that script (this decides whether the no-checkout fallback is even
// possible at all, before extracting/invoking a script that only exists
// once extracted), but deliberately narrow: unlike the script, this has no
// build/podman-socket-preflight/config-seed responsibilities at all, only
// "is there an engine that can plausibly run `compose up -d`".
func detectComposeEngine(ctx context.Context) (engine string, composeCmd []string, err error) {
	if usableEngine(ctx, "docker") && dockerComposeUsable(ctx) {
		return "docker", []string{"docker", "compose"}, nil
	}
	if usableEngine(ctx, "podman") {
		if _, lookErr := exec.LookPath("podman-compose"); lookErr == nil {
			return "podman", []string{"podman-compose"}, nil
		}
		return "", nil, fmt.Errorf("podman found but no podman-compose on PATH")
	}
	return "", nil, fmt.Errorf("no usable docker+compose or podman+podman-compose engine found on PATH")
}

// imageExists reports whether composeImageTag is already present in the
// given engine's local image store — the precondition
// deployFromEmbeddedAssets requires before invoking the deploy script at
// all, since the embedded-assets fallback can never build a fresh image
// (this file's own header comment).
func imageExists(ctx context.Context, engine, tag string) bool {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return exec.CommandContext(checkCtx, engine, "image", "inspect", tag).Run() == nil
}

// runDeployScript invokes <root>/scripts/deploy-container.sh — the single
// implementation both deployFromCheckout and deployFromEmbeddedAssets
// share (round-3 codex review: no more separate, narrower reimplementation
// of the embedded path). skipBuild, when true, sets
// DEPLOY_CONTAINER_SKIP_BUILD=1 in the child's environment — the embedded
// path's root has none of the go source Dockerfile's `COPY . .` build
// context needs, so it must never attempt a build (this file's own header
// comment); the checkout path always leaves it false, letting the script
// build a fresh image exactly as it always has.
func runDeployScript(ctx context.Context, root, token, addr string, skipBuild bool) error {
	scriptPath := filepath.Join(root, "scripts", "deploy-container.sh")
	deployCmd := exec.CommandContext(ctx, scriptPath) //nolint:gosec // scriptPath is built from a fixed, repo-relative suffix under either a located checkout root or this process's own extraction dir — not attacker-controlled input
	deployCmd.Dir = root
	// Filter out any inherited BOID_CLI_TOKEN before appending ours: a
	// naive append(os.Environ(), "BOID_CLI_TOKEN="+token) would leave TWO
	// entries when the invoking shell already exported one (e.g. a
	// stale/different value from a previous manual `docker compose` run)
	// — which one the child process sees for a duplicate key is
	// libc-dependent, not guaranteed to be "last wins". Filtering first
	// makes token's value unambiguous.
	env := append(filterEnv(os.Environ(), "BOID_CLI_TOKEN"), "BOID_CLI_TOKEN="+token)
	if skipBuild {
		env = append(env, "DEPLOY_CONTAINER_SKIP_BUILD=1")
	}
	deployCmd.Env = env
	// Progress goes to stderr, keeping stdout clean for the eventual
	// subcommand's own output (e.g. `boid task list -o json`).
	deployCmd.Stdout = os.Stderr
	deployCmd.Stderr = os.Stderr
	if err := deployCmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", scriptPath, err)
	}
	return waitForHealthy(ctx, addr, token)
}

// deployFromEmbeddedAssets is the round-2 codex review Major 1 fallback:
// invoked by ensureHostModeDaemon only when findComposeRoot could not
// locate a real boid repo checkout (see this file's own header comment for
// the two-path design). Extracts the embedded assets to an emulated
// checkout root (extractComposeAssets) and runs that root's own
// scripts/deploy-container.sh with DEPLOY_CONTAINER_SKIP_BUILD=1 — never
// attempting a `docker build`, which only ever works when composeImageTag
// already exists locally (checked explicitly up front, so the failure mode
// for a genuinely fresh host with neither a checkout nor a pre-built image
// is one clear, specific error rather than a confusing docker-build/
// git-rev-parse failure against a source-less extracted context).
func deployFromEmbeddedAssets(ctx context.Context, token, addr string) error {
	engine, _, err := detectComposeEngine(ctx)
	if err != nil {
		return fmt.Errorf(
			"no boid repo checkout found (set BOID_COMPOSE_ROOT, or run `boid` from within one) to build a fresh image, and %w", err)
	}
	if !imageExists(ctx, engine, composeImageTag) {
		return fmt.Errorf(
			"no boid repo checkout found (set BOID_COMPOSE_ROOT, or run `boid` from within one) to build a fresh image, "+
				"and no existing %q image found locally either — build it once from within a boid repo checkout "+
				"(scripts/deploy-container.sh) or pre-provision the image out of band", composeImageTag)
	}

	root, err := extractComposeAssets()
	if err != nil {
		return fmt.Errorf("extract embedded compose assets: %w", err)
	}

	fmt.Fprintln(os.Stderr, "boid: daemon container not reachable; no boid repo checkout found, starting the existing "+composeImageTag+" image via the embedded scripts/deploy-container.sh...")

	return runDeployScript(ctx, root, token, addr, true)
}

// waitForHealthy polls probeHostMode until it reports healthy, reports a
// stale image (fails immediately — see hostModeStaleImage's own doc
// comment), or hostModeStartTimeout passes — shared by both
// deployFromCheckout and deployFromEmbeddedAssets (via runDeployScript) so
// the two deploy strategies report readiness identically.
func waitForHealthy(ctx context.Context, addr, token string) error {
	deadline := time.Now().Add(hostModeStartTimeout)
	for time.Now().Before(deadline) {
		switch probeHostMode(ctx, addr, token) {
		case hostModeHealthyResult:
			fmt.Fprintln(os.Stderr, "boid: daemon container is up")
			return nil
		case hostModeStaleImage:
			return fmt.Errorf(
				"daemon container at %s is reachable but does not expose /api/cli-token-check (404) — "+
					"this looks like a stale boid-runner image that predates the CLI listener; the boid-runner image needs an update "+
					"(rebuild it from within a boid repo checkout via scripts/deploy-container.sh, or pre-provision a newer boid-runner:latest) "+
					"rather than waiting out the full startup timeout for a condition that cannot resolve on its own", addr)
		}
		time.Sleep(hostModeHealthPollInterval)
	}
	return fmt.Errorf("daemon container did not become healthy within %s (addr=%s)", hostModeStartTimeout, addr)
}

// deployFromCheckout is ensureHostModeDaemon's PRIMARY path: invokes
// scripts/deploy-container.sh from a real, located boid repo checkout root
// — see this file's own header comment for why this is the only path that
// can ever build a fresh image.
func deployFromCheckout(ctx context.Context, root, token, addr string) error {
	fmt.Fprintln(os.Stderr, "boid: daemon container not reachable; starting it now (this can take a while on first run)...")
	return runDeployScript(ctx, root, token, addr, false)
}

// withHostModeLock serializes concurrent daemon-start attempts (multiple
// `boid` invocations racing to notice "daemon not running" at once) behind
// an flock, so at most one deploy-container.sh invocation — and, for the
// embedded-assets path, at most one extractComposeAssets extraction — runs
// at a time. Linux-only (CLAUDE.md) — syscall.Flock is a direct,
// dependency-free fit; no need for a cross-platform library here.
//
// The lock file lives beside the extracted assets, under
// hostModeAssetsDir() (XDG_STATE_HOME) — NOT under hostModeConfigDir()
// (XDG_CONFIG_HOME), where it lived before this fix (round-3 codex review
// Minor 2). Two `boid` invocations that happen to see different
// XDG_CONFIG_HOME values (e.g. two shells with slightly different
// environments) but the SAME XDG_STATE_HOME previously took two DIFFERENT
// locks while racing to write the SAME extracted files — no mutual
// exclusion at all despite both intending to serialize against each other.
// Deriving the lock path from the same directory the race is actually
// over closes that gap directly; extractComposeAssets's own
// atomicfile.WriteAtomic use is independent, defense-in-depth hardening
// for the same race (its own doc comment) rather than a substitute for
// this fix — the two mechanisms guard different things (mutual exclusion
// vs. no reader ever observing a torn write).
func withHostModeLock(fn func() error) error {
	dir, err := hostModeAssetsDir()
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

// filterEnv returns env with every "key=..." entry removed — used to strip
// an inherited value before appending a caller-controlled replacement (see
// ensureHostModeDaemon's own BOID_CLI_TOKEN comment for why a plain append
// isn't safe when the key might already be present).
func filterEnv(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// ensureHostModeDaemon confirms the daemon container is reachable at addr
// (client.DefaultCLIAddr() in production; a parameter here so tests can
// point it at an httptest server instead), starting it if it is not — via
// deployFromCheckout or deployFromEmbeddedAssets, see this file's own
// header comment for which one and why — unless client.NoAutostartEnv
// (BOID_NO_AUTOSTART=1) is set, in which case it fails fast with a clear
// message instead (round-2 codex review Major 3: this used to ignore that
// existing global opt-out entirely, unlike the bare-metal
// client.EnsureRunningAt path, which has honored it since before this file
// existed).
func ensureHostModeDaemon(ctx context.Context, addr, token string) error {
	if hostModeHealthy(ctx, addr, token) {
		return nil
	}
	if os.Getenv(client.NoAutostartEnv) == "1" {
		return fmt.Errorf(
			"daemon container not reachable at %s and %s=1 is set; start it manually with `docker compose`/`podman compose` (see scripts/deploy-container.sh) up -d",
			addr, client.NoAutostartEnv)
	}
	return withHostModeLock(func() error {
		// Double-checked after acquiring the lock: another concurrent CLI
		// invocation may have already finished starting the daemon while
		// this one waited on the flock.
		if hostModeHealthy(ctx, addr, token) {
			return nil
		}

		// PRIMARY path: a real checkout was found — build (if needed) +
		// deploy via the existing, already-tested deploy-container.sh
		// (round-2 codex review Major 1). FALLBACK (round-2 Major 1, root
		// cause fixed round-3): no checkout found at all —
		// deployFromEmbeddedAssets extracts and runs that SAME script
		// against an ALREADY-BUILT image instead of reimplementing a
		// second, narrower copy of it. See this file's own header comment
		// for the full rationale of why these are the same script run from
		// two different roots, not two different implementations.
		if root, err := findComposeRoot(); err == nil {
			return deployFromCheckout(ctx, root, token, addr)
		}
		return deployFromEmbeddedAssets(ctx, token, addr)
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
	addr := client.DefaultCLIAddr()
	if err := ensureHostModeDaemon(ctx, addr, token); err != nil {
		return nil, fmt.Errorf("host mode: %w", err)
	}
	c, err := client.NewClient("http://"+addr, token)
	if err != nil {
		return nil, fmt.Errorf("host mode: build client: %w", err)
	}
	return c, nil
}
