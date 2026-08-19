package cmd

import (
	"sort"
	"testing"

	"github.com/spf13/cobra"
)

// TestAllCommandsHaveScopeAnnotation pins decision 18
// (docs/plans/workspace-db-consolidation.md, Phase 3 CLI リモート
// pre-requisite): every leaf command must declare boid.scope as one of
// remote/local/neutral via cobra Annotations. Unclassified commands are a
// build failure (fail-closed), not a silent default — Phase 3's CLI-remote
// work depends on this classification being exhaustive and accurate from
// day one rather than discovered command-by-command later.
//
// cobra's own built-in "completion" and "help" commands are skipped: they
// are not ours to annotate, and (depending on whether Execute()/
// InitDefault*Cmd has run yet) may or may not even be present in the tree
// at test time.
func TestAllCommandsHaveScopeAnnotation(t *testing.T) {
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, c := range cmd.Commands() {
			if c.Name() == "completion" || c.Name() == "help" {
				continue
			}
			if len(c.Commands()) == 0 {
				v, ok := c.Annotations[scopeAnnotationKey]
				if !ok {
					t.Errorf("command %q has no %q annotation", c.CommandPath(), scopeAnnotationKey)
					continue
				}
				switch v {
				case scopeRemote, scopeLocal, scopeNeutral:
					// ok
				default:
					t.Errorf("command %q has invalid %q annotation %q", c.CommandPath(), scopeAnnotationKey, v)
				}
				continue
			}
			walk(c)
		}
	}
	walk(rootCmd)
}

