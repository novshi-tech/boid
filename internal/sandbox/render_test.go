package sandbox

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// listenUnixSocket opens an AF_UNIX listener so a socket node exists on disk.
// Caller closes the returned listener to free the file descriptor.
func listenUnixSocket(path string) (net.Listener, error) {
	return net.Listen("unix", path)
}

// Bind-mount rendering for host files (e.g. boid binary, sockets, gate scripts).
// Caller constructs the Mount entry with IsFile+ReadOnly; setup script must
// touch the target path before binding and remount read-only afterward.
func TestPrepare_FileBindMountRendering(t *testing.T) {
	spec := Spec{
		ID:      "m4-bind-file",
		WorkDir: "/tmp/p",
		Env:     map[string]string{"HOME": "/tmp/p"},
		Argv:    []string{"/bin/true"},
		Mounts: []Mount{
			{Source: "/usr/local/bin/boid", Target: "/opt/boid/bin/boid", Type: MountBind, IsFile: true, ReadOnly: true},
		},
	}
	outerPath, err := Prepare(spec)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	setupPath := strings.TrimSuffix(outerPath, "-outer.sh") + "-setup.sh"
	innerPath := strings.TrimSuffix(outerPath, "-outer.sh") + "-inner.sh"
	defer os.Remove(outerPath)
	defer os.Remove(setupPath)
	defer os.Remove(innerPath)

	content, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(content)

	must := []string{
		`touch "$ROOT/opt/boid/bin/boid"`,
		`mount --bind /usr/local/bin/boid "$ROOT/opt/boid/bin/boid"`,
		`mount -o remount,bind,ro "$ROOT/opt/boid/bin/boid"`,
	}
	for _, s := range must {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in setup:\n%s", s, got)
		}
	}
	if strings.Contains(got, "cp /usr/local/bin/boid") {
		t.Errorf("unexpected cp: file bind-mounts should not copy content\n%s", got)
	}
}

// DetectType auto-creates the mountpoint based on the source's file type.
// The `-e` fallback covers non-dir non-regular files — sockets, FIFOs, device
// nodes — which the earlier `-f` form silently skipped.
func TestPrepare_DetectTypeAcceptsNonRegularFiles(t *testing.T) {
	spec := Spec{
		ID:      "detect-type-ext",
		WorkDir: "/tmp/p",
		Env:     map[string]string{"HOME": "/tmp/p"},
		Argv:    []string{"/bin/true"},
		Mounts: []Mount{
			{Source: "/run/user/1000/cetusguard/docker.sock", Target: "/run/user/1000/cetusguard/docker.sock", Type: MountBind, DetectType: true},
		},
	}
	outerPath, err := Prepare(spec)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	setupPath := strings.TrimSuffix(outerPath, "-outer.sh") + "-setup.sh"
	innerPath := strings.TrimSuffix(outerPath, "-outer.sh") + "-inner.sh"
	defer os.Remove(outerPath)
	defer os.Remove(setupPath)
	defer os.Remove(innerPath)

	content, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(content)

	if strings.Contains(got, "elif [ -f /run/user/1000/cetusguard/docker.sock ]") {
		t.Errorf("DetectType must not use `-f` (misses sockets/FIFOs):\n%s", got)
	}
	if !strings.Contains(got, "elif [ -e /run/user/1000/cetusguard/docker.sock ]") {
		t.Errorf("DetectType should use `-e` for the non-dir branch:\n%s", got)
	}
}

