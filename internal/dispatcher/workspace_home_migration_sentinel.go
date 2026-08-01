package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/novshi-tech/boid/internal/dockerres"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// workspace_home_migration_sentinel.go makes a workspace home migration
// recoverable when the daemon process stops existing partway through it
// (codex review of PR8 round 2, Major 2).
//
// # The invariant, in one line
//
// WHILE A RECORD EXISTS FOR A SLUG, NOTHING RUNS AGAINST THAT WORKSPACE'S HOME —
// not init.sh, not a job. Two places enforce it, and only the second one is a
// guarantee: the startup sweep (SweepInterruptedWorkspaceHomeMigrations) resolves
// records eagerly, and the dispatch path
// (Runner.recoverInterruptedWorkspaceHomeMigration, round 3 Blocker 1) refuses to
// proceed past a record it could not resolve. Round 3's finding was that with only
// the first, a boot where the engine happened to be unreachable let the record
// survive AND let the dispatch through — a single ordinary boot, no second crash
// required.
//
// "Resolve" is not "destroy": what resolving a record COSTS depends on the phase
// it carries, and round 4 is where that distinction was added. A record whose
// phase says the home was never destroyed is resolved by deleting the record and
// nothing else. See "The record has a PHASE" below.
//
// # What the round-1 cleanup does not cover
//
// discardPartialWorkspaceHome runs when Runner.ImportWorkspaceHome RETURNS an
// error. SIGKILL, an OOM kill, `docker compose down -t 0`, a power cut — none of
// those produce a return value, so neither the defers nor the cleanup run. The
// startup orphan sweep does not cover it either: it reaps CONTAINERS by label
// (reapOrphanWorkspaceInitContainers), and the container is not what survives —
// the VOLUME is, holding whatever crossed before the process died, with its
// completion marker already deleted.
//
// That state is worse than either neighbour, for the reason
// discardPartialWorkspaceHome spells out at length: init.sh DOES run against it,
// and an init.sh that probes for what it installs (`command -v claude`,
// `[ -x "$HOME/.local/bin/claude" ]` — the contractually idempotent shape,
// docs/plans/home-workspace-volume.md 「script 作者が守ること」) answers "already
// installed" to a half-extracted home. It exits 0, resolveWorkspaceHome writes a
// completion marker for it, and the breakage is frozen: nothing re-runs, and it
// surfaces much later as the adapter's "CLI not found in workspace $HOME". It
// also contradicts what docs/{ja,en}/guide/workspace-home.md promises an
// operator about a failed migration.
//
// # Why a file rather than a label on the volume
//
// The obvious design is to mark the VOLUME "migration in progress" and clear the
// mark on success, so the record and the thing it describes share one lifetime
// and cannot disagree. The Engine API cannot express it: there is no way to add,
// change or remove a label on an EXISTING volume. VolumeCreate against a name
// that exists returns that volume untouched — which is not an oversight but the
// property Runner.ensureWorkspaceHomeVolume's fast path is built on, and the
// same reason an unlabelled volume is a hard error there rather than something a
// re-init can repair. (Encoding the state in the volume's NAME instead would
// mean the migration wrote into a volume no dispatch mounts.)
//
// So the record goes where the daemon's other bookkeeping about workspace homes
// already lives: <dataHome>/homes-meta/, beside the completion marker and the
// init flock, inside the daemon's own persistent volume (see
// workspaceHomeMetaDir for why that root, and what it is and is not reachable
// from).
//
// # The record has a PHASE, because "a record exists" is not "a home was
// destroyed" (codex review of PR8 round 4, Blocker 1)
//
// Rounds 2 and 3 gave the record one meaning — "a migration was started" — and
// let recovery read it as "this home is wreckage". Those are not the same
// statement, and the gap between them is not hypothetical: a record survives
// whenever its own unlink fails, and the two ways that happens both leave a home
// that is perfectly fine.
//
//   - Route A. The extraction COMPLETES, and the unlink at the end fails
//     (unwritable homes-meta, a read-only remount, EIO). The API answers 200. The
//     next dispatch then destroys a home holding the harness authentication and
//     the toolchain the operator just spent an afternoon migrating.
//   - Route B. The migration is REFUSED at step 1 by the engine's 409 for a
//     volume a running job holds — the one refusal that is guaranteed to have
//     destroyed nothing — and the cleanup unlink that follows fails, its error
//     logged and dropped. The dispatch that arrives after that job finishes
//     destroys a home the migration never touched.
//
// So the record carries workspaceHomeMigrationRecord.Phase, and recovery acts on
// the phase rather than on the file's existence:
//
//	phase                 written when                     what recovery does
//	--------------------  -------------------------------  --------------------------
//	recorded              before ANYTHING is destroyed     delete the record, and
//	                                                       nothing else
//	home-destroyed        VolumeRemove has SUCCEEDED       discard the volume, the
//	                                                       marker and the record
//	home-absent           the volume is confirmed GONE     delete the record, and
//	                      (round 5)                        nothing else
//	home-rebuilt          the extraction has finished      delete the record, and
//	                                                       nothing else
//	(absent / unknown)    a build that predates the field  discard — see below
//
// # The ordering, and why every crash point lands on the safe side
//
//	write "recorded"  →  remove volume  →  write "home-destroyed"  →  remove marker
//	                  →  create volume  →  extract  →  write "home-rebuilt"  →  remove record
//	                     ^------ a kill in here is recovered by the sweep ------^
//
// Every path that leaves this line early commits "home-absent" in place of
// "home-rebuilt" and then removes the record the same way — see "the unlink is
// not a durability primitive" below.
//
// The property round 2 established is preserved, stated against the interval it
// was always really about: THE PHASE THAT AUTHORIZES A DISCARD IS ON DISK
// STRICTLY BEFORE, AND STRICTLY AFTER, THE INTERVAL IN WHICH A VOLUME EXISTS
// UNDER THE HOME'S NAME HOLDING CONTENTS NOBODY VOUCHED FOR. That interval opens
// when the replacement volume is created (step 3) and closes when the extraction
// returns; "home-destroyed" is committed before step 3 and replaced only after
// step 4. Every kill lands somewhere covered:
//
//   - Killed before the record exists: nothing destroyed, nothing to recover.
//   - Killed while the phase is "recorded": either the removal has not run or it
//     has not answered. If it never ran, the home is intact and recovery leaves
//     it alone — which is the whole point of the phase. If it ran and the answer
//     was lost, the volume is GONE, and "gone" is the never-dispatched-into state
//     a fresh dispatch handles on its own (new volume, new identity, stale
//     marker, init re-runs).
//   - Killed while the phase is "home-destroyed": the volume is absent, empty or
//     partial. Recovery removes it; the next dispatch builds from empty. This is
//     the case the whole file exists for.
//   - Killed while the phase is "home-absent": the volume is gone and something
//     already confirmed it, so recovery removes the record and nothing else. The
//     phase exists for the failure paths rather than for a kill, but a kill in
//     the instant between committing it and unlinking the record lands here too.
//   - Killed while the phase is "home-rebuilt": a complete, correct home, and
//     recovery keeps it. Route A above is this case with the unlink failing
//     rather than a kill.
//
// The residual over-reach window is now the gap between the extraction returning
// and the "home-rebuilt" rename committing — a single small write against an
// operation that runs for minutes — and its cost is still only one re-run.
//
// # The unlink is not a durability primitive (codex review of PR8 round 5,
// Blocker)
//
// Round 4 applied the argument above to the SUCCESS path and left the failure
// paths deleting a record that still said "home-destroyed". That is unsafe for a
// reason that has nothing to do with crashing mid-migration:
// removeWorkspaceHomeMigrationRecord unlinks FIRST and fsyncs the directory
// afterwards, so its failure mode is the awkward one — the record is gone as far
// as this process and every dispatch after it can see, while nothing guarantees
// it stays gone.
//
//	the volume is confirmed gone; record still says "home-destroyed"
//	unlink succeeds, its directory fsync returns EIO   -> logged or (in the sweep) dropped
//	the next dispatch sees no record                   -> builds a CORRECT home from init.sh
//	power cut rolls the unlink back                    -> the DURABLE "home-destroyed" is back
//	the next start's recovery obeys the phase          -> that correct home is destroyed
//
// So "home-absent" is committed before every one of those unlinks, and whichever
// of the two writes a crash keeps, neither authorizes destroying anything. The
// three places are Runner.ImportWorkspaceHome's windowErr path and its step-4b
// cleanup (both via clearWorkspaceHomeMigrationRecordForAnAbsentHome) and
// discardInterruptedWorkspaceHomeMigrationLocked's own discard below.
//
// When that write itself fails the record is KEPT rather than unlinked, which is
// the safe side of the same asymmetry: a record that is VISIBLY there stops
// every dispatch of this workspace at recoverInterruptedWorkspaceHomeMigration
// and is resolved before anything creates a volume — free, against a volume that
// is not there — while a record unlinked but not flushed is invisible for exactly
// as long as it takes a dispatch to build a good home.
//
// # Why an UNKNOWN phase still resolves to "discard"
//
// A record with no phase, or one this build does not recognize, was written by
// some other build — and every build that could have written one meant "a
// migration was started", which through round 3 authorized a discard. Reading it
// as "destroyed" is therefore both backward-compatible and the fail-safe
// direction, for the reason below. Nothing this build writes lands in that
// bucket: all three phases are set explicitly.
//
// # Why over-reach is the safe direction when the phase IS uncertain
//
// Deleting a home the migration had not yet touched sounds worse than leaving
// it. When the record genuinely cannot say, it is not, and the asymmetry is what
// decides the fallback:
//
//   - The operator explicitly asked for that home to be replaced. Destroying it
//     is what the command does; doing it during recovery is not a new outcome,
//     only an earlier one.
//   - Nothing is lost. `boid workspace import-home` only READS its source and
//     never deletes it (that is why the CLI refuses to), so re-running the
//     migration restores exactly the state the operator asked for — and
//     "re-run it" is already the documented recovery for every failed migration.
//   - Under-reaching has no such escape hatch: a half-extracted home that the
//     sweep decides to keep gets a completion marker written over it by the next
//     dispatch, and from that point nothing in the system ever looks at it again.
//
// So an uncertain record resolves to "delete and re-initialize", the same
// fail-safe direction workspaceHomeInitialized takes for an uncertain marker.
// What round 4 changes is that a record whose phase is KNOWN is no longer
// uncertain, and is not treated as though it were.

