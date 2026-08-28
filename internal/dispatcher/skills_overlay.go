package dispatcher

import (
	"os"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/integrationpack"
	"github.com/novshi-tech/boid/internal/skills"
)

// This file owns the daemon side of how skills — boid's own embedded set
// (/boid-task, /boid-orchestrate, /boid-web, /boid-signal) and every
// Integration Pack's — reach a job's sandbox.
//
// # One mechanism, after three
//
// The embedded set has moved between delivery mechanisms three times. Phase 4
// PR3 (docs/plans/home-workspace-volume.md) copy-synced it into the workspace
// HOME; PR3 of docs/plans/workspace-home-volume-persistence.md (論点 e-2)
// materialized it per installation under the host-visible runtimes root and
// bind-mounted it in, one read-only bind per skill; Pack skills, arriving
// later, used a symlink instead, because a Pack is baked into the image and
// has no host-visible directory to bind FROM.
//
// That last asymmetry was the bug. Its stated reason — "Packs are in the
// image, embedded skills are not" — was never true of the CONTENT: the boid
// binary is in the image (build/container/Dockerfile's `COPY --from=builder
// /out/boid`), and the embedded skills are inside that binary. They were
// merely not unpacked. Baking them out to embeddedSkillsImageDir at image
// build time makes the two sets identical in kind, and one symlink mechanism
// serves both.
//
// What went with the binds:
//
//   - the per-installation materialization under <runtimesDir>/skills, and
//     with it the requirement that a bind SOURCE resolve on the HOST
//     filesystem. That requirement is what tied skill delivery to a
//     host-visible path — the one piece of this that a non-DooD backend
//     (k8s, where a hostPath mount is a scheduling and policy problem rather
//     than a path problem) could not have satisfied.
//   - the per-skill entries in workspaceHomeSkeletonDirs, and therefore the
//     completion-marker churn they caused: the skeleton is recorded in
//     workspaceHomeMarker.SkeletonDirs, so a release that added one embedded
//     skill changed the set and re-ran init.sh for every workspace on the
//     installation. The set is now a constant of the discovery roots.
//   - homeMounts' skill loop, and the deliberate absence of a Guard on those
//     mounts (a vanished source had to abort the job rather than be skipped).

// embeddedSkillsImageDir is where build/container/Dockerfile unpacks the
// binary's embedded skill set, and therefore what a SkillLink for an embedded
// skill points at.
//
// Under /opt/boid alongside the Integration Pack registry (/opt/boid/
// integrations) because the two are the same kind of thing now: read-only
// image content that every container started from this image already has,
// named by a path rather than carried by a mount.
//
// It is a path inside the CONTAINER, never a host path and never a mount
// source. The init container and the job container both see it because both
// start from the image that baked it — the one assumption this rests on, and
// the same one Integration Pack targets already rested on.
//
// # Where that assumption does and does not hold
//
// buildWorkspaceInitScript checks each Target with `[ -d ]` before linking it
// and fails the whole init if it is missing, so the INIT container's copy of
// this path is verified rather than assumed. That covers the realistic skew —
// a daemon binary newer than the runner image it launches — because the init
// container runs from boid's own default image and so does every job by
// default.
//
// It does not cover a workspace with a container_image override: the init
// container still runs from boid's default image
// (container_backend_workspace_init.go §D8), so the check passes, while the
// JOB container runs the override and may have no /opt/boid/skills — leaving
// links that dangle there and only there. That gap predates image-baked
// skills (Pack targets always had it) but it did widen: the per-skill bind
// mounts used to carry the embedded set across any image. An override image
// that wants boid's skills has to bake this path itself.
//
// # The two names below
//
// defaultEmbeddedSkillsImageDir is the LITERAL, and it has to equal the
// destination of build/container/Dockerfile's `COPY internal/skills/data`.
// Nothing in Go can check that at compile time, so
// TestEmbeddedSkillsImageDir_IsWhereTheDockerfileCopiesThem reads the
// Dockerfile — the same trick, for the same reason, as
// TestBoidRunnerProtocolLabel_IsBakedIntoTheImage. Without it a drift on
// either side leaves every unit test green (both sides of every assertion
// would move together) and every embedded skill a dangling symlink in
// production.
//
// embeddedSkillsImageDir is the value actually used, a var so this package's
// TestMain can point it at a directory that exists: the `[ -d ]` check above
// makes a missing Target a hard failure, and /opt/boid/skills is not present
// on a machine running `go test`. Production never reassigns it — same
// convention as workspaceHomeSkeletonDirs. Runner.SkillsImageDir overrides it
// per Runner, which is how a test in ANOTHER package (internal/server wires
// its own dispatcher.Runner) reaches the same seam.
const defaultEmbeddedSkillsImageDir = "/opt/boid/skills"

