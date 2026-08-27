package dispatcher

import (
	"fmt"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/integrationpack"
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

// packSkillLinks computes the PackSkillLink set for packs — implementing
// docs/plans/signal-driven-review.md §6.4's last bullet ("job には利用する
// skill だけを read-only mount する", i.e. discoverable at an agent's own
// skill-search path, the same way embedded skills already are) for
// Integration Pack skills, which until this existed were reachable only by
// an explicit "read this path" instruction — a discoverability gap the
// shadow-b evaluation surfaced. Two deliberate departures from §6.4's text,
// not oversights — see PackSkillLink's own doc comment (workspace_init.go)
// for the first, and the EVERY-skill-EVERY-home paragraph below for the
// second:
//
//   - §6.4 does not distinguish bind mount from symlink; this uses a
//     symlink because there is no host-visible directory to bind FROM
//     (Integration Packs are baked into the very image the init container
//     runs from, unlike embedded skills' per-installation materialization).
//   - §6.4 says "利用する skill だけ" (only the skills a job actually
//     uses); this links every skill from every loaded Pack into every
//     workspace home instead.
//
// name validation is a single allowlist call
// (integrationpack.ValidSkillName), not a denylist assembled here — a
// denylist grew a new hole every review round this went through (Opus,
// pre-merge): first "/" (filepath.Base("/") == "/" survives a naive
// single-component check), then "." (filepath.IsLocal(".") is true), and
// finally a NUL byte (filepath.IsLocal("\x00") is true, filepath.Base
// unchanged by it, and bash strips NUL from a script it reads on stdin
// before executing it — turning "rm -rf -- '.claude/skills/<NUL>'" into
// "rm -rf -- '.claude/skills/'"). Each of those, left unrejected, wipes the
// directory every embedded skill's bind target lives under, replacing it
// with a symlink into the Pack — confirmed against a real shell, not just
// reasoned about. ParseManifest is the PRIMARY gate for this (a manifest
// failing it fails daemon startup outright, matching skills[].path's
// existing filepath.IsLocal check); this call is defense in depth for a
// PackSkillLink built some other way, not a second independent design.
//
// A second guard, embeddedSkillNames[skill.Name], rejects a Pack skill that
// collides with an embedded one (boid-task/boid-orchestrate/boid-web/
// boid-signal, internal/skills.EmbeddedSkillNames()). Without it, the same
// rm -rf replaces that skill's bind-mounted directory with a symlink into
// the Pack, silently breaking whichever embedded skill lost the race — v0
// Packs happen not to collide today, but nothing enforced that. Note the
// gap this does NOT close, and get the failure mode right (an earlier
// version of this comment guessed wrong and a review round, Opus round 3,
// corrected it against a real filesystem): if a FUTURE release adds an
// embedded skill whose name a Pack skill already claimed,
// workspaceHomeSkeletonDirs' mkdir -p for that name does NOT fail — `mkdir
// -p` against a symlink that resolves to an existing directory (which the
// Pack's still-present skill directory is) succeeds (exit 0), so the
// prelude completes and no wedge happens there. The actual failure lands
// one step later, at JOB START, not at workspace-home init: the runner's
// own preflight (internal/sandbox/runner's verifyHomeSkeleton) os.Stat's
// (follows the symlink) each skeleton path expecting the CONTAINER's own
// uid to own it, finds the Pack's root-owned image content instead, and
// refuses to start the harness — misdiagnosed by that check's own error
// text as "the engine auto-created this as uid 0", since that is the only
// cause it knows how to name. There IS a recovery path, just not an
// automatic one: the operator remedy that error prints (rm -rf the bind
// target under the workspace home, then redispatch) happens to unlink the
// symlink correctly, because rm -rf on a symlink argument removes the link
// itself rather than recursing into what it points at (the same POSIX
// behavior TestBuildWorkspaceInitRequest_PackSkillSymlinkRerunDoesNotDeleteTarget
// pins from the other direction).
//
// A skill failing either guard is skipped rather than failing the whole
// dispatch — one malformed Pack, or one Pack that happens to reuse an
// embedded skill's name, must not take every workspace's skill discovery
// down with it.
//
// Two Packs declaring the same skill Name is not rejected either: both
// entries land in the returned slice, in packs' own order, and the last one
// wins when buildWorkspaceInitScript's rm+ln pair for that Name runs twice.
// The same applies to two VERSIONS of one Pack both being present under
// integrations.dir (LoadPacks enumerates every version directory, not just
// the newest) — the winner is whichever sorts last out of os.ReadDir's
// lexicographic version-directory order, which is not necessarily the
// newest version ("1.10.0" < "1.2.0" < "1.9.0"). v0 has no Pack-namespacing
// or version-pinning story to make either case an error rather than a
// deployment-order footgun; today's deployment keeps exactly one version of
// each of the three official Packs, and their skill names do not collide.
//
// EVERY loaded Pack's EVERY skill is linked into EVERY workspace home,
// regardless of Skill.RequiresServiceProfile or whether the workspace has
// any service instance configured for that profile at all — this ignores
// docs/plans/signal-driven-review.md §6.4's stated design ("job には利用す
// る skill だけを read-only mount する"), which called for per-JOB,
// per-USED-skill mounting. Widening it to "every skill, every workspace
// home" was a deliberate scope cut for this evaluation follow-up (a
// workspace-scoped or job-scoped filter needs to resolve which service
// instances a workspace actually configured, which resolveWorkspaceHome
// does not currently do), not an oversight — but it does mean a workspace
// with no Jira instance configured still gets a discoverable jira-api
// skill, which is exactly the kind of irrelevant-service noise the
// shadow-b evaluation this follow-up responds to was trying to reduce.
// Revisit alongside §6.4 if that noise turns out to matter in practice.
//
// One more consequence of "every workspace home", specifically: the init
// container ALWAYS runs from the daemon's own default image
// (container_backend_workspace_init.go §D8), but a JOB container honors a
// workspace's container_image override (resolveContainerImage). A
// workspace with such an override gets Target symlinks pointing at
// /opt/boid/integrations/... paths that exist in the init container's
// image but may not exist in that workspace's own job image — a dangling
// symlink, not a destructive one, but still a gap in the "same image, so
// no bind mount needed" reasoning PackSkillLink's doc rests on.
func packSkillLinks(packs []*integrationpack.Pack) []PackSkillLink {
	names := skills.EmbeddedSkillNames()
	embeddedSkillNames := make(map[string]bool, len(names))
	for _, name := range names {
		embeddedSkillNames[name] = true
	}
	var links []PackSkillLink
	for _, pack := range packs {
		for _, skill := range pack.Manifest.Skills {
			if !integrationpack.ValidSkillName(skill.Name) {
				continue
			}
			if embeddedSkillNames[skill.Name] {
				continue
			}
			links = append(links, PackSkillLink{
				Name:   skill.Name,
				Target: filepath.Join(pack.Dir, skill.Path),
			})
		}
	}
	return links
}

// packSkillLinkMarkerStrings renders links as an order-insensitive set of
// strings for workspaceHomeMarker.PackSkillLinks, comparable across runs the
// same way workspaceHomeMarker.SkeletonDirs already is (equalStringSets).
//
// A changed set — a Pack upgraded to a new version directory, a skill
// added/removed/renamed — must force a re-init, on the same footing as a
// changed skeleton: left unnoticed, a symlink would keep pointing at a
// version directory the current image no longer has, or a newly-added
// skill would simply stay undiscoverable until something else happened to
// force a re-init.
func packSkillLinkMarkerStrings(links []PackSkillLink) []string {
	out := make([]string, 0, len(links))
	for _, link := range links {
		out = append(out, link.Name+"="+link.Target)
	}
	return out
}
