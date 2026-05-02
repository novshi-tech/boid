---
name: boid-sandbox
description: boid オーケストレータのサンドボックス環境でタスクを実行する。
  ~/.boid/context/ にタスクコンテキストが存在する場合に使用する。
  コンテキストファイルからタスク状態と指示を読み取り、状態に応じた作業を行う。
---

# boid Sandbox

## コンテキスト

| ファイル | 内容 |
|---------|------|
| `~/.boid/context/task.yaml` | タスク ID、タイトル、説明、ステータス、behavior |
| `~/.boid/context/instructions.yaml` | あなた宛の指示（配列） |
| `~/.boid/context/payload.yaml` | ペイロード全体（既存の成果物・検証結果） |
| `~/.boid/context/environment.yaml` | サンドボックス制約（RO/RW、ネットワーク、ツール） |

まず `task.yaml` と `instructions.yaml` を読み、タスクを把握すること。

## 出力

結果は behavior に応じた経路で出力する（plan は `boid task create` builtin、dev は dev-pr-flow skill に従う等）。
payload_patch.json は boid 内部の実装詳細であり、通常は意識しなくてよい。

## 状態と行動

`task.yaml` の `status` フィールドで現在の状態を確認する。
各状態で何をすべきかは [state-machine.md](references/state-machine.md) を参照。

## ルール

- `instructions` trait は出力に含めない（読み取り専用）
- `environment.yaml` の制約に従う
- `environment.yaml` の `worktree: true` の場合、作業内容を必ず git commit してから終了すること（タスク完了時に worktree は削除されるため）
