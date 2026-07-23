package dispatcher

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
	"golang.org/x/sys/unix"

	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
	"github.com/novshi-tech/boid/internal/sandbox/realization"
)

// containerBackend implements backend.SandboxBackend by translating a
// sandbox.Spec (via internal/sandbox/realization's PR3 translation) into a
// docker create/start/attach/wait/kill sequence against a sibling container
// (docker-out-of-docker — docs/plans/phase6-container-backend.md §PR5).
//
// This is the first docker SDK import in boid: github.com/moby/moby/client
// (the Docker Engine API's own standalone Go module — moby/moby split its
// client out of the monolithic github.com/docker/docker tree into
// github.com/moby/moby/client + github.com/moby/moby/api some time before
// this PR; the plan doc's "github.com/docker/docker/client" reference
// predates that split. The new module resolves the same Docker Engine API
// with a much smaller dependency footprint than the old
// github.com/docker/docker/client — see go.mod's diff for the exact set —
// so it satisfies this PR's "docker SDK dependency の minimum セット" mandate
// better than the path named in the plan, not worse).
//
// As of PR5 nothing wires containerBackend into real dispatch — see
// NewContainerBackend's doc comment. config sandbox.backend gating is PR7's
// job (docs/plans/phase6-container-backend.md §PR7 cutover).
type containerBackend struct {
	api          dockerAPI
	defaultImage string
	pullPolicy   ImagePullPolicy
	uid, gid     int
	installID    string
	// diagnosticsCollector, when non-nil, is invoked once a container has
	// exited — after Wait's fan-out has resolved and the attach stream has
	// fully drained (Major 3) — but strictly before the container is
	// removed. See ContainerBackendOptions.DiagnosticsCollector's doc
	// comment.
	diagnosticsCollector func(ctx context.Context, containerID string, exit backend.RuntimeExit)

	mu       sync.Mutex
	sessions map[string]*containerSession
	// adopting tracks in-flight Adopt cache-miss resolutions, keyed by
	// runtimeID, so concurrent Adopt calls for the same runtimeID share one
	// inspect/attach instead of each starting their own (see Adopt's doc
	// comment, PR5 review Major 5).
	adopting map[string]*adoptAttempt
}

var _ backend.SandboxBackend = (*containerBackend)(nil)

// ImagePullPolicy controls when containerBackend.Launch pulls an image
// before creating a container from it (docs/plans/
// phase6-container-backend.md §PR5's "default/pull policy").
type ImagePullPolicy int

const (
	// ImagePullIfNotPresent (the default) pulls only when the image is not
	// already present in the local docker image store.
	ImagePullIfNotPresent ImagePullPolicy = iota
	// ImagePullAlways pulls before every Launch, even when the image is
	// already present locally (picks up a moved tag).
	ImagePullAlways
	// ImagePullNever never pulls; Launch fails if the image is missing
	// locally.
	ImagePullNever
)

// ContainerBackendOptions configures NewContainerBackend. Every field has a
// documented zero-value fallback so `ContainerBackendOptions{}` is a valid
// (if minimal) configuration for tests.
type ContainerBackendOptions struct {
	// DefaultImage is used when a spec carries no ContainerImage override.
	// Empty falls back to defaultContainerImage.
	DefaultImage string
	// PullPolicy controls image pulling (see ImagePullPolicy). Zero value
	// is ImagePullIfNotPresent.
	PullPolicy ImagePullPolicy
	// UID/GID select the `--user <uid>:<gid>` job containers run as (§決定
	// 4 — non-root, matching the image's baked /etc/passwd entry, PR2's
	// Dockerfile). nil means "unset". A custom pair is only honored when
	// BOTH are provided (non-nil) AND both resolve to non-zero — anything
	// else (both unset, only one set, or either resolving to 0) falls back
	// to 1000:1000 (the PR2 image's default BOID_UID/BOID_GID build args)
	// rather than silently running the job as root. This is nullable
	// (*int, not int) specifically so "unset" and "explicitly 0" are
	// distinguishable: an int-typed field couldn't tell `UID: 0` (meant as
	// "use the default") apart from a caller who actually passed 0, which
	// let a partial override like `UID: 0, GID: 1000` slip through as a
	// root container (fixed — see the PR5 review's Major 1). A real UID 0
	// override is never a use case this backend supports (決定 4 requires
	// non-root).
	UID, GID *int
	// InstallID is the value stamped on every container's boid.install_id
	// label (§決定 6). Empty is valid — install_id generation lands in PR6
	// (~/.local/share/boid/install_id LoadOrCreate); PR5's ReapOrphans uses
	// a global (not install_id-scoped) label filter until then, per the
	// plan doc's PR5 TODO note.
	InstallID string
	// DiagnosticsCollector, when set, is called exactly once per exited
	// container — after containerSession.waitLoop finalizes exit state and
	// unblocks every Wait() caller, but strictly before the container (and
	// its volumes) are removed. This is the hook §決定 7's "診断回収 →
	// job fallback 処理 → resource remove" ordering contract requires: the
	// pre-fix waitLoop called close(s.done) and then immediately removed
	// the container in the same goroutine, racing ahead of any diagnostic
	// work a woken Wait() caller might still need to do against the live
	// container (e.g. a `docker inspect` for OOM/exit-reason before it's
	// gone — 決定 8's silent-exit classification, PR7's job). PR5 leaves
	// this nil (no consumer yet — see NewContainerBackend's doc comment on
	// production wiring); ContainerRemove is unconditionally sequenced
	// after it returns so a future collector can never lose its window.
	DiagnosticsCollector func(ctx context.Context, containerID string, exit backend.RuntimeExit)
}

