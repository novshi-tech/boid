package dispatcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/novshi-tech/boid/internal/dockerres"
	"github.com/novshi-tech/boid/internal/version"
)

// workspace_home_import.go is the DAEMON-side half of PR8 of
// docs/plans/workspace-home-volume-persistence.md (論点 f): replace one
// workspace's HOME volume with the contents of its pre-PR6 host directory,
// and guarantee that the next dispatch re-runs init.sh over it.
//
// # D1 — the daemon does the work; the CLI only supplies the bytes
//
// 論点 f says the migration 「は host mode CLI からの実行に限定される」, which is
// true of the READ: the legacy homes/<slug> tree is on the host filesystem, and
// under the compose deploy the daemon container cannot see it (that is the
// whole reason the payload is a tar stream rather than a bind — see
// WriteWorkspaceHomeTar). It does NOT follow that the CLI should drive the
// engine itself, and it deliberately does not:
//
//   - The completion marker lives at <dataHome>/homes-meta/<slug>.init.json,
//     inside the daemon's own `boid_state` volume. A host CLI cannot read or
//     delete it, and deleting it is HALF of 論点 f's acceptance condition.
//   - The per-workspace init flock is the daemon's
//     (workspaceHomeLockPath/acquireWorkspaceHomeLock). A CLI that swapped the
//     volume out from under a concurrent dispatch would race the very lock that
//     exists to serialize writes into that home.
//   - The volume's identity label is minted and compared by this package
//     (workspaceHomeMarker.HomeID). Keeping the mint, the stamp and the marker
//     write on one side of the wire is what keeps them consistent.
//
// `boid reap` IS a CLI that talks to the engine directly, and the difference is
// its requirement rather than its convenience: reap is the deploy-level
// rollback reaper and has to work when the daemon is down or unreachable
// (docs/plans/phase6-container-backend.md §決定6). This has the opposite
// requirement — it can only be correct WITH a live daemon, because everything
// it has to keep consistent lives in the daemon's own state.
//
// So: the CLI reads the host directory and streams a tar; the daemon takes the
// lock, replaces the volume, feeds the tar to a throwaway container, and clears
// the marker.

// ErrWorkspaceHomeInUse is what a migration refused because a job is running in
// that workspace comes back as.
//
// It is a sentinel because the API layer has to turn it into a 409 and the CLI
// into an actionable message, and neither can do that by matching on the
// engine's prose. See containerBackend.RemoveWorkspaceHomeVolume for the
// engine-side fact it is derived from.
var ErrWorkspaceHomeInUse = errors.New("the workspace HOME volume is in use by a running container")

// ErrWorkspaceHomeBusy is what a migration refused by the daemon's OWN
// in-flight registry comes back as (workspaceHomeInFlight): another operation
// in this process is already using that workspace's HOME volume.
//
// Distinct from ErrWorkspaceHomeInUse, which is the ENGINE's answer about a
// running container. This one covers the interval the engine cannot see: a
// dispatch that has resolved the home but has not yet created the container
// that would make it in-use, and a concurrent migration. Both are 409s at the
// API and both mean "wait and re-run", so the split is about the message an
// operator reads, not about what they do next.
var ErrWorkspaceHomeBusy = errors.New("the workspace HOME volume is busy in this daemon")

// WorkspaceHomeImportRequest is one extraction run, fully resolved.
type WorkspaceHomeImportRequest struct {
	// Slug is the normalized workspace slug, used for the container name, its
	// label and every diagnostic.
	Slug string

	// HomeSource is the NAME of the workspace's docker named volume
	// (dockerres.WorkspaceHomeVolumeName) — never a path, for the reason
	// workspaceInitHomeMount spells out.
	HomeSource string

	// HomeTarget is where HomeSource is mounted inside the extraction
	// container, and therefore what the archive's relative paths resolve
	// against. It is hostHomeDir(), the same path a JOB sees its own $HOME at,
	// which is what makes an absolute path baked into a migrated wrapper script
	// or shebang land where the harness will later look for it.
	HomeTarget string

	// HomeID is the identity the (freshly re-created) volume carries on its
	// dockerres.LabelWorkspaceHomeID label. The executor re-checks its own
	// VolumeCreate's answer against it before mounting anything, for the reason
	// verifyWorkspaceHomeIdentity gives.
	HomeID string

	// Tar is the archive to extract, streamed rather than buffered: the
	// measured homes on the machine this was written for run to 4.3GB, and the
	// daemon must not hold one of those in memory.
	Tar io.Reader
}

// WorkspaceHomeImporter is the capability Runner.ImportWorkspaceHome needs from
// a sandbox backend, on top of WorkspaceInitExecutor.
//
// A separate interface, discovered by type assertion, for the same three
// reasons WorkspaceInitExecutor is (see its §D10 note) — and separate from
// WorkspaceInitExecutor rather than folded into it because that interface is
// on the DISPATCH path: every backend has to satisfy it for jobs to run at all,
// and a backend that can run jobs but cannot serve a one-off migration should
// fail the migration, not every dispatch.
type WorkspaceHomeImporter interface {
	// RemoveWorkspaceHomeVolume deletes volumeName and reports whether there
	// was one to delete. A volume that is not there is NOT an error (a
	// workspace can be migrated into before it has ever been dispatched into),
	// which is why "existed" is a separate return rather than inferred from a
	// nil error. A volume held by a running container comes back as an error
	// wrapping ErrWorkspaceHomeInUse, with nothing removed.
	RemoveWorkspaceHomeVolume(ctx context.Context, volumeName string) (existed bool, err error)

	// ImportWorkspaceHome extracts req.Tar into req.HomeSource, mounted at
	// req.HomeTarget.
	ImportWorkspaceHome(ctx context.Context, req WorkspaceHomeImportRequest) error
}

