package sandbox

import (
	"context"
	"fmt"
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

	mu      sync.Mutex
	ctx     context.Context
	proxies map[string]*managedProxy
	started bool
}

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
	port, err := proxy.Start(m.ctx)
	if err != nil {
		return 0, fmt.Errorf("proxy manager: start workspace %q: %w", workspaceID, err)
	}
	m.proxies[workspaceID] = &managedProxy{proxy: proxy, port: port}
	return port, nil
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
