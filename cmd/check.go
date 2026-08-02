package cmd

// cmd/check.go implements `boid check`, a host-side preflight for the
// container backend (docs/plans/release-onboarding.md 穴5: "`boid check`
// が userns 時代の化石"). Before this PR it probed unprivileged user
// namespaces (`unshare --user --mount --map-root-user`) and required the
// `passt` binary — both are userns-backend artifacts from before Phase 6 /
// PR-4 (docs/plans/volume-only-daemon.md §論点e) removed that backend
// entirely (CLAUDE.md's「サンドボックス実行バックエンド」節). Neither check
// said anything about whether `boid start` (host mode, cmd/host.go) can
// actually stand up the compose daemon stack today.
//
// This rewrite instead reports on exactly what scripts/deploy-container.sh
// already checks by shell before it will attempt `compose up` — engine
// presence/reachability, the compose plugin, the docker-out-of-docker bind
// SOURCE socket (not just "some docker/podman is reachable" — the exact
// path compose.yml mounts into the daemon container, since that is what
// actually determines whether `boid start` and the job containers it later
// creates can talk to the engine), podman.socket's active state, and the
// uid that will actually run the daemon container — lifted to Go (mirroring,
// not copying, that script's own logic; see the reuse of
// usableEngine/dockerComposeUsable/detectComposeEngine below, all already
// defined in cmd/host.go for the exact same purpose, and
// effectiveBoidUID/refuseRootUID from cmd/start.go), plus a new check
// neither the old Go code nor the shell script had: host-arch vs.
// image-arch mismatch (docs/plans/release-onboarding.md 決定5 / §論点
// arm64 — required regardless of whether an arm64 image is ever published,
// since a host with binfmt/qemu registered would otherwise silently run a
// foreign-arch image under emulation instead of refusing it outright).
import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	dockerclient "github.com/moby/moby/client"
	"github.com/spf13/cobra"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/dispatcher"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/version"
)

var checkCmd = &cobra.Command{
	Use:          "check",
	Short:        "Check host prerequisites for the container backend and hook dependencies",
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
		// check's engine/compose/socket/arch probes inspect the docker/podman
		// engine and filesystem this CLI process itself can reach; against a
		// future https:// (remote daemon) profile those would report on the
		// wrong host entirely, since sandboxes execute wherever the daemon
		// is, not wherever the CLI happens to run. See cmd/scope_annotations_test.go's
		// expectedScopeAnnotations table for the full cross-check against
		// the plan doc.
		scopeAnnotationKey: scopeLocal,
	}
	rootCmd.AddCommand(checkCmd)
}

// checkEngineTimeout bounds every individual shell-out/RPC this file makes
// (`docker version`, `podman version`, `docker compose version`, `systemctl
// --user is-active podman.socket`, the engine Info/DistributionInspect/
// ImageInspect probes) — mirrors usableEngine/dockerComposeUsable's own 3s
// budget (cmd/host.go) so a hung/unreachable engine cannot make `boid
// check` itself hang.
const checkEngineTimeout = 3 * time.Second

