package skills_test

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/novshi-tech/boid/internal/skills"
)

func TestDeployAll_CreatesAllSkills(t *testing.T) {
	baseDir := t.TempDir()

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll: %v", err)
	}

	for _, skillName := range []string{"boid-web", "boid-orchestrate", "boid-task"} {
		content, err := os.ReadFile(filepath.Join(baseDir, skillName, "SKILL.md"))
		if err != nil {
			t.Fatalf("read %s/SKILL.md: %v", skillName, err)
		}
		if !strings.Contains(string(content), skillName) {
			t.Errorf("%s/SKILL.md missing skill name", skillName)
		}
	}

	for _, ref := range []string{"data-model.md", "builtins.md", "state-machine.md"} {
		path := filepath.Join(baseDir, "boid-task", "references", ref)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("boid-task reference file missing: %s", ref)
		}
	}
}

func TestDeployAll_Idempotent(t *testing.T) {
	baseDir := t.TempDir()

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (1st): %v", err)
	}
	content1, _ := os.ReadFile(filepath.Join(baseDir, "boid-task", "SKILL.md"))

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (2nd): %v", err)
	}
	content2, _ := os.ReadFile(filepath.Join(baseDir, "boid-task", "SKILL.md"))

	if string(content1) != string(content2) {
		t.Error("idempotent deploy changed SKILL.md content")
	}
}

func TestDeployAll_UpdatesChangedFiles(t *testing.T) {
	baseDir := t.TempDir()

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (1st): %v", err)
	}

	stale := filepath.Join(baseDir, "boid-task", "SKILL.md")
	if err := os.WriteFile(stale, []byte("old content"), 0o644); err != nil {
		t.Fatalf("write stale: %v", err)
	}

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (2nd): %v", err)
	}

	content, _ := os.ReadFile(stale)
	if string(content) == "old content" {
		t.Error("DeployAll did not update stale SKILL.md")
	}
	if !strings.Contains(string(content), "boid-task") {
		t.Error("updated SKILL.md missing expected content")
	}
}

// TestDeployAll_NoLeftoverTempFiles pins the atomic-write contract (docs/plans/
// home-workspace-volume.md Phase 4 PR3): deploySkill writes each file via a
// sibling temp file + rename rather than os.WriteFile directly, so a crash
// mid-write never leaves a partial file at the final path. This test can't
// simulate the crash itself (flaky by nature — see the plan's note), so it
// pins the observable half of the contract instead: after a normal (fresh)
// and a repeat (idempotent, some-files-changed) run, no stray temp file is
// left behind anywhere under baseDir.
func TestDeployAll_NoLeftoverTempFiles(t *testing.T) {
	baseDir := t.TempDir()

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (1st): %v", err)
	}
	assertNoTempFiles(t, baseDir)

	// Force a real rewrite (not a no-op) on the second pass so the atomic
	// write path actually executes again, not just the bytes.Equal skip.
	stale := filepath.Join(baseDir, "boid-task", "SKILL.md")
	if err := os.WriteFile(stale, []byte("stale content"), 0o644); err != nil {
		t.Fatalf("write stale: %v", err)
	}
	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (2nd, forces a rewrite): %v", err)
	}
	assertNoTempFiles(t, baseDir)
}

// TestDeployAll_NoOpWhenUnchanged asserts the "already up to date" fast path:
// a second DeployAll call over an already-current baseDir does not touch the
// file at all (mtime unchanged), matching the doc comment's "Files are only
// written when their content differs" contract without introducing a new
// version scheme.
func TestDeployAll_NoOpWhenUnchanged(t *testing.T) {
	baseDir := t.TempDir()

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (1st): %v", err)
	}
	target := filepath.Join(baseDir, "boid-web", "SKILL.md")
	before, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (2nd): %v", err)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat after 2nd deploy: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("mtime changed on a no-op re-deploy: before=%v after=%v", before.ModTime(), after.ModTime())
	}
}

