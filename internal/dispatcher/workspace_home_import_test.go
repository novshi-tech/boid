package dispatcher

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/novshi-tech/boid/internal/dockerres"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// This file covers PR8 of docs/plans/workspace-home-volume-persistence.md
// (論点 f): Runner.ImportWorkspaceHome, the daemon-side half of
// `boid workspace import-home`.
//
// The single most important thing here is 論点 f's ACCEPTANCE CONDITION —
// 「どの案でも移行直後に init.sh が 1 回走る」. Everything else this function does
// (destroy a volume, create a new one, feed a container a tar) is in service of
// that, and a migration that copied the contents perfectly while leaving the
// completion marker satisfied would be exactly the silent hole the plan doc
// names: the host-era toolchain, with its host-era absolute paths, would live
// on forever and nothing would ever say so.

// importTestTar builds a tar stream holding the given path -> content entries,
// with per-entry modes, using the same writer the CLI uses. Going through
// WriteWorkspaceHomeTar rather than hand-rolling an archive/tar writer is the
// point: these tests then exercise the REAL pair (this repo's writer, the
// extraction argv boid hands the container), so a change to either that breaks
// the round trip fails here.
func importTestTar(t *testing.T, files map[string]os.FileMode) []byte {
	t.Helper()
	src := t.TempDir()
	for name, mode := range files {
		full := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte("content of "+name+"\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := os.Chmod(full, mode); err != nil {
			t.Fatalf("chmod: %v", err)
		}
	}
	var buf bytes.Buffer
	if _, err := WriteWorkspaceHomeTar(&buf, src, nil); err != nil {
		t.Fatalf("WriteWorkspaceHomeTar: %v", err)
	}
	return buf.Bytes()
}

// settledWorkspaceHome runs one resolveWorkspaceHome against a workspace that
// declares an init.sh, leaving the marker/volume pair in the state PR6 leaves a
// dispatched-into workspace in: identity A on the volume, home_id A in the
// marker, current generation, current skeleton, current script hash. This is
// the state 論点 f says a naive migration would fail to disturb.
func settledWorkspaceHome(t *testing.T, slug string) (r *Runner, be *bashWorkspaceInitBackend, volume string) {
	t.Helper()
	_, configDir := setupWorkspaceHomeTestDirs(t)
	r, be = newWorkspaceHomeTestRunnerWithBackend(t)
	writeInitScript(t, configDir, slug, "#!/bin/bash\nmkdir -p .toolchain\nexit 0\n")

	volume, _, _, err := r.resolveWorkspaceHome(context.Background(), slug)
	if err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", err)
	}
	if be.runCount() != 1 {
		t.Fatalf("init ran %d times before the migration, want exactly 1", be.runCount())
	}
	return r, be, volume
}

func workspaceHomeMarkerPathFor(t *testing.T, r *Runner, slug string) string {
	t.Helper()
	metaDir, err := r.workspaceHomeMetaDir()
	if err != nil {
		t.Fatalf("workspaceHomeMetaDir: %v", err)
	}
	return workspaceHomeMarkerPath(metaDir, slug)
}

// TestRunner_ImportWorkspaceHome_NextDispatchRunsInitExactlyOnce IS 論点 f's
// acceptance condition, and is the test the plan doc asks for by name
// (「逆に『移行したのに init が 1 度も走らない』場合は上記の穴が開いている」).
//
// Both halves matter. "At least once" is the hole; "exactly once" is what
// keeps the fix from being "bump the generation and re-init the entire
// installation forever" — the migration must re-arm init for THIS workspace
// and then settle again like any other.
func TestRunner_ImportWorkspaceHome_NextDispatchRunsInitExactlyOnce(t *testing.T) {
	r, be, _ := settledWorkspaceHome(t, "myws")

	// Sanity: without a migration the settled workspace does NOT re-init.
	// Without this line the test below could pass against a build that
	// re-inits on every dispatch, which is a different bug wearing the same
	// green tick.
	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("resolveWorkspaceHome (settled): %v", err)
	}
	if be.runCount() != 1 {
		t.Fatalf("a settled workspace re-ran init (%d runs); this test cannot distinguish a working migration from that", be.runCount())
	}

	tar := importTestTar(t, map[string]os.FileMode{".claude/.credentials.json": 0o600})
	if _, err := r.ImportWorkspaceHome(context.Background(), "myws", bytes.NewReader(tar)); err != nil {
		t.Fatalf("ImportWorkspaceHome: %v", err)
	}

	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("resolveWorkspaceHome (post-migration): %v", err)
	}
	if be.runCount() != 2 {
		t.Fatalf("init ran %d times in total after the migration, want 2 (one before, one forced by the migration). "+
			"THE MIGRATION LEFT THE COMPLETION MARKER SATISFIED — 論点 f's hole: the host-era toolchain survives with its "+
			"host-era absolute paths and nothing ever re-runs init.sh over it", be.runCount())
	}

	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("resolveWorkspaceHome (settling): %v", err)
	}
	if be.runCount() != 2 {
		t.Fatalf("init ran %d times, want 2: the migration must re-arm init ONCE, not permanently", be.runCount())
	}
}

