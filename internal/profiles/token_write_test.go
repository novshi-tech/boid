package profiles

import (
	"os"
	"testing"
	"time"
)

func TestWriteToken_CreatesFileWithCorrectPermAndParentDir(t *testing.T) {
	withXDGConfigHome(t)
	tok := &Token{
		DeviceID: "dev_abc",
		Token:    "tk_xxx",
		IssuedAt: time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC),
		URL:      "https://work.example.com",
	}
	if err := WriteToken("work", tok); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}

	path, err := TokenPath("work")
	if err != nil {
		t.Fatalf("TokenPath: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file perm = %o, want 0600", perm)
	}

	dir, err := TokensDir()
	if err != nil {
		t.Fatalf("TokensDir: %v", err)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat tokens dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("tokens dir perm = %o, want 0700", perm)
	}
}

func TestWriteToken_RoundTripsThroughLoadToken(t *testing.T) {
	withXDGConfigHome(t)
	tok := &Token{
		DeviceID: "dev_abc",
		Token:    "tk_xxx",
		IssuedAt: time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC),
		URL:      "https://work.example.com",
	}
	if err := WriteToken("work", tok); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}

	got, err := LoadToken("work")
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if got.DeviceID != tok.DeviceID || got.Token != tok.Token || got.URL != tok.URL {
		t.Errorf("round-tripped token = %+v, want %+v", got, tok)
	}
	if !got.IssuedAt.Equal(tok.IssuedAt) {
		t.Errorf("IssuedAt = %v, want %v", got.IssuedAt, tok.IssuedAt)
	}
}

func TestWriteToken_OverwritesExisting(t *testing.T) {
	withXDGConfigHome(t)
	if err := WriteToken("work", &Token{DeviceID: "d1", Token: "old", URL: "https://work.example.com"}); err != nil {
		t.Fatalf("WriteToken (1): %v", err)
	}
	if err := WriteToken("work", &Token{DeviceID: "d2", Token: "new", URL: "https://work.example.com"}); err != nil {
		t.Fatalf("WriteToken (2): %v", err)
	}
	got, err := LoadToken("work")
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if got.Token != "new" || got.DeviceID != "d2" {
		t.Errorf("expected the second write to win, got %+v", got)
	}
}

func TestDeleteToken_RemovesFile(t *testing.T) {
	withXDGConfigHome(t)
	if err := WriteToken("work", &Token{DeviceID: "d", Token: "t", URL: "https://work.example.com"}); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	if err := DeleteToken("work"); err != nil {
		t.Fatalf("DeleteToken: %v", err)
	}
	path, err := TokenPath("work")
	if err != nil {
		t.Fatalf("TokenPath: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected token file to be gone, stat err = %v", err)
	}
}

func TestDeleteToken_MissingFile_NoError(t *testing.T) {
	withXDGConfigHome(t)
	if err := DeleteToken("never-logged-in"); err != nil {
		t.Fatalf("DeleteToken on a missing file should be a no-op, got: %v", err)
	}
}

func TestDeleteToken_MissingTokensDir_NoError(t *testing.T) {
	// Fresh XDG_CONFIG_HOME with no tokens/ dir at all yet (not even
	// created) — DeleteToken must still be idempotent-safe.
	withXDGConfigHome(t)
	if err := DeleteToken("work"); err != nil {
		t.Fatalf("DeleteToken with no tokens dir should be a no-op, got: %v", err)
	}
}
