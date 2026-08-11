package sandbox

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
)

// ProxyManager owns a set of long-lived per-workspace HTTP(S) egress proxies.
// Each workspace gets its own listener on a distinct loopback port; the
// allowlist of every listener can be live-swapped via Proxy.SetAllowed so
// dispatch-time changes (workspace.yaml edits, new kit, …) take effect
// immediately for the next sandbox without restarting the listener.
//
// Concurrent sandboxes launched under the same workspace share one listener
// — when their resolved allowlists differ, the most recent dispatch wins.
// This matches the semantics of the rest of the workspace surface (env,
// kits, …) where workspace state is read fresh at dispatch time and not
// frozen per-sandbox.
//
// A ProxyManager is created via NewProxyManager and must be started with
// Start(ctx) before GetOrCreate is called. StopAll closes every listener;
// the manager is single-shot and must not be reused after StopAll.
//
// Design rationale: per-workspace port separation (rather than embedded
// HTTPS_PROXY basic-auth) was chosen for client compatibility — many tools
// in the wild parse the proxy URL loosely or ignore the userinfo entirely.
type ProxyManager struct {
	// BindHost, when non-empty, overrides the loopback-only default
	// ("127.0.0.1") every listener GetOrCreate starts binds to ([Blocker 2,
	// PR7 codex review] — docs/plans/phase6-container-backend.md §決定5).
	// A container-backend deploy runs the daemon inside its own container;
	// a sibling job container reaches the egress proxy over the shared
	// compose network by this daemon container's own IP, which a
	// loopback-bound listener is unreachable from — see internal/server's
	// composeBindHost doc comment for the full rationale. Set once, before
	// Start (internal/server's New(), based on the config-selected sandbox
	// backend — §決定11's global-not-per-job selection), and never changed
	// again: every listener this manager ever creates (the default
	// workspace one included) shares the same bind host for the life of
	// the process. Empty (every pre-PR7 caller/test) preserves the
	// original "127.0.0.1" behavior exactly.
	BindHost string

	// PortStore, when non-nil, makes a listener's port survive daemon
	// restarts: the port allocated for a key is written here and reused on
	// the next process lifetime (docs/plans/egress-proxy-stable-port.md).
	//
	// Why this exists: Proxy ports used to be pure `:0` ephemeral, so every
	// daemon restart moved every workspace's egress proxy to a new port.
	// Tools that read the proxy from the environment (curl, git, boid's own
	// traffic) never noticed, but tools that BAKE it into a config file did
	// — a `~/.npmrc` on the persistent workspace HOME volume carrying a
	// long-dead port made npm/pnpm retry ECONNREFUSED on a one-minute
	// backoff, which reads as a hang rather than an error. See the design
	// doc's 背景 section for the incident this came from.
	//
	// nil (every pre-feature caller and every test that builds a bare
	// ProxyManager) keeps the original ephemeral behaviour exactly.
	PortStore PortStore

	// PortRangeLow/PortRangeHigh bound the band new ports are allocated
	// from, inclusive. Zero means DefaultProxyPortRange{Low,High}. Only
	// consulted when PortStore is set.
	PortRangeLow  int
	PortRangeHigh int

	mu      sync.Mutex
	ctx     context.Context
	proxies map[string]*managedProxy
	started bool
}

// PortStore persists the port allocated to a proxy key across daemon
// restarts. Implemented outside this package (internal/sandbox must not
// import internal/db) and injected, the same shape dispatcher.ProxyAllocator
// already uses in the other direction.
//
// Both methods are best-effort from the manager's point of view: an error
// from either degrades port stability but never fails dispatch.
type PortStore interface {
	// LoadPort returns the port recorded for key. ok is false when the key
	// has no record yet.
	LoadPort(key string) (port int, ok bool, err error)
	// SavePort records port as key's allocation, replacing any previous
	// record for that key.
	SavePort(key string, port int) error
}

// DefaultProxyPortRangeLow/High bound the default allocation band.
//
// The band sits deliberately BELOW the kernel's ephemeral port range, which
// is 32768-60999 on every machine boid runs on (`net.ipv4.ip_local_port_range`,
// verified on both the host and inside the daemon container). Drawing a
// FIXED port from the ephemeral range would mean that whenever some
// unrelated outgoing connection happens to hold that number as its source
// port at restart time, the bind fails — a failure mode the old `:0`
// allocation could not have, because it always took whatever was free. An
// operator who has lowered ip_local_port_range can move the band with
// PortRangeLow/PortRangeHigh.
const (
	DefaultProxyPortRangeLow  = 30000
	DefaultProxyPortRangeHigh = 32767
)

type managedProxy struct {
	proxy *Proxy
	port  int
}

// NewProxyManager returns a fresh, unstarted ProxyManager.
func NewProxyManager() *ProxyManager {
	return &ProxyManager{proxies: make(map[string]*managedProxy)}
}

