package config

import (
	"fmt"
	"sort"
	"strconv"
	"time"
)

// Tree is the generic YAML representation `boid config get/set/unset`
// operates on: the same shape `yaml.Unmarshal(data, &tree)` produces for a
// config.yaml document (nested map[string]any / []any / scalar values).
// Operating on this generic shape — rather than the typed Config struct —
// mirrors the existing `boid web set-addr`/`set-url` local-edit pattern
// (cmd/web.go): it round-trips only the keys actually present, so an
// unrelated section of a hand-authored config.yaml a user never touches
// through this CLI is never silently reshaped.
type Tree = map[string]any

// GetPath reads the value at a dotted path in tree. ok is false when any
// segment along the way is absent, or an intermediate segment resolves to a
// non-map value (a scalar/array sitting where a nested key was expected).
func GetPath(tree Tree, path string) (value any, ok bool) {
	segs := segments(path)
	if len(segs) == 0 {
		return nil, false
	}
	cur := any(tree)
	for _, seg := range segs {
		m, isMap := cur.(Tree)
		if !isMap {
			return nil, false
		}
		v, present := m[seg]
		if !present {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// setPathRaw writes value at a dotted path in tree, creating intermediate
// maps as needed. Only called with schema-validated paths (ResolveField
// already confirmed every intermediate segment is a legitimate schema
// container), so it never needs to worry about clobbering an existing
// scalar with a map.
func setPathRaw(tree Tree, path string, value any) {
	segs := segments(path)
	cur := tree
	for i, seg := range segs {
		if i == len(segs)-1 {
			cur[seg] = value
			return
		}
		next, ok := cur[seg].(Tree)
		if !ok {
			next = Tree{}
			cur[seg] = next
		}
		cur = next
	}
}

// deletePathRaw removes the leaf key at a dotted path from tree, pruning
// now-empty intermediate maps behind it (so `unset`ting the last field of a
// gateway.forges.<id> entry, or the last forge under gateway.forges,
// doesn't leave a dangling `{}` behind in the round-tripped document).
// Returns whether the key was actually present.
func deletePathRaw(tree Tree, path string) bool {
	segs := segments(path)
	return deleteSegments(tree, segs)
}

func deleteSegments(m Tree, segs []string) bool {
	if len(segs) == 0 {
		return false
	}
	seg := segs[0]
	if len(segs) == 1 {
		if _, ok := m[seg]; !ok {
			return false
		}
		delete(m, seg)
		return true
	}
	next, ok := m[seg].(Tree)
	if !ok {
		return false
	}
	deleted := deleteSegments(next, segs[1:])
	if deleted && len(next) == 0 {
		delete(m, seg)
	}
	return deleted
}

// Get resolves a dotted path against tree for `boid config get <key>`.
// Returns the raw value (a scalar, []any, or Tree) and an error naming the
// closest known key when path is not a recognized schema leaf/container.
func Get(tree Tree, path string) (any, error) {
	if _, isForge := IsForgeEntryPath(path); isForge {
		v, ok := GetPath(tree, path)
		if !ok {
			return nil, fmt.Errorf("key not found: %s", path)
		}
		return v, nil
	}
	if _, ok := ResolveField(path); !ok {
		return nil, unknownKeyError(path)
	}
	v, ok := GetPath(tree, path)
	if !ok {
		return nil, fmt.Errorf("key not found: %s", path)
	}
	return v, nil
}

// Set validates and applies `boid config set <key> <value...>` against
// tree, returning the ReloadClass the caller should report to the operator.
// Fails when path is not a scalar/array schema leaf (an unresolved path, or
// one resolving to a struct/map slot like a bare "gateway.forges.github" —
// docs/plans/volume-only-daemon.md §論点 f's unilateral decision: set never
// targets a whole map entry, only unset does) — or when the given values
// don't match the leaf's Kind (wrong arity, bad enum, unparseable
// duration/bool).
func Set(tree Tree, path string, values []string) (ReloadClass, error) {
	spec, ok := ResolveField(path)
	if !ok {
		return 0, unknownKeyError(path)
	}
	value, err := coerceValues(*spec, values)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	setPathRaw(tree, path, value)
	return spec.Reload, nil
}

// Unset validates and applies `boid config unset <key>` against tree,
// returning the ReloadClass the caller should report. Two shapes:
//
//   - a whole gateway.forges.<id> entry ("gateway.forges.github") removes
//     the entire map entry — the unilateral decision documented on
//     IsForgeEntryPath.
//   - any other recognized scalar/array leaf removes just that key.
//
// Fails with "key not found" when the path is unrecognized OR is
// recognized but not currently present in tree — per §論点 f's unilateral
// decision, unset never silently no-ops on a missing key.
func Unset(tree Tree, path string) (ReloadClass, error) {
	if id, isForge := IsForgeEntryPath(path); isForge {
		_ = id
		if !deletePathRaw(tree, path) {
			return 0, fmt.Errorf("key not found: %s", path)
		}
		// A whole forge entry always carries restart-required fields
		// (host/forge/secret_key); removing it is classified the same way.
		return ReloadRestartRequired, nil
	}
	spec, ok := ResolveField(path)
	if !ok {
		return 0, unknownKeyError(path)
	}
	if !deletePathRaw(tree, path) {
		return 0, fmt.Errorf("key not found: %s", path)
	}
	return spec.Reload, nil
}

// coerceValues converts a `boid config set`'s raw CLI arguments into the
// Go value setPathRaw should store for spec's Kind, validating arity and
// per-Kind syntax along the way.
func coerceValues(spec FieldSpec, values []string) (any, error) {
	switch spec.Kind {
	case KindStringArray:
		out := make([]any, len(values))
		for i, v := range values {
			out[i] = v
		}
		return out, nil
	case KindString:
		if len(values) != 1 {
			return nil, fmt.Errorf("expected exactly 1 value, got %d", len(values))
		}
		return values[0], nil
	case KindBool:
		if len(values) != 1 {
			return nil, fmt.Errorf("expected exactly 1 value, got %d", len(values))
		}
		b, err := strconv.ParseBool(values[0])
		if err != nil {
			return nil, fmt.Errorf("invalid bool %q", values[0])
		}
		return b, nil
	case KindDuration:
		if len(values) != 1 {
			return nil, fmt.Errorf("expected exactly 1 value, got %d", len(values))
		}
		if _, err := time.ParseDuration(values[0]); err != nil {
			return nil, fmt.Errorf("invalid duration %q: %w", values[0], err)
		}
		return values[0], nil
	case KindEnum:
		if len(values) != 1 {
			return nil, fmt.Errorf("expected exactly 1 value, got %d", len(values))
		}
		for _, want := range spec.EnumValues {
			if values[0] == want {
				return values[0], nil
			}
		}
		return nil, fmt.Errorf("invalid value %q (want one of %s)", values[0], sortedJoin(spec.EnumValues))
	default:
		return nil, fmt.Errorf("unsupported field kind %v", spec.Kind)
	}
}

func sortedJoin(vals []string) string {
	cp := append([]string(nil), vals...)
	sort.Strings(cp)
	out := ""
	for i, v := range cp {
		if i > 0 {
			out += ", "
		}
		out += v
	}
	return out
}

// unknownKeyError reports path as unrecognized, suggesting the schema
// pattern with the smallest edit distance — e.g. "sandbox.alowed_domains"
// suggests "sandbox.allowed_domains". The suggestion is the raw schema
// path text (wildcard segments included verbatim, e.g.
// "gateway.forges.*.host") rather than an attempt to substitute the user's
// own segment back in — close enough to point a typo in the right
// direction without pretending to know which forge id they meant.
func unknownKeyError(path string) error {
	best := closestSchemaPath(path)
	if best == "" {
		return fmt.Errorf("unknown config key: %s", path)
	}
	return fmt.Errorf("unknown config key: %s (did you mean %q?)", path, best)
}

func closestSchemaPath(path string) string {
	best := ""
	bestDist := -1
	for i := range Schema {
		d := levenshtein(path, Schema[i].Path)
		if bestDist == -1 || d < bestDist || (d == bestDist && Schema[i].Path < best) {
			bestDist = d
			best = Schema[i].Path
		}
	}
	return best
}

// levenshtein computes the classic edit distance between a and b (single
// insertion/deletion/substitution cost 1 each). Small, iterative,
// allocation-light — fine for the short dotted-path strings this package
// ever compares.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	cur := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := cur[j-1] + 1
			sub := prev[j-1] + cost
			m := del
			if ins < m {
				m = ins
			}
			if sub < m {
				m = sub
			}
			cur[j] = m
		}
		prev, cur = cur, prev
	}
	return prev[lb]
}
