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
// presence/reachability, the compose plugin, podman.socket's active state,
// engine socket path, and the invoking uid — lifted to Go (mirroring, not
// copying, that script's own logic; see the reuse of
// usableEngine/dockerComposeUsable/detectComposeEngine below, all already
// defined in cmd/host.go for the exact same purpose), plus a new check
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
// --user is-active podman.socket`, the engine Info/ImageInspect probe) —
// mirrors usableEngine/dockerComposeUsable's own 3s budget (cmd/host.go) so
// a hung/unreachable engine cannot make `boid check` itself hang.
const checkEngineTimeout = 3 * time.Second

func runCheck(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	allOK := true

	fmt.Fprintln(out, "=== Container engine ===")
	fmt.Fprintf(out, "  uid: %d\n", os.Getuid())

	dockerOK := usableEngine(ctx, "docker")
	reportCheck(out, "docker engine reachable (`docker version`)", dockerOK)
	if dockerOK {
		reportCheck(out, "docker compose plugin (`docker compose version`)", dockerComposeUsable(ctx))
	}

	podmanOK := usableEngine(ctx, "podman")
	reportCheck(out, "podman engine reachable (`podman version`)", podmanOK)
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

	fmt.Fprintln(out, "\n=== Engine socket ===")
	switch engine {
	case "docker":
		fmt.Fprintf(out, "  socket: %s\n", resolveDockerSocketPath())
		fmt.Fprintln(out, "  OK: reachability already confirmed above (`docker version`)")
	case "podman":
		sock := resolvePodmanSocketPath()
		fmt.Fprintf(out, "  socket: %s\n", sock)
		active := podmanSocketActive(ctx)
		reportCheck(out, "podman.socket active (`systemctl --user is-active podman.socket`)", active)
		if !active {
			fmt.Fprintln(out, "         fix: systemctl --user enable --now podman.socket")
			fmt.Fprintf(out, "         (required for the docker-out-of-docker engine-socket bind at %s)\n", sock)
			allOK = false
		}
	default:
		fmt.Fprintln(out, "  (skipped: no usable engine)")
	}

	fmt.Fprintln(out, "\n=== Host / image architecture ===")
	if engine == "" {
		fmt.Fprintln(out, "  (skipped: no usable engine)")
	} else {
		image := version.DefaultContainerImage()
		hostArch, imageArch, mismatch, note := probeArchMismatch(ctx, engine, image)
		switch {
		case note != "":
			fmt.Fprintf(out, "  %s\n", note)
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

// reportCheck prints a single OK/MISSING line — kept as a shared helper now
// that this file prints many more such lines than the old
// hostRequiredTools/hook-dependency loops did on their own.
func reportCheck(out io.Writer, label string, ok bool) {
	if ok {
		fmt.Fprintf(out, "  OK: %s\n", label)
	} else {
		fmt.Fprintf(out, "  MISSING: %s\n", label)
	}
}

// resolveDockerSocketPath reports the docker engine socket `boid start`
// would connect to — DOCKER_HOST if set (client.FromEnv's own resolution),
// else the moby client's documented default. Purely informational: actual
// reachability is already established by usableEngine(ctx, "docker")
// above, which round-trips `docker version` against this exact socket.
func resolveDockerSocketPath() string {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return h
	}
	return dockerclient.DefaultDockerHost
}

// resolvePodmanSocketPath mirrors scripts/deploy-container.sh's own
// BOID_DOCKER_SOCK_SRC resolution (`: "${BOID_DOCKER_SOCK_SRC:=/run/user/$(id
// -u)/podman/podman.sock}"`) — rootless podman's socket lives under the
// invoking user's XDG runtime dir, not at a fixed well-known path the way
// docker's always does, so this has to compute it the same way the deploy
// script does rather than reuse a docker-shaped default.
func resolvePodmanSocketPath() string {
	if v := os.Getenv("BOID_DOCKER_SOCK_SRC"); v != "" {
		return v
	}
	return fmt.Sprintf("unix:///run/user/%d/podman/podman.sock", os.Getuid())
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
// structurally satisfied by *dockerclient.Client (Info/ImageInspect have the
// exact same signatures the moby client already exposes — the same shape
// internal/dispatcher's containerBackend dockerAPI interface uses for the
// same two calls).
type archProbeAPI interface {
	Info(ctx context.Context, options dockerclient.InfoOptions) (dockerclient.SystemInfoResult, error)
	ImageInspect(ctx context.Context, image string, opts ...dockerclient.ImageInspectOption) (dockerclient.ImageInspectResult, error)
}

// newEngineClient builds a moby client pointed at the given engine.
// "docker" uses client.FromEnv verbatim (the same DOCKER_HOST/DOCKER_*
// resolution cmd/reap.go and internal/server/wire.go already use to talk to
// docker from outside the daemon). "podman" additionally pins the host to
// resolvePodmanSocketPath(): unlike docker, podman's rootless socket has no
// well-known fixed path, so relying on DOCKER_HOST alone would silently
// probe the wrong (or no) socket on a host that never exported it.
func newEngineClient(engine string) (*dockerclient.Client, error) {
	opts := []dockerclient.Opt{dockerclient.FromEnv}
	if engine == "podman" {
		opts = append(opts, dockerclient.WithHost(resolvePodmanSocketPath()))
	}
	return dockerclient.New(opts...)
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

// probeArchMismatchWithAPI reports the engine's host architecture, the
// target image's architecture (if the image is present locally — this
// deliberately never pulls just to run a check), and whether they mismatch.
// note carries a human-readable reason no verdict could be reached (engine
// unreachable, image not present locally) — in either case this is
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

	insp, err := api.ImageInspect(checkCtx, image)
	if err != nil {
		return hostArch, "", false, fmt.Sprintf("image %q not present locally (skipping architecture check; it will still be enforced at pull/launch time)", image)
	}
	imageArch = insp.Architecture

	if hostArch != "" && imageArch != "" && hostArch != imageArch {
		mismatch = true
	}
	return hostArch, imageArch, mismatch, ""
}
