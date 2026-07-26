package dispatcher

import (
	"context"
	"errors"
	"os"
	"os/exec"
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
// What it cannot stand in for is the container boundary, and it does not
// pretend to. There is no mount, so HomeSource and HomeTarget name the same
// directory only because this backend rewrites the two home-valued environment
// variables from the target to the source before executing (see run). What
// boid asks the ENGINE for — image, name, labels, uid, userns, network, mount
// — is pinned separately against a fake docker API in
// container_backend_workspace_init_test.go, and the two are tied together by
// the wiring test in workspace_init_wiring_test.go.
type bashWorkspaceInitBackend struct {
	mu       sync.Mutex
	requests []WorkspaceInitRequest
}

var (
	_ backend.SandboxBackend = (*bashWorkspaceInitBackend)(nil)
	_ WorkspaceInitExecutor  = (*bashWorkspaceInitBackend)(nil)
)

func newBashWorkspaceInitBackend(t *testing.T) *bashWorkspaceInitBackend {
	t.Helper()
	return &bashWorkspaceInitBackend{}
}

// newWorkspaceHomeTestRunner is the successor of the bare `&Runner{}` every
// resolveWorkspaceHome test used before PR5. A Runner with no Backend can no
// longer prepare a workspace home at all — see workspaceInitExecutorFor for
// why that is a hard error rather than a fallback — so the minimal wiring for
// these tests is now "a Runner that can run an init".
func newWorkspaceHomeTestRunner(t *testing.T) *Runner {
	t.Helper()
	return &Runner{Backend: newBashWorkspaceInitBackend(t)}
}

func (b *bashWorkspaceInitBackend) RunWorkspaceInit(ctx context.Context, req WorkspaceInitRequest) error {
	b.mu.Lock()
	b.requests = append(b.requests, req)
	b.mu.Unlock()

	// Stand in for the bind mount: inside a container HOME/BOID_WORKSPACE_HOME
	// point at HomeTarget, which IS HomeSource's contents. Here there is no
	// mount, so the two are reconciled by pointing the script at the source.
	env := make([]string, 0, len(req.Env)+1)
	for k, v := range req.Env {
		if v == req.HomeTarget {
			v = req.HomeSource
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
