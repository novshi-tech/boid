# Web UI 再構築 実装計画

## 背景と目的

boid は現在 TUI を主力 UI として機能開発してきた。Web UI は以下の価値を提供するために再構築する:

- **モバイルファースト**: Cloudflare Tunnel 経由でスマートフォンから TUI と同等の操作ができる
- **シングルユーザ認証**: トンネルで公開される前提で、パーソナルツールとして安全に使える
- **(後回し) ブラウザで対話 PTY**: xterm.js 経由で interactive ジョブに直接接続する

## 設計原則

1. **TUI 完全移植**: 画面スタック・filter・ツリー・インライン編集・動的 action を Web に写像する。新規機能は足さない
2. **CLI ファースト運用**: 認証のペアリング・デバイス失効は CLI で行う。Web 側には revoke UI を置かない
3. **外部依存は最小**: Go 側は標準 + x/crypto + 新規 2 本 (`rsc.io/qr`, `github.com/coder/websocket`) に留める。JS 側は HTMX + xterm.js のみ vendored
4. **層責務を守る**: reducer/orchestrator を越境せず、SSE hub も API 層に閉じ込める (`project_layer_responsibilities.md` の方針を踏襲)
5. **モバイル UX を TUI モデルに寄せる**: グローバルナビなし・パンくずのみ・ルートは TaskList のみ

## 決定事項サマリ

### A. 認証 (Phase 1)

| ID | 内容 | 確定内容 |
|---|---|---|
| A-1 | ペアリングコード形式 | 英数 8 桁 + ハイフン (例: `WX7K-4QJP`) |
| A-2 | コード寿命・使い回し | 5 分 / 単回使用 |
| A-3 | デバイス Cookie 寿命 | 90 日ローリング / アイドル 30 日 |
| A-4 | Cookie 属性 | `HttpOnly; Secure; SameSite=Lax; Path=/` |
| A-5 | 未ペアリング時のアクセス | loopback (127.0.0.1/::1) のみ許可、他は `/login` にリダイレクト |
| A-6 | CSRF | double-submit cookie + HTMX `hx-headers` で `X-CSRF-Token` |
| A-7 | レート制限 | IP 毎 5 回/5 分、超過で 15 分ロック。memory-only |
| A-8 | QR 生成 | `rsc.io/qr` + 自前 ASCII 描画 (半高ブロック、ASCII フォールバック) |
| A-9 | Cookie 署名鍵 | `~/.local/share/boid/web_secret` に初回自動生成 (0600) |
| A-10 | public URL | `~/.config/boid/config.yaml` の `web.public_url` に手動設定 |

### B. UI (Phase 1-2)

| ID | 内容 | 確定内容 |
|---|---|---|
| B-1 | CSS | 自前継続、`style.css` を 0 から書き直し |
| B-2 | ダークモード | `prefers-color-scheme` 自動追随のみ |
| B-3 | アイコン | Heroicons (MIT) の SVG を `web/templates/components/icon.templ` に直書き |
| B-4a | グローバルナビ | なし、パンくずのみ (TUI の画面スタック踏襲) |
| B-4b | `/jobs` `/projects` | 作らない、TaskList の filter に抜き出しリンク |
| B-4c | Me ページ | 作らない。device 管理は CLI (`boid web devices` 等) |

### C. HTMX (Phase 1-)

| ID | 内容 | 確定内容 |
|---|---|---|
| C-1 | バージョン | HTMX v2.0.x |
| C-2 | 配信 | `web/static/vendor/htmx-2.0.x.min.js` を commit + `//go:embed` |
| C-3 | SSE 実装 | 素の `EventSource` + 手書き fetch (extension 不使用) |

### D. 閲覧系 (Phase 2)

| ID | 内容 | 確定内容 |
|---|---|---|
| D-1 | ツリー折り畳み状態 | localStorage (デバイスローカル) |
| D-2 | SSE 粒度 | タスク単位のみ (`/api/tasks/{id}/events`)、リストは 5 秒ポーリング |

### E. 編集・アクション系 (Phase 3)

| ID | 内容 | 確定内容 |
|---|---|---|
| E-1 | タスク作成フォーム | TUI task_form と同等の全フィールド |
| E-2 | インライン編集 UX | フルページルート (`/tasks/{id}/edit/*`) |
| E-3 | 確認ダイアログ | destructive action (abort / rerun / gate replay) のみ |
| E-4 | ジョブ tail 認証 | 既存 Cookie、素 SSE でプレーン行配信 |
| E-5 | payload 編集 | TUI と同じ top-level key 別 YAML section 編集 (kit schema 不在が判明) |

