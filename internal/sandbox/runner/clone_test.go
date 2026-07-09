package runner

import (
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// These are logic-only unit tests that never exec a real git binary — both
// return before performCloneSteps reaches its first runGit call. The
// end-to-end sequence (real git clone/checkout against a real fixture repo)
// is covered by clone_e2e_test.go (//go:build e2e), mirroring the existing
// internal/sandbox split between git_builtin_logic_test.go (pure logic) and
// git_builtin_test.go (e2e, real git exec) — real git subprocess calls
// assume a host environment (writable TempDir, unrestricted git), which the
// default `go test ./...` run does not guarantee (docs/plans/git-gateway-cutover.md
// PR5: this package's own dev loop can run inside a boid sandbox, whose git
// is itself broker-dispatched and policy-restricted).

func TestPerformClone_DisabledIsNoOp(t *testing.T) {
	target := filepath.Join(t.TempDir(), "workspace")
	err := performClone(sandbox.CloneSpec{Enabled: false, TargetDir: target}, nil)
	if err != nil {
		t.Fatalf("performClone with Enabled=false: %v", err)
	}
}

func TestPerformClone_MissingRequiredFieldsErrors(t *testing.T) {
	cases := []sandbox.CloneSpec{
		{Enabled: true, TargetDir: "/x", Branch: "main", BaseBranch: "main"},   // URL missing
		{Enabled: true, URL: "file:///x", Branch: "main", BaseBranch: "main"},  // TargetDir missing
		{Enabled: true, URL: "file:///x", TargetDir: "/x", BaseBranch: "main"}, // Branch missing
		{Enabled: true, URL: "file:///x", TargetDir: "/x", Branch: "main"},     // BaseBranch missing
	}
	for i, cs := range cases {
		if err := performClone(cs, nil); err == nil {
			t.Errorf("case %d: expected error for incomplete CloneSpec %+v", i, cs)
		}
	}
}