// The tests below pin the symlink-attack defense added in response to a
// codex Blocker on PR #789 (docs/plans/home-workspace-volume.md Phase 4
// PR3). The threat model: a caller may hand DeployAll a baseDir that some
// less-trusted party can write to — as the Phase 4 caller literally did, by
// passing workspace HOME's `.claude/skills`, rw bind mounted into every
// sandbox dispatched against the workspace. Such a party can replace any path
// component under baseDir — including baseDir's own skill dirs and their
// subdirs — with a symlink to an arbitrary host path. DeployAll must refuse to
// follow such symlinks rather than silently writing through them as the
// daemon's own (real user permissions) process.
//
// PR3 of docs/plans/workspace-home-volume-persistence.md moved the production
// caller's baseDir to <RuntimesDir>/skills, which no job can reach, so today
// nothing exercises this defense in production. It is pinned anyway: the
// guarantee is DeployAll's, not its current caller's, and the same helpers
// still serve MkdirAllNoSymlink, whose caller does write into a job-owned tree
// (see safe_deploy.go's package comment).

// TestDeployAll_RejectsPreexistingSymlinkSkillDir pins the case where a
// skill directory itself (e.g. baseDir/boid-task) was replaced with a
// symlink to an attacker-chosen directory before DeployAll ever runs (e.g.
// the very first dispatch against a workspace home whose .claude/skills
// tree was tampered with out of band). DeployAll must error out instead of
// creating files inside the symlink target.
func TestDeployAll_RejectsPreexistingSymlinkSkillDir(t *testing.T) {
	baseDir := t.TempDir()
	attackTarget := t.TempDir()

	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		t.Fatalf("mkdir baseDir: %v", err)
	}
	if err := os.Symlink(attackTarget, filepath.Join(baseDir, "boid-task")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := skills.DeployAll(baseDir); err == nil {
		t.Fatal("DeployAll: expected error for symlinked skill dir, got nil")
	}

	entries, err := os.ReadDir(attackTarget)
	if err != nil {
		t.Fatalf("read attack target: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("attack target received unexpected entries: %v", entries)
	}
}

// TestDeployAll_RejectsSymlinkSwapAfterInitialDeploy pins the case where a
// skill subdirectory (baseDir/boid-task/references) that DeployAll itself
// created on a prior run is swapped for a symlink before the next dispatch's
// DeployAll call. This is the concrete attack the review described: a
// sandboxed job replaces a nested skill dir with a symlink, hoping the next
// daemon-side sync follows it.
func TestDeployAll_RejectsSymlinkSwapAfterInitialDeploy(t *testing.T) {
	baseDir := t.TempDir()
	attackTarget := t.TempDir()

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (seed): %v", err)
	}

	refsDir := filepath.Join(baseDir, "boid-task", "references")
	if err := os.RemoveAll(refsDir); err != nil {
		t.Fatalf("remove references dir: %v", err)
	}
	if err := os.Symlink(attackTarget, refsDir); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := skills.DeployAll(baseDir); err == nil {
		t.Fatal("DeployAll: expected error for symlinked references dir, got nil")
	}

	entries, err := os.ReadDir(attackTarget)
	if err != nil {
		t.Fatalf("read attack target: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("attack target received unexpected entries: %v", entries)
	}
}

// TestDeployAll_RejectsSymlinkBaseDirItself pins the case flagged in the
// review as the flock-based fallback's blind spot: baseDir itself
// (`.claude/skills`) is a symlink, not just something beneath it.
func TestDeployAll_RejectsSymlinkBaseDirItself(t *testing.T) {
	parent := t.TempDir()
	attackTarget := t.TempDir()
	baseDir := filepath.Join(parent, "skills")

	if err := os.Symlink(attackTarget, baseDir); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := skills.DeployAll(baseDir); err == nil {
		t.Fatal("DeployAll: expected error for symlinked baseDir, got nil")
	}

	entries, err := os.ReadDir(attackTarget)
	if err != nil {
		t.Fatalf("read attack target: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("attack target received unexpected entries: %v", entries)
	}
}

// TestDeployAll_ErrorsDoNotNameACallerSpecificLocation pins that DeployAll's
// own error text stays caller-neutral. internal/skills is a general-purpose
// package: baseDir is whatever the caller passed, and the caller that made
// this matter (internal/dispatcher, materializing the set under
// <RuntimesDir>/skills for the per-skill bind mounts) has since been retired
// along with those mounts — leaving DeployHostSkills, whose baseDir is
// ~/.claude/skills. Neither is a workspace HOME. An error that says
// "workspace HOME %q" therefore sends
// whoever reads a failed job's Output to a directory that has nothing to do
// with the failure — and would keep doing so for any future caller too. The
// path itself is already in the message (%q), which is the only location
// information this package can state truthfully.
//
// Both error-producing branches of DeployAll are exercised: the
// openBaseDirSafe failure (baseDir itself unusable) and the per-skill
// deploySkill failure (something under baseDir unusable).
func TestDeployAll_ErrorsDoNotNameACallerSpecificLocation(t *testing.T) {
	// Any phrase that asserts WHERE baseDir is rather than merely quoting it.
	forbidden := []string{"workspace HOME", "workspace home", "$HOME", "~/.claude"}

	t.Run("baseDir itself unusable", func(t *testing.T) {
		parent := t.TempDir()
		baseDir := filepath.Join(parent, "skills")
		if err := os.Symlink(t.TempDir(), baseDir); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		err := skills.DeployAll(baseDir)
		if err == nil {
			t.Fatal("DeployAll: expected an error for a symlinked baseDir, got nil")
		}
		assertCallerNeutral(t, err, baseDir, forbidden)
	})

	t.Run("a skill dir under baseDir unusable", func(t *testing.T) {
		baseDir := t.TempDir()
		if err := os.Symlink(t.TempDir(), filepath.Join(baseDir, "boid-task")); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		err := skills.DeployAll(baseDir)
		if err == nil {
			t.Fatal("DeployAll: expected an error for a symlinked skill dir, got nil")
		}
		assertCallerNeutral(t, err, baseDir, forbidden)
	})
}

// assertCallerNeutral fails t when err's text claims a specific location for
// baseDir instead of just quoting it.
func assertCallerNeutral(t *testing.T, err error, baseDir string, forbidden []string) {
	t.Helper()
	msg := err.Error()
	for _, phrase := range forbidden {
		if strings.Contains(msg, phrase) {
			t.Errorf("DeployAll error names a caller-specific location %q, but internal/skills cannot know where baseDir is; got: %s", phrase, msg)
		}
	}
	if !strings.Contains(msg, baseDir) {
		t.Errorf("DeployAll error does not quote baseDir %q, so it names no location at all; got: %s", baseDir, msg)
	}
}

// TestDeployAll_ConcurrentSymlinkSwapNeverEscapes races a goroutine that
// repeatedly swaps a skill dir between a real directory and a symlink to an
// attack target against repeated DeployAll calls, pinning that the
// per-syscall (openat2) symlink check — rather than a separate
// Lstat/EvalSymlinks pre-check — closes the TOCTOU window a concurrent job
// could otherwise exploit. Some DeployAll calls are expected to error out
// (whenever the swap goroutine has the dir in its symlink phase); what must
// never happen is any write landing inside attackTarget.
func TestDeployAll_ConcurrentSymlinkSwapNeverEscapes(t *testing.T) {
	baseDir := t.TempDir()
	attackTarget := t.TempDir()

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (seed): %v", err)
	}

	skillPath := filepath.Join(baseDir, "boid-task")
	stop := make(chan struct{})
	var swapWG sync.WaitGroup
	swapWG.Add(1)
	go func() {
		defer swapWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.RemoveAll(skillPath)
			_ = os.Symlink(attackTarget, skillPath)
			_ = os.Remove(skillPath) // removes the symlink itself, not attackTarget's contents
			_ = os.MkdirAll(skillPath, 0o755)
		}
	}()

	for i := 0; i < 100; i++ {
		_ = skills.DeployAll(baseDir) // errors expected while the dir is mid-swap; ignored here
	}
	close(stop)
	swapWG.Wait()

	entries, err := os.ReadDir(attackTarget)
	if err != nil {
		t.Fatalf("read attack target: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("attack target received %d unexpected entries after concurrent race: %v", len(entries), entries)
	}
}

// TestDeployAll_NormalRegressionAfterSafePathRewrite is a belt-and-suspenders
// regression check that the openat2-based rewrite still deploys a completely
// ordinary (never-tampered-with) tree correctly, matching
// TestDeployAll_CreatesAllSkills but run after the race test's baseDir
// pattern (fresh dir, no pre-existing state) to catch any accidental
// coupling introduced by the safe-path helpers.
func TestDeployAll_NormalRegressionAfterSafePathRewrite(t *testing.T) {
	baseDir := t.TempDir()

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll: %v", err)
	}
	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (2nd, no-op pass): %v", err)
	}

	for _, skillName := range []string{"boid-web", "boid-orchestrate", "boid-task"} {
		content, err := os.ReadFile(filepath.Join(baseDir, skillName, "SKILL.md"))
		if err != nil {
			t.Fatalf("read %s/SKILL.md: %v", skillName, err)
		}
		if !strings.Contains(string(content), skillName) {
			t.Errorf("%s/SKILL.md missing skill name", skillName)
		}
	}
}

