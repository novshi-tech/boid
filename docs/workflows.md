# Workflows

Three reference workflows for `boid` projects, with copy-pasteable `.boid/project.yaml` examples. The schema is the one finalised in the "task_behavior simplification" refactor (P0 – P4): one project pins one workflow shape, and that shape is expressed at the project top level rather than per behavior.

> Japanese readers: the supervisor / executor terminology is canonical. `plan` / `dev` are accepted as back-compat aliases with a deprecation warning during the migration period.

## Core idea: pick exactly one workflow per project

`boid` ships two canonical behaviors:

| Canonical | Legacy alias | Role |
|---|---|---|
| **`supervisor`** | `plan` | Readonly orchestrator. Reads the request, decides what to do, creates child executor tasks, monitors them, integrates the results. Never edits files. |
| **`executor`** | `dev` | Writable implementer. Receives a single focused task and produces an artifact (commit / PR / payload trait). |

Whether executor tasks get a worktree, what branch they cut from, and how their work integrates back to the main branch is a **project-wide policy**. It is set at the project top level of `project.yaml` and recorded once, not in each behavior. The three workflows below are three different choices for that policy.

**Default: the local model.** Out of the box `boid` assumes there is no remote PR review step — the executor commits to a worktree branch, exits cleanly, and the supervisor merges the work locally. If your project does not require PRs, this is what you want and you should pick workflow 1.

The other two workflows opt into PRs at different granularities:

- Workflow 2 — **1 executor 1 PR**: each executor opens its own PR, waits for CI, and the supervisor merges the PR.
- Workflow 3 — **1 supervisor 1 PR**: each supervisor session owns a single integration branch; all of its executor children merge into that branch locally, and the supervisor opens one consolidated PR at the end.

Pick whichever matches how your project ships code.

## Common shape

Every workflow below uses the same skeleton:

```yaml
id: <project-id>
name: <Display name>

# Project-wide policy (the bit that differs per workflow).
worktree: true | false
base_branch: <optional, supports ${TASK_REMOTE_ID} / ${current_branch}>

# Kits shared by every behavior.
kits:
  - github.com/novshi-tech/boid-kits/claude-code
  # add github-cli only when you need `gh` in the sandbox

task_behaviors:
  supervisor:
    name: Supervisor
    default_instruction: { ... }
  executor:
    name: executor
    default_instruction: { ... }
```

The `default_instruction.message` field is where the workflow's actual behaviour lives. The instructions below are abridged to focus on the workflow shape; in real projects you also include things like "run go vet before committing" or "if CI fails, run `gh run view --log-failed`".

## Workflow 1 — Local model (default)

**Use this when:** your project does not gate merges behind GitHub PR review. The supervisor agent has authority to merge directly to the working branch.

**Dependencies:** local git only. No `gh` CLI, no GitHub remote required.

**Shape:**

1. supervisor reads the request and creates one or more executor tasks.
2. Each executor task runs in its own worktree (project-top `worktree: true`), commits, and exits cleanly.
3. supervisor watches each executor reach `done`, then runs `git merge --ff-only <executor branch>` (or equivalent) in the project root to bring the change into the working branch.
4. supervisor moves on to the next child or exits.

**`project.yaml`:**

```yaml
id: local-demo
name: Local Demo
worktree: true            # executor tasks get a per-task worktree
# base_branch omitted → executor cuts from the daemon's current HEAD branch

kits:
  - github.com/novshi-tech/boid-kits/claude-code

task_behaviors:
  supervisor:
    name: Supervisor
    default_instruction:
      agent: claude-code
      model: opus
      message: |
        Follow the /boid-task skill (Supervisor mode).

        Integration policy: LOCAL.
        - Each child executor task commits to its own worktree branch and exits.
        - Once a child reaches `done`, fast-forward merge its branch into the
          current branch in the project root:
            git fetch origin
            git merge --ff-only <child-branch>
          If the merge is not fast-forward, run `git rebase origin/<base>` in
          the child branch first; if that still does not produce a clean merge,
          escalate via `boid task notify --ask`.
        - Spawn the next child (or exit) as the /boid-task skill
          describes.
  executor:
    name: executor
    default_instruction:
      agent: claude-code
      model: sonnet
      message: |
        Follow the /boid-task skill (Executor mode) to implement what is described in
        task.yaml's title and description.

        When implementation is complete:
        1. Run the project's release step (`go vet ./...` and `go test ./...`
           for a Go project, npm test for npm, ...). Fix any failures.
        2. `git add` + `git commit` on the worktree's branch.
        3. Exit cleanly. The supervisor will integrate the branch.
```