const (
	defaultContainerImage = "boid-runner:latest"
	defaultContainerUID   = 1000
	defaultContainerGID   = 1000
	// defaultPidsLimit is the fork-bomb-safety default the scope note
	// allows as an "implementation-time optional" item (docs/plans/
	// phase6-container-backend.md スコープ節 — full cgroup vocabulary is
	// Phase 7, but a PidsLimit default is explicitly permitted now).
	defaultPidsLimit int64 = 512

	// attachDrainGracePeriod bounds how long containerSession.waitLoop
	// waits for readLoop to drain the attach connection naturally (the
	// daemon closes the stream once the container's own stdout/stderr
	// pipes are fully flushed) before force-closing the connection itself.
	// A container's output can still be arriving on the attach stream for
	// a short window after ContainerWait resolves — closing immediately,
	// as PR5 originally did, could truncate a final burst of output
	// emitted right at exit (PR5 review Major 3).
	attachDrainGracePeriod = 500 * time.Millisecond

	// containerSpecPath / containerStatePath are the fixed sandbox-internal
	// paths the sandbox JSON spec / runner-state.json diagnostic file are
	// bind-mounted at — the container-backend analogue of the userns
	// backend's `--spec`/`--state` CLI flags pointing at host paths
	// runner-outer reads directly (userns has no such mount because it
	// shares the host mount namespace before pivot_root; a sibling
	// container needs an explicit bind). `boid runner-container`
	// (cmd/runner_container.go, PR2) is invoked with `--spec
	// containerSpecPath --state containerStatePath` as its Cmd (the image's
	// ENTRYPOINT is already `["/usr/local/bin/boid","runner-container"]` —
	// see build/container/Dockerfile — so Cmd carries only the trailing
	// flags, not the agent's own argv; spec.Argv travels inside the spec
	// JSON itself, read back by RunContainer, exactly like the userns path).
	containerSpecPath  = "/run/boid/spec.json"
	containerStatePath = "/run/boid/state.json"

	// Resource labels (§決定 6/9): boid.job_id + boid.workspace are always
	// set; boid.install_id is set whenever ContainerBackendOptions.InstallID
	// is non-empty (PR6 territory — see its doc comment). ReapOrphans (§決定
	// 6) filters on the mere presence of boid.job_id ("global filter") since
	// install_id-scoped filtering needs PR6's install_id generation.
	labelJobID     = "boid.job_id"
	labelWorkspace = "boid.workspace"
	labelInstallID = "boid.install_id"

	// boidRunnerProtocolLabel / boidRunnerProtocolVersion gate workspace
	// image overrides (§決定 11): an override image must carry this label
	// with this exact value, proving it derives from the shared boid base
	// image (§決定 2), before containerBackend.Launch will use it. Nothing
	// bakes this label into build/container/Dockerfile yet (that lands
	// alongside the real image-provenance work in PR6/PR7 — see the plan
	// doc's PR5 TODO note); until then every real override is rejected,
	// which is safe because containerBackend is not wired into production
	// dispatch as of PR5.
	boidRunnerProtocolLabel   = "boid.runner_protocol"
	boidRunnerProtocolVersion = "v1"
)

// NewContainerBackend constructs a containerBackend over api (typically a
// real *github.com/moby/moby/client.Client — see dockerAPI's doc comment for
// why the parameter is this narrower interface rather than that concrete
// type — or a fake for tests).
//
// Nothing in production dispatch calls this as of PR5 (docs/plans/
// phase6-container-backend.md §PR5: "内部フラグ/テスト専用で... 確認する
// (config 未公開)"). The seam this backend is exercised through is
// Runner.Backend (internal/dispatcher/runner.go's DI override, landed PR1)
// — a test (or, later, a hidden CLI flag / PR7's config-gated production
// wiring) constructs a containerBackend via this function and assigns it to
// Runner.Backend directly. sandbox.Backend config parsing/gating into that
// seam is explicitly out of PR5's scope (PR7 cutover).
func NewContainerBackend(api dockerAPI, opts ContainerBackendOptions) backend.SandboxBackend {
	b := &containerBackend{
		api:                  api,
		defaultImage:         opts.DefaultImage,
		pullPolicy:           opts.PullPolicy,
		installID:            opts.InstallID,
		diagnosticsCollector: opts.DiagnosticsCollector,
		sessions:             make(map[string]*containerSession),
	}
	if b.defaultImage == "" {
		b.defaultImage = defaultContainerImage
	}
	b.uid, b.gid = defaultContainerUID, defaultContainerGID
	switch {
	case opts.UID != nil && opts.GID != nil && *opts.UID != 0 && *opts.GID != 0:
		b.uid, b.gid = *opts.UID, *opts.GID
	case opts.UID != nil || opts.GID != nil:
		// A partial override (only one of the two set) or a pair that
		// resolves to root (either side == 0) is rejected in favor of the
		// non-root default — see ContainerBackendOptions.UID's doc comment
		// and the PR5 review's Major 1.
		slog.Warn("container backend: rejecting partial or root-resolving uid/gid override; using default (§決定 4 requires non-root)",
			"uid", formatIntPtr(opts.UID), "gid", formatIntPtr(opts.GID),
			"default_uid", defaultContainerUID, "default_gid", defaultContainerGID)
	}
	return b
}

// formatIntPtr renders a *int for logging: "<unset>" for nil, the decimal
// value otherwise. Used by NewContainerBackend's uid/gid rejection warning
// so the log line shows the caller's actual (possibly nil) input rather
// than a raw pointer address.
func formatIntPtr(p *int) string {
	if p == nil {
		return "<unset>"
	}
	return strconv.Itoa(*p)
}

