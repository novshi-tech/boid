# Workflows

Three reference workflows for `boid` projects, with copy-pasteable `.boid/project.yaml` examples. One project pins one workflow shape, expressed in each behavior's `default_instruction.message`.

> Japanese readers: `supervisor` / `executor` are the canonical behavior names. `plan` / `dev` remain accepted as back-compat aliases with a deprecation warning during the migration period.

Since the branch-policy-simplification project (v0.0.11 Phase 1, v0.0.12 Phase 2), every project-visible job runs in a fresh sandbox clone via the git gateway and checks out `base_branch` directly. There is no project-top `worktree:` field anymore, and the dispatcher does not create a per-task branch. Task-local branches now live entirely in the executor's instruction text — the schema does not distinguish workflows; the recipe does.

## Core idea: pick exactly one PR-flow policy per project

`boid` ships two canonical behaviors:

| Canonical | Legacy alias | Role |
|---|---|---|
| **`supervisor`** | `plan` | Readonly orchestrator. Reads the request, decides what to do, creates child executor tasks, monitors them, integrates the results. Never edits files. |
| **`executor`** | `dev` | Writable implementer. Receives a single focused task and produces an artifact (commit / PR / payload trait). |

How each executor's work is turned into a merge on the base branch is a **project-wide PR-flow policy**. `boid` does not encode that policy in the schema — every workflow uses the same `project.yaml` shape. The three workflows below express three different policies through the `default_instruction.message` of `supervisor` and `executor`.

**Default: the local model.** Out of the box `boid` assumes there is no remote PR review step — the executor commits and pushes a task-local branch, exits cleanly, and the supervisor fast-forward merges the child branch locally. If your project does not require PRs, this is what you want and you should pick workflow 1.

The other two workflows opt into PRs at different granularities:

- Workflow 2 — **1 executor 1 PR**: each executor opens its own PR, waits for CI, and the supervisor merges the PR.
- Workflow 3 — **1 supervisor 1 PR**: each supervisor session owns a single integration branch; all of its executor children merge into that branch, and the supervisor opens one consolidated PR at the end.

Pick whichever matches how your project ships code.

## Common shape

Every workflow below uses the same skeleton:

```yaml
id: <project-id>
name: <Display name>

# Optional — omit to have executor tasks branch from the daemon's current HEAD
# branch. Supports ${TASK_REMOTE_ID} / ${current_branch} interpolation.
base_branch: <optional>

# Kits shared by every behavior.
kits:
  - github.com/novshi-tech/boid-kits/claude-code
  # add github-cli only when you need `gh` in the sandbox

task_behaviors:
  supervisor:
    default_instruction: { ... }
  executor:
    default_instruction: { ... }
```

The `default_instruction.message` field is where each workflow's actual policy lives. In particular:

- **Task-local branch creation lives in the executor's instruction**, not in the dispatcher. The idiomatic snippet is:

  ```bash
  BRANCH="boid/${BOID_TASK_ID:0:8}"
  # Adopt origin/<branch> when it exists so a `reopen` (fresh clone) continues
  # the same branch history it pushed to in a prior dispatch; fall back to
  # creating fresh from base_branch on the first dispatch.
  git checkout -b "$BRANCH" "origin/$BRANCH" 2>/dev/null || git checkout -b "$BRANCH"
  ```

- **PR / merge orchestration lives in the supervisor's instruction** — whether to look up a PR, whether to fast-forward merge locally, whether to open one consolidated PR at the end, and so on.

The instructions below are abridged; in real projects you also include things like "run `go vet ./...` before committing", "if CI fails, run `gh run view --log-failed`", or "review the PR with the `/boid-review` skill before merging".

## Workflow 1 — Local model (default)

**Use this when:** your project does not gate merges behind GitHub PR review. The supervisor agent has authority to merge directly to the base branch.

**Dependencies:** local git only. No `gh` CLI, no GitHub remote required.

**Shape:**

