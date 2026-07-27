package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/novshi-tech/boid/internal/dockerres"
)

// container_backend_workspace_init.go is the container backend's half of PR5
// of docs/plans/workspace-home-volume-persistence.md (論点 c): run one
// WorkspaceInitRequest in a throwaway container that mounts the workspace home.
//
// # Why this is not Launch
//
// Launch already knows how to create, attach to, start, wait on and remove a
// container, but it does all of that as part of building a backend.SandboxSession
// — a transcript spool on disk, registration in b.sessions so Adopt can find it,
// a diagnostics collector on exit, per-job TLS material, a workspace network,
// a sandbox spec bind-mounted in. None of that applies to an init run: there is
// no job row it belongs to, nothing attaches to it, and every one of those
// side effects would have to be un-done or special-cased. This is the same
// primitives (§D10), used directly.

const (
	// workspaceInitConflictWaitTimeout bounds how long a run waits for an
	// incumbent container holding its name to finish (§D6).
	//
	// Generous on purpose: what it is waiting for is a toolchain install
	// (~1.5GB measured — go, node via volta, claude, codex, opencode), and the
	// alternative to waiting is destroying it partway through. On expiry the
	// run FAILS rather than force-removing: at that point boid cannot tell a
	// slow install from a wedged one, and of the two possible mistakes, "abort
	// a dispatch and tell the operator which container to look at" is
	// recoverable while "SIGKILL a 25-minute download" is not.
	workspaceInitConflictWaitTimeout = 30 * time.Minute
)

var _ WorkspaceInitExecutor = (*containerBackend)(nil)

// waitResponseEngineError reports the error the ENGINE attached to a wait
// response, or nil if it attached none.
//
// container.WaitResponse carries two independent facts, and only one of them is
// an exit status:
//
//	type WaitResponse struct {
//		Error      *WaitExitError // engine-side: the wait produced no status
//		StatusCode int64          // the container process's exit status
//	}
//
// Error is set when the engine could not report an exit status at all — the
// runtime failed to wait, the shim died, the container never started. The SDK
// does not look at it (moby/moby/client@v0.5.0 container_wait.go decodes the
// response body and forwards it verbatim on the Result channel), so it is the
// caller's job, and a caller that reads only StatusCode reads the ZERO VALUE of
// a field the engine never filled in — i.e. reports "exited 0" for a wait that
// failed.
//
// Returned as an error rather than a bool so that both call sites can name the
// engine's own message, which is the only description of what went wrong: the
// container is gone by the time anything renders this, and its logs with it.
func waitResponseEngineError(res container.WaitResponse) error {
	if res.Error == nil {
		return nil
	}
	// The Message field is `omitempty` in the API schema, so a response can
	// legitimately carry the Error object with nothing in it. That is still a
	// failed wait; only the description is missing.
	if res.Error.Message == "" {
		return errors.New("the engine gave no message")
	}
	return errors.New(res.Error.Message)
}

