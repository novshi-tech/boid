# Contributing

Contributions to `boid` are welcome — bug reports and feature suggestions in issues, patches in PRs, documentation fixes — all of them.

This page covers the external workflow and the coding conventions the project follows. The design background lives in [Concepts](../guide/concepts.md) and [State machine](../guide/state-machine.md); deeper internals will live in the (planned) architecture chapter.

## Development environment

You will need:

- Linux (`boid` is Linux-only).
- Go 1.24 or later.
- `git`.
- For E2E tests, having `bash`, `jq`, and `gh` around is convenient.

After cloning:

```bash
go test ./...        # unit tests
go test -race ./...  # race detector
go vet ./...         # static analysis
go build ./...       # build
```

`go install ./...` puts the in-progress binary into `$GOBIN` for local smoke-testing. Don't forget to restart the daemon (`boid stop && boid start`) after a reinstall — see [Troubleshooting](../guide/troubleshooting.md#a-bug-fix-i-just-installed-has-no-effect) for why.

## Coding conventions

Project-wide conventions — TDD, commit prefixes, keeping external dependencies minimal — live in `CLAUDE.md` at the repository root; we don't repeat general coding practice here.

The one boid-specific rule that matters most is **package layering**: don't break the dependency direction between orchestrator and sandbox / dispatcher. This boundary was broken once during a large refactor, so we now check it mechanically with `scripts/check-internal-architecture.sh` (run in CI) and `internal/client/architecture_test.go`.

Changes that span a wiring seam, or that claim something is "equivalent to" / "compatible with" an existing path, are exactly the class mechanisms can miss — run them through the `boid-review` skill's review lens before merging.

## E2E tests

The `e2e/scenarios/` directory contains black-box scenarios. Run all of them:

```bash
./e2e/run.sh
```

Run a specific one:

```bash
./e2e/run.sh project-smoke
```

If you are developing inside a `boid` sandbox, invoke `run-e2e [scenario]` (the declared short name) from within Claude Code — Phase 5 5a-3 cutover materializes `host_commands.run-e2e` as a `/run/boid/bin/run-e2e` symlink on PATH pointing at the boid shim, which dispatches the invocation to the host broker (the script actually runs on the host). Do NOT call `./e2e/run.sh` directly from inside the sandbox — that runs the checkout's script inline, which fails because a user namespace cannot spawn a nested user namespace.

When you add a feature, also add an E2E scenario as the regression guard. The scenario format is documented in the planned e2e guide.

## Sending a PR

1. **Branch.** Use `<topic>/<short-description>` (e.g. `fix/host-cmd-stdin`, `feat/web-ui-pty`).
2. **Tidy commits.** Aim for one logical change per commit. Squash fixup commits before sending.
3. **PR description.** Briefly cover what / why / how / how tested. Japanese or English is fine.
4. **CI is green.** `go test`, `go vet`, and (when relevant) the E2E suite should pass before sending.
5. **Take review.** Either accept the suggestion outright or counter with an alternative — both are fine.

Avoid history rewrites (`amend`, force push) on shared branches. To drop an intermediate commit, prefer a revert.

## Reporting a bug

Before opening an issue:

- Check [Troubleshooting](../guide/troubleshooting.md) for known patterns.
- Skim the daemon log (`~/.local/state/boid/boid.log`) so you can include relevant excerpts.
- Distil a reproduction.

Useful to include in the issue:

- The version of `boid` (e.g. the commit hash you installed).
- OS / distribution.
- Steps to reproduce.
- Expected vs actual behavior.
- The relevant excerpt from the daemon log.

## Feature requests

Open an issue to discuss the underlying need first. For larger features, aligning on the approach before you start coding saves rework on both sides.
