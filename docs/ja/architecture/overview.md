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
                          │   script        │
                          └─────────────────┘
```

3 種類のプロセスが関与します。

- **CLI (`boid`)** — ユーザが直接叩くコマンド。標準では UNIX socket を経由して daemon に要求を送り、結果を表示するだけ。実体の処理はしません
- **daemon (`boid start` で起動)** — 常駐サーバプロセス。 SQLite を持ち、 CLI / Web UI のリクエストを受け、状態機械を回し、 hook を起動します。 1 ホストにつき 1 つだけ走ります
- **broker と sandboxed hook** — daemon の子プロセスとして起動されるサンドボックス内の hook スクリプトと、サンドボックスの境界を越える要求 (host command や `boid task update`) を host 側に流すための broker

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
  dispatcher/         - hook のジョブ起動、サンドボックスの組み立て
  gitgateway/         - git gateway (認証注入リバースプロキシ)。sandbox 内 clone の唯一の transport
  sandbox/            - mount namespace + chroot 実装、 host command broker、 HTTP プロキシ
  adapters/           - harness アダプタ (claude / codex / opencode / shell) + registry
  initwizard/         - boid project init の対話セットアップ (旧 boid init は廃止済み)
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
- **dispatcher が橋渡し**。 orchestrator が出した「次に動かすべき hook」 と、 sandbox が要求する 「マウント / コマンドポリシー / プロセス起動」 の primitive 表現を、 dispatcher が翻訳する
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
   - 現在の status に対応する hook を選ぶ
   - dispatcher 経由で実行する
   - 戻ってきた payload patch をマージする
   - 状態機械の自動遷移ルールを再評価する
3. 状態が変わったら次のラウンドへ。状態が同じなら抜ける

エントリ: [`internal/api/service.go`](https://github.com/novshi-tech/boid/blob/main/internal/api/service.go)

### internal/orchestrator

`boid` のドメイン層です。

- **状態機械** (`machine.go`) — `pending → executing → awaiting / done` のルール、自動遷移条件、 abort 条件
- **Coordinator** (`coordinator.go`) — 1 ステップの dispatch + advance を実行
- **Evaluator** (`evaluator.go`) — どの hook を発火させるかを評価
- **ProjectStore** (`project_store.go`) — プロジェクトのメタ情報のメモリ上キャッシュ。 project に紐付く workspace の `host_commands` / `env` / `capabilities` / `additional_bindings` を投影して返す (`GetWithWorkspace`)。 kit を per-request に解決・注入する経路は Phase 2.5 PR6 で撤去済み (下記「kit」節参照)
- **lifecycle / payload merge / blocked / readonly** — 状態遷移条件で使う計算 trait や評価ヘルパ

dispatcher / sandbox に依存しないため、状態機械の挙動は単体テストで追えます。

### internal/dispatcher

ジョブ実行の橋渡し層です。

- **broker** — `sandbox.Broker` のラッパ。 hook を 1 個実行する `RunJob` を提供
- **sandbox_builder** — `orchestrator.ProjectMeta` から sandbox 用の実行プランを組み立てる
- **policy_translate** — project (project.yaml と workspace 由来の `host_commands` をマージ済み) の宣言を sandbox の `CommandDef` に翻訳
- **runner / runtime** — ジョブの実行とその結果の収集。project が可視なジョブは git gateway 経由で sandbox 内に project を clone する (host 側の git worktree は作らない)
- **secret_store** — 暗号化された secret の保存・取り出し

エントリ: [`internal/dispatcher/runner.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/runner.go)、 [`internal/sandbox/runner/clone.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/runner/clone.go) (sandbox 内 clone 後の branch 解決)

### internal/sandbox

Linux のサンドボックスを直に組む層です。

- **broker** — daemon が UNIX socket で待ち受け、サンドボックス内からの要求 (host command の実行、 `boid task update` 等) を受けて host 側で代理実行する
- **proxy** — 宣言済みドメインだけ通す HTTP プロキシ
- **mount namespace + chroot** — `unshare(2)` と bind mount で読み書きの境界を作る
- **boid shim** — サンドボックス内の `boid` バイナリ。実体は broker への薄いクライアント

orchestrator のドメイン型を見ない (依存方向の制約)。 入力は dispatcher が渡す primitive (BindMount のリスト、 CommandDef のリスト等)。

### workspace (DB 一元化。kit 機構は退役済み)

workspace は project の実行環境 (`host_commands` / `env` / `capabilities` / `allowed_domains` / `additional_bindings`) を machine 単位でまとめる設定単位で、`workspaces` テーブルで DB 管理されています (Phase 2.5、`docs/plans/workspace-db-consolidation.md`)。`default` workspace は daemon 起動時に常に自動生成され、project は登録時に自動的にそこへ割り当てられます。

かつて存在した kit 機構 (`internal/orchestrator/kit_registry.go` によるツール供給単位の動的解決、`boid kit init` によるマシンスキャン + カタログ生成、`boid workspace configure` による LLM 対話ベースの workspace 設定生成) は Phase 2.5 PR6 (2026-07) で撤去されました。project ごとに kit を per-request 解決して merge する経路 (`ProjectStore.GetWithWorkspace` 内の `MergeKitRuntime` 呼び出し) も同時に削除されています。

`kit.yaml` というファイル形式自体は無くなっていません。 `WorkspaceMeta.Kits` フィールド (workspace.yaml の `kits:`) は Phase 2.5 PR7 でコードから完全撤去され、 `POST`/`PUT`/`import /api/workspaces` は `kits:` を含む body を reject するようになりましたが、 `kits: [...]` 参照を一度だけ展開して `host_commands` / `env` / `additional_bindings` に畳み込む経路 (`workspace_migration.go` の `MaterializeWorkspaceKitsForPersist`/`materializeKitRuntimeIntoWorkspace`) 自体は残っており、 呼び出し元は (1) `boid workspace assign` の auto-create 補助 (`cmd/workspace.go`、 legacy shadow yaml の `kits:` をクライアント側で解決)、 (2) daemon 起動時の一度きりの DB 移行 (`MigrateWorkspaceYAMLToDB`、 cutover 前の旧 workspace yaml が対象) の 2 経路のみです。 `boid project migrate` が生成する legacy kit (`host_commands`/`additional_bindings` を project.yaml から同梱) は kit ディレクトリ経由の再解決を経ず、 workspace の対応フィールドへ直接畳み込まれます。

エントリ: [`internal/orchestrator/workspace_repository.go`](https://github.com/novshi-tech/boid/blob/main/internal/orchestrator/workspace_repository.go) (DB CRUD)、[`internal/orchestrator/workspace_meta.go`](https://github.com/novshi-tech/boid/blob/main/internal/orchestrator/workspace_meta.go) (`WorkspaceMeta` スキーマ)、[`internal/orchestrator/workspace_migration.go`](https://github.com/novshi-tech/boid/blob/main/internal/orchestrator/workspace_migration.go) (yaml → DB 移行 + legacy kit 展開)

### internal/db

SQLite のハンドルとマイグレーション。 `modernc.org/sqlite` (純 Go の SQLite 実装) を使うため、 cgo は不要です。

## 1 アクションのデータの流れ

ユーザが `boid action send --task <id> --type start` を実行したときの流れ:

1. **CLI**: `cmd/action.go` が `client.NewUnixClient(...)` で daemon に POST `/api/tasks/<id>/actions`
2. **daemon HTTP**: `internal/api` のハンドラがリクエストを受け、 `TaskWorkflowService.ApplyAction` を呼ぶ
3. **状態機械評価**: `orchestrator.StateMachine.ApplyAction` が `start` ルールを評価し、 `pending → executing` を返す
4. **永続化**: 新しい status と lifecycle を SQLite に書く
5. **dispatch ループ起動**: `runDispatchLoop` が走り、 `Coordinator.DispatchAndAdvance` を呼ぶ
6. **hook 評価**: `Evaluator` が `executing` (または `awaiting`) で発火すべき hook を project の `task_behaviors` メタから選ぶ (hook は project.yaml 由来のみで、workspace / kit は hook を提供しない)
7. **dispatcher が実行**: `dispatcher` が sandbox を組み立て、 `sandbox.Broker.RunJob` で hook スクリプトを起動
8. **payload patch 受領**: hook の stdout を JSON としてパースし、 payload にマージ
9. **自動遷移**: 状態機械のルールを再評価し、自動遷移があれば次のラウンドへ
10. **CLI に返却**: 最終的な status を JSON でレスポンス、 CLI が表示

ジョブのログ (stderr) は SQLite に保存され、 `boid job show <job-id>` で読めます。

## どこを読むか

| やりたいこと | エントリ |
|---|---|
| 状態遷移ルールを変える | [`internal/orchestrator/machine.go`](https://github.com/novshi-tech/boid/blob/main/internal/orchestrator/machine.go) |
| hook の発火条件を変える | [`internal/orchestrator/evaluator.go`](https://github.com/novshi-tech/boid/blob/main/internal/orchestrator/evaluator.go) |
| dispatch サイクル全体を追う | [`internal/api/service.go`](https://github.com/novshi-tech/boid/blob/main/internal/api/service.go) の `runDispatchLoop` |
| 1 ジョブの実行を追う | [`internal/dispatcher/runner.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/runner.go) |
| sandbox 内 clone の branch 解決を追う | [`internal/sandbox/runner/clone.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/runner/clone.go)、 [`internal/orchestrator/head_branch.go`](https://github.com/novshi-tech/boid/blob/main/internal/orchestrator/head_branch.go) (`BuildCloneDeclaration`) |
| host command の許可ロジック | [`internal/dispatcher/policy_translate.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/policy_translate.go) |
| サンドボックスの境界 | [`internal/sandbox/broker.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/broker.go) |
| Web UI 認証 | [`internal/api/web_auth.go`](https://github.com/novshi-tech/boid/blob/main/internal/api/) (関連ファイルを参照) |

## 関連ドキュメント

- [概念](../guide/concepts.md) — 用語の定義
- [状態機械](../guide/state-machine.md) — 状態と遷移ルール
- [`project.yaml` リファレンス](../reference/project-yaml.md) — プロジェクト定義のスキーマ
- [Kit 作者向け 概要](../kit-authoring/overview.md) — kit の I/O プロトコル
- [Contributing](../contributing/README.md) — 開発フロー
