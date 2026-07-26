package dispatcher

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
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
// different one: the daemon must keep ordinary file I/O + flock access to
// its own bookkeeping once PR6 turns each home into a named volume.
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
	// home directory it describes (docs/plans/workspace-home-volume-persistence.md
	// 論点b). It holds the same random token that was written into
	// <homeDir>/.boid-workspace-home-id at the end of the init run this
	// marker records.
	//
	// Why a marker needs this at all: before PR2 the marker sat in the same
	// parent directory as the home, so the two could only ever vanish
	// together and "a marker exists" implied "the home it describes exists".
	// PR2 moved the marker into the daemon's own persistent data root while
	// the home stayed derived from RuntimesDir — which under the container
	// backend resolves below BOID_RUNTIME_DIR, i.e. tmpfs. A host reboot now
	// drops the home and keeps the marker, and PR6 adds more ways for that to
	// happen (a stray `docker volume rm`, a reap misfire, a half-completed
	// workspace remove). Without an identity to compare, the next dispatch
	// would skip init.sh and hand the agent an empty $HOME; the error that
	// surfaces is the adapter's "CLI not found"
	// (internal/adapters/claude/run.go), which points at an unconfigured
	// init.sh rather than at the vanished home.
	//
	// Empty means "written by a build that predates the identity check". Such
	// a marker is treated as unverifiable and triggers a re-init rather than
	// a skip — the fail-safe direction, absorbed by the contractual
	// idempotence of init scripts.
	//
	// The comparison this field exists for detects ACCIDENTAL loss or
	// replacement of the home, not deliberate tampering by a job that owns
	// the home's other copy of the token; see workspaceHomeNonceFileName for
	// why that limit is structural and why it costs nothing.
	HomeID string `json:"home_id,omitempty"`
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
// # Why this cannot wait for PR6
//
// PR6 gives each workspace a FRESH named volume, which carries no identity
// file, so the nonce check alone would force a re-init there — the generation
// would be redundant if PR6 were the only sequel. It is not: PR8's migration
// CLI copies an existing host home's CONTENTS into that volume, and a faithful
// copy includes <home>/.boid-workspace-home-id. The nonce then MATCHES on the
// other side and init is skipped over a volume full of host-era paths, with
// nothing anywhere reporting a problem. The generation is what still
// disagrees. See docs/plans/workspace-home-volume-persistence.md 論点 f for
// the corresponding note on PR8's side.
//
// # Why a generation rather than reusing BoidVersion
//
// BoidVersion is already recorded and already changes on every release, so
// comparing it would force a re-init far more often than the environment
// actually changes — every patch release, for every workspace. A generation
// changes when the thing it names changes, which is what makes "the marker
// disagrees" mean something.
const workspaceHomeInitGeneration = 1

// workspaceHomeNonceFileName is the basename of the identity file written
// INSIDE the workspace home, whose content must match the governing marker's
// HomeID for init to be skipped (docs/plans/workspace-home-volume-persistence.md
// 論点b).
//
// It lives inside the home on purpose: the whole point is to be destroyed by
// whatever destroys the home. That also means a sandboxed job — which gets an
// rw mount of this exact directory — can read, rewrite, delete or replace it.
// What this check is for, and what it is not for, follows from that:
//
//   - What it detects (the reason it exists): ACCIDENTAL desync between the
//     marker and the home it describes. A host reboot clearing the tmpfs
//     homes/ lives on, a stray `docker volume rm`, a reap misfire, a
//     half-completed workspace remove, a home restored from a backup of a
//     different incarnation. Every one of those takes this file with it or
//     brings a different one along, so detection is complete for that class
//     — which is the class the split of marker and home lifetimes actually
//     created.
//
//   - What it does NOT detect: a job that deliberately wrecks its own $HOME
//     while preserving (or restoring) this file, so the next dispatch skips
//     init against a broken home. That is structurally unpreventable, not an
//     oversight: the token has to share the home's lifetime, so it has to
//     live in the home, and the home is rw-owned by the job. Anything put
//     there can be read and replayed by the job. The payoff of that attack
//     is "the next job in this same workspace runs against a home this job
//     already broke" — and a job can break its own workspace home with or
//     without a nonce, so the mechanism does not widen anything. In a
//     single-user personal orchestrator that is self-harm, not privilege
//     escalation.
//
// The one thing tampering must never do is take out the DAEMON, because that
// would reach other tasks. That is a property of how this file is READ, not
// of what it contains — see readWorkspaceHomeNonce, which refuses symlinks,
// non-regular files (a FIFO left behind would otherwise block the daemon
// forever) and oversized content. With that read in place, every reachable
// outcome of tampering is bounded by one extra run of a contractually
// idempotent init.sh (docs/plans/home-workspace-volume.md 「script 作者が守ること」)
// — the same cost an upgrade already imposes once.
const workspaceHomeNonceFileName = ".boid-workspace-home-id"

