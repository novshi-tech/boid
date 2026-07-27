package dispatcher

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// bashWorkspaceInitBackend is a backend.SandboxBackend whose only real
// capability is WorkspaceInitExecutor: it runs the wrapper resolveWorkspaceHome
// assembled through a REAL bash, in this test process, instead of in a
// throwaway container.
//
// It exists so the semantic coverage the pre-PR5 workspace_home_test.go had —
// the marker records the bytes that ran, the env is what the contract says,
// a failing script fails the dispatch with its own exit code and output tail,
// an unchanged script does not re-run, the identity file re-settles after
// tampering — survives the move into a container. A fake that merely recorded
// the request and returned nil would keep every one of those tests GREEN while
// the wrapper itself was syntactically broken, since none of them looks at the
// wrapper: they look at what running it did to the home.
//
// It also models the DOCKER VOLUME the home lives in as of PR6: a directory
// under a per-backend t.TempDir(), with the volume's identity label held
// alongside it. Modelling the identity rather than returning a constant is what
// keeps these tests describing production — VolumeCreate is idempotent and
// reports the EXISTING volume's label, which is the entire basis for "a settled
// workspace skips init" and for "a deleted and re-created volume does not".
//
// What it cannot stand in for is the container boundary, and it does not
// pretend to. There is no mount, so the wrapper reaches the modelled volume
// only because this backend rewrites the two home-valued environment variables
// from HomeTarget to that directory before executing (see RunWorkspaceInit).
// What boid asks the ENGINE for — image, name, labels, uid, userns, network,
// mount — is pinned separately against a fake docker API in
// container_backend_workspace_init_test.go, and the two are tied together by
// the wiring tests in workspace_init_wiring_test.go and
// workspace_home_volume_test.go.
type bashWorkspaceInitBackend struct {
	mu       sync.Mutex
	requests []WorkspaceInitRequest
	// volRoot stands in for the engine's volume store; volumes maps a volume
	// name to the identity label it carries. A name present with an EMPTY
	// identity models a volume that exists but was not created by boid (see
	// seedUnlabeledVolume).
	volRoot string
	volumes map[string]string
}

var (
	_ backend.SandboxBackend = (*bashWorkspaceInitBackend)(nil)
	_ WorkspaceInitExecutor  = (*bashWorkspaceInitBackend)(nil)
)

func newBashWorkspaceInitBackend(t *testing.T) *bashWorkspaceInitBackend {
	t.Helper()
	return &bashWorkspaceInitBackend{volRoot: t.TempDir(), volumes: map[string]string{}}
}

// newWorkspaceHomeTestRunner is the successor of the bare `&Runner{}` every
// resolveWorkspaceHome test used before PR5. A Runner with no Backend can no
// longer prepare a workspace home at all — see workspaceInitExecutorFor for
// why that is a hard error rather than a fallback — so the minimal wiring for
// these tests is now "a Runner that can run an init".
func newWorkspaceHomeTestRunner(t *testing.T) *Runner {
	t.Helper()
	r, _ := newWorkspaceHomeTestRunnerWithBackend(t)
	return r
}

// newWorkspaceHomeTestRunnerWithBackend is newWorkspaceHomeTestRunner for the
// tests that also need to reach the modelled engine — to look inside a volume,
// remove one, or check what identity it carries.
func newWorkspaceHomeTestRunnerWithBackend(t *testing.T) (*Runner, *bashWorkspaceInitBackend) {
	t.Helper()
	be := newBashWorkspaceInitBackend(t)
	return &Runner{Backend: be}, be
}

// EnsureWorkspaceHomeVolume models VolumeCreate: idempotent, and for a name
// that already exists it returns THAT volume's identity rather than the
// candidate offered now.
func (b *bashWorkspaceInitBackend) EnsureWorkspaceHomeVolume(_ context.Context, req WorkspaceHomeVolumeRequest) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if id, ok := b.volumes[req.Name]; ok {
		return id, nil
	}
	if err := os.MkdirAll(filepath.Join(b.volRoot, req.Name), 0o700); err != nil {
		return "", err
	}
	b.volumes[req.Name] = req.CandidateID
	return req.CandidateID, nil
}

