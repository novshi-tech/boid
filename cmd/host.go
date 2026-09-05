//go:build linux

package cmd

// cmd/host.go implements "host mode": the `boid` CLI itself becomes the
// outer wrapper responsible for the container-backend daemon's lifecycle,
// mirroring the shape of the pre-existing bare-metal on-demand daemon start
// UX (cmd/root.go's autostart, client.EnsureRunningAt) but for a
// `docker/podman compose` stack instead of a single host process.
//
// Unconditional: hostModeEnabled() always reports true. profiles.Resolve-
// based resolution (a genuine remote https:// profile, or the pre-compose
// unix socket default) still exists and now serves exactly one purpose: an
// EXPLICIT `--profile` flag bypasses host mode outright (cmd/root.go's
// PersistentPreRunE) — see profileExplicitlyRequested's own doc comment.
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
// source of truth for what "deploy the container backend" means (config
// seed + effective-backend validation, BOID_UID/BOID_GID/DOCKER_GID setup,
// runtime-dir provisioning, podman socket preflight). They differ only in
// which ROOT directory the script runs from, and whether that root is
// allowed to BUILD a fresh image from it.
//
// PRIMARY path (findComposeRoot found a checkout via the BOID_COMPOSE_ROOT
// env override — see findComposeRoot's own doc comment for why that is the
// only trusted source): host mode invokes deploy-container.sh directly out
// of that checkout (deployFromCheckout) with its `--build` dev-backdoor
// flag, so a dev checkout keeps picking up local code changes.
// build/container/Dockerfile's build context is `COPY . .` — the ENTIRE go
// source tree — so this path is the only one that can ever BUILD a fresh
// image; there is no way around needing a real checkout for that.
//
// FALLBACK path (no checkout discoverable, e.g. /usr/local/bin/boid invoked
// from an ordinary project directory): deployFromEmbeddedAssets extracts
// scripts/deploy-container.sh (go:embed'd into its own package,
// boidscripts.Assets — a separate package from compose.yml/Dockerfile below
// since go:embed cannot cross a directory boundary with "..") ALONGSIDE
// build/container/{compose.yml,Dockerfile} (go:embed'd into build/
// container's own package, boidcontainer.Assets) into a directory tree that
// mirrors this repo's own layout (so the script's own
// `ROOT_DIR="$(dirname "${BASH_SOURCE[0]}")/.."`-relative path computation
// resolves exactly as it would from a real checkout), then runs that
// extracted script WITHOUT `--build` — there is no source tree here to
// build from regardless (Dockerfile's `COPY . .` context is not present in
// an extracted asset directory). deployFromEmbeddedAssets exports a
// BOID_IMAGE first (resolveEmbeddedDeployImage — the caller's own setting
// if there is one, else this exact CLI binary's version identity per
// internal/version) so the pull targets a known ref rather than leaving the
// script to guess from a git checkout that does not exist here.

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
	"github.com/novshi-tech/boid/internal/version"
	boidscripts "github.com/novshi-tech/boid/scripts"
)

// cliTokenFileName names the persistent shared-secret file under
// hostModeConfigDir() (~/.config/boid — profiles.ConfigPath()'s
// os.UserConfigDir()-based "boid" subdirectory). cliLockFileName names the
// flock file under hostModeAssetsDir() instead (XDG_STATE_HOME — see
// withHostModeLock's own doc comment for why it lives there, not the
// config dir).
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

