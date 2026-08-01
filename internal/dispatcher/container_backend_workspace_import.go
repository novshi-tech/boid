package dispatcher

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/novshi-tech/boid/internal/dockerres"
)

// container_backend_workspace_import.go is the container backend's half of PR8
// of docs/plans/workspace-home-volume-persistence.md (論点 f): remove one
// workspace HOME volume, and extract a tar stream into its replacement.
//
// # D3 — what is reused from the init container, and what is not
//
// 論点 f asks whether PR5's init container machinery (RunWorkspaceInit) can be
// reused. The answer is: its PRIMITIVES yes, the function itself no.
//
// Not the function. RunWorkspaceInit's stdin carries a shell program and its
// entrypoint is `bash -s`; this run's stdin carries the payload and its
// entrypoint is `tar`. Making one function serve both would mean a request type
// with a mutually-exclusive "script or archive" pair and a wrapper builder that
// no-ops for one of them — and, more to the point, the init wrapper's whole
// diagnostic apparatus (the stage markers, workspaceInitStageOf, the boid-owned
// exit codes) describes stages that do not exist here.
//
// The primitives, yes, and deliberately:
//
//   - createWorkspaceInitContainer + clearWorkspaceInitContainer, unchanged.
//     This run takes the SAME deterministic container name as an init
//     (dockerres.WorkspaceInitContainerName) and carries the same
//     dockerres.LabelWorkspaceInit label — which is not an accident of reuse but
//     the point. A migration and an init must never be writing into one home at
//     once, the daemon-side flock only covers one process, and the engine's
//     name conflict is what covers the rest (a crashed daemon's in-flight init,
//     a second boid process). Sharing the name makes them mutually exclusive on
//     the engine for free, and the incumbent-verification and wait-rather-than-
//     kill behaviour applies unchanged.
//   - runWorkspaceHomeContainer (PR5's runWorkspaceInitContainer, generalized in
//     this PR to take an io.Reader): attach-before-start, the framed-stream
//     demux, the bounded tail-retaining output sink, the drain-then-close on
//     exit, and the WaitResponse.Error handling that keeps an engine-side wait
//     failure from reading as "exited 0". Every one of those is as load-bearing
//     here as there — more so, because the daemon has already destroyed the old
//     volume by the time this runs.
//   - reapOrphanWorkspaceInitContainers, for free, via the shared label: a
//     leaked extraction container is collected by the same startup sweep.
//
// What is NOT shared is the network. The init container is left on the default
// bridge because an init.sh downloads a toolchain; this one reads a pipe and
// writes a filesystem, so it is created with NetworkMode "none".

// workspaceHomeContainerCleanupTimeout bounds the deferred removal of a
// throwaway workspace-home container — the extraction container in this file,
// and PR5's init container in container_backend_workspace_init.go, both via
// removeWorkspaceHomeContainer.
//
// The same value, and the same reasoning, as
// workspaceHomeImportCleanupTimeout's: far longer than a ContainerRemove takes
// against a responsive engine, far shorter than a daemon shutdown will wait, so
// it only fires when the engine is genuinely not answering.
//
// Mutable purely so a test can shrink it
// (shrinkWorkspaceHomeContainerCleanupTimeout); nothing in production writes it.
//
// atomic rather than a plain var, and this is not defensive style [CI race
// detector, PR8]. containerCleanupContext's readers include waitLoop, which
// runs on a goroutine that outlives the test that started its session — so a
// later test shrinking the value races a previous test's teardown, with no
// t.Parallel() anywhere in the package. A plain var was reported as a genuine
// data race by -race on CI while passing locally, which is exactly the shape
// that ordering-dependent bug takes.
var workspaceHomeContainerCleanupTimeout = newAtomicDuration(30 * time.Second)

// removeWorkspaceHomeContainer deletes a throwaway workspace-home container,
// under a context that has dropped the caller's cancellation and carries a bound
// of its own.
//
// # Why not the caller's context (unchanged from what these defers always did)
//
// Both callers reach this from a `defer`, and the commonest route to that defer
// is a failure whose cause is the caller's context being cancelled — an
// operator's ^C during a multi-GB upload, a dropped CLI connection. Reusing that
// context would make the removal fail in precisely the case it exists for, and
// leak the container AND the deterministic name the next init/migration for this
// workspace needs.
//
// # Why a bound (codex review of PR8 round 2, Major 3)
//
// Dropping cancellation is not enough on its own, and both call sites used to do
// exactly that with a bare context.Background(). An engine socket that accepts a
// request and then never answers — which is what a wedged or half-dead
// docker/podman socket looks like from the client side — turns that removal into
// a permanent block, INSIDE a defer. The consequences are all downstream of the
// caller never getting its error back:
//
//   - Runner.ImportWorkspaceHome never reaches discardPartialWorkspaceHome, so
//     the deliberately WithoutCancel + 30s bounded VOLUME cleanup never runs and
//     the half-extracted home survives — the exact state PR8 round 1 added that
//     cleanup to prevent.
//   - Its `defer doneMigration()` never runs either, so every later dispatch of
//     that workspace parks in workspaceHomeInFlight.beginDispatch waiting for a
//     migration that has stopped making progress.
//   - For RunWorkspaceInit the caller is resolveWorkspaceHome, which is holding
//     this workspace's init flock — an flock no context can interrupt, so every
//     future dispatch of the workspace blocks in a syscall.
//
// A bound converts all three into one logged warning and a leaked container,
// which `boid reap`'s existing sweep of dockerres.LabelWorkspaceInit collects on
// the next daemon start.
func (b *containerBackend) removeWorkspaceHomeContainer(ctx context.Context, id, name, slug string) {
	cleanupCtx, cancel := containerCleanupContext(ctx)
	defer cancel()
	if _, err := b.api.ContainerRemove(cleanupCtx, id, client.ContainerRemoveOptions{Force: true}); err != nil {
		slog.Warn("container backend: remove workspace home container failed; it is left for the next daemon start's orphan sweep",
			"workspace_slug", slug, "container", name, "error", err)
	}
}

