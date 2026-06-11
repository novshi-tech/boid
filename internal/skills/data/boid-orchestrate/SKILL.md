---
name: boid-orchestrate
description: >
  タスクコンテキストの外（`boid exec` セッション、一般の project command セッションなど、
  BOID_TASK_ID がない状態）から boid の **supervisor** タスクを作成し、完了まで追跡する。
  「これを boid に任せて」「バックグラウンドでタスクを走らせて」「supervisor タスクを作って
  追跡して」など、外部セッションからタスクを委譲・監視したい場合に使用。
  タスク内部で子タスクを作る場合は `/boid-supervisor` を使うこと（下記参照）。
---

# boid-orchestrate — 外部セッションからの supervisor タスク委譲スキル

> **このスキルは `/boid-supervisor` と何が違うか**
>
> | スキル | 動作コンテキスト | 作るタスクの親 |
> |---|---|---|
> | `/boid-supervisor` | タスク内部（`BOID_TASK_ID` あり、`~/.boid/context/*.yaml` あり） | 自分の子タスク |
> | `/boid-orchestrate` | タスク外部（`BOID_TASK_ID` なし、context ファイルなし） | root タスク（親なし） |
>
> `boid exec` セッションや一般の project command セッションなど、タスクコンテキストが
> ない場所から作業を委譲するときにこのスキルを使う。

## 事前確認: サンドボックス制約

このスキルを呼ぶ前に、現在のセッションが以下の `boid` サブコマンドを使えるか確認する。

```bash
boid task create <<EOF 2>&1
title: probe
ref: probe-test
behavior: supervisor
auto_start: false
EOF
```

使えれば `task created: <id> (pending)` が返る。もし `unsupported` エラーが出たら
そのサブコマンドは利用不可なので SKILL.md のワークフローを調整すること。

**既知の制約**（調査済み）:
- `boid task create` — 利用可。ただし **`ref` フィールドが必須**（省略するとエラー）
- `boid task show <id> --field <path>` — 利用可
- `boid task list` — 利用可
- `boid task answer --task <id> --question-id <id> --answer <text>` — 利用可
- `boid task delete <id> --force` — 利用可
- `boid task watch` — **利用不可**（"unsupported boid task subcommand" エラー）。Monitor ツールでポーリングすること

## ワークフロー

### 1. 委譲先プロジェクトを決める

セッションが既にあるプロジェクト配下（CWD が project ルート以下）なら、そのプロジェクトが
デフォルトで使われる。別プロジェクトに委譲したい場合は `boid task create` の `project_id`
フィールドに指定する。

```bash
# 現在のプロジェクトを確認
boid project show 2>/dev/null || echo "no active project"
```

### 2. タイトルと description を整える

委譲する作業の title と description を決める。description には実装の詳細・期待する
成果物・参照すべきファイルなどを含める。大きなタスクは事前に `/boid-supervisor` 相当の
分解計画を立ててから description に落とすとよい。

### 3. supervisor タスクを作成する

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

### 4. タスクを追跡する

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

### 5. awaiting になった場合: ユーザへの質問を中継する

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

### 6. done になった場合: 結果を取得して提示する

```bash
TASK="<task-id>"
boid task show "$TASK" --field payload.artifact.report
```

`payload.artifact.report` に structured report が含まれる（`summary`, `evidence`,
`verification`, `caveats` など）。内容をユーザに分かりやすく要約して提示する。

### 7. aborted になった場合: 失敗内容を取得して提示する

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

## 完全な例

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

## 注意事項

- **`ref` は必須かつ安定させること。** 同じ `ref` で再作成すると既存タスクが返る（冪等）。
- **ポーリングはフォアグラウンドで行わないこと。** Monitor ツール内の `sleep` は OK。
- タスク完了後のクリーンアップ（不要なタスクの削除）は `boid task delete <id>` で行える。
- サンドボックス制約により一部コマンドが使えない場合は、制約を明示してユーザに代替案を提示する。

## 関連スキル

- [`/boid-supervisor`](../boid-supervisor/SKILL.md) — タスク内部で子タスクを作成・監視する（タスクコンテキストあり）
- [`/boid-executor`](../boid-executor/SKILL.md) — タスク内部でコードを実装してコミットする
- [`/boid-q-and-a`](../../../../../../../.claude/skills/boid-q-and-a/SKILL.md) — タスク内から `notify --ask` でユーザに質問する