func runCheck(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	allOK := true

	fmt.Fprintln(out, "=== Container engine ===")
	dockerOK := usableEngine(ctx, "docker")
	podmanOK := usableEngine(ctx, "podman")
	reportAvailability(out, "docker engine reachable (`docker version`)", dockerOK)
	if dockerOK {
		// [codex round 3 review, Minor]: softened to the same
		// available/unavailable wording as engine reachability itself
		// (reportAvailability, not reportCheck's OK/MISSING) — docker and
		// podman are ALTERNATIVES, so a healthy docker-only host with no
		// podman-compose installed would otherwise print a "MISSING" line
		// for a tool it will never need, immediately before "All checks
		// passed."
		reportAvailability(out, "docker compose plugin (`docker compose version`)", dockerComposeUsable(ctx))
	}
	reportAvailability(out, "podman engine reachable (`podman version`)", podmanOK)
	if podmanOK {
		_, lookErr := exec.LookPath("podman-compose")
		reportAvailability(out, "podman-compose on PATH", lookErr == nil)
	}
	engine, _, engineErr := detectComposeEngine(ctx)
	if engineErr != nil {
		fmt.Fprintf(out, "  ERROR: no usable engine+compose combination found (need docker+the compose plugin, or podman+podman-compose): %v\n", engineErr)
		allOK = false
	} else {
		fmt.Fprintf(out, "  OK: `boid start` will use engine=%s\n", engine)
	}

	fmt.Fprintln(out, "\n=== Engine socket (docker-out-of-docker bind source) ===")
	if engine == "" {
		fmt.Fprintln(out, "  (skipped: no usable engine)")
	} else if !checkEngineSocket(ctx, out, engine) {
		allOK = false
	}

	fmt.Fprintln(out, "\n=== uid ===")
	uid := effectiveBoidUID()
	fmt.Fprintf(out, "  effective BOID_UID for `boid start`: %d\n", uid)
	if err := validateRawBoidUIDEnv(); err != nil {
		// [codex round 2 review, Major]: effectiveBoidUID() deliberately
		// leaves a malformed BOID_UID's validation to whatever eventually
		// consumes it (its own doc comment) — for the CLI-side
		// refuseRootUID(effectiveBoidUID()) path that's fine, since
		// strconv.Atoi failing just falls back to os.Getuid(). But
		// scripts/deploy-container.sh is the ACTUAL enforcement point for
		// `boid start`, and it validates the RAW string against a
		// stricter "non-negative decimal integer" regex before ever
		// calling strconv — a value like "abc"/"-1"/"01" fails there but
		// sails through effectiveBoidUID()'s silent-fallback here. Check
		// the raw env var the same way deploy-container.sh does so this
		// preflight actually predicts what `boid start` will do.
		fmt.Fprintf(out, "  ERROR: %v\n", err)
		allOK = false
	} else if err := refuseRootUID(uid); err != nil {
		fmt.Fprintf(out, "  ERROR: %v\n", err)
		allOK = false
	} else {
		fmt.Fprintln(out, "  OK: non-root uid")
	}

	fmt.Fprintln(out, "\n=== Host / image architecture ===")
	switch {
	case engine == "":
		fmt.Fprintln(out, "  (skipped: no usable engine)")
	case os.Getenv("BOID_COMPOSE_ROOT") != "":
		// [codex round 4 review, Major]: BOID_COMPOSE_ROOT is exactly the
		// signal cmd/host.go's own findComposeRoot() uses to pick the
		// checkout/`--build` deploy path (deployFromCheckout,
		// cmd/host.go:619's --build) — that path's deploy-container.sh
		// invocation OVERRIDES BOID_IMAGE to "boid-runner:latest" and
		// BUILDS it locally from source rather than pulling anything. A
		// plain `docker build`/`podman build` with no --platform flag (and
		// no DOCKER_DEFAULT_PLATFORM override — checked below, [codex
		// round 5 review, Major]) always builds for the host's own
		// architecture, so this scenario normally cannot produce the
		// mismatch 決定5 exists to catch. What round 4 caught is the OTHER
		// half: running `boid check` in that same checkout BEFORE the
		// local image has ever been built would otherwise report "not
		// present locally, and no registry/remote manifest resolved
		// anything" as a failure — a false negative, since `boid start`
		// builds it fine right there. Skip the probe outright instead —
		// EXCEPT when DOCKER_DEFAULT_PLATFORM is explicitly set to
		// something other than this host's own architecture, which is the
		// one way `docker build`/`podman build` with no --platform flag
		// CAN still produce a foreign-arch image (deploy-container.sh's
		// own build invocation passes no --platform of its own — verified
		// by docs/plans/release-onboarding.md's own arm64 grep sweep
		// finding zero platform-related hits anywhere in the build
		// pipeline — so DOCKER_DEFAULT_PLATFORM is the only lever left).
		if msg, isErr := checkoutBuildPlatformWarning(ctx, engine); msg != "" {
			if isErr {
				fmt.Fprintf(out, "  ERROR: %s\n", msg)
				allOK = false
			} else {
				fmt.Fprintf(out, "  %s\n", msg)
			}
		} else {
			fmt.Fprintln(out, "  (skipped: BOID_COMPOSE_ROOT is set — `boid start` builds a host-native image locally via --build, which cannot mismatch this host's architecture by construction)")
		}
	default:
		image := resolveCheckImage()
		hostArch, imageArch, mismatch, note := probeArchMismatch(ctx, engine, image)
		switch {
		case note != "":
			// [codex round 2 review, Blocker 1]: an inconclusive probe is
			// NOT a pass. docs/plans/release-onboarding.md 決定5 requires
			// this fail-fast to be a "must", and resolveImage's own
			// launch-time check (internal/dispatcher/container_backend.go)
			// only guards JOB containers, not the compose daemon container
			// itself -- this is the only gate the daemon image ever gets,
			// so "could not verify" has to fail loudly rather than let
			// `boid check` exit 0 on a host/image combination that was
			// never actually confirmed compatible.
			fmt.Fprintf(out, "  ERROR: %s\n", note)
			allOK = false
		case mismatch:
			fmt.Fprintf(out, "  ERROR: image %q is built for arch %q, but this host is %q — running it would silently fall back to slow, crash-prone binfmt/qemu emulation instead of refusing outright; use an image built for %[3]s\n", image, imageArch, hostArch)
			allOK = false
		default:
			fmt.Fprintf(out, "  OK: host arch %q matches image %q's arch %q\n", hostArch, image, imageArch)
		}
	}

	// Check hook requires for registered projects.
	c := client.FromContext(cmd.Context())
	var projects []projectspec.Project
	if err := c.Do("GET", "/api/projects", nil, &projects); err != nil {
		fmt.Fprintln(out, "\n(server not running, skipping project hook checks)")
		if !allOK {
			return fmt.Errorf("some checks failed")
		}
		return nil
	}

	if len(projects) > 0 {
		fmt.Fprintln(out, "\n=== Hook dependencies ===")
		for _, p := range projects {
			for _, b := range p.Meta.TaskBehaviors {
				for _, h := range b.Hooks {
					for _, req := range h.Requires {
						if _, err := exec.LookPath(req); err != nil {
							fmt.Fprintf(out, "  MISSING: %s (project: %s, hook: %s)\n", req, p.ID, h.ID)
							allOK = false
						} else {
							fmt.Fprintf(out, "  OK: %s (project: %s, hook: %s)\n", req, p.ID, h.ID)
						}
					}
				}
			}
		}
	}

	if !allOK {
		return fmt.Errorf("some checks failed")
	}
	fmt.Fprintln(out, "\nAll checks passed.")
	return nil
}

