package apigateway

import "sync"

// Entry is one job token's authorization: the set of logical service names
// it may reach, whether it is restricted to safe (GET/HEAD) methods, plus
// the secret-store namespace and originating task id used downstream
// (credential resolution and timeline/audit recording, respectively).
//
// Services holds the effective (floor ∪ workspace) enabled-service set for
// the registering job's workspace (docs/plans/api-gateway.md §3
// "workspace 単位の service 有効化") — the caller (dispatcher) computes this
// union before calling Register; Registry itself has no notion of a floor or
// a workspace.
//
// ReadOnly mirrors task.readonly / command.readonly (docs/plans/api-gateway.md
// 前提となる決定事項: "task.readonly を HTTP メソッドに写像する") — Server.ServeHTTP
// rejects any non-GET/HEAD request when this is true, regardless of which
// service is targeted.
//
// TaskID is the originating task, used only for timeline/audit recording
// (docs/plans/api-gateway.md §論点3) — empty for a taskless job (`boid exec`),
// in which case recording is skipped rather than attributed to no task.
type Entry struct {
	Token     string
	Namespace string
	TaskID    string
	ReadOnly  bool
	Services  map[string]bool
}

// Registry is the job-token → allowed-service-set store. A deliberately
// simple sync.RWMutex-guarded map, mirroring internal/gitgateway.Registry's
// own shape and lifecycle contract (dispatch-time Register, job-completion
// Unregister).
type Registry struct {
	mu      sync.RWMutex
	entries map[string]*Entry
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]*Entry)}
}

// Register creates a new entry with a freshly generated token, scoped to
// namespace and taskID, and returns the token.
func (r *Registry) Register(services []string, namespace, taskID string, readOnly bool) string {
	token := GenerateToken()
	r.RegisterToken(token, services, namespace, taskID, readOnly)
	return token
}

// RegisterToken creates (or replaces) an entry under an explicit token. This
// is useful for callers (and tests) that already have a token value to
// correlate with.
func (r *Registry) RegisterToken(token string, services []string, namespace, taskID string, readOnly bool) string {
	set := make(map[string]bool, len(services))
	for _, s := range services {
		if s == "" {
			continue
		}
		set[s] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[string]*Entry)
	}
	r.entries[token] = &Entry{Token: token, Namespace: namespace, TaskID: taskID, ReadOnly: readOnly, Services: set}
	return token
}

// Unregister revokes a token. A subsequent Lookup/Authorize for it reports
// the token as invalid. Unregistering an unknown token is a no-op.
func (r *Registry) Unregister(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, token)
}

// Lookup returns the entry for token, if any. A nil Registry behaves as an
// always-empty one (every token reports invalid) rather than panicking, so a
// Server constructed without a registry fails closed with 401s instead of
// crashing.
func (r *Registry) Lookup(token string) (Entry, bool) {
	if r == nil {
		return Entry{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[token]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

// Authorize reports whether token may reach service. tokenValid is false
// when the token is unknown/expired (caller should respond 401); tokenValid
// is true but allowed is false when the token is valid but service isn't in
// its enabled set (caller should respond 403).
func (r *Registry) Authorize(token, service string) (allowed, tokenValid bool) {
	entry, ok := r.Lookup(token)
	if !ok {
		return false, false
	}
	return entry.Services[service], true
}
