package main

import (
	"fmt"
	"os"

	"github.com/novshi-tech/boid/cmd"
	"github.com/novshi-tech/boid/internal/sandbox"
)

func main() {
	command := sandbox.CommandFromArgv0(os.Args[0])
	if shouldRunBoidBuiltinShim(command, os.Args) {
		resp, err := sandbox.RunBoidShim(os.Args[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if resp.Stdout != "" {
			os.Stdout.WriteString(resp.Stdout)
		}
		if resp.Stderr != "" {
			os.Stderr.WriteString(resp.Stderr)
		}
		os.Exit(resp.ExitCode)
	}
	if command != "boid" {
		// The "git" PATH-overlay shim (boid binary bind-mounted at
		// /usr/bin/git, /bin/git) is retired as of docs/plans/git-gateway-cutover.md
		// PR6/PR8: sandbox git is now always the real binary via the base
		// rbind of /usr, so no invocation ever reaches this entrypoint named
		// "git" any more — every non-boid command falls through to the
		// generic host-command shim below.
		shimMain()
		return
	}

	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// isReservedRunnerSubcommand reports whether argv[1] (when present) names
// one of the internal "boid runner-*" entrypoints (cmd/runner.go,
// cmd/runner_container.go) — always the real cmd.Execute() dispatch,
// never the builtin shim, regardless of BOID_BUILTIN_SHIM (PR9 fix, see
// shouldRunBoidBuiltinShim's own doc comment for why this distinction is
// required specifically for the container backend).
func isReservedRunnerSubcommand(argv []string) bool {
	if len(argv) < 2 {
		return false
	}
	switch argv[1] {
	case "runner-outer", "runner-inner", "runner-inner-child", "runner-container":
		return true
	}
	return false
}

// shouldRunBoidBuiltinShim reports whether this invocation of the "boid"
// binary (argv0's basename already resolved to "boid" — command) should
// route through the builtin shim (sandbox.RunBoidShim) instead of the
// normal cmd.Execute() CLI dispatch. BOID_BUILTIN_SHIM=1
// (sandbox_builder.go's spec.Env, set whenever spec.BuiltinPolicies
// carries a "boid" entry) is meant to redirect NESTED "boid <subcommand>"
// calls a hook script or agent makes FROM WITHIN an already-running
// sandbox (e.g. "boid task update --payload-patch") through the
// broker-backed shim, not to redirect the sandbox's own entrypoint
// process.
//
// For the userns backend this distinction was implicit: spec.Env (and so
// BOID_BUILTIN_SHIM) is only ever applied to the FINAL exec'd hook/agent
// process (runner_inner_child.go), never to the runner-outer/-inner/
// -inner-child chain that runs before it, so those internal entrypoints
// never observed the env var at all. The container backend has no such
// staging: `docker create`'s Config.Env applies spec.Env to the
// CONTAINER'S OWN PID1 from the start — i.e. to `boid runner-container`
// itself — so without this reserved-subcommand carve-out, a
// capabilities.docker-declaring job's own container entrypoint would be
// misrouted into "unsupported boid subcommand \"runner-container\""
// instead of ever running (found via docs/plans/
// phase6-cutover-followups.md's e2e-container job debugging trail — the
// container backend genuinely never dispatched a real capabilities.docker
// job successfully before this fix).
func shouldRunBoidBuiltinShim(command string, argv []string) bool {
	if isReservedRunnerSubcommand(argv) {
		return false
	}
	return command == "boid" && os.Getenv("BOID_BUILTIN_SHIM") != ""
}

func shimMain() {
	// broker TCP wire completion (docs/plans/phase6-cutover-followups.md
	// §⓪): a container-backend job never gets BOID_BROKER_SOCKET at all
	// (there is no host-visible UNIX socket path a sibling container could
	// reach it at reliably — see internal/dispatcher/container_backend.go's
	// withBrokerTLSEnv doc comment) — only BOID_BROKER_TLS_ADDR. Reject
	// only when BOTH are empty, mirroring sandbox.RunBoidShim's identical
	// gate; brokerclient.DialFromEnv (called via ShimExec below) is the
	// actual transport-selection decision point either way.
	if os.Getenv("BOID_BROKER_SOCKET") == "" && os.Getenv("BOID_BROKER_TLS_ADDR") == "" {
		fmt.Fprintf(os.Stderr, "boid shim: neither BOID_BROKER_SOCKET nor BOID_BROKER_TLS_ADDR is set\n")
		os.Exit(1)
	}

	// Resolve this invocation's declared short (host_commands.<name>) name
	// from argv[0]'s basename. Post 5a-3 (docs/plans/phase5-shim-and-task-
	// context.md, "5a: shim 固定ディレクトリ化" PR3), every shim's bind-mount
	// basename == its declared short name by construction, so the basename is
	// authoritative — the pre-5a-3 BOID_HOST_COMMAND_NAMES env-map lookup
	// that used to bridge the aliased-basename case is retired.
	command := sandbox.CommandFromArgv0(os.Args[0])

	// Shim-side fast path: reject obviously-doomed invocations locally so we
	// don't pay for a broker round trip. The broker remains the authority and
	// enforces the same rules again; this only saves a hop and produces the
	// identical "host_commands.<name>: rejected: <reason>" message.
	if msg, rejected := sandbox.EarlyRejectFromEnv(command, os.Args[1:]); rejected {
		fmt.Fprintln(os.Stderr, msg)
		os.Exit(1)
	}

	resp, err := sandbox.ShimExec(command, os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "boid shim: %v\n", err)
		os.Exit(1)
	}

	if resp.Stdout != "" {
		os.Stdout.WriteString(resp.Stdout)
	}
	if resp.Stderr != "" {
		os.Stderr.WriteString(resp.Stderr)
	}
	os.Exit(resp.ExitCode)
}
