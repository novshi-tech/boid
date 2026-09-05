// Package packconformance implements the Pack contract v1 conformance test
// framework: a Pack author, official or custom, can run the same
// machine-checkable requirements against their Pack directory that CI runs
// against the official Packs.
//
// Deliberately at the MODULE ROOT (github.com/novshi-tech/boid/
// packconformance), not under internal/ — a package under internal/ is
// importable only from within github.com/novshi-tech/boid/... (Go's
// internal-package import rule), so a custom Pack author's own, completely
// separate Go module could never `import` it. testutil/ (this repo's
// existing test-helper package) sits at the module root for the identical
// reason.
//
// ConformancePack is the single entry point: given the directory of ONE
// Pack version (a directory that itself contains integration.yaml —
// integrationpack.Pack.Dir's shape, NOT the multi-pack installation root
// integrationpack.LoadPacks walks; see ConformancePack's own doc comment
// for why this package deliberately does not call LoadPacks), it runs
// every machine-checkable requirement as an independent t.Run subtest:
//
//   - manifest: integration.yaml parses via the existing
//     integrationpack.ParseManifest — no parsing logic of this package's own
//   - skill: no skills/**/SKILL.md or skills/**/references/*.md file
//     mentions a boid CLI command or names a builtin skill
//   - connector: every declared connectors[].executable exists and is
//     executable (hard requirement), and, best-effort and skippable,
//     actually launches without crashing or hanging when given the
//     contract's env and a deliberately-unreachable BOID_API_BASE
//   - no extension escape — a light grep guard: no .go source files, no
//     literal reference to boid's own internal/ import path, anywhere in
//     the Pack directory
//
// A custom (non-official) Pack author runs the exact same checks locally,
// from their OWN Go module (a plain `go get
// github.com/novshi-tech/boid@latest` away — this package is at the
// module root specifically so that works):
//
//	import "github.com/novshi-tech/boid/packconformance"
//
//	func TestConformance(t *testing.T) {
//	    packconformance.ConformancePack(t, "/path/to/my-pack/1.0.0")
//	}
//
// officialpacks_test.go is the CI-facing half: an env-var-gated sweep
// (BOID_API_SKILLS_DIR) that discovers every Pack directory in a checked-out
// boid-api-skills tree and runs ConformancePack against each.
package packconformance
