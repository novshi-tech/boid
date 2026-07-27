package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

// Daemon state volume self-inspection.
//
// # The exposure
//
// docs/plans/workspace-home-volume-persistence.md PR1 reserved the volume
// namespace boid NAMES ITSELF ("boid-ws-", internal/dockerres) so a
// `capabilities.docker` job cannot create, delete, or mount another
// workspace's HOME volume. The single highest-value volume in the deployment
// was not in that namespace: under build/container/compose.yml the daemon's
// entire persistent state is one named volume mounted at /home/boid, and
// COMPOSE names it — `name: boid` plus the `boid_state` key gives
// "boid_boid_state". So a job needed only
//
//	{"Image":"busybox","Cmd":["sh","-c","cat /state/.local/share/boid/secret.key"],
//	 "HostConfig":{"Mounts":[{"Type":"volume","Source":"boid_boid_state","Target":"/state"}]}}
//
// to read secret.key, boid.db, tls/ca.key, web_secret and install_id. Note it
// needs no WRITE access anywhere: PR1's create/delete rules were never what
// stood in the way. The plan doc recorded this under "未解決 / 本 doc の
// 範囲外"; this file closes it.
//
// # Why runtime detection rather than a name rule
//
// Owner decision, 2026-07-26. Two cheaper designs were rejected:
//
//   - Deny a blanket "boid_"/"boid-" volume-name prefix. Minimal code, but it
//     misfires on an operator's own `boid_myapp` volume, and it protects
//     nothing the moment someone runs the stack under a different compose
//     project name — the name is not ours to predict.
//   - Deny mounting any volume not in the job's own ledger. The tightest rule
//     available, and the one to reach for if the `capabilities.docker` threat
//     model is ever revisited wholesale, but it breaks legitimate sibling use
//     (testcontainers and friends attaching a fixture volume).
//
// So the daemon asks the engine what IT is running on, once, at startup. The
// accepted cost is that this only works where the daemon can identify its own
// container; where it cannot, the reservation is empty AND the operator is
// warned, which is the part that took a review round to get right — see
// daemonStateMayLiveOnVolume for why "am I in a container?" was the wrong
// question to gate the warning on, and DetectDaemonStateVolumes for the
// contract by outcome.

// selfInspectTimeout bounds the ENTIRE startup self-inspection, and exists
// because the failure mode it guards is not "the engine is down" — that is an
// immediate dial error — but "the socket accepts the connection and then never
// answers" (a wedged engine, a half-open socket surviving an engine restart, a
// proxy in front of it). With no deadline the inspect inherits the caller's
// context.Background() and `boid start` blocks forever, inside a function
// whose entire design is "warn and continue": an unbounded call turns the
// fail-open posture into a fail-never one. [Major 2, codex review 2026-07-26]
//
// Sized for a slow engine on a loaded host, not for latency: nothing waits on
// this result except the rest of startup, and being wrong in the short
// direction costs a spurious warning, not an outage.
//
// # What this deadline does NOT cover, and why that is acceptable
//
// Two operations in this file are synchronous filesystem reads that no context
// can interrupt:
//
//   - os.ReadFile on /proc/self/{mountinfo,cgroup}. These cannot block:
//     procfs generates both from in-kernel state, with no I/O and no lock a
//     userspace process can hold.
//   - filepath.EvalSymlinks on dataHome (resolveDataHomeForMountComparison).
//     This one genuinely touches the filesystem, and round 1 of this feature
//     avoided it for exactly that reason — a hung NFS/FUSE mount under the
//     data root would hang startup where a deadline could not reach.
//     [round 2 Major 1] retired that reasoning: the daemon OPENS boid.db under
//     this very path before buildRuntime ever calls this function, so a wedged
//     data root has already stopped startup whether or not the self-inspection
//     looks at it. Declining to resolve bought no availability and cost
//     correctness — see resolveDataHomeForMountComparison.
//
// The docker client is likewise constructed OUTSIDE this deadline, by the
// caller (internal/server's buildRuntime). That is deliberate and is the
// reason it is now shared with the container backend rather than built here:
// client.New(client.FromEnv) reads DOCKER_CERT_PATH's ca/cert/key
// synchronously, so a self-inspect that built its own would be a second
// uninterruptible read on the startup path — one this feature alone would have
// introduced. Sharing the backend's client makes that read a precondition the
// daemon already had.
//
// A var only so tests can shrink it; production never reassigns it.
var selfInspectTimeout = newAtomicDuration(5 * time.Second)