// workspaceHomeMigrationRecord is the on-disk "a migration of this workspace's
// HOME volume was started and has not been finished or cleaned up" sentinel, at
// <dataHome>/homes-meta/<slug>.migrating.json.
//
// It is deliberately tiny. Everything the sweep needs is the volume to remove,
// and everything else here is for a human reading the file after an incident.
type workspaceHomeMigrationRecord struct {
	// Slug is the normalized workspace slug. Recorded for a reader's benefit;
	// the sweep takes the slug from the FILE NAME, which is the value the marker
	// and lock paths are derived from and therefore the one that has to agree
	// with them.
	Slug string `json:"slug"`

	// Volume is the home volume this migration was replacing,
	// dockerres.WorkspaceHomeVolumeName(installID, slug) as it was computed at
	// the time.
	//
	// Recorded rather than recomputed by the sweep because the install id is an
	// input to that name, and a boot that cannot read install_id (or a restore
	// that changed it) would otherwise recompute a DIFFERENT name and leave the
	// real wreckage untouched while removing something else. The sweep falls
	// back to recomputing only when this is empty or unreadable.
	Volume string `json:"volume"`

	// Phase says whether this record authorizes recovery to DESTROY the volume
	// it names (codex review of PR8 round 4, Blocker 1). See this file's header
	// for the table, the ordering and the fallback for an unknown value; see
	// workspaceHomeMigrationRecord.discardsHome for the predicate itself.
	//
	// omitempty is deliberate even though every record this build writes sets it:
	// it keeps the field out of the file for the zero value, which is the value
	// the fallback is written for, so a record that somehow lacks a phase looks
	// on disk exactly like the pre-round-4 records the fallback also has to
	// cover.
	Phase workspaceHomeMigrationPhase `json:"phase,omitempty"`

	// StartedAt is when the record was FIRST written — i.e. the last moment
	// before anything was destroyed. Diagnostic only, and deliberately not
	// refreshed by the phase updates: an operator reading this file after an
	// incident wants to know when the migration began, not when it last changed
	// its mind.
	StartedAt time.Time `json:"started_at"`

	// BoidVersion mirrors workspaceHomeMarker.BoidVersion: internal/version.
	// Version() of the daemon that wrote this record — an image-build ldflags
	// override, a go-install module version, or "" for a build with neither
	// (docs/plans/release-onboarding.md 穴2).
	BoidVersion string `json:"boid_version,omitempty"`
}