// TestRunner_ImportWorkspaceHome_ReInitsWithTheMarkerRemovalUNDONE isolates
// 論点 f's leg 1 (「移行先 volume を作り直す」). It reconstructs the exact state a
// build whose marker deletion silently stopped working would leave behind — the
// pre-migration marker, byte for byte — and asserts init still runs.
//
// This is the "どちらの順序で失敗しても再 init に倒れる" claim, tested rather than
// asserted: with the marker back in place the ONLY thing that can still force a
// re-init is the volume's new identity.
func TestRunner_ImportWorkspaceHome_ReInitsWithTheMarkerRemovalUNDONE(t *testing.T) {
	r, be, _ := settledWorkspaceHome(t, "myws")
	markerPath := workspaceHomeMarkerPathFor(t, r, "myws")
	before, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("read pre-migration marker: %v", err)
	}

	tar := importTestTar(t, map[string]os.FileMode{"boid.json": 0o600})
	if _, err := r.ImportWorkspaceHome(context.Background(), "myws", bytes.NewReader(tar)); err != nil {
		t.Fatalf("ImportWorkspaceHome: %v", err)
	}
	// Undo leg 2.
	if err := os.WriteFile(markerPath, before, 0o600); err != nil {
		t.Fatalf("restore marker: %v", err)
	}

	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", err)
	}
	if be.runCount() != 2 {
		t.Fatalf("init ran %d times, want 2: with the marker restored, the recreated volume's NEW identity is the only "+
			"remaining re-init trigger and it did not fire (論点 f leg 1)", be.runCount())
	}
}

// TestRunner_ImportWorkspaceHome_ReInitsWithTheVolumeRecreateUNDONE isolates
// 論点 f's leg 2 (「移行後に marker を削除する」), the mirror image of the test
// above: the volume's identity is put back to what it was before the migration,
// so the marker's absence is the only remaining trigger.
func TestRunner_ImportWorkspaceHome_ReInitsWithTheVolumeRecreateUNDONE(t *testing.T) {
	r, be, volume := settledWorkspaceHome(t, "myws")
	identityBefore := be.identityOf(t, volume)

	tar := importTestTar(t, map[string]os.FileMode{"boid.json": 0o600})
	if _, err := r.ImportWorkspaceHome(context.Background(), "myws", bytes.NewReader(tar)); err != nil {
		t.Fatalf("ImportWorkspaceHome: %v", err)
	}
	// Undo leg 1: the volume looks like the one the marker described again.
	be.setIdentity(t, volume, identityBefore)

	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", err)
	}
	if be.runCount() != 2 {
		t.Fatalf("init ran %d times, want 2: with the volume identity restored, the deleted marker is the only remaining "+
			"re-init trigger and it did not fire (論点 f leg 2)", be.runCount())
	}
}

// TestRunner_ImportWorkspaceHome_ReplacesTheVolume pins leg 1's mechanism
// directly: the volume the workspace's home lives in is a NEW incarnation, not
// the old one with new contents. Same name, different identity.
func TestRunner_ImportWorkspaceHome_ReplacesTheVolume(t *testing.T) {
	r, be, volume := settledWorkspaceHome(t, "myws")
	before := be.identityOf(t, volume)

	res, err := r.ImportWorkspaceHome(context.Background(), "myws", bytes.NewReader(importTestTar(t, map[string]os.FileMode{"a": 0o644})))
	if err != nil {
		t.Fatalf("ImportWorkspaceHome: %v", err)
	}

	if res.Volume != volume {
		t.Errorf("result Volume = %q, want %q (the migration must target the volume dispatch mounts)", res.Volume, volume)
	}
	after := be.identityOf(t, volume)
	if after == before {
		t.Errorf("the home volume still carries identity %q: it was written into rather than replaced, so "+
			"「入れ物の incarnation」 never changed and the completion marker's home_id still matches", after)
	}
	if res.HomeID != after {
		t.Errorf("result HomeID = %q, but the volume carries %q", res.HomeID, after)
	}
	if !res.PreviousExisted {
		t.Error("PreviousExisted = false, but a settled workspace's volume was there to replace")
	}
}

