package dispatcher

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/skills"
)

// This file owns the daemon side of how the embedded skills (/boid-task,
// /boid-orchestrate, /boid-web) reach a job's sandbox — PR3 of
// docs/plans/workspace-home-volume-persistence.md (論点 e-2).
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
// skill in from (SandboxRuntimeInfo.SkillsSourceDir → homeMounts).
//
// It also creates the bind TARGETS inside the workspace home:
// <home>/.claude, and <home>/.claude/skills/<name> for every embedded skill.
// Those directories used to exist as a side effect — skills.DeployAll's
// symlink-safe walk created them on its way to writing the content there —
// and something has to keep creating them now that the content lands
// elsewhere, because an ABSENT bind target is not a benign condition: the
// container engine creates the missing intermediate path itself, at container
// start, as uid 0. Measured on podman 4.9.3 with an empty home and a
// /home/boid/.claude/skills bind (docs/plans/workspace-home-volume-persistence.md
// 論点 b-2):
//
//	drwxr-xr-t 3    0    0 .claude          ← engine-created, root-owned
//	drwxrwxr-x 2 1000 1000 .claude/skills
//
// A root-owned ~/.claude locks the uid 1000 harness out of
// ~/.claude/.credentials.json and ~/.claude/projects/*.jsonl — i.e. the
// change made to protect the credentials would be what stops them being
// written. The engine does this before the entrypoint runs, so nothing on
// the sandbox side can repair it.
//
// Creating them from the daemon is deliberately a STOPGAP, and the plan doc
// says where it goes: PR5 introduces a prep container that runs once per
// workspace home as uid 1000, and the skeleton mkdir moves there (論点 b-2),
// because from PR6 the home is a volume the daemon cannot write to at all.
// Expressing the requirement in the mount spec instead is not an option
// today: sandbox.Mount.NeedsDirs exists but is dead — the userns runner was
// its only reader and PR-4 of docs/plans/volume-only-daemon.md removed that
// backend, so internal/sandbox/realization never looks at the field.
//
// Failure is loud: the returned error fails the whole dispatch, preserving
// the contract Phase 4 PR3 established for the copy-sync this replaces. A
// job started against a stale, missing or partially-prepared skill set
// misbehaves silently — it simply cannot find /boid-task — which is far
// harder to diagnose than a failed dispatch. Nothing here is made
// best-effort, and the mounts homeMounts derives from the returned directory
// deliberately carry no Guard: a vanished source must abort the job, not be
// skipped.
//
// # The bind-target race, and the deliberate decision not to close it
//
// prepareBindTarget's mkdir and the container launch that consumes its result
// are separated by the whole rest of Runner.Dispatch. Another job of the SAME
// workspace holds that workspace home read-write for its entire lifetime —
// homeMounts binds <homes>/<slug> at $HOME with no ReadOnly, and
// resolveWorkspaceHome keys that directory by workspace slug alone, not by job
// — so in that window a concurrent job can rename or delete ~/.claude. The
// engine then finds the bind target missing at launch and auto-creates it as
// uid 0, exactly as measured above. That window is NOT closed here, for three
// reasons:
//
//  1. It is not new. A job of this workspace can already delete
//     ~/.claude/.credentials.json outright, or rename ~/.claude, at any moment
//     — with or without the bind targets this function creates. PR3 introduced
//     no new capability on that side; homeMounts' own doc comment has recorded
//     the "two jobs sharing a workspace can run concurrently" premise since
//     the Phase 5b PR6 codex review.
//  2. Closing it means serializing dispatch. The check and the launch would
//     have to be one critical section per workspace, i.e. every dispatch of a
//     workspace would hold a lock across container startup. That price is not
//     worth paying for a race whose window is already reachable by simpler
//     means (1).
//  3. PR5's prep container does not close it either. Prep runs once per
//     workspace home; jobs keep the home rw afterwards, so the same rename is
//     available the moment prep finishes. Deferring to PR5 would be deferring
//     to something that never arrives.
//
// What IS closed is the part that does not heal. A uid 0 directory — under
// rootless podman, a host subuid mapped away from the daemon's own uid — can
// be repaired by neither the uid 1000 harness nor the daemon itself, so the
// workspace home stays poisoned for every future dispatch and the symptom is a
// silent failure to persist ~/.claude/.credentials.json. prepareBindTarget
// therefore verifies, after each mkdir, that the directory now at that path is
// owned by this daemon, and fails the dispatch when it is not. That converts a
// permanent silent breakage into one loud failure carrying the repair steps.
//
// The check is a detector, not a lock: a swap landing after it still wins the
// race. Its value is that the NEXT dispatch reports the poisoning instead of
// running into it. Recorded as such in 論点 b-2 of
// docs/plans/workspace-home-volume-persistence.md.
func (r *Runner) syncEmbeddedSkills(workspaceHomeDir string) (string, error) {
	sourceDir, err := embeddedSkillsDir(r.RuntimesDir)
	if err != nil {
		return "", fmt.Errorf("resolve embedded skills dir: %w", err)
	}
	if err := skills.DeployAll(sourceDir); err != nil {
		return "", fmt.Errorf("materialize embedded skills into %q: %w", sourceDir, err)
	}

	// Every ancestor of a per-skill bind target, then the targets themselves.
	// <home>/.claude/skills is prepared explicitly rather than left to the
	// per-skill walk to create as a side effect, so that it too is covered by
	// the ownership check — the engine auto-creates the whole missing path, not
	// just its leaf, so any component of it can be the poisoned one.
	claudeDir := filepath.Join(workspaceHomeDir, ".claude")
	if err := prepareBindTarget(claudeDir); err != nil {
		return "", err
	}
	skillsDir := filepath.Join(claudeDir, "skills")
	if err := prepareBindTarget(skillsDir); err != nil {
		return "", err
	}
	for _, name := range skills.EmbeddedSkillNames() {
		if err := prepareBindTarget(filepath.Join(skillsDir, name)); err != nil {
			return "", err
		}
	}
	return sourceDir, nil
}