// containerCleanupContext derives the context every teardown engine call in this
// package runs under: the caller's values, none of its cancellation, and a bound
// of workspaceHomeContainerCleanupTimeout.
//
// Factored out rather than repeated because the property it carries is not
// visible at a call site — "this ContainerRemove cannot block forever" is a
// claim about the context, and the three teardown paths in
// containerBackend.Launch had the cancellation half right and the bound half
// missing for exactly as long as each of them was written by hand.
//
// The caller MUST call the returned cancel (defer, or immediately after the
// call) — a leaked timer per failed launch is small, but it is still a leak.
func containerCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), workspaceHomeContainerCleanupTimeout.Get())
}

var _ WorkspaceHomeImporter = (*containerBackend)(nil)

// RemoveWorkspaceHomeVolume deletes the workspace HOME volume, and is also the
// migration's in-use check (D4).
//
// # Force is deliberately NOT set, unlike WorkspaceHomeVolumeStore.Remove
//
// That path wants os.RemoveAll's tolerance of a volume that vanished, so it
// forces and treats 204 as success. Here a 404 has to be DISTINGUISHABLE from a
// deletion, because "there was nothing to remove" is a legitimate and common
// state — migrating into a workspace that has never been dispatched into — and
// the result reports it. Force would collapse the two into one status code.
//
// # The 409 is the whole in-use contract
//
// Measured against rootless podman 4.9.3 while writing PR7 (2026-07-27): with a
// running container holding the volume, DELETE /volumes/<name> returns 409
// ("volume is being used by the following container(s): <id>") WITH and WITHOUT
// force. Nothing was removed. That is what makes this a safe first step for a
// migration: the check and the destruction are one atomic engine operation, so
// a workspace with a live job is refused before anything is lost.
//
// It is translated into ErrWorkspaceHomeInUse rather than passed through,
// because both the HTTP layer (409) and the CLI (an actionable message) have to
// recognize it, and neither can do that by matching on engine prose that varies
// between docker and podman.
func (b *containerBackend) RemoveWorkspaceHomeVolume(ctx context.Context, volumeName string) (bool, error) {
	if !dockerres.IsValidVolumeName(volumeName) {
		// The same guard ensureNamedVolumes and WorkspaceHomeVolumeStore.Remove
		// have: never hand the engine a name that is not a name. Unreachable for
		// any slug that passed orchestrator.ValidWorkspaceSlug.
		return false, fmt.Errorf("workspace home volume %q is not a valid docker volume name", volumeName)
	}
	if _, err := b.api.VolumeRemove(ctx, volumeName, client.VolumeRemoveOptions{}); err != nil {
		switch {
		case errdefs.IsNotFound(err):
			// Not an error: a workspace that has never been dispatched into has
			// no volume yet, and migrating into it is exactly the fresh-install
			// restore case. Reported as existed=false rather than folded into
			// the nil error, so the CLI can tell "replaced" from "created".
			return false, nil
		case errdefs.IsConflict(err):
			return false, fmt.Errorf("%w: %s", ErrWorkspaceHomeInUse, err)
		default:
			return false, fmt.Errorf("remove workspace home volume %q: %w", volumeName, err)
		}
	}
	return true, nil
}

