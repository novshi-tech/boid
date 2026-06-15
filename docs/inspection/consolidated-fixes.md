# boid ドキュメント修正 — 統合レポート（修正 input）

- 作成日: 2026-06-15
- 目的: 複数モデル（Opus 4.8 / GPT-5.5 / GLM-5.1 / Composer 2.5 / Qwen / Kimi）によるドキュメント差異調査をマージ・重複排除し、実コードで裁定した結果を確定事実として埋め込んだ、修正作業用の単一バックログ。
- 各項目の「出典」はその差異を指摘したモデル名（調査元の個別レポートは一時生成物のため本リポジトリには未収録）。
- 凡例:
  - **[要削除]** ドキュメントに記載があるが実装に存在しない → 削除（または実装追加）
  - **[要追記]** 実装にあるがドキュメントにない → 追記
  - **[要修正]** 記述が実装と食い違う → 書き換え
  - ✅ = 本統合時にコードで裏取り済み / ⚠️ = モデル間で見解が割れた争点（裁定結果を併記）
- 注意: 英語版（`docs/en/**`）と日本語版（`docs/ja/**`）は mirror 構成。**ほぼ全ての差異は両言語に共通**するため、修正は必ず ja/en 同時に行うこと。

---

## 0. モデル間で割れた争点の裁定（最重要）

修正前にここを必ず読むこと。複数モデルが**真逆の主張**をした項目を実コードで裁定した。誤ったレポートを信じると逆方向の劣化を招く。

| # | 争点 | 裁定（✅ コード確認済み） | 根拠 | 誤ったレポート |
|---|---|---|---|---|
| D1 | `gc.enabled/interval/older_than` は config.yaml から読まれるか | **読まれる。ドキュメントは正しい。削除してはいけない** | `internal/config/config.go:86-138`（`Config.UnmarshalYAML` が `gc.*` を解釈）。struct タグ `yaml:"-"` だけ見ると誤解する罠 | Kimi・Qwen が「読まれない／ハードコード」と高重大度で誤断定 |
| D2 | ただし手動 `boid gc` / `POST /api/gc` は config の `older_than` を見るか | **見ない。既定 720h ハードコード**。config 値が効くのは daemon 自動 GC ループのみ | `cmd/gc.go:19`（既定 30×24h）, `internal/api/gc.go:77`（req 空なら 30×24h） | （GLM のみ正しく指摘） |
| D3 | reopen 時に `interactive` は継承されるか | **継承されない**。継承は `Agent` と `Model` のみ。`interactive` はそもそもフィールドが存在しない | `internal/api/workflow_action.go:67-73`, `internal/orchestrator/spec_types.go:113-118` | Composer 初回（汚染版）が「interactive も継承・整合」と誤記 |
| D4 | `notify.command` 空で HTTP 501 を返すか | **実質返さない**。`notify.Service` は常に non-nil で wire されるため 501 経路は到達困難。501 条件は「Notify==nil かつ ask 空かつ root task」 | `internal/api/task_notify.go:84-91`, `internal/server/wire.go:240-253` | （GLM が「到達不能」と最も正確） |
| D5 | `awaiting` 中に project lock は保持されるか | **解放される**。awaiting 突入時にロックを手放し、同一ブランチで別タスクが動ける | `internal/api/workflow_action.go:249` + テスト `project_lock_workflow_test.go:766,837` | ドキュメントは「保持」。GPT のみ「逆」と正しく指摘 |

---

## P0 — 誤誘導・操作不能・セキュリティ理解に直結

### P0-1. 存在しない `boid task abort` の案内 [要修正] ✅
- 対象: `docs/{en,ja}/guide/troubleshooting.md`
- 現状: stuck job 解放手順として `boid task abort <id>` を案内
- 実装: 該当サブコマンドなし（`cmd/task.go`）。正しくは `boid action send --task <id> --type abort`（`cmd/action.go`）
- 出典: GPT, Opus, Composer 2.5

### P0-2. 存在しない `boid start --http-addr` フラグ [要削除] ✅
- 対象: `docs/{en,ja}/reference/cli.md:31`, `docs/{en,ja}/guide/web-ui.md:12-15`, `docs/{en,ja}/reference/http-api.md:14`
- 実装: `boid start` のフラグは `--db-path`/`--socket-path`/`--kits-dir`/`--key-file-path` のみ（`cmd/start.go:40-43`）。HTTP アドレスは `config.yaml` の `web.http_addr` または `boid web set-addr` で設定。listen 既定 `:8080` は `cmd/start.go:136` で補完
- 影響: 記載通り打つと `unknown flag` で落ちる
- 出典: 全モデル一致

