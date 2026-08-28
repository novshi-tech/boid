package dispatcher

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/novshi-tech/boid/internal/dockerres"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/version"
)

// workspaceHomeMarker is the on-disk completion marker for a workspace home
// init run. It lives at <dataHome>/homes-meta/<slug>.init.json — inside the
// DAEMON's own persistent data root, not the workspace home it describes
// (docs/plans/workspace-home-volume-persistence.md 論点b, PR2; see
// workspaceHomeMetaDir for the derivation).
//
// Tamper resistance is why it was never kept inside the home directory in
// the first place (docs/plans/home-workspace-volume.md 「置き場」): a
// sandboxed job gets an rw mount of its workspace home, so a marker in there
// would be forgeable by the very jobs it governs. That reason is unchanged
// by PR2 — both the old location (homes/<slug>.init.json, beside the mounted
// directory) and the new one are outside the mount a job receives, so no job
// can write to either through its own $HOME. The reason for the move is a
// different one, and PR6 collected on it: the home is now a docker named
// volume, and the daemon keeps ordinary file I/O + flock over its own
// bookkeeping only because that bookkeeping stayed on its own side.
//
// What this deliberately does NOT claim is that the marker is unreachable by
// a job outright. Under the container deploy the daemon's data root IS a
// docker named volume (`boid_state`, mounted at /home/boid by
// build/container/compose.yml), and when PR2 was written dockerproxy's volume
// policy reserved only the `boid-ws-` name prefix — so a job holding
// capabilities.docker could create a sibling container that mounted that
// volume by name and read (or write) everything in it. That exposure predated
// PR2 and was much wider than this marker (secret.key, boid.db and tls/ca.key
// live in the same volume).
//
// PR2.5 closed it: the daemon now identifies its own container at startup and
// injects the volumes mounted into it into the dockerproxy policy's reserved
// set (DetectDaemonStateVolumes -> Runner.ReservedVolumeNames). The closure is
// conditional, not absolute — it depends on the daemon being able to identify
// its own container (see that function's contract for the fail-open case, which
// warns and continues) — so the guarantee stated here is unchanged and is still
// the one worth relying on: a job cannot reach the marker through the one path
// every job has, its own rw-mounted $HOME.
type workspaceHomeMarker struct {
	ScriptSHA256 string `json:"script_sha256"`
	// HomeID ties this marker to one specific incarnation of the workspace
	// home it describes (docs/plans/workspace-home-volume-persistence.md
	// 論点b). As of PR6 it holds the value of that home volume's
	// dockerres.LabelWorkspaceHomeID label — the random token minted when the
	// volume was created.
	//
	// Why a marker needs this at all: before PR2 the marker sat in the same
	// parent directory as the home, so the two could only ever vanish
	// together and "a marker exists" implied "the home it describes exists".
	// PR2 moved the marker into the daemon's own persistent data root, and
	// PR6 moved the home into a named volume — two independent lifetimes, and
	// several ordinary ways for the home to end and the marker to survive: a
	// stray `docker volume rm`, a reap misfire, a half-completed workspace
	// remove. Without an identity to compare, the next dispatch would skip
	// init.sh and hand the agent an empty $HOME; the error that surfaces is
	// the adapter's "CLI not found" (internal/adapters/claude/run.go), which
	// points at an unconfigured init.sh rather than at the vanished home.
	//
	// # Why a volume label rather than a file inside the home
	//
	// Through PR5 this token was written to <homeDir>/.boid-workspace-home-id
	// and read back with a hardened open (O_NOFOLLOW/O_NONBLOCK, regular-file
	// check, size cap — the target sat inside storage a job owns read-write).
	// PR6 ends that: the daemon cannot read anything inside a named volume
	// without starting a container, and starting one on every dispatch just to
	// read 64 bytes would turn the fast path into a container launch — the
	// very cost the completion marker exists to avoid. A file that can only be
	// written and never checked is not a check, so the file went and the
	// identity moved onto the volume itself, where ONE idempotent VolumeCreate
	// both ensures the volume and reports its labels
	// (dockerres.LabelWorkspaceHomeID has the measurement).
	//
	// What that trade costs, precisely:
	//
	//   - Still detected: the volume being deleted and re-created (by a reap
	//     misfire, an operator, a half-completed workspace remove), which is
	//     the accident class the split of marker and home lifetimes actually
	//     created — and, post-PR6, the ONLY way a workspace home can vanish,
	//     since a named volume survives the host reboot that used to clear the
	//     tmpfs one.
	//   - No longer detected: the volume surviving while its CONTENTS are
	//     emptied or replaced in place. Nothing daemon-side can see inside a
	//     volume, so this is not an omission that a different design would
	//     have caught more cheaply — it is the boundary itself.
	//   - Never was detected, either before or after: deliberate tampering by
	//     a job. The in-home file was readable and writable by the job that
	//     owned the home, so a job could always record the token, wreck its
	//     home and write the token back (PR2's codex review round 2 corrected
	//     the initial claim to the contrary). The payoff of that attack — "the
	//     next job in this workspace runs against a home this job already
	//     broke" — is available with or without an identity mechanism, in a
	//     single-user personal orchestrator, and is self-harm rather than
	//     escalation.
	//
	// Empty means "written by a build that predates the identity check", or
	// by PR2-PR5 (whose value described a file that no longer exists). Either
	// way it vouches for nothing and triggers a re-init rather than a skip —
	// the fail-safe direction, absorbed by the contractual idempotence of init
	// scripts. In practice the InitGeneration bump below reaches those markers
	// first.
	HomeID string `json:"home_id,omitempty"`
	// SkeletonDirs records the bind-target skeleton the init run this marker
	// vouches for was asked to create, relative to the home
	// (workspaceHomeSkeletonDirs at the time of that run).
	//
	// It is here because PR6 removes the per-dispatch daemon-side mkdir that
	// used to keep this set current. Those directories are the per-embedded-
	// skill bind targets and their ancestors, and an ABSENT one is not a
	// benign condition: the container engine creates the whole missing path
	// itself, at container start, as uid 0, which locks the uid-1000 harness
	// out of ~/.claude/.credentials.json for good (measured on podman 4.9.3
	// with a named-volume home and keep-id, 2026-07-27 — 論点 b-2).
	//
	// The set is a property of the BOID BINARY, not of the workspace: one
	// entry per embedded skill. So a release that adds a skill needs an
	// already-initialized home to grow a bind target, and until PR6 the daemon
	// did that itself on every dispatch. A named volume takes that ability
	// away, which leaves exactly one place the work can happen — the init
	// container — and therefore one thing the marker has to stop vouching for
	// wrongly: "this home was prepared for THIS binary's skeleton". A marker
	// whose set differs from the current one triggers a re-init, on the same
	// footing as a changed script hash.
	//
	// Compared as a SET (order-insensitive): the order comes from embed.FS's
	// directory listing, and treating a reordering there as a change would
	// re-run every workspace's init.sh across an installation for nothing.
	//
	// Empty/absent means a marker from PR2-PR5, which vouched for no skeleton
	// at all; the InitGeneration bump rejects those first.
	SkeletonDirs []string `json:"skeleton_dirs,omitempty"`
	// SkillLinks records the skill symlink set this marker's run created —
	// boid's embedded skills and every loaded Pack's — as
	// skillLinkMarkerStrings(name+"="+target) strings, given the same
	// "changed set forces re-init" treatment as SkeletonDirs and for the
	// same reason: like SkeletonDirs' mkdir -p,
	// buildWorkspaceInitScript's symlink step only runs at all when the
	// marker is stale, so a build that adds, removes or version-bumps a
	// skill needs an already-initialized home to be told its marker no
	// longer describes the current set.
	//
	// This is where the embedded set's staleness is now caught. It used to
	// be caught by SkeletonDirs, which carried one entry per embedded skill
	// while those were bind targets; moving the embedded set onto symlinks
	// moves that job here, and shrinks SkeletonDirs to a constant.
	//
	// This detects a stale set; it does not by itself REPAIR one. A Pack
	// removed (or a skill renamed) between two runs is correctly detected —
	// the string for it is simply absent from the current set — and the
	// re-init this triggers creates every symlink the CURRENT set names, but
	// nothing removes the now-orphaned <root>/<old-name> the previous run
	// left behind (buildWorkspaceInitScript has no "sweep unlisted names"
	// step). That symlink dangles rather than resolving to anything useful,
	// which is a smaller failure than the ones SkeletonDirs guards against
	// (it does not lock the harness out of its home), but it is not nothing:
	// a harness's skill scan still finds it.
	//
	// Empty/absent means "written before this field existed". Markers from
	// the era of the pack_skill_links field this replaces are rejected by
	// the InitGeneration bump that came with the rename, so no marker
	// reaching this comparison can have meant something else by an empty
	// value.
	SkillLinks []string `json:"skill_links,omitempty"`
	// InitGeneration records WHICH EXECUTION ENVIRONMENT ran the init this
	// marker vouches for (workspaceHomeInitGeneration). A marker whose
	// generation is not the current one triggers a re-init, exactly like a
	// changed script hash — see workspaceHomeInitialized.
	//
	// ScriptSHA256 answers "did the instructions change"; this answers "did
	// the machine that carried them out change". Those are independent, and
	// PR5 is the case that proves it: the script content was untouched, but
	// init moved from the daemon process (host `$HOME`,
	// ~/.local/share/boid/homes/<slug>) into a throwaway container whose
	// `$HOME` is the path a JOB sees. What a toolchain installer leaves behind
	// is full of that path — shebangs, wrapper scripts, symlink targets,
	// recorded prefixes — so a home prepared under the old one is not
	// equivalent to a home prepared under the new one, however identical the
	// script.
	//
	// Empty/zero means "written by a build that predates this field", i.e. by
	// the host-exec generation, and is therefore treated as a mismatch rather
	// than as "probably fine" — the same fail-safe reading HomeID gives its own
	// empty value.
	InitGeneration int       `json:"init_generation,omitempty"`
	BoidVersion    string    `json:"boid_version"`
	CompletedAt    time.Time `json:"completed_at"`
}

