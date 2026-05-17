# 4. Set up a kit

This page installs the **kit** that lets `boid` invoke an AI agent (Claude Code) and wires it into the `demo` project from [2. Initialize a project](02-init-project.md). After this chapter `boid` is ready to accept tasks and launch Claude Code for them. It takes about five minutes.

This page assumes you have completed [3. Set up the Web UI](03-web-ui.md).

## What this tutorial covers

- A concrete look at what a **kit** packages.
- Installing the `claude-code` kit.
- Referencing the kit from `project.yaml` so a `task_behaviors` entry can use it.

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

## Wire the kit into project.yaml

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

## Recap

What this tutorial covered:

- The contents of a kit (hooks / gates / commands / bindings / env).
- Pulling a kit repository in with `boid kit install`.
- Referencing a kit from `project.yaml` via `kits:` and binding it to a behavior through `default_instruction`.
- Picking edits up with `boid project reload`.

In the next chapter you will run a task against this setup and watch it execute from the CLI and the Web UI.

---

Next: [5. Your first task](05-first-task.md)
