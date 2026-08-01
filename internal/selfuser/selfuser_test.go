package selfuser

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// getUmask reads the process umask without permanently changing it —
// syscall.Umask itself is a set-and-return-old-value call, so reading
// requires setting then immediately restoring.
func getUmask(t *testing.T) int {
	t.Helper()
	old := syscall.Umask(0)
	syscall.Umask(old)
	return old
}

func setUmask(v int) error {
	syscall.Umask(v)
	return nil
}

func writeTestPasswd(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "passwd")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write test passwd: %v", err)
	}
	return path
}

// TestEnsurePasswdEntry_AppendsWhenMissing pins the core self-registration
// behavior the plan doc's shell sketch describes:
//
//	if ! getent passwd "$(id -u)" >/dev/null; then
//	  echo "boid:x:$(id -u):$(id -g)::/home/boid:/bin/bash" >> /etc/passwd
//	fi
func TestEnsurePasswdEntry_AppendsWhenMissing(t *testing.T) {
	path := writeTestPasswd(t, "root:x:0:0::/root:/bin/bash\n")

	registered, err := EnsurePasswdEntry(path, 1234, 5678, "/home/boid", "/bin/bash")
	if err != nil {
		t.Fatalf("EnsurePasswdEntry: %v", err)
	}
	if !registered {
		t.Fatal("registered = false, want true (uid 1234 had no entry)")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want := "root:x:0:0::/root:/bin/bash\nboid:x:1234:5678::/home/boid:/bin/bash\n"
	if string(got) != want {
		t.Errorf("passwd contents = %q, want %q", string(got), want)
	}
}

// TestEnsurePasswdEntry_NoOpWhenPresent ensures a uid that already has an
// entry (the common bare-host case, or a second self-registration on
// container restart) is left untouched rather than duplicated.
func TestEnsurePasswdEntry_NoOpWhenPresent(t *testing.T) {
	original := "root:x:0:0::/root:/bin/bash\nboid:x:1000:0::/home/boid:/bin/bash\n"
	path := writeTestPasswd(t, original)

	registered, err := EnsurePasswdEntry(path, 1000, 0, "/home/boid", "/bin/bash")
	if err != nil {
		t.Fatalf("EnsurePasswdEntry: %v", err)
	}
	if registered {
		t.Error("registered = true, want false (uid 1000 already has an entry)")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != original {
		t.Errorf("passwd contents changed:\n got  %q\n want %q", string(got), original)
	}
}

// TestEnsurePasswdEntry_IgnoresCommentsAndBlankLines pins that a passwd
// file with comments/blank lines (glibc tolerates both) is parsed
// correctly rather than mis-detecting a uid match.
func TestEnsurePasswdEntry_IgnoresCommentsAndBlankLines(t *testing.T) {
	path := writeTestPasswd(t, "# a comment\n\nroot:x:0:0::/root:/bin/bash\n")

	registered, err := EnsurePasswdEntry(path, 0, 0, "/home/boid", "/bin/bash")
	if err != nil {
		t.Fatalf("EnsurePasswdEntry: %v", err)
	}
	if registered {
		t.Error("registered = true, want false (uid 0 already present)")
	}
}

// TestEnsurePasswdEntry_MissingFileReturnsError pins that a missing/
// unreadable passwd path surfaces as an error rather than silently
// registering — EnsureRuntimeUserRegistered is the caller that decides to
// swallow this into a Debug log; EnsurePasswdEntry itself must report it.
func TestEnsurePasswdEntry_MissingFileReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := EnsurePasswdEntry(path, 1000, 0, "/home/boid", "/bin/bash")
	if err == nil {
		t.Fatal("EnsurePasswdEntry: err = nil, want a not-found error")
	}
}

// TestEnsurePasswdEntry_UnwritableFileReturnsError pins the bare-host
// safety case: a process without permission to modify /etc/passwd (or any
// read-only-mounted passwd file) must get an error back, not a panic or a
// silently-dropped write — EnsureRuntimeUserRegistered relies on this to
// downgrade the failure to a Debug log instead of crashing startup.
func TestEnsurePasswdEntry_UnwritableFileReturnsError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: file-mode write protection does not apply")
	}
	path := writeTestPasswd(t, "root:x:0:0::/root:/bin/bash\n")
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	_, err := EnsurePasswdEntry(path, 1234, 0, "/home/boid", "/bin/bash")
	if err == nil {
		t.Fatal("EnsurePasswdEntry: err = nil, want a permission error")
	}
}

// TestEnsureRuntimeUserRegistered_DoesNotPanic is a smoke test against the
// REAL /etc/passwd: it must never panic or crash the caller regardless of
// whether the current test process's uid already has an entry (it almost
// certainly does, in which case this is a no-op) or not.
func TestEnsureRuntimeUserRegistered_DoesNotPanic(t *testing.T) {
	EnsureRuntimeUserRegistered()
}

// TestApplyGroupWritableUmask_SetsGroupWritable pins §決定1 実装形2's
// recommended fix: after this call, a freshly-created file with a
// default-permission request (no explicit narrower mode) comes out
// group-writable, so a second uid sharing supplementary group 0 can still
// write it later.
func TestApplyGroupWritableUmask_SetsGroupWritable(t *testing.T) {
	old := getUmask(t)
	defer func() { _ = setUmask(old) }()

	ApplyGroupWritableUmask()

	dir := t.TempDir()
	path := filepath.Join(dir, "created")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	f.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm()&0o020 == 0 {
		t.Errorf("created file mode = %v, want group-writable bit set", info.Mode().Perm())
	}
}

// TestPasswdSelfRegisterShellSnippet_MatchesTheGoBehavior pins that the
// shell-level snippet fed to the workspace-init wrapper (the "third
// consumer" Go-side registration cannot reach) implements the same
// contract as EnsurePasswdEntry: getent short-circuits an existing entry,
// and the fallback line has the same "boid:x:<uid>:<gid>::<home>:<shell>"
// shape EnsurePasswdEntry writes.
func TestPasswdSelfRegisterShellSnippet_MatchesTheGoBehavior(t *testing.T) {
	if !strings.Contains(PasswdSelfRegisterShellSnippet, `getent passwd "$(id -u)"`) {
		t.Error(`snippet does not check getent passwd "$(id -u)" before registering`)
	}
	if !strings.Contains(PasswdSelfRegisterShellSnippet, "boid:x:$(id -u):$(id -g)::") {
		t.Error("snippet does not append a boid:x:<uid>:<gid>::... entry matching EnsurePasswdEntry's shape")
	}
	if !strings.Contains(PasswdSelfRegisterShellSnippet, ">> /etc/passwd") {
		t.Error("snippet does not append to /etc/passwd")
	}
	if !strings.HasSuffix(strings.TrimRight(PasswdSelfRegisterShellSnippet, "\n"), "|| true") {
		t.Error("snippet's registration line does not end in `|| true` — a failed append must not abort the wrapper")
	}
}
