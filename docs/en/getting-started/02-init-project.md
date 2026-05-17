# 2. Initialize a project

This page uses `boid init` to set up a project. The wizard also picks the **kits** (extension packages) the project will use, so by the end of this chapter the project is ready to file tasks against an AI agent. It takes about five minutes.

This page assumes you have completed [1. Install](01-install.md).

## What this tutorial covers

- Installing the official kit repository.
- Running `boid init`'s interactive wizard to scaffold a project.
- Inspecting the generated `.boid/project.yaml`.

## A note on agents

`boid`'s architecture is intentionally agent-neutral, but **Claude Code is currently the only agent with production-grade support**. The rest of the tutorial assumes Claude Code is set up locally: the `claude` CLI is on your `PATH` and you have signed in. See [Claude Code's docs](https://docs.claude.com/en/docs/claude-code/overview) for the CLI setup.

## Install the kit repository

`boid init` selects from kits that are **already installed**. Pull down the official kit registry first:

```bash
boid kit install github.com/novshi-tech/boid-kits
```

The repo is cloned to `~/.local/share/boid/kits/github.com/novshi-tech/boid-kits/`. Each subdirectory under it is one kit (`claude-code`, `github-cli`, and so on). The full story of what a kit packages lives in the [Kit authoring overview](../kit-authoring/overview.md).

Confirm what is installed:

```bash
boid kit list
```

## Create a workspace directory

Pick a directory for this tutorial:

```bash
mkdir -p ~/boid-demo
cd ~/boid-demo
```

Dropping `.boid/project.yaml` into an existing repository works too — just `cd` into it and skip the `mkdir`.

## Run `boid init`

```bash
boid init
```

The interactive wizard opens. Every prompt has a sensible default — pressing Enter without input accepts it.

```
Project name [boid-demo]:
Available kits (auto-detected marked with ✓):
  [✓] 1. Claude Code (github.com/novshi-tech/boid-kits/claude-code)
  [ ] 2. GitHub CLI (github.com/novshi-tech/boid-kits/github-cli) (optional)
  [ ] 3. Go development (github.com/novshi-tech/boid-kits/go-dev)
  ...
Enable/disable kits (space-separated numbers, prefix - to deselect, Enter to keep defaults):
>
Task behavior provider: boid-tasks - Default task behaviors
Use this? [Y/n]:
Checking requirements...
  ✓ claude (/home/<you>/.local/bin/claude)

✓ Created /home/<you>/boid-demo/.boid/project.yaml
project registered: <uuid> (boid-demo)
```

What each prompt asks:

1. **Project name** — the label shown in the Web UI / TUI. Defaults to the directory name.
2. **Available kits** — installed kits that look applicable to this machine are pre-selected (e.g. Claude Code shows up if `claude` is on your `PATH`). Type numbers to toggle.
3. **Task behavior provider** — which kit supplies the `task_behaviors.supervisor` / `task_behaviors.executor` scaffold. The default `boid-tasks` is normally what you want.
4. **Requirements check** — verifies that the host commands each selected kit needs are on your `PATH`.

The wizard then writes `.boid/project.yaml` and registers the project with the daemon.

## Inspect the generated project.yaml

```bash
cat .boid/project.yaml
```

You should see something close to:

```yaml
id: <uuid>
name: boid-demo
kits:
  - github.com/novshi-tech/boid-kits/claude-code
task_behaviors:
  executor:
    default_instruction:
      type: execution
      message: |
        ...
  supervisor:
    default_instruction:
      type: execution
      message: |
        ...
```

- **`kits:`** lists the kits you selected.
- **`task_behaviors.supervisor` / `task_behaviors.executor`** are the two canonical roles `boid` understands. Supervisor is the readonly orchestrator; executor is the writable implementer (see [Concepts / behavior](../guide/concepts.md#behavior)).
- **`default_instruction`** is the template message sent to the agent when a task starts. Edit it if you want, then run `boid project reload` to pick the change up.

Inspect the registration:

```bash
boid project list
boid project show boid-demo
```

## Recap

What this tutorial introduced:

- Pulling the official kit repository with **`boid kit install`**.
- Letting **`boid init`** assemble `.boid/project.yaml` and register the project in one shot.
- Reading back the generated `kits:` and `task_behaviors`.
- Reloading hand edits with `boid project reload`.

The next chapter sets up the Web UI against this same project.

## Cleanup (optional)

To remove what this chapter created:

```bash
boid project remove boid-demo
rm -rf ~/boid-demo
```

The later chapters reuse this project, so leave it in place if you plan to keep reading.

---

Next: [3. Set up the Web UI](03-web-ui.md)