// TestRunner_ImportWorkspaceHome_PreviousExistedDescribesTheVOLUME pins which
// question that field answers.
//
// The marker and the volume have independent lifetimes — that split is the
// whole reason workspaceHomeMarker.HomeID exists — so inferring "there was a
// home" from "there was a marker" is wrong in both directions. This is the
// direction an operator actually hits: they deleted the marker by hand (the
// documented manual-migration route), and the volume is still full of their
// credentials. Reporting "created" there would tell them nothing was
// destroyed, which is the opposite of what happened.
func TestRunner_ImportWorkspaceHome_PreviousExistedDescribesTheVOLUME(t *testing.T) {
	r, _, _ := settledWorkspaceHome(t, "myws")
	if err := os.Remove(workspaceHomeMarkerPathFor(t, r, "myws")); err != nil {
		t.Fatalf("remove marker: %v", err)
	}

	res, err := r.ImportWorkspaceHome(context.Background(), "myws", bytes.NewReader(importTestTar(t, map[string]os.FileMode{"a": 0o644})))
	if err != nil {
		t.Fatalf("ImportWorkspaceHome: %v", err)
	}
	if !res.PreviousExisted {
		t.Error("PreviousExisted = false with the marker gone but the volume still there: the field would be reporting " +
			"the marker's existence, and the CLI would say the home was 'created' while it was in fact destroyed and replaced")
	}
}

// TestRunner_ImportWorkspaceHome_RemovesTheCompletionMarker pins leg 2's
// mechanism directly.
func TestRunner_ImportWorkspaceHome_RemovesTheCompletionMarker(t *testing.T) {
	r, _, _ := settledWorkspaceHome(t, "myws")
	markerPath := workspaceHomeMarkerPathFor(t, r, "myws")
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("no marker to remove: %v", err)
	}

	res, err := r.ImportWorkspaceHome(context.Background(), "myws", bytes.NewReader(importTestTar(t, map[string]os.FileMode{"a": 0o644})))
	if err != nil {
		t.Fatalf("ImportWorkspaceHome: %v", err)
	}
	if !res.MarkerRemoved {
		t.Errorf("MarkerRemoved = false (error %q), want true", res.MarkerRemoveError)
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Errorf("the completion marker is still at %s (stat err = %v)", markerPath, err)
	}
}

// TestRunner_ImportWorkspaceHome_PreservesModes is D3's mode requirement:
// 「`0600` などの mode は維持すること (認証情報のパーミッションが緩むと退行)」.
//
// It runs the real tar writer through the real extraction argv, so it covers
// both ends of the only thing that can silently loosen a credential file's
// permissions: a header that does not record the mode, and an extraction that
// applies a umask over it.
func TestRunner_ImportWorkspaceHome_PreservesModes(t *testing.T) {
	// A umask that would visibly damage BOTH files if the extraction did not
	// ask tar to ignore it: 0600 -> 0600 is umask-proof, but 0666 -> 0606 and
	// 0755 -> 0715 are not.
	restoreUmask := setUmaskForTest(t, 0o070)

	r, _, volume := settledWorkspaceHome(t, "myws")
	tar := importTestTar(t, map[string]os.FileMode{
		".claude/.credentials.json": 0o600,
		"loose.txt":                 0o666,
		"bin/tool":                  0o755,
	})
	if _, err := r.ImportWorkspaceHome(context.Background(), "myws", bytes.NewReader(tar)); err != nil {
		t.Fatalf("ImportWorkspaceHome: %v", err)
	}
	restoreUmask()

	homeDir := workspaceHomeDirOf(t, r, volume)
	for name, want := range map[string]os.FileMode{
		".claude/.credentials.json": 0o600,
		"loose.txt":                 0o666,
		"bin/tool":                  0o755,
	} {
		fi, err := os.Lstat(filepath.Join(homeDir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode = %04o, want %04o", name, got, want)
		}
	}
}

// TestRunner_ImportWorkspaceHome_RefusesWhileAJobHoldsTheVolume is D4's
// 「実行中の job がある workspace は移行しない」.
//
// The refusal has to be side-effect free, which is the half worth testing:
// an operator who runs this against a busy workspace and gets told "no" must
// still have the workspace they had — same volume, same identity, same marker,
// and therefore no gratuitous re-init on the next dispatch.
func TestRunner_ImportWorkspaceHome_RefusesWhileAJobHoldsTheVolume(t *testing.T) {
	r, be, volume := settledWorkspaceHome(t, "myws")
	identityBefore := be.identityOf(t, volume)
	markerPath := workspaceHomeMarkerPathFor(t, r, "myws")
	markerBefore, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}

	be.removeVolumeErr = ErrWorkspaceHomeInUse

	_, err = r.ImportWorkspaceHome(context.Background(), "myws", bytes.NewReader(importTestTar(t, map[string]os.FileMode{"a": 0o644})))
	if !errors.Is(err, ErrWorkspaceHomeInUse) {
		t.Fatalf("error = %v, want it to wrap ErrWorkspaceHomeInUse so the API can answer 409", err)
	}
	if !strings.Contains(err.Error(), "myws") {
		t.Errorf("error %q does not name the workspace", err)
	}

	if got := be.identityOf(t, volume); got != identityBefore {
		t.Errorf("the home volume identity changed to %q despite the refusal", got)
	}
	markerAfter, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("the completion marker was removed despite the refusal: %v", err)
	}
	if !bytes.Equal(markerBefore, markerAfter) {
		t.Error("the completion marker was rewritten despite the refusal")
	}
	if be.importCount() != 0 {
		t.Errorf("the extraction ran %d times despite the refusal", be.importCount())
	}
}