// ImportWorkspaceHome extracts req.Tar into req.HomeSource.
//
// The tar is streamed straight from the caller's reader onto the container's
// stdin (see runWorkspaceHomeContainer's io.Copy) — it is never buffered, in
// this process or on disk. The measured homes this exists to migrate run to
// 4.3GB.
//
// § uid — the container runs as b.uid:b.gid, the same identity a JOB container
// gets, and the extraction is told to discard the archive's own ownership
// (workspaceHomeImportArgv's --no-same-owner). That pairing IS 論点 f's answer
// to the rootless-podman uid-mapping trap: the host user reads their own 0600
// credentials file in the CLI process, the bytes cross as data, and they land
// owned by whoever the harness will be.
//
// § UsernsMode — the same cached keep-id probe every other container here uses.
// Without it, on rootless podman, everything this run creates lands owned by a
// subuid and the workspace home becomes unwritable by the harness.
//
// § passwd self-registration (docs/plans/release-onboarding.md 決定1, PR2)
// — deliberately NOT done here, unlike the init.sh wrapper
// (container_backend_workspace_init.go / workspace_init.go's
// PasswdSelfRegisterShellSnippet). This container's entrypoint is `tar`
// itself (workspaceHomeImportArgv), not a shell running arbitrary
// toolchain installers — `tar -x --no-same-owner` never does an
// id/passwd/ssh lookup, it only reads uid/gid integers off the archive
// header and discards them (that is what --no-same-owner means). Verified
// by reading GNU tar's extraction path, not assumed.
func (b *containerBackend) ImportWorkspaceHome(ctx context.Context, req WorkspaceHomeImportRequest) error {
	homeMount, err := workspaceHomeVolumeMount(req.Slug, req.HomeSource, req.HomeTarget)
	if err != nil {
		return err
	}

	// Same reasoning as RunWorkspaceInit's: ContainerCreate would otherwise
	// auto-create the volume implicitly and UNLABELLED, which
	// Runner.ensureWorkspaceHomeVolume cannot repair on any later dispatch.
	// Runner.ImportWorkspaceHome created it moments ago; this is idempotent, and
	// the ANSWER is checked so that a volume replaced in the gap is refused
	// loudly rather than extracted into.
	got, err := b.EnsureWorkspaceHomeVolume(ctx, WorkspaceHomeVolumeRequest{
		Slug:        req.Slug,
		Name:        req.HomeSource,
		CandidateID: req.HomeID,
	})
	if err != nil {
		return err
	}
	if err := verifyWorkspaceHomeIdentity(req.HomeSource, req.HomeID, got); err != nil {
		return fmt.Errorf("workspace home import container for %q: %w", req.Slug, err)
	}

	image, err := b.resolveImage(ctx, "")
	if err != nil {
		return fmt.Errorf("workspace home import container for %q: %w", req.Slug, err)
	}

	// The SAME name and label an init run takes — see this file's D3 note on why
	// sharing them is the mechanism rather than a shortcut.
	name := dockerres.WorkspaceInitContainerName(b.installID, req.Slug)
	labels := map[string]string{dockerres.LabelWorkspaceInit: req.Slug}
	if b.installID != "" {
		labels[dockerres.LabelWorkspaceInitInstallID] = b.installID
	}

	initTrue := true
	hostCfg := &container.HostConfig{
		Init:   &initTrue,
		Mounts: []mount.Mount{homeMount},
		// Unlike the init container, which needs the default bridge to download
		// a toolchain: this run reads a pipe and writes a filesystem. Nothing it
		// does should be able to reach the network, and saying so costs nothing.
		NetworkMode: "none",
		UsernsMode:  b.resolveUsernsMode(ctx),
	}
	cfg := &container.Config{
		Image: image,
		// Overriding the image's own ENTRYPOINT
		// (["/usr/local/bin/boid","runner-container"]) for the same reason
		// RunWorkspaceInit does: that entrypoint looks for a sandbox spec that
		// does not exist here.
		Entrypoint: workspaceHomeImportArgv(req.HomeTarget),
		// Explicitly empty rather than nil: a nil Cmd inherits the image's own,
		// which would be appended to the tar command line as operands — i.e. tar
		// would try to extract only the members those words name.
		Cmd: []string{},
		// No Env at all. The extraction needs nothing from the environment, and
		// the three contractual init variables (HOME, BOID_WORKSPACE_*) describe
		// a script's environment, not an archive's.
		WorkingDir:   req.HomeTarget,
		Tty:          false,
		OpenStdin:    true,
		StdinOnce:    true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		User:         fmt.Sprintf("%d:%d", b.uid, b.gid),
		Labels:       labels,
	}

	id, err := b.createWorkspaceInitContainer(ctx, client.ContainerCreateOptions{
		Config: cfg, HostConfig: hostCfg, Name: name,
	}, name, req.Slug)
	if err != nil {
		return err
	}
	defer b.removeWorkspaceHomeContainer(ctx, id, name, req.Slug)

	exitCode, output, err := b.runWorkspaceHomeContainer(ctx, id, req.Slug, req.Tar)
	if err != nil {
		return fmt.Errorf("workspace home import container for %q: %w", req.Slug, err)
	}
	if exitCode != 0 {
		// tar's own stderr is the entire diagnosis and the container is gone by
		// the time this renders, so the tail goes into the error verbatim —
		// same bound, same reasoning as workspaceInitFailure.
		return fmt.Errorf(
			"workspace home import container for %q failed (tar exited %d)\n--- output tail ---\n%s",
			req.Slug, exitCode, output.Tail(workspaceInitOutputTail))
	}
	slog.Info("workspace home import completed",
		"workspace_slug", req.Slug, "container", name,
		"retained_output_bytes", output.Len(), "dropped_output_bytes", output.Dropped())
	return nil
}