### F. PTY (Phase 4、後回し)

| ID | 内容 | 確定内容 |
|---|---|---|
| F-1 | WebSocket サーバ | `github.com/coder/websocket` (新規依存 1) |
| F-2 | xterm.js 配信 | `web/static/vendor/` にバンドル済み ESM を embed、addon は fit-addon のみ |
| F-3 | 複数 attach | 全員可読可書きのミラーモード、resize は最後の resize 優先 |

### G. 運用・横断

| ID | 内容 | 確定内容 |
|---|---|---|
| G-1 | 計画ドキュメント | `docs/plans/web-ui-rebuild.md` (本ファイル) |
| G-2 | テスト戦略 | 既存 `stubWebService` 継承、SSE/WS は `httptest` で軽く、ブラウザ E2E なし |
| G-3 | CLAUDE.md | コンパクトな概要を追記、詳細は本ファイル参照 |

## 事前調査結果 (R-1〜R-3)

### R-1: SSE hub の置き場所

- 既存の pub-sub / observer パターンは**存在しない** → 新設必要
- イベント発火点 3 箇所はいずれも `internal/api/service.go`:
  - `TaskWorkflowService.ApplyAction` (status 遷移 + action INSERT)
  - `TaskWorkflowService.CompleteJob` (job 状態変化 + job_failed action)
  - `persistFiredEvents` (hook_fired / exit_gate_fired / entry_gate_fired)
- 推奨: `internal/api/task_event_hub.go` に `TaskEventHub` struct を新設、`TaskWorkflowService` にフィールド保持、`Tx.WithinTx()` コミット直後に `broadcast(taskID, event)`
- **broadcast は必ず commit 成功後**に呼ぶ (失敗した action を push しないため)
- subscribers 解除は context キャンセルで (EventSource 切断時に handler の `ctx.Done()` を watch)

### R-2: Task ツリーの primitive

- **ParentID** (string) がツリー primitive。DependsOn は論理依存であり display tree には使わない
- 参考実装: `internal/tui/task_list.go:719-792` の `buildTreeOrder` (DFS、prefix map 構築)
- `orchestrator.Task` (`internal/orchestrator/model.go:19-49`) に既に以下フィールド:
  - `ParentID`, `DependsOn`, `DependsOnPayload`
  - 集計フィールド: `TotalChildCount` / `DoneChildCount` / `AbortedChildCount` / `OpenChildCount`
- `TaskDetailView` (`internal/api/service.go:43-52`) に `DependsOnTree` / `DependentsTree` が存在 → タスク詳細の依存タブはこれで済む
- `TaskFilter` (`internal/orchestrator/store.go:18-25`) は `Status` / `ProjectID` / `Behavior` / `WorkspaceID` / `HasDependsOn` / `NoDependsOn` を既サポート → filter UI は新規エンドポイント不要

### R-3: payload schema / instructions role / actions

- **kit に payload schema は存在しない**。`RawPayload` = `json.RawMessage` で dynamic validation のみ
- TUI の `internal/tui/payload_section_edit.go:45-187` は top-level key ごとに JSON→YAML 変換してエディタで編集 → Web も同方式
- Instructions role は `map[string]Instruction`、`project.yaml` の `default_instructions` に自由定義、TUI は `extractInstructionRoles` で map keys を列挙 (`internal/tui/task_detail_instructions.go:19-32`)
- AvailableActions: `StateMachine.AvailableActions(status)` (`internal/orchestrator/machine.go:86-103`) が manual=true の Rule のみ返却 (start / done / reopen / abort / job_failed)
- **Rerun は AvailableActions に出ず別エンドポイント** `/api/tasks/{id}/rerun` (`internal/api/task.go:29`)
- 既存 API:
  - `POST /tasks` (`CreateTaskRequest`)
  - `PATCH /tasks/{id}` (`UpdateTaskRequest` で Payload / Instructions / Description 等を部分更新可) ← **Phase 3 で流用**
  - `GET /tasks/{id}/detail` (`TaskDetailView` に AvailableActions 同梱)

## 認証設計 (Phase 1 詳細)

