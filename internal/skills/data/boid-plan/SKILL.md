---
name: boid-plan
description: boid オーケストレータの plan タスク (readonly supervisor) を実行する。
  task.yaml の title/description を読み、 .boid/project.yaml の task_behaviors から
  適切な behavior を選び、 `boid task create` builtin で子タスクを生成して、
  完了まで supervisor として監視する。 子の状態を見て追加生成、 必要ならユーザに通知する。
---

# boid Plan

plan タスクは依頼を **トリアージ** して、 適切な behavior の子タスクを生成し、 完了まで **supervisor として監視** する。 plan 自身は readonly なので、 `git` 読み取りはできるがファイル編集は行わない。

## 全体フロー

### 自律モード (`BOID_INTERACTIVE` が未設定 / 0)

1. **計画**: タスクを読んで、 behavior と分解粒度を決める。
2. **生成**: `boid task create` で子タスクを 1 件以上作る。 作成時に返される task ID を控える。
3. **監視**: 子の状態を周期的に確認して待機する。
4. **再計画 (必要時)**: 子の結果を踏まえて追加 / 修正タスクを生成する。
5. **終了**: 全子が `done` / `aborted` に達したら exit する。 詰まった場合は artifact に状況を書いて exit する。

### interactive モード (`BOID_INTERACTIVE=1`)

1. **計画**: タスクを読んで、 behavior と分解粒度を決める。
2. **(条件付き) ユーザ承認 notify**: 子タスクを生成する前に、 「いつ notify を呼ぶか」 セクションの判定基準に従って plan 提示 → 承認を得る。
3. **生成**: `boid task create` で子タスクを 1 件以上作る。 作成時に返される task ID を控える。
4. **監視**: 子の状態を周期的に確認して待機する。
5. **再計画 (必要時)**: 子の結果を踏まえて追加 / 修正タスクを生成する。 必要なら notify でユーザに相談する。
6. **終了**: 全子が `done` / `aborted` に達したら exit する。

子が 1 件しかなくても supervisor として残って完了を見届けること。 単に状態を見ているだけでも、 ユーザにとっては 「親が代わりに監視してくれている」 ことに価値がある (= ユーザが個別子セッションを直接見にいく必要が無くなる)。

## behavior カタログ

利用可能な behavior は **`.boid/project.yaml` の `task_behaviors` セクション** に定義されている。 サンドボックスから直接読み取り、 各 behavior の `default_instruction.message` (何をするか) と `readonly` / `worktree` / `model` 等の設定を見て、 タスクに合うものを選ぶ。 SKILL.md は project ごとの behavior 名を持たない。

`boid task create` で `behavior` を省略すると **`plan` に default routing** される。 supervisor として再委譲したい場合 (= 子側でも独自に triage + 監視させたい場合) に使う。

## 子タスクの生成

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

stdout には `task created: <id> (<status>)` が返るので、 シェル変数に取り込んで監視に使える:

```bash
CHILD_A=$(boid task create <<YAML | awk '{print $3}'
title: ...
parent_id: ${BOID_TASK_ID}
auto_start: true
YAML
)
```

### 必ず指定するもの

- `title`: 必須。
- `parent_id: ${BOID_TASK_ID}`: 親子関係を維持するために必須。 これを忘れると独立タスクになり、 supervisor の監視範囲から外れる。 `$BOID_TASK_ID` は環境変数で渡される (`~/.boid/context/task.yaml` の `id` でも取得可)。

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
| `behavior_spec` | inline behavior 定義 (kit が自分用の behavior を持ち込む時)。 通常は project.yaml に定義済みの behavior 名を使えば不要 |

interactive / model / readonly 等の挙動は behavior template (project.yaml の `task_behaviors`) に従う。 plan 自身は `BOID_INTERACTIVE` 環境変数を見て対話的な相談 / 自律的な決定を切り替えるので、 「対話的な plan」 「自律 plan」 を別 behavior に切り出す必要は無い。

## supervisor として監視

子タスクを生成したあと、 完了するまで状態を見ながら待機する。 supervisor は子の ID を覚えておき、 周期的に状態を取得する。

```bash
# 個別の子の状態
boid task get ${CHILD_A} --field status
```

監視ループの基本形:

```bash
CHILDREN="$CHILD_A $CHILD_B $CHILD_C"

while true; do
  PENDING=0
  for id in $CHILDREN; do
    case "$(boid task get "$id" --field status)" in
      done|aborted) ;;
      *) PENDING=$((PENDING + 1)) ;;
    esac
  done
  [ $PENDING -eq 0 ] && break
  sleep 60
done
```

各 iteration で:

- 新しく `done` になった子の artifact を読み (`boid task get <id> --field artifact.<key>` 等)、 後続子を追加生成すべきか判断する
- `aborted` があれば原因 (`boid task get <id> --field lifecycle.abort.message`) を確認し、 retry / 別アプローチで再生成、 または ユーザへエスカレーションする
- 判断に詰まったら `boid task notify` でユーザに相談する (下記)

`sleep` 間隔は実装規模に合わせる (短い実装なら 30s、 大規模な build/test を含むなら 2-5min)。

### Claude Code Monitor を使う場合

監視ループを background 化して、 状態変化のたびに 1 行出力するスクリプトを Claude Code Monitor で読む形にしてもよい。 重複行を抑制するために前回値と比較する:

```bash
(prev=""
while true; do
  cur=""
  for id in $CHILDREN; do
    cur="$cur $(boid task get "$id" --field status)"
  done
  if [ "$cur" != "$prev" ]; then
    echo "$cur"
    prev="$cur"
  fi
  sleep 60
done) &
```

