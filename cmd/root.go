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
// must not trigger EnsureRunningAt (e.g. start, stop, gc).
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
		// Host mode (PR-3 Option 4 redesign, docs/plans/
		// volume-only-daemon.md §論点c; unconditional since docs/plans/
		// release-onboarding.md 決定2/PR5 — BOID_MODE is gone, `boid`
		// itself always manages the compose daemon's lifecycle
		// (cmd/host.go) for scope=remote commands unless the caller
		// opted out with an explicit --profile (see
		// profileExplicitlyRequested's own doc comment for why that one
		// flag wins over host mode, Fable M4). scope=local
		// (start/stop/gc/...) and scope=neutral (login/logout) fall
		// through unchanged, since host.go has no opinion about compose
		// daemon lifecycle machinery itself or profile-less commands.
		// TAB-completion queries degrade silently (same posture as a
		// broken profile below) rather than potentially blocking a shell
		// TAB press on a multi-minute container build.
		if hostModeEnabled() && isRemoteScope(cmd) && !profileExplicitlyRequested(cmd) {
			if isCompletionQuery(cmd) {
				return nil
			}
			// context.Background(), not cmd.Context(): Cobra only ever
			// replaces a nil Command.Context() with context.Background()
			// inside Execute()/ExecuteC — a real `boid` invocation always
			// goes through one of those, but a unit test driving
			// PersistentPreRunE directly against a bare, non-Execute()'d
			// *cobra.Command (as several in cmd/root_test.go do) has a nil
			// one, which context.WithTimeout (probeHostMode) panics on.
			// Mirrors the sibling client.EnsureRunningAt call further
			// down this same function, which already uses
			// context.Background() for the identical reason.
			ctx := context.Background()
			var (
				c   *client.Client
				err error
			)
			if hasSkipAutostartAnnotation(cmd) {
				// codex round-1 review of PR5, Major 3: `boid gc` carries
				// annotationSkipAutostart=skip specifically so a bare
				// invocation does not spin up a daemon just to
				// immediately garbage-collect it (gc.go's own doc
				// comment) — a contract that predates host mode and
				// applies identically to it. resolveHostModeClient
				// unconditionally calls ensureHostModeDaemon, which
				// deploys the compose stack when unreachable; that must
				// not happen here.
				c, err = resolveHostModeClientNoAutostart(ctx)
			} else {
				c, err = resolveHostModeClient(ctx)
			}
			if err != nil {
				return err
			}
			cmd.SetContext(client.WithClient(cmd.Context(), c))
			return nil
		}
		// scope=local commands (start/stop/check/project migrate/...) are,
		// since 決定2/PR5, host-mode-adjacent local machinery — the SAME
		// "only an EXPLICIT --profile flag routes through
		// profiles.Resolve at all" rule Fable M4 established for
		// scope=remote above must also apply here (codex round-1 review
		// of PR5, Major 1). Before this fix, an AMBIENT default_profile
		// or BOID_PROFILE naming a remote https daemon — set for
		// unrelated scope=remote commands, long before host mode existed
		// — would resolve here too and hit the scope=local hard-reject
		// below (decision 6: "'%s' はローカル専用コマンドだよ"), meaning a
		// plain `boid start`/`stop` with NO --profile at all could no
		// longer bring the compose stack up/down. Skipping resolution
		// entirely here (rather than only skipping the reject check) is
		// deliberate: it also skips resolveClient's token-load/origin-
		// bind round trip against that same ambient remote profile,
		// which could independently fail (missing/corrupt token, network
		// blip) and block start/stop for a reason that has nothing to do
		// with them. RunE for every scope=local command below either
		// never touches client.FromContext at all, or (cmd/check.go) is
		// fine with its fallback: FromContext's own doc comment says an
		// uninjected context degrades to NewUnixClient(DefaultSocketPath())
		// — bit-for-bit the same client an unset profile would have
		// resolved to anyway. Every scope=local command annotates
		// annotationSkipAutostart=skip, so the autostart check just below
		// this branch's own `return nil` is never reached for them
		// either way.
		if isLocalScope(cmd) && !profileExplicitlyRequested(cmd) {
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
		if hasSkipAutostartAnnotation(cmd) {
			return nil
		}
		if !c.IsUnix() {
			return nil
		}
		return client.EnsureRunningAt(context.Background(), c.SocketPath())
	},
}

// hasSkipAutostartAnnotation walks cmd's ancestor chain looking for
// annotationSkipAutostart=skip — shared by both the bare-metal-profile
// autostart check at the bottom of PersistentPreRunE (above) and host
// mode's own branch (below): a command opting out of "launch a daemon
// just for this" must mean it regardless of which of the two autostart
// mechanisms (client.EnsureRunningAt for a bare-metal unix profile,
// resolveHostModeClient's ensureHostModeDaemon for host mode) would
// otherwise have fired (codex round-1 review of PR5, Major 3 — host
// mode's branch used to ignore this annotation entirely, so `boid gc`
// — annotated skip specifically so a bare invocation of it does not spin
// up a daemon just to immediately garbage-collect it — silently lost
// that contract the moment it was reclassified to scope=remote and
// started going through host mode by default).
func hasSkipAutostartAnnotation(cmd *cobra.Command) bool {
	for anc := cmd; anc != nil; anc = anc.Parent() {
		if anc.Annotations[annotationSkipAutostart] == "skip" {
			return true
		}
	}
	return false
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

// isRemoteScope reports whether cmd is annotated boid.scope=remote — the
// commands host mode actually needs to intercept (they talk to the
// daemon's HTTP API). scope=local (start/stop/gc/...) and scope=neutral
// (login/logout) commands fall through to the ordinary
// profiles.Resolve-based path unchanged: they either are bare-metal daemon
// lifecycle machinery host mode has no opinion about, or need no daemon
// connection at all.
//
// Lives here rather than in host.go (where it was defined until the
// GOOS=windows client split) because PersistentPreRunE consults it on
// every platform, while host.go — which manages a LOCAL containerized
// daemon — is Linux-only.
func isRemoteScope(cmd *cobra.Command) bool {
	return cmd.Annotations[scopeAnnotationKey] == scopeRemote
}

// profileExplicitlyRequested reports whether the invocation named an
// explicit --profile (docs/plans/release-onboarding.md「profiles との
// 優先順位が未定義」, Fable M4): host mode becoming the unconditional
// default for scope=remote commands (決定2/PR5) would otherwise make
// `--profile <https-profile>` and a genuine unix profile alike
// unreachable — cmd/root.go's host-mode branch used to run
// unconditionally ahead of profiles.Resolve for every scope=remote
// command. The agreed coexistence rule: an explicit --profile flag wins
// outright and routes through the ordinary profiles.Resolve chain
// instead, exactly like before host mode existed. BOID_PROFILE / a
// config.yaml default_profile do NOT trigger this bypass — those are
// ambient defaults a user may have set for unrelated (pre-compose)
// reasons, whereas typing --profile on THIS invocation is an
// unambiguous, one-shot statement of intent.
func profileExplicitlyRequested(cmd *cobra.Command) bool {
	f := cmd.Flags().Lookup(profiles.ProfileFlagName)
	return f != nil && f.Changed
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