// resolveWorkspaceHome ensures the on-disk home directory for the workspace
// identified by workspaceID exists and, if the workspace declares an
// init.sh, that it has been run for the current content of that script. It
// returns the absolute path to the (now-ready) workspace home directory and
// the normalized workspace slug that directory belongs to, in that order.
//
// Returning the slug is PR4 of docs/plans/workspace-home-volume-persistence.md.
// It is not a convenience: this function is the ONLY place a workspaceID is
// normalized into a slug (normalizeWorkspaceSlug, first statement below), and
// before PR4 its one caller recovered that slug with
// filepath.Base(<returned home dir>) — correct only while workspaceHomeDirFor
// happens to name a home directory after its slug. PR6 replaces that name
// with a per-workspace named volume (boid-ws-home-<installID8>-<slug>, 論点a),
// so the basename stops being the slug; every consumer of
// SandboxRuntimeInfo.WorkspaceSlug (env BOID_WORKSPACE_SLUG, and through it
// the claude/codex/opencode adapters' "CLI not found in workspace $HOME"
// error, which tells the operator which workspace's init.sh to edit) would
// then silently name a workspace that does not exist. Handing the slug back
// from where it is decided removes the second derivation entirely rather than
// keeping two computations that have to be kept in agreement.
//
// Note that the empty-workspaceID case makes this strictly more than a
// refactor even today: a project with no explicit workspace normalizes to
// orchestrator.DefaultWorkspaceSlug here, and the caller's own workspaceID is
// still "" — so the returned slug is the only in-band source of that value
// that does not go through the path.
//
// Phase 4 PR1 (docs/plans/home-workspace-volume.md): this is wiring only —
// the returned directory is threaded into SandboxRuntimeInfo.WorkspaceHomeDir
// but BuildSandboxSpec does not read it yet (PR2 switches the sandbox HOME
// mount over to it). Behavior is otherwise unchanged.
//
// Contract (see the plan doc's 契約 section):
//   - the home directory always exists on return (nil error)
//   - init.sh runs at most once per distinct script content, per incarnation
//     of the home directory, AND per execution environment: a completion
//     marker keyed by the script's sha256 short-circuits every later call with
//     the same content, but only while the home still carries the identity
//     that marker records and only while the marker records the generation of
//     the environment this build runs inits in (see workspaceHomeInitialized,
//     workspaceHomeMarker.HomeID and workspaceHomeInitGeneration)
//   - concurrent calls for the same slug serialize on a flock so the script
//     runs exactly once; waiters block until the winner finishes and then
//     re-check the marker. As of PR5 the flock is only HALF of that: it dies
//     with the process, and the init container does not, so the deterministic
//     container name is what keeps a crashed daemon's in-flight init from
//     being raced by its successor (dockerres.WorkspaceInitContainerName)
//   - a failing init script returns an error and leaves no marker, so the
//     next call retries from scratch
//   - every successful return has stamped the home with an identity, whether
//     or not an init.sh existed to run
//
// Where it runs changed in PR5 (論点c): the daemon no longer exec's
// `/bin/bash <tmpfile>` itself. Both the builtin prep steps and the
// workspace's own init.sh run inside a throwaway container that mounts the
// home, because from PR6 the home is a named volume the daemon can neither
// write to nor chdir into. The trusted boundary is unchanged — boid picks the
// image, assembles the script and starts the container; nothing about this
// runs inside a sandboxed job (論点c's A vs A') — but several script-visible
// details did change, and they are recorded in docs/plans/home-workspace-volume.md
// 「契約」and docs/{ja,en}/guide/workspace-home.md rather than only here.
//
// The home directory and the init bookkeeping (marker, lock) live under two
// different roots as of PR2 of
// docs/plans/workspace-home-volume-persistence.md (論点b): homes/ stays
// derived from RuntimesDir, while homes-meta/ moves to the daemon's own
// persistent data root. See workspaceHomeMetaDir for why. PR5 removed the
// third inhabitant of that directory, the private temp copy of init.sh: the
// bytes now travel to the container on its stdin, since a file the daemon
// writes is not a path the sibling engine can mount.
//
// Splitting those roots is also what makes the identity check above
// load-bearing rather than decorative: it gave the marker a strictly longer
// lifetime than the thing it describes. The nonce is the counterweight, and
// it belongs in the same PR as the split — see workspaceHomeMarker.HomeID.
//
// A marker left behind in the OLD location by a pre-PR2 daemon is neither
// read nor deleted: not read, because that root is tmpfs under the container
// backend and honouring a stale record of a run whose home may well have
// been wiped is the exact failure mode the split is meant to end; not
// deleted, because destroying data the daemon has stopped relying on buys
// nothing and PR6/PR7 revisit the homes/ layout wholesale anyway. The
// observable consequence on an upgraded installation is one extra init.sh
// run per workspace, absorbed by the contractual idempotence of init scripts
// (docs/plans/home-workspace-volume.md 「script 作者が守ること」).
func (r *Runner) resolveWorkspaceHome(ctx context.Context, workspaceID string) (string, string, error) {
	slug, err := normalizeWorkspaceSlug(workspaceID)
	if err != nil {
		return "", "", err
	}

	homesDir, err := r.workspaceHomesDir()
	if err != nil {
		return "", "", fmt.Errorf("workspace home: %w", err)
	}
	if err := os.MkdirAll(homesDir, 0o700); err != nil {
		return "", "", fmt.Errorf("workspace home: create homes dir %q: %w", homesDir, err)
	}

	metaDir, err := r.workspaceHomeMetaDir()
	if err != nil {
		return "", "", fmt.Errorf("workspace home: %w", err)
	}
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		return "", "", fmt.Errorf("workspace home: create homes meta dir %q: %w", metaDir, err)
	}

	homeDir := workspaceHomeDirFor(homesDir, slug)
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		return "", "", fmt.Errorf("workspace home %q: create home dir %q: %w", slug, homeDir, err)
	}

	scriptPath, err := workspaceInitScriptPath(slug)
	if err != nil {
		return "", "", fmt.Errorf("workspace home %q: %w", slug, err)
	}
	scriptBytes, scriptExists, err := readIfExists(scriptPath)
	if err != nil {
		return "", "", fmt.Errorf("workspace home %q: read init script %q: %w", slug, scriptPath, err)
	}
	scriptSHA := scriptSHA256Hex(scriptBytes, scriptExists)

	markerPath := workspaceHomeMarkerPath(metaDir, slug)

	// Fast path: already initialized for this exact script content AND the
	// home the marker describes is still the one on disk, no lock needed.
	if marker, ok, err := readWorkspaceHomeMarker(markerPath); err != nil {
		return "", "", fmt.Errorf("workspace home %q: read marker %q: %w", slug, markerPath, err)
	} else if ok && workspaceHomeInitialized(marker, scriptSHA, homeDir, slug) {
		return homeDir, slug, nil
	}

	lockPath := workspaceHomeLockPath(metaDir, slug)
	release, err := acquireWorkspaceHomeLock(lockPath)
	if err != nil {
		return "", "", fmt.Errorf("workspace home %q: acquire init lock: %w", slug, err)
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
		return "", "", fmt.Errorf("workspace home %q: read init script %q: %w", slug, scriptPath, err)
	}
	scriptSHA = scriptSHA256Hex(scriptBytes, scriptExists)

	// Double-checked: another dispatch may have finished init while we were
	// waiting on the lock. Same identity check as the fast path — a marker
	// alone still does not prove the home behind it survived.
	if marker, ok, err := readWorkspaceHomeMarker(markerPath); err != nil {
		return "", "", fmt.Errorf("workspace home %q: read marker %q: %w", slug, markerPath, err)
	} else if ok && workspaceHomeInitialized(marker, scriptSHA, homeDir, slug) {
		return homeDir, slug, nil
	}

	// One throwaway container per init, ALWAYS — not only when the workspace
	// declares an init.sh (PR5 §D1, 論点b-2). It carries three things in one
	// start: the bind-target skeleton, the workspace's own script if it has
	// one, and the home identity stamp. The first and third are needed by
	// every workspace, so making the run conditional the way the old host exec
	// was would leave the pass-through class (docs/plans/home-workspace-volume.md
	// 「script が無い workspace は素通し (マーカーだけ打つ)」) with no writer for
	// either — and the identity check below would then be the one thing that
	// does not apply to exactly the workspaces most likely to be hit by it.
	//
	// The identity is minted here and stamped into the home by the container's
	// postlude, BEFORE the marker that vouches for it is written here.
	// Ordering matters in one direction only: if the stamp lands and the marker
	// write then fails, the next call finds no (or a stale) marker and re-inits
	// — correct. The reverse order would let a marker exist that claims an
	// identity the home does not carry, which is indistinguishable from the
	// tampering case and would also re-init, but only after having reported
	// success for a home that was never stamped.
	nonce, err := newWorkspaceHomeNonce()
	if err != nil {
		return "", "", fmt.Errorf("workspace home %q: mint home id: %w", slug, err)
	}
	req, err := buildWorkspaceInitRequest(workspaceInitParams{
		Slug:       slug,
		HomeSource: homeDir,
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
		SkeletonDirs: workspaceHomeSkeletonDirs(),
		Script:       scriptBytes,
		ScriptExists: scriptExists,
		Nonce:        nonce,
	})
	if err != nil {
		return "", "", err
	}
	executor, err := workspaceInitExecutorFor(r)
	if err != nil {
		return "", "", fmt.Errorf("workspace home %q: %w", slug, err)
	}
	if err := executor.RunWorkspaceInit(ctx, req); err != nil {
		return "", "", fmt.Errorf("workspace home %q: init failed: %w", slug, err)
	}

	marker := workspaceHomeMarker{
		ScriptSHA256: scriptSHA,
		HomeID:       nonce,
		// Recorded from the constant, never from the marker being replaced:
		// this marker vouches for the run that just happened, in the
		// environment this build runs inits in.
		InitGeneration: workspaceHomeInitGeneration,
		BoidVersion:    boidVersion,
		CompletedAt:    time.Now().UTC(),
	}
	if err := writeWorkspaceHomeMarker(markerPath, marker); err != nil {
		return "", "", fmt.Errorf("workspace home %q: write marker: %w", slug, err)
	}

	return homeDir, slug, nil
}