// workspaceHomeMigrationPhase is how far through the destructive part of a
// migration the daemon had got when it last wrote the record.
//
// A string rather than an int so the file stays readable by the operator who
// opens it after an incident — this record's secondary job, stated in
// workspaceHomeMigrationRecord's own doc comment — and so an unrecognized value
// from a future build is visibly a name rather than an off-by-one.
type workspaceHomeMigrationPhase string

const (
	// workspaceHomeMigrationRecorded: the migration is under way and NOTHING has
	// been destroyed yet.
	//
	// This is the phase a record is created with, before the volume removal that
	// is also the in-use check. A record found in this phase means the removal
	// either never ran or never answered — in the first case the home is intact,
	// in the second it is gone entirely — and neither is a state recovery can
	// improve by deleting a volume. Recovery therefore removes the record and
	// touches nothing else.
	workspaceHomeMigrationRecorded workspaceHomeMigrationPhase = "recorded"

	// workspaceHomeMigrationHomeDestroyed: VolumeRemove SUCCEEDED, so the home
	// under this name is no longer a home, and whatever appears under it next was
	// put there by a migration that may not have finished.
	//
	// This is the only phase that authorizes a discard. It is committed before
	// the replacement volume is created and replaced only after the extraction
	// returns, which is what makes the recorded destructive interval strictly
	// wider than the interval in which the volume holds contents nobody vouched
	// for.
	workspaceHomeMigrationHomeDestroyed workspaceHomeMigrationPhase = "home-destroyed"

	// workspaceHomeMigrationHomeAbsent: the home volume was destroyed AND boid
	// has since confirmed that nothing exists under its name — either the
	// replacement was never created, or the half-extracted one was successfully
	// removed. Nothing is left for recovery to discard.
	//
	// Written on the two failure paths that end with the volume gone, and written
	// BEFORE the record's own unlink, for the reason this file's header gives
	// under "the unlink is not a durability primitive" (codex round 5, Blocker).
	//
	// # Why not reuse one of the other two
	//
	// The ACTION recovery takes is the same as for the other two non-destructive
	// phases — delete the record, touch nothing — so the case for a third name is
	// about what the record SAYS, which is this file's other job (see
	// workspaceHomeMigrationRecord's doc comment: an operator reads this file
	// after an incident). "recorded" states that nothing was destroyed and would
	// be a lie here; "home-rebuilt" states that a complete home exists under this
	// name and would be a worse one, since the operator's next dispatch is about
	// to build that home from scratch and their old contents are gone for good.
	//
	// Recovery does not have to distinguish "the volume is absent" from "the
	// volume holds a home somebody vouched for" TODAY, and this phase does not
	// invent a branch for it: discardsHome answers false for both. What the split
	// buys is that neither phase asserts the other's fact, so a later change that
	// does care — anything that decides whether the home still needs building —
	// finds the distinction already recorded rather than having to re-derive it
	// from a record that flattened the two.
	//
	// A build that predates this constant reads it as an unknown phase and falls
	// back to "discard", which is safe here for the same reason this phase is
	// written at all: the volume it names does not exist, and both recovery paths
	// resolve the record before anything creates one (see
	// resolveWorkspaceHome's call to recoverInterruptedWorkspaceHomeMigration,
	// which runs above ensureWorkspaceHomeVolume). The discard is then a
	// VolumeRemove that reports "not found" as success.
	workspaceHomeMigrationHomeAbsent workspaceHomeMigrationPhase = "home-absent"

	// workspaceHomeMigrationHomeRebuilt: the extraction finished, so the home is
	// a home again and this record no longer describes anything destructive.
	//
	// Written immediately before the record is removed, and the reason route A of
	// this file's header is not fatal: a record whose unlink fails is left in a
	// phase that costs the next daemon start one unlink rather than this
	// workspace's entire home.
	workspaceHomeMigrationHomeRebuilt workspaceHomeMigrationPhase = "home-rebuilt"
)

// discardsHome reports whether this record authorizes recovery to remove the
// home volume it names.
//
// The default arm is the contract, not a leftover: an absent phase (a record
// written by a build from before round 4) and an unrecognized one (a record
// written by a build from after this one) both mean "boid cannot tell", and an
// uncertain record resolves the same fail-safe way an uncertain completion
// marker does — see this file's header for why over-reach is the safe direction
// when, and only when, the record genuinely cannot say.
func (rec workspaceHomeMigrationRecord) discardsHome() bool {
	switch rec.Phase {
	case workspaceHomeMigrationRecorded, workspaceHomeMigrationHomeAbsent, workspaceHomeMigrationHomeRebuilt:
		return false
	default:
		return true
	}
}

