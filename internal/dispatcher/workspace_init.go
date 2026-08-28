package dispatcher

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/novshi-tech/boid/internal/selfuser"
)

// This file owns the BACKEND-INDEPENDENT half of PR5 of
// docs/plans/workspace-home-volume-persistence.md (論点 b-2 / 論点 c): turning
// a workspace's own init.sh, plus boid's builtin prep steps, into a single
// shell program plus the environment it runs under.
//
// Until PR5 the daemon process exec'd `/bin/bash <tmpfile>` itself, with HOME
// pointed at the workspace home directory. PR6 turns that directory into a
// docker named volume the daemon cannot write to, and cannot chdir into, so
// the execution has to move into a container that mounts it. The container is
// started by the daemon from an image the daemon chose, running a script the
// daemon assembled — 論点 c's A vs A' distinction: the trusted boundary is
// unchanged, only the process boundary moved. What DID change is stated in the
// contract docs (docs/plans/home-workspace-volume.md 「契約」,
// docs/{ja,en}/guide/workspace-home.md).
//
// # One container, not two (§D1)
//
// The prep work — the bind-target skeleton (論点 b-2 §1) — is not a second
// container run. It is a builtin prelude ahead of the user's script, so a
// workspace with an init.sh still costs exactly one container start. Prep is
// UNCONDITIONAL: a workspace with no init.sh at all still gets a run, because
// its ~/.claude has to exist before any job container starts or the ENGINE
// creates it as uid 0 (論点 b-2's measurement). Making the run conditional
// would leave Phase 4's pass-through class
// (docs/plans/home-workspace-volume.md:98) with nothing to prepare it.
//
// PR5 also had a builtin POSTLUDE here, stamping a home identity token into a
// file inside the home. PR6 removed it along with the file: the identity moved
// onto the home volume's own label, where one idempotent VolumeCreate both
// ensures the volume and reads it back (workspaceHomeMarker.HomeID). A write
// nothing can read is not a check.
//
// # What "hash した bytes をそのまま実行する" means here (§D2)
//
// PR #787 made the completion marker's sha256 describe the bytes that actually
// ran, by writing the hashed bytes to a private file and exec'ing that instead
// of re-opening init.sh by name. That property is preserved, but it is worth
// being precise about its subject: the hash is of the USER SCRIPT, not of the
// wrapper this file builds. The wrapper carries the user script through a
// quoted heredoc into a container-local file and runs `bash` on that file, so
// what the marker vouches for is still exactly what a shell interpreted —
// buildWorkspaceInitRequest refuses to emit anything it cannot carry byte for
// byte (see its trailing-newline handling, and the two errors it returns).

const (
	// workspaceInitHeredocPrefix leads the quoted-heredoc delimiter that
	// carries the user script. The random suffix (32 hex characters from
	// crypto/rand, minted per run) is the part that matters: a fixed
	// delimiter — or one derived from the slug or the script — could be
	// embedded by a script author as a line of its own, truncating the
	// payload at that point and running a PREFIX of what the marker's hash
	// vouches for.
	workspaceInitHeredocPrefix = "BOID_INIT_EOF_"

	// Exit codes for the stages boid itself owns. Deliberately in the
	// 90s: the range is not reserved by anything, but the point is not
	// exclusivity (a user script may legitimately return any of these) —
	// workspaceInitStageOf resolves that ambiguity from the marker line the
	// wrapper prints, and only falls back to these when there is no output
	// at all to read. See §D3.
	//
	// 93 is deliberately not reused: it belonged to the postlude PR5 added to
	// stamp an identity file inside the home, which PR6 removed along with the
	// file (the identity moved onto the volume's own label — see
	// workspaceHomeMarker.HomeID). A marker line or an exit code from a
	// straggling PR5-era container should not be re-interpreted as some new
	// stage's.
	workspaceInitPreludeExitCode     = 91
	workspaceInitScriptSetupExitCode = 92

	// Stage names, as they appear both in the wrapper's marker lines and in
	// the operator-facing error.
	workspaceInitStagePrelude     = "prelude"
	workspaceInitStageScriptSetup = "script-setup"
	workspaceInitStageUserScript  = "init.sh"
	workspaceInitStageUnknown     = "unknown"

	// workspaceInitStageMarker prefixes the one line the wrapper prints to
	// stderr when a stage fails: `boid-init: stage=<stage> exit=<code>`.
	workspaceInitStageMarker = "boid-init: stage="

	// workspaceInitOutputLimit caps how much of the run's combined output is
	// retained in memory. An init.sh downloading a toolchain can print
	// megabytes of progress, and none of it can be spooled anywhere: the
	// container and its logs are gone by the time the error is rendered.
	//
	// What is retained is the TAIL, and that is not a detail. Both things that
	// diagnose a failed init live at the very end of the stream — the
	// installer's own last words, and the wrapper's `boid-init: stage=`
	// marker, which is printed AFTER the user script exits. A head-retaining
	// bound keeps neither, so the error shows unrelated early progress output
	// and workspaceInitStageOf, finding no marker, falls back to the exit-code
	// table and names a stage that never ran.
	workspaceInitOutputLimit = 1 << 20
	// workspaceInitOutputTail is how much of it goes into the error,
	// matching the pre-PR5 host-exec route's own tail size exactly.
	workspaceInitOutputTail = 4096

	// workspaceInitTruncationPrefix leads the notice workspaceInitOutput
	// inserts whenever it dropped anything, and is a constant of its own so a
	// test can assert that dropping was ANNOUNCED. An unannounced truncation
	// is worse than a truncation: an operator reading "--- output tail ---"
	// with no notice reasonably concludes that what follows is where the run
	// began.
	workspaceInitTruncationPrefix = "[boid: omitted "
	workspaceInitTruncationNotice = workspaceInitTruncationPrefix +
		"%d earlier bytes of this run's output; the last %d bytes follow]\n"
)

