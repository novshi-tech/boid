package dispatcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/novshi-tech/boid/internal/dockerres"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// This file covers PR6 of docs/plans/workspace-home-volume-persistence.md:
// the workspace HOME stops being a directory under the (tmpfs-backed) runtimes
// root and becomes a per-workspace docker named volume.
//
// The properties that only exist after that switch live here; the ones PR2-PR5
// established and PR6 merely carries over (script hashing, the flock, the
// generation) stay in workspace_home_test.go, retargeted at the volume.

// --- the resolved value is a volume name, not a path ------------------------

// TestResolveWorkspaceHome_ReturnsThePerWorkspaceHomeVolumeName pins the whole
// point of PR6 at its narrowest: what resolveWorkspaceHome hands back is the
// name dockerres computes for this (install, slug) pair, and nothing about it
// is a filesystem path any more.
func TestResolveWorkspaceHome_ReturnsThePerWorkspaceHomeVolumeName(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	r, _ := newWorkspaceHomeTestRunnerWithBackend(t)
	r.InstallID = "install-abcdefgh12345"

	home, slug, _, err := r.resolveWorkspaceHome(context.Background(), "myws")
	if err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", err)
	}
	if slug != "myws" {
		t.Fatalf("slug = %q, want %q", slug, "myws")
	}
	want := dockerres.WorkspaceHomeVolumeName("install-abcdefgh12345", "myws")
	if home != want {
		t.Errorf("workspace home = %q, want the named volume %q", home, want)
	}
	if filepath.IsAbs(home) {
		t.Errorf("workspace home %q is still an absolute path; realization.classifySource keys named-volume "+
			"classification off exactly that (論点 e option (i))", home)
	}
}

// TestResolveWorkspaceHome_CreatesNoDaemonSideHomeDirectory pins the negative
// half of the same switch. The pre-PR6 implementation mkdir'd
// <runtimesRoot>/homes/<slug> on every call; leaving that behind would keep
// seeding empty directories on a root the daemon no longer uses for this, and
// would let a reader of the code believe the home still lives there.
func TestResolveWorkspaceHome_CreatesNoDaemonSideHomeDirectory(t *testing.T) {
	dataDir, _ := setupWorkspaceHomeTestDirs(t)
	r, _ := newWorkspaceHomeTestRunnerWithBackend(t)

	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", err)
	}

	homesDir := filepath.Join(dataDir, "boid", "homes")
	if _, err := os.Stat(homesDir); !os.IsNotExist(err) {
		entries, _ := os.ReadDir(homesDir)
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("resolveWorkspaceHome created %s (entries: %v); the home is a named volume as of PR6", homesDir, names)
	}
}

// --- the identity moved from a file inside the home to the volume's label ---

// TestResolveWorkspaceHome_MarkerRecordsTheVolumeIdentityAndSkeletonSet pins
// what a completion marker vouches for after PR6. HomeID is no longer the
// content of a file inside the home (nothing can read that without a
// container) but the identity label the volume itself carries, and
// SkeletonDirs records which bind-target skeleton the init that produced this
// home was asked to create.
func TestResolveWorkspaceHome_MarkerRecordsTheVolumeIdentityAndSkeletonSet(t *testing.T) {
	dataDir, _ := setupWorkspaceHomeTestDirs(t)
	r, be := newWorkspaceHomeTestRunnerWithBackend(t)

	vol, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws")
	if err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", err)
	}

	marker, ok, err := readWorkspaceHomeMarker(filepath.Join(dataDir, "boid", "homes-meta", "myws.init.json"))
	if err != nil || !ok {
		t.Fatalf("read marker: ok=%v err=%v", ok, err)
	}
	if want := be.identityOf(t, vol); marker.HomeID != want {
		t.Errorf("marker HomeID = %q, want the volume's identity label %q", marker.HomeID, want)
	}
	if marker.HomeID == "" {
		t.Fatal("marker HomeID is empty; a marker that vouches for nothing must not be written")
	}
	if got, want := marker.SkeletonDirs, workspaceHomeSkeletonDirs(); !equalStringSets(got, want) {
		t.Errorf("marker SkeletonDirs = %v, want %v", got, want)
	}
	if marker.InitGeneration != workspaceHomeInitGeneration {
		t.Errorf("marker InitGeneration = %d, want %d", marker.InitGeneration, workspaceHomeInitGeneration)
	}
}

