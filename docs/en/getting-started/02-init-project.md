# 2. Initialize a project

> **Notice**: The old `boid init` wizard has been removed.
> The new setup flow uses three commands.
> See [Onboarding](../guide/onboarding.md) for the full reference.

This page walks through the new three-step flow for setting up a project.
This page assumes you have completed [1. Install](01-install.md).

## What this tutorial covers

- Generating the kit catalog with `boid kit init`.
- Creating a new project with `boid project init`.
- Generating a workspace configuration with `boid workspace configure`.

## A note on agents

`boid`'s architecture is intentionally agent-neutral, but **Claude Code is currently the only agent with production-grade support**. The rest of the tutorial assumes Claude Code is set up locally: the `claude` CLI is on your `PATH` and you have signed in. See [Claude Code's docs](https://docs.claude.com/en/docs/claude-code/overview) for the CLI setup.

## Step 1: Generate the kit catalog

```bash
boid kit init
```

Generates the kit catalog for this machine.
Confirm what is installed:

```bash
boid kit list
```

## Step 2a: Create a new project

```bash
mkdir -p ~/boid-demo
boid project init ~/boid-demo --workspace dev
```

`--workspace dev` links the project to a workspace slug named `dev`. You can omit it and link later.

Dropping `.boid/project.yaml` into an existing repository works the same way:

```bash
boid project init ~/src/myrepo --workspace dev
```

## Step 2b: Register an existing project (if project.yaml already exists)

If a `.boid/project.yaml` already exists, use `project add` instead:

```bash
boid project add ~/src/myrepo --workspace dev
```

## Step 3: Configure the workspace

```bash
boid workspace configure dev
```

Generates the workspace configuration (which kits to activate, env overrides, host_commands, etc.).

## Inspect the generated project.yaml

```bash
cat ~/boid-demo/.boid/project.yaml
```

You should see something close to:

```yaml
id: <uuid>
name: boid-demo
worktree: true
task_behaviors:
  dev:
    default_instruction:
      agent: claude-code
      message: |
        Implement what the task describes, commit on the current branch, and exit.
```

- **`worktree: true`** — executor tasks run on a dedicated isolated branch on the in-sandbox clone.
- **`task_behaviors`** — defines how tasks run (see [Concepts / behavior](../guide/concepts.md#behavior)).

Inspect the registration:

```bash
boid project list
boid project show boid-demo
```

## Recap

What this tutorial introduced:

- **`boid kit init`** to generate the kit catalog.
- **`boid project init`** to scaffold `.boid/project.yaml` and register the project.
- **`boid workspace configure`** to generate workspace configuration.
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