// WorkspaceInitRequest is one workspace-home preparation run, fully resolved:
// which home to mount where, what environment to run under, and the complete
// shell program to feed the container's stdin.
//
// Exported, together with WorkspaceInitExecutor, for one reason: the executor
// is discovered by type assertion on Runner.Backend (see §D10 on
// workspaceInitExecutorFor), and a test in another package that wires its own
// backend.SandboxBackend into internal/server's Config.Backend has to be able
// to satisfy that interface. An unexported request type would make the
// interface unimplementable outside this package, which would turn "this fake
// backend cannot prepare workspace homes" from a compile error into a runtime
// one.
type WorkspaceInitRequest struct {
	// Slug is the normalized workspace slug, used for the container name,
	// its label, BOID_WORKSPACE_SLUG and every diagnostic.
	Slug string

	// HomeSource is what the executor mounts: as of PR6 the NAME of this
	// workspace's docker named volume (dockerres.WorkspaceHomeVolumeName), not
	// a path. Through PR5 it was an absolute host path — the homes/<slug>
	// directory, host-visible only because it derived from RuntimesDir =
	// BOID_RUNTIME_DIR, i.e. tmpfs, which is the persistence regression PR6
	// closed. workspaceInitHomeMount rejects an absolute value outright rather
	// than binding it, so the two eras cannot be confused silently.
	HomeSource string

	// HomeTarget is where HomeSource is mounted INSIDE the container, and
	// therefore the value of HOME and BOID_WORKSPACE_HOME. It is the same
	// path a job container sees its own $HOME at (hostHomeDir(), what
	// BuildSandboxSpec uses), which is the point: a toolchain init.sh installs
	// records absolute paths in wrapper scripts, .pth files, shebangs and
	// symlinks, so the path it installs under has to be the path the harness
	// later runs under. Before PR5 those two differed and every absolute
	// symlink an init.sh created dangled inside the sandbox.
	HomeTarget string

	// Env is the complete environment for the run — see buildWorkspaceInitEnv
	// for what is in it and, more to the point, what is deliberately not.
	Env map[string]string

	// Script is the assembled wrapper (builtin prelude + user script),
	// fed to `bash -s` on the container's stdin. Never a path: the daemon's
	// own filesystem is not visible to the sibling engine that has to start
	// the container (build/container/compose.yml's KNOWN GAP note), so a
	// temp file the daemon writes cannot be a bind source.
	Script string

	// SkeletonDirs are the bind-target directories the prelude creates,
	// relative to the home (workspaceHomeSkeletonDirs). Carried on the request
	// rather than recomputed by the executor so that the set the init actually
	// created and the set resolveWorkspaceHome records in the completion
	// marker are the same value, not two evaluations of one function.
	SkeletonDirs []string

	// HomeID is the identity the workspace home volume carries on its
	// dockerres.LabelWorkspaceHomeID label (論点 b). resolveWorkspaceHome
	// records the same value in the completion marker's home_id and compares
	// the two on every later dispatch.
	//
	// The executor needs it because it has to guarantee the volume exists
	// before creating a container that mounts it, and a volume that vanished
	// between resolveWorkspaceHome's check and this run must come back with
	// THIS identity — otherwise the marker written afterwards would describe
	// an incarnation that no longer exists and every subsequent dispatch would
	// re-init.
	HomeID string

	// SkillLinks are the skills to symlink into every entry of
	// skillDiscoveryRoots — boid's own embedded set and every loaded
	// Integration Pack's, assembled by skillLinks (skills_overlay.go).
	//
	// See that function's doc comment for how the set is built and what it
	// deliberately does not filter, skillDiscoveryRoots for why a link is
	// written into more than one directory, and SkillLink for why a symlink
	// is the whole mechanism.
	//
	// Carried on the request for the same reason as SkeletonDirs just
	// above, even though (like SkeletonDirs) no production executor reads
	// this field back off the request — buildWorkspaceInitScript consumes
	// it directly to assemble Script, and that is the only consumer today.
	// The value carrying it here anyway: resolveWorkspaceHome computes
	// skillLinks ONCE and passes that single value both into this struct
	// (to build the script) and into the completion marker it writes
	// afterward — so "what the script actually links" and "what the marker
	// vouches for" are provably the same evaluation, not two calls to
	// skillLinks that could in principle disagree if Runner.Packs were read
	// twice.
	SkillLinks []SkillLink
}

