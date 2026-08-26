package conformance

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/skills"
)

// boidCommandPattern matches an ASCII, lowercase "boid <subcommand>"
// invocation — e.g. "boid signal list", "boid workspace services add",
// "boid config get" — the shape every real boid CLI command takes.
//
// Calibrated against the actual reference-skill corpus in boid-api-skills
// (13 service skills, grepped while designing this check — see
// TestBoidCommandPattern, which pins the exact cases below verbatim):
// every real command reference found there matches, and nothing else in
// that corpus does. In particular, case-sensitivity and the required
// whitespace right after "boid" are load-bearing, not incidental:
//
//   - boid's own env-var contract (BOID_API_BASE, BOID_SIGNAL_SERVICE, …)
//     is ALL-CAPS, so it never matches a lowercase pattern regardless of
//     anything else — describing "authentication goes through the
//     $BOID_API_BASE env var" is explicitly allowed knowledge (§7.1: it's
//     part of what boid PROVIDES to a Pack), not a command, and this is
//     the mechanism that keeps the two from colliding
//   - Japanese prose in this corpus glues "boid" straight onto a particle
//     or another word with no space ("boidの…", "boid経由で…",
//     "boidサンドボックス…") — none of that matches either, since a space
//     is required
//   - a hyphen right after "boid" (as in the "boid-gateway" hostname
//     placeholder or the "boid-job" User-Agent literal, both real strings
//     in that corpus) is not whitespace either, so this pattern alone
//     leaves them alone — the separate builtin-skill-name check in
//     findBoidReferences below is scoped even more narrowly (an exact,
//     whole-word match against the real builtin skill list) for exactly
//     this reason, rather than a blanket "boid-[a-z-]+" pattern that would
//     flag them
//
// Known residual false-positive class, deliberately accepted rather than
// designed around: plain English prose that puts a verb right after "boid"
// with a space — "boid is/provides/does/has ..." — reads exactly like a
// subcommand and WILL match (e.g. "boid is a personal AI orchestrator").
// The real corpus this pattern was calibrated against is entirely
// Japanese, where "boid" is always either glued straight onto the next
// word/particle (no match, see above) or followed by an actual command
// (a real match) — this English-prose shape essentially never occurs in
// it. A tighter pattern could avoid this by matching only a curated list
// of real subcommand names, but that list would have to be hand-maintained
// against cmd/'s cobra tree, and this package cannot import cmd/ to derive
// it dynamically (cmd/ pulls in internal/db, which does not build inside a
// boid sandbox — see internal/integrationpack/conformance's own package
// doc comment on why a custom Pack author needs `go test` here to work
// from inside one). A Pack author who trips this on ordinary English prose
// should just avoid putting a bare verb directly after "boid " (say
// "boid's gateway" instead of "boid provides a gateway").
var boidCommandPattern = regexp.MustCompile(`\bboid[ \t]+[a-z][a-z0-9_-]*`)

// boidReference is one match findBoidReferences found in a skill document.
type boidReference struct {
	// kind is "command" (matched boidCommandPattern) or "builtin-skill"
	// (named one of skills.EmbeddedSkillNames()).
	kind string
	// match is the matched text (the command invocation, or the skill
	// name).
	match string
}

// findBoidReferences is the pure detection half of the Q21 "skill" check —
// separated from checkSkillsNoBoidCommands' t.Errorf reporting so it can be
// unit-tested directly (see skill_check_test.go) without synthesizing a
// *testing.T harness around it. builtinSkillNames is normally
// skills.EmbeddedSkillNames(), passed in rather than read here so tests can
// substitute a fixed list.
func findBoidReferences(content string, builtinSkillNames []string) []boidReference {
	var refs []boidReference
	for _, m := range boidCommandPattern.FindAllString(content, -1) {
		refs = append(refs, boidReference{kind: "command", match: m})
	}
	for _, name := range builtinSkillNames {
		if name == "" {
			continue
		}
		re := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
		if re.MatchString(content) {
			refs = append(refs, boidReference{kind: "builtin-skill", match: name})
		}
	}
	return refs
}

