package dockerproxy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLedger_AppendAndContains(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(filepath.Join(dir, "docker-resources.jsonl"))

	ok, err := l.Contains("container", "abc123")
	if err != nil || ok {
		t.Fatalf("empty ledger: Contains=%v err=%v", ok, err)
	}

	if err := l.Append(ResourceEntry{Type: "container", ID: "abc123"}); err != nil {
		t.Fatal("Append:", err)
	}
	if err := l.Append(ResourceEntry{Type: "network", ID: "net456"}); err != nil {
		t.Fatal("Append:", err)
	}

	ok, err = l.Contains("container", "abc123")
	if err != nil || !ok {
		t.Errorf("Contains container abc123: got %v %v", ok, err)
	}
	ok, err = l.Contains("network", "net456")
	if err != nil || !ok {
		t.Errorf("Contains network net456: got %v %v", ok, err)
	}
	// Wrong type should not match.
	ok, err = l.Contains("network", "abc123")
	if err != nil || ok {
		t.Errorf("Contains network abc123 (wrong type): got %v %v", ok, err)
	}
}

func TestLedger_PersistenceAcrossInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")

	l1 := NewLedger(path)
	if err := l1.Append(ResourceEntry{Type: "volume", ID: "vol789"}); err != nil {
		t.Fatal(err)
	}

	// New instance should load from disk.
	l2 := NewLedger(path)
	ok, err := l2.Contains("volume", "vol789")
	if err != nil || !ok {
		t.Errorf("l2.Contains volume vol789: got %v %v", ok, err)
	}
}

func TestLedger_FileFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l := NewLedger(path)

	_ = l.Append(ResourceEntry{Type: "container", ID: "c1"})
	_ = l.Append(ResourceEntry{Type: "network", ID: "n1"})
	_ = l.Append(ResourceEntry{Type: "exec", ID: "e1"})

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Each entry must be on its own line (JSONL).
	lines := splitNonEmpty(string(raw), '\n')
	if len(lines) != 3 {
		t.Errorf("expected 3 lines, got %d:\n%s", len(lines), raw)
	}
}

func TestLedger_ReadAll(t *testing.T) {
	l := NewLedger(filepath.Join(t.TempDir(), "l.jsonl"))
	_ = l.Append(ResourceEntry{Type: "container", ID: "c1"})
	_ = l.Append(ResourceEntry{Type: "network", ID: "n1"})
	_ = l.Append(ResourceEntry{Type: "volume", ID: "v1"})

	entries, err := l.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if entries[0].Type != "container" || entries[0].ID != "c1" {
		t.Errorf("entry[0] = %+v", entries[0])
	}
}

func TestLedger_MissingFile(t *testing.T) {
	l := NewLedger(filepath.Join(t.TempDir(), "nosuchfile.jsonl"))

	entries, err := l.ReadAll()
	if err != nil || len(entries) != 0 {
		t.Errorf("ReadAll on missing file: entries=%v err=%v", entries, err)
	}

	ok, err := l.Contains("container", "x")
	if err != nil || ok {
		t.Errorf("Contains on missing file: ok=%v err=%v", ok, err)
	}
}

// TestRewriteLedger_ReplacesContent pins Major 8 (PR6 codex review):
// internal/reap's drain step needs to rewrite a ledger file to contain
// only the entries that still need reaping. RewriteLedger must fully
// replace the file's content (not append/merge), and a fresh Ledger
// reading the path afterward must see exactly the new content.
func TestRewriteLedger_ReplacesContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l := NewLedger(path)
	_ = l.Append(ResourceEntry{Type: "container", ID: "c1"})
	_ = l.Append(ResourceEntry{Type: "container", ID: "c2"})
	_ = l.Append(ResourceEntry{Type: "volume", ID: "v1"})

	if err := RewriteLedger(path, []ResourceEntry{{Type: "container", ID: "c2"}}); err != nil {
		t.Fatalf("RewriteLedger: %v", err)
	}

	entries, err := NewLedger(path).ReadAll()
	if err != nil {
		t.Fatalf("ReadAll after rewrite: %v", err)
	}
	if len(entries) != 1 || entries[0].Type != "container" || entries[0].ID != "c2" {
		t.Fatalf("entries after rewrite = %+v, want exactly [{container c2}]", entries)
	}
}

// TestRewriteLedger_EmptyEntries_MissingFileSemantics pins RewriteLedger's
// "empty entries" contract: the file still exists but ReadAll treats it
// exactly like a missing ledger (no entries) — matching ensureLoaded's
// existing "missing file -> nil, not an error" behavior.
func TestRewriteLedger_EmptyEntries_MissingFileSemantics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	_ = NewLedger(path).Append(ResourceEntry{Type: "container", ID: "c1"})

	if err := RewriteLedger(path, nil); err != nil {
		t.Fatalf("RewriteLedger: %v", err)
	}

	entries, err := NewLedger(path).ReadAll()
	if err != nil {
		t.Fatalf("ReadAll after rewrite: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("entries after rewrite = %+v, want none", entries)
	}
}

// TestRewriteLedger_CreatesFileWhenMissing covers RewriteLedger on a path
// that never existed — internal/reap only calls it when it already read
// the ledger successfully, but the function's own contract should not
// silently no-op on a fresh path either.
func TestRewriteLedger_CreatesFileWhenMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")

	if err := RewriteLedger(path, []ResourceEntry{{Type: "network", ID: "n1"}}); err != nil {
		t.Fatalf("RewriteLedger: %v", err)
	}

	entries, err := NewLedger(path).ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(entries) != 1 || entries[0].Type != "network" || entries[0].ID != "n1" {
		t.Fatalf("entries = %+v, want exactly [{network n1}]", entries)
	}
}

// splitNonEmpty splits s by sep, dropping empty tokens.
func splitNonEmpty(s string, sep byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