// reportCheck prints a single OK/MISSING line for a check whose failure is
// meaningful on its own (i.e. worth flagging even when some OTHER path to
// success exists) — used for compose-plugin/podman-compose/podman.socket,
// each gated behind the specific engine that needs it.
func reportCheck(out io.Writer, label string, ok bool) {
	if ok {
		fmt.Fprintf(out, "  OK: %s\n", label)
	} else {
		fmt.Fprintf(out, "  MISSING: %s\n", label)
	}
}

// reportAvailability prints a single engine's own reachability without the
// OK/MISSING framing reportCheck uses — deliberately softer wording
// ([codex review round 1, Minor 1]): docker and podman are ALTERNATIVES
// (`boid start` needs exactly one usable engine+compose combination, not
// both), so a perfectly healthy docker-only host would otherwise print
// "MISSING: podman engine reachable" immediately before "All checks
// passed.", reading like an unresolved problem when it is not one.
func reportAvailability(out io.Writer, label string, ok bool) {
	if ok {
		fmt.Fprintf(out, "  available: %s\n", label)
	} else {
		fmt.Fprintf(out, "  unavailable: %s\n", label)
	}
}

// resolveCheckImage picks the image the arch probe validates —
// BOID_IMAGE, when set, else version.DefaultContainerImage().
//
// [codex round 3 review, Major]: build/container/compose.yml's own image
// line is `${BOID_IMAGE:-ghcr.io/novshi-tech/boid-runner:latest}` — BOID_IMAGE
// always wins when set, and every wrapped invocation (scripts/
// deploy-container.sh's pull-first default AND its `--build` dev path,
// e2e/run-container.sh) exports it before calling compose. Honoring the
// same override here means `BOID_IMAGE=<foreign-arch-ref> boid check`
// reports on the SAME image `boid start` would actually run.
//
// Deliberately does NOT replicate deploy-container.sh's checkout+`--build`
// dev path forcing `boid-runner:latest` (round 3's own example): that
// path always BUILDS the image locally from source rather than pulling
// one, and a plain `docker build`/`podman build` with no --platform flag
// always builds for the host's own architecture — the mismatch decision5
// exists to catch (a foreign-arch image silently run under binfmt/qemu
// emulation) cannot occur from a local build by construction, so there is
// nothing for this diagnostic to usefully catch on that path.
func resolveCheckImage() string {
	if v := os.Getenv("BOID_IMAGE"); v != "" {
		return v
	}
	return version.DefaultContainerImage()
}

