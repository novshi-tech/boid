package dispatcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file covers what happens to a workspace home migration that is never
// finished OR cleaned up, because the daemon process stopped existing partway
// through (codex review of PR8 round 2, Major 2).
//
// # Why the round-1 cleanup is not enough on its own
//
// discardPartialWorkspaceHome runs when ImportWorkspaceHome RETURNS an error.
// SIGKILL, an OOM kill, `docker compose down -t 0`, a power cut — none of those
// produce a return value, so neither the defers nor the cleanup run. What is
// left on disk is a home volume holding whatever crossed before the process
// died, with its completion marker already deleted, which is the precise state
// PR8 round 1 established as WORSE than either neighbour: init.sh does run
// against it, an idempotent init.sh that probes for what it installs
// (`command -v claude`, `[ -x ~/.local/bin/claude ]`) concludes the toolchain is
// present, exits 0, and the daemon writes a fresh completion marker over a
// broken home. From then on nothing re-runs and the breakage surfaces much later
// as the adapter's "CLI not found in workspace $HOME".
//
// The startup orphan sweep does not cover it either: it reaps CONTAINERS by
// label (reapOrphanWorkspaceInitContainers), and the extraction container is not
// what survives — the VOLUME is.
//
// # Why the record is a file rather than a label on the volume
//
// The obvious alternative is to mark the volume itself "migration in progress"
// and clear the mark on success, which would make the record and the thing it
// describes share a lifetime. The Engine API cannot do it: there is no way to
// add, change or remove a label on an EXISTING volume (VolumeCreate on an
// existing name returns it untouched — the property
// Runner.ensureWorkspaceHomeVolume's fast path is built on, and the same reason
// an unlabelled volume is a hard error there rather than something a re-init can
// repair). Encoding the state in the volume's NAME instead would mean the
// migration wrote into a volume no dispatch mounts, which defeats the point.
//
// So the record goes where the daemon's other bookkeeping about workspace homes
// already lives: <dataHome>/homes-meta/, beside the completion marker and the
// init flock, inside the daemon's own persistent volume.

func workspaceHomeMigrationRecordPathFor(t *testing.T, r *Runner, slug string) string {
	t.Helper()
	metaDir, err := r.workspaceHomeMetaDir()
	if err != nil {
		t.Fatalf("workspaceHomeMetaDir: %v", err)
	}
	return workspaceHomeMigrationRecordPath(metaDir, slug)
}

// snapshotDir captures every regular file under dir. restoreDir puts a snapshot
// back, deleting anything that is not in it.
//
// Together they reconstruct HALF of what a SIGKILL mid-migration leaves behind,
// and the half they reconstruct is the half worth measuring rather than
// asserting: run the real migration, let it reach the extraction, capture
// <dataHome>/homes-meta/ AT THAT INSTANT, then — after the migration has unwound
// normally, because a test cannot skip Go's defers — put that directory back.
// The daemon's own bookkeeping is then byte for byte what a killed daemon would
// have left, so these tests cannot drift away from the ORDER the implementation
// actually writes in (record present, completion marker already gone). That
// ordering is the entire contract workspace_home_migration_sentinel.go adds, and
// it is what the snapshot pins.
//
// What is NOT measured, and must not be described as if it were (codex review of
// PR8 round 3, Minor): the VOLUME's contents. bashWorkspaceInitBackend.onImport
// fires at the top of ImportWorkspaceHome, before any byte is extracted, so at
// snapshot time the volume holds nothing; the half-extracted residue the tests
// below plant in it afterwards is the test's own model of "a stream that stopped
// partway", not a recording of one. Nothing here can produce a genuine one,
// because the interruption being modelled is a process that stops existing
// mid-tar and Go's test binary cannot do that to itself. The residue's SHAPE is
// what matters and it is chosen deliberately — `.local/bin/claude`, the file an
// inventory-checking init.sh probes for — so the model is honest about the risk
// even though it is not a measurement of it.
func snapshotDir(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	snap := make(map[string][]byte, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		snap[e.Name()] = data
	}
	return snap
}

func restoreDir(t *testing.T, dir string, snap map[string][]byte) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if _, keep := snap[e.Name()]; !keep && !e.IsDir() {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				t.Fatalf("remove %s: %v", e.Name(), err)
			}
		}
	}
	for name, data := range snap {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatalf("restore %s: %v", name, err)
		}
	}
}

// TestRunner_ImportWorkspaceHome_RecordsTheMigrationBeforeItDestroysAnything is
// the ordering half: at the moment the extraction BEGINS — the old volume
// already destroyed, its replacement about to be written into, and every instant
// from here until the extraction finishes unrecoverable by anything in this
// process — the daemon's own persistent area must already say so.
//
// Both assertions inside the gate matter. The record has to be there, and the
// completion marker has to be GONE, because those two together are what tell the
// next boot "the volume under this name is not a home anybody vouched for".
func TestRunner_ImportWorkspaceHome_RecordsTheMigrationBeforeItDestroysAnything(t *testing.T) {
	r, be, _ := settledWorkspaceHome(t, "myws")
	recordPath := workspaceHomeMigrationRecordPathFor(t, r, "myws")
	markerPath := workspaceHomeMarkerPathFor(t, r, "myws")

	var recordExists, markerExists bool
	be.onImport = func() {
		_, rerr := os.Stat(recordPath)
		recordExists = rerr == nil
		_, merr := os.Stat(markerPath)
		markerExists = merr == nil
	}

	if _, err := r.ImportWorkspaceHome(context.Background(), "myws",
		bytes.NewReader(importTestTar(t, map[string]os.FileMode{"a": 0o644}))); err != nil {
		t.Fatalf("ImportWorkspaceHome: %v", err)
	}

	if !recordExists {
		t.Errorf("no in-progress record at %s while the extraction was running: a daemon killed at that instant leaves a "+
			"half-extracted home volume with its completion marker already deleted, and nothing on the next boot can tell "+
			"that home apart from a finished one — the next dispatch runs init.sh against it and, if init.sh checks its own "+
			"inventory, writes a fresh marker over the wreckage", recordPath)
	}
	if markerExists {
		t.Error("the completion marker still existed during the extraction: it must be deleted with the volume, not at the end")
	}

	// And the record is not left behind by a migration that finished.
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Errorf("the in-progress record survived a successful migration (stat err = %v): every later daemon start would "+
			"destroy this workspace's home", err)
	}
}

// TestRunner_ImportWorkspaceHome_KeepsNoRecordAfterASideEffectFreeRefusal is the
// other end of the same requirement, and the one that keeps the mechanism from
// being destructive on its own.
//
// A migration refused at step 1 (the engine's 409 for a volume a running job
// holds) has changed nothing — that is the whole reason the removal goes first.
// A record left behind by such a refusal would make the NEXT daemon start delete
// an intact, untouched home.
func TestRunner_ImportWorkspaceHome_KeepsNoRecordAfterASideEffectFreeRefusal(t *testing.T) {
	r, be, _ := settledWorkspaceHome(t, "myws")
	be.removeVolumeErr = ErrWorkspaceHomeInUse

	if _, err := r.ImportWorkspaceHome(context.Background(), "myws",
		bytes.NewReader(importTestTar(t, map[string]os.FileMode{"a": 0o644}))); !errors.Is(err, ErrWorkspaceHomeInUse) {
		t.Fatalf("error = %v, want ErrWorkspaceHomeInUse", err)
	}

	recordPath := workspaceHomeMigrationRecordPathFor(t, r, "myws")
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Errorf("an in-progress record survived a refusal that destroyed nothing (stat err = %v): the next daemon start "+
			"would delete this workspace's intact home", err)
	}
}

// TestRunner_SweepInterruptedWorkspaceHomeMigrations_DiscardsAKilledMigrationsHome
// is the recovery.
//
// The daemon's BOOKKEEPING is not hand-written: it is captured from a real
// migration at the instant the extraction begins (see snapshotDir) and put back
// afterwards, so the test cannot drift away from what the implementation writes
// or from the order it writes it in. The volume's contents ARE hand-written —
// snapshotDir's doc comment says why that half cannot be measured here, and what
// the residue is chosen to model.
//
// The sweep then runs on a FRESH Runner — the daemon that just came back up,
// with no in-memory knowledge of anything.
func TestRunner_SweepInterruptedWorkspaceHomeMigrations_DiscardsAKilledMigrationsHome(t *testing.T) {
	r, be, volume := settledWorkspaceHome(t, "myws")
	metaDir, err := r.workspaceHomeMetaDir()
	if err != nil {
		t.Fatalf("workspaceHomeMetaDir: %v", err)
	}

	var killState map[string][]byte
	be.onImport = func() { killState = snapshotDir(t, metaDir) }
	if _, err := r.ImportWorkspaceHome(context.Background(), "myws",
		bytes.NewReader(importTestTar(t, map[string]os.FileMode{"a": 0o644}))); err != nil {
		t.Fatalf("ImportWorkspaceHome: %v", err)
	}
	if killState == nil {
		t.Fatal("test wiring: the extraction hook never ran")
	}

	// Reconstruct the kill: the daemon's bookkeeping as of mid-extraction, plus
	// a volume holding the residue that fools an inventory-checking init.sh.
	restoreDir(t, metaDir, killState)
	partial := filepath.Join(be.dirFor(volume), ".local", "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(partial), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(partial, []byte("half-extracted\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The daemon comes back up.
	rebooted := &Runner{Backend: be}
	swept, err := rebooted.SweepInterruptedWorkspaceHomeMigrations(context.Background())
	if err != nil {
		t.Fatalf("SweepInterruptedWorkspaceHomeMigrations: %v", err)
	}
	if swept != 1 {
		t.Errorf("swept %d interrupted migrations, want 1", swept)
	}

	if be.volumeExists(volume) {
		t.Errorf("the home volume %q survived the sweep, still holding a half-extracted home: the next dispatch runs "+
			"init.sh against it, and an init.sh that probes for what it installs concludes the toolchain is already "+
			"there and freezes the breakage behind a fresh completion marker", volume)
	}
	if _, err := os.Stat(workspaceHomeMigrationRecordPathFor(t, rebooted, "myws")); !os.IsNotExist(err) {
		t.Errorf("the in-progress record survived a successful sweep (stat err = %v): every later boot would repeat it", err)
	}
	if _, err := os.Stat(workspaceHomeMarkerPathFor(t, rebooted, "myws")); !os.IsNotExist(err) {
		t.Errorf("the completion marker survived the sweep (stat err = %v)", err)
	}

	// End to end: the next dispatch finds a workspace in the state it would be
	// in before its very first one.
	before := be.runCount()
	if _, _, _, err := rebooted.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("resolveWorkspaceHome after the sweep: %v", err)
	}
	if be.runCount() != before+1 {
		t.Errorf("init ran %d times after the sweep, want one more than the %d before it", be.runCount(), before)
	}
	if _, err := os.Lstat(partial); !os.IsNotExist(err) {
		t.Errorf("the half-extracted residue survived into the re-initialized home (stat err = %v)", err)
	}
}

