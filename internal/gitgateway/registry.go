package gitgateway

import "sync"

// Permission is the access level a job token has on one repo.
type Permission int

const (
	// PermFetch allows git-upload-pack only (read/clone/fetch).
	PermFetch Permission = iota + 1
	// PermFetchPush allows both git-upload-pack and git-receive-pack.
	PermFetchPush
)

// Allows reports whether this permission covers the given operation.
func (p Permission) Allows(op Operation) bool {
	switch op {
	case OpPush:
		return p == PermFetchPush
	case OpFetch:
		return p == PermFetch || p == PermFetchPush
	default:
		return false
	}
}

// Entry is one job token's authorization: which repos it may reach and at
// what permission level. docs/plans/git-gateway-cutover.md PR3 の許可集合
// (自 project は fetch+push or fetch のみ、workspace peer は fetch のみ、
// read-only 追加許可 repo も fetch のみ) はすべて呼び出し側 (PR4 の dispatch
// 配線) が Repos map の構築時に表現する — Registry 自体は集合の形を知らない。
type Entry struct {
	Token string
	Repos map[RepoKey]Permission
}

// Registry is the job-token → allowed-repo-set store. It is a deliberately
// simple sync.RWMutex-guarded map (docs/plans/git-gateway-cutover.md PR3
// 実装ヒント: 「job token レジストリは sync.RWMutex で守った map[token]entry
// の素朴実装で足りる」); PR4 adds the dispatch-time Register/Unregister
// call sites and a richer lifecycle, but the shape here doesn't need to
// change for that.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]*Entry
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]*Entry)}
}

// Register creates a new entry with a freshly generated token and returns it.
func (r *Registry) Register(repos map[RepoKey]Permission) string {
	token := GenerateToken()
	r.RegisterToken(token, repos)
	return token
}

// RegisterToken creates (or replaces) an entry under an explicit token. This
// is useful for callers (and tests) that already have a token value to
// correlate with — e.g. PR4 dispatch, which will want the gateway job token
// alongside the job id in logs.
func (r *Registry) RegisterToken(token string, repos map[RepoKey]Permission) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[string]*Entry)
	}
	r.entries[token] = &Entry{Token: token, Repos: repos}
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

// Authorize reports whether token may perform op against repo. tokenValid is
// false when the token is unknown/expired (caller should respond 401);
// tokenValid is true but allowed is false when the token is valid but the
// repo/operation isn't in its allowed set (caller should respond 403).
func (r *Registry) Authorize(token string, repo RepoKey, op Operation) (allowed, tokenValid bool) {
	entry, ok := r.Lookup(token)
	if !ok {
		return false, false
	}
	perm, ok := entry.Repos[repo]
	if !ok {
		return false, true
	}
	return perm.Allows(op), true
}
