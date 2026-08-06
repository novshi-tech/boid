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

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"golang.org/x/sys/unix"

	"github.com/novshi-tech/boid/internal/dockerres"
	"github.com/novshi-tech/boid/internal/mtls"
	"github.com/novshi-tech/boid/internal/reap"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
	"github.com/novshi-tech/boid/internal/sandbox/realization"
	"github.com/novshi-tech/boid/internal/version"
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
	// brokerTLSCA / brokerTLSAddr implement §⓪'s per-job broker client
	// cert delivery — see ContainerBackendOptions.BrokerTLSCA's doc
	// comment. brokerTLSCA nil (every pre-this-feature caller) disables
	// the whole feature: Launch neither issues a cert nor adds any
	// BOID_BROKER_TLS_* env. brokerTLSAddr is a pointer (not a resolved
	// string), dereferenced fresh in Launch — see
	// ContainerBackendOptions.BrokerTLSAddr's doc comment for why.
	brokerTLSCA   *mtls.CA
	brokerTLSAddr *string
	// runtimeDir, when non-empty, is the host-visible directory
	// materializeDockerClientCert writes per-job TLS material under —
	// see ContainerBackendOptions.RuntimeDir's doc comment for why this
	// (not os.MkdirTemp's container-private default) is required for a
	// real compose deploy.
	runtimeDir string
	// transcriptDir, when non-empty, overrides runtimeDir as the spool root
	// for transcript.log (openTranscriptSpool) — see
	// ContainerBackendOptions.TranscriptDir's doc comment. Empty falls back
	// to runtimeDir, same as every pre-this-field caller.
	transcriptDir string
	// diagnosticsCollector, when non-nil, is invoked once a container has
	// exited — after Wait's fan-out has resolved and the attach stream has
	// fully drained (Major 3) — but strictly before the container is
	// removed. See ContainerBackendOptions.DiagnosticsCollector's doc
	// comment.
	diagnosticsCollector func(ctx context.Context, containerID string, exit backend.RuntimeExit)

	// selfContainerID implements §決定5's daemon-self-connect (PR9): see
	// ContainerBackendOptions.SelfContainerID's doc comment. Empty (every
	// pre-PR9 caller) disables ensureWorkspaceNetwork's NetworkConnect step
	// entirely.
	selfContainerID string

	// usernsOnce/usernsMode cache the engine-identity probe backing
	// resolveUsernsMode (container_backend_userns.go): rootless podman needs
	// every job container created with "keep-id" so the job's uid keeps
	// matching the daemon-created bind mounts it has to read. Resolved
	// lazily on the first Launch rather than in NewContainerBackend, which
	// has no context to probe with and must not do I/O.
	usernsOnce sync.Once
	usernsMode container.UsernsMode

	// infoMu/infoOK/info cache the engine /info probe itself
	// (container_backend_userns.go's resolveEngineInfo), shared by
	// resolveUsernsMode (rootless detection) AND resolveHostArch (the arch
	// mismatch fail-fast in resolveImage needs the engine's own reported
	// host architecture, not this Go binary's build architecture — see
	// resolveImage's own comment for why those differ under emulation) so
	// two independent features consuming the same engine identity pay for
	// exactly one round trip, not one each, ONCE a probe has actually
	// succeeded.
	//
	// [Blocker, PR4 codex review round 2]: a plain sync.Once (this field's
	// pre-fix shape) would cache a FAILED probe just as permanently as a
	// successful one — silently disabling resolveHostArch's arch mismatch
	// fail-fast for the rest of this backend's lifetime after one
	// transient /info hiccup, directly contradicting docs/plans/
	// release-onboarding.md 決定5's "must" fail-fast requirement. infoOK
	// only flips true on success, so a failed probe is retried on every
	// subsequent resolveImage/resolveUsernsMode call instead of being
	// cached as a permanent "unknown" — resolveUsernsMode's own posture
	// (a failure there is fine to degrade forever, per its own doc
	// comment) is unaffected: it still only ever calls resolveEngineInfo
	// once via usernsOnce above, so a later successful retry (triggered by
	// a DIFFERENT job's resolveHostArch call) never revisits an
	// already-decided usernsMode.
	infoMu sync.Mutex
	infoOK bool
	info   client.SystemInfoResult

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
	// Empty falls back to version.DefaultContainerImage() — see
	// NewContainerBackend's own assignment for why.
	DefaultImage string
	// PullPolicy controls image pulling (see ImagePullPolicy). Zero value
	// is ImagePullIfNotPresent.
	PullPolicy ImagePullPolicy
	// UID/GID select the `--user <uid>:<gid>` job containers run as (§決定
	// 4 — non-root; docs/plans/release-onboarding.md 決定1/PR2 — arbitrary-
	// uid, gid 0 + `g=u` group permissions, self-registered into
	// /etc/passwd at runtime rather than baked per-uid at image build
	// time). nil means "unset". A custom pair is only honored when BOTH
	// are provided (non-nil) AND uid resolves to non-zero — anything else
	// (both unset, only one set, or uid == 0) falls back to 1000:0 (the
	// non-root default — see defaultContainerGID's own doc comment for
	// why the fallback gid is 0, not 1000) rather than silently running
	// the job as root.
	//
	// gid == 0 is DELIBERATELY allowed through as a real value, not
	// rejected the way it was before PR2: compose's `user: "<uid>:0"`
	// (build/container/compose.yml) is the whole point of the gid-0
	// arbitrary-uid design, and internal/server/wire.go's
	// sandboxBackendForConfig passes the DAEMON's own os.Getuid()/
	// os.Getgid() straight through here — under that compose config
	// os.Getgid() legitimately IS 0. Only uid == 0 remains a hard reject
	// (決定 4 still requires non-root job containers; gid 0 is a *group*,
	// not "running as root").
	//
	// This is nullable (*int, not int) specifically so "unset" and
	// "explicitly 0" are distinguishable: an int-typed field couldn't
	// tell `UID: 0` (meant as "use the default") apart from a caller who
	// actually passed 0, which let a partial override like `UID: 0, GID:
	// 1000` slip through as a root container (fixed — see the PR5
	// review's Major 1). A real UID 0 override is never a use case this
	// backend supports (決定 4 requires non-root); GID 0 is (この doc
	// comment 自身、PR2 で「both resolve to non-zero」から更新).
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

	// TranscriptDir, when non-empty, is where openTranscriptSpool spools
	// transcript.log (and NewDefaultDiagnosticsCollector writes
	// diagnostics.json) INSTEAD of RuntimeDir — <TranscriptDir>/<containerID>/.
	// Unlike RuntimeDir, this directory does NOT need to be host-visible for
	// DooD: both files are written exclusively by the daemon process itself,
	// reading its own `docker attach`/`docker inspect`/`docker logs` calls —
	// never bind-mounted into a job container the way spec.json/state.json
	// are (writeContainerSpec's containerSpecPath/containerStatePath, which
	// DO require RuntimeDir's host-visibility). So TranscriptDir can point at
	// a real persistent volume (e.g. the daemon's own boid_state-backed data
	// home) instead of RuntimeDir's typical host tmpfs
	// (BOID_RUNTIME_DIR/XDG_RUNTIME_DIR) — fixing the "every job transcript
	// is lost on host reboot" gap hostVisibleRuntimesDirFor's own doc comment
	// (internal/server/wire.go) flags. Empty (every pre-this-field caller)
	// falls back to RuntimeDir, unchanged from before this field existed —
	// see openTranscriptSpool's own fallback.
	TranscriptDir string

	// SelfContainerID, when non-empty, is this daemon's OWN docker container
	// ID (or name) — typically `os.Getenv("HOSTNAME")`, which docker sets to
	// a container's own short ID unless overridden, inside the compose
	// daemon service itself (see sandboxBackendForConfig's wiring). PR9
	// (docs/plans/phase6-container-backend.md §決定5, §PR9): a job launched
	// with a non-empty LaunchOptions.Workspace is confined to an `Internal:
	// true` per-workspace network with no route out — the ONLY way it can
	// still reach the git gateway (mandatory: every project-visible
	// dispatch clones, see runner.go's Visibility.Clone comment), the
	// egress proxy, or the broker (docs/plans/phase6-cutover-followups.md
	// §⓪ — all three hosted in-process in this same daemon container,
	// §決定4/5) is if the daemon container ALSO joins that network, under
	// the same "boid-gateway"/"boid-egress"/"boid-broker" DNS aliases a job
	// resolves on the static `boid_internal` compose network. Empty (every
	// pre-PR9 caller, and any non-compose test/DI usage) skips the
	// self-connect step entirely — ensureWorkspaceNetwork still creates the
	// isolated network and attaches the job container to it, just without
	// also connecting the daemon, matching every unit test's expectations
	// unchanged.
	SelfContainerID string

	// BrokerTLSCA, when non-nil, is the mTLS CA (internal/mtls.CA) Launch
	// uses to issue a short-lived per-job client certificate for the
	// broker's TCP(mTLS) listener (docs/plans/phase6-cutover-followups.md
	// §⓪ "broker TCP wire completion") — the broker-side analogue of
	// DockerTLSCA above. Unlike DockerTLSCA (gated on
	// LaunchOptions.DockerEnabled — only a docker-capable job needs the
	// dockerproxy identity), Launch materializes a broker cert
	// unconditionally whenever this is non-nil: every non-foreground job
	// posts `boid job done` through the broker at minimum (the former
	// EXIT-trap replacement, internal/sandbox/runner.postJobDone), and most
	// hooks also call task-context/payload-patch RPCs, so "does this job
	// need broker RPC" is not a meaningful per-job gate the way
	// DockerEnabled is. nil (every pre-this-feature caller) disables the
	// whole feature: no cert is issued, no BOID_BROKER_TLS_* env is added,
	// no bind mount is created — byte-for-byte the same Launch behavior as
	// before this field existed. The design decision behind keeping mTLS
	// here (rather than downgrading to mtls.CA.ServerOnlyTLSConfig the way
	// the git gateway TLS fix — commit 577f9a8 — did) is documented on
	// mtls.CA.ServerOnlyTLSConfig's own doc comment and
	// docs/plans/phase6-cutover-followups.md §⓪: the broker is an
	// arbitrary-RPC endpoint (task update, job done, host-command exec),
	// not a single-purpose per-job-token-authorized clone endpoint like the
	// gateway, so per-connection client identity binding is worth keeping.
	BrokerTLSCA *mtls.CA
	// BrokerTLSAddr is a pointer to the compose-network `host:port`
	// (composeBrokerServiceName + the broker's actual bound TLS port, e.g.
	// "boid-broker:54321") job containers' BOID_BROKER_TLS_ADDR env should
	// point at. This is a POINTER, not a plain string like
	// DockerProxyAddr — unlike dockerproxy's fixed compose DNS name +
	// well-known port, the broker's TLS listener binds an OS-assigned
	// ephemeral port (internal/server/server.go's `s.broker.TLSAddr =
	// gatewayBindHost(...) + ":0"`) that is only known once Server.Start
	// has bound it, strictly AFTER sandboxBackendForConfig (internal/
	// server/wire.go) has already constructed this containerBackend inside
	// buildRuntime. internal/server.Server resolves this exact same
	// "value not known until Start runs" problem for GatewayURL via a
	// late-binding pointer (Runner.GatewayURL *string, dereferenced fresh
	// on every Dispatch) — this field mirrors that pattern one level
	// deeper: Server owns the string (brokerTLSSandboxAddr), hands
	// sandboxBackendForConfig a pointer to it at construction time, and
	// Start writes the real "boid-broker:<port>" value into it once
	// s.broker.Start has bound the listener. Launch dereferences this
	// pointer fresh on every call (not once at construction), so a job
	// launched even a long time after this backend was constructed still
	// observes the real address. nil (every non-server-wired test/caller,
	// and any deployment with BrokerTLSCA also nil) is treated the same as
	// a nil-pointing-at-empty-string — Launch adds no DOCKER_HOST-style env
	// pointing nowhere.
	BrokerTLSAddr *string
}