### ペアリングフロー

```
[CLI 側]                         [サーバ]                     [ブラウザ]
 boid web pair
   │                                │
   ├── generate code (8-digit hex)──┤ INSERT web_pairing_codes
   │                                │ (code_hash, expires_at)
   ├── stdout: code + URL + QR ─────┤
   │                                │
   │                                │   ユーザが code を入力
   │                                │   or URL 踏む
   │                                │
   │                                │◄── GET/POST /auth
   │                                │    (code or ?token=...)
   │                                │
   │                                ├── verify + consume
   │                                │   INSERT web_devices
   │                                │   set session cookie
   │                                │
   │                                ├── 302 → /
```

### DB スキーマ

```sql
-- セッションデバイス
CREATE TABLE web_devices (
  id TEXT PRIMARY KEY,           -- UUID
  label TEXT,                    -- ユーザ付与ラベル、pair 時 --label
  cookie_hash BLOB NOT NULL,     -- HMAC-SHA256(secret, device_id || version)
  created_at TIMESTAMP NOT NULL,
  last_seen_at TIMESTAMP NOT NULL,
  revoked_at TIMESTAMP            -- NULL なら有効
);

-- ワンタイムペアリングコード
CREATE TABLE web_pairing_codes (
  code_hash BLOB PRIMARY KEY,    -- SHA-256(code)
  label TEXT,                    -- pair --label の値
  created_at TIMESTAMP NOT NULL,
  expires_at TIMESTAMP NOT NULL,
  consumed_at TIMESTAMP           -- NULL なら未使用
);
```

### Cookie 形式

```
boid_session=<device_id>.<HMAC-SHA256(secret, device_id || last_seen_epoch_hour)>
```

- `last_seen_epoch_hour` は 1 時間粒度でローテーション = cookie の自然失効を 90 日ローリングで実現
- 署名検証 → DB の `web_devices` を引き `revoked_at IS NULL` を確認 → `last_seen_at` を UPDATE

### ミドルウェアチェーン (`internal/server/wire.go`)

```
chi Router
 └── logging
     └── LoopbackExempt (for /api/* administered by CLI; unchanged)
         └── [static /static/* (unauthenticated)]
         └── WebAuth (apply to all /*, /api/* except /login, /auth, /static)
             ├── missing cookie:
             │     if remote is loopback AND devices table empty → allow with warning
             │     else → redirect /login
             ├── invalid cookie → clear + redirect /login
             └── valid cookie → set last_seen_at; continue
```

### CLI サブコマンド

| コマンド | 動作 |
|---|---|
| `boid web pair [--label NAME]` | code 発行、QR + URL 表示。コード / URL / QR を stdout に |
| `boid web devices` | 現在有効なデバイス一覧 (id 短縮・label・last_seen・created_at) |
| `boid web revoke <id>` | 特定 device を失効 |
| `boid web revoke-all` | 全 device 失効 (紛失時用) |
| `boid web set-url <URL>` | `config.yaml` の `web.public_url` を書き換え |

### config.yaml 追加

```yaml
web:
  enabled: true                        # 既存
  public_url: https://boid.example.com # A-10、未設定なら QR に URL なし
  # 将来: access_jwt_team (Cloudflare Access 二段目を入れる時の拡張ポイント)
```

### 認証されない状態でのアクセス

- `--web` 起動後、`web_devices` が 1 行もなく、かつ remote が loopback なら「パスする & daemon ログに `boid web pair` 実行を促す警告」
- その状態で外部 IP アクセスは即 `/login`
- `/login` では pairing code 入力フォームのみ

## UI 設計 (Phase 1-2)

### パンくず

```
┌────────────────────────────────┐
│ ← Tasks / abc-task title       │  ← 階層が深いほどセグメント追加
├────────────────────────────────┤
│ (コンテンツ)                   │
└────────────────────────────────┘
```

- ルート `/` では `Tasks` 単独 (back arrow なし)
- `/tasks/{id}` では `← Tasks / {title}`
- `/tasks/{id}/edit/description` では `← Tasks / {title} / description`
- 各セグメントは祖先 URL へのリンク、back arrow は `window.history.back()` 相当

### モバイル / デスクトップ切替

- ブレークポイント 1 本: `@media (min-width: 768px)` → desktop
- モバイルはフルワイド、1 カラム
- デスクトップは max-width 960px 中央寄せ (将来 split 対応はここを 2 カラムに)

