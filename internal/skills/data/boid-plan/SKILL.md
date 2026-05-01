---
name: boid-plan
description: boid オーケストレータの plan タスク (readonly トリアージ役) を実行する。
  task.yaml の title/description を読み、 .boid/project.yaml の task_behaviors から
  適切な behavior を選び、 `boid task create` builtin で子タスクを生成する。
  小規模なら 1 件、 分解が必要なら複数、 さらに計画が必要なら子 plan に再委譲する。
---

# boid Plan

plan タスクは依頼を **トリアージ** して、 適切な behavior の子タスクを `boid task create` builtin で生成する。 plan 自身は readonly なので、 `git` 読み取りはできるがファイル編集は行わない。

## 行動の分岐

タスクを読んで以下のいずれかに振り分ける:

1. **そのまま実行**: 小規模で迷いがないなら、 適切な behavior を選んで子タスクを 1 件作って exit する。
2. **複数に分解**: 並列 / 直列で実行可能な複数子タスクを作る (依存があれば `depends_on` で順序付け)。
3. **子 plan に再委譲**: フェーズごとに段階計画が必要な大きい依頼なら、 子 plan タスクを作って委譲する。

判断に迷ったら、 interactive モードならユーザに相談する。

## behavior カタログ

利用可能な behavior は **`.boid/project.yaml` の `task_behaviors` セクション** に定義されている。 サンドボックスから直接読み取り、 各 behavior の `default_instruction.message` (何をするか) と `readonly` / `worktree` / `model` 等の設定を見て、 タスクに合うものを選ぶ。 SKILL.md は project ごとの behavior 名を持たない。

`boid task create` で `behavior` を省略すると **`plan` に default routing** される。 さらに triage したい場合や behavior 選択を迷うときに使える (= 「もう一段考えてくれ」 の意)。

## サブタスクの作成

`boid task create` は YAML / JSON を stdin から受け取る。 ref を使った同一バッチ内の依存解決もサーバ側で行われる。

```bash
boid task create <<YAML
title: タスクのタイトル
behavior: <project.yaml の task_behaviors のキー、 または省略>
parent_id: ${BOID_TASK_ID}
description: |
  このサブタスクへの実装指示。 何を / どのように作るかを詳しく書く。
auto_start: true
YAML
```

### 必ず指定するもの

- `title`: 必須。
- `parent_id: ${BOID_TASK_ID}`: 親子関係を維持するために必須。 これを忘れると独立タスクになり、 `artifact.children.all_done` などの phase 依存が機能しない。 `$BOID_TASK_ID` は環境変数で渡される (`~/.boid/context/task.yaml` の `id` でも取得可)。

### よく使うフィールド

| フィールド | 説明 |
|---|---|
| `description` | エージェントへの指示。 何を / どのように実装するかを詳細に記述する |
| `ref` | 依存解決用の名前 (同一バッチ内) |
| `depends_on` | 依存先 ref の配列 |
| `depends_on_payload` | 待機条件 (下記) |
| `auto_start` | bool。 true で依存解消時に自動開始 |
| `base_branch` | worktree の分岐元。 省略時は behavior の設定を継承 |
| `project_id` | タスクを作成するプロジェクト。 省略時は親と同じ |
| `instructions` | behavior の `default_instruction` を per-task で上書き (interactive / model / message 等) |

`api.CreateTaskRequest` の他のフィールド (traits, readonly, worktree, branch_prefix, behavior_spec) も指定可能。 通常は behavior template の defaults で十分。

## 依存関係

順序依存があるタスクは後続側に設定する:

```bash
boid task create <<YAML
title: 後続タスク
behavior: <名前>
parent_id: ${BOID_TASK_ID}
ref: task-b
description: ...
depends_on:
  - task-a
depends_on_payload: artifact.auto-merge.merged
auto_start: true
YAML
```

順序依存がないタスクには `depends_on` を設定しない (並列実行される)。

`depends_on_payload` の主な値:

| 値 | 待機条件 |
|---|---|
| `artifact.auto-merge.merged` | 依存先タスクの PR が auto-merge でマージされるまで |
| `artifact.children.all_done` | 依存先タスクの子が全て done になるまで |

## Phase 依存パターン

並列実装の完了を待ってから次フェーズに進む場合、 フェーズ間に親 plan を挟む:

```bash
# Phase 1: 並列実装 (impl-a, impl-b を作る、 ここでは省略)

# Phase 2: 全完了を待って次フェーズを計画
boid task create <<YAML
title: フェーズ2 計画
ref: phase2
parent_id: ${BOID_TASK_ID}
description: フェーズ1の成果を踏まえてフェーズ2のタスクを生成する
depends_on:
  - impl-a
  - impl-b
depends_on_payload: artifact.children.all_done
auto_start: true
YAML
```

`behavior` 省略により `plan` に default routing される (フェーズ2 自身もトリアージから始める)。

## interactive / model の上書き

behavior の `default_instruction` の値はタスクごとに `instructions` 配列で上書きできる。 例えば 「behavior は plan のままだが、 このタスクは自律 (非対話) で走らせたい」 場合:

```bash
boid task create <<YAML
title: 自律的に走らせる
parent_id: ${BOID_TASK_ID}
description: ...
instructions:
  - type: execution
    consumer: claude-code
    interactive: false
    model: sonnet
    message: "/boid-plan の指示に従って自律的に計画・実行せよ"
auto_start: true
YAML
```

## base_branch

worktree の分岐元 (PR のマージ先)。 省略時は behavior の `base_branch` を継承する。 plan 実行時の現在のブランチに派生タスクを乗せたい場合に明示指定する:

```bash
CURRENT_BRANCH="$(git rev-parse --abbrev-ref HEAD)"

boid task create <<YAML
title: feature ブランチ上での実装
behavior: <名前>
parent_id: ${BOID_TASK_ID}
base_branch: ${CURRENT_BRANCH}
description: ...
auto_start: true
YAML
```

通常の `main` ベースで十分なら省略してよい。

## project_id

別プロジェクトでタスクを動かす場合に指定する。 省略時は親と同じプロジェクト。 プロジェクト名は環境に登録されているものを使う (例: `boid` 本体に連動して `boid-kits` を更新するなら `project_id: boid-kits`)。

## 既存タスクの参照

巨大な計画を立てる前に既存タスクを確認したいとき:

```bash
boid task list --status pending --limit 50
boid task list --workspace-id <ws-id>
```

workspace 範囲外のタスクは broker で弾かれる (自プロジェクト / 同 workspace のみ列挙)。