// dockerAPI is the narrow, containerBackend-owned subset of the docker
// Engine API this file actually calls — structurally satisfied by
// *github.com/moby/moby/client.Client (whose method set is the union of
// client.ContainerAPIClient + client.ImageAPIClient + client.NetworkAPIClient
// + client.VolumeAPIClient, a strict superset of this interface) with no
// wrapping required, and trivially fake-able for unit tests without stubbing
// the dozens of methods those full SDK interfaces carry (ContainerCommit,
// ContainerExport, ContainerStats, image save/load, volume update, ...) that
// containerBackend never calls. This is the standard Go "accept a small
// interface, not the SDK's big one" idiom — a fake docker client written
// against this interface is, by construction, also a fake of whichever of
// the SDK's own *APIClient interfaces callers might have expected, just
// without the unused-method boilerplate.
type dockerAPI interface {
	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerStart(ctx context.Context, containerID string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerInspect(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerAttach(ctx context.Context, containerID string, options client.ContainerAttachOptions) (client.ContainerAttachResult, error)
	ContainerWait(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult
	ContainerKill(ctx context.Context, containerID string, options client.ContainerKillOptions) (client.ContainerKillResult, error)
	ContainerStop(ctx context.Context, containerID string, options client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerResize(ctx context.Context, containerID string, options client.ContainerResizeOptions) (client.ContainerResizeResult, error)
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)

	ImageInspect(ctx context.Context, image string, opts ...client.ImageInspectOption) (client.ImageInspectResult, error)
	ImagePull(ctx context.Context, ref string, options client.ImagePullOptions) (client.ImagePullResponse, error)

	NetworkList(ctx context.Context, options client.NetworkListOptions) (client.NetworkListResult, error)
	NetworkRemove(ctx context.Context, networkID string, options client.NetworkRemoveOptions) (client.NetworkRemoveResult, error)

	VolumeCreate(ctx context.Context, options client.VolumeCreateOptions) (client.VolumeCreateResult, error)
	VolumeList(ctx context.Context, options client.VolumeListOptions) (client.VolumeListResult, error)
	VolumeRemove(ctx context.Context, volumeID string, options client.VolumeRemoveOptions) (client.VolumeRemoveResult, error)
}

// Launch translates spec into a `docker create` + `docker start` call and
// returns a live containerSession attached to it.
//
// Ordering matters for two independent reasons pinned by the plan doc:
//   - attach happens BEFORE start (not after), so no output between the
//     entry process's first byte and a post-start attach race is lost.
//   - HostConfig.Init is always set (§決定 3): docker-init (tini) becomes
//     PID 1, owning zombie reap; SIGUSR1→agent forwarding is already
//     handled by the harness adapters' own sigutil.ForwardAndWait once a
//     signal reaches the entrypoint process, so nothing new is embedded
//     here for that.
func (b *containerBackend) Launch(ctx context.Context, spec sandbox.Spec, opts backend.LaunchOptions) (backend.SandboxSession, error) {
	specPath, statePath, err := writeContainerSpec(spec)
	if err != nil {
		return nil, fmt.Errorf("write container sandbox spec: %w", err)
	}
	cleanupFiles := func() {
		_ = os.Remove(specPath)
		_ = os.Remove(statePath)
	}

	realized, err := realization.Realize(spec)
	if err != nil {
		cleanupFiles()
		return nil, fmt.Errorf("realize sandbox spec: %w", err)
	}

	image, err := b.resolveImage(ctx, spec.ContainerImage)
	if err != nil {
		cleanupFiles()
		return nil, err
	}

	labels := map[string]string{labelJobID: opts.JobID}
	if b.installID != "" {
		labels[labelInstallID] = b.installID
	}
	if ws := realized.Env["BOID_WORKSPACE_SLUG"]; ws != "" {
		labels[labelWorkspace] = ws
	}

	mounts, namedVolumes := containerMounts(realized)
	if err := b.ensureNamedVolumes(ctx, namedVolumes, labels); err != nil {
		cleanupFiles()
		return nil, err
	}
	mounts = append(mounts,
		mount.Mount{Type: mount.TypeBind, Source: specPath, Target: containerSpecPath, ReadOnly: true},
		mount.Mount{Type: mount.TypeBind, Source: statePath, Target: containerStatePath},
	)

	initTrue := true
	pidsLimit := defaultPidsLimit
	hostCfg := &container.HostConfig{
		Init:   &initTrue,
		Mounts: mounts,
	}
	hostCfg.Resources.PidsLimit = &pidsLimit

	cfg := &container.Config{
		Image: image,
		// The entrypoint (build/container/Dockerfile's ENTRYPOINT) is
		// already `/usr/local/bin/boid runner-container`; Cmd carries only
		// its trailing flags. The agent's own argv (spec.Argv) is NOT
		// threaded here — it travels inside the spec JSON bind-mounted at
		// containerSpecPath, exactly like the userns backend's
		// runner-outer/-inner/-inner-child chain reads it back from disk
		// rather than from its own argv.
		Cmd:          []string{"--spec", containerSpecPath, "--state", containerStatePath},
		Env:          envSlice(realized.Env),
		WorkingDir:   realized.Workdir,
		Tty:          realized.TTY,
		OpenStdin:    true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		User:         fmt.Sprintf("%d:%d", b.uid, b.gid),
		Labels:       labels,
	}

	createRes, err := b.api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     cfg,
		HostConfig: hostCfg,
		Name:       containerName(opts.JobID),
	})
	if err != nil {
		cleanupFiles()
		return nil, fmt.Errorf("container create: %w", err)
	}

	sess := newContainerSession(b, createRes.ID, realized.TTY, specPath)
	if err := sess.attach(ctx, false); err != nil {
		_, _ = b.api.ContainerRemove(context.Background(), createRes.ID, client.ContainerRemoveOptions{Force: true})
		cleanupFiles()
		return nil, fmt.Errorf("container attach: %w", err)
	}

	if _, err := b.api.ContainerStart(ctx, createRes.ID, client.ContainerStartOptions{}); err != nil {
		sess.closeConn()
		_, _ = b.api.ContainerRemove(context.Background(), createRes.ID, client.ContainerRemoveOptions{Force: true})
		cleanupFiles()
		return nil, fmt.Errorf("container start: %w", err)
	}

	sess.start()
	b.registerSession(sess)
	return sess, nil
}

// Adopt reconstructs (or returns the already-cached) SandboxSession for a
// runtimeID (= container ID). Unlike the userns backend — whose Adopt is a
// cheap per-call wrapper because LocalRuntime itself owns the single
// long-lived attach/fan-out state (see usernsSession's doc comment) —
// containerBackend must cache sessions itself: repeated Adopt calls for the
// same runtimeID (WS attach and the Web UI SSE follow endpoint can both
// Adopt the same runtimeID concurrently, docs/plans/
// phase6-container-backend.md 現状棚卸し) must share one docker-attach
// connection and one fan-out, not open a second independent attach each —
// the cache below (populated by both Launch and this method) is what makes
// that true.
//
// A cache miss (nothing in-process remembers runtimeID — the common case
// right after a daemon restart, which is Adopt's entire reason for existing)
// falls back to `docker inspect`: if the container exists and is running, a
// fresh session is attached (with Logs:true, replaying already-produced
// output as the fan-out's initial buffer — the closest containerBackend
// gets to a separate `docker logs` call, decision 8's third primitive) and
// its own single-owner Wait loop is started, exactly as Launch does.
//
// Concurrent cache misses for the SAME runtimeID (WS attach and the Web UI
// SSE follow endpoint racing right after a daemon restart, before either
// has populated the cache) are serialized through the adopting map below:
// the first caller to observe a miss reserves an in-flight adoptAttempt
// under the lock and does the inspect/attach/start work alone; every other
// concurrent caller for that same runtimeID finds the reservation, releases
// the lock, and blocks on the attempt's done channel instead of starting
// its own independent inspect/attach — otherwise two attach calls would
// each start their own ContainerWait owner, breaking §決定 7's
// single-owner contract (PR5 review Major 5).
func (b *containerBackend) Adopt(ctx context.Context, runtimeID string) (backend.SandboxSession, bool) {
	if runtimeID == "" {
		return nil, false
	}

	b.mu.Lock()
	if sess, ok := b.sessions[runtimeID]; ok {
		b.mu.Unlock()
		return sess, true
	}
	if attempt, inFlight := b.adopting[runtimeID]; inFlight {
		b.mu.Unlock()
		<-attempt.done
		if attempt.session == nil {
			return nil, false
		}
		return attempt.session, true
	}
	attempt := &adoptAttempt{done: make(chan struct{})}
	if b.adopting == nil {
		b.adopting = make(map[string]*adoptAttempt)
	}
	b.adopting[runtimeID] = attempt
	b.mu.Unlock()

	sess := b.doAdopt(ctx, runtimeID)

	b.mu.Lock()
	delete(b.adopting, runtimeID)
	if sess != nil {
		if b.sessions == nil {
			b.sessions = make(map[string]*containerSession)
		}
		b.sessions[runtimeID] = sess
	}
	b.mu.Unlock()

	attempt.session = sess
	close(attempt.done)

	if sess == nil {
		return nil, false
	}
	return sess, true
}

// adoptAttempt tracks a single in-flight Adopt cache-miss resolution so
// concurrent callers for the same runtimeID share its outcome instead of
// each starting their own inspect/attach (see Adopt's doc comment). session
// is only safe to read after done is closed.
type adoptAttempt struct {
	done    chan struct{}
	session *containerSession
}

// doAdopt performs the actual `docker inspect` + attach + start sequence
// for a runtimeID Adopt found neither cached nor already in flight. Returns
// nil when the container cannot be adopted (inspect failed, or the
// container exists but isn't running).
func (b *containerBackend) doAdopt(ctx context.Context, runtimeID string) *containerSession {
	insp, err := b.api.ContainerInspect(ctx, runtimeID, client.ContainerInspectOptions{})
	if err != nil || insp.Container.State == nil || !insp.Container.State.Running {
		return nil
	}

	tty := insp.Container.Config != nil && insp.Container.Config.Tty
	sess := newContainerSession(b, runtimeID, tty, "")
	if err := sess.attach(ctx, true); err != nil {
		slog.Warn("container backend: adopt attach failed; session will support signal/stop/wait only",
			"container_id", runtimeID, "error", err)
	}
	sess.start()
	return sess
}

// ReapOrphans reconciles job containers a daemon restart lost track of.
// §決定 6: label enumeration → destroy, using the mere presence of
// boid.job_id as the filter ("global filter") rather than an
// install_id-scoped one — install_id generation is PR6's job (see
// ContainerBackendOptions.InstallID's doc comment); until it lands every
// container this backend ever created is a fair reap target. Volumes and
// networks are reaped best-effort by the same label — nothing in PR5
// creates job-labeled volumes/networks yet (workspace HOME stays a host
// bind through Phase 6, §決定 4; workspace networks are PR6), so these two
// loops are forward-compat scaffolding, not exercised by real traffic yet.
func (b *containerBackend) ReapOrphans(ctx context.Context) (backend.ReapReport, error) {
	filters := client.Filters{}.Add("label", labelJobID)

	listRes, err := b.api.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		wrapped := fmt.Errorf("list orphan containers: %w", err)
		return backend.ReapReport{GlobalError: wrapped}, wrapped
	}

	report := backend.ReapReport{}
	for _, c := range listRes.Items {
		jobID := c.Labels[labelJobID]
		b.forgetSession(c.ID)
		if _, err := b.api.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
			slog.Warn("container backend: reap orphan container failed", "container_id", c.ID, "job_id", jobID, "error", err)
			if jobID != "" {
				report.FailedJobIDs = append(report.FailedJobIDs, jobID)
			}
			continue
		}
		if jobID != "" {
			report.ReapedJobIDs = append(report.ReapedJobIDs, jobID)
		}
	}

	b.reapOrphanVolumes(ctx, filters)
	b.reapOrphanNetworks(ctx, filters)

	return report, nil
}

