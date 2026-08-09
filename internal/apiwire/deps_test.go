package apiwire

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// allowedInternalImportPrefix is the ONLY boid package apiwire may import.
//
// orchestrator earns the exception because its types (Task, Action,
// WorkspaceMeta, BehaviorSpec, FiredEvent) appear directly inside these
// payloads, and it is already portable — it holds the state machine and the
// project.yaml model, no syscalls.
const allowedInternalImportPrefix = "github.com/novshi-tech/boid/internal/orchestrator"

// TestApiwireDependencies pins the whole point of this package (see doc.go):
// apiwire may import the standard library and internal/orchestrator, and
// nothing else.
//
// Without this, the split silently rots. Any import of a package that
// itself reaches internal/dispatcher, internal/db, internal/sandbox,
// internal/skills or web/templates would put the daemon back on the
// client's compile path and break the GOOS=windows/darwin CLI build again
// — and it would do so at the NEXT cross-compile, far from the commit that
// caused it. The failure belongs here, on the line that added the import.
//
// This is a source-level check, not a transitive one: the CI cross-build
// job is what proves the whole client path still compiles for a non-Linux
// GOOS. This test is the fast, local, points-at-the-culprit half.
func TestApiwireDependencies(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: unquote import %s: %v", name, imp.Path.Value, err)
			}
			// Standard library paths have no dot in their first segment.
			if first, _, _ := strings.Cut(path, "/"); !strings.Contains(first, ".") {
				continue
			}
			if path == allowedInternalImportPrefix || strings.HasPrefix(path, allowedInternalImportPrefix+"/") {
				continue
			}
			t.Errorf("%s imports %q: apiwire may only import the standard library and %s (see doc.go)",
				name, path, allowedInternalImportPrefix)
		}
	}

	if checked == 0 {
		t.Fatal("no non-test .go files found; the check would pass vacuously")
	}
}
