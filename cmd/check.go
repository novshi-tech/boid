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
		reportCheck(out, "docker compose plugin (`docker compose version`)", dockerComposeUsable(ctx))
	}
	reportAvailability(out, "podman engine reachable (`podman version`)", podmanOK)
	if podmanOK {
		_, lookErr := exec.LookPath("podman-compose")
		reportCheck(out, "podman-compose on PATH", lookErr == nil)
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
	if engine == "" {
		fmt.Fprintln(out, "  (skipped: no usable engine)")
	} else {
		image := version.DefaultContainerImage()
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
	return probeArchMismatchWithAPI(ctx, cli, image)
}

// probeArchMismatchWithAPI reports the engine's host architecture and the
// target image's architecture, and whether they mismatch.
//
// [codex review round 1, Blocker 1]: the image's architecture is resolved
// via DistributionInspect FIRST — a registry manifest query that does NOT
// require the image to be pulled locally, which is the common case for a
// brand-new install (the whole point of this check is to catch the
// mismatch BEFORE `boid start` ever pulls/launches anything). Only when
// that fails (no registry reachable, or the ref is a purely local build
// tag like version.LocalBuildImage with no registry behind it at all) does
// this fall back to a local ImageInspect — which still catches the case
// where an operator already has a mismatched image sitting locally, e.g.
// from a copied volume.
//
// note carries a human-readable reason no verdict could be reached (engine
// unreachable, neither the registry nor a local copy resolved anything) —
// diagnostic only: resolveImage's own launch-time fail-fast
// (internal/dispatcher/container_backend.go) is the actual gate, so a
// probe failure here degrades to "skipped", never a false positive.
func probeArchMismatchWithAPI(ctx context.Context, api archProbeAPI, image string) (hostArch, imageArch string, mismatch bool, note string) {
	checkCtx, cancel := context.WithTimeout(ctx, checkEngineTimeout)
	defer cancel()

	info, err := api.Info(checkCtx, dockerclient.InfoOptions{})
	if err != nil {
		return "", "", false, fmt.Sprintf("could not query engine info to verify architecture: %v", err)
	}
	hostArch = dispatcher.NormalizeArch(info.Info.Architecture)

	if dist, distErr := api.DistributionInspect(checkCtx, image, dockerclient.DistributionInspectOptions{}); distErr == nil && len(dist.Platforms) > 0 {
		supported := make([]string, 0, len(dist.Platforms))
		matched := false
		for _, p := range dist.Platforms {
			a := dispatcher.NormalizeArch(p.Architecture)
			supported = append(supported, a)
			if hostArch != "" && a == hostArch {
				matched = true
			}
		}
		if hostArch != "" {
			if !matched {
				return hostArch, strings.Join(supported, ","), true, ""
			}
			return hostArch, hostArch, false, ""
		}
	}

	insp, err := api.ImageInspect(checkCtx, image)
	if err != nil {
		return hostArch, "", false, fmt.Sprintf("could not determine image %q's architecture (not present locally, and its registry manifest could not be queried): %v — it will still be validated at pull/launch time", image, err)
	}
	imageArch = insp.Architecture

	if hostArch != "" && imageArch != "" && hostArch != imageArch {
		mismatch = true
	}
	return hostArch, imageArch, mismatch, ""
}
