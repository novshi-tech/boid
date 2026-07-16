# 2. Initialize a project

> **Notice**: The old `boid init` wizard has been removed.
> The new setup flow is **2 steps**: register the project, then (optionally) configure a workspace.
> If the `default` workspace is good enough, it's effectively **1 step**.
> See [Onboarding](../guide/onboarding.md) for the full reference.

This page walks through the new flow for setting up a project.
This page assumes you have completed [1. Install](01-install.md).

## What this tutorial covers

- Creating a new project with `boid project init`.
- Setting up a dedicated workspace with `boid workspace create` / `edit`, when the runtime environment needs customizing.

## A note on agents

`boid`'s architecture is intentionally agent-neutral, but **Claude Code is currently the only agent with production-grade support**. The rest of the tutorial assumes Claude Code is set up locally: the `claude` CLI is on your `PATH` and you have signed in. See [Claude Code's docs](https://docs.claude.com/en/docs/claude-code/overview) for the CLI setup.

## Step 1: Create a new project

```bash
mkdir -p ~/boid-demo
boid project init ~/boid-demo
```

Omitting `--workspace` assigns the project to the `default` workspace automatically (the daemon guarantees `default` always exists at startup). Only pass `--workspace` when you need to customize the runtime environment — `host_commands` / `env` / `allowed_domains`, etc.:

```bash
boid project init ~/boid-demo --workspace dev
```

`--workspace` is get-or-create: if `dev` doesn't exist yet, an empty workspace is created automatically before the project is assigned to it.

Dropping `.boid/project.yaml` into an existing repository works the same way:

```bash
boid project init ~/src/myrepo --workspace dev
```

## Step 1b: Register an existing project (if project.yaml already exists)

If a `.boid/project.yaml` already exists, use `project add` instead:

```bash
boid project add ~/src/myrepo --workspace dev
```

Same get-or-create semantics for `--workspace` as `project init`; omitting it uses `default`.

## (Optional) Step 2: Fill in the workspace's contents

If you passed `--workspace dev` in Step 1, `dev` already exists (get-or-create created it empty and assigned the project to it) — so filling in its contents is an **edit**, not a create:

```bash
boid workspace edit dev --from-file dev-workspace.yaml
```

(`boid workspace create dev --from-file ...` would now fail with `409` — `dev` already has a DB row. `create` is only for a slug that doesn't exist yet, e.g. if you registered the project against `default` in Step 1 and are setting up a *different*, brand-new workspace now — in that case, follow up with `boid workspace assign boid-demo <slug>` to actually attach the project to it.)

Example `dev-workspace.yaml`:

```yaml
env:
  MY_TOKEN: "secret:my-token"
host_commands:
  - gh
allowed_domains:
  - example.com
```

`host_commands` here is a list of **reference names**, not definitions — each name (`gh` above) must already be defined in the daemon-wide `~/.config/boid/host_commands.yaml`. See [Onboarding / Defining host_commands](../guide/onboarding.md#defining-host_commands-the-daemon-wide-registry) if that file doesn't have the name yet.

Use `boid workspace show dev` to inspect the contents, or `boid workspace export dev` to get it back out as yaml. See [Onboarding / Creating/editing a workspace](../guide/onboarding.md#creatingediting-a-workspace) for details.

## Inspect the generated project.yaml

```bash
cat ~/boid-demo/.boid/project.yaml
```

You should see something close to (the wizard's built-in scaffold, `internal/initwizard/default_behaviors.tmpl`):

```yaml
id: <uuid>
name: boid-demo
default_task_behavior: supervisor
task_behaviors:
  executor:
    default_instruction:
      agent: claude-code
      message: |
        Implement what the task.yaml title and description ask
        for, then commit on the current branch (boid/<task_id8>,
        cut from the project's base branch) and exit. Do not
        push, do not open a PR — the parent supervisor merges
        the branch into the base branch locally.
  supervisor:
    default_instruction:
      agent: claude-code
      message: |
        Triage the request, create child executor tasks, and
        monitor them in order. Each child commits onto its
        boid/<task_id8> branch (cut from the base branch by
        boid's worktree feature). When a child reaches `done`:
          1. git checkout <base_branch>
          2. git merge --no-ff boid/<child_id8>
             -m "Merge boid/<child_id8>"
          3. Verify the merged result, then launch the next
             child.
        If a merge conflicts or the verification fails, reopen
        the child with `boid task reopen <child_id> -m "..."`.
```

- **`default_task_behavior`** — which `task_behaviors` entry `boid task create` uses when a task omits `behavior:`.
- **`task_behaviors`** — defines how tasks run (see [Concepts / behavior](../guide/concepts.md#behavior)). Any name is allowed (free naming); `supervisor` / `executor` here are just the wizard's own default names, not reserved keywords.

Inspect the registration:

```bash
boid project list
boid project show boid-demo
```

## Recap

What this tutorial introduced:

- **`boid project init`** to scaffold `.boid/project.yaml` and register the project (auto-assigned to the `default` workspace).
- `--workspace <slug>` (get-or-create) plus `boid workspace create` / `edit` when a dedicated runtime environment is needed.
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