// checkoutBuildPlatformWarning is the BOID_COMPOSE_ROOT branch's own
// [codex round 5 review, Major] guard: DOCKER_DEFAULT_PLATFORM is the one
// way a plain `docker build`/`podman build` with no --platform flag (which
// is exactly what scripts/deploy-container.sh's `--build` path invokes —
// see the call site's own doc comment) can still target a foreign
// architecture. Returns ("", false) when it is safe to skip the arch probe
// entirely (unset, or explicitly set to this host's own architecture);
// (msg, true) when it is NOT set to something matching this host (an
// actual foreign-arch build risk — the caller treats this as a failure);
// (msg, false) is never returned by the "definitely mismatched" branch
// below, but is reserved for a genuinely unparseable value.
func checkoutBuildPlatformWarning(ctx context.Context, engine string) (msg string, isErr bool) {
	platform := os.Getenv("DOCKER_DEFAULT_PLATFORM")
	if platform == "" {
		return "", false
	}
	_, arch, ok := strings.Cut(platform, "/")
	if !ok || arch == "" {
		return fmt.Sprintf("DOCKER_DEFAULT_PLATFORM=%q is set but not in the expected os/arch form — cannot verify the checkout `--build` path's target architecture", platform), true
	}
	platformArch := dispatcher.NormalizeArch(arch)

	cli, err := newEngineClient(engine)
	if err != nil {
		return fmt.Sprintf("DOCKER_DEFAULT_PLATFORM=%q is set, but the engine could not be reached to verify this host's own architecture: %v", platform, err), true
	}
	defer cli.Close()
	checkCtx, cancel := context.WithTimeout(ctx, checkEngineTimeout)
	defer cancel()
	info, err := cli.Info(checkCtx, dockerclient.InfoOptions{})
	if err != nil {
		return fmt.Sprintf("DOCKER_DEFAULT_PLATFORM=%q is set, but this host's own architecture could not be queried: %v", platform, err), true
	}
	hostArch := dispatcher.NormalizeArch(info.Info.Architecture)
	if hostArch == "" {
		return fmt.Sprintf("DOCKER_DEFAULT_PLATFORM=%q is set, but the engine reported no host architecture", platform), true
	}
	if platformArch != hostArch {
		return fmt.Sprintf("DOCKER_DEFAULT_PLATFORM=%q targets arch %q, but this host is %q — the checkout `--build` path would build a foreign-arch image and, under binfmt/qemu, run it via emulation instead of refusing outright", platform, platformArch, hostArch), true
	}
	return "", false
}