// TestDeployAll_CleansUpStaleTempFiles pins the crash-recovery half of the
// Should-fix #1 review comment: a temp file left behind by a killed-mid-write
// daemon (no defer runs on SIGKILL) must be swept up by the *next* DeployAll
// call, not accumulate forever.
func TestDeployAll_CleansUpStaleTempFiles(t *testing.T) {
	baseDir := t.TempDir()

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (seed): %v", err)
	}

	stale := filepath.Join(baseDir, "boid-web", ".SKILL.md.tmp-stale-12345")
	if err := os.WriteFile(stale, []byte("leftover from a killed daemon"), 0o644); err != nil {
		t.Fatalf("write stale temp file: %v", err)
	}

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (2nd, should reclaim stale temp): %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale temp file was not cleaned up (stat err=%v)", err)
	}
}

// TestDeployAll_PreservesTempFileOwnedByLiveProcess pins the PR4 E2E
// investigation fix (docs/plans/home-workspace-volume.md): a temp file whose
// name encodes a PID that is still running must NOT be reaped by a
// concurrent DeployAll call against the same baseDir — reaping it would
// race the true owner's own imminent renameat call out from under it,
// producing a "rename ... no such file or directory" failure
// (internal/dispatcher calls DeployAll once per dispatch with no
// cross-dispatch locking, so concurrent dispatches do call DeployAll on the
// same baseDir — originally the per-workspace HOME shared by every job of one
// workspace, and since PR3 of docs/plans/workspace-home-volume-persistence.md
// the single <RuntimesDir>/skills shared by every job of the whole
// installation, i.e. the overlap got wider). The test process's own PID
// (os.Getpid()) is guaranteed
// alive for the duration of the test, standing in for "another concurrently
// running DeployAll call".
func TestDeployAll_PreservesTempFileOwnedByLiveProcess(t *testing.T) {
	baseDir := t.TempDir()

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (seed): %v", err)
	}

	// Matches createUniqueTempFile's naming convention exactly: ".<name>.tmp-<pid>-<nanotime>-<attempt>".
	live := filepath.Join(baseDir, "boid-web", fmt.Sprintf(".SKILL.md.tmp-%d-123456789-0", os.Getpid()))
	if err := os.WriteFile(live, []byte("owned by a still-running process"), 0o644); err != nil {
		t.Fatalf("write live-owned temp file: %v", err)
	}

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (2nd, must not reap a live owner's temp file): %v", err)
	}

	if _, err := os.Stat(live); err != nil {
		t.Errorf("temp file owned by a live process (pid %d) was reaped: stat err=%v", os.Getpid(), err)
	}
}

