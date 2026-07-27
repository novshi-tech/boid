package dispatcher

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/dockerres"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// This file covers the mutual exclusion between a DISPATCH and a MIGRATION of
// the same workspace HOME volume (codex review of PR8, Blocker 2).
//
// # The interleaving these tests are about
//
// resolveWorkspaceHome's fast path — a completion marker that matches — returns
// WITHOUT taking the per-workspace flock. So a settled workspace's dispatch
// holds no lock at all between resolving its home and asking the engine for a
// container, and in that gap:
//
//  1. the dispatch resolves volume A and observes identity A
//  2. the migration removes A, creates B, and starts extracting into it
//  3. the dispatch's own Launch runs ensureNamedVolumes, which VolumeCreates by
//     NAME — getting B — and then ContainerCreate mounts it
//  4. the job's agent and the migration's tar are both writing into B
//
// PR6's verifyWorkspaceHomeIdentity does not close this. It compares what the
// dispatch's own VolumeCreate answered against the identity resolved earlier,
// which catches an interleaving that lands BEFORE that comparison — but the
// comparison is not atomic with ContainerCreate, and a migration that lands
// between the two is mounted by name, matching nothing to compare. The plan
// doc's claim that the job side 「大声で失敗する」 is therefore true of one
// ordering and not of the other, which is why the fix is a registry rather than
// another identity check.
//
// # Why the registry is in memory
//
// A daemon is one process per installation, and the migration only exists as an
// HTTP route on that daemon, so both parties to this race are goroutines in one
// address space. The alternative — widening the flock's lifetime to cover the
// fast path and to be held from resolve until Launch returns — would put a
// file lock across the whole project-registry-guarded dispatch section, where
// it would interleave with Runner.WithProjectLock in a way nobody could read
// off the code. See workspaceHomeInFlight for what the registry does and does
// not cover.

// inFlightTestBackend is a sandbox backend that can be stopped in the middle of
// a Launch or in the middle of an extraction, so a test can hold one of the two
// racing operations open and observe what the other one does.
//
// It embeds the modelled-volume double the rest of the workspace-home tests use
// (bashWorkspaceInitBackend), so a dispatch through it leaves the home in the
// state production leaves it in and the migration operates on the same modelled
// volume store. Only Launch and ImportWorkspaceHome are overridden, and each
// only to add the gate.
type inFlightTestBackend struct {
	*bashWorkspaceInitBackend

	// launchGate, when non-nil, blocks Launch until it is closed. entered is
	// closed by the first Launch call, so a test can wait for the dispatch to
	// have got that far instead of sleeping.
	launchGate    chan struct{}
	launchEntered chan struct{}

	// removeGate/removeEntered do the same for the migration's FIRST step, the
	// removal that replaces the home volume.
	//
	// That is the point a test has to hold a migration at in order to see the
	// window this file is about: at step 1 the completion marker is still
	// intact and the volume still carries the identity it records, so a
	// dispatch arriving now takes resolveWorkspaceHome's FAST path — the one
	// that returns without touching the flock. Holding the migration any later
	// (at the extraction, say) means the marker is already gone, the dispatch
	// takes the slow path, and it blocks on the flock for reasons that have
	// nothing to do with the registry — a test gated there passes with or
	// without this fix.
	removeGate    chan struct{}
	removeEntered chan struct{}

	// failLaunch makes Launch fail AFTER passing the gate, so a test can put a
	// dispatch failure on the far side of resolveWorkspaceHome.
	failLaunch error

	mu       sync.Mutex
	launches int
	// launchOpts records the backend.LaunchOptions every Launch was handed.
	//
	// Recorded rather than discarded (codex review of PR8 round 2, Minor 1)
	// because WorkspaceHomeID is the only in-band evidence of WHICH incarnation
	// of the home volume a dispatch resolved. A test that instead compares the
	// volume's CURRENT identity against the migration's own request compares two
	// values the migration produced and the dispatch never touched, so it stays
	// green for a build whose dispatch resolved the pre-migration home — which is
	// precisely the bug the registry exists to prevent.
	launchOpts []backend.LaunchOptions
	// launchOnce/removeOnce keep a second call from closing an already-closed
	// "entered" channel.
	launchOnce sync.Once
	removeOnce sync.Once
}