// The two variables below are the ONLY seams in this file, and they exist for
// the same underlying reason: the condition prepareBindTarget detects — a bind
// target owned by somebody other than this daemon — cannot be created by a
// test at all, because chown(2) to a foreign uid needs privileges no test
// process has. So the inequality has to be produced by moving one side of the
// comparison instead. Both are plain function variables initialized to the
// real implementation, in the same style as internal/api/workspace_homes.go's
// apparentSizeFn and internal/sandbox/fetch_builtin.go's newFetchClient: the
// production path has no test-only branch in it, and reads exactly as if the
// indirection were not there.
//
// Which seam a test reaches for follows from which property it pins, and
// neither subsumes the other:
//
//   - daemonUID moves the DAEMON's side, globally. The directory is real and
//     the fstat is real, so this is the axis that pins "the comparison is
//     against whatever uid this process runs as", not a hard-coded 1000 —
//     a literal would survive bindTargetOwnerUID, since a test running as uid
//     1000 would still see the injected 1001 rejected.
//   - bindTargetOwnerUID moves the DIRECTORY's side, for one path at a time
//     (the test wrapper delegates to the real function for every other path).
//     That is the only axis on which "which component was checked" is
//     observable, which is what makes deleting one of the three
//     prepareBindTarget call sites in syncEmbeddedSkills detectable.
//
// See TestDispatch_SkillsBindTargetNotDaemonOwned_FailsDispatch and
// TestDispatch_SkillsBindTargetForeignOwner_FailsPerComponent respectively.

// daemonUID reports the uid this daemon process runs as, i.e. the uid every
// directory the daemon itself creates ends up owned by.
var daemonUID = os.Getuid

// bindTargetOwnerUID creates one bind target directory and reports the uid
// owning the result — skills.MkdirAllNoSymlink, which does both in a single
// symlink-checked walk and reads the owner with fstat(2) on the descriptor
// that walk ended on (see its doc comment for why the ownership read cannot be
// a separate stat by path).
var bindTargetOwnerUID = skills.MkdirAllNoSymlink