**Why it works:** there is no external review system, so there is also no need to round-trip through one. The supervisor agent is the merge authority, and its policy (fast-forward merge, escalate on conflict) is encoded in its instruction text.

## Workflow 2 — 1 executor 1 PR

**Use this when:** you want one GitHub PR per logical change, your project requires CI to pass before merge, and each executor's change is small enough to ship on its own.

**Dependencies:**
- GitHub remote configured (`origin`).
- `gh` CLI signed in (`gh auth login`).
- The `github-cli` kit attached so the sandbox can call `gh`.

**Shape:**

1. supervisor decomposes the request into multiple executor tasks, each scoped to a single PR's worth of change.
2. Each executor opens a PR (`gh pr create`), waits for CI (`gh pr checks --watch --fail-fast`), and exits cleanly when CI passes.
3. supervisor watches each executor reach `done`, then runs `gh pr merge --merge --delete-branch` for the corresponding PR and pulls the merged commit into the project root.
4. supervisor spawns the next executor (or exits) as needed.

This is the workflow the `boid` repository itself uses.

**`project.yaml`:**

```yaml
id: pr-per-executor-demo
name: PR-per-executor demo
worktree: true            # each executor gets its own worktree+branch
# base_branch omitted → executor cuts from the daemon's current HEAD branch
# (typically `main` once `git fetch && git pull --ff-only` is up to date)

kits:
  - github.com/novshi-tech/boid-kits/claude-code
  - github.com/novshi-tech/boid-kits/github-cli

task_behaviors:
  supervisor:
    name: Supervisor
    default_instruction:
      agent: claude-code
      model: opus
      message: |
        Follow the /boid-task skill (Supervisor mode).

        Integration policy: PR PER EXECUTOR.
        Once a child executor task reaches `done`:
        1. Locate the PR via `gh pr list --head boid/<task_id8>`.
        2. If the PR is open and mergeable, run
           `gh pr merge --merge --delete-branch`, then
           `git fetch origin && git pull --ff-only origin main`.
        3. If the merge cannot proceed (conflict, CI failure with a clear fix,
           ...): `boid task reopen <child_id> -m "<focused fix instruction>"`
           and resume monitoring it.
        4. Anything ambiguous: `boid task notify "$BOID_TASK_ID" --ask "..."`
           to consult the user.

        Treat aborted children per /boid-task's existing guidance
        (no merge attempt; diagnose and either retry or escalate).
  executor:
    name: executor
    default_instruction:
      agent: claude-code
      model: sonnet
      message: |
        Follow the /boid-task skill (Executor mode) to implement what is described in
        task.yaml's title and description.

        This project ships changes via the PR-per-executor model.
        When implementation is complete:
        1. `git add` + `git commit` on the worktree's branch.
        2. `git push -u origin HEAD`.
        3. Check for an existing PR with `gh pr list --head ...`. If absent,
           create one with `gh pr create --title "<task title>" --body "task:
           <task_id>\n\n<summary>"`.
        4. `gh pr checks --watch --fail-fast`. If CI fails, exit non-zero and
           boid will transition to `aborted` (the parent supervisor will
           decide on retry vs. escalation).
        5. On CI success, exit cleanly. The parent supervisor will merge the
           PR as part of its supervise loop.
