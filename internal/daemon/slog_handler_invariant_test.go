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
// rests on: no non-test code anywhere in cmd/ or internal/ may call
// slog.SetDefault or construct a custom Handler via slog.New. That
// invariant is documented in three separate doc comments today
// (LogConfig, ApplyLogLevel, and internal/dispatcher/
// container_backend_workspace_init.go's "workspace home init completed"
// log line) but, before this file, nothing MECHANICALLY enforced it — a
// future change anywhere in the tree that added a slog.SetDefault call
// would leave every existing test green while silently breaking two things
// at once:
//
//   - boid.log's line format (every grep-based runbook — daemon liveness
//     checks, etc. — depends on today's
//     "2009/11/10 23:00:00 INFO msg key=value" shape, produced only by
//     slog's package-private defaultHandler).
//   - log.level itself: once something calls slog.SetDefault, slog.
//     SetLogLoggerLevel's own meaning changes (per its doc comment) from
//     "the bridge's minimum emitted level" to "the level a log.Print
//     record enters slog at" — ApplyLogLevel's existing calls would keep
//     compiling and running, just silently stop doing what LogConfig's
//     doc comment says they do.
//
// TestNoNonTestSlogHandlerInstall walks every non-test .go file under
// cmd/ and internal/ (an AST parse, not a grep — see
// internal/sandbox/broker_op_escape_test.go for the sibling pattern this
// mirrors, Tier 1 #2 of docs/plans/quality-gates.md) looking for a
// slog.SetDefault(...) or slog.New(...) call. A file found calling either
// must be registered in slogHandlerInstallExemptions with a reason, or the
// test fails — turning "don't add a Handler" from a doc-comment request
// into a mechanical check.

// slogHandlerInstallExemptions lists every non-test source file (repo-root
// relative path, e.g. "internal/daemon/log_level.go") allowed to call
// slog.SetDefault or slog.New, and why. Empty today: landing an entry here
// means the invariant above no longer holds tree-wide, and both LogConfig's
// and ApplyLogLevel's doc comments (and this file's own) need to be
// revisited at the same time, not just this map.
var slogHandlerInstallExemptions = map[string]string{}

// TestNoNonTestSlogHandlerInstall is the forcing function described above.
func TestNoNonTestSlogHandlerInstall(t *testing.T) {
	repoRoot := repoRootFromTestFile(t)

	violations := make(map[string][]int) // repo-relative path -> line numbers
	for _, dir := range []string{"cmd", "internal"} {
		scanDirForSlogHandlerInstall(t, repoRoot, dir, violations)
	}

	for relPath, lines := range violations {
		if reason, ok := slogHandlerInstallExemptions[relPath]; ok {
			if reason == "" {
				t.Errorf("%s: has an empty slogHandlerInstallExemptions reason — document why it's exempt", relPath)
			}
			continue
		}
		t.Errorf("%s: calls slog.SetDefault/slog.New at line(s) %v — this breaks boid.log's documented line "+
			"format and silently changes what log.level's slog.SetLogLoggerLevel calls do (see "+
			"internal/config.LogConfig's doc comment). If this is deliberate, add a named, reasoned entry "+
			"to slogHandlerInstallExemptions (internal/daemon/slog_handler_invariant_test.go) instead of "+
			"silently letting it through.", relPath, lines)
	}

	// Stale-exemption check, mirroring
	// internal/sandbox/broker_op_escape_test.go's TestOpEscapeCoverage_ManifestComplete:
	// an exemption for a file that no longer calls slog.SetDefault/slog.New
	// (or was renamed/deleted) should be removed rather than left to rot.
	for relPath := range slogHandlerInstallExemptions {
		if _, ok := violations[relPath]; !ok {
			t.Errorf("slogHandlerInstallExemptions has a stale entry %q: that file no longer calls "+
				"slog.SetDefault/slog.New (or does not exist) — remove the exemption", relPath)
		}
	}
}

// scanDirForSlogHandlerInstall walks repoRoot/relDir (skipping *_test.go and
// non-.go files) and records, into violations, the repo-relative path and
// line number of every slog.SetDefault(...)/slog.New(...) call site found.
func scanDirForSlogHandlerInstall(t *testing.T, repoRoot, relDir string, violations map[string][]int) {
	t.Helper()
	root := filepath.Join(repoRoot, relDir)
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}

		slogLocalName, imported := fileImportsSlogAs(f)
		if !imported {
			return nil
		}

		relPath, rerr := filepath.Rel(repoRoot, path)
		if rerr != nil {
			return rerr
		}
		relPath = filepath.ToSlash(relPath)

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
			if !ok || pkgIdent.Name != slogLocalName {
				return true
			}
			if sel.Sel.Name != "SetDefault" && sel.Sel.Name != "New" {
				return true
			}
			line := fset.Position(call.Pos()).Line
			violations[relPath] = append(violations[relPath], line)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// fileImportsSlogAs reports the local identifier f's import of "log/slog"
// is bound to (accounting for a renaming import), and whether f imports it
// at all. Returns ("", false) when f does not import "log/slog".
//
// Does not special-case a dot import (`. "log/slog"`) — such a call would
// appear as a bare *ast.Ident, not the *ast.SelectorExpr this scan looks
// for, and would therefore evade detection. Not handled because no file in
// this tree uses that form today (verified: `grep -rn '\. "log/slog"'` is
// empty) — the same class of "not exhaustive, but nothing in the tree
// exploits the gap today" limitation
// internal/sandbox/broker_op_escape_test.go's own opConstantNames doc
// comment documents for its iota-form gap.
func fileImportsSlogAs(f *ast.File) (localName string, imported bool) {
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if path != "log/slog" {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name, true
		}
		return "slog", true
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
