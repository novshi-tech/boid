---
name: boid-orchestrate
description: >
  CLI (`boid exec`) またはタスク詳細の Commands ボタンから起動される汎用エントリポイント。
  起動コンテキストを自動検出し、タスクコンテキストがある場合は対話的なタスク管理モード
  （description / instructions 更新、notify --ask への回答）で動作し、コンテキストがない
  場合は外部から supervisor タスクを作成・追跡する外部オーケストレーションモードで動作する。
  CLI / Web UI Commands ボタン両対応の統合 entry point。
---

# boid-orchestrate — CLI / Web UI 両対応の統合 entry point

## 起動コンテキストの検出

最初に `$BOID_TASK_ID` の有無を確認する。**それだけでは確定させない** — task-less
session (`boid exec` / project command セッション) は project/workspace 側の
`env:` 設定を継承するため、そこに (意図せず) `BOID_TASK_ID` という名前の変数が
紛れ込んでいると、実在しない・または無関係な過去タスクを指す値のまま非空判定を
通過してしまう。`boid task current` が実際に成功する (= そのタスクが存在し、
このセッションから見える) ことまで確認して初めてタスク管理モードと確定する。

```bash
if [ -n "$BOID_TASK_ID" ] && boid task current >/dev/null 2>&1; then
  # タスク管理モード（Web UI Commands ボタンから起動）
  TASK_ID="$BOID_TASK_ID"
  echo "タスク管理モードで起動: $TASK_ID"
else
  # 外部オーケストレーションモード（boid exec / CLI から起動）
  echo "外部オーケストレーションモードで起動"
fi
```

| 条件 | モード |
|---|---|
| `$BOID_TASK_ID` が設定され、かつ `boid task current` が成功する | **タスク管理モード** — 対象タスクを対話的に更新する |
| それ以外（`$BOID_TASK_ID` 未設定、または `boid task current` が失敗する） | **外部オーケストレーションモード** — supervisor タスクを作成・追跡する |

---

## タスク管理モード（Web UI Commands ボタンから起動）

`$BOID_TASK_ID` が設定され、かつ `boid task current` が成功する場合はこのモードで動作する。
対象タスクをユーザーと対話で詳細化したり、awaiting 中の質問に回答するために使う。

### 起動時の流れ

1. `$BOID_TASK_ID` を検証済みのタスク ID として使う（上の起動コンテキスト検出で `boid task current` の成功を確認済み）
2. 現在のタスク状態を確認してユーザーに提示する
3. ユーザーのゴールを聞いて実行する

```bash
TASK_ID="$BOID_TASK_ID"

# 現在状態の確認
title=$(boid task show "$TASK_ID" --field title 2>/dev/null)
status=$(boid task show "$TASK_ID" --field status 2>/dev/null)
description=$(boid task show "$TASK_ID" --field description 2>/dev/null)

echo "対象タスク: $title ($TASK_ID)"
echo "ステータス: $status"
```

### ゴール（どちらか）

1. **タスクを意図に合わせて更新する** — description / instructions / title の変更
2. **awaiting 中の質問に回答する** — notify --ask の回答を送信する

### 利用可能な操作

```bash
# 現状確認
boid task show "$TASK_ID" --field title
boid task show "$TASK_ID" --field description
boid task show "$TASK_ID" --field status
boid task show "$TASK_ID" --field instructions

# タスク更新（description / title など）
boid task update "$TASK_ID" --patch-file - <<EOF
description: |
  <新しい説明>
EOF

# instructions 追記（reopen 時に活きる）
boid task update "$TASK_ID" --instructions-file - <<EOF
- message: |
    <追加する指示>
EOF

# タイムラインに議論の記録を残す
boid task notify "$TASK_ID" --progress "<メモ>"

# awaiting 回答（status が awaiting の場合のみ）
question=$(boid task show "$TASK_ID" --field awaiting.question 2>/dev/null)
question_id=$(boid task show "$TASK_ID" --field awaiting.question_id 2>/dev/null)
boid task answer --task "$TASK_ID" --question-id "$question_id" --answer "<回答>"
```

