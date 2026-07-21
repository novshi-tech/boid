# Hook スクリプトプロトコル リファレンス

`boid` の hook スクリプトと本体の間の入出力契約をまとめたリファレンスです。

[Kit 作者向け 概要](../kit-authoring/overview.md) には抜粋を載せていますが、 このページでは入力 (stdin、 環境変数、 作業ディレクトリ)、 出力 (`payload_patch.json`、 stdout、 stderr)、 終了コード、 データ構造の正規仕様を網羅します。

## 入力

### stdin

すべての hook ジョブは `Interactive: true` で起動されます。つまり stdin は PTY であり、**起動時に stdin へデータは書き込まれません**。stdin から TaskJSON を読もうとすると永遠にブロックします。

タスクのメタデータは、hook 起動前に `$HOME/.boid/context/` へ書き込まれた**コンテキストファイル**経由で提供されます:

| ファイル | フォーマット | 内容 |
|---|---|---|
| `task.yaml` | YAML | コアタスクフィールド (下表参照) |
| `instructions.yaml` | YAML | routing 済み instruction (`kind: agent` hook 向け) |
| `payload.json` | JSON | 現在の payload 全体 |

追加の環境メタデータ (ネットワークの許可ドメイン、 host_commands の allow/deny/reject ルール) はコンテキストファイルではなく `boid task env` コマンド (broker RPC、 サンドボックス内 PATH から実行可能) 経由で取得します。 hook スクリプトから直接呼べます:

```bash
boid task env                       # YAML (既定)
boid task env --format json         # JSON
boid task env --field allowed_domains
```

**`task.yaml` のフィールド** (存在するフィールドはこれだけです。意図的に最小化されています):

| キー | 型 | 役割 |
|---|---|---|
| `id` | string | タスク ID (UUID 形式) |
| `title` | string | タスクのタイトル |
| `status` | string | 現在の status (`pending` / `executing` / `awaiting` / `done` / `aborted`) |
| `behavior` | string | behavior 名 (`supervisor` / `executor`) |
| `description` | string | 任意の本文 |

スクリプト起動直後にコンテキストファイルを読んでおきます:

```bash
TASK_ID=$(yq -r .id "$HOME/.boid/context/task.yaml")
PAYLOAD=$(cat "$HOME/.boid/context/payload.json")
```

> **非インタラクティブジョブのみ**: `kind: exec` (非インタラクティブ) の hook は、コンテキストファイルに加えて trait フィルタ済み payload を stdin でも受け取ります。インタラクティブな agent hook は stdin データを受け取りません。