// TestRunner_SweepInterruptedWorkspaceHomeMigrations_LeavesFinishedWorkspacesAlone
// is the vacuity guard. A sweep that removed home volumes unconditionally would
// pass every assertion above and destroy every workspace on the installation at
// each daemon start.
func TestRunner_SweepInterruptedWorkspaceHomeMigrations_LeavesFinishedWorkspacesAlone(t *testing.T) {
	r, be, volume := settledWorkspaceHome(t, "myws")
	identityBefore := be.identityOf(t, volume)
	markerBefore, err := os.ReadFile(workspaceHomeMarkerPathFor(t, r, "myws"))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}

	rebooted := &Runner{Backend: be}
	swept, err := rebooted.SweepInterruptedWorkspaceHomeMigrations(context.Background())
	if err != nil {
		t.Fatalf("SweepInterruptedWorkspaceHomeMigrations: %v", err)
	}
	if swept != 0 {
		t.Errorf("swept %d workspaces with no interrupted migration, want 0", swept)
	}
	if !be.volumeExists(volume) {
		t.Fatal("the sweep destroyed the home of a workspace that had no migration in flight")
	}
	if got := be.identityOf(t, volume); got != identityBefore {
		t.Errorf("the home volume identity changed to %q", got)
	}
	markerAfter, err := os.ReadFile(workspaceHomeMarkerPathFor(t, r, "myws"))
	if err != nil {
		t.Fatalf("the completion marker was removed by a no-op sweep: %v", err)
	}
	if !bytes.Equal(markerBefore, markerAfter) {
		t.Error("the completion marker was rewritten by a no-op sweep")
	}
	if be.removeCount() != 0 {
		t.Errorf("the sweep issued %d volume removals with nothing to sweep", be.removeCount())
	}
}

// TestRunner_ImportWorkspaceHome_KeepsTheRecordWhenTheDiscardFails is the hand-off
// between the two mechanisms.
//
// The in-process cleanup is the fast path and the record is the backstop; the
// case that needs them to agree is the one where the cleanup RAN and could not
// finish (the engine refused the removal). The migration already reports that to
// the operator and tells them to remove the volume by hand — but if they do not,
// the next daemon start has to finish the job rather than treat the workspace as
// settled. So the record stays.
func TestRunner_ImportWorkspaceHome_KeepsTheRecordWhenTheDiscardFails(t *testing.T) {
	r, be, volume := settledWorkspaceHome(t, "myws")
	be.importErr = errors.New("tar: unexpected EOF")
	be.importPartial = []string{".local/bin/claude"}
	// Fail the SECOND removal only — the discard — so the migration gets far
	// enough to have something to discard.
	be.removeVolumeErr = errors.New("engine unreachable")
	be.removeVolumeErrFromCall = 2

	if _, err := r.ImportWorkspaceHome(context.Background(), "myws", bytes.NewReader([]byte("truncated"))); err == nil {
		t.Fatal("ImportWorkspaceHome returned nil for a failed extraction")
	}

	recordPath := workspaceHomeMigrationRecordPathFor(t, r, "myws")
	if _, err := os.Stat(recordPath); err != nil {
		t.Fatalf("the in-progress record was removed even though the half-extracted volume could NOT be discarded "+
			"(stat err = %v): the next daemon start then treats a broken home as a finished one", err)
	}

	// And the next boot finishes it.
	be.removeVolumeErr = nil
	rebooted := &Runner{Backend: be}
	if _, err := rebooted.SweepInterruptedWorkspaceHomeMigrations(context.Background()); err != nil {
		t.Fatalf("SweepInterruptedWorkspaceHomeMigrations: %v", err)
	}
	if be.volumeExists(volume) {
		t.Errorf("the half-extracted volume %q survived the next boot's sweep", volume)
	}
}

// killedMigrationResidue reconstructs the on-disk state a SIGKILL inside a
// migration leaves, and returns the path of the residue it planted in the
// volume.
//
// The daemon's bookkeeping half is captured from the implementation; the
// volume's contents are the test's own model. See snapshotDir for which is
// which and why.
func killedMigrationResidue(t *testing.T, r *Runner, be *bashWorkspaceInitBackend, slug, volume string) string {
	t.Helper()
	metaDir, err := r.workspaceHomeMetaDir()
	if err != nil {
		t.Fatalf("workspaceHomeMetaDir: %v", err)
	}

	var killState map[string][]byte
	be.onImport = func() { killState = snapshotDir(t, metaDir) }
	if _, err := r.ImportWorkspaceHome(context.Background(), slug,
		bytes.NewReader(importTestTar(t, map[string]os.FileMode{"a": 0o644}))); err != nil {
		t.Fatalf("ImportWorkspaceHome: %v", err)
	}
	if killState == nil {
		t.Fatal("test wiring: the extraction hook never ran")
	}
	be.onImport = nil
	restoreDir(t, metaDir, killState)

	// The captured record has to be the one that AUTHORIZES a discard, or the
	// tests built on this helper would be asserting round 3's behaviour against a
	// state round 4 no longer produces — green, and about nothing.
	var captured workspaceHomeMigrationRecord
	if err := json.Unmarshal(killState[filepath.Base(workspaceHomeMigrationRecordPath(metaDir, slug))], &captured); err != nil {
		t.Fatalf("parse the record captured mid-extraction: %v", err)
	}
	if !captured.discardsHome() {
		t.Fatalf("the record the migration held while extracting is in phase %q, which does not authorize a discard: the "+
			"volume under this name really is half-written at that instant, and recovery has to be allowed to remove it",
			captured.Phase)
	}

	partial := filepath.Join(be.dirFor(volume), ".local", "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(partial), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(partial, []byte("half-extracted\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	return partial
}

// TestRunner_ResolveWorkspaceHome_RepairsWreckageASweepCouldNotRemove is codex
// round 3's Blocker 1, and it is the failure mode the startup sweep ALONE cannot
// cover.
//
// The sweep releases its in-flight registration on the way out whether or not it
// managed to remove the volume, and the daemon logs that failure and carries on
// into auto-reopen. So a boot in which the engine is briefly unreachable —
// exactly the boot that follows the crash which produced the wreckage — leaves
// this state: a record on disk, a half-extracted volume, an empty registry, and
// nothing between them and the next dispatch.
//
// That dispatch is the dangerous one. It waits for nothing, mounts the partial
// home and runs init.sh against it; an init.sh that probes for what it installs
// (`command -v claude`, `[ -x ~/.local/bin/claude ]` — the contractually
// idempotent shape) answers "already installed", exits 0, and the daemon writes
// a fresh completion marker over the wreckage. From that point the marker matches
// forever and the breakage surfaces much later as the adapter's "CLI not found in
// workspace $HOME".
//
// So the record has to be consulted on the dispatch path too, and — since the
// engine is typically back by then — the dispatch repairs the state itself.
func TestRunner_ResolveWorkspaceHome_RepairsWreckageASweepCouldNotRemove(t *testing.T) {
	r, be, volume := settledWorkspaceHome(t, "myws")
	partial := killedMigrationResidue(t, r, be, "myws", volume)

	// The daemon comes back up while the engine is unreachable: the sweep runs,
	// fails, logs, and startup continues.
	rebooted := &Runner{Backend: be}
	be.removeVolumeErr = errors.New("engine unreachable")
	if _, err := rebooted.SweepInterruptedWorkspaceHomeMigrations(context.Background()); err == nil {
		t.Fatal("test wiring: the sweep was supposed to fail")
	}
	// ... and then the engine recovers, which is what makes self-repair the right
	// answer rather than a permanent refusal.
	be.removeVolumeErr = nil

	before := be.runCount()
	if _, _, _, err := rebooted.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("resolveWorkspaceHome after a failed sweep: %v", err)
	}
	if be.runCount() != before+1 {
		t.Errorf("init ran %d times, want one more than the %d before the dispatch", be.runCount(), before)
	}
	if _, err := os.Lstat(partial); !os.IsNotExist(err) {
		t.Errorf("init.sh ran against the half-extracted home an interrupted migration left behind (stat err = %v): "+
			"an init.sh that probes for what it installs concludes the toolchain is already there, exits 0, and the "+
			"daemon freezes the breakage behind a fresh completion marker", err)
	}
	if _, err := os.Stat(workspaceHomeMigrationRecordPathFor(t, rebooted, "myws")); !os.IsNotExist(err) {
		t.Errorf("the in-progress record survived the repair (stat err = %v): every later dispatch would repeat it", err)
	}

	// And the workspace settles again rather than re-initializing forever.
	settled := be.runCount()
	if _, _, _, err := rebooted.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("resolveWorkspaceHome (settling): %v", err)
	}
	if be.runCount() != settled {
		t.Errorf("init ran %d times, want %d: the repair must re-arm init ONCE, not permanently", be.runCount(), settled)
	}
}

