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
	"path/filepath"
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

	"github.com/novshi-tech/boid/internal/mtls"
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
	// dockerTLSCA / dockerProxyAddr implement §決定5's per-job dockerproxy
	// client cert delivery — see ContainerBackendOptions.DockerTLSCA's doc
	// comment. dockerTLSCA nil (every pre-this-feature caller) disables the
	// whole feature: Launch neither issues a cert nor adds any DOCKER_* env.
	dockerTLSCA     *mtls.CA
	dockerProxyAddr string
	// runtimeDir, when non-empty, is the host-visible directory
	// materializeDockerClientCert writes per-job TLS material under —
	// see ContainerBackendOptions.RuntimeDir's doc comment for why this
	// (not os.MkdirTemp's container-private default) is required for a
	// real compose deploy.
	runtimeDir string
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

	// DockerTLSCA, when non-nil, is the mTLS CA (internal/mtls.CA) Launch
	// uses to issue a short-lived per-job client certificate for any spec
	// launched with LaunchOptions.DockerEnabled — §決定5's "per-job 短命
	// client cert (mTLS) を... env で配送" (the plan's chosen delivery style;
	// a URL-path-embedded token was ruled out because DOCKER_HOST cannot
	// carry a path). nil (every pre-PR6 caller) disables this entirely: no
	// cert is issued, no DOCKER_* env is added, no bind mount is created —
	// byte-for-byte the same Launch behavior as before this field existed.
	// Real production wiring of a daemon-owned CA into this field, and of a
	// compose-reachable dockerproxy TCP listener behind DockerProxyAddr, is
	// PR6-residual/PR7 territory (see build/container/compose.yml's own
	// "NOT yet true of this file" note) — this option exists so the
	// materialize-cert / mount / env-delivery mechanics are real and
	// unit-tested ahead of that wiring landing.
	DockerTLSCA *mtls.CA
	// DockerProxyAddr is the compose-network `host:port` (typically a
	// compose service DNS name) job containers' DOCKER_HOST env should
	// point at. Ignored when DockerTLSCA is nil.
	DockerProxyAddr string
	// RuntimeDir, when non-empty, is the host-visible directory (typically
	// $BOID_RUNTIME_DIR, bind-mounted source == target into this daemon's
	// own container — build/container/compose.yml's "Persistence" header
	// comment) materializeDockerClientCert writes each job's per-job TLS
	// material (cert.pem/key.pem/ca.pem) under, as
	// <RuntimeDir>/tls/<jobID>/, instead of a fresh os.MkdirTemp("", ...)
	// directory (Major 11, PR6 codex review). This matters because Launch
	// is a DooD (docker-out-of-docker) backend: the container it creates
	// is a SIBLING via the HOST's own docker daemon, not nested inside
	// this daemon's own container, so a mount Source it hands that
	// daemon has to be a path the host filesystem actually has.
	// os.MkdirTemp's default (this daemon container's own, typically
	// unmounted, private /tmp) is not one — the sibling docker daemon
	// would either mount the wrong host directory or fail outright. Empty
	// (every pre-this-field caller/test) falls back to the prior
	// os.MkdirTemp("", ...) behavior unchanged — correct for any caller
	// NOT running under a compose deploy with BOID_RUNTIME_DIR bind
	// mounted (e.g. every existing unit test, which shares a real host
	// /tmp with its own test process either way).
	RuntimeDir string
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

	// containerDockerTLSDir is the fixed container-internal path a per-job
	// dockerproxy client cert (§決定5) is bind-mounted at, and the value the
	// job's DOCKER_CERT_PATH env is set to. docker CLI's own
	// DOCKER_CERT_PATH convention expects exactly cert.pem/key.pem/ca.pem
	// under this directory (dockerCertFileName / dockerKeyFileName /
	// dockerCAFileName below).
	containerDockerTLSDir = "/run/boid/docker-tls"

	dockerCertFileName = "cert.pem"
	dockerKeyFileName  = "key.pem"
	dockerCAFileName   = "ca.pem"

	// perJobDockerCertValidity bounds how long a per-job dockerproxy client
	// cert (materializeDockerClientCert) stays valid (Blocker 4, PR6 codex
	// review) — deliberately far short of mtls.CA's default 30-day leaf
	// validity: this cert is bind-mounted read-only into a job container
	// whose own lifetime is normally minutes, and a copy the job's own
	// process makes onto a sibling before exiting must not remain usable
	// long after the job's materialization directory (dockerTLSDir, always
	// removed on exit — see containerSession's own doc comment) is gone.
	// Full job-identity binding (cert CN/SAN → job_id, verified by
	// dockerproxy itself) is PR7 scope per the plan doc; this short leaf
	// validity is PR6's "revocation by expiry" mitigation in the meantime.
	perJobDockerCertValidity = time.Hour

	// Resource labels (§決定 6/9): boid.job_id + boid.workspace are always
	// set; boid.install_id is set whenever ContainerBackendOptions.InstallID
	// is non-empty (PR6 territory — see its doc comment). ReapOrphans (§決定
	// 6) filters on the mere presence of boid.job_id ("global filter") since
	// install_id-scoped filtering needs PR6's install_id generation.
	labelJobID     = "boid.job_id"
	labelWorkspace = "boid.workspace"
	labelInstallID = "boid.install_id"

	// LabelJobID / LabelWorkspace / LabelInstallID are exported aliases of
	// the label constants above, so PR6's daemon-independent `boid reap`
	// CLI (internal/reap, cmd/reap.go — docs/plans/phase6-container-backend.md
	// §決定6) and this package's own label emission read the exact same
	// string literal rather than risking drift between two independently
	// hand-typed copies of "boid.install_id".
	LabelJobID     = labelJobID
	LabelWorkspace = labelWorkspace
	LabelInstallID = labelInstallID

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
// As of PR7 (docs/plans/phase6-container-backend.md §PR7 cutover),
// internal/server/wire.go's sandboxBackendForConfig calls this in
// production when config.yaml sets `sandbox.backend: container`, and
// assigns the result to Runner.Backend — the same DI seam
// (internal/dispatcher/runner.go, landed PR1) tests have exercised this
// backend through since PR5. Every pre-PR7 caller (and every test that
// doesn't opt in via that config key) is unaffected: Runner.Backend stays
// nil and Runner.sandboxBackend() keeps constructing the usernsBackend.
func NewContainerBackend(api dockerAPI, opts ContainerBackendOptions) backend.SandboxBackend {
	b := &containerBackend{
		api:                  api,
		defaultImage:         opts.DefaultImage,
		pullPolicy:           opts.PullPolicy,
		installID:            opts.InstallID,
		diagnosticsCollector: opts.DiagnosticsCollector,
		dockerTLSCA:          opts.DockerTLSCA,
		dockerProxyAddr:      opts.DockerProxyAddr,
		runtimeDir:           opts.RuntimeDir,
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

// IsContainerBackend reports whether be is a containerBackend constructed
// by NewContainerBackend. Exists solely as an external-package
// introspection helper for docs/plans/phase6-container-backend.md §PR7's
// config-driven backend-selection wiring (internal/server/wire.go's
// sandboxBackendForConfig) — that package cannot type-assert against the
// unexported *containerBackend type directly, and this is cheaper for a
// test to depend on than reflect-based %T string matching.
func IsContainerBackend(be backend.SandboxBackend) bool {
	_, ok := be.(*containerBackend)
	return ok
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
	specPath, statePath, err := writeContainerSpec(spec, b.runtimeDir)
	if err != nil {
		return nil, fmt.Errorf("write container sandbox spec: %w", err)
	}
	// dockerTLSDir is set below (only when opts.DockerEnabled && b.dockerTLSCA
	// != nil) but declared here so cleanupFiles's closure sees whichever
	// value it ends up with, even on an early return before it is set (the
	// zero value "" makes the RemoveAll a no-op).
	var dockerTLSDir string
	cleanupFiles := func() {
		if b.runtimeDir != "" {
			// Blocker 1 (PR7 codex review): specPath/statePath live under
			// <runtimeDir>/spec/<jobID>/ when RuntimeDir is configured (see
			// writeContainerSpec's doc comment) — remove the whole per-job
			// directory rather than the two files individually, so no empty
			// directory accumulates under runtimeDir/spec across the
			// lifetime of a long-running daemon.
			_ = os.RemoveAll(filepath.Dir(specPath))
		} else {
			_ = os.Remove(specPath)
			_ = os.Remove(statePath)
		}
		if dockerTLSDir != "" {
			_ = os.RemoveAll(dockerTLSDir)
		}
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

	// labelWorkspace is always set from opts.Workspace, even when empty
	// ("workspace unknown" — an explicit, visible value rather than the
	// label being silently omitted; see LaunchOptions.Workspace's doc
	// comment, PR5 review Minor finding).
	labels := map[string]string{
		labelJobID:     opts.JobID,
		labelWorkspace: opts.Workspace,
	}
	if b.installID != "" {
		labels[labelInstallID] = b.installID
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

	// Per-job dockerproxy client cert delivery (§決定5): only when the job
	// declared docker capabilities AND this backend was configured with a
	// CA to issue from (ContainerBackendOptions.DockerTLSCA — nil for
	// every caller before this feature, and still nil in production as of
	// PR6, see its doc comment). env starts as realized.Env unmodified in
	// the disabled case (the overwhelmingly common path today) — no copy,
	// no behavior change.
	env := realized.Env
	if opts.DockerEnabled && b.dockerTLSCA != nil {
		dir, derr := b.materializeDockerClientCert(opts.JobID)
		if derr != nil {
			cleanupFiles()
			return nil, derr
		}
		dockerTLSDir = dir
		mounts = append(mounts, mount.Mount{Type: mount.TypeBind, Source: dir, Target: containerDockerTLSDir, ReadOnly: true})
		env = withDockerTLSEnv(env, b.dockerProxyAddr)
	}

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
		Env:          envSlice(env),
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

	sess := newContainerSession(b, createRes.ID, realized.TTY, specPath, dockerTLSDir)
	// Disk transcript spool (§決定8, PR7): only for freshly-Launch'd
	// sessions — see openTranscriptSpool's doc comment for why Adopt
	// (doAdopt, below) deliberately does not also open one.
	//
	// [Major 8, PR7 codex review]: a genuine open/create failure (as
	// opposed to "b.runtimeDir unset, spooling not configured" — see
	// openTranscriptSpool's own doc comment) fails Launch hard, torn down
	// exactly like a ContainerCreate/attach/start failure below, rather than
	// silently starting a job whose output cannot survive its own container
	// removal.
	spoolFile, spoolPath, spoolErr := b.openTranscriptSpool(createRes.ID)
	if spoolErr != nil {
		_, _ = b.api.ContainerRemove(context.Background(), createRes.ID, client.ContainerRemoveOptions{Force: true})
		cleanupFiles()
		return nil, fmt.Errorf("open transcript spool: %w", spoolErr)
	}
	sess.transcriptFile, sess.transcriptPath = spoolFile, spoolPath
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
	sess := newContainerSession(b, runtimeID, tty, "", "")
	if err := sess.attach(ctx, true); err != nil {
		slog.Warn("container backend: adopt attach failed; session will support signal/stop/wait only",
			"container_id", runtimeID, "error", err)
	}
	sess.start()
	return sess
}

// ReapOrphans reconciles job containers a daemon restart lost track of.
// §決定 6: label enumeration → destroy, using the mere presence of
// boid.job_id as the docker-side LIST filter ("global filter" — a container
// with no boid.job_id label was never created by this backend at all, no
// matter which installation).
//
// [Blocker 5, PR7 codex review]: within that list, every candidate is now
// ALSO checked against boid.install_id in application code (not folded into
// the docker filter query itself — see the note on that choice below)
// whenever b.installID is non-empty (PR6's install_id generation has landed
// by PR7 — see ContainerBackendOptions.InstallID's doc comment). WITHOUT
// this, two boid installations sharing one docker engine (distinct install
// IDs — e.g. two users, or a dev + prod compose stack on the same host)
// would each force-remove the OTHER's live, in-flight job containers on
// restart: the pre-fix filter matched on the mere presence of boid.job_id,
// which every container either installation ever creates carries
// regardless of whose daemon made it.
//
// The install_id check runs in Go rather than as a second `label` filter
// value on the same docker ContainerListOptions.Filters query deliberately:
// client.Filters' own doc comment states "a filter TERM is satisfied if ANY
// ONE of the values in its set is a match" (OR within a term) — the mere
// presence check (labelJobID, no "=value") and an exact-match check
// (labelInstallID+"="+installID) are two VALUES under the same "label" term,
// so relying on the dockerd server to AND them instead of OR them would be
// betting an accidental-deletion-of-another-installation's-live-containers
// bug on an undocumented server-side special case this package has no way
// to verify without a live multi-install docker engine to test against.
// Filtering candidates by label in Go after a broader docker-side list is
// unambiguous and directly unit-testable with the fake dockerAPI.
//
// b.installID empty (a fresh daemon before PR6's install_id LoadOrCreate has
// ever run, or test/DI wiring that never sets
// ContainerBackendOptions.InstallID) skips the install_id check entirely —
// every boid.job_id-labeled container is a fair reap target, exactly as
// before this fix; this is the same degrade NewContainerBackend's own
// InstallID doc comment already documents for the empty-installID case
// elsewhere (resource labeling degrades the same way). Volumes and networks
// are reaped by the identical logic — nothing in PR5/PR6 creates
// job-labeled volumes/networks yet (workspace HOME stays a host bind through
// Phase 6, §決定 4; workspace networks are PR6), so these two loops are
// forward-compat scaffolding, not exercised by real traffic yet.
func (b *containerBackend) ReapOrphans(ctx context.Context) (backend.ReapReport, error) {
	filters := client.Filters{}.Add("label", labelJobID)

	listRes, err := b.api.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		wrapped := fmt.Errorf("list orphan containers: %w", err)
		return backend.ReapReport{GlobalError: wrapped}, wrapped
	}

	report := backend.ReapReport{}
	for _, c := range listRes.Items {
		if !b.reapOwnsLabels(c.Labels) {
			continue
		}
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

// reapOwnsLabels reports whether a docker resource's labels belong to this
// backend's installation and are therefore safe for ReapOrphans to destroy
// (Blocker 5, PR7 codex review — see ReapOrphans' own doc comment for why
// this check runs in application code rather than as a docker-side filter
// value). b.installID empty means "no install_id scoping configured yet"
// (pre-PR6 wiring / tests) — every boid.job_id-labeled resource is owned,
// matching the original global-filter behavior.
func (b *containerBackend) reapOwnsLabels(labels map[string]string) bool {
	if b.installID == "" {
		return true
	}
	return labels[labelInstallID] == b.installID
}

func (b *containerBackend) reapOrphanVolumes(ctx context.Context, filters client.Filters) {
	listRes, err := b.api.VolumeList(ctx, client.VolumeListOptions{Filters: filters})
	if err != nil {
		slog.Warn("container backend: list orphan volumes failed", "error", err)
		return
	}
	for _, v := range listRes.Items {
		if !b.reapOwnsLabels(v.Labels) {
			continue
		}
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
		if !b.reapOwnsLabels(n.Labels) {
			continue
		}
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

// writeContainerSpec writes spec's JSON and an empty runner-state.json to a
// host path Launch bind-mounts into the sibling job container.
//
// [Blocker 1, PR7 codex review]: when runtimeDir is empty (every pre-PR7
// caller/test, and any deploy that hasn't wired ContainerBackendOptions.
// RuntimeDir), this reproduces the original behavior verbatim — the exact
// same `/tmp/boid-<ID>-runner-{spec,state}.json` naming convention
// dispatcher.sandboxPreparerImpl.PrepareSandbox uses for the userns backend
// (see its own doc comment), so the existing `/tmp/boid-*` 30-day GC sweep
// (CLAUDE.md「ディスク使用量の管理」) still covers it. But a REAL compose
// deploy runs this daemon inside its own container: Launch is a DooD
// (docker-out-of-docker) backend, so a mount Source it hands the HOST's own
// docker daemon has to be a path the HOST filesystem actually has — the
// daemon container's private /tmp is not (ContainerCreate would either bind
// the wrong host directory or fail outright, exactly like
// dockerTLSCertDir's identical DooD rationale, see its own doc comment).
//
// When runtimeDir is set, the spec/state pair instead lands under
// <runtimeDir>/spec/<spec.ID>/runner-{spec,state}.json — runtimeDir is
// b.runtimeDir, which ContainerBackendOptions.RuntimeDir's own doc comment
// establishes is bind-mounted source == target into this daemon's own
// container (build/container/compose.yml's BOID_RUNTIME_DIR), so any
// absolute path this process computes under it is, by construction, already
// a real path the sibling docker daemon can mount from. Cleanup (Launch's
// cleanupFiles, containerSession.waitLoop) removes the whole per-job
// <runtimeDir>/spec/<spec.ID>/ directory rather than the two files
// individually.
//
// Deliberately does NOT call sandboxPreparerImpl.PrepareSandbox: it also
// allocates spec.RootDir (a tmpfs mount point for userns pivot_root) which a
// container backend has no use for — the container's own image rootfs is
// the sandbox root.
//
// statePath is created empty (not just planned) up front because it is
// bind-mounted into the container as a single file: docker's bind-mount
// setup does not create a missing host **file** path the way it can create
// a missing directory, so the target must already exist before
// ContainerCreate runs.
func writeContainerSpec(spec sandbox.Spec, runtimeDir string) (specPath, statePath string, err error) {
	if runtimeDir == "" {
		specPath = fmt.Sprintf("/tmp/boid-%s-runner-spec.json", spec.ID)
		statePath = fmt.Sprintf("/tmp/boid-%s-runner-state.json", spec.ID)
	} else {
		dir := filepath.Join(runtimeDir, "spec", spec.ID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", "", fmt.Errorf("create sandbox spec dir: %w", err)
		}
		specPath = filepath.Join(dir, "runner-spec.json")
		statePath = filepath.Join(dir, "runner-state.json")
	}

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

// materializeDockerClientCert issues a fresh per-job dockerproxy client
// certificate from b.dockerTLSCA and writes the cert/key/ca PEM trio to a
// host temp directory, in the exact file-name layout docker's own
// DOCKER_CERT_PATH convention expects (cert.pem/key.pem/ca.pem — §決定5).
// The caller bind-mounts the returned directory read-only into the
// container at containerDockerTLSDir; containerSession.waitLoop removes it
// once the container exits (mirroring specPath's own always-cleaned-up
// retention contract — see containerSession.dockerTLSDir's doc comment).
func (b *containerBackend) materializeDockerClientCert(jobID string) (dir string, err error) {
	leaf, err := b.dockerTLSCA.IssueShortLivedClientCert("job-"+jobID, perJobDockerCertValidity)
	if err != nil {
		return "", fmt.Errorf("issue docker client cert: %w", err)
	}
	certPEM, keyPEM, err := mtls.EncodeCertPEM(leaf)
	if err != nil {
		return "", fmt.Errorf("encode docker client cert: %w", err)
	}

	dir, err = b.dockerTLSCertDir(jobID)
	if err != nil {
		return "", err
	}

	files := map[string][]byte{
		dockerCertFileName: certPEM,
		dockerKeyFileName:  keyPEM,
		dockerCAFileName:   b.dockerTLSCA.CertPEM(),
	}
	for name, data := range files {
		// 0600: the private key lives in this same directory (docker's
		// convention keeps all three files together) — no reason for any
		// of the three to be broader than the key needs.
		if werr := os.WriteFile(filepath.Join(dir, name), data, 0o600); werr != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("write %s: %w", name, werr)
		}
	}
	return dir, nil
}

// dockerTLSCertDir returns (creating it if necessary) the directory
// materializeDockerClientCert writes jobID's cert.pem/key.pem/ca.pem trio
// into (Major 11, PR6 codex review — see ContainerBackendOptions.
// RuntimeDir's doc comment for the DooD host-visibility rationale):
//   - b.runtimeDir set (the compose/container-backend deploy):
//     <runtimeDir>/tls/<jobID> — a fixed, host-path-stable location
//     under the already bind-mounted (source == target) BOID_RUNTIME_DIR
//     a sibling docker daemon can actually mount FROM.
//   - b.runtimeDir empty (every pre-this-field test/caller): a fresh
//     os.MkdirTemp("", ...) directory, unchanged from this backend's
//     original behavior.
func (b *containerBackend) dockerTLSCertDir(jobID string) (string, error) {
	if b.runtimeDir == "" {
		dir, err := os.MkdirTemp("", "boid-"+jobID+"-docker-tls-")
		if err != nil {
			return "", fmt.Errorf("create docker tls cert dir: %w", err)
		}
		return dir, nil
	}
	dir := filepath.Join(b.runtimeDir, "tls", jobID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create docker tls cert dir: %w", err)
	}
	return dir, nil
}

// openTranscriptSpool creates (truncating any stale leftover) and opens
// <runtimeDir>/<containerID>/transcript.log for a freshly-Launch'd session
// (§決定8, PR7) — the same path/filename ReadTranscript/StatTranscript
// (transcript.go) already read for the userns backend, and the same
// directory dockerTLSCertDir's <runtimeDir>/tls/<jobID> is host-visible
// under (see its own doc comment for the DooD host-visibility rationale;
// b.runtimeDir is the identical field).
//
// [Major 8, PR7 codex review]: returns (nil, "", nil) — spooling
// intentionally disabled, in-memory-only transcript, unchanged from PR5's
// behavior — ONLY when b.runtimeDir is empty (every pre-PR7 test/caller);
// that is a configuration choice, not a failure. A non-nil error return
// (directory creation or file open genuinely failed — e.g. the runtimes
// filesystem is full or unwritable) is now a real error Launch's caller
// must fail hard on: §決定8's contract is that `boid job log` sees the FULL
// transcript once a container backend deploy is live (this is what
// distinguishes it from the tail-only silent-exit diagnostics), so silently
// degrading to an in-memory-only buffer (invisible the moment the container
// is removed) when the operator's own deploy configured a persistent spool
// directory would violate that contract without ever telling anyone.
// Launch treats this the same as any other Launch-phase failure: the
// container is torn down and Dispatch reports the error, rather than
// starting a job whose output will not survive its own container removal.
//
// Deliberately NOT called from doAdopt (Adopt's cache-miss path): Adopt's
// `Logs: true` attach replays the container's ENTIRE output history
// through appendTranscript again (the closest this backend gets to a
// separate `docker logs` call — doAdopt's own doc comment), so opening a
// fresh spool file there in append mode would duplicate everything before
// the restart, and opening it with O_TRUNC would destroy it. A container
// adopted after a daemon restart keeps whatever transcript.log content
// this process wrote before it went away — readable via `boid job log`
// exactly as it was — but gets no further disk-spool writes for the rest
// of its lifetime (the in-memory buffer + live Subscribe/fan-out still
// works normally). Full restart-continuity for the disk spool is left as
// a documented gap for PR9 (docs/plans/phase6-container-backend.md's own
// "実装残余" territory) rather than risking log corruption to close it now.
func (b *containerBackend) openTranscriptSpool(containerID string) (f *os.File, path string, err error) {
	if b.runtimeDir == "" {
		return nil, "", nil
	}
	dir := filepath.Join(b.runtimeDir, containerID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", fmt.Errorf("create runtime dir for transcript spool: %w", err)
	}
	spoolPath := filepath.Join(dir, localRuntimeTranscriptFile)
	spoolFile, err := os.OpenFile(spoolPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, "", fmt.Errorf("open transcript spool: %w", err)
	}
	return spoolFile, spoolPath, nil
}

// withDockerTLSEnv returns a copy of env with the three DOCKER_* variables
// a docker client (CLI, SDK, TestContainers, ...) reads to select and
// authenticate an mTLS-secured DOCKER_HOST added — DOCKER_HOST pointing at
// the compose-network dockerproxy address, DOCKER_CERT_PATH at the
// bind-mounted per-job cert directory (containerDockerTLSDir),
// DOCKER_TLS_VERIFY enabling mTLS. Always overrides any pre-existing
// values for these three specific keys (daemon-controlled, not
// spec-controlled) rather than only filling gaps — a job cannot opt out of
// or redirect its own docker mTLS identity via its own Env. Every other
// key in env is carried through unchanged; env itself is never mutated
// (Launch's realized.Env may be reused elsewhere).
func withDockerTLSEnv(env map[string]string, proxyAddr string) map[string]string {
	out := make(map[string]string, len(env)+3)
	for k, v := range env {
		out[k] = v
	}
	out["DOCKER_HOST"] = "tcp://" + proxyAddr
	out["DOCKER_CERT_PATH"] = containerDockerTLSDir
	out["DOCKER_TLS_VERIFY"] = "1"
	return out
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
	// specDir, when non-empty, is the per-job directory writeContainerSpec
	// created specPath/statePath under (<runtimeDir>/spec/<spec.ID> —
	// Blocker 1, PR7 codex review) and is removed wholesale (os.RemoveAll)
	// instead of specPath alone, so no empty directory accumulates under
	// runtimeDir/spec over the daemon's lifetime. Empty when
	// ContainerBackendOptions.RuntimeDir was unset (the pre-PR7 flat
	// /tmp/boid-<ID>-runner-*.json layout, where only the file itself is
	// ever removed) or for Adopt-reconstructed sessions.
	specDir string
	// dockerTLSDir is the per-job cert directory materializeDockerClientCert
	// wrote (§決定5), removed alongside specPath once the container exits.
	// Empty whenever LaunchOptions.DockerEnabled was false or no
	// ContainerBackendOptions.DockerTLSCA was configured — the overwhelming
	// majority of sessions today.
	dockerTLSDir string

	// transcriptFile / transcriptPath implement §決定8's "daemon 側が
	// attach stream を runtime storage へ逐次 spool" full-persistence
	// contract (PR7 — modeled directly on localRuntimeSession's own
	// transcriptFile/transcriptPath in runtime_local_linux.go, per §決定8's
	// own "現行 session 層の抽出・流用" instruction): every chunk
	// appendTranscript records to the in-memory buffer is also written here,
	// at <runtimeDir>/<containerID>/transcript.log — the exact path
	// ReadTranscript/StatTranscript (transcript.go, backend-neutral) already
	// read, and the exact filename (localRuntimeTranscriptFile) the userns
	// backend's own transcript.log uses. This is what lets `boid job log`
	// keep working after ContainerRemove: docker itself discards `docker
	// logs` history once a container is removed, but this file survives on
	// the host bind-mounted runtimes dir.
	//
	// Both are empty when ContainerBackendOptions.RuntimeDir was empty
	// (every pre-PR7 test/caller — see dockerTLSCertDir's identical
	// fallback) or when spool-file creation failed (advisory: a spool
	// failure degrades `boid job log` for this one job, it must never fail
	// Launch), and are ALWAYS empty for Adopt-reconstructed sessions — see
	// openTranscriptSpool's own doc comment for why re-spooling on Adopt is
	// deliberately not attempted yet.
	transcriptFile *os.File
	transcriptPath string

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

func newContainerSession(b *containerBackend, id string, tty bool, specPath, dockerTLSDir string) *containerSession {
	sess := &containerSession{
		backend:      b,
		id:           id,
		api:          b.api,
		tty:          tty,
		specPath:     specPath,
		dockerTLSDir: dockerTLSDir,
		subscribers:  make(map[int]chan []byte),
		running:      true,
		done:         make(chan struct{}),
		readDone:     make(chan struct{}),
	}
	if specPath != "" && b.runtimeDir != "" {
		sess.specDir = filepath.Dir(specPath)
	}
	return sess
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
	// Disk spool (§決定8, PR7): mirrors localRuntimeSession.appendTranscript's
	// own `s.transcriptFile.Write(chunk)` — nil (spooling disabled or an
	// Adopt-reconstructed session, see openTranscriptSpool's doc comment)
	// is the overwhelming majority of PR5-vintage callers and a no-op here.
	if s.transcriptFile != nil {
		if _, err := s.transcriptFile.Write(chunk); err != nil {
			slog.Warn("container backend: write transcript spool failed", "container_id", s.id, "error", err)
		}
	}
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

	// Close (and flush) the disk transcript spool now: readLoop — the sole
	// writer via appendTranscript — has already returned (readDone closed
	// above), so no further writes can race this Close. Doing this BEFORE
	// finalizing exit state / closing s.done means a diagnostics collector
	// that reads transcript.log from disk (§決定8's silent-exit
	// classification) always sees the complete file, and BEFORE
	// ContainerRemove means the file is guaranteed durable before the
	// container itself (and any `docker logs` fallback) is gone.
	if s.transcriptFile != nil {
		if err := s.transcriptFile.Close(); err != nil {
			slog.Warn("container backend: close transcript spool failed", "container_id", s.id, "path", s.transcriptPath, "error", err)
		}
	}

	s.mu.Lock()
	s.running = false
	s.exit = backend.RuntimeExit{ExitCode: exitCode, TranscriptPath: s.transcriptPath}
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
	if s.specDir != "" {
		// Blocker 1 (PR7 codex review): a runtimeDir-scoped spec lives in its
		// own per-job directory (<runtimeDir>/spec/<spec.ID>/) — remove it
		// wholesale rather than just specPath, matching Launch's cleanupFiles
		// on the error path.
		_ = os.RemoveAll(s.specDir)
	} else if s.specPath != "" {
		_ = os.Remove(s.specPath)
	}
	if s.dockerTLSDir != "" {
		_ = os.RemoveAll(s.dockerTLSDir)
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