// readSelfMountInfo / readSelfCgroup are the package-level indirections that
// make this testable with no container and no engine, mirroring
// internal/api/workspace_homes.go's apparentSizeFn and this package's own
// daemonUID / bindTargetOwnerUID / workspaceHomeDirFor. Production never
// reassigns them.
var (
	readSelfMountInfo = func() ([]byte, error) { return os.ReadFile("/proc/self/mountinfo") }
	readSelfCgroup    = func() ([]byte, error) { return os.ReadFile("/proc/self/cgroup") }
)

// SelfContainerInspector is the one docker API call this feature needs. It is
// declared as its own narrow interface rather than reusing dockerAPI so
// internal/server can hand in a bare *client.Client without constructing a
// container backend, and so the test stub does not have to implement two
// dozen unrelated methods. dockerAPI satisfies it (pinned by a compile-time
// assertion in the test file).
type SelfContainerInspector interface {
	ContainerInspect(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
}

// DaemonStateVolumes is what the startup self-inspection learned. It is
// returned even alongside a non-nil error, because StateVolumeExpected is
// meaningful in exactly that case: it is how the caller decides between
// logging nothing and logging a warning.
type DaemonStateVolumes struct {
	// StateVolumeExpected reports whether a named volume could be holding this
	// daemon's persistent state — i.e. whether an empty Names is an ANSWER or
	// a FAILURE. False is a positive finding ("the data root is an ordinary
	// directory on the root filesystem; there is no volume to reserve") and
	// callers must stay silent on it. True with a non-nil error is
	// "something is probably holding my state and I could not name it", which
	// callers must warn about.
	//
	// It is deliberately NOT "am I in a container". That question was the
	// first implementation and it was wrong in the dangerous direction: see
	// daemonStateMayLiveOnVolume.
	StateVolumeExpected bool

	// ContainerID is the daemon's own container, when it could be determined.
	// Recorded for the startup log line, so an operator can re-run the same
	// `docker inspect` by hand.
	ContainerID string

	// Names is every NAMED VOLUME mounted into this daemon's container,
	// sorted. See DetectDaemonStateVolumes for why it is all of them and not
	// only the one holding dataHome.
	Names []string

	// DataHomeCovered reports whether the daemon's own persistent data root
	// (server/wire.go's dataHomeFor(cfg) — where boid.db and secret.key live)
	// actually falls inside one of the volumes in Names. False with a
	// non-empty dataHome means name-based reservation is not covering the
	// crown jewels and the operator should be told, even though every
	// individual step succeeded.
	DataHomeCovered bool

	// DataHomeUnresolved carries why the data root could not be normalized
	// (made absolute and symlink-free) for comparison against the engine's
	// mount destinations. It is set whenever that step failed — including on
	// the error returns below, where the same reason is also folded into the
	// returned error, so a caller that reads both must not report it twice.
	// The path it exists for is the all-succeeded one, which has nowhere
	// else to report it.
	//
	// It exists because that failure has nowhere honest to go otherwise
	// [round 3 Minor 1, codex review]. It cannot be the returned error: a
	// non-nil error means "containment is INACTIVE" to the caller, and it is
	// not — the volumes were named and reserved. It cannot be silently
	// dropped either, which is what the first implementation did: with an
	// unresolvable data root and an otherwise healthy inspect, the run
	// returned a nil error and the operator was never told that
	// DataHomeCovered had been computed against a path the daemon could not
	// resolve. So it rides alongside, and the caller warns about it
	// separately from the INACTIVE case.
	//
	// DataHomeCovered is still computed (against the absolute-but-unresolved
	// spelling, the closest available approximation) rather than forced
	// false — a false there would claim the crown jewels are outside every
	// reserved volume, which is a different and equally unsupported claim.
	DataHomeUnresolved error
}

// DetectDaemonStateVolumes identifies the daemon's own container and returns
// the named volumes mounted into it, for injection into the dockerproxy
// policy's reserved set (dockerproxy.Server.SetReservedVolumeNames).
//
// # Contract, by outcome
//
//   - No volume can be holding the daemon's state (daemonStateMayLiveOnVolume
//     says no): DaemonStateVolumes{} and a nil error, and api is never called.
//     Bare `boid start` keeps its state in an ordinary directory on the root
//     filesystem, where there is no volume to name — an empty result is the
//     right answer, not a degraded one, and callers must not warn about it.
//   - A volume might be holding it, and it was identified: Names populated,
//     nil error.
//   - A volume might be holding it and it was NOT identified (unknown id,
//     ambiguous id, no docker client, inspect failed, inspect timed out):
//     StateVolumeExpected true, Names empty, non-nil error. Callers warn once
//     and continue. Refusing to start would be a much worse trade: it turns "a
//     docker-capable job could read the daemon's secrets" — which is the status
//     quo ante for every release so far — into "the daemon does not run at all"
//     on any engine or runtime this detection does not understand.
//
// # The invariant those three outcomes rest on
//
// EVERY non-nil error this function returns carries StateVolumeExpected=true,
// and StateVolumeExpected=false is returned by exactly one path — the positive
// finding, with a nil error. Callers therefore never have to guess which of the
// two a zero value meant, and "silent" is reachable only from an answer, never
// from a failure. It is upheld by ordering (the engine-independent
// classification runs before anything engine-shaped can fail) and pinned by
// TestDetectDaemonStateVolumes_everyErrorCarriesStateVolumeExpected.
// [round 2 Major 2, codex review 2026-07-26.]
//
// Every path is bounded by selfInspectTimeout, with the two synchronous
// filesystem reads that no deadline can interrupt enumerated and justified in
// that variable's own doc comment.
//
// # Why EVERY named volume, not just the one holding dataHome
//
// Deliberate, and the safer of the two readings the brief allowed:
//
//   - The daemon's persistent state is not confined to dataHome. Under the
//     compose deploy XDG_CONFIG_HOME=/home/boid/.config (config.yaml,
//     host_commands.yaml, workspaces/<slug>/init.sh) sits OUTSIDE
//     filepath.Dir(cfg.DBPath) and is only covered today because both happen
//     to live under the same /home/boid mount. A dataHome-only rule would
//     silently stop covering the config half the day someone splits it out.
//   - A dataHome-containment test degrades badly when dataHome is empty,
//     which dataHomeFor genuinely can return (see its own doc comment).
//     "Every volume I hold" has no such degenerate case.
//   - There is no legitimate reason for a sandboxed job to mount a volume the
//     daemon itself has mounted. Over-reserving costs a job nothing it can
//     articulate a use for; under-reserving costs secret.key.
//   - It cannot drift. A second daemon-owned volume added to compose later is
//     protected the day it is added, with no second PR and no shared constant
//     for two packages to disagree about.
//
// Bind mounts are excluded on purpose: their Source is a host path, not a
// volume name, so it is not the kind of thing the policy's name rules can
// match — and HostConfig.Binds / Mounts[].Type=bind are already denied
// wholesale by dockerproxy, which covers that shape completely.
func DetectDaemonStateVolumes(ctx context.Context, api SelfContainerInspector, dataHome string) (DaemonStateVolumes, error) {
	// [Major 2] One deadline over the whole thing, including the engine call.
	// WithTimeout takes the earlier of this and any deadline the caller already
	// set, so a caller may tighten it but not remove it.
	ctx, cancel := context.WithTimeout(ctx, selfInspectTimeout.Get())
	defer cancel()

	// ORDER MATTERS, and it is the second half of [round 2 Major 2]. The
	// mount-based classification below needs no engine, so it runs FIRST and
	// fixes StateVolumeExpected before anything that can fail for an
	// engine-shaped reason. From the `res` assignment onward every return in
	// this function carries StateVolumeExpected=true, which is what lets the
	// caller treat "error" and "nothing to protect" as disjoint instead of
	// having to guess which one a zero value meant. Pinned by
	// TestDetectDaemonStateVolumes_everyErrorCarriesStateVolumeExpected.
	resolvedHome, resolveErr := resolveDataHomeForMountComparison(dataHome)
	mayLive, undecided := daemonStateMayLiveOnVolume(resolvedHome, resolveErr)
	if !mayLive {
		return DaemonStateVolumes{}, nil
	}

	// undecided is recorded on the result, not only folded into the error
	// paths below, so it survives a run where every subsequent step succeeds
	// — see DataHomeUnresolved.
	res := DaemonStateVolumes{StateVolumeExpected: true, DataHomeUnresolved: undecided}
	id, idErr := selfContainerID()
	if idErr != nil {
		// undecided is nil unless the "is there anything to protect?" question
		// could not be answered either; errors.Join drops it when it is. Both
		// halves are reported because they are different repairs: one is
		// "teach the parser this runtime", the other is "the daemon cannot see
		// its own mount table".
		return res, errors.Join(
			fmt.Errorf("identify this daemon's own container: %w", idErr),
			undecided,
		)
	}
	res.ContainerID = id

	if api == nil {
		return res, errors.New("no docker client available to inspect this daemon's own container")
	}
	insp, err := api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return res, fmt.Errorf("inspect this daemon's own container %s: %w", id, err)
	}

	seen := make(map[string]struct{})
	for _, m := range insp.Container.Mounts {
		if m.Type != mount.TypeVolume || m.Name == "" {
			continue
		}
		if _, dup := seen[m.Name]; !dup {
			seen[m.Name] = struct{}{}
			res.Names = append(res.Names, m.Name)
		}
		// resolvedHome, not dataHome: the mount destinations the engine
		// reports are kernel-resolved absolute paths, so a relative or
		// symlink-traversing spelling of the data root would compare unequal
		// to all of them and report DataHomeCovered=false — the caller's
		// "your secrets are not in any reserved volume" warning, on a
		// deployment where they are. When resolution failed outright
		// resolvedHome falls back to the absolute (unresolved) form, which is
		// still strictly closer than the raw input.
		if resolvedHome != "" && pathIsWithin(resolvedHome, m.Destination) {
			res.DataHomeCovered = true
		}
	}
	sort.Strings(res.Names) // stable log output and stable test expectations
	return res, nil
}