1. Supervisor reads the request and creates one or more executor tasks.
2. Each executor task runs in an isolated fresh clone, checks out a task-local branch, commits, pushes it back through the gateway, and exits cleanly.
3. Supervisor watches each executor reach `done`, then `git fetch origin && git merge --ff-only origin/boid/<child_id8>` in the project root to bring the change into the base branch.
4. Supervisor moves on to the next child or exits.

**`project.yaml`:**

```yaml
id: local-demo
name: Local Demo
# base_branch omitted → executor cuts from the daemon's current HEAD branch

kits:
  - github.com/novshi-tech/boid-kits/claude-code

task_behaviors:
  supervisor:
    default_instruction:
      agent: claude-code
      model: opus
      message: |
        Follow the /boid-task skill (Supervisor mode).

        Integration policy: LOCAL.
        - Each child executor task commits and pushes to its own
          task-local branch (`boid/<child_id8>`, first 8 chars of the
          child task id) and exits.
        - Once a child reaches `done`, fast-forward merge its branch
          into the current branch in the project root:
            git fetch origin
            git merge --ff-only origin/boid/<child_id8>
          If the merge is not fast-forward, `boid task reopen
          <child_id> -m "<focused rebase instruction>"` and resume
          monitoring; if that still does not produce a clean merge,
          escalate via `boid task ask`.
        - Spawn the next child (or exit) as the /boid-task skill
          describes.
  executor:
    default_instruction:
      agent: claude-code
      model: sonnet
      message: |
        Follow the /boid-task skill (Executor mode) to implement what
        is described in task.yaml's title and description.

        First, isolate your work on a task-local branch. The in-sandbox
        clone checks out `base_branch` itself, so per-task branch
        creation lives here in the executor instruction:

            BRANCH="boid/${BOID_TASK_ID:0:8}"
            git checkout -b "$BRANCH" "origin/$BRANCH" 2>/dev/null || git checkout -b "$BRANCH"

        When implementation is complete:
        1. Run the project's release step (`go vet ./...` and
           `go test ./...` for a Go project, `npm test` for npm, ...).
           Fix any failures.
        2. `git add` + `git commit`.
        3. `git push -u origin HEAD` so the parent supervisor's clone
           can see the branch. Exit cleanly.
```

**Why it works:** there is no external review system, so there is also no need to round-trip through one. The supervisor agent is the merge authority, and its policy (fast-forward merge, escalate on conflict) is encoded in its instruction text.

## Workflow 2 — 1 executor 1 PR

**Use this when:** you want one GitHub PR per logical change, your project requires CI to pass before merge, and each executor's change is small enough to ship on its own.

**Dependencies:**
- GitHub remote configured (`origin`).
- `gh` CLI signed in (`gh auth login`).
- The `github-cli` kit attached so the sandbox can call `gh`.

**Shape:**

1. Supervisor decomposes the request into multiple executor tasks, each scoped to a single PR's worth of change.
2. Each executor opens a PR (`gh pr create`), waits for CI (`gh pr checks --watch --fail-fast`), and exits cleanly when CI passes.
3. Supervisor watches each executor reach `done`, then runs `gh pr merge --merge --delete-branch` for the corresponding PR and pulls the merged commit into the project root.
4. Supervisor spawns the next executor (or exits) as needed.