// TestRunner_ImportWorkspaceHome_FailedExtractionStillReInits is D4's
// 「失敗した場合に元より悪い状態にならない」, stated precisely: once the volume is gone
// the migration cannot be un-done, so the guarantee is not "nothing changed" but
// "whatever is left re-initializes itself on the next dispatch".
func TestRunner_ImportWorkspaceHome_FailedExtractionStillReInits(t *testing.T) {
	r, be, _ := settledWorkspaceHome(t, "myws")
	be.importErr = errors.New("tar: unexpected EOF")

	if _, err := r.ImportWorkspaceHome(context.Background(), "myws", bytes.NewReader([]byte("garbage"))); err == nil {
		t.Fatal("ImportWorkspaceHome returned nil for a failed extraction")
	}

	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("resolveWorkspaceHome after a failed migration: %v", err)
	}
	if be.runCount() != 2 {
		t.Fatalf("init ran %d times, want 2: a half-completed migration must leave the workspace re-initializable, "+
			"not silently trusted", be.runCount())
	}
}

// TestRunner_ImportWorkspaceHome_PartialExtractionLeavesNoVolume is the OTHER
// half of D4's 「失敗した場合に元より悪い状態にならない」, and the half the first
// implementation only claimed.
//
// The claim was 「途中で失敗すると『一度も dispatch していない workspace』と同じ状態に
// なり、次の dispatch が init から作り直す」. A workspace never dispatched into has NO
// home volume. An extraction that died partway leaves one, holding whatever
// crossed before the stream stopped — which is not the same state and is worse
// than either neighbour, because init.sh runs against it rather than against
// nothing.
//
// Worse how, concretely: an init.sh is contractually idempotent
// (docs/plans/home-workspace-volume.md 「script 作者が守ること」), and the ordinary
// way to write one is to probe for what it installs — `command -v claude` — and
// return early. A half-extracted home whose `.local/bin/claude` arrived but
// whose payload did not answers that probe YES. init.sh then exits 0, the
// daemon writes a completion marker vouching for a home that cannot run the
// harness, and the state is FROZEN: the marker matches on every later dispatch,
// so nothing re-runs and the failure surfaces much later as the adapter's "CLI
// not found".
//
// So the guarantee has to be produced rather than described: on a failed
// extraction the volume goes, and the next dispatch starts from nothing.
func TestRunner_ImportWorkspaceHome_PartialExtractionLeavesNoVolume(t *testing.T) {
	r, be, volume := settledWorkspaceHome(t, "myws")
	be.importErr = errors.New("tar: unexpected EOF")
	// The exact residue that fools a `command -v` probe.
	be.importPartial = []string{".local/bin/claude", ".claude/settings.json"}

	if _, err := r.ImportWorkspaceHome(context.Background(), "myws", bytes.NewReader([]byte("truncated"))); err == nil {
		t.Fatal("ImportWorkspaceHome returned nil for a failed extraction")
	}

	if be.volumeExists(volume) {
		t.Errorf("the home volume %q still exists after a failed extraction, holding whatever crossed before the stream "+
			"stopped. 「一度も dispatch していない workspace と同じ状態」 means NO volume: a half-extracted one is what makes an "+
			"idempotent init.sh conclude the toolchain is installed and write a marker over a broken home", volume)
	}

	// The state the next dispatch actually finds, asserted end to end rather
	// than only at the volume-store level: a re-created volume must not carry
	// the wreckage forward.
	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("resolveWorkspaceHome after a failed migration: %v", err)
	}
	homeDir := workspaceHomeDirOf(t, r, volume)
	for _, leftover := range []string{".local/bin/claude", ".claude/settings.json"} {
		if _, err := os.Lstat(filepath.Join(homeDir, leftover)); !os.IsNotExist(err) {
			t.Errorf("%s survived into the re-initialized home (stat err = %v): the partial extraction was not discarded",
				leftover, err)
		}
	}
}

