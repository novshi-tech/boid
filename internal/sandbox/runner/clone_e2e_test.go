//go:build e2e

// このファイルは実 git バイナリを直接 exec し、実リポジトリ相手に
// clone/checkout/branch 解決する end-to-end 試験。ホスト環境 (本物の git /
// サンドボックス外 / 書き込み可能な TempDir) を前提とするため、通常の
// go test ./... からは //go:build e2e タグで除外する。CI では
// go test -tags=e2e ./internal/sandbox/... で走らせる。
// 実 git を呼ばない純粋ロジック試験は clone_test.go を参照。
package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// runGitFixture runs `git <args...>` with cwd=dir, failing the test on
// error.
func runGitFixture(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (dir=%s): %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// newSourceRepo creates a small non-bare git repo at t.TempDir() with a
// "main" branch (one commit) that performClone can clone from directly
// (git clone accepts a plain local path — the gateway URL construction
// itself is dispatcher's concern, tested separately; this package only
// needs an opaque clone-able URL string). Identity is set as local (repo)
// config rather than via -c/--global so commits work regardless of the
// host's global git config.
func newSourceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGitFixture(t, dir, "init", "-q")
	runGitFixture(t, dir, "config", "user.name", "boid-test")
	runGitFixture(t, dir, "config", "user.email", "boid-test@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGitFixture(t, dir, "add", "README.md")
	runGitFixture(t, dir, "commit", "-q", "-m", "initial commit")
	// Ensure the branch is literally named "main" regardless of the host's
	// git version default.
	runGitFixture(t, dir, "branch", "-M", "main")
	return dir
}

// gitRevParse resolves ref inside dir, failing the test on error.
func gitRevParse(t *testing.T, dir, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", ref)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse %s (dir=%s): %v\n%s", ref, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// currentBranch returns the short branch name checked out in dir.
func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "symbolic-ref", "--short", "HEAD")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git symbolic-ref (dir=%s): %v\n%s", dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestPerformClone_RootTaskCheckoutOnly(t *testing.T) {
	src := newSourceRepo(t)
	target := filepath.Join(t.TempDir(), "workspace")
	st := OpenState(filepath.Join(t.TempDir(), "runner-state.json"))
	defer st.Close()

	err := performClone(sandbox.CloneSpec{
		Enabled:      true,
		URL:          src,
		TargetDir:    target,
		Branch:       "main",
		BaseBranch:   "main",
		CheckoutOnly: true,
	}, st)
	if err != nil {
		t.Fatalf("performClone: %v", err)
	}

	if got := currentBranch(t, target); got != "main" {
		t.Errorf("checked-out branch = %q, want main", got)
	}
	if got, want := gitRevParse(t, target, "HEAD"), gitRevParse(t, src, "main"); got != want {
		t.Errorf("HEAD = %s, want %s (source main tip)", got, want)
	}
}

func TestPerformClone_ChildTaskForksFromBaseBranch(t *testing.T) {
	src := newSourceRepo(t)
	target := filepath.Join(t.TempDir(), "workspace")

	err := performClone(sandbox.CloneSpec{
		Enabled:    true,
		URL:        src,
		TargetDir:  target,
		Branch:     "boid/abcd1234",
		BaseBranch: "main",
	}, nil)
	if err != nil {
		t.Fatalf("performClone: %v", err)
	}

	if got := currentBranch(t, target); got != "boid/abcd1234" {
		t.Errorf("checked-out branch = %q, want boid/abcd1234", got)
	}
	if got, want := gitRevParse(t, target, "HEAD"), gitRevParse(t, src, "main"); got != want {
		t.Errorf("HEAD = %s, want %s (forked from base branch tip)", got, want)
	}
}