// WorkspaceHomeImportResult describes a completed migration.
type WorkspaceHomeImportResult struct {
	// Slug is the normalized workspace slug the migration ran against — not
	// necessarily the string the caller passed (an empty workspace id
	// normalizes to the default workspace).
	Slug string

	// Volume is the home volume that was replaced,
	// dockerres.WorkspaceHomeVolumeName(installID, slug).
	Volume string

	// HomeID is the identity the NEW incarnation carries. It differs from
	// whatever the old one had, which is 論点 f's leg 1.
	HomeID string

	// PreviousExisted reports whether there was a HOME VOLUME to replace — the
	// engine's answer, not an inference from the completion marker (the two can
	// disagree: an operator can delete either one independently, which is the
	// whole reason workspaceHomeMarker.HomeID exists). false means the
	// workspace had never been dispatched into, which is not an error and is
	// the case a fresh install restoring from a backup lands in.
	PreviousExisted bool

	// MarkerRemoved / MarkerRemoveError report 論点 f's leg 2. A failure here
	// is not fatal (leg 1 has already re-armed the init on its own) but it IS
	// reported, because the two legs exist so that either one alone suffices
	// and an operator should be told when they are down to one.
	MarkerRemoved     bool
	MarkerRemoveError string

	// MigrationRecordRemoveError is non-empty when the migration SUCCEEDED but
	// its in-progress record could not be deleted afterwards
	// (workspace_home_migration_sentinel.go).
	//
	// Not fatal — the home is correct and complete — and reported anyway,
	// because leftover daemon state is worth saying out loud. What it COSTS
	// depends on MigrationRecordDiscardsHome.
	MigrationRecordRemoveError string

	// MigrationRecordDiscardsHome says whether that leftover record still
	// authorizes the next daemon start to DESTROY this home (codex round 4,
	// Blocker 1). Meaningful only alongside MigrationRecordRemoveError.
	//
	// false — the ordinary case — means the migration managed to move the record
	// into its "the home was rebuilt" phase before failing to delete it, so
	// recovery will do nothing but the unlink this daemon could not do. The
	// operator's home is safe and no action is required.
	//
	// true means it could not do even that, so the record is still in the phase
	// that says this home is wreckage. The consequence arrives later and looks
	// unrelated when it does: the next daemon start discards this workspace's
	// freshly imported home and re-initializes it from scratch. Deleting the
	// named file before restarting saves re-doing a multi-GB migration, and that
	// difference is the whole reason this is a separate field rather than
	// something the CLI could infer.
	MigrationRecordDiscardsHome bool
}

// workspaceHomeImportArgv is the command the extraction container runs, shared
// by the container backend and the test double so the flags can only be got
// right or wrong in ONE place.
//
// Every flag is load-bearing:
//
//   - --no-same-owner (D3): ownership recorded in the archive is discarded and
//     everything lands owned by the uid the container runs as, which is the uid
//     the job container runs as. This is the flag that makes the tar stream a
//     valid answer to the rootless-podman uid-mapping trap 論点 f names — it is
//     also GNU tar's default for a non-root extractor, and stated anyway so the
//     contract does not depend on which uid the container happened to get.
//   - --same-permissions (D3: 「`0600` などの mode は維持すること」): without it
//     a non-root GNU tar applies the process umask to every extracted mode. A
//     0600 credentials file survives a umask by luck (a umask can only clear
//     bits it already has), but a 0666 or 0755 entry does not, and "the
//     permissions are mostly right" is not a contract. WITH it, the modes are
//     the archive's, full stop.
//   - --directory: the volume's mount point. Entry names are relative
//     (normalizeWorkspaceHomeHeader), so this is the only thing deciding where
//     they land.
//   - --file - : read the archive from stdin, which is where the daemon writes
//     it.
//
// Deliberately NOT here: --same-owner (would need root and would re-introduce
// the host's uids), -z/-J (the CLI streams uncompressed; compressing would
// spend CPU on a local socket transfer), and --overwrite/--keep-old-files
// (the volume is brand new by the time this runs, so there is nothing to
// resolve).
func workspaceHomeImportArgv(target string) []string {
	return []string{
		"tar",
		"--extract",
		"--file", "-",
		"--directory", target,
		"--no-same-owner",
		"--same-permissions",
	}
}

