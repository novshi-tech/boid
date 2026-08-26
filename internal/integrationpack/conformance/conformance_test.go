package conformance

import (
	"os"
	"testing"
	"time"
)

// TestConformancePack_GoodPackPasses is the positive case, exercised
// through the actual public entry point: a Pack that satisfies every §7.2
// requirement passes ConformancePack outright, launch-smoke check
// included.
func TestConformancePack_GoodPackPasses(t *testing.T) {
	ConformancePack(t, "testdata/good-pack")
}

// TestConformancePack_SkipLaunchSkipsTheSmokeCheck is also exercised
// through the public entry point (SkipLaunch: true) — the SAME hanging-
// connector fixture used in TestEvaluateConnectorLaunch_HangingConnector
// below passes cleanly here, since nothing else about it is
// non-conforming.
func TestConformancePack_SkipLaunchSkipsTheSmokeCheck(t *testing.T) {
	ConformancePackOpts(t, "testdata/bad-connector-hangs-pack", Options{SkipLaunch: true})
}

// The remaining negative fixtures are asserted against the PURE detection
// functions directly (findSkillDocViolations / findConnectorExecutableViolations
// / evaluateConnectorLaunch / parseManifestFile), not through ConformancePack
// itself. This is deliberate, not a shortcut: testing.common.Fail
// unconditionally propagates a failing subtest's status to every ancestor
// test (see connector_check.go's findConnectorExecutableViolations doc
// comment), so there is no way to run a check that is SUPPOSED to fail
// against a negative fixture through the real t.Run/t.Errorf path without
// also failing this package's own `go test` run. The pure functions are
// exactly what ConformancePackOpts' thin *testing.T wrappers call, so
// asserting on them directly still exercises the real detection logic.

// TestParseManifestFile_BrokenManifestFails pins that a manifest
// ParseManifest itself rejects (here: an unsupported apiVersion) surfaces
// as an error — using the existing PR-4 parser unmodified.
func TestParseManifestFile_BrokenManifestFails(t *testing.T) {
	_, err := parseManifestFile("testdata/bad-manifest-pack")
	if err == nil {
		t.Fatal("expected an invalid apiVersion manifest to fail ParseManifest, got nil error")
	}
	t.Logf("got expected error: %v", err)
}

// TestFindSkillDocViolations_BoidCommandReferenceDetected is Q21: a skill
// telling the reader to run boid commands (and naming a builtin skill) is
// flagged.
func TestFindSkillDocViolations_BoidCommandReferenceDetected(t *testing.T) {
	violations, err := findSkillDocViolations("testdata/bad-skill-pack")
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) == 0 {
		t.Fatal("expected a skill mentioning boid commands to be flagged, got none")
	}
	t.Logf("found %d violation(s): %+v", len(violations), violations)
}

// TestFindSkillDocViolations_GoodPackHasNone is
// TestFindSkillDocViolations_BoidCommandReferenceDetected's positive
// counterpart at the same (pure-function) level.
func TestFindSkillDocViolations_GoodPackHasNone(t *testing.T) {
	violations, err := findSkillDocViolations("testdata/good-pack")
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Errorf("expected no violations in the conforming fixture, got %+v", violations)
	}
}

// TestFindConnectorExecutableViolations_MissingExecutableDetected pins the
// "declared executable does not exist on disk" case.
func TestFindConnectorExecutableViolations_MissingExecutableDetected(t *testing.T) {
	dir := "testdata/bad-connector-missing-pack"
	m, err := parseManifestFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	violations := findConnectorExecutableViolations(dir, m)
	if len(violations) == 0 {
		t.Fatal("expected a missing connector executable to be flagged, got none")
	}
	t.Logf("found: %+v", violations)
}

// TestFindConnectorExecutableViolations_NotExecutableDetected pins the
// "file exists but has no +x bit" case.
func TestFindConnectorExecutableViolations_NotExecutableDetected(t *testing.T) {
	dir := "testdata/bad-connector-not-executable-pack"
	m, err := parseManifestFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	violations := findConnectorExecutableViolations(dir, m)
	if len(violations) == 0 {
		t.Fatal("expected a non-executable connector file to be flagged, got none")
	}
	t.Logf("found: %+v", violations)
}

// TestEvaluateConnectorLaunch_HangingConnectorTimesOut pins the launch
// smoke check's timeout-kill path: a connector that never exits (ignoring
// the deliberately-unreachable BOID_API_BASE entirely) must be flagged as
// failed, not hang the whole conformance run forever. Uses a short timeout
// so this test itself stays fast. This is also the test that originally
// caught a real hang bug: killing only the direct child process left an
// orphaned "sleep 999" grandchild running (see connector_check.go's
// evaluateConnectorLaunch doc comment on cmd.Cancel/Setpgid).
func TestEvaluateConnectorLaunch_HangingConnectorTimesOut(t *testing.T) {
	dir := "testdata/bad-connector-hangs-pack"
	m, err := parseManifestFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Connectors) != 1 {
		t.Fatalf("fixture assumption broken: want exactly 1 connector, got %d", len(m.Connectors))
	}
	outcome := evaluateConnectorLaunch(dir, m.Connectors[0], 300*time.Millisecond)
	if !outcome.failed {
		t.Fatal("expected a hanging connector to fail the launch-smoke check")
	}
	t.Logf("got expected failure: %s", outcome.failReason)
}

// TestEvaluateConnectorLaunch_GoodPackExitsCleanly is the launch smoke
// check's positive case at the pure-function level: a connector that
// launches and exits promptly (any exit code) is not flagged.
func TestEvaluateConnectorLaunch_GoodPackExitsCleanly(t *testing.T) {
	dir := "testdata/good-pack"
	m, err := parseManifestFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Connectors) != 1 {
		t.Fatalf("fixture assumption broken: want exactly 1 connector, got %d", len(m.Connectors))
	}
	outcome := evaluateConnectorLaunch(dir, m.Connectors[0], DefaultLaunchTimeout)
	if outcome.failed {
		t.Errorf("expected the good-pack connector to launch cleanly, got failure: %s", outcome.failReason)
	}
	if outcome.skipped {
		t.Errorf("expected the good-pack connector to be found, got skip: %s", outcome.skipReason)
	}
}

// TestFindExtensionViolations_GoFileDetected pins the Q16-18 grep guard's
// ".go source file" half.
func TestFindExtensionViolations_GoFileDetected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir+"/sneaky.go", "package main\n")
	violations, err := findExtensionViolations(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) == 0 {
		t.Fatal("expected a .go file to be flagged, got none")
	}
}

// TestFindExtensionViolations_InternalImportReferenceDetected pins the
// grep guard's "references boid's internal/ import path" half.
func TestFindExtensionViolations_InternalImportReferenceDetected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir+"/notes.txt", "see github.com/novshi-tech/boid/internal/dispatcher for details\n")
	violations, err := findExtensionViolations(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) == 0 {
		t.Fatal("expected an internal/ import reference to be flagged, got none")
	}
}

// TestFindExtensionViolations_GoodPackHasNone is the guard's positive case.
func TestFindExtensionViolations_GoodPackHasNone(t *testing.T) {
	violations, err := findExtensionViolations("testdata/good-pack")
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Errorf("expected no violations in the conforming fixture, got %+v", violations)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