### P0-3. Web UI 無効化「`--http-addr ""`」が成立しない [要修正] ✅
- 対象: `docs/{en,ja}/guide/web-ui.md:15`, `docs/{en,ja}/getting-started/03-web-ui.md`
- 実装: 空アドレス設定時も `:8080` にフォールバックし、TCP listener は常に起動（`cmd/start.go:134-137`, `internal/server/server.go:172-178`）。現状 Web UI を完全停止する手段はない
- 対応: 「無効化できない」旨を明記、または実装に disable semantics を追加
- 出典: 全モデル一致

### P0-4. 設定キー名 `web.listen` は誤り [要修正] ✅
- 対象: `docs/{en,ja}/getting-started/03-web-ui.md:33`
- 実装: 正しくは `web.http_addr`（`internal/config/config.go:40`, `cmd/web.go`）
- 出典: Opus, GPT, Composer 初回

### P0-5. 削除済み `boid task list --has-depends-on / --no-depends-on` [要削除] ✅
- 対象: `docs/{en,ja}/reference/cli.md:71`
- 実装: フラグは `--status`/`--workspace`/`--behavior` のみ（`cmd/task.go:124-126`）。`depends_on` 機能は migration `0026_drop_tasks_depends_on.sql` で削除済み
- 出典: 全モデル一致

### P0-6. hook 入力契約が旧モデルのまま（stdin = TaskJSON 全体）[要修正] ✅
- 対象: `docs/{en,ja}/reference/hook-contract.md`, `docs/{en,ja}/kit-authoring/overview.md`, `docs/{en,ja}/architecture/sandbox-internals.md`
- 現状: 「stdin にタスク全体の JSON（TaskJSON）が流れる」「`TASK_JSON=$(cat)` で読む」
- 実装: 全 hook job は `Interactive: true`（`internal/orchestrator/planner.go:90`）のため stdin は PTY で TaskJSON は流れない。タスクメタは `$HOME/.boid/context/{task,instructions,environment,payload}.{yaml,json}` の context ファイル経由（`internal/dispatcher/sandbox_builder.go:208-218,610-659`）。non-interactive job のみ trait フィルタ済み payload を stdin に流す
- 付随: `task.yaml` のフィールドは `id/title/status/behavior/description` の 5 つのみ（doc の TaskJSON 表より大幅に少ない）
- 出典: 全モデル一致（深さは GPT / Composer 2.5 / Opus が上）

### P0-7. `Instruction.type` / `interactive` は実装に存在しない [要削除] ✅
- 対象: `docs/{en,ja}/reference/project-yaml.md`, `docs/workflows.md`（`type: execution` の例が複数箇所）
- 実装: `Instruction` struct は `Agent/Name/Message/Model` のみ（`internal/orchestrator/spec_types.go:113-118`）。`type:` / `interactive:` は非厳格 YAML デコードで**黙殺**され、エラーにも警告にもならない（ユーザは誤りに気づけない）
- 対応: ドキュメントから削除。`interactive` を残すなら実装にフィールド追加
- 出典: 全モデル一致

### P0-8. 廃止済み `gate` 概念の残存 [要修正] ✅
- 対象: `CLAUDE.md`（セキュリティモデル節）, `docs/{en,ja}/architecture/overview.md`, `persistence.md`, `sandbox-internals.md`, `web-internals.md`
- 実装: Gate 機構は廃止。現行 dispatch は hook のみ、`JobKind` は `hook`/`exec`（`internal/orchestrator/coordinator.go`, `jobspec.go`）。`project.yaml` トップレベル `gates:` はロード時に拒否（`spec_loader.go:92`）
- 補足: CLAUDE.md の「role による差分なし」自体は正しい。書き換え時に残すこと
- 出典: Composer（両版）, Opus, GLM, GPT

