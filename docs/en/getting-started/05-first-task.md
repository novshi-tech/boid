# 5. Your first task

This page hands a small question to the Claude Code agent and walks through `boid`'s core use case in the shortest possible loop: file a task, the AI runs it inside a sandbox, the result lands on the task. It takes about five minutes.

This page assumes you have completed [4. Set up a kit](04-kits.md).

## What this tutorial covers

- Filing a small question task that starts immediately via `auto_start: true`.
- Observing progress from both the CLI (`boid task watch`) and the Web UI.
- Inspecting the result written to `payload.artifact`.

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

After a moment the hook job runs Claude. Following the instruction wired up in [4. Set up a kit](04-kits.md), the agent calls `boid task update` to write `artifact`, and once the hook exits cleanly the auto-transition moves the task `executing → done`.

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

Next: a GitHub PR-driven dev workflow that combines a worktree with auto-merge.

## Cleanup

```bash
boid task delete <task-id>
boid project remove demo
rm -rf ~/boid-demo
```

`boid kit remove github.com/novshi-tech/boid-kits` removes the installed repository, but it is convenient to keep it around for the next tutorial.

---

Next: [6. The GitHub PR-driven dev workflow](06-dev-workflow.md)
