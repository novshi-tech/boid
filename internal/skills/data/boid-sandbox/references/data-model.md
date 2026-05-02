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
behavior: "dev"
```

| フィールド | 説明 |
|-----------|------|
| id | タスクの一意識別子 |
| title | タスクタイトル |
| description | タスクの詳細説明 |
| status | 現在の状態（[state-machine.md](state-machine.md) 参照） |
| behavior | タスクの実行モデル名 |

## instructions.yaml

あなた宛の指示の配列。 配列の最後の要素が今の active 指示で、 reopen のたびに append される。 過去の指示も配列の前方に残るので、 「前回何が依頼されたか」 を辿れる。

```yaml
- role: executor
  type: execution
  agent: claude-code
  message: "TDD で実装してください。テストを先に書くこと。"
- role: executor
  type: execution
  agent: claude-code
  message: "lint エラーを修正して再 push してください。"   # reopen で append された
```

| フィールド | 説明 |
|-----------|------|
| role | 指示の論理名 |
| type | `execution` のみ |
| agent | 宛先エージェント名 |
| message | 具体的な指示内容 |

最後の要素を主指示として読み、 必要であれば前方の要素を文脈として参照する。

## payload.yaml

**読み取り専用**の入力ファイル。過去の hook が積み上げた artifact 等を文脈として読むのに使う。エージェントが書き込む経路ではない。

instructions は payload の trait ではなく、 `task.yaml` と並ぶ別ファイル (`instructions.yaml`) で配信される。

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
