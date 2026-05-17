# 4. Your first task

This page hands a small question to the Claude Code agent and walks through `boid`'s core use case in the shortest possible loop: file a task, the AI runs it inside a sandbox, the result lands on the task. It takes about ten minutes.

This page assumes you have completed [3. Set up the Web UI](03-web-ui.md).

## What this tutorial covers

- A concrete look at what a **kit** is.
- Installing the `claude-code` kit and wiring it into `project.yaml`.
- Running one small task and observing it from both the CLI and the Web UI.

## A note on agents

`boid`'s architecture is intentionally agent-neutral, but **Claude Code is currently the only agent with production-grade support**. A kit for the OpenAI Codex CLI (`github.com/novshi-tech/boid-kits/codex`) is shipped, but harness-side support for it is still in flux.

From this chapter onward we assume Claude Code is set up locally: the `claude` CLI is on your `PATH` and you have signed in. See [Claude Code's docs](https://docs.claude.com/en/docs/claude-code/overview) for the CLI setup.

## What a kit packages

On disk a kit is a directory containing a `kit.yaml` file plus a few scripts. The `kit.yaml` declares some combination of:

- **hooks** — which scripts to run in which states (e.g. `executing`).
- **gates** — which scripts to run on which state transitions (entry / exit).
- **commands** — commands that can be invoked through `boid exec` from within the sandbox.
- **host_commands** — commands the kit is allowed to forward from the sandbox out to the host.
- **additional_bindings** — extra paths to mount into the sandbox.
- **env** — environment variables to set inside the sandbox.

Put differently: a kit bundles "what to launch in a particular state, and what that script is allowed to do." A project then refers to a kit from a behavior to pull in that bundle of behavior without having to write the hooks itself.

The official kits live in the [`github.com/novshi-tech/boid-kits`](https://github.com/novshi-tech/boid-kits) repository. A few representatives:

| Kit ref | What it does |
|---|---|
| `github.com/novshi-tech/boid-kits/claude-code` | Runs the Claude Code agent as a hook. |
| `github.com/novshi-tech/boid-kits/codex` | Runs the OpenAI Codex CLI agent as a hook (experimental). |
| `github.com/novshi-tech/boid-kits/go-dev` | Mounts `~/go` and friends into the sandbox. |
| `github.com/novshi-tech/boid-kits/github-cli` | Makes `gh` callable from inside the sandbox. |
| `github.com/novshi-tech/boid-kits/github-auto-merge` | Adds an exit gate on `executing → done` that runs `gh pr merge`. |

## Install a kit repository

`boid kit install` clones the repo into `~/.local/share/boid/kits/<repo path>/`.

```bash
boid kit install github.com/novshi-tech/boid-kits
```

Each subdirectory of the cloned repo is its own kit. The kit ref for Claude Code, for example, is `github.com/novshi-tech/boid-kits/claude-code`.

List what is installed:

```bash
boid kit list
```

## Wire a kit into project.yaml

Edit the `~/boid-demo/.boid/project.yaml` from [2. Initialize a project](02-init-project.md) so that the behavior invokes the Claude Code agent.

```yaml
id: demo
name: Demo

kits:
  - github.com/novshi-tech/boid-kits/claude-code

task_behaviors:
  supervisor:
    name: Supervisor
    default_instruction:
      type: execution
      agent: claude-code
      message: |
        Answer the question in the task title / description, then write the
        answer into the artifact trait:
          echo '{"artifact":{"answer":"<your answer>"}}' \
            | boid task update <task_id> --payload-file -
```

What is going on:

- **Top-level `kits:`** lists the kits used across the whole project. Here, just `claude-code`.
- **`task_behaviors.supervisor`** declares the canonical readonly behavior. We don't need to set `readonly:` explicitly — supervisor is always readonly, which is fine because this task only needs to write back to the payload, not edit files.
- **`default_instruction`** holds a single Instruction object passed to the agent on `executing`. `agent: claude-code` is how the claude-code kit's hook recognises "this instruction is meant for me".

Reload the project:

```bash
boid project reload
```

## Run one task

Create a task and have it start automatically:

```bash
boid task create <<'YAML'
project_id: demo
title: What is Linux, in one sentence?
behavior: supervisor
auto_start: true
YAML
```

`auto_start: true` skips `pending` and goes straight to `executing`.

### Watch from the CLI

In another terminal, follow the task:

```bash
boid task watch <task-id>
```

After a moment the hook job runs Claude, the agent calls `boid task update` to write `artifact`, and once the hook exits cleanly the auto-transition moves the task `executing → done`.

### Watch from the Web UI

Refresh the `http://localhost:8080` tab opened in [3. Set up the Web UI](03-web-ui.md) and you should see the task in the list. Click the row to drill into details — payload and jobs update live.

## Inspect the result

When the task reaches `done`, look at the final state:

```bash
boid task show <task-id>
```

If `payload.artifact.answer` holds the answer, it worked.

To inspect what the hook actually printed:

```bash
boid job list --task <task-id>
boid job show <job-id>
```

The Web UI also surfaces per-job logs.

## Recap

What this tutorial covered:

- The contents of a kit (hooks / gates / commands / bindings / env).
- Pulling a kit repository in with `boid kit install`.
- The shape of `project.yaml`, including `kits` and `default_instruction`.
- Skipping `pending` with `auto_start: true` and going straight to execution.
- What triggers the auto-transition `executing → done` (a clean hook exit plus the `artifact` trait).

Next: a GitHub PR-driven dev workflow that combines a worktree with auto-merge.

## Cleanup

```bash
boid task delete <task-id>
boid project remove demo
rm -rf ~/boid-demo
```

`boid kit remove github.com/novshi-tech/boid-kits` removes the installed repository, but it is convenient to keep it around for the next tutorial.

---

Next: [5. The GitHub PR-driven dev workflow](05-dev-workflow.md)