// SkillLink is one skill to make discoverable inside a workspace home, by
// symlink rather than bind mount.
//
// Both kinds of skill are delivered this way, and both for the same reason:
// Target is baked into the very image the init container (and every job
// container) runs from, so it already exists inside this container before the
// prelude does anything. Integration Packs are copied there by
// build/container/Dockerfile's `cp -r ... /opt/boid/integrations/`; boid's own
// embedded set is unpacked from the binary to embeddedSkillsImageDir by the
// same Dockerfile. A symlink is the entire mechanism — there is nothing to
// materialize and nothing to bind.
//
// Until embedded skills were baked out, they were the exception: their
// content was materialized fresh per installation into a host-visible
// directory and bind-mounted in, because a sibling job container had no other
// way to see it. See skills_overlay.go's header for what removing that
// exception bought.
type SkillLink struct {
	// Name becomes the symlink's basename under each of skillDiscoveryRoots,
	// joined with filepath.Join by buildWorkspaceInitScript — never
	// interpolated as a raw path. For a Pack skill it MUST match
	// integrationpack.ValidSkillName's allowlist: skillLinks
	// (skills_overlay.go), the only production constructor, checks exactly
	// that before a Skill's manifest-declared name ever reaches this struct,
	// so a manifest's skills[].name cannot steer the wrapper outside a
	// discovery root. (An earlier version of this filter was a denylist —
	// filepath.IsLocal + matching filepath.Base — replaced after a review
	// round found it kept missing individual unsafe names one at a time,
	// most recently a NUL byte bash strips from a script it reads on stdin;
	// see ValidSkillName's own doc comment.) A caller building a SkillLink
	// some other way (a test, a future constructor) inherits the same
	// obligation; this struct does not re-validate it.
	Name string
	// Target is the absolute, image-baked path the symlink points at —
	// /opt/boid/skills/<name> for an embedded skill, or e.g.
	// /opt/boid/integrations/jira-cloud/1.0.0/skills/jira-api for a Pack's.
	// Never a bind-mount source, since no such host path exists for either.
	Target string
}

// WorkspaceHomeVolumeRequest asks an executor to make sure one workspace's
// HOME volume exists, and to report the identity it carries.
//
// The two halves are one request, and one engine call, on purpose: VolumeCreate
// is idempotent and returns an ALREADY-EXISTING volume together with its own
// labels, so "ensure" and "read the identity" are the same operation. Splitting
// them into ensure-then-inspect would double the per-dispatch engine round
// trips on the hot path and open a window between them. See
// dockerres.LabelWorkspaceHomeID for the measurement this rests on.
type WorkspaceHomeVolumeRequest struct {
	// Slug is the normalized workspace slug; it becomes the value of the
	// dockerres.LabelWorkspaceHome label on a newly created volume.
	Slug string

	// Name is the volume name, computed by the caller
	// (dockerres.WorkspaceHomeVolumeName). Executors must use it verbatim
	// rather than recomputing it: this same string becomes the job container's
	// HOME mount source, and two independent computations of a deterministic
	// name are two things that can disagree.
	Name string

	// CandidateID is the identity to stamp on the volume IF this call is the
	// one that creates it. For a volume that already exists it is discarded by
	// the engine, and the executor reports the incumbent identity instead —
	// which is exactly how a deleted-and-recreated volume becomes detectable.
	CandidateID string
}

// WorkspaceInitExecutor is the capability resolveWorkspaceHome needs from a
// sandbox backend: bring a workspace's HOME volume into existence, report its
// identity, and run one WorkspaceInitRequest to completion.
//
// Both methods live on one interface because after PR6 they are inseparable: a
// backend that cannot create a named volume cannot host a workspace home at
// all, and a backend that cannot start a container cannot prepare one. Two
// interfaces would let a backend satisfy half the contract and fail the other
// half at run time, which is precisely what the assertion in
// workspaceInitExecutorFor exists to turn into a startup-time answer.
//
// §D10 — why this is a separate, assertion-discovered interface rather than a
// method on backend.SandboxBackend:
//
//   - backend.SandboxBackend is the JOB lifecycle contract (Launch → session →
//     Wait/Stop/Signal → ReapOrphans). A workspace-home init has no job row, no
//     session, no transcript, no attach ingress and no place in ReapReport's
//     job accounting — adding it there would force every implementation,
//     including that package's own test doubles, to carry a method that means
//     nothing to them.
//   - The request type lives here, in internal/dispatcher, and
//     internal/dispatcher imports internal/sandbox/backend. Putting the method
//     on that interface would mean moving the request type down into it,
//     i.e. putting workspace-home vocabulary into the generic backend package.
//   - This package already discovers container-only capabilities by assertion
//     rather than by growing the interface: IsContainerBackend,
//     ContainerBackendUIDGID, ContainerBackendBrokerTLS,
//     ContainerBackendUsesDockerAPI. This is the same shape, and — because
//     both the interface and *containerBackend live in this package — needs no
//     exported helper at all.
type WorkspaceInitExecutor interface {
	// EnsureWorkspaceHomeVolume creates req.Name if it does not exist (with
	// req.CandidateID as its identity) and returns the identity the volume
	// carries afterwards. An empty return means the volume exists but was not
	// created by boid; the caller decides what to do about that.
	EnsureWorkspaceHomeVolume(ctx context.Context, req WorkspaceHomeVolumeRequest) (string, error)

	RunWorkspaceInit(ctx context.Context, req WorkspaceInitRequest) error
}

// workspaceInitExecutorFor resolves the backend's init capability, failing
// loud when it has none.
//
// Failing loud rather than degrading is the whole point. Every alternative is
// worse: running the script on the daemon instead is the thing PR5 removes (and
// stops working outright at PR6), and skipping it silently hands the agent a
// $HOME with no toolchain, whose symptom is the adapter's "CLI not found" —
// an error that points at an unconfigured init.sh rather than at an init that
// never ran. Production has exactly one backend and it implements this; a
// caller that sees this error has wired a DI/test backend and needs to say
// what such a backend should do about workspace homes.
func workspaceInitExecutorFor(r *Runner) (WorkspaceInitExecutor, error) {
	executor, ok := r.Backend.(WorkspaceInitExecutor)
	if !ok {
		return nil, fmt.Errorf(
			"the configured sandbox backend (%T) cannot run a workspace home init container; "+
				"workspace home preparation (bind-target skeleton, home identity, init.sh) runs in a throwaway "+
				"container as of PR5 of docs/plans/workspace-home-volume-persistence.md and has no host-side fallback",
			r.Backend)
	}
	return executor, nil
}