// embeddedSkillsImageDir resolves the value actually used. Runner.SkillsImageDir
// wins when set; otherwise BOID_TEST_IMAGE_SKILLS_DIR, which testutil/homeenv
// points at a populated temp directory for the lifetime of a test binary (see
// that package's imageSkillsEnv for why the seam is an env var: the suites that
// need it live in several packages, and an unexported var here cannot be reached
// from any of them); otherwise the real image path. Nothing in a deployment sets
// the variable — boid builds the image that carries the real path.
//
// Read per call rather than at package init so a TestMain that sets it after
// this package is initialized still takes effect.
func embeddedSkillsImageDir() string {
	if dir := os.Getenv("BOID_TEST_IMAGE_SKILLS_DIR"); dir != "" {
		return dir
	}
	return defaultEmbeddedSkillsImageDir
}

// skillDiscoveryRoots are the directories, relative to a workspace home, that
// every skill is symlinked into.
//
// Two rather than one because no single root is read by all three harnesses
// boid dispatches, and the intersection is empty:
//
//	harness      ~/.claude/skills   ~/.agents/skills
//	-----------  -----------------  ----------------
//	Claude Code  yes                no
//	codex        no                 yes
//	opencode     yes                yes
//
// Claude Code scans ~/.claude/skills, .claude/skills, plugin directories and
// --add-dir targets, and nothing else; there is no ~/.agents/skills in it at
// all. codex scans $HOME/.agents/skills, the repo-relative .agents/skills and
// /etc/codex/skills. opencode is the permissive one and reads both, plus its
// own ~/.config/opencode/skills.
//
// ~/.agents/skills is the cross-vendor convention (GitHub Copilot CLI and
// Gemini CLI read it too), so a harness added later is more likely than not
// to be covered by an entry that already exists here. Claude Code is the
// holdout, which is why .claude/skills cannot simply be dropped in its
// favour.
//
// Writing a link into both roots is what makes this list additive: covering a
// new harness means appending its root, not choosing between the ones already
// here. The cost is one symlink per skill per root, which is nothing on disk.
//
// The cost that is NOT nothing, and is accepted rather than absent: opencode
// reads both roots, so every skill is presented to it twice. Claude Code and
// codex each read exactly one of the two and cannot see the duplication at
// all. Whether opencode merges the two entries or lists them separately is not
// established here — codex is documented to list same-named skills separately
// rather than merging, and opencode's docs say only that a nearer directory
// overrides a farther one, which is about project-vs-global precedence and not
// about two global roots. Presenting boid's four skills twice to one of three
// harnesses was judged cheaper than either dropping a root (which loses a
// harness outright) or making the link set depend on which harness a job will
// use (which resolveWorkspaceHome cannot know: the home is prepared per
// workspace, not per job).
var skillDiscoveryRoots = []string{
	filepath.Join(".claude", "skills"),
	filepath.Join(".agents", "skills"),
}

// workspaceHomeSkeletonDirs lists, relative to a workspace home, every
// directory that must exist before a job container starts: each skill
// discovery root and its parent.
//
// The ancestors are listed explicitly rather than left to `mkdir -p` to
// create as a side effect, because the condition being guarded against
// applies to them individually — the container engine auto-creates the WHOLE
// missing path as uid 0, not just its leaf, so any component of it can be the
// poisoned one. A root-owned ~/.claude locks the uid-1000 harness out of
// ~/.claude/.credentials.json and ~/.claude/projects/*.jsonl for good; under
// rootless podman the host-side owner is a subuid neither the harness nor the
// daemon can chown back (measured on podman 4.9.3, 論点 b-2).
//
// What is deliberately NOT here is a per-skill leaf. Those existed while the
// leaves were bind TARGETS and had to be present before the engine could
// mount onto them. They are symlinks now, and `mkdir -p` on one would be
// actively harmful: `ln -sfn` against an existing DIRECTORY nests the link
// inside it rather than replacing it, so the skill would land at
// .claude/skills/<name>/<name> and never be discovered.
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
// A drift between them is not cosmetic — a directory the prelude forgot is
// one the engine creates at uid 0 on the next launch.
//
// A swappable var rather than a plain function so a test can produce a
// DIFFERENT skeleton set, which is otherwise unconstructible: the real set is
// decided by skillDiscoveryRoots, a constant of the binary. Production never
// reassigns it.
var workspaceHomeSkeletonDirs = func() []string {
	dirs := make([]string, 0, len(skillDiscoveryRoots)*2)
	for _, root := range skillDiscoveryRoots {
		dirs = append(dirs, filepath.Dir(root), root)
	}
	return dirs
}