### デザイントークン (`style.css` 冒頭)

```css
:root {
  --bg: #ffffff;
  --fg: #1a1a1a;
  --muted: #666;
  --border: #e0e0e0;
  --accent: #0066cc;
  --danger: #cc3333;
  --success: #2a8a3f;
  --warning: #b06a00;
  --card: #fafafa;
  --space-1: 4px;
  --space-2: 8px;
  --space-3: 16px;
  --space-4: 24px;
  --space-5: 32px;
  --radius-1: 4px;
  --radius-2: 8px;
  --tap-target: 44px;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #0d0d0d;
    --fg: #e6e6e6;
    --muted: #999;
    --border: #2a2a2a;
    --accent: #4d9fff;
    --card: #1a1a1a;
  }
}
```

### アイコン (`web/templates/components/icon.templ`)

Heroicons から必要分のみ直書き:

- `IconTask` / `IconJob` / `IconProject`
- `IconBack` (chevron-left)
- `IconCheck` / `IconDanger` / `IconClock`
- `IconChevronRight` / `IconChevronDown` (ツリー折り畳み)
- `IconEdit`
- `IconTerminal` (Phase 4)

## Phase 定義

各 Phase は独立した auto_plan タスクとして投下する。Phase は順序依存あり (Phase 1 完了 → Phase 2 開始)。

---

### Phase 1: 認証 + モバイル CSS + HTMX 基盤

**Deliverable**: Cloudflare Tunnel 経由で外部から Web UI に安全にアクセスでき、既存の閲覧機能がモバイル対応した状態

**前提**: なし

**スコープ内**:
- 認証システム (ペアリング、cookie session、CSRF、rate limit、loopback exempt)
- CLI サブコマンド (`pair` / `devices` / `revoke` / `revoke-all` / `set-url`)
- HTMX v2 embed
- layout.templ 全面書き換え (パンくず + HTMX script)
- style.css 全面書き直し (モバイルファースト、デザイントークン)
- 既存画面 (TaskList / TaskDetail / JobList / JobDetail) のマークアップ微調整 — **既存の情報密度・機能は保ったまま** CSS クラス名だけ新体系に揃える。ツリー化・SSE は Phase 2
- Heroicons コンポーネント雛形
- `/login`, `/auth` ルート + ログイン画面
- config.yaml に `web.public_url` 追加
- CLAUDE.md に Web UI セクション追記

**スコープ外** (Phase 2 以降):
- タスクツリー表示
- multi-filter UI 拡張
- SSE によるライブ更新
- /jobs /projects ルート削除 (Phase 2 でまとめて対処、Phase 1 時点では存在したまま)
- タスク作成・編集 UI

**主要な変更対象**:

| 変更種別 | ファイル / ディレクトリ |
|---|---|
| 新規 | `internal/api/auth/` (package; session, csrf, ratelimit, middleware) |
| 新規 | `internal/api/auth/pairing.go`, `session_store.go`, `loopback.go` |
| 新規 | `web/templates/login.templ`, `web/templates/auth.templ` |
| 新規 | `web/templates/components/icon.templ`, `breadcrumb.templ` |
| 新規 | `web/static/vendor/htmx-2.0.x.min.js` |
| 新規 | `cmd/web.go` (pair / devices / revoke / set-url サブコマンド) |
| 新規 | DB migration: `web_devices`, `web_pairing_codes` テーブル (既存 migration 機構に従う) |
| 新規 | `internal/qrterm/` (rsc.io/qr ラッパ; 半高ブロック描画, ASCII fallback) |
| 変更 | `internal/server/wire.go` (認証 middleware chain) |
| 変更 | `internal/config/*` (web.public_url 追加) |
| 変更 | `web/templates/layout.templ` (パンくず + HTMX script) |
| 変更 | `web/static/style.css` (**全面書き直し**) |
| 変更 | `web/embed.go` (vendor ディレクトリを embed に含める) |
| 変更 | 既存 `tasks.templ` / `jobs.templ` / `projects.templ` のクラス置換 (最小限) |
| 変更 | `CLAUDE.md` (Web UI セクション追記) |
| 依存追加 | `rsc.io/qr` (go.mod) |
| 依存追加 (既にあれば無視) | `golang.org/x/crypto/bcrypt` は不要 (パスワードなし)。HMAC は `crypto/hmac` + `crypto/sha256` 標準のみ |