func (b *containerBackend) reapOrphanVolumes(ctx context.Context, filters client.Filters) {
	listRes, err := b.api.VolumeList(ctx, client.VolumeListOptions{Filters: filters})
	if err != nil {
		slog.Warn("container backend: list orphan volumes failed", "error", err)
		return
	}
	for _, v := range listRes.Items {
		if _, err := b.api.VolumeRemove(ctx, v.Name, client.VolumeRemoveOptions{Force: true}); err != nil {
			slog.Warn("container backend: reap orphan volume failed", "volume", v.Name, "error", err)
		}
	}
}

func (b *containerBackend) reapOrphanNetworks(ctx context.Context, filters client.Filters) {
	listRes, err := b.api.NetworkList(ctx, client.NetworkListOptions{Filters: filters})
	if err != nil {
		slog.Warn("container backend: list orphan networks failed", "error", err)
		return
	}
	for _, n := range listRes.Items {
		if _, err := b.api.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{}); err != nil {
			slog.Warn("container backend: reap orphan network failed", "network", n.ID, "error", err)
		}
	}
}

func (b *containerBackend) registerSession(sess *containerSession) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sessions == nil {
		b.sessions = make(map[string]*containerSession)
	}
	b.sessions[sess.id] = sess
}

func (b *containerBackend) forgetSession(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.sessions, id)
}