// workspaceHomeInitialized reports whether marker may be trusted to
// short-circuit init for homeDir: the script content it records must match
// the current one, AND the home must still carry the identity the marker
// vouches for (docs/plans/workspace-home-volume-persistence.md 論点b).
//
// Every "no" answer means one extra run of a contractually idempotent
// init.sh, which is why every uncertain case answers no:
//
//   - marker.InitGeneration is not the current one — the run it records
//     happened in a different execution environment, so the home it produced
//     is not the home this build would produce. Zero (the field's absence)
//     means a PR2-PR4 daemon that ran init on the host; see
//     workspaceHomeInitGeneration. The comparison is deliberately an equality
//     and not a floor, so a rolled-back release re-inits too.
//   - marker.HomeID empty — written before the identity check existed, so it
//     vouches for nothing.
//   - nonce file missing — the home was wiped (host reboot clearing tmpfs,
//     `docker volume rm`, a reap misfire) and re-created empty by
//     resolveWorkspaceHome's own MkdirAll moments ago, or a job deleted it.
//   - nonce file rejected by readWorkspaceHomeNonce — a symlink, a FIFO, a
//     directory, something oversized, or simply unreadable (permissions).
//     None of those is evidence of a good home, and the first four are what
//     a tampering job leaves behind. Logged, since unlike the others it is
//     not an expected steady-state transition.
//   - nonce content different — a home restored from a different incarnation
//     (an old backup, another workspace's contents), or a job rewrote it.
//
// A job can push every one of these buttons on its own home, and the price
// list above is the whole consequence: one extra init.sh run. What it cannot
// do — since PR2's codex review — is make the read itself hang or exhaust
// the daemon; see readWorkspaceHomeNonce.
func workspaceHomeInitialized(marker workspaceHomeMarker, scriptSHA, homeDir, slug string) bool {
	if marker.ScriptSHA256 != scriptSHA {
		return false
	}
	if marker.InitGeneration != workspaceHomeInitGeneration {
		slog.Info("workspace home: completion marker records a different init execution environment, re-running init (docs/plans/workspace-home-volume-persistence.md 論点c)",
			"workspace_slug", slug, "home_dir", homeDir,
			"marker_generation", marker.InitGeneration, "current_generation", workspaceHomeInitGeneration)
		return false
	}
	if marker.HomeID == "" {
		slog.Info("workspace home: completion marker predates the home identity check, re-running init",
			"workspace_slug", slug, "home_dir", homeDir)
		return false
	}
	nonce, exists, err := readWorkspaceHomeNonce(homeDir)
	if err != nil {
		slog.Warn("workspace home: could not read the home identity file, re-running init",
			"workspace_slug", slug, "home_dir", homeDir, "error", err)
		return false
	}
	if !exists {
		slog.Warn("workspace home: completion marker exists but the home carries no identity file; the home was emptied or replaced since it was initialized, re-running init (docs/plans/workspace-home-volume-persistence.md 論点b)",
			"workspace_slug", slug, "home_dir", homeDir)
		return false
	}
	if nonce != marker.HomeID {
		slog.Warn("workspace home: home identity does not match the completion marker; the home was replaced since it was initialized, re-running init (docs/plans/workspace-home-volume-persistence.md 論点b)",
			"workspace_slug", slug, "home_dir", homeDir)
		return false
	}
	return true
}