// hostModeEnabled reports whether host mode (this file) is this
// invocation's resolution path. Unconditionally true — the compose daemon
// is the only daemon shape boid supports now. Kept as a named function
// (rather than inlining `true` at its one call site in cmd/root.go) so the
// seam stays easy to find/grep and easy to read at the call site.
func hostModeEnabled() bool {
	return true
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
// result is a plausible token. A manually restored token file commonly
// carries a trailing newline (e.g. `echo $TOKEN > cli-token` instead of
// `printf`), which — left untrimmed — would
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
// polling".
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
// Deliberately checks an authenticated route rather than the public
// /api/health: hitting the public route would only prove some process is
// listening, not that the token host mode is about to use for every real
// request actually works — e.g. if ~/.config/boid/cli-token gets
// deleted/regenerated while the daemon container keeps running with the OLD
// BOID_CLI_TOKEN baked into its environment, /api/health would keep
// reporting 200 forever and every subsequent CLI command would 401
// indefinitely.
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
// script rather than embedding its assets. Trusts ONLY an explicit
// BOID_COMPOSE_ROOT override; returns an error otherwise, so
// ensureHostModeDaemon's caller falls back to deployFromEmbeddedAssets
// (the go:embed'd copy this binary ships with, not anything read off the
// filesystem).
//
// Deliberately does NOT also walk up from the current working directory
// looking for scripts/deploy-container.sh: since host mode is the
// unconditional default for every scope=remote command, that walk would be
// a drive-by code-execution vector — an operator who `cd`s into ANY
// checkout that happens to contain its own scripts/deploy-container.sh (an
// attacker-controlled repo, not necessarily boid's own) and runs an
// ordinary `boid` command would have that checkout's script executed with
// their own user privileges, no confirmation asked. The only way to make
// host mode trust a checkout is to set BOID_COMPOSE_ROOT explicitly.
func findComposeRoot() (string, error) {
	if v := os.Getenv("BOID_COMPOSE_ROOT"); v != "" {
		return v, nil
	}
	return "", fmt.Errorf(
		"host mode: BOID_COMPOSE_ROOT is not set; falling back to the embedded deploy assets " +
			"(set BOID_COMPOSE_ROOT to a boid repo checkout root to build/deploy from source instead)")
}

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
// checkout, letting both host-mode paths share the same script instead of
// reimplementing a second, narrower copy of its config-seed/UID-GID/
// podman-preflight logic directly in Go. Returns <root>.
//
// Unconditionally overwrites on every call via atomicfile.WriteAtomic
// (write-temp + rename) — cheap, keeps the extracted copy from ever
// drifting stale against whichever `boid` binary version is running, and
// avoids a concurrent reader (this repo's own deploy-container.sh, invoked
// by a second `boid` process) observing a truncated/partially-written file
// mid-extraction. Independent, defense-in-depth hardening alongside
// withHostModeLock's own fix for the same race (different XDG_CONFIG_HOME
// values resolving to different lock files while sharing the same
// XDG_STATE_HOME).
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
	// compose.podman.override.yml: the rootless-podman overlay
	// deploy-container.sh stacks on compose.yml. Extracted unconditionally,
	// exactly like the other two, because the extraction cannot know which
	// engine the script will go on to detect.
	podmanOverride, err := boidcontainer.Assets.ReadFile("compose.podman.override.yml")
	if err != nil {
		return "", fmt.Errorf("read embedded compose.podman.override.yml: %w", err)
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
	if err := atomicfile.WriteAtomic(filepath.Join(buildDir, "compose.podman.override.yml"), 0o644, podmanOverride); err != nil {
		return "", fmt.Errorf("extract compose.podman.override.yml: %w", err)
	}
	if err := atomicfile.WriteAtomic(filepath.Join(buildDir, "Dockerfile"), 0o644, dockerfile); err != nil {
		return "", fmt.Errorf("extract Dockerfile: %w", err)
	}
	return root, nil
}

// usableEngine reports whether name (docker or podman) is not just present
// on PATH but actually answers `<name> version` — mirrors
// scripts/deploy-container.sh's own usable() helper: a CLI binary can be
// installed with no reachable daemon/socket behind it, and presence-only
// detection would wrongly prefer that over a genuinely working second
// engine.
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
// it. A docker install with a reachable daemon but no compose plugin would
// otherwise be preferred over a genuinely usable podman+podman-compose and
// only fail much later, at the actual `up` call.
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

// runDeployScript invokes <root>/scripts/deploy-container.sh — the single
// implementation both deployFromCheckout and deployFromEmbeddedAssets
// share. build, when true, passes the script's `--build` dev-backdoor flag
// (the script's own default is to pull, so a caller that still wants a
// fresh local build — deployFromCheckout — must opt in explicitly). The
// embedded path's root has none of the go source Dockerfile's `COPY . .`
// build context needs, so it always leaves this false and lets the
// script's pull-first default handle it. extraEnv carries additional
// "KEY=VALUE" pairs into the child's environment on top of BOID_CLI_TOKEN —
// deployFromEmbeddedAssets uses this for BOID_IMAGE rather than mutating
// this process's own environment via os.Setenv, which would otherwise leak
// across every subsequent call in the same process (tests included).
func runDeployScript(ctx context.Context, root, token, addr string, build bool, extraEnv ...string) error {
	scriptPath := filepath.Join(root, "scripts", "deploy-container.sh")
	args := []string{}
	if build {
		args = append(args, "--build")
	}
	deployCmd := exec.CommandContext(ctx, scriptPath, args...) //nolint:gosec // scriptPath is built from a fixed, repo-relative suffix under either a located checkout root or this process's own extraction dir — not attacker-controlled input
	deployCmd.Dir = root
	// Filter out any inherited BOID_CLI_TOKEN before appending ours: a
	// naive append(os.Environ(), "BOID_CLI_TOKEN="+token) would leave TWO
	// entries when the invoking shell already exported one (e.g. a
	// stale/different value from a previous manual `docker compose` run)
	// — which one the child process sees for a duplicate key is
	// libc-dependent, not guaranteed to be "last wins". Filtering first
	// makes token's value unambiguous. Every extraEnv key gets the same
	// treatment (e.g. deployFromEmbeddedAssets's BOID_IMAGE) — an inherited
	// shell value for the same key would otherwise be similarly ambiguous.
	env := os.Environ()
	env = filterEnv(env, "BOID_CLI_TOKEN")
	for _, kv := range extraEnv {
		if key, _, ok := strings.Cut(kv, "="); ok {
			env = filterEnv(env, key)
		}
	}
	env = append(env, "BOID_CLI_TOKEN="+token)
	env = append(env, extraEnv...)
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

// deployFromEmbeddedAssets is the fallback invoked by ensureHostModeDaemon
// only when findComposeRoot could not locate a real boid repo checkout
// (see this file's own header comment for the two-path design). Extracts
// the embedded assets to an emulated checkout root (extractComposeAssets)
// and runs that root's own scripts/deploy-container.sh WITHOUT `--build` —
// the script's own pull-first default — since this path can never build a
// fresh image regardless (Dockerfile's `COPY . .` context is not present
// in an extracted asset directory). BOID_IMAGE is set to
// version.DefaultContainerImage() before invoking the script so the pull
// targets this exact running CLI binary's own version identity
// (internal/version) rather than the script's own git-based guess (which
// has nothing to work from here — no .git in an extracted asset
// directory).
func deployFromEmbeddedAssets(ctx context.Context, token, addr string) error {
	if _, _, err := detectComposeEngine(ctx); err != nil {
		return fmt.Errorf(
			"no boid repo checkout found (set BOID_COMPOSE_ROOT to one — findComposeRoot no longer auto-discovers a checkout by walking up from cwd, codex round-10 review of PR5) to build a fresh image, and %w", err)
	}

	root, err := extractComposeAssets()
	if err != nil {
		return fmt.Errorf("extract embedded compose assets: %w", err)
	}

	image, notice := resolveEmbeddedDeployImage(os.Getenv("BOID_IMAGE"), version.Version())
	if notice != "" {
		fmt.Fprintln(os.Stderr, notice)
	}
	fmt.Fprintln(os.Stderr, "boid: daemon container not reachable; no boid repo checkout found, pulling "+image+" via the embedded scripts/deploy-container.sh...")

	return runDeployScript(ctx, root, token, addr, false, "BOID_IMAGE="+image)
}

// resolveEmbeddedDeployImage picks the image ref the checkout-less deploy
// path hands to scripts/deploy-container.sh as BOID_IMAGE, and returns
// alongside it a notice to print first when that ref needs explaining.
//
// envImage is the caller's own BOID_IMAGE (empty when unset) and selfVersion
// is this binary's version.Version(). An explicit envImage always wins
// outright, since a non-release build is precisely the shape that needs an
// operator-supplied ref. Otherwise: a non-release build (pseudo-version,
// "(devel)", a pre-release tag — version.IsExactRelease's rule) has no
// published GHCR ref, so this falls back to the bare local tag
// version.LocalBuildImage and returns a notice explaining that — since that
// image does not exist on a fresh machine and cannot be pulled from
// anywhere, printing just "pulling boid-runner:latest" would read as an
// ordinary registry pull and send the operator looking for network/registry
// faults instead.
func resolveEmbeddedDeployImage(envImage, selfVersion string) (image, notice string) {
	if envImage != "" {
		return envImage, ""
	}
	if version.IsExactRelease(selfVersion) {
		return version.BoidRunnerImageRepo + ":" + selfVersion, ""
	}
	shownVersion := selfVersion
	if shownVersion == "" {
		shownVersion = "unknown"
	}
	return version.LocalBuildImage, fmt.Sprintf(
		"boid: this binary is not a release build (version: %s), so there is no published image matching it.\n"+
			"boid: falling back to the local image %q, which only exists on a machine that built it — if it is absent this deploy will fail.\n"+
			"boid:   to pull a published image instead: go install github.com/novshi-tech/boid@<release-tag>\n"+
			"boid:   to use an image you already have:  BOID_IMAGE=<ref> boid start",
		shownVersion, version.LocalBuildImage)
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
					"(rebuild it from within a boid repo checkout via `scripts/deploy-container.sh --build`, or pull a newer one — see BOID_IMAGE in build/container/compose.yml) "+
					"rather than waiting out the full startup timeout for a condition that cannot resolve on its own", addr)
		}
		time.Sleep(hostModeHealthPollInterval)
	}
	return fmt.Errorf("daemon container did not become healthy within %s (addr=%s)", hostModeStartTimeout, addr)
}