// Start binds the manager to ctx. Listener teardown follows ctx
// cancellation; StopAll() is the explicit alternative.
func (m *ProxyManager) Start(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ctx = ctx
	m.started = true
}

// GetOrCreate returns the port of the listener bound to workspaceID after
// (re)applying allowed as its egress allowlist. If no listener exists yet
// for workspaceID, a new one is started on a free loopback port.
//
// allowed is copied internally — callers may mutate the slice after the
// call. An empty workspaceID is a programmer error: the manager refuses to
// allocate an unkeyed listener.
func (m *ProxyManager) GetOrCreate(workspaceID string, allowed []string) (int, error) {
	if workspaceID == "" {
		return 0, fmt.Errorf("proxy manager: workspace id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.started || m.ctx == nil {
		return 0, fmt.Errorf("proxy manager: not started")
	}
	if mp, ok := m.proxies[workspaceID]; ok {
		mp.proxy.SetAllowed(allowed)
		return mp.port, nil
	}
	proxy := NewProxy(allowed)
	proxy.BindHost = m.BindHost
	port, err := m.startStable(proxy, workspaceID)
	if err != nil {
		return 0, fmt.Errorf("proxy manager: start workspace %q: %w", workspaceID, err)
	}
	m.proxies[workspaceID] = &managedProxy{proxy: proxy, port: port}
	return port, nil
}

// startStable binds proxy on a port that survives daemon restarts when a
// PortStore is configured, and on a plain ephemeral port when it is not.
//
// The order is: persisted port first (the steady-state path, and the whole
// reason this exists), then a deterministic walk of the band, then `:0`.
// Every step down that ladder is a degradation in stability, never in
// isolation — a workspace's egress is confined by its own listener's
// allowlist, which is unaffected by which port that listener sits on. That
// is why exhaustion falls back instead of failing dispatch.
func (m *ProxyManager) startStable(proxy *Proxy, key string) (int, error) {
	if m.PortStore == nil {
		return proxy.Start(m.ctx)
	}

	persisted, ok, err := m.PortStore.LoadPort(key)
	if err != nil {
		slog.Warn("egress proxy: reading the persisted port failed; allocating a fresh one",
			"key", key, "error", err)
		ok = false
	}
	if ok {
		proxy.DesiredPort = persisted
		if port, startErr := proxy.Start(m.ctx); startErr == nil {
			return port, nil
		}
		// Deliberately loud, and deliberately naming both numbers: this is
		// the exact moment a job-side config that baked the old port goes
		// stale, and the symptom on the job side (a retry loop against a
		// dead port) gives no hint of what happened.
		slog.Warn("egress proxy: the persisted port is unavailable, reallocating",
			"key", key, "old_port", persisted,
			"hint", "job-side configs that baked the old port (e.g. ~/.npmrc) will need updating")
	}

	low, high := m.portRange()
	// Start the walk at a position derived from the key so that a store
	// that was lost (fresh DB, restored backup) still tends to hand each
	// key back the port it had. Best-effort — the walk below moves on as
	// soon as a candidate is taken, so nothing depends on this holding.
	span := high - low + 1
	start := int(hashKey(key) % uint32(span))
	for i := 0; i < span; i++ {
		candidate := low + (start+i)%span
		proxy.DesiredPort = candidate
		port, startErr := proxy.Start(m.ctx)
		if startErr != nil {
			continue
		}
		if saveErr := m.PortStore.SavePort(key, port); saveErr != nil {
			// The listener is up and usable; only the NEXT restart's
			// stability is lost. Not worth tearing down a working proxy.
			slog.Warn("egress proxy: persisting the allocated port failed; it may change on the next restart",
				"key", key, "port", port, "error", saveErr)
		}
		return port, nil
	}

	// Nothing free in the band. Fall back to the pre-feature behaviour
	// rather than refusing to dispatch, and persist nothing — recording an
	// ephemeral port would hand the next restart a number with no meaning.
	slog.Warn("egress proxy: no free port in the configured range; falling back to an ephemeral port (it will not survive a restart)",
		"key", key, "range_low", low, "range_high", high)
	proxy.DesiredPort = 0
	return proxy.Start(m.ctx)
}

func (m *ProxyManager) portRange() (low, high int) {
	low, high = m.PortRangeLow, m.PortRangeHigh
	if low <= 0 || high <= 0 || high < low {
		return DefaultProxyPortRangeLow, DefaultProxyPortRangeHigh
	}
	return low, high
}

// hashKey is FNV-1a over the proxy key. Any stable, well-spread hash would
// do; the value is only ever used to pick a starting offset in the band.
func hashKey(key string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return h.Sum32()
}

// StopAll closes every listener owned by the manager. Subsequent
// GetOrCreate calls return an error.
func (m *ProxyManager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mp := range m.proxies {
		mp.proxy.Stop()
	}
	m.proxies = nil
	m.started = false
}

// Count returns the number of active per-workspace listeners. Useful for
// diagnostics and tests; not part of the dispatch hot path.
func (m *ProxyManager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.proxies)
}
