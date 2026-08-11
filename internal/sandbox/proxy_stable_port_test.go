package sandbox_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// fakePortStore is an in-memory sandbox.PortStore.
type fakePortStore struct {
	mu          sync.Mutex
	ports       map[string]int
	loadErr     error
	saveErr     error
	reservedErr error
	saveCall    int
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

func (f *fakePortStore) ReservedPorts() (map[int]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reservedErr != nil {
		return nil, f.reservedErr
	}
	out := make(map[int]string, len(f.ports))
	for k, p := range f.ports {
		out[p] = k
	}
	return out, nil
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

// TestProxyManager_AllocatesInsideDefaultRange pins that a freshly
// allocated port lands inside the DEFAULT band (this test deliberately sets
// no PortRangeLow/High — the configured-band case is covered by
// TestProxyManager_AllocatesInsideConfiguredRange below). The default band sits
// BELOW the kernel's ephemeral range (32768-60999 on the machines boid runs
// on) on purpose: a fixed port drawn from the ephemeral range can be held as
// the source port of some unrelated outgoing connection at the moment the
// daemon restarts, and the bind then fails. `:0` never had that problem
// because it always took whatever was free.
func TestProxyManager_AllocatesInsideDefaultRange(t *testing.T) {
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
//
// Asserting "err == nil && port != 0" would prove nothing — that holds with
// the whole feature reverted AND with the band being used. The real claim is
// that the port comes from the KERNEL's ephemeral range rather than boid's
// band, so the assertion reads the kernel's own range and checks membership.
func TestProxyManager_NoPortStore_UsesEphemeral(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	epLow, epHigh := kernelEphemeralRange(t)

	m := sandbox.NewProxyManager()
	m.Start(ctx)
	defer m.StopAll()

	port, err := m.GetOrCreate("ws-a", nil)
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if port < epLow || port > epHigh {
		t.Errorf("GetOrCreate = %d, want a kernel-ephemeral port in [%d, %d] — with no PortStore the manager must not allocate from boid's own band", port, epLow, epHigh)
	}
}

// kernelEphemeralRange reads net.ipv4.ip_local_port_range. boid is
// Linux-only, so this file is always present; the test skips rather than
// fails if it somehow cannot be read.
func kernelEphemeralRange(t *testing.T) (low, high int) {
	t.Helper()
	raw, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		t.Skipf("cannot read ip_local_port_range: %v", err)
	}
	parts := strings.Fields(string(raw))
	if len(parts) != 2 {
		t.Skipf("unexpected ip_local_port_range contents: %q", string(raw))
	}
	low, err = strconv.Atoi(parts[0])
	if err != nil {
		t.Skipf("unparseable ip_local_port_range low: %v", err)
	}
	high, err = strconv.Atoi(parts[1])
	if err != nil {
		t.Skipf("unparseable ip_local_port_range high: %v", err)
	}
	// The default band must sit outside the ephemeral range for the
	// membership assertion above to mean anything. If an operator (or CI
	// image) has lowered the range so they overlap, the test cannot
	// distinguish the two allocators.
	if sandbox.DefaultProxyPortRangeHigh >= low {
		t.Skipf("boid's default band [%d, %d] overlaps the kernel ephemeral range [%d, %d]; cannot distinguish allocators",
			sandbox.DefaultProxyPortRangeLow, sandbox.DefaultProxyPortRangeHigh, low, high)
	}
	return low, high
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

// TestProxyManager_DoesNotStealAnotherKeysReservedPort is the regression
// test for the hole the first self-review found: the walk used to decide a
// candidate was free purely by net.Listen succeeding.
//
// Listeners are created lazily at dispatch time, so a workspace that has not
// been dispatched since the last daemon restart holds a reservation with
// NOTHING listening on it — indistinguishable from a free port by a bind
// probe. Taking it silently moved that workspace's port and deleted its row,
// so its next dispatch saw "no record" and allocated afresh. That is exactly
// the incident this whole feature exists to prevent, reintroduced by the
// allocator itself, and with no log at either end.
func TestProxyManager_DoesNotStealAnotherKeysReservedPort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Two free ports; reserve the first for an idle key that is NOT running.
	l1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen probe: %v", err)
	}
	reserved := l1.Addr().(*net.TCPAddr).Port
	l1.Close() // nothing is listening — exactly the idle-workspace state

	store := newFakePortStore()
	if err := store.SavePort("ws-idle", reserved); err != nil {
		t.Fatal(err)
	}

	m := sandbox.NewProxyManager()
	m.PortStore = store
	m.BindHost = "127.0.0.1"
	// A two-port band: the reserved one and its neighbour. The newcomer must
	// take the neighbour, not the reservation.
	m.PortRangeLow, m.PortRangeHigh = reserved, reserved+1
	m.Start(ctx)
	defer m.StopAll()

	port, err := m.GetOrCreate("ws-new", nil)
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if port == reserved {
		t.Errorf("GetOrCreate = %d, which ws-idle has reserved — binding succeeded only because ws-idle is not dispatched right now", port)
	}
	if saved, ok := store.get("ws-idle"); !ok || saved != reserved {
		t.Errorf("ws-idle's reservation is now (%d, %v), want (%d, true) — its baked proxy URL must survive another workspace being allocated", saved, ok, reserved)
	}
}

// TestProxyManager_TakesReservedPortWhenNothingElseIsLeft: respecting
// reservations must not become a way to fail. When the band has no
// unreserved port left, the newcomer takes a reserved-but-idle one rather
// than degrading to an ephemeral port — a stable port for someone beats a
// stable port for no one. The displaced key loses its row (and gets a fresh
// port on its next dispatch), which is why the code logs that loudly.
func TestProxyManager_TakesReservedPortWhenNothingElseIsLeft(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	l1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen probe: %v", err)
	}
	reserved := l1.Addr().(*net.TCPAddr).Port
	l1.Close()

	store := newFakePortStore()
	if err := store.SavePort("ws-idle", reserved); err != nil {
		t.Fatal(err)
	}

	m := sandbox.NewProxyManager()
	m.PortStore = store
	m.BindHost = "127.0.0.1"
	m.PortRangeLow, m.PortRangeHigh = reserved, reserved // nothing else exists
	m.Start(ctx)
	defer m.StopAll()

	port, err := m.GetOrCreate("ws-new", nil)
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if port != reserved {
		t.Errorf("GetOrCreate = %d, want the reserved port %d (the band holds nothing else, so falling back to ephemeral would waste the only stable port available)", port, reserved)
	}
	if saved, ok := store.get("ws-new"); !ok || saved != reserved {
		t.Errorf("store holds (%d, %v) for ws-new, want (%d, true)", saved, ok, reserved)
	}
}