// TestRunner_ImportWorkspaceHome_PartialCleanupSurvivesACancelledContext is the
// case the cleanup actually exists for (codex review of PR8 round 2, Minor 2).
//
// The commonest route to a failed extraction is not a corrupt archive — it is
// the operator's CLI going away mid-upload: ^C, a dropped connection, a closed
// laptop. That cancels the HTTP request context, which IS the context the
// migration is running under, and it does so at the worst possible moment: after
// the old home volume has been destroyed and while its replacement holds a
// partial extraction. A cleanup issued on a descendant of that context would
// fail before reaching the engine, leaving exactly the half-extracted volume the
// whole mechanism exists to remove.
//
// Every other partial-cleanup test here passes context.Background(), so all of
// them stay green if discardPartialWorkspaceHome's context.WithoutCancel is
// changed back to an ordinary derived context. This one is the pin.
func TestRunner_ImportWorkspaceHome_PartialCleanupSurvivesACancelledContext(t *testing.T) {
	r, be, volume := settledWorkspaceHome(t, "myws")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancelled as the extraction STARTS: the migration is already past the point
	// of no return (the old volume is gone, the marker is gone), and the double
	// then writes importPartial and fails — so by the time the cleanup runs there
	// is a volume with a partial home in it, which is precisely where an
	// operator's ^C lands. (The cancel fires before that residue exists; what is
	// being pinned is that the CLEANUP still reaches the engine on a context the
	// caller already cancelled.)
	be.onImport = cancel
	be.importErr = errors.New("unexpected EOF")
	be.importPartial = []string{".local/bin/claude"}

	if _, err := r.ImportWorkspaceHome(ctx, "myws", bytes.NewReader([]byte("truncated"))); err == nil {
		t.Fatal("ImportWorkspaceHome returned nil for a failed extraction")
	}

	if be.volumeExists(volume) {
		t.Errorf("the home volume %q survived a failed extraction whose context was cancelled: the cleanup was issued on "+
			"a descendant of the caller's context, so it fails in exactly the case it exists for (an operator's ^C during "+
			"the upload) and the next dispatch runs init.sh against a half-extracted home", volume)
	}
}

// TestRunner_ImportWorkspaceHome_ReportsAFailedCleanupRemoval covers the case
// where the repair above cannot be carried out.
//
// It is reported rather than swallowed for the reason every other unhappy
// branch in this function is: the operator's next action differs. A migration
// that failed and cleaned up is re-runnable and needs nothing else; a migration
// that failed and could NOT clean up has left a half-extracted volume that the
// next dispatch will init.sh against, and the operator has to remove it by hand
// before re-running. Collapsing the two into "extract failed" would send them
// to re-run the migration into exactly the state that cannot be repaired by
// re-running.
func TestRunner_ImportWorkspaceHome_ReportsAFailedCleanupRemoval(t *testing.T) {
	r, be, volume := settledWorkspaceHome(t, "myws")
	be.importErr = errors.New("tar: unexpected EOF")
	be.importPartial = []string{".local/bin/claude"}
	// Fails the SECOND removal only: the migration's own replacement removal
	// has to succeed or the extraction never runs.
	be.removeVolumeErr = errors.New("engine unreachable")
	be.removeVolumeErrFromCall = 2

	_, err := r.ImportWorkspaceHome(context.Background(), "myws", bytes.NewReader([]byte("truncated")))
	if err == nil {
		t.Fatal("ImportWorkspaceHome returned nil for a failed extraction")
	}
	msg := err.Error()
	if !strings.Contains(msg, "tar: unexpected EOF") {
		t.Errorf("error %q loses the extraction failure, which is what actually went wrong", msg)
	}
	if !strings.Contains(msg, "engine unreachable") {
		t.Errorf("error %q does not report that the cleanup removal ALSO failed; the operator is not told that a "+
			"half-extracted volume is still there", msg)
	}
	if !strings.Contains(msg, volume) {
		t.Errorf("error %q does not name the volume the operator now has to remove by hand", msg)
	}
}

