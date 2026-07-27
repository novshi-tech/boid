//go:build linux

package runner

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// These cases cover the workspace HOME skeleton check PR6 of
// docs/plans/workspace-home-volume-persistence.md moved into the job container
// (論点 b-2 / e-2). Through PR5 the daemon did it: it mkdir'd the per-skill bind
// targets inside the workspace home and fstat'd each result. The home is a
// docker named volume now, which the daemon can neither write to nor stat, so
// the check runs here — not in the init container that CREATES those
// directories, because 論点 e-2's decision is that a check inside the creator
// says nothing.

func TestVerifyHomeSkeleton_AllPresentAndOwned(t *testing.T) {
	home := t.TempDir()
	dirs := []string{".claude", filepath.Join(".claude", "skills")}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	if err := verifyHomeSkeleton(sandbox.Spec{HomeSkeletonRoot: home, HomeSkeletonDirs: dirs}); err != nil {
		t.Errorf("verifyHomeSkeleton on a correctly prepared home: %v", err)
	}
}

// TestVerifyHomeSkeleton_DirsWithoutARootFail pins the wiring bug the two
// fields make possible: entries are relative, so a spec that declares them
// without the root they are relative to would otherwise be resolved against
// this process's cwd — checking directories that have nothing to do with the
// workspace home and reporting them as fine.
func TestVerifyHomeSkeleton_DirsWithoutARootFail(t *testing.T) {
	err := verifyHomeSkeleton(sandbox.Spec{HomeSkeletonDirs: []string{".claude"}})
	if err == nil {
		t.Fatal("verifyHomeSkeleton accepted a skeleton with no root to resolve it against")
	}
	if !strings.Contains(err.Error(), "HomeSkeletonRoot") {
		t.Errorf("error does not name the missing field:\n%v", err)
	}
}

// TestVerifyHomeSkeleton_EmptyIsANoOp pins that a sandbox with no workspace
// home — a plain tmpfs $HOME, which is what dispatcher.homeMounts falls back to
// — is not failed by a check that has nothing to check.
func TestVerifyHomeSkeleton_EmptyIsANoOp(t *testing.T) {
	if err := verifyHomeSkeleton(sandbox.Spec{}); err != nil {
		t.Errorf("verifyHomeSkeleton with no skeleton declared: %v", err)
	}
}

// TestVerifyHomeSkeleton_MissingDirectoryFails covers the state that comes
// BEFORE the poisoning: a job of this workspace deleted ~/.claude and the
// engine has not yet re-created it as uid 0. Nothing downstream would notice —
// the harness would simply start against a $HOME with no ~/.claude and quietly
// fail to persist its credentials.
func TestVerifyHomeSkeleton_MissingDirectoryFails(t *testing.T) {
	home := t.TempDir()
	missing := filepath.Join(home, ".claude")

	err := verifyHomeSkeleton(sandbox.Spec{HomeSkeletonRoot: home, HomeSkeletonDirs: []string{".claude"}})
	if err == nil {
		t.Fatal("verifyHomeSkeleton accepted a missing skeleton directory")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error does not name the missing directory:\n%v", err)
	}
}

// TestVerifyHomeSkeleton_NotADirectoryFails covers a file left where a bind
// target belongs. The engine would fail the mount outright in production, but
// the check is cheaper and names the path.
func TestVerifyHomeSkeleton_NotADirectoryFails(t *testing.T) {
	home := t.TempDir()
	blocked := filepath.Join(home, ".claude")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}

	err := verifyHomeSkeleton(sandbox.Spec{HomeSkeletonRoot: home, HomeSkeletonDirs: []string{".claude"}})
	if err == nil {
		t.Fatal("verifyHomeSkeleton accepted a regular file where a bind target belongs")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error does not say what is wrong:\n%v", err)
	}
}

