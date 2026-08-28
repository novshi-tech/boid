package sandbox

import (
	"errors"
	"reflect"
	"testing"
)

// fakeGitRun records every invocation and lets a test script canned
// success/failure per exact argv, without shelling out to a real git
// binary — ResolveCloneBranchRef's whole contract is pure command-sequence
// logic, so a fake is enough to pin it precisely (which real-git-backed
// e2e coverage, internal/sandbox/runner/clone_e2e_test.go and internal/
// dispatcher/checkout_test.go, complements from the other side: "the real
// git commands this emits actually do the right thing").
type fakeGitRun struct {
	calls [][]string
	// fail maps a space-joined argv to an error to return for that call.
	fail map[string]error
}

func (f *fakeGitRun) run(args ...string) error {
	f.calls = append(f.calls, append([]string(nil), args...))
	if f.fail != nil {
		if err, ok := f.fail[joinArgs(args)]; ok {
			return err
		}
	}
	return nil
}

func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}

func TestResolveCloneBranchRef_BaseBranchAlreadyOnOrigin(t *testing.T) {
	f := &fakeGitRun{}
	created, err := ResolveCloneBranchRef(f.run, "main", "main", "")
	if err != nil {
		t.Fatalf("ResolveCloneBranchRef: %v", err)
	}
	if created {
		t.Fatal("created = true, want false (base branch already resolved on origin)")
	}
	want := [][]string{
		{"rev-parse", "--verify", "--quiet", "origin/main"},
		{"checkout", "-B", "main", "origin/main"},
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("git calls = %v, want %v", f.calls, want)
	}
}

// TestResolveCloneBranchRef_OriginPrefixStripped pins the round-2 codex
// review gap: a project.yaml `base_branch: origin/main` must resolve to a
// local branch named "main" (checkout -B main origin/main), never
// "origin/main" checked out from "origin/origin/main" — the naive
// PrepareJobCheckout bug this shared function replaces.
func TestResolveCloneBranchRef_OriginPrefixStripped(t *testing.T) {
	f := &fakeGitRun{}
	created, err := ResolveCloneBranchRef(f.run, "origin/main", "origin/main", "")
	if err != nil {
		t.Fatalf("ResolveCloneBranchRef: %v", err)
	}
	if created {
		t.Fatal("created = true, want false")
	}
	want := [][]string{
		{"rev-parse", "--verify", "--quiet", "origin/main"},
		{"checkout", "-B", "main", "origin/main"},
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("git calls = %v, want %v", f.calls, want)
	}
}

// TestResolveCloneBranchRef_MissingBaseBranchCreatedFromForkPoint pins the
// other round-2 codex review gap: base_branch: release/1.0 with
// fork_point: main, where release/1.0 does not exist on origin yet, must be
// created from the fork point rather than failing outright.
func TestResolveCloneBranchRef_MissingBaseBranchCreatedFromForkPoint(t *testing.T) {
	f := &fakeGitRun{fail: map[string]error{
		"rev-parse --verify --quiet origin/release/1.0": errors.New("not found"),
	}}
	created, err := ResolveCloneBranchRef(f.run, "release/1.0", "release/1.0", "main")
	if err != nil {
		t.Fatalf("ResolveCloneBranchRef: %v", err)
	}
	if !created {
		t.Fatal("created = false, want true (base branch created from fork point)")
	}
	want := [][]string{
		{"rev-parse", "--verify", "--quiet", "origin/release/1.0"},
		{"rev-parse", "--verify", "--quiet", "main"},
		{"branch", "release/1.0", "main"},
		{"checkout", "-B", "release/1.0", "release/1.0"},
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("git calls = %v, want %v", f.calls, want)
	}
}

func TestResolveCloneBranchRef_MissingBaseBranchFallsBackToOriginHEAD(t *testing.T) {
	f := &fakeGitRun{fail: map[string]error{
		"rev-parse --verify --quiet origin/release/1.0": errors.New("not found"),
	}}
	created, err := ResolveCloneBranchRef(f.run, "release/1.0", "release/1.0", "")
	if err != nil {
		t.Fatalf("ResolveCloneBranchRef: %v", err)
	}
	if !created {
		t.Fatal("created = false, want true")
	}
	want := [][]string{
		{"rev-parse", "--verify", "--quiet", "origin/release/1.0"},
		{"rev-parse", "--verify", "--quiet", "refs/remotes/origin/HEAD"},
		{"branch", "release/1.0", "refs/remotes/origin/HEAD"},
		{"checkout", "-B", "release/1.0", "release/1.0"},
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("git calls = %v, want %v", f.calls, want)
	}
}

func TestResolveCloneBranchRef_ForkPointDoesNotResolveErrors(t *testing.T) {
	f := &fakeGitRun{fail: map[string]error{
		"rev-parse --verify --quiet origin/release/1.0": errors.New("not found"),
		"rev-parse --verify --quiet does-not-exist":     errors.New("not found"),
	}}
	_, err := ResolveCloneBranchRef(f.run, "release/1.0", "release/1.0", "does-not-exist")
	if err == nil {
		t.Fatal("expected an error when fork_point does not resolve")
	}
}

func TestResolveCloneBranchRef_NoForkPointAndNoOriginHEADErrors(t *testing.T) {
	f := &fakeGitRun{fail: map[string]error{
		"rev-parse --verify --quiet origin/release/1.0":       errors.New("not found"),
		"rev-parse --verify --quiet refs/remotes/origin/HEAD": errors.New("not found"),
	}}
	_, err := ResolveCloneBranchRef(f.run, "release/1.0", "release/1.0", "")
	if err == nil {
		t.Fatal("expected an error when neither fork_point nor origin/HEAD resolve")
	}
}

func TestResolveCloneBranchRef_CheckoutFailurePropagates(t *testing.T) {
	f := &fakeGitRun{fail: map[string]error{
		"checkout -B main origin/main": errors.New("checkout failed"),
	}}
	_, err := ResolveCloneBranchRef(f.run, "main", "main", "")
	if err == nil {
		t.Fatal("expected checkout failure to propagate")
	}
}

func TestResolveCloneBranchRef_EmptyBranchOrBaseBranchErrors(t *testing.T) {
	cases := []struct {
		name       string
		branch     string
		baseBranch string
	}{
		{"empty branch", "", "main"},
		{"empty base branch", "main", ""},
		{"both empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeGitRun{}
			if _, err := ResolveCloneBranchRef(f.run, tc.branch, tc.baseBranch, ""); err == nil {
				t.Fatal("expected an error for missing branch/base_branch")
			}
			if len(f.calls) != 0 {
				t.Fatalf("expected no git calls before validating required args, got %v", f.calls)
			}
		})
	}
}
