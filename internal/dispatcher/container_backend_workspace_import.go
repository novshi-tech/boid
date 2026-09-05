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

// container_backend_workspace_import.go is the container backend's half of
// workspace HOME volume migration: remove one workspace HOME volume, and
// extract a tar stream into its replacement.
//
// This reuses the init container's primitives (createWorkspaceInitContainer,
// runWorkspaceHomeContainer, the shared dockerres.WorkspaceInitContainerName
// / LabelWorkspaceInit, reapOrphanWorkspaceInitContainers) but not the
// RunWorkspaceInit function itself — its entrypoint is `bash -s` running a
// shell program, while this run's entrypoint is `tar` consuming a payload;
// see docs/plans/workspace-home-volume-persistence.md 論点f for the full
// rationale. Sharing the container name/label makes a migration and an init
// mutually exclusive on the engine, which matters because the daemon-side
// flock only covers one process.
//
// What is NOT shared is the network: the init container stays on the
// default bridge to download a toolchain, while this one only reads a pipe
// and writes a filesystem, so it is created with NetworkMode "none".

// workspaceHomeContainerCleanupTimeout bounds the deferred removal of a
// throwaway workspace-home container — the extraction container in this
// file, and the init container in container_backend_workspace_init.go, both
// via removeWorkspaceHomeContainer. Far longer than a ContainerRemove takes
// against a responsive engine, far shorter than a daemon shutdown will wait,
// so it only fires when the engine is genuinely not answering.
//
// Mutable purely so a test can shrink it
// (shrinkWorkspaceHomeContainerCleanupTimeout); nothing in production writes
// it. atomic rather than a plain var because containerCleanupContext's
// readers include waitLoop, which runs on a goroutine that outlives the test
// that started its session — a plain var here is a genuine data race between
// a later test's shrink and a previous test's still-running teardown.
var workspaceHomeContainerCleanupTimeout = newAtomicDuration(30 * time.Second)

// removeWorkspaceHomeContainer deletes a throwaway workspace-home container,
// under a context that has dropped the caller's cancellation and carries a
// bound of its own.
//
// Both callers reach this from a `defer`, commonly because the caller's own
// context was cancelled (an operator's ^C during a multi-GB upload, a
// dropped CLI connection) — reusing that context would make the removal
// fail in precisely the case it exists for. Dropping cancellation alone is
// not enough either: an engine socket that accepts a request and never
// answers would turn an unbounded removal into a permanent block inside a
// defer, parking every later dispatch of the workspace behind a migration
// or init flock that never releases. A bound converts that failure mode
// into one logged warning and a leaked container, which `boid reap`'s
// existing sweep of dockerres.LabelWorkspaceInit collects on the next
// daemon start.
func (b *containerBackend) removeWorkspaceHomeContainer(ctx context.Context, id, name, slug string) {
	cleanupCtx, cancel := containerCleanupContext(ctx)
	defer cancel()
	if _, err := b.api.ContainerRemove(cleanupCtx, id, client.ContainerRemoveOptions{Force: true}); err != nil {
		slog.Warn("container backend: remove workspace home container failed; it is left for the next daemon start's orphan sweep",
			"workspace_slug", slug, "container", name, "error", err)
	}
}

// containerCleanupContext derives the context every teardown engine call in
// this package runs under: the caller's values, none of its cancellation,
// and a bound of workspaceHomeContainerCleanupTimeout. Factored out because
// "this ContainerRemove cannot block forever" is a claim about the context
// that is not visible at a call site.
//
// The caller MUST call the returned cancel (defer, or immediately after the
// call) — a leaked timer per failed launch is small, but it is still a leak.
func containerCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), workspaceHomeContainerCleanupTimeout.Get())
}

var _ WorkspaceHomeImporter = (*containerBackend)(nil)

// RemoveWorkspaceHomeVolume deletes the workspace HOME volume, and is also
// the migration's in-use check.
//
// Force is deliberately NOT set, unlike WorkspaceHomeVolumeStore.Remove:
// that path wants os.RemoveAll's tolerance of a volume that vanished, but
// here a 404 has to be distinguishable from a deletion, because "there was
// nothing to remove" is a legitimate and common state (migrating into a
// workspace that has never been dispatched into) and the result reports it.
//
// The 409 is the whole in-use contract: on a running container holding the
// volume, DELETE /volumes/<name> returns 409 with and without force, and
// nothing is removed — so the check and the destruction are one atomic
// engine operation, and a workspace with a live job is refused before
// anything is lost. It is translated into ErrWorkspaceHomeInUse rather than
// passed through, because both the HTTP layer (409) and the CLI (an
// actionable message) have to recognize it, and neither can do that by
// matching on engine prose that varies between docker and podman.
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
// this process or on disk; measured homes this exists to migrate run to 4.3GB.
//
// The container runs as b.uid:b.gid (the same identity a job container
// gets) and the extraction discards the archive's own ownership
// (workspaceHomeImportArgv's --no-same-owner), which avoids the
// rootless-podman uid-mapping trap: the host user reads their own 0600
// credentials file in the CLI process, the bytes cross as data, and they
// land owned by whoever the harness will be. UsernsMode uses the same
// cached keep-id probe every other container here uses, for the same
// reason. Unlike the init.sh wrapper, this container does no passwd
// self-registration: its entrypoint is `tar` itself, and `tar -x
// --no-same-owner` never does an id/passwd/ssh lookup — it only reads
// uid/gid integers off the archive header and discards them.
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

	// The same name and label an init run takes — see this file's top comment
	// for why sharing them is the mechanism rather than a shortcut.
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