// RunWorkspaceInit prepares one workspace home: it starts a container from the
// backend's own image with the home mounted at req.HomeTarget, feeds the
// assembled wrapper to `bash -s` on its stdin, and reports whether it
// succeeded.
//
// § D4 — network. The container is left on the ENGINE'S DEFAULT BRIDGE:
// neither NetworkMode nor NetworkingConfig is set, and ensureWorkspaceNetwork
// is deliberately not called. Three reasons, in order of weight:
//
//   - The per-workspace network a JOB gets is `Internal: true` and reaches the
//     outside world only through the allowlist egress proxy. An init.sh exists
//     to download a toolchain; under that allowlist essentially every installer
//     fails, so putting the init container there would make the feature not
//     work at all.
//   - Connecting it to the compose project's own `boid_default` network
//     instead was rejected: doing so needs the daemon to identify its own
//     container, and the field that would carry that (ContainerBackendOptions.
//     SelfContainerID, sourced from os.Getenv("HOSTNAME")) is EMPTY on a live
//     rootless podman deploy — measured 2026-07-26, the same measurement that
//     made DetectDaemonStateVolumes read /proc/self/mountinfo instead. The
//     property actually needed here is only "can reach the internet", and the
//     default bridge provides it without depending on self-identification.
//   - There is nothing on boid's internal network for it to talk to. The git
//     gateway, egress proxy, broker and dockerproxy all serve JOBS; an init run
//     has no job token, no broker cert and no clone to do.
//
// The consequence — that this container has unrestricted egress — is part of
// the trusted boundary, stated rather than implied: boid chooses the image,
// boid assembles the script, boid starts the container. Nothing here runs
// inside a sandboxed job (論点 c's A vs A'), and the script itself is the
// operator's own init.sh, which has never been sandboxed.
//
// § D8 — image. Always b.defaultImage; a workspace's own ContainerImage
// override is deliberately NOT honored:
//
//   - There is no path to it from here. The override is resolved by
//     Runner.resolveContainerImage, an independent WorkspaceLookup.Load that
//     runs later in Dispatch, and resolveWorkspaceHome (which owns the init
//     lock) runs before it. Threading it in would add a second workspace
//     lookup on the init path for no gain.
//   - It would turn an optional job-image override into a hard init failure
//     TODAY. resolveImage rejects any override that does not carry
//     boid.runner_protocol=v1, and nothing bakes that label yet (see
//     boidRunnerProtocolLabel) — so every workspace that sets container_image
//     would stop being able to prepare its home at all.
//   - The prep does not need it. What the wrapper uses is bash, coreutils and
//     whatever the operator's init.sh reaches for; §決定 11 requires an
//     override to derive from this same base image, so the base is a subset of
//     any legal override rather than a different environment.
func (b *containerBackend) RunWorkspaceInit(ctx context.Context, req WorkspaceInitRequest) error {
	homeMount, err := workspaceInitHomeMount(req)
	if err != nil {
		return err
	}
	// §D4 — the init container does NOT go through Launch, so it does not go
	// through ensureNamedVolumes either. Without this call ContainerCreate
	// would happily auto-create the volume implicitly, unlabeled: the name
	// checks in reap and dockerproxy would still recognize it (they key off
	// the name prefix), but every label-based check would not — including the
	// identity resolveWorkspaceHome compares its completion marker against,
	// which would then be permanently absent and wedge this workspace (see
	// Runner.ensureWorkspaceHomeVolume for why that cannot be repaired by
	// re-running).
	//
	// resolveWorkspaceHome has normally already created it moments ago; this
	// is idempotent and re-creates it with the SAME identity if it vanished in
	// between, so the marker written afterwards stays accurate either way.
	//
	// The ANSWER is checked, not just the call (codex review of PR6, Major 1).
	// A volume that survived reports the identity it was born with and the
	// request's CandidateID is discarded, so "it came back as something else"
	// means a third incarnation of this name is in place — and preparing THAT
	// while resolveWorkspaceHome goes on to stamp a marker recording req.HomeID
	// produces a marker that vouches for contents nobody prepared. See
	// verifyWorkspaceHomeIdentity for what the check does and does not close.
	got, err := b.EnsureWorkspaceHomeVolume(ctx, WorkspaceHomeVolumeRequest{
		Slug:        req.Slug,
		Name:        req.HomeSource,
		CandidateID: req.HomeID,
	})
	if err != nil {
		return err
	}
	if err := verifyWorkspaceHomeIdentity(req.HomeSource, req.HomeID, got); err != nil {
		return fmt.Errorf("workspace init container for %q: %w", req.Slug, err)
	}
	image, err := b.resolveImage(ctx, "")
	if err != nil {
		return fmt.Errorf("workspace init container for %q: %w", req.Slug, err)
	}

	name := dockerres.WorkspaceInitContainerName(b.installID, req.Slug)
	labels := map[string]string{dockerres.LabelWorkspaceInit: req.Slug}
	if b.installID != "" {
		labels[dockerres.LabelWorkspaceInitInstallID] = b.installID
	}

	initTrue := true
	hostCfg := &container.HostConfig{
		Init:   &initTrue,
		Mounts: []mount.Mount{homeMount},
		// UsernsMode (§D7): "" on every engine but rootless podman, where it
		// is "keep-id". Without it every directory and file this run creates
		// lands on the host owned by a subuid, and PR3's prepareBindTarget
		// then refuses the very skeleton this run just made — turning a
		// working install into a dispatch that fails on every job with the
		// poisoned-bind-target error. Same cached probe the job containers use.
		UsernsMode: b.resolveUsernsMode(ctx),
	}
	cfg := &container.Config{
		Image: image,
		// §D8: the image's own ENTRYPOINT is
		// ["/usr/local/bin/boid","runner-container"], which would look for a
		// sandbox spec that does not exist. Launch never sets Entrypoint
		// because a job container is exactly what that entrypoint is for; this
		// one has to override it.
		Entrypoint: []string{"/bin/bash", "-s"},
		// Explicitly empty rather than nil: a nil Cmd inherits the image's own.
		Cmd:          []string{},
		Env:          envSlice(req.Env),
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
	createOpts := client.ContainerCreateOptions{Config: cfg, HostConfig: hostCfg, Name: name}

	id, err := b.createWorkspaceInitContainer(ctx, createOpts, name, req.Slug)
	if err != nil {
		return err
	}
	defer func() {
		// Unconditional: this container is throwaway, and leaving it behind
		// would additionally hold the deterministic name the NEXT init for
		// this workspace needs.
		if _, rerr := b.api.ContainerRemove(context.Background(), id, client.ContainerRemoveOptions{Force: true}); rerr != nil {
			slog.Warn("container backend: remove workspace init container failed",
				"workspace_slug", req.Slug, "container", name, "error", rerr)
		}
	}()

	exitCode, output, err := b.runWorkspaceInitContainer(ctx, id, req)
	if err != nil {
		return fmt.Errorf("workspace init container for %q: %w", req.Slug, err)
	}
	if exitCode != 0 {
		return workspaceInitFailure(req.Slug, exitCode, output)
	}
	slog.Info("workspace home init completed",
		"workspace_slug", req.Slug, "container", name,
		"retained_output_bytes", output.Len(), "dropped_output_bytes", output.Dropped())
	return nil
}

// workspaceInitHomeMount turns req's home into the one mount the init
// container gets: a VOLUME mount of the workspace's own named volume (PR6, 論点
// a/e).
//
// Fail-closed on an absolute source, which is the exact inverse of what this
// function guarded through PR5 — and the inversion is the point. The home was a
// host directory then, and docker reads a RELATIVE mount source as a volume
// name, so a path that stopped being absolute would have silently prepared a
// brand new empty volume and reported success. Now the home IS a volume name,
// and an absolute value would silently bind some host directory into the
// container instead: the init would appear to succeed, the marker would be
// stamped, and the workspace's real home would never be prepared. Both eras
// fail the same way when the two representations are confused, so the check
// tracks which representation is current rather than being dropped.
//
// The name is validated against docker's own grammar as well, for the reason
// dockerres.IsValidVolumeName exists: a mount source is classified as a named
// volume purely by "it does not start with /", so a stray relative path would
// otherwise reach the engine as a volume name.
func workspaceInitHomeMount(req WorkspaceInitRequest) (mount.Mount, error) {
	if filepath.IsAbs(req.HomeSource) {
		return mount.Mount{}, fmt.Errorf(
			"workspace init container for %q: home source %q is an absolute path; as of PR6 of "+
				"docs/plans/workspace-home-volume-persistence.md the workspace home is a docker named volume "+
				"(dockerres.WorkspaceHomeVolumeName), and binding a host path here would prepare something other than "+
				"the home the job will actually get", req.Slug, req.HomeSource)
	}
	if !dockerres.IsValidVolumeName(req.HomeSource) {
		return mount.Mount{}, fmt.Errorf(
			"workspace init container for %q: home source %q is not a valid docker volume name", req.Slug, req.HomeSource)
	}
	if !filepath.IsAbs(req.HomeTarget) {
		return mount.Mount{}, fmt.Errorf(
			"workspace init container for %q: home target %q is not an absolute path", req.Slug, req.HomeTarget)
	}
	return mount.Mount{Type: mount.TypeVolume, Source: req.HomeSource, Target: req.HomeTarget}, nil
}

// EnsureWorkspaceHomeVolume creates the workspace HOME volume if it is not
// there and reports the identity it carries afterwards (PR6, 論点 a/b).
//
// One VolumeCreate does both jobs. The Engine API returns an already-existing
// volume for a create of the same name — 201, the existing volume, the
// EXISTING labels, request labels discarded (measured against podman 4.9.3 on
// 2026-07-27: three creates of one name carrying different label sets all came
// back with the first call's labels). So a fresh volume gets req.CandidateID
// stamped on it and a surviving one reports whatever it was born with, which is
// what makes "this volume was deleted and re-created since the marker was
// written" observable at all.
//
// It follows that the identity CANNOT be updated afterwards, and everything
// downstream is built on that: see Runner.ensureWorkspaceHomeVolume for why an
// unlabelled volume is a hard error rather than a re-init.
//
// The labels are 論点 a's decision, unchanged: boid.workspace_home +
// boid.workspace_home_install_id and NOTHING else, because each of the three
// job labels is an enumeration filter that would put a persistent volume into a
// reaper's sweep. The identity label joins them; it is likewise a key nothing
// enumerates on.
func (b *containerBackend) EnsureWorkspaceHomeVolume(ctx context.Context, req WorkspaceHomeVolumeRequest) (string, error) {
	if !dockerres.IsValidVolumeName(req.Name) {
		return "", fmt.Errorf("workspace home volume name %q is not a valid docker volume name", req.Name)
	}
	if req.CandidateID == "" {
		return "", fmt.Errorf("workspace home volume %q: no candidate identity to stamp on it", req.Name)
	}
	labels := map[string]string{
		dockerres.LabelWorkspaceHome:   req.Slug,
		dockerres.LabelWorkspaceHomeID: req.CandidateID,
	}
	if b.installID != "" {
		labels[dockerres.LabelWorkspaceHomeInstallID] = b.installID
	}
	res, err := b.api.VolumeCreate(ctx, client.VolumeCreateOptions{Name: req.Name, Labels: labels})
	if err != nil {
		return "", fmt.Errorf("create workspace home volume %q: %w", req.Name, err)
	}
	return res.Volume.Labels[dockerres.LabelWorkspaceHomeID], nil
}

// verifyWorkspaceHomeIdentity reports whether the volume a caller is ABOUT TO
// MOUNT is still the incarnation the daemon resolved: got is what this caller's
// own VolumeCreate answered with, want is the identity
// Runner.resolveWorkspaceHome observed and (for the init path) is going to
// record in the completion marker.
//
// # Why the check is repeated at each use site instead of trusted from resolve
//
// resolveWorkspaceHome already compares the volume's identity against the
// marker, but that comparison is finished before either container exists. Two
// things can happen in the gap, and both are ordinary rather than exotic — an
// operator's `docker volume rm`, a reap misfire, a half-completed workspace
// remove:
//
//   - the volume is REPLACED, and the mount silently attaches somebody else's
//     contents;
//   - the volume is simply GONE, in which case the create on this path brings
//     back an empty one — or, if nothing calls VolumeCreate at all,
//     ContainerCreate does it implicitly and UNLABELLED, which is
//     unrecoverable (the Engine API has no way to add a label to an existing
//     volume, so Runner.ensureWorkspaceHomeVolume hard-fails every later
//     dispatch of that workspace forever).
//
// The daemon has no way to hold a volume in place across those calls: docker
// has no lease, no reservation and no compare-and-swap on a volume, and the
// mount is not established until the container is created.
//
// # What this does NOT close
//
// The race is not closed, only narrowed, and deliberately so. The volume can
// still be removed AFTER this returns and before ContainerCreate takes the
// mount — the window shrinks from "the whole of resolve + init + spec build +
// launch" to "two adjacent engine calls", but it does not reach zero. What the
// check buys is the difference between a mismatch that is SILENT (a marker
// vouching for contents nobody prepared; an agent starting against an empty
// $HOME and reporting "CLI not found") and one that is LOUD, at the point of
// use, naming both identities. That is the same trade the identity mechanism
// makes everywhere else — see workspaceHomeMarker.HomeID's account of what a
// volume label can and cannot witness.
func verifyWorkspaceHomeIdentity(volumeName, want, got string) error {
	if want == "" || want == got {
		return nil
	}
	if got == "" {
		return fmt.Errorf(
			"the workspace HOME volume %q carries no %s label, so this is not the volume boid resolved for this dispatch "+
				"(it expected the identity %q). Every volume boid creates for a workspace home is labelled at creation, so an "+
				"unlabelled one under this name was created by something else — most likely the container engine's own "+
				"implicit create after the real volume was removed. "+
				"[復旧] その volume が不要なら削除する (`docker volume rm %[1]s`) — 次の dispatch が正しい label 付きで作り直す",
			volumeName, dockerres.LabelWorkspaceHomeID, want)
	}
	return fmt.Errorf(
		"the workspace HOME volume %q carries the identity %q, but this dispatch resolved %q — the volume was removed and "+
			"re-created after boid checked it and before it could be mounted (a `docker volume rm`, a reap misfire, a "+
			"half-completed workspace remove). Refusing to use it: its contents are not the ones the completion marker "+
			"vouches for. The next dispatch re-runs the workspace home init against the volume that is there now",
		volumeName, got, want)
}

// createWorkspaceInitContainer creates the container, resolving a name
// conflict once (§D6).
//
// The conflict is not an error condition to be papered over — it is the
// mechanism. A flock serializes inits within one daemon process and vanishes
// when that process dies; the container does not. So after a crash-and-restart
// the engine's own "a container with that name already exists" is the only
// thing standing between the next dispatch and two inits writing into one home
// concurrently, which is precisely the Phase 4 contract
// (docs/plans/home-workspace-volume.md:83) that would otherwise break.
func (b *containerBackend) createWorkspaceInitContainer(ctx context.Context, opts client.ContainerCreateOptions, name, slug string) (string, error) {
	res, err := b.api.ContainerCreate(ctx, opts)
	if err == nil {
		return res.ID, nil
	}
	if !errdefs.IsConflict(err) {
		return "", fmt.Errorf("create workspace init container %q: %w", name, err)
	}

	if cerr := b.clearWorkspaceInitContainer(ctx, name, slug); cerr != nil {
		return "", cerr
	}

	res, err = b.api.ContainerCreate(ctx, opts)
	if err != nil {
		// Fail loud rather than retrying in a loop: a second conflict means
		// something else is creating this exact name concurrently, i.e. the
		// exclusivity this name exists to provide cannot be established, and
		// proceeding anyway is the concurrent init it was meant to prevent.
		return "", fmt.Errorf(
			"create workspace init container %q after clearing a name conflict: %w "+
				"(another process appears to be preparing this same workspace home; "+
				"inspect it with `docker ps -a --filter name=%s`)", name, err, name)
	}
	return res.ID, nil
}

// clearWorkspaceInitContainer makes the deterministic name available again —
// or refuses to, loudly.
//
// Every step here exists because the thing being cleared is, by construction,
// the most expensive object in the system to destroy by mistake: a container
// that is very likely tens of minutes into a ~1.5GB toolchain download, writing
// into the same workspace home this run wants. Restarting that is not a retry,
// it is throwing away the work and then re-running init.sh over a home the
// killed run left half-built. So the sequence answers three questions in
// order, and STOPS at the first one it cannot answer:
//
//  1. Does the incumbent still exist? Only errdefs.IsNotFound says "no".
//     Every other inspect error — a 500 from an engine busy serving that same
//     download, a socket timeout, a proxy hiccup — means the state is UNKNOWN,
//     and this returns it as an error instead of proceeding. The first version
//     of this code collapsed all errors into "it vanished" and went straight
//     to a force remove, which turned a transient engine hiccup into a killed
//     install; that is precisely what §D6's 「running なら完了を待つ。force
//     kill しない」forbids. errdefs.IsNotFound is the same predicate
//     internal/reap/reap.go uses for the same distinction.
//
//  2. Is it boid's, and is it THIS workspace's? The deterministic name is
//     reserved by nothing. `docker run --name boid-ws-init-<install8>-<slug> …`
//     succeeds for anybody, and even among boid's own installs the name
//     carries only the first 8 characters of the install id
//     (dockerres.installIDPart), so two installations sharing one engine can
//     compute the same name. Without this check the conflict path would force
//     remove whatever holds it — a stranger's container, a boid JOB container,
//     another install's in-flight init, or (see
//     verifyWorkspaceInitIncumbent's third clause) another WORKSPACE's.
//     Refusing is strictly better than guessing: boid loses one dispatch and
//     says exactly which container is in the way.
//
//  3. Is it busy? A running incumbent is waited for, not killed; one that is
//     known to have stopped is unambiguous garbage and goes immediately.
//     "Known" is the load-bearing word: container.InspectResponse.State is a
//     POINTER, and a nil one means the engine did not report a state, not that
//     there is no process. Treating nil as stopped would force remove a
//     container whose liveness was never established — the same mistake as
//     step 1's, one field further in — so a missing State stops this function
//     exactly the way a failed inspect does.
//
// Everything after the inspect addresses the container by ID, never by name.
// A name is a mutable handle: the incumbent can exit, be collected by a
// startup sweep, and the name be re-bound to a NEW init container between the
// inspect and the remove — at which point a remove by name destroys a
// container this function never looked at. Pinned to an id, the worst case is
// a harmless 404.
func (b *containerBackend) clearWorkspaceInitContainer(ctx context.Context, name, slug string) error {
	insp, err := b.api.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if err != nil {
		if errdefs.IsNotFound(err) {
			// The incumbent vanished between the create and the inspect
			// (another daemon's own cleanup, an operator's `docker rm`).
			// Nothing to wait for and nothing to remove; the retry the caller
			// does next will either succeed or conflict again.
			slog.Info("container backend: the workspace init container holding this name is already gone; retrying the create",
				"container", name)
			return nil
		}
		return fmt.Errorf(
			"inspect the workspace init container %q holding this name: %w "+
				"(refusing to remove it while its state is unknown — it may be mid-install into this workspace home; "+
				"check the engine with `docker ps -a --filter name=%s`)", name, err, name)
	}

	incumbent := insp.Container
	if incumbent.ID == "" {
		// No id means nothing downstream can be pinned to an individual, so
		// the safe operations are not available. Treated like any other
		// unknown state rather than falling back to the name.
		return fmt.Errorf(
			"the engine reported no container id for the workspace init container %q holding this name; "+
				"refusing to wait on or remove it by name, because a name can be re-bound to a different container", name)
	}
	if err := b.verifyWorkspaceInitIncumbent(name, slug, incumbent); err != nil {
		return err
	}
	if incumbent.State == nil {
		// Step 3's "known" clause. Every branch below this point either waits
		// for a running container or destroys a stopped one, and both need the
		// engine to have said which it is. It did not.
		//
		// The alternative — read nil as stopped — is what this used to do, and
		// it is unsound in exactly the expensive direction: the container this
		// resolves against is by construction the one most likely to be
		// mid-toolchain-install into this same home, so a wrong guess costs a
		// ~1.5GB download and leaves the home half-built for the re-run.
		// Missing information is not evidence of a stopped container.
		return fmt.Errorf(
			"the engine reported no state for the workspace init container %q (id %s) holding this name; "+
				"refusing to wait on or remove it, because boid cannot tell a container that is mid-install into this "+
				"workspace home from one that has already exited. Inspect it (`docker inspect %s`) and remove it by hand "+
				"(`docker rm -f %s`) if it is genuinely finished", name, incumbent.ID, incumbent.ID, incumbent.ID)
	}

	if incumbent.State.Running {
		slog.Info("container backend: another workspace init container already holds this name and is still running; waiting for it to finish",
			"container", name, "container_id", incumbent.ID, "timeout", workspaceInitConflictWaitTimeout)
		waitCtx, cancel := context.WithTimeout(ctx, workspaceInitConflictWaitTimeout)
		defer cancel()
		waitRes := b.api.ContainerWait(waitCtx, incumbent.ID, client.ContainerWaitOptions{
			Condition: container.WaitConditionNotRunning,
		})
		select {
		case res := <-waitRes.Result:
			// The wait is here to establish ONE fact — the incumbent is no
			// longer running — because the very next statement force removes
			// it. An engine that answered with an error established nothing, so
			// removing anyway would be §D6's forbidden "SIGKILL a 25-minute
			// download" taken on an unread field. (The earlier version of this
			// select discarded the response entirely, so there was not even a
			// StatusCode to reason about.)
			if eerr := waitResponseEngineError(res); eerr != nil {
				return fmt.Errorf(
					"the engine put an error in the wait response for the workspace init container %q (id %s) already holding this "+
						"name, so whether it is still running is unknown: %w "+
						"(refusing to remove it on that basis — it may be mid-install into this workspace home; "+
						"check the engine with `docker ps -a --filter name=%s`)",
					name, incumbent.ID, eerr, name)
			}
		case werr := <-waitRes.Error:
			if !errdefs.IsNotFound(werr) {
				return fmt.Errorf("wait for the workspace init container %q (id %s) already holding this name: %w",
					name, incumbent.ID, werr)
			}
			// It finished and was collected while this call was setting up the
			// wait. That is the outcome being waited for, not a failure.
		case <-waitCtx.Done():
			return fmt.Errorf(
				"the workspace init container %q (id %s) was still running after %s; refusing to kill it because it is most likely "+
					"still installing a toolchain into this workspace home. Inspect it (`docker logs %s`) and remove it "+
					"(`docker rm -f %s`) if it is genuinely stuck: %w",
				name, incumbent.ID, workspaceInitConflictWaitTimeout, incumbent.ID, incumbent.ID, waitCtx.Err())
		}
	}

	if _, rerr := b.api.ContainerRemove(ctx, incumbent.ID, client.ContainerRemoveOptions{Force: true}); rerr != nil && !errdefs.IsNotFound(rerr) {
		slog.Warn("container backend: remove the workspace init container holding the name failed; retrying the create anyway",
			"container", name, "container_id", incumbent.ID, "error", rerr)
	}
	return nil
}

// verifyWorkspaceInitIncumbent reports whether the container holding the
// deterministic name is one of THIS installation's workspace init containers,
// i.e. whether removing it is boid cleaning up after itself rather than boid
// destroying somebody else's work. See clearWorkspaceInitContainer's step 2.
//
// The errors are written for an operator who has to act on them, because the
// action differs from every other failure on this path: nothing boid does will
// clear the name by itself, so the fix is to rename or remove the container
// that holds it.
//
// It asks three questions: is boid's init label there at all, does its VALUE
// name the workspace this run is preparing, and does the install-id label name
// this installation. The first is separate from the second only so the error
// can distinguish "not boid's container" from "boid's, but somebody else's
// work" — the two call for different operator actions.
//
// The second question is the one an earlier version of this function skipped,
// on an argument that does not hold:
//
//	"the name is a pure function of (installIDPart, slug), so a container
//	 carrying this install's init label under this name is this workspace's
//	 init container by construction."
//
// That reasons from the conclusion. The name being a pure function tells you
// what name THIS process computes; it says nothing about the provenance of
// whatever already holds it, which is precisely the unknown here. Two ordinary
// inputs break it:
//
//   - `docker rename` / `podman rename` moves a name onto an existing
//     container without touching its labels, so another workspace's init
//     container can end up under this one's name.
//   - creating the name directly (`docker run --name … --label
//     boid.workspace_init=otherws`) needs no privilege beyond engine access,
//     which is the same access a job with capabilities.docker already has.
//
// Under the presence-only check both cases passed verification and the
// incumbent — another workspace's in-flight install — was force removed. The
// comparison costs one string equality, and the failure it "adds" is the one
// this whole function exists to produce: boid declines to destroy a container
// it did not prove is its own.
//
// (The slug in the NAME still cannot collide — orchestrator.ValidWorkspaceSlug
// restricts slugs to [a-z0-9-], which SanitizeNamePart passes through
// unchanged — but that is a statement about boid's own naming, not about who
// holds the name.)
func (b *containerBackend) verifyWorkspaceInitIncumbent(name, slug string, incumbent container.InspectResponse) error {
	var labels map[string]string
	if incumbent.Config != nil {
		labels = incumbent.Config.Labels
	}
	if _, ok := labels[dockerres.LabelWorkspaceInit]; !ok {
		return fmt.Errorf(
			"the workspace init container name %q is already taken by container %s, which boid did not create "+
				"(it carries no %s label) — refusing to remove it. Rename or remove that container "+
				"(`docker inspect %s`) so this workspace home can be prepared",
			name, incumbent.ID, dockerres.LabelWorkspaceInit, incumbent.ID)
	}
	if got := labels[dockerres.LabelWorkspaceInit]; got != slug {
		return fmt.Errorf(
			"the workspace init container name %q is already taken by container %s, which is preparing a DIFFERENT workspace "+
				"(%s=%q, this run is preparing %q) — refusing to remove it, because it may be mid-install into that workspace's "+
				"own home. A container can acquire this name without having been created for it (`docker rename`, or a create "+
				"that simply set the label); remove it by hand once it has finished",
			name, incumbent.ID, dockerres.LabelWorkspaceInit, got, slug)
	}
	// Only 8 characters of the install id go into the name, so a collision
	// between two installations sharing one engine is possible; the full id
	// lives in the label. An empty b.installID means this daemon does not
	// scope its names by install at all (DI/test callers), so there is nothing
	// to compare against.
	if b.installID != "" && labels[dockerres.LabelWorkspaceInitInstallID] != b.installID {
		return fmt.Errorf(
			"the workspace init container name %q is already taken by container %s, which belongs to a DIFFERENT boid installation "+
				"(%s=%q, this daemon is %q) — refusing to remove it, because it may be mid-install into that installation's own "+
				"workspace home. Only the leading characters of an install id go into the name, so two installations sharing one "+
				"engine can collide here; remove that container by hand once it has finished",
			name, incumbent.ID, dockerres.LabelWorkspaceInitInstallID,
			labels[dockerres.LabelWorkspaceInitInstallID], b.installID)
	}
	return nil
}

// runWorkspaceInitContainer attaches, starts, feeds the wrapper to stdin and
// collects the exit code plus the combined output.
//
// Attach precedes start for the same reason it does in Launch: output produced
// between the first byte and a post-start attach would be lost, and here that
// output is the entire diagnosis of a failed init.
func (b *containerBackend) runWorkspaceInitContainer(ctx context.Context, id string, req WorkspaceInitRequest) (int, *workspaceInitOutput, error) {
	attachRes, err := b.api.ContainerAttach(ctx, id, client.ContainerAttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		return 0, nil, fmt.Errorf("attach: %w", err)
	}
	hijack := attachRes.HijackedResponse
	defer hijack.Close()

	// No TTY, so the stream carries docker's 8-byte-header framing; the same
	// demux Launch's sessions use (stdout and stderr deliberately combined —
	// §決定8's "TTY/非 TTY とも単一結合").
	outCh := make(chan *workspaceInitOutput, 1)
	go func() {
		out := newWorkspaceInitOutput()
		for {
			chunk, rerr := demuxDockerFrame(hijack.Reader)
			if len(chunk) > 0 {
				_, _ = out.Write(chunk)
			}
			if rerr != nil {
				break
			}
		}
		outCh <- out
	}()

	if _, err := b.api.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		hijack.Close()
		<-outCh
		return 0, nil, fmt.Errorf("start: %w", err)
	}

	// Feed the wrapper, then half-close: `bash -s` reads its whole script from
	// stdin and will not run the last command until it sees EOF.
	if _, err := io.WriteString(hijack.Conn, req.Script); err != nil {
		hijack.Close()
		<-outCh
		return 0, nil, fmt.Errorf("write the init wrapper to the container's stdin: %w", err)
	}
	if err := hijack.CloseWrite(); err != nil {
		slog.Warn("container backend: half-closing the workspace init container's stdin failed",
			"workspace_slug", req.Slug, "error", err)
	}

	waitRes := b.api.ContainerWait(ctx, id, client.ContainerWaitOptions{
		Condition: container.WaitConditionNotRunning,
	})
	var exitCode int
	select {
	case res := <-waitRes.Result:
		// A response is not the same thing as an exit status: see
		// waitResponseEngineError. Failing here rather than trusting
		// res.StatusCode is what keeps RunWorkspaceInit's caller from stamping
		// a completion marker over a home nothing ever prepared.
		if eerr := waitResponseEngineError(res); eerr != nil {
			hijack.Close()
			<-outCh
			return 0, nil, fmt.Errorf(
				"the engine put an error in the wait response for the init container instead of an exit status "+
					"(the container's own exit status is unknown; this is not init.sh failing): %w", eerr)
		}
		exitCode = int(res.StatusCode)
	case werr := <-waitRes.Error:
		hijack.Close()
		<-outCh
		return 0, nil, fmt.Errorf("wait: %w", werr)
	case <-ctx.Done():
		hijack.Close()
		<-outCh
		return 0, nil, ctx.Err()
	}

	// Same drain-then-force-close shape as containerSession.waitLoop: the
	// stream can still be delivering already-produced output for a short
	// window after the wait resolves, and truncating it here would take the
	// tail of exactly the failure being reported.
	var output *workspaceInitOutput
	select {
	case output = <-outCh:
	case <-time.After(attachDrainGracePeriod):
		hijack.Close()
		output = <-outCh
	}
	return exitCode, output, nil
}