**着手前調査** (Phase 1 auto_plan が最初に流す):
- 既存 DB migration の追加方法 (`internal/db/` または `internal/orchestrator/store.go` を確認)
- `~/.local/share/boid/` 下へのファイル書き込み慣例 (既存コードの参照)

**dev タスク分割目安** (auto_plan の guide):
1. DB migration + `web_devices` / `web_pairing_codes` テーブル + store 関数
2. 署名鍵自動生成 + cookie signer + session lookup/check
3. Pairing code 生成 + redeem エンドポイント + rate limiter
4. QR 描画ユーティリティ (`internal/qrterm`)
5. CLI サブコマンド (`boid web pair` / `devices` / `revoke` / `revoke-all` / `set-url`)
6. CSRF middleware + loopback exempt middleware + auth middleware + wire.go 組込
7. `/login` 画面 templ + `/auth` ルート
8. HTMX embed + `layout.templ` 書き換え + `breadcrumb.templ` + `icon.templ` 初期版
9. `style.css` モバイルファースト書き直し + 既存 templ のクラス置換
10. CLAUDE.md 更新 + README 補足

**並列 dev タスクのコンフリクト注意**:
- `wire.go` は 1, 2, 6 で触るので直列化 (`depends_on` + `artifact.auto-merge.merged`)
- `layout.templ` は 8, 9 で触るので 8 → 9 の順
- `style.css` は 9 単独 (完全書き直しのため 1 タスクに収める)
- CLI サブコマンド (5) は `cmd/` 配下で他タスクと重ならないので並列可

**テスト方針**:
- `stubWebService` を流用して `/login` / `/auth` の handler テスト
- session 発行・検証の単体テスト
- rate limiter の単体テスト (時刻を mock)
- CSRF ミドルウェアのテスト
- E2E: `e2e/scenarios/web-auth/` を新規 (CI で実行、ローカル実行は環境依存のためスキップ可)

**受け入れ基準**:
- [ ] `boid web pair` でコード発行、`/auth?token=...` で redeem、session cookie 発行
- [ ] CSRF トークン未付で POST すると 403
- [ ] レート制限超過で 429
- [ ] loopback からは device 未登録でも `/` アクセス可
- [ ] 外部 IP は device 未登録なら `/login` へ
- [ ] モバイルサイズ (375px 幅) で既存画面が破綻しない
- [ ] `go test ./... -race` パス

---

### Phase 2: 閲覧系 TUI 機能移植

**Deliverable**: タスクツリー表示・multi-filter・ライブタイムラインが機能し、TUI と同水準の閲覧体験がモバイルで得られる

**前提**: Phase 1 merge 済み

**スコープ内**:
- タスクツリー表示 (DFS + 折り畳み)
- multi-filter UI (status / project / behavior / workspace / title search)
- SSE hub (`TaskEventHub`) 新設 + broadcast 発火 3 箇所
- `GET /api/tasks/{id}/events` エンドポイント
- task detail のライブ更新 (timeline / status / jobs)
- task list の 5 秒ポーリング (HTMX `hx-trigger="every 5s"`)
- `/jobs` `/projects` ルート **削除**
- Projects 相当の体験は filter ドロップダウンの「プロジェクト選択」に統合

**スコープ外** (Phase 3 以降):
- タスク作成・編集
- 動的 action の拡張
- ジョブ出力 live tail

**主要な変更対象**:

| 変更種別 | ファイル / ディレクトリ |
|---|---|
| 新規 | `internal/api/task_event_hub.go` (`TaskEventHub` struct + broadcast / subscribe) |
| 新規 | `internal/api/events_handler.go` (SSE endpoint) |
| 新規 | `web/templates/components/task_tree.templ` |
| 新規 | `web/templates/components/filters.templ` |
| 変更 | `internal/api/service.go` (`TaskWorkflowService` に hub フィールド、3 箇所 broadcast) |
| 変更 | `internal/api/store.go` (WebService に `ListProjects` 等既存ベース、ツリー build は handler 側) |
| 変更 | `internal/api/web.go` (`/` で tree build & filter apply、`/jobs` `/projects` 削除、`/tasks/{id}` に EventSource 用 JS 同梱) |
| 変更 | `web/templates/tasks.templ` (tree 対応、filter UI) |
| 削除 | `web/templates/jobs.templ` (list 部分のみ、job detail は残す) |
| 削除 | `web/templates/projects.templ` |

