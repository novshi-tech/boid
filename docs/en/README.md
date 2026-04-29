# boid Documentation

`boid` is a personal AI orchestrator that turns end-to-end workflows — generate, verify, release — into structured tasks running safely inside a sandbox.

This index is the entry point. The doc set is being built out incrementally; planned pages are listed below without links until they are written.

[日本語版](../ja/README.md)

## I want to...

- **...try boid for the first time.** → [Install](getting-started/01-install.md)
- **...understand the model.** → [Concepts](guide/concepts.md) and [State machine](guide/state-machine.md)
- **...drive boid from a phone.** → [Web UI](guide/web-ui.md)
- **...debug something that is stuck.** → [Troubleshooting](guide/troubleshooting.md)
- **...read past design notes.** → [Archive](archive/)

## Sections

### Getting started

Step-by-step walkthroughs.

- [1. Install](getting-started/01-install.md)
- 2. Your first task — *planned*
- 3. Projects and kits — *planned*
- 4. Feedback loop — *planned*

### Guide

Concept-oriented how-to.

- [Concepts](guide/concepts.md) — task, job, hook, gate, kit, payload, trait
- [State machine](guide/state-machine.md) — `executing → verifying → reworking → done`
- [Web UI](guide/web-ui.md) — pairing, device management, Cloudflare Tunnel
- [Troubleshooting](guide/troubleshooting.md)

### Reference

Stable interface specifications. Currently planned, not yet written.

- CLI: `boid start`, `boid task`, `boid job`
- `project.yaml` schema

### Kit authoring

Planned. For now, the [boid-kits](https://github.com/novshi-tech/boid-kits) repository is the working reference.

### Architecture

Planned. For internals, the source under [`internal/`](https://github.com/novshi-tech/boid/tree/main/internal) is the source of truth.

### Contributing

Planned. The short version: TDD, minimum dependencies, commit prefix `feat:` / `fix:` / `refactor:` / `test:`. See [`CLAUDE.md`](https://github.com/novshi-tech/boid/blob/main/CLAUDE.md) for the working conventions.

### Archive

- [Past design notes and implementation plans](archive/)