// ImportWorkspaceHome replaces workspaceID's HOME volume with the contents of
// tarStream, and guarantees the next dispatch runs its init.sh once.
//
// # The guarantee, and why it takes two mechanisms (論点 f)
//
// The skip condition (workspaceHomeInitialized) is five ANDs, and a migration
// changes NONE of them by itself: it copies contents, and the marker describes
// the script, the generation, the skeleton and the volume's IDENTITY — never
// its contents. A settled workspace would therefore keep skipping init and the
// host-era toolchain, with its host-era absolute paths, would live on forever.
// 論点 f calls that the hole and requires the migration to close it itself.
// Both of its options are taken, and the ORDER is what makes either one enough
// on its own:
//
//  1. Remove the volume. This is also the in-use check (see below) and is the
//     only step that can refuse WITHOUT side effects, so it goes first: an
//     operator told "no" still has exactly the workspace they had.
//  2. Delete the completion marker. From here the home is already gone, so
//     nothing is being risked by trying; a failure is reported and not fatal.
//  3. Create the volume again, with a fresh identity.
//  4. Extract the tar into it.
//
// After step 1 succeeds, every later outcome re-initializes:
//
//   - all four succeed -> the marker is gone AND the identity is new.
//   - step 2 fails, 3-4 succeed -> the marker survives but describes an
//     identity that no longer exists (leg 1).
//   - step 3 or 4 fails -> the marker is gone (leg 2). If step 3 failed there
//     is no volume at all, and the next dispatch creates a fresh one, which is
//     re-initialized on both counts.
//
// The numbering skips half-steps that belong to the crash-recovery mechanism
// rather than to 論点 f — step 0 and 1.5 write the interrupted-migration record
// and open its destruction window, step 5 closes it and step 6 removes it. They
// are numbered in the body where they occur; see
// workspace_home_migration_sentinel.go for what each phase means and why 1.5
// sits between the removal and the re-creation. The only one that can stop a
// migration whose steps 1-4 would have worked is 1.5, and it stops it in the
// same "the home is empty, the next dispatch rebuilds it" state step 3's own
// failure produces.
//
// Every path that leaves before step 5 closes the window too, with the phase
// that matches where it stopped: "home-absent" once the volume is confirmed
// gone, and nothing at all while the record still says "recorded" (it already
// authorizes no destruction). The window is never left open across an unlink —
// see clearWorkspaceHomeMigrationRecordForAnAbsentHome for why the unlink cannot
// carry that job itself.
//
// TestRunner_ImportWorkspaceHome_ReInitsWithTheMarkerRemovalUNDONE and its
// mirror pin the two legs INDEPENDENTLY, by undoing one and checking the other
// still fires.
//
// # Refusing a workspace with a job in it (D4)
//
// The refusal is the engine's own 409 on the volume removal ("volume is being
// used by the following container(s)", measured against podman 4.9.3 in PR7,
// and not overridable by any force flag). That IS the pre-flight check: it is
// atomic with the removal, so a busy workspace is refused before anything has
// been destroyed. A separate ContainerList beforehand would be strictly worse —
// it could only be more stale than the removal it precedes, and it would
// suggest a guarantee ("we checked") that the window after it cannot support.
//
// # What a failure leaves behind (D4)
//
// Once step 1 has succeeded the migration is not reversible: the old contents
// are gone and boid never had a copy of them (the source is read-only, and it
// is still on the host — this is why the CLI refuses to delete it). What a
// failure leaves is a workspace whose home is EMPTY (not partial — step 5
// discards a half-extracted volume, see discardPartialWorkspaceHome) and whose
// init is armed, i.e. the state a brand-new workspace is in. The next dispatch
// prepares it from scratch; re-running the migration is also safe. That is the
// whole recovery story, and it is why the legacy directory must not be deleted
// by this command.
//
// # What a KILL leaves behind (codex review of PR8 round 2, Major 2)
//
// Step 5 only runs when this function returns. A daemon that is SIGKILLed, OOM
// killed or powered off inside steps 1-4 runs no defer and no cleanup, and
// leaves precisely the half-extracted volume step 5 exists to prevent. Step 0
// below writes a durable record of the migration before anything is destroyed,
// and Runner.SweepInterruptedWorkspaceHomeMigrations finishes the job at the
// next daemon start. See workspace_home_migration_sentinel.go for the ordering,
// for why every crash point lands on the "delete and re-initialize" side, and
// for why this cannot be a label on the volume.
//
// # Excluding a concurrent dispatch, and a concurrent remove
//
// (codex review of PR8 round 1, Blocker 2; round 2, Major 1.)
//
// The flock serializes this against another dispatch's INIT, and that is all it
// does: resolveWorkspaceHome's fast path (a satisfied marker) returns without
// taking it, so a settled workspace's dispatch holds nothing between resolving
// its home and asking the engine for a container. A migration landing in that
// gap removes the volume, re-creates it under the same name and starts
// extracting, and the dispatch's ContainerCreate then mounts the replacement BY
// NAME — the job's agent and the migration's tar writing into one volume.
//
// PR6's identity re-check does not close that. It compares what
// containerBackend.ensureNamedVolumes' own VolumeCreate answered against the
// identity the dispatch resolved, which catches a replacement landing before
// that call and cannot catch one landing after it, because the comparison is
// not atomic with ContainerCreate. (This function's first version claimed the
// job side would 「大声で失敗する」 in every ordering; it fails loudly in one of
// them.)
//
// So the exclusion is explicit: r.homeInFlight, registered at the top of this
// function, refuses a migration while any dispatch of the same workspace is
// between those two points, and parks a dispatch that arrives while a migration
// is running.
//
// `boid workspace remove` is the third participant, and the one round 2 found.
// It deletes the workspace ROW and then the home VOLUME, so a migration that
// overlaps it re-creates the volume for a row that is gone: credentials in a
// volume no dispatch can mount and `workspace remove` can no longer target (it
// 404s on the missing row). Removals therefore register too, and a migration is
// refused while one is in flight. That covers the overlap and not the ordering
// where a removal FINISHES before this function registers, which is what
// r.ConfirmWorkspaceExists below is for — asked under the registration, so its
// answer cannot go stale.
//
// See workspaceHomeInFlight for why the registry is in memory, why the three
// directions are not symmetric (a dispatch waits, a migration and a removal
// refuse), and which windows it deliberately leaves open (another process; the
// remaining delete-only paths, which the identity check really does cover).
func (r *Runner) ImportWorkspaceHome(ctx context.Context, workspaceID string, tarStream io.Reader) (WorkspaceHomeImportResult, error) {
	var res WorkspaceHomeImportResult

	slug, err := normalizeWorkspaceSlug(workspaceID)
	if err != nil {
		return res, err
	}
	res.Slug = slug

	// Registered before ANYTHING else — before the capability probes, before
	// the meta dir, before the flock — because this is the one refusal that has
	// to be able to happen while the workspace is still untouched, and because
	// a dispatch parked in beginDispatch is waiting for the whole of this
	// function rather than for its destructive part.
	//
	// It is the counterpart of Dispatch's own registration and covers the gap
	// the flock does not: a dispatch on resolveWorkspaceHome's fast path holds
	// no lock at all between resolving this volume and asking the engine to
	// mount it, so the flock taken below can be free while a job is about to
	// start against the very volume this function replaces. See
	// workspaceHomeInFlight.
	doneMigration, err := r.homeInFlight.beginMigration(slug)
	if err != nil {
		return res, err
	}
	defer doneMigration()

	// Immediately AFTER the registration and before anything else, which is the
	// whole of the fix (codex round 2, Major 1): from here a `boid workspace
	// remove` of this workspace is refused, so this answer cannot go stale
	// before the volume exists. Asked before the registration it would only
	// narrow the window; asked after the volume is created it would be reporting
	// an orphan rather than preventing one. See Runner.ConfirmWorkspaceExists.
	//
	// The service's error is returned as-is (wrapped) rather than translated,
	// because it carries the status the API has to answer with — a deleted
	// workspace is a 404, an unreachable database is a 500, and this function
	// has no business deciding which.
	if r.ConfirmWorkspaceExists != nil {
		if err := r.ConfirmWorkspaceExists(slug); err != nil {
			return res, fmt.Errorf(
				"workspace home %q: the workspace is no longer registered, so migrating into it would leave a HOME "+
					"volume nothing can dispatch into and `boid workspace remove` can no longer delete. Nothing has been "+
					"changed: %w", slug, err)
		}
	}

	executor, err := workspaceInitExecutorFor(r)
	if err != nil {
		return res, fmt.Errorf("workspace home %q: %w", slug, err)
	}
	importer, err := workspaceHomeImporterFor(r)
	if err != nil {
		return res, fmt.Errorf("workspace home %q: %w", slug, err)
	}

	metaDir, err := r.workspaceHomeMetaDir()
	if err != nil {
		return res, fmt.Errorf("workspace home %q: %w", slug, err)
	}
	// mkdirAllDurable, not os.MkdirAll (codex round 4, Blocker 2): the record
	// written at step 0 is only as durable as the directory holding it, and this
	// is the one caller that destroys something on the strength of that record.
	// See mkdirAllDurable for what it flushes and why it also flushes when the
	// directory already existed.
	if err := mkdirAllDurable(metaDir, 0o700); err != nil {
		return res, fmt.Errorf(
			"workspace home %q: create the homes meta dir %q durably: %w. Nothing has been changed — the in-progress "+
				"migration record has to outlive a power cut, and a directory that may not itself survive one cannot hold "+
				"it. [復旧] daemon の state 領域が書ける状態か確認してから再実行すること",
			slug, metaDir, err)
	}

	// Computed here and handed downstream, exactly as resolveWorkspaceHome does
	// and for the same reason: this string has to name the volume a dispatch
	// mounts, and two computations of a deterministic name are two things that
	// can disagree.
	volumeName := dockerres.WorkspaceHomeVolumeName(r.InstallID, slug)
	res.Volume = volumeName

	// The LAST refusal that costs nothing, and the last thing before the flock
	// (codex round 3, Blocker 2): an archive with no members would extract
	// perfectly into the volume this function is about to destroy and re-create,
	// and report success over a home that now holds nothing. It is checked here,
	// above the lock, so waiting for the first block of a network stream does not
	// hold a file lock a dispatch's init could be waiting on.
	//
	// It reads exactly one tar block and replays it, so the extraction still gets
	// the archive from byte zero — see peekWorkspaceHomeTarHasEntries.
	replayed, hasEntries, err := peekWorkspaceHomeTarHasEntries(tarStream)
	if err != nil {
		return res, fmt.Errorf("workspace home %q: read the start of the uploaded archive: %w", slug, err)
	}
	if !hasEntries {
		return res, fmt.Errorf(
			"workspace home %[1]q: the uploaded archive contains no entries, so finishing this migration would replace the "+
				"workspace's HOME volume with an empty one — its harness authentication and its installed toolchain would be "+
				"gone. Nothing has been changed. 移行元が空か、`--from` が空のディレクトリ (typo・未マウントのバックアップ等) を "+
				"指している可能性が高い。 意図して空の home にしたい場合は `boid workspace remove %[1]s` してから作り直すこと: %[2]w",
			slug, ErrWorkspaceHomeImportEmpty)
	}
	tarStream = replayed

	// The same flock a dispatch's init takes, so a migration and an init can
	// never be writing into one home at once.
	release, err := acquireWorkspaceHomeLock(workspaceHomeLockPath(metaDir, slug))
	if err != nil {
		return res, fmt.Errorf("workspace home %q: acquire init lock: %w", slug, err)
	}
	defer release()

	markerPath := workspaceHomeMarkerPath(metaDir, slug)
	recordPath := workspaceHomeMigrationRecordPath(metaDir, slug)

	// --- step 0: say so, durably, BEFORE anything is destroyed ---------------
	//
	// Everything from here to the end of step 4 is a window in which this
	// workspace's home is not a home, and a process that stops existing inside
	// it runs none of the recovery below. The record is what makes the next
	// daemon start able to tell that state from a finished one — see
	// workspace_home_migration_sentinel.go for the ordering argument and for why
	// the Engine API cannot express this as a label on the volume itself.
	//
	// It goes in as phase "recorded" — the migration has BEGUN and nothing has
	// been destroyed (codex round 4, Blocker 1). That is not a formality: this
	// exact record is what survives when the removal at step 1 is refused and the
	// cleanup unlink then fails, and a recovery that read it as "the home was
	// destroyed" would delete a home no part of this function ever touched.
	//
	// It FAILS the migration rather than proceeding without a durable record
	// (codex round 3, Major): the record's only value is that it outlives the
	// process, and destroying the home behind a sentinel that may not survive a
	// power cut is the exact state the sweep exists to make impossible. Stopping
	// here is the cheapest possible failure — nothing has been destroyed, so the
	// operator still has the workspace they had.
	rec := workspaceHomeMigrationRecord{
		Slug:        slug,
		Volume:      volumeName,
		Phase:       workspaceHomeMigrationRecorded,
		StartedAt:   time.Now().UTC(),
		BoidVersion: version.Version(),
	}
	if err := writeWorkspaceHomeMigrationRecord(recordPath, rec); err != nil {
		// Whatever the failed write did manage to leave behind goes with it, on
		// the same reasoning as the refusal at step 1 below: a record describing a
		// migration that never began would make the next daemon start discard an
		// intact home.
		discardWorkspaceHomeMigrationRecord(recordPath, slug)
		return res, fmt.Errorf(
			"workspace home %q: could not durably record the in-progress migration at %q: %w. Nothing has been changed — "+
				"the record is written before anything is destroyed precisely so this failure is free. [復旧] "+
				"daemon の state 領域 (homes-meta) が書ける状態か確認してから再実行すること",
			slug, recordPath, err)
	}

	// --- step 1: destroy the old incarnation (and refuse a busy workspace) ---
	existed, err := importer.RemoveWorkspaceHomeVolume(ctx, volumeName)
	if err != nil {
		// Nothing was destroyed — that is the whole reason the removal goes
		// first — so the record is removed with the refusal. Unlike before round
		// 4 this is TIDINESS rather than safety: the record is still in phase
		// "recorded", so a cleanup that fails here leaves something recovery
		// resolves by deleting the file. That is precisely route B of
		// workspace_home_migration_sentinel.go's header, and the reason the phase
		// exists — this unlink's error is logged and dropped, and before round 4
		// dropping it cost the workspace its home.
		//
		// The one case that is not literally "nothing destroyed" is an engine
		// that removed the volume and then failed to say so (a dropped
		// connection on the response). Leaving the record in "recorded" is still
		// right there, because that outcome is the one PR6's identity mechanism
		// genuinely covers: the volume is gone, the next dispatch's own
		// VolumeCreate mints a fresh identity, the surviving completion marker no
		// longer matches it, and init re-runs (workspaceHomeInitialized). No
		// half-extracted home can exist on this path — nothing has been extracted
		// yet.
		discardWorkspaceHomeMigrationRecord(recordPath, slug)
		if errors.Is(err, ErrWorkspaceHomeInUse) {
			return res, fmt.Errorf(
				"workspace home %q: refusing to migrate while a job is running in it — the engine is holding the volume %q "+
					"for a live container, and nothing has been changed. Wait for the job to finish (`boid job list`) and re-run: %w",
				slug, volumeName, err)
		}
		return res, fmt.Errorf("workspace home %q: replace the home volume %q: %w", slug, volumeName, err)
	}
	res.PreviousExisted = existed

	// --- step 1.5: open the destruction window, durably ----------------------
	//
	// (codex round 4, Blocker 1.) The removal has SUCCEEDED, so from here until
	// step 4 returns, whatever appears under this volume name was put there by a
	// migration that may not finish. This is the transition that authorizes
	// recovery to throw it away, and it is committed BEFORE the replacement
	// volume is created — which is what keeps the recorded destructive interval
	// strictly wider than the interval in which such a volume exists.
	//
	// Fail-closed, exactly like step 0 and for the same reason one step further
	// on: proceeding to extract behind a window that may not survive a power cut
	// would leave a partial home under a record saying nothing was destroyed, and
	// recovery would then hand it to init.sh. Stopping here is safe because the
	// volume is GONE rather than partial — the state a workspace that has never
	// been dispatched into is in — so the next dispatch builds it from empty.
	rec.Phase = workspaceHomeMigrationHomeDestroyed
	windowErr := writeWorkspaceHomeMigrationRecord(recordPath, rec)

	// --- step 2: 論点 f leg 2 -------------------------------------------------
	//
	// Immediately after the destruction, not at the end: from this point the
	// home the marker vouches for does not exist, so a marker left behind is
	// already wrong, and doing it here means even an abrupt daemon death
	// between the steps lands on the re-init side. Done even when the window
	// write above failed, because the marker is wrong either way and leg 2 is
	// free.
	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		res.MarkerRemoveError = err.Error()
		slog.Warn("workspace home import: could not remove the completion marker; relying on the new volume identity to force the re-init",
			"workspace_slug", slug, "marker", markerPath, "error", err)
	} else {
		res.MarkerRemoved = true
	}

	if windowErr != nil {
		// The volume is GONE — step 1 removed it and step 3 never ran — so this
		// record has nothing left to authorize and must stop claiming otherwise
		// BEFORE it is unlinked (codex round 5, Blocker). What is on disk at this
		// point is NOT reliably "recorded", which is what this branch used to
		// assume: writeWorkspaceHomeJSONFile fsyncs the directory AFTER the
		// rename, so the commonest way that write fails is with "home-destroyed"
		// already in place and merely unflushed.
		clearWorkspaceHomeMigrationRecordForAnAbsentHome(recordPath, slug, rec)
		return res, fmt.Errorf(
			"workspace home %q: the old home volume %q was removed, but the migration could not durably record that it "+
				"had been, so it stopped rather than extract into a volume nothing could later identify as unfinished: %w. "+
				"この workspace の home は「一度も dispatch していない」状態 (= 空) になっており、次の dispatch が init.sh で "+
				"作り直す。 移行元は読んだだけなので失われたものは無い。 [復旧] daemon の state 領域 (homes-meta) が書ける状態か "+
				"確認してから再実行すること",
			slug, volumeName, windowErr)
	}

	// --- step 3: 論点 f leg 1 — a NEW incarnation with a NEW identity --------
	//
	// The record deliberately STAYS if this fails. The old home is already gone
	// and whatever is (or is not) under this name now was never prepared by
	// anybody, so the next daemon start removing it and letting the next
	// dispatch build from scratch is exactly right.
	homeID, err := r.ensureWorkspaceHomeVolume(ctx, executor, slug, volumeName)
	if err != nil {
		return res, err
	}
	res.HomeID = homeID

	// --- step 4: extract ----------------------------------------------------
	if err := importer.ImportWorkspaceHome(ctx, WorkspaceHomeImportRequest{
		Slug:       slug,
		HomeSource: volumeName,
		HomeTarget: hostHomeDir(),
		HomeID:     homeID,
		Tar:        tarStream,
	}); err != nil {
		// --- step 4b: make the failure state real (codex review of PR8, Major) -
		//
		// See discardPartialWorkspaceHome: without this the volume survives
		// holding whatever crossed before the stream stopped, which is NOT the
		// "never dispatched into" state this function's contract promises.
		discarded, importErr := discardPartialWorkspaceHome(ctx, importer, slug, volumeName, err)
		if discarded {
			// The wreckage is gone, so nothing under this name is wreckage any
			// more and the window closes HERE — before the unlink, not by means of
			// it (codex round 5, Blocker). The window had to stay open right up to
			// this point, which is why the phase is only moved on the branch where
			// the removal actually succeeded.
			clearWorkspaceHomeMigrationRecordForAnAbsentHome(recordPath, slug, rec)
		}
		// When it is NOT discarded the record stays on purpose, still in the
		// phase that authorizes a discard: the error above tells the operator to
		// remove the volume by hand, and if they do not, the next daemon start
		// has to finish the job rather than let a dispatch run init.sh over a
		// partial home.
		return res, importErr
	}

	// --- step 5: close the destruction window --------------------------------
	//
	// (codex round 4, Blocker 1.) The home is a home again, so this record must
	// stop authorizing anybody to destroy it — and that has to be true BEFORE the
	// unlink rather than as a consequence of it, because the unlink is exactly
	// what fails in route A of workspace_home_migration_sentinel.go's header.
	// After this write a record left behind by a failed unlink costs the next
	// daemon start one unlink; before it, the same leftover cost this workspace
	// its freshly migrated home.
	//
	// After the extraction rather than before it, so no crash inside steps 1-4
	// can be mistaken for a completed migration. The residual over-reach window
	// is now the gap between the extraction returning and this rename committing.
	//
	// NOT fatal, unlike the two writes before it. Those guarded a destruction
	// that had not happened yet; this one guards nothing — the migration has
	// succeeded and the operator's home is complete and correct, and failing it
	// would report a disaster for a workspace that is fine. What a failure costs
	// is only what the leftover record then costs, which is what step 6 reports.
	windowClosed := true
	rec.Phase = workspaceHomeMigrationHomeRebuilt
	if err := writeWorkspaceHomeMigrationRecord(recordPath, rec); err != nil {
		windowClosed = false
		slog.Error("workspace home import: the migration finished but its in-progress record could not be updated to say so; if the record cannot be deleted either, the NEXT daemon start will discard this workspace's home",
			"workspace_slug", slug, "record", recordPath, "error", err)
	}

	// --- step 6: the migration is over; stop claiming otherwise --------------
	if err := removeWorkspaceHomeMigrationRecord(recordPath); err != nil {
		// Not fatal: the migration itself succeeded and the home is correct.
		// Reported anyway, because leftover daemon state is worth an operator's
		// attention even in the cheap case, and because in the expensive one the
		// consequence is delayed and surprising.
		res.MigrationRecordRemoveError = err.Error()
		res.MigrationRecordDiscardsHome = !windowClosed
		if windowClosed {
			slog.Warn("workspace home import: the migration completed but its in-progress record could not be removed; it no longer authorizes discarding this home, so the next daemon start will simply delete it",
				"workspace_slug", slug, "record", recordPath, "error", err)
		} else {
			slog.Error("workspace home import: the migration completed but its in-progress record could neither be closed nor removed; the NEXT daemon start will discard this workspace's home and re-run its init from scratch",
				"workspace_slug", slug, "record", recordPath, "error", err)
		}
	}

	slog.Info("workspace home imported (docs/plans/workspace-home-volume-persistence.md 論点 f)",
		"workspace_slug", slug, "home_volume", volumeName,
		"replaced_existing", res.PreviousExisted, "marker_removed", res.MarkerRemoved)
	return res, nil
}