// workspaceHomeInitGeneration identifies the environment
// resolveWorkspaceHome currently runs an init in. Bump it in any PR that
// changes that environment in a way a prepared home can observe.
//
//	1 — PR5 of docs/plans/workspace-home-volume-persistence.md (論点c): a
//	    throwaway container started from the runner image, with the home
//	    mounted at the path a job sees it at (hostHomeDir()). Generation 0 is
//	    the absence of this field: PR2-PR4, where the daemon process exec'd
//	    /bin/bash itself against the host homes/<slug> path.
//	2 — PR6 (論点a/b): the same container, but the home it mounts is now a
//	    per-workspace docker NAMED VOLUME
//	    (dockerres.WorkspaceHomeVolumeName) rather than a bind of
//	    <runtimesRoot>/homes/<slug>, and the home's identity moved from a file
//	    inside it onto the volume's own label.
//	3 — image-baked skills: the embedded skill set stopped arriving as one
//	    read-only bind mount per skill and became symlinks into
//	    embeddedSkillsImageDir, written by this same container's prelude into
//	    every skillDiscoveryRoots entry. A generation-2 home is not equivalent
//	    to a generation-3 one in a way no other field catches: its
//	    .claude/skills/<name> entries are real DIRECTORIES (the old bind
//	    targets, created by that era's skeleton), and a home whose marker still
//	    matched would keep them — with nothing mounting over them any more,
//	    leaving every embedded skill undiscoverable. The re-init's `rm -rf`
//	    before `ln -sfn` is what converts them, and this bump is what makes it
//	    run. It also retires the marker's pack_skill_links field in favour of
//	    skill_links, whose value set is different (it now includes the embedded
//	    skills), so a generation-2 marker's absent skill_links must not be read
//	    as "no skills were linked".
//
// # Why an existing installation must re-init rather than be grandfathered
//
// Without this field the PR5 skip condition (script hash + home identity) is
// satisfied by every workspace a PR2-PR4 daemon had already prepared, so the
// new execution path would never run on any existing installation and the
// homes would keep the host-era absolute paths baked into them.
//
// The cost of re-running is low and bounded. At generation 1 the home's actual
// STORAGE is unchanged (still the host homes/<slug> directory, bind-mounted
// into the init container) — the toolchain is physically still there, so a
// contractually idempotent init.sh
// (docs/plans/home-workspace-volume.md 「script 作者が守ること」) short-circuits
// most of its work. What the re-run is really for is the artifacts that
// encode a path rather than the payload: symlinks, shebangs, wrapper scripts.
//
// # Why PR6 bumps it even though its fresh volume would re-init anyway
//
// A brand new volume carries a brand new identity label, so PR6's own cutover
// re-initializes on the identity check alone and the bump buys nothing THERE.
// It buys two things elsewhere, and 論点 f asks for both:
//
//   - Generation 1 markers describe a home identity that no longer exists in
//     the form they describe it (a file inside the home, which PR6 stopped
//     writing and stopped reading). Rejecting them on the generation is a
//     statement about the record's vocabulary rather than a coincidence about
//     its values, which is the difference between "cannot match" and "did not
//     happen to match".
//   - PR8's migration CLI copies an existing host home's CONTENTS into the
//     volume, and every route that bypasses PR8 — a manual `docker cp`, a
//     restore from a backup — copies content into a volume whose label
//     already matches its marker. The generation is then the only thing that
//     still disagrees, and it is what forces the one init run that re-writes
//     the host-era absolute paths.
//
// # Why a generation rather than reusing BoidVersion
//
// BoidVersion is already recorded and already changes on every release, so
// comparing it would force a re-init far more often than the environment
// actually changes — every patch release, for every workspace. A generation
// changes when the thing it names changes, which is what makes "the marker
// disagrees" mean something.
const workspaceHomeInitGeneration = 3

