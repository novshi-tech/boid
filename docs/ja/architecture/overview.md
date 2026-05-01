# アーキテクチャ概要

`boid` のソースコードを読み始める contributor 向けに、コンポーネントの並びとデータの流れをまとめます。

このページは internal の網羅的なリファレンスではなく、 「最初の 1 時間」 で全体像を把握するためのものです。実装詳細は各パッケージの `*.go` を直接読んでください。

## プロセスの並び

```
┌──────────┐  UNIX socket   ┌────────────────┐
│ boid CLI │ ─────────────► │  boid daemon   │
└──────────┘                │ (常駐サーバ)   │
                            │                │
                            │ ┌────────────┐ │
                            │ │ broker     │ │
                            │ └────────────┘ │
                            │      ▲         │
                            └──────┼─────────┘
                                   │ stdin/stdout
                                   │
                          ┌────────┴────────┐
                          │ sandboxed hook  │
                          │ / gate script   │
                          └─────────────────┘
```

3 種類のプロセスが関与します。

- **CLI (`boid`)** — ユーザが直接叩くコマンド。標準では UNIX socket を経由して daemon に要求を送り、結果を表示するだけ。実体の処理はしません
- **daemon (`boid start` で起動)** — 常駐サーバプロセス。 SQLite を持ち、 CLI / Web UI のリクエストを受け、状態機械を回し、 hook / gate を起動します。 1 ホストにつき 1 つだけ走ります
- **broker と sandboxed hook / gate** — daemon の子プロセスとして起動されるサンドボックス内のスクリプトと、サンドボックスの境界を越える要求 (host command や `boid task update`) を host 側に流すための broker