### P0-9. HTTP API 認証モデルが実装より弱い（無認証の制御プレーン）[実装修正で決定] ⚠️🔴（セキュリティ）
- 対象（コード）: `internal/server/wire.go`, `internal/server/server.go`, `internal/api/auth/`
- 対象（doc）: `docs/{en,ja}/reference/http-api.md`（実装修正後に実態へ追従）
- 現状の doc: Web UI 経由の `/api/*` は auth middleware + CSRF 配下、直接呼び出しには `boid_session` cookie + `csrf_token` 必須、と説明
- 実装の実態（コード確認済み ✅）:
  - `/api/*` は CSRF 免除（`internal/api/auth/csrf.go:70-73`）
  - データ/制御系 API（`/api/shutdown`, `/api/secrets`, `/api/tasks`, `/api/jobs`, `/api/projects`, `/api/gc`, `/api/broker`, `/api/proxy`, `/api/web` 等）は WebAuth グループ**外**に mount（`internal/server/wire.go`）。WebAuth 配下は `GET /api/tasks/{id}/events` と `GET /api/jobs/{id}/attach/ws` と HTML のみ
  - UNIX socket と TCP listener は**同一 router・同一 httpServer**を共有（`server.go:117,170,178`）。トランスポートでの区別なし
  - TCP 既定 bind は `:8080`（全インターフェース、`cmd/start.go`）
- 影響: `:8080` に到達できる相手が**無認証で**シークレット読取・タスク生成（≒任意コード実行）・デーモン停止が可能。Cloudflare Tunnel 公開や複数ユーザ環境（loopback も非ユーザ分離）で顕在化
- **実装済み・main マージ済み（2026-06-15、PR #563）✅**
  - リスナー分離: UNIX socket は素の router（信頼・無認証維持）、TCP は `auth.NewTCPAPIAuthMiddleware` でラップ（`internal/api/auth/api_middleware.go`, `internal/server/{server,wire}.go`）
  - TCP では `/api/*`（`/api/health` 除く）にセッション必須・失敗時 401。loopback bootstrap（device 0 件）維持、proxy/tunnel ヘッダ付きは bootstrap 対象外
  - TCP 既定 bind を `127.0.0.1:8080` に変更（`cmd/start.go`、ユーザ判断で loopback 絞り採用。Tunnel は 127.0.0.1 接続なので影響なし）
  - テスト: `internal/api/auth/api_middleware_test.go`（ユニット）, `internal/server/wire_tcp_auth_test.go`（統合）
  - doc 追従済み: `docs/{en,ja}/reference/http-api.md` の認証モデル節
  - 残: CSRF は `SameSite=Lax` で緩和済みだが、必要なら TCP 側 `/api/*` に CSRF も追加検討。web-internals.md / web-ui.md / 03-web-ui.md の関連記述（P0-3/P0-4/P2-6）追従は別 PR
- 出典: GPT（最詳）, Composer 初回。再実行版・他モデルは薄い

### P0-10. `POST /api/web/url` は実装に存在しない [要削除] ✅
- 対象: `docs/{en,ja}/reference/http-api.md:226`
- 実装: `WebManagementHandler` は `/pair`(POST)・`/devices`(GET)・`/devices/{id}`(DELETE)・`/devices`(DELETE) のみ。`boid web set-url` は CLI が `config.yaml` を直接書き込む（`internal/api/web.go:754-761`, `cmd/web.go`）
- 出典: 全モデル一致

### P0-11. CLAUDE.md の internal パッケージ構成が実態と乖離 [要修正] ✅
- 対象: `CLAUDE.md:17-37`
- 現状記載で**実在しない**: `internal/reducer/`, `hook/`, `project/`, `hostcmd/`, `job/`, `tmux/`, `model/`
- **記載漏れ**: `config/`, `daemon/`, `dispatcher/`, `initwizard/`, `logrotate/`, `notify/`, `orchestrator/`, `qrterm/`, `skills/`, `timeline/`, `tui/`
- 補足: 状態機械/project パース/hook は `internal/orchestrator/`・`internal/api/` に集約済み
- 出典: 全モデル一致

### P0-12. README の `c2-flow.md` リンク切れ [要修正] ✅
- 対象: `README.md:55`, `README.ja.md:55`
- 実装: `docs/{en,ja}/architecture/` には `overview.md`/`persistence.md`/`sandbox-internals.md`/`web-internals.md` のみ。`c2-flow.md` 不在
- 対応: リンク修正、または C2/Q&A アーキ doc を新規作成
- 出典: 全モデル一致