// resolveWorkspaceHome ensures the docker named volume that holds the
// workspace identified by workspaceID exists and, if the workspace declares an
// init.sh, that it has been run for the current content of that script. It
// returns three values, in this order: the VOLUME NAME of the (now-ready)
// workspace home, the normalized workspace slug it belongs to, and the identity
// that volume was observed to carry.
//
// All three are strings and the compiler will not catch a transposition, so the
// named results below are the contract: `volumeName` is opaque and never a path,
// `slug` is what goes into BOID_WORKSPACE_SLUG and label values, `homeID` is a
// 64-character hex token that appears in no user-facing surface at all.
//
// # The third return: the identity, threaded to the point of USE (codex Major 1)
//
// homeID is the value of the home volume's dockerres.LabelWorkspaceHomeID
// label as this call observed it — the same value the completion marker
// comparison below was made against, and the one written into a fresh marker
// when an init runs. It is returned rather than kept private because this
// function's check has a horizon: it is finished before either the init
// container or the job container exists, and in between the volume can be
// removed and replaced (an operator's `docker volume rm`, a reap misfire, a
// half-completed workspace remove). Both consumers therefore re-check their own
// VolumeCreate's answer against this value before mounting anything —
// containerBackend.RunWorkspaceInit and containerBackend.ensureNamedVolumes,
// via verifyWorkspaceHomeIdentity, which documents what that narrows and what
// it deliberately does not close.
//
// # The first return value is a volume name, not a path (PR6, 論点a/e)
//
// Through PR5 it was <runtimesRoot>/homes/<slug>, an ordinary directory. That
// root resolves under BOID_RUNTIME_DIR in every real deployment, i.e. tmpfs,
// so a host reboot destroyed every workspace's harness credentials and its
// ~1.5GB toolchain — the regression the whole plan doc exists to repair. It is
// now dockerres.WorkspaceHomeVolumeName(r.InstallID, slug).
//
// Callers must treat it as opaque and NEVER as a path: the container backend
// classifies a mount source as a named volume by exactly one rule, "it does not
// start with /" (internal/sandbox/realization.classifySource — the existing
// convention 論点e's option (i) reuses rather than adding a MountType), so
// filepath.Join'ing anything onto this value silently produces a DIFFERENT
// volume name rather than a path inside the home.
//
// Returning the slug separately is PR4's doing and PR6 is what makes it
// load-bearing: this function is the ONLY place a workspaceID is normalized
// into a slug (normalizeWorkspaceSlug, first statement below), and the caller
// used to recover it with filepath.Base(<first return value>). That worked
// only while a home directory was named after its slug. It is now named
// boid-ws-home-<installID8>-<slug>, so every consumer of
// SandboxRuntimeInfo.WorkspaceSlug (env BOID_WORKSPACE_SLUG, and through it the
// claude/codex/opencode adapters' "CLI not found in workspace $HOME" error,
// which tells the operator which workspace's init.sh to edit) would name a
// workspace that does not exist.
//
// Note that the empty-workspaceID case makes the second return strictly more
// than a refactor: a project with no explicit workspace normalizes to
// orchestrator.DefaultWorkspaceSlug here while the caller's own workspaceID is
// still "", so this is the only in-band source of that value.
//
// Contract (see the plan doc's 契約 section):
//   - the home volume always exists on return (nil error), carrying its own
//     identity label
//   - init runs at most once per distinct script content, per incarnation of
//     the home volume, per skeleton set, AND per execution environment: a
//     completion marker keyed by the script's sha256 short-circuits every later
//     call with the same content, but only while the volume still carries the
//     identity that marker records, only while the marker records the
//     bind-target skeleton this build needs, and only while it records the
//     generation of the environment this build runs inits in (see
//     workspaceHomeInitialized and the three marker fields it reads)
//   - concurrent calls for the same slug serialize on a flock so the script
//     runs exactly once; waiters block until the winner finishes and then
//     re-check the marker. As of PR5 the flock is only HALF of that: it dies
//     with the process, and the init container does not, so the deterministic
//     container name is what keeps a crashed daemon's in-flight init from
//     being raced by its successor (dockerres.WorkspaceInitContainerName)
//   - a failing init script returns an error and leaves no marker, so the
//     next call retries from scratch
//   - every successful return has run the builtin prep (bind-target skeleton)
//     at least once against this volume incarnation, whether or not an init.sh
//     existed to run
//
// Where it runs changed in PR5 (論点c): the daemon does not exec anything
// itself. Both the builtin prep steps and the workspace's own init.sh run
// inside a throwaway container that mounts the home, which is the only way to
// touch a named volume's contents at all. The trusted boundary is unchanged —
// boid picks the image, assembles the script and starts the container; nothing
// about this runs inside a sandboxed job (論点c's A vs A') — but several
// script-visible details did change, and they are recorded in
// docs/plans/home-workspace-volume.md 「契約」and
// docs/{ja,en}/guide/workspace-home.md rather than only here.
//
// The home and the init bookkeeping (marker, lock) live on opposite sides of
// the volume boundary as of PR2 (論点b): the marker and lock are ordinary files
// under <dataHome>/homes-meta/, inside the daemon's OWN persistent volume, so
// file I/O and flock keep working; see workspaceHomeMetaDir. That split is also
// what makes the identity check load-bearing rather than decorative — it gave
// the marker a strictly longer lifetime than the thing it describes, and
// workspaceHomeMarker.HomeID is the counterweight.
//
// Markers left behind in the pre-PR2 location (beside the old homes/ tree) are
// neither read nor deleted; so is the old homes/ tree itself, which PR6 stops
// creating and stops mounting but does not remove. Destroying data the daemon
// has stopped relying on buys nothing, and PR7/PR8 revisit that layout
// deliberately (a migration CLI has to be able to READ those directories).
func (r *Runner) resolveWorkspaceHome(ctx context.Context, workspaceID string) (volumeName, slug, homeID string, err error) {
	slug, err = normalizeWorkspaceSlug(workspaceID)
	if err != nil {
		return "", "", "", err
	}

	metaDir, err := r.workspaceHomeMetaDir()
	if err != nil {
		return "", "", "", fmt.Errorf("workspace home: %w", err)
	}
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		return "", "", "", fmt.Errorf("workspace home: create homes meta dir %q: %w", metaDir, err)
	}

	// Before ANY of the volume work below (codex review of PR8 round 3, Blocker
	// 1): a workspace whose interrupted-migration record is still on disk holds a
	// home nobody vouched for, and the whole point of that record is that no init
	// and no job may run against it. The startup sweep normally clears it, but a
	// sweep whose VolumeRemove failed logs and lets startup continue, so a
	// dispatch is the second — and last — place the invariant can be enforced.
	//
	// Placed above ensureWorkspaceHomeVolume rather than beside the marker check
	// because it is the VOLUME that is wreckage: observing its identity first
	// would mean minting one for a volume about to be removed, and the marker
	// question does not arise at all until the home is known to be a home.
	//
	// See recoverInterruptedWorkspaceHomeMigration for why this repairs in place
	// rather than refusing, and for what it does when the engine is still down.
	if err := r.recoverInterruptedWorkspaceHomeMigration(ctx, metaDir, slug); err != nil {
		return "", "", "", err
	}

	// Computed HERE and handed to everything downstream, rather than
	// recomputed by the backend from its own installID: the job container's
	// mount, the init container's mount and this function's return value have
	// to name the same volume, and two computations of a "deterministic" name
	// are two things that can disagree (r.InstallID and containerBackend's own
	// installID are wired from the same source in server/wire.go, but nothing
	// in the type system says so).
	volumeName = dockerres.WorkspaceHomeVolumeName(r.InstallID, slug)

	executor, err := workspaceInitExecutorFor(r)
	if err != nil {
		return "", "", "", fmt.Errorf("workspace home %q: %w", slug, err)
	}

	scriptPath, err := workspaceInitScriptPath(slug)
	if err != nil {
		return "", "", "", fmt.Errorf("workspace home %q: %w", slug, err)
	}
	scriptBytes, scriptExists, err := readIfExists(scriptPath)
	if err != nil {
		return "", "", "", fmt.Errorf("workspace home %q: read init script %q: %w", slug, scriptPath, err)
	}
	scriptSHA := scriptSHA256Hex(scriptBytes, scriptExists)

	skeleton := workspaceHomeSkeletonDirs()
	links := skillLinks(r.Packs)
	markerPath := workspaceHomeMarkerPath(metaDir, slug)

	homeID, err = r.ensureWorkspaceHomeVolume(ctx, executor, slug, volumeName)
	if err != nil {
		return "", "", "", err
	}

	// Fast path: already initialized for this exact script content, this
	// skeleton, this execution generation AND this volume incarnation. One
	// idempotent VolumeCreate is the whole engine cost — no container.
	if marker, ok, err := readWorkspaceHomeMarker(markerPath); err != nil {
		return "", "", "", fmt.Errorf("workspace home %q: read marker %q: %w", slug, markerPath, err)
	} else if ok && workspaceHomeInitialized(marker, scriptSHA, skeleton, links, homeID, slug, volumeName) {
		return volumeName, slug, homeID, nil
	}

	lockPath := workspaceHomeLockPath(metaDir, slug)
	release, err := acquireWorkspaceHomeLock(lockPath)
	if err != nil {
		return "", "", "", fmt.Errorf("workspace home %q: acquire init lock: %w", slug, err)
	}
	defer release()

	// Re-read + re-hash under the lock (TOCTOU fix, codex review PR #787):
	// the fast-path read above happened before we serialized with any
	// concurrent writer of scriptPath, so scriptBytes/scriptSHA could
	// already be stale by the time we get here. Re-reading now, with the
	// lock held, makes this the authoritative snapshot for both the
	// double-checked marker compare directly below and the bytes actually
	// executed further down — so the hash recorded in the marker can never
	// diverge from the content that ran.
	scriptBytes, scriptExists, err = readIfExists(scriptPath)
	if err != nil {
		return "", "", "", fmt.Errorf("workspace home %q: read init script %q: %w", slug, scriptPath, err)
	}
	scriptSHA = scriptSHA256Hex(scriptBytes, scriptExists)

	// Re-ensure for the same reason the script is re-read: the volume could
	// have been removed and re-created while this call waited on the lock (a
	// concurrent `boid reap --include-workspace-homes`, an operator), and the
	// marker written below must record the identity that was actually in place
	// when the init ran, not one observed before the wait.
	homeID, err = r.ensureWorkspaceHomeVolume(ctx, executor, slug, volumeName)
	if err != nil {
		return "", "", "", err
	}

	// Double-checked: another dispatch may have finished init while we were
	// waiting on the lock. Same identity check as the fast path — a marker
	// alone still does not prove the volume behind it survived.
	if marker, ok, err := readWorkspaceHomeMarker(markerPath); err != nil {
		return "", "", "", fmt.Errorf("workspace home %q: read marker %q: %w", slug, markerPath, err)
	} else if ok && workspaceHomeInitialized(marker, scriptSHA, skeleton, links, homeID, slug, volumeName) {
		return volumeName, slug, homeID, nil
	}

	// One throwaway container per init, ALWAYS — not only when the workspace
	// declares an init.sh (PR5 §D1, 論点b-2). It carries the bind-target
	// skeleton and the workspace's own script, if it has one, in a single
	// start. The skeleton is needed by every workspace, so making the run
	// conditional the way the old host exec was would leave the pass-through
	// class (docs/plans/home-workspace-volume.md 「script が無い workspace は
	// 素通し (マーカーだけ打つ)」) with nothing to create its ~/.claude — and the
	// container engine would then create it as uid 0 at the first job launch.
	req, err := buildWorkspaceInitRequest(workspaceInitParams{
		Slug:       slug,
		HomeSource: volumeName,
		// hostHomeDir() is the path a JOB container sees its own $HOME at
		// (BuildSandboxSpec's homeDir). Preparing the home under the very
		// same path is not cosmetic: a toolchain installer bakes absolute
		// paths into wrapper scripts, shebangs and symlinks, so anything
		// init.sh installs under a path the harness does not later run under
		// is installed wrong. Before PR5 those two genuinely differed — the
		// script saw the host homes/<slug> path — which is why
		// docs/examples/workspace-home-init.sh carries a helper whose only job
		// is to rewrite absolute $HOME symlinks as relative ones.
		HomeTarget:   hostHomeDir(),
		SkeletonDirs: skeleton,
		SkillLinks:   links,
		Script:       scriptBytes,
		ScriptExists: scriptExists,
		HomeID:       homeID,
	})
	if err != nil {
		return "", "", "", err
	}
	if err := executor.RunWorkspaceInit(ctx, req); err != nil {
		return "", "", "", fmt.Errorf("workspace home %q: init failed: %w", slug, err)
	}

	marker := workspaceHomeMarker{
		ScriptSHA256: scriptSHA,
		HomeID:       homeID,
		SkeletonDirs: skeleton,
		SkillLinks:   skillLinkMarkerStrings(links),
		// Recorded from the constant, never from the marker being replaced:
		// this marker vouches for the run that just happened, in the
		// environment this build runs inits in.
		InitGeneration: workspaceHomeInitGeneration,
		BoidVersion:    version.Version(),
		CompletedAt:    time.Now().UTC(),
	}
	if err := writeWorkspaceHomeMarker(markerPath, marker); err != nil {
		return "", "", "", fmt.Errorf("workspace home %q: write marker: %w", slug, err)
	}

	return volumeName, slug, homeID, nil
}