func TestPerformClone_ChildTaskForksFromExplicitForkPoint(t *testing.T) {
	src := newSourceRepo(t)
	// Create a second branch ahead of main so ForkPoint resolution is
	// observably distinct from just forking off BaseBranch.
	runGitFixture(t, src, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(src, "feature.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write feature.txt: %v", err)
	}
	runGitFixture(t, src, "add", "feature.txt")
	runGitFixture(t, src, "commit", "-q", "-m", "feature commit")
	runGitFixture(t, src, "checkout", "-q", "main")

	target := filepath.Join(t.TempDir(), "workspace")
	err := performClone(sandbox.CloneSpec{
		Enabled:    true,
		URL:        src,
		TargetDir:  target,
		Branch:     "boid/child1234",
		BaseBranch: "main",
		ForkPoint:  "feature", // remote-backed fork point (resolves via origin/feature)
	}, nil)
	if err != nil {
		t.Fatalf("performClone: %v", err)
	}

	if got := currentBranch(t, target); got != "boid/child1234" {
		t.Errorf("checked-out branch = %q, want boid/child1234", got)
	}
	if got, want := gitRevParse(t, target, "HEAD"), gitRevParse(t, src, "feature"); got != want {
		t.Errorf("HEAD = %s, want %s (forked from explicit fork point)", got, want)
	}
}

func TestPerformClone_ForkPointNotFoundReturnsError(t *testing.T) {
	src := newSourceRepo(t)
	target := filepath.Join(t.TempDir(), "workspace")

	err := performClone(sandbox.CloneSpec{
		Enabled:    true,
		URL:        src,
		TargetDir:  target,
		Branch:     "boid/child1234",
		BaseBranch: "main",
		ForkPoint:  "boid/parent-never-pushed", // worktree-local branch, never exists in a fresh clone
	}, nil)
	if err == nil {
		t.Fatal("expected error when ForkPoint does not resolve in the clone")
	}
}

func TestPerformClone_BaseBranchMissingCreatedFromExplicitForkPoint(t *testing.T) {
	src := newSourceRepo(t)
	target := filepath.Join(t.TempDir(), "workspace")

	err := performClone(sandbox.CloneSpec{
		Enabled:             true,
		URL:                 src,
		TargetDir:           target,
		Branch:              "release/1.0",
		BaseBranch:          "release/1.0", // does not exist yet, anywhere
		CheckoutOnly:        true,
		BaseBranchForkPoint: "main",
	}, nil)
	if err != nil {
		t.Fatalf("performClone: %v", err)
	}
	if got := currentBranch(t, target); got != "release/1.0" {
		t.Errorf("checked-out branch = %q, want release/1.0", got)
	}
	if got, want := gitRevParse(t, target, "HEAD"), gitRevParse(t, src, "main"); got != want {
		t.Errorf("HEAD = %s, want %s (base branch case-3 created from fork point)", got, want)
	}
}

func TestPerformClone_BaseBranchMissingFallsBackToOriginHEAD(t *testing.T) {
	src := newSourceRepo(t)
	target := filepath.Join(t.TempDir(), "workspace")

	err := performClone(sandbox.CloneSpec{
		Enabled:      true,
		URL:          src,
		TargetDir:    target,
		Branch:       "release/2.0",
		BaseBranch:   "release/2.0", // does not exist yet; BaseBranchForkPoint left empty
		CheckoutOnly: true,
	}, nil)
	if err != nil {
		t.Fatalf("performClone: %v", err)
	}
	if got, want := gitRevParse(t, target, "HEAD"), gitRevParse(t, src, "main"); got != want {
		t.Errorf("HEAD = %s, want %s (fell back to origin/HEAD == main)", got, want)
	}
}

func TestPerformClone_ReferenceDirUsedWhenPresent(t *testing.T) {
	src := newSourceRepo(t)
	target := filepath.Join(t.TempDir(), "workspace")
	// The reference repo's own .git dir stands in for the RO bind-mounted
	// host repo `.git` the dispatcher mounts in production.
	referenceDir := filepath.Join(src, ".git")

	err := performClone(sandbox.CloneSpec{
		Enabled:      true,
		URL:          src,
		TargetDir:    target,
		Branch:       "main",
		BaseBranch:   "main",
		CheckoutOnly: true,
		ReferenceDir: referenceDir,
	}, nil)
	if err != nil {
		t.Fatalf("performClone: %v", err)
	}
	alternates := filepath.Join(target, ".git", "objects", "info", "alternates")
	data, statErr := os.ReadFile(alternates)
	if statErr != nil {
		t.Fatalf("expected objects/info/alternates from --reference, read error: %v", statErr)
	}
	if !strings.Contains(string(data), referenceDir) {
		t.Errorf("alternates = %q, want it to reference %q", data, referenceDir)
	}
}