### P0-13. CLAUDE.md の `docs/plans/web-ui-rebuild.md` 参照切れ [要修正] ✅
- 対象: `CLAUDE.md:65`
- 実装: 該当ファイル不在。Cloudflare Tunnel 手順は実際は `guide/web-ui.md` がカバー
- 出典: Composer, Kimi, Opus, GLM

---

## P1 — 機能が実装済みなのに文書化されていない（発見性）

### P1-1. `boid task notify` の未記載モード [要追記] ✅
- 対象: `docs/{en,ja}/reference/cli.md`, `http-api.md`, `guide/notifications.md`
- 実装: `--done`/`--fail`/`--progress`/`--session-id` の 4 モード（`cmd/task.go:143-146`）。`--ask`/`--done`/`--fail`/`--progress` は相互排他、`--message` は `--progress` 以外で必須（`internal/api/task_notify.go:29-45`）。`--done`→done 遷移、`--fail`→aborted 遷移、`--progress`→timeline のみ
- HTTP 側: `POST /api/tasks/{id}/notify` に `session_id`/`progress`/`done`/`fail` フィールド追記
- 付随: FYI notify hook は root task のみ発火、子タスク FYI は無音 drop（`task_notify.go:84`）
- 出典: 全モデル一致

### P1-2. 未記載 CLI コマンド [要追記] ✅
- 対象: `docs/{en,ja}/reference/cli.md`
- `boid fetch <url>`（`cmd/fetch.go`）/ `boid agent stop <job-id>`（`cmd/agent.go`、SIGUSR1 送信）/ `boid web set-addr <addr>`（`cmd/web.go`）/ `boid completion`（Cobra 標準）
- 出典: 全モデル一致

### P1-3. 未記載 CLI フラグ [要追記] ✅
- 対象: `docs/{en,ja}/reference/cli.md`
- `boid gc --dry-run`（`cmd/gc.go:20`）/ `boid task hook list|replay --status`（`cmd/task_hook.go:35,37`）/ `boid task artifacts --field|--output-file`（`cmd/task_inspect.go:33-34`）/ `boid job watch --interval`（`cmd/job.go:57`）/ `boid exec --name`（`cmd/exec.go:31`）/ `boid web pair --label`（`cmd/web.go:68`）/ `boid kit install --ssh`（`cmd/kit.go:146`）/ `boid project local init --force` ・ `add-binding --mode`（`cmd/project_local.go:68-69`）/ `boid secret -n/--namespace`（`cmd/secret.go:47-48`）/ `boid task update|rerun --instructions-file`（`cmd/task.go:135,139`）
- ショートハンド: `task update -f`(=`--patch-file`), `task reopen -m`(=`--message`)
- 出典: 全モデル（範囲は GLM / Composer / Opus が広い）

### P1-4. 状態機械の遷移表が不完全 [要追記] ✅
- 対象: `docs/{en,ja}/guide/state-machine.md`
- 未記載の手動遷移（`internal/orchestrator/machine.go:161-183`）: `fail`(executing→aborted) / `done`(awaiting→done) / `reopen`(aborted→executing)
- 非遷移アクション（timeline 記録のみ）: `progress` / `done_request` / `fail_request`
- 自動遷移の 3 ルール（`machine.go:188-215`、ドキュメントは 1 ルールに簡略化）:
  1. `executing→aborted`（`lifecycle.executed && lifecycle.fail`）
  2. `executing→done`（`lifecycle.executed && lifecycle.done`）
  3. `executing→done`（`lifecycle.executed` のみ、レガシー hook 経路）
- 補足: `notify --done/--fail` は即時遷移ではなく `done_request`/`fail_request` を記録し、runtime 終了後に auto-advance
- 出典: 全モデル一致

### P1-5. `awaiting` 状態の永続化ドキュメント欠落 [要追記] ✅
- 対象: `docs/{en,ja}/architecture/persistence.md`（`tasks.status` 列挙に `awaiting` 追加）, `overview.md`
- 実装: 状態は `pending/executing/awaiting/done/aborted` の 5 つ（`internal/orchestrator/model.go`）
- 出典: GPT, Opus, Composer 2.5, GLM