// TestVerifyHomeSkeleton_ForeignOwnerFailsWithRecoverySteps is the case the
// whole check exists for: an engine-created, uid-0-owned ~/.claude. It cannot
// be produced directly here — chown(2) to a foreign uid needs privileges no
// test process has — so the inequality is produced from the other side, by
// moving what the check believes its own uid to be. The stat is real, against a
// real directory this process really owns; only the comparison's other operand
// moves, which exercises exactly the branch the real condition takes.
//
// The error's content is asserted, not just its existence. Its entire value is
// that it turns a permanently, silently broken workspace HOME into one
// diagnosable failure, and an error that does not carry the way out is not that.
func TestVerifyHomeSkeleton_ForeignOwnerFailsWithRecoverySteps(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	real := runnerUID
	runnerUID = func() int { return real() + 1 }
	t.Cleanup(func() { runnerUID = real })

	err := verifyHomeSkeleton(sandbox.Spec{HomeSkeletonRoot: home, HomeSkeletonDirs: []string{".claude"}})
	if err == nil {
		t.Fatal("verifyHomeSkeleton accepted a directory owned by somebody else")
	}
	msg := err.Error()
	for _, want := range []string{
		dir,                      // which directory, as this container sees it
		strconv.Itoa(real()),     // who owns it
		strconv.Itoa(real() + 1), // who this sandbox is
		"docker volume inspect",  // how to find it on the host
		"podman unshare rm -rf",  // how to delete it under rootless podman
		"sudo rm -rf",            // ...and under rootful docker
		"homes-meta",             // why deleting alone is not enough
		".credentials.json",      // what breaks if it is ignored
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not mention %q; an error that cannot be acted on is not worth failing the job for:\n%v", want, err)
		}
	}
}

// TestVerifyHomeSkeleton_ForeignOwner_RecoveryPathIsRelativeToTheVolumeRoot
// pins the path the recovery instructions actually tell an operator to delete.
//
// The workspace HOME volume is mounted AT the sandbox's $HOME, so the volume's
// root and $HOME are the same directory: a path inside the volume is what is
// left of an in-container absolute path once the mount point is taken off.
// Concatenating the whole in-container path onto `docker volume inspect`'s
// Mountpoint instead names <mountpoint>/home/boid/.claude — which does not
// exist, so the operator deletes nothing, the uid-0 directory survives, and the
// next job fails the same check. Re-running the init cannot repair it either
// (`mkdir -p` on an existing root-owned directory succeeds and changes
// nothing), so the workspace is stuck in a loop the error itself sent it into.
func TestVerifyHomeSkeleton_ForeignOwner_RecoveryPathIsRelativeToTheVolumeRoot(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	real := runnerUID
	runnerUID = func() int { return real() + 1 }
	t.Cleanup(func() { runnerUID = real })

	err := verifyHomeSkeleton(sandbox.Spec{HomeSkeletonRoot: home, HomeSkeletonDirs: []string{".claude"}})
	if err == nil {
		t.Fatal("verifyHomeSkeleton accepted a directory owned by somebody else")
	}
	msg := err.Error()
	if !strings.Contains(msg, "<mountpoint>/.claude") {
		t.Errorf("the recovery instructions do not name the path INSIDE the volume (`<mountpoint>/.claude`):\n%v", err)
	}
	if strings.Contains(msg, "<mountpoint>"+dir) {
		t.Errorf("the recovery instructions concatenate the whole in-container path (%s) onto the volume's mountpoint; "+
			"the volume's root IS $HOME, so that path does not exist and deleting it is a no-op:\n%v", dir, err)
	}
}

// TestVerifyHomeSkeleton_ChecksEveryEntry pins that the loop does not stop at
// the first entry. The engine creates the WHOLE missing bind-target path as uid
// 0, not just its leaf, so any component can be the poisoned one — and a check
// that only looked at ~/.claude would pass on a home whose
// ~/.claude/skills/<name> is root-owned.
func TestVerifyHomeSkeleton_ChecksEveryEntry(t *testing.T) {
	home := t.TempDir()
	first := ".claude"
	second := filepath.Join(".claude", "skills")
	if err := os.MkdirAll(filepath.Join(home, first), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// second is deliberately NOT created.

	err := verifyHomeSkeleton(sandbox.Spec{HomeSkeletonRoot: home, HomeSkeletonDirs: []string{first, second}})
	if err == nil {
		t.Fatal("verifyHomeSkeleton stopped after the first (valid) entry")
	}
	if !strings.Contains(err.Error(), filepath.Join(home, second)) {
		t.Errorf("error names %q rather than the second entry %q:\n%v", first, second, err)
	}
}