const (
	defaultContainerUID = 1000
	// defaultContainerGID is 0, not 1000 (codex review of PR2, Major 2):
	// the arbitrary-uid image (build/container/Dockerfile, docs/plans/
	// release-onboarding.md 決定1) only makes /run/boid/bin, /workspace,
	// and /home/boid group-0-writable (`chgrp -R 0` + `chmod -R g=u`) —
	// it does NOT chown them to uid 1000 specifically anymore. A fallback
	// of 1000:1000 would put a job container in NO group with write access
	// to any of those directories (group 1000 owns nothing there), so the
	// "safe default" this constant exists to provide would itself be
	// broken the moment it was ever used. 1000:0 is the pairing that
	// actually works against this image regardless of which uid 1000
	// happens to belong to.
	defaultContainerGID = 0
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

	// containerBrokerTLSDir is the fixed container-internal path a per-job
	// broker client cert (docs/plans/phase6-cutover-followups.md §⓪) is
	// bind-mounted at — the broker-side analogue of containerDockerTLSDir.
	// Deliberately under /run/boid/bin (not /run/boid itself), same
	// rationale as containerGitGatewayCAPath's own doc comment
	// (sandbox_builder.go): only /run/boid/bin is chowned to the job uid
	// in build/container/Dockerfile, so a non-root job container can only
	// ever successfully write/have-mounted-into new paths under that one
	// directory. This is a bind mount (not a spec.Files write the job
	// itself has to create), so the ownership constraint is actually about
	// the MOUNT TARGET's parent directory needing to already exist and be
	// enterable by the job uid, not about who creates the leaf — reusing
	// /run/boid/bin keeps every per-container TLS/CA artifact under one
	// directory instead of splitting the convention.
	containerBrokerTLSDir = "/run/boid/bin/broker-tls"

	brokerCertFileName = "cert.pem"
	brokerKeyFileName  = "key.pem"
	brokerCAFileName   = "ca.pem"

	// perJobBrokerCertValidity bounds how long a per-job broker client cert
	// (materializeBrokerClientCert) stays valid. Deliberately NOT reusing
	// perJobDockerCertValidity (1h) despite the identical
	// IssueShortLivedClientCert mechanics: dockerproxy calls are typically
	// bursty and tool-invocation-scoped, but broker RPC (task update,
	// job-done, host-command exec) can legitimately happen at any point
	// across a job's ENTIRE lifetime — including the final `boid job done`
	// call every non-foreground job makes right as it exits, which must
	// still succeed even for a job that has been running for hours (a long
	// research/build task, a multi-hour agent loop). A 1h-scoped cert would
	// make the daemon itself kill a job's own completion report out from
	// under it purely from clock skew, which is a strictly worse failure
	// mode than the dockerproxy exposure window this validity is meant to
	// bound in the first place. 24h keeps the same "revocation by expiry"
	// mitigation spirit (still 30x shorter than mtls.CA's default 30-day
	// leaf validity) while comfortably covering "a job that runs long is
	// the exception, not something we want to fail on".
	perJobBrokerCertValidity = 24 * time.Hour

	// Resource labels (§決定 6/9): boid.job_id + boid.workspace are always
	// set; boid.install_id is set whenever ContainerBackendOptions.InstallID
	// is non-empty (PR6 territory — see its doc comment). ReapOrphans (§決定
	// 6) filters on the mere presence of boid.job_id ("global filter") since
	// install_id-scoped filtering needs PR6's install_id generation.
	//
	// The literals themselves live in internal/dockerres, the import-free
	// leaf package internal/reap and internal/sandbox/dockerproxy can reach
	// too (docs/plans/workspace-home-volume-persistence.md PR1 §D2). The
	// aliases below are kept so this package's many existing call sites need
	// no rewrite; the previous arrangement — each package hand-typing its
	// own copy of "boid.install_id" with a doc comment warning about drift —
	// is gone, and with it the drift.
	labelJobID     = dockerres.LabelJobID
	labelWorkspace = dockerres.LabelWorkspace
	labelInstallID = dockerres.LabelInstallID

	// LabelJobID / LabelWorkspace / LabelInstallID are exported aliases of
	// the label constants above, kept for the external callers that already
	// reference them.
	LabelJobID     = labelJobID
	LabelWorkspace = labelWorkspace
	LabelInstallID = labelInstallID

	// boidRunnerProtocolLabel / boidRunnerProtocolVersion gate workspace
	// image overrides (§決定 11): an override image must carry this label
	// with this exact value before containerBackend.Launch will use it.
	// build/container/Dockerfile bakes it (pinned by
	// TestBoidRunnerProtocolLabel_IsBakedIntoTheImage against the Dockerfile
	// and by the image-build CI job against a built image).
	//
	// This is a COMPATIBILITY DECLARATION, not a provenance proof, and the
	// distinction is worth stating because this comment used to claim the
	// stronger thing ("proving it derives from the shared boid base image").
	// A label is three lines of Dockerfile that anyone can write, so it
	// establishes nothing about an image's origin; what it catches is an
	// override aimed at an image that was never meant to be a boid runner,
	// turning a confusing mid-job failure into a clear launch-time one. Real
	// provenance would be a separate mechanism (signatures, a registry
	// allowlist), not a stricter reading of this check.
	//
	// Until 2026-07-28 nothing baked the label at all, so EVERY override was
	// rejected — including boid's own boid-runner:latest named explicitly —
	// while `container_image` remained a public field on the DB row, the
	// envelope and the spec. The comment here justified that with "safe
	// because containerBackend is not wired into production dispatch as of
	// PR5", a premise the container cutover retired without the conclusion
	// being revisited.
	boidRunnerProtocolLabel   = "boid.runner_protocol"
	boidRunnerProtocolVersion = "v1"

	// composeGatewayServiceName duplicates internal/server's own unexported
	// composeGatewayServiceName constant (PR9, §決定5's daemon-self-connect —
	// see ContainerBackendOptions.SelfContainerID's doc comment). Not
	// imported: internal/server already imports internal/dispatcher, so the
	// reverse would cycle — the same reasoning composeEgressServiceName's
	// own doc comment (above/below in this package) already documents for
	// the identical situation. Must stay in sync with internal/server/
	// server.go's composeGatewayServiceName and build/container/compose.yml's
	// own "boid-gateway" alias.
	composeGatewayServiceName = "boid-gateway"

	// composeBrokerServiceName duplicates internal/server's own unexported
	// composeBrokerServiceName constant, same reasoning as
	// composeGatewayServiceName above (import cycle avoidance) — used only
	// for BOID_BROKER_TLS_SERVER_NAME (withBrokerTLSEnv), the hostname a
	// job container's TLS client hands as its ServerName so certificate
	// hostname verification matches the SAN Server.Start's
	// daemonCA.ServerTLSConfig(..., composeBrokerServiceName) issued the
	// broker's listener cert with. Must stay in sync with internal/server/
	// server.go's composeBrokerServiceName and build/container/compose.yml's
	// own "boid-broker" alias.
	composeBrokerServiceName = "boid-broker"
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
		brokerTLSCA:          opts.BrokerTLSCA,
		brokerTLSAddr:        opts.BrokerTLSAddr,
		runtimeDir:           opts.RuntimeDir,
		transcriptDir:        opts.TranscriptDir,
		selfContainerID:      opts.SelfContainerID,
		sessions:             make(map[string]*containerSession),
	}
	if b.defaultImage == "" {
		// docs/plans/release-onboarding.md 穴4: the fallback image ref is
		// no longer a bare, registry-less "boid-runner:latest" (which a
		// pull would send to docker.io, not GHCR, and fail) — it now
		// tracks this binary's own version identity, so a released daemon
		// pulls its own matching job-container image by default.
		b.defaultImage = version.DefaultContainerImage()
	}
	b.uid, b.gid = defaultContainerUID, defaultContainerGID
	switch {
	// uid == 0 remains a hard reject (決定 4: job containers must be
	// non-root); gid == 0 is a real, honored value as of PR2 (docs/plans/
	// release-onboarding.md 決定1) — see ContainerBackendOptions.UID's doc
	// comment for why the gid-0 case is not "running as root".
	case opts.UID != nil && opts.GID != nil && *opts.UID != 0:
		b.uid, b.gid = *opts.UID, *opts.GID
	case opts.UID != nil || opts.GID != nil:
		// A partial override (only one of the two set) or a uid that
		// resolves to root (uid == 0, regardless of gid) is rejected in
		// favor of the non-root default — see
		// ContainerBackendOptions.UID's doc comment and the PR5 review's
		// Major 1.
		slog.Warn("container backend: rejecting partial or root uid override; using default (§決定 4 requires non-root)",
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

// ContainerBackendHasDiagnosticsCollector reports whether be is a
// containerBackend constructed with a non-nil
// ContainerBackendOptions.DiagnosticsCollector. Exists solely as an
// external-package introspection helper — the same rationale as
// IsContainerBackend's own doc comment — for [Major 7, PR7 codex review]'s
// production wiring test (internal/server/wire_backend_test.go): that
// package can observe that sandboxBackendForConfig actually wired
// NewDefaultDiagnosticsCollector in without being able to name (or
// type-assert into) the unexported *containerBackend/diagnosticsCollector
// fields directly.
func ContainerBackendHasDiagnosticsCollector(be backend.SandboxBackend) bool {
	cb, ok := be.(*containerBackend)
	if !ok {
		return false
	}
	return cb.diagnosticsCollector != nil
}

// ContainerBackendUIDGID returns the effective uid/gid a containerBackend
// launches job containers under (after NewContainerBackend's own
// unset/partial/root-resolving-pair rejection has already run — see
// ContainerBackendOptions.UID's doc comment), and false if be is not a
// *containerBackend. Same external-package introspection rationale as
// ContainerBackendHasDiagnosticsCollector — for
// internal/server/wire_backend_test.go's own pin that
// sandboxBackendForConfig wires the daemon's actual os.Getuid()/os.Getgid()
// through (PR9, §決定4 — see sandboxBackendForConfig's own doc comment for
// why a mismatch here silently broke every job's access to its own
// workspace home directory).
func ContainerBackendUIDGID(be backend.SandboxBackend) (uid, gid int, ok bool) {
	cb, isContainer := be.(*containerBackend)
	if !isContainer {
		return 0, 0, false
	}
	return cb.uid, cb.gid, true
}

// ContainerBackendDefaultImage returns the effective default image a
// containerBackend launches job containers from when a spec carries no
// ContainerImage override (after NewContainerBackend's own
// empty-falls-back-to-version.DefaultContainerImage() resolution has
// already run), and false if be is not a *containerBackend. Same
// external-package introspection rationale as IsContainerBackend's own doc
// comment — used by internal/server/wire_backend_test.go to pin that
// sandboxBackendForConfig threads BOID_IMAGE through (docs/plans/
// release-onboarding.md 穴4/PR4 codex review: the daemon and the job
// containers it launches must agree on the same image ref, or a daemon
// that pulled its own GHCR image successfully can still fail every job
// dispatch trying to pull an unrelated, unqualified "boid-runner:latest"
// from docker.io).
func ContainerBackendDefaultImage(be backend.SandboxBackend) (string, bool) {
	cb, isContainer := be.(*containerBackend)
	if !isContainer {
		return "", false
	}
	return cb.defaultImage, true
}

// ContainerBackendBrokerTLS returns whether be is a *containerBackend
// configured with a non-nil BrokerTLSCA, and the broker TLS address it
// would currently dereference (dereferencing the late-bound
// ContainerBackendOptions.BrokerTLSAddr pointer fresh, "" if the pointer
// itself is nil or points at an empty string). Same external-package
// introspection rationale as ContainerBackendUIDGID/
// ContainerBackendHasDiagnosticsCollector — for
// internal/server/wire_backend_test.go's own pin that sandboxBackendForConfig
// actually wires the daemon's CA + late-binding address pointer into the
// containerBackend it constructs (docs/plans/phase6-cutover-followups.md
// §⓪).
func ContainerBackendBrokerTLS(be backend.SandboxBackend) (addr string, hasCA bool, ok bool) {
	cb, isContainer := be.(*containerBackend)
	if !isContainer {
		return "", false, false
	}
	if cb.brokerTLSAddr != nil {
		addr = *cb.brokerTLSAddr
	}
	return addr, cb.brokerTLSCA != nil, true
}

// ContainerBackendUsesDockerAPI reports whether be is a *containerBackend
// whose engine handle IS api — pointer identity, not merely "both are
// non-nil docker clients". Same external-package introspection rationale as
// ContainerBackendUIDGID/ContainerBackendBrokerTLS.
//
// It exists because the wiring test for [round 2 Major 2, codex review
// 2026-07-26] could not state its own claim from internal/server [round 3
// Minor 2]: buildRuntime must hand the container backend and the
// daemon-state-volume self-inspection (DetectDaemonStateVolumes) THE SAME
// client.New(client.FromEnv) result, and asserting only that each got some
// non-nil *client.Client passes just as happily on a regression that builds
// two — which is exactly the shape round 2 removed, and exactly the shape (a
// second synchronous DOCKER_CERT_PATH read on the startup path) whose cost is
// invisible until a wedged filesystem hangs `boid start`. A nil api never
// matches, so the helper cannot report a shared handle where neither side has
// one.
func ContainerBackendUsesDockerAPI(be backend.SandboxBackend, api any) bool {
	cb, isContainer := be.(*containerBackend)
	if !isContainer || api == nil || cb.api == nil {
		return false
	}
	return cb.api == api
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
	// ContainerLogs [Major 7, PR7 codex review]: consumed by
	// NewDefaultDiagnosticsCollector (container_backend_diagnostics.go) to
	// capture a container's own docker-side log buffer as part of
	// silent-exit diagnosis (§決定8's third primitive) — the attach-stream
	// transcript spool can still be truncated/empty for an OOM-killed or
	// setup-failure container, but dockerd's own log buffer independently
	// retains output up to the moment of a SIGKILL.
	ContainerLogs(ctx context.Context, containerID string, options client.ContainerLogsOptions) (client.ContainerLogsResult, error)

	ImageInspect(ctx context.Context, image string, opts ...client.ImageInspectOption) (client.ImageInspectResult, error)
	ImagePull(ctx context.Context, ref string, options client.ImagePullOptions) (client.ImagePullResponse, error)

	NetworkList(ctx context.Context, options client.NetworkListOptions) (client.NetworkListResult, error)
	NetworkRemove(ctx context.Context, networkID string, options client.NetworkRemoveOptions) (client.NetworkRemoveResult, error)
	// NetworkCreate / NetworkConnect (PR9, §決定5): ensureWorkspaceNetwork's
	// idempotent per-workspace `Internal: true` network + daemon-self-connect
	// (see ContainerBackendOptions.SelfContainerID's doc comment).
	NetworkCreate(ctx context.Context, name string, options client.NetworkCreateOptions) (client.NetworkCreateResult, error)
	NetworkConnect(ctx context.Context, networkID string, options client.NetworkConnectOptions) (client.NetworkConnectResult, error)
	// NetworkInspect backs WorkspaceNetworkCIDRs: the per-workspace network's
	// subnets are assigned by the ENGINE (boid passes no IPAM config to
	// NetworkCreate), so they can only be read back, never computed.
	NetworkInspect(ctx context.Context, networkID string, options client.NetworkInspectOptions) (client.NetworkInspectResult, error)

	// ServerVersion / Info identify the ENGINE behind the socket, not any
	// one container: resolveUsernsMode (container_backend_userns.go) needs
	// to know whether it is talking to rootless podman, which requires job
	// containers be created with a podman-specific userns mode that docker
	// would reject. Probed once per backend and cached.
	ServerVersion(ctx context.Context, options client.ServerVersionOptions) (client.ServerVersionResult, error)
	Info(ctx context.Context, options client.InfoOptions) (client.SystemInfoResult, error)

	VolumeCreate(ctx context.Context, options client.VolumeCreateOptions) (client.VolumeCreateResult, error)
	VolumeList(ctx context.Context, options client.VolumeListOptions) (client.VolumeListResult, error)
	VolumeRemove(ctx context.Context, volumeID string, options client.VolumeRemoveOptions) (client.VolumeRemoveResult, error)
}

// containerWorkspaceNetworkName returns the deterministic docker network
// name a Workspace-scoped Launch (and the runner's own matching
// SetWorkspaceNetwork call for that job's dockerproxy — see runner.go's
// startDockerProxy caller) both compute independently for the SAME
// (installID, workspace) pair (PR9, §決定5's "internal network は workspace
// 単位で分離する").
//
// A thin delegate to internal/dockerres.WorkspaceNetworkName, which now owns
// the naming convention so internal/sandbox/dockerproxy's reserved-namespace
// policy and internal/reap's skip rules key off the exact same prefix
// (docs/plans/workspace-home-volume-persistence.md PR1 §D2). Kept as a
// package-local name because both this file and runner.go call it.
//
// The sanitizeDockerNamePart helper that used to live next to it moved to
// dockerres.SanitizeNamePart wholesale rather than staying behind as a
// delegate: WorkspaceNetworkName was its only caller, so a delegate would be
// dead code and .golangci.yml enables the `unused` linter.
func containerWorkspaceNetworkName(installID, workspace string) string {
	return dockerres.WorkspaceNetworkName(installID, workspace)
}

// isAlreadyConnectedNetworkError reports whether err is a NetworkConnect
// failure caused by this daemon's own container already being a member of
// the target network — the steady state from the SECOND Launch onward for a
// given workspace (every Launch attempts the self-connect; there is no
// first-call/cache distinction), not a genuine connect failure.
//
// The two engines this daemon actually runs against map that SAME condition
// to two DIFFERENT HTTP status codes, discovered by pointing a real
// client.Client at each engine's live socket and inspecting the resulting
// error (not by inference from documentation):
//
//   - docker (daemon/container_operations.go findAndAttachNetwork) responds
//     409, which errhttp.ToNative resolves to errdefs.ErrConflict — plain
//     errdefs.IsConflict is sufficient, and is the SAME classifier
//     ensureWorkspaceNetwork's own NetworkCreate check above uses for the
//     analogous "network already exists" condition.
//   - podman v4.9.3 (pkg/api/handlers/compat/networks.go Connect(), on
//     define.ErrNetworkConnected) instead responds 403, which resolves to
//     errdefs.ErrPermissionDenied — a code errdefs.IsConflict never
//     recognizes. This is NOT symmetric with NetworkCreate: a duplicate
//     podman NetworkCreate returns 201/nil (no error at all), so that
//     check's IsConflict branch is docker-only, and this NetworkConnect
//     check needing an IsPermissionDenied branch is podman-only.
//
// errdefs.IsPermissionDenied alone would be too broad — it would also
// swallow a genuine permission failure (e.g. a plugin denying the connect
// for a real authorization reason) — so the 403 branch additionally
// requires the daemon's own "already connected to network" wording podman
// uses for this condition. docker's 409 message differs ("already attached
// to network"), which is why that message check is scoped to the
// permission-denied branch only; the conflict branch does not need it.
func isAlreadyConnectedNetworkError(err error) bool {
	if errdefs.IsConflict(err) {
		return true
	}
	return errdefs.IsPermissionDenied(err) && strings.Contains(err.Error(), "already connected to network")
}

// ensureWorkspaceNetwork idempotently creates (or confirms) the isolated
// `Internal: true` docker network for workspace, and — when
// b.selfContainerID is configured — connects this daemon's own container to
// it under the gateway/egress/broker DNS aliases a job on that network needs
// (see ContainerBackendOptions.SelfContainerID's doc comment). Returns the
// network name Launch attaches the job container's own NetworkingConfig to.
//
// Fails closed on a genuine NetworkCreate error (anything other than an
// "already exists" conflict — every concurrent/repeat Launch for the same
// workspace hits this path, since there is no first-call/cache
// distinction): §決定5 frames workspace network isolation as a security
// invariant, so Launch must never silently fall back to launching the job
// container unisolated on docker's default network just because ensuring
// its own network failed. A NetworkConnect (self-attach) failure, by
// contrast, only degrades to a logged warning — see its own inline comment
// for why that half is not fail-closed.
func (b *containerBackend) ensureWorkspaceNetwork(ctx context.Context, workspace string) (string, error) {
	netName := containerWorkspaceNetworkName(b.installID, workspace)

	labels := map[string]string{labelWorkspace: workspace}
	if b.installID != "" {
		labels[labelInstallID] = b.installID
	}
	_, err := b.api.NetworkCreate(ctx, netName, client.NetworkCreateOptions{
		Driver:   "bridge",
		Internal: true,
		Labels:   labels,
	})
	if err != nil && !errdefs.IsConflict(err) {
		return "", fmt.Errorf("ensure workspace network %q: %w", netName, err)
	}

	if b.selfContainerID != "" {
		// Best-effort: a failure here only risks THIS daemon's own
		// gateway/egress reachability for jobs on netName (e.g. the
		// endpoint-already-exists steady state after the first Launch for
		// this workspace already connected it) — not a job's isolation
		// from other workspaces, which the NetworkCreate fail-closed check
		// above already guarantees regardless of whether this succeeds.
		//
		// isAlreadyConnectedNetworkError (below) recognizes that steady
		// state across BOTH engines this daemon runs against — see its own
		// doc comment for why a bare errdefs.IsConflict (sufficient for
		// NetworkCreate's "already exists" check above) is not sufficient
		// here. Anything else (engine unreachable, network vanished, a
		// real permission failure, ...) still warns.
		_, cerr := b.api.NetworkConnect(ctx, netName, client.NetworkConnectOptions{
			Container: b.selfContainerID,
			EndpointConfig: &network.EndpointSettings{
				// composeBrokerServiceName (docs/plans/
				// phase6-cutover-followups.md §⓪ "broker TCP wire
				// completion"): added alongside gateway/egress here for
				// the exact same reason those two are — a job container
				// confined to this `Internal: true` workspace network has
				// NO route to the daemon at all unless the daemon ALSO
				// joins this specific network connection under the same
				// alias its BOID_BROKER_TLS_ADDR env (containerBackend.
				// Launch's withBrokerTLSEnv) tells it to dial by name.
				// Missing here (as it was before this fix — this endpoint
				// alias set predates the broker TCP wire followup, PR9's
				// own gateway/egress-only self-connect) meant "boid-broker"
				// resolved fine on the static boid_internal network the
				// daemon is always a member of, but NOT on any
				// PER-WORKSPACE network a real job container actually runs
				// on — found via the real-docker e2e-container CI job:
				// "dial tcp: lookup boid-broker on 127.0.0.11:53: server
				// misbehaving" (docker's embedded per-network DNS resolver
				// has no record for a name that was never aliased on
				// THIS network connection, regardless of the daemon
				// having that alias elsewhere).
				Aliases: []string{composeGatewayServiceName, composeEgressServiceName, composeBrokerServiceName},
			},
		})
		if cerr != nil && !isAlreadyConnectedNetworkError(cerr) {
			slog.Warn("container backend: connect daemon to workspace network failed; jobs on this network may be unable to reach the git gateway/egress proxy/broker",
				"network", netName, "self_container_id", b.selfContainerID, "error", cerr)
		}
	}

	return netName, nil
}

// WorkspaceNetworkCIDRs returns the subnets of workspace's own per-workspace
// network, in CIDR form ("10.89.9.0/24", "fd00:b01d::/64"), for
// applyProxyEnv's no_proxy list — see Runner.Dispatch's
// workspaceNetworkCIDRResolver call site and
// TestDispatch_ContainerBackend_NoProxyIncludesWorkspaceNetworkCIDRs for the
// contract these serve (§決定5's "job → sibling の到達は container IP +
// container port の直アクセス", which every proxy-env-respecting client inside
// the sandbox loses unless its own workspace subnet bypasses HTTP_PROXY).
//
// It goes through ensureWorkspaceNetwork rather than inspecting directly
// because the subnets are engine-assigned at create time: on the first
// dispatch for a workspace the network does not exist yet, and a bare inspect
// would 404 — shipping that job without its own subnet in no_proxy. That call
// is idempotent (Launch makes the identical one for the same pair moments
// later), so pulling it forward costs nothing on every subsequent dispatch.
//
// Only the job's OWN workspace subnets are returned, never every network
// boid manages: no_proxy is a bypass of the egress proxy's refusal, and that
// refusal is exactly what keeps a cross-workspace address unreachable
// (internal/sandbox/proxy.go's isRefusedDotlessTarget). Widening this to
// other workspaces' subnets would hand a job a direct route into them.
func (b *containerBackend) WorkspaceNetworkCIDRs(ctx context.Context, workspace string) ([]string, error) {
	netName, err := b.ensureWorkspaceNetwork(ctx, workspace)
	if err != nil {
		return nil, err
	}
	res, err := b.api.NetworkInspect(ctx, netName, client.NetworkInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("inspect workspace network %q: %w", netName, err)
	}
	var cidrs []string
	for _, cfg := range res.Network.IPAM.Config {
		// An engine can report an IPAM entry with only a Gateway or IPRange
		// set; a zero Prefix would stringify to "invalid Prefix" and land in
		// no_proxy as a garbage entry.
		if cfg.Subnet.IsValid() {
			cidrs = append(cidrs, cfg.Subnet.String())
		}
	}
	return cidrs, nil
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
	// dockerTLSDir / brokerTLSDir / specPath / statePath are all set below
	// but declared here so cleanupFiles's closure sees whichever value
	// each ends up with, even on an early return before any of them is
	// set (the zero value "" makes every guard below a no-op — specPath's
	// own emptiness gates the spec-file cleanup entirely, not just which
	// branch it takes, because materializeDockerClientCert/
	// materializeBrokerClientCert can now fail BEFORE writeContainerSpec
	// has even run — see the reordering this whole function underwent,
	// below).
	var dockerTLSDir string
	var brokerTLSDir string
	var specPath, statePath string
	cleanupFiles := func() {
		if specPath != "" {
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
		}
		if dockerTLSDir != "" {
			_ = os.RemoveAll(dockerTLSDir)
		}
		if brokerTLSDir != "" {
			_ = os.RemoveAll(brokerTLSDir)
		}
	}

	// Per-job dockerproxy / broker client cert delivery (§決定5 /
	// docs/plans/phase6-cutover-followups.md §⓪): materialized and merged
	// into spec.Env HERE — before writeContainerSpec below serializes
	// spec.json, and before realization.Realize copies spec.Env into
	// Realization.Env — rather than as a later `docker create`-only
	// Config.Env addition (an earlier version of this function did
	// exactly that, via the local `env` variable derived from
	// realized.Env). That ordering bug went unnoticed until the broker
	// TCP wire followup's own e2e-container CI run exercised it for the
	// first time against a real container: internal/adapters/shell.
	// Adapter.Run — the harness EVERY plain `command:` hook uses — builds
	// the hook's entire child-process environment from RunContext.Env ==
	// spec.Env, read back out of spec.json INSIDE the container by `boid
	// runner-container` (internal/sandbox/runner/runner_container_linux.go's
	// RunContainer -> readSpec), NOT from the container's own
	// os.Environ() docker create's Config.Env populates — see
	// envSlice's own doc comment (internal/adapters/shell/run.go): "no
	// inheritance from os.Environ()" is a deliberate userns-backend
	// contract (spec.Env is the sandboxed, already-sanitized source of
	// truth there) that turns out to be load-bearing for the container
	// backend too, just for a different reason (no os.Environ() to leak
	// from in the first place — the gap is the OPPOSITE one: a var this
	// backend added only to Config.Env, after spec.json was already
	// written, silently never reached the child at all). The claude/
	// codex/opencode agent adapters DO also inherit os.Environ() (see
	// internal/adapters/claude/run.go's own parentEnv overlay), which is
	// why this went unnoticed for agent-harness jobs — only shell-adapter
	// hooks ever observed the gap, and dockerproxy's own TLS-cert
	// mechanism (the DockerTLSCA branch below) has never actually been
	// wired into production dispatch by any caller yet (see
	// ContainerBackendOptions.DockerTLSCA's own doc comment), so nothing
	// had ever exercised ITS identical gap in a real deployment either
	// until this fix.
	if opts.DockerEnabled && b.dockerTLSCA != nil {
		dir, derr := b.materializeDockerClientCert(opts.JobID)
		if derr != nil {
			cleanupFiles()
			return nil, derr
		}
		dockerTLSDir = dir
		spec.Env = withDockerTLSEnv(spec.Env, b.dockerProxyAddr)
	}
	// Per-job broker client cert delivery (docs/plans/
	// phase6-cutover-followups.md §⓪): unconditional whenever this backend
	// was configured with a CA to issue from
	// (ContainerBackendOptions.BrokerTLSCA — nil for every caller before
	// this feature). Unlike the dockerproxy block above, there is no
	// per-job opts flag gating this: see BrokerTLSCA's own doc comment for
	// why "does this job need broker RPC" is not a meaningful per-job gate
	// the way DockerEnabled is.
	if b.brokerTLSCA != nil {
		dir, berr := b.materializeBrokerClientCert(opts.JobID)
		if berr != nil {
			cleanupFiles()
			return nil, berr
		}
		brokerTLSDir = dir
		var addr string
		if b.brokerTLSAddr != nil {
			addr = *b.brokerTLSAddr
		}
		spec.Env = withBrokerTLSEnv(spec.Env, addr)
	}

	var err error
	specPath, statePath, err = writeContainerSpec(spec, b.runtimeDir)
	if err != nil {
		cleanupFiles()
		return nil, fmt.Errorf("write container sandbox spec: %w", err)
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

	// Workspace network isolation (PR9, §決定5): a non-empty opts.Workspace
	// ensures (idempotently) the per-workspace `Internal: true` docker
	// network exists and computes networkingConfig so the job container
	// created below attaches to it instead of docker's default bridge — see
	// ensureWorkspaceNetwork's own doc comment for the fail-closed
	// rationale. opts.Workspace == "" (every pre-PR9 caller) leaves
	// networkingConfig nil, byte-for-byte the same ContainerCreate call as
	// before this feature.
	var networkingConfig *network.NetworkingConfig
	var workspaceNetworkMode container.NetworkMode
	if opts.Workspace != "" {
		netName, nerr := b.ensureWorkspaceNetwork(ctx, opts.Workspace)
		if nerr != nil {
			cleanupFiles()
			return nil, nerr
		}
		networkingConfig = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{netName: {}},
		}
		// NetworkMode alongside NetworkingConfig (not one or the other):
		// mirrors exactly what `docker run --network <name>` itself sends —
		// belt-and-suspenders against the container instead landing on
		// docker's implicit default bridge IN ADDITION to netName, which
		// would silently defeat §決定5's isolation invariant (the job
		// container would then be reachable from — and able to reach —
		// every other unisolated container on that default bridge, network
		// isolation notwithstanding).
		workspaceNetworkMode = container.NetworkMode(netName)
	}

	mounts, namedVolumes := containerMounts(realized)
	if err := b.ensureNamedVolumes(ctx, namedVolumes, opts.WorkspaceSlug, opts.WorkspaceHomeID, labels); err != nil {
		cleanupFiles()
		return nil, err
	}
	mounts = append(mounts,
		mount.Mount{Type: mount.TypeBind, Source: specPath, Target: containerSpecPath, ReadOnly: true},
		mount.Mount{Type: mount.TypeBind, Source: statePath, Target: containerStatePath},
	)
	// dockerTLSDir / brokerTLSDir bind mounts: the corresponding env vars
	// (DOCKER_*/BOID_BROKER_TLS_*) are already baked into realized.Env —
	// they were merged into spec.Env before realization.Realize ran, above
	// — so only the mount itself (the bind target the child process's
	// DOCKER_CERT_PATH/BOID_BROKER_TLS_CERT_PATH env values point at)
	// remains to be added here.
	if dockerTLSDir != "" {
		mounts = append(mounts, mount.Mount{Type: mount.TypeBind, Source: dockerTLSDir, Target: containerDockerTLSDir, ReadOnly: true})
	}
	if brokerTLSDir != "" {
		mounts = append(mounts, mount.Mount{Type: mount.TypeBind, Source: brokerTLSDir, Target: containerBrokerTLSDir, ReadOnly: true})
	}

	env := realized.Env

	initTrue := true
	pidsLimit := defaultPidsLimit
	hostCfg := &container.HostConfig{
		Init:        &initTrue,
		Mounts:      mounts,
		NetworkMode: workspaceNetworkMode,
		// UsernsMode: "" on every engine but rootless podman — see
		// container_backend_userns.go's resolveUsernsMode. Without it, a
		// rootless-podman job container's uid maps to a host subuid and
		// cannot read any of the Mounts above, starting with its own
		// workspace HOME.
		UsernsMode: b.resolveUsernsMode(ctx),
	}
	hostCfg.Resources.PidsLimit = &pidsLimit

	// dockerCreateWorkingDir (PR9 e2e-container fix): realized.Workdir is
	// spec.WorkDir carried through unchanged — for a clone-visibility job
	// this is the per-project clone TARGET subdirectory itself
	// (sandbox_builder.go's resolveWorkDir/sandboxCloneDir, e.g.
	// "/workspace/myproject"), which is a MountSourceContainerLocal target
	// (realization.go's classifySource): §決定 4 deliberately leaves it
	// unmounted so the clone step creates it fresh inside the container.
	// Only the PARENT ("/workspace", sandboxCloneTargetDir) is baked into
	// the image and chowned to the job uid at build time
	// (build/container/Dockerfile) — the per-project leaf does not exist
	// yet when this ContainerCreate call is made.
	//
	// Passing that not-yet-existing leaf straight through as docker's own
	// `--workdir` hits the exact "missing WORKDIR is created as root,
	// bypassing --user" gotcha this repo's own Dockerfile already
	// documents for its build-time WORKDIR instruction (see that file's
	// comment directly above its `RUN mkdir -p /workspace && chown ...`
	// line) — except here at container-CREATE time: dockerd/runc
	// auto-mkdir's the missing directory as root before the entrypoint
	// process (running as b.uid:b.gid) ever execs, leaving it owned by
	// root with no write access for the job uid. `boid runner-container`'s
	// own clone step (clone.go's performCloneSteps) then fails creating
	// `.git` inside that already-existing, wrongly-owned directory with
	// "permission denied" — reproducibly, on every clone-visibility job,
	// on any host whose docker actually implements this auto-create
	// behavior (confirmed via the e2e-container CI job on ubuntu-24.04's
	// real docker engine; podman instead refuses to start the container at
	// all with "workdir ... does not exist", a different failure mode that
	// masked this on the podman-only dev host — see CLAUDE.md's own note
	// on host docker availability).
	//
	// Rewriting to the always-present, always-correctly-owned parent here
	// is safe: nothing in the container backend's own runtime depends on
	// the container's OS-level starting cwd matching the clone target.
	// RunContainer (runner_container_linux.go) never calls os.Getwd() —
	// every step that needs the real working directory threads
	// realized.Workdir/spec.WorkDir through explicitly as an absolute path
	// instead (clone.go's cs.TargetDir, runner.go's runAgent ->
	// adapters.RunContext.Workspace -> every harness adapter's own
	// `cmd.Dir = rc.Workspace`). So starting the container's own cwd at
	// sandboxCloneTargetDir instead of the not-yet-existing per-project
	// leaf changes nothing boid's own logic observes, and the leaf
	// directory ends up created fresh by the clone step itself — running
	// as the correct (already-matching) job uid — exactly as
	// realization.MountSourceContainerLocal's own doc comment intends
	// ("either it is created fresh inside the container").
	dockerCreateWorkingDir := realized.Workdir
	if dockerCreateWorkingDir == sandboxCloneTargetDir || strings.HasPrefix(dockerCreateWorkingDir, sandboxCloneTargetDir+"/") {
		dockerCreateWorkingDir = sandboxCloneTargetDir
	}

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
		WorkingDir:   dockerCreateWorkingDir,
		Tty:          realized.TTY,
		OpenStdin:    true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		User:         fmt.Sprintf("%d:%d", b.uid, b.gid),
		Labels:       labels,
	}

	createRes, err := b.api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:           cfg,
		HostConfig:       hostCfg,
		NetworkingConfig: networkingConfig,
		Name:             containerName(opts.JobID),
	})
	if err != nil {
		cleanupFiles()
		return nil, fmt.Errorf("container create: %w", err)
	}

	sess := newContainerSession(b, createRes.ID, realized.TTY, specPath, dockerTLSDir, brokerTLSDir)
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
		b.removeHalfBuiltContainer(ctx, createRes.ID)
		cleanupFiles()
		return nil, fmt.Errorf("open transcript spool: %w", spoolErr)
	}
	sess.transcriptFile, sess.transcriptPath = spoolFile, spoolPath
	if err := sess.attach(ctx, false); err != nil {
		// [Major 10, PR7 codex review]: close the spool file on this error
		// path too — without it, every Launch that reaches here (attach
		// failing after the spool was already opened) leaked one fd. The
		// normal exit path's own close (waitLoop) never runs because
		// waitLoop is only started by sess.start(), below, which this
		// return never reaches.
		if sess.transcriptFile != nil {
			_ = sess.transcriptFile.Close()
		}
		b.removeHalfBuiltContainer(ctx, createRes.ID)
		cleanupFiles()
		return nil, fmt.Errorf("container attach: %w", err)
	}

	if _, err := b.api.ContainerStart(ctx, createRes.ID, client.ContainerStartOptions{}); err != nil {
		sess.closeConn()
		// [Major 10, PR7 codex review]: same fd-leak fix as the attach error
		// path above.
		if sess.transcriptFile != nil {
			_ = sess.transcriptFile.Close()
		}
		b.removeHalfBuiltContainer(ctx, createRes.ID)
		cleanupFiles()
		return nil, fmt.Errorf("container start: %w", err)
	}

	sess.start()
	b.registerSession(sess)
	return sess, nil
}

