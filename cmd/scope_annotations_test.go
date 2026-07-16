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
//   - `gc`: pinned to scopeLocal per the plan doc's "daemon lifecycle
//     machinery" grouping (start/stop/gc/init), even though gc's own work
//     (POST /api/gc) is dispatched entirely through the daemon's HTTP API
//     and would function correctly against a remote daemon too.
//   - `check`: pinned to scopeLocal — its exec.LookPath/unshare probes
//     inspect the machine the CLI process runs on, which only coincides
//     with the daemon's host under today's UNIX-socket-only transport.
//   - `project add` / `project init` / `project reload`: pinned to
//     scopeLocal per the plan doc's "境界越えで壊れる" row — each resolves a
//     local filesystem path (or a project's stored WorkDir) against the
//     daemon's own host, which only works because there is no remote
//     daemon transport yet. Phase 6 is expected to move this to scopeRemote.
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
	"boid exec":                 scopeRemote,
	"boid host-commands list":   scopeRemote,
	"boid host-commands reload": scopeRemote,
	"boid job done":             scopeRemote,
	"boid job list":             scopeRemote,
	"boid job log":              scopeRemote,
	"boid job show":             scopeRemote,
	"boid job watch":            scopeRemote,
	"boid project behaviors":    scopeRemote,
	"boid project list":         scopeRemote,
	"boid project remove":       scopeRemote,
	"boid project show":         scopeRemote,
	"boid secret delete":        scopeRemote,
	"boid secret get":           scopeRemote,
	"boid secret list":          scopeRemote,
	"boid secret set":           scopeRemote,
	"boid task answer":          scopeRemote,
	"boid task artifacts":       scopeRemote,
	"boid task create":          scopeRemote,
	"boid task delete":          scopeRemote,
	"boid task duplicate":       scopeRemote,
	"boid task hook list":       scopeRemote,
	"boid task hook replay":     scopeRemote,
	"boid task import":          scopeRemote,
	"boid task list":            scopeRemote,
	"boid task notify":          scopeRemote,
	"boid task reopen":          scopeRemote,
	"boid task rerun":           scopeRemote,
	"boid task show":            scopeRemote,
	"boid task tree":            scopeRemote,
	"boid task update":          scopeRemote,
	"boid task watch":           scopeRemote,
	"boid web devices":          scopeRemote,
	"boid web pair":             scopeRemote,
	"boid web revoke":           scopeRemote,
	"boid web revoke-all":       scopeRemote,
	"boid workspace assign":     scopeRemote,
	"boid workspace clear":      scopeRemote,
	"boid workspace configure":  scopeRemote,
	"boid workspace create":     scopeRemote,
	"boid workspace edit":       scopeRemote,
	"boid workspace list":       scopeRemote,
	"boid workspace remove":     scopeRemote,
	"boid workspace show":       scopeRemote,

	// local — daemon lifecycle machinery itself, sandbox-launch plumbing,
	// commands that never talk to a daemon, or (see the doc comment above)
	// a deliberate judgment call reconciling mechanism against the plan doc.
	"boid check":              scopeLocal,
	"boid fetch":              scopeLocal,
	"boid gc":                 scopeLocal,
	"boid init":               scopeLocal,
	"boid kit init":           scopeLocal,
	"boid kit list":           scopeLocal,
	"boid kit remove":         scopeLocal,
	"boid project add":        scopeLocal,
	"boid project init":       scopeLocal,
	"boid project migrate":    scopeLocal,
	"boid project reload":     scopeLocal,
	"boid runner-inner":       scopeLocal,
	"boid runner-inner-child": scopeLocal,
	"boid runner-outer":       scopeLocal,
	"boid start":              scopeLocal,
	"boid stop":               scopeLocal,
	"boid web set-addr":       scopeLocal,
	"boid web set-url":        scopeLocal,

	// neutral — none shipped yet. Reserved for `login`/`logout`
	// (docs/plans/cli-remote-connection.md, not yet implemented).
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