// workspaceInitParams is buildWorkspaceInitRequest's input: everything about
// one run that the caller knows and the wrapper needs.
type workspaceInitParams struct {
	Slug       string
	HomeSource string
	HomeTarget string
	// SkeletonDirs are the directories the prelude creates, RELATIVE to the
	// workspace home — workspaceHomeSkeletonDirs().
	SkeletonDirs []string
	// Script is the workspace's init.sh, exactly as read (and hashed) by
	// resolveWorkspaceHome under the lock. Ignored when ScriptExists is false.
	Script       []byte
	ScriptExists bool
	// HomeID is the identity the home volume carries, as reported by
	// EnsureWorkspaceHomeVolume.
	HomeID string

	// SkillLinks mirrors WorkspaceInitRequest.SkillLinks — see there.
	SkillLinks []SkillLink

	// heredocDelimiter overrides the randomly minted delimiter. Test-only:
	// the collision refusal below is unreachable at 2^-128 odds, and a
	// refusal nothing can trigger is a refusal nothing can verify.
	heredocDelimiter string
}

// buildWorkspaceInitRequest assembles the wrapper script and the environment
// for one workspace-home preparation run.
func buildWorkspaceInitRequest(p workspaceInitParams) (WorkspaceInitRequest, error) {
	if p.HomeID == "" {
		return WorkspaceInitRequest{}, fmt.Errorf("workspace home %q: init request has no home volume identity", p.Slug)
	}
	if p.HomeTarget == "" {
		return WorkspaceInitRequest{}, fmt.Errorf("workspace home %q: init request has no container-side home path", p.Slug)
	}

	script := p.Script
	if !p.ScriptExists {
		script = nil
	}
	if bytes.IndexByte(script, 0) >= 0 {
		return WorkspaceInitRequest{}, fmt.Errorf(
			"workspace home %q: init.sh contains a NUL byte and cannot be delivered to the init container verbatim; "+
				"a shell script must be text", p.Slug)
	}

	delim := p.heredocDelimiter
	if delim == "" {
		minted, err := newWorkspaceInitHeredocDelimiter()
		if err != nil {
			return WorkspaceInitRequest{}, fmt.Errorf("workspace home %q: mint heredoc delimiter: %w", p.Slug, err)
		}
		delim = minted
	}
	if containsLine(script, delim) {
		// Unreachable for a minted delimiter (32 hex characters of
		// crypto/rand). Refused rather than assumed away because the failure
		// it prevents is silent: the heredoc would end early and the run would
		// execute a prefix of the script the marker's hash describes.
		return WorkspaceInitRequest{}, fmt.Errorf(
			"workspace home %q: init.sh contains the generated heredoc delimiter %q as a line of its own", p.Slug, delim)
	}

	wrapper, err := buildWorkspaceInitScript(p, script, delim)
	if err != nil {
		return WorkspaceInitRequest{}, err
	}
	return WorkspaceInitRequest{
		Slug:         p.Slug,
		HomeSource:   p.HomeSource,
		HomeTarget:   p.HomeTarget,
		Env:          buildWorkspaceInitEnv(p.Slug, p.HomeTarget),
		Script:       wrapper,
		SkeletonDirs: p.SkeletonDirs,
		HomeID:       p.HomeID,
		SkillLinks:   p.SkillLinks,
	}, nil
}