// TestRunner_ResolveWorkspaceHome_RefusesWreckageItCannotRemove is the other half
// of Blocker 1: when the engine is STILL not answering, there is no safe way to
// proceed, so the dispatch fails rather than running init.sh over the wreckage.
//
// Failing a job is a real cost (it is why beginDispatch waits rather than refuses
// elsewhere), and it is the cheaper of the two outcomes here: a failed job is
// visible on the task and re-runnable, while a completion marker written over a
// half-extracted home is permanent and surfaces somewhere else entirely.
func TestRunner_ResolveWorkspaceHome_RefusesWreckageItCannotRemove(t *testing.T) {
	r, be, volume := settledWorkspaceHome(t, "myws")
	partial := killedMigrationResidue(t, r, be, "myws", volume)

	rebooted := &Runner{Backend: be}
	be.removeVolumeErr = errors.New("engine unreachable")
	if _, err := rebooted.SweepInterruptedWorkspaceHomeMigrations(context.Background()); err == nil {
		t.Fatal("test wiring: the sweep was supposed to fail")
	}

	before := be.runCount()
	_, _, _, err := rebooted.resolveWorkspaceHome(context.Background(), "myws")
	if err == nil {
		t.Fatal("a dispatch was allowed to proceed against the half-extracted home of an interrupted migration")
	}
	msg := err.Error()
	if !strings.Contains(msg, volume) {
		t.Errorf("error %q does not name the volume the operator has to remove by hand", msg)
	}
	if !strings.Contains(msg, "docker volume rm") {
		t.Errorf("error %q does not say what an operator can DO about it", msg)
	}
	// The true cause — an interrupted migration — has to be readable, or the
	// operator debugs the job instead of the leftover.
	if !strings.Contains(msg, "import-home") && !strings.Contains(msg, "中断") {
		t.Errorf("error %q does not attribute the failure to an interrupted `boid workspace import-home`", msg)
	}

	if be.runCount() != before {
		t.Errorf("init ran %d times despite the refusal (was %d)", be.runCount(), before)
	}
	if _, err := os.Lstat(partial); err != nil {
		t.Errorf("the wreckage was disturbed by a refusal that should have changed nothing: %v", err)
	}
}

// failEveryDirSync makes every parent-directory fsync in this package fail, and
// restores the real one afterwards.
//
// A seam rather than a real failure because there is no portable way to make
// fsync(2) on a healthy directory return an error, and the branch it guards is
// the one that only matters when the storage is failing. Both callers of the
// homes-meta writer go through it, which is what lets the two tests below assert
// that they answer a broken fsync DIFFERENTLY.
func failEveryDirSync(t *testing.T, err error) {
	t.Helper()
	prev := fsyncDir
	fsyncDir = func(string) error { return err }
	t.Cleanup(func() { fsyncDir = prev })
}

// TestWriteWorkspaceHomeMigrationRecord_FailsClosedWhenItCannotBeMadeDurable is
// codex round 3's Major.
//
// The record's ONLY job is to survive the machine going away. A rename that is
// still only in the page cache when the power cuts records nothing — so a daemon
// that could not fsync the directory and reported success would go on to destroy
// the home volume while holding a sentinel that may not exist after a crash,
// which is exactly the state (partial volume, no record, no marker) the whole
// mechanism was built to make impossible.
func TestWriteWorkspaceHomeMigrationRecord_FailsClosedWhenItCannotBeMadeDurable(t *testing.T) {
	failEveryDirSync(t, errors.New("input/output error"))

	path := filepath.Join(t.TempDir(), "myws"+workspaceHomeMigrationRecordSuffix)
	err := writeWorkspaceHomeMigrationRecord(path, workspaceHomeMigrationRecord{Slug: "myws", Volume: "vol"})
	if err == nil {
		t.Fatal("writeWorkspaceHomeMigrationRecord reported success although the directory could not be synced: the " +
			"migration then destroys the home volume while its sentinel may not survive a power cut, and the next boot " +
			"finds a half-extracted home nothing vouches against")
	}
	if !strings.Contains(err.Error(), "input/output error") {
		t.Errorf("error %q loses the underlying failure", err)
	}
}

// TestWriteWorkspaceHomeMarker_ToleratesADirectorySyncFailure pins the OTHER
// side of that decision, because the two files share one writer and it would be
// easy to make them share one policy too.
//
// The marker needs ATOMICITY, not durability. Losing it costs one extra run of a
// contractually idempotent init.sh — the fail-safe direction this whole package
// takes for every uncertain marker — while failing the write hard would turn a
// storage hiccup into a failed dispatch for the workspace, which is strictly
// worse than the thing it would be protecting against.
func TestWriteWorkspaceHomeMarker_ToleratesADirectorySyncFailure(t *testing.T) {
	failEveryDirSync(t, errors.New("input/output error"))

	dir := t.TempDir()
	path := workspaceHomeMarkerPath(dir, "myws")
	if err := writeWorkspaceHomeMarker(path, workspaceHomeMarker{ScriptSHA256: "abc", HomeID: "id"}); err != nil {
		t.Fatalf("writeWorkspaceHomeMarker failed on an unsyncable directory: %v — a lost marker re-runs an idempotent "+
			"init, so this must not fail a dispatch", err)
	}
	marker, ok, err := readWorkspaceHomeMarker(path)
	if err != nil || !ok || marker.HomeID != "id" {
		t.Errorf("marker = %+v, ok = %v, err = %v; want the marker to have been written anyway", marker, ok, err)
	}
}

// TestRemoveWorkspaceHomeMigrationRecord_MakesTheDeletionDurable is the other
// end of the same invariant, and the direction that DESTROYS a good home when it
// is missing.
//
// A record whose unlink is lost to a power cut comes back, and a record that is
// back means the next daemon start discards a home that was migrated perfectly —
// several gigabytes and an operator's afternoon. The unlink therefore has to be
// followed by a directory fsync, on the same footing as the write.
func TestRemoveWorkspaceHomeMigrationRecord_MakesTheDeletionDurable(t *testing.T) {
	dir := t.TempDir()
	path := workspaceHomeMigrationRecordPath(dir, "myws")
	if err := writeWorkspaceHomeMigrationRecord(path, workspaceHomeMigrationRecord{Slug: "myws"}); err != nil {
		t.Fatalf("writeWorkspaceHomeMigrationRecord: %v", err)
	}

	var synced []string
	prev := fsyncDir
	fsyncDir = func(d string) error { synced = append(synced, d); return prev(d) }
	t.Cleanup(func() { fsyncDir = prev })

	if err := removeWorkspaceHomeMigrationRecord(path); err != nil {
		t.Fatalf("removeWorkspaceHomeMigrationRecord: %v", err)
	}
	if len(synced) == 0 {
		t.Fatal("the record was unlinked without syncing its parent directory: a power cut can bring the record back, and " +
			"the next daemon start then discards the home a completed migration had just filled")
	}

	// A sync that FAILS is reported, not swallowed: the caller
	// (ImportWorkspaceHome) surfaces it as MigrationRecordRemoveError so the
	// operator can delete the file before restarting.
	failEveryDirSync(t, errors.New("input/output error"))
	if err := writeWorkspaceHomeMigrationRecordForTest(t, path); err != nil {
		t.Fatalf("re-create the record: %v", err)
	}
	if err := removeWorkspaceHomeMigrationRecord(path); err == nil {
		t.Error("a record removal whose directory sync failed reported success")
	}

	// "Already gone" stays a success, and does not need a sync at all.
	failEveryDirSync(t, errors.New("input/output error"))
	if err := removeWorkspaceHomeMigrationRecord(filepath.Join(dir, "absent"+workspaceHomeMigrationRecordSuffix)); err != nil {
		t.Errorf("removing a record that is not there = %v, want nil", err)
	}
}

// writeWorkspaceHomeMigrationRecordForTest writes a record while the durable
// writer is stubbed out, so a test can re-create one under failEveryDirSync.
func writeWorkspaceHomeMigrationRecordForTest(t *testing.T, path string) error {
	t.Helper()
	return os.WriteFile(path, []byte(`{"slug":"myws"}`), 0o600)
}

