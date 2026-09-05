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
// carry: which of scopeRemote/scopeLocal/scopeNeutral it belongs to.
// Distinct from annotationSkipAutostart above — that is about launching the
// daemon first; this is about whether the command's work happens through
// the daemon's HTTP API at all. A command can be both "remote scope" and
// annotationSkipAutostart=skip (e.g. gc: talks to the API but should not
// spin one up just to immediately garbage-collect it).
//
// cmd/scope_annotations_test.go enforces that every leaf command sets this
// to one of the three values below — an unclassified command is a build
// failure, not a silent default.
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
	// all (`login`/`logout` — see cmd/login.go). See cmd/check.go's
	// annotation comment for why `check` is scopeLocal, not scopeNeutral.
	scopeNeutral = "neutral"
)

var rootCmd = &cobra.Command{
	Use:   "boid",
	Short: "Personal AI orchestrator",
	// PersistentPreRunE is inherited by all subcommands and does two things,
	// in order, every single invocation:
	//
	//  1. Resolve which daemon this invocation targets (profiles.Resolve:
	//     --profile > BOID_PROFILE > default_profile > the unix socket
	//     default) and inject the resulting *client.Client into cmd's own
	//     context, so every runXxx(cmd, args) below can fetch it via
	//     client.FromContext(cmd.Context()) instead of constructing its
	//     own client directly. Completion paths are treated specially:
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
	//     Known limitation: a completion query with an EXPLICIT `--profile`
	//     flag in the args (e.g. `boid --profile work task <TAB>`) does not
	//     see that flag reflected here — Cobra's __complete parses its own
	//     args string after root PersistentPreRunE runs, so the query
	//     silently falls back to default_profile / unix.
	//  2. Ensure the boid server is running before any command that
	//     requires a socket connection — but ONLY for a unix-scheme
	//     resolution: daemon autostart only ever makes sense for a daemon
	//     this same host can spawn; login/logout and an https-scheme
	//     profile never autostart anything. Commands (or any ancestor
	//     command) annotated with boid.autostart=skip bypass this
	//     regardless of scheme.
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// Script generation needs no profile at all; bail out BEFORE
		// touching profile resolution (see the doc comment above).
		if isCompletionScriptGen(cmd) {
			return nil
		}
		// Host mode: `boid` itself always manages the compose daemon's
		// lifecycle (cmd/host.go) for scope=remote commands, unless the
		// caller opted out with an explicit --profile (see
		// profileExplicitlyRequested). scope=local (start/stop/gc/...) and
		// scope=neutral (login/logout) fall through unchanged, since
		// host.go has no opinion about compose daemon lifecycle machinery
		// itself or profile-less commands. TAB-completion queries degrade
		// silently rather than blocking a shell TAB press on a
		// multi-minute container build.
		if hostModeEnabled() && isRemoteScope(cmd) && !profileExplicitlyRequested(cmd) {
			if isCompletionQuery(cmd) {
				return nil
			}
			// context.Background(), not cmd.Context(): a unit test driving
			// PersistentPreRunE directly against a bare *cobra.Command has
			// a nil Context(), which context.WithTimeout (probeHostMode)
			// would panic on. Mirrors the EnsureRunningAt call below.
			ctx := context.Background()
			var (
				c   *client.Client
				err error
			)
			if hasSkipAutostartAnnotation(cmd) {
				// `boid gc` carries annotationSkipAutostart=skip so a bare
				// invocation does not spin up a daemon just to immediately
				// garbage-collect it. resolveHostModeClient unconditionally
				// calls ensureHostModeDaemon, which deploys the compose
				// stack when unreachable, so that path must be skipped here.
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
		// scope=local commands (start/stop/check/...): only an EXPLICIT
		// --profile flag routes through profiles.Resolve at all — an
		// ambient default_profile/BOID_PROFILE naming a remote https daemon
		// (set for unrelated scope=remote commands) must not block a plain
		// `boid start`/`stop` with no --profile. Skipping resolution
		// entirely here (not just the reject check below) also avoids
		// resolveClient's token-load/origin-bind round trip failing for a
		// reason unrelated to these commands. Every scope=local command
		// also carries annotationSkipAutostart=skip.
		if isLocalScope(cmd) && !profileExplicitlyRequested(cmd) {
			return nil
		}
		// Two-phase resolution: resolve profile identity (name/URL/scheme)
		// FIRST, deliberately without loading the Bearer token, so a
		// scope=local rejection can fire even when the resolved https
		// profile has a missing or corrupted token file — otherwise the
		// caller sees a misleading "run 'boid login' first" instead of
		// "this command is local-only".
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
			// scope=neutral commands (login/logout) must not be blocked by
			// a profile resolution failure — that is often exactly the
			// situation they exist to fix (e.g. `boid login --profile
			// <brand-new-name>` names a profile not yet in config.yaml).
			if isNeutralScope(cmd) {
				return nil
			}
			return err
		}
		// scope=local commands complete entirely without a remote daemon —
		// they either never talk to one, or *are* daemon lifecycle
		// machinery itself (start/stop/gc/...). Running one against a
		// resolved https-scheme profile would silently operate on the
		// wrong host, so fail hard rather than fail-open.
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
// autostart check at the bottom of PersistentPreRunE and host mode's own
// branch: a command opting out of "launch a daemon just for this" must
// mean it regardless of which of the two autostart mechanisms
// (client.EnsureRunningAt or resolveHostModeClient's ensureHostModeDaemon)
// would otherwise have fired.
func hasSkipAutostartAnnotation(cmd *cobra.Command) bool {
	for anc := cmd; anc != nil; anc = anc.Parent() {
		if anc.Annotations[annotationSkipAutostart] == "skip" {
			return true
		}
	}
	return false
}

// isNeutralScope reports whether cmd is annotated boid.scope=neutral
// (login/logout). Unlike isCompletionQuery/isCompletionScriptGen
// (completion.go), this does not walk the parent chain — the scope
// annotation is only ever set on the leaf command being invoked, which is
// what PersistentPreRunE receives as cmd.
func isNeutralScope(cmd *cobra.Command) bool {
	return cmd.Annotations[scopeAnnotationKey] == scopeNeutral
}

// isLocalScope reports whether cmd is annotated boid.scope=local: commands
// that complete entirely without a remote daemon (daemon lifecycle
// machinery like start/stop/gc, or commands that read local state
// directly). Mirrors isNeutralScope above.
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
// explicit --profile: host mode being the unconditional default for
// scope=remote commands would otherwise make `--profile <https-profile>`
// unreachable, so an explicit --profile flag wins outright and routes
// through the ordinary profiles.Resolve chain instead, exactly like before
// host mode existed. BOID_PROFILE / a config.yaml default_profile do NOT
// trigger this bypass — those are ambient defaults, whereas typing
// --profile on THIS invocation is an unambiguous, one-shot statement of
// intent.
func profileExplicitlyRequested(cmd *cobra.Command) bool {
	f := cmd.Flags().Lookup(profiles.ProfileFlagName)
	return f != nil && f.Changed
}

// resolveClient resolves cmd's connection profile (profiles.Resolve) and
// builds the *client.Client it names. Split out from PersistentPreRunE's
// closure so it stays independently testable — the "load the token and
// build the transport" second half of the two-phase resolution
// (PersistentPreRunE runs ResolveWithoutToken first).
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