// TestRunner_ImportWorkspaceHome_RefusesAWorkspaceThatIsNoLongerThere is the
// second half of codex round 2's Major 1 (the first is the registry covering
// `workspace remove` — see workspace_home_inflight_test.go).
//
// The registry alone cannot close the race, because one interleaving has nothing
// for it to observe: a removal that STARTS AND FINISHES before the migration
// registers leaves an empty registry and a deleted row. The migration would then
// mint a HOME volume for a workspace that no longer exists and extract the
// operator's credentials into it — a volume `boid workspace remove` can never
// target again (it 404s on the missing row) and no dispatch will ever mount.
//
// So the row is re-checked, and the placement is what makes it a fix rather than
// a narrower window: it happens while the migration holds its registration, so
// from that point on a removal is refused and the answer cannot go stale.
func TestRunner_ImportWorkspaceHome_RefusesAWorkspaceThatIsNoLongerThere(t *testing.T) {
	r, be, volume := settledWorkspaceHome(t, "myws")
	identityBefore := be.identityOf(t, volume)
	gone := errors.New("workspace \"myws\" not found")
	r.ConfirmWorkspaceExists = func(string) error { return gone }

	_, err := r.ImportWorkspaceHome(context.Background(), "myws",
		bytes.NewReader(importTestTar(t, map[string]os.FileMode{"a": 0o644})))
	if !errors.Is(err, gone) {
		t.Fatalf("error = %v, want it to wrap the confirmation's own error so the API can answer with that error's status", err)
	}

	// Side-effect free, on the same footing as the two other refusals: an
	// operator told "no" still has exactly the workspace they had.
	if be.removeCount() != 0 {
		t.Errorf("RemoveWorkspaceHomeVolume was called %d times for a workspace that is gone", be.removeCount())
	}
	if be.importCount() != 0 {
		t.Errorf("the extraction ran %d times for a workspace that is gone", be.importCount())
	}
	if got := be.identityOf(t, volume); got != identityBefore {
		t.Errorf("the home volume identity changed to %q despite the refusal", got)
	}
	if _, serr := os.Stat(workspaceHomeMarkerPathFor(t, r, "myws")); serr != nil {
		t.Errorf("the completion marker was removed despite the refusal: %v", serr)
	}
}

// TestRunner_ImportWorkspaceHome_ConfirmsTheWorkspaceUnderTheRegistration is
// what makes the check above worth having.
//
// A confirmation made BEFORE the registration only narrows the window: a removal
// landing between the two still finds the registry empty, deletes the row and
// the volume, and the migration then re-creates the volume anyway. The
// confirmation has to be made at a point from which a removal is already
// excluded — which is exactly what this asserts, from inside the callback.
func TestRunner_ImportWorkspaceHome_ConfirmsTheWorkspaceUnderTheRegistration(t *testing.T) {
	r, _, _ := settledWorkspaceHome(t, "myws")

	var removalRefused, confirmed bool
	r.ConfirmWorkspaceExists = func(slug string) error {
		confirmed = true
		release, err := r.BeginWorkspaceHomeRemoval(slug)
		if err == nil {
			release()
			return nil
		}
		removalRefused = errors.Is(err, ErrWorkspaceHomeBusy)
		return nil
	}

	if _, err := r.ImportWorkspaceHome(context.Background(), "myws",
		bytes.NewReader(importTestTar(t, map[string]os.FileMode{"a": 0o644}))); err != nil {
		t.Fatalf("ImportWorkspaceHome: %v", err)
	}

	if !confirmed {
		t.Fatal("ConfirmWorkspaceExists was never called: a migration can still mint a HOME volume for a workspace " +
			"`boid workspace remove` deleted moments earlier")
	}
	if !removalRefused {
		t.Error("a `workspace remove` of this workspace was ACCEPTED at the moment the migration confirmed the row " +
			"still existed: the confirmation is made outside the migration's registration, so its answer can go stale " +
			"before the volume is created and the orphan-volume race is only narrowed, not closed")
	}
}

