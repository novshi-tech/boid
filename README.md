# boid

**A personal AI orchestrator.** Track and automate end-to-end workflows — from generating an artifact to releasing it — as structured tasks that execute safely inside a sandbox, on your own machine.

[日本語 README](README.ja.md)

## Features

- **Local & single-user.** A self-contained daemon backed by SQLite. No cloud account, no team plumbing, no shared state.
- **Structured task lifecycle.** Every task moves through `executing → verifying → reworking → done`. Rework is driven by verification findings on the task payload, not by ad-hoc prompts.
- **Sandbox-first execution.** Hooks and agent execs run inside a sandbox by default. Only commands declared as `host_commands` cross to the host, and the policy is per-kit.
- **Worktree + PR workflow.** Run parallel dev tasks in isolated git worktrees. Auto-merge and CI verification ship as kits, so the core stays environment-agnostic.
- **Pluggable kits.** Reuse Claude Code, Codex, GitHub PR/auto-merge, and other building blocks from [boid-kits](https://github.com/novshi-tech/boid-kits) — or write your own.
- **TUI, CLI, and Web UI.** Drive boid from your terminal, or pair a phone over Cloudflare Tunnel for mobile control.

## Install

```bash
go install github.com/novshi-tech/boid@latest
```

Linux only.

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
