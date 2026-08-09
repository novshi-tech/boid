package apiwire

// ConfigApplyResult is POST /api/config's response body.
type ConfigApplyResult struct {
	// Warnings are pre-formatted, operator-facing lines (docs/plans/
	// volume-only-daemon.md §論点 f's exact wording for the
	// restart-required and sandbox.backend-retirement cases) — the CLI
	// prints these verbatim rather than reconstructing them client-side,
	// so the daemon (which alone knows exactly which leaf paths actually
	// changed) is the single source of truth for when a warning fires.
	Warnings []string `json:"warnings,omitempty"`
}

// ConfigMutateOp names a POST /api/config/mutate operation.
type ConfigMutateOp string

const (
	// ConfigMutateSet sets a scalar (1 Value) or replaces an array
	// wholesale (multiple Values) at Key.
	ConfigMutateSet ConfigMutateOp = "set"
	// ConfigMutateUnset removes Key (Value is ignored).
	ConfigMutateUnset ConfigMutateOp = "unset"
)

// ConfigMutateRequest is POST /api/config/mutate's request body: either a
// single `boid config set <key> <value...>` / `unset <key>` operation (the
// Op/Key/Value fields, unchanged since BLOCKER 1 round 1), or — when Ops is
// non-empty — a BATCH of operations applied as one atomic read-modify-write
// (BLOCKER, codex review round 1 on PR #831).
//
// The batch shape exists because a single-op call validates the FULL
// document after every call (MutateConfig's doc comment), which makes it
// impossible to create a brand-new gateway.forges.<id> entry through a
// sequence of single-op calls: setting just "<id>.host" leaves the document
// with an empty "<id>.forge", which config.ValidateYAML rejects
// ("unrecognized forge \"\"") before a second call ever gets a chance to set
// "<id>.forge" — see internal/config/config.go's resolveForgeConfig. Ops
// applies every listed op to the same in-memory tree first, and validates
// only once against the fully-mutated result, so the three leaves of a new
// forge (or any other structural, multi-leaf add) land together or not at
// all.
//
// When Ops is non-empty, the top-level Op/Key/Value fields are ignored.
type ConfigMutateRequest struct {
	Op    ConfigMutateOp `json:"op,omitempty"`
	Key   string         `json:"key,omitempty"`
	Value []string       `json:"value,omitempty"`
	// Ops, when non-empty, requests a batch of set/unset operations
	// (BLOCKER, codex review round 1 on PR #831) — see this type's doc
	// comment. Each element's own (nested) Ops field, if set, is ignored;
	// only Op/Key/Value are read per element.
	Ops []ConfigMutateRequest `json:"ops,omitempty"`
}

// ConfigMutateResult is POST /api/config/mutate's response body: the same
// Warnings ConfigApplyResult carries, plus the resulting full document and
// its new revision (for a caller that wants to keep editing without a
// follow-up GET — not currently consumed by `boid config set/unset`, which
// only prints Warnings, but useful to a future Web UI settings page per
// docs/plans/volume-only-daemon.md §論点 f).
type ConfigMutateResult struct {
	ConfigApplyResult
	YAML     []byte `json:"yaml"`
	Revision string `json:"revision"`
}