### awaiting タスクへの回答フロー

```bash
# 質問内容を確認
status=$(boid task show "$TASK_ID" --field status 2>/dev/null)
if [ "$status" = "awaiting" ]; then
  question=$(boid task show "$TASK_ID" --field awaiting.question 2>/dev/null)
  question_id=$(boid task show "$TASK_ID" --field awaiting.question_id 2>/dev/null)
  echo "タスクが回答待ちです:"
  echo "$question"
  # ユーザーから回答を得て送信
  boid task answer --task "$TASK_ID" --question-id "$question_id" --answer "<ユーザの回答>"
fi
```

### やってはいけないこと

- status 遷移（done / aborted）を勝手に行わない（ユーザーが reopen で明示）
- 子タスクを作らない（それは `/boid-task` の Supervisor Mode の役割）
- 対象タスク以外のタスクを変更しない

### セッション終了

ユーザーが目的を達成したと言ったら、完了サマリを述べて終了する。
PTY が閉じることで exec job が完了する。

---

## 外部オーケストレーションモード（CLI / boid exec から起動）

`$BOID_TASK_ID` が未設定、または `boid task current` が失敗する場合はこのモードで動作する。
外部セッション（`BOID_TASK_ID` がない状態）から supervisor タスクを作成し、完了まで追跡する。

> **このスキルは `/boid-task` と何が違うか**
>
> | スキル | 動作コンテキスト | 作るタスクの親 |
> |---|---|---|
> | `/boid-task` | タスク内部（`BOID_TASK_ID` あり、`boid task current` 等で取得可） | 自分の子タスク |
> | `/boid-orchestrate` | タスク外部（`BOID_TASK_ID` なし） | root タスク（親なし） |
>
> `boid exec` セッションや一般の project command セッションなど、タスクコンテキストが
> ない場所から作業を委譲するときにこのスキルを使う。

### 事前確認: サンドボックス制約

**既知の制約**（調査済み）:
- `boid task create` — 利用可。ただし **`ref` フィールドが必須**（省略するとエラー）
- `boid task show <id> --field <path>` — 利用可
- `boid task list` — 利用可
- `boid task answer --task <id> --question-id <id> --answer <text>` — 利用可
- `boid task delete <id> --force` — 利用可
- `boid task watch` — **利用不可**（"unsupported boid task subcommand" エラー）。Monitor ツールでポーリングすること

### ワークフロー

#### 1. 委譲先プロジェクトを決める

セッションが既にあるプロジェクト配下（CWD が project ルート以下）なら、そのプロジェクトが
デフォルトで使われる。別プロジェクトに委譲したい場合は `boid task create` の `project_id`
フィールドに指定する。

```bash
# 現在のプロジェクトを確認
boid project show 2>/dev/null || echo "no active project"
```

#### 2. タイトルと description を整える

委譲する作業の title と description を決める。description には実装の詳細・期待する
成果物・参照すべきファイルなどを含める。大きなタスクは事前に `/boid-task` の Supervisor
Mode 相当の分解計画を立ててから description に落とすとよい。

#### 3. supervisor タスクを作成する

```bash
TASK_ID=$(boid task create <<YAML | awk '{print $3}'
title: <タスクのタイトル>
behavior: supervisor
auto_start: true
ref: <kebab-case-の安定したref>
description: |
  <委譲する作業の詳細な説明>
YAML
)
echo "created: $TASK_ID"
```

**`ref` は必須。** 毎回異なるランダム値は使わず、作業内容を表す安定した slug を使う
（例: `migrate-db-schema`, `add-feature-x`, `fix-bug-y`）。

`project_id` を指定したい場合:
```bash
TASK_ID=$(boid task create <<YAML | awk '{print $3}'
title: <タイトル>
behavior: supervisor
auto_start: true
ref: <ref>
project_id: <project-id>
description: |
  ...
YAML
)
```