// discardWorkspaceHomeMigrationRecord removes the in-progress record on a path
// where the record is ALREADY in a phase that authorizes nothing, and logs
// rather than returning when it cannot.
//
// Two of its callers are the failures that leave the record in phase "recorded"
// — step 0's own write and step 1's refusal — and the third is
// clearWorkspaceHomeMigrationRecordForAnAbsentHome, once it has moved the record
// into "home-absent". In all three, removing it is tidiness rather than safety: a
// record that survives costs the next daemon start or dispatch one unlink and
// nothing else (workspace_home_migration_sentinel.go's route B). It does not
// return an error because every caller is already returning one about something
// more important.
//
// A caller whose record is still in a DESTRUCTIVE phase must not use this
// directly — go through clearWorkspaceHomeMigrationRecordForAnAbsentHome, which
// is this operation with the phase moved first.
func discardWorkspaceHomeMigrationRecord(recordPath, slug string) {
	if err := removeWorkspaceHomeMigrationRecord(recordPath); err != nil {
		slog.Error("workspace home import: could not remove the in-progress migration record; it authorizes no destruction, so the next daemon start or dispatch simply deletes it",
			"workspace_slug", slug, "record", recordPath, "error", err)
	}
}

// clearWorkspaceHomeMigrationRecordForAnAbsentHome retires a record whose
// migration ended with the home volume GONE: it commits the non-destructive
// "home-absent" phase durably first, and only then unlinks (codex review of PR8
// round 5, Blocker).
//
// # Why the phase cannot ride on the unlink
//
// removeWorkspaceHomeMigrationRecord unlinks and fsyncs the directory
// afterwards, so its failure mode is the awkward one: the record is gone as far
// as this process and every dispatch after it can see, while nothing guarantees
// it stays gone. Unlinking a record that still says "home-destroyed" therefore
// costs a home on an entirely ordinary sequence, with no second bug and no crash
// during the migration itself:
//
//	partial volume removed, record still "home-destroyed"
//	unlink succeeds, its directory fsync returns EIO   -> logged, dropped; record invisible
//	the waiting dispatch sees no record                -> builds a CORRECT home from init.sh
//	power cut rolls the unlink back                    -> the durable "home-destroyed" is back
//	next start's recovery obeys the phase              -> that correct home is destroyed
//
// Moving the phase first makes the survivor harmless: whichever of the two
// writes the crash keeps, neither authorizes destroying anything.
//
// # When the phase write ITSELF fails
//
// The record is KEPT — deliberately not unlinked — because on storage that
// cannot flush that directory, the unlink would fail in the invisible direction
// above. The asymmetry decides it: a record that is VISIBLY there stops every
// dispatch of this workspace at Runner.recoverInterruptedWorkspaceHomeMigration,
// which resolves it before anything creates a volume, and against an absent
// volume that resolution is free — RemoveWorkspaceHomeVolume reports "not found"
// as success (containerBackend.RemoveWorkspaceHomeVolume's errdefs.IsNotFound
// arm), the marker removal tolerates ENOENT, and the record goes. A record that
// has been unlinked but not flushed is invisible for exactly as long as it takes
// a dispatch to build a good home, and then comes back.
//
// So the worst durable outcome of keeping it is a leftover "home-destroyed"
// record over a volume that does not exist, which is the cheap end of the trade
// this whole file makes everywhere else.
func clearWorkspaceHomeMigrationRecordForAnAbsentHome(recordPath, slug string, rec workspaceHomeMigrationRecord) {
	if err := markWorkspaceHomeMigrationRecordHomeAbsent(recordPath, slug, rec.Volume, rec); err != nil {
		slog.Error("workspace home import: the migration failed and left this workspace with no home volume at all, but could not durably record that; the record is kept rather than deleted, so the next daemon start or dispatch resolves it against a volume that no longer exists",
			"workspace_slug", slug, "record", recordPath, "error", err)
		return
	}
	discardWorkspaceHomeMigrationRecord(recordPath, slug)
}