// TestProxyManager_PersistedPortOutsideBand_Rehomes: an operator who moves
// the band (the documented remedy for a band that overlaps the kernel's
// ephemeral range) must see workspaces actually move. Reusing a persisted
// port with no band-membership check made the config change a no-op for
// exactly the population it was written for.
func TestProxyManager_PersistedPortOutsideBand_Rehomes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	l1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen probe: %v", err)
	}
	inBand := l1.Addr().(*net.TCPAddr).Port
	l1.Close()

	// The stored port is free and bindable — the ONLY reason to move off it
	// is that it is outside the configured band.
	l2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen probe: %v", err)
	}
	outOfBand := l2.Addr().(*net.TCPAddr).Port
	l2.Close()
	if outOfBand >= inBand && outOfBand <= inBand+1 {
		t.Skipf("probe ports %d and %d are adjacent; cannot build a disjoint band", inBand, outOfBand)
	}

	store := newFakePortStore()
	if err := store.SavePort("ws-a", outOfBand); err != nil {
		t.Fatal(err)
	}

	m := sandbox.NewProxyManager()
	m.PortStore = store
	m.BindHost = "127.0.0.1"
	m.PortRangeLow, m.PortRangeHigh = inBand, inBand+1
	m.Start(ctx)
	defer m.StopAll()

	port, err := m.GetOrCreate("ws-a", nil)
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if port == outOfBand {
		t.Errorf("GetOrCreate = %d, the out-of-band persisted port — moving the band must move the workspace", port)
	}
	if port < inBand || port > inBand+1 {
		t.Errorf("GetOrCreate = %d, want within the configured band [%d, %d]", port, inBand, inBand+1)
	}
	if saved, ok := store.get("ws-a"); !ok || saved != port {
		t.Errorf("store holds (%d, %v), want the re-homed port %d", saved, ok, port)
	}
}

