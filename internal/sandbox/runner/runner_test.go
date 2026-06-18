package runner

import (
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

func TestStopSignal(t *testing.T) {
	cases := []struct {
		name string
		want syscall.Signal
	}{
		{"", syscall.SIGUSR1},
		{"USR1", syscall.SIGUSR1},
		{"USR2", syscall.SIGUSR2},
		{"TERM", syscall.SIGTERM},
		{"bogus", syscall.SIGUSR1},
	}
	for _, tc := range cases {
		got := stopSignal(sandbox.Spec{StopSignalName: tc.name})
		if got != tc.want {
			t.Errorf("stopSignal(%q) = %v, want %v", tc.name, got, tc.want)
		}
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
