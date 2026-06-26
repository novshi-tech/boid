package orchestrator

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// SecretFinding describes a YAML scalar value that looks like a raw secret
// (a high-entropy token, or text containing a literal marker such as
// "password=").
//
// The scanner is intentionally conservative. The goal is to catch obvious
// leaks of credentials written into kit.yaml / workspace.yaml during
// `boid kit init` / `boid workspace configure` generation, not to be a
// comprehensive DLP system. False positives on long opaque identifiers
// (digest strings, UUIDs without dashes, image references) are accepted as
// the cost of a simple implementation.
type SecretFinding struct {
	// Path is the source file path used for attribution.
	Path string
	// Line / Column are 1-based, from the YAML node position.
	Line   int
	Column int
	// Kind is "high-entropy" or "literal-marker".
	Kind string
	// Pattern names the matching marker (only set for literal-marker findings,
	// e.g. "password=").
	Pattern string
	// Snippet is a redacted fingerprint of the offending text suitable for
	// display in error messages: short values are summarised by length only,
	// longer values are reduced to "<first4>...<last4>" so the report never
	// echoes the raw value back to logs.
	Snippet string
}

// String returns a human-readable single-line description that never
// includes the raw value.
func (f SecretFinding) String() string {
	if f.Pattern != "" {
		return fmt.Sprintf("%s:%d:%d: %s [%s] %s", f.Path, f.Line, f.Column, f.Kind, f.Pattern, f.Snippet)
	}
	return fmt.Sprintf("%s:%d:%d: %s %s", f.Path, f.Line, f.Column, f.Kind, f.Snippet)
}

var (
	// highEntropyRE matches a run of 32 or more "token-like" characters.
	// The character class mirrors the plan: [A-Za-z0-9_-]{32,}.
	highEntropyRE = regexp.MustCompile(`[A-Za-z0-9_-]{32,}`)

	// literalMarkerREs lists case-insensitive substring patterns that
	// indicate a secret literal embedded in an argv-style value. Each
	// pattern requires at least one non-whitespace character after the
	// "=" so trailing-equals fragments (e.g. command flag examples) are
	// not flagged.
	literalMarkerREs = []struct {
		name string
		re   *regexp.Regexp
	}{
		{"password=", regexp.MustCompile(`(?i)password\s*=\s*\S`)},
		{"token=", regexp.MustCompile(`(?i)\btoken\s*=\s*\S`)},
		{"secret=", regexp.MustCompile(`(?i)\bsecret\s*=\s*\S`)},
		{"api_key=", regexp.MustCompile(`(?i)api[_-]?key\s*=\s*\S`)},
		{"apikey=", regexp.MustCompile(`(?i)\bapikey\s*=\s*\S`)},
	}

	// envVarRefRE matches a value that is *only* a shell-style ${VAR}
	// expansion. Such references are intentional and never embed a literal
	// secret value.
	envVarRefRE = regexp.MustCompile(`^\$\{[^}]+\}$`)
)

// ScanSecretsFile reads path as YAML and returns any suspicious-looking
// scalar values.
//
// Returns nil + nil if the file parses cleanly and contains no findings.
// Returns nil + non-nil for I/O failures or YAML parse errors so callers can
// distinguish "could not check" from "checked and clean".
func ScanSecretsFile(path string) ([]SecretFinding, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return ScanSecretsBytes(path, data)
}

// ScanSecretsBytes scans an in-memory YAML document.
//
// path is used only for finding attribution (Finding.Path). Pass the
// expected on-disk path when scanning generated content before persisting it
// to disk, or any descriptive label for tests.
func ScanSecretsBytes(path string, data []byte) ([]SecretFinding, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse yaml %s: %w", path, err)
	}
	var findings []SecretFinding
	walkScalarValues(&root, func(n *yaml.Node) {
		findings = append(findings, checkScalarValue(path, n)...)
	})
	return findings, nil
}

// walkScalarValues invokes fn for every scalar value node reachable from n,
// skipping mapping keys (a leak only matters when it appears in a value
// position).
func walkScalarValues(n *yaml.Node, fn func(*yaml.Node)) {
	if n == nil {
		return
	}
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			walkScalarValues(c, fn)
		}
	case yaml.MappingNode:
		// Content alternates key, value, key, value, ...
		for i := 0; i+1 < len(n.Content); i += 2 {
			walkScalarValues(n.Content[i+1], fn)
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			walkScalarValues(c, fn)
		}
	case yaml.ScalarNode:
		fn(n)
	case yaml.AliasNode:
		// Aliases are rare in our generated files; follow the link so the
		// referenced scalar is still inspected, but guard against cycles by
		// only descending if Alias is set.
		if n.Alias != nil {
			walkScalarValues(n.Alias, fn)
		}
	}
}

// checkScalarValue applies the whitelist and pattern matchers to a single
// scalar node.
func checkScalarValue(path string, n *yaml.Node) []SecretFinding {
	v := n.Value
	if isSecretWhitelisted(v) {
		return nil
	}
	var out []SecretFinding
	for _, m := range literalMarkerREs {
		if m.re.MatchString(v) {
			out = append(out, SecretFinding{
				Path:    path,
				Line:    n.Line,
				Column:  n.Column,
				Kind:    "literal-marker",
				Pattern: m.name,
				Snippet: redactSnippet(v),
			})
		}
	}
	if loc := highEntropyRE.FindStringIndex(v); loc != nil {
		out = append(out, SecretFinding{
			Path:    path,
			Line:    n.Line,
			Column:  n.Column,
			Kind:    "high-entropy",
			Snippet: redactSnippet(v[loc[0]:loc[1]]),
		})
	}
	return out
}

// isSecretWhitelisted returns true for values that the scanner intentionally
// skips: empty strings, `secret:<key>` references (intentional indirection),
// `${VAR}` env-var expansions, absolute paths, and content-digest prefixes
// (`sha256:` / `sha512:`).
func isSecretWhitelisted(v string) bool {
	if v == "" {
		return true
	}
	if strings.HasPrefix(v, "secret:") {
		return true
	}
	if envVarRefRE.MatchString(v) {
		return true
	}
	if strings.HasPrefix(v, "/") {
		return true
	}
	if strings.HasPrefix(v, "sha256:") || strings.HasPrefix(v, "sha512:") {
		return true
	}
	return false
}

// redactSnippet reduces s to a fingerprint safe for display. Short values
// are summarised by their byte length only; values 9+ bytes long are shown
// as "<first4>...<last4>".
func redactSnippet(s string) string {
	if len(s) <= 8 {
		return fmt.Sprintf("(<%d chars>)", len(s))
	}
	return s[:4] + "..." + s[len(s)-4:]
}
