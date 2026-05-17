# 4. Projects and extension packages (kits)

In [3. Your first task](03-first-task.md) you ran the state machine with no handlers attached. This page introduces **kits** so `boid` can do real work for you. It takes about ten minutes.

## What this tutorial covers

- What a kit actually is on disk, beyond the abstract description.
- Installing a kit repository with `boid kit install`.
- Wiring kits into `project.yaml` under `task_behaviors`.
- Running one minimal end-to-end task driven by an AI agent (Claude Code).

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
| `github.com/novshi-tech/boid-kits/codex` | Runs the OpenAI Codex CLI agent as a hook. |
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

## Run it

You will need the `claude` CLI on your `PATH` and a signed-in Claude Code session. See [Claude Code's docs](https://docs.claude.com/en/docs/claude-code/overview) for how to set that up.

Create a task and have it start automatically.

```bash
boid task create <<'YAML'
project_id: demo
title: What is Linux, in one sentence?
behavior: supervisor
auto_start: true
YAML
```

`auto_start: true` skips `pending` and goes straight to `executing`.

In another terminal, follow the task:

```bash
boid task watch <task-id>
```

After a moment the hook job runs Claude, the agent calls `boid task update` to write `artifact`, and once the hook exits cleanly the auto-transition moves the task `executing → done`.

Final state:

```bash
boid task show <task-id>
```

If `payload.artifact.answer` holds the answer, it worked.

To inspect what the hook actually printed:

```bash
boid job list --task <task-id>
boid job show <job-id>
```

## Recap

What this tutorial covered:

- The contents of a kit (hooks / gates / commands / bindings / env).
- Pulling a kit repository in with `boid kit install`.
- The shape of `project.yaml`, including `kits` and `default_instruction`.
- Skipping `pending` with `auto_start: true`.

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
