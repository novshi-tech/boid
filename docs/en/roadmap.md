# Roadmap

A snapshot of where `boid` stands and the directions we are working on next. Not a fixed release schedule — the point is to make the current priorities visible.

## Where we are today

- The author and a few close collaborators use `boid` daily, and `boid` runs its own development through itself.
- The smallest documentation set that survives a public release is in place: Getting started 01–04, the guide, the reference, contributing, the kit-authoring overview, and the architecture overview.
- The Web UI is on Phase 1–3. We use it daily, including from a phone over Cloudflare Tunnel.
- Public location: the GitHub repository `novshi-tech/boid`.

## Stages

We are widening access in stages.

### 1. Direct-support stage (now)

- Users are limited to people who can reach the author directly.
- Documentation gaps (for example fine-grained per-kit configuration) are acceptable because we can fill them in conversation.
- Goals: surface defects in real use, find documentation gaps, harden kit configuration templates.

### 2. Self-service setup stage

- Polish the docs and the defaults so a new user can go from install to one finished task without direct support.
- Flesh out `boid init` to scaffold a project skeleton and a kit set interactively.
- Add a tutorial (extending the current 04, or a new page) that exercises every feature claimed in the README within one task.
- Decide on release tags and binary distribution.

### 3. Public release stage

- Announce the GitHub repository, accept issues and discussions.
- Pre-announce breaking changes and provide migration guidance.
- Maintain a versioning scheme (SemVer-leaning) and a changelog.

## Active focus areas

Priorities shift, but these are the areas we are deliberately polishing right now.

### Web UI Phase 4 (xterm.js / live attach)

- Connect the browser to a live PTY agent session via xterm.js.
- Larger than Phase 1–3 because it needs bidirectional streaming.

### Workflows that include a review agent

- Configurations where several AI agents review each other's output and write findings back.
- Once that lands, the next chapter after [4. The GitHub PR-driven dev workflow](getting-started/04-dev-workflow.md) — a "feedback loop" tutorial — becomes natural to write.
- Related design topics: trait-level division of responsibility, extending `consumer`-based instruction routing.

### More language / framework kits

- Today's official kits are `claude-code`, `codex`, `go-dev`, `python-uv`, `dotnet-dev`, `volta`, `docker`, `github-cli`, `github-auto-merge`, `boid-tasks`, and a few more.
- We will keep adding and polishing kits to match the projects users actually run.

### Documentation

In progress. The next pages we expect to land:

- `reference/cli/*` — per-subcommand reference (today, `--help` is the canonical source).
- `reference/handler-contract.md` — full hook/gate script protocol reference.
- `reference/traits.md` — full payload-trait spec.
- `architecture/persistence.md` — SQLite schema details.
- `architecture/sandbox-internals.md` — namespace / chroot / bind mount / proxy details.
- `adr/*` — records of major design decisions (script removal, host-command relaxation, external kits, consumes/produces split, ...).

## Out of scope

To keep the design coherent, we explicitly do **not** plan to do the following.

- **Multi-user or team features.** `boid` is built for one user. Sharing one daemon across multiple users is not a goal.
- **A hosted SaaS.** Distribution is open-source code plus binaries — we have no plans to operate a shared service.
- **Non-Linux support.** The sandbox implementation depends on Linux primitives (mount namespaces, chroot, `unshare`). Porting to macOS or Windows is not in scope.
- **Pulling agent-side controls into the core.** Prompt construction, model selection, and tool permissions live in the agent (Claude Code etc.) and are expressed through instructions. The core stays focused on state machine and orchestration responsibilities.

## Feedback

Wishes for the roadmap, including "do this earlier", are welcome as GitHub issues. See [Contributing](contributing/README.md) for the details.