// TestRunner_ImportWorkspaceHome_RefusesAnArchiveWithNoEntries is codex round
// 3's Blocker 2, daemon side — the half that does not depend on the CLI having
// got its pre-flight right.
//
// The destruction comes first and cannot not come first (it is also the in-use
// check), so "extract nothing into the volume we just destroyed" is a complete,
// successful, 200-answering way to delete a workspace's credentials. The CLI's
// own refusal covers the operator running `boid workspace import-home`; this
// covers everything else — an older CLI, a curl, a bug on the other side of the
// wire — and it is the only one of the two that sits on the same side as the
// deletion it is guarding.
//
// A body with no entries is refused BEFORE the removal, so the refusal is
// side-effect free on the same footing as the busy-workspace one.
func TestRunner_ImportWorkspaceHome_RefusesAnArchiveWithNoEntries(t *testing.T) {
	for name, body := range map[string][]byte{
		// What a symlinked --from produced before the walk was fixed, and what a
		// --from pointed at an empty directory produces today: a well-formed tar
		// whose entry count is zero.
		"well-formed empty tar": importTestTar(t, nil),
		// And the degenerate one: a client that declared the media type and sent
		// no body at all.
		"no body": nil,
	} {
		t.Run(name, func(t *testing.T) {
			r, be, volume := settledWorkspaceHome(t, "myws")
			identityBefore := be.identityOf(t, volume)
			markerPath := workspaceHomeMarkerPathFor(t, r, "myws")
			markerBefore, err := os.ReadFile(markerPath)
			if err != nil {
				t.Fatalf("read marker: %v", err)
			}

			_, err = r.ImportWorkspaceHome(context.Background(), "myws", bytes.NewReader(body))
			if !errors.Is(err, ErrWorkspaceHomeImportEmpty) {
				t.Fatalf("error = %v, want it to wrap ErrWorkspaceHomeImportEmpty so the API can answer 400", err)
			}
			if !strings.Contains(err.Error(), "myws") {
				t.Errorf("error %q does not name the workspace", err)
			}

			if be.removeCount() != 0 {
				t.Errorf("the home volume was removed %d times for an archive that would replace it with nothing", be.removeCount())
			}
			if be.importCount() != 0 {
				t.Errorf("the extraction ran %d times for an archive with no entries", be.importCount())
			}
			if got := be.identityOf(t, volume); got != identityBefore {
				t.Errorf("the home volume identity changed to %q despite the refusal", got)
			}
			markerAfter, err := os.ReadFile(markerPath)
			if err != nil {
				t.Fatalf("the completion marker was removed despite the refusal: %v", err)
			}
			if !bytes.Equal(markerBefore, markerAfter) {
				t.Error("the completion marker was rewritten despite the refusal")
			}
			if _, err := os.Stat(workspaceHomeMigrationRecordPathFor(t, r, "myws")); !os.IsNotExist(err) {
				t.Errorf("an in-progress record survived a refusal that destroyed nothing (stat err = %v): the next daemon "+
					"start would delete this workspace's intact home", err)
			}
		})
	}
}

// TestRunner_ImportWorkspaceHome_StillMigratesTheFirstEntry is the vacuity guard
// for the check above: peeking at the head of the stream must not eat it.
//
// A "does the archive have entries" test that consumed the first tar block would
// pass while every migration silently lost its first file.
func TestRunner_ImportWorkspaceHome_StillMigratesTheFirstEntry(t *testing.T) {
	r, _, volume := settledWorkspaceHome(t, "myws")

	// Names chosen so the FIRST entry in the archive (walk order is lexical) is
	// the one an operator would miss most.
	tar := importTestTar(t, map[string]os.FileMode{
		".credentials.json": 0o600,
		"zz-last":           0o644,
	})
	if _, err := r.ImportWorkspaceHome(context.Background(), "myws", bytes.NewReader(tar)); err != nil {
		t.Fatalf("ImportWorkspaceHome: %v", err)
	}

	homeDir := workspaceHomeDirOf(t, r, volume)
	for _, name := range []string{".credentials.json", "zz-last"} {
		if _, err := os.Lstat(filepath.Join(homeDir, name)); err != nil {
			t.Errorf("%s did not survive the migration (%v): the empty-archive check consumed part of the stream instead "+
				"of replaying it", name, err)
		}
	}
}

// TestRunner_ImportWorkspaceHome_RejectsAnInvalidSlug keeps the CLI's
// arbitrary string from reaching dockerres.WorkspaceHomeVolumeName, whose
// SanitizeNamePart would happily fold "../../etc" into a plausible volume name.
func TestRunner_ImportWorkspaceHome_RejectsAnInvalidSlug(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	r, be := newWorkspaceHomeTestRunnerWithBackend(t)
	if _, err := r.ImportWorkspaceHome(context.Background(), "Not A Slug", bytes.NewReader(nil)); err == nil {
		t.Fatal("ImportWorkspaceHome accepted an invalid slug")
	}
	if be.importCount() != 0 || be.removeCount() != 0 {
		t.Error("an invalid slug reached the engine")
	}
}