// workspaceHomeImportCleanupTimeout bounds the removal that discards a
// half-extracted volume.
//
// A bound at all, rather than the caller's own deadline, because the caller's
// deadline is typically already blown by the time this runs — see
// discardPartialWorkspaceHome. A generous one, because the alternative to
// waiting is leaving the wreckage: 30s is far longer than a VolumeRemove takes
// against a responsive engine and far shorter than a daemon shutdown will wait,
// so it only fires when the engine is genuinely not answering, which is exactly
// the case the error message has to describe.
const workspaceHomeImportCleanupTimeout = 30 * time.Second

// discardPartialWorkspaceHome removes the home volume after a failed
// extraction and returns the error the migration reports.
//
// # Why the volume has to go (codex review of PR8, Major)
//
// ImportWorkspaceHome's contract for a mid-flight failure is that what is left
// behind is 「一度も dispatch していない workspace と同じ状態」, and the next
// dispatch prepares it from scratch. A workspace that has never been dispatched
// into has no home volume at all. An extraction that died partway leaves one
// holding whatever crossed before the stream stopped — no marker, so init.sh
// does run, but it runs against a populated home rather than an empty one, and
// that difference is the whole problem:
//
// init scripts are contractually idempotent
// (docs/plans/home-workspace-volume.md 「script 作者が守ること」), and the ordinary
// way to write one is to probe for what it installs and return early —
// `command -v claude`, `[ -x "$HOME/.local/bin/claude" ]`. A partial extraction
// that delivered `.local/bin/claude` and not its payload answers that probe
// YES. init.sh exits 0, resolveWorkspaceHome writes a completion marker for it,
// and from then on the marker MATCHES on every dispatch: nothing re-runs, and
// the breakage surfaces much later and much further away, as the adapter's "CLI
// not found in workspace $HOME". Removing the volume converts that permanent,
// misattributed failure into one more init run.
//
// # Why not the caller's context
//
// The commonest route to a failed extraction is the operator's CLI going away
// mid-upload (^C, a dropped connection, a laptop lid) — which cancels the HTTP
// request context, which IS ctx here. Reusing it would make the cleanup fail
// precisely in the case it exists for, so cancellation is dropped
// (context.WithoutCancel keeps the values, e.g. tracing, and loses only the
// deadline) and a bound of its own applies.
//
// # Why a failed cleanup is reported rather than logged
//
// Because the operator's next action differs. "The migration failed, re-run it"
// is right when the volume went; when it did not, re-running extracts into
// whatever is already there, and the half-extracted volume has to be removed by
// hand first. A warning in the daemon log is not where somebody reading a
// failed CLI command looks, so both facts and the manual repair go into the
// error the CLI prints.
//
// # The boolean return
//
// It says whether the volume actually went, which is what the caller needs to
// decide the fate of the in-progress migration record: a discarded volume leaves
// nothing for a later daemon start to sweep, an undiscarded one leaves exactly
// the wreckage that sweep exists for (workspace_home_migration_sentinel.go).
func discardPartialWorkspaceHome(ctx context.Context, importer WorkspaceHomeImporter, slug, volumeName string, cause error) (discarded bool, err error) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workspaceHomeImportCleanupTimeout)
	defer cancel()

	if _, rmErr := importer.RemoveWorkspaceHomeVolume(cleanupCtx, volumeName); rmErr != nil {
		slog.Error("workspace home import: the extraction failed AND the half-extracted volume could not be removed; the next daemon start's migration sweep will retry it",
			"workspace_slug", slug, "home_volume", volumeName, "extract_error", cause, "remove_error", rmErr)
		return false, fmt.Errorf(
			"workspace home %[1]q: extract into %[2]q: %[3]w — and the half-extracted volume could NOT be discarded: %[4]w. "+
				"[復旧] その volume を手で削除してから移行をやり直すこと (`docker volume rm %[2]s`)。 削除せずに再実行すると "+
				"途中まで展開された中身の上に展開することになり、次の dispatch の init.sh も「もう入っている」と誤判定しうる。 "+
				"移行元のディレクトリは読んだだけなので、失われたものは無い",
			slug, volumeName, cause, rmErr)
	}

	slog.Warn("workspace home import: extraction failed; the half-extracted volume was discarded so the next dispatch starts from an empty home",
		"workspace_slug", slug, "home_volume", volumeName, "error", cause)
	return true, fmt.Errorf(
		"workspace home %q: extract into %q: %w "+
			"(the half-extracted volume was removed, so this workspace is now in the state it would be in before its "+
			"first dispatch and the next dispatch initializes it from scratch; the legacy source directory was not "+
			"modified, so the migration can simply be re-run)",
		slug, volumeName, cause)
}