// reapOrphanWorkspaceInitContainers collects init containers left behind by a
// crashed daemon (§D5).
//
// Deliberately separate from ReapOrphans' primary loop, and deliberately
// silent in backend.ReapReport. The primary loop enumerates on the PRESENCE of
// boid.job_id and feeds every id it touches into ReapedJobIDs/FailedJobIDs,
// which MarkStale and reapOrphansBeforeReopen use to decide which tasks may
// auto-reopen. An init container has no job, so anything it contributed to
// those lists would be a fabricated job id corrupting unrelated tasks'
// recovery — which is why it carries its own label keys in the first place
// (dockerres.LabelWorkspaceInit) rather than the job ones.
//
// What it collects is decided by workspaceInitContainerIsTerminal, an
// ALLOWLIST. The sweep is for residue that no longer serves any purpose;
// everything else — including states nobody here has heard of — is left where
// it is. See that function for why the direction of the test matters.
func (b *containerBackend) reapOrphanWorkspaceInitContainers(ctx context.Context) {
	listRes, err := b.api.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: client.Filters{}.Add("label", dockerres.LabelWorkspaceInit),
	})
	if err != nil {
		slog.Warn("container backend: list orphan workspace init containers failed", "error", err)
		return
	}
	for _, c := range listRes.Items {
		if b.installID != "" && c.Labels[dockerres.LabelWorkspaceInitInstallID] != b.installID {
			continue
		}
		if !workspaceInitContainerIsTerminal(c.State) {
			slog.Info("container backend: leaving a non-terminal workspace init container alone",
				"container_id", c.ID, "state", string(c.State),
				"workspace_slug", c.Labels[dockerres.LabelWorkspaceInit])
			continue
		}
		if _, rerr := b.api.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true}); rerr != nil {
			// Logged, never folded into ReapReport — see this function's doc
			// comment. A leaked init container costs disk; a fabricated entry
			// in FailedJobIDs costs an unrelated task its auto-reopen.
			slog.Warn("container backend: reap orphan workspace init container failed",
				"container_id", c.ID, "workspace_slug", c.Labels[dockerres.LabelWorkspaceInit], "error", rerr)
		}
	}
}