// buildWorkspaceInitScript writes the wrapper text.
//
// The whole program is deliberately POSIX-plain and free of `set -e`: every
// stage checks its own status explicitly, because §D3 requires the caller to
// be able to tell WHICH stage failed, and `set -e` would collapse all of them
// into one anonymous non-zero exit.
func buildWorkspaceInitScript(p workspaceInitParams, script []byte, delim string) (string, error) {
	var b strings.Builder

	b.WriteString("# boid workspace home init wrapper — generated by internal/dispatcher/workspace_init.go\n")
	b.WriteString("# (docs/plans/workspace-home-volume-persistence.md 論点 b-2 / 論点 c, PR5).\n")
	b.WriteString("# Fed to `bash -s` on the init container's stdin; never written to a file by the daemon.\n")

	// Arbitrary-uid self-registration (docs/plans/release-onboarding.md
	// 決定1, PR2): this container's ENTRYPOINT is overridden to
	// `/bin/bash -s` (internal/dispatcher/container_backend_workspace_init.go),
	// so it never runs boid's own Go entrypoints and therefore never
	// reaches internal/selfuser.EnsureRuntimeUserRegistered — the "届かな
	// い第 3 の consumer" the plan doc calls out. Goes ahead of everything
	// else, including __boid_stage below, since the toolchain installers
	// this wrapper runs (go/volta/claude/codex/opencode) commonly do an
	// id/ssh/git passwd lookup themselves.
	b.WriteString("\n# --- prelude: arbitrary-uid /etc/passwd self-registration ---\n")
	b.WriteString(selfuser.PasswdSelfRegisterShellSnippet)

	// The LEADING newline is not cosmetic: workspaceInitStageOf only reads a
	// marker that begins a line, and the preceding write in the merged stream
	// is the user script's, which is free to end mid-line (`printf 'installing…'`,
	// a progress bar, `echo -n`). Without it, an ordinary script like that
	// glues its last partial line to the marker and its own failure gets
	// classified `unknown` instead of `init.sh`. Opening a line first costs one
	// blank line in the common case.
	b.WriteString("__boid_stage() { printf '\\n" + workspaceInitStageMarker + "%s exit=%s\\n' \"$1\" \"$2\" >&2; }\n")

	// ---- prelude ----------------------------------------------------------
	//
	// cd into the home first so every path below is relative to it: the home
	// path can be anything (a host path today, a volume mount point after
	// PR6), and not spelling it out repeatedly keeps the quoting surface at
	// exactly one value.
	b.WriteString("\n# --- prelude: enter the workspace home ---\n")
	b.WriteString("__boid_home=$BOID_WORKSPACE_HOME\n")
	b.WriteString("cd -- \"$__boid_home\"\n")
	b.WriteString(workspaceInitStageCheck(workspaceInitStagePrelude, workspaceInitPreludeExitCode))

	if len(p.SkeletonDirs) > 0 {
		// Before anything traverses these paths: refuse if any of them is a
		// SYMLINK (Opus security review of this PR, finding 2).
		//
		// The workspace home is mounted read-write into every job of the
		// workspace and persists across them, so a job can replace `.claude`
		// (or any other skeleton entry) with a symlink pointing outside the
		// home. `mkdir -p` follows it, and the `rm -rf` below then deletes
		// <elsewhere>/skills/<name> recursively before planting boid's link
		// there — demonstrated against a real shell, not merely reasoned about.
		//
		// The escape is bounded (the init container mounts ONLY the home: no
		// docker socket, no daemon state volume, so the reachable victims are
		// ephemeral image paths of the shape <attacker-chosen>/skills/<name>)
		// which is why this is a check rather than a redesign. What made it
		// worth adding NOW is that image-baked skills changed its reach: a
		// workspace with no Integration Packs used to emit ZERO `rm -rf`, and
		// now emits one per embedded skill per root, with `.agents` a brand-new
		// traversable prefix.
		//
		// Refusing rather than repairing: `rm`-ing the link would be boid
		// deleting something in a home an operator may have arranged
		// deliberately, on a guess about intent. A stage-tagged exit names the
		// path and lets a human decide. It also gives the job container's own
		// preflight (verifyHomeSkeleton) one less way to be reached, since its
		// error text can only explain this state as "the engine created it as
		// uid 0", which is the wrong diagnosis.
		b.WriteString("\n# --- prelude: refuse a symlinked skeleton path ---\n")
		for _, dir := range p.SkeletonDirs {
			b.WriteString("if [ -L " + shellQuoteDir(dir) + " ]; then\n")
			b.WriteString("  printf 'boid: %s is a symlink; refusing to prepare this workspace home through it\\n' " +
				shellQuoteDir(dir) + " >&2\n")
			b.WriteString("  __boid_stage " + workspaceInitStagePrelude + " " +
				strconv.Itoa(workspaceInitPreludeExitCode) + "; exit " +
				strconv.Itoa(workspaceInitPreludeExitCode) + "\n")
			b.WriteString("fi\n")
		}

		// umask 022 rather than the container's default, so the skeleton comes
		// out 0755 — byte for byte what the daemon-side
		// skills.MkdirAllNoSymlink produced (unix.Mkdirat(..., 0o755)) for the
		// same directories before PR5. It is restored immediately: the user
		// script must observe the environment's own umask, not one boid picked
		// for its own mkdir.
		b.WriteString("\n# --- prelude: bind-target skeleton (論点 b-2) ---\n")
		b.WriteString("__boid_umask=$(umask)\n")
		b.WriteString("umask 022\n")
		b.WriteString("mkdir -p --")
		for _, dir := range p.SkeletonDirs {
			b.WriteString(" " + shellQuoteDir(dir))
		}
		b.WriteString("\n__boid_status=$?\n")
		b.WriteString("umask \"$__boid_umask\"\n")
		b.WriteString(workspaceInitStageCheckStatus(workspaceInitStagePrelude, workspaceInitPreludeExitCode))
	}

	if len(p.SkillLinks) > 0 {
		// Skills need no materialization step (unlike the mkdir -p above,
		// which stages directories the engine would otherwise create as uid
		// 0): Target is already present in this very container, baked into
		// its image — see SkillLink.
		//
		// One link per skill PER ROOT, because the three harnesses boid
		// dispatches do not share a discovery directory; see
		// skillDiscoveryRoots for the table.
		//
		// `rm -rf` before `ln -sfn` rather than relying on -f alone because
		// the thing being replaced may be a real directory, not a stale
		// symlink. Two ways that happens, and the second is why this stays
		// even now that boid never creates such a directory itself: a skill
		// hand-copied into .claude/skills/<name> from a since-superseded
		// checkout (the state found in production before Pack symlinks
		// existed), and — for one migration only — the bind TARGET
		// directories the pre-image-baking builds created for embedded
		// skills, which survive in every workspace home this release
		// upgrades. `ln -sfn` alone would nest the symlink INSIDE such a
		// directory, leaving the skill at <root>/<name>/<name> where nothing
		// discovers it.
		//
		// Each Target is checked for existence FIRST, and a miss fails the whole
		// run (codex/Opus review of this PR, finding 1). `ln -s` succeeds against
		// a target that does not exist, so without this a runner image lacking
		// /opt/boid/skills produces a home full of dangling links, an init that
		// exits 0, and a completion marker written over it — after which nothing
		// re-runs: the script hash, the generation, the home identity, the
		// skeleton and this very link set all still match, so the home stays
		// broken even after the image is fixed, until an operator deletes the
		// marker by hand. That is the silent-and-permanent failure the old
		// bind-mount route could not have: it sourced the CONTENT from the same
		// binary that computed the NAMES, so the two could not disagree.
		//
		// The realistic trigger is not a corrupt image but ordinary skew: the
		// name list comes from the DAEMON binary's embed.FS while Target names a
		// path in the IMAGE, and a bare `boid start` from a fresh build against a
		// stale local boid-runner:latest has exactly that shape.
		//
		// Failing the run rather than skipping the link, for both kinds of skill:
		// a skipped link would reproduce the same silent-permanent outcome
		// (skillLinks runs daemon-side and cannot see the image, so the marker
		// would still record a link that was never made), and a Target missing
		// from the image the init container itself runs from is an image/binary
		// disagreement rather than one malformed Pack.
		b.WriteString("\n# --- prelude: skill symlinks (embedded + Integration Pack) ---\n")
		for _, link := range p.SkillLinks {
			b.WriteString("if [ ! -d " + shellQuoteDir(link.Target) + " ]; then\n")
			b.WriteString("  printf 'boid: skill %s is missing from this image at %s; the runner image and the boid binary disagree\\n' " +
				shellQuoteDir(link.Name) + " " + shellQuoteDir(link.Target) + " >&2\n")
			b.WriteString("  __boid_stage " + workspaceInitStagePrelude + " " +
				strconv.Itoa(workspaceInitPreludeExitCode) + "; exit " +
				strconv.Itoa(workspaceInitPreludeExitCode) + "\n")
			b.WriteString("fi\n")
			for _, root := range skillDiscoveryRoots {
				dest := filepath.Join(root, link.Name)
				b.WriteString("rm -rf -- " + shellQuoteDir(dest) + "\n")
				b.WriteString(workspaceInitStageCheck(workspaceInitStagePrelude, workspaceInitPreludeExitCode))
				b.WriteString("ln -sfn -- " + shellQuoteDir(link.Target) + " " + shellQuoteDir(dest) + "\n")
				b.WriteString(workspaceInitStageCheck(workspaceInitStagePrelude, workspaceInitPreludeExitCode))
			}
		}
	}

	// ---- user script ------------------------------------------------------
	if p.ScriptExists {
		tmpPath := "/tmp/boid-workspace-init-" + strings.TrimPrefix(delim, workspaceInitHeredocPrefix) + ".sh"
		b.WriteString("\n# --- the workspace's own init.sh, carried verbatim (§D2) ---\n")
		b.WriteString("__boid_script=" + shellQuoteDir(tmpPath) + "\n")
		b.WriteString("cat > \"$__boid_script\" <<'" + delim + "'\n")
		body, trimTrailingNewline := heredocBody(script)
		b.WriteString(body)
		b.WriteString(delim + "\n")
		b.WriteString(workspaceInitStageCheck(workspaceInitStageScriptSetup, workspaceInitScriptSetupExitCode))
		if trimTrailingNewline {
			// A heredoc always terminates its final line, so a script that did
			// NOT end in a newline would gain one — one byte of divergence
			// from what the completion marker's sha256 describes. `truncate`
			// is GNU coreutils, present in build/container/Dockerfile's debian
			// base.
			b.WriteString("truncate -s -1 -- \"$__boid_script\"\n")
			b.WriteString(workspaceInitStageCheck(workspaceInitStageScriptSetup, workspaceInitScriptSetupExitCode))
		}
		// </dev/null restores the pre-PR5 stdin (exec.Cmd with a nil Stdin) and
		// closes the trap that comes with feeding this wrapper to `bash -s`:
		// the wrapper IS bash's stdin, so a script that read stdin would eat
		// the rest of it — silently dropping the postlude below.
		b.WriteString("bash \"$__boid_script\" </dev/null\n")
		b.WriteString("__boid_status=$?\n")
		b.WriteString("rm -f -- \"$__boid_script\"\n")
		// The user script's own exit code is propagated verbatim rather than
		// mapped onto a boid code (§D3): an operator has to be able to tell
		// "my installer returned 4" from "boid's own prep broke".
		b.WriteString("if [ \"$__boid_status\" -ne 0 ]; then __boid_stage " + workspaceInitStageUserScript + " \"$__boid_status\"; exit \"$__boid_status\"; fi\n")
	}

	// No postlude. PR5 ended here by writing an identity token into a file
	// inside the home, which PR6 removed together with the file: the identity
	// now lives on the VOLUME's own label, where the daemon can read it with a
	// single VolumeCreate instead of needing a container (論点 b, and
	// workspaceHomeMarker.HomeID for what that trade does and does not cover).
	// Stamping it from in here as well would produce a second copy that
	// nothing ever compares — a value that looks like a check and is not one.
	b.WriteString("\nexit 0\n")

	return b.String(), nil
}