// deployFromCheckout is ensureHostModeDaemon's PRIMARY path: invokes
// scripts/deploy-container.sh from a real, located boid repo checkout root
// — see this file's own header comment for why this is the only path that
// can ever build a fresh image. Passes `--build`: a dev checkout should
// keep picking up local code changes on every `boid start` — a checkout is
// the one place that opt-in actually makes sense (it is also the only
// place it is even possible, per this file's own header comment).
func deployFromCheckout(ctx context.Context, root, token, addr string) error {
	fmt.Fprintln(os.Stderr, "boid: daemon container not reachable; starting it now (this can take a while on first run)...")
	return runDeployScript(ctx, root, token, addr, true)
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
// (XDG_CONFIG_HOME). Two `boid` invocations that happen to see different
// XDG_CONFIG_HOME values (e.g. two shells with slightly different
// environments) but the SAME XDG_STATE_HOME would otherwise take two
// DIFFERENT locks while racing to write the SAME extracted files — no
// mutual exclusion at all despite both intending to serialize against each
// other. Deriving the lock path from the same directory the race is
// actually over closes that gap directly; extractComposeAssets's own
// atomicfile.WriteAtomic use is independent, defense-in-depth hardening for
// the same race rather than a substitute for this — the two mechanisms
// guard different things (mutual exclusion vs. no reader ever observing a
// torn write).
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
// message instead, the same as the bare-metal client.EnsureRunningAt path.
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
		// deploy via the existing deploy-container.sh. FALLBACK: no
		// checkout found at all — deployFromEmbeddedAssets extracts and
		// runs that SAME script against an ALREADY-BUILT image instead of
		// reimplementing a second, narrower copy of it. See this file's own
		// header comment for the full rationale.
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

// resolveHostModeClientNoAutostart is resolveHostModeClient's sibling for a
// command carrying annotationSkipAutostart=skip: builds the exact same
// client when the compose daemon is ALREADY reachable, but — unlike
// resolveHostModeClient, which unconditionally calls ensureHostModeDaemon
// and deploys the stack when it is not — refuses outright instead of
// autostarting anything when it is unreachable (e.g. `boid gc`, which
// should not spin up a daemon just to immediately garbage-collect it).
func resolveHostModeClientNoAutostart(ctx context.Context) (*client.Client, error) {
	token, err := loadOrCreateCLIToken()
	if err != nil {
		return nil, fmt.Errorf("host mode: %w", err)
	}
	addr := client.DefaultCLIAddr()
	if !hostModeHealthy(ctx, addr, token) {
		return nil, fmt.Errorf(
			"host mode: daemon container not reachable at %s; not starting it automatically for this command — run `boid start` first",
			addr)
	}
	c, err := client.NewClient("http://"+addr, token)
	if err != nil {
		return nil, fmt.Errorf("host mode: build client: %w", err)
	}
	return c, nil
}