// workspaceHomeMigrationRecordSuffix is the file-name suffix the sweep
// enumerates. A workspace slug is [a-z0-9-]{1,64}
// (orchestrator.ValidWorkspaceSlug) so it can contain no '.', which is what
// makes stripping this suffix an unambiguous inverse of building the name — and
// what keeps "<slug>.init.json" and "<slug>.migrating.json" from ever colliding.
const workspaceHomeMigrationRecordSuffix = ".migrating.json"

// workspaceHomeMigrationRecordPath returns metaDir/<slug>.migrating.json,
// pairing with workspaceHomeMarkerPath and workspaceHomeLockPath.
func workspaceHomeMigrationRecordPath(metaDir, slug string) string {
	return filepath.Join(metaDir, slug+workspaceHomeMigrationRecordSuffix)
}

// writeWorkspaceHomeMigrationRecord writes the sentinel durably, and FAILS
// rather than reporting success when it cannot (codex review of PR8 round 3,
// Major).
//
// It reuses the marker's temp-file + rename + parent-dir fsync writer rather
// than an os.WriteFile, and the fsync is the entire point: this record's job is
// to survive the machine going away, so a record that is only in the page cache
// when the power cuts records nothing. Sharing writeWorkspaceHomeMarker's
// implementation would mean sharing its struct, so the durability discipline is
// factored instead (see writeWorkspaceHomeJSONFile).
//
// homesMetaRequiredDurability is where sharing that implementation stops. The
// marker tolerates a failed directory fsync — losing it costs one re-run of an
// idempotent init — and this file cannot, because the caller's very next
// statement destroys the home volume. Round 3's finding was precisely that the
// shared writer swallowed the fsync error, so a daemon on failing storage would
// report a durable sentinel it did not have and then destroy the home behind it.
//
// # Every PHASE transition goes through here too (round 4, Blocker 1)
//
// The record is rewritten twice more after it is created — once when the volume
// removal has succeeded and once when the extraction has finished — and both go
// through this same writer at the same durability, because each of those writes
// is the thing the NEXT one's correctness rests on. A "home-destroyed" that is
// only in the page cache when the power cuts leaves a partial home behind a
// record that says nothing was destroyed; a "home-rebuilt" that is only in the
// page cache leaves a finished home behind a record that says it is wreckage.
// The rename is what makes each transition atomic, so a reader never sees a
// half-written phase — only the old one or the new one.
func writeWorkspaceHomeMigrationRecord(path string, rec workspaceHomeMigrationRecord) error {
	return writeWorkspaceHomeJSONFile(path, rec, homesMetaRequiredDurability)
}

// markWorkspaceHomeMigrationRecordHomeAbsent commits the "there is no volume
// under this name any more" phase durably (codex review of PR8 round 5,
// Blocker).
//
// Every path that ends with the home volume gone calls this BEFORE it unlinks
// the record — Runner.ImportWorkspaceHome's two failure paths through
// clearWorkspaceHomeMigrationRecordForAnAbsentHome, and recovery's own discard —
// for the reason removeWorkspaceHomeMigrationRecord's doc comment gives about
// which way an unlink fails. The write and the unlink land in the same directory
// and need the same fsync, so this usually fails when the unlink is about to;
// that is not wasted work, because the caller's answer to a failure here is to
// KEEP the record rather than unlink it, and a kept record is the safe kind.
//
// slug and volumeName fill in a record that could not be parsed (recovery reads
// a record it may find corrupt, and still has to leave a readable one behind for
// the operator). A parsed record's own values win: they were written when the
// migration knew them, and the install id that computes volumeName may have
// changed since.
func markWorkspaceHomeMigrationRecordHomeAbsent(recordPath, slug, volumeName string, rec workspaceHomeMigrationRecord) error {
	rec.Phase = workspaceHomeMigrationHomeAbsent
	if rec.Slug == "" {
		rec.Slug = slug
	}
	if rec.Volume == "" {
		rec.Volume = volumeName
	}
	return writeWorkspaceHomeMigrationRecord(recordPath, rec)
}

// removeWorkspaceHomeMigrationRecord deletes the sentinel durably, treating
// "already gone" as success.
//
// A failure is returned rather than swallowed so the caller can decide: for
// Runner.ImportWorkspaceHome a record that could not be removed means the next
// boot destroys a home that is actually fine, which is worth telling the
// operator about even though it is not worth failing a completed migration for.
//
// # Why the unlink is fsync'd too (round 3, Major)
//
// This deletion is the statement "the migration finished; do not touch this
// home". It is the SAME invariant the write side carries, pointing the other way,
// and it fails in the more expensive direction: an unlink that a power cut rolls
// back brings the record back, and a record that is back makes the next daemon
// start discard a home a migration had just filled correctly — several GB and an
// operator's afternoon, silently, for a workspace nothing was wrong with.
//
// A sync failure is reported rather than swallowed for the same reason the
// unlink's is: ImportWorkspaceHome surfaces it as
// WorkspaceHomeImportResult.MigrationRecordRemoveError, which is what tells an
// operator to delete the named file before restarting the daemon.
//
// The "already gone" branch returns before syncing anything: there is no
// deletion to make durable, and syncing then would only invent a failure mode
// for the idempotent path (the sweep and the two discard helpers all call this
// on records that may already be gone).
func removeWorkspaceHomeMigrationRecord(path string) error {
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := fsyncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf(
			"sync the directory after removing %s: %w (the record is gone now, but a crash could bring it back and the "+
				"next daemon start would then discard this workspace's freshly migrated home)", filepath.Base(path), err)
	}
	return nil
}

