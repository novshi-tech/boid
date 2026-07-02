package sandbox_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// This file implements Tier 1 #2 of docs/plans/quality-gates.md: the builtin
// op ↔ escape-guard invariant gate. Every brokered op (BoidOp / GitOp) is a
// security-relevant surface — it must pass through the AllowedOps policy gate
// (broker.go handleBoidBuiltin) before any op-specific dispatch. The meta test
// below enumerates the op constants straight from protocol.go via the AST, so a
// newly added constant is discovered automatically, and requires each to be
// registered in opEscapeCoverage with either a named escape/enforcement test or
// an explicit exemption reason. Adding an op without an entry fails the test —
// the forcing function that turns "add an op → write an escape test (or justify
// skipping it)" into a mechanical check rather than a review-time judgement.
//
// The three *_PolicyReject tests below were added alongside the meta test to
// close the gaps it surfaced: task_get / task_notify / task_delete previously
// had no broker test at all (they appeared only in TestOpConstantsMirror).

func TestBroker_BoidTaskGet_PolicyReject(t *testing.T) {
	assertBoidOpRejectedByPolicy(t, &sandbox.BoidRequest{Op: sandbox.BoidOpTaskGet, TaskID: "t1"})
}

func TestBroker_BoidTaskNotify_PolicyReject(t *testing.T) {
	assertBoidOpRejectedByPolicy(t, &sandbox.BoidRequest{Op: sandbox.BoidOpTaskNotify, TaskID: "t1", Message: "hi"})
}

func TestBroker_BoidTaskDelete_PolicyReject(t *testing.T) {
	assertBoidOpRejectedByPolicy(t, &sandbox.BoidRequest{Op: sandbox.BoidOpTaskDelete, TaskID: "t1"})
}

// assertBoidOpRejectedByPolicy registers a boid policy that allows only an
// unrelated op (job_done), then asserts the given request is rejected by the
// policy gate — before any op-specific dispatch — and never reaches the
// executor. This exercises the single choke point (broker.go:296) that all
// boid ops share.
func assertBoidOpRejectedByPolicy(t *testing.T, req *sandbox.BoidRequest) {
	t.Helper()
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	policies := map[string]sandbox.BuiltinPolicy{
		"boid": {AllowedOps: map[string]struct{}{string(sandbox.BoidOpJobDone): {}}},
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, policies, sandbox.TokenContext{
		Role:       testRoleHook,
		ProjectDir: projectDir,
	})
	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    req,
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "not allowed") {
		t.Fatalf("op %q: expected policy rejection, got exit=%d stderr=%q", req.Op, resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("op %q: executor must not be called on policy rejection, got %d calls", req.Op, len(exec.calls))
	}
}

// opCoverage records how a single brokered op's guard is tested.
type opCoverage struct {
	// escapeTest names a test (in the internal/sandbox package) that drives
	// this op through the broker policy / enforcement gate. Empty when exempt.
	escapeTest string
	// exempt, when non-empty, records why this op needs no dedicated escape
	// test. Mutually exclusive with escapeTest.
	exempt string
}

// opEscapeCoverage maps every BoidOp / GitOp constant name to its guard test.
// Keyed by the Go constant identifier (e.g. "BoidOpJobDone"), which the meta
// test cross-checks against protocol.go. When you add an op, add a line here.
var opEscapeCoverage = map[string]opCoverage{
	// --- BoidOp ---
	"BoidOpJobDone":    {escapeTest: "TestBroker_BoidBuiltinRejectsWrongJobAndCwd"},
	"BoidOpJobList":    {escapeTest: "TestBroker_BoidJobList_PolicyReject"},
	"BoidOpJobShow":    {escapeTest: "TestBroker_BoidJobShow_PolicyReject"},
	"BoidOpJobLog":     {escapeTest: "TestBroker_BoidJobLog_PolicyReject"},
	"BoidOpActionSend": {escapeTest: "TestBroker_BoidActionSend_PolicyReject"},
	"BoidOpAgentStop":  {escapeTest: "TestBroker_BoidBuiltinAgentStopRejectsWrongJob"},
	"BoidOpTaskCreate": {escapeTest: "TestBroker_BoidBuiltinPolicy_HookRole"},
	"BoidOpTaskGet":    {escapeTest: "TestBroker_BoidTaskGet_PolicyReject"},
	"BoidOpTaskUpdate": {escapeTest: "TestBroker_BoidBuiltinPolicy_HookRoleRejectsTaskUpdate"},
	"BoidOpTaskImport": {escapeTest: "TestBroker_BoidTaskImport_HookRejected"},
	"BoidOpTaskReopen": {escapeTest: "TestBroker_BoidBuiltinPolicy_HookRoleRejectsReopen"},
	"BoidOpTaskList":   {escapeTest: "TestBroker_BoidTaskList_ProjectIDDenied"},
	"BoidOpTaskNotify": {escapeTest: "TestBroker_BoidTaskNotify_PolicyReject"},
	"BoidOpTaskAnswer": {escapeTest: "TestBroker_BoidTaskAnswer_PolicyReject"},
	"BoidOpTaskAsk":    {escapeTest: "TestBroker_TaskAsk_RejectsEmptyQuestion"},
	"BoidOpTaskDelete": {escapeTest: "TestBroker_BoidTaskDelete_PolicyReject"},

	// --- GitOp ---
	"GitOpFetch":      {escapeTest: "TestBroker_GitDirectExec_DeniedSubcommand"},
	"GitOpPush":       {escapeTest: "TestBroker_GitDirectExec_DeniedGlobalOptions"},
	"GitOpPushDelete": {escapeTest: "TestBroker_GitSubmodule_IsRejected"},
	"GitOpCloneLocal": {escapeTest: "TestValidateGitCloneLocal_SourceMustBePeer"},
}