// removeHalfBuiltContainer tears down a container Launch created but could not
// finish wiring up (spool open, attach, or start failed).
//
// The removal deliberately runs outside the caller's cancellation — a Launch
// cancelled mid-flight is exactly when the teardown matters most — and, since
// the codex round-2 review of PR8 (Major 3), under a bound as well. Each of
// these three sites used to pass a bare context.Background(), which supplies the
// first property and not the second: against an engine socket that accepts a
// request and never answers, Launch would never return the error it already had,
// and (since PR8) never release the workspace's in-flight home registration
// either, so every later `boid workspace import-home` for that workspace would
// be refused with a message about a job that is not running. See
// containerCleanupContext.
//
// Force is set for the same reason it always was here: the container may be
// created, created-and-attached, or created-attached-and-started depending on
// which branch got here, and an unforced remove refuses a running one.
func (b *containerBackend) removeHalfBuiltContainer(ctx context.Context, id string) {
	cleanupCtx, cancel := containerCleanupContext(ctx)
	defer cancel()
	if _, err := b.api.ContainerRemove(cleanupCtx, id, client.ContainerRemoveOptions{Force: true}); err != nil {
		slog.Warn("container backend: could not remove a half-built container after a failed launch; it is left for the next daemon start's orphan sweep",
			"container_id", id, "error", err)
	}
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
		// Best-effort re-attach on every cache hit (Opus review of PR #857):
		// see reattachIfLost's own doc comment for the full rationale. This
		// is a near-no-op for the overwhelming majority of cache hits (a
		// session that has been attached and streaming the whole time) —
		// reattachIfLost's own first check (running && !attached) returns
		// immediately without touching the engine. It only does real I/O
		// for the rare running-but-not-attached case: doAdopt's own attach
		// failed when this session was first adopted (the engine was
		// unreachable at the time), or a later attach dropped mid-stream.
		// Without this, that state was permanent until the daemon
		// restarted — no cache hit ever gave it a second chance to recover.
		sess.reattachIfLost(ctx)
		return sess, true
	}
	if attempt, inFlight := b.adopting[runtimeID]; inFlight {
		b.mu.Unlock()
		// select on ctx too (Opus review of PR #857, Major 1): a caller that
		// passed a bounded ctx specifically so it would not hang forever
		// (StopJobRuntime, SignalJobRuntime, the runtime_subscriber_export.go
		// ingress points) would otherwise wait on <-attempt.done alone, which
		// only resolves when the FIRST caller's own doAdopt returns — itself
		// unbounded whenever that first caller passed context.Background()
		// (every one of those call sites did, pre-fix). Against a wedged
		// engine the owning attempt's ContainerInspect never returns, so
		// every later joiner — bounded ctx or not — hung right alongside it.
		select {
		case <-attempt.done:
		case <-ctx.Done():
			return nil, false
		}
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
//
// A failed attach here (this function's own ContainerAttach call — e.g. the
// engine answers ContainerInspect but has since gone slow or unreachable
// again before the attach round trip) is deliberately NOT fatal: the
// resulting session is still returned (Adopt caches it and reports ok=true)
// so signal/stop/wait keep working. It used to also be PERMANENT — the only
// way a session in this state ever got a working stream was a full daemon
// restart, because nothing ever attempted a second attach — which is
// exactly the bug Opus's review of PR #857 found: Subscribe() answered
// ok=true (checking only running, not whether anything was actually
// attached) with a channel that would then never receive a single byte, no
// matter how quickly the engine recovered. That permanence is gone now:
// Adopt's cache-hit path calls sess.reattachIfLost on every later Adopt for
// this runtimeID, so once the engine is reachable again the very next
// cache-hit re-establishes the stream — see reattachIfLost's and
// Subscribe's own doc comments for the two halves of the fix.
func (b *containerBackend) doAdopt(ctx context.Context, runtimeID string) *containerSession {
	insp, err := b.api.ContainerInspect(ctx, runtimeID, client.ContainerInspectOptions{})
	if err != nil || insp.Container.State == nil || !insp.Container.State.Running {
		return nil
	}

	tty := insp.Container.Config != nil && insp.Container.Config.Tty
	sess := newContainerSession(b, runtimeID, tty, "", "", "")
	if err := sess.attach(ctx, true); err != nil {
		slog.Warn("container backend: adopt attach failed; session will support signal/stop/wait only until a later cache-hit Adopt successfully re-attaches",
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
// elsewhere (resource labeling degrades the same way).
//
// Volumes and networks are reaped by the same install-scoped logic, with one
// exception. The workspace network sweep is unconditional — a per-workspace
// network is recreated on demand by ensureWorkspaceNetwork, so destroying it
// costs nothing. The volume sweep is NOT: reapOrphanVolumes now preserves
// persistent workspace HOME volumes (docs/plans/workspace-home-volume-persistence.md
// 論点 a 経路 2). The older note here — that the volume/network loops were
// "forward-compat scaffolding, not exercised by real traffic" because
// workspace HOME stayed a host bind through Phase 6 (§決定 4) — no longer
// holds: workspace networks have been real traffic since PR9, and PR6 of the
// workspace-HOME plan makes the volume loop real too, which is exactly why
// its exclusion rule has to be in place first.
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
	// A separate enumeration, on a separate label, contributing nothing to
	// `report` — see reapOrphanWorkspaceInitContainers' own doc comment
	// (docs/plans/workspace-home-volume-persistence.md 論点 c §D5). It cannot
	// share `filters` above: the whole reason a workspace init container does
	// not carry boid.job_id is that the sweep above would force-remove it
	// mid-toolchain-install and fold a non-existent job id into the accounting
	// that gates other tasks' auto-reopen.
	b.reapOrphanWorkspaceInitContainers(ctx)

	// [Major 6, PR7 codex review]: dockerproxy's sibling child resources
	// (created by the *client* inside a job's sandbox — docker CLI,
	// TestContainers, ... — never by this backend directly, when the job
	// declared capabilities.docker) carry NO boid label at all, so the
	// label-based sweep above can never find them: they are only
	// discoverable via the per-job docker-resources.jsonl ledger under
	// runtimeDir (§決定8). internal/reap.Run — the same daemon-independent
	// logic `boid reap` uses (§決定6's "label ∪ ledger union") — is run here
	// as an additional best-effort pass so startup reap catches these too,
	// not just the primary job containers the loop above already handled
	// (those are already gone by the time this call lists again via its own
	// label query, so this is not double-destroying anything — merely one
	// extra API round trip). b.api's method set is a strict superset of
	// reap.Run's own narrow dockerAPI interface, so no adapter is needed.
	// Errors are logged, not folded into ReapReport: a ledger-cleanup
	// failure here is a docker-resource leak, not a reason to block a
	// task's auto-reopen (ReapReport's own job-level contract — see its doc
	// comment; only the primary-container loop above feeds FailedJobIDs).
	if b.runtimeDir != "" {
		if _, rerr := reap.Run(ctx, b.api, b.installID, b.runtimeDir, reap.PreserveWorkspaceHomes); rerr != nil {
			slog.Warn("container backend: reap.Run ledger-union pass failed", "error", rerr)
		}
	}

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

// reapOrphanVolumes destroys this installation's job-labeled volumes, with
// one class deliberately excluded: the persistent per-workspace HOME volumes
// (docs/plans/workspace-home-volume-persistence.md 論点 a 経路 2). Those hold
// harness authentication and a multi-GB toolchain — state boid cannot
// regenerate — and this sweep runs on every daemon startup, so failing to
// exclude them means a restart silently de-authenticates every workspace.
//
// The exclusion is checked two independent ways (defense in depth), either
// of which alone is sufficient:
//
//   - the NAME is in the boid-ws-home- namespace. Note this is the NARROW
//     prefix: the wider reserved "boid-ws-" namespace also covers the
//     per-workspace network names, and a volume that merely shares that
//     prefix is still reaped, since networks are regenerable (see
//     internal/dockerres's package doc).
//   - the boid.workspace_home label is PRESENT, so a volume created under a
//     future/legacy naming scheme is still recognized. Presence, not a
//     non-empty value: ensureNamedVolumes sets this key to opts.Workspace,
//     which is empty for the DI/test wiring that never supplies a workspace,
//     and a value check would drop exactly those volumes back out of the
//     protected set while the doc claimed otherwise (PR1 codex review,
//     Minor). The fail-safe reading of "boid.workspace_home is set at all" is
//     "this is a workspace HOME volume".
func (b *containerBackend) reapOrphanVolumes(ctx context.Context, filters client.Filters) {
	listRes, err := b.api.VolumeList(ctx, client.VolumeListOptions{Filters: filters})
	if err != nil {
		slog.Warn("container backend: list orphan volumes failed", "error", err)
		return
	}
	for _, v := range listRes.Items {
		_, hasHomeLabel := v.Labels[dockerres.LabelWorkspaceHome]
		if dockerres.IsWorkspaceHomeVolumeName(v.Name) || hasHomeLabel {
			slog.Debug("container backend: preserving workspace HOME volume during orphan sweep",
				"volume", v.Name, "workspace", v.Labels[dockerres.LabelWorkspaceHome])
			continue
		}
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
// (for an override only) §決定 11's runner-protocol label check. A single
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

	// Arch mismatch fail-fast (docs/plans/release-onboarding.md 決定5's
	// arm64 論点, required regardless of whether an arm64 image is ever
	// published): a host with binfmt/qemu registered silently runs a
	// foreign-arch image under emulation instead of refusing it outright
	// — no error, just extreme slowness and unexplained crashes that look
	// nothing like an architecture problem.
	//
	// [Blocker, PR4 codex review]: this must compare against the ENGINE's
	// own reported host architecture (b.resolveHostArch, docker/podman
	// /info), never runtime.GOARCH — GOARCH names the architecture THIS
	// GO BINARY was compiled for, which is meaningless as a "real machine"
	// signal from inside a container that is itself already running under
	// emulation (an amd64-compiled daemon binary, baked into an amd64
	// image, still reports GOARCH=="amd64" when QEMU is emulating that
	// entire image on genuinely arm64 hardware — comparing against GOARCH
	// would silently pass exactly the case this check exists to catch, and
	// would ALSO wrongly reject a host-native arm64 override image on that
	// same emulated host). dockerd/podman itself always runs natively on
	// the real host — only individual containers may be emulated — so its
	// own /info response is the one honest source for "what architecture
	// is this machine actually".
	//
	// insp.Architecture / hostArch are only compared when BOTH are
	// non-empty (the fakeDockerAPI test default for either, and any real
	// engine's honest answer for an image whose manifest genuinely lacks
	// platform metadata, or an Info() probe that failed) so this cannot
	// misfire against a legitimately unknown answer — it only fires when
	// the image POSITIVELY claims a platform that doesn't match a
	// POSITIVELY known host arch.
	if hostArch := b.resolveHostArch(ctx); insp.Architecture != "" && hostArch != "" && insp.Architecture != hostArch {
		return "", fmt.Errorf(
			"container image %q is built for arch %q, but this host is %q — refusing to run it under binfmt/qemu emulation (silently works but is extremely slow and can crash in ways that look nothing like an architecture problem); use an image built for %[3]s",
			image, insp.Architecture, hostArch)
	}

	if override != "" {
		got := ""
		if insp.Config != nil {
			got = insp.Config.Labels[boidRunnerProtocolLabel]
		}
		if got != boidRunnerProtocolVersion {
			return "", fmt.Errorf(
				"container image override %q rejected: %s label = %q, want %q — an override image must declare the runner protocol it speaks, which the boid base image does (build/container/Dockerfile); build yours FROM it, or add the same LABEL if you assemble the runner yourself (§決定 11)",
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
// reference, before ContainerCreate would otherwise auto-create it unlabeled
// (PR5 review Major 6). Which labels it applies depends on WHICH volume it
// is, because the two classes have opposite lifecycle requirements
// (docs/plans/workspace-home-volume-persistence.md 論点 a 経路 3):
//
//   - a JOB volume is ephemeral and must be found by the reapers, so it
//     carries jobLabels (boid.job_id / boid.workspace / boid.install_id).
//     reapOrphanVolumes enumerates on boid.job_id; an unlabeled volume would
//     leak forever, which is the gap Major 6 closed.
//   - a WORKSPACE HOME volume is persistent and must NOT be found by them,
//     so it carries only boid.workspace_home (+ boid.workspace_home_install_id
//     for the install scoping boid.install_id would otherwise provide). Every
//     one of the three job labels is itself an enumeration filter: with
//     boid.install_id it would be force-removed by reap.Run on every daemon
//     startup, with boid.job_id by reapOrphanVolumes. "Correctly labeled"
//     here means "invisible to the sweeps", the exact inverse of the job case.
//
// workspaceSlug is the slug recorded in the workspace-home label. It is
// Launch's opts.WorkspaceSlug, NOT opts.Workspace, and PR6 (論点 D5) is where
// that distinction started to matter. opts.Workspace is the raw
// project.WorkspaceID — empty for every project with no explicit workspace
// assignment — while the volume NAME is built from the slug
// resolveWorkspaceHome normalized that value into, so "" becomes "default".
// Labelling the volume from the raw field would leave a volume called
// boid-ws-home-<install8>-default carrying boid.workspace_home="", which no
// lookup by slug can find. The reapers are unaffected (both test the label's
// PRESENCE, deliberately — 論点 a), but 論点 a-2's workspace-remove and
// orphan-detection rewiring in PR7 is a lookup by value.
//
// opts.Workspace keeps its own meaning untouched (the boid.workspace job label
// and the per-workspace network name); normalizing THAT would change which jobs
// get an isolated network, which is a different decision and not PR6's.
//
// A workspace HOME volume created HERE is also given a freshly minted identity
// (dockerres.LabelWorkspaceHomeID). Normally resolveWorkspaceHome created it
// long before Launch runs and this create is a no-op that changes nothing — but
// the volume can be removed in the window between the two, and an unlabelled
// one left behind here would make every later dispatch of that workspace fail
// (Runner.ensureWorkspaceHomeVolume, which cannot repair a missing label
// because the Engine API has no way to add one). Minting keeps the invariant
// "every workspace HOME volume boid brings into existence carries an identity"
// true on BOTH creation paths, and the mismatch with the completion marker
// resolves itself with one re-init.
//
// # workspaceHomeID: what the create ANSWERED with is checked, not just made
//
// The identity is minted FRESH here even though the caller knows which one it
// resolved (opts.WorkspaceHomeID, threaded down as workspaceHomeID), and that
// is the load-bearing part rather than an oversight. Stamping the resolved
// identity onto a volume this call had to create would make the empty
// replacement indistinguishable from the real home — the label would match by
// construction, and the mismatch that makes a vanished home detectable at all
// would be erased by the very code meant to detect it.
//
// So: mint a fresh candidate, then compare what came back against
// workspaceHomeID. VolumeCreate returns an EXISTING volume with its own labels
// and discards the request's, so a surviving home answers with the resolved
// identity and matches, while a volume that was removed (this call created it)
// or replaced (somebody else created it) answers with something else and fails
// Launch. See verifyWorkspaceHomeIdentity for why the check has to happen here,
// at the point of use, and for the window it narrows without closing.
//
// An EMPTY workspaceHomeID means the caller resolved no home for this launch,
// which production never does — Runner.Dispatch resolves the home before it
// builds the spec that mounts it — but DI/test wiring that calls Launch
// directly does. That case keeps the pre-check behaviour (mint, create, do not
// compare): there is no expectation to compare against, and refusing would turn
// "this test did not go through a resolve" into a Launch failure while
// protecting nothing.
//
// Each name is validated against docker's volume-name grammar first. The
// container backend classifies a mount source as a named volume purely by
// "it does not start with /" (internal/sandbox/realization.classifySource —
// the convention 論点 e's option (i) deliberately reuses rather than adding a
// MountType), so a relative path that lands in the mount list by accident
// would otherwise be handed to the engine as a volume name. Failing closed
// here surfaces it as a Launch error naming the offending source instead.
//
// VolumeCreate is idempotent (Docker returns the existing volume, unchanged,
// for an already-existing name — it does not error, and it does NOT apply
// the request's labels to an existing volume, since the API has no
// volume-label-update endpoint), so this is safe to call on every Launch.
// An already-existing JOB volume that predates Major 6's fix (no boid.job_id
// label) is left as-is rather than deleted-and-recreated, which would be
// destructive to whatever it holds; a warning is logged instead so the reap
// gap is at least visible. That warning does NOT apply to workspace HOME
// volumes: for them, having no boid.job_id label is the correct and intended
// state, so warning about it would be noise that trains operators to ignore
// the message that matters.
func (b *containerBackend) ensureNamedVolumes(ctx context.Context, names []string, workspaceSlug, workspaceHomeID string, jobLabels map[string]string) error {
	for _, name := range names {
		if !dockerres.IsValidVolumeName(name) {
			return fmt.Errorf("mount source %q is not a valid docker volume name "+
				"(a relative path reached the named-volume classification; mount sources must be absolute host paths or valid volume names)", name)
		}

		isWorkspaceHome := dockerres.IsWorkspaceHomeVolumeName(name)
		labels := jobLabels
		if isWorkspaceHome {
			candidate, err := newWorkspaceHomeID()
			if err != nil {
				return fmt.Errorf("create named volume %q: mint home id: %w", name, err)
			}
			labels = map[string]string{
				dockerres.LabelWorkspaceHome:   workspaceSlug,
				dockerres.LabelWorkspaceHomeID: candidate,
			}
			if b.installID != "" {
				labels[dockerres.LabelWorkspaceHomeInstallID] = b.installID
			}
		}

		res, err := b.api.VolumeCreate(ctx, client.VolumeCreateOptions{Name: name, Labels: labels})
		if err != nil {
			return fmt.Errorf("create named volume %q: %w", name, err)
		}
		if isWorkspaceHome {
			if err := verifyWorkspaceHomeIdentity(name, workspaceHomeID, res.Volume.Labels[dockerres.LabelWorkspaceHomeID]); err != nil {
				return err
			}
			continue
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

// materializeBrokerClientCert issues a fresh per-job broker client
// certificate from b.brokerTLSCA and writes the cert/key/ca PEM trio to a
// host directory, in the same cert.pem/key.pem/ca.pem layout
// materializeDockerClientCert uses (docs/plans/phase6-cutover-followups.md
// §⓪). The caller bind-mounts the returned directory read-only into the
// container at containerBrokerTLSDir; containerSession.waitLoop removes it
// once the container exits (mirroring dockerTLSDir's own always-cleaned-up
// retention contract — see containerSession.brokerTLSDir's doc comment).
//
// "job-broker-"+jobID (not just jobID, matching materializeDockerClientCert's
// own "job-"+jobID convention) gives this leaf a certificate CN visibly
// distinct from a docker-proxy cert issued for the very same job, in case
// both ever need to be told apart in a log or a future per-request identity
// check (neither this backend nor the broker inspects CN today — see
// mtls.CA's own package doc comment on the state of per-job client identity
// binding — but the two leaves being trivially distinguishable costs
// nothing and avoids ambiguity later).
func (b *containerBackend) materializeBrokerClientCert(jobID string) (dir string, err error) {
	leaf, err := b.brokerTLSCA.IssueShortLivedClientCert("job-broker-"+jobID, perJobBrokerCertValidity)
	if err != nil {
		return "", fmt.Errorf("issue broker client cert: %w", err)
	}
	certPEM, keyPEM, err := mtls.EncodeCertPEM(leaf)
	if err != nil {
		return "", fmt.Errorf("encode broker client cert: %w", err)
	}

	dir, err = b.brokerTLSCertDir(jobID)
	if err != nil {
		return "", err
	}

	files := map[string][]byte{
		brokerCertFileName: certPEM,
		brokerKeyFileName:  keyPEM,
		brokerCAFileName:   b.brokerTLSCA.CertPEM(),
	}
	for name, data := range files {
		// 0600: same rationale as materializeDockerClientCert's identical
		// write — the private key lives in this same directory.
		if werr := os.WriteFile(filepath.Join(dir, name), data, 0o600); werr != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("write %s: %w", name, werr)
		}
	}
	return dir, nil
}

// brokerTLSCertDir returns (creating it if necessary) the directory
// materializeBrokerClientCert writes jobID's cert.pem/key.pem/ca.pem trio
// into — the broker-side analogue of dockerTLSCertDir, same b.runtimeDir
// set/unset split and same DooD host-visibility rationale (see its own doc
// comment).
func (b *containerBackend) brokerTLSCertDir(jobID string) (string, error) {
	if b.runtimeDir == "" {
		dir, err := os.MkdirTemp("", "boid-"+jobID+"-broker-tls-")
		if err != nil {
			return "", fmt.Errorf("create broker tls cert dir: %w", err)
		}
		return dir, nil
	}
	dir := filepath.Join(b.runtimeDir, "broker-tls", jobID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create broker tls cert dir: %w", err)
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
	// transcriptDir, when set, is a persistent root distinct from runtimeDir
	// (see ContainerBackendOptions.TranscriptDir's doc comment) — falls back
	// to runtimeDir when unset, unchanged from before this field existed.
	spoolRoot := b.transcriptDir
	if spoolRoot == "" {
		spoolRoot = b.runtimeDir
	}
	if spoolRoot == "" {
		return nil, "", nil
	}
	dir := filepath.Join(spoolRoot, containerID)
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
// key in env is carried through unchanged; env itself is never mutated.
//
// Called on spec.Env directly (Launch reassigns spec.Env = withDockerTLSEnv(
// spec.Env, ...) BEFORE writeContainerSpec/realization.Realize run), not on
// realized.Env after the fact — see Launch's own doc comment on why that
// ordering is load-bearing: internal/adapters/shell.Adapter.Run builds the
// hook child process's entire environment from spec.Env (read back out of
// spec.json inside the container), not from the container's own
// os.Environ() a later Config.Env-only addition would have landed in.
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

// withBrokerTLSEnv returns a copy of env with the five BOID_BROKER_TLS_*
// variables internal/sandbox/brokerclient.SendJSONFromEnv reads to select
// and authenticate a TCP+mTLS broker connection added — the broker-side
// analogue of withDockerTLSEnv, docs/plans/phase6-cutover-followups.md §⓪.
//
// Deliberately does NOT touch BOID_BROKER_SOCKET: that key (when present at
// all) comes from a completely separate mechanism — sandbox_builder.go's
// generic BrokerSocket bind-mount, applied identically for both backends —
// and SendJSONFromEnv already prefers BOID_BROKER_TLS_ADDR over
// BOID_BROKER_SOCKET whenever both are set (see its own doc comment), so
// there is nothing to gain and a real risk in this function reaching into a
// key it does not own: a future container-backend-only fix to that other
// mount's correctness must not have to remember this function also writes
// the same key.
//
// addr may be empty (b.brokerTLSAddr's pointer was nil, or pointed at an
// empty string — Server.Start has not yet bound the broker's TLS listener,
// a construction-ordering case that should not happen in production wiring
// but is not this function's job to validate) — BOID_BROKER_TLS_ADDR is set
// to the empty string in that case rather than omitted, so a job that hits
// this is loud (SendJSONFromEnv's own dial attempt fails immediately with a
// clear "empty address" error) instead of silently falling back to a
// stale/wrong transport.
//
// Called on spec.Env directly, same ordering rationale as withDockerTLSEnv's
// own doc comment (Launch reassigns spec.Env = withBrokerTLSEnv(spec.Env,
// ...) before writeContainerSpec/realization.Realize run) — this is what
// makes BOID_BROKER_TLS_ADDR actually visible to a shell-adapter hook's
// `boid task update --payload-patch`/`boid job done` calls, not just to the
// container's own os.Environ().
func withBrokerTLSEnv(env map[string]string, addr string) map[string]string {
	out := make(map[string]string, len(env)+5)
	for k, v := range env {
		out[k] = v
	}
	out["BOID_BROKER_TLS_ADDR"] = addr
	out["BOID_BROKER_TLS_CERT_PATH"] = containerBrokerTLSDir + "/" + brokerCertFileName
	out["BOID_BROKER_TLS_KEY_PATH"] = containerBrokerTLSDir + "/" + brokerKeyFileName
	out["BOID_BROKER_TLS_CA_PATH"] = containerBrokerTLSDir + "/" + brokerCAFileName
	out["BOID_BROKER_TLS_SERVER_NAME"] = composeBrokerServiceName
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
	// brokerTLSDir is the per-job cert directory materializeBrokerClientCert
	// wrote (docs/plans/phase6-cutover-followups.md §⓪), removed alongside
	// specPath/dockerTLSDir once the container exits. Empty whenever no
	// ContainerBackendOptions.BrokerTLSCA was configured — every session
	// before this feature, and any deployment (including every unit test)
	// that never wires one in.
	brokerTLSDir string

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

	connMu sync.Mutex
	hijack *client.HijackedResponse
	// stdinCloseOnce guards CloseInput's half-close against the CURRENT
	// generation's hijack, and is itself replaced with a fresh *sync.Once
	// every time attach() installs a new one (Opus review of PR #864,
	// N6). A single session-lifetime sync.Once here (the pre-fix shape)
	// would only ever half-close the FIRST generation's connection: once
	// CloseInput fired for generation 1, its Once.Do would never run its
	// function body again, so a caller that called CloseInput before a
	// mid-stream drop — then relies on it again after reattachIfLost
	// installs generation 2 — would silently get a no-op forever, with no
	// error to signal it. A pointer field (not a plain sync.Once value)
	// so CloseInput can snapshot the CURRENT generation's *sync.Once under
	// connMu and call Do on that local copy outside the lock, exactly the
	// same pattern this type already uses for hijack itself.
	stdinCloseOnce *sync.Once

	mu          sync.Mutex
	transcript  []byte
	subscribers map[int]chan []byte
	nextSubID   int
	running     bool
	exit        backend.RuntimeExit
	// attached reports whether the session currently has a live attach
	// connection with its own readLoop actively feeding appendTranscript —
	// i.e. whether there is anything for Subscribe to hand a new caller a
	// channel onto. This is INDEPENDENT of running (§決定 7's container-exit
	// state): doAdopt's own best-effort attach (its doc comment) can leave
	// behind a session that is running (ContainerWait is still blocked, the
	// container is genuinely alive) but not attached (the attach call itself
	// failed — e.g. the engine was unreachable at adoption time), and until
	// this field existed Subscribe() only ever checked running, so it
	// answered ok=true with a channel that would never receive anything
	// (Opus review of PR #857): the Web UI/WS ingress saw "connected" and
	// went silent forever, no matter how long the caller waited or how
	// quickly the engine recovered — only a daemon restart (which forces a
	// fresh doAdopt) could ever fix it. See Subscribe's and
	// reattachIfLost's own doc comments for the two halves of the fix: (1)
	// Subscribe now requires both running AND attached before returning
	// ok=true, so ingress gets an honest error instead of a dead channel,
	// and (2) Adopt's cache-hit path best-effort re-attaches a running,
	// not-yet-attached session so a later Subscribe can recover once the
	// engine comes back, without waiting for a daemon restart.
	//
	// Set true the moment attach() stores a fresh hijack (before its
	// readLoop goroutine even starts — so a Subscribe racing right after
	// Launch/doAdopt/reattachIfLost never sees a false negative), and false
	// again the moment attach() fails synchronously OR readLoop's own defer
	// runs (the stream — whatever ended it, container exit or a dropped
	// connection — is no longer live). Guarded by mu (not connMu) precisely
	// so Subscribe can read it atomically alongside running under one lock,
	// the same way it already read running alone before this field existed.
	attached bool
	// reattaching is non-nil exactly while one goroutine's best-effort
	// re-attach (reattachIfLost) is in flight, so concurrent cache-hit
	// Adopt callers for the SAME session (multiple WS ingress racing a
	// cache hit right as the engine recovers) share its outcome instead of
	// each firing their own ContainerAttach — the session-scoped analogue
	// of containerBackend.adopting's backend-scoped dedup for cache-miss
	// doAdopt calls. Closed (and reset to nil) once that one attempt
	// returns; every other concurrent caller just waits on it.
	reattaching chan struct{}

	done chan struct{}
	// readDone is the CURRENT attach generation's completion signal: each
	// call to attach() — the session's first (Launch/doAdopt) or a later
	// best-effort reattachIfLost retry — allocates and stores a brand new
	// channel here rather than reusing whatever the field already held.
	// That is deliberate, not an oversight: a naive design that kept one
	// readDone for the session's whole lifetime would panic the first time
	// a re-attach ran (close of an already-closed channel — the previous
	// generation's attach() error path or readLoop defer already closed
	// it), which is exactly the failure mode a first-draft fix for this
	// hit. Read together with `attached` under mu (see waitLoop's own
	// drain-select, which snapshots both under one lock before deciding
	// whether there is anything to drain at all) rather than referenced
	// directly, so a concurrent attach()/reattachIfLost swapping this
	// field never races waitLoop's own read of it.
	readDone chan struct{}
}

var _ backend.SandboxSession = (*containerSession)(nil)

func newContainerSession(b *containerBackend, id string, tty bool, specPath, dockerTLSDir, brokerTLSDir string) *containerSession {
	sess := &containerSession{
		backend:      b,
		id:           id,
		api:          b.api,
		tty:          tty,
		specPath:     specPath,
		dockerTLSDir: dockerTLSDir,
		brokerTLSDir: brokerTLSDir,
		subscribers:  make(map[int]chan []byte),
		running:      true,
		done:         make(chan struct{}),
		// readDone is deliberately NOT initialized here (Opus review of PR
		// #864, N7): both callers of this constructor (Launch, doAdopt)
		// always call attach() — which unconditionally sets readDone,
		// success or failure — before start() ever launches the waitLoop
		// goroutine that is readDone's only reader. A placeholder channel
		// here would be dead code (never read before being overwritten),
		// which is exactly what the pre-fix version of this field was.
		stdinCloseOnce: &sync.Once{},
	}
	if specPath != "" && b.runtimeDir != "" {
		sess.specDir = filepath.Dir(specPath)
	}
	return sess
}

func (s *containerSession) ID() string { return s.id }

// attach establishes the session's docker-attach connection for the CURRENT
// generation and starts a read loop that feeds appendTranscript. withLogs
// replays already-produced output before switching to the live stream —
// Adopt's post-restart recovery path (the closest this backend gets to a
// separate `docker logs` call); Launch passes false since nothing has been
// produced yet at create time.
//
// This is called more than once over a session's lifetime whenever the
// FIRST attach attempt fails: doAdopt's own best-effort attach (its doc
// comment) and reattachIfLost's later best-effort retry both funnel through
// here. Each call allocates a brand new readDone channel rather than
// reusing whatever s.readDone already held — see that field's own doc
// comment for why reusing it would panic (close of an already-closed
// channel) the moment a second generation's own close ran. attached is set
// (true on success, false on failure) in the SAME locked section as
// readDone, so a concurrent Subscribe or waitLoop reading both together
// under mu (see their own doc comments) never observes one updated without
// the other.
func (s *containerSession) attach(ctx context.Context, withLogs bool) error {
	readDone := make(chan struct{})

	result, err := s.api.ContainerAttach(ctx, s.id, client.ContainerAttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
		Logs:   withLogs,
	})
	if err != nil {
		close(readDone)
		s.mu.Lock()
		s.readDone = readDone
		s.attached = false
		s.mu.Unlock()
		return err
	}
	hijack := result.HijackedResponse
	s.connMu.Lock()
	staleHijack := s.hijack
	s.hijack = &hijack
	// A fresh *sync.Once per generation (N6): see stdinCloseOnce's own
	// doc comment for why reusing the previous generation's Once here
	// would permanently no-op CloseInput after the very first call.
	s.stdinCloseOnce = &sync.Once{}
	s.connMu.Unlock()
	// A stale hijack here (Opus review of PR #864, N1) is a PREVIOUS
	// generation's connection that reattachIfLost is replacing: its own
	// readLoop has already exited (attach()/reattachIfLost's gate
	// guarantees this attach call only runs when attached is currently
	// false), so nothing is still reading from it — closing it is safe
	// and does not race the new generation this call just installed.
	// Without this, every mid-stream-drop reattach leaked one fd (and the
	// moby HijackedResponse's own buffered *bufio.Reader) until process
	// exit or the Go net.Conn's finalizer happened to run. nil for every
	// session's FIRST attach (Launch/doAdopt) — closeConn's own nil check
	// makes that a no-op, not a special case here.
	if staleHijack != nil {
		staleHijack.Close()
	}

	s.mu.Lock()
	s.readDone = readDone
	s.attached = true
	s.mu.Unlock()

	go s.readLoop(&hijack, readDone)
	return nil
}

// reattachIfLost is Adopt's cache-hit companion to doAdopt's own best-effort
// attach: called every time a cache-HIT session is about to be handed back
// out, it best-effort re-attaches a session that is running (the container
// is genuinely alive — ContainerWait has not resolved) but not currently
// attached (attached is false — either the session's very first attach,
// doAdopt's, failed, or a previous successful attach's readLoop has since
// ended). A session that is not running, or already attached, returns
// immediately with no I/O — the overwhelming majority of calls, since most
// cache hits are for a session that has been streaming fine the whole time.
//
// Concurrent callers for the SAME session (multiple WS ingress cache-hitting
// it at once, right as the engine recovers) share ONE attach attempt rather
// than each firing their own ContainerAttach: the first caller to observe
// the not-attached state reserves s.reattaching and does the work alone;
// every other concurrent caller finds the reservation and just waits on it
// (or on ctx, so a caller with a bounded ctx — every real caller, see
// Runner.Subscribe's own doc comment — can never hang here longer than its
// own deadline even though the attempt it is waiting on was started by
// somebody else's ctx). This is the session-scoped analogue of
// containerBackend.adopting's backend-scoped dedup for cache-miss doAdopt
// calls (Adopt's own doc comment, PR5 review Major 5) — same shape, smaller
// scope.
//
// A failed re-attach (engine still down) is logged and otherwise swallowed:
// Adopt itself stays ok=true regardless (the session still supports
// signal/stop/wait, exactly as doAdopt's own best-effort attach already
// documented) — only Subscribe observes the difference (see its own doc
// comment).
func (s *containerSession) reattachIfLost(ctx context.Context) {
	s.mu.Lock()
	if !s.running || s.attached {
		s.mu.Unlock()
		return
	}
	if attempt := s.reattaching; attempt != nil {
		s.mu.Unlock()
		select {
		case <-attempt:
		case <-ctx.Done():
		}
		return
	}
	attempt := make(chan struct{})
	s.reattaching = attempt
	// replayLogs decides Logs:true vs Logs:false for the ContainerAttach
	// call below, and MUST be read in the same locked section that
	// reserved s.reattaching above, before releasing s.mu — otherwise a
	// concurrent appendTranscript (a stray write arriving on some other
	// path) could flip the answer between the check and the attach call
	// (Opus review of PR #864, B1).
	//
	// The gate this function reattaches under — running && !attached — is
	// reached by TWO distinct routes, and they need OPPOSITE Logs
	// behavior:
	//
	//  1. doAdopt's own first attach failed (its doc comment): this
	//     containerSession has never produced a single byte of
	//     transcript. Logs:true is not just safe here, it is the ONLY way
	//     to backfill whatever the container already emitted before this
	//     daemon ever knew about it — exactly what doAdopt's own direct
	//     attach(ctx, true) call does for the very same reason.
	//  2. A PREVIOUSLY successful attach's readLoop ended while the
	//     container kept running (an engine/socket-proxy hiccup, or a
	//     live-restore daemon restart) — this session's transcript
	//     already holds everything the container produced up to the
	//     drop. Logs:true here would make the engine replay that SAME
	//     history a second time, landing it in the transcript (and, for a
	//     Launched session, the on-disk spool file — the one thing `boid
	//     job log` can still read after ContainerRemove) as a literal
	//     duplicate. This is not hypothetical: it was measured directly
	//     (a fake engine's Logs:true replay produced "EARLY-OUTPUT" twice
	//     in the transcript after a reattach) before this check existed.
	//
	// Both routes reach this same running&&!attached gate with no other
	// distinguishing signal available except the transcript itself, so
	// "is the transcript still empty" is what selects between them. The
	// trade-off this choice makes (documented so a future change to it is
	// deliberate, not accidental): replaying zero-content for case 1 is
	// always safe (there is nothing to duplicate), but choosing Logs:
	// false for case 2 means whatever the container produced DURING the
	// disconnect window (between readLoop ending and this re-attach
	// succeeding) is permanently lost — it is never in the transcript and
	// Logs:false means the engine never replays it either. The
	// alternative (always Logs:true) trades that gap for corrupting the
	// transcript with a duplicated run of everything already captured,
	// which is worse for `boid job log`'s "this is the literal
	// transcript" contract than a bounded, one-time gap during a
	// reconnect window is — a duplicate is confusing and silently wrong
	// forever, while a gap is at least a clean edge. If a future need
	// makes that gap unacceptable, the fix is to track exactly how much
	// of the container's own output this session has already consumed
	// (a byte offset / `docker logs --since`) and request only the
	// remainder — not to fall back to blanket replay.
	replayLogs := len(s.transcript) == 0
	s.mu.Unlock()

	if err := s.attach(ctx, replayLogs); err != nil {
		slog.Warn("container backend: best-effort re-attach to a running session failed; it will keep supporting signal/stop/wait only until the next cache hit finds the engine reachable",
			"container_id", s.id, "error", err)
	}

	s.mu.Lock()
	s.reattaching = nil
	s.mu.Unlock()
	close(attempt)
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

// readLoop is the session's one and only reader of ITS generation's attach
// connection — hijack/readDone are this specific attach() call's own values
// (passed explicitly, not re-read from the session's own possibly-already-
// reassigned fields), so a concurrent reattachIfLost swapping s.hijack/
// s.readDone for a NEW generation can never cause this, the OLD
// generation's reader, to operate on (or close) the wrong channel. Non-TTY
// containers multiplex stdout/stderr with docker's 8-byte-header framing
// (demuxDockerFrame); both streams are combined into a single transcript
// exactly like the userns backend's combined pipe (§決定 8: "TTY/非 TTY と
// も単一結合で stdout/stderr 分離は意図的に無い").
func (s *containerSession) readLoop(hijack *client.HijackedResponse, readDone chan struct{}) {
	defer func() {
		// attached=false BEFORE close(readDone): a concurrent waitLoop or
		// reattachIfLost that wakes up on readDone closing should already
		// see the up to date attached value if it happens to also read
		// mu-guarded state around the same moment (not load-bearing for
		// correctness — both are only ever read together as one snapshot,
		// see their own doc comments — but keeping state-then-signal
		// ordering here matches the pattern used by every doc comment
		// referencing this file's "close as final act" convention).
		s.mu.Lock()
		s.attached = false
		s.mu.Unlock()
		close(readDone)
	}()

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
//
// ok requires BOTH running AND attached (Opus review of PR #857): running
// alone used to be the whole check, which meant a session doAdopt attached
// to best-effort (its doc comment) — running (the container is genuinely
// alive) but never actually attached (the attach call itself failed) —
// answered ok=true with a channel that would then never receive a single
// byte. That is exactly the "connected but the terminal stays blank
// forever" symptom the Web UI/WS ingress hit: the caller had no way to
// distinguish "attached and just quiet" from "will never speak", so it kept
// waiting either way. Requiring attached too means that case now honestly
// reports ok=false.
//
// finished (SandboxSession.Subscribe's own doc comment has the full
// rationale — Opus review of PR #864, B2) is what actually lets
// a caller act on that ok=false correctly: it is simply !running, checked
// under the SAME lock as ok so the two are never observed inconsistently.
// running==false means the container genuinely exited — ingress should
// treat this as "job done", the pre-existing behavior. running==true (this
// is the running-but-not-attached case above) means finished=false — ingress
// must NOT treat this as "job done"; it should surface an error and let
// Adopt's cache-hit path (reattachIfLost) get a chance to fix the
// underlying cause on a LATER call, without needing a daemon restart. The
// first version of this fix set finished implicitly by leaving ingress's
// pre-existing "ok=false → job done" branch untouched, which was itself
// wrong (an unconditional false positive is a worse diagnostic than the
// dead-channel hang it replaced) — the finished return value is what closes
// that gap.
func (s *containerSession) Subscribe() ([]byte, <-chan []byte, func(), bool, bool) {
	s.mu.Lock()
	snapshot := append([]byte(nil), s.transcript...)
	running := s.running
	live := running && s.attached
	var subID int
	var ch chan []byte
	if live {
		subID = s.nextSubID
		s.nextSubID++
		ch = make(chan []byte, 64)
		s.subscribers[subID] = ch
	}
	s.mu.Unlock()

	finished := !running
	if !live {
		return snapshot, nil, func() {}, false, finished
	}
	return snapshot, ch, func() { s.unsubscribe(subID) }, true, false
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
	// Snapshot the CURRENT generation's hijack AND its own *sync.Once
	// together under connMu (N6): attach() replaces both atomically in
	// the same critical section, so this pairing is never observed
	// half-updated. Calling Do on the local copy outside the lock avoids
	// holding connMu across the CloseWrite call itself.
	s.connMu.Lock()
	once := s.stdinCloseOnce
	hijack := s.hijack
	s.connMu.Unlock()
	once.Do(func() {
		if hijack == nil {
			return
		}
		_ = hijack.CloseWrite()
	})
	return nil
}

// sessionControlCallTimeout is defined in runner.go (moved there, Opus
// review of PR #857, Nit 7: most of its consumers are Runner methods, not
// containerSession ones) — see its doc comment there for the full
// rationale. Resize below is the one containerSession-side consumer.

func (s *containerSession) Resize(size backend.TerminalSize) error {
	if size.Rows <= 0 || size.Cols <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionControlCallTimeout.Get())
	defer cancel()
	_, err := s.api.ContainerResize(ctx, s.id, client.ContainerResizeOptions{
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
	// engineError mirrors the RuntimeExit.EngineError this session reports
	// from Wait — see that field's doc comment (internal/sandbox/backend/
	// backend.go) for the full contract. Left empty on the ordinary
	// "the engine gave us a real StatusCode" path below; only the two
	// engine-fault branches (wait response carrying an Error, and the
	// ContainerWait call itself failing) set it, from the ONE place either
	// error's message is still available — the container is gone, and its
	// logs with it, by the time anything downstream of Wait() runs.
	var engineError string
	select {
	case res := <-waitRes.Result:
		// A response is not the same thing as an exit status: container.WaitResponse
		// carries two independent facts, and only StatusCode is one. When the engine
		// could not report an exit status at all (the runtime failed to wait, the
		// shim died, the container never started), it answers with StatusCode: 0 and
		// a non-nil Error — the SDK does not distinguish this from an actual "exited
		// 0" (moby/moby/client@v0.5.0 container_wait.go forwards the body verbatim;
		// see waitResponseEngineError's doc comment in
		// container_backend_workspace_init.go).
		//
		// This is this layer's own correctness question — exitCode is the value
		// this session reports as ITS exit status, and an engine that never filled
		// in StatusCode must not be read as "exited 0" here regardless of what any
		// caller happens to do with that value afterward. In today's one caller,
		// Runner.watchRuntime (runner.go) independently coerces a zero exit code to
		// 1 and unconditionally marks the job JobStatusFailed on this path (a
		// container exiting without the in-container runner's own "job done"
		// report), so this fix does not change whether that job ends up failed —
		// it changes whether the failure is reported honestly (exit_code in the
		// job's own output stops contradicting job.ExitCode) and whether
		// NewDefaultDiagnosticsCollector's exit.ExitCode != 0 gate — the collector
		// that exists specifically for the "container didn't run right" case —
		// actually fires for this one. Depending on watchRuntime's coercion for the
		// task-done consequence would be relying on a property of a different layer
		// that could drift independently of this one; this fix keeps it correct
		// here on its own terms as well.
		if eerr := waitResponseEngineError(res); eerr != nil {
			slog.Warn("container backend: ContainerWait response carried an engine error", "container_id", s.id, "error", eerr)
			exitCode = 1
			engineError = eerr.Error()
		} else {
			exitCode = int(res.StatusCode)
		}
	case err := <-waitRes.Error:
		exitCode = 1
		// waitChannelError (container_backend_workspace_init.go) covers the
		// nil case this branch would otherwise panic on; see its doc comment.
		//
		// Resolved BEFORE the Warn, not after: the Warn used to fire above
		// the nil guard, so the one case the guard exists for was logged as
		// `error=<nil>` while job.Output and diagnostics.json both got the
		// fallback wording — the daemon log, which is where an operator looks
		// first, was the only place that said nothing
		// (next-session-container-backend-followups.md #3).
		engineError = waitChannelError(err).Error()
		slog.Warn("container backend: ContainerWait failed", "container_id", s.id, "error", engineError)
	}

	// The container process has exited, but its attach stream can still
	// deliver a final burst of already-produced output for a short window
	// afterward. Prefer letting readLoop drain it naturally — it returns
	// (closing readDone) once the daemon itself closes the stream — rather
	// than closing our side immediately, which could truncate exactly that
	// final burst. Only force-close via closeConn if draining hasn't
	// finished within attachDrainGracePeriod: this bounds the wait and
	// still guarantees readDone closes even if the daemon is slow.
	//
	// attached/readDone are snapshotted together under mu — not read as
	// `s.readDone` directly — for two reasons (Opus review of PR #857).
	// First, readDone is no longer a fixed, once-set field: reattachIfLost
	// can swap it for a fresh generation at any point while the container
	// is still running (concurrently with this very goroutine, which spends
	// most of its life blocked in the ContainerWait select above), so a
	// bare field read here would race that write. Second, and more
	// fundamentally, whether there is anything to drain at all is no longer
	// implied by readDone alone: a session that was NEVER successfully
	// attached (doAdopt's best-effort attach failed and no later cache-hit
	// reattachIfLost fixed it before the container happened to exit) has
	// attached=false, and skips the select/drain entirely rather than
	// relying on the old "attach's error path already closed readDone
	// synchronously, so the select falls through immediately" indirection —
	// simpler to read and no longer even true once readDone is per-
	// generation (an old, already-closed generation's channel would make
	// the select fall through for the WRONG reason if reattachIfLost had
	// since installed a new, live, not-yet-closed one).
	s.mu.Lock()
	attached := s.attached
	readDone := s.readDone
	s.mu.Unlock()
	if attached {
		select {
		case <-readDone:
			s.closeConn()
		case <-time.After(attachDrainGracePeriod):
			s.closeConn()
			<-readDone
		}
	} else {
		// No live stream to drain. closeConn is still called — a no-op
		// when s.hijack is nil (attach's own error path never sets it),
		// but a real (if likely redundant) close in the rarer case where
		// attached went false because a PREVIOUS generation's readLoop
		// already ended (a dropped connection reattachIfLost never got a
		// chance to retry before the container exited) and its hijack is
		// still sitting there unclosed.
		s.closeConn()
	}

	// Close (and flush) the disk transcript spool now: readLoop — the sole
	// writer via appendTranscript — has already returned (readDone closed
	// above), so no further writes can race this Close in the common case.
	// Doing this BEFORE finalizing exit state / closing s.done means a
	// diagnostics collector that reads transcript.log from disk (§決定8's
	// silent-exit classification) always sees the complete file, and
	// BEFORE ContainerRemove means the file is guaranteed durable before
	// the container itself (and any `docker logs` fallback) is gone.
	//
	// [N3, Opus review of PR #864]: "no further writes can race this" is
	// no longer a PURE structural guarantee of this file's own logic the
	// way it was before reattachIfLost existed — it now also leans on the
	// engine's own behavior. s.running only flips false a few lines below
	// this point, strictly AFTER this drain-select and this Sync/Close —
	// so a cache-hit Adopt racing exactly this window still sees
	// running==true and (once the drain-select above has finished)
	// attached==false, and will fire a best-effort reattachIfLost against
	// a container that has, in fact, already exited (ContainerWait already
	// resolved) but has not been ContainerRemove'd yet (that happens even
	// later in this same function). If that stray ContainerAttach were to
	// SUCCEED, it would start a brand new readLoop that could then write
	// to s.transcriptFile concurrently with the Sync()/Close() calls right
	// below — a real race, not merely a wasted API call. In measured
	// practice this window is not empty (26-30 out of 30 induced exit-vs-
	// reattach races land a stray attach attempt in it, each logged as a
	// WARN) but is harmless BECAUSE both docker and podman refuse to
	// attach to a non-running container, so the stray attach fails and no
	// second readLoop ever starts — the invariant holds today because of
	// that engine behavior, not because this window is provably empty on
	// this file's own terms. If a future engine (or a mocked/fake one in a
	// test) ever allowed an attach against an already-exited-but-not-yet-
	// removed container, this comment's claim would stop being true.
	//
	// [Major 9, PR7 codex review]: Sync() runs BEFORE Close(), not just
	// Close() alone. Close() flushes the process's own userspace buffers to
	// the kernel but makes no durability guarantee beyond that — a power
	// loss between Close() and the data actually reaching disk could still
	// lose the tail of a job's transcript right as its container is
	// removed, at precisely the moment `boid job log`'s only remaining
	// source of truth. A Sync failure is escalated to Error (louder than
	// the general Warn used elsewhere in this file) since it is the
	// durability guarantee §決定8's "full 永続" contract depends on; Close
	// still runs (and ContainerRemove still proceeds) even when Sync fails
	// — blocking container teardown indefinitely on a persistent disk error
	// would leak the container itself and defeat the reap contract, a worse
	// outcome than a possibly-incomplete transcript tail.
	if s.transcriptFile != nil {
		if err := s.transcriptFile.Sync(); err != nil {
			slog.Error("container backend: sync transcript spool failed; the transcript tail may not survive a crash before it reaches disk",
				"container_id", s.id, "path", s.transcriptPath, "error", err)
		}
		if err := s.transcriptFile.Close(); err != nil {
			slog.Warn("container backend: close transcript spool failed", "container_id", s.id, "path", s.transcriptPath, "error", err)
		}
	}

	s.mu.Lock()
	s.running = false
	s.exit = backend.RuntimeExit{ExitCode: exitCode, TranscriptPath: s.transcriptPath, EngineError: engineError}
	s.closeSubscribersLocked()
	exit := s.exit
	s.mu.Unlock()
	close(s.done)

	if collector := s.backend.diagnosticsCollector; collector != nil {
		collector(context.Background(), s.id, exit)
	}

	s.backend.forgetSession(s.id)
	// Bounded, like every other teardown removal in this file (codex round 2 of
	// PR8, Major 3 — see containerCleanupContext). This one blocks no caller (the
	// exit is already published and s.done already closed), so an unbounded
	// version leaks a goroutine rather than wedging a dispatch; it is bounded
	// anyway so that "a teardown ContainerRemove in this package always has a
	// deadline" is a property of the file rather than of each site. The base is
	// Background() rather than a request context because waitLoop outlives every
	// request that could have started it.
	removeCtx, cancelRemove := containerCleanupContext(context.Background())
	if _, err := s.api.ContainerRemove(removeCtx, s.id, client.ContainerRemoveOptions{RemoveVolumes: true}); err != nil {
		slog.Warn("container backend: remove exited container failed; retrying with Force", "container_id", s.id, "error", err)
		if _, ferr := s.api.ContainerRemove(removeCtx, s.id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); ferr != nil {
			slog.Warn("container backend: force remove exited container failed", "container_id", s.id, "error", ferr)
		}
	}
	cancelRemove()
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
	if s.brokerTLSDir != "" {
		_ = os.RemoveAll(s.brokerTLSDir)
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