### P1-6. Web UI インタラクティブターミナルは出荷済み [要修正] ✅
- 対象: `docs/{en,ja}/guide/web-ui.md:134`（「roadmap / not yet shipped」）
- 実装: xterm.js + WebSocket attach（`GET /api/jobs/{id}/attach/ws`）で出荷済み。ジョブ詳細にインライン端末（`web/templates/jobs.templ`, `internal/api/ws_attach.go`）。`docs/plans/web-terminal-vt-emulator.md` Phase 1 完了
- 出典: 全モデル一致

### P1-7. traits リファレンスの欠落 [要追記] ✅
- 対象: `docs/{en,ja}/reference/traits.md`
- 未記載 trait: `verification`（shared マージモード、ハンドラ ID 配下の sub-key にマージ）, `awaiting`（永続 payload trait、`session_id`/`question`/`question_id`/`pending_answer` を保持）
- 未記載 lifecycle フィールド: `lifecycle.executed`/`done`/`fail`（自動遷移の駆動はこれら。doc は `abort.*` のみ列挙）
- マージモードは exclusive / shared / default の 3 区分（`internal/orchestrator/spec_payload.go:10-17`）
- 出典: 全モデル（深さは GPT / Opus / Composer 2.5）

### P1-8. `verifyDoneClaim` ゲートが未文書化 [要追記] ✅
- 対象: `docs/{en,ja}/guide/notifications.md` または state-machine 関連
- 実装: `notify --done` は (1) 未完了 child task があると拒否(409)、(2) 報告された release commit が実 repo に存在しないと拒否（`internal/api/task_notify.go:285-318`）。anti-confabulation ガード
- 出典: GPT, Composer 2.5（独自の深い発見）

### P1-9. `BOID_PROJECT_ID` は hook 環境に設定されない [要修正] ✅
- 対象: `docs/{en,ja}/reference/hook-contract.md:44`
- 実装: hook の env 組み立てに `BOID_PROJECT_ID` はない。export は notify コマンド用（`internal/notify/notify.go:52`）のみ（`internal/dispatcher/sandbox_builder.go:91-92`）
- 出典: 全モデル一致

### P1-10. hook 環境変数の網羅不足 [要追記] ✅
- 対象: `docs/{en,ja}/reference/hook-contract.md`
- 未記載 env: `BOID_MODEL`, `BOID_INVOKED_ROLE/NAME/BEHAVIOR`, `BOID_INSTRUCTIONS`, `BOID_INTERACTIVE`, `BOID_BUILTIN_SHIM`, `BOID_HOST_IP`, `BOID_BROKER_SOCKET/TOKEN`, `BOID_SOCKET`, `BOID_AGENT_SESSION_ID`, `BOID_USER_ANSWER`, `BOID_QUESTION_ID`, `TERM`（`sandbox_builder.go:91-145`, `planner.go:185-194`）
- 特に Q&A resume 用 env（`BOID_AGENT_SESSION_ID`/`USER_ANSWER`/`QUESTION_ID`）は kit 作者に必要
- 出典: 全モデル一致

---

## P2 — 整合性・レガシー整理・完全性

### P2-1. HTTP API 未記載エンドポイント [要追記] ✅
- 対象: `docs/{en,ja}/reference/http-api.md`
- `GET /api/broker`（`wire.go:499`）/ `GET,DELETE /api/projects/{id}`・`POST .../commands/{name}/execute`（`project.go:53-55`）/ `GET /api/tasks/{id}/field`・`GET .../commands`・`POST .../commands/{name}/execute`（`task.go:94,100-101`）/ `PATCH /api/jobs/{id}`（display-name）・`POST /api/jobs/{id}/agent-stop|attach|resize`（`job.go:26,28`, `job_runtime_routes.go:29,65`）
- 出典: 全モデル一致

### P2-2. HTTP API 未記載クエリ/フィールド [要追記] ✅
- `GET /api/projects?workspace_id=`, `GET /api/tasks?project_id=`, `DELETE /api/tasks/{id}?force=true`, `GET /api/tasks/{id}/hooks?status=`, `GET /api/jobs`（global list, `status/interactive/taskless`）
- `POST /api/tasks` 追加フィールド: `id/description/remote_id/traits/ref/parent_id`
- `POST /api/gc`: `dry_run`、`POST /api/web/pair`: `label`/`expires_in`、`POST /api/jobs/{id}/done`: `output`
- 出典: Composer, Opus, Qwen, GLM