// resolveImage picks the image Launch creates the container from (spec's
// override or the backend's default) and enforces both the pull policy and
// (for an override only) §決定 11's boid-base-derived label check. A single
// ImageInspect call serves both the presence check most pull-policy
// branches need and the label read the override check needs — reused
// rather than inspecting twice.
func (b *containerBackend) resolveImage(ctx context.Context, override string) (string, error) {
	image := b.defaultImage
	if override != "" {
		image = override
	}

	insp, err := b.api.ImageInspect(ctx, image)
	if err != nil {
		if b.pullPolicy == ImagePullNever {
			return "", fmt.Errorf("container image %q not present locally (pull policy: never): %w", image, err)
		}
		if pullErr := b.pullImage(ctx, image); pullErr != nil {
			return "", pullErr
		}
		insp, err = b.api.ImageInspect(ctx, image)
		if err != nil {
			return "", fmt.Errorf("inspect container image %q after pull: %w", image, err)
		}
	} else if b.pullPolicy == ImagePullAlways {
		if pullErr := b.pullImage(ctx, image); pullErr != nil {
			return "", pullErr
		}
		// Re-inspect after pulling: a pull can replace the local image
		// (e.g. a moved tag), so the ImageInspect result from the
		// presence check above would otherwise validate stale metadata —
		// in particular the boidRunnerProtocolLabel check below, which
		// must see what was actually just pulled, not what was locally
		// present before the pull (PR5 review Major 2).
		insp, err = b.api.ImageInspect(ctx, image)
		if err != nil {
			return "", fmt.Errorf("inspect container image %q after pull: %w", image, err)
		}
	}

	if override != "" {
		got := ""
		if insp.Config != nil {
			got = insp.Config.Labels[boidRunnerProtocolLabel]
		}
		if got != boidRunnerProtocolVersion {
			return "", fmt.Errorf(
				"container image override %q rejected: %s label = %q, want %q (workspace override images must derive from the boid base image — §決定 11)",
				override, boidRunnerProtocolLabel, got, boidRunnerProtocolVersion)
		}
	}
	return image, nil
}

func (b *containerBackend) pullImage(ctx context.Context, ref string) error {
	resp, err := b.api.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull container image %q: %w", ref, err)
	}
	defer resp.Close()
	if err := resp.Wait(ctx); err != nil {
		return fmt.Errorf("pull container image %q: %w", ref, err)
	}
	return nil
}

// writeContainerSpec writes spec's JSON and an empty runner-state.json to
// host paths using the exact same `/tmp/boid-<ID>-runner-{spec,state}.json`
// naming convention dispatcher.sandboxPreparerImpl.PrepareSandbox uses for
// the userns backend (see its own doc comment). Deliberately does NOT call
// that preparer: it also allocates spec.RootDir (a tmpfs mount point for
// userns pivot_root) which a container backend has no use for — the
// container's own image rootfs is the sandbox root. Reusing the naming
// convention rather than inventing a new one means the existing
// `/tmp/boid-*` 30-day GC sweep (CLAUDE.md「ディスク使用量の管理」) covers
// container-backend leftovers with no new GC code.
//
// statePath is created empty (not just planned) up front because it is
// bind-mounted into the container as a single file: docker's bind-mount
// setup does not create a missing host **file** path the way it can create
// a missing directory, so the target must already exist before
// ContainerCreate runs.
func writeContainerSpec(spec sandbox.Spec) (specPath, statePath string, err error) {
	specPath = fmt.Sprintf("/tmp/boid-%s-runner-spec.json", spec.ID)
	statePath = fmt.Sprintf("/tmp/boid-%s-runner-state.json", spec.ID)

	data, err := json.Marshal(spec)
	if err != nil {
		return "", "", fmt.Errorf("marshal sandbox spec: %w", err)
	}
	// 0600: the spec carries the broker token and any project secrets in Env
	// (same rationale as sandboxPreparerImpl.PrepareSandbox).
	if err := os.WriteFile(specPath, data, 0o600); err != nil {
		return "", "", fmt.Errorf("write sandbox spec: %w", err)
	}
	if err := os.WriteFile(statePath, nil, 0o600); err != nil {
		_ = os.Remove(specPath)
		return "", "", fmt.Errorf("create runner state file: %w", err)
	}
	return specPath, statePath, nil
}