// TestRunner_ImportWorkspaceHome_DestroysNothingWhenTheRecordIsNotDurable is
// the Major stated as behaviour rather than as a unit contract.
//
// The record is written BEFORE the first destructive step precisely so that the
// failure has somewhere safe to land. If the write cannot be made durable, the
// right answer is to stop there — the operator still has exactly the workspace
// they had, and re-running once the storage is healthy costs them nothing.
func TestRunner_ImportWorkspaceHome_DestroysNothingWhenTheRecordIsNotDurable(t *testing.T) {
	r, be, volume := settledWorkspaceHome(t, "myws")
	identityBefore := be.identityOf(t, volume)
	markerBefore, err := os.ReadFile(workspaceHomeMarkerPathFor(t, r, "myws"))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}

	failEveryDirSync(t, errors.New("input/output error"))

	if _, err := r.ImportWorkspaceHome(context.Background(), "myws",
		bytes.NewReader(importTestTar(t, map[string]os.FileMode{"a": 0o644}))); err == nil {
		t.Fatal("the migration proceeded although its in-progress record could not be made durable")
	}

	if be.removeCount() != 0 {
		t.Errorf("the home volume was removed %d times after the sentinel write failed", be.removeCount())
	}
	if got := be.identityOf(t, volume); got != identityBefore {
		t.Errorf("the home volume identity changed to %q", got)
	}
	markerAfter, err := os.ReadFile(workspaceHomeMarkerPathFor(t, r, "myws"))
	if err != nil {
		t.Fatalf("the completion marker was removed: %v", err)
	}
	if !bytes.Equal(markerBefore, markerAfter) {
		t.Error("the completion marker was rewritten")
	}

	// And the half-written record does not survive to bite later: it describes a
	// migration that never began, so a daemon start that believed it would
	// discard a home nothing was wrong with.
	if _, err := os.Stat(workspaceHomeMigrationRecordPathFor(t, r, "myws")); !os.IsNotExist(err) {
		t.Errorf("the in-progress record survived a migration that was refused before it destroyed anything "+
			"(stat err = %v)", err)
	}
}

// TestRunner_SweepInterruptedWorkspaceHomeMigrations_KeepsTheRecordWhenTheEngineRefuses
// keeps a transient engine failure from silently converting "this home is
// wreckage" into "this home is fine".
func TestRunner_SweepInterruptedWorkspaceHomeMigrations_KeepsTheRecordWhenTheEngineRefuses(t *testing.T) {
	r, be, volume := settledWorkspaceHome(t, "myws")
	metaDir, err := r.workspaceHomeMetaDir()
	if err != nil {
		t.Fatalf("workspaceHomeMetaDir: %v", err)
	}
	var killState map[string][]byte
	be.onImport = func() { killState = snapshotDir(t, metaDir) }
	if _, err := r.ImportWorkspaceHome(context.Background(), "myws",
		bytes.NewReader(importTestTar(t, map[string]os.FileMode{"a": 0o644}))); err != nil {
		t.Fatalf("ImportWorkspaceHome: %v", err)
	}
	restoreDir(t, metaDir, killState)

	be.removeVolumeErr = errors.New("engine unreachable")
	rebooted := &Runner{Backend: be}
	if _, err := rebooted.SweepInterruptedWorkspaceHomeMigrations(context.Background()); err == nil {
		t.Error("a sweep whose volume removal failed reported success")
	}
	if _, err := os.Stat(workspaceHomeMigrationRecordPathFor(t, rebooted, "myws")); err != nil {
		t.Errorf("the in-progress record was deleted although the volume removal failed (stat err = %v): the wreckage "+
			"is still there and no later boot will look at it again", err)
	}
	if !be.volumeExists(volume) {
		t.Fatal("test wiring: the volume should still exist")
	}
}

// --- codex review of PR8 round 4 --------------------------------------------
//
// Round 3 established "while a record exists for a slug, nothing runs against
// that workspace's home", and made the dispatch path DISCARD the home rather
// than proceed past a record it found. Round 4's Blocker 1 is the other half of
// that trade: a record survives for reasons that have nothing to do with the
// home being wreckage, and the round-3 dispatch destroys a perfectly good home
// for every one of them.
//
// The two routes below are the ones that actually occur, and neither involves a
// second crash:
//
//   - the migration SUCCEEDS and the record's unlink fails afterwards, and
//   - the migration is REFUSED at step 1 (the engine's 409 for a volume a
//     running job holds) and the record's cleanup unlink fails afterwards.
//
// In both the home is exactly what its owner expects it to be, and in both the
// round-3 dispatch throws it away — several GB, the harness authentication, and
// (in the first case) a multi-GB migration that had just finished.
//
// The tests below model "the unlink failed" by putting back the record the
// implementation ITSELF last wrote, captured from its own directory fsyncs (see
// migrationRecordTrace). Hand-writing that file would test a state the daemon
// might no longer produce; capturing it means these tests move with the
// implementation and would catch a phase that is set in the wrong order just as
// well as one that is never set at all.

// migrationRecordTrace captures the content of one workspace's
// <slug>.migrating.json at every parent-directory fsync the implementation
// performs while it runs.
//
// fsyncDir is the seam because it is where every durable write to homes-meta/
// ends: the record writer calls it after its rename and the record remover calls
// it after its unlink, so a trace taken there sees each committed state of the
// file exactly once, in order, without the test having to know how many writes
// the implementation makes or what it puts in them.
type migrationRecordTrace struct {
	path   string
	states [][]byte
}

func traceMigrationRecord(t *testing.T, path string) *migrationRecordTrace {
	t.Helper()
	tr := &migrationRecordTrace{path: path}
	prev := fsyncDir
	fsyncDir = func(dir string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			data = nil
		}
		tr.states = append(tr.states, data)
		return prev(dir)
	}
	t.Cleanup(func() { fsyncDir = prev })
	return tr
}

// lastPresent returns the final content the record had before it stopped
// existing — i.e. byte for byte what an unlink that FAILED would have left
// behind.
func (tr *migrationRecordTrace) lastPresent(t *testing.T) []byte {
	t.Helper()
	for i := len(tr.states) - 1; i >= 0; i-- {
		if tr.states[i] != nil {
			return tr.states[i]
		}
	}
	t.Fatalf("no in-progress migration record was ever written at %s", tr.path)
	return nil
}

// replantMigrationRecord puts a captured record back, modelling the unlink that
// could not be performed.
func replantMigrationRecord(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("replant the in-progress record: %v", err)
	}
}

// TestRunner_ResolveWorkspaceHome_KeepsTheHomeAMigrationFinished is round 4's
// Blocker 1, route A: the migration completed and only the record's unlink
// failed.
//
// What is on disk at that point is a home holding the harness authentication and
// the toolchain the operator just spent an afternoon moving. The record proves
// nothing about it — the destruction it was written for is over — so the next
// dispatch must clear the record and leave the home alone.
func TestRunner_ResolveWorkspaceHome_KeepsTheHomeAMigrationFinished(t *testing.T) {
	r, be, volume := settledWorkspaceHome(t, "myws")
	recordPath := workspaceHomeMigrationRecordPathFor(t, r, "myws")

	trace := traceMigrationRecord(t, recordPath)
	if _, err := r.ImportWorkspaceHome(context.Background(), "myws",
		bytes.NewReader(importTestTar(t, map[string]os.FileMode{".claude/.credentials.json": 0o600}))); err != nil {
		t.Fatalf("ImportWorkspaceHome: %v", err)
	}
	leftover := trace.lastPresent(t)
	replantMigrationRecord(t, recordPath, leftover)

	migrated := filepath.Join(be.dirFor(volume), ".claude", ".credentials.json")
	if _, err := os.Stat(migrated); err != nil {
		t.Fatalf("test wiring: the migration did not deliver the credentials file: %v", err)
	}
	identityBefore := be.identityOf(t, volume)
	removesBefore := be.removeCount()

	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("resolveWorkspaceHome with a leftover record from a FINISHED migration: %v", err)
	}

	if !be.volumeExists(volume) {
		t.Fatalf("the dispatch destroyed the home volume %q a migration had just finished filling: the record it acted on "+
			"was left behind by the migration's own cleanup, not by an interruption, and nothing about the home was wrong", volume)
	}
	if got := be.identityOf(t, volume); got != identityBefore {
		t.Errorf("the home volume was replaced (identity %q -> %q)", identityBefore, got)
	}
	if _, err := os.Stat(migrated); err != nil {
		t.Errorf("the migrated credentials file is gone (stat err = %v): the operator's harness authentication was "+
			"destroyed by a record that described a migration which had SUCCEEDED", err)
	}
	if be.removeCount() != removesBefore {
		t.Errorf("the dispatch issued %d volume removals (was %d) against a home nothing was wrong with",
			be.removeCount()-removesBefore, removesBefore)
	}
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Errorf("the leftover record survived the dispatch (stat err = %v): it has to be cleared, or every later "+
			"dispatch re-asks the same question", err)
	}
}

// TestRunner_ResolveWorkspaceHome_KeepsTheHomeARefusedMigrationNeverTouched is
// round 4's Blocker 1, route B.
//
// A migration refused at step 1 changed NOTHING — that is the entire reason the
// volume removal goes first and is also the in-use check. The record cleanup
// that follows the refusal has its error logged and dropped, so a cleanup that
// fails leaves a record over a home that is not merely fine but literally
// untouched. The dispatch that arrives once the job holding the volume has
// finished then destroys it.
func TestRunner_ResolveWorkspaceHome_KeepsTheHomeARefusedMigrationNeverTouched(t *testing.T) {
	r, be, volume := settledWorkspaceHome(t, "myws")
	recordPath := workspaceHomeMigrationRecordPathFor(t, r, "myws")
	settledFile := filepath.Join(be.dirFor(volume), ".toolchain")
	if _, err := os.Stat(settledFile); err != nil {
		t.Fatalf("test wiring: the settled home has no init.sh output: %v", err)
	}

	trace := traceMigrationRecord(t, recordPath)
	be.removeVolumeErr = ErrWorkspaceHomeInUse
	if _, err := r.ImportWorkspaceHome(context.Background(), "myws",
		bytes.NewReader(importTestTar(t, map[string]os.FileMode{"a": 0o644}))); !errors.Is(err, ErrWorkspaceHomeInUse) {
		t.Fatalf("error = %v, want ErrWorkspaceHomeInUse", err)
	}
	be.removeVolumeErr = nil
	replantMigrationRecord(t, recordPath, trace.lastPresent(t))

	identityBefore := be.identityOf(t, volume)
	markerBefore, err := os.ReadFile(workspaceHomeMarkerPathFor(t, r, "myws"))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	runsBefore := be.runCount()

	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("resolveWorkspaceHome with a leftover record from a REFUSED migration: %v", err)
	}

	if !be.volumeExists(volume) {
		t.Fatalf("the dispatch destroyed the home volume %q of a migration that was refused before it changed anything", volume)
	}
	if got := be.identityOf(t, volume); got != identityBefore {
		t.Errorf("the home volume was replaced (identity %q -> %q)", identityBefore, got)
	}
	if _, err := os.Stat(settledFile); err != nil {
		t.Errorf("the settled home's contents are gone (stat err = %v)", err)
	}
	markerAfter, readErr := os.ReadFile(workspaceHomeMarkerPathFor(t, r, "myws"))
	if readErr != nil {
		t.Errorf("the completion marker was removed (%v): the migration never got past its own refusal", readErr)
	} else if !bytes.Equal(markerBefore, markerAfter) {
		t.Error("the completion marker was rewritten")
	}
	if be.runCount() != runsBefore {
		t.Errorf("init ran %d times (was %d): a refused migration must not re-arm anything", be.runCount(), runsBefore)
	}
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Errorf("the leftover record survived the dispatch (stat err = %v)", err)
	}
}

