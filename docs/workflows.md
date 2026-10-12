# Workflows

Runtime workflows belong to a workspace envelope. The daemon does not read repository project.yaml files. Apply an example with `boid workspace apply -f workflow.yaml`, then register repositories with `boid project add <git-url> --workspace=<example-name>`.

Behavior names are free-form. `readonly` selects the boid-task supervisor/executor mode; names do not select it. Every repository job starts in a fresh checkout. An empty `base_branch` uses the origin default branch at checkout; the agent may then create its working branch. Record actual branches and pushed commits in `payload.artifact.report`.

A readonly planner reviews results and manages children. Repository edits and integration operations belong to writable executor tasks. Each task follows the boid-task lifecycle and reports through `boid task notify --done` or `--fail`; questions use `boid task ask`.

These examples select a policy through instructions. Tool installation, harness login and enabled forge API services are configured separately in the workspace; see [onboarding](en/guide/onboarding.md). A PR policy needs the owner's configured forge service and authorization to publish and integrate changes.

## Sequential integration

```yaml
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: sequential-integration
spec:
  default_task_behavior: plan
  task_behaviors:
    plan:
      readonly: true
      default_instruction:
        agent: codex
        message: |
          Follow /boid-task for task management and lifecycle.
          Create sequential implementation children. Each child commits and pushes to the agreed integration branch. Verify its report and pushed commits before starting the next child. Delegate any repository edits or integration commands to a writable child.
      hooks:
        - id: plan
          kind: agent
          agent: codex
    implement:
      readonly: false
      default_instruction:
        agent: codex
        message: |
          Follow /boid-task in executor mode. Implement the requested work,
          run relevant tests, commit and push, and report the result.
          Follow the parent instruction for the branch and PR policy.
      hooks:
        - id: implement
          kind: agent
          agent: codex
```

## A PR for each executor

```yaml
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: pr-per-executor
spec:
  default_task_behavior: plan
  task_behaviors:
    plan:
      readonly: true
      default_instruction:
        agent: codex
        message: |
          Follow /boid-task for task management and lifecycle.
          Create sequential implementation children. Each child creates a dedicated branch in its checkout, commits, pushes, opens a PR, and waits for CI. Review the reported PR before arranging integration. Use boid task ask for decisions that need the owner.
      hooks:
        - id: plan
          kind: agent
          agent: codex
    implement:
      readonly: false
      default_instruction:
        agent: codex
        message: |
          Follow /boid-task in executor mode. Implement the requested work,
          run relevant tests, commit and push, and report the result.
          Follow the parent instruction for the branch and PR policy.
      hooks:
        - id: implement
          kind: agent
          agent: codex
```

## A PR for the integration branch

```yaml
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: pr-per-integration
spec:
  default_task_behavior: plan
  task_behaviors:
    plan:
      readonly: true
      default_instruction:
        agent: codex
        message: |
          Follow /boid-task for task management and lifecycle.
          Arrange an integration branch before using it as a task base_branch. Create implementation children sequentially against that branch and verify their pushed commits. Delegate the final PR creation and CI watch to a writable child.
      hooks:
        - id: plan
          kind: agent
          agent: codex
    implement:
      readonly: false
      default_instruction:
        agent: codex
        message: |
          Follow /boid-task in executor mode. Implement the requested work,
          run relevant tests, commit and push, and report the result.
          Follow the parent instruction for the branch and PR policy.
      hooks:
        - id: implement
          kind: agent
          agent: codex
```

Explicit `base_branch` values must exist when startup checkout runs. For `${TASK_REMOTE_ID}` templates, supply `remote_id` when creating the task. Ensure the integration branch exists before dispatching children against it. Parallel children require distinct branches; these examples use sequential children.

See the [workspace configuration reference](en/reference/project-yaml.md) for all fields. Keep deployment, merging and outbound communication within the task owner's authorization.