// ensureWorkspaceHomeVolume makes sure volumeName exists and reports the
// identity it carries (dockerres.LabelWorkspaceHomeID).
//
// # Why an empty identity is a hard error rather than a re-init
//
// Everywhere else in this file, doubt resolves to "run the init again" — the
// fail-safe direction, whose whole cost is one run of a contractually
// idempotent script. That answer only works when the re-run RESTORES the
// invariant. Here it does not: the Engine API has no way to add a label to an
// existing volume (VolumeCreate returns the existing volume untouched, which is
// the very property the fast path is built on), so a volume without an identity
// stays without one however many times init runs. "Fail-safe" would be a
// livelock — an init container, and on a real workspace a multi-GB toolchain
// re-check, on EVERY dispatch, forever, with nothing in the logs saying why.
//
// And the state is not one boid can reach on its own. Both paths that can bring
// a workspace HOME volume into existence stamp an identity: this one, and
// containerBackend.ensureNamedVolumes when Launch finds the volume gone. So an
// unlabeled volume under a boid-owned name was made by something else, and
// telling the operator that — with the name, the label and what to do — is
// strictly more useful than absorbing it.
func (r *Runner) ensureWorkspaceHomeVolume(ctx context.Context, executor WorkspaceInitExecutor, slug, volumeName string) (string, error) {
	candidate, err := newWorkspaceHomeID()
	if err != nil {
		return "", fmt.Errorf("workspace home %q: mint home id: %w", slug, err)
	}
	homeID, err := executor.EnsureWorkspaceHomeVolume(ctx, WorkspaceHomeVolumeRequest{
		Slug:        slug,
		Name:        volumeName,
		CandidateID: candidate,
	})
	if err != nil {
		return "", fmt.Errorf("workspace home %q: %w", slug, err)
	}
	if homeID == "" {
		return "", fmt.Errorf(
			"workspace home %q: the volume %q exists but carries no %s label, so boid cannot tell whether the "+
				"completion marker it holds describes THIS volume's contents. Every volume boid creates for a workspace "+
				"home is labelled at creation, and the Engine API cannot add a label to an existing volume, so this "+
				"cannot be repaired by re-running the init — it would re-run on every dispatch forever. "+
				"[復旧] その volume が不要なら削除する (`docker volume rm %[2]s`) — 次の dispatch が正しい label 付きで作り直す。"+
				"中身を残したい場合は、別名の volume に退避してから削除し、PR8 の移行経路で入れ直すこと",
			slug, volumeName, dockerres.LabelWorkspaceHomeID)
	}
	return homeID, nil
}

