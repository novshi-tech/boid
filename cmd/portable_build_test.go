package cmd

import (
	"go/ast"
	"go/build/constraint"
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
		if !excludedFromPortableBuild(src) {
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

// excludedFromPortableBuild reports whether src's //go:build constraint
// keeps the file out of the portable client build.
//
// The constraint is PARSED and evaluated, not prefix-matched. A literal
// `strings.HasPrefix(src, "//go:build linux\n")` was the first cut and it
// is exactly the kind of gate that reads as protection while quietly
// letting the real cases through: `//go:build !windows`,
// `//go:build linux && amd64`, and any file carrying a license or doc
// comment above its constraint all slip past a prefix test, and each one
// removes a command from the portable build just as effectively as the
// spelling the test happened to look for.
//
// "Portable" here is evaluated as GOOS=windows — the platform this split
// exists for. A file excluded there is excluded from the client build.
func excludedFromPortableBuild(src []byte) bool {
	line, ok := buildConstraintLine(src)
	if !ok {
		return false // no constraint: builds everywhere
	}
	expr, err := constraint.Parse(line)
	if err != nil {
		return false // not a constraint we understand; go/build would have rejected it
	}
	return !expr.Eval(func(tag string) bool {
		// Only the tags a GOOS=windows/amd64 build satisfies. Everything
		// else — linux, unix, darwin, custom tags — is false.
		switch tag {
		case "windows", "amd64", "gc":
			return true
		}
		return false
	})
}

// buildConstraintLine returns the file's //go:build line, searching the
// comment block that precedes the package clause rather than assuming the
// constraint is the very first line (go/build allows blank lines and other
// comments before it).
func buildConstraintLine(src []byte) (string, bool) {
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "package ") {
			return "", false
		}
		if constraint.IsGoBuild(trimmed) {
			return trimmed, true
		}
	}
	return "", false
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
