package runner

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

func TestPastaArgs_Matches1to1(t *testing.T) {
	got := pastaArgs("/usr/local/bin/boid", "/tmp/boid-J-runner-spec.json", "/tmp/boid-J-runner-state.json")
	want := []string{
		"--config-net",
		"-4",
		"-a", "10.0.2.0", "-n", "24", "-g", "10.0.2.2",
		"--dns-forward", "10.0.2.3",
		"-t", "none", "-u", "none",
		"--",
		"/usr/local/bin/boid", "runner-inner",
		"--spec", "/tmp/boid-J-runner-spec.json",
		"--state", "/tmp/boid-J-runner-state.json",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pastaArgs mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestStopSignalIsSIGUSR1(t *testing.T) {
	if got := stopSignal(); got != syscall.SIGUSR1 {
		t.Errorf("stopSignal() = %v, want SIGUSR1", got)
	}
}

func TestEvalGuard(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "adir")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	file := filepath.Join(dir, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	missing := filepath.Join(dir, "nope")

	cases := []struct {
		guard string
		want  bool
	}{
		{"", true},                    // empty always passes
		{"-d " + subdir, true},        // dir exists
		{"-d " + file, false},         // file is not a dir
		{"-d " + missing, false},      // missing
		{"-f " + file, true},          // regular file
		{"-f " + subdir, false},       // dir is not a regular file
		{"-e " + subdir, true},        // exists (dir)
		{"-e " + file, true},          // exists (file)
		{"-e " + missing, false},      // missing
		{"-z something", true},        // unknown op → fail-open
		{"-d '" + subdir + "'", true}, // single-quoted path
	}
	for _, tc := range cases {
		if got := evalGuard(tc.guard); got != tc.want {
			t.Errorf("evalGuard(%q) = %v, want %v", tc.guard, got, tc.want)
		}
	}
}

func TestShellUnquote(t *testing.T) {
	cases := map[string]string{
		"/plain/path":    "/plain/path",
		"'/quoted/path'": "/quoted/path",
		`'a'"'"'b'`:      "a'b", // shellQuote's embedded-quote escape
		"bare":           "bare",
		"'with space'":   "with space",
	}
	for in, want := range cases {
		if got := shellUnquote(in); got != want {
			t.Errorf("shellUnquote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEnvSlice_SortedKeyValue(t *testing.T) {
	got := envSlice(map[string]string{"B": "2", "A": "1", "C": "3"})
	want := []string{"A=1", "B=2", "C=3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("envSlice = %v, want %v", got, want)
	}
}

// --- Backend-shared post-namespace-setup steps ---------------------------
// (docs/plans/phase6-container-backend.md §PR2: applySpecFiles /
// applySpecSymlinks / applyPathEnv / resolveJobOutput are shared between the
// userns runner (runner_linux.go's RunInnerChild) and the Phase 6 container
// entrypoint (runner_container_linux.go's RunContainer) — exercised here
// off the syscall path, the same way evalGuard/shellUnquote/envSlice above
// are.)

func TestApplySpecFiles_WritesFilesAndRecordsOK(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	st := OpenState(statePath)
	defer st.Close()

	files := []sandbox.FileWrite{
		{Path: filepath.Join(dir, "a.txt"), Content: "hello"},
		{Path: filepath.Join(dir, "nested", "b.txt"), Content: "world"},
	}
	if err := applySpecFiles("test-stage", files, st); err != nil {
		t.Fatalf("applySpecFiles: %v", err)
	}
	for _, f := range files {
		got, err := os.ReadFile(f.Path)
		if err != nil {
			t.Fatalf("read %s: %v", f.Path, err)
		}
		if string(got) != f.Content {
			t.Errorf("%s content = %q, want %q", f.Path, got, f.Content)
		}
	}

	lines := readStateLines(t, statePath)
	if len(lines) != 1 || lines[0].Phase != "write-files" || lines[0].Status != "ok" {
		t.Errorf("expected a single write-files/ok state line, got %+v", lines)
	}
}

func TestApplySpecFiles_ErrorPropagatesAndRecordsFail(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	st := OpenState(statePath)
	defer st.Close()

	// A blocking regular file where a FileWrite needs its parent to be a
	// directory forces writeFileAt's MkdirAll to fail.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	badPath := filepath.Join(blocker, "child.txt")

	err := applySpecFiles("test-stage", []sandbox.FileWrite{{Path: badPath, Content: "x"}}, st)
	if err == nil {
		t.Fatal("expected an error when the parent directory cannot be created")
	}

	lines := readStateLines(t, statePath)
	if len(lines) != 1 || lines[0].Status != "error" || lines[0].Phase != "write-file "+badPath {
		t.Errorf("expected a single write-file/error state line for %s, got %+v", badPath, lines)
	}
}

func TestApplySpecSymlinks_CreatesParentAndOverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	st := OpenState(statePath)
	defer st.Close()

	linkPath := filepath.Join(dir, "bin", "gh")
	// Pre-existing stale symlink at the same path must be overwritten, not
	// left in place or errored on (matches RunInnerChild's pre-extraction
	// `_ = os.Remove(s.LinkPath)` behaviour).
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink("stale-target", linkPath); err != nil {
		t.Fatalf("pre-create stale symlink: %v", err)
	}

	err := applySpecSymlinks("test-stage", []sandbox.Symlink{{LinkPath: linkPath, LinkTarget: "boid"}}, st)
	if err != nil {
		t.Fatalf("applySpecSymlinks: %v", err)
	}
	got, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if got != "boid" {
		t.Errorf("symlink target = %q, want %q", got, "boid")
	}
}

func TestApplySpecSymlinks_MkdirFailureRecordsFail(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	st := OpenState(statePath)
	defer st.Close()

	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	linkPath := filepath.Join(blocker, "gh")

	err := applySpecSymlinks("test-stage", []sandbox.Symlink{{LinkPath: linkPath, LinkTarget: "boid"}}, st)
	if err == nil {
		t.Fatal("expected an error when the symlink's parent directory cannot be created")
	}
	lines := readStateLines(t, statePath)
	if len(lines) != 1 || lines[0].Status != "error" || lines[0].Phase != "mkdir symlink parent "+linkPath {
		t.Errorf("expected a single mkdir-symlink-parent/error state line, got %+v", lines)
	}
}

func TestApplyPathEnv_SetsWhenPresentLeavesUnsetOtherwise(t *testing.T) {
	old, hadOld := os.LookupEnv("PATH")
	defer func() {
		if hadOld {
			_ = os.Setenv("PATH", old)
		} else {
			_ = os.Unsetenv("PATH")
		}
	}()

	if err := os.Setenv("PATH", "/before"); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	applyPathEnv(sandbox.Spec{Env: map[string]string{}})
	if got := os.Getenv("PATH"); got != "/before" {
		t.Errorf("applyPathEnv with no PATH override changed PATH to %q", got)
	}

	applyPathEnv(sandbox.Spec{Env: map[string]string{"PATH": "/run/boid/bin:/usr/bin"}})
	if got := os.Getenv("PATH"); got != "/run/boid/bin:/usr/bin" {
		t.Errorf("PATH = %q, want the spec override", got)
	}
}

func TestResolveJobOutput_Precedence(t *testing.T) {
	dir := t.TempDir()

	// Neither file present → nil.
	spec := sandbox.Spec{}
	if got := resolveJobOutput(spec); got != nil {
		t.Errorf("expected nil output with no patch/capture files, got %q", got)
	}

	// Only StdoutCaptureFile present → its content.
	stdoutPath := filepath.Join(dir, "stdout.txt")
	if err := os.WriteFile(stdoutPath, []byte("stdout-output"), 0o644); err != nil {
		t.Fatalf("write stdout capture: %v", err)
	}
	spec.StdoutCaptureFile = stdoutPath
	if got := string(resolveJobOutput(spec)); got != "stdout-output" {
		t.Errorf("resolveJobOutput = %q, want stdout capture content", got)
	}

	// Both present → payload patch wins.
	patchPath := filepath.Join(dir, "patch.json")
	if err := os.WriteFile(patchPath, []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatalf("write payload patch: %v", err)
	}
	spec.PayloadPatchPath = patchPath
	if got := string(resolveJobOutput(spec)); got != `{"a":1}` {
		t.Errorf("resolveJobOutput = %q, want payload patch content (higher precedence)", got)
	}
}

func TestPostJobDone_NoSocketIsNoop(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	st := OpenState(statePath)
	defer st.Close()

	// No BOID_BROKER_SOCKET in spec.Env: postJobDone must return without
	// attempting a broker call or recording any state phase.
	postJobDone("test-stage", sandbox.Spec{Env: map[string]string{}}, 0, st)

	lines := readStateLines(t, statePath)
	if len(lines) != 0 {
		t.Errorf("expected no state lines when no broker socket is configured, got %+v", lines)
	}
}

// readStateLines parses every NDJSON line phase/status/detail written to
// path, skipping any leading spec-dump line (Spec != nil).
func readStateLines(t *testing.T, path string) []stateLine {
	t.Helper()
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("open state file: %v", err)
	}
	defer f.Close()

	var lines []stateLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l stateLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("each line must be valid JSON: %v (line=%q)", err, sc.Text())
		}
		if l.Spec != nil {
			continue
		}
		lines = append(lines, l)
	}
	return lines
}

func TestReadSpec_RoundTrip(t *testing.T) {
	spec := sandbox.Spec{
		ID:      "job-1",
		Argv:    []string{"/bin/echo", "hi"},
		WorkDir: "/work",
		Env:     map[string]string{"HOME": "/home/x", "PATH": "/bin"},
		Mounts:  []sandbox.Mount{{Source: "/usr", Target: "/usr", Type: sandbox.MountRBind, Slave: true}},
		TTY:     true,
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "spec.json")
	// Write via the same encoding the dispatcher uses.
	if err := writeJSON(t, path, spec); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	got, err := readSpec(path)
	if err != nil {
		t.Fatalf("readSpec: %v", err)
	}
	if !reflect.DeepEqual(got, spec) {
		t.Errorf("readSpec round-trip mismatch\n got: %+v\nwant: %+v", got, spec)
	}
}