// newWorkspaceHomeNonce mints a workspace home's identity token: 32 bytes of
// crypto/rand, hex-encoded. crypto/rand rather than anything derived from the
// slug, the clock or the path, because the one thing this token must not be
// is predictable from inside the sandbox — see workspaceHomeNonceFileName's
// own doc comment for why that is the property the whole check rests on.
func newWorkspaceHomeNonce() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// workspaceHomeNoncePath returns homeDir/.boid-workspace-home-id.
func workspaceHomeNoncePath(homeDir string) string {
	return filepath.Join(homeDir, workspaceHomeNonceFileName)
}

// maxWorkspaceHomeNonceFileSize caps how many bytes of the identity file are
// ever read. A nonce is 32 bytes hex-encoded = 64 characters, so 1 KiB is
// three orders of magnitude of slack for hand-editing/trailing whitespace
// while still being a hard ceiling on what a job can make the daemon
// allocate.
const maxWorkspaceHomeNonceFileSize = 1024

// readWorkspaceHomeNonce reads the identity file inside homeDir, returning
// ("", false, nil) when it does not exist. Surrounding whitespace is trimmed
// so that a hand-inspected/hand-restored file with a trailing newline still
// compares equal.
//
// This is the one read in the whole mechanism whose target a sandboxed job
// controls (the nonce lives inside the rw-mounted workspace home; the marker
// and the init script do not — see workspaceHomeNonceFileName and
// workspaceInitScriptPath). It therefore cannot be a bare os.ReadFile, which
// is what PR2 shipped and what the codex review flagged: that call follows
// symlinks, opens anything the name resolves to, and grows its buffer to
// whatever the descriptor yields. A job that exits leaving
//
//   - a FIFO at this path wedges the DAEMON: open(2) blocks until some
//     process opens the other end, which after the job's container is gone
//     never happens, so every later dispatch — including other tasks' —
//     hangs before it can even decide to re-init;
//   - a symlink to /dev/zero (or any endless source) makes the daemon read
//     until it is OOM-killed.
//
// Both reach beyond the offending job, which is exactly what the documented
// "worst case is a redundant re-init" contract rules out. So:
//
//   - O_NOFOLLOW — the final component is never resolved through a symlink.
//     Only that component is attacker-controlled: everything above it is the
//     mount point itself and its parents, which a job cannot rename from
//     inside its own $HOME. That is why this stops at one flag instead of
//     re-walking every component through openat2/RESOLVE_NO_SYMLINKS the way
//     internal/skills/safe_deploy.go has to (it writes into subdirectories a
//     job CAN swap). Cheaper, and with no risk of rejecting an installation
//     whose ~/.local/share is a symlink for legitimate reasons.
//   - O_NONBLOCK — makes the open of a FIFO (or a device) return immediately
//     instead of waiting for a peer, so the shape check below can run at all.
//     No effect on a regular file.
//   - fstat + IsRegular — the actual shape check; catches the FIFO, a
//     directory, a socket, a device node.
//   - io.LimitReader — bounds the allocation regardless of what the
//     descriptor claims its size is.
//
// Every rejection is returned as an error rather than swallowed, and
// workspaceHomeInitialized turns any error into a re-init (fail-safe) plus a
// slog.Warn — none of these shapes is a normal steady-state transition, so a
// silent skip would hide a tampered or broken home.
func readWorkspaceHomeNonce(homeDir string) (string, bool, error) {
	path := workspaceHomeNoncePath(homeDir)

	// os.OpenFile adds O_CLOEXEC itself.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		if errors.Is(err, syscall.ELOOP) {
			return "", false, fmt.Errorf("home id file %q is a symlink; refusing to follow it: %w", path, err)
		}
		return "", false, fmt.Errorf("open home id file %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return "", false, fmt.Errorf("stat home id file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("home id file %q is not a regular file (mode %s); refusing to read it", path, info.Mode())
	}

	data, err := io.ReadAll(io.LimitReader(f, maxWorkspaceHomeNonceFileSize+1))
	if err != nil {
		return "", false, fmt.Errorf("read home id file %q: %w", path, err)
	}
	if len(data) > maxWorkspaceHomeNonceFileSize {
		return "", false, fmt.Errorf("home id file %q is larger than the %d byte limit; refusing to read it", path, maxWorkspaceHomeNonceFileSize)
	}
	return strings.TrimSpace(string(data)), true, nil
}