// workspaceHomeInitialized reports whether marker may be trusted to
// short-circuit init for the workspace home volume volumeName, whose current
// identity label is homeID (docs/plans/workspace-home-volume-persistence.md
// 論点b).
//
// Every "no" answer means one extra run of a contractually idempotent
// init.sh, which is why every uncertain case answers no:
//
//   - the script content it records is not the current one.
//   - marker.InitGeneration is not the current one — the run it records
//     happened in a different execution environment, so the home it produced
//     is not the home this build would produce. Zero (the field's absence)
//     means a PR2-PR4 daemon that ran init on the host, 1 means a PR5 daemon
//     that ran it against a bind-mounted host directory; see
//     workspaceHomeInitGeneration. The comparison is deliberately an equality
//     and not a floor, so a rolled-back release re-inits too.
//   - marker.HomeID empty — written before the identity check existed, so it
//     vouches for nothing.
//   - the identity differs — the volume was deleted and re-created since this
//     marker was written (a reap misfire, an operator's `docker volume rm`, a
//     half-completed workspace remove), so it is empty and the marker
//     describes contents that are gone. This is the case the whole identity
//     mechanism exists for; see workspaceHomeMarker.HomeID for what it does
//     and does not cover.
//   - the recorded bind-target skeleton is not the set this build needs —
//     typically a release that added an embedded skill. Left unnoticed, the
//     new bind target would be created by the container engine as uid 0 at
//     the next job launch and the workspace home would be permanently
//     unwritable by the harness (論点 b-2); see
//     workspaceHomeMarker.SkeletonDirs.
//
// homeID is never empty here: ensureWorkspaceHomeVolume refuses to return an
// unlabelled volume at all, for reasons that are about convergence rather than
// about trust — see its doc comment.
func workspaceHomeInitialized(marker workspaceHomeMarker, scriptSHA string, skeleton []string, links []SkillLink, homeID, slug, volumeName string) bool {
	if marker.ScriptSHA256 != scriptSHA {
		return false
	}
	if marker.InitGeneration != workspaceHomeInitGeneration {
		slog.Info("workspace home: completion marker records a different init execution environment, re-running init (docs/plans/workspace-home-volume-persistence.md 論点c)",
			"workspace_slug", slug, "home_volume", volumeName,
			"marker_generation", marker.InitGeneration, "current_generation", workspaceHomeInitGeneration)
		return false
	}
	if marker.HomeID == "" {
		slog.Info("workspace home: completion marker predates the home identity check, re-running init",
			"workspace_slug", slug, "home_volume", volumeName)
		return false
	}
	if marker.HomeID != homeID {
		slog.Warn("workspace home: the home volume's identity does not match the completion marker; the volume was deleted and re-created since it was initialized, re-running init (docs/plans/workspace-home-volume-persistence.md 論点b)",
			"workspace_slug", slug, "home_volume", volumeName)
		return false
	}
	if !equalStringSets(marker.SkeletonDirs, skeleton) {
		slog.Info("workspace home: completion marker records a different bind-target skeleton (typically a release that added an embedded skill), re-running init (docs/plans/workspace-home-volume-persistence.md 論点b-2)",
			"workspace_slug", slug, "home_volume", volumeName,
			"marker_skeleton", marker.SkeletonDirs, "current_skeleton", skeleton)
		return false
	}
	if currentLinks := skillLinkMarkerStrings(links); !equalStringSets(marker.SkillLinks, currentLinks) {
		slog.Info("workspace home: completion marker records a different skill symlink set (an embedded skill or a Pack was added/removed/upgraded), re-running init",
			"workspace_slug", slug, "home_volume", volumeName,
			"marker_skill_links", marker.SkillLinks, "current_skill_links", currentLinks)
		return false
	}
	return true
}

// equalStringSets reports whether a and b hold the same elements, ignoring
// order and without mutating either argument.
//
// Order-insensitive on purpose: workspaceHomeInitialized's SkeletonDirs
// comparison needs it because that order comes from embed.FS's directory
// listing, and a reordering there says nothing about what a home needs —
// treating it as a change would re-run every workspace's init.sh across an
// entire installation. The SkillLinks comparison added alongside it
// reuses the same reasoning for the same reason: LoadPacks' enumeration
// order is a directory listing too.
func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// newWorkspaceHomeID mints a workspace home volume's identity token: 32 bytes
// of crypto/rand, hex-encoded, destined for the volume's
// dockerres.LabelWorkspaceHomeID label.
//
// crypto/rand rather than anything derived from the slug, the clock or the
// name: the token has to differ between two incarnations of the SAME volume
// name, which is exactly what a derivation from any of those would fail to do.
func newWorkspaceHomeID() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// normalizeWorkspaceSlug maps a JobSpec/Project WorkspaceID to the slug used
// to key a workspace home directory. An empty WorkspaceID (a project not
// assigned to any explicit workspace) normalizes to the default workspace's
// slug, matching resolveWorkspaceProxy's treatment of the same field
// elsewhere in this package.
func normalizeWorkspaceSlug(workspaceID string) (string, error) {
	if workspaceID == "" {
		return orchestrator.DefaultWorkspaceSlug, nil
	}
	if err := orchestrator.ValidWorkspaceSlug(workspaceID); err != nil {
		return "", fmt.Errorf("workspace home: %w", err)
	}
	return workspaceID, nil
}

