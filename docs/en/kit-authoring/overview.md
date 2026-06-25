# Kit authoring overview

A minimal guide for people who want to write their own kit.

The definition of "kit" lives in [Concepts](../guide/concepts.md#kit). This page covers the on-disk layout, the main `kit.yaml` fields, the hook script protocol, and how to ship a kit.

## On-disk layout

A kit is a directory that contains a `kit.yaml`. Smallest example:

```
my-kit/
├── kit.yaml
├── detect.sh         (optional)
└── hooks/
    └── my-hook.sh
```

To ship multiple kits in one repository, place each kit in its own subdirectory at the repo root (the official [boid-kits](https://github.com/novshi-tech/boid-kits) follows this layout).

## Main `kit.yaml` fields

```yaml
meta:
  name: My kit
  description: One-line description shown in UIs
  category: workflow            # language / vcs / ci / agent / workflow / utility

detect:
  script: detect.sh             # (optional) applicability detection script

requires:
  commands:                     # (optional) host commands needed on PATH
    - gh

provides_agent: my-agent        # (optional) agent name this kit listens for

hooks:
  - id: my-hook
    kind: agent                 # (optional) "agent" opts in to instruction routing
    agent: my-agent             # (optional) instructions addressed to this agent
    traits:
      consumes: [artifact?]     # payload traits to read; valid values: artifact, verification, awaiting
      produces: [artifact]

commands:                       # (optional) commands callable via boid exec inside the sandbox
  build:
    command: [make, build]

host_commands:                  # (optional) commands forwarded out of the sandbox to the host
  gh:
    allow: [pr, issue]

additional_bindings:            # (optional) extra mounts into the sandbox
  - source: ${HOME}/.config/my-tool

env:                            # (optional) env vars set in the sandbox
  MY_TOOL_FLAG: "1"
```

The shared building blocks (`HostCommands`, `BindMount`, `Instruction`) are exactly the same shape used in `project.yaml` — see the [`project.yaml` reference](../reference/project-yaml.md) for their detailed schema.

### `meta`

The label `boid` UIs use to identify the kit. By convention, `category` is one of `language`, `vcs`, `ci`, `agent`, `workflow`, or `utility`.

### `detect`

A POSIX sh script used during setup flows (such as `boid project init`) to decide whether this kit applies to a given project. Print one of:

- `required` — auto-select this kit for the project.
- `optional` — show as a candidate, do not auto-select.
- empty / anything else — not applicable.

Runs with a 5-second timeout, with the project root as the working directory.

### `requires.commands`

Host-side commands that need to be on `PATH` for this kit to function. Used during install and surfaced in UIs.

### `provides_agent`

Declares which agent name's instructions this kit is responsible for. For example, `claude-code` sets `provides_agent: claude-code` and handles any instruction whose `agent:` is `claude-code`.

### `hooks`

The script protocol is in the next section. Hooks always run in `executing`.

`traits.consumes` and `traits.produces` declare which payload traits the hook reads and writes. Suffixing a `consumes` entry with `?` makes it optional (hook fires even if that trait is absent from the payload). Valid trait names are `artifact`, `verification`, and `awaiting`.

## Hook script protocol

This section is the summary; the full reference (context file schemas, all environment variables, the `payload_patch.json` file path, ...) lives in the [Hook script protocol reference](../reference/hook-contract.md).

### Input (context files and environment variables)

Hook jobs run with an interactive PTY session (`Interactive: true`), so **stdin is not used for data delivery**. Task metadata is provided through context files and environment variables instead.

**Context files** (written to `$HOME/.boid/context/` before the hook runs):

- `task.yaml` — task snapshot: `id`, `title`, `status`, `behavior`, `description`
- `instructions.yaml` — routed instructions for this hook (only for `kind: agent` hooks)
- `environment.yaml` — environment metadata
- `payload.yaml` / `payload.json` — the filtered payload (traits declared in `consumes`)

**Environment variables** such as `BOID_TASK_ID` / `BOID_JOB_ID` are set in the sandbox. See the [Hook script protocol reference](../reference/hook-contract.md) for the full list.

### Output (payload patch)

To update the payload, write JSON of this shape to `$HOME/.boid/output/payload_patch.json`:

```json
{
  "payload_patch": {
    "artifact": { "result": "ok" }
  }
}
```

Only when that file is absent does the runtime fall back to stdout, treating its content as the payload patch. New hooks should prefer the file path — agent-style hooks write incidental output to stdout, and the file path avoids mistaking that for a payload patch.

The `payload_patch` body is a JSON merge instruction applied to the payload. Nested keys merge into their corresponding subtrees. If you have nothing to write, output nothing.

### Logs (stderr)

Send progress messages and error detail to stderr. `boid job show <job-id>` surfaces them, so log freely.

### Exit code

- `0` — success.
- non-zero — failure. The task itself is not aborted; `boid` marks the job as `failed` and the state machine decides whether to retry.

## Distribution

Kits are distributed as their own git repositories. `boid kit install <git-host>/<owner>/<repo>` clones the repo into `~/.local/share/boid/kits/<git-host>/<owner>/<repo>/`. Users reference individual kits with `<git-host>/<owner>/<repo>/<sub-path>` from `project.yaml`'s `kits:` field.

Conventions for publishing:

- The README should state what the kit does, which agent's instructions it listens for, and which host commands it requires.
- If you ship multiple kits in one repo, give each subdirectory its own README.
- Set `meta.category` to match the kit's actual role.
- Always declare `requires.commands` — it drives the user's initial setup checks.

## Reference implementations

- [`github.com/novshi-tech/boid-kits`](https://github.com/novshi-tech/boid-kits) — the official kits. `claude-code`, `github-cli`, `go-dev`, and similar are good reference reads for different shapes of kit.

## Related docs

- [Concepts](../guide/concepts.md) — for the meaning of hook / kit / trait.
- [`project.yaml` reference](../reference/project-yaml.md) — how `project.yaml` references kits.
- [State machine](../guide/state-machine.md) — when hooks fire.