// TestDeployAll_ReapsTempFileOwnedByDeadProcess complements the live-owner
// test above with a real (not synthetic) dead PID: a short-lived child
// process is spawned and waited on to completion, guaranteeing its PID no
// longer identifies a running process, then a temp file encoding that PID
// is planted and must still be reaped — the crash-recovery contract
// (TestDeployAll_CleansUpStaleTempFiles above) must keep working once a
// genuinely dead, numeric PID is involved, not just the pre-existing
// non-numeric "stale-12345" fixture.
func TestDeployAll_ReapsTempFileOwnedByDeadProcess(t *testing.T) {
	baseDir := t.TempDir()

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (seed): %v", err)
	}

	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run short-lived child process: %v", err)
	}
	deadPID := cmd.Process.Pid

	dead := filepath.Join(baseDir, "boid-web", fmt.Sprintf(".SKILL.md.tmp-%d-123456789-0", deadPID))
	if err := os.WriteFile(dead, []byte("owned by a now-dead process"), 0o644); err != nil {
		t.Fatalf("write dead-owned temp file: %v", err)
	}

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (2nd, should reclaim dead owner's temp file): %v", err)
	}

	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Errorf("temp file owned by a dead process (pid %d) was not reaped: stat err=%v", deadPID, err)
	}
}

