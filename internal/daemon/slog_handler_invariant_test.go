package daemon

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// This file is the forcing-function guard for the ONE process-wide
// invariant internal/config.LogConfig / ApplyLogLevel's entire contract
// rests on: no non-test code anywhere in this repository's production Go
// tree may call slog.SetDefault, construct a custom Handler via slog.New,
// or reach into the standard "log" package's own format/output knobs
// (log.SetFlags / log.SetPrefix / log.SetOutput) — any of these silently
// invalidates boid.log's documented line format and/or log.level itself.
// That invariant is documented in two doc comments today (LogConfig,
// ApplyLogLevel) but, before this file, nothing MECHANICALLY enforced it —
// a future change anywhere in the tree that added one of the calls above
// would leave every existing test green while silently breaking two things
// at once:
//
//   - boid.log's line format (every grep-based runbook — daemon liveness
//     checks, etc. — depends on today's
//     "2009/11/10 23:00:00 INFO msg key=value" shape, produced only by
//     slog's package-private defaultHandler delegating to the "log"
//     package's own date/time/flags/prefix machinery).
//   - log.level itself: once something calls slog.SetDefault, slog.
//     SetLogLoggerLevel's own meaning changes (per its doc comment) from
//     "the bridge's minimum emitted level" to "the level a log.Print
//     record enters slog at" — ApplyLogLevel's existing calls would keep
//     compiling and running, just silently stop doing what LogConfig's
//     doc comment says they do.
//
// TestNoNonTestLogFormatTampering walks every non-test .go file in this
// repository's production tree (an AST parse, not a grep — see
// internal/sandbox/broker_op_escape_test.go for the sibling pattern this
// mirrors, Tier 1 #2 of docs/plans/quality-gates.md) looking for any of
// the forbidden calls above. A file found calling one must be registered
// in logFormatTamperingExemptions with a reason, or the test fails —
// turning "don't touch boid.log's format/level machinery" from a
// doc-comment request into a mechanical check.
//
// Scan root: the whole repository, MINUS a short, explicit exclude list
// (excludedScanDirs below) — not an allow-list of "the directories that
// matter today". An allow-list needs updating every time production Go
// code gains a new top-level home (main.go itself was originally missing
// from an earlier allow-list version of this scan — the exact class of gap
// an allow-list invites), where an exclude-list only needs updating when a
// new test-only tree appears, which is rarer and more visible in review.

// excludedScanDirs are top-level (repository-root-relative) directories
// TestNoNonTestLogFormatTampering does not walk into — verified, at the
// time this list was written, to contain no code that ships as part of the
// daemon binary (i.e., not reachable from main.go's import graph):
//   - "e2e": e2e/cmd/boid-e2e is a separate CLI tool built only for the
//     E2E harness (e2e/run-container.sh); e2e/upstream is a git-upstream
//     test fixture. Neither is imported by main.go, cmd/, internal/, web/,
//     or build/container/.
//   - "testutil": a test-helper library imported exclusively by *_test.go
//     files repository-wide.
//   - dot-directories (".git", ".github", ".claude", ".boid", ".playwright"):
//     contain no Go source at all; excluded for walk performance (".git"
//     alone can be enormous) as much as correctness.
//
// Deliberately NOT excluded: "build" (build/container/assets.go IS
// imported by cmd/host.go — production code, a root an earlier version of
// this scan's "5 root" accounting missed too) and "scripts"
// (scripts/embed.go, likewise reachable from the daemon build).
var excludedScanDirs = map[string]bool{
	".git":        true,
	".github":     true,
	".claude":     true,
	".boid":       true,
	".playwright": true,
	"e2e":         true,
	"testutil":    true,
}

// logFormatTamperingExemptions lists every non-test source file
// (repo-root-relative path, e.g. "internal/daemon/log_level.go") allowed to
// call one of the forbidden functions this scan looks for, and why. Empty
// today: landing an entry here means the invariant above no longer holds
// tree-wide, and LogConfig's, ApplyLogLevel's, and this file's own doc
// comments all need revisiting at the same time, not just this map.
var logFormatTamperingExemptions = map[string]string{}

// forbiddenCall names one function this scan treats as forbidden non-test
// production code, identified by the import path it must come from (so an
// unrelated package's same-named method, e.g. some other SetOutput, is
// never mistaken for this one) and the local identifier that import path's
// package is bound to in the file under inspection (accounting for a
// renaming import).
type forbiddenCall struct {
	importPath string // e.g. "log/slog"
	selector   string // e.g. "SetDefault"
	reason     string // why this call is forbidden, for the failure message
}

