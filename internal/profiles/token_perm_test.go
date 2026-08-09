package profiles

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeTokenFileWithPerm drops a valid token JSON at the profile's token
// path with the given mode, and points the profiles package's config root
// at a temp dir for the duration of the test.
func writeTokenFileWithPerm(t *testing.T, profileName string, perm os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	path, err := TokenPath(profileName)
	if err != nil {
		t.Fatalf("TokenPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data, err := json.Marshal(&Token{Token: "t", URL: "https://example.com"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatalf("write token: %v", err)
	}
	// os.WriteFile applies umask; force the exact bits we are testing.
	if err := os.Chmod(path, perm); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	return path
}

// captureSlog swaps the default slog logger for one writing into the
// returned buffer, restoring the original when the test ends.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// TestLoadToken_WarnsOnLoosePermissions pins the POSIX behaviour the
// per-GOOS split must not lose: a token readable by anyone still warns.
func TestLoadToken_WarnsOnLoosePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not meaningful on Windows; see token_perm_windows.go")
	}
	writeTokenFileWithPerm(t, "loose", 0o666)
	buf := captureSlog(t)

	if _, err := LoadToken("loose"); err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if !strings.Contains(buf.String(), "looser permissions") {
		t.Errorf("no permission warning logged; output = %q", buf.String())
	}
}

// TestLoadToken_QuietOnCorrectPermissions is the other half: 0600 must not
// warn, or the warning becomes noise that gets ignored.
func TestLoadToken_QuietOnCorrectPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not meaningful on Windows; see token_perm_windows.go")
	}
	writeTokenFileWithPerm(t, "tight", 0o600)
	buf := captureSlog(t)

	if _, err := LoadToken("tight"); err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if strings.Contains(buf.String(), "looser permissions") {
		t.Errorf("warned about a 0600 token file; output = %q", buf.String())
	}
}

// TestTokenProtectionNote_EmptyOnPOSIX pins that the login-time caveat is
// Windows-only. On POSIX the 0600 bits are real and kernel-enforced, so a
// disclaimer there would be a false warning.
//
// The Windows counterpart's non-empty return cannot be exercised from a
// Linux test run — the CI cross-build (docs/plans/windows-client-build.md)
// is what proves that file still compiles.
func TestTokenProtectionNote_EmptyOnPOSIX(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this pins the POSIX side; the Windows side deliberately returns a note")
	}
	if note := TokenProtectionNote(); note != "" {
		t.Errorf("TokenProtectionNote() = %q, want empty on %s", note, runtime.GOOS)
	}
}