// TestResolveWorkspaceHome_VolumeDeletedAndRecreated_ReInitializes is the
// accident this identity exists to catch, and the one that actually happens:
// a stray `docker volume rm`, a reap misfire, a half-completed workspace
// remove. The volume comes back empty with a fresh identity, so the surviving
// marker must not be allowed to skip init over it.
func TestResolveWorkspaceHome_VolumeDeletedAndRecreated_ReInitializes(t *testing.T) {
	_, configDir := setupWorkspaceHomeTestDirs(t)
	writeInitScript(t, configDir, "myws", "#!/bin/bash\necho x >> \"$BOID_WORKSPACE_HOME/counter\"\n")
	r, be := newWorkspaceHomeTestRunnerWithBackend(t)

	vol, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws")
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	firstID := be.identityOf(t, vol)

	be.removeVolume(t, vol)

	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if got := be.identityOf(t, vol); got == firstID {
		t.Fatalf("the re-created volume kept identity %q; the test fixture is not modelling a new volume", got)
	}
	if lines := countLines(t, filepath.Join(be.dirFor(vol), "counter")); lines != 1 {
		t.Errorf("counter lines = %d, want 1 — the re-created volume must have been initialized from scratch", lines)
	}
	if be.runCount() != 2 {
		t.Errorf("init ran %d times, want 2 (once per volume incarnation)", be.runCount())
	}
}

// TestResolveWorkspaceHome_UnchangedVolume_SkipsInitWithoutAContainer pins the
// fast path PR6 has to keep fast. Reading the identity is ONE VolumeCreate —
// idempotent, and on an existing volume it returns that volume's own labels
// (measured against podman 4.9.3, see the plan doc's 論点 b) — so a settled
// workspace never starts a container just to find out it is settled.
func TestResolveWorkspaceHome_UnchangedVolume_SkipsInitWithoutAContainer(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	r, be := newWorkspaceHomeTestRunnerWithBackend(t)

	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if got := be.runCount(); got != 1 {
		t.Errorf("init container runs = %d, want 1 — a matching marker must short-circuit before any container starts", got)
	}
}

// TestResolveWorkspaceHome_VolumeWithoutAnIdentityLabel_FailsLoud covers the
// one state the fail-safe "re-init on any doubt" rule cannot resolve. Every
// path in boid that creates a workspace HOME volume stamps an identity on it,
// so a volume without one was made by somebody else — and the Engine API has
// no way to add a label to an existing volume, so re-initializing would run
// the init on EVERY dispatch forever without ever converging. A non-converging
// fail-safe is a livelock, so this stops and says so.
func TestResolveWorkspaceHome_VolumeWithoutAnIdentityLabel_FailsLoud(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	r, be := newWorkspaceHomeTestRunnerWithBackend(t)
	r.InstallID = "inst1234"
	vol := dockerres.WorkspaceHomeVolumeName("inst1234", "myws")
	be.seedUnlabeledVolume(t, vol)

	_, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws")
	if err == nil {
		t.Fatal("resolveWorkspaceHome succeeded against a volume carrying no identity label")
	}
	if !strings.Contains(err.Error(), vol) {
		t.Errorf("error does not name the offending volume:\n%v", err)
	}
	if !strings.Contains(err.Error(), dockerres.LabelWorkspaceHomeID) {
		t.Errorf("error does not name the missing label:\n%v", err)
	}
	if be.runCount() != 0 {
		t.Errorf("init ran %d times against a volume boid cannot vouch for; want 0", be.runCount())
	}
}

// baseSkeletonDirsForTest is the skeleton every real dispatch creates: each
// skill discovery root and its parent. Cases that swap in a custom skeleton
// build on top of it rather than replacing it outright, because the prelude's
// symlink step writes into those roots — a skeleton that omits one is not a
// smaller skeleton, it is a broken init.
func baseSkeletonDirsForTest() []string {
	var dirs []string
	for _, root := range skillDiscoveryRoots {
		dirs = append(dirs, filepath.Dir(root), root)
	}
	return dirs
}

// --- the skeleton set is now part of what the marker vouches for ------------