// TestRunner_ResolveWorkspaceHome_StillDiscardsWreckageMidExtraction is the
// vacuity guard for the two above, and the reason they cannot be satisfied by
// making the dispatch path ignore records altogether.
//
// It replants the record the implementation held WHILE THE EXTRACTION WAS
// RUNNING — the one interval in which the volume under this name really does
// hold a home nobody vouched for — and requires the round-3 behaviour unchanged:
// the volume goes, and the next dispatch builds from empty.
func TestRunner_ResolveWorkspaceHome_StillDiscardsWreckageMidExtraction(t *testing.T) {
	r, be, volume := settledWorkspaceHome(t, "myws")
	recordPath := workspaceHomeMigrationRecordPathFor(t, r, "myws")

	var midExtraction []byte
	be.onImport = func() {
		data, err := os.ReadFile(recordPath)
		if err != nil {
			t.Errorf("no in-progress record existed while the extraction ran: %v", err)
			return
		}
		midExtraction = data
	}
	if _, err := r.ImportWorkspaceHome(context.Background(), "myws",
		bytes.NewReader(importTestTar(t, map[string]os.FileMode{"a": 0o644}))); err != nil {
		t.Fatalf("ImportWorkspaceHome: %v", err)
	}
	be.onImport = nil
	if midExtraction == nil {
		t.Fatal("test wiring: the extraction hook never ran")
	}

	replantMigrationRecord(t, recordPath, midExtraction)
	partial := filepath.Join(be.dirFor(volume), ".local", "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(partial), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(partial, []byte("half-extracted\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", err)
	}
	if _, err := os.Lstat(partial); !os.IsNotExist(err) {
		t.Errorf("init.sh ran against the half-extracted home an interrupted migration left behind (stat err = %v): the "+
			"record captured mid-extraction has to keep authorizing the discard, or round 3's fix is undone", err)
	}
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Errorf("the record survived the discard (stat err = %v)", err)
	}
}

// TestRunner_ImportWorkspaceHome_MakesTheHomesMetaDirItselfDurable is round 4's
// Blocker 2.
//
// The record writer fsyncs homes-meta/ so its own rename survives a power cut.
// That says nothing about homes-meta/ ITSELF: a directory's entry lives in its
// PARENT, so a homes-meta/ this migration just created can vanish whole — record
// and all — while the partial volume it was written to describe survives. The
// startup sweep then reads "no meta dir" as "no interrupted migration", the next
// dispatch re-creates the directory, and init.sh runs against a partial home with
// nothing left to say so: the exact state the sentinel exists to make impossible.
func TestRunner_ImportWorkspaceHome_MakesTheHomesMetaDirItselfDurable(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	r, _ := newWorkspaceHomeTestRunnerWithBackend(t)
	metaDir, err := r.workspaceHomeMetaDir()
	if err != nil {
		t.Fatalf("workspaceHomeMetaDir: %v", err)
	}
	if _, err := os.Stat(metaDir); !os.IsNotExist(err) {
		t.Fatalf("test wiring: %s already exists (stat err = %v); this test needs a workspace never dispatched into",
			metaDir, err)
	}

	var synced []string
	prev := fsyncDir
	fsyncDir = func(dir string) error { synced = append(synced, dir); return prev(dir) }
	t.Cleanup(func() { fsyncDir = prev })

	if _, err := r.ImportWorkspaceHome(context.Background(), "myws",
		bytes.NewReader(importTestTar(t, map[string]os.FileMode{"a": 0o644}))); err != nil {
		t.Fatalf("ImportWorkspaceHome: %v", err)
	}

	// Every level this call had to create needs its OWN entry flushed, which
	// means flushing that level's parent. <XDG_DATA_HOME>/boid and
	// <XDG_DATA_HOME>/boid/homes-meta were both missing, so both parents count.
	for _, want := range []string{filepath.Dir(metaDir), filepath.Dir(filepath.Dir(metaDir))} {
		found := false
		for _, got := range synced {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%q was never fsync'd (synced: %v): a directory created by this migration can be lost whole by a "+
				"power cut, taking the in-progress record with it and leaving the partial home volume behind with nothing "+
				"to describe it", want, synced)
		}
	}
}

// TestRunner_ImportWorkspaceHome_SyncsAnExistingHomesMetaDirsParentToo is the
// half of Blocker 2 that a "only sync what MkdirAll created" fix would miss.
//
// homes-meta/ is created by whichever of the two callers gets there first, and
// resolveWorkspaceHome's MkdirAll is not durable (its own file, the completion
// marker, is deliberately best-effort). So a migration can find the directory
// already present and still be standing on a directory entry that is only in the
// page cache — a dispatch seconds earlier is enough.
func TestRunner_ImportWorkspaceHome_SyncsAnExistingHomesMetaDirsParentToo(t *testing.T) {
	r, _, _ := settledWorkspaceHome(t, "myws")
	metaDir, err := r.workspaceHomeMetaDir()
	if err != nil {
		t.Fatalf("workspaceHomeMetaDir: %v", err)
	}
	if _, err := os.Stat(metaDir); err != nil {
		t.Fatalf("test wiring: %s should already exist: %v", metaDir, err)
	}

	var synced []string
	prev := fsyncDir
	fsyncDir = func(dir string) error { synced = append(synced, dir); return prev(dir) }
	t.Cleanup(func() { fsyncDir = prev })

	if _, err := r.ImportWorkspaceHome(context.Background(), "myws",
		bytes.NewReader(importTestTar(t, map[string]os.FileMode{"a": 0o644}))); err != nil {
		t.Fatalf("ImportWorkspaceHome: %v", err)
	}

	want := filepath.Dir(metaDir)
	for _, got := range synced {
		if got == want {
			return
		}
	}
	t.Errorf("%q was never fsync'd (synced: %v): the migration trusted a homes-meta directory entry that another "+
		"code path created without flushing it", want, synced)
}

// TestRunner_SweepInterruptedWorkspaceHomeMigrations_BoundsTheEngineCall is
// round 4's Major.
//
// buildRuntime runs this sweep before auto-reopen and before the API listener,
// on context.Background(). An engine socket that accepts the connection and then
// answers nothing — a hung dockerd, a podman socket whose service died mid-
// request — parks the daemon inside VolumeRemove forever: no jobs are reopened,
// no CLI can connect, and nothing in the log says which of the two it is stuck
// on. Round 2 already gave the in-process cleanup a bound of its own for the same
// reason (workspaceHomeImportCleanupTimeout); startup has strictly less
// cancellation available than a request does, so it needs one at least as much.
func TestRunner_SweepInterruptedWorkspaceHomeMigrations_BoundsTheEngineCall(t *testing.T) {
	r, be, volume := settledWorkspaceHome(t, "myws")
	killedMigrationResidue(t, r, be, "myws", volume)

	// Only the calls the SWEEP makes are in scope. The migration that produced
	// the residue runs under the operator's own request context, which is
	// cancellable and deliberately not shortened here.
	beforeSweep := len(be.removeDeadlines())

	rebooted := &Runner{Backend: be}
	if _, err := rebooted.SweepInterruptedWorkspaceHomeMigrations(context.Background()); err != nil {
		t.Fatalf("SweepInterruptedWorkspaceHomeMigrations: %v", err)
	}

	deadlines := be.removeDeadlines()[beforeSweep:]
	if len(deadlines) == 0 {
		t.Fatal("test wiring: the sweep never called the engine")
	}
	for i, hadDeadline := range deadlines {
		if !hadDeadline {
			t.Errorf("the startup sweep handed the engine call #%d a context with no deadline: an engine that accepts the "+
				"connection and never answers parks daemon startup inside it forever, before auto-reopen and before the "+
				"API listener", i+1)
		}
	}
}