// workspaceInitStageCheck emits the "did the previous command succeed"
// guard: capture $? first, then compare, so the comparison itself cannot
// clobber the status being reported.
func workspaceInitStageCheck(stage string, code int) string {
	return "__boid_status=$?\n" + workspaceInitStageCheckStatus(stage, code)
}

// workspaceInitStageCheckStatus is workspaceInitStageCheck for a caller that
// already captured $? into __boid_status (because something else had to run in
// between).
func workspaceInitStageCheckStatus(stage string, code int) string {
	return "if [ \"$__boid_status\" -ne 0 ]; then __boid_stage " + stage + " \"$__boid_status\"; exit " + strconv.Itoa(code) + "; fi\n"
}

// heredocBody renders script as the body of a quoted heredoc, and reports
// whether the result gained a trailing newline the caller has to trim back.
//
// The mapping is exact in every case that matters:
//
//	""        -> ""      (no lines at all; cat writes an empty file)
//	"\n"      -> "\n"    (one empty line)
//	"a\n"     -> "a\n"
//	"a\n\n"   -> "a\n\n"
//	"a"       -> "a\n"   + trim, since a heredoc cannot express an unterminated
//	                       final line
func heredocBody(script []byte) (body string, trimTrailingNewline bool) {
	if len(script) == 0 {
		return "", false
	}
	s := string(script)
	if strings.HasSuffix(s, "\n") {
		return s, false
	}
	return s + "\n", true
}

