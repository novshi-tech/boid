# Register a repository and configure its workspace

The daemon does not read `.boid/project.yaml` or `project.local.yaml`. Runtime configuration comes from the workspace database. Apply a workspace envelope with `boid workspace apply -f workspace.yaml`; export the current definition with `boid workspace export <slug>`. Existing project IDs remain unchanged. New IDs come from the normalized repository URL; names come from `--name` or the URL.

Save the following as `workspace.yaml` and run `boid workspace apply -f workspace.yaml`.

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

Register an existing remote repository:

```bash
boid project add https://github.com/you/boid-demo.git --workspace dev
boid project show boid-demo --explain
```

See the [workspace configuration reference](../reference/project-yaml.md) and [first task tutorial](04-first-task.md).