// daemonStateMayLiveOnVolume answers the ONE question that decides between
// silence and a warning: could a named volume be holding this daemon's
// persistent state? It reports (false, nil) only as a positive finding, and
// resolves every "cannot tell" toward true — the second return value carries
// why, for the caller to fold into its error.
//
// # Why this replaced the "am I in a container?" test
//
// [Major 1, codex review 2026-07-26.] The first implementation asked whether
// an engine sentinel file existed (/run/.containerenv, /.dockerenv) or a
// container id could be extracted, and treated "neither" as "not in a
// container, nothing to protect, stay silent". That is wrong for any runtime
// that ships no sentinel and puts no id where this file looks — containerd/CRI
// and Kubernetes are the obvious ones, gVisor and Firecracker-backed runtimes
// the less obvious. In exactly those environments the daemon would return a
// SUCCESSFUL empty reservation and start silently undefended, and if the node
// exposes a docker-compatible socket to jobs (which is the only configuration
// where any of this matters) the sibling-mount attack lands untouched. A
// detection whose failure mode is indistinguishable from its no-op is not a
// detection.
//
// So the question is asked about the ASSET instead of about the runtime: is
// the daemon's persistent data root a mount point of its own — something laid
// over the filesystem — or an ordinary directory on the root mount? A named
// volume is always the former. The property is engine-independent, sentinel-
// independent, and it is the very thing the reservation exists to defend, so
// it cannot drift away from the feature the way "docker or podman?" can.
//
//   - On its own mount → something is mounted where the state lives; it may
//     well be a named volume, so failing to name it is a FAILURE (warn).
//     Verified against the live rootless-podman deploy on 2026-07-26: the
//     daemon container's /proc/self/mountinfo carries
//     "... /home/boid rw,nosuid,nodev,relatime - ext4 ..." while the data root
//     is /home/boid/.local/share/boid, so the covering mount is /home/boid.
//   - On the root mount → nothing is mounted over it, so no volume holds it
//     and there is nothing to reserve (silent no-op). Verified against this
//     developer host the same day: its mountinfo has mount points /, /boot,
//     /boot/efi, /run, /dev, /sys and their children — no /home — so a data
//     root of /home/nosen/.local/share/boid is covered by "/".
//
// # The known false positive, and why it is the right direction
//
// A bare host with /home (or /var) on its own filesystem — a very ordinary
// Linux install — classifies as "may live on a volume", fails to find a
// container id, and warns once at startup about a containment it never needed.
// That is a line of noise on a deployment with nothing to protect, traded
// against silence on a deployment with everything to protect. The warning text
// (internal/server's reservedDaemonStateVolumes) is written to be actionable
// in both readings.
// The dataHome it receives is the RESOLVED one (see
// resolveDataHomeForMountComparison), and resolveErr is that resolution's own
// failure, threaded in rather than swallowed: a path that could not be
// resolved cannot be compared against the kernel's mount points at all, so the
// only honest classification is "could not tell" — which, by the rule above,
// means true.
func daemonStateMayLiveOnVolume(dataHome string, resolveErr error) (bool, error) {
	if resolveErr != nil {
		// [round 2 Major 1] Deliberately NOT "carry on with the unresolved
		// spelling and see what matches". An unresolved path matches nothing
		// but "/", which reads as "an ordinary directory on the root
		// filesystem" — the one answer this function is allowed to give
		// silently. Failing to resolve must never be able to produce it.
		return true, resolveErr
	}
	if dataHome == "" {
		// dataHomeFor genuinely can return "" (an in-memory DB with no socket
		// path). Nothing to locate, so nothing was ruled out.
		return true, errors.New("the daemon's data root is unknown, so it could not be checked for being on a mount of its own")
	}
	b, err := readSelfMountInfo()
	if err != nil {
		return true, fmt.Errorf("read mountinfo to locate the daemon's data root: %w", err)
	}
	mountPoint, ok := mountPointCovering(string(b), dataHome)
	if !ok {
		// Not even "/" matched, which means the file was unparseable or
		// truncated rather than that the path is unmounted.
		return true, fmt.Errorf("no mount in /proc/self/mountinfo covers the daemon's data root %s", dataHome)
	}
	return mountPoint != "/", nil
}