// containsLine reports whether data contains line as a complete line — the
// condition that would terminate a quoted heredoc early.
func containsLine(data []byte, line string) bool {
	for _, candidate := range strings.Split(string(data), "\n") {
		if candidate == line {
			return true
		}
	}
	return false
}

// newWorkspaceInitHeredocDelimiter mints the per-run heredoc delimiter.
func newWorkspaceInitHeredocDelimiter() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return workspaceInitHeredocPrefix + hex.EncodeToString(buf), nil
}

// buildWorkspaceInitEnv builds the environment an init run sees: the three
// contractual variables, and nothing else (§D9).
//
// Before PR5 this also forwarded PATH / USER / LOGNAME / LANG / LC_ALL / TERM
// from the DAEMON PROCESS. That was coherent while init.sh ran on the host —
// the script really was going to shell out to host installers — and is
// incoherent now that it runs in a container:
//
//   - PATH named host directories the image does not have. It is dropped
//     rather than replaced with a boid-owned constant precisely so the ENGINE
//     applies the image's own PATH (build/container/Dockerfile inherits
//     debian's): a constant here would be a second copy of that value, free to
//     drift from the image it is supposed to describe.
//   - USER / LOGNAME named the account the daemon process runs under, which
//     has nothing to do with the uid the container runs as. As of PR2
//     (docs/plans/release-onboarding.md 決定1, arbitrary-uid) the image no
//     longer bakes a passwd entry for that uid at build time — this
//     wrapper's own prelude (buildWorkspaceInitScript's
//     PasswdSelfRegisterShellSnippet) self-registers one at runtime instead,
//     so anything that actually needs the identity can still ask the system
//     for it.
//   - LANG / LC_ALL named a locale the image need not have generated; a
//     missing one makes glibc and perl emit warnings on every command.
//   - TERM described a terminal that is not there — the init container is
//     created with no TTY — which is how a curses-style installer ends up
//     trying to draw into a pipe.
//
// A script that needs any of these can set them itself, which is both explicit
// and reproducible across deploys; inheriting them from whatever shell started
// the daemon was neither.
func buildWorkspaceInitEnv(slug, containerHome string) map[string]string {
	return map[string]string{
		"HOME":                containerHome,
		"BOID_WORKSPACE_SLUG": slug,
		"BOID_WORKSPACE_HOME": containerHome,
	}
}

// workspaceInitStageOf classifies a failed run.
//
// The marker line the wrapper prints wins over the exit code, and the LAST
// marker wins over earlier ones. That precedence is what makes the boid-side
// exit codes safe to pick at all: a user script is free to return 91, and does
// not thereby get reported as a prelude failure, because the wrapper stamped
// `stage=init.sh` on its way out. The code table is the fallback for the case
// with no output to read — an engine-side failure, or a container killed before
// it printed anything.
//
// # The marker is read only at the start of a line, and that is a heuristic
//
// A marker is only recognized at offset 0 or immediately after a newline,
// matching how the wrapper emits it (see __boid_stage in
// buildWorkspaceInitScript, which opens its own line for exactly this reason).
//
// This does not make user output and wrapper output distinguishable, and no
// rule applied to this string could: stdout and stderr are deliberately merged
// (§決定8's 「TTY/非 TTY とも単一結合」), the user script may print any bytes it
// likes including a perfectly-formed marker at the start of a line, and the
// wrapper has no channel the script cannot also write to. Anchoring only
// removes the cases where a marker was never even claimed to be one — a string
// quoted inside a log line, a fragment left by the retention bound discarding
// the front of an earlier line — which is what makes the difference in the
// case this is actually for: an OOM kill or SIGKILL between the user script and
// its `__boid_stage` call leaves the wrapper with NO marker of its own, and
// exit 137 should then read as "unknown", not as whatever stage the script's
// last progress line happened to mention.
func workspaceInitStageOf(output string, exitCode int) string {
	if idx := lastLineStartIndex(output, workspaceInitStageMarker); idx >= 0 {
		rest := output[idx+len(workspaceInitStageMarker):]
		if end := strings.IndexAny(rest, " \n"); end >= 0 {
			rest = rest[:end]
		}
		switch rest {
		case workspaceInitStagePrelude, workspaceInitStageScriptSetup,
			workspaceInitStageUserScript:
			return rest
		}
	}
	switch exitCode {
	case workspaceInitPreludeExitCode:
		return workspaceInitStagePrelude
	case workspaceInitScriptSetupExitCode:
		return workspaceInitStageScriptSetup
	}
	// No marker and no boid code: the wrapper never got to report, so nothing
	// here knows which stage was running. Deliberately NOT attributed to
	// init.sh — that is where a SIGKILL (exit 137, an OOM-killed toolchain
	// build) or an engine-side start failure would land, and blaming the
	// workspace's script for it sends the operator to the wrong file.
	return workspaceInitStageUnknown
}

// lastLineStartIndex is strings.LastIndex restricted to occurrences that begin
// a line, i.e. at offset 0 or just after a '\n'. It returns -1 when there is no
// such occurrence.
//
// It walks backwards from the last unanchored match rather than splitting the
// output into lines: this runs over up to workspaceInitOutputLimit bytes of
// captured output, and the answer is almost always the final line, so scanning
// from the end touches a few hundred bytes where a split would allocate a slice
// of every line in a megabyte.
func lastLineStartIndex(output, marker string) int {
	for idx := strings.LastIndex(output, marker); idx >= 0; idx = strings.LastIndex(output[:idx], marker) {
		if idx == 0 || output[idx-1] == '\n' {
			return idx
		}
	}
	return -1
}

