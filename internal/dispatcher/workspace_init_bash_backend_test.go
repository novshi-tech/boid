package dispatcher

import (
	"context"
	"errors"
	"fmt"
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

	// --- PR8 (論点 f), the migration half -------------------------------
	//
	// imports records every WorkspaceHomeImportRequest this backend was handed;
	// removes counts RemoveWorkspaceHomeVolume calls. removeVolumeErr /
	// importErr let a test model the two engine-side failures the migration has
	// to behave correctly under: a volume held by a running job (409 ->
	// ErrWorkspaceHomeInUse) and an extraction that died partway through.
	imports         []WorkspaceHomeImportRequest
	removes         int
	removeVolumeErr error
	importErr       error

	// removeVolumeErrFromCall restricts removeVolumeErr to the Nth
	// RemoveWorkspaceHomeVolume call and later (1-based; 0 means "every call",
	// which is what every pre-existing case wants).
	//
	// It exists for exactly one situation, and that situation is the reason the
	// field is not just a second error variable: the migration issues TWO
	// removals — the one that replaces the home, and the one that discards a
	// half-extracted volume after a failed extraction — and the failure worth
	// testing is the second one failing while the first succeeded. A single
	// removeVolumeErr cannot express that, because making it fire on call 1
	// means the migration never gets far enough to attempt call 2.
	removeVolumeErrFromCall int

	// importPartial names entries ImportWorkspaceHome writes into the volume
	// BEFORE returning importErr.
	//
	// This is the failure the extraction actually has. The archive arrives as a
	// stream over a socket, so tar has already created directories and files by
	// the time the upload is cut short or the disk fills — a double that errors
	// before touching the volume models a failure mode that does not exist and
	// would keep "a failed migration leaves the workspace as if it had never
	// been dispatched into" green while the volume held a half-installed
	// toolchain. The dangerous residue is specifically an executable an
	// idempotent init.sh probes for with `command -v` and concludes is already
	// installed.
	importPartial []string

	// removeDeadline records, per RemoveWorkspaceHomeVolume call, whether the
	// context it was handed carried a DEADLINE.
	//
	// It is the only way to observe round 4's Major from a unit test at its
	// production timeout: the property being asserted is "the caller bounded this
	// call", and waiting 30s to watch a bound fire would test the same thing far
	// more slowly and far more flakily. The moby client turns a context deadline
	// into a request deadline, so a call with none can wait on a hung engine
	// forever — which inside daemon startup means no auto-reopen and no API
	// listener, ever.
	removeDeadline []bool

	// blockRemoveUntilDone makes RemoveWorkspaceHomeVolume park until its context
	// is done, standing in for an engine socket that accepts the connection and
	// then answers nothing. Paired with a shrunk
	// workspaceHomeMigrationSweepTimeout it is what shows the sweep RETURNING
	// rather than merely being handed a deadline.
	blockRemoveUntilDone bool

	// onImport, when non-nil, runs at the top of ImportWorkspaceHome — before
	// the partial residue is written and before importErr is returned.
	//
	// It exists for one case and it is the case codex round 2 (Minor 2) said was
	// untested: the commonest route to a failed extraction is the OPERATOR'S CLI
	// GOING AWAY mid-upload, which cancels the HTTP request context, which is the
	// same context the migration is running under. A test that models that has to
	// be able to cancel exactly there — after the destructive removal, during the
	// extraction — which is a moment no test can reach from the outside.
	onImport func()
}