// workspaceHomeMigrationSweepTimeout bounds the ENGINE work the startup sweep
// does for ONE record (codex review of PR8 round 4, Major).
//
// # Why a bound at all
//
// buildRuntime calls the sweep on context.Background(), before auto-reopen and
// before the API listener, and the moby client has no timeout of its own: a
// request whose context never fires waits for the engine forever. An engine
// socket that accepts the connection and then answers nothing — a hung dockerd,
// a podman service that died mid-request, a socket forwarded to nowhere — parks
// daemon startup inside VolumeRemove with no jobs reopened, no CLI able to
// connect, and nothing in the log naming the workspace it is stuck on. Round 2
// gave the in-process cleanup its own bound (workspaceHomeImportCleanupTimeout)
// for the same reason; startup has strictly LESS cancellation available than an
// HTTP request does, so it needs one at least as much.
//
// # Why per record rather than one budget for the whole sweep
//
// A single budget shared by every record makes the last workspaces in the
// directory pay for the first ones: one unreachable-engine record consumes the
// lot, every record after it fails instantly on an already-expired context, and
// their wreckage stays for the dispatch path to catch — which is the LAST line
// of defence, not the eager one this function is. Per record, each workspace gets
// the same guarantee regardless of what the others did.
//
// The cost of that choice is that the total is records × this bound rather than
// this bound. That is acceptable because the count is not a function of
// installation size: a record exists only for a workspace whose OPERATOR-INITIATED
// migration was interrupted and which no later boot or dispatch has resolved,
// and both the sweep and the dispatch path clear it. In practice that is zero,
// occasionally one, and never a number that makes 30s each a startup people
// notice.
//
// # What a timeout leaves behind
//
// A timed-out VolumeRemove is a FAILED VolumeRemove, so the record stays in the
// phase it arrived in: the sweep's only phase write is the "home-absent" that
// follows a SUCCESSFUL removal (round 5), and a call that timed out never
// reaches it. A record that still says "home-destroyed" is still wreckage on the
// next boot and on the next dispatch, both of which retry; a record in any
// non-destructive phase never reaches the engine at all, so a hung engine cannot
// strand one.
//
// A var rather than a const only so tests can shrink it: the behaviour it buys —
// a sweep that RETURNS instead of hanging — cannot be observed at 30s without a
// 30-second test, and asserting only that a deadline exists would not show the
// sweep surviving the engine that never answers.
var workspaceHomeMigrationSweepTimeout = newAtomicDuration(30 * time.Second)

// SweepInterruptedWorkspaceHomeMigrations resolves every leftover
// interrupted-migration record on this installation and reports how many it
// resolved.
//
// Call it at daemon start, BEFORE anything can dispatch: it is the EAGER half of
// this file's acting mechanism, clearing wreckage while nothing is competing for
// it, including for workspaces no job will touch for days.
//
// It is not the guarantee, though — a sweep whose volume removal fails logs and
// lets startup continue, so the invariant "no init and no job runs against a home
// with a record" is enforced where it can actually be enforced: on the dispatch
// path (Runner.recoverInterruptedWorkspaceHomeMigration, codex round 3 Blocker
// 1). The two share their discard so a workspace repaired by a boot and one
// repaired by a dispatch end up in the same state.
//
// # What it does per record
//
// It registers the slug in the in-flight registry as a migration (so a dispatch
// that somehow reaches this workspace first waits rather than mounting a volume
// that is about to be removed) and takes the workspace's init flock (the same one
// an init and a migration take, so the sweep can never interleave with either).
// What happens under those two exclusions depends on the record's PHASE — see
// discardInterruptedWorkspaceHomeMigrationLocked, and this file's header for the
// table. A record that does not authorize a discard costs one unlink and no
// engine call at all.
//
// # The count
//
// swept counts RECORDS RESOLVED, of both kinds, because the caller's only use for
// it is deciding whether to say anything at startup — and "the daemon cleared
// leftover migration state" is worth saying whichever kind it was. Which kind
// each one was is in the per-record log line, which is where an operator looking
// for "was my home destroyed" will actually be.
//
// # Errors
//
// A per-record failure is logged, joined into the returned error and does not
// stop the sweep: one workspace whose volume the engine refuses to remove must
// not leave the others unswept. A non-nil error does not abort daemon startup —
// like the startup container reap it is scoped to resource reconciliation — but
// it does mean at least one workspace still holds a home nobody vouches for.
func (r *Runner) SweepInterruptedWorkspaceHomeMigrations(ctx context.Context) (swept int, err error) {
	metaDir, err := r.workspaceHomeMetaDir()
	if err != nil {
		return 0, fmt.Errorf("workspace home migration sweep: %w", err)
	}
	entries, err := os.ReadDir(metaDir)
	if err != nil {
		if os.IsNotExist(err) {
			// No homes-meta dir at all: an installation that has never resolved
			// a workspace home, so there is nothing that could have been
			// interrupted.
			return 0, nil
		}
		return 0, fmt.Errorf("workspace home migration sweep: read %q: %w", metaDir, err)
	}

	var slugs []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), workspaceHomeMigrationRecordSuffix) {
			continue
		}
		slug := strings.TrimSuffix(e.Name(), workspaceHomeMigrationRecordSuffix)
		if verr := orchestrator.ValidWorkspaceSlug(slug); verr != nil {
			// Not a name this daemon can have written. Left alone rather than
			// removed: deleting an unexplained file out of the daemon's own
			// state directory is not this function's business.
			slog.Warn("workspace home migration sweep: ignoring a record whose name is not a workspace slug",
				"file", e.Name(), "error", verr)
			continue
		}
		slugs = append(slugs, slug)
	}
	if len(slugs) == 0 {
		return 0, nil
	}

	// The importer is resolved LAZILY, per record, and only once a record has
	// been read and found to authorize a discard (round 4, Blocker 1). Demanding
	// it up front was right while every record meant "this home is wreckage"; now
	// that most leftover records mean the opposite, a backend with no migration
	// capability must not be stopped from clearing them — the clearing is an
	// unlink, and refusing to do it would leave records that then fail every
	// dispatch of those workspaces on the same missing capability.
	importerFor := workspaceHomeImporterOnce(r)

	var errs []error
	for _, slug := range slugs {
		if serr := r.sweepOneInterruptedWorkspaceHomeMigration(ctx, importerFor, metaDir, slug); serr != nil {
			errs = append(errs, serr)
			continue
		}
		swept++
	}
	return swept, errors.Join(errs...)
}