// workspaceInitOutput is the bounded sink for one init run's combined
// stdout/stderr: an io.Writer that retains at most workspaceInitOutputLimit
// bytes, discarding from the FRONT, and remembers how much it discarded.
//
// It is a type rather than three lines inside the demux loop for two reasons.
// The container backend and the bash-backed test backend have to capture
// identically or the tests stop describing production. And the retention rule
// is the kind of thing that reads as correct while being backwards — the first
// version of this bound stopped appending once it was full, which is a cap on
// memory but keeps the beginning of a stream whose entire diagnostic value is
// at the end (see workspaceInitOutputLimit).
//
// Not safe for concurrent use. Both callers write from a single goroutine:
// the demux loop is alone on the attach stream, and os/exec runs one writer
// when Stdout and Stderr are the same comparable value.
type workspaceInitOutput struct {
	// limit is the retention target. buf is allowed to grow to twice it
	// between compactions so that a run printing megabytes does not memmove
	// the whole retained window once per frame; trim brings it back.
	limit   int
	buf     []byte
	dropped int
}

func newWorkspaceInitOutput() *workspaceInitOutput {
	return &workspaceInitOutput{limit: workspaceInitOutputLimit}
}

func (o *workspaceInitOutput) Write(p []byte) (int, error) {
	o.buf = append(o.buf, p...)
	if len(o.buf) > 2*o.limit {
		o.trim()
	}
	return len(p), nil
}

// trim drops the front of buf down to limit. Each compaction discards at least
// limit bytes, so the amortized cost per byte written stays constant however
// small the individual writes are.
func (o *workspaceInitOutput) trim() {
	over := len(o.buf) - o.limit
	if over <= 0 {
		return
	}
	o.dropped += over
	// copy semantics, not aliasing: append into the same array from a later
	// offset is a memmove, which is defined for overlapping ranges.
	o.buf = append(o.buf[:0], o.buf[over:]...)
}

// Len is how many bytes are currently retained, and Dropped how many were
// discarded to stay under the limit. Both exist so a successful run can be
// logged without materializing a megabyte of progress output into a string
// just to take its length.
func (o *workspaceInitOutput) Len() int {
	o.trim()
	return len(o.buf)
}

func (o *workspaceInitOutput) Dropped() int {
	o.trim()
	return o.dropped
}

// workspaceInitOutputStringCalls counts calls to workspaceInitOutput.String,
// across every instance. It exists purely so a test can observe WHETHER
// String was called at all — see
// TestContainerBackend_RunWorkspaceInit_SuccessDebugOffSkipsMaterializingFullOutput,
// which pins that RunWorkspaceInit's DEBUG line (container_backend_
// workspace_init.go) passes the output itself, not output.String(), to
// slog.Debug: a *workspaceInitOutput satisfies fmt.Stringer, so slog defers
// the String() call to whichever Handler.Handle actually renders the
// record — and Handle is never invoked for a level the logger has disabled.
// Calling .String() directly at the call site (the mistake this counts)
// defeats that: Go evaluates a function's arguments before the call, so
// output.String() runs and materializes the whole retained window every
// time, whether or not anything is listening at DEBUG. An atomic package
// counter, not a field, because RunWorkspaceInit's own *workspaceInitOutput
// is created and discarded inside runWorkspaceHomeContainer — nothing
// outside this file ever gets a handle on the instance to read a field off
// of.
var workspaceInitOutputStringCalls atomic.Int64

// String returns the retained window verbatim, with no truncation notice.
//
// This is what workspaceInitStageOf reads: it scans for a marker line the
// wrapper printed, and a boid-authored banner in that text would be one more
// thing it has to not mistake for output the container produced.
func (o *workspaceInitOutput) String() string {
	workspaceInitOutputStringCalls.Add(1)
	o.trim()
	return string(o.buf)
}

// Tail returns the last n retained bytes, prefixed by ONE notice accounting
// for everything that did not make it in — both what the bound discarded while
// the run was in flight and what this call is discarding now. One combined
// count rather than two nested ones because the reader does not care which of
// boid's internal stages dropped what; the question a truncation notice
// answers is "how much of this run am I not looking at".
func (o *workspaceInitOutput) Tail(n int) string {
	o.trim()
	tail := o.buf
	omitted := o.dropped
	if n >= 0 && len(tail) > n {
		omitted += len(tail) - n
		tail = tail[len(tail)-n:]
	}
	if omitted == 0 {
		return string(tail)
	}
	return fmt.Sprintf(workspaceInitTruncationNotice, omitted, len(tail)) + string(tail)
}

// workspaceInitFailure renders the operator-facing error for a non-zero init
// run: which workspace, which stage, which code, and the tail of what was
// printed. The tail matters more than it did before PR5 — neither the
// container nor its output survives the call, so this string is the only
// record of what happened.
//
// It takes the capture rather than a finished string so that the two bounds
// (the in-flight retention limit and this error's own tail) are applied by one
// piece of code that can account for both at once, and so that no caller can
// substitute an unbounded buffer and quietly stop exercising the real path.
func workspaceInitFailure(slug string, exitCode int, out *workspaceInitOutput) error {
	stage := workspaceInitStageOf(out.String(), exitCode)
	return fmt.Errorf(
		"init container for workspace %q failed at stage %q (exit %d)\n--- output tail ---\n%s",
		slug, stage, exitCode, out.Tail(workspaceInitOutputTail))
}