// TestResolveWorkspaceHome_SkeletonSetChanged_ReInitializes is PR6's
// replacement for the daemon-side per-dispatch mkdir it removes.
//
// The bind-target skeleton is a property of the BOID BINARY (one directory per
// embedded skill), while the completion marker used to cover only init.sh. A
// release that adds a skill therefore added a bind target to a home whose
// marker still matched — and an absent bind target is auto-created by the
// container engine as uid 0, locking the harness out of ~/.claude for good
// (measured; 論点 b-2). Until PR6 the daemon papered over that by re-creating
// the whole skeleton itself on every dispatch, which a named volume makes
// impossible. So the set has to be part of what the marker vouches for.
func TestResolveWorkspaceHome_SkeletonSetChanged_ReInitializes(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	r, be := newWorkspaceHomeTestRunnerWithBackend(t)

	// Every real discovery root, plus one synthetic entry that stands in for
	// whatever a future release might add. The roots have to be there: the
	// prelude's symlink step writes into them, so a skeleton omitting one
	// makes `ln -sfn` fail and the init container exit at stage "prelude"
	// rather than reaching the marker this case is about.
	base := baseSkeletonDirsForTest()
	restore := swapSkeletonDirs(t, append(append([]string(nil), base...), ".boid-test/skeleton-a"))
	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if be.runCount() != 1 {
		t.Fatalf("init ran %d times on a fresh volume, want 1", be.runCount())
	}
	restore()

	// A release that needs one more directory staged.
	swapSkeletonDirs(t, append(append([]string(nil), base...), ".boid-test/skeleton-a", ".boid-test/skeleton-b"))
	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if got := be.runCount(); got != 2 {
		t.Errorf("init ran %d times, want 2 — a home whose skeleton predates a newly added skill must be re-prepared", got)
	}
	if got := be.lastRequest(t).SkeletonDirs; !contains(got, ".boid-test/skeleton-b") {
		t.Errorf("the re-run was not asked to create the newly added directory; SkeletonDirs = %v", got)
	}
}

// TestResolveWorkspaceHome_SkeletonSetReordered_DoesNotReInitialize pins that
// the comparison is over a SET. workspaceHomeSkeletonDirs' order comes from
// embed.FS's directory listing; a reordering there is not a change to what the
// home needs, and treating it as one would re-run every workspace's init.sh
// across an entire installation for nothing.
func TestResolveWorkspaceHome_SkeletonSetReordered_DoesNotReInitialize(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	r, be := newWorkspaceHomeTestRunnerWithBackend(t)

	base := baseSkeletonDirsForTest()
	ordered := append(append([]string(nil), base...), ".boid-test/a", ".boid-test/b")
	restore := swapSkeletonDirs(t, ordered)
	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	restore()

	reordered := append([]string(nil), ordered...)
	for i, j := 0, len(reordered)-1; i < j; i, j = i+1, j-1 {
		reordered[i], reordered[j] = reordered[j], reordered[i]
	}
	swapSkeletonDirs(t, reordered)
	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if got := be.runCount(); got != 1 {
		t.Errorf("init ran %d times, want 1 — the same set in a different order is not a change", got)
	}
}

// --- the wiring seam: one volume name, both containers ----------------------

