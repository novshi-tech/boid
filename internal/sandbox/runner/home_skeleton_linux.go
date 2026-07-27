//go:build linux

package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// runnerUID reports the uid this sandbox process runs as, i.e. the uid the
// harness will run as and therefore the uid every directory it has to write
// into must be owned by.
//
// A swappable var for one reason, the same one internal/dispatcher used for its
// daemon-side predecessor: the condition this check detects — a directory owned
// by somebody else — cannot be created by a test at all, because chown(2) to a
// foreign uid needs privileges no test process has. So the inequality has to be
// produced by moving the OTHER operand. Production never reassigns it, and a
// literal would be wrong anyway (§D7 derives the uid from the daemon's own;
// GH Actions has been observed at 1001).
var runnerUID = os.Getuid

// verifyHomeSkeleton checks that every directory in spec.HomeSkeletonDirs —
// resolved against spec.HomeSkeletonRoot, the sandbox's $HOME — exists and is
// owned by the uid this process runs as, and returns the first failure.
//
// # What it is for
//
// Those directories are the ancestors of the per-embedded-skill bind targets
// inside the workspace HOME. When one is missing at container start the ENGINE
// creates the whole missing path itself, as uid 0, before this process
// exists — measured on podman 4.9.3 with a named-volume home and
// UsernsMode keep-id (2026-07-27):
//
//	drwxr-xr-t 3    0    0 .claude          <- engine-created, root-owned
//	drwxr-xr-t 3    0    0 .claude/skills
//	$ touch /home/boid/.claude/probe   ->   Permission denied
//
// A root-owned ~/.claude means the harness cannot write
// ~/.claude/.credentials.json or ~/.claude/projects/*.jsonl. Nothing fails
// visibly; the workspace simply stops persisting its authentication, and the
// operator sees a re-login prompt on every task. Under rootless podman the
// host-side owner is a subuid the daemon cannot chown either, so the home stays
// that way until somebody deletes the directory by hand.
//
// # Why the check is here and not on the daemon
//
// Through PR5 of docs/plans/workspace-home-volume-persistence.md the daemon did
// this itself: the workspace home was a host directory, so it could mkdir the
// targets and fstat the results before launching. PR6 made the home a docker
// named volume, whose contents no daemon-side call can see. 論点 e-2 requires
// the verification to live somewhere other than whatever CREATES the
// directories (that is now the init container's prelude), and the job container
// is the one remaining place that qualifies — and it qualifies better than the
// daemon did on three counts: it runs on every dispatch (the init container
// does not), it runs AFTER the engine's auto-creation rather than before it,
// and it runs as the uid whose access is the actual question.
//
// # Why ownership rather than a write probe
//
// A probe would create and delete a file inside a directory the agent is about
// to use, and would pass on a world-writable directory somebody else owns — the
// property being defended is "the daemon's init prepared this", of which "I can
// write to it today" is a weaker consequence. The comparison is against this
// process's own uid (runnerUID) rather than a literal 1000, for the same reason
// dispatcher's daemon-side predecessor used its own: which uid the runner runs
// as follows from the deploy.
//
// # Why it is a detector and not a fix
//
// It cannot repair what it finds: unlinking the contents of a uid-0 directory
// needs write permission on that directory, which is precisely what is missing.
// It also cannot prevent the condition, which is created before this process
// starts. Its value is that a permanently, silently broken workspace HOME
// becomes one loud job failure carrying the repair steps — the same value the
// daemon-side check had (「これは lock ではなく detector である」, 論点 b-2).
//
// # Two spellings of the same directory, and why both appear in the error
//
// Every entry names one directory under two different paths, and the error has
// to carry BOTH because they belong to different readers. The in-container
// absolute path (root + entry) is what this process stats and what a reader of
// the job log recognizes as $HOME/.claude. The path under the volume's host-side
// Mountpoint is entry alone — the HOME mount IS the volume's root, so the
// in-container prefix has no counterpart over there — and it is the only one an
// operator can act on, since deleting the directory requires the host side
// (`podman unshare` / `sudo`). Rendering the in-container path in the recovery
// command was the bug codex's Major 2 found: it produces
// <Mountpoint>/home/boid/.claude, which does not exist, so the repair silently
// does nothing and the next job fails identically.
func verifyHomeSkeleton(spec sandbox.Spec) error {
	if len(spec.HomeSkeletonDirs) == 0 {
		return nil
	}
	if spec.HomeSkeletonRoot == "" {
		// A wiring bug rather than a state to tolerate: without the root the
		// entries would resolve against this process's cwd, which is neither
		// the home nor stable. Failing loud beats checking the wrong
		// directories and reporting them as fine.
		return fmt.Errorf(
			"workspace HOME skeleton: %d directories were declared (%v) with no HomeSkeletonRoot to resolve them against",
			len(spec.HomeSkeletonDirs), spec.HomeSkeletonDirs)
	}
	uid := runnerUID()
	for _, rel := range spec.HomeSkeletonDirs {
		dir := filepath.Join(spec.HomeSkeletonRoot, rel)
		info, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf(
				"workspace HOME skeleton: %q is missing (%v). boid's workspace home init creates it; a job that "+
					"deleted it, or a home that was never prepared, leaves the harness without a writable ~/.claude",
				dir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("workspace HOME skeleton: %q is not a directory (mode %s)", dir, info.Mode())
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			// Unreachable on linux; treated as "cannot tell" rather than
			// "fine", so a platform that stopped answering would surface here
			// instead of silently disabling the check.
			return fmt.Errorf("workspace HOME skeleton: could not read the owner of %q", dir)
		}
		if int(st.Uid) != uid {
			return fmt.Errorf(
				"workspace HOME skeleton: %[1]q is owned by uid %[2]d, not by this sandbox's uid %[3]d. "+
					"container engine が bind target 不在のまま container を起動して uid 0 で自動生成した可能性が高い "+
					"(同じ workspace の並行 job がこのディレクトリを削除/rename した場合に起きる)。"+
					"このまま走ると harness は ~/.claude/.credentials.json を保存できず、認証が毎回失われる。"+
					"[復旧] workspace HOME volume の該当ディレクトリを所有者権限で削除する — "+
					"`docker volume inspect <boid-ws-home-...>` の Mountpoint は volume の root、"+
					"つまり sandbox の $HOME (%[4]s) に対応するので、消すのは volume 内の相対パス %[5]q である。"+
					"rootless podman なら `podman unshare rm -rf <mountpoint>/%[5]s`、"+
					"rootful docker なら `sudo rm -rf <mountpoint>/%[5]s`。"+
					"次の dispatch では completion marker の skeleton 記録は一致したままなので、"+
					"再作成させるには marker (`<dataHome>/homes-meta/<slug>.init.json`) も消して init を 1 回走らせること",
				dir, st.Uid, uid, spec.HomeSkeletonRoot, rel)
		}
	}
	return nil
}
