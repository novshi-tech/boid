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

var expectedScopeAnnotations = map[string]string{
	// remote — the command's actual work happens through the daemon's HTTP
	// API (today always the local UNIX socket).
	"boid action send":          scopeRemote,
	"boid agent claude":         scopeRemote,
	"boid agent codex":          scopeRemote,
	"boid agent opencode":       scopeRemote,
	"boid agent stop":           scopeRemote,
	"boid attach":               scopeRemote,
	"boid card requests":        scopeRemote,
	"boid card run":             scopeRemote,
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
	"boid project list":         scopeRemote,
	"boid project remove":       scopeRemote,
	"boid project set-perm":     scopeRemote,
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
	// docs/plans/signal-ingest-detailed-design.md §3.1 (PR-2): both are
	// ordinary HTTP API calls against the daemon (GET /api/signals, POST
	// /api/signals/ack) — same axis as `boid task list`/`boid action send`
	// above.
	"boid signal list":               scopeRemote,
	"boid signal ack":                scopeRemote,
	"boid task answer":               scopeRemote,
	"boid task artifacts":            scopeRemote,
	"boid task identity link":        scopeRemote,
	"boid task identity unlink":      scopeRemote,
	"boid task identity resolve":     scopeRemote,
	"boid task resolve-or-capture":   scopeRemote,
	"boid task create":               scopeRemote,
	"boid task delete":               scopeRemote,
	"boid task diagnose-cards":       scopeRemote,
	"boid task duplicate":            scopeRemote,
	"boid task hook list":            scopeRemote,
	"boid task hook replay":          scopeRemote,
	"boid task import":               scopeRemote,
	"boid task list":                 scopeRemote,
	"boid task notify":               scopeRemote,
	"boid task release-card-request": scopeRemote,
	"boid task reopen":               scopeRemote,
	"boid task rerun":                scopeRemote,
	"boid task show":                 scopeRemote,
	"boid task tree":                 scopeRemote,
	"boid task update":               scopeRemote,
	"boid task watch":                scopeRemote,
	// docs/plans/ingestion-identity.md PR-4 (B-5): fires a trigger through
	// POST /api/projects/{id}/triggers/{name}/run — same axis as `boid
	// exec`/`boid action send` above, the daemon's HTTP API is the entire
	// mechanism.
	"boid trigger run":           scopeRemote,
	"boid web devices":           scopeRemote,
	"boid web pair":              scopeRemote,
	"boid web revoke":            scopeRemote,
	"boid web revoke-all":        scopeRemote,
	"boid web set-addr":          scopeRemote,
	"boid web set-url":           scopeRemote,
	"boid workspace apply":       scopeRemote,
	"boid workspace assign":      scopeRemote,
	"boid workspace clear":       scopeRemote,
	"boid workspace create":      scopeRemote,
	"boid workspace edit":        scopeRemote,
	"boid workspace export":      scopeRemote,
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
	"boid check":    scopeLocal,
	"boid checkout": scopeLocal,
	"boid fetch":    scopeLocal,
	"boid init":     scopeLocal,
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