// forbiddenCalls is every (import, selector) pair TestNoNonTestLogFormatTampering
// rejects in non-test production code:
//   - log/slog SetDefault / New: installs a custom Handler — the whole
//     "boid.log's format comes only from slog's package-private
//     defaultHandler" precondition LogConfig's doc comment states.
//   - log SetFlags / SetPrefix / SetOutput: none of these touch slog at
//     all, but slog's defaultHandler delegates timestamp/prefix rendering
//     AND the actual io.Writer to the exact same standard-library "log"
//     package's default Logger — so any of these three silently changes
//     boid.log's line format (SetFlags/SetPrefix) or destination
//     (SetOutput) without slog ever being involved. Round-3 PR #858 review
//     mutation proof: log.SetFlags(0) alone drops boid.log's date/time
//     prefix entirely, passing every existing slog-focused test.
var forbiddenCalls = []forbiddenCall{
	{importPath: "log/slog", selector: "SetDefault", reason: "installs a custom slog default Logger/Handler"},
	{importPath: "log/slog", selector: "New", reason: "constructs a custom slog Handler"},
	{importPath: "log", selector: "SetFlags", reason: "changes the standard log package's date/time/flags — the same package slog's defaultHandler delegates formatting to"},
	{importPath: "log", selector: "SetPrefix", reason: "changes the standard log package's line prefix — the same package slog's defaultHandler delegates formatting to"},
	{importPath: "log", selector: "SetOutput", reason: "redirects the standard log package's writer — the same package slog's defaultHandler delegates output to"},
}

// TestNoNonTestLogFormatTampering is the forcing function described above.
func TestNoNonTestLogFormatTampering(t *testing.T) {
	repoRoot := repoRootFromTestFile(t)

	type hit struct {
		line   int
		reason string
	}
	violations := make(map[string][]hit) // repo-relative path -> hits

	fset := token.NewFileSet()
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != repoRoot && filepath.Dir(path) == repoRoot && excludedScanDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}

		relPath, rerr := filepath.Rel(repoRoot, path)
		if rerr != nil {
			return rerr
		}
		relPath = filepath.ToSlash(relPath)

		// localNames maps each forbidden call's import path to the local
		// identifier THIS file binds it to (only computed for import paths
		// this file actually imports).
		localNames := make(map[string]string)
		for _, fc := range forbiddenCalls {
			if _, done := localNames[fc.importPath]; done {
				continue
			}
			if name, ok := localImportName(f, fc.importPath); ok {
				localNames[fc.importPath] = name
			}
		}
		if len(localNames) == 0 {
			return nil
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkgIdent, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			for _, fc := range forbiddenCalls {
				localName, imported := localNames[fc.importPath]
				if !imported || pkgIdent.Name != localName || sel.Sel.Name != fc.selector {
					continue
				}
				line := fset.Position(call.Pos()).Line
				violations[relPath] = append(violations[relPath], hit{line: line, reason: fc.reason})
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", repoRoot, err)
	}

	for relPath, hits := range violations {
		if reason, ok := logFormatTamperingExemptions[relPath]; ok {
			if reason == "" {
				t.Errorf("%s: has an empty logFormatTamperingExemptions reason — document why it's exempt", relPath)
			}
			continue
		}
		for _, h := range hits {
			t.Errorf("%s:%d: %s — this breaks boid.log's documented line format and/or silently changes what "+
				"log.level's slog.SetLogLoggerLevel calls do (see internal/config.LogConfig's doc comment). If "+
				"this is deliberate, add a named, reasoned entry to logFormatTamperingExemptions "+
				"(internal/daemon/slog_handler_invariant_test.go) instead of silently letting it through.",
				relPath, h.line, h.reason)
		}
	}

	// Stale-exemption check, mirroring
	// internal/sandbox/broker_op_escape_test.go's TestOpEscapeCoverage_ManifestComplete:
	// an exemption for a file that no longer makes any forbidden call (or
	// was renamed/deleted) should be removed rather than left to rot.
	for relPath := range logFormatTamperingExemptions {
		if _, ok := violations[relPath]; !ok {
			t.Errorf("logFormatTamperingExemptions has a stale entry %q: that file no longer calls any forbidden "+
				"function (or does not exist) — remove the exemption", relPath)
		}
	}
}

// localImportName reports the local identifier f's import of importPath is
// bound to (accounting for a renaming import), and whether f imports it at
// all. Returns ("", false) when f does not import importPath.
//
// Does not special-case a dot import (`. "log/slog"` / `. "log"`) — such a
// call would appear as a bare *ast.Ident, not the *ast.SelectorExpr this
// scan looks for, and would therefore evade detection. Not handled because
// no file in this tree uses that form for either package today (verified:
// `grep -rn '\. "log/slog"\|\. "log"'` is empty) — the same class of "not
// exhaustive, but nothing in the tree exploits the gap today" limitation
// internal/sandbox/broker_op_escape_test.go's own opConstantNames doc
// comment documents for its iota-form gap.
func localImportName(f *ast.File, importPath string) (localName string, imported bool) {
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if path != importPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name, true
		}
		// Both "log" and "log/slog" bind to their final path segment
		// ("log" and "slog" respectively) when unaliased, which is also
		// simply importPath's own base name in both cases.
		return filepath.Base(importPath), true
	}
	return "", false
}

// repoRootFromTestFile returns the absolute path to the boid repo root by
// walking up from the location of this test file — mirrors internal/
// orchestrator/spec_loader_test.go's repoRootFromTestFile (this test file
// lives at internal/daemon/slog_handler_invariant_test.go, also two
// directories below the repo root).
func repoRootFromTestFile(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller(0) failed; cannot locate test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
}