// workspaceHomeDirOf is the bridge every pre-PR6 test needs: resolveWorkspaceHome
// now returns a VOLUME NAME, and a test that wants to look at what the init put
// in the home has to ask the modelled engine where that volume's contents are.
// The type assertion is the point — a test wired with some other backend has no
// business reading a directory that would then be somebody else's.
func workspaceHomeDirOf(t *testing.T, r *Runner, volumeName string) string {
	t.Helper()
	be, ok := r.Backend.(*bashWorkspaceInitBackend)
	if !ok {
		t.Fatalf("runner backend is %T, not the modelled-volume test backend", r.Backend)
	}
	return be.dirFor(volumeName)
}

// dirFor is the directory standing in for a volume's contents.
func (b *bashWorkspaceInitBackend) dirFor(volumeName string) string {
	return filepath.Join(b.volRoot, volumeName)
}

// identityOf reports the identity label the modelled volume carries.
func (b *bashWorkspaceInitBackend) identityOf(t *testing.T, volumeName string) string {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	id, ok := b.volumes[volumeName]
	if !ok {
		t.Fatalf("no volume named %q was ever created", volumeName)
	}
	return id
}

// removeVolume models `docker volume rm`: the volume object and its contents
// both go, and the next EnsureWorkspaceHomeVolume creates a NEW one with a new
// identity.
func (b *bashWorkspaceInitBackend) removeVolume(t *testing.T, volumeName string) {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := os.RemoveAll(filepath.Join(b.volRoot, volumeName)); err != nil {
		t.Fatalf("remove modelled volume %q: %v", volumeName, err)
	}
	delete(b.volumes, volumeName)
}

// seedUnlabeledVolume models the one state boid cannot produce itself: a volume
// under a boid-owned name that carries no identity label, because somebody
// created it by hand. The Engine API cannot add a label to an existing volume,
// which is what makes this unrecoverable-by-retry rather than merely unknown.
func (b *bashWorkspaceInitBackend) seedUnlabeledVolume(t *testing.T, volumeName string) {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := os.MkdirAll(filepath.Join(b.volRoot, volumeName), 0o700); err != nil {
		t.Fatalf("seed modelled volume %q: %v", volumeName, err)
	}
	b.volumes[volumeName] = ""
}

func (b *bashWorkspaceInitBackend) RunWorkspaceInit(ctx context.Context, req WorkspaceInitRequest) error {
	b.mu.Lock()
	b.requests = append(b.requests, req)
	b.mu.Unlock()

	// Stand in for the volume mount: inside a container HOME/BOID_WORKSPACE_HOME
	// point at HomeTarget, which IS the volume named by HomeSource. Here there
	// is no mount, so the two are reconciled by pointing the script at the
	// directory modelling that volume.
	homeDir := b.dirFor(req.HomeSource)
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		return err
	}
	env := make([]string, 0, len(req.Env)+1)
	for k, v := range req.Env {
		if v == req.HomeTarget {
			v = homeDir
		}
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	// PATH is deliberately absent from req.Env (§D9: the image's own PATH
	// applies inside a container). This process has no image, so it lends its
	// own — nothing else is inherited.
	env = append(env, "PATH="+os.Getenv("PATH"))

	cmd := exec.CommandContext(ctx, "/bin/bash", "-s")
	cmd.Stdin = strings.NewReader(req.Script)
	cmd.Env = env
	// The same bounded, tail-retaining sink the container backend's demux
	// writes into, rather than a bytes.Buffer: these tests assert on the
	// resulting error, and a different capture here would let the real one
	// regress without any of them noticing.
	out := newWorkspaceInitOutput()
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return workspaceInitFailure(req.Slug, exitErr.ExitCode(), out)
		}
		return err
	}
	return nil
}

// lastRequest returns the most recent request this backend was handed.
func (b *bashWorkspaceInitBackend) lastRequest(t *testing.T) WorkspaceInitRequest {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.requests) == 0 {
		t.Fatal("no workspace init request reached the backend")
	}
	return b.requests[len(b.requests)-1]
}

func (b *bashWorkspaceInitBackend) runCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.requests)
}

// --- backend.SandboxBackend, none of which these tests exercise -------------

func (b *bashWorkspaceInitBackend) Launch(context.Context, sandbox.Spec, backend.LaunchOptions) (backend.SandboxSession, error) {
	return nil, errors.New("bashWorkspaceInitBackend cannot launch jobs")
}

func (b *bashWorkspaceInitBackend) Adopt(context.Context, string) (backend.SandboxSession, bool) {
	return nil, false
}

func (b *bashWorkspaceInitBackend) ReapOrphans(context.Context) (backend.ReapReport, error) {
	return backend.ReapReport{}, nil
}
