---
name: boid-plan
description: boid オーケストレータの plan / auto_plan タスクを実行する。
  task.yaml の title/description を読み、実装計画を策定して
  `boid task create` builtin でサブタスクを直接登録する。
---

# boid Plan

## 概要

plan / auto_plan タスクは実装計画を策定し、サブタスクを生成する。
かつては `payload_patch.tasks[]` を出力する形だったが、現在は **`boid task create` builtin を直接呼び出す** 方式に変わった。
plan agent は sandbox 内から Bash ツールで `boid task create` を発火する。

サブタスクは複数生成されることが多いが、単一タスクで完結する場合は 1 つでもよい。
タスクを過剰に分割するより、適切な粒度に収めることを優先する。

## サブタスクの作成

`boid task create` は YAML / JSON を stdin から受け取る。 ref を使った同一バッチ内の依存解決もサーバ側で行われる。

```bash
boid task create <<'YAML'
title: タスクのタイトル
ref: task-a
behavior: dev
description: |
  このサブタスクへの実装指示。
  何を / どのように作るかを詳しく書く。
auto_start: true
YAML
```

依存があるタスクは ref で指定する:

```bash
boid task create <<'YAML'
title: 次のタスク
ref: task-b
behavior: dev
description: |
  task-a の成果に依存する実装。
depends_on:
  - task-a
depends_on_payload: artifact.auto-merge.merged
auto_start: true
YAML
```

### 親タスクへの紐付け

plan エージェントは `$BOID_TASK_ID` 環境変数で自身のタスク id を持っているので、 サブタスク作成時には `parent_id: ${BOID_TASK_ID}` を明示する。 これを忘れると親子関係が無く独立したタスクになるため、 `artifact.children.all_done` などの phase 依存が機能しない。

```bash
boid task create <<YAML
title: 子タスク
behavior: dev
parent_id: ${BOID_TASK_ID}
description: ...
auto_start: true
YAML
```

`task.yaml` (`~/.boid/context/task.yaml`) の `id` フィールドからも取得できる。

### フィールド一覧

| フィールド | 必須 | 説明 |
|-----------|------|------|
| `title` | ✓ | タスクのタイトル |
| `behavior` | ✓ | タスクの実行モデル名 (`task_behaviors` のキー: `dev`, `plan`, `auto_plan` など) |
| `description` | | エージェントへの指示。何を / どのように実装するかを詳細に記述する |
| `ref` | | 依存関係で参照するための名前 (同一バッチ内) |
| `auto_start` | | bool (デフォルト: false)。 true のとき、 (a) タスク作成直後に依存条件が満たされていれば自動開始、 (b) 依存先タスクの更新で条件が満たされたときに自動開始。 false のタスクは手動 start するまで pending のまま残る |
| `depends_on` | | 依存先 ref の配列 |
| `depends_on_payload` | | ペイロード条件 (文字列) |
| `project_id` | | タスクを作成するプロジェクト名。 省略時は親タスクのプロジェクト |
| `base_branch` | | サブタスクの worktree が分岐する base branch (PR のマージ先)。 省略時は behavior の `base_branch` 設定を継承 |
| `parent_id` | | 親タスクの id。 省略時は呼び出し元の plan タスクが親になる |

## 依存関係

タスク間に順序依存がある場合、後続タスクに設定する:

- `depends_on`: 依存先タスクの ref 名の配列 (同一バッチ内の ref のみ参照可)
- `depends_on_payload: "artifact.auto-merge.merged"` — 依存先 PR が auto-merge で正常マージされるまで待機
- 順序依存がないタスクには `depends_on` を設定しないこと (並列実行される)

## Phase 依存パターン

複数タスクを完了してから次フェーズに進む場合、フェーズ間に親タスクを挟む:

```bash
# Phase 1: 並列実装
boid task create <<'YAML'
title: 実装 A
ref: impl-a
behavior: dev
description: ...
auto_start: true
YAML

boid task create <<'YAML'
title: 実装 B
ref: impl-b
behavior: dev
description: ...
auto_start: true
YAML

# Phase 2: 全子の完了を待ってから計画
boid task create <<'YAML'
title: フェーズ2 計画
ref: phase2
behavior: auto_plan
description: フェーズ1の成果を踏まえてフェーズ2のタスクを生成する
depends_on:
  - impl-a
  - impl-b
depends_on_payload: artifact.children.all_done
auto_start: true
YAML
```

子タスクの完了を待つ `depends_on_payload` の値:

| 値 | 待機条件 |
|----|---------|
| `artifact.children.all_done` | 子タスクが全て done になるまで待機 |
| `artifact.auto-merge.merged` | 依存先タスクの PR が auto-merge でマージされるまで待機 |

## base_branch

サブタスクの worktree が分岐する base branch (PR のマージ先)。
省略時は選択した `behavior` の `base_branch` 設定 (`project.yaml` の
`task_behaviors.<name>.base_branch`) を継承する。

plan 実行時の現在のブランチを引き継いで dev サブタスクを派生させたい場合に
明示指定する。例えば feature ブランチ上で plan を走らせ、 そこから派生する
dev タスクを同じ feature ブランチに乗せたい場合:

```bash
CURRENT_BRANCH="$(git rev-parse --abbrev-ref HEAD)"

boid task create <<YAML
title: feature ブランチ上での実装
behavior: dev
base_branch: ${CURRENT_BRANCH}
description: ...
auto_start: true
YAML
```

base_branch を取得するには、 plan エージェントが実行中の worktree で
`git rev-parse --abbrev-ref HEAD` を呼び、 その結果を各サブタスクに付与する
(plan は readonly worktree で動くため `git` は読み取り専用で使用可)。

通常の `main` ベースで十分な場合は省略してよい。

## project_id

省略時は親タスクのプロジェクト (現在のリポジトリ) にタスクが作成される。
別プロジェクトでタスクを実行したい場合は `project_id` にプロジェクト名を指定する:

```bash
boid task create <<'YAML'
title: 別リポジトリでの実装
behavior: dev
description: ...
project_id: other-project
auto_start: true
YAML
```

例として、 boid 本体の作業と連動して boid-kits (キットパッケージ群のリポジトリ) を更新する場合は `project_id: boid-kits` を指定する。 プロジェクト名は環境に登録されているものを使うこと。

## 既存タスクの参照 (boid task list)

巨大な計画を立てる前に、 既存のサブタスク状況を確認したい場合は `boid task list` を使う:

```bash
boid task list --status pending --limit 50
boid task list --workspace-id <ws-id>
```

workspace 範囲外のタスクは broker 側で弾かれるため、 自プロジェクト / 同じ workspace のタスクのみ列挙される。

## plan と auto_plan の使い分け

| | plan | auto_plan |
|---|------|-----------|
| 対話 | あり (ユーザーと相談しながら計画) | なし (自律的に計画) |
| モデル | opus | sonnet |
| `auto_start` | 省略可 (ユーザー確認を挟める) | 原則 `true` |

**plan**: ユーザーと対話しながら計画を策定する。 `auto_start` を省略することでユーザー確認を挟める。

**auto_plan**: ユーザー介入なしで自律的に計画を策定・実行する。 原則すべてのタスクに `auto_start: true` を設定する。
