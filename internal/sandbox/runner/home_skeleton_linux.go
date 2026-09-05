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
// into must be owned by. A swappable var because the condition this check
// detects — a directory owned by somebody else — cannot be produced in a
// test via chown(2) without privileges, so the inequality is instead
// produced by moving this operand.
var runnerUID = os.Getuid

// verifyHomeSkeleton checks that every directory in spec.HomeSkeletonDirs —
// resolved against spec.HomeSkeletonRoot, the sandbox's $HOME — exists and is
// owned by the uid this process runs as, and returns the first failure.
//
// This detects a container engine auto-creating a missing bind-target
// ancestor (e.g. ~/.claude) as uid 0 at container start, which silently
// blocks the harness from persisting its own credentials there. It is a
// detector, not a fix: it cannot repair or prevent the condition, only turn
// a permanently, silently broken workspace HOME into one loud job failure
// carrying the repair steps. See docs/plans/workspace-home-volume-persistence.md
// for the full background and the podman reproduction that motivated it.
//
// The error message below names the same directory under two different
// paths on purpose: the in-container absolute path is what this process
// stats, while the path relative to the volume's host-side Mountpoint (HOME
// mount IS the volume's root) is the only one an operator can actually act
// on to delete it.
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