// TestWire_WorkspaceHomeVolume_ReachesBothTheInitAndTheJobContainerMounts is
// the seam guard this PR most needs.
//
// Each end has its own tests — resolveWorkspaceHome returns a volume name,
// workspaceInitHomeMount builds a volume mount, homeMounts puts the value it is
// given into the HOME mount — and all of them would stay green if the two
// containers ended up mounting DIFFERENT volumes, which is this repository's
// recurring "both ends wired, nothing crosses" failure. So this drives one real
// resolve through the real container backend and then launches a real job spec
// built from that same resolve, and asserts both ContainerCreate calls carry
// the same named volume at the same target.
func TestWire_WorkspaceHomeVolume_ReachesBothTheInitAndTheJobContainerMounts(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)

	api := &fakeDockerAPI{ContainerWaitFunc: exitWith(0)}
	api.ContainerAttachFunc = attachThenEOF(api, "")
	r := Wire(WireConfig{
		DataHomeDir: filepath.Join(t.TempDir(), "state"),
		RuntimesDir: filepath.Join(t.TempDir(), "runtime", "runtimes"),
	})
	be := NewContainerBackend(api, ContainerBackendOptions{InstallID: testWorkspaceInitInstallID})
	r.Backend = be
	r.InstallID = testWorkspaceInitInstallID

	vol, slug, homeID, err := r.resolveWorkspaceHome(context.Background(), "myws")
	if err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", err)
	}
	if homeID == "" {
		t.Fatal("resolveWorkspaceHome returned no home identity; the seam below would not be crossed")
	}

	spec, err := BuildSandboxSpec(&orchestrator.JobSpec{}, SandboxRuntimeInfo{
		WorkspaceHomeVolume: vol,
		WorkspaceSlug:       slug,
	})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	// The identity travels with the launch exactly as Runner.Dispatch sends it,
	// so this case also proves the two ends AGREE: the job path re-checks the
	// volume's label against it (verifyWorkspaceHomeIdentity), and a Launch that
	// stamped its own freshly minted identity over the resolved one — or a
	// resolve that reported an identity the volume does not carry — fails here
	// rather than silently mounting a home nothing prepared.
	mustLaunch(t, be, spec, backend.LaunchOptions{
		JobID: "job-1", Workspace: "myws", WorkspaceSlug: slug, WorkspaceHomeID: homeID,
	})

	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.createCalls) != 2 {
		t.Fatalf("ContainerCreate calls = %d, want 2 (one init container, one job container)", len(api.createCalls))
	}
	for i, label := range []string{"init container", "job container"} {
		m := findMountByTarget(api.createCalls[i].HostConfig.Mounts, hostHomeDir())
		if m == nil {
			t.Fatalf("%s has no mount at %s; mounts = %+v", label, hostHomeDir(), api.createCalls[i].HostConfig.Mounts)
		}
		if m.Type != mount.TypeVolume {
			t.Errorf("%s HOME mount type = %q, want %q", label, m.Type, mount.TypeVolume)
		}
		if m.Source != vol {
			t.Errorf("%s HOME mount source = %q, want the resolved volume %q", label, m.Source, vol)
		}
	}
}

func findMountByTarget(mounts []mount.Mount, target string) *mount.Mount {
	for i := range mounts {
		if mounts[i].Target == target {
			return &mounts[i]
		}
	}
	return nil
}

// --- the workspace-home volume label carries the NORMALIZED slug ------------

// TestLaunch_WorkspaceHomeVolumeLabel_UsesTheNormalizedSlug pins 論点 D5. The
// volume NAME is built from the normalized slug (resolveWorkspaceHome is the
// only place that normalization happens), while LaunchOptions.Workspace is the
// raw project WorkspaceID — empty for every project with no explicit workspace.
// Left unfixed, an unassigned project produces a volume named ...-default whose
// boid.workspace_home label value is the empty string, and PR7's
// volume-to-workspace lookup would simply not find it.
func TestLaunch_WorkspaceHomeVolumeLabel_UsesTheNormalizedSlug(t *testing.T) {
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{InstallID: "install-xyz"})

	homeVol := dockerres.WorkspaceHomeVolumeName("install-xyz", "default")
	spec := sandbox.Spec{
		ID:     "job-default-ws",
		Argv:   []string{"true"},
		Mounts: []sandbox.Mount{{Source: homeVol, Target: "/home/boid", Type: sandbox.MountBind}},
	}
	// Workspace is the RAW WorkspaceID (a project with no workspace assigned),
	// WorkspaceSlug is what resolveWorkspaceHome normalized it to.
	mustLaunch(t, be, spec, backend.LaunchOptions{JobID: "job-default-ws", Workspace: "", WorkspaceSlug: "default"})

	if len(api.volumeCreateCalls) != 1 {
		t.Fatalf("VolumeCreate calls = %d, want 1", len(api.volumeCreateCalls))
	}
	if got := api.volumeCreateCalls[0].Labels[dockerres.LabelWorkspaceHome]; got != "default" {
		t.Errorf("Labels[%q] = %q, want %q — the label must carry the normalized slug the volume NAME was built from",
			dockerres.LabelWorkspaceHome, got, "default")
	}
}

