# 4. Your first task

This page hands a small question to the Claude Code agent and walks through `boid`'s core use case in the shortest possible loop: file a task, the AI runs it inside a sandbox, the result lands on the task. It takes about five minutes.

This page assumes you have completed [3. Set up the Web UI](03-web-ui.md).

## What this tutorial covers

- Filing a small question task that starts immediately via `auto_start: true`.
- Observing progress from both the CLI (`boid task watch`) and the Web UI.
- Inspecting the result written to `payload.artifact`.

## Confirm claude is signed in

This assumes you already registered an `init.sh` for the workspace in Step 4 of [2. Initialize a project](02-init-project.md) (skip that and the command below fails with `CLI not found`).

The claude harness running inside the sandbox does not see your host's `~/.claude` credentials directly — each workspace has its own `$HOME` on its own volume. If you have not signed in yet for the workspace your project is assigned to (`default` unless you set `--workspace`), do that once first:

```bash
boid agent claude -p boid-demo
```

(If this is the workspace's first-ever dispatch, this same command also triggers `init.sh` to install claude before opening the session — it may take a little longer than usual.) Sign in with `/login` (or however your session prompts) once the interactive session opens, then exit (`Ctrl-D` or end the session). This is the same step as "Next steps" item 4 in [1. Install](01-install.md) — sign in once per workspace, and later tasks against that same workspace skip this step.

## Grab the project ID

`boid task create` needs the project's ID (a uuid) in the `project_id` field. `boid project add` (see [2. Initialize a project](02-init-project.md)) printed it as `project registered: <uuid> (boid-demo)` at the very end. If you have lost the output:

```bash
boid project list
```

In the steps below, replace `<project-id>` with that uuid.

## Run one task

Create a task and have it start automatically:

```bash
boid task create <<YAML
project_id: <project-id>
title: What is Linux, in one sentence?
behavior: supervisor
auto_start: true
YAML
```

`auto_start: true` skips `pending` and goes straight to `executing`. Note the task ID it prints — referred to as `<task-id>` below.

### Watch from the CLI

In another terminal, follow the task:

```bash
boid task watch <task-id>
```

After a moment the hook job runs Claude. Following the template instruction `boid project init` wrote into `.boid/project.yaml`, the agent calls `boid task update` to write `artifact`, and once the hook exits cleanly the auto-transition moves the task `executing → done`.

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

- Skipping `pending` with `auto_start: true` and going straight to execution.
- What triggers the auto-transition `executing → done` (a clean hook exit plus the `artifact` trait).
- That the same task can be followed from either the CLI or the Web UI.

This is the end of the Getting started tutorials. For richer project shapes see [Workflows](../../workflows.md), and for per-field detail see the [Reference](../reference/project-yaml.md).

## Cleanup

```bash
boid task delete <task-id>
boid project remove boid-demo
rm -rf ~/boid-demo
```
