# boid

**A personal AI orchestrator (Linux only).** Track each request, from kickoff to a finished artifact, as a single task and hand it to an AI agent. The agent reads and writes your local filesystem directly, so it can use the tools and environment you already have. Writes are confined to a sandbox-bounded scope, so a runaway agent cannot damage the rest of your machine.

[日本語 README](README.ja.md)

## Features

- **Use the tools you already installed.** Hand the agent your local Claude Code, Codex, git, gh, editors, and language toolchains. Cloud-side agent sandboxes can't touch your real environment; this one does.
- **Up and running in two commands.** `go install`, then `boid start`. No config file, no server provisioning, no signup.
- **The task model is built in.** Request → execute → verify → rework → done, with the data captured at each step pre-defined. You don't have to re-explain context every time the agent loops back.
- **Bound the write scope with a sandbox.** The agent reads your real directories directly, but its writes are confined to a git worktree or a similarly limited scope. A runaway agent cannot reach your home directory or other projects.
- **Run several requests in parallel.** Each task gets its own git worktree, so concurrent jobs don't trip over each other.
- **Extensions are swappable packages.** Pick which AI agent (Claude Code, Codex), which CI integration, which PR / auto-merge flow — the building blocks live in separate packages such as [boid-kits](https://github.com/novshi-tech/boid-kits).
- **Drive it from the terminal or a browser.** TUI and CLI for everyday use; expose the Web UI through Cloudflare Tunnel to drive it from your phone.

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
- **[State machine](docs/en/guide/state-machine.md)**
- **[Web UI](docs/en/guide/web-ui.md)** — including Cloudflare Tunnel setup
- **[Troubleshooting](docs/en/guide/troubleshooting.md)**

The full doc index is at [docs/en/](docs/en/README.md). Japanese docs are at [docs/ja/](docs/ja/README.md).

## Status

Currently being evaluated within the scope the author can directly support. A wider public release will follow.

## License

[MIT](LICENSE).