// Verify the DetectType rendered snippet actually creates the expected
// mountpoint for each file-type on disk: directory (mkdir), regular file
// (touch), and socket (touch — this is the regression we fixed).
//
// Calls renderMount directly and strips the `mount ...` lines (which need
// root) so the exercise covers the `if/elif/fi` target-prep block only.
func TestRenderMount_DetectTypeRuntimeBehavior(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}

	srcRoot := t.TempDir()
	dirSource := filepath.Join(srcRoot, "somedir")
	if err := os.Mkdir(dirSource, 0o755); err != nil {
		t.Fatalf("mkdir dir source: %v", err)
	}
	fileSource := filepath.Join(srcRoot, "somefile")
	if err := os.WriteFile(fileSource, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file source: %v", err)
	}
	sockSource := filepath.Join(srcRoot, "some.sock")
	ln, err := listenUnixSocket(sockSource)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	defer ln.Close()

	mounts := []Mount{
		{Source: dirSource, Target: "/want/dir", Type: MountBind, DetectType: true},
		{Source: fileSource, Target: "/want/file", Type: MountBind, DetectType: true},
		{Source: sockSource, Target: "/want/sock", Type: MountBind, DetectType: true},
	}

	tmpRoot := t.TempDir()
	var script strings.Builder
	script.WriteString("set -e\nROOT=")
	script.WriteString(tmpRoot)
	script.WriteByte('\n')
	for _, m := range mounts {
		var b strings.Builder
		renderMount(&b, m)
		for line := range strings.SplitSeq(b.String(), "\n") {
			trimmed := strings.TrimLeft(line, " \t")
			if strings.HasPrefix(trimmed, "mount") {
				continue
			}
			script.WriteString(line)
			script.WriteByte('\n')
		}
	}

	out, err := exec.Command(bash, "-c", script.String()).CombinedOutput()
	if err != nil {
		t.Fatalf("run rendered mountpoint prep: %v\nscript:\n%s\noutput:\n%s", err, script.String(), out)
	}

	stat := func(p string) os.FileMode {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		return fi.Mode()
	}

	if !stat(tmpRoot + "/want/dir").IsDir() {
		t.Errorf("dir source: expected directory mountpoint at /want/dir")
	}
	if mode := stat(tmpRoot + "/want/file"); !mode.IsRegular() {
		t.Errorf("file source: expected regular file mountpoint, got mode %v", mode)
	}
	// Socket source — the regression: before the fix this path was NOT created.
	if mode := stat(tmpRoot + "/want/sock"); !mode.IsRegular() {
		t.Errorf("socket source: expected regular file mountpoint (touch), got mode %v", mode)
	}
}

// The cleanup() trap must enumerate mount targets globally rather than calling
// `findmnt --submounts "$ROOT"`, because $ROOT is a regular tmpdir (not a mount
// point) and the --submounts query returns empty even when sub-mounts are
// present. Prior to this fix, `rm -rf "$ROOT"` ran against still-mounted host
// directories.
func TestPrepare_CleanupChecksMountsGlobally(t *testing.T) {
	spec := Spec{
		ID:      "cleanup-mount-check",
		WorkDir: "/tmp/p",
		Env:     map[string]string{"HOME": "/tmp/p"},
		Argv:    []string{"/bin/true"},
	}
	outerPath, err := Prepare(spec)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	setupPath := strings.TrimSuffix(outerPath, "-outer.sh") + "-setup.sh"
	innerPath := strings.TrimSuffix(outerPath, "-outer.sh") + "-inner.sh"
	defer os.Remove(outerPath)
	defer os.Remove(setupPath)
	defer os.Remove(innerPath)

	content, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(content)

	if strings.Contains(got, `findmnt --submounts --noheadings --output TARGET "$ROOT"`) {
		t.Errorf("cleanup must not rely on --submounts against a non-mountpoint $ROOT:\n%s", got)
	}
	if !strings.Contains(got, `findmnt --noheadings --output TARGET`) {
		t.Errorf("cleanup should enumerate all mount targets via findmnt:\n%s", got)
	}
	if !strings.Contains(got, `awk -v r="$ROOT"`) {
		t.Errorf("cleanup should scan targets for $ROOT-prefixed paths via awk:\n%s", got)
	}
}

// Verify the guard's awk selector matches "$ROOT" exactly and any "$ROOT/..."
// prefix, but rejects coincidental prefix collisions (e.g. sibling tmpdirs).
func TestPrepare_CleanupAwkGuardSemantics(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}

	script := `ROOT=/tmp/boid-root-XYZ
found=$(printf '%s\n' "$1" | awk -v r="$ROOT" '$0 == r || index($0, r "/") == 1 { found=1 } END { exit !found }' && echo match || echo no-match)
echo "$found"
`
	cases := []struct {
		line string
		want string
	}{
		{"/tmp/boid-root-XYZ", "match"},
		{"/tmp/boid-root-XYZ/lib", "match"},
		{"/tmp/boid-root-XYZ/lib/node_modules/npm", "match"},
		{"/tmp/boid-root-XYZ-other", "no-match"}, // sibling tmpdir with same prefix
		{"/tmp/boid-root-OTHER", "no-match"},
		{"/", "no-match"},
	}
	for _, tc := range cases {
		out, err := exec.Command(bash, "-c", script, "bash", tc.line).Output()
		if err != nil {
			t.Fatalf("run awk guard with %q: %v", tc.line, err)
		}
		got := strings.TrimSpace(string(out))
		if got != tc.want {
			t.Errorf("line %q: got %q, want %q", tc.line, got, tc.want)
		}
	}
}
