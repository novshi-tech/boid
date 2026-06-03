package dockerproxy

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
)

// ResourceEntry is a single entry in the Docker resource ledger.
type ResourceEntry struct {
	Type string `json:"type"` // "container", "network", "volume", "exec"
	ID   string `json:"id"`
}

// Ledger persists Docker resource IDs created by a sandbox job to a JSONL file,
// enabling both id scope checks and GC (Reap) after the job exits.
// The file path (runtime directory) is injected by the caller; Ledger itself has
// no knowledge of the daemon's directory structure.
type Ledger struct {
	path    string
	mu      sync.Mutex
	entries []ResourceEntry // in-memory mirror; populated lazily on first use
	loaded  bool
}

// NewLedger returns a Ledger backed by the given file path.
// The file is created on first Append if it does not exist.
func NewLedger(path string) *Ledger {
	return &Ledger{path: path}
}

// Append writes the entry to the ledger file (append + fsync) before updating
// the in-memory cache.  Callers must call Append before returning the creation
// response to the client so that "every ID the client knows is in the ledger".
func (l *Ledger) Append(e ResourceEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.ensureLoaded(); err != nil {
		return err
	}

	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}

	l.entries = append(l.entries, e)
	return nil
}

// ReadAll returns a snapshot of all entries currently in the ledger.
// Returns nil (not an error) when the ledger file does not exist yet.
func (l *Ledger) ReadAll() ([]ResourceEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.ensureLoaded(); err != nil {
		return nil, err
	}
	result := make([]ResourceEntry, len(l.entries))
	copy(result, l.entries)
	return result, nil
}

// Contains returns true when the ledger holds an entry with the given type and ID.
func (l *Ledger) Contains(resourceType, id string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.ensureLoaded(); err != nil {
		return false, err
	}
	for _, e := range l.entries {
		if e.Type == resourceType && e.ID == id {
			return true, nil
		}
	}
	return false, nil
}

// ensureLoaded reads entries from disk into the in-memory cache.
// Must be called with l.mu held.
func (l *Ledger) ensureLoaded() error {
	if l.loaded {
		return nil
	}
	f, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			l.loaded = true
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e ResourceEntry
		if json.Unmarshal(line, &e) == nil && e.ID != "" {
			l.entries = append(l.entries, e)
		}
	}
	l.loaded = true
	return sc.Err()
}