// containerMounts translates a realization.Realization's Volumes/Tmpfs into
// docker `Mounts` entries, applying the host-side Guard evaluation
// realization.VolumeMount/TmpfsMount's doc comments require of the
// container backend (Realize deliberately does not evaluate Guard itself —
// see its own doc comment on why). MountSourceContainerLocal entries are
// skipped entirely: they have no host-side counterpart to bind (§決定 4 —
// `/workspace/<name>` lands in the container's own writable layer).
//
// namedVolumes returns the distinct MountSourceNamedVolume source names
// among the mounts that passed their Guard, so Launch can pre-create them
// (with reap labels) before ContainerCreate implicitly references them —
// see ensureNamedVolumes's doc comment (PR5 review Major 6).
func containerMounts(r realization.Realization) (mounts []mount.Mount, namedVolumes []string) {
	for _, v := range r.Volumes {
		if v.Guard != "" && !evaluateMountGuard(v.Guard) {
			continue
		}
		switch v.Source.Kind {
		case realization.MountSourceHostPath:
			mounts = append(mounts, mount.Mount{
				Type:     mount.TypeBind,
				Source:   v.Source.Value,
				Target:   v.Target,
				ReadOnly: v.ReadOnly,
			})
		case realization.MountSourceNamedVolume:
			mounts = append(mounts, mount.Mount{
				Type:     mount.TypeVolume,
				Source:   v.Source.Value,
				Target:   v.Target,
				ReadOnly: v.ReadOnly,
			})
			namedVolumes = append(namedVolumes, v.Source.Value)
		case realization.MountSourceContainerLocal:
			// No host-side counterpart; nothing to add.
		}
	}
	for _, t := range r.Tmpfs {
		if t.Guard != "" && !evaluateMountGuard(t.Guard) {
			continue
		}
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeTmpfs,
			Target:   t.Target,
			ReadOnly: t.ReadOnly,
		})
	}
	return mounts, namedVolumes
}

// ensureNamedVolumes explicitly creates every named volume Launch's mounts
// reference, carrying the same job/install/workspace labels the container
// itself gets. Docker auto-creates a missing named volume the first time a
// container references it, but that auto-created volume gets NO labels —
// and ReapOrphans's volume sweep (reapOrphanVolumes) only finds
// labelJobID-labeled volumes, so an auto-created volume would silently
// never be reaped (PR5 review Major 6).
//
// VolumeCreate is idempotent (Docker returns the existing volume, unchanged,
// for an already-existing name — it does not error, and it does NOT apply
// the request's labels to an existing volume, since the API has no
// volume-label-update endpoint), so this is safe to call on every Launch.
// An already-existing volume that predates this fix (no boid.job_id label)
// is left as-is rather than deleted-and-recreated, which would be
// destructive to whatever it holds; a warning is logged instead so the
// reap gap is at least visible.
func (b *containerBackend) ensureNamedVolumes(ctx context.Context, names []string, labels map[string]string) error {
	for _, name := range names {
		res, err := b.api.VolumeCreate(ctx, client.VolumeCreateOptions{Name: name, Labels: labels})
		if err != nil {
			return fmt.Errorf("create named volume %q: %w", name, err)
		}
		if res.Volume.Labels[labelJobID] == "" {
			slog.Warn("container backend: named volume exists without a boid.job_id label; ReapOrphans's volume sweep will not find it",
				"volume", name)
		}
	}
	return nil
}

// evaluateMountGuard evaluates a sandbox.Mount.Guard expression on the host
// side, since docker has no equivalent of the userns runner's generated
// `if [ <guard> ]; then mount ...; fi` shell idiom (realization.
// VolumeMount.Guard's doc comment). Rather than embedding a shell
// interpreter, this parses the two fixed shapes dispatcher's own
// dirGuardExpr/existsGuardExpr generators ever produce — "-d '<path>'" or
// "-e '<path>'", i.e. a `[ -d ... ]` / `[ -e ... ]` test — and stats the
// host path directly. Any other shape fails closed (mount skipped, warning
// logged): silently mounting something the userns backend would have
// skipped is a behavior divergence this backend must not introduce.
func evaluateMountGuard(guard string) bool {
	flag, quoted, ok := strings.Cut(guard, " ")
	if !ok {
		slog.Warn("container backend: unrecognized mount guard shape; skipping mount", "guard", guard)
		return false
	}
	path := unquoteShellArg(quoted)
	info, err := os.Stat(path)
	switch flag {
	case "-d":
		return err == nil && info.IsDir()
	case "-e":
		return err == nil
	default:
		slog.Warn("container backend: unrecognized mount guard flag; skipping mount", "guard", guard)
		return false
	}
}

// unquoteShellArg reverses dispatcher.shellQuoteDir's single-quoting
// ("'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'") for the one shape
// evaluateMountGuard needs to parse back out. Returns s unchanged if it is
// not single-quoted (defensive; every real Guard value is, per
// dirGuardExpr/existsGuardExpr).
func unquoteShellArg(s string) string {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		inner := s[1 : len(s)-1]
		return strings.ReplaceAll(inner, `'"'"'`, "'")
	}
	return s
}

func envSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(env))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

func containerName(jobID string) string {
	if jobID == "" {
		return ""
	}
	return "boid-job-" + jobID
}

// containerSession implements backend.SandboxSession over a single docker
// container: one docker-attach connection feeding an in-memory transcript
// buffer + multi-subscriber fan-out (§決定 8/9's "1 attach 所有者 + memory
// buffer + fan-out" core — modeled directly on localRuntimeSession's
// readLoop/appendTranscript/subscribe in runtime_local_linux.go, the
// existing session layer §決定 8 calls out to extract and reuse rather than
// redesign), and one ContainerWait call feeding a `done` channel every
// Wait() caller selects on (§決定 7's "backend 内で一度だけ wait して exit
// future を fan-out").
//
// Full disk-spool persistence of the transcript (so `boid job log` survives
// container remove) is explicitly deferred to PR7 (docs/plans/
// phase6-container-backend.md §決定 8: "PR5 では transcript spool の実装は
// skeleton まで OK") — the in-memory buffer here satisfies live
// Subscribe/snapshot semantics for the lifetime of the containerBackend
// process but is not written to the runtime dir the way
// localRuntimeSession's transcriptFile is.
type containerSession struct {
	backend *containerBackend
	id      string
	api     dockerAPI
	tty     bool

	// specPath is removed unconditionally once the container exits (it
	// carries secrets — same retention contract as cleanupSandboxSpec for
	// the userns path: the spec is always deleted, runner-state.json is
	// retained for post-hoc diagnosis). Empty for Adopt-reconstructed
	// sessions, which never wrote one (mirrors usernsSession.prepared being
	// nil for Adopt — see sessionLocalArtifacts's doc comment).
	specPath string

	connMu         sync.Mutex
	hijack         *client.HijackedResponse
	stdinCloseOnce sync.Once

	mu          sync.Mutex
	transcript  []byte
	subscribers map[int]chan []byte
	nextSubID   int
	running     bool
	exit        backend.RuntimeExit

	done     chan struct{}
	readDone chan struct{}
}

