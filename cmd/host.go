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
// PRIMARY path (findComposeRoot found a checkout — BOID_COMPOSE_ROOT env
// override, or walking up from cwd looking for scripts/deploy-container.sh
// — nose's actual dev workflow always runs `boid` from within, or below,
// this checkout): host mode invokes the existing, already-tested
// deploy-container.sh directly (deployFromCheckout) rather than
// reimplementing its image build + engine detection + podman preflight in
// Go. build/container/Dockerfile's build context is `COPY . .` — the
// ENTIRE go source tree — so this path is the only one that can ever BUILD
// a fresh image; there is no way around needing a real checkout for that
// (embedding what an image build needs into the very binary being built
// from that same source would mean embedding a full second copy of the
// repository inside itself — impractical and circular).
//
// FALLBACK path (round-2 codex review Major 1 — "host mode cannot autostart
// from an installed standalone CLI"; no checkout discoverable, e.g.
// /usr/local/bin/boid invoked from an ordinary project directory):
// deployFromEmbeddedAssets extracts build/container/{compose.yml,Dockerfile}
// (go:embed'd into build/container's own package, boidcontainer.Assets) to
// a stable path and runs `compose up -d` against them directly — but ONLY
// when a "boid-runner:latest" image already exists locally (built at some
// earlier point from within a real checkout — the common case for repeat
// invocations from an installed CLI on a host that has run host mode at
// least once before from a checkout). No image and no checkout is a genuine
// dead end (nothing to build from, nothing to run) and fails with a clear,
// specific message rather than either the old "could not locate
// scripts/deploy-container.sh" error (which fired even when a perfectly
// usable pre-built image already existed) or a confusing `docker build`
// failure against a source-less extracted context.

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

