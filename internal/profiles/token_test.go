package profiles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withXDGConfigHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return dir
}

func writeTokenFile(t *testing.T, profileName, content string, perm os.FileMode) {
	t.Helper()
	dir, err := TokensDir()
	if err != nil {
		t.Fatalf("TokensDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir tokens dir: %v", err)
	}
	path := filepath.Join(dir, profileName+".json")
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatalf("write token file: %v", err)
	}
}

func TestLoadToken_MissingFile_ReturnsNotExistError(t *testing.T) {
	withXDGConfigHome(t)
	_, err := LoadToken("work")
	if err == nil {
		t.Fatal("expected an error for a missing token file")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected an os.IsNotExist error, got %v", err)
	}
}

func TestLoadToken_ParsesFields(t *testing.T) {
	withXDGConfigHome(t)
	writeTokenFile(t, "work", `{
		"device_id": "dev_abc123",
		"token": "tk_xxxxxxxxxxxxxxxx",
		"issued_at": "2026-07-16T12:00:00Z",
		"url": "https://work.example.com"
	}`, 0o600)

	tok, err := LoadToken("work")
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if tok.DeviceID != "dev_abc123" {
		t.Errorf("DeviceID = %q", tok.DeviceID)
	}
	if tok.Token != "tk_xxxxxxxxxxxxxxxx" {
		t.Errorf("Token = %q", tok.Token)
	}
	if tok.URL != "https://work.example.com" {
		t.Errorf("URL = %q", tok.URL)
	}
}

func TestLoadToken_EmptyTokenField_HardError(t *testing.T) {
	// A `{"token":""}` file would sail through decode but every
	// subsequent HTTPS request would go out Bearer-less (silent auth
	// failure). Reject up front so the diagnostic names the missing
	// field and directs the operator to re-login.
	withXDGConfigHome(t)
	writeTokenFile(t, "work", `{"device_id":"d","token":"","url":"https://x"}`, 0o600)

	_, err := LoadToken("work")
	if err == nil {
		t.Fatal("expected a hard error for an empty token field")
	}
	if !strings.Contains(err.Error(), "empty \"token\"") {
		t.Errorf("error should name the missing token field, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "re-login required") {
		t.Errorf("error should direct the operator to re-login, got %q", err.Error())
	}
}

func TestLoadToken_EmptyURLField_HardError(t *testing.T) {
	// Same rationale: an empty `url` field would produce a "config=X,
	// token=" origin-mismatch diagnostic later, which is confusing.
	withXDGConfigHome(t)
	writeTokenFile(t, "work", `{"device_id":"d","token":"t","url":""}`, 0o600)

	_, err := LoadToken("work")
	if err == nil {
		t.Fatal("expected a hard error for an empty url field")
	}
	if !strings.Contains(err.Error(), "empty \"url\"") {
		t.Errorf("error should name the missing url field, got %q", err.Error())
	}
}

func TestLoadToken_RejectsUnknownField(t *testing.T) {
	withXDGConfigHome(t)
	writeTokenFile(t, "work", `{"device_id":"d","token":"t","url":"https://x","extra":"nope"}`, 0o600)

	_, err := LoadToken("work")
	if err == nil {
		t.Fatal("expected an error for an unknown field in the token file")
	}
}

func TestLoadToken_LoosePermissions_WarnsButStillLoads(t *testing.T) {
	withXDGConfigHome(t)
	writeTokenFile(t, "work", `{"device_id":"d","token":"t","url":"https://x"}`, 0o644)

	// LoadToken must not hard-fail on loose permissions (decision 2: warn,
	// not refuse) — this only asserts it still returns usable data. The
	// warning itself goes through log/slog and is not asserted here (no
	// slog capture helper exists in this package yet; the behavior contract
	// under test is "still loads", not "logs exactly this message").
	tok, err := LoadToken("work")
	if err != nil {
		t.Fatalf("LoadToken should warn, not fail, on loose permissions: %v", err)
	}
	if tok.Token != "t" {
		t.Errorf("Token = %q", tok.Token)
	}
}

func TestTokenPath_UsesProfileName(t *testing.T) {
	withXDGConfigHome(t)
	path, err := TokenPath("work")
	if err != nil {
		t.Fatalf("TokenPath: %v", err)
	}
	if filepath.Base(path) != "work.json" {
		t.Errorf("TokenPath base = %q, want %q", filepath.Base(path), "work.json")
	}
	dir, err := TokensDir()
	if err != nil {
		t.Fatalf("TokensDir: %v", err)
	}
	if filepath.Dir(path) != dir {
		t.Errorf("TokenPath dir = %q, want %q", filepath.Dir(path), dir)
	}
}