// workspaceDataHomeRoot returns ~/.local/share/boid (or
// $XDG_DATA_HOME/boid), matching the XDG_DATA_HOME-first convention used
// throughout this codebase (e.g. cmd/start.go's defaultDBPath).
func workspaceDataHomeRoot() (string, error) {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "boid"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("could not determine user home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "boid"), nil
}

// WorkspaceHomesDir returns ~/.local/share/boid/homes, the directory under
// which every workspace's home USED to live
// (docs/plans/home-workspace-volume.md 「レイアウト」).
//
// # PR6 status: the dispatcher no longer writes here
//
// As of PR6 of docs/plans/workspace-home-volume-persistence.md a workspace home
// is a docker named volume (dockerres.WorkspaceHomeVolumeName), so
// resolveWorkspaceHome neither creates nor reads anything under this
// directory. PR7 finished 論点 a-2's rewiring, so internal/api's
// size-reporting, orphan-detection and `workspace remove` handlers now go
// through the engine (dispatcher.WorkspaceHomeVolumeStore) rather than
// through this path, and the intermediate state PR6 documented here — silent
// no-op deletes, empty sizes, no orphans — is over.
//
// This function is kept, and still exported, for the legacy root itself: it
// is where the pre-PR6 homes live, and PR8's migration CLI resolves them
// from here — `boid workspace import-home`'s --from default is
// <this>/<slug>, computed in cmd/workspace_import_home.go's
// defaultWorkspaceHomeSource with an EMPTY runtimesDir (a CLI has no way to
// know the daemon's, and the fallback branch below is exactly where a pre-PR6
// install put them).
//
// Existing directories under this root are NOT deleted by PR6, and PR8 does
// not delete them either: the migration only READS them, which is what makes
// it re-runnable and what makes a failed migration recoverable.
//
// Prefers deriving from runtimesDir when non-empty. Deriving homes/ from
// whatever root the runtime dirs use means a daemon instance running against
// an isolated data dir (e.g. every test in this codebase that spins up a
// real server) gets its own isolated homes/ next to its own runtimes,
// instead of every such instance converging on one global
// ~/.local/share/boid/homes and leaking into the real developer machine's
// home directory during `go test`. Falls back to the $XDG_DATA_HOME /
// ~/.local/share/boid convention only when runtimesDir is empty (minimal
// test wiring that constructs a bare &Runner{}, or a daemon build that never
// wired RuntimesDir).
//
// Note that root is NOT the daemon's persistent data root, despite what this
// comment claimed while the userns backend still existed (it named
// runtimesDirFor(cfg) — filepath.Dir(cfg.DBPath) + "/runtimes" — and called
// it "the same per-installation data root skills/ already lives under"; the
// daemon stopped writing a skills/ dir there at all in PR3 of
// docs/plans/workspace-home-volume-persistence.md).
// server/wire.go wires Runner.RuntimesDir from hostVisibleRuntimesDirFor(cfg)
// unconditionally, which resolves under cfg.SocketPath's dir =
// BOID_RUNTIME_DIR, typically tmpfs — see that function's own doc comment. That
// tmpfs is precisely the persistence regression PR6 repaired by moving the home
// off this root entirely.
func WorkspaceHomesDir(runtimesDir string) (string, error) {
	if runtimesDir != "" {
		return filepath.Join(filepath.Dir(runtimesDir), "homes"), nil
	}
	root, err := workspaceDataHomeRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "homes"), nil
}

// workspaceHomeMetaDir returns <dataHome>/homes-meta, the directory holding
// the per-workspace init completion marker and the per-workspace init lock
// (docs/plans/workspace-home-volume-persistence.md 論点b, PR2).
//
// PR2 also put the private temp copy of init.sh here, so that
// runWorkspaceInitScript's "the executed bytes sit in the same lock-serialized
// directory as the lock" argument stayed self-contained. PR5 removed that file
// along with the host exec itself: the bytes now reach the init container on
// its stdin, and the TOCTOU property they were there for is carried by the
// heredoc instead (see buildWorkspaceInitRequest).
//
// dataHome is the daemon's OWN persistent data root — server/wire.go's
// dataHomeFor(cfg). In a real deployment (cfg.DBPath is a real file) that is
// filepath.Dir(cfg.DBPath), i.e. the directory boid.db / web_secret /
// install_id / secret.key / tls/ / kits/ already live in, which
// under the volume-only compose deploy is the `boid_state` named volume.
// dataHomeFor has two further branches that only arise in test wiring — an
// in-memory or unset DB path falls back to filepath.Dir(cfg.SocketPath), and
// a config with neither yields "" — so "= filepath.Dir(cfg.DBPath)" is the
// production answer, not the whole function; see dataHomeFor's own doc
// comment. The "" case is handled by this function's own fallback below, NOT
// by dataHomeFor's other callers' "feature disabled" reading of it.
//
// That is a different root from the one homes/ is derived from:
// WorkspaceHomesDir derives from RuntimesDir, which server/wire.go resolves
// via hostVisibleRuntimesDirFor(cfg) to BOID_RUNTIME_DIR — typically
// /run/user/<uid>, i.e. tmpfs (see hostVisibleRuntimesDirFor's own doc
// comment for why that root has to stay host-visible).
//
// Two reasons for the split:
//
//   - Durability, now. Sitting beside the homes meant the marker and the
//     lock were already living on tmpfs under the container backend, so a
//     host reboot silently dropped them. Nothing was corrupted by that (a
//     missing marker just re-runs a contractually idempotent init.sh), but
//     the daemon's own bookkeeping does not belong on a volatile root.
//   - Reachability, next. PR6 turns each workspace home into a named volume,
//     whose contents the daemon can only touch through a container. Ordinary
//     file I/O and flock keep working here precisely because homes-meta/ is
//     inside the daemon's own volume, not the workspace's.
//
// The naming pairs with homes/ deliberately, so `grep homes-meta` and `grep
// WorkspaceHomesDir` lead to each other.
//
// Falls back to the $XDG_DATA_HOME / ~/.local/share/boid convention when
// dataHome is empty — the same fallback, for the same reason, as
// WorkspaceHomesDir's: a bare &Runner{} (minimal test wiring, or a daemon
// build that never wired DataHomeDir) must not write into whatever real
// $HOME `go test` happens to run under.
func workspaceHomeMetaDir(dataHome string) (string, error) {
	if dataHome != "" {
		return filepath.Join(dataHome, "homes-meta"), nil
	}
	root, err := workspaceDataHomeRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "homes-meta"), nil
}