// TestRunner_ImportWorkspaceHome_RequiresAnImporterBackend mirrors
// workspaceInitExecutorFor's contract: a backend that cannot do this fails
// loudly rather than reporting a migration that never happened.
func TestRunner_ImportWorkspaceHome_RequiresAnImporterBackend(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	r := &Runner{Backend: &initOnlyBackend{}}
	_, err := r.ImportWorkspaceHome(context.Background(), "myws", bytes.NewReader(nil))
	if err == nil {
		t.Fatal("ImportWorkspaceHome succeeded against a backend that cannot import")
	}
	if !strings.Contains(err.Error(), "initOnlyBackend") {
		t.Errorf("error %q does not name the backend that is missing the capability", err)
	}
}

// TestRunner_ImportWorkspaceHome_TargetsTheVolumeDispatchMounts pins the one
// value a migration cannot get wrong quietly: extract into the wrong volume
// name and every assertion about markers and identities still passes while the
// workspace's real home stays empty.
func TestRunner_ImportWorkspaceHome_TargetsTheVolumeDispatchMounts(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	r, be := newWorkspaceHomeTestRunnerWithBackend(t)
	r.InstallID = "0123456789abcdef"

	res, err := r.ImportWorkspaceHome(context.Background(), "myws", bytes.NewReader(importTestTar(t, map[string]os.FileMode{"a": 0o644})))
	if err != nil {
		t.Fatalf("ImportWorkspaceHome: %v", err)
	}
	want := dockerres.WorkspaceHomeVolumeName(r.InstallID, "myws")
	if res.Volume != want {
		t.Errorf("Volume = %q, want %q", res.Volume, want)
	}
	req := be.lastImport(t)
	if req.HomeSource != want {
		t.Errorf("the extraction mounted %q, want %q", req.HomeSource, want)
	}
	if req.HomeTarget != hostHomeDir() {
		t.Errorf("the extraction targeted %q, want the path a job sees its own $HOME at (%q)", req.HomeTarget, hostHomeDir())
	}
	if req.HomeID != res.HomeID || req.HomeID == "" {
		t.Errorf("the extraction was handed identity %q, result reports %q", req.HomeID, res.HomeID)
	}
	// A workspace never dispatched into has no volume to replace; that is the
	// init-on-first-dispatch contract, not an error.
	if res.PreviousExisted {
		t.Error("PreviousExisted = true for a workspace that was never dispatched into")
	}
}

// setUmaskForTest installs a process-wide umask and returns a function that
// restores the previous one (also registered with t.Cleanup, so a failing test
// cannot leak it into the rest of the binary).
//
// Process-wide is safe here only because no test in this package calls
// t.Parallel(); the mode test needs a REAL umask because the thing it is
// checking is precisely whether tar was told to ignore one.
func setUmaskForTest(t *testing.T, mask int) (restore func()) {
	t.Helper()
	prev := syscall.Umask(mask)
	var once sync.Once
	restore = func() { once.Do(func() { syscall.Umask(prev) }) }
	t.Cleanup(restore)
	return restore
}

// initOnlyBackend satisfies WorkspaceInitExecutor and deliberately NOT
// WorkspaceHomeImporter, so the capability assertion has something to reject.
// Written out in full rather than embedding bashWorkspaceInitBackend: an
// embedded field would PROMOTE the importer methods and the type would satisfy
// the very interface it exists to fail.
//
// Its name appears in the error message the test asserts on, which is the
// point — an operator who wired a backend that cannot migrate has to be told
// which one.
type initOnlyBackend struct{}

var _ WorkspaceInitExecutor = (*initOnlyBackend)(nil)

func (*initOnlyBackend) EnsureWorkspaceHomeVolume(context.Context, WorkspaceHomeVolumeRequest) (string, error) {
	return "id", nil
}

func (*initOnlyBackend) RunWorkspaceInit(context.Context, WorkspaceInitRequest) error { return nil }

func (*initOnlyBackend) Launch(context.Context, sandbox.Spec, backend.LaunchOptions) (backend.SandboxSession, error) {
	return nil, errors.New("initOnlyBackend cannot launch jobs")
}

func (*initOnlyBackend) Adopt(context.Context, string) (backend.SandboxSession, bool) {
	return nil, false
}

func (*initOnlyBackend) ReapOrphans(context.Context) (backend.ReapReport, error) {
	return backend.ReapReport{}, nil
}
