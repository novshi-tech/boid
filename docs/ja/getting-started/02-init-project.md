# Repository の登録と workspace 設定

daemon は `.boid/project.yaml` と `project.local.yaml` を読みません。実行設定は workspace DB から解決します。`boid workspace apply -f workspace.yaml` で envelope を適用し、`boid workspace export <slug>` で現在の定義を取得します。既存 project の ID は維持し、新規 ID は repository URL から、名前は `--name` または URL から決まります。

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
