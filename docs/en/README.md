# boid Documentation

`boid` is a personal AI orchestrator. It hands a request, end to end, to an AI agent following a predefined task model — and confines the agent's writes to a sandbox-bounded scope so the rest of your machine stays safe.

This index is the entry point. The doc set is being built out incrementally; planned pages are listed below without links until they are written.

[日本語版](../ja/README.md)

## I want to...

- **...try boid for the first time.** → [Install](getting-started/01-install.md)
- **...understand the model.** → [Concepts](guide/concepts.md) and [State machine](guide/state-machine.md)
- **...drive boid from a phone.** → [Web UI](guide/web-ui.md)
- **...debug something that is stuck.** → [Troubleshooting](guide/troubleshooting.md)

## Sections

### Getting started

Step-by-step walkthroughs.

- [1. Install](getting-started/01-install.md)
- [2. Your first task](getting-started/02-first-task.md)
- [3. Projects and extension packages (kits)](getting-started/03-projects-and-kits.md)
- [4. The GitHub PR-driven dev workflow](getting-started/04-dev-workflow.md)

### Guide

Concept-oriented how-to.

- [Concepts](guide/concepts.md) — explains the internal vocabulary: task, job, hook, gate, kit, payload, trait, and more
- [State machine](guide/state-machine.md) — `executing → verifying → reworking → done`
- [Web UI](guide/web-ui.md) — pairing and revoking devices, exposing the UI through Cloudflare Tunnel
- [Troubleshooting](guide/troubleshooting.md)

### Reference

- [`project.yaml` reference](reference/project-yaml.md) — every field of the project definition file
- [Handler script protocol](reference/handler-contract.md) — the hook / gate I/O contract (stdin, env vars, `payload_patch.json`, exit codes, ...)
- [Payload trait reference](reference/traits.md) — the shape of `artifact` / `tasks` / `verification` / `lifecycle`, what the state machine reads, and the merge modes
- [CLI reference](reference/cli.md) — index of every subcommand grouped by role (per-flag detail lives in `boid <subcommand> --help`)
- [HTTP API reference](reference/http-api.md) — the `/api/*` endpoints the daemon exposes over the UNIX socket and HTTP listener, plus SSE and error format

### Kit authoring

- [Overview](kit-authoring/overview.md) — on-disk layout, key `kit.yaml` fields, the hook/gate script protocol
- Official kits: [boid-kits](https://github.com/novshi-tech/boid-kits)

### Architecture

- [Overview](architecture/overview.md) — process layout, layering, the major components, and one action traced end to end
- [Persistence layer](architecture/persistence.md) — SQLite tables, key columns, JSON column contents, and migration conventions
- [Sandbox internals](architecture/sandbox-internals.md) — the outer / setup / inner three-script chain, mount / user namespaces, pasta + nftables, the broker / shim split, and the cleanup safety guards
- [Web UI internals](architecture/web-internals.md) — auth middleware, session cookie (HMAC), the pairing flow, CSRF, and Server-Sent Events

### Contributing

- [Contributing guide](contributing/README.md) — development setup, coding conventions, PRs, bug reports