// resolveComposeBindSource mirrors scripts/deploy-container.sh's own
// BOID_DOCKER_SOCK_SRC resolution — the SINGLE source of truth this file
// uses for both the socket-existence check and the arch probe's connection
// target, deliberately NOT the CLI's own DOCKER_HOST/DOCKER_CONTEXT/
// CONTAINER_HOST resolution ([codex review round 1, Blocker 2]: those
// answer "what does the `docker`/`podman` CLI on THIS process's PATH talk
// to", which is a different, weaker question than "what socket will
// compose.yml actually bind-mount into the daemon container as
// /var/run/docker.sock" — build/container/compose.yml's own volumes entry,
// `${BOID_DOCKER_SOCK_SRC:-/var/run/docker.sock}`, is engine-agnostic and
// completely ignores DOCKER_HOST/DOCKER_CONTEXT; it is BOID_DOCKER_SOCK_SRC
// or nothing). deploy-container.sh only ever SETS BOID_DOCKER_SOCK_SRC
// itself on the podman branch (defaulting to the rootless XDG runtime
// path); on the docker branch it never touches the var at all, leaving
// compose.yml's own bare-docker default in effect — mirrored here as the
// engine=="docker" fallback.
func resolveComposeBindSource(engine string) string {
	if v := os.Getenv("BOID_DOCKER_SOCK_SRC"); v != "" {
		return v
	}
	if engine == "podman" {
		return fmt.Sprintf("/run/user/%d/podman/podman.sock", os.Getuid())
	}
	return "/var/run/docker.sock"
}

// checkEngineSocket prints the compose bind source, whether it exists as a
// real unix socket, and (podman only) podman.socket's active state.
// Returns false when any of these fail — the caller folds that into allOK.
func checkEngineSocket(ctx context.Context, out io.Writer, engine string) bool {
	ok := true
	bindSource := resolveComposeBindSource(engine)
	fmt.Fprintf(out, "  bind source: %s\n", bindSource)

	info, statErr := os.Stat(bindSource)
	switch {
	case statErr != nil:
		fmt.Fprintf(out, "  MISSING: %s (this is the exact path `boid start` bind-mounts into the daemon container as /var/run/docker.sock — %v)\n", bindSource, statErr)
		ok = false
	case info.Mode()&os.ModeSocket == 0:
		// [codex round 2 review, Blocker 2]: not a soft warning — a
		// bindSource that exists but is not actually a unix socket (a
		// stray plain file, a stale path left behind by something else)
		// bind-mounts into the daemon container exactly the same as a
		// real one; `docker version`'s earlier success says nothing about
		// THIS path, since the CLI may have resolved a completely
		// different socket via DOCKER_HOST/DOCKER_CONTEXT. `boid start`
		// would come up "successfully" and then fail every single
		// docker-API call the daemon makes from inside its container.
		fmt.Fprintf(out, "  MISSING: %s exists but is not a unix socket (mode %s)\n", bindSource, info.Mode())
		ok = false
	default:
		fmt.Fprintln(out, "  OK: bind source exists and is a unix socket")
	}

	if engine == "podman" {
		active := podmanSocketActive(ctx)
		reportCheck(out, "podman.socket active (`systemctl --user is-active podman.socket`)", active)
		if !active {
			fmt.Fprintln(out, "         fix: systemctl --user enable --now podman.socket")
			ok = false
		}
	}
	return ok
}

// rawBoidUIDPattern mirrors scripts/deploy-container.sh's own BOID_UID
// validation regex verbatim (`^(0|[1-9][0-9]*)$`) — a plain non-negative
// decimal integer, no leading zeros (Bash's `((...))` treats a leading
// zero as octal, and a larger value like "010" could silently mean the
// wrong uid) and no sign.
var rawBoidUIDPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// validateRawBoidUIDEnv checks BOID_UID's RAW string value the same way
// scripts/deploy-container.sh does, before compose ever substitutes it into
// `user: "${BOID_UID}:0"` — see this function's own call site for why this
// is a separate check from refuseRootUID(effectiveBoidUID()).
func validateRawBoidUIDEnv() error {
	v := os.Getenv("BOID_UID")
	if v == "" {
		return nil
	}
	if !rawBoidUIDPattern.MatchString(v) {
		return fmt.Errorf("BOID_UID must be a plain non-negative decimal integer (got %q)", v)
	}
	return nil
}