### P2-3. `GET /api/jobs/{id}/log` の SSE は条件付き [要修正] ✅
- 対象: `docs/{en,ja}/reference/http-api.md`
- 実装: `?follow=true` 時のみ SSE。無しは `text/plain` スナップショット（`internal/api/job.go:35-44`）。SSE フォーマットは `data: <line>` のみで `event:` フィールドなし。`:ping` keepalive も events 系のみ
- 出典: Composer, Qwen, GLM, GPT

### P2-4. `?follow=true` on `POST /api/tasks/{id}/actions` は未実装 [要削除] ✅
- 対象: `docs/{en,ja}/reference/http-api.md:185`
- 実装: `action.go` は `follow` パラメータを一切読まない。dispatch は非同期
- 出典: Qwen（高）, Composer 初回

### P2-5. `GET /api/workspaces/{id}` は未実装 [要削除] ✅
- 対象: `docs/{en,ja}/reference/http-api.md:67`
- 実装: `WorkspaceHandler` は List のみ（`internal/api/workspace.go:13-17`）
- 出典: 全モデル一致

### P2-6. Web internals のルート/SSE パスが全面的に誤り [要修正] ✅
- 対象: `docs/{en,ja}/architecture/web-internals.md`
- doc: `/auth/pair`(POST), `/auth/redeem`(POST), `/auth/logout`(POST), `/events/tasks/<id>`, `/events/jobs/<id>/log`, `/projects/<id>`
- 実装: ペアリング `POST /api/web/pair`、redeem は `POST /login`（フォーム）/ `GET /auth?token=`（マジックリンク）、Task SSE `GET /api/tasks/{id}/events`、Job log SSE `GET /api/jobs/{id}/log`。`/auth/redeem`・`/auth/logout`・`GET /projects/<id>` は不在
- 未記載ページ: `/sessions`, `/sessions/new`, `/tasks/{id}/questions/{id}`, `/tasks/{id}/hooks`, `/tasks/{id}/edit`
- 出典: 全モデル一致

### P2-7. `POST /api/secrets` の namespace は body フィールド [要修正] ✅
- 対象: `docs/{en,ja}/reference/http-api.md:202,206`
- 実装: POST は JSON body の `namespace`。`GET`/`DELETE` は query `?namespace=`。一貫性なし（`internal/api/secret.go:50-69`）
- 出典: Opus, GPT, Qwen, GLM

### P2-8. persistence.md の陳腐化 [要修正] ✅
- 対象: `docs/{en,ja}/architecture/persistence.md`
- `jobs.status` は `success` ではなく **`completed`**（`internal/api/job_model.go:9`）✅
- `jobs.role` の `gate` 表記削除（`hook` のみ）
- migration 一覧が 0022 止まり → 実際は 0027 まで（`0023`〜`0027`）
- `jobs.display_name` 列（migration 0027）追記、`datasource_id`（0025 削除）・`remote_id` 部分 index（0024 削除）の整理
- 出典: GPT, GLM（job status は両者のみ）, Opus

### P2-9. sandbox-internals.md の builtin / op 欠落 [要追記] ✅
- 対象: `docs/{en,ja}/architecture/sandbox-internals.md`
- builtin に `fetch` 追記（doc は `boid`/`git` の 2 つのみ、実際は 3 つ。`internal/orchestrator/policy.go:73-74`）
- boid op `agent_stop`、git op `clone_local` 追記（`policy_ops.go:14,28`）
- 出典: Opus, GLM, GPT, Composer 2.5

### P2-10. ネットワーク allowlist の説明ずれ [要修正] ✅
- 対象: `docs/{en,ja}/guide/concepts.md`, `architecture/sandbox-internals.md`
- 現状: 「kit が宣言したドメインのみ到達可能」
- 実装: kit 単位の domain allowlist は存在しない。`cmd/start.go:defaultAllowedDomains()` の組み込みリスト + `config.yaml` の `sandbox.allowed_domains` のマージ（`cmd/start.go:138`）
- 出典: GPT, Composer 2.5, GLM

### P2-11. config-yaml.md の補足 [要追記] ✅（D1/D2 と整合させること）
- `gc.*` は config から読まれる（D1 参照、削除禁止）が、**手動 `boid gc` は config の `older_than` を見ない**点を明記（D2）
- `web.http_addr` 既定 `:8080` の出どころは `cmd/start.go`(フォールバック)で、`config.DefaultConfig()` 自体は空である点
- `set-addr`/`set-url` は YAML round-trip でコメントを消失する点（`cmd/web.go`）
- 出典: GLM（D2）, Composer/Opus/GPT