// TestLaunch_WorkspaceHomeVolumeCreatedByLaunch_CarriesAnIdentity pins the
// invariant resolveWorkspaceHome's fail-loud branch rests on: EVERY boid path
// that can bring a workspace HOME volume into existence stamps an identity on
// it. Launch is the second such path — resolveWorkspaceHome already ensured the
// volume, but it can be removed in the window before ContainerCreate, and an
// unlabeled volume left behind here would wedge every later dispatch of that
// workspace.
func TestLaunch_WorkspaceHomeVolumeCreatedByLaunch_CarriesAnIdentity(t *testing.T) {
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{InstallID: "install-xyz"})

	homeVol := dockerres.WorkspaceHomeVolumeName("install-xyz", "myws")
	spec := sandbox.Spec{
		ID:     "job-1",
		Argv:   []string{"true"},
		Mounts: []sandbox.Mount{{Source: homeVol, Target: "/home/boid", Type: sandbox.MountBind}},
	}
	mustLaunch(t, be, spec, backend.LaunchOptions{JobID: "job-1", Workspace: "myws", WorkspaceSlug: "myws"})

	if len(api.volumeCreateCalls) != 1 {
		t.Fatalf("VolumeCreate calls = %d, want 1", len(api.volumeCreateCalls))
	}
	if got := api.volumeCreateCalls[0].Labels[dockerres.LabelWorkspaceHomeID]; got == "" {
		t.Errorf("Launch created a workspace HOME volume with no %q label; resolveWorkspaceHome would then fail every "+
			"later dispatch of this workspace with nothing able to repair it", dockerres.LabelWorkspaceHomeID)
	}
}

// --- the identity is re-checked where the volume is actually USED -----------
//
// resolveWorkspaceHome compares the volume's identity against the completion
// marker, but that comparison is over by the time either container is created.
// Between the two, the volume can be removed and a fresh, empty, unlabelled one
// put in its place by the engine's own implicit create. Both use sites
// therefore re-check their OWN VolumeCreate's answer — see
// containerBackend.ensureNamedVolumes and RunWorkspaceInit for the window that
// remains open after that, and why narrowing it is still worth doing.

