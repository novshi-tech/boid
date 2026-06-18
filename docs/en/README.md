# boid Documentation

`boid` is a personal AI orchestrator. It is built to keep the human from becoming the bottleneck when several AI coding agents run in parallel: agents are given room to make progress autonomously, a sandbox with a configurable write scope keeps them from doing damage, and every in-flight task is visible at a glance through the CLI and Web UI. Everything stays on your own machine — no servers, no signup.

The architecture is intentionally agent-neutral, but **Claude Code is currently the only agent with production-grade support**. The tutorials assume that setup.

This index is the entry point. The doc set is being built out incrementally; planned pages are listed below without links until they are written.

[日本語版](../ja/README.md)

## I want to...

- **...try boid for the first time.** → [Install](getting-started/01-install.md)
- **...understand the model.** → [Concepts](guide/concepts.md) and [State machine](guide/state-machine.md)
- **...drive boid from a phone.** → [Web UI](guide/web-ui.md)
- **...get a push notification when boid needs my input.** → [Notifications](guide/notifications.md)
- **...debug something that is stuck.** → [Troubleshooting](guide/troubleshooting.md)

## Sections

### Getting started

Step-by-step walkthroughs.

- [1. Install](getting-started/01-install.md)
- [2. Initialize a project](getting-started/02-init-project.md)
- [3. Set up the Web UI](getting-started/03-web-ui.md)
- [4. Your first task](getting-started/04-first-task.md)
- [Workflows](../workflows.md) — three end-to-end workflow shapes (local merge / 1 executor 1 PR / 1 supervisor 1 PR) with project.yaml templates

### Guide

Concept-oriented how-to.

- [Concepts](guide/concepts.md) — explains the internal vocabulary: task, job, hook, kit, payload, trait, and more
- [State machine](guide/state-machine.md) — `pending → executing → done` (plus `aborted`)
- [Web UI](guide/web-ui.md) — pairing and revoking devices, exposing the UI through Cloudflare Tunnel
- [Notifications](guide/notifications.md) — configuring `notify.command`, ntfy and Pushover script examples
- [Troubleshooting](guide/troubleshooting.md)

### Reference

- [`project.yaml` reference](reference/project-yaml.md) — every field of the project definition file
- [Hook script protocol](reference/hook-contract.md) — the hook I/O contract (stdin, env vars, `payload_patch.json`, exit codes, ...)
- [Payload trait reference](reference/traits.md) — the shape of `artifact` / `lifecycle`, what the state machine reads, and the merge modes
- [CLI reference](reference/cli.md) — index of every subcommand grouped by role (per-flag detail lives in `boid <subcommand> --help`)
- [HTTP API reference](reference/http-api.md) — the `/api/*` endpoints the daemon exposes over the UNIX socket and HTTP listener, plus SSE and error format

### Kit authoring

- [Overview](kit-authoring/overview.md) — on-disk layout, key `kit.yaml` fields, the hook script protocol
- Official kits: [boid-kits](https://github.com/novshi-tech/boid-kits)

### Architecture

- [Overview](architecture/overview.md) — process layout, layering, the major components, and one action traced end to end
- [Persistence layer](architecture/persistence.md) — SQLite tables, key columns, JSON column contents, and migration conventions
- [Sandbox internals](architecture/sandbox-internals.md) — the outer / setup / inner three-script chain, mount / user namespaces, pasta + nftables, the broker / shim split, and the cleanup safety guards
- [Web UI internals](architecture/web-internals.md) — auth middleware, session cookie (HMAC), the pairing flow, CSRF, and Server-Sent Events

### Contributing

- [Contributing guide](contributing/README.md) — development setup, coding conventions, PRs, bug reports