var (
	_ backend.SandboxBackend = (*bashWorkspaceInitBackend)(nil)
	_ WorkspaceInitExecutor  = (*bashWorkspaceInitBackend)(nil)
	_ WorkspaceHomeImporter  = (*bashWorkspaceInitBackend)(nil)
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

// volumeExists reports whether the modelled engine still has a volume under
// this name — the question "did the migration clean up after itself" is asked
// of the volume STORE, not of the directory, because a directory that happens
// to be gone would also be reported by an implementation that only emptied it.
func (b *bashWorkspaceInitBackend) volumeExists(volumeName string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.volumes[volumeName]
	return ok
}

// setIdentity rewrites the identity label a modelled volume carries WITHOUT
// touching its contents — a thing the Engine API cannot do, and which exists
// here only so a test can un-do exactly one of 論点 f's two re-init triggers
// and check that the other one still fires on its own.
func (b *bashWorkspaceInitBackend) setIdentity(t *testing.T, volumeName, identity string) {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.volumes[volumeName]; !ok {
		t.Fatalf("no volume named %q was ever created", volumeName)
	}
	b.volumes[volumeName] = identity
}

// RemoveWorkspaceHomeVolume models `docker volume rm` as the migration issues
// it: the volume object AND its contents go, a name that is not there is not an
// error (the CLI can migrate into a workspace never dispatched into), and
// removeVolumeErr stands in for the engine's 409 when a job holds it.
//
// It HONOURS ctx, which is not decoration (codex round 2, Minor 2): the moby
// client fails a request whose context is already done without touching the
// engine, so a double that ignored ctx would keep
// discardPartialWorkspaceHome's context.WithoutCancel green after it was
// changed back to an ordinary derived context — i.e. it would stop testing the
// one property that cleanup exists for.
func (b *bashWorkspaceInitBackend) RemoveWorkspaceHomeVolume(ctx context.Context, volumeName string) (bool, error) {
	_, hasDeadline := ctx.Deadline()
	b.mu.Lock()
	b.removeDeadline = append(b.removeDeadline, hasDeadline)
	block := b.blockRemoveUntilDone
	b.mu.Unlock()

	if block {
		// Outside the mutex on purpose: a real engine that never answers does not
		// stop the rest of the daemon from touching its own bookkeeping, and a
		// double that held the lock would deadlock the assertions instead of
		// modelling the hang.
		<-ctx.Done()
	}

	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("volume remove %q: %w", volumeName, err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.removes++
	from := b.removeVolumeErrFromCall
	if from == 0 {
		from = 1
	}
	if b.removeVolumeErr != nil && b.removes >= from {
		return false, b.removeVolumeErr
	}
	_, existed := b.volumes[volumeName]
	if err := os.RemoveAll(filepath.Join(b.volRoot, volumeName)); err != nil {
		return false, err
	}
	delete(b.volumes, volumeName)
	return existed, nil
}

// ImportWorkspaceHome models the extraction container: it runs the REAL
// extraction argv (workspaceHomeImportArgv) against the directory standing in
// for the volume, with the request's tar on its stdin.
//
// Running the actual argv rather than an archive/tar reader in Go is the whole
// value of this double for PR8: the flags on that command line are what decide
// whether a 0600 credentials file survives the trip, and a Go-side extraction
// would keep TestRunner_ImportWorkspaceHome_PreservesModes green while the
// command boid actually ships lost `--same-permissions`.
func (b *bashWorkspaceInitBackend) ImportWorkspaceHome(ctx context.Context, req WorkspaceHomeImportRequest) error {
	b.mu.Lock()
	b.imports = append(b.imports, req)
	importErr := b.importErr
	partial := append([]string(nil), b.importPartial...)
	onImport := b.onImport
	_, volumeExists := b.volumes[req.HomeSource]
	b.mu.Unlock()

	if onImport != nil {
		onImport()
	}

	if !volumeExists {
		// The container backend gets this for free — ContainerCreate cannot
		// mount a volume that is not there (and its implicit create is the
		// unlabelled-volume trap verifyWorkspaceHomeIdentity exists for). The
		// double has to assert it explicitly or an implementation that never
		// re-created the volume would still "succeed" here.
		return fmt.Errorf("no volume named %q to extract into", req.HomeSource)
	}
	dir := b.dirFor(req.HomeSource)
	if importErr != nil {
		// The half-written volume a real interrupted extraction leaves behind
		// (see importPartial's doc comment), created before the error is
		// returned rather than instead of it.
		for _, name := range partial {
			full := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(full, []byte("half-extracted "+name+"\n"), 0o755); err != nil {
				return err
			}
		}
		return importErr
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	argv := workspaceHomeImportArgv(dir)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = req.Tar
	out := newWorkspaceInitOutput()
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("extract into workspace home %q (exit %d): %s", req.Slug, exitErr.ExitCode(), out.Tail(512))
		}
		return err
	}
	return nil
}

func (b *bashWorkspaceInitBackend) importCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.imports)
}

func (b *bashWorkspaceInitBackend) removeCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.removes
}

// removeDeadlines reports, in call order, whether each
// RemoveWorkspaceHomeVolume was given a context with a deadline.
func (b *bashWorkspaceInitBackend) removeDeadlines() []bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]bool(nil), b.removeDeadline...)
}

func (b *bashWorkspaceInitBackend) lastImport(t *testing.T) WorkspaceHomeImportRequest {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.imports) == 0 {
		t.Fatal("no workspace home import request reached the backend")
	}
	return b.imports[len(b.imports)-1]
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