// TestProxyManager_AllocatesInsideConfiguredRange covers the band actually
// being configured (its sibling above exercises the built-in default).
func TestProxyManager_AllocatesInsideConfiguredRange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Probe a small window of free ports to use as the band.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen probe: %v", err)
	}
	low := l.Addr().(*net.TCPAddr).Port
	l.Close()
	high := low + 20

	store := newFakePortStore()
	m := sandbox.NewProxyManager()
	m.PortStore = store
	m.BindHost = "127.0.0.1"
	m.PortRangeLow, m.PortRangeHigh = low, high
	m.Start(ctx)
	defer m.StopAll()

	port, err := m.GetOrCreate("ws-a", nil)
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if port < low || port > high {
		t.Errorf("GetOrCreate = %d, want within the configured band [%d, %d]", port, low, high)
	}
}

// TestProxyManager_UnusablePortRange_FallsBackToDefaultBand: config
// validation only runs on the `boid config set/edit/apply` paths — a
// hand-edited or deploy-seeded config.yaml reaches the runtime through
// config.Load, which does not validate. An out-of-range band must therefore
// be rejected here too, or every allocation burns a full band of
// guaranteed-invalid binds and then silently degrades to ephemeral ports,
// i.e. turns the feature off with no diagnosis.
func TestProxyManager_UnusablePortRange_FallsBackToDefaultBand(t *testing.T) {
	for name, band := range map[string][2]int{
		"above the maximum port": {70000, 70999},
		"inverted":               {32000, 31000},
		"negative":               {-5, 100},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			store := newFakePortStore()
			m := sandbox.NewProxyManager()
			m.PortStore = store
			m.PortRangeLow, m.PortRangeHigh = band[0], band[1]
			m.Start(ctx)
			defer m.StopAll()

			port, err := m.GetOrCreate("ws-a", nil)
			if err != nil {
				t.Fatalf("GetOrCreate: %v", err)
			}
			if port < sandbox.DefaultProxyPortRangeLow || port > sandbox.DefaultProxyPortRangeHigh {
				t.Errorf("GetOrCreate = %d, want the built-in band [%d, %d]", port, sandbox.DefaultProxyPortRangeLow, sandbox.DefaultProxyPortRangeHigh)
			}
		})
	}
}

// TestProxyManager_ReservedPortsErrorIsNonFatal: the reservation lookup is a
// safety net, not a gate. Losing it degrades to the old "bind probe only"
// behaviour rather than refusing to allocate — an unreachable listener is
// worse than a small risk of displacing an idle key.
func TestProxyManager_ReservedPortsErrorIsNonFatal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := newFakePortStore()
	store.reservedErr = fmt.Errorf("db is down")
	m := sandbox.NewProxyManager()
	m.PortStore = store
	m.Start(ctx)
	defer m.StopAll()

	if _, err := m.GetOrCreate("ws-a", nil); err != nil {
		t.Errorf("GetOrCreate with a failing ReservedPorts = %v, want nil", err)
	}
}

// TestProxyManager_StopReleasesPortImmediately is the regression test for the
// listener leak CI found (TestProxyManager_ReusesPersistedPort failed there
// while passing locally).
//
// Proxy.Start hands the listener to http.Server.Serve on a NEW GOROUTINE. If
// Stop's Close lands before that goroutine is scheduled, Serve sees an
// already-shutting-down server, returns ErrServerClosed immediately, and
// never closes the listener it was handed — leaving the port bound for the
// life of the process. A loaded machine loses that race often; a developer's
// laptop almost never does.
//
// Stopping and immediately re-binding the same port is the tightest way to
// pin it, and the loop makes the scheduling race actually show up.
func TestProxyManager_StopReleasesPortImmediately(t *testing.T) {
	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithCancel(context.Background())

		store := newFakePortStore()
		m := sandbox.NewProxyManager()
		m.PortStore = store
		m.BindHost = "127.0.0.1"
		m.Start(ctx)

		port, err := m.GetOrCreate("ws-a", nil)
		if err != nil {
			cancel()
			t.Fatalf("iteration %d: GetOrCreate: %v", i, err)
		}
		// Deliberately no delay: StopAll may well run before the Serve
		// goroutine has been scheduled at all. That is the case under test.
		m.StopAll()
		cancel()

		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			t.Fatalf("iteration %d: port %d still bound after StopAll: %v — a leaked listener means the next daemon lifetime cannot reuse this workspace's port", i, port, err)
		}
		ln.Close()
	}
}
