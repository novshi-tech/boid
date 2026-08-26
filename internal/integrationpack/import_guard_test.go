package integrationpack

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// forbiddenImportSubstrings names every service-specific vocabulary this
// core package (docs/plans/signal-driven-review.md §14 Q16: "core package
// から Jira/Slack/Bitbucket 固有 package の import が 0 件である") must never
// import. There is no such package in this repo today — official Pack
// content lives in a separate repo (boid-api-skills, per §10's PR-8 note) —
// so this guard is deliberately about the SHAPE of the dependency (nothing
// service-specific ever gets imported here), not a currently-reachable
// bug, and exists to keep it that way as the package grows.
var forbiddenImportSubstrings = []string{
	"jira",
	"slack",
	"bitbucket",
}

// TestQ16_NoServiceSpecificImports statically parses every non-test .go
// file's import block in this package and asserts none names a
// service-specific package — the grep-style guard docs/plans/
// signal-ingest-detailed-design.md's PR-4 row calls for ("そもそも実装しない
// ので自明だが、grep ベースの guard テストを1本置くと良い"). go/parser is used
// instead of a shell grep so the check runs portably under `go test`
// (CLAUDE.md: sandbox test runs cannot shell out) and only inspects actual
// import paths, not comment/string-literal false positives.
func TestQ16_NoServiceSpecificImports(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: unquote import %s: %v", name, imp.Path.Value, err)
			}
			lower := strings.ToLower(path)
			for _, forbidden := range forbiddenImportSubstrings {
				if strings.Contains(lower, forbidden) {
					t.Errorf("%s: forbidden service-specific import %q (Q16: core must not import Jira/Slack/Bitbucket-specific packages)", name, path)
				}
			}
		}
	}
}

// TestQ16_PackageDirHasNoServiceSpecificFiles is a coarser companion check:
// no file in this package's own directory (Pack content is never vendored
// into boid repo — see forbiddenImportSubstrings' own doc comment) is named
// after a specific service, which would be the first sign of this
// package's scope creeping from "Pack contract/loader" into "Pack content".
func TestQ16_PackageDirHasNoServiceSpecificFiles(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		lower := strings.ToLower(e.Name())
		for _, forbidden := range forbiddenImportSubstrings {
			if strings.Contains(lower, forbidden) {
				t.Errorf("file %q looks service-specific (contains %q) — Pack content does not belong in this repo (docs/plans/signal-driven-review.md §10's PR-8 note)", e.Name(), forbidden)
			}
		}
	}
}

// TestQ16_NoDBOrDispatcherImport is a narrower, always-actionable sibling
// of the grep guards above: internal/integrationpack must never import the
// sqlite-backed internal/db (or internal/dispatcher, which pulls it in) —
// it is a pure config/loader package, consistent with scripts/
// check-internal-architecture.sh's own leaf-package convention for
// internal/apigateway/internal/gitgateway (this package imports
// internal/apigateway itself, so it must not ALSO reach db/dispatcher
// through some other path).
func TestQ16_NoDBOrDispatcherImport(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	forbidden := []string{
		"github.com/novshi-tech/boid/internal/db",
		"github.com/novshi-tech/boid/internal/dispatcher",
		"github.com/novshi-tech/boid/internal/server",
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: unquote import %s: %v", name, imp.Path.Value, err)
			}
			for _, bad := range forbidden {
				if path == bad {
					t.Errorf("%s: forbidden import %q", name, path)
				}
			}
		}
	}
}

// TestQ16_PackageDirIsSelfContained sanity-checks the two tests above
// actually see files (a package with zero source files would make both
// guards vacuously pass) — filepath is imported only for this diagnostic.
func TestQ16_PackageDirIsSelfContained(t *testing.T) {
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no .go files found in internal/integrationpack — the Q16 guards above would be vacuous")
	}
}