// TestRunner_SweepInterruptedWorkspaceHomeMigrations_ReturnsWhenTheEngineNeverAnswers
// is the same Major stated as the behaviour rather than as the mechanism: a
// deadline nobody acts on would satisfy the test above and still hang startup.
//
// The engine here accepts the call and answers nothing, which is what a hung
// dockerd or a dead-but-listening podman socket looks like from the daemon's
// side. What has to come out is a sweep that RETURNS, an error naming the
// workspace, and a record still on disk — the wreckage is untouched, so a later
// start (or the dispatch path) has to be the one that finishes it.
func TestRunner_SweepInterruptedWorkspaceHomeMigrations_ReturnsWhenTheEngineNeverAnswers(t *testing.T) {
	r, be, volume := settledWorkspaceHome(t, "myws")
	killedMigrationResidue(t, r, be, "myws", volume)

	prev := workspaceHomeMigrationSweepTimeout.Set(50 * time.Millisecond)
	t.Cleanup(func() { workspaceHomeMigrationSweepTimeout.Set(prev) })
	be.blockRemoveUntilDone = true

	rebooted := &Runner{Backend: be}
	done := make(chan error, 1)
	go func() {
		_, serr := rebooted.SweepInterruptedWorkspaceHomeMigrations(context.Background())
		done <- serr
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the sweep reported success although the engine never answered its volume removal")
		}
		if !strings.Contains(err.Error(), "myws") {
			t.Errorf("error %q does not name the workspace whose home is still wreckage", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the startup sweep never returned against an engine that accepts the connection and answers nothing: " +
			"daemon startup parks here, before auto-reopen and before the API listener, with no way in and no way out")
	}

	if _, err := os.Stat(workspaceHomeMigrationRecordPathFor(t, rebooted, "myws")); err != nil {
		t.Errorf("the record was removed although the volume removal timed out (stat err = %v): a timed-out removal is a "+
			"FAILED removal, and the wreckage it describes is still there for a later start to finish", err)
	}
}

// TestMkdirAllDurable_SyncsEveryLevelItCreated is Blocker 2 at the unit, where
// the "how far up" decision lives.
//
// The multi-level case is the one a fresh install lands in: a daemon whose
// <dataHome> does not exist yet creates both it and homes-meta/ in one call, and
// flushing only the deepest would leave the whole subtree hanging off an entry
// that is still only in the page cache — i.e. would lose the record exactly as
// completely as not flushing anything.
func TestMkdirAllDurable_SyncsEveryLevelItCreated(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "boid", "homes-meta")

	var synced []string
	prev := fsyncDir
	fsyncDir = func(dir string) error { synced = append(synced, dir); return prev(dir) }
	t.Cleanup(func() { fsyncDir = prev })

	if err := mkdirAllDurable(target, 0o700); err != nil {
		t.Fatalf("mkdirAllDurable: %v", err)
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Fatalf("stat %s: %v (dir = %v)", target, err, info)
	}

	for _, want := range []string{root, filepath.Join(root, "boid")} {
		found := false
		for _, got := range synced {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%q was never synced (synced: %v)", want, synced)
		}
	}

	// A second call creates nothing and must still flush the parent: see
	// mkdirAllDurable on why "already there" says nothing about "already
	// durable".
	synced = nil
	if err := mkdirAllDurable(target, 0o700); err != nil {
		t.Fatalf("mkdirAllDurable (second call): %v", err)
	}
	if len(synced) != 1 || synced[0] != filepath.Join(root, "boid") {
		t.Errorf("synced = %v, want exactly the immediate parent: a directory another code path created without "+
			"flushing is indistinguishable from one this call created, and the record inside it is only as durable as "+
			"the entry it hangs off", synced)
	}
}

// TestMkdirAllDurable_FailsWhenItCannotBeMadeDurable keeps the helper on the
// same footing as the record writer it exists for: the caller destroys a home
// on the strength of what it writes into this directory, so "the directory may
// not survive a crash" has to stop the migration rather than warn about it.
func TestMkdirAllDurable_FailsWhenItCannotBeMadeDurable(t *testing.T) {
	failEveryDirSync(t, errors.New("input/output error"))

	err := mkdirAllDurable(filepath.Join(t.TempDir(), "boid", "homes-meta"), 0o700)
	if err == nil {
		t.Fatal("mkdirAllDurable reported success although no directory entry could be flushed")
	}
	if !strings.Contains(err.Error(), "input/output error") {
		t.Errorf("error %q loses the underlying failure", err)
	}
}

// TestWorkspaceHomeMigrationRecord_DiscardsHome pins the predicate every
// recovery path branches on, including the two arms no code in this repo can
// produce.
//
// The absent phase is what a record written by a build from BEFORE round 4 looks
// like, and those records meant "a migration was started", which through round 3
// authorized a discard. An unrecognized phase is what a record from a LATER
// build could look like. Both are "boid cannot tell", and both have to resolve
// the way an uncertain completion marker does — over-reach, one extra init run —
// rather than the way a known-safe record does.
func TestWorkspaceHomeMigrationRecord_DiscardsHome(t *testing.T) {
	for _, tc := range []struct {
		phase workspaceHomeMigrationPhase
		want  bool
		why   string
	}{
		{workspaceHomeMigrationRecorded, false, "nothing had been destroyed when this was written"},
		{workspaceHomeMigrationHomeDestroyed, true, "the home volume was removed and may hold a partial extraction"},
		{workspaceHomeMigrationHomeAbsent, false, "the volume is confirmed gone, so there is nothing under this name to discard"},
		{workspaceHomeMigrationHomeRebuilt, false, "the extraction finished, so the home is a home again"},
		{"", true, "a record from a build that predates the phase field meant only \"a migration was started\""},
		{"something-a-later-build-invented", true, "an unrecognized phase is not a known-safe one"},
	} {
		rec := workspaceHomeMigrationRecord{Slug: "myws", Phase: tc.phase}
		if got := rec.discardsHome(); got != tc.want {
			t.Errorf("phase %q: discardsHome() = %v, want %v — %s", tc.phase, got, tc.want, tc.why)
		}
	}
}

// failDirSyncOnceTheRecordReaches makes every parent-directory fsync fail from
// the moment the record at recordPath carries phase, and returns a function that
// stops it doing so.
//
// Aiming a storage failure at ONE of the migration's record writes needs
// something better than "fail from the Nth call": the count depends on how many
// directories the migration happens to create and would silently retarget the
// next time that changes. The record's own content is the stable landmark —
// fsyncDir runs after the rename, so by the time it is called the file already
// says which write it belongs to.
//
// The disarm flips a flag inside this closure rather than restoring fsyncDir, so
// it composes with a traceMigrationRecord installed on top. A test needs it when
// it goes on to exercise what happens AFTER the incident: the storage that failed
// the migration is normally working again by the time the next daemon start or
// dispatch arrives, and leaving it broken would measure the fail-loud path
// instead of the recovery being asserted.
func failDirSyncOnceTheRecordReaches(t *testing.T, recordPath string, phase workspaceHomeMigrationPhase, failure error) (disarm func()) {
	t.Helper()
	prev := fsyncDir
	armed := true
	fsyncDir = func(dir string) error {
		if armed {
			if data, err := os.ReadFile(recordPath); err == nil {
				var rec workspaceHomeMigrationRecord
				if json.Unmarshal(data, &rec) == nil && rec.Phase == phase {
					return failure
				}
			}
		}
		return prev(dir)
	}
	t.Cleanup(func() { fsyncDir = prev })
	return func() { armed = false }
}

// TestRunner_ImportWorkspaceHome_ExtractsNothingWhenTheWindowIsNotDurable is the
// step-1.5 half of round 4's Blocker 1, and it is the one that keeps the phase
// from becoming decoration.
//
// A "home-destroyed" that is only in the page cache records nothing. If the
// migration extracted anyway and the power then cut, what would be on disk is a
// PARTIAL home under a record saying nothing had been destroyed — and round 4's
// own recovery reads that record as "leave this home alone", hands the wreckage
// to init.sh, and freezes the breakage behind a fresh completion marker. That is
// worse than the state round 2 set out to prevent, because the sentinel would be
// actively vouching for it.
//
// So the migration stops there. The cost is bounded and visible: the old volume
// is already gone, so the workspace's home is EMPTY rather than partial, which
// is the state a never-dispatched-into workspace is in and which the next
// dispatch rebuilds from `init.sh`.
func TestRunner_ImportWorkspaceHome_ExtractsNothingWhenTheWindowIsNotDurable(t *testing.T) {
	r, be, volume := settledWorkspaceHome(t, "myws")
	recordPath := workspaceHomeMigrationRecordPathFor(t, r, "myws")
	failDirSyncOnceTheRecordReaches(t, recordPath, workspaceHomeMigrationHomeDestroyed, errors.New("input/output error"))

	_, err := r.ImportWorkspaceHome(context.Background(), "myws",
		bytes.NewReader(importTestTar(t, map[string]os.FileMode{"a": 0o644})))
	if err == nil {
		t.Fatal("the migration extracted into the new volume although it could not durably record that the old one had " +
			"been destroyed: a power cut during that extraction leaves a partial home under a record saying nothing was " +
			"destroyed, which recovery then hands to init.sh")
	}
	if !strings.Contains(err.Error(), "input/output error") {
		t.Errorf("error %q loses the underlying failure", err)
	}

	if be.importCount() != 0 {
		t.Errorf("the extraction ran %d times", be.importCount())
	}
	if be.volumeExists(volume) {
		t.Errorf("the home volume %q still exists: step 1 had already removed it, and re-creating it here would be the "+
			"very thing this branch refuses to do", volume)
	}

	// The recovery story the error promises has to be real: an empty home that
	// the next dispatch rebuilds, with init run exactly once more.
	runsBefore := be.runCount()
	if _, _, _, derr := r.resolveWorkspaceHome(context.Background(), "myws"); derr != nil {
		t.Fatalf("resolveWorkspaceHome after the refusal: %v", derr)
	}
	if be.runCount() != runsBefore+1 {
		t.Errorf("init ran %d times, want one more than the %d before the dispatch", be.runCount(), runsBefore)
	}
}

