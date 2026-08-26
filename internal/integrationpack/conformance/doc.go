// Package conformance implements the Pack contract v1 conformance test
// framework — docs/plans/signal-ingest-detailed-design.md §7.2 ("Pack が
// 満たすべきもの", the Pack-author-facing half of the contract) and §7.3
// ("conformance test は boid リポジトリに置き、公式 Pack は CI で常時通す。
// custom Pack の作者も同じテストを手元で回せる"), docs/plans/signal-driven-
// review.md §14 Q21/Q22.
//
// ConformancePack is the single entry point: given the directory of ONE
// Pack version (a directory that itself contains integration.yaml —
// integrationpack.Pack.Dir's shape, NOT the multi-pack installation root
// integrationpack.LoadPacks walks; see ConformancePack's own doc comment
// for why this package deliberately does not call LoadPacks), it runs
// every machine-checkable §7.2 requirement as an independent t.Run
// subtest:
//
//   - manifest: integration.yaml parses via the existing
//     integrationpack.ParseManifest (PR-4) — no parsing logic of this
//     package's own
//   - skill (Q21): no skills/**/SKILL.md or skills/**/references/*.md file
//     mentions a boid CLI command or names a builtin skill
//   - connector: 終了 — every declared connectors[].executable exists and
//     is executable (hard requirement), and, best-effort and skippable,
//     actually launches without crashing or hanging when given the
//     contract's env (§5.2/§7.1) and a deliberately-unreachable
//     BOID_API_BASE
//   - 拡張禁止 (Q16-18 相当) — a light grep guard: no .go source files, no
//     literal reference to boid's own internal/ import path, anywhere in
//     the Pack directory
//
// A custom (non-official) Pack author runs the exact same checks locally,
// which is the whole point of putting this in a plain go test package
// rather than a shell script or a boid subcommand:
//
//	func TestConformance(t *testing.T) {
//	    conformance.ConformancePack(t, "/path/to/my-pack/1.0.0")
//	}
//
// officialpacks_test.go is the other half of Q22 ("公式 Pack は CI で常時
// 通す"): an env-var-gated sweep (BOID_API_SKILLS_DIR) that discovers every
// Pack directory in a checked-out boid-api-skills tree and runs
// ConformancePack against each. .github/workflows/blackbox-e2e.yml's
// pack-conformance job sets that env var to a sibling actions/checkout of
// novshi-tech/boid-api-skills and invokes it.
package conformance