// expectedScopeAnnotations pins MAJOR 6 (codex review round 1) / MAJOR 3
// (codex review round 2, docs/plans/workspace-db-consolidation.md): the
// scope classification of every leaf command in the tree, cross-checked
// against docs/plans/cli-remote-connection.md's "分類一覧 (全 leaf command)"
// table (Phase 3 CLI リモート pre-requisite). Unlike round 1's hand-picked
// subset, this table is exhaustive — TestScopeAnnotations_MatchExpectedTable
// below asserts the set of keys here equals the set of live leaf commands
// exactly, in both directions, so neither a missing entry nor a stray extra
// one can go unnoticed. Adding a brand-new leaf command therefore now DOES
// fail this test until its scope is added here — that is deliberate
// (fail-closed): round 1's "only listed commands are checked" contract let
// `host-commands reload` and a dozen others silently drift out of this
// table without anything catching it.
//
// A few entries deliberately diverge from what the mechanism alone would
// suggest, reconciled against the plan doc during codex review round 2:
//   - `gc` / `project reload`: RECLASSIFIED to scopeRemote by
//     docs/plans/release-onboarding.md 決定2/scope 再分類表 (PR5). Both were
//     scopeLocal ("daemon lifecycle machinery" / "境界越えで壊れる"
//     groupings) even though their actual work (POST /api/gc,
//     POST /api/projects/reload) was always dispatched entirely through
//     the daemon's HTTP API — under the bare-metal daemon this reached it
//     via an implicit channel (the runtime-dir bind happens to make the
//     same unix socket path resolve on both sides). Now that host mode
//     (cmd/host.go) is the CLI's unconditional default resolution path
//     for scope=remote commands, both go through the same explicit,
//     authenticated CLI listener as every other daemon API call instead
//     of relying on that bind-mount coincidence.
//   - `check`: pinned to scopeLocal — its exec.LookPath/unshare probes
//     inspect the machine the CLI process runs on, which only coincides
//     with the daemon's host under today's UNIX-socket-only transport.
//   - `project init`: RECLASSIFIED to scopeNeutral by docs/plans/
//     release-onboarding.md 穴 7/PR6 (codex round-21 review) — it used to
//     be scopeLocal for the identical "resolves [dir] against the
//     daemon's own host" reason as `project reload` above, back when it
//     also registered the scaffolded project with the daemon
//     (POST /api/projects, work_dir-based). That registration call is
//     gone: `project init` now only scaffolds [dir] locally (on whatever
//     host the CLI process itself runs on) and prints git-URL
//     registration guidance — it never calls client.FromContext or
//     resolves a profile at all. scopeLocal's contract is stricter than
//     that: root.go's PersistentPreRunE hard-rejects the command
//     outright whenever the active profile is a remote (https-scheme)
//     one, appropriate for genuine "this daemon, this host" commands
//     (start/stop/gc) but wrong for one that has no opinion on daemons or
//     profiles at all — a user on a remote profile could no longer reach
//     the wizard under scopeLocal. scopeNeutral (same as login/logout,
//     cmd/login.go) is the correct fit now.
//   - `project add`: RECLASSIFIED to scopeRemote by docs/plans/
//     volume-only-daemon.md §論点a — this is the exact cutover the comment
//     above used to predict ("Phase 6 is expected to move this to
//     scopeRemote"). `boid project add <git-url>` no longer resolves any
//     local filesystem path at all; the daemon clones the URL itself into a
//     bare repository it manages, so this is now an ordinary HTTP API
//     operation like `project show`/`project list`. `project fetch` (new in
//     the same PR — `boid project fetch <id>`, runs `git fetch --all`
//     inside the bare repo) is scopeRemote for the identical reason.
//
// See cmd/check.go, cmd/gc.go, and cmd/project.go's own annotation comments
// for the full reasoning behind each.
var expectedScopeAnnotations = map[string]string{
	// remote — the command's actual work happens through the daemon's HTTP
	// API (today always the local UNIX socket).
	"boid action send":          scopeRemote,
	"boid agent claude":         scopeRemote,
	"boid agent codex":          scopeRemote,
	"boid agent opencode":       scopeRemote,
	"boid agent stop":           scopeRemote,
	"boid attach":               scopeRemote,
	"boid config apply":         scopeRemote,
	"boid config edit":          scopeRemote,
	"boid config get":           scopeRemote,
	"boid config set":           scopeRemote,
	"boid config unset":         scopeRemote,
	"boid exec":                 scopeRemote,
	"boid gc":                   scopeRemote,
	"boid host-commands list":   scopeRemote,
	"boid host-commands reload": scopeRemote,
	"boid job done":             scopeRemote,
	"boid job list":             scopeRemote,
	"boid job log":              scopeRemote,
	"boid job show":             scopeRemote,
	"boid job watch":            scopeRemote,
	"boid project add":          scopeRemote,
	"boid project behaviors":    scopeRemote,
	"boid project fetch":        scopeRemote,
	"boid project list":         scopeRemote,
	"boid project reload":       scopeRemote,
	"boid project remove":       scopeRemote,
	"boid project show":         scopeRemote,
	"boid secret delete":        scopeRemote,
	"boid secret get":           scopeRemote,
	"boid secret list":          scopeRemote,
	"boid secret set":           scopeRemote,
	// docs/plans/api-gateway.md §7, PR3: the daemon HTTP API is the entire
	// mechanism (POST/GET .../api/oauth/login*) — the CLI's own local
	// loopback listener (RFC 8252 §7.3) is a CLI-process-local detail that
	// doesn't depend on daemon locality any more than `attach`'s local
	// terminal handling above does.
	"boid secret oauth login": scopeRemote,
	"boid task answer":        scopeRemote,
	"boid task artifacts":     scopeRemote,
	"boid task create":        scopeRemote,
	"boid task delete":        scopeRemote,
	"boid task duplicate":     scopeRemote,
	"boid task hook list":     scopeRemote,
	"boid task hook replay":   scopeRemote,
	"boid task import":        scopeRemote,
	"boid task list":          scopeRemote,
	"boid task notify":        scopeRemote,
	"boid task reopen":        scopeRemote,
	"boid task rerun":         scopeRemote,
	"boid task show":          scopeRemote,
	"boid task tree":          scopeRemote,
	"boid task update":        scopeRemote,
	"boid task watch":         scopeRemote,
	// docs/plans/ingestion-identity.md PR-4 (B-5): fires a trigger through
	// POST /api/projects/{id}/triggers/{name}/run — same axis as `boid
	// exec`/`boid action send` above, the daemon's HTTP API is the entire
	// mechanism.
	"boid trigger run":      scopeRemote,
	"boid web devices":      scopeRemote,
	"boid web pair":         scopeRemote,
	"boid web revoke":       scopeRemote,
	"boid web revoke-all":   scopeRemote,
	"boid web set-addr":     scopeRemote,
	"boid web set-url":      scopeRemote,
	"boid workspace apply":  scopeRemote,
	"boid workspace assign": scopeRemote,
	"boid workspace clear":  scopeRemote,
	"boid workspace create": scopeRemote,
	"boid workspace edit":   scopeRemote,
	"boid workspace export": scopeRemote,
	// import-home reads a LOCAL directory to build its payload and is still
	// scopeRemote, unlike `project reload`, which also reads a local path
	// (a project's stored WorkDir) and is scopeLocal. The axis is whose
	// filesystem the path has to be on: `project reload` hands the DAEMON
	// a path it opens itself (so it only works while CLI and daemon share
	// a host), while import-home opens it in this process and streams the
	// BYTES — which is exactly why PR8's payload is a tar stream rather
	// than a bind mount (docs/plans/workspace-home-volume-persistence.md
	// 論点 f). (`project init` used to be the scopeLocal comparison point
	// here too, but docs/plans/release-onboarding.md 穴 7/PR6 reclassified
	// it to scopeNeutral — see cmd/project.go's own annotation comment —
	// since it no longer hands the daemon anything at all.)
	"boid workspace import-home": scopeRemote,
	// The init-script quartet (PR9 of
	// docs/plans/workspace-home-volume-persistence.md, 論点 d). scopeRemote on
	// the same axis import-home above is: -f/-o name files on THIS machine and
	// are opened in this process, but the init.sh itself lives with the daemon
	// — in its own config root, which under the container deploy is inside its
	// state volume — so every one of these is an API call and none of them
	// works without a daemon.
	"boid workspace get-init-script":   scopeRemote,
	"boid workspace set-init-script":   scopeRemote,
	"boid workspace edit-init-script":  scopeRemote,
	"boid workspace unset-init-script": scopeRemote,
	"boid workspace list":              scopeRemote,
	"boid workspace remove":            scopeRemote,
	"boid workspace show":              scopeRemote,
	// docs/plans/api-gateway.md §論点5: the add/remove/list trio is a
	// GET-then-PUT round trip against the daemon's HTTP API, same axis as
	// every other workspace subcommand above.
	"boid workspace services add":    scopeRemote,
	"boid workspace services remove": scopeRemote,
	"boid workspace services list":   scopeRemote,

	// local — daemon lifecycle machinery itself, sandbox-launch plumbing,
	// commands that never talk to a daemon, or (see the doc comment above)
	// a deliberate judgment call reconciling mechanism against the plan doc.
	"boid check":           scopeLocal,
	"boid fetch":           scopeLocal,
	"boid init":            scopeLocal,
	"boid project migrate": scopeLocal,
	// workspace import is a retired, always-failing stub (2026-07-28) — it no
	// longer talks to a daemon at all, so it is scopeLocal like `boid init`
	// (also a deprecated stub pointing elsewhere), NOT scopeRemote. This also
	// carries annotationSkipAutostart=skip (see workspaceImportCmd's own
	// Annotations): without it, PersistentPreRunE would still autostart a
	// daemon before RunE ever gets a chance to print the deprecation
	// guidance — the whole reason a caller might still be typing this
	// command is that they are on old muscle memory or a stale script, which
	// includes the case of a machine with no daemon running yet.
	"boid workspace import": scopeLocal,
	"boid reap":             scopeLocal,
	"boid runner-container": scopeLocal,
	"boid start":            scopeLocal,
	"boid stop":             scopeLocal,
	// install-skills only writes SKILL.md files under ~/.claude/skills/ (or
	// --dir) from the embedded hostdata FS — it never talks to a daemon, same
	// axis as `boid check`/`boid fetch` above.
	"boid install-skills": scopeLocal,

	// neutral — requires no profile precondition at all (docs/plans/
	// cli-remote-connection.md PR2): these are how a profile comes to
	// exist (or stops existing) in the first place, so they must work even
	// when profile resolution itself would otherwise fail.
	"boid login":  scopeNeutral,
	"boid logout": scopeNeutral,
	// `project init` (docs/plans/release-onboarding.md 穴 7/PR6, codex
	// round-21 review): reclassified from scopeLocal — see cmd/project.go's
	// own annotation comment and this file's "境界越えで壊れる" section
	// above for the full reasoning. It never calls client.FromContext or
	// resolves a profile, so it must work regardless of which profile
	// (including a remote https one) happens to be active.
	"boid project init": scopeNeutral,
	// docs/plans/release-onboarding.md PR1 (codex review round 1 of this
	// PR): reads internal/version only (ldflags override or
	// debug.ReadBuildInfo()), never the daemon's HTTP API, and its result
	// must not depend on profile resolution having gone well. scopeLocal was
	// the first attempt, but scopeLocal is rejected by root's
	// PersistentPreRunE whenever an https profile is selected (isLocalScope,
	// decision 6 of cli-remote-connection.md) — "what version of the CLI am
	// I running" has nothing to do with which daemon --profile points at, so
	// it belongs in this group instead, same as login/logout above (whose
	// own PersistentPreRunE still resolves the profile on the happy path;
	// scopeNeutral only means that failing to do so is swallowed rather than
	// fatal — see cmd/version.go's own annotation comment).
	// annotationSkipAutostart=skip still applies.
	"boid version": scopeNeutral,
}

