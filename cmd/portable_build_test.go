package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// linuxOnlyRemoteCommandFiles is the allowlist of `//go:build linux` files
// in package cmd that are nevertheless allowed to declare a scope=remote
// command — i.e. a command the portable (GOOS=windows/darwin) client build
// deliberately does NOT ship.
//
// There is exactly one, and it needs a reason on the record:
//
//   - workspace_import_home.go — `boid workspace import-home` is scope=remote
//     because the import itself happens through the daemon's HTTP API, but
//     the CLI half walks a local directory and streams it as a tar using
//     internal/dispatcher's WorkspaceHomesDir / ResolveWorkspaceHomeSource /
//     WriteWorkspaceHomeTar helpers, which live in the Linux-only daemon
//     half. Making it portable means lifting those helpers out of
//     dispatcher, which is a bigger change than this split, and the command
//     is a one-off migration tool run on the daemon's own host anyway.
var linuxOnlyRemoteCommandFiles = map[string]string{
	"workspace_import_home.go": "CLI half tars a local dir via internal/dispatcher helpers",
}

// TestNoAccidentallyLinuxOnlyRemoteCommands pins the contract of the
// portable client build (docs/plans/windows-client-build.md): a
// scope=remote command is one that does nothing but talk to the daemon's
// HTTP API, so it MUST be available to a remote CLI on any GOOS — unless
// it is on the allowlist above, with a reason.
//
// Without this, the portable surface erodes silently. Adding
// `//go:build linux` to a cmd file is a one-line edit that nothing else
// flags, and the loss would only surface as "why is this command missing
// on my laptop" long after the commit that caused it. scope=local and
// scope=neutral commands are unaffected — daemon lifecycle machinery
// (start/stop/gc/reap) is exactly what SHOULD be Linux-only.
func TestNoAccidentallyLinuxOnlyRemoteCommands(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	fset := token.NewFileSet()
	var offenders []string
	sawLinuxOnly := false

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !strings.HasPrefix(string(src), "//go:build linux\n") {
			continue
		}
		sawLinuxOnly = true

		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		if !declaresRemoteScope(f) {
			continue
		}
		if _, allowed := linuxOnlyRemoteCommandFiles[name]; allowed {
			continue
		}
		offenders = append(offenders, name)
	}

	if !sawLinuxOnly {
		t.Fatal("no //go:build linux files found in package cmd; the check would pass vacuously")
	}

	sort.Strings(offenders)
	for _, name := range offenders {
		t.Errorf("%s is //go:build linux but declares a scope=remote command: "+
			"a remote command only talks to the daemon's HTTP API, so it should build for every GOOS. "+
			"Make it portable, or add it to linuxOnlyRemoteCommandFiles with a reason.", name)
	}
}

// declaresRemoteScope reports whether f contains a literal
// `scopeAnnotationKey: scopeRemote` pair — the way every command in this
// package declares its scope. AST-based rather than a text search so the
// many doc comments discussing scopeRemote do not read as declarations.
func declaresRemoteScope(f *ast.File) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "scopeAnnotationKey" {
			return true
		}
		if val, ok := kv.Value.(*ast.Ident); ok && val.Name == "scopeRemote" {
			found = true
			return false
		}
		return true
	})
	return found
}