// workspaceInitContainerIsTerminal reports whether a workspace init container
// in this state has definitively finished, i.e. whether removing it destroys
// nothing but a record.
//
// # Why an allowlist
//
// The first version of the sweep asked the opposite question — it protected
// `running` and `restarting` and removed everything else — and `paused` fell
// straight through it. A paused container is not residue: it is a live process
// tree frozen mid-syscall, with a partially installed toolchain in its
// writable layer, and `ContainerRemove{Force: true}` discards all of it. The
// trigger is a daemon restart; nothing exotic is required. Any denylist has
// the same shape of bug for every state it has not enumerated, and the set is
// not boid's to close: the field is a bare string on the wire, boid talks to
// docker AND podman (whose libpod layer carries states with no docker
// counterpart — "stopping", "configured" — behind a compat shim boid does not
// control), and a value outside every constant below is therefore reachable
// without anything in this repository changing.
//
// Inverting the test makes the unknown case land on the safe side by
// construction. The cost of being wrong in that direction is one leaked
// container record — and even that is bounded, because the deterministic name
// means the NEXT init for the same workspace runs into it and resolves it
// through clearWorkspaceInitContainer, which knows how to wait.
//
// # The set
//
// Enumerated from container.ContainerState in
// github.com/moby/moby/api/types/container/state.go (moby/moby/api v1.55.0) —
// the declared type of the container.Summary.State field this reads —
// alongside container.ValidateContainerState, which is moby's own statement of
// what the complete set is:
//
//   - exited — the process ran and finished. The ordinary case, and what the
//     sweep exists for.
//   - dead — moby's own comment: "the container failed to be deleted.
//     Containers in this state are attempted to be cleaned up when the daemon
//     restarts." There is no process; the record is the whole of it, and
//     retrying the remove is exactly the right response.
//
// Deliberately NOT terminal:
//
//   - running, restarting, paused — a live (or about to be live) process tree,
//     which for this container means a toolchain install in progress.
//   - removing — the engine is already disposing of it. A concurrent force
//     remove races the engine's own bookkeeping to no benefit; the record
//     disappears on its own.
//   - created — the container has never run, so nothing has been lost yet, but
//     it is a PRE-terminal state rather than a finished one: a create that has
//     not yet been followed by its start looks exactly like this. ReapOrphans
//     runs at daemon startup (internal/server/wire.go's
//     reapOrphansBeforeReopen), which makes that window narrow for this
//     daemon's own inits and does not close it for a second boid process on
//     the same engine and install id. The recovery path costs one dispatch's
//     conflict resolution; the failure mode costs an install.
//   - anything else, including the empty string. Podman's docker-compat
//     endpoint does answer in docker's vocabulary for the states that matter
//     here — measured 2026-07-27 against podman 4.9.3's own socket, `GET
//     /v1.41/containers/json?all=true` returning State values of
//     "created"/"exited"/"running" — which is why the allowlist is useful on
//     that engine at all rather than matching nothing. But that measurement
//     covers the states those containers happened to be in, not the shim's
//     whole mapping, so it is a reason to expect the allowlist to fire, never a
//     reason to treat an unrecognized answer as terminal.
func workspaceInitContainerIsTerminal(state container.ContainerState) bool {
	switch state {
	case container.StateExited, container.StateDead:
		return true
	default:
		return false
	}
}