// TestRunWorkspaceInit_HomeVolumeReplacedSinceResolve_FailsBeforeStartingAContainer
// covers the init path. Preparing a volume whose identity is not the one the
// caller resolved means the completion marker written afterwards records an
// identity that describes SOMEBODY ELSE'S contents, so the very next dispatch
// re-runs a full init — silently, and again on every dispatch after any repeat
// of the same accident.
func TestRunWorkspaceInit_HomeVolumeReplacedSinceResolve_FailsBeforeStartingAContainer(t *testing.T) {
	api := &fakeDockerAPI{ContainerWaitFunc: exitWith(0)}
	api.ContainerAttachFunc = attachThenEOF(api, "")
	b := newWorkspaceInitBackend(api)
	req := testWorkspaceInitRequest()

	// The volume the caller resolved is gone; a different incarnation holds
	// the name now.
	api.seedVolume(req.HomeSource, map[string]string{
		dockerres.LabelWorkspaceHome:   req.Slug,
		dockerres.LabelWorkspaceHomeID: "a-different-incarnation",
	})

	err := b.RunWorkspaceInit(context.Background(), req)
	if err == nil {
		t.Fatal("RunWorkspaceInit prepared a home volume whose identity is not the one the caller resolved")
	}
	for _, want := range []string{req.HomeSource, req.HomeID, "a-different-incarnation"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
	if len(api.createCalls) != 0 {
		t.Errorf("an init container was created anyway (%d creates); the run must stop before it prepares the wrong home",
			len(api.createCalls))
	}
}

// TestRunWorkspaceInit_HomeVolumeWithoutAnIdentity_Fails is the same check's
// other input. An unlabelled volume under a boid-owned name was made by
// something else, and preparing it would stamp a marker whose home_id names an
// identity the volume does not carry — after which
// Runner.ensureWorkspaceHomeVolume hard-fails every later dispatch of this
// workspace, with nothing able to repair it (the Engine API cannot add a label
// to an existing volume).
func TestRunWorkspaceInit_HomeVolumeWithoutAnIdentity_Fails(t *testing.T) {
	api := &fakeDockerAPI{ContainerWaitFunc: exitWith(0)}
	api.ContainerAttachFunc = attachThenEOF(api, "")
	b := newWorkspaceInitBackend(api)
	req := testWorkspaceInitRequest()
	api.seedVolume(req.HomeSource, nil)

	err := b.RunWorkspaceInit(context.Background(), req)
	if err == nil {
		t.Fatal("RunWorkspaceInit prepared a home volume carrying no identity label")
	}
	if !strings.Contains(err.Error(), dockerres.LabelWorkspaceHomeID) {
		t.Errorf("error does not name the missing label:\n%v", err)
	}
	if len(api.createCalls) != 0 {
		t.Errorf("an init container was created anyway (%d creates)", len(api.createCalls))
	}
}

// TestLaunch_WorkspaceHomeVolumeReplacedSinceResolve_FailsLoud covers the job
// path, and it is the more consequential of the two: what the job container
// would otherwise mount is a home nothing ever prepared. The ownership check in
// the runner (verifyHomeSkeleton) catches the case where the engine had to
// create ~/.claude as uid 0, but a volume whose contents happen to satisfy that
// check passes it — and the harness then starts against a $HOME the completion
// marker never vouched for.
func TestLaunch_WorkspaceHomeVolumeReplacedSinceResolve_FailsLoud(t *testing.T) {
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{InstallID: "install-xyz"})

	homeVol := dockerres.WorkspaceHomeVolumeName("install-xyz", "myws")
	api.seedVolume(homeVol, map[string]string{
		dockerres.LabelWorkspaceHome:   "myws",
		dockerres.LabelWorkspaceHomeID: "a-different-incarnation",
	})
	spec := sandbox.Spec{
		ID:     "job-1",
		Argv:   []string{"true"},
		Mounts: []sandbox.Mount{{Source: homeVol, Target: "/home/boid", Type: sandbox.MountBind}},
	}

	_, err := be.Launch(context.Background(), spec, backend.LaunchOptions{
		JobID: "job-1", Workspace: "myws", WorkspaceSlug: "myws",
		WorkspaceHomeID: "the-one-that-was-resolved",
	})
	if err == nil {
		t.Fatal("Launch mounted a workspace HOME volume that is not the incarnation the dispatch resolved")
	}
	for _, want := range []string{homeVol, "the-one-that-was-resolved", "a-different-incarnation"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
	if len(api.createCalls) != 0 {
		t.Errorf("the job container was created anyway (%d creates)", len(api.createCalls))
	}
}

// TestLaunch_WorkspaceHomeVolumeVanishedSinceResolve_FailsLoud is the accident
// that actually produces the above: the volume is simply GONE by the time
// Launch runs, so ensureNamedVolumes' own create is the one that brings it
// back — brand new, empty, and carrying the identity this call minted rather
// than the one the marker vouches for.
//
// Failing here rather than mounting it is what keeps the marker honest: the
// next dispatch's resolve sees the new identity, disagrees with the marker, and
// re-runs init. Mounting it would instead hand the agent an empty $HOME while
// the marker still claimed the home was prepared.
func TestLaunch_WorkspaceHomeVolumeVanishedSinceResolve_FailsLoud(t *testing.T) {
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{InstallID: "install-xyz"})

	homeVol := dockerres.WorkspaceHomeVolumeName("install-xyz", "myws")
	spec := sandbox.Spec{
		ID:     "job-1",
		Argv:   []string{"true"},
		Mounts: []sandbox.Mount{{Source: homeVol, Target: "/home/boid", Type: sandbox.MountBind}},
	}

	_, err := be.Launch(context.Background(), spec, backend.LaunchOptions{
		JobID: "job-1", Workspace: "myws", WorkspaceSlug: "myws",
		WorkspaceHomeID: "the-one-that-was-resolved",
	})
	if err == nil {
		t.Fatal("Launch mounted a workspace HOME volume it had just created itself, empty and never initialized")
	}
	if len(api.volumeCreateCalls) != 1 {
		t.Fatalf("VolumeCreate calls = %d, want 1", len(api.volumeCreateCalls))
	}
	// The replacement still gets an identity of its own: leaving it unlabelled
	// would wedge the workspace permanently (Runner.ensureWorkspaceHomeVolume
	// cannot repair a missing label), whereas a fresh identity resolves itself
	// with one re-init.
	if got := api.volumeCreateCalls[0].Labels[dockerres.LabelWorkspaceHomeID]; got == "" {
		t.Errorf("the volume Launch created carries no %q label", dockerres.LabelWorkspaceHomeID)
	} else if got == "the-one-that-was-resolved" {
		t.Errorf("Launch stamped the RESOLVED identity onto a volume it created itself; the mismatch that makes an " +
			"empty home detectable would be erased by exactly that")
	}
	if len(api.createCalls) != 0 {
		t.Errorf("the job container was created anyway (%d creates)", len(api.createCalls))
	}
}

