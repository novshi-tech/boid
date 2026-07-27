package dispatcher

import (
	"fmt"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/skills"
)

// This file owns the daemon side of how the embedded skills (/boid-task,
// /boid-orchestrate, /boid-web) reach a job's sandbox — PR3 of
// docs/plans/workspace-home-volume-persistence.md (論点 e-2), as amended by
// PR6.
//
// Phase 4 PR3 (docs/plans/home-workspace-volume.md) had Runner.Dispatch
// copy-sync the skill content straight into the workspace HOME
// (<home>/.claude/skills), replacing the per-skill bind mounts the
// claude/codex/opencode adapters used to declare. PR6 of the current plan
// turns the workspace HOME into a per-workspace named volume, whose contents
// the daemon can only touch through a container — so that write has to go
// somewhere the daemon still owns, and the skills have to reach $HOME by
// some route other than "the daemon already wrote them there".
//
// The route back is the one the adapters used to take, minus the adapters:
// materialize the embedded set once per installation in a host-visible
// runtime dir and bind it into each job. That is sound specifically because
// embedded skills are regenerable from the boid binary at any moment — they
// are the one piece of workspace HOME content that may freely be volatile,
// which is why they can leave the persistent home while the harness
// credentials and toolchain cannot.
//
// PR6 completed that move: the workspace HOME is now the named volume this
// file's prose anticipated, and the last daemon-side write into it — the
// per-dispatch mkdir of the bind targets — is gone with it. See
// syncEmbeddedSkills for where creating and verifying those directories went.

// embeddedSkillsDir returns <runtimesDir>/skills, the single per-INSTALLATION
// directory Runner.Dispatch materializes the embedded skill set into.
//
// Under the runtimes root rather than the daemon's own data root because the
// job container's bind mount SOURCE has to resolve on the HOST filesystem:
// server/wire.go wires Runner.RuntimesDir from hostVisibleRuntimesDirFor(cfg)
// for exactly that reason (see its doc comment and realization.go's
// MountSourceHostPath), whereas the data root is the `boid_state` named
// volume under the compose deploy — invisible to the host engine resolving a
// sibling container's bind source.
//
// One directory per installation, not per workspace or per job: the content
// is byte-identical for every job of a given boid build, skills.DeployAll
// only rewrites files whose content differs, and its writes are safe against
// concurrent calls over the same baseDir (internal/skills/safe_deploy.go —
// atomic temp-file writes plus a PID-liveness-checked sweep of temp files a
// crashed run left behind). Concurrent dispatches therefore converge on the
// same tree instead of racing.
//
// INTERACTION WITH cleanOrphanRuntimes — do not "optimize" the per-dispatch
// call away. internal/server/wire.go's cleanOrphanRuntimes walks every
// directory directly under the runtimes root at daemon startup and
// os.RemoveAll's the ones with no matching jobs.runtime_id row, so `skills`
// is deleted on EVERY daemon restart — the same treatment the sibling
// spec/tls/broker-tls dirs get, and harmless for the same reason: whatever a
// restart removes, the next dispatch re-materializes before it can be
// mounted. Making the sync conditional (a sync.Once, a "already deployed"
// flag, a check for the directory's existence) would turn that startup sweep
// into jobs silently dispatched with no skills at all.
//
// Falls back to the $XDG_DATA_HOME / ~/.local/share/boid convention when
// runtimesDir is empty — the same fallback, for the same reason, as
// WorkspaceHomesDir's and workspaceHomeMetaDir's: a bare &Runner{} (minimal
// test wiring, or a daemon build that never wired RuntimesDir) must not
// write into whatever real $HOME `go test` happens to run under, and must
// still resolve SOMEWHERE rather than failing dispatch. server/wire.go never
// produces an empty RuntimesDir, so no deployment reaches this branch — and
// a mount source resolved through it would not be host-visible if one did.
func embeddedSkillsDir(runtimesDir string) (string, error) {
	if runtimesDir != "" {
		return filepath.Join(runtimesDir, "skills"), nil
	}
	root, err := workspaceDataHomeRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "skills"), nil
}