var _ backend.SandboxSession = (*containerSession)(nil)

func newContainerSession(b *containerBackend, id string, tty bool, specPath string) *containerSession {
	return &containerSession{
		backend:     b,
		id:          id,
		api:         b.api,
		tty:         tty,
		specPath:    specPath,
		subscribers: make(map[int]chan []byte),
		running:     true,
		done:        make(chan struct{}),
		readDone:    make(chan struct{}),
	}
}

func (s *containerSession) ID() string { return s.id }

// attach establishes the session's single docker-attach connection and
// starts the read loop that feeds appendTranscript. withLogs replays
// already-produced output before switching to the live stream — Adopt's
// post-restart recovery path (the closest this backend gets to a separate
// `docker logs` call); Launch passes false since nothing has been produced
// yet at create time.
func (s *containerSession) attach(ctx context.Context, withLogs bool) error {
	result, err := s.api.ContainerAttach(ctx, s.id, client.ContainerAttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
		Logs:   withLogs,
	})
	if err != nil {
		close(s.readDone)
		return err
	}
	hijack := result.HijackedResponse
	s.connMu.Lock()
	s.hijack = &hijack
	s.connMu.Unlock()
	go s.readLoop()
	return nil
}

func (s *containerSession) closeConn() {
	s.connMu.Lock()
	hijack := s.hijack
	s.connMu.Unlock()
	if hijack != nil {
		hijack.Close()
	}
}

// start kicks off the session's single ContainerWait owner (§決定 7).
func (s *containerSession) start() {
	go s.waitLoop()
}

// readLoop is the session's one and only reader of the attach connection.
// Non-TTY containers multiplex stdout/stderr with docker's 8-byte-header
// framing (demuxDockerFrame); both streams are combined into a single
// transcript exactly like the userns backend's combined pipe (§決定 8:
// "TTY/非 TTY とも単一結合で stdout/stderr 分離は意図的に無い").
func (s *containerSession) readLoop() {
	defer close(s.readDone)

	s.connMu.Lock()
	hijack := s.hijack
	s.connMu.Unlock()
	if hijack == nil {
		return
	}

	if s.tty {
		buf := make([]byte, 4096)
		for {
			n, err := hijack.Reader.Read(buf)
			if n > 0 {
				s.appendTranscript(append([]byte(nil), buf[:n]...))
			}
			if err != nil {
				return
			}
		}
	}

	for {
		chunk, err := demuxDockerFrame(hijack.Reader)
		if len(chunk) > 0 {
			s.appendTranscript(chunk)
		}
		if err != nil {
			return
		}
	}
}