// TestLaunch_WorkspaceHomeVolumeRemovedBetweenDispatches_FailsLoud is the same
// accident as the case above, reached the way it actually happens: not "the
// volume was never there", but "boid used it, something removed it, boid used
// the name again". `docker volume rm` between two dispatches of the same
// workspace is the whole reason the identity exists.
//
// It is also the case that pins the fake engine's volume store as a STORE.
// VolumeCreate answering with an existing volume's own labels is what makes the
// first Launch succeed and what a removal has to undo; a VolumeRemove that
// records the call without touching the store leaves the removed volume
// answering for its own name forever, so this dispatch would be reported as
// ordinary while a real engine hands boid a brand-new empty home (codex review
// of PR6, Minor 1).
func TestLaunch_WorkspaceHomeVolumeRemovedBetweenDispatches_FailsLoud(t *testing.T) {
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{InstallID: "install-xyz"})

	homeVol := dockerres.WorkspaceHomeVolumeName("install-xyz", "myws")
	api.seedVolume(homeVol, map[string]string{
		dockerres.LabelWorkspaceHome:   "myws",
		dockerres.LabelWorkspaceHomeID: "resolved-identity",
	})
	launch := func(jobID string) error {
		_, err := be.Launch(context.Background(), sandbox.Spec{
			ID:     jobID,
			Argv:   []string{"true"},
			Mounts: []sandbox.Mount{{Source: homeVol, Target: "/home/boid", Type: sandbox.MountBind}},
		}, backend.LaunchOptions{
			JobID: jobID, Workspace: "myws", WorkspaceSlug: "myws",
			WorkspaceHomeID: "resolved-identity",
		})
		return err
	}

	// Negative control: the settled workspace dispatches normally.
	if err := launch("job-1"); err != nil {
		t.Fatalf("first Launch against the resolved home volume: %v", err)
	}

	// `docker volume rm boid-ws-home-install-xyz-myws`, from outside boid.
	if _, err := api.VolumeRemove(context.Background(), homeVol, client.VolumeRemoveOptions{}); err != nil {
		t.Fatalf("VolumeRemove: %v", err)
	}

	err := launch("job-2")
	if err == nil {
		t.Fatal("Launch mounted a workspace HOME volume that had been removed since it was resolved; the engine " +
			"would have re-created it empty, with an identity nothing vouches for")
	}
	if !strings.Contains(err.Error(), "resolved-identity") {
		t.Errorf("error does not mention the resolved identity it could not find:\n%v", err)
	}
}

// TestLaunch_WorkspaceHomeVolumeUnchanged_Proceeds is the negative control the
// three cases above need: the check must not fail the ordinary dispatch, in
// which resolveWorkspaceHome created (or found) the volume moments earlier and
// it is still there.
func TestLaunch_WorkspaceHomeVolumeUnchanged_Proceeds(t *testing.T) {
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{InstallID: "install-xyz"})

	homeVol := dockerres.WorkspaceHomeVolumeName("install-xyz", "myws")
	api.seedVolume(homeVol, map[string]string{
		dockerres.LabelWorkspaceHome:   "myws",
		dockerres.LabelWorkspaceHomeID: "resolved-identity",
	})
	spec := sandbox.Spec{
		ID:     "job-1",
		Argv:   []string{"true"},
		Mounts: []sandbox.Mount{{Source: homeVol, Target: "/home/boid", Type: sandbox.MountBind}},
	}

	mustLaunch(t, be, spec, backend.LaunchOptions{
		JobID: "job-1", Workspace: "myws", WorkspaceSlug: "myws",
		WorkspaceHomeID: "resolved-identity",
	})
	if len(api.createCalls) != 1 {
		t.Errorf("ContainerCreate calls = %d, want 1", len(api.createCalls))
	}
}

// --- helpers ----------------------------------------------------------------

// swapSkeletonDirs replaces the bind-target skeleton generator for the rest of
// the test (or until the returned function is called, for tests that need two
// different sets in one run) and restores it afterwards.
//
// The real set is decided by the embed directive, so within a single test
// binary it is a constant. "A release added an embedded skill" is therefore not
// reachable without moving this seam — and it is the exact input the marker's
// skeleton record exists to notice.
func swapSkeletonDirs(t *testing.T, dirs []string) (restore func()) {
	t.Helper()
	prev := workspaceHomeSkeletonDirs
	restored := false
	restore = func() {
		if !restored {
			workspaceHomeSkeletonDirs = prev
			restored = true
		}
	}
	workspaceHomeSkeletonDirs = func() []string { return append([]string(nil), dirs...) }
	t.Cleanup(restore)
	return restore
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
