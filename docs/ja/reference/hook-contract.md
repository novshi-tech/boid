# Hook スクリプトプロトコル リファレンス

`boid` の hook スクリプトと本体の間の入出力契約をまとめたリファレンスです。

[Kit 作者向け 概要](../kit-authoring/overview.md) には抜粋を載せていますが、 このページでは入力 (stdin、 環境変数、 作業ディレクトリ)、 出力 (`payload_patch.json`、 stdout、 stderr)、 終了コード、 データ構造の正規仕様を網羅します。

## 入力

### stdin

hook が起動されるとき、 stdin にはタスク全体を JSON でシリアライズしたもの (TaskJSON) が流し込まれます。長さは可変なので、 スクリプト側は EOF まで読み切ってから JSON パースしてください。

TaskJSON の主なフィールド:

| キー | 型 | 役割 |
|---|---|---|
| `id` | string | タスク ID (UUID 形式) |
| `project_id` | string | 所属プロジェクトの ID |
| `title` | string | タスクのタイトル |
| `description` | string | 任意の本文 |
| `status` | string | 現在の status (`pending` / `executing` / ...) |
| `behavior` | string | behavior 名 (`supervisor` / `executor`) |
| `traits` | string のリスト | この behavior が宣言した payload trait |
| `readonly` | bool | サンドボックスが読み取り専用か (behavior 名から導出: supervisor=true / executor=false) |
| `worktree` | bool | このタスクが worktree を持つか (project トップ `worktree:` と behavior 名の組み合わせで決まる) |
| `branch_prefix` | string | worktree のブランチ名プレフィックス (常に `boid/` で固定。 設定不可) |
| `base_branch` | string | worktree のベースブランチ (project トップ `base_branch:` を `${TASK_REMOTE_ID}` / `${current_branch}` 展開して解決) |
| `payload` | object | 現在の payload 全体 (hook が読みたい主要素) |
| `instructions` | map (role → Instruction) | `kind: agent` の hook でのみ意味を持つ、 routing 済みの instruction |
| `auto_start` | bool | タスク作成時の auto_start 指定 |
| `depends_on` | string のリスト | 依存先タスク ID |
| `parent_id` | string | 親タスク (任意) |
| `created_at` / `updated_at` | RFC3339 timestamp | 作成 / 更新時刻 |

完全な構造は [`internal/orchestrator/spec_types.go`](https://github.com/novshi-tech/boid/blob/main/internal/orchestrator/spec_types.go) の `Task` 型を参照してください。

### 環境変数

hook の実行コンテキストには次の環境変数が設定されます。

| 変数 | 役割 |
|---|---|
| `BOID_TASK_ID` | 現在のタスク ID (TaskJSON の `id` と同じ値) |
| `BOID_JOB_ID` | 現在のジョブ ID (`boid job show <id>` で参照される) |
| `BOID_PROJECT_ID` | プロジェクト ID |
| `HOME` | サンドボックス内のホーム |
| `PATH` | 起動側から継承したパス (kit の `env` で上書き可能) |

加えて、 kit の `kit.yaml` で宣言した変数がすべて流し込まれます。

### 作業ディレクトリ

- タスクが worktree を持つとき (project トップ `worktree: true` + executor behavior の組み合わせ) は、 hook の cwd は **その worktree のルート** です
- そうでないとき (supervisor タスク、 または project トップ `worktree:` 未設定の executor) は、 cwd は **プロジェクトルート** (`project.yaml` がある親ディレクトリ) です

これにより、 `git`、 `gh`、ビルドコマンド等は明示的にディレクトリ指定せずに使えます。

### ファイルシステムアクセス

hook はサンドボックス内で動きます。 読み書き可能なのは worktree (または readonly = true な supervisor タスクではどこも書けない) のみ。 kit が `additional_bindings` で宣言したパスは追加でマウントされます。 host のホーム / SSH 鍵 / 他プロジェクトは見えません。

## 出力

hook は payload を更新したい場合に **payload patch** を返します。出力経路は 2 通りあり、優先順位があります。

### 経路 1: `$HOME/.boid/output/payload_patch.json` (推奨)

`$HOME/.boid/output/payload_patch.json` に JSON を書き出します。 hook 終了時のフックがこのファイルを優先的に読み取って `boid` 本体に渡します。

ファイルが存在すれば stdout は無視されます。

```bash
mkdir -p "$HOME/.boid/output"
cat > "$HOME/.boid/output/payload_patch.json" <<'JSON'
{
  "payload_patch": {
    "artifact": { "result": "ok" }
  }
}
JSON
```

### 経路 2: stdout (フォールバック)

`payload_patch.json` が無いときに限り、 stdout に書いた JSON が payload patch として扱われます。 1 行で出しても複数行で出しても構いませんが、有効な JSON 1 件であること。

```bash
echo '{"payload_patch":{"artifact":{"result":"ok"}}}'
```

新規に書く hook では経路 1 を推奨します。 複雑な agent 系 hook (`claude-code` など) は agent の stdout に副次的な出力を含むため、 ファイル経路で誤認を避けます。

### payload patch の構造

payload patch のトップレベルは必ず `payload_patch` キーで、 その下が現在の payload にマージされます。 マージは JSON の deep merge です。

```json
{
  "payload_patch": {
    "artifact": {
      "<キー>": "<値>"
    }
  }
}
```

hook が書ける trait は実質 `artifact` のみで、 何が許されるかは [`kit.yaml`](../kit-authoring/overview.md) の `traits.produces` 宣言で絞られます。 trait の意味は [概念 / payload と trait](../guide/concepts.md#payload-と-trait) を参照。

### stderr (ログ)

hook が stderr に書いた内容はジョブのログとして保存され、 `boid job show <job-id>` で読めます。 デバッグ情報はここに吐いてください。 stderr は payload patch には影響しません。

## 終了コード

| 終了コード | 扱い |
|---|---|
| `0` | 成功。 payload patch があればマージされる |
| 非 0 | ジョブを `failed` にマーク。 タスクは即 `aborted` にはならない (状態機械の自動遷移ルール次第) |

非 0 で終わった場合でも、 `payload_patch.json` を書いていればマージは試みられます。

## agent 用の追加コンテキスト

`kind: agent` で宣言した hook は、 instruction routing の対象になります。 TaskJSON の `instructions` フィールドにこの hook 宛の instruction (`Instruction` のマップ) が入って渡されます。 たとえば claude-code kit の hook は `instructions.main` を読んで agent への message として組み立てます。

`Instruction` のフィールドは [`project.yaml` リファレンス / Instruction](project-yaml.md#instruction) を参照。

## 最小例 (Bash)

```bash
#!/usr/bin/env bash
set -euo pipefail

# 入力
TASK_JSON=$(cat)
TASK_ID=$(echo "$TASK_JSON" | jq -r .id)
echo "[my-hook] processing task $TASK_ID" >&2

# 何かする (ここでは固定値を artifact に書くだけ)
mkdir -p "$HOME/.boid/output"
cat > "$HOME/.boid/output/payload_patch.json" <<'JSON'
{
  "payload_patch": {
    "artifact": { "hello": "world" }
  }
}
JSON
```

## 関連ドキュメント

- [Kit 作者向け 概要](../kit-authoring/overview.md) — kit の作り方全般
- [`project.yaml` リファレンス](project-yaml.md) — `Instruction` 等の型定義
- [概念 / payload と trait](../guide/concepts.md#payload-と-trait) — trait の意味
- [状態機械](../guide/state-machine.md) — hook 発火タイミング