// podmanSocketActive mirrors scripts/deploy-container.sh's own preflight
// (`systemctl --user is-active podman.socket`): a missing/inactive
// podman.socket produces a confusing failure much later (compose up's bind
// mount can silently succeed against a not-yet-existing path, then every
// docker-API call the daemon makes once running fails with a bare
// connection-refused/no-such-file) — checked explicitly here instead, with
// the actual remediation printed by the caller.
func podmanSocketActive(ctx context.Context) bool {
	checkCtx, cancel := context.WithTimeout(ctx, checkEngineTimeout)
	defer cancel()
	return exec.CommandContext(checkCtx, "systemctl", "--user", "is-active", "podman.socket").Run() == nil
}

// archProbeAPI is the minimal docker-engine surface probeArchMismatchWithAPI
// needs — narrow enough to fake in tests without a real engine, and
// structurally satisfied by *dockerclient.Client (Info/DistributionInspect/
// ImageInspect have the exact same signatures the moby client already
// exposes).
type archProbeAPI interface {
	Info(ctx context.Context, options dockerclient.InfoOptions) (dockerclient.SystemInfoResult, error)
	DistributionInspect(ctx context.Context, image string, options dockerclient.DistributionInspectOptions) (dockerclient.DistributionInspectResult, error)
	ImageInspect(ctx context.Context, image string, opts ...dockerclient.ImageInspectOption) (dockerclient.ImageInspectResult, error)
}

// newEngineClient builds a moby client pointed DIRECTLY at
// resolveComposeBindSource(engine) — the same socket compose.yml itself
// bind-mounts (see that function's own doc comment for why this,
// deliberately, is not DOCKER_HOST/DOCKER_CONTEXT/CONTAINER_HOST
// resolution). Sharing that single source of truth between the socket
// existence check and the arch probe means both agree by construction.
func newEngineClient(engine string) (*dockerclient.Client, error) {
	return dockerclient.New(dockerclient.WithHost("unix://" + resolveComposeBindSource(engine)))
}

// probeArchMismatch connects to engine and delegates to
// probeArchMismatchWithAPI — split out so the actual comparison logic is
// testable against a fake archProbeAPI without a real docker/podman engine.
func probeArchMismatch(ctx context.Context, engine, image string) (hostArch, imageArch string, mismatch bool, note string) {
	cli, err := newEngineClient(engine)
	if err != nil {
		return "", "", false, fmt.Sprintf("could not connect to the %s engine to verify architecture: %v", engine, err)
	}
	defer cli.Close()
	return probeArchMismatchWithAPI(ctx, cli, engine, image)
}

