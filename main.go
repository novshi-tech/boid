package main

import (
	"fmt"
	"os"

	"github.com/novshi-tech/boid/cmd"
	"github.com/novshi-tech/boid/internal/sandbox"
)

func main() {
	command := sandbox.CommandFromArgv0(os.Args[0])
	if shouldRunBoidBuiltinShim(command) {
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

func shouldRunBoidBuiltinShim(command string) bool {
	return command == "boid" && os.Getenv("BOID_BUILTIN_SHIM") != ""
}

func shimMain() {
	brokerSocket := os.Getenv("BOID_BROKER_SOCKET")
	if brokerSocket == "" {
		fmt.Fprintf(os.Stderr, "boid shim: BOID_BROKER_SOCKET not set\n")
		os.Exit(1)
	}

	// Resolve this invocation's declared short (host_commands.<name>) name
	// once and reuse it for both the local fast-path check below and the
	// broker request itself, so the two never disagree (5a-2:
	// docs/plans/phase5-shim-and-task-context.md — ResolveShimCommandName
	// prefers BOID_HOST_COMMAND_NAMES over the argv0 basename so a
	// host_commands.<name>.path alias whose file basename differs from name
	// still resolves to the declared name).
	command := sandbox.ResolveShimCommandName(os.Args[0])

	// Shim-side fast path: reject obviously-doomed invocations locally so we
	// don't pay for a broker round trip. The broker remains the authority and
	// enforces the same rules again; this only saves a hop and produces the
	// identical "host_commands.<name>: rejected: <reason>" message.
	if msg, rejected := sandbox.EarlyRejectFromEnv(command, os.Args[1:]); rejected {
		fmt.Fprintln(os.Stderr, msg)
		os.Exit(1)
	}

	resp, err := sandbox.ShimExec(brokerSocket, command, os.Args[1:])
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
