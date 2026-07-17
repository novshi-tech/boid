package cmd

import (
	"context"
	"fmt"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/profiles"
	"github.com/spf13/cobra"
)

// annotationSkipAutostart is the cobra annotation key used to opt a command
// out of automatic server startup. Set the value to "skip" on commands that
// must not trigger EnsureRunning (e.g. start, stop, gc).
const annotationSkipAutostart = "boid.autostart"

// scopeAnnotationKey is the cobra annotation key every leaf command must
// carry (docs/plans/workspace-db-consolidation.md decision 18, Phase 3 CLI
// リモート pre-requisite): which of scopeRemote/scopeLocal/scopeNeutral it
// belongs to. This is a distinct concern from annotationSkipAutostart above
// — that one is about whether invoking the command should try to launch the
// daemon first; this one is about whether the command's own work happens
// through the daemon's HTTP API at all (relevant once Phase 3 lets the CLI
// target a *remote* daemon over the network instead of always the local
// UNIX socket). The two annotations use different keys and coexist without
// conflict; a command can be, and several are, "remote scope, but also
// annotationSkipAutostart=skip" (e.g. gc: it talks to the API but should not
// spin up a daemon just to immediately garbage-collect it).
//
// cmd/scope_annotations_test.go enforces that every leaf command sets this
// to one of the three values below — an unclassified command is a build
// failure (fail-closed), not silently defaulted.
const scopeAnnotationKey = "boid.scope"

const (
	// scopeRemote marks a command whose work happens through the daemon's
	// HTTP API (today always the local UNIX socket; Phase 3 makes this
	// potentially a remote daemon over the network).
	scopeRemote = "remote"
	// scopeLocal marks a command that completes entirely without a daemon
	// — it either never talks to one (e.g. `kit list`, which reads
	// ~/.local/share/boid/kits directly) or *is* daemon lifecycle machinery
	// itself (start/stop) rather than a client of it.
	scopeLocal = "local"
	// scopeNeutral marks a command that requires no profile resolution at
	// all (docs/plans/cli-remote-connection.md PR2: `login`/`logout` — see
	// cmd/login.go). `check` used to be cited here as the example (it works
	// standalone but also opportunistically talks to the daemon when one
	// happens to be reachable), but codex review round 2
	// (docs/plans/workspace-db-consolidation.md MAJOR 3) reclassified it to
	// scopeLocal to match the plan doc's classification table — see
	// cmd/check.go's annotation comment for the reasoning.
	scopeNeutral = "neutral"
)