// probeArchMismatchWithAPI reports the engine's host architecture and the
// target image's architecture, and whether they mismatch.
//
// [codex round 3 review, Blocker 2]: a LOCAL image is checked FIRST, ahead
// of any registry/remote-manifest query. Compose's default pull_policy
// ("missing") never re-pulls a tag that already resolves to a locally
// cached image, however stale — so if a wrong-arch image already sits
// under this exact tag (e.g. copied in from another host's volume, or a
// leftover from a previous arch), THAT is what `boid start` will actually
// run, regardless of what the registry currently publishes. Only when
// nothing is cached locally does querying the manifest WITHOUT pulling
// (the common brand-new-install case) become the right question, since a
// pull is exactly what compose is about to do.
//
// [codex round 4 review, Blocker 1]: the remote-manifest step is
// engine-specific, not just a single DistributionInspect call. Docker's
// `/distribution/{name}/json` (api.DistributionInspect) has no Podman
// equivalent — Podman's API server does not implement that endpoint at
// all, so on Podman this would ALWAYS fail for an image that has never
// been pulled, making `boid check` refuse a fresh Podman install outright
// even though `boid start` itself can pull the very same public image
// fine. Podman's own `podman manifest inspect <ref>` CLI subcommand DOES
// support inspecting a remote reference's manifest without pulling it, so
// that is used instead — see podmanManifestArches.
//
// note carries a human-readable reason no verdict could be reached (engine
// unreachable, neither a remote nor a local copy resolved anything) —
// [codex round 2/3 review]: this is treated as a FAILURE by the caller
// (runCheck), not a silent pass — 決定5 requires the fail-fast to be a
// "must", and resolveImage's own launch-time check
// (internal/dispatcher/container_backend.go) only ever guards JOB
// containers, never the compose daemon image itself, so an inconclusive
// verdict here is the only gate the daemon image gets at all.
func probeArchMismatchWithAPI(ctx context.Context, api archProbeAPI, engine, image string) (hostArch, imageArch string, mismatch bool, note string) {
	checkCtx, cancel := context.WithTimeout(ctx, checkEngineTimeout)
	defer cancel()

	info, err := api.Info(checkCtx, dockerclient.InfoOptions{})
	if err != nil {
		return "", "", false, fmt.Sprintf("could not query engine info to verify architecture: %v", err)
	}
	hostArch = dispatcher.NormalizeArch(info.Info.Architecture)
	if hostArch == "" {
		return "", "", false, "engine reported no host architecture"
	}

	// [codex round 5 review, Major]: `:latest` (or no tag at all, which
	// docker treats identically) is compose's own documented exception to
	// pull_policy "missing" — compose ALWAYS re-pulls a `:latest`-tagged
	// image even when a local copy already exists, per the compose-file
	// spec's own pull_policy documentation. Checking the local cache
	// first for that tag would validate a potentially stale image compose
	// is about to discard and replace anyway — remote-first is correct
	// there, with the local cache only as a last-resort fallback (still
	// better than nothing when the registry cannot be reached at all).
	tryLocalFirst := !isLatestTag(image)

	if tryLocalFirst {
		if a, ok := probeLocalImageArch(checkCtx, api, image); ok {
			imageArch = a
			if imageArch == "" {
				return hostArch, "", false, fmt.Sprintf("image %q is present locally but its manifest reports no architecture", image)
			}
			return hostArch, imageArch, hostArch != imageArch, ""
		}
	}

	if supported := remoteImageArches(checkCtx, api, engine, image); len(supported) > 0 {
		matched := false
		for _, a := range supported {
			if a == hostArch {
				matched = true
				break
			}
		}
		if !matched {
			return hostArch, strings.Join(supported, ","), true, ""
		}
		return hostArch, hostArch, false, ""
	}

	if !tryLocalFirst {
		// Remote resolution failed/was inconclusive for a `:latest` image
		// — fall back to whatever is cached locally rather than giving up
		// outright. Weaker evidence than the primary (remote) path since
		// compose will still re-pull regardless of what's cached, but
		// better than no verdict at all when the registry cannot be
		// reached.
		if a, ok := probeLocalImageArch(checkCtx, api, image); ok {
			imageArch = a
			if imageArch == "" {
				return hostArch, "", false, fmt.Sprintf("image %q is present locally but its manifest reports no architecture", image)
			}
			return hostArch, imageArch, hostArch != imageArch, ""
		}
	}

	return hostArch, "", false, fmt.Sprintf("could not determine image %q's architecture (not present locally, and its registry/remote manifest could not be queried) — it will still be validated at pull/launch time", image)
}

// isLatestTag reports whether ref's tag component is (or defaults to,
// when absent) "latest" — the exact string compose-file's pull_policy spec
// treats specially. Splits on the LAST "/" first so a registry host with a
// port (e.g. "localhost:5000/repo") is never mistaken for a tag separator;
// only a colon appearing AFTER that slash is a tag delimiter.
func isLatestTag(ref string) bool {
	rest := ref
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		rest = ref[i+1:]
	}
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		return rest[i+1:] == "latest"
	}
	return true // no tag at all defaults to "latest"
}

// probeLocalImageArch inspects image's LOCAL copy via api.ImageInspect,
// returning (architecture, true) if the image is present locally at all
// (architecture may still be "" if the local manifest itself lacks
// platform metadata — that is a distinct, reportable condition from "not
// present"), or ("", false) if it is not present locally.
func probeLocalImageArch(ctx context.Context, api archProbeAPI, image string) (string, bool) {
	insp, err := api.ImageInspect(ctx, image)
	if err != nil {
		return "", false
	}
	return insp.Architecture, true
}