// workspaceHomeMetaDir is a thin *Runner-bound wrapper around the free
// function of the same name (mirrors workspaceHomesDir/WorkspaceHomesDir).
func (r *Runner) workspaceHomeMetaDir() (string, error) {
	return workspaceHomeMetaDir(r.DataHomeDir)
}

// workspaceHomeMarkerPath returns metaDir/<slug>.init.json.
func workspaceHomeMarkerPath(metaDir, slug string) string {
	return filepath.Join(metaDir, slug+".init.json")
}

// workspaceHomeLockPath returns metaDir/<slug>.lock, the flock used to
// serialize concurrent init runs for the same slug.
func workspaceHomeLockPath(metaDir, slug string) string {
	return filepath.Join(metaDir, slug+".lock")
}

// workspaceInitScriptPath returns ~/.config/boid/workspaces/<slug>/init.sh
// (or $XDG_CONFIG_HOME's equivalent), mirroring
// orchestrator.DefaultWorkspaceDir's XDG_CONFIG_HOME-via-os.UserConfigDir
// convention. This is a plain host-config file, not a DB-backed workspace
// resource (docs/plans/home-workspace-volume.md 「置き場」決定: init.sh is
// environment-dependent shell content, outside the workspace's otherwise
// environment-independent DB-backed config, and stays in the same
// file-based category as host_commands.yaml).
func workspaceInitScriptPath(slug string) (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("could not determine user config directory: %w", err)
	}
	return filepath.Join(configDir, "boid", "workspaces", slug, "init.sh"), nil
}

// readIfExists reads path, returning (nil, false, nil) when it does not
// exist rather than an error.
//
// Deliberately unhardened, and only usable for paths NO sandboxed job can
// write to through its own $HOME mount. Its two remaining callers qualify:
// the completion marker (<dataHome>/homes-meta/, the daemon's own data root)
// and the init script (~/.config/boid/workspaces/<slug>/init.sh, plain host
// config). The workspace home's identity file was the third and did NOT
// qualify: it sat inside storage a job owns read-write, so a bare os.ReadFile
// on it let a job wedge the daemon by leaving a FIFO or a /dev/zero symlink
// behind (PR2's codex review round 2, Blocker). PR5 gave it a hardened reader;
// PR6 deleted the file outright, moving the identity onto the volume's own
// label where the daemon asks the ENGINE rather than the filesystem
// (workspaceHomeMarker.HomeID). Adding a caller here means first checking that
// its path is on the daemon's side of that line. Both of the current ones used to carry a residual exposure — a job
// holding capabilities.docker could mount the boid_state volume by name from a
// sibling container — which PR2.5 closed by reserving the daemon's own volumes
// in the dockerproxy policy (see DetectDaemonStateVolumes). That closure is
// conditional on the daemon being able to identify its own container, so the
// rule above still stands on its own rather than on the proxy holding.
func readIfExists(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return data, true, nil
}

// scriptSHA256Hex returns the hex-encoded sha256 of scriptBytes, or the
// empty string when exists is false — the documented "init not required"
// marker value (docs/plans/home-workspace-volume.md).
func scriptSHA256Hex(scriptBytes []byte, exists bool) string {
	if !exists {
		return ""
	}
	sum := sha256.Sum256(scriptBytes)
	return hex.EncodeToString(sum[:])
}

// readWorkspaceHomeMarker reads and parses the completion marker at path.
// Returns (zero, false, nil) when the file does not exist. A marker that
// exists but fails to parse (truncated write, manual edit) is treated the
// same as "does not exist" rather than a hard error — resolveWorkspaceHome
// then re-runs init and overwrites the corrupt marker with a fresh one,
// which is safe precisely because init scripts are contractually idempotent
// (docs/plans/home-workspace-volume.md 「script 作者が守ること」).
func readWorkspaceHomeMarker(path string) (workspaceHomeMarker, bool, error) {
	data, exists, err := readIfExists(path)
	if err != nil {
		return workspaceHomeMarker{}, false, err
	}
	if !exists {
		return workspaceHomeMarker{}, false, nil
	}
	var m workspaceHomeMarker
	if err := json.Unmarshal(data, &m); err != nil {
		slog.Warn("workspace home: corrupt completion marker, treating as absent and re-initializing",
			"path", path, "error", err)
		return workspaceHomeMarker{}, false, nil
	}
	return m, true, nil
}

// writeWorkspaceHomeMarker writes marker to path via a sibling temp file +
// rename, so concurrent readers (or a crash mid-write) never observe a
// partially written marker.
//
// homesMetaBestEffortDurability, deliberately: see that constant, and
// TestWriteWorkspaceHomeMarker_ToleratesADirectorySyncFailure for the behaviour
// it buys.
func writeWorkspaceHomeMarker(path string, marker workspaceHomeMarker) error {
	return writeWorkspaceHomeJSONFile(path, marker, homesMetaBestEffortDurability)
}

// homesMetaDurability says what a file under <dataHome>/homes-meta/ needs from
// its parent-directory fsync — the step that decides whether a rename or an
// unlink survives the machine losing power.
//
// It is a policy rather than a constant because the two files in that directory
// want OPPOSITE answers when that fsync fails, and getting one of them wrong is
// invisible until an incident (codex review of PR8 round 3, Major).
type homesMetaDurability int

const (
	// homesMetaBestEffortDurability: try to flush, ignore a failure.
	//
	// This is the completion marker's policy. What the marker needs from this
	// writer is ATOMICITY — a concurrent dispatch must never read a half-written
	// one — and atomicity comes from the temp-file + rename, not from the fsync.
	// Losing the rename to a power cut costs exactly one extra run of a
	// contractually idempotent init.sh, which is the direction every uncertain
	// marker already resolves to (workspaceHomeInitialized). Failing hard instead
	// would convert a storage hiccup into a failed dispatch for that workspace —
	// strictly worse than the outcome being avoided.
	homesMetaBestEffortDurability homesMetaDurability = iota

	// homesMetaRequiredDurability: a failure to flush is a failure to write.
	//
	// This is the interrupted-migration record's policy, and its whole existence
	// is the argument. The record is written immediately BEFORE the home volume is
	// destroyed, so that a daemon which stops existing mid-migration leaves
	// something the next start can recognize. A record that is still only in the
	// page cache records nothing, and reporting success for one would let the
	// migration proceed to destroy the volume while its only safety net may not
	// be there after a crash — reintroducing exactly the state
	// workspace_home_migration_sentinel.go exists to make impossible (a partial
	// home, no marker, no record, and a dispatch that runs init.sh over it).
	//
	// Failing here is cheap in a way failing anywhere later is not: nothing has
	// been destroyed yet, so the operator still has the workspace they had and can
	// re-run once the storage is healthy.
	homesMetaRequiredDurability
)

