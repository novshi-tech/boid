---
name: boid-sandbox
description: boid オーケストレータのサンドボックス環境でタスクを実行する。
  ~/.boid/context/ にタスクコンテキストが存在する場合に使用する。
  コンテキストファイルからタスク状態と指示を読み取り、状態に応じた作業を行い、
  結果を ~/.boid/output/payload_patch.json に書き出す。
---

# boid Sandbox

## コンテキスト

| ファイル | 内容 |
|---------|------|
| `~/.boid/context/task.yaml` | タスク ID、タイトル、説明、ステータス、behavior |
| `~/.boid/context/instructions.json` | あなた宛の指示（配列） |
| `~/.boid/context/payload.json` | ペイロード全体（既存の成果物・検証結果） |
| `~/.boid/context/environment.yaml` | サンドボックス制約（RO/RW、ネットワーク、ツール） |

まず `task.yaml` と `instructions.json` を読み、タスクを把握すること。

## 出力

`~/.boid/output/payload_patch.json` に JSON を書き出す。
フォーマットの詳細は [output-format.md](references/output-format.md) を参照。

## 状態と行動

`task.yaml` の `status` フィールドで現在の状態を確認する。
各状態で何をすべきかは [state-machine.md](references/state-machine.md) を参照。

## ルール

- `instructions` trait は出力に含めない（読み取り専用）
- `environment.yaml` の制約に従う
- `verification` の `source_state` は自動注入される（設定不要）