// boidVersion is embedded into every completion marker's boid_version field.
// No build-time version stamping (ldflags -X) exists in this repo yet, so
// this stays empty for now — a later PR can wire it up without touching the
// marker format (see the plan doc's BoidVersion note).
const boidVersion = ""

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

// WorkspaceHomesDir returns ~/.local/share/boid/homes, the parent directory
// under which every workspace's home lives (docs/plans/home-workspace-volume.md
// 「レイアウト」). Unlike runtimes/, this directory is never GC'd — workspace
// homes are persistent (PR5 wires deletion to `workspace remove`).
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
// server/wire.go now wires Runner.RuntimesDir from hostVisibleRuntimesDirFor
// (cfg) unconditionally, which resolves under cfg.SocketPath's dir =
// BOID_RUNTIME_DIR, typically tmpfs — see that function's own doc comment,
// and docs/plans/workspace-home-volume-persistence.md for the persistence
// regression that follows from it (PR6 replaces this path-based home with a
// per-workspace named volume). The daemon's own persistent root is reached
// via Runner.DataHomeDir instead; see workspaceHomeMetaDir.
//
// Exported (Phase 4 PR5, docs/plans/home-workspace-volume.md) as a pure
// free function — independent of any *Runner state — so internal/api's
// handlers (GET /api/workspaces/{slug} size reporting, POST /api/gc's
// workspace_homes listing, DELETE /api/workspaces/{slug}'s home dir
// deletion) can resolve the exact same homes/ directory the dispatcher
// itself uses, from the same runtimes root server/wire.go already threads
// through those handlers, without needing a live *Runner.
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

