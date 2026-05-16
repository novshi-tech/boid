# boid

**A personal AI orchestrator (Linux only).** Built to keep the human from becoming the bottleneck when several AI coding agents run in parallel. Agents are given room to make progress autonomously; a sandbox with a configurable write scope keeps them from doing damage; and every in-flight task is visible at a glance through a Web UI and TUI.

[日本語 README](README.ja.md)

## Features

- **Give agents room to run on their own.** The task lifecycle — request → execute → verify → fix → done — and the data captured at each step are predefined, so you don't have to re-feed context on every revision cycle. Hand the agent your local Claude Code, Codex, git, gh, editors, and language toolchains; it uses the tools you already have.
- **Autonomy and safety, reconciled by the sandbox.** Agents read your real directories directly, but writes are confined to a scope you choose (typically a git worktree). A runaway agent can't reach your home directory or other projects. Give each task its own worktree and several requests run on separate branches in separate directories without colliding.
- **See every task at a glance.** Every task lives in a single list with its current state, viewable from CLI, TUI, or Web UI. Expose the Web UI through Cloudflare Tunnel and you can check or steer progress from your phone.
- **Stays on your own machine.** `go install`, then `boid start`. No config file, no server provisioning, no signup. Unlike cloud-side sandboxes, the agent can act on the real environment you actually work in.
- **Swappable extension packages.** Pick which AI agent (Claude Code, Codex), which CI integration, which PR / auto-merge flow — the building blocks live in separate packages such as [boid-kits](https://github.com/novshi-tech/boid-kits).

## Install

```bash
go install github.com/novshi-tech/boid@latest
```

## Quickstart

```bash
boid start              # start the daemon (auto-detached)
boid task list          # list tasks
boid task show <id>     # inspect a task
boid stop               # stop the daemon
```

A guided walkthrough lives in [docs/en/getting-started/01-install.md](docs/en/getting-started/01-install.md).

## Documentation

- **[Install and quickstart](docs/en/getting-started/01-install.md)**
- **[Concepts](docs/en/guide/concepts.md)** — vocabulary
- **[State machine](docs/en/guide/state-machine.md)** — including the `awaiting` state for C2 Q&A
- **[C2 flow](docs/en/architecture/c2-flow.md)** — non-interactive sessions + Q&A architecture
- **[Web UI](docs/en/guide/web-ui.md)** — including Cloudflare Tunnel setup
- **[Troubleshooting](docs/en/guide/troubleshooting.md)**

The full doc index is at [docs/en/](docs/en/README.md). Japanese docs are at [docs/ja/](docs/ja/README.md).

## Status

Currently being evaluated within the scope the author can directly support. A wider public release will follow.

## License

[MIT](LICENSE).
