package install

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

var uuidv4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestLoadOrCreate_GeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()

	id, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if !uuidv4Pattern.MatchString(id) {
		t.Errorf("id = %q, want a canonical lowercase UUIDv4", id)
	}

	path := filepath.Join(dir, FileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("file mode = %#o, want 0644 (install_id is non-secret)", info.Mode().Perm())
	}

	// A second call must return the exact same ID (persisted, not
	// regenerated on every call).
	id2, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("second LoadOrCreate: %v", err)
	}
	if id2 != id {
		t.Errorf("second LoadOrCreate = %q, want %q (persisted id)", id2, id)
	}
}

func TestLoadOrCreate_CreatesMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "boid")

	id, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if id == "" {
		t.Fatal("expected a non-empty id")
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Fatalf("expected install_id to exist under the created dir: %v", err)
	}
}

func TestLoadOrCreate_TrimsWhitespaceOnRead(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("  some-existing-id  \n"), 0o644); err != nil {
		t.Fatalf("seed install_id: %v", err)
	}

	id, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if id != "some-existing-id" {
		t.Errorf("id = %q, want %q (trimmed)", id, "some-existing-id")
	}
}

func TestLoadOrCreate_RegeneratesOnEmptyFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), nil, 0o644); err != nil {
		t.Fatalf("seed empty install_id: %v", err)
	}

	id, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if !uuidv4Pattern.MatchString(id) {
		t.Errorf("id = %q, want a freshly generated UUIDv4 (empty file must regenerate)", id)
	}
}