// workspaceHomeImporterOnce returns a memoized resolver for the backend's
// migration capability.
//
// Memoized because the answer is a type assertion on a field that does not
// change, and the sweep may ask once per record; a function rather than a value
// because most records must never ask at all (see the call site).
func workspaceHomeImporterOnce(r *Runner) func() (WorkspaceHomeImporter, error) {
	var (
		importer WorkspaceHomeImporter
		err      error
		done     bool
	)
	return func() (WorkspaceHomeImporter, error) {
		if !done {
			importer, err = workspaceHomeImporterFor(r)
			done = true
		}
		return importer, err
	}
}

// sweepOneInterruptedWorkspaceHomeMigration is one record's worth of
// SweepInterruptedWorkspaceHomeMigrations; see that function for the ordering
// and for why a failed removal keeps the record.
func (r *Runner) sweepOneInterruptedWorkspaceHomeMigration(ctx context.Context, importerFor func() (WorkspaceHomeImporter, error), metaDir, slug string) error {
	// The same exclusion a live migration takes, for the same reason: from here
	// on this function is a migration, and a dispatch must not mount a volume it
	// is about to remove. It refuses rather than waits if something is somehow
	// already in flight — at daemon start nothing is, and a refusal keeps the
	// record for the next boot rather than fighting over the volume.
	done, berr := r.homeInFlight.beginMigration(slug)
	if berr != nil {
		return fmt.Errorf("workspace home %q: sweep the interrupted migration: %w", slug, berr)
	}
	defer done()

	release, lerr := acquireWorkspaceHomeLock(workspaceHomeLockPath(metaDir, slug))
	if lerr != nil {
		return fmt.Errorf("workspace home %q: sweep the interrupted migration: acquire init lock: %w", slug, lerr)
	}
	defer release()

	// The bound (round 4, Major). Applied HERE rather than at the caller because
	// the guarantee belongs to the operation: buildRuntime hands this function
	// context.Background() and has nothing better to hand it, and a startup path
	// that can hang forever inside a reconciliation step is a property of the
	// step, not of who called it. Applied per record rather than around the whole
	// loop for the reason workspaceHomeMigrationSweepTimeout gives.
	//
	// It wraps only the engine-facing part. The two exclusions above are process-
	// local (a mutex-backed registry and a flock this daemon is alone in wanting
	// at startup), so a deadline on them would bound nothing that can hang.
	boundedCtx, cancel := context.WithTimeout(ctx, workspaceHomeMigrationSweepTimeout.Get())
	defer cancel()

	_, err := r.discardInterruptedWorkspaceHomeMigrationLocked(boundedCtx, importerFor, metaDir, slug)
	return err
}

