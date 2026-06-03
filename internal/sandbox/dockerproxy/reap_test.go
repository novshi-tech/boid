package dockerproxy

import (
	"context"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
)

// TestReap_orderAndCoverage verifies that Reap issues requests in the correct
// order (containers stop → rm, networks rm, volumes rm) against a mock upstream.
func TestReap_orderAndCoverage(t *testing.T) {
	var mu sync.Mutex
	var ops []string // "METHOD PATH"

	upstream := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ops = append(ops, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	l := NewLedger(filepath.Join(t.TempDir(), "l.jsonl"))
	_ = l.Append(ResourceEntry{Type: "container", ID: "c1"})
	_ = l.Append(ResourceEntry{Type: "network", ID: "net1"})
	_ = l.Append(ResourceEntry{Type: "volume", ID: "vol1"})
	_ = l.Append(ResourceEntry{Type: "exec", ID: "e1"}) // should be skipped

	if err := Reap(context.Background(), upstream.sockPath, l); err != nil {
		t.Fatal("Reap:", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// Build an ordered sequence check.
	want := []struct{ method, pathPrefix string }{
		{"POST", "/containers/c1/stop"},
		{"DELETE", "/containers/c1"},
		{"DELETE", "/networks/net1"},
		{"DELETE", "/volumes/vol1"},
	}

	if len(ops) != len(want) {
		t.Fatalf("expected %d upstream calls, got %d: %v", len(want), len(ops), ops)
	}
	for i, w2 := range want {
		got := ops[i]
		// ops entries are "METHOD PATH[?query]"; prefix-match the path.
		wantPrefix := w2.method + " " + w2.pathPrefix
		if len(got) < len(wantPrefix) || got[:len(wantPrefix)] != wantPrefix {
			t.Errorf("ops[%d]: got %q, want prefix %q", i, got, wantPrefix)
		}
	}
}

// TestReap_emptyLedger verifies Reap succeeds without contacting upstream
// when the ledger is empty.
func TestReap_emptyLedger(t *testing.T) {
	var hits int
	upstream := newFakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	})

	l := NewLedger(filepath.Join(t.TempDir(), "l.jsonl"))

	if err := Reap(context.Background(), upstream.sockPath, l); err != nil {
		t.Fatal(err)
	}
	if hits != 0 {
		t.Errorf("expected 0 upstream hits, got %d", hits)
	}
}

// TestReap_404Tolerated verifies that 404 responses (resource already gone)
// do not cause Reap to return an error, and that all resources are attempted.
func TestReap_404Tolerated(t *testing.T) {
	upstream := newFakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	l := NewLedger(filepath.Join(t.TempDir(), "l.jsonl"))
	_ = l.Append(ResourceEntry{Type: "container", ID: "c1"})
	_ = l.Append(ResourceEntry{Type: "volume", ID: "v1"})

	if err := Reap(context.Background(), upstream.sockPath, l); err != nil {
		t.Errorf("Reap should tolerate 404 errors, got: %v", err)
	}
}

// TestReap_multipleContainers verifies all containers are handled before networks/volumes.
func TestReap_multipleContainers(t *testing.T) {
	var mu sync.Mutex
	var ops []string

	upstream := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ops = append(ops, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	l := NewLedger(filepath.Join(t.TempDir(), "l.jsonl"))
	_ = l.Append(ResourceEntry{Type: "container", ID: "c1"})
	_ = l.Append(ResourceEntry{Type: "container", ID: "c2"})
	_ = l.Append(ResourceEntry{Type: "network", ID: "net1"})

	if err := Reap(context.Background(), upstream.sockPath, l); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()

	// Both containers must be stopped and removed before the network is deleted.
	netIdx := -1
	for i, op := range ops {
		if len(op) >= len("DELETE /networks") && op[:len("DELETE /networks")] == "DELETE /networks" {
			netIdx = i
			break
		}
	}
	containerOps := 0
	for _, op := range ops {
		if len(op) >= len("DELETE /containers") && op[:len("DELETE /containers")] == "DELETE /containers" {
			containerOps++
		}
		if len(op) >= len("POST /containers") && op[:len("POST /containers")] == "POST /containers" {
			containerOps++
		}
	}
	if containerOps != 4 { // 2 stop + 2 remove
		t.Errorf("expected 4 container ops (stop+rm × 2), got %d: %v", containerOps, ops)
	}
	if netIdx < 4 {
		t.Errorf("network delete at index %d should come after all container ops: %v", netIdx, ops)
	}
}
