---
name: boid-plan
description: boid オーケストレータの plan / auto_plan タスクを実行する。
  task.yaml の title/description を読み、実装計画を策定して
  ~/.boid/output/payload_patch.json の tasks[] 形式で書き出す。
---

# boid Plan

## 概要

plan / auto_plan タスクは実装計画を策定し、サブタスクを生成する。
`~/.boid/output/payload_patch.json` の `tasks[]` 形式で出力する。

サブタスクは複数生成されることが多いが、単一タスクで完結する場合は 1 つでもよい。
タスクを過剰に分割するより、適切な粒度に収めることを優先する。

## 出力フォーマット

```json
{
  "payload_patch": {
    "tasks": [
      {
        "title": "タスクのタイトル",
        "ref": "task-a",
        "behavior": "dev",
        "description": "...",
        "auto_start": true
      },
      {
        "title": "次のタスク",
        "ref": "task-b",
        "behavior": "dev",
        "description": "...",
        "depends_on": ["task-a"],
        "depends_on_payload": "artifact.auto-merge.merged",
        "auto_start": true
      }
    ]
  }
}
```

## フィールド

| フィールド | 必須 | 説明 |
|-----------|------|------|
| title | ✓ | タスクのタイトル |
| behavior | ✓ | タスクの実行モデル名（`task_behaviors` に定義されたキー: `dev`, `plan`, `auto_plan` など） |
| description | ✓ | エージェントへの指示。何を・どのように実装するかを詳細に記述する |
| ref | | 依存関係で参照するための名前（同一バッチ内） |
| auto_start | | bool（デフォルト: false）。true のとき、(a) タスク作成直後に依存条件が満たされていれば自動開始、(b) 依存先タスクの更新で条件が満たされたときに自動開始。false のタスクは手動 start するまで pending のまま残る |
| depends_on | | 依存先 ref の配列 |
| depends_on_payload | | ペイロード条件（文字列） |
| project_id | | タスクを作成するプロジェクト名。省略時は親タスクのプロジェクト |
| base_branch | | サブタスクの worktree が分岐する base branch（PR のマージ先）。省略時は behavior の `base_branch` 設定を継承 |

## 依存関係

タスク間に順序依存がある場合、後続タスクに設定する:

- `depends_on`: 依存先タスクの ref 名の配列（同一バッチ内の ref のみ参照可）
- `depends_on_payload: "artifact.auto-merge.merged"` — 依存先 PR が auto-merge で正常マージされるまで待機
- 順序依存がないタスクには `depends_on` を設定しないこと（並列実行される）

## Phase 依存パターン

複数タスクを完了してから次フェーズに進む場合、フェーズ間に親タスクを挟む:

```json
{
  "payload_patch": {
    "tasks": [
      {
        "title": "実装 A",
        "ref": "impl-a",
        "behavior": "dev",
        "description": "...",
        "auto_start": true
      },
      {
        "title": "実装 B",
        "ref": "impl-b",
        "behavior": "dev",
        "description": "...",
        "auto_start": true
      },
      {
        "title": "フェーズ2 計画",
        "ref": "phase2",
        "behavior": "auto_plan",
        "description": "フェーズ1の成果を踏まえてフェーズ2のタスクを生成する",
        "depends_on": ["impl-a", "impl-b"],
        "depends_on_payload": "artifact.children.all_done",
        "auto_start": true
      }
    ]
  }
}
```

子タスクの完了を待つ `depends_on_payload` の値:

| 値 | 待機条件 |
|----|---------|
| `artifact.children.all_done` | 子タスクが全て done（verifying → done）になるまで待機 |
| `artifact.children.all_resolved` | 子タスクの全 findings が resolved になるまで待機 |
| `artifact.auto-merge.merged` | 依存先タスクの PR が auto-merge でマージされるまで待機 |

## base_branch

サブタスクの worktree が分岐する base branch（PR のマージ先）。
省略時は選択した `behavior` の `base_branch` 設定（`project.yaml` の
`task_behaviors.<name>.base_branch`）を継承する。

plan 実行時の現在のブランチを引き継いで dev サブタスクを派生させたい場合に
明示指定する。例えば feature ブランチ上で plan を走らせ、そこから派生する
dev タスクを同じ feature ブランチに乗せたい場合:

```json
{
  "payload_patch": {
    "tasks": [
      {
        "title": "feature ブランチ上での実装",
        "behavior": "dev",
        "base_branch": "feature/awesome",
        "description": "...",
        "auto_start": true
      }
    ]
  }
}
```

base_branch を取得するには、plan エージェントが実行中の worktree で
`git rev-parse --abbrev-ref HEAD` を呼び、その結果を各サブタスクに付与する
（plan は readonly worktree で動くため `git` は読み取り専用で使用可）。

通常の `main` ベースで十分な場合は省略してよい。

## project_id

省略時は親タスクのプロジェクト（現在のリポジトリ）にタスクが作成される。
別プロジェクトでタスクを実行したい場合は `project_id` にプロジェクト名を指定する:

```json
{
  "payload_patch": {
    "tasks": [
      {
        "title": "別リポジトリでの実装",
        "behavior": "dev",
        "description": "...",
        "project_id": "other-project",
        "auto_start": true
      }
    ]
  }
}
```

例として、boid 本体の作業と連動して boid-kits（キットパッケージ群のリポジトリ）を更新する場合は `project_id: boid-kits` を指定する。プロジェクト名は環境に登録されているものを使うこと。

## plan と auto_plan の使い分け

| | plan | auto_plan |
|---|------|-----------|
| 対話 | あり（ユーザーと相談しながら計画） | なし（自律的に計画） |
| モデル | opus | sonnet |
| `auto_start` | 省略可（ユーザー確認を挟める） | 原則 `true` |

**plan**: ユーザーと対話しながら計画を策定する。`auto_start` を省略することでユーザー確認を挟める。

**auto_plan**: ユーザー介入なしで自律的に計画を策定・実行する。原則すべてのタスクに `auto_start: true` を設定する。
