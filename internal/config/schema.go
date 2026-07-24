package config

import "strings"

// This file defines the CLI-editable schema for config.yaml
// (docs/plans/volume-only-daemon.md §論点 f: "boid config get/set/unset/apply/edit").
// It is the single source of truth `internal/config`'s dotted-path
// operations (dotted.go) and validation (validate.go) both consult — every
// leaf a `boid config set/unset` can target, its value shape, and whether a
// daemon can hot-apply a change to it without a restart.
//
// default_harness is deliberately NOT part of this schema: the config key
// (and config.DefaultHarness()/SetDefaultHarness()) was removed outright in
// Phase 2.5 PR7 (docs/ja/reference/config-yaml.md's "default_harness (撤去済み)"
// section) — it does not exist on the Config struct any more. The plan
// doc's §論点 f example (written the same day as this PR, before that
// history was cross-checked against the current schema) still lists it in
// the CLI example and the dynamic-reload table; this omission is a
// deliberate, flagged deviation (see the PR body) rather than an oversight
// — `boid config set default_harness ...` / `get default_harness` fall
// through to the ordinary "unknown key" rejection path, exactly like any
// other typo.

// FieldKind describes the shape of value a schema leaf accepts.
type FieldKind int

const (
	// KindString is a single scalar string.
	KindString FieldKind = iota
	// KindBool is a single scalar bool ("true"/"false").
	KindBool
	// KindDuration is a single scalar Go duration string (e.g. "24h").
	KindDuration
	// KindStringArray is zero or more scalar strings (`boid config set` multi-arg).
	KindStringArray
	// KindEnum is a single scalar string constrained to EnumValues.
	KindEnum
)

// ReloadClass classifies whether the daemon can apply a changed leaf live
// (docs/plans/volume-only-daemon.md §論点 f "reload semantics" table).
type ReloadClass int

const (
	// ReloadDynamic keys are hot-reloaded silently (an info log line, no
	// operator-facing warning) as soon as the daemon accepts the write.
	ReloadDynamic ReloadClass = iota
	// ReloadRestartRequired keys are persisted but only take effect on the
	// next daemon restart — the daemon prints a loud warning naming the
	// restart command.
	ReloadRestartRequired
	// ReloadRetirementWarning is sandbox.backend's own bucket: still a
	// fully valid, accepted write (its removal is PR-4, docs/plans/
	// volume-only-daemon.md §論点 e) but flagged with a retirement notice
	// on every successful set, distinct wording from the ordinary
	// restart-required warning.
	ReloadRetirementWarning
)

// FieldSpec describes one CLI-editable scalar/array leaf in config.yaml.
// Path is dotted, with "*" standing in for a single wildcard segment (only
// ever gateway.forges.<any-id> today).
type FieldSpec struct {
	Path       string
	Kind       FieldKind
	Reload     ReloadClass
	EnumValues []string
}

// Schema is every leaf path `boid config set/get/unset` (and `apply`'s
// structural validation) recognizes. Order matches config.yaml's own
// section order (config.go's Config struct field order) for readability in
// error messages / help output that enumerates it.
var Schema = []FieldSpec{
	{Path: "gc.enabled", Kind: KindBool, Reload: ReloadRestartRequired},
	{Path: "gc.interval", Kind: KindDuration, Reload: ReloadRestartRequired},
	{Path: "gc.older_than", Kind: KindDuration, Reload: ReloadRestartRequired},

	{Path: "web.public_url", Kind: KindString, Reload: ReloadDynamic},
	{Path: "web.http_addr", Kind: KindString, Reload: ReloadRestartRequired},

	{Path: "notify.command", Kind: KindStringArray, Reload: ReloadDynamic},

	{Path: "sandbox.allowed_domains", Kind: KindStringArray, Reload: ReloadDynamic},
	{Path: "sandbox.backend", Kind: KindEnum, Reload: ReloadRetirementWarning, EnumValues: []string{"userns", "container"}},

	{Path: "task_ask.disconnect_grace", Kind: KindDuration, Reload: ReloadRestartRequired},

	{Path: "gateway.forges.*.host", Kind: KindString, Reload: ReloadRestartRequired},
	{Path: "gateway.forges.*.forge", Kind: KindEnum, Reload: ReloadRestartRequired, EnumValues: []string{"github", "bitbucket"}},
	{Path: "gateway.forges.*.secret_key", Kind: KindString, Reload: ReloadRestartRequired},
}

// segments splits a dotted path into its components. Exported for reuse by
// dotted.go/validate.go so both stay byte-for-byte consistent about what
// "a path" means (currently just strings.Split on ".").
func segments(path string) []string {
	if path == "" {
		return nil
	}
	return strings.Split(path, ".")
}

// pathMatches reports whether specPath (which may contain "*" wildcard
// segments) matches the concrete, wildcard-free path segments in actual.
func pathMatches(specPath []string, actual []string) bool {
	if len(specPath) != len(actual) {
		return false
	}
	for i, seg := range specPath {
		if seg == "*" {
			if actual[i] == "" {
				return false
			}
			continue
		}
		if seg != actual[i] {
			return false
		}
	}
	return true
}

// ResolveField looks up the FieldSpec matching a concrete dotted path (no
// wildcards in path itself — path is what a user typed, e.g.
// "gateway.forges.github.host"). Returns (nil, false) when no schema leaf
// matches — the caller (dotted.go) is responsible for turning that into a
// helpful "unknown key" error, since only it has the surrounding tree to
// offer a closest-match suggestion against.
func ResolveField(path string) (*FieldSpec, bool) {
	actual := segments(path)
	for i := range Schema {
		if pathMatches(segments(Schema[i].Path), actual) {
			return &Schema[i], true
		}
	}
	return nil, false
}

// IsForgeEntryPath reports whether path names a whole gateway.forges.<id>
// entry (exactly "gateway.forges.<id>", no further segment) — the one
// dotted path `boid config unset` treats specially, per docs/plans/
// volume-only-daemon.md §論点 f's unilateral decision: "Removing a map
// entry (e.g. gateway.forges.github) removes the whole entry." id is
// returned when ok is true.
func IsForgeEntryPath(path string) (id string, ok bool) {
	segs := segments(path)
	if len(segs) != 3 || segs[0] != "gateway" || segs[1] != "forges" {
		return "", false
	}
	if segs[2] == "" {
		return "", false
	}
	return segs[2], true
}