This is the workflow the `boid` repository itself uses; see [`.boid/project.yaml`](https://github.com/novshi-tech/boid/blob/main/.boid/project.yaml) in the boid repo for the current canonical recipe.

**`project.yaml`:**

```yaml
id: pr-per-executor-demo
name: PR-per-executor demo
# base_branch omitted → executor cuts from the daemon's current HEAD branch
# (typically `main` once `git fetch && git pull --ff-only` is up to date)

kits:
  - github.com/novshi-tech/boid-kits/claude-code
  - github.com/novshi-tech/boid-kits/github-cli

task_behaviors:
  supervisor:
    default_instruction:
      agent: claude-code
      model: opus
      message: |
        Follow the /boid-task skill (Supervisor mode).

        Integration policy: PR PER EXECUTOR.
        Once a child executor task reaches `done`:
        1. Locate the PR via `gh pr list --head boid/<child_id8>` (first
           8 chars of the child task id).
        2. Review before merging. Run the `boid-review` skill on the PR
           and `/code-review` for generic bugs. Merge only on a GO
           verdict. On NO-GO, `boid task reopen <child_id> -m
           "<the finding>"` instead of merging.
        3. If open, mergeable, and reviewed GO: `gh pr merge --merge
           --delete-branch`, then `git fetch origin && git pull --ff-only
           origin main`, verify, and continue.
        4. If not mergeable:
           - Plain conflict / CI failure with a focused fix:
             `boid task reopen <child_id> -m "<instruction>"` and
             resume monitoring.
           - Anything ambiguous: `boid task notify "$BOID_TASK_ID"
             --ask "..."` the user; do not guess.

        Treat aborted children per /boid-task's existing guidance
        (no merge attempt; diagnose and either retry or escalate).
  executor:
    default_instruction:
      agent: claude-code
      model: sonnet
      message: |
        Follow the /boid-task skill (Executor mode) to implement what
        is described in task.yaml's title and description.

        First, isolate your work on a task-local branch so push does
        not land directly on base_branch. The in-sandbox clone checks
        out base_branch itself, so per-task branch creation lives here:

            BRANCH="boid/${BOID_TASK_ID:0:8}"
            git checkout -b "$BRANCH" "origin/$BRANCH" 2>/dev/null || git checkout -b "$BRANCH"

        This project ships changes via the PR-per-executor model.
        When implementation is complete:
        1. `git add` + `git commit`.
        2. `git push -u origin HEAD`.
        3. Check for an existing PR with `gh pr list --head "$BRANCH"`.
           If absent, create one with `gh pr create --title
           "<task title>" --body "task: <task_id>\n\n<summary>"`.
        4. `gh pr checks --watch --fail-fast`. If CI fails, exit
           non-zero and boid will transition to `aborted` (the parent
           supervisor will decide on retry vs. escalation).
        5. On CI success, exit cleanly. The parent supervisor will
           merge the PR as part of its supervise loop.
```

**Why it works:** each PR is small and reviewable, and the GitHub side gives you CI and branch protection for free. The cost is that one feature may become several PRs that need to land in a specific order — that ordering is the supervisor's job.

## Workflow 3 — 1 supervisor 1 PR

**Use this when:** you want one PR per high-level request (one Jira ticket, one GitHub issue, one user-visible feature), but the change is large enough that you want to break implementation into smaller executor steps for parallelism or for clarity. Each supervisor session owns a single integration branch; its executor children merge into that branch and the supervisor opens one consolidated PR at the end.

**Dependencies:**
- GitHub remote (`origin`) and `gh` CLI, same as workflow 2.
- A way to inject `${TASK_REMOTE_ID}` — for example a Jira / GitHub issue link recorded on the supervisor task at creation time (see `boid task create --remote-id ...`).

**Shape:**

1. Supervisor records its `remote_id` (Jira ticket / issue number) at creation. The supervisor's integration branch is `feature/${TASK_REMOTE_ID}`.
2. Supervisor decomposes the request into multiple executor tasks. Each executor cuts a task-local branch from the integration branch (set via project-top `base_branch:`).
3. Each executor commits, pushes, and exits cleanly.
4. Supervisor watches each executor reach `done`, then fast-forward merges the child branch into `feature/${TASK_REMOTE_ID}` and pushes the integration branch.
5. Once all children are done, supervisor opens a single PR from `feature/${TASK_REMOTE_ID}` targeting `main`.

**`project.yaml`:**

```yaml
id: pr-per-supervisor-demo
name: PR-per-supervisor demo

# Executor tasks cut from the parent supervisor's integration branch
# (resolved per task at dispatch time).
base_branch: "feature/${TASK_REMOTE_ID}"

kits:
  - github.com/novshi-tech/boid-kits/claude-code
  - github.com/novshi-tech/boid-kits/github-cli

task_behaviors:
  supervisor:
    default_instruction:
      agent: claude-code
      model: opus
      message: |
        Follow the /boid-task skill (Supervisor mode).

        Integration policy: PR PER SUPERVISOR.
        Setup (before creating the first child):
        - Ensure `feature/${TASK_REMOTE_ID}` exists locally and on origin.
          If absent, create it from `main`:
            git fetch origin
            git checkout -b feature/${TASK_REMOTE_ID} origin/main
            git push -u origin feature/${TASK_REMOTE_ID}

        Per child cycle:
        - When a child executor reaches `done`, fast-forward merge its
          branch (`boid/<child_id8>`) into the integration branch, then
          push:
            git fetch origin
            git checkout feature/${TASK_REMOTE_ID}
            git merge --ff-only origin/boid/<child_id8>
            git push origin feature/${TASK_REMOTE_ID}
          On conflict: reopen the child with a rebase-onto-integration
          instruction, or escalate via `boid task ask`.
        - When all planned children are done, open one PR:
            gh pr create --base main \
              --title "<request title>" \
              --body "Closes: ${TASK_REMOTE_ID}\n\n<summary of children>"
          Then `gh pr checks --watch --fail-fast` and `gh pr merge --merge`.
  executor:
    default_instruction:
      agent: claude-code
      model: sonnet
      message: |
        Follow the /boid-task skill (Executor mode) to implement what
        is described in task.yaml's title and description.

        The in-sandbox clone checks out `feature/${TASK_REMOTE_ID}` (the
        parent supervisor's integration branch). Isolate your work on a
        task-local branch cut from it:

            BRANCH="boid/${BOID_TASK_ID:0:8}"
            git checkout -b "$BRANCH" "origin/$BRANCH" 2>/dev/null || git checkout -b "$BRANCH"

        When implementation is complete:
        1. Run the project's release step.
        2. `git add` + `git commit`.
        3. `git push -u origin HEAD` so the parent supervisor can
           fast-forward merge your branch into the integration branch.
           Do not open a PR — the parent supervisor opens one
           consolidated PR at the end.
```

**Why it works:** reviewers see one logically coherent PR per high-level request, regardless of how many executor steps the supervisor decided to break the work into. The cost is that reviewers see a larger diff, and the integration branch can drift from `main` over a long supervisor session — bake `git rebase origin/main` into the supervisor's per-cycle policy if drift is a concern.

## Which workflow should I pick?

| Question | If yes ⇒ workflow |
|---|---|
| Does the project skip GitHub PR review entirely (solo project / internal scratch repo)? | 1 (Local) |
| Does the project gate every merge on PR + CI, and is each executor's change small enough to ship on its own? | 2 (1 executor 1 PR) |
| Does the project want one PR per Jira ticket / GitHub issue, even when implementation needs several executor steps? | 3 (1 supervisor 1 PR) |

You can mix workflows across projects — one project per workflow shape — but do not mix shapes inside a single project. Since v0.0.12, workflow choice is a matter of the executor and supervisor instruction wording, not a schema flag; consistency across a project therefore lives in the instructions themselves rather than in a top-level toggle.

## Related documents

- [`project.yaml` reference](en/reference/project-yaml.md) — the canonical schema this document builds on.
- [Concepts](en/guide/concepts.md) — vocabulary (task / behavior / kit / worktree / ...).
- [`/boid-task` SKILL](../internal/skills/data/boid-task/SKILL.md) — the unified task agent: supervisor mode (readonly orchestrator) and executor mode (writable implementer), selected from `environment.yaml` `readonly`.
- [branch-policy-simplification plan](plans/branch-policy-simplification.md) — the v0.0.11 + v0.0.12 refactor that retired the `worktree:` field and per-task branch mechanism this doc used to rely on.