// discardInterruptedWorkspaceHomeMigrationLocked is the actual discard, shared
// by the two callers that can reach a leftover record: the startup sweep and —
// since codex round 3's Blocker 1 — the dispatch path itself
// (Runner.recoverInterruptedWorkspaceHomeMigration).
//
// It reports what it DID, which is separate from whether it succeeded: the
// dispatch path prints nothing when there was nothing to do, and an operator
// reading the log after an incident needs "your home was discarded" and "a stale
// record was cleared" to be distinguishable.
//
// # The phase decides, not the file's existence (round 4, Blocker 1)
//
// A record whose phase does not authorize a discard is CLEARED: the record is
// removed and the home volume, the completion marker and the engine are all left
// alone. That is the whole of routes A and B in this file's header — a migration
// that finished, and a migration that was refused before it changed anything —
// and it is why this function no longer needs a migration-capable backend to
// resolve most records. It asks for one only on the branch that has already read
// a record saying the home was destroyed.
//
// On the branch that DOES discard, the record is moved into "home-absent" once
// the volume is gone and before it is unlinked (round 5, Blocker) — this
// function's own unlink is one of the three the file header's "the unlink is not
// a durability primitive" section covers, and the sweep is the caller that makes
// it reachable, because it logs a failure and lets startup continue.
//
// # What "Locked" means, and why it is the caller's job
//
// The caller must already hold BOTH exclusions this operation needs:
//
//   - the workspace's init flock, so no init or migration is writing into the
//     same home, and
//   - the in-flight registration for slug, so no OTHER dispatch or migration is
//     between resolving this volume and mounting it.
//
// The two callers satisfy the second one differently and that is exactly why it
// is not taken here. The sweep takes a MIGRATION registration
// (workspaceHomeInFlight.beginMigration), which is right at daemon start when
// nothing is dispatching. A dispatch already holds a DISPATCH registration by
// the time it gets here (Runner.beginWorkspaceHomeDispatch, taken before
// resolveWorkspaceHome), which excludes a migration on its own — and asking for
// a migration registration on top of it would deadlock against the dispatch's
// own, since beginMigration refuses while any dispatch of the same workspace is
// registered.
func (r *Runner) discardInterruptedWorkspaceHomeMigrationLocked(ctx context.Context, importerFor func() (WorkspaceHomeImporter, error), metaDir, slug string) (workspaceHomeMigrationRecovery, error) {
	recordPath := workspaceHomeMigrationRecordPath(metaDir, slug)

	// Read for the phase, the volume name and the log line an operator will want.
	// A record that will not parse is not a reason to skip: it still proves a
	// migration was started, and an unparseable record has no phase, which is
	// exactly the uncertain case that resolves to "discard".
	var rec workspaceHomeMigrationRecord
	data, exists, rerr := readIfExists(recordPath)
	if rerr != nil {
		return workspaceHomeMigrationRecoveryNone,
			fmt.Errorf("workspace home %q: read the interrupted-migration record %q: %w", slug, recordPath, rerr)
	}
	if !exists {
		// Either raced by something that removed it, or — on the dispatch path —
		// simply never there. Not an error: the state it describes is gone too.
		return workspaceHomeMigrationRecoveryNone, nil
	}
	if jerr := json.Unmarshal(data, &rec); jerr != nil {
		slog.Warn("workspace home: the interrupted-migration record is unreadable; treating it as a migration that DID destroy this home and falling back to the volume name derived from the slug",
			"workspace_slug", slug, "record", recordPath, "error", jerr)
		rec = workspaceHomeMigrationRecord{}
	}

	// The branch round 4 adds, and the one most leftover records take. Nothing
	// was destroyed (phase "recorded") or everything was rebuilt (phase
	// "home-rebuilt"), so the ONLY thing standing between this workspace and a
	// normal dispatch is the file itself.
	if !rec.discardsHome() {
		if derr := removeWorkspaceHomeMigrationRecord(recordPath); derr != nil {
			return workspaceHomeMigrationRecoveryCleared,
				fmt.Errorf("workspace home %q: clear the leftover migration record %q (its migration destroyed nothing, "+
					"so the home is left untouched): %w", slug, recordPath, derr)
		}
		slog.Info("workspace home: cleared a leftover `boid workspace import-home` record whose migration left the home intact; nothing was discarded (docs/plans/workspace-home-volume-persistence.md 論点 f)",
			"workspace_slug", slug, "phase", string(rec.Phase), "migration_started_at", rec.StartedAt)
		return workspaceHomeMigrationRecoveryCleared, nil
	}

	volumeName := rec.Volume
	if volumeName == "" {
		volumeName = dockerres.WorkspaceHomeVolumeName(r.InstallID, slug)
	}

	// Only now, on the branch that has actually read a record saying this home
	// was destroyed: a backend with no migration capability cannot discard the
	// volume, and proceeding would let init.sh run over it.
	importer, ierr := importerFor()
	if ierr != nil {
		slog.Error("workspace home: an interrupted migration destroyed this home and this backend cannot remove the volume it left; the record is kept for a later start",
			"workspace_slug", slug, "home_volume", volumeName, "error", ierr)
		return workspaceHomeMigrationRecoveryDiscarded, fmt.Errorf(
			"workspace home %q: an interrupted `boid workspace import-home` left a record at %q saying this home was "+
				"destroyed, and this backend cannot discard the home volume it describes: %w", slug, recordPath, ierr)
	}

	if _, verr := importer.RemoveWorkspaceHomeVolume(ctx, volumeName); verr != nil {
		slog.Error("workspace home: could not remove the home volume left by an interrupted migration; the record is kept so a later start or dispatch retries it",
			"workspace_slug", slug, "home_volume", volumeName, "error", verr)
		return workspaceHomeMigrationRecoveryDiscarded, fmt.Errorf(
			"workspace home %[1]q: 中断された `boid workspace import-home` が home volume %[2]q に途中まで展開された home を "+
				"残しており、それを削除できなかった: %[3]w. [復旧] 手で削除する (`docker volume rm %[2]s`) — 次の dispatch が空から "+
				"init.sh で作り直す。 放置すると、在庫確認型の init.sh が「もう入っている」と誤判定して壊れた home に完了マーカーを書きうる",
			slug, volumeName, verr)
	}

	// After the volume, never before: a marker removed while the volume it
	// describes survived would make the next dispatch re-run init.sh over the
	// partial home instead of over an empty one.
	markerPath := workspaceHomeMarkerPath(metaDir, slug)
	if merr := os.Remove(markerPath); merr != nil && !os.IsNotExist(merr) {
		// Not fatal: the volume is gone, so the marker describes an identity
		// that no longer exists and the next dispatch re-inits on that alone
		// (the same two-legged argument Runner.ImportWorkspaceHome makes).
		slog.Warn("workspace home: could not remove the completion marker while discarding an interrupted migration; relying on the vanished volume identity to force the re-init",
			"workspace_slug", slug, "marker", markerPath, "error", merr)
	}

	// The window closes HERE rather than by means of the unlink below (codex round
	// 5, Blocker). The wreckage this record described is gone, so the record must
	// stop authorizing a discard before it is deleted — otherwise a deletion that
	// is rolled back by a power cut brings back a record that destroys whatever
	// the next dispatch built in its place.
	//
	// The dispatch path would survive without it (it returns the unlink's error,
	// so no home gets built behind a record it could not remove) but the SWEEP
	// would not: buildRuntime logs a sweep failure and continues into auto-reopen,
	// deliberately, which is exactly the window the resurrection needs.
	//
	// A failure here keeps the record — the same choice, for the same reason, as
	// clearWorkspaceHomeMigrationRecordForAnAbsentHome's — and is returned, so a
	// dispatch fails loudly rather than proceeding and a sweep leaves it for the
	// next boot. Retrying it then costs one VolumeRemove that reports "not found".
	if perr := markWorkspaceHomeMigrationRecordHomeAbsent(recordPath, slug, volumeName, rec); perr != nil {
		return workspaceHomeMigrationRecoveryDiscarded, fmt.Errorf(
			"workspace home %[1]q: the home volume %[2]q left by an interrupted `boid workspace import-home` was removed, "+
				"but that could not be durably recorded at %[3]q, so the record is kept rather than deleted: %[4]w. "+
				"[復旧] daemon の state 領域 (homes-meta) が書ける状態か確認すること。 この record は存在しない volume を指して "+
				"いるので、次の起動・dispatch での再試行は無害",
			slug, volumeName, recordPath, perr)
	}

	if derr := removeWorkspaceHomeMigrationRecord(recordPath); derr != nil {
		return workspaceHomeMigrationRecoveryDiscarded,
			fmt.Errorf("workspace home %q: remove the interrupted-migration record %q: %w", slug, recordPath, derr)
	}

	slog.Warn("workspace home: discarded the home volume of a migration this daemon never finished; the workspace is initialized from scratch on its next dispatch (docs/plans/workspace-home-volume-persistence.md 論点 f)",
		"workspace_slug", slug, "home_volume", volumeName, "migration_started_at", rec.StartedAt)
	return workspaceHomeMigrationRecoveryDiscarded, nil
}