var rootCmd = &cobra.Command{
	Use:   "boid",
	Short: "Personal AI orchestrator",
	// PersistentPreRunE is inherited by all subcommands. It does two things,
	// in order, every single invocation (docs/plans/cli-remote-connection.md
	// Phase 3 PR1):
	//
	//  1. Resolve which daemon this invocation targets (profiles.Resolve:
	//     --profile > BOID_PROFILE > default_profile > the pre-Phase-3 unix
	//     socket default) and inject the resulting *client.Client into
	//     cmd's own context, so every runXxx(cmd, args) below can fetch it
	//     via client.FromContext(cmd.Context()) instead of constructing its
	//     own client.NewUnixClient(client.DefaultSocketPath()) directly.
	//     Completion paths are treated specially:
	//       - `boid completion bash|zsh|fish|powershell` (script generation)
	//         is genuinely neutral — no daemon, no profile, no token needed
	//         — and bails BEFORE profile resolution so a broken profile file
	//         does not prevent the user re-installing shell completion.
	//       - `boid __complete ...` / `__completeNoDesc ...` (a live TAB
	//         query) attempts resolution but degrades silently on failure:
	//         a scary error in the user's shell is worse than "no
	//         candidates". Downstream completion callbacks use
	//         FromContextOrNil (which does NOT unix-fall-back) to detect
	//         the uninjected case and return no candidates rather than
	//         querying whichever daemon happens to be on the local socket.
	//     Known limitation (docs/plans/cli-remote-connection.md 未解決論点,
	//     PR1 round-3): a completion query with an EXPLICIT `--profile`
	//     flag in the args (e.g. `boid --profile work task <TAB>`) does not
	//     see that flag reflected here — Cobra's __complete parses its
	//     own args string after root PersistentPreRunE runs, so the flag
	//     is unset at resolution time and the query silently falls back
	//     to default_profile / unix. Deferred to a future PR (would
	//     require manually re-parsing __complete's args or resolving
	//     inside the completion callback).
	//  2. Ensure the boid server is running before any command that
	//     requires a socket connection — but ONLY for a unix-scheme
	//     resolution (decision 6: daemon autostart only ever makes sense
	//     for a daemon this same host can spawn; login/logout and an
	//     https-scheme profile never autostart anything). Commands (or any
	//     ancestor command) annotated with boid.autostart=skip bypass this
	//     regardless of scheme, same as before Phase 3.
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// Script generation (`boid completion bash|zsh|fish|powershell`)
		// is genuinely neutral — no daemon, no profile, no token needed —
		// and must not hard-fail on a broken profile file, or the user
		// cannot re-install their shell completion. Bail out BEFORE
		// touching profile resolution.
		if isCompletionScriptGen(cmd) {
			return nil
		}
		// Two-phase resolution (docs/plans/cli-remote-connection.md
		// decision 6, PR4 codex review round 1): resolve profile
		// identity (name / URL / scheme) FIRST — deliberately without
		// loading the Bearer token — so a scope=local rejection can fire
		// even when the resolved https profile has a missing or
		// corrupted token file. Loading the token first would surface a
		// misleading "run 'boid login' first" error instead of "this
		// command is local-only", and the caller would waste time
		// re-logging in on a command that will never accept the token
		// anyway.
		rp, err := profiles.ResolveWithoutToken(cmd)
		if err != nil {
			// TAB-completion queries (`__complete` / `__completeNoDesc`)
			// must degrade gracefully on a broken profile — a scary
			// error in the user's shell is worse than "no candidates".
			// A downstream completeXxx callback pulls FromContext,
			// which unix-falls-back when no client was injected, so
			// the shell still gets a well-formed empty response.
			if isCompletionQuery(cmd) {
				return nil
			}
			// scope=neutral commands (docs/plans/cli-remote-connection.md
			// PR2: login/logout) must not be blocked by a profile
			// resolution failure — that is often exactly the situation
			// they exist to fix. See the old resolveClient/Resolve
			// comment for the full rationale; the same argument applies
			// to ResolveWithoutToken (a `boid login --profile
			// <brand-new-name>` invocation names a profile that, by
			// definition, is not in config.yaml yet).
			if isNeutralScope(cmd) {
				return nil
			}
			return err
		}
		// scope=local commands (docs/plans/cli-remote-connection.md decision
		// 6, PR4) complete entirely without a remote daemon — they either
		// never talk to one at all, or *are* daemon lifecycle machinery
		// itself (start/stop/gc/...). Running one against a resolved
		// https-scheme profile would silently operate on the wrong host (or
		// simply make no sense, e.g. `boid start` for a daemon that already
		// has to be running to have accepted the connection). Fail hard
		// rather than fail-open, per decision 6.
		if isLocalScope(cmd) && !rp.IsUnix() {
			return fmt.Errorf(
				"'%s' はローカル専用コマンドだよ。\n"+
					"現在の接続先: %s (profile: %s)\n"+
					"ローカル操作するときは --profile <local-profile> を指定してね。",
				cmd.CommandPath(), rp.URL, rp.Name)
		}
		// Now that scope=local is out of the way, complete the resolution
		// (this loads the Bearer token for an https profile and runs the
		// origin-bind check; unix falls through with Token==""). Any
		// error here belongs to the actual invocation, not to a
		// scope=local violation that would have preempted it, so the
		// same completion / neutral swallow branches apply.
		c, err := resolveClient(cmd)
		if err != nil {
			if isCompletionQuery(cmd) {
				return nil
			}
			if isNeutralScope(cmd) {
				return nil
			}
			return err
		}
		cmd.SetContext(client.WithClient(cmd.Context(), c))

		// TAB queries never autostart a daemon — the user hit TAB, they
		// did not opt in to spawning a background process.
		if isCompletionQuery(cmd) {
			return nil
		}
		for anc := cmd; anc != nil; anc = anc.Parent() {
			if anc.Annotations[annotationSkipAutostart] == "skip" {
				return nil
			}
		}
		if !c.IsUnix() {
			return nil
		}
		return client.EnsureRunningAt(context.Background(), c.SocketPath())
	},
}

// isNeutralScope reports whether cmd is annotated boid.scope=neutral
// (docs/plans/cli-remote-connection.md PR2: login/logout). Unlike
// isCompletionQuery/isCompletionScriptGen (completion.go), this does not
// walk the parent chain — the scope annotation is only ever set on the
// leaf command actually being invoked (cmd/scope_annotations_test.go
// enforces this for every leaf in the tree), and PersistentPreRunE always
// receives that leaf command directly as cmd.
func isNeutralScope(cmd *cobra.Command) bool {
	return cmd.Annotations[scopeAnnotationKey] == scopeNeutral
}

// isLocalScope reports whether cmd is annotated boid.scope=local
// (docs/plans/cli-remote-connection.md decision 6, PR4: commands that
// complete entirely without a remote daemon — daemon lifecycle machinery
// like start/stop/gc, or commands that read local state directly). Mirrors
// isNeutralScope above: only ever checked on the leaf command actually
// being invoked, which is what PersistentPreRunE receives as cmd.
func isLocalScope(cmd *cobra.Command) bool {
	return cmd.Annotations[scopeAnnotationKey] == scopeLocal
}

// resolveClient resolves cmd's connection profile (profiles.Resolve) and
// builds the *client.Client it names (client.NewClient). Split out from
// PersistentPreRunE's closure so it stays independently testable. As of
// PR4's two-phase resolution, PersistentPreRunE runs ResolveWithoutToken
// FIRST (for the scope=local rejection to fire even with a
// missing/corrupt token file) and only reaches for resolveClient once
// scope is out of the way — so this function is the "load the token and
// build the transport" second half.
func resolveClient(cmd *cobra.Command) (*client.Client, error) {
	rp, err := profiles.Resolve(cmd)
	if err != nil {
		return nil, err
	}
	c, err := client.NewClient(rp.URL, rp.Token)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func init() {
	rootCmd.PersistentFlags().StringP("output", "o", "plain", "Output format: plain, json, yaml")
	rootCmd.PersistentFlags().String(profiles.ProfileFlagName, "", "connection profile name (see ~/.config/boid/config.yaml); overrides BOID_PROFILE and default_profile")
}

func Execute() error {
	return rootCmd.Execute()
}