### P2-12. project-yaml.md の不足・非対称 [要追記/要修正] ✅
- 英語版に `fork_point` 追記（ja:32 にはあり、en の top-level 表に欠落。`spec_types.go:436`）
- behavior-level `kits` / `commands` 追記（`spec_types.go:349-350`, `spec_loader.go:498-550`）
- `TaskBehavior.name` は実装に無く黙殺される旨（または例から削除）
- behavior 名は任意可・非 canonical は `readonly=false`、`plan`/`dev` は deprecated エイリアス（`behavior_resolve.go`, `BehaviorAliases`）
- `${WORKTREE}`/`${PROJECT_WORKDIR}` bind mount トークン、`local/<name>` kit 解決、`git`/`boid`/`fetch` の host_commands 予約
- BindMount mode 検証が project.yaml と project.local.yaml で非対称（local は `ro`/`rw` 必須、空不可）
- project lock は `awaiting` で**解放**される（D5、ドキュメントは逆）
- 出典: 全モデル（範囲は GPT / Composer / GLM / Opus）

### P2-13. GC スコープの過小記載 [要修正] ✅
- 対象: `CLAUDE.md`, `docs/{en,ja}/guide/troubleshooting.md`, `getting-started/01-install.md`
- 現状: 主に `runtimes/<runtime_id>/` のみと読める
- 実装: 加えて terminal tasks/actions/jobs(DB)・worktree dirs・`/tmp/boid-*`・revoked devices を削除。初回 GC は起動直後でなく **10 秒後**（`gc_loop.go`, `wire.go InitialDelay:10s`）
- 出典: Composer, Opus, GLM

### P2-14. auto-start 例外リストの不足 [要追記] ✅
- 対象: `docs/{en,ja}/reference/cli.md:25`
- 実装: `start`/`stop`/`gc` に加え `check`/`init`/`fetch`/`project local *`/`web set-url`/`web set-addr` も skip。`BOID_NO_AUTOSTART=1` も未記載（`internal/client/autostart.go`）
- 出典: Composer, Opus, GLM, GPT

### P2-15. notify guide のシグナル/URL 説明 [要修正] ✅
- 対象: `docs/{en,ja}/guide/notifications.md`
- lifecycle signal（ask/done/fail）は SIGTERM ではなく **SIGUSR1** で claude に停止要求し、EXIT trap を生かして `boid job done` の正常完了を維持
- `BOID_TASK_URL` はモード別: ask→`/tasks/{id}/questions/{qid}`、done/fail→`/tasks/{id}`、FYI→running interactive job があれば `/jobs/{jid}`
- 出典: GPT, Composer 2.5（SIGUSR1 / verifyDoneClaim と併せ深い）

### P2-16. kit-authoring の `consumes: [instructions]` 例が機能しない懸念 [要確認→要修正] ⚠️
- 対象: `docs/{en,ja}/kit-authoring/overview.md:43`
- 指摘: `instructions` は payload trait ではないため、`consumes` に列挙しても hook 評価で payload の active trait と照合されず、hook が発火しない可能性
- 状態: GPT が「危険」と指摘（推論ラベル付き）。実機での発火確認を推奨してから修正
- 出典: GPT（独自）

---

## 修正運用メモ

1. **ja/en 同時修正**: mirror 構成のため、片方だけ直すと非対称が増える。
2. **削除系は二重確認**: 特に D1（gc 設定）・D4（notify 501）のように「ドキュメントが実は正しい/挙動が微妙」な項目は、Kimi/Qwen のレポートを根拠に消さないこと。本レポートの裁定を優先。
3. **実機確認の推奨**: P0-9（API 認証）と P0-6（hook 入力契約）、P2-16（consumes 例）は影響が大きいので、ドキュメント修正前に実機で挙動を再現確認するのが望ましい。
4. **「一致項目」の保全**: 調査で「ドキュメントが実装を正しく反映している」と確認できた箇所は触らないこと（差異の列挙だけを見て誤って削除しない）。
5. CLI / HTTP API は `boid <cmd> --help` とルート実装が権威。将来的には help / route から machine-check で doc 整合を検証できる仕組みが望ましい（GPT 提案）。