// TestRunner_ImportWorkspaceHome_SucceedsWhenOnlyTheWindowCloseFails is the
// other side of that decision, and the reason the three record writes cannot
// share one durability policy.
//
// The first two guard a destruction that has not happened yet, so failing them
// is free. The third guards nothing: the extraction is over and the operator's
// home is complete and correct. Failing the migration there would report a
// disaster for a workspace that is fine — and, worse, invite a re-run of a
// multi-GB import that had already succeeded.
func TestRunner_ImportWorkspaceHome_SucceedsWhenOnlyTheWindowCloseFails(t *testing.T) {
	r, be, volume := settledWorkspaceHome(t, "myws")
	recordPath := workspaceHomeMigrationRecordPathFor(t, r, "myws")
	failDirSyncOnceTheRecordReaches(t, recordPath, workspaceHomeMigrationHomeRebuilt, errors.New("input/output error"))

	res, err := r.ImportWorkspaceHome(context.Background(), "myws",
		bytes.NewReader(importTestTar(t, map[string]os.FileMode{".claude/.credentials.json": 0o600})))
	if err != nil {
		t.Fatalf("a migration that copied everything correctly was failed because it could not update its own bookkeeping "+
			"afterwards: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(be.dirFor(volume), ".claude", ".credentials.json")); serr != nil {
		t.Errorf("the migrated file is missing (stat err = %v)", serr)
	}
	// The unlink itself still succeeded here (only the flush after it failed), so
	// there is no leftover record to warn about.
	if res.MigrationRecordDiscardsHome {
		t.Errorf("result claims the leftover record will discard this home, but the record was removed: %+v", res)
	}
	if _, serr := os.Stat(recordPath); !os.IsNotExist(serr) {
		t.Errorf("the record survived (stat err = %v)", serr)
	}
}

// --- codex review of PR8 round 5 --------------------------------------------
//
// Round 4 gave the record a phase and closed the destruction window on the
// SUCCESS path: step 5 commits "home-rebuilt" durably before step 6 unlinks, so
// a record that survives its own unlink no longer claims the right to destroy
// anything. It left the two FAILURE paths open, and they fail exactly the way
// route A did before round 4 fixed it:
//
//   - the extraction dies and the partial volume IS successfully discarded, and
//     the record — still in "home-destroyed" — is unlinked as it stands;
//   - the destruction window cannot be recorded durably, the migration stops
//     with the old volume already gone, and the same unlink runs.
//
// removeWorkspaceHomeMigrationRecord unlinks FIRST and fsyncs the directory
// afterwards, so a failure of that fsync leaves the record gone as far as this
// process — and every dispatch that follows — can see, while nothing guarantees
// it stays gone. discardWorkspaceHomeMigrationRecord logs that failure and drops
// it. The sequence that costs an operator their home needs no second bug:
//
//	extraction fails, partial volume removed  ->  record still says "home-destroyed"
//	unlink succeeds, its fsync returns EIO    ->  logged and dropped; record now invisible
//	a waiting dispatch: os.Stat = "no record" ->  builds a CORRECT home from init.sh
//	power cut rolls the unlink back           ->  the DURABLE "home-destroyed" is back
//	next start's recovery obeys the phase     ->  the correct home is destroyed
//
// (Step 4's completion marker is written with homesMetaBestEffortDurability, so
// the same failing storage does not stop the dispatch in the middle of that
// sequence — it only warns.)
//
// The fix is round 4's, applied to the two paths where the volume ends up ABSENT
// rather than rebuilt: commit a non-destructive phase durably BEFORE unlinking.

// decodeMigrationRecord parses a captured record so a test can assert on the
// PHASE it carries rather than on the bytes.
func decodeMigrationRecord(t *testing.T, data []byte) workspaceHomeMigrationRecord {
	t.Helper()
	var rec workspaceHomeMigrationRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("the in-progress migration record is not readable JSON (%v): %s", err, data)
	}
	return rec
}

// leftoverThatMustNotDiscard returns the last state the record was committed in
// before it stopped existing — byte for byte what an unlink whose directory
// fsync failed leaves behind — and asserts that it authorizes no destruction.
//
// The absent case is a failure rather than a skip, and it is the shape the defect
// takes on the recovery path: an implementation that unlinks a destructive record
// without committing anything first commits NO state a trace can see, which is
// precisely the property being denied — that what survives the unlink is
// something somebody chose to leave behind.
func leftoverThatMustNotDiscard(t *testing.T, tr *migrationRecordTrace, unlinkedBy string) []byte {
	t.Helper()
	for i := len(tr.states) - 1; i >= 0; i-- {
		if tr.states[i] == nil {
			continue
		}
		if rec := decodeMigrationRecord(t, tr.states[i]); rec.discardsHome() {
			t.Errorf("%s unlinked a record that was still in phase %q, which authorizes a discard: the unlink is the one "+
				"step whose durability nothing guarantees, so a non-destructive phase has to be committed BEFORE it rather "+
				"than as a consequence of it", unlinkedBy, rec.Phase)
		}
		return tr.states[i]
	}
	t.Fatalf("%s unlinked the record without ever committing a phase over it: whatever a rolled-back unlink brings back is "+
		"then whatever the last writer happened to leave, which on this path is the phase that destroys the home", unlinkedBy)
	return nil
}

// lastInPhase returns the last state the record was captured in carrying phase,
// so a test can model a crash that kept ONE of the implementation's writes and
// lost the ones after it.
func (tr *migrationRecordTrace) lastInPhase(t *testing.T, phase workspaceHomeMigrationPhase) []byte {
	t.Helper()
	for i := len(tr.states) - 1; i >= 0; i-- {
		if tr.states[i] == nil {
			continue
		}
		if decodeMigrationRecord(t, tr.states[i]).Phase == phase {
			return tr.states[i]
		}
	}
	t.Fatalf("the record was never committed in phase %q at %s", phase, tr.path)
	return nil
}

// TestRunner_ImportWorkspaceHome_LeavesNoDestructiveRecordAfterDiscardingAPartialHome
// is round 5's Blocker on the step-4b path, walked end to end.
//
// The extraction fails, the half-extracted volume is removed successfully, and
// from that instant there is nothing under this name for anybody to discard. The
// record that the migration then unlinks must already say so — because the
// unlink is precisely the operation whose durability cannot be assumed, and
// because what fills the volume next is a home init.sh built correctly.
func TestRunner_ImportWorkspaceHome_LeavesNoDestructiveRecordAfterDiscardingAPartialHome(t *testing.T) {
	r, be, volume := settledWorkspaceHome(t, "myws")
	recordPath := workspaceHomeMigrationRecordPathFor(t, r, "myws")

	trace := traceMigrationRecord(t, recordPath)
	be.importErr = errors.New("unexpected EOF")
	be.importPartial = []string{".local/bin/claude"}
	if _, err := r.ImportWorkspaceHome(context.Background(), "myws",
		bytes.NewReader(importTestTar(t, map[string]os.FileMode{"a": 0o644}))); err == nil {
		t.Fatal("the migration reported success although its extraction failed")
	}
	be.importErr = nil
	be.importPartial = nil
	if be.volumeExists(volume) {
		t.Fatalf("test wiring: the half-extracted volume %q should have been discarded; this test is about what the record "+
			"says once it HAS been", volume)
	}

	leftover := leftoverThatMustNotDiscard(t, trace, "the migration's step-4b cleanup")

	// The workspace is dispatched into and gets a home that is entirely correct.
	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("resolveWorkspaceHome after the failed migration: %v", err)
	}
	rebuilt := filepath.Join(be.dirFor(volume), ".toolchain")
	if _, err := os.Stat(rebuilt); err != nil {
		t.Fatalf("test wiring: the dispatch did not rebuild the home: %v", err)
	}
	identityBefore := be.identityOf(t, volume)
	removesBefore := be.removeCount()

	// The power cut rolls the unlink back.
	replantMigrationRecord(t, recordPath, leftover)

	rebooted := &Runner{Backend: be}
	if _, err := rebooted.SweepInterruptedWorkspaceHomeMigrations(context.Background()); err != nil {
		t.Fatalf("SweepInterruptedWorkspaceHomeMigrations: %v", err)
	}

	if !be.volumeExists(volume) {
		t.Fatalf("the next daemon start destroyed the home volume %q that a dispatch had built correctly, on the strength of "+
			"a record whose migration had already discarded everything it was written about", volume)
	}
	if got := be.identityOf(t, volume); got != identityBefore {
		t.Errorf("the home volume was replaced (identity %q -> %q)", identityBefore, got)
	}
	if _, err := os.Stat(rebuilt); err != nil {
		t.Errorf("the rebuilt home's contents are gone (stat err = %v)", err)
	}
	if be.removeCount() != removesBefore {
		t.Errorf("recovery issued %d volume removals (was %d) against a home nothing was wrong with",
			be.removeCount()-removesBefore, removesBefore)
	}
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Errorf("the resurrected record survived the sweep (stat err = %v)", err)
	}
}

