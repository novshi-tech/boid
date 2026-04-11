# Data Model

## Contents

- [task.yaml](#taskyaml)
- [instructions.yaml](#instructionsyaml)
- [payload.yaml](#payloadyaml)
- [environment.yaml](#environmentyaml)

## task.yaml

```yaml
id: "abc-12345678"
title: "ユーザー認証機能の実装"
description: "OAuth2 を使ったログイン機能を追加する"
status: "executing"
behavior: "impl"
```

| フィールド | 説明 |
|-----------|------|
| id | タスクの一意識別子 |
| title | タスクタイトル |
| description | タスクの詳細説明 |
| status | 現在の状態（[state-machine.md](state-machine.md) 参照） |
| behavior | タスクの実行モデル名 |

## instructions.yaml

あなた宛の指示の配列。複数の instruction が届くことがある。

```yaml
- role: executor
  type: execution
  consumer: claude-code
  message: "TDD で実装してください。テストを先に書くこと。"
```

| フィールド | 説明 |
|-----------|------|
| role | 指示の論理名（同一 consumer に複数 role がありうる） |
| type | `execution`（実装指示）または `verification`（検証指示） |
| consumer | 宛先エージェント名 |
| message | 具体的な指示内容 |

全ての message を読み、総合的に作業すること。

## payload.yaml

タスクのペイロード全体。トップレベルキーが trait 名。

```yaml
instructions: { ... }
artifact: { ... }
verification: { ... }
```

### Traits

| trait | 用途 | 読み書き |
|-------|------|---------|
| instructions | エージェントへの指示 | 読み取り専用 |
| artifact | 実装成果物 | 書き込み可（executing / reworking） |
| verification | 検証結果 | 書き込み可（verifying） |
| tasks | サブタスク配列 | 書き込み可（triage 用途） |

### verification の構造

複数レビュアーの結果が reviewer ID ごとに格納される:

```yaml
verification:
  security-reviewer:
    source_state: verifying
    findings:
      - message: "XSS チェック OK"
        status: resolved
      - message: "SQL インジェクションの可能性"
        status: open
```

- `source_state`: この検証が行われた時の task status（システム自動注入）
- `findings[].status`: `open`（未解決）または `resolved`（解決済み）

rework 時は `status: "open"` の findings を確認し、対応すること。

## environment.yaml

サンドボックスの動的な制約情報。

```yaml
readonly: false
worktree: false
network:
  restricted: true
tools:
  - git
  - python3
workspace_projects:
  - path: /home/user/shared-lib
    name: shared-lib
```

| フィールド | 説明 |
|-----------|------|
| readonly | プロジェクトディレクトリの書き込み可否 |
| worktree | git worktree モードか |
| network.restricted | 外部ネットワークアクセスが制限されているか |
| tools | 利用可能なコマンド |
| workspace_projects | 同一ワークスペース内の他プロジェクト（読み取り専用） |