#### 4. タスクを追跡する

`boid task watch` はサンドボックスで使えないため、**Monitor ツール**でポーリングする。
Monitor のスクリプト内では `sleep` が使える（フォアグラウンド `sleep` はブロックされるが
Monitor 内は問題ない）。

```
Monitor({
  description: "task <short-id> status changes",
  command: `
    TASK="<task-id>"
    prev=""
    while true; do
      st=$(boid task show "$TASK" --field status 2>/dev/null || echo "")
      if [ -n "$st" ] && [ "$st" != "$prev" ]; then
        echo "task $TASK -> $st"
        prev="$st"
      fi
      case "$st" in done|aborted) exit 0 ;; esac
      sleep 30
    done
  `,
  timeout_ms: 3600000
})
```

Monitor を起動したら **生成を止めて待つ**。ステータス変化があると通知が届く。

#### 5. awaiting になった場合: ユーザへの質問を中継する

タスクが `awaiting` になったら子タスク（supervisor 内の executor など）の質問ではなく、
root supervisor がユーザに対して質問を出している。読み取って中継する。

```bash
TASK="<task-id>"
question=$(boid task show "$TASK" --field awaiting.question 2>/dev/null)
question_id=$(boid task show "$TASK" --field awaiting.question_id 2>/dev/null)
echo "Question (id=$question_id):"
echo "$question"
```

ユーザの回答を受け取ったら `boid task answer` で中継する:

```bash
boid task answer --task "$TASK" --question-id "$question_id" --answer "<ユーザの回答>"
```

その後、Monitor は自動的に次のステータス変化を通知する（Monitor の再起動は不要）。

#### 6. done になった場合: 結果を取得して提示する

```bash
TASK="<task-id>"
boid task show "$TASK" --field payload.artifact.report
```

`payload.artifact.report` に structured report が含まれる（`summary`, `evidence`,
`verification`, `caveats` など）。内容をユーザに分かりやすく要約して提示する。

#### 7. aborted になった場合: 失敗内容を取得して提示する

```bash
TASK="<task-id>"
# Layer A: structured self-report
boid task show "$TASK" --field payload.artifact.report
# Layer B: abort メッセージ
boid task show "$TASK" --field lifecycle.abort.message
```

失敗内容を提示し、再試行するかどうかをユーザに確認する。

再試行（reopen）は supervisor タスクに対してはできない（aborted 遷移は終端）ため、
必要なら新しいタスクを作成する。

### 完全な例

```bash
# 1. タスクを作成して追跡
TASK_ID=$(boid task create <<YAML | awk '{print $3}'
title: サンプル機能の実装
behavior: supervisor
auto_start: true
ref: implement-sample-feature
description: |
  internal/sample/ に新しい機能を実装する。
  要件:
  - ...
  完了条件:
  - テストが green
  - /dev-pr-flow で PR を出す
YAML
)
echo "タスク作成: $TASK_ID"

# 2. Monitor でステータス変化を待つ（Monitor ツール呼び出し）
#    → 通知が届いたらステータスに応じて処理
```

### 注意事項

- **`ref` は必須かつ安定させること。** 同じ `ref` で再作成すると既存タスクが返る（冪等）。
- **ポーリングはフォアグラウンドで行わないこと。** Monitor ツール内の `sleep` は OK。
- タスク完了後のクリーンアップ（不要なタスクの削除）は `boid task delete <id>` で行える。
- サンドボックス制約により一部コマンドが使えない場合は、制約を明示してユーザに代替案を提示する。

---

## 関連スキル

- [`/boid-task`](../boid-task/SKILL.md) — タスク内部で動く統合スキル。Supervisor Mode（子タスクの作成・監視）と Executor Mode（実装してコミット）を `boid task current` の `readonly` フィールドで切り替える。