長時間 / 子数が多いケースで便利だが、 シンプルなケースでは前述の foreground ループで十分。

## いつ notify を呼ぶか

### interactive モード

- **plan 承認 notify (条件付き必須)** — 子タスクを 1 件以上生成する前に、 plan 全体 (子の一覧 / behavior / 順序 / 想定リスク) を提示して承認を得る。 ただし以下の *いずれかに該当* する場合は承認 notify を省略してよい:
  - ユーザの依頼内容が既に十分 specific で、 解釈の余地が小さい (例: 「このファイルのこの行をこう直して」 「`xxx` を yyy に rename して」 等、 子タスクの title/description に転記するだけで決まる場合)
  - 子タスクが 1 件で、 かつ behavior と粒度が依頼から自明な場合

  迷ったら notify する側に倒す

- 子の半数以上が aborted、 または計画方針の変更が必要になった時
- hard cap (子 20 件 / 12 時間) 到達時
- 子の artifact から想定外の事実が出て、 残り計画を見直す必要がある時

### 自律モード

notify は呼ばない。 詰まった場合は artifact に状況を書いて exit する (ユーザが task list で気付いて reopen + instruction で指示を出すフロー)。

### 計画提示テンプレート

plan 承認 notify と一緒に session に出力する際は以下の形式を使う:

````markdown
## 改善計画

### 子タスク
| # | title | behavior | 並列/直列 | 概算 |
|---|-------|----------|-----------|------|
| 1 | ... | dev | - | 数時間 |
| 2 | ... | dev | 1 のあと | 数時間 |

### リスク・前提
- ...

### 判断してほしいこと
- A. 上記方針で進める
- B. 修正案を提示
- C. 中止
````

「子タスク表」 「リスク・前提」 「判断選択肢 (A/B/C)」 の 3 ブロック構成を必ず含めること。

## ユーザ通知 (boid task notify)

ユーザの判断が必要なとき、 `boid task notify` を呼ぶと `~/.config/boid/config.yaml` の `notify.command` が exec される。

```bash
boid task notify ${BOID_TASK_ID} --message "PR #284 のレビュー反映方針を判断してほしい"
```

通知スクリプトには env で `BOID_TASK_ID` / `BOID_PROJECT_ID` / `BOID_MESSAGE` / `BOID_TASK_URL` (config に `web.public_url` が設定済なら clickable link) が渡される。

### interactive 前提 + セッション内で待つ

notify は **interactive モード (`BOID_INTERACTIVE=true`) のときだけ呼ぶ**。 自律モードで詰まったときは notify せず、 状況を artifact に書いて exit する (ユーザが task list で気付いて reopen + instruction で指示を出す)。

interactive モードでは notify 直後に質問本文 (選択肢 / 必要な判断材料 / context) を session に出力してユーザの返答を待つ:

```bash
boid task notify ${BOID_TASK_ID} --message "..."
echo "判断してほしいこと:"
echo "  A. ...の方針で進める"
echo "  B. ...の方針で進める"
echo "  C. 別の案を提示"
# ここで agent はユーザの入力を待つ
```

質問の中身は session transcript に残るので、 ユーザは Web UI のセッションビューアで読んで返答する (boid 側に質問履歴を保存する仕組みは無い)。

### 通知のセマンティクス

- 呼ぶたびに 1 回 notify される。 1 タスク内で複数回呼んでよい (呼出回数 = 通知回数)
- 進捗報告のための notify は呼ばない (子の状態は task list / Web UI で見える)
- **判断分岐** (どの方針で進めるか) と **事前承認** (plan 全体を流す前) のために呼ぶ
- 「ユーザが見ない限り進めない」 状態に達した時点で 1 回呼ぶ

## hard cap (暴走防止)

supervisor が無限に子を生成しないよう、 自分の判断で上限を設けて守ること:

- 生成済み子タスクの累計 (このセッションで作った数) が **20 件** を超えたら新規生成を停止し、 `boid task notify` でユーザに相談する
- 計画開始から **12 時間** 以上経過しているのに完了に近づかない場合も同様

数値は実装規模に応じて調整してよい。 「上限を持たない supervisor は作らない」 ことだけ徹底する。

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

supervisor が自前で順序付けするなら `depends_on_payload` を使わずに監視ループ内で次の子を生成してもよい。 両者を併用すると挙動が二重になって混乱するので、 どちらかに寄せる。

## 子フェーズの分割

依存連鎖が長いプロジェクトでは、 supervisor がフェーズごとに子を生成 → 完了を待つ → 次フェーズを生成 という流れで管理できる:

```bash
# Phase 1
PHASE1_A=$(boid task create <<<... | awk '{print $3}')
PHASE1_B=$(boid task create <<<... | awk '{print $3}')
# 監視ループで PHASE1_A, PHASE1_B の done を待つ

# Phase 1 の結果を見て Phase 2 を計画
PHASE2_A=$(boid task create <<<... | awk '{print $3}')
# ...
```

計画があらかじめ確定していて supervisor が中継する必要がないなら、 phase plan を子として挟む declarative パターンも使える:

```bash
boid task create <<YAML
title: Phase 2 計画
ref: phase2
parent_id: ${BOID_TASK_ID}
depends_on: [phase1-a, phase1-b]
depends_on_payload: artifact.children.all_done
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
boid task list --status pending
boid task list --workspace <ws-id>
```

workspace 範囲外のタスクは broker で弾かれる (自プロジェクト / 同 workspace のみ列挙)。