func TestPerformClone_ReferenceDirGracefullyIgnoredWhenMissing(t *testing.T) {
	src := newSourceRepo(t)
	target := filepath.Join(t.TempDir(), "workspace")

	err := performClone(sandbox.CloneSpec{
		Enabled:      true,
		URL:          src,
		TargetDir:    target,
		Branch:       "main",
		BaseBranch:   "main",
		CheckoutOnly: true,
		ReferenceDir: filepath.Join(t.TempDir(), "does-not-exist.git"),
	}, nil)
	if err != nil {
		t.Fatalf("performClone should degrade gracefully when ReferenceDir is missing: %v", err)
	}
	alternates := filepath.Join(target, ".git", "objects", "info", "alternates")
	if _, statErr := os.Stat(alternates); !os.IsNotExist(statErr) {
		t.Errorf("expected no alternates file when --reference was skipped, stat err = %v", statErr)
	}
}

// TestPerformClone_ReopenReClonesCleanly proves reopen == re-running the
// same sequence idempotently: a leftover file from a previous (simulated)
// invocation must not survive a second performClone against the same
// TargetDir (docs/plans/git-gateway-cutover.md: 「reopen = 同シーケンス再実行」).
func TestPerformClone_ReopenReClonesCleanly(t *testing.T) {
	src := newSourceRepo(t)
	target := filepath.Join(t.TempDir(), "workspace")

	cs := sandbox.CloneSpec{
		Enabled:      true,
		URL:          src,
		TargetDir:    target,
		Branch:       "main",
		BaseBranch:   "main",
		CheckoutOnly: true,
	}
	if err := performClone(cs, nil); err != nil {
		t.Fatalf("first performClone: %v", err)
	}
	marker := filepath.Join(target, "leftover-from-previous-job.txt")
	if err := os.WriteFile(marker, []byte("stale"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	if err := performClone(cs, nil); err != nil {
		t.Fatalf("second (reopen) performClone: %v", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Errorf("expected reopen to wipe the previous TargetDir, marker file still present (stat err = %v)", statErr)
	}
	if got := currentBranch(t, target); got != "main" {
		t.Errorf("checked-out branch after reopen = %q, want main", got)
	}
}

// TestPerformClone_TokenNotLeakedIntoStateOnFailure proves a clone failure's
// error message (which performClone's caller in runner_linux.go feeds to
// st.Fail) does not itself embed the gateway URL's job token in a way that
// would defeat runner-state.json's redaction. performClone itself never
// writes to State directly on the URL; the URL only ever reaches
// runner-state.json through buildSpecDump's already-redacted copy (see
// TestBuildSpecDump_CloneRedactsTokenAndCapturesDeclaration in
// state_test.go) — this test guards the complementary invariant that a raw
// clone failure (e.g. a deliberately unreachable URL) doesn't echo the
// token back in its error text either, since callers might plausibly log
// err.Error() directly.
func TestPerformClone_TokenNotLeakedIntoStateOnFailure(t *testing.T) {
	const token = "leaketymctokenface"
	target := filepath.Join(t.TempDir(), "workspace")
	err := performClone(sandbox.CloneSpec{
		Enabled:    true,
		URL:        "http://127.0.0.1:1/j/" + token + "/github.com/owner/repo.git",
		TargetDir:  target,
		Branch:     "main",
		BaseBranch: "main",
	}, nil)
	if err == nil {
		t.Fatal("expected clone against an unreachable URL to fail")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("clone failure error embeds the raw job token (git's own stderr echoes the URL): %v", err)
	}
}