var (
	_ backend.SandboxBackend = (*inFlightTestBackend)(nil)
	_ WorkspaceInitExecutor  = (*inFlightTestBackend)(nil)
	_ WorkspaceHomeImporter  = (*inFlightTestBackend)(nil)
)

func (b *inFlightTestBackend) Launch(_ context.Context, _ sandbox.Spec, opts backend.LaunchOptions) (backend.SandboxSession, error) {
	b.mu.Lock()
	b.launches++
	b.launchOpts = append(b.launchOpts, opts)
	fail := b.failLaunch
	b.mu.Unlock()

	if b.launchEntered != nil {
		b.launchOnce.Do(func() { close(b.launchEntered) })
	}
	if b.launchGate != nil {
		<-b.launchGate
	}
	if fail != nil {
		return nil, fail
	}
	return &gwFakeSession{id: "inflight-runtime"}, nil
}

func (b *inFlightTestBackend) launchCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.launches
}

// lastLaunch returns the options the most recent Launch was handed — i.e. what
// the DISPATCH decided, as opposed to what the migration or the modelled engine
// ended up holding.
func (b *inFlightTestBackend) lastLaunch(t *testing.T) backend.LaunchOptions {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.launchOpts) == 0 {
		t.Fatal("no Launch reached the backend")
	}
	return b.launchOpts[len(b.launchOpts)-1]
}

func (b *inFlightTestBackend) RemoveWorkspaceHomeVolume(ctx context.Context, volumeName string) (bool, error) {
	if b.removeEntered != nil {
		b.removeOnce.Do(func() { close(b.removeEntered) })
	}
	if b.removeGate != nil {
		<-b.removeGate
	}
	return b.bashWorkspaceInitBackend.RemoveWorkspaceHomeVolume(ctx, volumeName)
}

// newInFlightTestRunner builds a Runner that can BOTH dispatch a job and run a
// migration, against one modelled volume store.
//
// Every other test in this package needs only one of those two, which is
// exactly why this race had no coverage: gwFakeBackend can dispatch and cannot
// migrate, bashWorkspaceInitBackend can migrate and cannot dispatch.
func newInFlightTestRunner(t *testing.T, workspaceID string) (*Runner, *inFlightTestBackend) {
	t.Helper()
	be := &inFlightTestBackend{bashWorkspaceInitBackend: newBashWorkspaceInitBackend(t)}
	d := newGatewayTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	r := &Runner{
		DB: d.Conn,
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "proj-1", WorkDir: "/tmp", WorkspaceID: workspaceID},
		}},
		Backend:     be,
		BoidBinary:  "/boid",
		RuntimesDir: filepath.Join(t.TempDir(), "runtimes"),
	}
	return r, be
}

// inFlightTestSpec mirrors skillsSyncTestSpec: the smallest JobSpec Dispatch
// accepts.
func inFlightTestSpec() *orchestrator.JobSpec {
	return &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
	}
}

