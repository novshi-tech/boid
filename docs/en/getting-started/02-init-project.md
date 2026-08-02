# 2. Initialize a project

> **Notice**: The old `boid init` wizard has been removed.
> The new setup flow is **scaffold → push → register by git URL**, plus an optional workspace step.
> See [Onboarding](../guide/onboarding.md) for the full reference.

This page walks through the new flow for setting up a project.
This page assumes you have completed [1. Install](01-install.md).

## What this tutorial covers

- Scaffolding `.boid/project.yaml` with `boid project init`.
- Pushing it and registering it with `boid project add <git-url>`.
- Setting up a dedicated workspace with `boid workspace create` / `edit`, when the runtime environment needs customizing.

## A note on agents

`boid`'s architecture is intentionally agent-neutral, but **Claude Code is currently the only agent with production-grade support**. The rest of the tutorial assumes Claude Code is set up locally: the `claude` CLI is on your `PATH` and you have signed in. See [Claude Code's docs](https://docs.claude.com/en/docs/claude-code/overview) for the CLI setup.

## Why scaffolding and registration are separate commands

The daemon runs inside the compose stack's own container ([1. Install](01-install.md)), which has no visibility into a host directory like `~/boid-demo`. The daemon can only load a project it clones itself from a **git remote URL**. So `boid project init` only goes as far as writing the local scaffold and printing the exact next commands to run; actual registration (telling the daemon about it) happens afterwards, once you have pushed, via `boid project add <git-url>`.

## Step 1: Scaffold the project

```bash
mkdir -p ~/boid-demo
cd ~/boid-demo
boid project init
```

(You can also pass a target directory: `boid project init <dir>`.) After prompting for a project name, it writes `.boid/project.yaml`. Nothing is registered with the daemon at this point.

Passing `--workspace` bakes that workspace name into the `boid project add` example printed in the next step (the actual workspace assignment happens at `project add` time, get-or-create):

```bash
boid project init --workspace dev
```

When it finishes, you'll see guidance like this (substitute `<git-url>` with your real URL):

```
Next steps:
  1. Make sure your project's actual source code -- not just this scaffold -- is already committed and pushed to your remote.
  2. Commit the scaffold and push it to your remote (safe to run even if some of this is already done):
       cd ~/boid-demo && { git init && git add .boid/project.yaml && ... && git push '<git-url>' HEAD && ... }
  3. Register the pushed URL with the running boid daemon:
       boid project add '<git-url>' --workspace=default
```

## Step 2: Push

If you have real code that hasn't been pushed yet, commit and push it first, following the printed guidance. For a genuinely brand-new directory (like this tutorial's `~/boid-demo`), you can just run the printed command as-is — it handles committing and pushing `.boid/project.yaml`:

```bash
cd ~/boid-demo && { git init && git add .boid/project.yaml && (git diff --cached --quiet -- .boid/project.yaml || git commit -m 'add boid project scaffold' -- .boid/project.yaml) && git push 'https://github.com/you/boid-demo.git' HEAD; }
```

This is safe to re-run against an already-existing repository (it never touches `origin` or any other named remote — it is always a one-shot push straight to the given URL). If the pushed branch differs from the remote's default branch, you'll get a warning — merge/PR it there before the next step, since the daemon reads `.boid/project.yaml` off the remote's default branch.

## Step 3: Register with the daemon

```bash
boid project add 'https://github.com/you/boid-demo.git' --workspace=default
```

`--workspace` is required — it cannot be omitted (get-or-create: an unknown slug gets an empty workspace created for it before assignment). Omitting `--name` derives the project name from the URL's last path component (`boid-demo`).

On success, the daemon prints the registered project's ID and workspace:

```
project registered: <uuid> (boid-demo)
  workspace: default
  bare repo: /home/boid/.local/share/boid/...
```

## Registering an existing project

If you already have `.boid/project.yaml` in a pushed repository, skip Step 1 (`project init`) entirely and register the URL directly:

```bash
boid project add 'https://github.com/you/existing-repo.git' --workspace dev
```

If the existing repository does not have `.boid/project.yaml` yet, run Step 1's `boid project init` at its root (committing and pushing the scaffold alongside your existing code), then register as above.

## (Optional) Step 4: Fill in the workspace's contents

If you passed `--workspace dev` in Step 3 and `dev` was newly created, filling in its contents is an **edit**, not a create:

```bash
boid workspace edit dev --from-file dev-workspace.yaml
```

(`boid workspace create dev --from-file ...` would now fail with `409` — `dev` already has a DB row. `create` is only for a slug that doesn't exist yet.)

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

Toolchain install (npm / the claude CLI / the codex CLI, ...) goes through the workspace's `init.sh`, not `additional_bindings`. See the [workspace home guide](../guide/workspace-home.md) and steps 3–4 of [1. Install](01-install.md)'s "Next steps".

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

- **`boid project init`** to scaffold `.boid/project.yaml` locally (does not register with the daemon).
- **`boid project add <git-url> --workspace=<name>`** after pushing, to register with the daemon (`--workspace` required, get-or-create).
- `boid workspace create` / `edit` when a dedicated runtime environment is needed.
- Reloading hand edits with `boid project reload` (or re-fetching a pushed remote with `boid project fetch <ref>`).

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