// demuxDockerFrame reads one frame of docker's non-TTY attach multiplexed
// stream format: an 8-byte header (byte 0 = stream type [stdout/stderr],
// bytes 1-3 reserved, bytes 4-7 = big-endian uint32 payload size) followed
// by that many payload bytes. This is a small, stable, publicly documented
// wire format (the same one github.com/moby/moby/pkg/stdcopy implements) —
// reimplemented directly here rather than importing that package, which
// lives in the full github.com/moby/moby module and would drag in far more
// than this PR's minimum-dependency mandate allows for ~15 lines of framing
// logic.
func demuxDockerFrame(r *bufio.Reader) ([]byte, error) {
	header := make([]byte, 8)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[4:8])
	if size == 0 {
		return nil, nil
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func (s *containerSession) appendTranscript(chunk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transcript = append(s.transcript, chunk...)
	for id, ch := range s.subscribers {
		copyChunk := append([]byte(nil), chunk...)
		select {
		case ch <- copyChunk:
		default:
			close(ch)
			delete(s.subscribers, id)
		}
	}
}

// Subscribe mirrors LocalRuntime.SubscribeRuntime's contract exactly
// (including its not-obviously-symmetric ok=false case): a snapshot is
// always returned, even when the session has already exited — a late
// connect after exit still gets the final transcript — but ok is false and
// no channel/cancel is handed back so callers don't wait for output that
// will never arrive.
func (s *containerSession) Subscribe() ([]byte, <-chan []byte, func(), bool) {
	s.mu.Lock()
	snapshot := append([]byte(nil), s.transcript...)
	running := s.running
	var subID int
	var ch chan []byte
	if running {
		subID = s.nextSubID
		s.nextSubID++
		ch = make(chan []byte, 64)
		s.subscribers[subID] = ch
	}
	s.mu.Unlock()

	if !running {
		return snapshot, nil, func() {}, false
	}
	return snapshot, ch, func() { s.unsubscribe(subID) }, true
}

func (s *containerSession) unsubscribe(subID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.subscribers[subID]; ok {
		close(ch)
		delete(s.subscribers, subID)
	}
}

func (s *containerSession) closeSubscribersLocked() {
	for id, ch := range s.subscribers {
		close(ch)
		delete(s.subscribers, id)
	}
}

func (s *containerSession) WriteInput(data []byte) error {
	s.connMu.Lock()
	hijack := s.hijack
	s.connMu.Unlock()
	if hijack == nil {
		return ErrRuntimeUnsupported
	}
	_, err := hijack.Conn.Write(data)
	return err
}

// CloseInput half-closes the attach connection's write side exactly once
// (HijackedResponse.CloseWrite — a no-op, not an error, when the
// underlying net.Conn doesn't support half-close, matching that method's
// own documented fallback). This does not close the output stream (current
// contract, preserved as-is — same as the userns backend's
// LocalRuntime.CloseInputRuntime).
func (s *containerSession) CloseInput() error {
	s.stdinCloseOnce.Do(func() {
		s.connMu.Lock()
		hijack := s.hijack
		s.connMu.Unlock()
		if hijack == nil {
			return
		}
		_ = hijack.CloseWrite()
	})
	return nil
}

func (s *containerSession) Resize(size backend.TerminalSize) error {
	if size.Rows <= 0 || size.Cols <= 0 {
		return nil
	}
	_, err := s.api.ContainerResize(context.Background(), s.id, client.ContainerResizeOptions{
		Height: uint(size.Rows),
		Width:  uint(size.Cols),
	})
	return err
}

// Wait blocks until the session's single waitLoop (started once, by
// Launch/Adopt's call to start()) observes container exit and closes done —
// §決定 7's single-owner fan-out: however many goroutines call Wait
// concurrently (Runner.watchRuntime and Runner.cleanupSandboxAfterWait both
// do, on the very same session — see launchSandbox's doc comment), exactly
// one ContainerWait API call is ever made.
func (s *containerSession) Wait(ctx context.Context) (backend.RuntimeExit, error) {
	select {
	case <-ctx.Done():
		return backend.RuntimeExit{}, ctx.Err()
	case <-s.done:
		s.mu.Lock()
		exit := s.exit
		s.mu.Unlock()
		return exit, nil
	}
}

// waitLoop is the session's single ContainerWait owner. Ordering after
// detecting exit follows §決定 7/8's "diagnostics before resource teardown"
// contract: drain the read loop (readDone) so the transcript buffer is
// final, THEN finalize exit state and close done (unblocking Wait
// callers), THEN run the diagnostics collector (if any — see
// ContainerBackendOptions.DiagnosticsCollector's doc comment) to
// completion, THEN — strictly after all of that — remove the container and
// the secret-carrying host spec file. Because container removal happens
// last, both after Wait has already returned to every caller and after the
// diagnostics collector has finished, no caller — nor the collector — can
// observe a removed container through this session's own state.
//
// Removal itself tries without Force first: the container already exited
// (ContainerWait resolved), so a plain remove should succeed; Force is
// reserved for the retry after an error, rather than being applied
// unconditionally on every removal (a "silent force" masks whatever made
// the plain remove fail).
func (s *containerSession) waitLoop() {
	waitRes := s.api.ContainerWait(context.Background(), s.id, client.ContainerWaitOptions{
		Condition: container.WaitConditionNotRunning,
	})
	var exitCode int
	select {
	case res := <-waitRes.Result:
		exitCode = int(res.StatusCode)
	case err := <-waitRes.Error:
		slog.Warn("container backend: ContainerWait failed", "container_id", s.id, "error", err)
		exitCode = 1
	}

	// The container process has exited, but its attach stream can still
	// deliver a final burst of already-produced output for a short window
	// afterward. Prefer letting readLoop drain it naturally — it returns
	// (closing readDone) once the daemon itself closes the stream — rather
	// than closing our side immediately, which could truncate exactly that
	// final burst. Only force-close via closeConn if draining hasn't
	// finished within attachDrainGracePeriod: this bounds the wait and
	// still guarantees readDone closes even if the daemon is slow (or, for
	// a session with no attach at all — Adopt's best-effort-attach-failed
	// path — readDone was already closed synchronously by attach's own
	// error path, so this select returns immediately).
	select {
	case <-s.readDone:
		s.closeConn()
	case <-time.After(attachDrainGracePeriod):
		s.closeConn()
		<-s.readDone
	}

	s.mu.Lock()
	s.running = false
	s.exit = backend.RuntimeExit{ExitCode: exitCode}
	s.closeSubscribersLocked()
	exit := s.exit
	s.mu.Unlock()
	close(s.done)

	if collector := s.backend.diagnosticsCollector; collector != nil {
		collector(context.Background(), s.id, exit)
	}

	s.backend.forgetSession(s.id)
	if _, err := s.api.ContainerRemove(context.Background(), s.id, client.ContainerRemoveOptions{RemoveVolumes: true}); err != nil {
		slog.Warn("container backend: remove exited container failed; retrying with Force", "container_id", s.id, "error", err)
		if _, ferr := s.api.ContainerRemove(context.Background(), s.id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); ferr != nil {
			slog.Warn("container backend: force remove exited container failed", "container_id", s.id, "error", ferr)
		}
	}
	if s.specPath != "" {
		_ = os.Remove(s.specPath)
	}
}

// Stop requests graceful termination: docker stop sends the container's
// configured stop signal (SIGTERM by default) and waits up to a timeout
// (docker's own default, 10s — not overridden here) before SIGKILL.
func (s *containerSession) Stop(ctx context.Context) error {
	_, err := s.api.ContainerStop(ctx, s.id, client.ContainerStopOptions{})
	return err
}

// Signal delivers sig to the container's PID 1 (docker-init, §決定 3) via
// `docker kill --signal=<sig>` — no SIGKILL follow-up, matching the
// interface contract. docker-init forwards signals to its child (the boid
// runner-container entrypoint), whose harness adapters' own
// sigutil.ForwardAndWait reacts to SIGUSR1 exactly as the userns path's
// SIG_IGN'd-then-adapter-handled chain does (see the plan doc's §決定 3).
func (s *containerSession) Signal(ctx context.Context, sig syscall.Signal) error {
	name := unix.SignalName(sig)
	if name == "" {
		name = strconv.Itoa(int(sig))
	}
	_, err := s.api.ContainerKill(ctx, s.id, client.ContainerKillOptions{Signal: name})
	return err
}