// TestRunner_ImportWorkspaceHome_LeavesNoDestructiveRecordWhenTheWindowIsNotDurable
// is the same Blocker on the windowErr path.
//
// The step-1.5 write fails only in its LAST step — the directory fsync runs
// after the rename — so the record on disk already says "home-destroyed" when
// the migration decides to stop. The volume, meanwhile, is gone: step 1 removed
// it and step 3 never ran. So this path unlinks a record that claims a
// destruction it has, at that point, nothing left to authorize, and it unlinks it
// on storage that has just demonstrated it cannot flush.
func TestRunner_ImportWorkspaceHome_LeavesNoDestructiveRecordWhenTheWindowIsNotDurable(t *testing.T) {
	r, be, volume := settledWorkspaceHome(t, "myws")
	recordPath := workspaceHomeMigrationRecordPathFor(t, r, "myws")

	failDirSyncOnceTheRecordReaches(t, recordPath, workspaceHomeMigrationHomeDestroyed, errors.New("input/output error"))
	// Installed second, so it wraps the injector rather than replacing it.
	trace := traceMigrationRecord(t, recordPath)

	if _, err := r.ImportWorkspaceHome(context.Background(), "myws",
		bytes.NewReader(importTestTar(t, map[string]os.FileMode{"a": 0o644}))); err == nil {
		t.Fatal("the migration reported success although it could not durably record that the home had been destroyed")
	}
	if be.volumeExists(volume) {
		t.Fatalf("test wiring: the old volume %q should be gone and no replacement created", volume)
	}

	leftover := leftoverThatMustNotDiscard(t, trace, "the migration's windowErr path")

	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("resolveWorkspaceHome after the refused migration: %v", err)
	}
	if _, err := os.Stat(filepath.Join(be.dirFor(volume), ".toolchain")); err != nil {
		t.Fatalf("test wiring: the dispatch did not rebuild the home: %v", err)
	}
	// What a rebuilt home really holds, and what init.sh does NOT put back: the
	// harness credentials the first job to run there authenticated with. Asserting
	// on init.sh's own output instead would be nearly vacuous — a discard followed
	// by a re-init recreates that.
	credentials := filepath.Join(be.dirFor(volume), ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(credentials), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(credentials, []byte("token\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	identityBefore := be.identityOf(t, volume)

	replantMigrationRecord(t, recordPath, leftover)

	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("resolveWorkspaceHome with the resurrected record: %v", err)
	}
	if !be.volumeExists(volume) {
		t.Fatalf("the dispatch destroyed the home volume %q that the previous dispatch had built correctly", volume)
	}
	if got := be.identityOf(t, volume); got != identityBefore {
		t.Errorf("the home volume was replaced (identity %q -> %q)", identityBefore, got)
	}
	if _, err := os.Stat(credentials); err != nil {
		t.Errorf("the harness authentication in the rebuilt home is gone (stat err = %v)", err)
	}
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Errorf("the resurrected record survived the dispatch (stat err = %v)", err)
	}
}

// TestRunner_ImportWorkspaceHome_KeepsTheRecordWhenItCannotBeMadeNonDestructive
// decides what happens when the non-destructive write ITSELF fails, which on
// this storage is the likely case rather than the exotic one — the write that
// just failed and the unlink that would follow it need the same fsync of the
// same directory.
//
// The answer is to keep the record rather than unlink it, and the asymmetry is
// the whole argument. A record that is VISIBLY there stops every dispatch of this
// workspace at recoverInterruptedWorkspaceHomeMigration, which resolves it
// BEFORE any volume is created — and against an absent volume that resolution is
// free (RemoveWorkspaceHomeVolume reports "not found" as success). A record that
// has been unlinked but not flushed is invisible for exactly as long as it takes
// a dispatch to build a good home, and then comes back.
//
// So this test also pins the worst DURABLE outcome of that choice: the phase
// write's rename lost to the crash, leaving "home-destroyed" behind. Recovery
// must still be harmless, because by then the volume it names does not exist.
func TestRunner_ImportWorkspaceHome_KeepsTheRecordWhenItCannotBeMadeNonDestructive(t *testing.T) {
	r, be, volume := settledWorkspaceHome(t, "myws")
	recordPath := workspaceHomeMigrationRecordPathFor(t, r, "myws")

	storageHealthy := failDirSyncOnceTheRecordReaches(t, recordPath, workspaceHomeMigrationHomeAbsent,
		errors.New("input/output error"))
	trace := traceMigrationRecord(t, recordPath)

	be.importErr = errors.New("unexpected EOF")
	be.importPartial = []string{".local/bin/claude"}
	if _, err := r.ImportWorkspaceHome(context.Background(), "myws",
		bytes.NewReader(importTestTar(t, map[string]os.FileMode{"a": 0o644}))); err == nil {
		t.Fatal("the migration reported success although its extraction failed")
	}
	be.importErr = nil
	be.importPartial = nil
	if be.volumeExists(volume) {
		t.Fatalf("test wiring: the half-extracted volume %q should have been discarded", volume)
	}

	if _, err := os.Stat(recordPath); err != nil {
		t.Fatalf("the record was unlinked although its non-destructive phase could not be made durable (stat err = %v): an "+
			"unlink on that same storage is what resurrects a destructive record after the next power cut, while a record "+
			"left in place is resolved by the next dispatch before any home exists to lose", err)
	}

	// The worst durable outcome of keeping it: the crash keeps the step-1.5 write
	// and loses the "home-absent" rename that could not be flushed. The storage is
	// working again by the time the next dispatch arrives, which is the case worth
	// measuring — one that is still broken fails the dispatch loudly instead, and
	// that path is pinned by
	// TestRunner_ResolveWorkspaceHome_RefusesWreckageItCannotRemove's sibling
	// reasoning.
	replantMigrationRecord(t, recordPath, trace.lastInPhase(t, workspaceHomeMigrationHomeDestroyed))
	storageHealthy()

	removesBefore := be.removeCount()
	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("resolveWorkspaceHome with a kept destructive record over an ABSENT volume: %v — recovery removes a volume "+
			"that is not there, which the engine reports as success", err)
	}
	if !be.volumeExists(volume) {
		t.Fatalf("the dispatch left the workspace with no home volume %q at all", volume)
	}
	if _, err := os.Stat(filepath.Join(be.dirFor(volume), ".toolchain")); err != nil {
		t.Errorf("the dispatch did not rebuild the home from init.sh (stat err = %v)", err)
	}
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Errorf("the record survived the dispatch (stat err = %v)", err)
	}
	if got := be.removeCount() - removesBefore; got != 1 {
		t.Errorf("recovery issued %d volume removals, want exactly the 1 no-op against the absent volume", got)
	}
}

// TestRunner_SweepInterruptedWorkspaceHomeMigrations_LeavesNoDestructiveRecordAfterDiscardingWreckage
// is the same Blocker in RECOVERY's own discard, which unlinks a record that is
// still in "home-destroyed" the moment after it removes the volume that phase
// described.
//
// The dispatch path survives that on its own — it RETURNS the unlink's error, so
// a dispatch whose record could not be removed fails instead of building a home
// behind it. The sweep does not: buildRuntime logs a sweep failure and continues
// into auto-reopen, deliberately, so the record can be invisible-but-not-durably-
// gone for exactly as long as it takes the next dispatch to build a correct home.
func TestRunner_SweepInterruptedWorkspaceHomeMigrations_LeavesNoDestructiveRecordAfterDiscardingWreckage(t *testing.T) {
	r, be, volume := settledWorkspaceHome(t, "myws")
	metaDir, err := r.workspaceHomeMetaDir()
	if err != nil {
		t.Fatalf("workspaceHomeMetaDir: %v", err)
	}
	recordPath := workspaceHomeMigrationRecordPath(metaDir, "myws")

	// A genuine interruption: the daemon's own bookkeeping as of mid-extraction,
	// plus a volume holding the residue an inventory-checking init.sh is fooled by.
	var killState map[string][]byte
	be.onImport = func() { killState = snapshotDir(t, metaDir) }
	if _, err := r.ImportWorkspaceHome(context.Background(), "myws",
		bytes.NewReader(importTestTar(t, map[string]os.FileMode{"a": 0o644}))); err != nil {
		t.Fatalf("ImportWorkspaceHome: %v", err)
	}
	be.onImport = nil
	if killState == nil {
		t.Fatal("test wiring: the extraction hook never ran")
	}
	restoreDir(t, metaDir, killState)
	partial := filepath.Join(be.dirFor(volume), ".local", "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(partial), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(partial, []byte("half-extracted\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	rebooted := &Runner{Backend: be}
	trace := traceMigrationRecord(t, recordPath)
	if _, err := rebooted.SweepInterruptedWorkspaceHomeMigrations(context.Background()); err != nil {
		t.Fatalf("SweepInterruptedWorkspaceHomeMigrations: %v", err)
	}
	if be.volumeExists(volume) {
		t.Fatalf("test wiring: the sweep should have discarded the wreckage in %q; this test is about what the record says "+
			"once it HAS", volume)
	}

	leftover := leftoverThatMustNotDiscard(t, trace, "the sweep's discard")

	// The next dispatch rebuilds the home correctly, and the operator authenticates
	// into it.
	if _, _, _, err := rebooted.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("resolveWorkspaceHome after the sweep: %v", err)
	}
	credentials := filepath.Join(be.dirFor(volume), ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(credentials), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(credentials, []byte("token\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	identityBefore := be.identityOf(t, volume)

	replantMigrationRecord(t, recordPath, leftover)

	if _, err := rebooted.SweepInterruptedWorkspaceHomeMigrations(context.Background()); err != nil {
		t.Fatalf("SweepInterruptedWorkspaceHomeMigrations with the resurrected record: %v", err)
	}
	if !be.volumeExists(volume) {
		t.Fatalf("the next daemon start destroyed the home volume %q the previous one had rebuilt", volume)
	}
	if got := be.identityOf(t, volume); got != identityBefore {
		t.Errorf("the home volume was replaced (identity %q -> %q)", identityBefore, got)
	}
	if _, err := os.Stat(credentials); err != nil {
		t.Errorf("the harness authentication in the rebuilt home is gone (stat err = %v)", err)
	}
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Errorf("the resurrected record survived the sweep (stat err = %v)", err)
	}
}