**着手前調査** (Phase 2 auto_plan が最初に流す):
- `Tx.WithinTx()` のパターン確認 (`internal/orchestrator/store.go`) — broadcast を commit 後に呼ぶための合流点
- `persistFiredEvents` の呼び出し経路 (`internal/api/service.go`) — event payload の正確な形を把握
- EventSource の IIFE JS 雛形が layout.templ に入るか、page-local script に入るかの判断

**dev タスク分割目安** (auto_plan の guide):
1. `TaskEventHub` 新設 + subscribe/unsubscribe + broadcast を unit test
2. `service.go` の 3 箇所 (`ApplyAction` / `CompleteJob` / `persistFiredEvents`) で broadcast 呼び出しを追加
3. `/api/tasks/{id}/events` SSE handler 実装 + httptest で 2-3 イベント受信確認
4. task list の tree build ロジック + `task_tree.templ`
5. filter UI + URL query バインド + 5 秒ポーリング
6. task detail に EventSource JS 組込 + 部分 HTML fragment renderer
7. `/jobs` `/projects` ルート削除 (handler + templ ファイル + `wire.go` の mount 削除)

**並列コンフリクト**:
- `service.go` は 1, 2 で触るので直列 (hub 実装 → broadcast 組込)
- `web.go` は 3, 4, 5, 6, 7 全てで触る → **基本直列**。分割は 1 PR 目 (3 + 4)、2 PR 目 (5 + 6)、3 PR 目 (7) を推奨
- `tasks.templ` は 4, 5, 6 で触る → 直列

**テスト方針**:
- `TaskEventHub` の並行サブスクライブテスト (`go test -race` 前提)
- SSE endpoint に `httptest.NewServer` で接続、`bufio.NewScanner` でイベント行を最大 3 行読み取り検証
- tree build 関数の単体テスト (親子・循環なし・ソート順)
- filter パラメータ適用の handler テスト

**受け入れ基準**:
- [ ] 親子関係のあるタスクがツリー表示される
- [ ] folding 状態が localStorage で保持される
- [ ] filter (project / behavior / status / search) が動作、URL クエリに反映
- [ ] タスク詳細を開いた状態で別 shell から action を起こすと、リロードせず timeline に追記
- [ ] タスク list ページが 5 秒ごとに差分更新
- [ ] `/jobs` `/projects` にアクセスすると 404
- [ ] `go test ./... -race` パス

---

### Phase 3: 編集・アクション系 TUI 機能移植

**Deliverable**: Web からタスク作成・description/payload/instructions/deps 編集・動的 action (rerun, gate replay 含む) 実行・ジョブ出力ライブ tail が可能

**前提**: Phase 2 merge 済み

**スコープ内**:
- `/tasks/new` + `POST /tasks`
- `/tasks/{id}/edit/description` + POST
- `/tasks/{id}/edit/payload` (section 一覧) + `/tasks/{id}/edit/payload/{section}` (YAML editor) + POST
- `/tasks/{id}/edit/instructions/{role}` + POST
- `/tasks/{id}/edit/deps` + POST
- Rerun / gate replay の動的 action ボタン拡張
- destructive action の confirm ダイアログ
- `GET /api/jobs/{id}/log?follow=true` SSE tail
- job detail に EventSource 組込で live 追加表示

**スコープ外**:
- xterm.js (Phase 4)

**主要な変更対象**:

| 変更種別 | ファイル / ディレクトリ |
|---|---|
| 新規 | `web/templates/task_form.templ` (新規作成) |
| 新規 | `web/templates/edit_description.templ` / `edit_payload.templ` / `edit_instructions.templ` / `edit_deps.templ` |
| 新規 | `internal/api/job_log_sse.go` (SSE tail handler) |
| 変更 | `internal/api/web.go` (route 追加、各 POST handler) |
| 変更 | `internal/api/store.go` WebService (Payload / Instructions / Deps update 用メソッド。既存 PATCH /tasks/{id} を流用可能) |
| 変更 | `web/templates/tasks.templ` (task detail の各セクションに edit アイコン) |
| 変更 | `web/templates/jobs.templ` (Phase 2 で list 削除済、detail のみ残している) → live tail UI 追加 |