完全なタスク構造は [`internal/orchestrator/spec_types.go`](https://github.com/novshi-tech/boid/blob/main/internal/orchestrator/spec_types.go) の `Task` 型を参照してください。

### 環境変数

hook の実行コンテキストには次の環境変数が設定されます。

| 変数 | 役割 |
|---|---|
| `BOID_TASK_ID` | 現在のタスク ID |
| `BOID_JOB_ID` | 現在のジョブ ID (`boid job show <id>` で参照される) |
| `BOID_BASE_BRANCH` | タスクの `base_branch` (PR target となるブランチ名)。 root / child ともに設定される |
| `BOID_MODEL` | このタスクの instruction に設定されているモデル名 |
| `BOID_INVOKED_ROLE` | この hook 呼び出しを起動したロール名 |
| `BOID_INVOKED_NAME` | ロール内の hook 名 |
| `BOID_INVOKED_BEHAVIOR` | behavior 名 (`supervisor` / `executor`) |
| `BOID_INSTRUCTIONS` | この hook に渡される instruction のシリアライズ済み文字列 (`kind: agent` hook 向け) |
| `BOID_INTERACTIVE` | インタラクティブ (PTY) ジョブなら `1`、そうでなければ `0` |
| `BOID_BUILTIN_SHIM` | サンドボックスに注入される組み込みシムバイナリのパス |
| `BOID_HOST_IP` | サンドボックス内から到達可能なホストの IP アドレス |
| `BOID_BROKER_SOCKET` | ホストコマンドブローカーの UNIX ソケットパス |
| `BOID_BROKER_TOKEN` | ブローカーソケットの認証トークン |
| `BOID_SOCKET` | boid デーモンの UNIX ソケットパス (hook 内から `boid` CLI を呼ぶために使う) |
| `BOID_USER_ANSWER` | (レガシー) `notify --ask` 経路の awaiting レコードに残った `pending_answer` を hook 環境に流し込む。 daemon は answer 時の resume hook を dispatch しないため、 通常は空。 `boid task ask` 経由の回答は in-memory で agent に直接届くので env には現れない |
| `BOID_QUESTION_ID` | `BOID_USER_ANSWER` に対応する質問 ID。 同じくレガシー経路でのみ意味を持つ |
| `TERM` | ターミナルタイプ (例: `xterm-256color`) |
| `HOME` | サンドボックス内のホームディレクトリ |
| `PATH` | 起動側から継承したパス (kit の `env` で上書き可能) |

> **注意**: `BOID_PROJECT_ID` は hook 環境には**設定されません**。この変数は `boid task notify` コマンドが内部的にエクスポートするもので、hook スクリプトには渡されません。

> **Q&A**: agent 主導の Q&A は `boid task ask` (blocking RPC) に統一されており、 agent process は exit せず broker 接続を握ったまま回答を待ちます。 `BOID_AGENT_SESSION_ID` は廃止 (session-id resume 経路自体が削除されました)。 `BOID_USER_ANSWER` / `BOID_QUESTION_ID` はレガシー `notify --ask` 経路の awaiting レコードを再ディスパッチする際にしか set されず、 daemon はその再ディスパッチを行わないため実質的に非アクティブな env です。

加えて、 kit の `kit.yaml` で宣言した変数がすべて流し込まれます。

### 作業ディレクトリ

project が可視なタスクの hook cwd は、 sandbox 内に git gateway 経由で新規 clone された project のコピーのルートです (`/workspace/<project-name>` 相当。 host 側の project ディレクトリそのものではありません)。 root / child、 case 1/2/3 いずれでも同じ — 違いは clone の中でどの branch を checkout するか (case 1 は `base_branch` を直接、 child は `boid/<task_id8>` を新規作成) だけです。 詳細は [`project.yaml` リファレンス / タスク種別と HEAD branch](project-yaml.md#タスク種別と-head-branch) を参照してください。

これにより、 `git`、 `gh`、ビルドコマンド等は明示的にディレクトリ指定せずに使えます。

### ファイルシステムアクセス

hook はサンドボックス内で動きます。 読み書き可能なのは sandbox 内の clone (または readonly = true な supervisor タスクではどこも書けない — この場合も clone 自体はローカルに存在するが、 push が git gateway 側で拒否される) のみ。 kit が `additional_bindings` で宣言したパスは追加でマウントされます。 host のホーム / SSH 鍵 / 他プロジェクトは見えません。

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

hook が書ける trait は実質 `artifact` のみで、 何が許されるかは [`project.yaml`](project-yaml.md) の `task_behaviors.<name>.hooks[].traits.produces` 宣言で絞られます (kit は hook を提供しないため `kit.yaml` にこの宣言はありません)。 trait の意味は [概念 / payload と trait](../guide/concepts.md#payload-と-trait) を参照。

### stderr (ログ)

hook が stderr に書いた内容はジョブのログとして保存され、 `boid job show <job-id>` で読めます。 デバッグ情報はここに吐いてください。 stderr は payload patch には影響しません。

## 終了コード

| 終了コード | 扱い |
|---|---|
| `0` | 成功。 payload patch があればマージされる |
| 非 0 | ジョブを `failed` にマーク。 タスクは即 `aborted` にはならない (状態機械の自動遷移ルール次第) |

非 0 で終わった場合でも、 `payload_patch.json` を書いていればマージは試みられます。

## agent 用の追加コンテキスト

`kind: agent` で宣言した hook は、 instruction routing の対象になります。 routing 済みの instruction は `$HOME/.boid/context/instructions.yaml` と環境変数 `BOID_INSTRUCTIONS` 経由で参照できます。 たとえば claude-code kit の hook は `instructions.main` をコンテキストファイルから読んで agent への message として組み立てます。

`Instruction` のフィールドは [`project.yaml` リファレンス / Instruction](project-yaml.md#instruction) を参照。

## 最小例 (Bash)

```bash
#!/usr/bin/env bash
set -euo pipefail

# コンテキストファイルからタスクメタデータを読む (stdin は PTY のため読まないこと)
TASK_ID=$(yq -r .id "$HOME/.boid/context/task.yaml")
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