// liveLeafCommands walks rootCmd and returns every leaf command's full path
// (cobra's own "completion"/"help" excluded, same as TestAllCommandsHaveScopeAnnotation).
func liveLeafCommands(t *testing.T) map[string]string {
	t.Helper()
	actual := map[string]string{}
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, c := range cmd.Commands() {
			if c.Name() == "completion" || c.Name() == "help" {
				continue
			}
			if len(c.Commands()) == 0 {
				actual[c.CommandPath()] = c.Annotations[scopeAnnotationKey]
				continue
			}
			walk(c)
		}
	}
	walk(rootCmd)
	return actual
}

// TestScopeAnnotations_MatchExpectedTable asserts expectedScopeAnnotations
// and the live command tree's leaf set are identical — both the scope value
// for every shared entry, AND the set of command paths itself (MAJOR 3,
// codex review round 2, docs/plans/workspace-db-consolidation.md). Round 1's
// version of this test only checked entries already present in the table
// against the live tree, silently ignoring any live leaf command the table
// didn't happen to mention — that gap is exactly how `host-commands reload`
// (among others) went unpinned. Any of the three failure modes below now
// fails the build:
//  1. an entry in the table whose live scope differs (misclassification)
//  2. an entry in the table that no longer exists in the live tree (renamed/removed)
//  3. a live leaf command missing from the table entirely (new command, or a
//     stale table that fell behind)
func TestScopeAnnotations_MatchExpectedTable(t *testing.T) {
	actual := liveLeafCommands(t)

	var missingFromTree, extraInTree, mismatched []string

	for path, want := range expectedScopeAnnotations {
		got, ok := actual[path]
		if !ok {
			missingFromTree = append(missingFromTree, path)
			continue
		}
		if got != want {
			mismatched = append(mismatched, path+": got "+got+", want "+want)
		}
	}
	for path := range actual {
		if _, ok := expectedScopeAnnotations[path]; !ok {
			extraInTree = append(extraInTree, path)
		}
	}

	sort.Strings(missingFromTree)
	sort.Strings(extraInTree)
	sort.Strings(mismatched)

	for _, path := range missingFromTree {
		t.Errorf("expected command %q not found in the command tree (renamed or removed? update expectedScopeAnnotations)", path)
	}
	for _, path := range extraInTree {
		t.Errorf("command %q exists in the live tree but has no entry in expectedScopeAnnotations (new command? add its scope to the table)", path)
	}
	for _, msg := range mismatched {
		t.Errorf("command %s (see expectedScopeAnnotations table)", msg)
	}
}