// workspaceHomeImporterFor resolves the backend's migration capability,
// failing loud when it has none — the same shape, and the same reasoning, as
// workspaceInitExecutorFor. Degrading here would mean reporting a migration
// that never happened, against a home the caller believes now holds their
// credentials.
func workspaceHomeImporterFor(r *Runner) (WorkspaceHomeImporter, error) {
	importer, ok := r.Backend.(WorkspaceHomeImporter)
	if !ok {
		return nil, fmt.Errorf(
			"the configured sandbox backend (%T) cannot import a workspace home; the migration replaces the home's "+
				"docker volume and extracts a tar stream into a throwaway container, which has no host-side fallback "+
				"(PR8 of docs/plans/workspace-home-volume-persistence.md, 論点 f)",
			r.Backend)
	}
	return importer, nil
}

// workspaceHomeTarBlockSize is tar's fixed block size, and the exact amount of
// the request body peekWorkspaceHomeTarHasEntries has to look at.
//
// Every tar variant — v7, ustar, PAX, GNU — begins the stream with one 512-byte
// header block for the first member, and an archive with no members is nothing
// but zero blocks (archive/tar's Close writes two of them). So "is there a
// non-zero byte in the first block" is the whole question, and it is answerable
// without a tar parser and without a bound larger than one block.
const workspaceHomeTarBlockSize = 512