// prepareBindTarget creates one per-skill bind target (or one of their shared
// ancestors) inside the workspace home and verifies the daemon owns the result.
//
// The mkdir is skills.MkdirAllNoSymlink rather than os.MkdirAll because the
// path is inside storage the job owns read-write; see that function and
// internal/skills/safe_deploy.go's package comment for the symlink threat
// model it exists for.
//
// The ownership check is the part that is about a DIFFERENT threat. A symlink
// makes the daemon write somewhere else and is refused outright by the walk; a
// directory already created by the container engine at uid 0 is a perfectly
// ordinary directory that the walk happily enters — the mkdir returns EEXIST,
// the dispatch proceeds, and the job starts against a $HOME it cannot write
// its credentials into. Nothing later in dispatch would notice. See
// syncEmbeddedSkills' doc comment for why this is detected here rather than
// prevented.
//
// Both the mkdir and the ownership read go through bindTargetOwnerUID (see its
// doc comment and the seam note above it): one symlink-checked walk that ends
// on a descriptor, and an fstat on that same descriptor. Splitting them into a
// mkdir plus a follow-up stat by path would put a check-then-use gap back
// exactly where this function is trying to remove one.
//
// The comparison is against the daemon's own uid (daemonUID), never a literal:
// which uid runs the daemon differs between a bare `boid start`, the compose
// deploy's in-image `boid` user, a developer machine and CI, and the property
// that matters is "did I create this" rather than "is this uid 1000". A
// consequence worth naming: a daemon that itself runs as uid 0 accepts an
// engine-created uid 0 directory, correctly — such a daemon CAN chown it — even
// though the uid-1000 harness still could not write there. That combination
// (rootful daemon, non-root harness) is outside the deploy this project
// supports; the check does not pretend to cover it.
//
// # Why the recovery below is a direct deletion rather than `workspace remove`
//
// This error's entire value is that it turns a permanently, silently broken
// workspace HOME into one diagnosable failure. That value is only real if the
// steps it names can actually be carried out, so the primary instruction is the
// one that works everywhere: delete the offending directory with whatever
// privileges its owner requires. Under rootless podman the owner is a host
// subuid the daemon's own user owns the range of but cannot unlink as itself,
// which is what `podman unshare` exists for; under rootful docker it is real
// root, so `sudo`. The next dispatch then re-creates the directory as the
// daemon (syncEmbeddedSkills runs on every dispatch — see its doc comment).
//
// Nothing of value is lost with it. Only this directory's own subtree goes,
// leaving the rest of the workspace HOME — the toolchain init.sh installed,
// everything outside the deleted path — untouched; and the harness demonstrably
// never managed to write inside the deleted subtree, since being unable to
// write there is the failure this error reports.
//
// `boid workspace remove <slug>` was the pre-review recommendation and is a bad
// primary instruction on three independent counts, each verified against the
// code rather than assumed:
//
//   - The `default` workspace cannot be removed at all, and it is the workspace
//     every project without an explicit assignment dispatches into
//     (normalizeWorkspaceSlug). Three layers reject the reserved slug before
//     anything is deleted — cmd/workspace.go's runWorkspaceRemove, internal/
//     api's ProjectAppService.RemoveWorkspace, and orchestrator.
//     WorkspaceRepository.Remove — and internal/api/workspace_homes.go's
//     deleteWorkspaceHome additionally short-circuits on the same slug, so even
//     a caller that got past the row guard would not delete its home directory.
//   - For any other workspace, home deletion is best-effort and merely
//     REPORTED. api.WorkspaceHandler.Remove deletes the DB row first and keeps
//     the response at 200 even when the subsequent os.RemoveAll fails, per
//     WorkspaceRemoveResponse's documented "part-completed" contract. A
//     foreign-owned directory inside the home — precisely the condition this
//     error is about — is what makes that RemoveAll fail: unlinking the entries
//     UNDER a uid 0 directory needs write permission on that directory, which
//     the daemon's uid does not have (measured: RemoveAll over a non-writable
//     directory with a child returns EACCES on the child's unlinkat; over an
//     empty one it succeeds, since only the parent's permissions matter then).
//     The engine-created case always has children, because it creates the whole
//     missing bind-target path rather than just its leaf — 論点 b-2's measured
//     .claude + .claude/skills pair. The outcome is the worst of both:
//     the workspace definition is gone and the poisoned home is still there,
//     now an orphan (ListWorkspaceHomeSizes reports it as such). Whether that
//     happened is only visible in the CLI output line — `home dir deleted:` vs
//     `warning: home dir delete failed` (formatWorkspaceRemoveResult).
//   - Even on complete success it removes the workspace itself, re-pointing
//     every project assigned to it at `default` (WorkspaceRepository.Remove's
//     transaction). Recovering from that means recreating the workspace and
//     re-assigning those projects, which is a great deal more than the operator
//     asked for when all they needed was one directory gone.
//
// It stays in the message as a documented fallback with its caveats attached,
// rather than being cut: it is genuinely the way out when the home is damaged
// beyond this one directory. TestPrepareBindTarget_ForeignOwnerRecoveryIsAvailable
// pins that pairing — mention it, and the caveats must come with it.
//
// The init.sh claim the pre-review text made is separately true but was
// attached to the wrong route: deleting the home directory (by any means) takes
// the .boid-workspace-home-id nonce with it, so workspaceHomeInitialized
// refuses the surviving completion marker and the next dispatch re-runs init.sh
// (see workspace_home.go, 論点b). That is why the direct-deletion route below
// says the rest of the home survives — it deletes strictly less than the home.
func prepareBindTarget(dir string) error {
	ownerUID, err := bindTargetOwnerUID(dir)
	if err != nil {
		return fmt.Errorf("prepare skill bind target %q: %w", dir, err)
	}
	if ownerUID == daemonUID() {
		return nil
	}
	return fmt.Errorf(
		"prepare skill bind target %[1]q: このディレクトリの所有者が daemon (uid %[2]d) ではなく uid %[3]d になっている。"+
			"考えられる原因は 2 つ: (a) 同じ workspace の並行 job がこのディレクトリを削除/rename し、"+
			"bind target 不在のまま job container が起動したため container engine が uid 0 "+
			"(rootless podman では host 側 subuid に写像される) で自動生成した、"+
			"(b) job が直接この所有者に差し替えた。"+
			"いずれの場合も daemon はこのディレクトリを chown で修復できず、"+
			"このまま dispatch すると harness が ~/.claude/.credentials.json を保存できないまま走り、"+
			"認証が毎回失われる。"+
			"[復旧] 当該ディレクトリを所有者権限で削除する — rootless podman なら `podman unshare rm -rf %[1]q`、"+
			"rootful docker なら `sudo rm -rf %[1]q`。次の dispatch が daemon 所有で作り直す。"+
			"消えるのはこのディレクトリ配下だけなので workspace HOME の他の中身 (init.sh が入れた toolchain 等) は残り、"+
			"配下に harness の認証情報も入っていない (書き込めないことがこの障害そのもの)。"+
			"[非推奨: `boid workspace remove <slug>`] default workspace は予約 slug なので削除自体が拒否され、"+
			"home ディレクトリの削除もスキップされるため、この復旧には使えない。"+
			"他の workspace でも home の削除は best-effort で、所有者の違うディレクトリが残っていると削除に失敗し、"+
			"workspace 定義 (DB) だけ消えて home が孤児として残る — 出力の `home dir deleted:` / "+
			"`warning: home dir delete failed` で成否の確認が必須。"+
			"削除に成功した場合も workspace 定義ごと消えるので、workspace を作り直して project を再 assign する必要がある",
		dir, daemonUID(), ownerUID)
}