**着手前調査** (Phase 3 auto_plan が最初に流す):
- **gate replay のエンドポイント探索**: TUI の JobDetailScreen `R` キーの実装を読み、どの API を呼んでいるか。`/api/jobs/{id}/replay` 的なものがあるか、あるいは内部関数を handler 経由で公開する必要があるか
- 既存 `PATCH /tasks/{id}` が description / payload / instructions の部分更新をサポートしているか (`UpdateTaskRequest` フィールド確認)
- `persistFiredEvents` 経由の gate replay イベントが SSE hub に正しく流れるか
- transcript log の既存 subscriber mechanism (`internal/dispatcher/runtime_local_linux.go`) から SSE に流すアダプタ設計

**dev タスク分割目安** (auto_plan の guide):
1. タスク作成画面 `/tasks/new` + `POST /tasks` 統合
2. description 編集画面 + POST
3. payload section 一覧 + section 別 YAML editor + POST
4. instructions role 別編集 + POST
5. deps 編集 + POST
6. Rerun / gate replay 動的ボタン + 確認ダイアログ
7. ジョブ出力 SSE tail endpoint
8. job detail の EventSource 組込

**並列コンフリクト**:
- `web.go` の route 定義は 1, 2, 3, 4, 5, 6 で触る → 直列
- `tasks.templ` は 2, 3, 4, 5 で触る → 直列
- 7 と 8 は独立ファイルなので並列可 (web.go の mount を 7 で触るのみ、8 は templ)

**テスト方針**:
- 各 POST handler の stubWebService テスト
- YAML validation 失敗時のエラー応答テスト
- 確認ダイアログは onsubmit="return confirm(...)" なので JS テストなし、templ 出力 diff 確認
- SSE tail は transcript file の fake subscriber で mocktest

**受け入れ基準**:
- [ ] Web で作成したタスクが CLI `boid task list` で見え、フィールド値一致
- [ ] payload の各 section を YAML で編集・保存できる
- [ ] instructions を role 別に編集できる
- [ ] Rerun ボタンで /api/tasks/{id}/rerun が呼ばれる
- [ ] gate replay が TUI と同等に動く
- [ ] abort を押すと `confirm('本当に中止しますか？')`
- [ ] job detail を開いて live 実行中のジョブの出力が追記される
- [ ] `go test ./... -race` パス

---

### Phase 4: xterm.js PTY (後回し、要件発生時に着手)

**Deliverable**: ブラウザから interactive ジョブの PTY に接続、xterm.js でターミナル操作できる

**前提**: Phase 3 merge 済み (ただし独立度高。Phase 3 と並走も可)

**スコープ内**:
- `coder/websocket` 依存追加
- `GET /api/jobs/{id}/attach/ws` WS エンドポイント
- xterm.js vendored embed
- `/tasks/{id}/terminal` ページ
- fit-addon 連携、resize forwarding
- multi-client ミラーモード
- task detail に "open terminal" CTA

**主要な変更対象**:

| 変更種別 | ファイル / ディレクトリ |
|---|---|
| 新規 | `internal/api/ws_attach.go` |
| 新規 | `web/static/vendor/xterm-5.x/` (xterm.js, xterm.css, addon-fit) |
| 新規 | `web/templates/terminal.templ` |
| 新規 | `web/templates/components/xterm_loader.templ` |
| 変更 | `internal/dispatcher/runtime_local_linux.go` (必要なら subscribe API の export / mirror write 経路) |
| 変更 | `internal/api/job_runtime_routes.go` (既存 attach と併設) |
| 変更 | `web/templates/tasks.templ` (open terminal CTA) |
| 依存追加 | `github.com/coder/websocket` (go.mod) |

**着手前調査** (R-5):
- `localRuntimeSession` (`internal/dispatcher/runtime_local_linux.go`) の以下を確認:
  - `subscribers` フィールドの export 状況 (外部 package から使えるか)
  - PTY master への write 口 (single writer / mutex 状態)
  - subscriber ID 払い出しの race 条件
  - runtime 終了時の cleanup で broadcast 切断が確実に起きるか
- tmux 統合との干渉 (TUI の `o` キーで tmux pane を開くフローと、Web からの WS attach が同時に走った時)

**dev タスク分割目安**:
1. R-5 調査完了 & 必要に応じて runtime session に export 追加
2. WS endpoint 実装 + 認証 hook
3. xterm.js vendor + /tasks/{id}/terminal 画面
4. fit-addon + resize forwarding
5. task detail に CTA + 起動導線