// resolveDataHomeForMountComparison puts the daemon's data root into the same
// form the kernel writes into /proc/self/mountinfo, so the lexical comparison
// in mountPointCovering means what it looks like it means: absolute, and with
// every symlink resolved.
//
// # Why both steps, and why failure is loud
//
// [round 2 Major 1.] The comparison is pure string work, and pathIsWithin
// answers true for the mount point "/" unconditionally — so ANY spelling the
// kernel would not have produced falls through to "/" and classifies as "an
// ordinary directory on the root filesystem, nothing to protect", which is the
// one outcome DetectDaemonStateVolumes reports silently. Two such spellings
// reach this code in practice:
//
//   - Relative. cmd/start.go accepts --db-path verbatim: it neither rejects a
//     relative path nor absolutizes one. `boid start --db-path
//     ../home/boid/.local/share/boid/boid.db` from /workspace puts boid.db on
//     the state volume and hands this function a string starting "..".
//   - Symlinked ancestor. /home/boid -> /mnt/state, or any operator's own
//     tidy-up. Round 1 knew about this one and accepted it as a residual
//     limit on the grounds that filepath.EvalSymlinks might block under a
//     hung filesystem. The premise does not hold: the daemon opens boid.db
//     under this path before this function runs, so a wedged data root has
//     already stopped startup. Declining to resolve bought nothing.
//
// A failure here is therefore reported, never swallowed: the caller turns it
// into "may live on a volume" (warn), not into "on the root mount" (silent).
// The returned path is still the best available approximation — the absolute
// form when only EvalSymlinks failed, the input when even filepath.Abs did —
// because DetectDaemonStateVolumes still uses it for the DataHomeCovered
// diagnostic, which is advisory rather than load-bearing.
//
// Note that EvalSymlinks fails with ENOENT on a data root that does not exist
// yet. That is treated as any other resolution failure (warn), and it is not
// the production shape: buildRuntime calls this only after the DB at that path
// has been opened.
func resolveDataHomeForMountComparison(dataHome string) (string, error) {
	if dataHome == "" {
		// Not a resolution failure — "unknown data root" is its own,
		// separately-worded undecidable in daemonStateMayLiveOnVolume.
		return "", nil
	}
	abs, err := filepath.Abs(dataHome)
	if err != nil {
		return dataHome, fmt.Errorf("make the daemon's data root %q absolute so it can be compared against mount points: %w", dataHome, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs, fmt.Errorf("resolve symlinks in the daemon's data root %s so it can be compared against mount points: %w", abs, err)
	}
	return resolved, nil
}

// mountPointCovering returns the mount point of the mount that path resolves
// onto: the longest mount point in mountInfo that is path itself or an
// ancestor of it. Longest wins because mounts nest — /home/boid shadows / for
// anything beneath it, which is precisely the case that matters here.
//
// Pure string work: the paths in /proc/self/mountinfo are already fully
// resolved by the kernel, and path is expected to have been put into the same
// form by resolveDataHomeForMountComparison before it gets here. Handing this
// function a relative or symlink-traversing spelling silently returns "/" —
// see that function for why that is the dangerous direction and why its own
// failures are reported rather than absorbed.
func mountPointCovering(mountInfo, path string) (string, bool) {
	target := filepath.Clean(path)
	best := ""
	for _, line := range strings.Split(mountInfo, "\n") {
		// mountinfo: id parent major:minor root mountPoint options... - fstype source superOpts
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mountPoint := filepath.Clean(unescapeMountInfoField(fields[4]))
		if !pathIsWithin(target, mountPoint) {
			continue
		}
		if len(mountPoint) > len(best) {
			best = mountPoint
		}
	}
	return best, best != ""
}

// pathIsWithin reports whether path is mountPoint itself or lives underneath
// it. Used to answer "does this volume hold the daemon's data root?" — a
// diagnostic, never a filter (see DetectDaemonStateVolumes' "why every named
// volume") — and to find the mount a path sits on (mountPointCovering).
func pathIsWithin(path, mountPoint string) bool {
	if path == "" || mountPoint == "" {
		return false
	}
	p := filepath.Clean(path)
	m := filepath.Clean(mountPoint)
	if m == "/" || p == m {
		return true
	}
	return strings.HasPrefix(p, m+string(filepath.Separator))
}

// selfContainerID returns this process's own container id, preferring
// /proc/self/mountinfo and falling back to /proc/self/cgroup.
//
// The ORDER is the whole point. /proc/self/cgroup is the answer most code on
// the internet reaches for, and under cgroup v2 it is frequently useless:
// verified against the live rootless-podman 4.9.3 dogfood deploy on
// 2026-07-26, where `cat /proc/self/cgroup` inside the daemon container prints
// exactly "0::/" — no id anywhere. (The same container's HOSTNAME env var was
// also empty, so the $HOSTNAME shortcut ContainerBackendOptions.SelfContainerID
// uses is not a viable source here either.) mountinfo, by contrast, always
// carries it: both engines bind-mount /etc/hostname, /etc/hosts and
// /etc/resolv.conf out of a per-container directory whose path contains the
// full 64-hex id.
func selfContainerID() (string, error) {
	var errs []error

	if b, err := readSelfMountInfo(); err != nil {
		errs = append(errs, fmt.Errorf("read mountinfo: %w", err))
	} else if id, err := containerIDFromMountInfo(string(b)); err != nil {
		errs = append(errs, fmt.Errorf("mountinfo: %w", err))
	} else {
		return id, nil
	}

	if b, err := readSelfCgroup(); err != nil {
		errs = append(errs, fmt.Errorf("read cgroup: %w", err))
	} else if id, err := containerIDFromCgroup(string(b)); err != nil {
		errs = append(errs, fmt.Errorf("cgroup: %w", err))
	} else {
		return id, nil
	}

	return "", errors.Join(errs...)
}

// runtimeInjectedMountPoints are the in-container paths BOTH docker and podman
// bind-mount out of their own per-container directory. Keying off the mount
// DESTINATION — a fixed, well-known set — rather than pattern-matching paths
// anywhere in the file is what keeps another container's id out of the answer:
// a docker host's mountinfo also contains entries whose mount POINT is
// /var/lib/docker/containers/<some other id>/shm (moby's own mountinfo test
// corpus has exactly that line), and a parser scanning indiscriminately would
// happily inspect a container that is not itself.
var runtimeInjectedMountPoints = map[string]bool{
	"/etc/hostname":      true,
	"/etc/resolv.conf":   true,
	"/etc/hosts":         true,
	"/run/.containerenv": true,
}

// containerIDFromMountInfo extracts this container's id from the contents of
// /proc/self/mountinfo.
//
// The two engines put their per-container directory in different places, and
// this is the only real difference the parser has to absorb. Verified shapes
// (the mountinfo ROOT field, i.e. field 4):
//
//	docker  /var/lib/docker/containers/<64-hex>/hostname
//	        (moby/moby daemon/daemon_linux_test.go's own mountinfo corpus;
//	         rootless docker moves the prefix to ~/.local/share/docker/)
//	podman  /containers/overlay-containers/<64-hex>/userdata/hostname
//	        (live rootless podman 4.9.3, 2026-07-26 — note the root is
//	         relative to the /run/user/<uid> tmpfs it is mounted from, so it
//	         carries neither /var/lib nor ~/.local/share)
//
// Rather than encode either prefix, the rule is "the one 64-hex path segment
// in the root of a runtime-injected mount". Requiring exactly one per line
// means a future layout with two hex-looking segments is skipped rather than
// guessed at. Requiring all matching lines to agree means a mountinfo carrying
// two containers' directories fails loudly instead of picking one.
func containerIDFromMountInfo(contents string) (string, error) {
	seen := make(map[string]struct{})
	var ids []string

	for _, line := range strings.Split(contents, "\n") {
		// mountinfo: id parent major:minor root mountPoint options... - fstype source superOpts
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if !runtimeInjectedMountPoints[unescapeMountInfoField(fields[4])] {
			continue
		}
		id := soleContainerIDInPath(unescapeMountInfoField(fields[3]))
		if id == "" {
			continue
		}
		if _, dup := seen[id]; !dup {
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}

	switch len(ids) {
	case 0:
		return "", errors.New("no container id found in the roots of /etc/hostname, /etc/hosts, /etc/resolv.conf or /run/.containerenv")
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("ambiguous container id: %v", ids)
	}
}

// soleContainerIDInPath returns the single 64-hex path segment in p, or "" if
// there is none or more than one — the mountinfo-side spelling of the same
// refusal containerIDFromCgroup makes explicit: a path naming two containers
// does not say which one is this process, so it contributes no evidence.
// Dropping the line (rather than erroring outright) is deliberate: another,
// unambiguous injected mount in the same file is still real evidence, and if
// there is none the caller reports "no container id found".
func soleContainerIDInPath(p string) string {
	var found string
	for _, seg := range strings.Split(p, "/") {
		if !isContainerID(seg) {
			continue
		}
		if found != "" {
			return "" // ambiguous line; let the caller fall through
		}
		found = seg
	}
	return found
}

// containerIDFromCgroup is the fallback for cgroup v1 (and for the v2 layouts
// that do put the id in the path, e.g. systemd's libpod-<id>.scope). It errors
// when the file carries no id — the normal cgroup v2 case, see selfContainerID's
// doc comment — and when it carries more than one.
//
// # Why ambiguity is an error rather than a pick
//
// [Major 3, codex review 2026-07-26.] The first version returned the first
// 64-hex segment it saw. Nested runtimes write the OUTER container's id first:
//
//	0::/docker/<parent container id>/docker/<this container's id>
//
// so "first match" hands back the parent, and the daemon then inspects the
// parent and reserves the PARENT's volumes. If the parent happens to have a
// volume of its own — which it does, in any docker-in-docker setup where the
// outer container also persists state — every startup check passes:
// DataHomeCovered can even come back true, the log line says containment is
// active, and the one volume actually holding this daemon's secret.key is left
// mountable. That is strictly worse than reserving nothing, because the
// warning that would have told an operator never fires. There is nothing in
// this file that distinguishes "me" from "my parent", so the only honest
// answer is to refuse and let the caller warn.
//
// # What counts as ambiguous
//
// The DISTINCT ids across the whole file, after stripping each runtime's unit
// wrapper. More than one → ambiguous. Repetition is not ambiguity: cgroup v1
// writes one line per controller, all naming the same container, and that is
// the shape of the normal, unambiguous v1 answer.
func containerIDFromCgroup(contents string) (string, error) {
	// The scope/slice unit names each runtime wraps the raw id in.
	prefixes := []string{"docker-", "libpod-", "crio-", "cri-containerd-", "containerd-"}

	seen := make(map[string]struct{})
	var ids []string

	for _, line := range strings.Split(contents, "\n") {
		// cgroup v1: <hierarchy-id>:<controllers>:<path>
		// cgroup v2: 0::<path>
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 3 {
			continue
		}
		for _, seg := range strings.Split(parts[2], "/") {
			seg = strings.TrimSuffix(strings.TrimSuffix(seg, ".scope"), ".slice")
			for _, pfx := range prefixes {
				seg = strings.TrimPrefix(seg, pfx)
			}
			if !isContainerID(seg) {
				continue
			}
			if _, dup := seen[seg]; !dup {
				seen[seg] = struct{}{}
				ids = append(ids, seg)
			}
		}
	}

	switch len(ids) {
	case 0:
		return "", errors.New("no container id (expected under cgroup v2, which usually reports just \"0::/\")")
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("ambiguous container id, refusing to guess which is this process's own: %v", ids)
	}
}

// isContainerID reports whether s is a full container id: 64 lowercase hex
// digits, the form both docker and podman use and the form ContainerInspect
// accepts verbatim.
func isContainerID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// unescapeMountInfoField decodes the octal escapes the kernel writes into the
// root and mount-point fields of /proc/self/mountinfo for the four characters
// that would otherwise break its space-separated format (see fs/proc_namespace.c's
// mangle()/show_mountinfo). None of the paths this file cares about normally
// contain one, but a mount point that failed to decode would silently fall out
// of runtimeInjectedMountPoints and turn detection off.
func unescapeMountInfoField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	replacer := strings.NewReplacer(
		`\040`, " ",
		`\011`, "\t",
		`\012`, "\n",
		`\134`, `\`,
	)
	return replacer.Replace(s)
}