// waitFor blocks until ch is closed, failing the test rather than hanging the
// whole run if it never is.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestImportWorkspaceHome_RefusedWhileADispatchIsInFlight is Blocker 2's
// primary direction: while a dispatch has resolved this workspace's home and
// has not yet handed it to the engine, a migration must not destroy it.
//
// The dispatch is held inside Launch, which is the LAST moment the window is
// still open — from ContainerCreate on, the engine itself refuses to remove a
// volume a container references and the existing 409 path takes over.
//
// The refusal must also be side-effect free, for the same reason D4's in-use
// refusal is: an operator told "no" still has exactly the workspace they had.
func TestImportWorkspaceHome_RefusedWhileADispatchIsInFlight(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	r, be := newInFlightTestRunner(t, "myws")
	be.launchGate = make(chan struct{})
	be.launchEntered = make(chan struct{})

	dispatchDone := make(chan error, 1)
	go func() {
		_, derr := r.Dispatch(context.Background(), inFlightTestSpec(), nil)
		dispatchDone <- derr
	}()
	waitFor(t, be.launchEntered, "the dispatch to reach Launch")

	volume := dockerres.WorkspaceHomeVolumeName(r.InstallID, "myws")
	identityBefore := be.identityOf(t, volume)

	_, err := r.ImportWorkspaceHome(context.Background(), "myws", bytes.NewReader(importTestTar(t, map[string]os.FileMode{"a": 0o644})))
	if err == nil {
		t.Fatal("the migration ran while a dispatch was between resolving this workspace's home and creating its " +
			"container: the job container mounts the volume BY NAME, so it would have been started against the volume " +
			"the extraction is still writing into")
	}
	if !errors.Is(err, ErrWorkspaceHomeBusy) {
		t.Errorf("error = %v, want it to wrap ErrWorkspaceHomeBusy so the API can answer 409 rather than 500", err)
	}
	if !strings.Contains(err.Error(), "myws") {
		t.Errorf("error %q does not name the workspace", err)
	}

	// Side-effect free: nothing removed, nothing extracted, identity untouched.
	if n := be.removeCount(); n != 0 {
		t.Errorf("RemoveWorkspaceHomeVolume was called %d times despite the refusal", n)
	}
	if n := be.importCount(); n != 0 {
		t.Errorf("the extraction ran %d times despite the refusal", n)
	}
	if got := be.identityOf(t, volume); got != identityBefore {
		t.Errorf("the home volume identity changed to %q despite the refusal", got)
	}

	close(be.launchGate)
	if derr := <-dispatchDone; derr != nil {
		t.Fatalf("Dispatch: %v", derr)
	}
}

// TestImportWorkspaceHome_AcceptedOnceTheDispatchHasLaunched is the vacuity
// guard for the case above: the registration must be RELEASED, or the fix is
// "migrations stop working after the first dispatch" rather than mutual
// exclusion.
func TestImportWorkspaceHome_AcceptedOnceTheDispatchHasLaunched(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	r, _ := newInFlightTestRunner(t, "myws")

	if _, err := r.Dispatch(context.Background(), inFlightTestSpec(), nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if _, err := r.ImportWorkspaceHome(context.Background(), "myws", bytes.NewReader(importTestTar(t, map[string]os.FileMode{"a": 0o644}))); err != nil {
		t.Fatalf("the migration was refused after the dispatch had completed: %v", err)
	}
}

// TestDispatch_ReleasesTheHomeRegistrationOnEveryFailurePath is what keeps the
// registration from becoming a permanent one.
//
// A dispatch can fail at a dozen points between resolving the home and
// launching the container, and a registration leaked by any ONE of them would
// wedge every later migration of that workspace for the daemon's whole
// lifetime, with an error message about a job that is not running. The two
// cases below sit on opposite sides of resolveWorkspaceHome — one fails before
// it (the init script itself), one after (Launch) — so they witness the release
// covering the whole span rather than a single call site.
func TestDispatch_ReleasesTheHomeRegistrationOnEveryFailurePath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, r *Runner, be *inFlightTestBackend, configDir string)
	}{
		{
			name: "the init script fails (before the home is resolved)",
			setup: func(t *testing.T, _ *Runner, _ *inFlightTestBackend, configDir string) {
				writeInitScript(t, configDir, "myws", "#!/bin/bash\nexit 7\n")
			},
		},
		{
			name: "Launch fails (after the home is resolved)",
			setup: func(t *testing.T, _ *Runner, be *inFlightTestBackend, _ string) {
				be.launchGate = make(chan struct{})
				close(be.launchGate)
				be.failLaunch = errors.New("engine refused the container")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, configDir := setupWorkspaceHomeTestDirs(t)
			r, be := newInFlightTestRunner(t, "myws")
			tc.setup(t, r, be, configDir)

			if _, err := r.Dispatch(context.Background(), inFlightTestSpec(), nil); err == nil {
				t.Fatal("test wiring: this Dispatch was supposed to fail")
			}

			// The migration is the observer: if the failed dispatch left its
			// registration behind, this is refused.
			if _, err := r.ImportWorkspaceHome(context.Background(), "myws", bytes.NewReader(importTestTar(t, map[string]os.FileMode{"a": 0o644}))); err != nil {
				t.Fatalf("a migration was refused after a FAILED dispatch: %v\n"+
					"the dispatch leaked its in-flight registration, so every later migration of this workspace is "+
					"refused for the lifetime of the daemon", err)
			}
		})
	}
}

