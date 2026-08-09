---
name: boid-cli-task
description: >
  boid の task / job / action をホスト側 (boid CLI を直接叩けるマシン上、job
  サンドボックスの外) から作成・追跡・操作する。「boid でタスクを作って」
  「タスクの状態を見て」「awaiting のタスクに答えて」「job のログを見て」
  「stuck っぽいタスクを調べて」など、boid のタスク/ジョブをホストの通常セッション
  から操作するときに使用。サンドボックス内で動く boid-task / boid-orchestrate
  (`$BOID_TASK_ID` を持つ agent 自身の挙動を定義するスキル) とは別物 — こちらは
  タスクの**外部**から、ユーザー自身の視点で操作する。
---

# boid-cli-task — task / job 管理 (host 側)

このスキルは **boid job サンドボックスの外**、boid CLI をホストで直接叩く前提。
`$BOID_TASK_ID` は無い（自分自身がタスクではない）。正確なフラグは
`boid <command> --help` で確認する。詳しくは boid リポジトリ内なら
`docs/ja/reference/cli.md` を参照。

## `/boid-task` / `/boid-orchestrate` との違い

| スキル | 動作コンテキスト | 用途 |
|---|---|---|
| このスキル (`boid-cli-task`) | ホスト、`$BOID_TASK_ID` 無し | ユーザー自身が host からタスクを作成・監視・操作する |
| `boid-task` (sandbox 埋め込み) | job サンドボックス内、自分がそのタスクの agent | タスク自身の実装/orchestration ロジック |
| `boid-orchestrate` (sandbox 埋め込み) | `boid exec` セッション (サンドボックス内だが task-less) | サンドボックス内から外部委譲する場合 |

## コマンド一覧

### Task

| コマンド | 役割 |
|---|---|
| `boid task list [--status STATUS] [--workspace ID] [--behavior NAME]` | タスク一覧 |
| `boid task create [-f FILE]` | YAML を stdin (または `-f`) で渡して作成 |
| `boid task show <id> [--field PATH]` | 詳細。`--field` で dotted path 1 フィールドのみ抽出 (例: `--field status`, `--field payload.artifact.report`) |
| `boid task watch <id> [--interval DURATION]` | status/payload の変化をライブ表示 (host からは使える。サンドボックス内では unsupported) |
| `boid task update <id> [-f FILE \| --patch-file FILE]` | 更新 |
| `boid task delete <id> [--force]` | 削除 (active 中は `--force` が要る) |
| `boid task duplicate <source_id> [--auto-start]` | 複製 |
| `boid task reopen <id> [-m MSG]` | done のタスクを executing に戻し instruction を追加 |
| `boid task rerun <id> [--auto-start]` | done/aborted のタスクを pending にリセットして同じ ID で再実行 |
| `boid task notify <id> --message MSG [--done\|--fail\|--progress\|--ask Q]` | 通知 (通常は agent 自身が呼ぶもので、host から手動で叩くのは稀) |
| `boid task answer --task ID --question-id ID --answer TEXT` | `awaiting` のタスクに回答 |
| `boid task import [-f FILE] [--project ID]` | JSONL から一括インポート |
| `boid task hook list <task-id> [--status STATUS]` | このタスクで発火する hook 一覧 |
| `boid task hook replay <task-id> <hook-id>` | 特定 hook を再実行 (中断からの復旧) |
| `boid task artifacts <id> [--field PATH]` | `payload.artifact` を整形表示 |
| `boid task tree [<id>]` | 親子タスクのツリー表示 |

### Job / Action

| コマンド | 役割 |
|---|---|
| `boid job list --task <task-id>` | 指定タスクで動いた全ジョブ |
| `boid job show <job-id>` | 1 ジョブの詳細 (status/exit_code/output) |
| `boid job watch <job-id> [--interval DURATION]` | 終了するまで待つ |
| `boid job log <job-id>` | transcript ログ (実行ストリーム全文) |
| `boid action send --task <id> --type <start\|done\|reopen\|abort> [--payload FILE]` | 手動状態遷移 |

## 手順・落とし穴

### 新しいタスクを作る

```bash
boid project list                      # 委譲先候補を確認
boid project behaviors <project-ref>   # 実在する behavior 名を確認 (supervisor/executor を決め打ちしない)

TASK_ID=$(boid task create <<YAML | awk '{print $3}'
title: <タイトル>
behavior: <確認した behavior 名、省略可 (default_task_behavior に任せる)>
project_id: <project-id>   # 省略可、省略時は default workspace の project
auto_start: true
description: |
  <詳細な依頼内容>
YAML
)
echo "created: $TASK_ID"
```

### 進捗を追う

Host からは `boid task watch <id>` がそのまま使える（サンドボックス内のスキルと
違い、Monitor ツールでのポーリング回避は必須ではない）。ワンショットで確認する
だけなら:

```bash
boid task show "$TASK_ID" --field status
boid task show "$TASK_ID" -o json | jq .
```

### `awaiting` になったら回答する

```bash
question=$(boid task show "$TASK_ID" --field awaiting.question)
question_id=$(boid task show "$TASK_ID" --field awaiting.question_id)
echo "$question"
boid task answer --task "$TASK_ID" --question-id "$question_id" --answer "<回答>"
```

### `done`/`aborted` の内容を見る

```bash
boid task show "$TASK_ID" --field payload.artifact.report   # 構造化レポート
boid task show "$TASK_ID" --field lifecycle.abort.message   # aborted の場合の理由
last_job=$(boid job list --task "$TASK_ID" -o json | jq -r '.[-1].id')  # job list は created_at 昇順なので最後の要素が最新
boid job log "$last_job" | tail -200                        # 生ログで裏取り
```

### stuck っぽいタスクを疑う

`executing` のまま動きが無いタスクは、対応する最新 job の
`transcript_idle_seconds` を見る。`boid job show` に `--field` は無い
(`boid task show` 専用のフラグ) ので `-o json` + `jq` で抜く:

```bash
last_job=$(boid job list --task "$TASK_ID" -o json | jq -r '.[-1].id')  # job list は created_at 昇順なので最後の要素が最新
boid job show "$last_job" -o json | jq -r '.status'
boid job show "$last_job" -o json | jq -r '.transcript_idle_seconds'
```

`boid stop` 等で hook が中断された場合は `boid task hook list <task-id>` で
再発火可能な hook を確認し、`boid task hook replay <task-id> <hook-id>` で復旧する。

### 出力を jq で加工する

`-o json` はほぼ全コマンドに付けられる:

```bash
boid task list -o json | jq '.[] | select(.status=="executing")'
```

## 関連スキル

- [`boid-cli-workspace`](../boid-cli-workspace/SKILL.md) — workspace / project 管理
- [`boid-cli-daemon`](../boid-cli-daemon/SKILL.md) — daemon ライフサイクル・Web UI
