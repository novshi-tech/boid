package sandbox_test

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// fakePortStore is an in-memory sandbox.PortStore.
type fakePortStore struct {
	mu       sync.Mutex
	ports    map[string]int
	loadErr  error
	saveErr  error
	saveCall int
}

func newFakePortStore() *fakePortStore {
	return &fakePortStore{ports: make(map[string]int)}
}

func (f *fakePortStore) LoadPort(key string) (int, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loadErr != nil {
		return 0, false, f.loadErr
	}
	p, ok := f.ports[key]
	return p, ok, nil
}

func (f *fakePortStore) SavePort(key string, port int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saveCall++
	if f.saveErr != nil {
		return f.saveErr
	}
	f.ports[key] = port
	return nil
}

func (f *fakePortStore) get(key string) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.ports[key]
	return p, ok
}

// TestProxyManager_ReusesPersistedPort is the whole point of the feature
// (docs/plans/egress-proxy-stable-port.md): a daemon restart must land the
// same workspace's egress proxy back on the same port, so a job-side config
// that baked "http://boid-egress:<port>" keeps working. Simulated by running
// two managers in sequence against one store, exactly as two daemon
// lifetimes would.
func TestProxyManager_ReusesPersistedPort(t *testing.T) {
	store := newFakePortStore()

	ctx1, cancel1 := context.WithCancel(context.Background())
	m1 := sandbox.NewProxyManager()
	m1.PortStore = store
	m1.Start(ctx1)
	first, err := m1.GetOrCreate("ws-a", []string{"a.example.com"})
	if err != nil {
		t.Fatalf("GetOrCreate (first daemon): %v", err)
	}
	m1.StopAll()
	cancel1()

	if saved, ok := store.get("ws-a"); !ok || saved != first {
		t.Fatalf("store holds (%d, %v) after first daemon, want (%d, true)", saved, ok, first)
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	m2 := sandbox.NewProxyManager()
	m2.PortStore = store
	m2.Start(ctx2)
	defer m2.StopAll()
	second, err := m2.GetOrCreate("ws-a", []string{"a.example.com"})
	if err != nil {
		t.Fatalf("GetOrCreate (second daemon): %v", err)
	}

	if second != first {
		t.Errorf("port changed across daemon restart: first=%d second=%d — a baked proxy URL would go stale, which is the bug this feature exists to prevent", first, second)
	}
}

// TestProxyManager_AllocatesInsideConfiguredRange pins that a freshly
// allocated port lands inside the configured band. The default band sits
// BELOW the kernel's ephemeral range (32768-60999 on the machines boid runs
// on) on purpose: a fixed port drawn from the ephemeral range can be held as
// the source port of some unrelated outgoing connection at the moment the
// daemon restarts, and the bind then fails. `:0` never had that problem
// because it always took whatever was free.
func TestProxyManager_AllocatesInsideConfiguredRange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := newFakePortStore()
	m := sandbox.NewProxyManager()
	m.PortStore = store
	m.Start(ctx)
	defer m.StopAll()

	for _, key := range []string{"ws-a", "ws-b", "ws-c"} {
		port, err := m.GetOrCreate(key, nil)
		if err != nil {
			t.Fatalf("GetOrCreate(%q): %v", key, err)
		}
		if port < sandbox.DefaultProxyPortRangeLow || port > sandbox.DefaultProxyPortRangeHigh {
			t.Errorf("GetOrCreate(%q) = %d, want within [%d, %d] (outside the kernel ephemeral range)",
				key, port, sandbox.DefaultProxyPortRangeLow, sandbox.DefaultProxyPortRangeHigh)
		}
	}
}

// TestProxyManager_PersistedPortTaken_ReallocatesAndRepersists: when the
// persisted port is held by something else, the manager must still come up
// (this is a convenience feature, not an isolation mechanism — refusing to
// dispatch over it would be the wrong trade) AND must write the new port
// back, so the NEXT restart is stable again rather than re-rolling forever.
func TestProxyManager_PersistedPortTaken_ReallocatesAndRepersists(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Occupy a port inside the band and persist it as ws-a's.
	squatter, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen squatter: %v", err)
	}
	defer squatter.Close()
	taken := squatter.Addr().(*net.TCPAddr).Port

	store := newFakePortStore()
	if err := store.SavePort("ws-a", taken); err != nil {
		t.Fatal(err)
	}

	m := sandbox.NewProxyManager()
	m.PortStore = store
	m.BindHost = "127.0.0.1"
	// Narrow the band to the squatted port plus a couple of neighbours, so
	// the reallocation path is what is actually exercised.
	m.PortRangeLow, m.PortRangeHigh = taken, taken+2
	m.Start(ctx)
	defer m.StopAll()

	port, err := m.GetOrCreate("ws-a", nil)
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if port == taken {
		t.Fatalf("GetOrCreate = %d, the port the squatter holds", taken)
	}
	if saved, ok := store.get("ws-a"); !ok || saved != port {
		t.Errorf("store holds (%d, %v), want the reallocated port %d — without re-persisting, every restart would roll a new port", saved, ok, port)
	}
}