**並列コンフリクト**:
- `runtime_local_linux.go` を 1 で触り、2 以降は handler 経由なので直列は 1 → 2 のみ

**テスト方針**:
- WS endpoint を `coder/websocket/wstest` (あれば) もしくは `net/http/httptest` + Dial で疎通テスト
- xterm.js 本体のテストは書かない (vendored ライブラリのため)

**受け入れ基準**:
- [ ] 手動: Phase 3 までで動くジョブ (claude CLI など) に xterm.js で attach し、入力・出力が TUI と同等に見える
- [ ] 複数ブラウザタブから同時 attach で表示がミラーされる
- [ ] Cloudflare Tunnel 経由でも WebSocket が張れる (tunnel 設定に依存、ドキュメントに明記)

## 並列 dev タスクの全体コンフリクト指針

以下のファイルは「同時刻に複数 dev タスクから書かれうる」ため、auto_plan 時に `depends_on` で直列化する:

- `internal/api/web.go` — Phase 2/3 で route 追加が集中
- `internal/api/service.go` — Phase 2 の broadcast 組込
- `internal/server/wire.go` — Phase 1 認証 middleware、Phase 4 WS route
- `web/templates/layout.templ` — Phase 1 で刷新、Phase 2 以降は触らない
- `web/templates/tasks.templ` — Phase 1/2/3 で継続編集
- `web/static/style.css` — Phase 1 で全面書き直し以降は追記のみ
- `CLAUDE.md` — Phase 1 で追記以降は変更最小
- `go.mod` — Phase 1 (`rsc.io/qr`), Phase 4 (`coder/websocket`) で go-get 並走禁止

## テスト戦略 (G-2)

### 単体テスト

- 既存 `internal/api/web_test.go` の `stubWebService` パターンを流用
- 新規 handler は全て stub ベースでカバー
- `TaskEventHub` は race テスト必須
- レート制限は時刻 mock

### 結合テスト

- SSE / WS は `httptest.NewServer` + 実クライアント (標準ライブラリ or coder/websocket)
- 実 DB は `:memory:` SQLite で OK (既存 migration 流用)

### E2E

- 既存 `e2e/scenarios/` 配下に `web-auth/` を追加 (Phase 1)
  - `boid daemon` 起動 → `boid web pair` → curl で `/auth` redeem → cookie 取得 → `/` 200 確認
- ブラウザ E2E (Playwright 等) は導入しない
- `e2e/run.sh` はサンドボックス内で実行できない (既存 CLAUDE.md 参照) → CI 任せ

## CLAUDE.md 追記内容 (Phase 1 の成果)

Phase 1 完了時に CLAUDE.md へ以下のセクションを追加:

```markdown
## Web UI

- `--web` フラグ付きで daemon 起動すると Web UI が有効化される
- 初回は `boid web pair` でペアリングコード (5 分有効、単回) を発行、コード / URL / QR で登録
- デバイス管理: `boid web devices` / `boid web revoke <id>` / `boid web revoke-all`
- loopback (127.0.0.1/::1) からはペアリング不要、外部公開 (Cloudflare Tunnel 等) からは必須
- public URL は `~/.config/boid/config.yaml` の `web.public_url` に設定 (マジックリンク用)
- 署名鍵は `~/.local/share/boid/web_secret` に自動生成 (0600)

Cloudflare Tunnel 公開手順は docs/plans/web-ui-rebuild.md を参照。
```

## 運用メモ

### Cloudflare Tunnel 設定例

boid daemon を `--web --web-listen 127.0.0.1:5171` で起動し、Cloudflare Tunnel で `https://boid.example.com → http://127.0.0.1:5171` に向ける。トンネル側での認証 (Cloudflare Access) は必須ではないが、二段目として入れると IdP + MFA が使えるようになる。

### 紛失時のリカバリ

1. 別経路でホストに SSH
2. `boid web revoke-all` で全デバイス失効
3. `boid web pair` で新デバイスを再登録

### Web UI を無効化したい時

- `config.yaml` の `web.enabled: false` (既存)
- または `--web` フラグ外して再起動

## 変更履歴

- 2026-04-22: 初版 (A-G 決定事項 + R-1/R-2/R-3 調査結果 + Phase 1-4 定義)
