---
name: boid-plan
description: boid オーケストレータの plan / auto_plan タスクを実行する。
  task.yaml の title/description を読み、実装計画を策定して
  ~/.boid/output/payload_patch.yaml の tasks[] 形式で書き出す。
---

# boid Plan

## 概要

plan / auto_plan タスクは実装計画を策定し、サブタスク群を生成する。
`~/.boid/output/payload_patch.yaml` の `tasks[]` 形式で出力する。

## 出力フォーマット

```yaml
payload_patch:
  tasks:
    - title: "タスクのタイトル"
      ref: "task-a"
      behavior: "dev"
      description: "..."
      auto_start: true
    - title: "次のタスク"
      ref: "task-b"
      behavior: "dev"
      description: "..."
      depends_on: ["task-a"]
      depends_on_payload: "artifact.auto-merge.merged"
      auto_start: true
```

## フィールド

| フィールド | 必須 | 説明 |
|-----------|------|------|
| title | ✓ | タスクのタイトル |
| behavior | ✓ | タスクの実行モデル名（`task_behaviors` に定義されたキー: `dev`, `plan`, `auto_plan` など） |
| description | ✓ | エージェントへの指示。何を・どのように実装するかを詳細に記述する |
| ref | | 依存関係で参照するための名前（同一バッチ内） |
| auto_start | | bool（デフォルト: false）。true にするとタスク作成直後に自動開始 |
| depends_on | | 依存先 ref の配列 |
| depends_on_payload | | ペイロード条件（文字列） |
| project_id | | タスクを作成するプロジェクト名。省略時は親タスクのプロジェクト |

## 依存関係

タスク間に順序依存がある場合、後続タスクに設定する:

- `depends_on`: 依存先タスクの ref 名の配列（同一バッチ内の ref のみ参照可）
- `depends_on_payload: "artifact.auto-merge.merged"` — 依存先 PR が auto-merge で正常マージされるまで待機
- 順序依存がないタスクには `depends_on` を設定しないこと（並列実行される）

## Phase 依存パターン

複数タスクを完了してから次フェーズに進む場合、フェーズ間に親タスクを挟む:

```yaml
payload_patch:
  tasks:
    # フェーズ1（並列）
    - title: "実装 A"
      ref: "impl-a"
      behavior: dev
      description: "..."
      auto_start: true
    - title: "実装 B"
      ref: "impl-b"
      behavior: dev
      description: "..."
      auto_start: true
    # フェーズ1の全タスクが完了してから実行する親タスク
    - title: "フェーズ2 計画"
      ref: "phase2"
      behavior: auto_plan
      description: "フェーズ1の成果を踏まえてフェーズ2のタスクを生成する"
      depends_on: ["impl-a", "impl-b"]
      depends_on_payload: "artifact.children.all_done"
      auto_start: true
```

子タスクの完了を待つ `depends_on_payload` の値:

| 値 | 待機条件 |
|----|---------|
| `artifact.children.all_done` | 子タスクが全て done（verifying → done）になるまで待機 |
| `artifact.children.all_resolved` | 子タスクの全 findings が resolved になるまで待機 |
| `artifact.auto-merge.merged` | 依存先タスクの PR が auto-merge でマージされるまで待機 |

## project_id

boid-kits（キットパッケージ群）の更新を伴うタスクは `project_id: boid-kits` を明示する:

```yaml
payload_patch:
  tasks:
    - title: "go-dev キットに lint gate を追加"
      behavior: dev
      description: "..."
      project_id: "boid-kits"
      auto_start: true
```

省略時は親タスクのプロジェクト（現在のリポジトリ）にタスクが作成される。

## plan と auto_plan の使い分け

| | plan | auto_plan |
|---|------|-----------|
| 対話 | あり（ユーザーと相談しながら計画） | なし（自律的に計画） |
| モデル | opus | sonnet |
| `auto_start` | 省略可（ユーザー確認を挟める） | 原則 `true` |

**plan**: ユーザーと対話しながら計画を策定する。`auto_start` を省略することでユーザー確認を挟める。

**auto_plan**: ユーザー介入なしで自律的に計画を策定・実行する。原則すべてのタスクに `auto_start: true` を設定する。
