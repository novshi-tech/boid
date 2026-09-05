package dispatcher

import (
	"os"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/integrationpack"
	"github.com/novshi-tech/boid/internal/skills"
)

// This file owns the daemon side of how skills — boid's own embedded set
// (/boid-task, /boid-orchestrate, /boid-web, /boid-signal, /boid-metaproject)
// and every Integration Pack's — reach a job's sandbox: each skill is
// symlinked into the workspace home from image-baked content
// (embeddedSkillsImageDir for the embedded set, pack.Dir for Packs), so a
// bind source need not resolve on the host filesystem. See
// docs/plans/workspace-home-volume-persistence.md for how this mechanism
// replaced the earlier per-skill bind mounts.

// embeddedSkillsImageDir is where build/container/Dockerfile unpacks the
// binary's embedded skill set, and therefore what a SkillLink for an
// embedded skill points at — a path inside the CONTAINER, never a host path
// or mount source. The init container and job container both see it
// because both start from the image that baked it in.
//
// buildWorkspaceInitScript verifies each Target with `[ -d ]` before
// linking, but that only covers the init container's copy of this path —
// not a workspace with a container_image override, whose job container
// runs a different image and may have no /opt/boid/skills at all. An
// override image that wants boid's skills has to bake this path itself.
//
// defaultEmbeddedSkillsImageDir is the literal that must equal the
// destination of build/container/Dockerfile's `COPY internal/skills/data`;
// TestEmbeddedSkillsImageDir_IsWhereTheDockerfileCopiesThem reads the
// Dockerfile to check this, since nothing in Go can check it at compile
// time. embeddedSkillsImageDir (the func below) is the value actually used,
// a var so tests can point it at a directory that exists — production
// never reassigns it, and Runner.SkillsImageDir overrides it per Runner for
// tests in other packages.
const defaultEmbeddedSkillsImageDir = "/opt/boid/skills"

// embeddedSkillsImageDir resolves the value actually used: Runner.SkillsImageDir
// wins when set; otherwise BOID_TEST_IMAGE_SKILLS_DIR (an env var because the
// suites that need it span several packages, so an unexported var here
// isn't reachable from them), which testutil/homeenv points at a populated
// temp directory for tests; otherwise the real image path.
//
// Read per call rather than at package init so a TestMain that sets it
// after this package is initialized still takes effect.
func embeddedSkillsImageDir() string {
	if dir := os.Getenv("BOID_TEST_IMAGE_SKILLS_DIR"); dir != "" {
		return dir
	}
	return defaultEmbeddedSkillsImageDir
}

// skillDiscoveryRoots are the directories, relative to a workspace home,
// that every skill is symlinked into. Two roots because no single one is
// read by all three harnesses boid dispatches:
//
//	harness      ~/.claude/skills   ~/.agents/skills
//	-----------  -----------------  ----------------
//	Claude Code  yes                no
//	codex        no                 yes
//	opencode     yes                yes
//
// ~/.agents/skills is also the cross-vendor convention (GitHub Copilot CLI,
// Gemini CLI), so a harness added later is more likely to already be
// covered by an existing entry; Claude Code is the holdout, which is why
// .claude/skills can't simply be dropped in its favour.
//
// opencode reads both roots and so sees every skill twice — accepted rather
// than making the link set depend on which harness a job will use, since
// resolveWorkspaceHome prepares the home per workspace, not per job.
var skillDiscoveryRoots = []string{
	filepath.Join(".claude", "skills"),
	filepath.Join(".agents", "skills"),
}

// workspaceHomeSkeletonDirs lists, relative to a workspace home, every
// directory that must exist before a job container starts: each skill
// discovery root and its parent.
//
// Ancestors are listed explicitly rather than left to `mkdir -p` as a side
// effect: the container engine auto-creates the whole missing path as uid
// 0, not just its leaf, so any component can end up root-owned and lock the
// uid-1000 harness out of files under it (e.g. ~/.claude/.credentials.json)
// with no way to chown it back under rootless podman.
//
// Deliberately NOT here: a per-skill leaf. Those are symlinks now, not bind
// targets, and `mkdir -p` on one would be harmful — `ln -sfn` against an
// existing directory nests the link inside it instead of replacing it.
//
// One function, three consumers: the init container's prelude creates this
// set (buildWorkspaceInitScript); resolveWorkspaceHome records it in the
// completion marker so a changed set re-runs the prelude
// (workspaceHomeMarker.SkeletonDirs); BuildSandboxSpec puts it into
// sandbox.Spec.HomeSkeletonDirs, where the job container's runner verifies
// it before the harness starts. A drift between them is not cosmetic — a
// directory the prelude forgot is one the engine creates at uid 0.
//
// A swappable var rather than a plain function so a test can produce a
// different skeleton set; production never reassigns it.
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
// A Pack skill whose Name matches an embedded one is skipped rather than
// appended — boid's own skills are not displaceable, so an operator who
// installs a Pack cannot silently take over /boid-task.
//
// Name validation uses a single allowlist (integrationpack.ValidSkillName)
// rather than a denylist — a denylist is easy to under-specify (e.g. a bare
// "/", ".", or an embedded NUL byte each need separate handling to keep an
// unvalidated name from wiping a discovery root wholesale via the rm+ln
// pair). ParseManifest is the primary gate at daemon startup; this call is
// defense in depth for a SkillLink built some other way, so a rejected name
// is skipped rather than failing the whole dispatch. Embedded names are not
// validated — they come from boid's own embed.FS, not operator input.
//
// Every loaded Pack's every skill is linked into every workspace home,
// regardless of Skill.RequiresServiceProfile — a workspace with no Jira
// instance still gets a discoverable jira-api skill. That is a deliberate
// scope cut, not an oversight (see docs/plans/signal-driven-review.md
// §6.4): a workspace-scoped filter needs to resolve which service instances
// a workspace configured, which this does not do.
//
// Two Packs (or two versions of one Pack) declaring the same skill Name is
// not rejected: both entries land in the returned slice, and the last one
// wins when buildWorkspaceInitScript's rm+ln pair for that Name runs twice
// — a deployment-order footgun v0 accepts rather than solves.
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
