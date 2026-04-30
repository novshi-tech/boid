# 4. The GitHub PR-driven dev workflow

This page runs a GitHub PR-driven dev workflow end to end: an AI agent works on a fresh branch, opens a PR, waits for CI to finish, writes the result back into the task payload, and `boid` finally merges the PR automatically. All in one task. It picks up where [3. Projects and extension packages (kits)](03-projects-and-kits.md) left off.

After this tutorial you will have run the smallest realistic configuration of one of `boid`'s primary use cases: **delegating a dev task to an AI agent**.

## Prerequisites

- You finished [3. Projects and extension packages (kits)](03-projects-and-kits.md) and ran `boid kit install github.com/novshi-tech/boid-kits`.
- The `claude` CLI is on your `PATH` and signed in.
- The `gh` CLI is signed in (`gh auth login`).
- A throw-away GitHub repository cloned locally (e.g. `~/src/github.com/<you>/boid-demo-repo`).
- Ideally that repository runs GitHub Actions on each PR. Without CI you can still walk most of the flow.

## Who does what

In the configuration below, most of the workflow is encoded as **instructions to the agent**. `boid` itself only takes care of creating a per-task worktree and running the final merge gate; the agent does the editing, committing, pushing, opening the PR, waiting for CI, and writing the result back.

| Component | Role |
|---|---|
| `boid` itself (`worktree: true`) | Creates a per-task git worktree on a new branch, then cleans it up. |
| `claude-code` kit (hook) | Launches the Claude Code agent in `executing`. |
| `github-cli` kit | Lets the sandbox use `gh`. |
| **Instructions to the agent** | Edit, commit, push, open the PR, wait for CI, write the outcome to `verification.findings`. |
| `github-auto-merge` kit (gate) | An entry gate on `done` that runs `gh pr merge`. |

Putting the bulk of the workflow into instructions means you can adjust the workflow content — what to verify, where to stop, what to log — per project, in plain text. That is why no specific PR-creation or CI-verification kit is needed here: the agent does it from the instructions.

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
      - github.com/novshi-tech/boid-kits/github-auto-merge
    default_instructions:
      main:
        type: execution
        consumer: claude-code
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
             (returns immediately if there is no CI). On failure run
             `gh run view --log-failed` to capture the log.
          5. Write the outcome to verification.findings:
             on success:
               echo '{"verification":{"findings":[
                 {"status":"resolved","message":"CI passed: '"$PR_URL"'"}
               ]}}' | boid task update <task_id> --payload-file -
             on failure (status=open, message contains a summary plus a tail of the failing log):
               echo '{"verification":{"findings":[
                 {"status":"open","message":"CI failed: <jobs>\n<tail>"}
               ]}}' | boid task update <task_id> --payload-file -
      rework:
        type: rework
        consumer: claude-code
        message: |
          Resolve every finding in verification.findings.
          The procedure is the same as in main (commit → push → wait for CI → write the finding back).
          Reuse the existing PR (check with `gh pr list --head`).
```

What is going on:

- **Top-level `kits:`** loads the kits used across the whole project (`claude-code` and `github-cli`).
- **`task_behaviors.dev`** is the focus here.
  - `worktree: true` gives each task its own branch and directory.
  - Only `github-auto-merge` is listed under the behavior's kits. PR creation and CI checking come from the instructions.
- **`default_instructions.main`** is what `executing` passes to claude-code. The full sequence — commit, push, open the PR, wait for CI, record the result — lives in this text.
- **`default_instructions.rework`** is what `reworking` passes to claude-code when fixing things up.

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
2. `executing`: `boid` creates the worktree on a new branch and the claude-code hook starts.
3. The agent edits the file, commits, pushes, runs `gh pr create`, runs `gh pr checks --watch`, and writes the result back as a finding.
4. With a `status: resolved` finding present and no open ones, the task moves `executing → verifying` automatically.
5. `verifying` has no handler attached, so with no open findings it falls straight through to `done`.
6. The `github-auto-merge` entry gate on `done` runs `gh pr merge` and the PR lands.

Final state:

```bash
boid task show <task-id>
```

The PR is merged; the worktree has been torn down.

## When CI fails

If the agent writes a `status: open` finding to `verification.findings` instead, steps 4 and beyond branch like this:

- 4'. An `executing`-sourced open finding remains, so the auto-transition `executing → reworking` fires.
- 5'. `reworking`: claude-code's rework hook starts and receives `default_instructions.rework`. It reads the failure detail in the finding, fixes the code, pushes again, waits for CI, and rewrites the finding to `resolved`.
- 6'. With all findings resolved, the task moves `reworking → verifying → done` and gets auto-merged.

If the rework count exceeds `state_machine.rework_limit` (default 5), the task auto-aborts.

The exact transition rules between `verifying` and `reworking` are documented in [State machine](../guide/state-machine.md).

## Why this shape

The key insight here is that **most of the workflow is written in the instructions**, not in dedicated handlers.

- You don't need a separate verification kit — instruct the agent to interpret CI results and write the finding back.
- You can adjust per-project how much to automate, which failures should trigger a rework cycle, and how much detail to log, all in plain text.
- `boid` itself is responsible only for driving the state machine and for the worktree/auto-merge bookends. The kit + instructions combination is what shapes the actual workflow.

Variants that introduce a separate review-agent step, static analysis, or security checks will be covered in later tutorials.

## What to read next

- [Concepts](../guide/concepts.md) — re-read with concrete examples in mind.
- [State machine](../guide/state-machine.md) — the exact rules for `verifying ↔ reworking`.
- [Web UI](../guide/web-ui.md) — to watch a task from a browser or your phone.
- [Troubleshooting](../guide/troubleshooting.md) — when something gets stuck.