// skillLinks computes the full symlink set for a dispatch: boid's embedded
// skills first, then every loaded Pack's.
//
// # Embedded first, and why that decides collisions
//
// A Pack skill whose Name matches an embedded one is SKIPPED rather than
// appended. Both sides are symlinks into the same two directories now, so
// without the guard the winner would be decided by iteration order —
// buildWorkspaceInitScript's rm+ln pair for a repeated Name simply runs
// twice and the last one wins. The rule is that boid's own skills are not
// displaceable: an operator who installs a Pack cannot silently take over
// /boid-task.
//
// This was already the rule when embedded skills were bind mounts, and the
// failure it prevented then was worse in kind (the rm -rf of a bind TARGET,
// which the job's own preflight then reported as an engine-created uid-0
// directory — the wrong diagnosis, since that check has no vocabulary for
// "a Pack replaced this"). It is cheaper now: the loser is a symlink that
// never gets created.
//
// # name validation
//
// A single allowlist call (integrationpack.ValidSkillName), not a denylist
// assembled here — a denylist grew a new hole every review round it went
// through: first "/" (filepath.Base("/") == "/" survives a naive
// single-component check), then "." (filepath.IsLocal(".") is true), and
// finally a NUL byte (filepath.IsLocal("\x00") is true, filepath.Base is
// unchanged by it, and bash strips NUL from a script it reads on stdin —
// turning "rm -rf -- '.claude/skills/<NUL>'" into "rm -rf --
// '.claude/skills/'"). Each of those, left unrejected, wipes a discovery root
// wholesale. ParseManifest is the PRIMARY gate (a manifest failing it fails
// daemon startup outright); this call is defense in depth for a SkillLink
// built some other way, not a second independent design.
//
// Embedded names are not passed through ValidSkillName: they come from an
// embed.FS directory listing in boid's own source tree, not from operator
// input, and a name that could steer a path out of a discovery root would
// have to be committed to this repository to exist at all.
//
// A Pack skill failing either guard is skipped rather than failing the whole
// dispatch — one malformed Pack must not take every workspace's skill
// discovery down with it.
//
// # What is deliberately not filtered
//
// EVERY loaded Pack's EVERY skill is linked into EVERY workspace home,
// regardless of Skill.RequiresServiceProfile or whether the workspace has any
// service instance configured for that profile — this ignores
// docs/plans/signal-driven-review.md §6.4's stated design ("job には利用する
// skill だけを read-only mount する"). Widening it was a deliberate scope cut
// (a workspace-scoped filter needs to resolve which service instances a
// workspace configured, which resolveWorkspaceHome does not do), not an
// oversight — but it does mean a workspace with no Jira instance still gets a
// discoverable jira-api skill. Revisit alongside §6.4 if that noise matters.
//
// Two Packs declaring the same skill Name is not rejected either: both
// entries land in the returned slice, in packs' own order, and the last one
// wins when the rm+ln pair for that Name runs twice. The same applies to two
// VERSIONS of one Pack both being present under integrations.dir (LoadPacks
// enumerates every version directory), where the winner is whichever sorts
// last in os.ReadDir's lexicographic order — which is not necessarily the
// newest ("1.10.0" < "1.2.0" < "1.9.0"). v0 has no Pack-namespacing or
// version-pinning story to make either an error rather than a
// deployment-order footgun.
func skillLinks(imageSkillsDir string, packs []*integrationpack.Pack) []SkillLink {
	if imageSkillsDir == "" {
		imageSkillsDir = embeddedSkillsImageDir()
	}
	names := skills.EmbeddedSkillNames()
	embeddedSkillNames := make(map[string]bool, len(names))
	links := make([]SkillLink, 0, len(names))
	for _, name := range names {
		embeddedSkillNames[name] = true
		links = append(links, SkillLink{
			Name:   name,
			Target: filepath.Join(imageSkillsDir, name),
		})
	}
	for _, pack := range packs {
		for _, skill := range pack.Manifest.Skills {
			if !integrationpack.ValidSkillName(skill.Name) {
				continue
			}
			if embeddedSkillNames[skill.Name] {
				continue
			}
			links = append(links, SkillLink{
				Name:   skill.Name,
				Target: filepath.Join(pack.Dir, skill.Path),
			})
		}
	}
	return links
}

// skillLinkMarkerStrings renders links as an order-insensitive set of strings
// for workspaceHomeMarker.SkillLinks, comparable across runs the same way
// workspaceHomeMarker.SkeletonDirs is (equalStringSets).
//
// A changed set must force a re-init, on the same footing as a changed
// skeleton: the symlinks are written by the init container's prelude, which
// runs only when the marker is already stale, so nothing else would ever
// notice. Left unnoticed, a link would keep pointing at a Pack version
// directory the current image no longer has, or a newly-added embedded skill
// would simply stay undiscoverable.
//
// Name and Target are both in the string because either can move
// independently: a Pack version bump changes Target while Name stays put, and
// a renamed skill changes Name while Target's parent stays put.
func skillLinkMarkerStrings(links []SkillLink) []string {
	out := make([]string, 0, len(links))
	for _, link := range links {
		out = append(out, link.Name+"="+link.Target)
	}
	return out
}