// workspaceHomesDir is a thin *Runner-bound wrapper around WorkspaceHomesDir.
func (r *Runner) workspaceHomesDir() (string, error) {
	return WorkspaceHomesDir(r.RuntimesDir)
}

// workspaceHomeDirFor decides the on-disk NAME a workspace's home is stored
// under inside homesDir. Today that name simply IS the slug, so
// filepath.Base(<home dir>) == <slug> holds by construction — a coincidence
// PR6 of docs/plans/workspace-home-volume-persistence.md ends, when the home
// becomes a per-workspace named volume called
// boid-ws-home-<installID8>-<sanitized-slug> (論点a).
//
// Naming that one line is the point: everything downstream that needs the
// slug must take it from resolveWorkspaceHome's own return value rather than
// re-deriving it from the path (PR4). Keeping the derivation behind a single
// indirection is what lets a test construct the post-PR6 shape — a home
// directory whose basename is NOT the slug — and so makes the "threaded, not
// re-derived" property observable BEFORE PR6 makes it load-bearing in
// production. Without that, every such test would be tautological: the two
// values are equal today, so asserting one against the other proves nothing.
//
// Swappable-var rather than a plain function for exactly that reason,
// mirroring internal/api/workspace_homes.go's apparentSizeFn and this
// package's daemonUID / bindTargetOwnerUID. Production never reassigns it.
//
// Note that internal/api/workspace_homes.go's resolveWorkspaceHomePath still
// open-codes the same filepath.Join for its size/GC reporting. That is
// deliberate for now — it is a different package with no *Runner in hand, and
// PR7 rewires the whole of it onto the volume API rather than onto this
// helper.
var workspaceHomeDirFor = func(homesDir, slug string) string {
	return filepath.Join(homesDir, slug)
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
// config). The workspace home's nonce file was the third and does not
// qualify — it moved to readWorkspaceHomeNonce, which explains what a bare
// os.ReadFile hands an attacker who controls the target. Adding a caller
// here means first checking that its path is on the daemon's side of that
// line. Both of the current ones used to carry a residual exposure — a job
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
func writeWorkspaceHomeMarker(path string, marker workspaceHomeMarker) (retErr error) {
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal marker: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".init.json.*.tmp")
	if err != nil {
		return fmt.Errorf("create temp marker file: %w", err)
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
		return fmt.Errorf("write temp marker file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp marker file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp marker file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp marker file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename marker file: %w", err)
	}
	cleanup = false
	// Best-effort parent dir fsync so the rename survives a crash right
	// after this call returns; not fatal if unsupported.
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
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
