package config

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// This file backs `boid config apply -f`/`boid config edit`'s whole-document
// validation, and the same validation the daemon re-runs server-side on
// every POST /api/config (defense in depth — the CLI already runs this
// client-side first, per docs/plans/workspace-db-consolidation.md's
// existing `workspace edit`/`DecodeWorkspaceMetaStrict` precedent this
// package's tree-walk below mirrors).
//
// ValidateYAML performs three passes:
//  1. unknown-key rejection against Schema (schema.go) — a document-shaped
//     tree walk, so a typo'd/legacy field anywhere (not just at a
//     `boid config set` leaf) is caught with a path + suggestion.
//  2. structural decode via the EXISTING Config.UnmarshalYAML — reused
//     as-is, so every invariant config.Load already enforces at daemon
//     startup (duration parsing, sandbox.backend enum, gateway.forges
//     host/forge/secret_key completeness) is enforced here too, upfront,
//     rather than only discovered at the next daemon restart.
//  3. extra invariants UnmarshalYAML does not itself check today:
//     sandbox.allowed_domains entry syntax.

// ValidateYAML decodes and fully validates a candidate config.yaml document,
// returning the resolved *Config on success. data may be a full document (a
// `boid config apply -f`/`edit` body) or a partial one built by Set/Unset
// starting from an already-valid tree — either way, every key present must
// resolve against Schema, and the document must decode cleanly through
// Config.UnmarshalYAML.
func ValidateYAML(data []byte) (*Config, error) {
	var tree Tree
	if len(bytes.TrimSpace(data)) > 0 {
		if err := yaml.Unmarshal(data, &tree); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	if tree == nil {
		tree = Tree{}
	}
	if err := ValidateKnownKeys(tree); err != nil {
		return nil, err
	}

	cfg := DefaultConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	if err := validateAllowedDomains(cfg.Sandbox.AllowedDomains); err != nil {
		return nil, err
	}

	return cfg, nil
}

// schemaNode is one node of the trie ValidateKnownKeys walks a decoded tree
// against — built once from Schema (schema.go) at package init.
type schemaNode struct {
	children map[string]*schemaNode // key: concrete segment, or "*" for a wildcard (forge id)
	leaf     *FieldSpec             // non-nil at a node that is itself a settable leaf
}

var schemaTrieRoot = buildSchemaTrie()

func buildSchemaTrie() *schemaNode {
	root := &schemaNode{children: map[string]*schemaNode{}}
	for i := range Schema {
		node := root
		for _, seg := range segments(Schema[i].Path) {
			child, ok := node.children[seg]
			if !ok {
				child = &schemaNode{children: map[string]*schemaNode{}}
				node.children[seg] = child
			}
			node = child
		}
		node.leaf = &Schema[i]
	}
	return root
}

// ValidateKnownKeys walks tree against the schema trie, rejecting any key
// (at any depth) that has no corresponding schema entry. Every
// gateway.forges.<id> map entry's id is accepted as a wildcard match, but
// the fields inside it are still checked against {host, forge, secret_key}.
func ValidateKnownKeys(tree Tree) error {
	return walkKnownKeys(schemaTrieRoot, tree, nil)
}

func walkKnownKeys(node *schemaNode, value any, pathSoFar []string) error {
	m, isMap := value.(Tree)
	if !isMap {
		// A scalar/array sitting where we still have schema children to
		// descend into (i.e. this path is not itself a leaf) is a shape
		// error, not an "unknown key" one — report it distinctly.
		if node.leaf == nil && len(node.children) > 0 {
			return fmt.Errorf("%s: expected a mapping, got %s", dottedJoin(pathSoFar), yamlTypeName(value))
		}
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		child, ok := node.children[k]
		if !ok {
			if wc, hasWildcard := node.children["*"]; hasWildcard {
				child = wc
			} else {
				return unknownKeyErrorAt(pathSoFar, k, siblingNames(node))
			}
		}
		if err := walkKnownKeys(child, m[k], append(append([]string(nil), pathSoFar...), k)); err != nil {
			return err
		}
	}
	return nil
}

func siblingNames(node *schemaNode) []string {
	out := make([]string, 0, len(node.children))
	for k := range node.children {
		if k == "*" {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func dottedJoin(segs []string) string {
	return strings.Join(segs, ".")
}

func unknownKeyErrorAt(pathSoFar []string, key string, siblings []string) error {
	full := dottedJoin(append(append([]string(nil), pathSoFar...), key))
	if len(siblings) == 0 {
		return fmt.Errorf("unknown config key: %s", full)
	}
	best := ""
	bestDist := -1
	for _, s := range siblings {
		d := levenshtein(key, s)
		if bestDist == -1 || d < bestDist {
			bestDist = d
			best = s
		}
	}
	return fmt.Errorf("unknown config key: %s (did you mean %q?)", full, dottedJoin(append(append([]string(nil), pathSoFar...), best)))
}

func yamlTypeName(v any) string {
	switch v.(type) {
	case []any:
		return "a list"
	case nil:
		return "null"
	default:
		return "a scalar"
	}
}

// validateAllowedDomains checks the basic syntax of every
// sandbox.allowed_domains entry. This mirrors, without importing,
// internal/sandbox/proxy.go's isDomainAllowed matching convention: an
// entry either starts with "." (suffix match, e.g. ".freee.co.jp") or names
// a bare hostname to match exactly (e.g. "api.example.com") — no port, no
// scheme, no path, no whitespace, no wildcard other than the leading-dot
// suffix form. internal/sandbox is not imported here (it has no exported
// syntax validator of its own — only the runtime matcher — and pulling it
// in would cross internal/config into sandbox's dependency footprint for a
// pure string-format check) — see the PR body for why this is a
// reimplementation rather than a reuse.
func validateAllowedDomains(domains []string) error {
	for _, d := range domains {
		if err := ValidateDomainEntry(d); err != nil {
			return fmt.Errorf("sandbox.allowed_domains: %w", err)
		}
	}
	return nil
}

// ValidateDomainEntry validates a single sandbox.allowed_domains entry's
// syntax. Exported so `boid config set sandbox.allowed_domains ...`/`apply`/
// `edit` all share this one check (dotted.go's Set does not itself know
// about per-Kind semantic constraints beyond parsing, so validate.go's
// ValidateYAML pass is what actually enforces this — see its own doc
// comment).
func ValidateDomainEntry(entry string) error {
	if entry == "" {
		return fmt.Errorf("empty domain entry")
	}
	host := entry
	suffixMatch := strings.HasPrefix(entry, ".")
	if suffixMatch {
		host = entry[1:]
	}
	if host == "" {
		return fmt.Errorf("%q: a bare \".\" is not a valid suffix-match entry", entry)
	}
	if strings.ContainsAny(entry, " \t\r\n") {
		return fmt.Errorf("%q: must not contain whitespace", entry)
	}
	if strings.Contains(entry, "://") {
		return fmt.Errorf("%q: must be a bare hostname, not a URL", entry)
	}
	if strings.ContainsAny(entry, "/:@?#") {
		return fmt.Errorf("%q: must be a bare hostname (no path, port, or userinfo)", entry)
	}
	// RFC 1035 §3.1: a full hostname (excluding this package's own leading
	// "." suffix-match marker, which is not part of the DNS name itself)
	// must not exceed 253 characters (MINOR 3, codex review round 1: the
	// pre-fix validator had no length ceiling at all).
	if len(host) > 253 {
		return fmt.Errorf("%q: host exceeds the 253-character DNS name limit", entry)
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if label == "" {
			return fmt.Errorf("%q: empty label (stray \"..\" or trailing \".\")", entry)
		}
		// RFC 1035 §2.3.4: a single DNS label is capped at 63 characters
		// (MINOR 3).
		if len(label) > 63 {
			return fmt.Errorf("%q: label %q exceeds the 63-character DNS label limit", entry, label)
		}
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return fmt.Errorf("%q: label %q must not start or end with \"-\"", entry, label)
		}
		for _, r := range label {
			// RFC 1035 §2.3.1 restricts a DNS label to letters, digits,
			// and "-" ("LDH" labels) — no underscore. The pre-fix
			// validator allowed "_" too, accepting strings that are not
			// valid DNS hostnames at all (MINOR 3, codex review round 1).
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return fmt.Errorf("%q: label %q contains an invalid character %q (only letters, digits, and \"-\" are allowed)", entry, label, string(r))
			}
		}
	}
	return nil
}