// syncEmbeddedSkills materializes the embedded skill set for this dispatch
// and returns the directory holding it, for BuildSandboxSpec to bind each
// skill in from (SandboxRuntimeInfo.SkillsSourceDir -> homeMounts).
//
// # What it no longer does, and where that work went (PR6)
//
// PR3 also had this function create the bind TARGETS inside the workspace home
// — <home>/.claude, <home>/.claude/skills and one directory per embedded skill
// — on every dispatch, and verify that the daemon owned each of them. That was
// never optional work: an ABSENT bind target is not a benign condition, because
// the container engine creates the whole missing path itself, at container
// start, as uid 0. Measured on podman 4.9.3 with a named-volume home and
// UsernsMode keep-id (2026-07-27, re-confirming 論点 b-2's earlier measurement
// against the new storage):
//
//	drwxr-xr-t 3    0    0 .claude          <- engine-created, root-owned
//	drwxr-xr-t 3    0    0 .claude/skills
//	$ touch /home/boid/.claude/probe   ->   Permission denied
//
// A root-owned ~/.claude locks the uid-1000 harness out of
// ~/.claude/.credentials.json and ~/.claude/projects/*.jsonl — the change made
// to protect the credentials would be what stops them being written — and under
// rootless podman the host-side owner is a subuid (100000, measured) that
// neither the harness nor the daemon can chown back.
//
// PR6 turns the workspace home into a docker named volume, which takes the
// daemon's ability to create those directories away outright. The work split in
// two, and both halves are load-bearing:
//
//   - CREATION moved into the init container's builtin prelude, which already
//     created the same set (workspaceHomeSkeletonDirs, shared so the two cannot
//     drift). What PR6 had to add is a reason for that container to run again
//     when the SET changes rather than only when init.sh changes: the completion
//     marker now records the skeleton it was prepared for
//     (workspaceHomeMarker.SkeletonDirs). Without that, a release adding an
//     embedded skill would add a bind target to a home whose marker still
//     matched — no prep, no directory, engine creates it as uid 0.
//   - VERIFICATION moved into the job container itself, where
//     sandbox.Spec.HomeSkeletonDirs is checked by the runner before the harness
//     starts (internal/sandbox/runner's verifyHomeSkeleton). 論点 e-2 requires
//     the check to live somewhere other than the thing that creates these
//     directories, and the runner satisfies that better than the daemon did: it
//     is not the creator, it runs on EVERY dispatch (the creator does not), it
//     runs after the engine's auto-creation rather than before it, and it runs
//     as the very uid whose write access is the property in question.
//
// What is genuinely lost is the daemon-side mkdir's incidental HEALING: a job
// that deleted ~/.claude and exited used to have it re-created, correctly
// owned, by the next dispatch. Nothing daemon-side can do that to a volume, so
// the next launch now poisons the home instead and the runner reports it. The
// detection this trades against was always the documented value of the check
// (「これは lock ではなく detector である」, 論点 b-2) and the window it covers
// was never closed; the healing was a side effect of doing the mkdir at all.
// See docs/plans/workspace-home-volume-persistence.md 論点 b-2 for the full
// account.
//
// # Everything below is unchanged from PR3
//
// Failure is loud: the returned error fails the whole dispatch, preserving
// the contract Phase 4 PR3 established for the copy-sync this replaces. A
// job started against a stale, missing or partially-prepared skill set
// misbehaves silently — it simply cannot find /boid-task — which is far
// harder to diagnose than a failed dispatch. Nothing here is made
// best-effort, and the mounts homeMounts derives from the returned directory
// deliberately carry no Guard: a vanished source must abort the job, not be
// skipped.
func (r *Runner) syncEmbeddedSkills() (string, error) {
	sourceDir, err := embeddedSkillsDir(r.RuntimesDir)
	if err != nil {
		return "", fmt.Errorf("resolve embedded skills dir: %w", err)
	}
	if err := skills.DeployAll(sourceDir); err != nil {
		return "", fmt.Errorf("materialize embedded skills into %q: %w", sourceDir, err)
	}
	return sourceDir, nil
}

// workspaceHomeSkeletonDirs lists, relative to a workspace home, every
// directory that must exist before a job container starts: each per-skill bind
// target and every ancestor of one.
//
// The ancestors are listed explicitly rather than left to `mkdir -p`/the
// per-skill walk to create as a side effect, because the condition being
// guarded against applies to them individually — the container engine
// auto-creates the WHOLE missing path as uid 0, not just its leaf, so any
// component of it can be the poisoned one and each therefore has to be covered
// by the checks below.
//
// One function, three consumers, and that is the point (論点 b-2):
//
//   - the init container's builtin prelude CREATES this set inside the home
//     (dispatcher.buildWorkspaceInitScript);
//   - resolveWorkspaceHome RECORDS it in the completion marker, so a release
//     that changes the set makes the prelude run again
//     (workspaceHomeMarker.SkeletonDirs);
//   - BuildSandboxSpec puts it into sandbox.Spec.HomeSkeletonDirs, where the
//     job container's own runner VERIFIES it before the harness starts.
//
// A drift between them is not cosmetic — a directory the prelude forgot is one
// the engine creates at uid 0 on the next launch, which neither the uid 1000
// harness nor the daemon can chown back.
//
// A swappable var rather than a plain function so a test can produce the one
// input that matters and cannot otherwise be constructed: a DIFFERENT skeleton
// set. The real set is decided by the embed directive, so it is a constant of
// the binary under test, and "the marker notices a release that added a skill"
// is otherwise unobservable. Same style as internal/api/workspace_homes.go's
// apparentSizeFn; production never reassigns it.
var workspaceHomeSkeletonDirs = func() []string {
	dirs := []string{
		".claude",
		filepath.Join(".claude", "skills"),
	}
	for _, name := range skills.EmbeddedSkillNames() {
		dirs = append(dirs, filepath.Join(".claude", "skills", name))
	}
	return dirs
}
