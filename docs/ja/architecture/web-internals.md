# Web UI 内部実装

`boid` の Web UI がリクエストを受けてからレスポンスを返すまで、内部で何が起きているかをまとめたページです。 [アーキテクチャ概要](overview.md) の Web UI 節を、認証・セッション・SSE の粒度で掘り下げています。

主な読者は `internal/api/auth/` や `internal/api/events_handler.go` 周辺に手を入れる contributor です。 利用者向けの Web UI 概要は [Web UI](../guide/web-ui.md) にあります。

## コンポーネント一覧

```
+----------------------+        +-----------------------+
| Browser (or phone)   |        | boid daemon (HTTP)    |
|                      |        |                       |
|  GET /tasks          | -----> |  chi router           |
|  POST /tasks/<id>... |  HTTPS |    └─ middleware:     |
|  EventSource /events |        |       ├─ WebAuth      |
|                      |        |       ├─ CSRF         |
|                      | <----- |       └─ Templ + chi  |
|  cookies:            |        |                       |
|    boid_session      |        |  TaskEventHub (SSE)   |
|    csrf_token        |        |  RuntimeSubscriber    |
+----------------------+        +-----------------------+
                                          |
                                          v
                                +-----------------------+
                                | SQLite                |
                                |   web_devices         |
                                |   web_pairing_codes   |
                                +-----------------------+
```

主要コードの所在:

- ルータ組み立て: [`internal/server/server.go`](https://github.com/novshi-tech/boid/blob/main/internal/server/server.go) と `internal/server/wire.go`
- 認証ミドルウェアとペアリング: [`internal/api/auth/`](https://github.com/novshi-tech/boid/tree/main/internal/api/auth)
- HTTP ハンドラ: `internal/api/web.go`、 `internal/api/web_deps.go`
- SSE: `internal/api/events_handler.go` (タスクイベント)、 `internal/api/job_log_sse.go` (ジョブログ)
- テンプレート: `web/templates/` (Templ)、静的ファイル: `web/static/`

## 認証ミドルウェア

[`internal/api/auth/middleware.go`](https://github.com/novshi-tech/boid/blob/main/internal/api/auth/middleware.go) の `NewWebAuthMiddleware` がセッション cookie を検査します。

ロジック:

| cookie | リクエスト元 | 結果 |
|---|---|---|
| 無し | loopback、 かつ DB に device 登録なし | warn ログを出して通す (**bootstrap モード**) |
| 無し | それ以外 | `302 /login` |
| 不正な値 | (任意) | cookie をクリア + `302 /login` |
| 正常 | (任意) | 通す。 device の `last_seen_at` を更新 |

例外パス (チェック自体をスキップ): `/login`、 `/auth*`、 `/static/*`。

### bootstrap モードの安全性

「device がまだ 1 つも登録されていない loopback アクセス」 だけが認証なしで通る経路です。 これがあるおかげで、 初回 `boid start` の直後に手元の `http://localhost:8080` を開いてペアリング画面を見ることができます。

リバースプロキシ越しの偽 loopback を防ぐため、 `IsLoopback` (`internal/api/auth/loopback.go`) は `X-Forwarded-For`、 `CF-Connecting-IP`、 `Forwarded` ヘッダのいずれかが付いたリクエストを loopback として扱いません。 Cloudflare Tunnel から localhost にプロキシされた場合でも、 これらヘッダが付くため bootstrap 例外は発動しません。

## セッション cookie

[`internal/api/auth/session_store.go`](https://github.com/novshi-tech/boid/blob/main/internal/api/auth/session_store.go) の `SessionSigner` が cookie の発行と検証を行います。

### cookie の形

```
boid_session = <deviceID> "." <hex(HMAC-SHA256(secret, deviceID))>
```

- `<deviceID>` は `web_devices.id` (UUID)
- `<sig>` は HMAC-SHA256 を hex エンコードしたもの
- 鍵 `secret` は `~/.local/share/boid/web_secret` (パーミッション 0600) を daemon 起動時に読み込む

cookie 属性: `Path=/; HttpOnly; Secure; SameSite=Lax; Max-Age=7776000` (90 日)。 アイドル 30 日でも失効しますが、 これは DB 上の `last_seen_at` を見て middleware が判断します (現状はリクエストのたびに更新)。

### Verify の手順

1. `boid_session` cookie を取り出す
2. 末尾の `.` で分割して `deviceID` と `sig` に分ける
3. `HMAC-SHA256(secret, deviceID)` を計算し、 `hmac.Equal` で一致を確認 (timing-safe)
4. `web_devices` テーブルに該当 ID の生きている行があるか確認 (`revoked_at IS NULL`)
5. `last_seen_at` を `now()` で更新

不正な cookie や revoked された device の cookie は middleware が即時に `302 /login` に飛ばし、 cookie をクリアします。

## ペアリングフロー

[`internal/api/auth/pairing.go`](https://github.com/novshi-tech/boid/blob/main/internal/api/auth/pairing.go) の `PairingManager` がコード発行と引き換えを担当します。

```
host (CLI)               daemon (HTTP)              browser
========================================================
boid web pair      → POST /auth/pair
                  ←  code: WX7K-4QJP
"WX7K-4QJP"
を表示
                                                    GET /login
                                                    code 入力
                                                    POST /auth/redeem
                  ← session cookie + 302 /
```

### コード発行 (`Issue`)

1. `crypto/rand` で英数 8 文字をランダム生成、 中央にハイフンを入れて `WX7K-4QJP` 形式に
2. `SHA-256` でハッシュ化、 `web_pairing_codes(code_hash, label, created_at, expires_at)` に挿入
3. `expires_at = now() + 5 minutes`、 単回使用 (consumed_at が non-NULL になると以後使えない)

### コード引き換え (`Redeem`)

1. 入力されたコードを `SHA-256` でハッシュ化
2. `web_pairing_codes` を検索、 expires_at < now() または consumed_at != NULL なら拒否
3. 行を `consumed_at = now()` で更新 (atomic に取得 + 更新)
4. 新しい device row を `web_devices` に作成
5. その deviceID で `SessionSigner.Issue` を呼んで cookie を発行

### レート制限

[`internal/api/auth/ratelimit.go`](https://github.com/novshi-tech/boid/blob/main/internal/api/auth/ratelimit.go):

- IP ごとに 5 分窓で 5 回まで
- 超えると 15 分ロック
- ロック中は 429 で即時拒否
- in-memory のみ (daemon 再起動でリセット、 永続化しない)

ペアリング画面と redeem エンドポイントに適用されます。

## CSRF

[`internal/api/auth/csrf.go`](https://github.com/novshi-tech/boid/blob/main/internal/api/auth/csrf.go) で **double-submit cookie** 方式を実装しています。

### GET / HEAD / OPTIONS / TRACE

- `csrf_token` cookie が無ければ、ランダム値を生成して set
- pass through

### POST / PUT / PATCH / DELETE

1. `csrf_token` cookie が必須 (無ければ 403)
2. `X-CSRF-Token` ヘッダの値が cookie の値と一致することを要求 (無ければ / 不一致なら 403)

JS 側 (HTMX) は `hx-headers` で `X-CSRF-Token` を付けるよう設定しています。 `Origin` / `Referer` チェックではなく cookie とヘッダの一致だけを見るので、リバースプロキシ環境でも動きます。

## Server-Sent Events (SSE)

`boid` は 2 系統の SSE エンドポイントを持ちます。

### タスクイベント (`/events/tasks/<id>`)

`internal/api/events_handler.go` の `WebHandler.TaskEvents` がハンドラ。タスクの状態変化や payload 更新を browser に push します。

実装のポイント:

- レスポンスヘッダ: `Content-Type: text/event-stream`、 `Cache-Control: no-cache`、 `Connection: keep-alive`
- `h.Hub.Subscribe(ctx, taskID)` で `TaskEventHub` (in-memory pub/sub) を購読
- ループの各 iteration:
  - チャネルからイベント受信 → `event: <kind>\ndata: <json>\n\n` を書いて flush
  - 20 秒ごとに `:ping\n\n` を送り、プロキシのアイドル切断を防ぐ
  - `r.Context().Done()` で client 切断を検知 → return

`TaskEventHub` 自体は `internal/api/task_event_hub.go` 等で実装されており、 dispatch ループが状態遷移を行うたびにイベントを publish しています。

### ジョブログ (`/events/jobs/<id>/log`)

`internal/api/job_log_sse.go` の `JobLogSSEHandler` がハンドラ。 hook / gate のリアルタイム stdout/stderr を browser に流します。

- snapshot (現時点までのログ) を最初に送信
- 以降は `RuntimeSubscriber.Subscribe` でランタイム側から流れてくる差分を append

ジョブが終了すると subscriber チャネルが閉じ、 SSE もクローズします。

## ルートのマウント

[`internal/server/wire.go`](https://github.com/novshi-tech/boid/blob/main/internal/server/wire.go) でルートが組み立てられます。代表的な構造:

```
chi.Router
├── /static/*           (静的アセット)
├── /login              (ペアリング画面)
├── /auth/pair          (POST: コード発行)
├── /auth/redeem        (POST: コード引き換え)
├── /auth/logout        (POST: cookie クリア + device revoke)
├── (以下は WebAuthMiddleware + CSRFMiddleware の保護下)
├── /                   (タスク一覧)
├── /tasks/<id>         (タスク詳細)
├── /projects/<id>      (プロジェクト詳細)
├── /jobs/<id>          (ジョブ詳細)
├── /events/tasks/<id>  (SSE: タスクイベント)
└── /events/jobs/<id>/log (SSE: ジョブログ)
```

## 関連ドキュメント

- [Web UI](../guide/web-ui.md) — ユーザ視点でのペアリングと Cloudflare Tunnel
- [アーキテクチャ概要](overview.md) — Web UI レイヤの位置づけ
- [永続化レイヤ](persistence.md) — `web_devices` / `web_pairing_codes` テーブルの定義
