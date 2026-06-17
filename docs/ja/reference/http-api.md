# HTTP API リファレンス

`boid` daemon が公開する `/api/*` エンドポイントの一覧です。 主な利用者は CLI (`boid` コマンド) と Web UI、 そして外部ツールから daemon を呼びたいスクリプトです。

このページは網羅性を狙ったリファレンスで、各エンドポイントの完全な JSON フィールド仕様まではカバーしません。データ型は [`internal/orchestrator/model.go`](https://github.com/novshi-tech/boid/blob/main/internal/orchestrator/model.go) と [`internal/orchestrator/spec_types.go`](https://github.com/novshi-tech/boid/blob/main/internal/orchestrator/spec_types.go) を参照してください。

## アクセス経路

`boid` daemon は 2 つの接続経路を持ちます。

| 経路 | アドレス | 認証 |
|---|---|---|
| UNIX socket (CLI 用) | `$XDG_RUNTIME_DIR/boid.sock` (fallback `/tmp/boid-<uid>.sock`) | OS のファイルパーミッション — 信頼済 (CLI・サンドボックス内エージェント) |
| HTTP listener (Web UI 用) | `127.0.0.1:8080` (既定、 `boid web set-addr` で変更) | デバイスセッション。データ/制御系 `/api/*` は TCP 経由では認証必須 |

CLI とサンドボックス内エージェントは UNIX socket 経由で `/api/*` を叩きます。 UNIX socket はファイルパーミッションで保護された信頼済み経路で、認証ゲートはかかりません。

HTTP/TCP listener 経由では、データ/制御系の `/api/*` は有効な `boid_session` cookie を要求します。 例外は 2 つ: `/api/health` は公開、 および最初のデバイスをペアリング (`boid web pair`) するまでの間は loopback bootstrap 例外で実ローカルブラウザを通します。 リバースプロキシ / トンネル経由のリクエストには bootstrap は適用されません。 未認証の呼び出しは `401` を返します。 `/api/*` は CSRF 免除ですが、 `boid_session` cookie は `SameSite=Lax` でクロスサイトリクエストを遮断します。

listener の既定 bind は **loopback のみ**です。 外部公開は (`boid web set-addr <addr>` + トンネル / リバースプロキシで) 意図的に行ってください。全インターフェースへの bind を既定にはしません。

curl で UNIX socket 経由に直接叩く例:

```bash
curl --unix-socket "$XDG_RUNTIME_DIR/boid.sock" http://localhost/api/health
```

## 共通の規約

### リクエスト / レスポンス

- リクエストボディと成功レスポンスは原則 JSON
- POST / PUT / PATCH の `Content-Type` は `application/json`
- 失敗レスポンスは `{"error": "<message>"}` 形式

### ID 表記

- `<id>` (タスク ID 等) は UUID 文字列
- `<project-ref>` は project の `id` または `name` の部分一致

## エンドポイント一覧

### サーバ管理

| Method | Path | 役割 |
|---|---|---|
| GET | `/api/health` | health check (200 で生存) |
| POST | `/api/shutdown` | daemon を停止 (`boid stop` 経路) |
| GET | `/api/proxy` | サンドボックス向け HTTP proxy のメタ情報 |

### Project

| Method | Path | 役割 |
|---|---|---|
| GET | `/api/projects` | 登録済みプロジェクト一覧 (クエリ: `workspace_id`) |
| POST | `/api/projects` | プロジェクト登録 (`{"work_dir": "<path>"}`) |
| GET | `/api/projects/{id}` | プロジェクト詳細 |
| DELETE | `/api/projects/{id}` | プロジェクトの登録を解除 |
| POST | `/api/projects/reload` | 全プロジェクトの `project.yaml` を再読み込み |
| GET | `/api/projects/{id}/commands` | このプロジェクトの `commands` 一覧 |
| GET | `/api/projects/{id}/commands/{name}` | 特定 command の詳細 |
| POST | `/api/projects/{id}/commands/{name}/execute` | 名前付きコマンドを実行 |
| PUT | `/api/projects/{id}/workspace` | workspace への割り当てを更新 |

詳細は [`project.yaml` リファレンス](project-yaml.md) と [CLI / project](cli.md#プロジェクト) を参照。

### Workspace

| Method | Path | 役割 |
|---|---|---|
| GET | `/api/workspaces` | ワークスペース一覧 |

### Task

| Method | Path | 役割 |
|---|---|---|
| GET | `/api/tasks` | タスク一覧 (クエリ: `status`、 `behavior`、 `workspace_id`、 `project_id`) |
| POST | `/api/tasks` | タスク作成 (本文は `taskCreateSpec` の JSON) |
| POST | `/api/tasks/import` | JSONL インポート (タスクを一括登録) |
| GET | `/api/tasks/{id}` | タスク詳細 |
| GET | `/api/tasks/{id}/detail` | タスク詳細 + 関連 actions / jobs (Web UI が使う詳細ビュー) |
| PATCH | `/api/tasks/{id}` | タスク更新 (`UpdateTaskRequest`: `payload` / `instructions` / その他フィールド) |
| DELETE | `/api/tasks/{id}` | タスク削除 (`?force=true` で状態チェックをスキップ) |
| POST | `/api/tasks/{id}/duplicate` | タスクを複製 |
| POST | `/api/tasks/{id}/rerun` | done / aborted を pending にリセットして再実行 |
| GET | `/api/tasks/{id}/hooks` | 現在の status で発火する hook 一覧 (クエリ: `status`) |
| GET | `/api/tasks/{id}/field` | タスクの特定フィールドの値を取得 |
| GET | `/api/tasks/{id}/commands` | タスクのプロジェクトコマンド一覧 |
| POST | `/api/tasks/{id}/commands/{name}/execute` | 名前付きプロジェクトコマンドをタスクのコンテキストで実行 |
| POST | `/api/tasks/{id}/hooks/{hook_id}/replay` | 特定 hook を再実行 |
| GET | `/api/tasks/{id}/events` | **SSE** ストリーム (タスクイベント) |
| POST | `/api/tasks/{id}/notify` | agent からの通知を送信。 `ask` フィールドがあると `awaiting` に遷移 |
| POST | `/api/tasks/{id}/answer` | `awaiting` タスクにユーザの回答を送信し `executing` に遷移 |

`POST /api/tasks` のリクエスト形式:

```json
{
  "id": "<uuid>",
  "project_id": "<id>",
  "title": "...",
  "description": "...",
  "behavior": "<name>",
  "auto_start": true,
  "payload": { ... },
  "instructions": { ... },
  "remote_id": "...",
  "traits": { ... },
  "ref": "...",
  "parent_id": "<uuid>"
}
```

`behavior` の代わりに `behavior_spec` を渡すと inline で behavior 設定を指定できます (詳細は [`project.yaml` / BehaviorSpec](project-yaml.md))。 `id` で呼び出し側が UUID を固定できます (冪等 create)。 `parent_id` で親タスクにリンクします。

### 通知と回答

エージェントがユーザに質問し、回答で再開する Q&A フローを制御する 2 つのエンドポイントです。

#### `POST /api/tasks/{id}/notify`

agent からユーザへ通知を送信します。 `ask` フィールドが存在するときは Q&A モードとなり、タスクを `executing → awaiting` に遷移させます。

リクエスト形式:

```json
{
  "message": "PR #42 をマージしてよいですか？",
  "ask": "マージしてよいですか？",
  "question_id": "q-550e8400",
  "session_id": "<agent-session-id>",
  "progress": true,
  "done": true,
  "fail": true
}
```

| フィールド | 必須 | 説明 |
|---|---|---|
| `message` | ◎ (`progress` 時は任意) | 通知テキスト。 `notify.command` スクリプトに `BOID_MESSAGE` として渡す |
| `ask` | | 質問テキスト。 指定時は `awaiting` に遷移して Q&A 待機に入る。 `done`/`fail`/`progress` と排他 |
| `question_id` | | Q&A ターンの UUID。 省略時は boid が自動生成する |
| `session_id` | | この通知を発した agent セッション ID |
| `progress` | | `true` のとき、タイムラインに progress エントリを記録するのみ (状態遷移なし) |
| `done` | | `true` のとき、タイムラインに `done_request` を記録。runtime 終了後にタスクが `done` に遷移する |
| `fail` | | `true` のとき、タイムラインに `fail_request` を記録。runtime 終了後にタスクが `aborted` に遷移する |

`ask`、 `done`、 `fail`、 `progress` は相互排他です。 子タスクからの FYI 通知 (いずれも指定なし) は無音 drop されます — ルートタスクの FYI のみ notify hook を発火します。

レスポンス: `204 No Content`

エラーコード:

| コード | 意味 |
|---|---|
| 400 | `message` が空 (必須の場合) |
| 404 | タスクが存在しない |
| 501 | `notify.command` が未設定 (サービスは常に wire 済みのため実質到達困難) |
| 409 | `ask` を指定したがタスクが `executing` 状態でない |

#### `POST /api/tasks/{id}/answer`

`awaiting` 状態のタスクにユーザの回答を送信します。 `payload.awaiting.pending_answer` に回答を設定し、タスクを `awaiting → executing` に遷移させて hook を再起動します。

リクエスト形式:

```json
{
  "question_id": "q-550e8400",
  "answer": "yes"
}
```

| フィールド | 必須 | 説明 |
|---|---|---|
| `question_id` | ◎ | 回答対象の Q&A ターンの UUID |
| `answer` | ◎ | 回答テキスト |

レスポンス: `204 No Content`

エラーコード:

| コード | 意味 |
|---|---|
| 400 | `question_id` または `answer` が空 |
| 404 | タスクが存在しない |
| 409 | タスクが `awaiting` 状態でない |

### Action

タスクに対する状態遷移アクションを発行します。

| Method | Path | 役割 |
|---|---|---|
| POST | `/api/tasks/{taskID}/actions` | action を送信 |

リクエスト形式:

```json
{
  "type": "start",
  "payload": { ... }
}
```

`type` は `start` / `done` / `reopen` / `ask` / `answer` / `abort` のいずれか。 `payload` は任意で、 action 固有のメタ情報を渡したいときに使います。 `ask` / `answer` の操作には上記の `/notify` / `/answer` エンドポイントを使う方が簡便です。

### Job

| Method | Path | 役割 |
|---|---|---|
| GET | `/api/jobs` | ジョブ一覧 (クエリ: `task_id`、 `status`、 `interactive`、 `taskless`) |
| GET | `/api/jobs/{id}` | ジョブ詳細 (status / exit_code / output) |
| PATCH | `/api/jobs/{id}` | ジョブのメタデータ更新 (例: `display_name`) |
| POST | `/api/jobs/{id}/done` | (内部) hook の終了を通知。 body に `exit_code` と `output` を受け取る。 payload patch ファイルも指定可 |
| GET | `/api/jobs/{id}/log` | ジョブログ。 `?follow=true` なしは `text/plain` スナップショット。 `?follow=true` ありは **SSE** ストリーム (`data: <line>` のみ、 `event:` フィールドなし) |
| GET | `/api/jobs/{id}/attach/ws` | **WebSocket** で実行中 runtime に attach (interactive ジョブ用) |
| POST | `/api/jobs/{id}/agent-stop` | 実行中 agent に停止シグナルを送信 |
| POST | `/api/jobs/{id}/attach` | (内部) 実行中ジョブに PTY/pipe を attach |
| POST | `/api/jobs/{id}/resize` | 実行中 interactive ジョブの PTY サイズ変更 |

### Secret

| Method | Path | 役割 |
|---|---|---|
| GET | `/api/secrets` | キー一覧 (値は返さない)。 namespace はクエリ `?namespace=` |
| POST | `/api/secrets` | secret 設定。 body: `{"key": "...", "value": "...", "namespace": "..."}` |
| DELETE | `/api/secrets/{key}` | 削除。 namespace はクエリ `?namespace=` |
| GET | `/api/secrets/{key}/value` | 値を取得 (sandbox / agent からの呼び出し前提) |

`POST` では namespace は **body フィールド** (query パラメータではない)。 `GET` / `DELETE` では namespace は **query パラメータ** (`?namespace=`)。いずれも省略時は `default`。

### GC

| Method | Path | 役割 |
|---|---|---|
| POST | `/api/gc` | GC を即時実行。 リクエストで `older_than`、 `dry_run` 等を渡せる |

daemon 起動時から自動 GC が走っているので、手動で叩く機会は少ないですが、開発時にデバッグ用途で使えます。 `dry_run: true` で実際に削除せずに対象を確認できます。

### Web UI 管理

[Web UI](../guide/web-ui.md) のペアリング・デバイス管理用エンドポイント。これらは認証ミドルウェアの保護下にあります。

| Method | Path | 役割 |
|---|---|---|
| POST | `/api/web/pair` | ペアリングコードを発行。 body (任意): `{"label": "...", "expires_in": "<duration>"}` |
| GET | `/api/web/devices` | ペアリング済みデバイス一覧 |
| DELETE | `/api/web/devices/{id}` | 特定デバイスを失効 |
| DELETE | `/api/web/devices` | 全デバイスを失効 |

加えて、 ペアリング画面の `/login` (HTML) と `/auth/redeem` (POST、 cookie 発行) があります。詳細は [Web UI 内部実装](../architecture/web-internals.md) を参照。

### Broker (内部用)

サンドボックス内の `boid` shim から host 側に届く要求のエンドポイント。通常はユーザが直接叩くものではありません。

| Method | Path | 役割 |
|---|---|---|
| GET | `/api/broker` | アクティブな broker トークン / shim 接続一覧 |
| POST | `/api/broker/register` | hook 起動時に shim 用トークンを発行 |

## SSE (Server-Sent Events)

主な SSE エンドポイントは `/api/tasks/{id}/events` です。 `/api/jobs/{id}/log` はオプションで SSE モードをサポートします。

### 共通 (SSE エンドポイント)

- Content-Type: `text/event-stream`
- 20 秒おきに `:ping\n\n` を送信し、プロキシのアイドル切断を防ぐ (events 系のみ)
- クライアント切断 (`r.Context().Done()`) でハンドラ側もクリーンアップ

### `/api/tasks/{id}/events`

タスクの状態変化や payload 更新を push します。 イベントは:

```
event: <kind>
data: <json>

```

詳細は [Web UI 内部実装 / SSE](../architecture/web-internals.md#server-sent-events-sse)。

### `/api/jobs/{id}/log`

- **`?follow=true` なし**: 現時点までのログを `text/plain` スナップショットとして返してクローズ
- **`?follow=true` あり**: SSE ストリーム。フォーマットは `data: <line>` のみ — `event:` フィールドはなし。このエンドポイントに `:ping` keepalive は**送信しない**。ジョブ終了で EOF

## エラーフォーマット

異常系では HTTP ステータスコードと共に次のような JSON を返します。

```json
{
  "error": "task not found"
}
```

ステータスコードの目安:

| コード | 意味 |
|---|---|
| 400 | リクエスト形式不正 |
| 403 | CSRF / Web auth 失敗 (HTTP listener 経由) |
| 404 | リソース無し |
| 409 | 状態遷移の前提が満たされない (例: 終端 status からの action) |
| 500 | 内部エラー |

## 関連ドキュメント

- [CLI リファレンス](cli.md) — 各エンドポイントを叩く CLI 経路
- [`project.yaml` リファレンス](project-yaml.md) — タスク作成時に必要な project / behavior の構造
- [Hook スクリプトプロトコル](hook-contract.md) — `POST /api/jobs/{id}/done` を呼ぶ EXIT trap の挙動
- [Web UI 内部実装](../architecture/web-internals.md) — 認証ミドルウェア、 SSE、 ルートマウント全体図