// writeWorkspaceHomeJSONFile is the durable-write discipline every file under
// <dataHome>/homes-meta/ is written with: marshal, write to a sibling temp file,
// chmod 0600, fsync it, rename over the target, then fsync the parent directory.
//
// Each step earns its place, and the two consumers need them for different
// reasons. The completion marker (workspaceHomeMarker) needs the ATOMICITY: a
// concurrent dispatch reading a half-written marker would see a shorter script
// hash and re-run an init for no reason. The interrupted-migration record
// (workspaceHomeMigrationRecord) needs the DURABILITY: its whole job is to
// survive the machine going away, so a record that is still only in the page
// cache when the power cuts records nothing at all.
//
// Extracted from writeWorkspaceHomeMarker rather than duplicated (PR8 round 2,
// Major 2) because "the record is fsync'd" is an argument the sweep depends on,
// and an argument that lives in one place cannot be half-true in another.
//
// durability is what round 3's Major added: sharing the steps must not mean
// sharing the answer to "what if the last one fails". See homesMetaDurability
// for why the marker and the record want opposite answers.
func writeWorkspaceHomeJSONFile(path string, value any, durability homesMetaDurability) (retErr error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", filepath.Base(path), err)
	}
	dir := filepath.Dir(path)
	// The pattern ends in ".tmp" on purpose: SweepInterruptedWorkspaceHomeMigrations
	// enumerates this directory by suffix, and a temp file that could end in
	// ".migrating.json" would be read as a record of its own.
	tmp, err := os.CreateTemp(dir, ".homes-meta.*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file beside %s: %w", filepath.Base(path), err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file for %s: %w", filepath.Base(path), err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file for %s: %w", filepath.Base(path), err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file for %s: %w", filepath.Base(path), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file for %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename %s into place: %w", filepath.Base(path), err)
	}
	cleanup = false
	// The parent dir fsync, which is what makes the rename above survive the
	// machine going away rather than merely surviving this process. Whether a
	// failure here fails the write is the caller's call — see homesMetaDurability.
	if err := fsyncDir(dir); err != nil {
		if durability == homesMetaRequiredDurability {
			return fmt.Errorf(
				"sync the directory holding %s so the write survives a crash: %w (the file is on disk but nothing "+
					"guarantees it stays there, and this record only has value if it does)", filepath.Base(path), err)
		}
		slog.Warn("workspace home: could not sync the homes-meta directory after writing; the write may not survive a power cut",
			"path", path, "error", err)
	}
	return nil
}

// mkdirAllDurable is os.MkdirAll for a directory whose CONTENTS will be written
// with homesMetaRequiredDurability — i.e. one whose own existence has to survive
// a power cut too (codex review of PR8 round 4, Blocker 2).
//
// # The hole it closes
//
// writeWorkspaceHomeJSONFile fsyncs the directory it renames into, which makes
// the RENAME durable. It says nothing about the directory itself: a directory's
// entry lives in its PARENT, so a homes-meta/ that was created moments ago and
// never had <dataHome> flushed can be lost whole by a power cut — record and all
// — while the partial home volume the record described survives in the engine's
// own store. The next start's sweep then reads "no homes-meta" as "no
// interrupted migration", the next dispatch re-creates the directory, and
// init.sh runs against a partial home with nothing left to say so. That is
// exactly the state workspace_home_migration_sentinel.go exists to make
// impossible, reached by losing the sentinel rather than by never writing it.
//
// # How far up it flushes, and why
//
//   - The parent of every directory this call CREATED. MkdirAll can create
//     several levels (a fresh install where <dataHome> itself is not there yet),
//     and each level's entry lives one level up, so each level needs its own
//     parent flushed. Flushing only the deepest would leave the whole subtree
//     hanging off an entry that is still only in the page cache.
//   - The immediate parent of path even when NOTHING was created. This is not
//     belt-and-braces: homes-meta/ is created by whichever of resolveWorkspaceHome
//     and ImportWorkspaceHome gets there first, and the former's MkdirAll is
//     deliberately not durable (the only file it writes there is the completion
//     marker, whose policy is homesMetaBestEffortDurability). So a migration can
//     find the directory already present and still be standing on an entry a
//     dispatch created seconds earlier without flushing. The cost of asking
//     unconditionally is one fsync of one directory per migration, against a
//     multi-GB extraction.
//
// It deliberately stops there rather than walking to the filesystem root. Every
// ancestor above <dataHome> either was created by this call (covered by the
// first rule) or is part of the installation's long-lived layout — <dataHome>
// itself holds boid.db, whose SQLite writes have flushed that entry many times
// over — and an unbounded walk would spend fsyncs on / and /home to defend
// against a directory tree that vanished for other reasons.
//
// A failure is returned rather than logged: the one caller needs the answer
// before it destroys anything, and "the directory may not survive" has the same
// consequence there as "the record may not survive" — see homesMetaDurability.
func mkdirAllDurable(path string, perm os.FileMode) error {
	// Collect the missing prefix BEFORE creating anything; afterwards there is no
	// way to tell which levels this call was responsible for.
	var created []string
	for p := filepath.Clean(path); ; {
		if _, err := os.Stat(p); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat %q: %w", p, err)
		}
		created = append(created, p)
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}

	if err := os.MkdirAll(path, perm); err != nil {
		return err
	}

	// Deduplicated because the unconditional entry below overlaps the created
	// set whenever path itself was created.
	dirs := make([]string, 0, len(created)+1)
	seen := map[string]bool{}
	for _, dir := range append(created, filepath.Clean(path)) {
		parent := filepath.Dir(dir)
		if parent == dir || seen[parent] {
			continue
		}
		seen[parent] = true
		dirs = append(dirs, parent)
	}
	for _, dir := range dirs {
		if err := fsyncDir(dir); err != nil {
			return fmt.Errorf(
				"sync %q so that %q survives a crash: %w (a directory entry that is only in the page cache can be lost "+
					"whole, taking the interrupted-migration record inside it with it)", dir, path, err)
		}
	}
	return nil
}

// fsyncDir flushes a directory's own metadata — i.e. makes a rename or an
// unlink inside it survive a power cut — and is a variable only so tests can
// make it fail: there is no portable way to provoke an fsync(2) error on healthy
// storage, and the branches that depend on one are exactly the branches that
// only matter when the storage is not healthy.
var fsyncDir = func(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// acquireWorkspaceHomeLock opens (creating if needed) lockPath and takes an
// exclusive advisory flock on it, returning a release function that MUST be
// called (typically via defer) to unlock and close the file. Mirrors
// internal/profiles/write.go's LockConfigMutation and
// internal/client/autostart.go's ensureRunning lock pattern.
func acquireWorkspaceHomeLock(lockPath string) (release func(), err error) {
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %q: %w", lockPath, err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("flock %q: %w", lockPath, err)
	}
	return func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}, nil
}