// remoteImageArches resolves image's architecture(s) from its
// registry/remote manifest WITHOUT pulling it — engine-specific (see
// podmanManifestArches's own doc comment for why podman needs a different
// technique than docker's DistributionInspect).
func remoteImageArches(ctx context.Context, api archProbeAPI, engine, image string) []string {
	if engine == "podman" {
		archs, err := podmanManifestArches(ctx, image)
		if err != nil {
			return nil
		}
		return archs
	}
	dist, err := api.DistributionInspect(ctx, image, dockerclient.DistributionInspectOptions{})
	if err != nil {
		return nil
	}
	supported := make([]string, 0, len(dist.Platforms))
	for _, p := range dist.Platforms {
		supported = append(supported, dispatcher.NormalizeArch(p.Architecture))
	}
	return supported
}

// podmanManifestArches resolves a REMOTE image reference's architecture(s)
// without pulling it — the podman-side counterpart of the docker branch's
// api.DistributionInspect (docs comment on probeArchMismatchWithAPI's
// [codex round 4 review, Blocker 1]: podman's API server does not
// implement docker's distribution-inspect REST endpoint at all).
//
// [codex round 5 review, Blocker]: a first attempt using ONLY `podman
// manifest inspect <ref>` was not sufficient for the common case boid
// actually publishes (決定5: a single-platform, non-manifest-list amd64
// image, per .github/workflows/blackbox-e2e.yml's own plain `docker
// build`+push). For a single-platform OCI/Docker image manifest there is
// no top-level
// "architecture" field at all — that lives in the referenced CONFIG blob,
// a separate fetch `podman manifest inspect` does not make; only a
// manifest LIST/index (multiple platforms) has per-entry
// "manifests[].platform.architecture", which the original implementation
// correctly parsed but which never applies to boid's own published image.
// `skopeo inspect docker://<ref>` resolves the config blob itself and
// reports a direct top-level "Architecture" field — tried first since
// skopeo ships alongside podman on essentially every distro packaging of
// it (same containers/image library). `podman manifest inspect` remains a
// second attempt for the (currently hypothetical, but decision-5-intended
// future) manifest-list case. If NEITHER resolves anything, the caller
// (probeArchMismatchWithAPI) reports this as inconclusive — a genuine "no
// way to verify without pulling" case now that both known techniques have
// been tried, not a shortcut.
var podmanManifestArches = func(ctx context.Context, image string) ([]string, error) {
	if archs, err := skopeoInspectArches(ctx, image); err == nil {
		return archs, nil
	}
	return podmanManifestListArches(ctx, image)
}

// skopeoInspectArches shells out to `skopeo inspect docker://<image>`,
// which resolves the image's config blob and reports its architecture
// directly — the one remote-manifest technique that works for a
// single-platform image (see podmanManifestArches's own doc comment).
func skopeoInspectArches(ctx context.Context, image string) ([]string, error) {
	if _, err := exec.LookPath("skopeo"); err != nil {
		return nil, err
	}
	out, err := exec.CommandContext(ctx, "skopeo", "inspect", "docker://"+image).Output()
	if err != nil {
		return nil, err
	}
	var result struct {
		Architecture string `json:"Architecture"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, err
	}
	if result.Architecture == "" {
		return nil, fmt.Errorf("skopeo inspect reported no architecture")
	}
	return []string{dispatcher.NormalizeArch(result.Architecture)}, nil
}

// podmanManifestListArches shells out to `podman manifest inspect <ref>` —
// only useful for a manifest LIST/OCI index (multiple platforms); a
// single-platform manifest has no top-level architecture field at all (see
// podmanManifestArches's own doc comment).
func podmanManifestListArches(ctx context.Context, image string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "podman", "manifest", "inspect", image).Output()
	if err != nil {
		return nil, err
	}
	var manifest struct {
		Manifests []struct {
			Platform struct {
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(out, &manifest); err != nil {
		return nil, err
	}
	var archs []string
	for _, m := range manifest.Manifests {
		if a := m.Platform.Architecture; a != "" {
			archs = append(archs, dispatcher.NormalizeArch(a))
		}
	}
	if len(archs) == 0 {
		return nil, fmt.Errorf("manifest reported no architecture (not a manifest list, or a single-platform manifest with no per-entry platform info)")
	}
	return archs, nil
}