```

**Why it works:** each PR is small and reviewable, and the GitHub side gives you CI and branch protection for free. The cost is that one feature may become several PRs that need to land in a specific order — that ordering is the supervisor's job.

## Workflow 3 — 1 supervisor 1 PR

**Use this when:** you want one PR per high-level request (one Jira ticket, one GitHub issue, one user-visible feature), but the change is large enough that you want to break implementation into smaller executor steps for parallelism or for clarity. Each supervisor session owns a single integration branch; its executor children commit into that branch and the supervisor opens one consolidated PR at the end.

**Dependencies:**
- GitHub remote (`origin`) and `gh` CLI, same as workflow 2.
- A way to inject `${TASK_REMOTE_ID}` — for example a Jira / GitHub issue link recorded on the supervisor task at creation time (see `boid task create --remote-id ...`).

**Shape:**

1. supervisor records its `remote_id` (Jira ticket / issue number) at creation. The supervisor's integration branch is `feature/${TASK_REMOTE_ID}`.
2. supervisor decomposes the request into multiple executor tasks. Each executor cuts from `feature/${TASK_REMOTE_ID}` (set via project-top `base_branch:`).
3. Each executor commits to its own worktree branch and exits cleanly.
4. supervisor watches each executor reach `done`, then `git merge --ff-only <executor branch>` into `feature/${TASK_REMOTE_ID}` locally.
5. Once all children are done, supervisor pushes `feature/${TASK_REMOTE_ID}` and opens a single PR.

**`project.yaml`:**

```yaml
id: pr-per-supervisor-demo
name: PR-per-supervisor demo

# Project-top: executor tasks get a worktree, and they cut from the parent
# supervisor's integration branch (resolved per task at dispatch time).
worktree: true
base_branch: "feature/${TASK_REMOTE_ID}"

kits:
  - github.com/novshi-tech/boid-kits/claude-code
  - github.com/novshi-tech/boid-kits/github-cli

task_behaviors:
  supervisor:
    name: Supervisor
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
        - When a child executor reaches `done`, fast-forward merge its branch
          into the integration branch locally:
            git checkout feature/${TASK_REMOTE_ID}
            git merge --ff-only <child-branch>
          On conflict: rebase the child branch onto the integration branch
          first, or reopen the child with a focused fix instruction.
        - When all planned children are done, push the integration branch
          and open one PR:
            git push origin feature/${TASK_REMOTE_ID}
            gh pr create --base main \
              --title "<request title>" \
              --body "Closes: ${TASK_REMOTE_ID}\n\n<summary of children>"
          Then `gh pr checks --watch --fail-fast` and `gh pr merge --merge`.
  executor:
    name: executor
    default_instruction:
      agent: claude-code
      model: sonnet
      message: |
        Follow the /boid-task skill (Executor mode) to implement what is described in
        task.yaml's title and description.

        Your worktree is cut from `feature/${TASK_REMOTE_ID}` (the parent
        supervisor's integration branch). When implementation is complete:
        1. Run the project's release step.
        2. `git add` + `git commit` on the worktree's branch.
        3. Exit cleanly. The parent supervisor will fast-forward merge your
           branch into the integration branch and decide what to do next.
```

**Why it works:** reviewers see one logically coherent PR per high-level request, regardless of how many executor steps the supervisor decided to break the work into. The cost is that reviewers see a larger diff, and the integration branch can drift from `main` over a long supervisor session — bake `git rebase origin/main` into the supervisor's per-cycle policy if drift is a concern.

## Which workflow should I pick?

| Question | If yes ⇒ workflow |
|---|---|
| Does the project skip GitHub PR review entirely (solo project / internal scratch repo)? | 1 (Local) |
| Does the project gate every merge on PR + CI, and is each executor's change small enough to ship on its own? | 2 (1 executor 1 PR) |
| Does the project want one PR per Jira ticket / GitHub issue, even when implementation needs several executor steps? | 3 (1 supervisor 1 PR) |

You can mix workflows across projects — one project per workflow shape — but do not mix shapes inside a single project. The whole point of moving `worktree:` / `base_branch:` to the project top level is to enforce that consistency.

## Related documents

- [`project.yaml` reference](en/reference/project-yaml.md) — the canonical schema this document builds on.
- [Concepts](en/guide/concepts.md) — vocabulary (task / behavior / kit / worktree / ...).
- [`/boid-task` SKILL](../internal/skills/data/boid-task/SKILL.md) — the unified task agent: supervisor mode (readonly orchestrator) and executor mode (writable implementer), selected from `environment.yaml` `readonly`.