// isSkillDoc reports whether path (whose basename is base, located
// somewhere under skillsDir) is in scope for the boid-command-reference
// scan: exactly "SKILL.md" at any depth, or a "*.md" file that sits
// somewhere under a "references" directory.
func isSkillDoc(skillsDir, path, base string) bool {
	if base == "SKILL.md" {
		return true
	}
	if filepath.Ext(base) != ".md" {
		return false
	}
	rel, err := filepath.Rel(skillsDir, path)
	if err != nil {
		return false
	}
	for _, seg := range strings.Split(filepath.Dir(rel), string(filepath.Separator)) {
		if seg == "references" {
			return true
		}
	}
	return false
}

// skillDocViolation is one finding from findSkillDocViolations.
type skillDocViolation struct {
	// path is relative to the Pack directory (not the skills/ subdirectory)
	// — this is what a Pack author sees named in the failure.
	path string
	ref  boidReference
}

// findSkillDocViolations is the pure detection half of the Q21 "skill"
// check (docs/plans/signal-ingest-detailed-design.md §7.2's "skill" row,
// docs/plans/signal-driven-review.md §14): a Pack's skill content must
// describe only the external service (source-side knowledge), never
// boid's own commands — boid usage is the core's job (built-in skills,
// docs/plans/signal-driven-review.md §8.2), not a Pack's.
//
// Scans every file in scope per isSkillDoc, at any depth under
// <dir>/skills — independent of what the manifest happens to declare in
// skills[], since an undeclared stray file shipped in the same directory
// is just as visible to whatever eventually reads it. A Pack with no
// skills/ directory at all trivially returns no violations (nothing to
// scan).
//
// Separated from checkSkillsNoBoidCommands' *testing.T reporting so this
// package's own tests can assert directly on what was found (see
// conformance_test.go) — a *testing.T subtest failure always propagates to
// every ancestor test (testing.common.Fail calls c.parent.Fail()
// unconditionally), so there is no way to run a check that is SUPPOSED to
// fail against a negative fixture through the real t.Run/t.Errorf path
// without also failing this package's own test suite. Pure functions sidestep
// that entirely.
func findSkillDocViolations(dir string) ([]skillDocViolation, error) {
	skillsDir := filepath.Join(dir, "skills")
	if _, err := os.Stat(skillsDir); os.IsNotExist(err) {
		return nil, nil
	}

	builtinSkillNames := skills.EmbeddedSkillNames()
	var violations []skillDocViolation

	walkErr := filepath.WalkDir(skillsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !isSkillDoc(skillsDir, path, d.Name()) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			rel = path
		}
		for _, ref := range findBoidReferences(string(data), builtinSkillNames) {
			violations = append(violations, skillDocViolation{path: rel, ref: ref})
		}
		return nil
	})
	if walkErr != nil {
		return violations, walkErr
	}
	return violations, nil
}

// checkSkillsNoBoidCommands is findSkillDocViolations' *testing.T reporter
// — see that function's own doc comment for what it checks and why the
// detection logic lives separately.
func checkSkillsNoBoidCommands(t *testing.T, dir string) {
	t.Helper()
	violations, err := findSkillDocViolations(dir)
	if err != nil {
		t.Errorf("scan skills/: %v", err)
	}
	for _, v := range violations {
		switch v.ref.kind {
		case "command":
			t.Errorf("%s: mentions a boid command (%q) — a Pack skill must only describe the external service, never boid usage (docs/plans/signal-ingest-detailed-design.md §7.2 \"skill\")", v.path, v.ref.match)
		case "builtin-skill":
			t.Errorf("%s: mentions boid's builtin %q skill by name — a Pack skill must only describe the external service, never boid usage", v.path, v.ref.match)
		}
	}
}
