---
name: boid-sandbox
description: Runs tasks in the boid orchestrator sandbox environment.
  Used when a task context exists in ~/.boid/context/.
  Reads task state and instructions from context files and performs work according to the current state.
---

# boid Sandbox

## Context

| File | Contents |
|---------|------|
| `~/.boid/context/task.yaml` | Task ID, title, description, status, behavior |
| `~/.boid/context/instructions.yaml` | Instructions addressed to you (array) |
| `~/.boid/context/payload.yaml` | Full payload (existing artifacts, verification results) |
| `~/.boid/context/environment.yaml` | Sandbox constraints (RO/RW, network, tools) |

Start by reading `task.yaml` and `instructions.yaml` to understand the task.

## Output

Deliver results via the route appropriate for the behavior (plan uses the `boid task create` builtin, dev follows the dev-pr-flow skill, etc.).
payload_patch.json is an internal boid implementation detail and can normally be ignored.

## State and Actions

Check the current state in the `status` field of `task.yaml`.
See [state-machine.md](references/state-machine.md) for what to do in each state.

## Progress Reporting

During long-running work you can leave a progress note on the task timeline at any time:

```
boid task notify <task_id> --progress "<message>"
```

- **状態遷移なし** — executing は executing のまま
- **通知音なし** — notify.command は呼ばれない
- **タイムラインに記録** — Web UI / TUI の task detail でイベント行として表示される

Q&A (`--ask`) との使い分け:

| フラグ | 状態遷移 | hook 発火 | 用途 |
|---|---|---|---|
| (なし) FYI | なし | する | 外部通知だけ送りたい |
| `--ask` | executing → awaiting | する | ユーザの判断を待つ |
| `--progress` | なし | **しない** | タイムラインに中間報告だけ残す |

推奨タイミング:
- 長時間ジョブの節目ごとの中間報告
- 多段階処理で各フェーズ完了時
- エラーから recover する経路を選択した直後

## Rules

- Do not include the `instructions` trait in output (read-only)
- Follow the constraints in `environment.yaml`
- When `environment.yaml` has `worktree: true`, always git commit your work before exiting (the worktree is deleted when the task completes)