// TestDispatch_WaitsForAnInFlightMigration is the reverse direction, and the
// one where the choice between waiting and refusing matters.
//
// It waits. A migration is an operator standing at a terminal: refusing it
// costs one retry and destroys nothing, because the refusal lands before the
// volume is touched. A dispatch is not — it comes out of a hook evaluation, and
// "refused" there means a FAILED JOB, with the task-state consequences that
// carries, for a condition that resolves itself in seconds. The wait is also
// not a new behaviour: a dispatch whose marker does NOT match already blocks on
// the same workspace's flock for exactly as long as a concurrent init takes.
// This extends that to the fast path, where the only reason it did not block
// was that the fast path takes no lock.
//
// The second assertion is the one that makes the wait worth having: the
// dispatch that waited must resolve the home AFTER the migration finished, so
// it mounts the new incarnation and carries its identity. A dispatch that
// merely paused and then used its pre-migration answer would have gained
// nothing.
//
// That assertion is made against what the DISPATCH handed the backend
// (backend.LaunchOptions.WorkspaceHomeID), not against what the modelled engine
// currently holds (codex review of PR8 round 2, Minor 1). Comparing the volume's
// present identity with the migration's own request compares two values the
// migration itself produced: it is true however late the dispatch resolved, so
// it stayed green for a build that registered AFTER resolveWorkspaceHome — i.e.
// for a dispatch carrying the pre-migration identity into Launch, which is the
// exact defect this file exists to catch.
func TestDispatch_WaitsForAnInFlightMigration(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	r, be := newInFlightTestRunner(t, "myws")
	be.removeGate = make(chan struct{})
	be.removeEntered = make(chan struct{})

	// Settle the workspace first, so the dispatch below takes the FAST path —
	// the one that does not touch the flock, i.e. the interleaving the flock
	// alone never covered. Together with holding the migration at its FIRST
	// step (see removeGate), this is what makes the case non-vacuous: the
	// marker is still valid and the volume still carries the identity it
	// records, so nothing but the registry can make this dispatch wait.
	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("resolveWorkspaceHome (settling): %v", err)
	}

	migrationDone := make(chan error, 1)
	go func() {
		_, merr := r.ImportWorkspaceHome(context.Background(), "myws",
			bytes.NewReader(importTestTar(t, map[string]os.FileMode{"a": 0o644})))
		migrationDone <- merr
	}()
	waitFor(t, be.removeEntered, "the migration to reach its first destructive step")

	dispatchDone := make(chan error, 1)
	go func() {
		_, derr := r.Dispatch(context.Background(), inFlightTestSpec(), nil)
		dispatchDone <- derr
	}()

	// The dispatch must not have reached the engine while the migration is
	// still in flight. Time-based, because "has not happened yet" has no event
	// to wait for; generous enough that a slow machine does not flake, and it
	// only ever makes a BROKEN implementation look correct, never a correct one
	// look broken.
	time.Sleep(250 * time.Millisecond)
	if n := be.launchCount(); n != 0 {
		t.Fatalf("the dispatch reached Launch (%d calls) while a migration of this workspace was in flight: the job "+
			"container mounts the home volume BY NAME, so it would be started against the volume the migration is about "+
			"to replace and extract into", n)
	}
	select {
	case derr := <-dispatchDone:
		t.Fatalf("the dispatch finished (err=%v) while the migration was still in flight", derr)
	default:
	}

	close(be.removeGate)
	if merr := <-migrationDone; merr != nil {
		t.Fatalf("ImportWorkspaceHome: %v", merr)
	}
	select {
	case derr := <-dispatchDone:
		if derr != nil {
			t.Fatalf("the dispatch that waited for the migration then failed: %v", derr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the dispatch never resumed after the migration finished")
	}

	// It resolved the POST-migration home: same name, new incarnation.
	volume := dockerres.WorkspaceHomeVolumeName(r.InstallID, "myws")
	if be.launchCount() != 1 {
		t.Fatalf("Launch ran %d times, want 1", be.launchCount())
	}
	req := be.lastImport(t)
	if got := be.identityOf(t, volume); got != req.HomeID {
		t.Fatalf("test wiring: the home volume carries identity %q but the migration extracted into %q", got, req.HomeID)
	}
	// The load-bearing one: what the DISPATCH resolved and handed the engine.
	if got := be.lastLaunch(t).WorkspaceHomeID; got != req.HomeID {
		t.Errorf("the dispatch launched with WorkspaceHomeID %q, but the migration left the home volume at %q: the "+
			"dispatch resolved the home BEFORE the migration replaced it and carried a stale identity into Launch — the "+
			"registration must be in place before resolveWorkspaceHome, not after it", got, req.HomeID)
	}
}

// --- the registry itself ----------------------------------------------------
//
// The three cases below exercise workspaceHomeInFlight directly rather than
// through Dispatch/ImportWorkspaceHome. That is the right level for exactly one
// of them and a convenience for the other two:
//
// The migration-vs-migration case CANNOT be written end to end. Both migrations
// take the same flock, and holding the first one open anywhere past that point
// — which any gate in this file does — blocks the second inside a syscall that
// no context can interrupt, so the test would hang rather than fail. That is
// also why refusing the second one is a real improvement and not just tidiness:
// the pre-registry behaviour is "the second caller's HTTP request blocks,
// invisibly, for as long as the first migration takes", which for a 4.3GB
// extraction is minutes with no output.

func TestWorkspaceHomeInFlight_MigrationRefusesASecondMigration(t *testing.T) {
	var reg workspaceHomeInFlight

	done, err := reg.beginMigration("myws")
	if err != nil {
		t.Fatalf("beginMigration: %v", err)
	}

	if _, err := reg.beginMigration("myws"); !errors.Is(err, ErrWorkspaceHomeBusy) {
		t.Errorf("a second concurrent migration of the same workspace got err = %v, want ErrWorkspaceHomeBusy", err)
	}
	// A DIFFERENT workspace is unaffected: the registry keys on the slug, and a
	// registry that serialized the whole installation would make one 4.3GB
	// migration block every other workspace's jobs.
	otherDone, err := reg.beginMigration("otherws")
	if err != nil {
		t.Fatalf("a migration of an unrelated workspace was refused: %v", err)
	}
	otherDone()

	done()
	secondDone, err := reg.beginMigration("myws")
	if err != nil {
		t.Fatalf("a migration was refused after the previous one finished: %v", err)
	}
	secondDone()
}

// --- the third participant: `boid workspace remove` --------------------------
//
// codex round 2, Major 1. `workspace remove` deletes the workspace ROW first and
// the home VOLUME second (api.WorkspaceHandler.Remove), and until this PR it was
// outside the registry entirely. That is enough to produce the one thing the API
// layer's own comment says cannot happen — a HOME volume with no workspace row —
// with no crash and no engine failure involved:
//
//	import:  GetWorkspace("myws") -> ok
//	remove:                          delete the row, delete the volume
//	import:                                                            remove the
//	                                 volume (absent: a legitimate state, the
//	                                 never-dispatched-into case), create it,
//	                                 extract the operator's credentials into it
//	import:  200 OK
//
// What is left is a volume full of harness credentials that `boid workspace
// remove` can no longer delete (the row it keys on is gone, so it 404s), that
// shows up in `boid gc` as an orphan, and that no dispatch will ever mount.
//
// Closing it takes BOTH halves below, and neither is sufficient alone:
//
//   - the registry has to cover removals, so the two cannot overlap; and
//   - the migration has to CONFIRM the row while holding its registration,
//     because a removal that finished before the migration registered leaves
//     nothing for the registry to see.

func TestWorkspaceHomeInFlight_MigrationRefusedWhileARemovalIsInFlight(t *testing.T) {
	var reg workspaceHomeInFlight

	done, err := reg.beginRemoval("myws")
	if err != nil {
		t.Fatalf("beginRemoval: %v", err)
	}

	if _, err := reg.beginMigration("myws"); !errors.Is(err, ErrWorkspaceHomeBusy) {
		t.Errorf("a migration was accepted while `workspace remove` was deleting this workspace: err = %v, want "+
			"ErrWorkspaceHomeBusy. The migration would create a fresh HOME volume for a row that is being deleted, "+
			"leaving credentials in a volume nothing can dispatch into and `workspace remove` can no longer target", err)
	}
	// A different workspace is unaffected: removals key on the slug like
	// everything else here.
	otherDone, err := reg.beginMigration("otherws")
	if err != nil {
		t.Fatalf("a migration of an unrelated workspace was refused: %v", err)
	}
	otherDone()

	done()
	after, err := reg.beginMigration("myws")
	if err != nil {
		t.Fatalf("a migration was refused after the removal finished: %v", err)
	}
	after()
}

// TestWorkspaceHomeInFlight_RemovalRefusedWhileAMigrationIsInFlight is the other
// direction, and it REFUSES rather than waits — the opposite of what a dispatch
// does.
//
// The asymmetry is the same one beginDispatch's doc comment argues, applied to a
// third party. A dispatch comes out of hook evaluation and a refusal there is a
// failed job, so it waits. A removal is a person at a terminal, and the wait it
// would be signing up for is a multi-GB extraction — minutes of a command
// printing nothing. Refusing costs one retry, destroys nothing (the refusal
// lands before Service.RemoveWorkspace deletes the row), and says out loud that
// two contradictory intents were issued for one workspace.
func TestWorkspaceHomeInFlight_RemovalRefusedWhileAMigrationIsInFlight(t *testing.T) {
	var reg workspaceHomeInFlight

	done, err := reg.beginMigration("myws")
	if err != nil {
		t.Fatalf("beginMigration: %v", err)
	}

	if _, err := reg.beginRemoval("myws"); !errors.Is(err, ErrWorkspaceHomeBusy) {
		t.Errorf("`workspace remove` was accepted while a migration of the same workspace was running: err = %v, want "+
			"ErrWorkspaceHomeBusy", err)
	}

	done()
	after, err := reg.beginRemoval("myws")
	if err != nil {
		t.Fatalf("a removal was refused after the migration finished: %v", err)
	}
	after()
}

// TestWorkspaceHomeInFlight_RemovalAndDispatchDoNotExcludeEachOther pins a
// deliberate NON-exclusion, so a later reading of the registry does not "fix" it.
//
// A removal only DELETES, and the delete-only paths are the ones PR6's identity
// check genuinely covers: a dispatch whose home vanishes under it gets a fresh
// volume from its own ensureNamedVolumes carrying an identity that is not the
// resolved one, and fails loudly (verifyWorkspaceHomeIdentity). Making removals
// exclude dispatches would additionally turn today's documented part-completed
// outcome — the row goes, the volume's deletion is refused by the engine's 409
// and reported in the response — into a hard refusal of `workspace remove` for
// any workspace with a job starting in it.
func TestWorkspaceHomeInFlight_RemovalAndDispatchDoNotExcludeEachOther(t *testing.T) {
	var reg workspaceHomeInFlight

	releaseDispatch, err := reg.beginDispatch(context.Background(), "myws")
	if err != nil {
		t.Fatalf("beginDispatch: %v", err)
	}
	removalDone, err := reg.beginRemoval("myws")
	if err != nil {
		t.Fatalf("a removal was refused while a dispatch was in flight: %v", err)
	}
	removalDone()
	releaseDispatch()

	// And the reverse order.
	removalDone, err = reg.beginRemoval("myws")
	if err != nil {
		t.Fatalf("beginRemoval: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	releaseDispatch, err = reg.beginDispatch(ctx, "myws")
	if err != nil {
		t.Fatalf("a dispatch was blocked by an in-flight removal: %v", err)
	}
	releaseDispatch()
	removalDone()
}

// TestWorkspaceHomeInFlight_ConcurrentRemovalsAllRegister mirrors the dispatch
// counter's reasoning. Two `workspace remove` calls for one slug are harmless on
// their own (the second 404s on a row that is gone), so they are counted rather
// than refused — but the count has to be a count, or the first one to finish
// would clear the registration while the other is still deleting a volume.
func TestWorkspaceHomeInFlight_ConcurrentRemovalsAllRegister(t *testing.T) {
	var reg workspaceHomeInFlight

	first, err := reg.beginRemoval("myws")
	if err != nil {
		t.Fatalf("beginRemoval #1: %v", err)
	}
	second, err := reg.beginRemoval("myws")
	if err != nil {
		t.Fatalf("beginRemoval #2: %v", err)
	}

	first()
	if _, err := reg.beginMigration("myws"); !errors.Is(err, ErrWorkspaceHomeBusy) {
		t.Fatalf("a migration was accepted with one removal still in flight (err = %v)", err)
	}
	second()
	done, err := reg.beginMigration("myws")
	if err != nil {
		t.Fatalf("a migration was refused after every removal had released: %v", err)
	}
	done()
}

func TestWorkspaceHomeInFlight_DispatchWaitRespectsContextCancellation(t *testing.T) {
	var reg workspaceHomeInFlight

	done, err := reg.beginMigration("myws")
	if err != nil {
		t.Fatalf("beginMigration: %v", err)
	}
	defer done()

	// The wait is otherwise unbounded on purpose (see beginDispatch), so the
	// caller's context has to be the way out — a daemon shutting down, or a
	// dispatch whose own context was cancelled, must not be pinned by a
	// migration that is not making progress.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reg.beginDispatch(ctx, "myws"); !errors.Is(err, context.Canceled) {
		t.Errorf("beginDispatch with a cancelled context returned err = %v, want it to wrap context.Canceled", err)
	}
}

func TestWorkspaceHomeInFlight_ConcurrentDispatchesAllRegister(t *testing.T) {
	var reg workspaceHomeInFlight

	// The registration is a COUNT, not a flag: several jobs in one workspace
	// dispatch at once routinely, and a flag would let the first one to finish
	// clear the registration while the others were still between resolving
	// their home and creating their containers.
	releases := make([]func(), 0, 4)
	for i := 0; i < 4; i++ {
		release, err := reg.beginDispatch(context.Background(), "myws")
		if err != nil {
			t.Fatalf("beginDispatch #%d: %v", i, err)
		}
		releases = append(releases, release)
	}
	for i, release := range releases {
		if _, err := reg.beginMigration("myws"); !errors.Is(err, ErrWorkspaceHomeBusy) {
			t.Fatalf("a migration was accepted with %d dispatches still in flight (err = %v)", len(releases)-i, err)
		}
		release()
	}
	done, err := reg.beginMigration("myws")
	if err != nil {
		t.Fatalf("a migration was refused after every dispatch had released: %v", err)
	}
	done()
}