CLI は autostart 機能を持っており、 daemon が止まっていれば必要なコマンド (例: `boid task list`) で自動起動されます ([`internal/client/autostart.go`](https://github.com/novshi-tech/boid/blob/main/internal/client/autostart.go))。

## ソースツリー

```
cmd/                  - cobra ベースの CLI (boid task, boid project, ...)
main.go               - エントリポイント。 cmd/Execute() を呼ぶだけ
internal/
  client/             - daemon との UNIX socket HTTP クライアント、 autostart
  daemon/             - daemon 化 (Spawn / WaitForSocket / RedirectToLog)
  config/             - ~/.config/boid/config.yaml の読み込み
  db/                 - SQLite ハンドル + マイグレーション
  server/             - HTTP / UNIX socket リスナと chi ルータの組み立て
  api/                - HTTP ハンドラ、 task workflow service
  orchestrator/       - 状態機械、 ProjectStore、 task / job / project の永続化と評価
  dispatcher/         - hook / gate のジョブ起動、サンドボックスの組み立て、 worktree 管理
  sandbox/            - mount namespace + chroot 実装、 host command broker、 HTTP プロキシ
  kit/                - kit リポジトリの clone・読み込み・detect
  tui/                - bubbletea ベースの TUI
  initwizard/         - boid init の対話セットアップ
  logrotate/          - daemon ログのサイズローテーション
  qrterm/             - ターミナルへの QR コード描画
  skills/             - Claude Code スキルの埋め込み配布
  timeline/           - タスクのタイムラインビュー
web/                  - Templ テンプレート + 静的アセット
testutil/             - テストヘルパ
e2e/                  - black-box E2E (scenarios + fixture kit + fake host commands)
```

## レイヤと依存方向

`internal/` の 4 つの主要レイヤ間の依存方向は以下です。

```
        ┌─────────┐
        │ sandbox │ ← 他のレイヤには依存しない
        └─────────┘
             ▲
             │
       ┌─────────────┐
       │ dispatcher  │ → orchestrator にも依存
       └─────────────┘
             ▲
             │
       ┌─────────────┐
       │ orchestrator│ → db のみに依存 (dispatcher / sandbox には触らない)
       └─────────────┘
             ▲
             │
        ┌────────┐
        │ api /  │ → 上記すべてを束ねる
        │ server │
        └────────┘
```

設計上守らなければならない重要な制約は次の 3 点です:

- **orchestrator → dispatcher / sandbox の依存は禁止**。 orchestrator はドメインロジック (状態機械、 task / job / project の評価) だけを持ち、サンドボックスや実行環境の詳細を知らない
- **dispatcher が橋渡し**。 orchestrator が出した「次に動かすべき hook / gate」 と、 sandbox が要求する 「マウント / コマンドポリシー / プロセス起動」 の primitive 表現を、 dispatcher が翻訳する
- **sandbox は orchestrator を見ない**。サンドボックス内で実行するための情報 (どのファイルをマウントするか、どんなプロセスを起動するか) は dispatcher が primitive として渡す

この境界は過去に大規模リファクタで一度破られた経緯があり、再発防止のため `go list` 等で機械的に確認することがあります (詳細は [Contributing](../contributing/README.md) を参照)。

## 主要コンポーネント

### internal/server

daemon の組み立てを行う薄い層。 `New()` で:

1. SQLite を開いてマイグレーションを実行
2. `orchestrator.ProjectStore` を初期化
3. `sandbox.Broker` を立てる
4. `dispatcher` 系の runtime を組み上げる
5. chi ルータに API ハンドラをマウントする

`Start()` で UNIX socket、 TCP listener、 HTTP server、 GC ループを順に起動します。

エントリ: [`internal/server/server.go`](https://github.com/novshi-tech/boid/blob/main/internal/server/server.go)

### internal/api

HTTP ハンドラと、その内部で動く `TaskWorkflowService`。タスク作成・アクション送信・タスク更新といった外部リクエストを受けると、必要に応じて `runDispatchLoop` を呼んで状態機械を進めます。

`runDispatchLoop` は次のループを回します:

1. `orchestrator.Coordinator.DispatchAndAdvance` を呼ぶ
2. coordinator が:
   - 現在の status に対応する hook / gate を選ぶ
   - dispatcher 経由で実行する
   - 戻ってきた payload patch をマージする
   - 状態機械の自動遷移ルールを再評価する
3. 状態が変わったら次のラウンドへ。状態が同じなら抜ける

エントリ: [`internal/api/service.go`](https://github.com/novshi-tech/boid/blob/main/internal/api/service.go)

### internal/orchestrator

`boid` のドメイン層です。

- **状態機械** (`machine.go`) — `pending → executing → done` のルール、自動遷移条件、 abort 条件
- **Coordinator** (`coordinator.go`) — 1 ステップの dispatch + advance を実行
- **Evaluator** (`evaluator.go`) — どの hook / gate を発火させるかを評価
- **ProjectStore** (`project_store.go`) — プロジェクトとそれに紐付く kit 解決済みメタ情報のメモリ上キャッシュ
- **lifecycle / payload merge / blocked / readonly** — 状態遷移条件で使う計算 trait や評価ヘルパ

dispatcher / sandbox に依存しないため、状態機械の挙動は単体テストで追えます。

### internal/dispatcher

ジョブ実行の橋渡し層です。

- **broker** — `sandbox.Broker` のラッパ。 hook / gate を 1 個実行する `RunJob` を提供
- **sandbox_builder** — `orchestrator.ProjectMeta` から sandbox 用の実行プランを組み立てる
- **policy_translate** — kit の `host_commands` 宣言を sandbox の `CommandDef` に翻訳
- **runner / runtime** — ジョブの実行とその結果の収集
- **worktree_manager** — `worktree: true` な behavior 用の git worktree の作成・再構築・破棄
- **secret_store** — 暗号化された secret の保存・取り出し

エントリ: [`internal/dispatcher/runner.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/runner.go)、 [`internal/dispatcher/worktree_manager.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/worktree_manager.go)

### internal/sandbox

Linux のサンドボックスを直に組む層です。

- **broker** — daemon が UNIX socket で待ち受け、サンドボックス内からの要求 (host command の実行、 `boid task update` 等) を受けて host 側で代理実行する
- **proxy** — 宣言済みドメインだけ通す HTTP プロキシ
- **mount namespace + chroot** — `unshare(2)` と bind mount で読み書きの境界を作る
- **boid shim** — サンドボックス内の `boid` バイナリ。実体は broker への薄いクライアント

orchestrator のドメイン型を見ない (依存方向の制約)。 入力は dispatcher が渡す primitive (BindMount のリスト、 CommandDef のリスト等)。

### internal/kit

kit リポジトリの clone・配置、 `kit.yaml` の読み込み、 `detect.sh` の実行を行います。

エントリ: [`internal/kit/registry.go`](https://github.com/novshi-tech/boid/blob/main/internal/kit/registry.go)

### internal/db

SQLite のハンドルとマイグレーション。 `modernc.org/sqlite` (純 Go の SQLite 実装) を使うため、 cgo は不要です。

## 1 アクションのデータの流れ

ユーザが `boid action send --task <id> --type start` を実行したときの流れ:

1. **CLI**: `cmd/action.go` が `client.NewUnixClient(...)` で daemon に POST `/api/tasks/<id>/actions`
2. **daemon HTTP**: `internal/api` のハンドラがリクエストを受け、 `TaskWorkflowService.ApplyAction` を呼ぶ
3. **状態機械評価**: `orchestrator.StateMachine.ApplyAction` が `start` ルールを評価し、 `pending → executing` を返す
4. **永続化**: 新しい status と lifecycle を SQLite に書く
5. **dispatch ループ起動**: `runDispatchLoop` が走り、 `Coordinator.DispatchAndAdvance` を呼ぶ
6. **hook 評価**: `Evaluator` が `executing` で発火すべき hook を kit メタから選ぶ
7. **dispatcher が実行**: `dispatcher` が sandbox を組み立て、 `sandbox.Broker.RunJob` で hook スクリプトを起動
8. **payload patch 受領**: hook の stdout を JSON としてパースし、 payload にマージ
9. **自動遷移**: 状態機械のルールを再評価し、自動遷移があれば次のラウンドへ
10. **CLI に返却**: 最終的な status を JSON でレスポンス、 CLI が表示

ジョブのログ (stderr) は SQLite に保存され、 `boid job show <job-id>` で読めます。

## どこを読むか

| やりたいこと | エントリ |
|---|---|
| 状態遷移ルールを変える | [`internal/orchestrator/machine.go`](https://github.com/novshi-tech/boid/blob/main/internal/orchestrator/machine.go) |
| hook / gate の発火条件を変える | [`internal/orchestrator/evaluator.go`](https://github.com/novshi-tech/boid/blob/main/internal/orchestrator/evaluator.go) |
| dispatch サイクル全体を追う | [`internal/api/service.go`](https://github.com/novshi-tech/boid/blob/main/internal/api/service.go) の `runDispatchLoop` |
| 1 ジョブの実行を追う | [`internal/dispatcher/runner.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/runner.go) |
| worktree 管理 | [`internal/dispatcher/worktree_manager.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/worktree_manager.go) |
| host command の許可ロジック | [`internal/dispatcher/policy_translate.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/policy_translate.go) |
| サンドボックスの境界 | [`internal/sandbox/broker.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/broker.go) |
| Web UI 認証 | [`internal/api/web_auth.go`](https://github.com/novshi-tech/boid/blob/main/internal/api/) (関連ファイルを参照) |
| TUI 全体 | [`internal/tui/app.go`](https://github.com/novshi-tech/boid/blob/main/internal/tui/app.go) |

## 関連ドキュメント

- [概念](../guide/concepts.md) — 用語の定義
- [状態機械](../guide/state-machine.md) — 状態と遷移ルール
- [`project.yaml` リファレンス](../reference/project-yaml.md) — プロジェクト定義のスキーマ
- [Kit 作者向け 概要](../kit-authoring/overview.md) — kit の I/O プロトコル
- [Contributing](../contributing/README.md) — 開発フロー