// TestOpEscapeCoverage_ManifestComplete asserts opEscapeCoverage covers exactly
// the op constants declared in protocol.go — no missing entries (new op without
// a guard test) and no stale entries (removed op left in the manifest).
func TestOpEscapeCoverage_ManifestComplete(t *testing.T) {
	declared := opConstantNames(t)

	for name := range declared {
		cov, ok := opEscapeCoverage[name]
		if !ok {
			t.Errorf("op %q has no opEscapeCoverage entry: add an escape/enforcement test and register it here, or mark it exempt with a reason", name)
			continue
		}
		if cov.escapeTest == "" && cov.exempt == "" {
			t.Errorf("op %q: opEscapeCoverage entry must set either escapeTest or exempt", name)
		}
		if cov.escapeTest != "" && cov.exempt != "" {
			t.Errorf("op %q: opEscapeCoverage entry sets both escapeTest and exempt; pick one", name)
		}
	}
	for name := range opEscapeCoverage {
		if _, ok := declared[name]; !ok {
			t.Errorf("opEscapeCoverage has stale entry %q: no such BoidOp/GitOp constant in protocol.go", name)
		}
	}
}

// TestOpEscapeCoverage_NamedTestsExist guards against manifest rot: every named
// escapeTest must resolve to a real test function in the internal/sandbox
// package (typo / renamed / deleted tests are caught here rather than silently
// weakening coverage).
func TestOpEscapeCoverage_NamedTestsExist(t *testing.T) {
	funcs := packageTestFuncNames(t)
	for op, cov := range opEscapeCoverage {
		if cov.escapeTest == "" {
			continue
		}
		if !funcs[cov.escapeTest] {
			t.Errorf("op %q: escapeTest %q not found in internal/sandbox test files (renamed or deleted?)", op, cov.escapeTest)
		}
	}
}

// opConstantNames parses protocol.go and returns the set of const identifiers
// whose declared type is BoidOp or GitOp.
func opConstantNames(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "protocol.go", nil, 0)
	if err != nil {
		t.Fatalf("parse protocol.go: %v", err)
	}
	names := make(map[string]bool)
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			typeIdent, ok := vs.Type.(*ast.Ident)
			if !ok {
				continue
			}
			if typeIdent.Name != "BoidOp" && typeIdent.Name != "GitOp" {
				continue
			}
			for _, n := range vs.Names {
				names[n.Name] = true
			}
		}
	}
	if len(names) == 0 {
		t.Fatal("no BoidOp/GitOp constants found in protocol.go — parser assumption broke")
	}
	return names
}

// packageTestFuncNames parses every *_test.go in the current directory and
// returns the set of top-level Test* function names.
func packageTestFuncNames(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse test dir: %v", err)
	}
	funcs := make(map[string]bool)
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Recv != nil {
					continue
				}
				if strings.HasPrefix(fd.Name.Name, "Test") {
					funcs[fd.Name.Name] = true
				}
			}
		}
	}
	if len(funcs) == 0 {
		t.Fatal("no Test funcs discovered — parser assumption broke")
	}
	// Deterministic failure output aid (not strictly required).
	_ = sortedKeys(funcs)
	return funcs
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