// TestProxyManager_RangeExhausted_FallsBackToEphemeral: a fully squatted
// band must degrade to the pre-feature behaviour (`:0`) rather than failing
// dispatch. Port stability is a convenience; egress isolation is carried by
// the allowlist and the per-workspace listener split, neither of which
// depends on the port number being stable.
func TestProxyManager_RangeExhausted_FallsBackToEphemeral(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A one-port band with that port squatted is the exhaustion case, and
	// the only way to construct it deterministically: ports handed out by
	// `:0` are not consecutive, so squatting N of them does not produce a
	// fully-occupied span.
	squatter, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen squatter: %v", err)
	}
	defer squatter.Close()
	low := squatter.Addr().(*net.TCPAddr).Port
	high := low

	store := newFakePortStore()
	m := sandbox.NewProxyManager()
	m.PortStore = store
	m.BindHost = "127.0.0.1"
	m.PortRangeLow, m.PortRangeHigh = low, high
	m.Start(ctx)
	defer m.StopAll()

	port, err := m.GetOrCreate("ws-a", nil)
	if err != nil {
		t.Fatalf("GetOrCreate must not fail when the band is exhausted: %v", err)
	}
	if port == 0 {
		t.Fatal("GetOrCreate returned port 0")
	}
	// Nothing may be persisted: an ephemeral port is not stable, and
	// recording it would hand the next restart a port with no meaning.
	if saved, ok := store.get("ws-a"); ok {
		t.Errorf("store recorded ephemeral fallback port %d; it must record nothing", saved)
	}
}

// TestProxyManager_NoPortStore_UsesEphemeral pins the opt-in shape: every
// caller that predates this feature (and every test that builds a bare
// ProxyManager) keeps the original `:0` behaviour untouched.
func TestProxyManager_NoPortStore_UsesEphemeral(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := sandbox.NewProxyManager()
	m.Start(ctx)
	defer m.StopAll()

	port, err := m.GetOrCreate("ws-a", nil)
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if port == 0 {
		t.Fatal("GetOrCreate returned port 0")
	}
}

// TestProxyManager_StoreErrorsAreNonFatal: the store is a convenience, so
// neither a failing Load nor a failing Save may take dispatch down with it.
func TestProxyManager_StoreErrorsAreNonFatal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	t.Run("load", func(t *testing.T) {
		store := newFakePortStore()
		store.loadErr = fmt.Errorf("db is down")
		m := sandbox.NewProxyManager()
		m.PortStore = store
		m.Start(ctx)
		defer m.StopAll()
		if _, err := m.GetOrCreate("ws-a", nil); err != nil {
			t.Errorf("GetOrCreate with a failing LoadPort = %v, want nil", err)
		}
	})

	t.Run("save", func(t *testing.T) {
		store := newFakePortStore()
		store.saveErr = fmt.Errorf("db is down")
		m := sandbox.NewProxyManager()
		m.PortStore = store
		m.Start(ctx)
		defer m.StopAll()
		if _, err := m.GetOrCreate("ws-a", nil); err != nil {
			t.Errorf("GetOrCreate with a failing SavePort = %v, want nil", err)
		}
	})
}

// TestProxyManager_ReservedKeyIsPersistable pins that the no-workspace
// listener key (dispatcher.NoWorkspaceProxyKey, "__no_workspace__") is a
// legal store key. It is deliberately NOT a workspace slug, which is why the
// persistence table is keyed by proxy key with no foreign key to workspaces
// — see the design doc's 永続化先 section.
func TestProxyManager_ReservedKeyIsPersistable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := newFakePortStore()
	m := sandbox.NewProxyManager()
	m.PortStore = store
	m.Start(ctx)
	defer m.StopAll()

	port, err := m.GetOrCreate("__no_workspace__", nil)
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if saved, ok := store.get("__no_workspace__"); !ok || saved != port {
		t.Errorf("store holds (%d, %v), want (%d, true)", saved, ok, port)
	}
}