// ErrWorkspaceHomeImportEmpty is what a migration refused because its archive
// would replace the home with nothing comes back as.
//
// A sentinel because the API layer answers it with a 400 rather than the 409 the
// two busy errors get: nothing is in flight, nothing needs waiting for, and the
// thing to fix is the request. See peekWorkspaceHomeTarHasEntries for why this
// is checked at all, and WorkspaceHomeSourceHasEntries for the CLI-side half.
var ErrWorkspaceHomeImportEmpty = errors.New("the workspace home archive carries no entries")

// peekWorkspaceHomeTarHasEntries reports whether stream begins with a tar member,
// and returns a reader that still yields every byte it consumed.
//
// # Why the daemon checks this at all (codex round 3, Blocker 2)
//
// This function is the ONLY guard that sits on the same side of the wire as the
// deletion it protects. ImportWorkspaceHome removes the home volume before it
// reads a single byte of the body — it has to, because that removal is also the
// in-use check — so an archive with no members is a complete, successful,
// 200-answering way to erase a workspace's harness credentials. The CLI refuses
// to send one (WorkspaceHomeSourceHasEntries), and that covers an operator
// running `boid workspace import-home`; it covers nothing about an older CLI, a
// hand-written curl, or the next bug on the producing side.
//
// # Why a peek rather than a parse
//
// The body is a multi-GB stream that must never be materialized, so the check
// has to be O(1) and has to hand the stream on intact. One block is enough (see
// workspaceHomeTarBlockSize), and io.MultiReader replays it, so the extraction
// container still receives the archive from byte zero — the property
// TestRunner_ImportWorkspaceHome_StillMigratesTheFirstEntry exists to pin, since
// a peek that ATE the first header would silently drop the first file of every
// migration.
//
// A malformed non-tar body is deliberately NOT this function's business: it has
// a non-zero first block, so it passes, and tar in the extraction container
// rejects it. That failure is loud and already handled
// (discardPartialWorkspaceHome). The one outcome this exists to prevent is the
// SILENT one.
func peekWorkspaceHomeTarHasEntries(stream io.Reader) (replayed io.Reader, hasEntries bool, err error) {
	head := make([]byte, workspaceHomeTarBlockSize)
	n, err := io.ReadFull(stream, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		// A transport failure this early is reported rather than read as "empty":
		// treating it as empty would refuse with the wrong reason, and treating it
		// as non-empty would destroy the home on a connection that already broke.
		return nil, false, err
	}
	head = head[:n]
	for _, b := range head {
		if b != 0 {
			hasEntries = true
			break
		}
	}
	return io.MultiReader(bytes.NewReader(head), stream), hasEntries, nil
}