// assertNoTempFiles walks dir and fails the test if any entry looks like a
// leftover atomic-write temp file (a dotfile containing "tmp" in its name).
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if !d.IsDir() && strings.HasPrefix(name, ".") && strings.Contains(name, "tmp") {
			t.Errorf("leftover temp file found: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
}

// --- MkdirAllNoSymlink (PR3 of docs/plans/workspace-home-volume-persistence.md,
// 論点 e-2) ---

// TestMkdirAllNoSymlink_CreatesMissingComponents pins the primitive PR3's
// dispatcher-side bind-target preparation relies on: every missing component
// of the requested path is created, and the call is idempotent.
func TestMkdirAllNoSymlink_CreatesMissingComponents(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, ".claude", "skills", "boid-task")

	if _, err := skills.MkdirAllNoSymlink(target); err != nil {
		t.Fatalf("MkdirAllNoSymlink: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat %s: %v", target, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", target)
	}
	if _, err := skills.MkdirAllNoSymlink(target); err != nil {
		t.Fatalf("MkdirAllNoSymlink (2nd, must be idempotent): %v", err)
	}
}

// TestMkdirAllNoSymlink_RefusesSymlinkComponent is the reason this is not a
// plain os.MkdirAll: the dispatcher creates these directories inside a
// workspace HOME that the job itself owns read-write, so a job that swaps
// ~/.claude for a symlink must not turn the daemon's next mkdir into a write
// outside the home.
func TestMkdirAllNoSymlink_RefusesSymlinkComponent(t *testing.T) {
	base := t.TempDir()
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(base, ".claude")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := skills.MkdirAllNoSymlink(filepath.Join(base, ".claude", "skills", "boid-task"))
	if err == nil {
		t.Fatal("MkdirAllNoSymlink through a symlinked component must fail")
	}
	if _, statErr := os.Stat(filepath.Join(elsewhere, "skills")); statErr == nil {
		t.Errorf("MkdirAllNoSymlink followed the symlink and created %s/skills", elsewhere)
	}
}

// TestMkdirAllNoSymlink_RejectsRelativePath pins the absolute-path
// precondition MkdirAllNoSymlink inherits from the openat2 walk (it starts at
// "/"), so a caller that forgot to resolve a root gets an error rather than a
// directory tree rooted at the daemon's cwd.
func TestMkdirAllNoSymlink_RejectsRelativePath(t *testing.T) {
	if _, err := skills.MkdirAllNoSymlink("relative/path"); err == nil {
		t.Fatal("MkdirAllNoSymlink on a relative path must fail")
	}
}

// TestMkdirAllNoSymlink_ReportsOwnerUID pins the owner uid MkdirAllNoSymlink
// returns alongside the directory it prepared. The caller needs it to answer
// one question the mkdir alone cannot: whether the directory now at that path
// is the one this process owns, or one somebody else created first.
//
// It has to come from this function rather than a follow-up os.Stat by the
// caller, and it has to be an fstat(2) on the very descriptor the symlink-safe
// walk ended on: a separate stat by path would re-resolve every component from
// scratch, so it could report the owner of a *different* directory than the
// one this call created or entered. That is the same check-then-use gap
// safe_deploy.go's whole openat2 design exists to remove — reintroducing it in
// the ownership check would make the check itself the race.
//
// os.Getuid() rather than a literal uid: the daemon's uid is environment
// dependent (a developer machine, a CI runner and the compose deploy's
// in-image `boid` user are all different), and the property being pinned is
// "the process that created it owns it", not "uid 1000 owns it".
// linuxPathMax is PATH_MAX from <linux/limits.h>: the hard ceiling on the
// length of a path STRING any single path-based syscall (stat(2), openat(2)
// with a multi-component name, ...) will accept, independent of how deep the
// directory tree itself may go. Exceeding it is ENAMETOOLONG.
//
// fd-relative resolution has no such ceiling, because each syscall only ever
// sees one component name — which is what
// TestMkdirAllNoSymlink_ReportsOwnerUIDWithoutReResolvingThePath below turns
// into an observable difference.
const linuxPathMax = 4096

// TestMkdirAllNoSymlink_ReportsOwnerUIDWithoutReResolvingThePath is the
// regression guard for HOW the owner uid is obtained, as opposed to what it
// comes out as (TestMkdirAllNoSymlink_ReportsOwnerUID above).
//
// MkdirAllNoSymlink must read the owner with fstat(2) on the descriptor its
// symlink-checked walk ended on, never with a stat by path: a stat by path
// re-resolves every component from scratch, so it can report the owner of a
// different directory than the one just created or entered — the exact
// check-then-use gap safe_deploy.go's openat2 design exists to remove, which
// would make the ownership check itself the race (see MkdirAllNoSymlink's own
// doc comment, and internal/dispatcher/skills_overlay.go for the caller that
// acts on the result).
//
// Every no-contention test — including the uid comparison directly above —
// passes either way, because with nothing racing the walk, both readings name
// the same directory. Producing a real divergence would need either a
// privileged chown or a won race, neither of which a test can arrange
// deterministically. What CAN be arranged deterministically is a path that
// only fd-relative resolution can reach at all: past PATH_MAX, every
// path-based syscall fails with ENAMETOOLONG while an openat2 walk — one short
// component name per syscall — is unaffected, and so is an fstat on the
// descriptor it produced. A regression to any stat-by-path therefore turns
// this call into an error instead of a uid.
//
// The premise is asserted rather than assumed: the case fails loudly if
// os.Stat on the very same path unexpectedly succeeds, since the whole test
// would then be proving nothing.
func TestMkdirAllNoSymlink_ReportsOwnerUIDWithoutReResolvingThePath(t *testing.T) {
	base := t.TempDir()

	// One 120-char component at a time until the whole path is comfortably
	// past PATH_MAX. Depth is irrelevant to what is being pinned — only the
	// length of the assembled string is.
	target := base
	for len(target) <= linuxPathMax+512 {
		target = filepath.Join(target, strings.Repeat("d", 120))
	}

	if _, err := os.Stat(target); err == nil {
		t.Fatalf("test premise broken: os.Stat on a %d-character path succeeded, so this case can no longer tell fstat(fd) apart from a stat by path", len(target))
	}

	uid, err := skills.MkdirAllNoSymlink(target)
	if err != nil {
		t.Fatalf("MkdirAllNoSymlink on a %d-character path: %v\n"+
			"An fd-relative walk plus an fstat on the descriptor it ends on has no PATH_MAX ceiling; "+
			"an ENAMETOOLONG here means some step re-resolved the whole path string instead.", len(target), err)
	}
	if uid != os.Getuid() {
		t.Errorf("owner uid = %d, want the calling process's uid %d", uid, os.Getuid())
	}
}

func TestMkdirAllNoSymlink_ReportsOwnerUID(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, ".claude", "skills", "boid-task")

	uid, err := skills.MkdirAllNoSymlink(target)
	if err != nil {
		t.Fatalf("MkdirAllNoSymlink: %v", err)
	}
	if uid != os.Getuid() {
		t.Errorf("owner uid of a freshly created dir = %d, want the calling process's uid %d", uid, os.Getuid())
	}

	// The already-exists path resolves through a different branch of
	// openOrCreateDirNoSymlink (the fast-path Openat2, not the mkdirat +
	// retry), so it needs its own assertion rather than being implied.
	uid, err = skills.MkdirAllNoSymlink(target)
	if err != nil {
		t.Fatalf("MkdirAllNoSymlink (2nd, already exists): %v", err)
	}
	if uid != os.Getuid() {
		t.Errorf("owner uid of an existing dir = %d, want the calling process's uid %d", uid, os.Getuid())
	}
}