// hostModeHealthy reports whether the daemon container's dedicated CLI
// listener answers GET /api/cli-token-check — an AUTHENTICATED no-op route
// (internal/server/wire.go) — within hostModeProbeTimeout, using token as
// the Authorization: Bearer credential.
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
func hostModeHealthy(ctx context.Context, addr, token string) bool {
	reqCtx, cancel := context.WithTimeout(ctx, hostModeProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "http://"+addr+"/api/cli-token-check", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
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
// deployFromEmbeddedAssets extracts its embedded compose.yml/Dockerfile
// into — $XDG_STATE_HOME/boid/compose (or ~/.local/state/boid/compose),
// mirroring internal/client's autostartLogPath's own XDG_STATE_HOME
// fallback convention. State-dir, not config-dir (hostModeConfigDir):
// these are regenerable, binary-version-derived artifacts, not a
// standalone secret like cli-token.
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

// extractComposeAssets writes the embedded compose.yml and Dockerfile
// (boidcontainer.Assets, build/container/assets.go) into
// hostModeAssetsDir(), unconditionally overwriting on every call — cheap,
// and keeps the extracted copy from ever drifting stale against whichever
// `boid` binary version is running (unlike loadOrCreateCLIToken's
// generate-ONCE secret, there is no "first writer wins" concern here: the
// content is a deterministic function of the binary, not random state, and
// this only ever runs serialized under withHostModeLock's flock anyway).
// Returns the directory the two files were written into.
func extractComposeAssets() (string, error) {
	dir, err := hostModeAssetsDir()
	if err != nil {
		return "", err
	}
	compose, err := boidcontainer.Assets.ReadFile("compose.yml")
	if err != nil {
		return "", fmt.Errorf("read embedded compose.yml: %w", err)
	}
	dockerfile, err := boidcontainer.Assets.ReadFile("Dockerfile")
	if err != nil {
		return "", fmt.Errorf("read embedded Dockerfile: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), compose, 0o644); err != nil {
		return "", fmt.Errorf("extract compose.yml: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), dockerfile, 0o644); err != nil {
		return "", fmt.Errorf("extract Dockerfile: %w", err)
	}
	return dir, nil
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

// detectComposeEngine picks a usable engine + compose invocation, the same
// preference order as scripts/deploy-container.sh's own engine selection
// (docker first, podman via podman-compose second) — kept independent of
// that script (this is the no-checkout fallback path, which by definition
// cannot invoke a script that only exists inside a checkout), but
// deliberately narrow: unlike the script, this has no build/podman-socket-
// preflight/config-seed responsibilities at all, only "pick a `compose up
// -d` command line that will work".
func detectComposeEngine(ctx context.Context) (engine string, composeCmd []string, err error) {
	if usableEngine(ctx, "docker") {
		return "docker", []string{"docker", "compose"}, nil
	}
	if usableEngine(ctx, "podman") {
		if _, lookErr := exec.LookPath("podman-compose"); lookErr == nil {
			return "podman", []string{"podman-compose"}, nil
		}
		return "", nil, fmt.Errorf("podman found but no podman-compose on PATH")
	}
	return "", nil, fmt.Errorf("no usable docker or podman engine found on PATH")
}

// imageExists reports whether composeImageTag is already present in the
// given engine's local image store — the precondition
// deployFromEmbeddedAssets requires before attempting `compose up` at all,
// since compose.yml has no `build:` section to fall back to.
func imageExists(ctx context.Context, engine, tag string) bool {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return exec.CommandContext(checkCtx, engine, "image", "inspect", tag).Run() == nil
}

// hostModeRuntimeDir mirrors scripts/deploy-container.sh's own
// BOID_RUNTIME_DIR computation (that script's own header comment explains
// why this must match internal/client.DefaultSocketPath()'s fallback chain
// rather than just XDG_RUNTIME_DIR-or-/run/user/<uid>) — duplicated here,
// narrowly, because deployFromEmbeddedAssets runs `compose up` directly
// rather than through that script when no checkout is available to invoke
// it from.
func hostModeRuntimeDir() string {
	if v := os.Getenv("BOID_RUNTIME_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_RUNTIME_DIR"); v != "" {
		return v
	}
	uid := os.Getuid()
	runDir := fmt.Sprintf("/run/user/%d", uid)
	if _, err := os.Stat(runDir); err == nil {
		return runDir
	}
	return "/tmp"
}

// deployFromEmbeddedAssets is the round-2 codex review Major 1 fallback:
// invoked by ensureHostModeDaemon only when findComposeRoot could not
// locate a real boid repo checkout (see this file's own header comment for
// the two-path design). Brings the compose stack up directly against the
// extracted embedded compose.yml, WITHOUT ever attempting a `docker build`
// — that only works when composeImageTag already exists locally (checked
// explicitly up front, so the failure mode for a genuinely fresh host with
// neither a checkout nor a pre-built image is one clear, specific error
// rather than a confusing docker-build failure against a source-less
// context).
func deployFromEmbeddedAssets(ctx context.Context, token, addr string) error {
	engine, composeCmd, err := detectComposeEngine(ctx)
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

	assetsDir, err := extractComposeAssets()
	if err != nil {
		return fmt.Errorf("extract embedded compose assets: %w", err)
	}
	composePath := filepath.Join(assetsDir, "compose.yml")

	fmt.Fprintln(os.Stderr, "boid: daemon container not reachable; no boid repo checkout found, starting the existing "+composeImageTag+" image via the embedded compose.yml...")

	args := append(append([]string{}, composeCmd[1:]...), "-f", composePath, "up", "-d")
	upCmd := exec.CommandContext(ctx, composeCmd[0], args...) //nolint:gosec // engine/composeCmd come from detectComposeEngine's own fixed literals, not attacker-controlled input
	upCmd.Env = append(filterEnv(os.Environ(), "BOID_CLI_TOKEN"),
		"BOID_CLI_TOKEN="+token,
		"BOID_RUNTIME_DIR="+hostModeRuntimeDir(),
	)
	if engine == "podman" {
		// Mirrors scripts/deploy-container.sh's own podman rootless socket
		// default — compose.yml's BOID_DOCKER_SOCK_SRC has no default that
		// works for podman (it's deliberately docker-shaped, see that
		// variable's own doc comment in compose.yml).
		upCmd.Env = append(upCmd.Env, fmt.Sprintf("BOID_DOCKER_SOCK_SRC=/run/user/%d/podman/podman.sock", os.Getuid()))
	}
	upCmd.Stdout = os.Stderr
	upCmd.Stderr = os.Stderr
	if err := upCmd.Run(); err != nil {
		return fmt.Errorf("%s up -d (embedded compose.yml) failed: %w", strings.Join(composeCmd, " "), err)
	}

	return waitForHealthy(ctx, addr, token)
}

// waitForHealthy polls hostModeHealthy until it reports healthy or
// hostModeStartTimeout passes — shared by both deployFromCheckout and
// deployFromEmbeddedAssets so the two deploy strategies report readiness
// identically.
func waitForHealthy(ctx context.Context, addr, token string) error {
	deadline := time.Now().Add(hostModeStartTimeout)
	for time.Now().Before(deadline) {
		if hostModeHealthy(ctx, addr, token) {
			fmt.Fprintln(os.Stderr, "boid: daemon container is up")
			return nil
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
	deployCmd := exec.CommandContext(ctx, filepath.Join(root, "scripts", "deploy-container.sh")) //nolint:gosec // fixed, repo-relative script path — not attacker-controlled input
	deployCmd.Dir = root
	// Filter out any inherited BOID_CLI_TOKEN before appending ours:
	// a naive append(os.Environ(), "BOID_CLI_TOKEN="+token) would leave
	// TWO entries when the invoking shell already exported one (e.g. a
	// stale/different value from a previous manual `docker compose`
	// run) — which one `deploy-container.sh`'s child process sees for
	// a duplicate key is libc-dependent, not guaranteed to be "last
	// wins". Filtering first makes token's value unambiguous.
	deployCmd.Env = append(filterEnv(os.Environ(), "BOID_CLI_TOKEN"), "BOID_CLI_TOKEN="+token)
	// Progress goes to stderr, keeping stdout clean for the eventual
	// subcommand's own output (e.g. `boid task list -o json`).
	deployCmd.Stdout = os.Stderr
	deployCmd.Stderr = os.Stderr
	if err := deployCmd.Run(); err != nil {
		return fmt.Errorf("scripts/deploy-container.sh failed: %w", err)
	}
	return waitForHealthy(ctx, addr, token)
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
		// compose up via the existing, already-tested deploy-container.sh,
		// unchanged from before round-2's Major 1 fix. FALLBACK
		// (round-2 codex review Major 1): no checkout found at all —
		// deployFromEmbeddedAssets can still bring up an ALREADY-BUILT
		// image via the embedded compose.yml. See this file's own header
		// comment for the full rationale of why these are two genuinely
		// different code paths rather than one unified implementation.
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