// workspaceHomeMigrationRecovery says what recovery did with a leftover record.
//
// Three outcomes rather than the "found" boolean round 3 had, because round 4
// splits the one that used to be implicit: a record can now be resolved WITHOUT
// anything being destroyed, and the difference between that and a discarded home
// is the difference between a log line an operator can ignore and one they
// cannot.
type workspaceHomeMigrationRecovery int

const (
	// workspaceHomeMigrationRecoveryNone: there was no record.
	workspaceHomeMigrationRecoveryNone workspaceHomeMigrationRecovery = iota

	// workspaceHomeMigrationRecoveryCleared: a record was found and removed, and
	// the workspace's home was NOT touched — its phase said the migration had
	// destroyed nothing, or had finished rebuilding.
	workspaceHomeMigrationRecoveryCleared

	// workspaceHomeMigrationRecoveryDiscarded: the record authorized a discard,
	// so the home volume was removed (or the attempt to remove it failed, which
	// is reported through the error rather than through this value — the point of
	// the value is which OUTCOME was being pursued, so a caller can word its log
	// line before knowing whether it worked).
	workspaceHomeMigrationRecoveryDiscarded
)

// recoverInterruptedWorkspaceHomeMigration is the DISPATCH-path half of the
// sentinel mechanism (codex review of PR8 round 3, Blocker 1).
//
// # Why the startup sweep is not enough on its own
//
// SweepInterruptedWorkspaceHomeMigrations releases its registration on the way
// out whether or not the volume actually went, and buildRuntime logs a sweep
// failure and continues into auto-reopen — deliberately, because a reconciliation
// failure must not brick a daemon start. Put together, one entirely ordinary boot
// defeats the whole mechanism:
//
//	kill mid-migration  ->  partial volume + record, no marker
//	boot, engine flaky  ->  sweep's VolumeRemove fails; record kept, startup continues
//	engine recovers     ->  nothing re-runs the sweep
//	auto-reopen/dispatch->  in-flight registry is EMPTY, so nothing waits
//	                        init.sh runs against the partial home
//
// and an init.sh that probes for what it installs answers "already installed",
// exits 0, and resolveWorkspaceHome writes a completion marker over the wreckage.
// That is precisely the frozen state the sentinel exists to prevent, reached
// without any second crash.
//
// So the invariant is stated where it can actually be enforced: WHILE A RECORD
// EXISTS FOR THIS SLUG, NO INIT AND NO JOB RUNS AGAINST THAT HOME. resolveWorkspaceHome
// calls this before it ensures the volume, so the check is upstream of every use.
//
// # Repair rather than refuse (option (a), not (b))
//
// A record could simply make the dispatch fail until an operator intervened, and
// that would be sound. It would also be worse in the case that actually occurs:
// the record survives because the ENGINE was briefly unreachable, and by the time
// a job is dispatched the engine is normally back. Refusing would convert a
// transient infrastructure blip into an indefinite outage of one workspace,
// resolvable only by a human who first has to work out which volume to delete —
// while repairing converts it into one extra init run, which is the cost this
// whole file already accepts everywhere else (see the "over-reach is the safe
// direction" section above).
//
// The discard is IDENTICAL to the sweep's, deliberately: same order (volume,
// then marker, then record), same shared implementation. A dispatch that heals a
// workspace must not leave it in a state a boot would have left differently.
//
// When the engine is still not answering this returns an error and the dispatch
// fails. That is the fail-loud side and it is the cheaper of the two outcomes: a
// failed job is recorded on the task and re-runnable, while a completion marker
// written over a half-extracted home is permanent and surfaces much later,
// somewhere else.
//
// # Cost on the ordinary path
//
// One os.ReadFile of a path that does not exist, per dispatch, before the flock.
// The lock and the importer lookup happen only once a record has actually been
// seen.
func (r *Runner) recoverInterruptedWorkspaceHomeMigration(ctx context.Context, metaDir, slug string) error {
	recordPath := workspaceHomeMigrationRecordPath(metaDir, slug)
	if _, err := os.Stat(recordPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		// Unreadable is not "absent": the fail-safe reading of an uncertain
		// record is that a migration WAS interrupted, and the caller must not
		// mount the home on a guess.
		return fmt.Errorf(
			"workspace home %q: could not tell whether an interrupted `boid workspace import-home` left this home in a "+
				"partial state (%q): %w. [復旧] このファイルを確認し、移行が中断されていたなら home volume を削除してから消すこと",
			slug, recordPath, err)
	}

	// The lock is scoped to this closure rather than deferred to the enclosing
	// function, because the enclosing caller (resolveWorkspaceHome) takes this
	// very lock again on its slow path — and flock on a SECOND open file
	// description of the same file blocks against the first one, even within one
	// process. A closure rather than a bare release() call so a panic in the
	// discard cannot leave the workspace's init lock held for the life of the
	// daemon.
	//
	// The importer is resolved lazily inside the discard (round 4, Blocker 1):
	// most leftover records describe a migration that destroyed nothing, and
	// failing a dispatch over a missing migration capability that the record does
	// not need would turn a stale file into an outage of the workspace.
	var outcome workspaceHomeMigrationRecovery
	derr := func() error {
		release, lerr := acquireWorkspaceHomeLock(workspaceHomeLockPath(metaDir, slug))
		if lerr != nil {
			return fmt.Errorf("workspace home %q: acquire init lock to discard an interrupted migration: %w", slug, lerr)
		}
		defer release()
		var ferr error
		outcome, ferr = r.discardInterruptedWorkspaceHomeMigrationLocked(ctx, workspaceHomeImporterOnce(r), metaDir, slug)
		return ferr
	}()
	if derr != nil {
		return derr
	}
	if outcome == workspaceHomeMigrationRecoveryDiscarded {
		slog.Warn("workspace home: a dispatch found the wreckage of an interrupted migration that no daemon start had managed to clear, and discarded it; this workspace initializes from scratch",
			"workspace_slug", slug)
	}
	return nil
}
