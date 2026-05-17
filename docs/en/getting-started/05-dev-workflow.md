# 5. The GitHub PR-driven dev workflow

This page runs a GitHub PR-driven dev workflow end to end: an AI agent works on a fresh branch, opens a PR, waits for CI to finish, and `boid` finally merges the PR automatically. All in one task. It picks up where [4. Projects and extension packages (kits)](04-projects-and-kits.md) left off.

After this tutorial you will have run the smallest realistic configuration of one of `boid`'s primary use cases: **delegating a dev task to an AI agent**.

## Prerequisites

- You finished [4. Projects and extension packages (kits)](04-projects-and-kits.md) and ran `boid kit install github.com/novshi-tech/boid-kits`.
- The `claude` CLI is on your `PATH` and signed in.
- The `gh` CLI is signed in (`gh auth login`).
- A throw-away GitHub repository cloned locally (e.g. `~/src/github.com/<you>/boid-demo-repo`).
- Ideally that repository runs GitHub Actions on each PR. Without CI you can still walk most of the flow.

## Who does what

In the configuration below, most of the workflow is encoded as **instructions to the agent**. `boid` itself only takes care of creating a per-task worktree and running the final merge gate; the agent does the editing, committing, pushing, opening the PR, and waiting for CI. Deciding what to do when CI fails (abort or wait for an operator-driven reopen) is an agent / operator concern, not a harness concern.

| Component | Role |
|---|---|
| `boid` itself (project-top `worktree: true`) | Creates a per-task git worktree on a new branch for each **executor** task, then cleans it up. |
| `claude-code` kit (hook) | Launches the Claude Code agent in `executing`. |
| `github-cli` kit | Lets the sandbox use `gh`. |
| **Instructions to the agent** | Edit, commit, push, open the PR, wait for CI, abort on failure. |
| `github-auto-merge` kit (gate) | Exit gate on `executing → done` that runs `gh pr merge`. |

Putting the bulk of the workflow into instructions means you can adjust the workflow content — what to verify, where to stop, what to log — per project, in plain text.

## Write project.yaml

Create `~/src/github.com/<you>/boid-demo-repo/.boid/project.yaml`:

```yaml
id: boid-demo
name: boid demo repo

# Project-top: each executor task gets its own worktree on a fresh branch.
worktree: true

kits:
  - github.com/novshi-tech/boid-kits/claude-code
  - github.com/novshi-tech/boid-kits/github-cli

task_behaviors:
  executor:
    name: executor
    kits:
      - github.com/novshi-tech/boid-kits/github-auto-merge
    default_instruction:
      type: execution
      agent: claude-code
      message: |
        Implement what the task title and description ask for.
        Make sure unit tests pass before committing.

        Once the implementation is done, run the following in order:
        1. `git add` and `git commit` on the worktree's branch.
        2. `git push -u origin HEAD` to push the current branch to origin.
        3. Check whether a PR already exists:
           PR_URL=$(gh pr list --head "$(git branch --show-current)" \
                      --json url --jq '.[0].url // ""')
           If empty, create one with `gh pr create --title "<task title>" --body "<summary>"`.
        4. `gh pr checks --watch --fail-fast` until CI finishes
           (returns immediately if there is no CI).
        5. If CI passes, exit cleanly. The `boid job done` trap drives
           the auto-transition `executing → done`.
        6. If CI fails, run `gh run view --log-failed` to inspect the
           failure. If you can fix it, edit the code and start again
           from step 1. If it cannot be fixed, abort the task with
           `boid task abort <task_id> --code ci_failed --message "<summary>"`.
```

What is going on:

- **Project-top `worktree: true`** gives each executor task its own branch and directory. The flag is no longer set per behavior — it applies to every executor task in the project. Supervisor tasks in the same project always run readonly in the project root regardless of this flag.
- **Top-level `kits:`** loads the kits used across the whole project (`claude-code` and `github-cli`).
- **`task_behaviors.executor`** is the focus here. Only `github-auto-merge` is listed under the behavior's kits. PR creation and CI checking come from the instructions. (`executor` is the canonical name; the legacy alias `dev` is still accepted with a deprecation warning.)
- **`default_instruction`** is the single Instruction object that `executing` passes to claude-code. The whole sequence — commit, push, open the PR, wait for CI, decide what to do on failure — lives in this text.
- There is no separate verification or rework instruction. On failure the agent aborts, or an operator runs `boid task reopen <id> --message "..."` to send a new instruction.

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
behavior: executor
auto_start: true
YAML
```

In another terminal, follow the task:

```bash
boid task watch <task-id>
```

What you should see:

1. `pending → executing` (because of `auto_start`).
2. `executing`: `boid` creates the worktree on a new branch and the claude-code hook starts.
3. The agent edits the file, commits, pushes, runs `gh pr create`, runs `gh pr checks --watch`.
4. The agent exits cleanly and the state machine auto-transitions `executing → done`.
5. Just before entering `done`, the `github-auto-merge` exit gate runs `gh pr merge` and the PR lands.

Final state:

```bash
boid task show <task-id>
```

The PR is merged; the worktree has been torn down.

## When CI fails

If the agent's instruction tells it to abort on CI failure, the task ends in `aborted` with `lifecycle.abort.code` / `lifecycle.abort.message` recorded on the task.

An operator can recover with:

```bash
# Send a done task back through executing with a new ask
boid task reopen <task-id> --message "Please fix the lint failure and push again"
```

For aborted tasks, `boid task rerun <id>` resets it to `pending` and runs the original instruction again.

## When auto-merge hits a conflict

If the `github-auto-merge` exit gate's `gh pr merge` call hits a conflict, the gate exits non-zero and the `done` transition is blocked. The task stays in `executing`, so:

```bash
boid task reopen <task-id> --message "Merge the latest main, resolve conflicts, and push again"
```

asks the agent to fix the conflict.

## Why this shape

The key insight here is that **most of the workflow is written in the instructions**, not in dedicated handlers.

- You don't need a separate verification kit — instruct the agent to interpret CI results and decide whether to abort.
- You can adjust per-project how much to automate, which failures end the task, and how much detail to log, all in plain text.
- `boid` itself is responsible only for driving the state machine and for the worktree/auto-merge bookends. The kit + instructions combination is what shapes the actual workflow.

## What to read next

- [Workflows](../../workflows.md) — three end-to-end workflow shapes (local merge / 1 executor 1 PR / 1 supervisor 1 PR) with copy-pasteable `project.yaml` examples.
- [Concepts](../guide/concepts.md) — re-read with concrete examples in mind.
- [State machine](../guide/state-machine.md) — the exact transition rules.
- [Web UI](../guide/web-ui.md) — to watch a task from a browser or your phone.
- [Troubleshooting](../guide/troubleshooting.md) — when something gets stuck.
