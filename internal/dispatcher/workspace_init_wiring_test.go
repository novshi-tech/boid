package dispatcher

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"

	"github.com/novshi-tech/boid/internal/dockerres"
)

// TestWire_WorkspaceInit_ReachesTheContainerBackendAsAContainerRun is the
// wiring-seam guard for PR5, in the same family as PR2's
// TestWire_DataHomeDir_ReachesMarkerAndLockOnDisk and PR3's
// TestDispatch_EmbeddedSkills_ReachLaunchedSpecAsPerSkillReadOnlyBinds.
//
// Neither end's own tests can state this claim. workspace_init_test.go proves
// the wrapper does the right thing when bash runs it;
// container_backend_workspace_init_test.go proves RunWorkspaceInit asks the
// engine for the right container. Both would stay green if
// resolveWorkspaceHome never called a backend at all — which is exactly the
// shape of this repo's recurring "both ends wired, nothing crosses" failure.
// So this test drives the REAL resolveWorkspaceHome against the REAL container
// backend over a fake engine, and asserts that a workspace's own init.sh came
// out the far end, inside a container, addressed to that workspace's home.
func TestWire_WorkspaceInit_ReachesTheContainerBackendAsAContainerRun(t *testing.T) {
	_, configDir := setupWorkspaceHomeTestDirs(t)
	const marker = "echo unmistakable-init-marker\n"
	writeInitScript(t, configDir, "myws", marker)

	api := &fakeDockerAPI{ContainerWaitFunc: exitWith(0)}
	api.ContainerAttachFunc = attachThenEOF(api, "")
	dataHome := filepath.Join(t.TempDir(), "state")
	runtimesDir := filepath.Join(t.TempDir(), "runtime", "runtimes")
	r := Wire(WireConfig{DataHomeDir: dataHome, RuntimesDir: runtimesDir})
	r.Backend = NewContainerBackend(api, ContainerBackendOptions{InstallID: testWorkspaceInitInstallID})
	r.InstallID = testWorkspaceInitInstallID

	homeDir, slug, err := r.resolveWorkspaceHome(context.Background(), "myws")
	if err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", err)
	}
	if slug != "myws" {
		t.Fatalf("slug = %q, want %q", slug, "myws")
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.createCalls) != 1 {
		t.Fatalf("ContainerCreate called %d times, want exactly 1 — the init must run in a container, and one container covers prep + init.sh (§D1)", len(api.createCalls))
	}
	created := api.createCalls[0]

	if want := dockerres.WorkspaceInitContainerName(testWorkspaceInitInstallID, "myws"); created.Name != want {
		t.Errorf("container name = %q, want %q", created.Name, want)
	}

	// The home this resolve is about, mounted where a job will see its $HOME.
	var homeMount *mount.Mount
	for i := range created.HostConfig.Mounts {
		if created.HostConfig.Mounts[i].Source == homeDir {
			homeMount = &created.HostConfig.Mounts[i]
		}
	}
	if homeMount == nil {
		t.Fatalf("no mount of the resolved home %q; mounts = %+v", homeDir, created.HostConfig.Mounts)
	}
	if homeMount.Target != hostHomeDir() {
		t.Errorf("home mounted at %q, want %q — the path a job container sees its own $HOME at, so absolute paths an installer bakes in stay valid",
			homeMount.Target, hostHomeDir())
	}

	// The environment the contract promises, pointing at the CONTAINER's home
	// path rather than the daemon's view of it.
	env := map[string]string{}
	for _, kv := range created.Config.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	for k, want := range map[string]string{
		"HOME":                hostHomeDir(),
		"BOID_WORKSPACE_SLUG": "myws",
		"BOID_WORKSPACE_HOME": hostHomeDir(),
	} {
		if env[k] != want {
			t.Errorf("container env %s = %q, want %q", k, env[k], want)
		}
	}

	// ...and the workspace's actual script rode along on stdin.
	conn := api.lastAttachConn(t)
	conn.mu.Lock()
	stdin := string(joinBytes(conn.writes))
	conn.mu.Unlock()
	if !strings.Contains(stdin, marker) {
		t.Errorf("the workspace's init.sh never reached the container's stdin; stdin was:\n%s", stdin)
	}
	if !strings.Contains(stdin, workspaceInitHeredocPrefix) {
		t.Error("the script was not carried by the generated heredoc; something is concatenating it into the wrapper (§D2)")
	}
}

// TestResolveWorkspaceHome_NoBackendCapability_FailsLoud pins the other side of
// §D10's assertion-based discovery: a Runner whose backend cannot run an init
// container must fail the dispatch, not silently hand the agent an unprepared
// $HOME. The symptom of the silent version is the adapter's "CLI not found",
// which names the wrong culprit.
func TestResolveWorkspaceHome_NoBackendCapability_FailsLoud(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	r := &Runner{}

	_, _, err := r.resolveWorkspaceHome(context.Background(), "myws")
	if err == nil {
		t.Fatal("resolveWorkspaceHome succeeded with a backend that cannot prepare a workspace home")
	}
	if !strings.Contains(err.Error(), "init container") {
		t.Errorf("error does not explain what is missing:\n%v", err)
	}
}

// lastAttachConn returns the fakeAttachConn handed out by the most recent
// ContainerAttach. Only usable with attachThenEOF-style funcs that construct
// one per call.
func (f *fakeDockerAPI) lastAttachConn(t *testing.T) *fakeAttachConn {
	t.Helper()
	if f.lastConn == nil {
		t.Fatal("no attach connection was recorded")
	}
	return f.lastConn
}
