# Workspace configuration reference

The daemon does not read `.boid/project.yaml` or `project.local.yaml`. Runtime configuration comes from the workspace database. Apply a workspace envelope with `boid workspace apply -f workspace.yaml`; export the current definition with `boid workspace export <slug>`. Existing project IDs remain unchanged. New IDs come from the normalized repository URL; names come from `--name` or the URL.

## Example

```yaml
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: dev
spec:
  default_task_behavior: implement
  task_behaviors:
    implement:
      readonly: false
      default_instruction:
        agent: codex
        message: Implement the requested change and report through boid task notify.
      hooks:
        - id: implement
          kind: agent
          agent: codex
  triggers:
    - name: sweep
      every: 10m
      run: echo sweep
  services: [jira-cloud]
  signals:
    sources:
      - connector: jira-cloud/assigned-issues
        service: jira-cloud
        every: 10m
  card_commands:
    discuss:
      label: Discuss
      card_write: true
      run: echo discuss
  card_events:
    command: discuss
```

| `spec` field | Meaning |
|---|---|
| `task_behaviors` | Named task behaviors, including `readonly`, `traits`, `default_instruction`, and `hooks`. Names are compared verbatim. |
| `default_task_behavior` | Behavior used when task creation omits an explicit behavior. |
| `base_branch` | Branch template; `${TASK_REMOTE_ID}` is expanded at task creation. Empty uses the origin default branch at agent checkout. `${current_branch}` is retired. Existing task branch snapshots do not change when the workspace changes. |
| `fork_point` | Optional fork point. |
| `triggers` | Scheduled or event triggers with `name`, `every` or `on`, `run`, and optional `timeout`. Workspace schedules run once per workspace, regardless of its project count. |
| `signals.sources` | Connector sources with `connector`, `service`, `every`, and optional `config`; polling triggers are derived when resolving the workspace. |
| `card_commands` | Named commands with `label`, `run`, and optional `card_write`; declaration order survives apply/export and DB storage. |
| `card_events.command` | Card command selected for automatic judgment. It must reference a declared command. The built-in judge applies when no custom command is configured. |
| `env` | Workspace environment variables. |
| `host_commands` | Names referring to daemon host command definitions. |
| `capabilities` | Optional sandbox capabilities such as Docker. |
| `allowed_domains` | Additional proxy egress domains. |
| `services` | Enabled API gateway services, added to the daemon floor. |
| `projects` | Registered project assignments by name; use `boid project add <git-url> --workspace=<slug>` to register a repository. |

Omitted fields preserve current settings on apply. Explicit empty values clear them. Workspace definitions are the sole configuration source; repository files cannot override behaviors, triggers, signals, commands, or card events. Duplicate explicit/derived trigger names are rejected within a workspace.

A hook uses either `command` (shell) or `kind: agent` plus `agent`. Agent hooks cannot also set `command`. `default_instruction` supplies the agent, model, type and bootstrap message for task jobs. Runtime definitions are resolved at dispatch; changing workspace settings needs no repository fetch.

The daemon no longer maintains mirrors. `boid project fetch` and `boid project reload` have retired. Repository registration stores a URL and jobs clone through the git gateway. `boid project init` and `boid project migrate` have retired; write and apply a workspace envelope instead.

## task_behaviors

Define free behavior names under `spec.task_behaviors`. `readonly` defaults to true; set it explicitly for implementation tasks. `default_task_behavior` selects the default name.

## hooks

Hooks run during `executing`. Use `command` for shell hooks or `kind: agent` with an adapter `agent`. `on` is retired on hooks.

## triggers

Triggers use `every` for schedules or `on: signals` / `on: queues` for predicates. Names must be unique within the workspace. `timeout` bounds a round; single-flight prevents overlapping runs.

## signals

`signals.sources` derives polling triggers named `signal:<pack>/<connector>`. Sources require `connector`, `service`, and `every`. Enable the service with `spec.services`.

## card_commands

Declare ordered command keys with `label` and `run`. `card_write: true` permits a command continuation to update the owning card.

## card_events

`card_events.command` must name a declared command. If unspecified, workspace cards use the built-in judge.

`extra_repos` has retired. Register read-only repositories with `boid project add <git-url> --workspace=<name> --perm=fetch` and include their names in `spec.projects`. Migration 0063 converts existing entries into registry rows.
