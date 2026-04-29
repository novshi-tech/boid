# 4. Feedback loop

This page walks one full PR-driven dev task end to end: an AI agent writes code, opens a PR, waits for CI, reworks the change if CI fails, and merges automatically. It picks up where [3. Projects and extension packages (kits)](03-projects-and-kits.md) left off.

After this tutorial you will have driven `boid`'s core use case — **continuous, AI-driven dev work** — once on the smallest realistic configuration.

## Prerequisites

- You finished [3. Projects and extension packages (kits)](03-projects-and-kits.md) and ran `boid kit install github.com/novshi-tech/boid-kits`.
- The `claude` CLI is on your `PATH` and signed in.
- The `gh` CLI is signed in (`gh auth login`).
- A throw-away GitHub repository cloned locally (e.g. `~/src/github.com/<you>/boid-demo-repo`).
- Ideally that repository has GitHub Actions running some CI on each PR. Without CI you can still walk most of the flow.

## What we are building

A behavior named `dev` that combines four kits:

| Kit | Role |
|---|---|
| `claude-code` | A hook running the Claude Code agent in `executing` and `reworking`. |
| `github-cli` | Lets the sandbox call `gh`. |
| `github-pr-verification` | A `verifying`-state gate that pulls the PR's CI status into the payload. |
| `github-auto-merge` | An entry gate on `done` that runs `gh pr merge`. |

We also set `worktree: true` so each task gets its own git worktree on a fresh branch — keeping the work isolated from other tasks.

## Write project.yaml

Create `~/src/github.com/<you>/boid-demo-repo/.boid/project.yaml`:

```yaml
id: boid-demo
name: boid demo repo

kits:
  - github.com/novshi-tech/boid-kits/claude-code
  - github.com/novshi-tech/boid-kits/github-cli

task_behaviors:
  dev:
    name: dev
    worktree: true
    kits:
      - github.com/novshi-tech/boid-kits/github-pr-verification
      - github.com/novshi-tech/boid-kits/github-auto-merge
    default_instructions:
      main:
        type: execution
        consumer: claude-code
        message: |
          Implement what the task title and description ask for.
          Then commit, push, open a PR, wait for CI, and write the result
          into verification.findings:
            on success:
              echo '{"verification":{"findings":[{"status":"resolved","message":"CI passed"}]}}' \
                | boid task update <task_id> --payload-file -
            on failure (status open, message contains a tail of the failing log):
              echo '{"verification":{"findings":[{"status":"open","message":"CI failed: ..."}]}}' \
                | boid task update <task_id> --payload-file -
      rework:
        type: rework
        consumer: claude-code
        message: |
          Resolve every finding in verification.findings.
          The procedure is the same as in main.
```

`worktree: true` is the key part: `boid` creates a dedicated git worktree on a new branch for this task and confines the hook's work to that worktree. Other dev tasks can run in parallel without colliding.

```bash
cd ~/src/github.com/<you>/boid-demo-repo
boid project add .
```

## Create a task and watch it

A small example: append a line to README.

```bash
boid task create <<'YAML'
project_id: boid-demo
title: Add a one-line "hello from boid" to README.md
behavior: dev
auto_start: true
YAML
```

In another terminal, follow the task:

```bash
boid task watch <task-id>
```

What you should see:

1. `pending → executing` (because of `auto_start`).
2. `executing`: the claude-code hook starts. Inside the worktree, it edits README, commits, pushes, and runs `gh pr create`.
3. Claude writes a `status: resolved` finding to `verification.findings` once CI passes.
4. `executing → verifying` (note: the completion signal here is the finding being written, not `artifact`).
5. `verifying`: the `github-pr-verification` gate confirms the PR's mergeable status.
6. With nothing unresolved, `verifying → done`.
7. The `github-auto-merge` entry gate on `done` runs `gh pr merge`.

Final state:

```bash
boid task show <task-id>
```

The PR is merged; the worktree has been torn down.

## When CI fails (the rework cycle)

If you make a small breaking change (e.g. a test you know will fail) and re-run the same kind of task, steps 3–5 above branch like this:

3'. Claude writes a `status: open` finding to `verification.findings`.
4'. `executing → verifying` (same as above).
5'. The verifying-state gate sees the open finding and bounces the task to `reworking`.
6'. `reworking`: the claude-code rework hook runs (using `default_instructions.rework`). It reads the failure detail in the finding's message, fixes it, and pushes a new commit. CI runs again.
7'. If CI passes, the finding is rewritten as `resolved`, and the task progresses `reworking → verifying → done`.

`boid task watch <task-id>` shows this back-and-forth in real time. If the rework count exceeds `state_machine.rework_limit` (default 5), the task auto-aborts.

## Recap

What this tutorial threaded together:

- `worktree: true` to give each task its own branch and directory, so concurrent runs do not collide.
- The `claude-code` hook to implement in `executing` and to fix in `reworking`.
- The `github-pr-verification` gate to land CI status in the payload.
- The `github-auto-merge` gate to merge automatically when the task lands in `done`.

You have now run the core "AI agent drives a development task to completion" loop end to end with the smallest realistic configuration.

## What to read next

- [Concepts](../guide/concepts.md) — re-read with concrete examples in mind.
- [State machine](../guide/state-machine.md) — the exact rules for `verifying ↔ reworking`.
- [Web UI](../guide/web-ui.md) — to watch a task from a browser or your phone.
- [Troubleshooting](../guide/troubleshooting.md) — when something gets stuck.
