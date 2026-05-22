# internal/ リファクタ候補抽出レポート

調査日: 2026-05-22  
対象: `internal/` 直下 17 パッケージ・計 29,395 LOC（本体 `.go` のみ、テスト除く）

---

## 1. Executive Summary

`internal/` 全体を俯瞰したうえで、優先度順にリファクタ対象を 5 件挙げる。

| # | 対象 | 問題の骨格 |
|---|---|---|
| 1 | `api/broker.go:9`, `api/store.go:9` | **api → sandbox 層境界違反** — `sandbox.BuiltinPolicy` が api 層に漏洩 |
| 2 | `tui/task_list.go` (1,316 LOC) | **単一ファイル肥大化** — filter / fetch / render / keybind が混在 |
| 3 | `sandbox/boid_shim.go` (734 LOC) | **サブコマンドパース肥大化** — 全 `boid` サブコマンドが 1 ファイルに詰まる |
| 4 | `orchestrator/spec_loader.go` (1,020 LOC) | **load / validate / merge 混在** — 3 責務が同居 |
| 5 | `dispatcher/runner.go` (701 LOC) | **3 埋め込みレジストリ** — 相互依存なしの 3 struct が単一型に詰まる |

---

## 2. パッケージ別所見

### api （6,081 LOC、auth サブパッケージ含む）

**役割**: HTTP API ハンドラ、Web UI ハンドラ、サービス層（task / project / workflow）、認証ミドルウェア。

**主要ファイル**:
- `api/web.go` 966 LOC
- `api/task.go` 461 LOC
- `api/workflow_action.go` 448 LOC
- `api/task_service.go` 376 LOC
- `api/task_notify.go` 314 LOC
- `api/auth/store.go` 219 LOC

**リファクタ候補**:

- **`api/broker.go:9` — api → sandbox 層境界違反**  
  `api/broker.go` が `sandbox.BuiltinPolicy` を import し、`api/store.go:53` の `BrokerRegistry` インタフェース引数にも同型を使っている。api は sandbox より上位の層であり、sandbox の型変更が api インタフェース変更を引き起こす構造になっている。  
  修正方針: `BuiltinPolicy` を `orchestrator` に昇格させる（型は policy の論理的な設置場所）か、`api` 自前の `BuiltinPolicySpec` 型を定義して `server/wire.go` で変換する。

- **`api/ws_attach.go:21-22` — api → dispatcher 依存**  
  `WSAttachHandler` が `dispatcher.RuntimeSubscriber` / `dispatcher.RuntimeInputWriter` を直接フィールドに持つ。api が dispatcher の具体型に依存することでテストが難しい。  
  修正方針: `api/store.go` に `RuntimeSubscriber` / `RuntimeInputWriter` インタフェースを定義し、dispatcher 側がそれを実装する形に逆転させる。

- **`api/web.go` 3 struct 同居**  
  `WebHandler`（task UI）、`WebManagementHandler`（デバイス管理）、`LoginHandler`（認証）の 3 struct が 966 LOC に同居。各 struct は独立した関心を持つ。分割候補: `web_tasks.go`, `web_management.go`, `web_auth.go`。

- **`api/task_notify.go:24` NotifyTask 9 引数**  
  `NotifyTask(ctx, taskID, message, ask, questionID, sessionID, progress, done, fail string)` — 引数 9 個はモードを表す排他フラグの集合であり、呼び出し元で複数フラグを誤って渡すことをコンパイラが検出できない。`NotifyRequest` struct 化で修正できる。

- **`api/web.go:266-279` lookupProjectName の N+1 パターン**  
  `lookupProjectName` は呼ばれるたびに `Service.ListProjects()` を全件取得してループしている。タスク一覧や詳細ページで複数回呼ばれると O(N×リクエスト数) になる。スライスをリクエストスコープでキャッシュするか、`GetProject(id)` を呼ぶ単一ルックアップに切り替えるべき。

---

### client （523 LOC）

**役割**: UNIX ソケット経由で daemon API を呼ぶ HTTP クライアント。`client.go` 405 LOC、`autostart.go` 118 LOC。

**主要ファイル**: `client/client.go` 405 LOC

**リファクタ候補**: 特になし。関数が機能別に整理されており肥大化の予兆もない。

---

### config （127 LOC）

**役割**: `~/.config/boid/config.yaml` の読み書き。XDG パス解決。単一ファイル。

**リファクタ候補**: 特になし。

---

### daemon （173 LOC）

**役割**: `boid start` 時の daemon 化（double-fork、PID ファイル書き込み）。単一ファイル。

**リファクタ候補**: 特になし。

---

### db （482 LOC、migrate サブパッケージ含む）

**役割**: SQLite 接続ラッパー（`db.go` 62 LOC）と スキーママイグレーション（`migrate/migrate.go` 420 LOC）。

**主要ファイル**: `db/migrate/migrate.go` 420 LOC

**リファクタ候補**:

- **スキーマとクエリ列の二重管理**  
  `orchestrator/store.go` の `taskSelectCols` 定数（34-37 行）がテーブルカラムを文字列で列挙しており、`migrate/migrate.go` のスキーマ定義と二重管理になっている。スキーマ変更時に両ファイルの同期を手作業で行わねばならない。型安全なカラム列挙を導入するのは過剰かもしれないが、少なくとも `taskSelectCols` のそばにスキーマ参照コメントを入れるべき。

---

### dispatcher （3,992 LOC）

**役割**: sandbox の起動・終了管理、worktree の割り当て、broker token 管理、job の完了待ち。orchestrator から JobSpec を受け取り sandbox.Spec に変換して実行する層。

**主要ファイル**:
- `dispatcher/sandbox_builder.go` 729 LOC
- `dispatcher/runner.go` 701 LOC
- `dispatcher/worktree_manager.go` 562 LOC
- `dispatcher/store.go` 429 LOC
- `dispatcher/runtime_local_linux.go` 534 LOC

**リファクタ候補**:

- **`dispatcher/runner.go` — 3 つの埋め込みレジストリ**  
  `Runner` struct がロック変数 3 本（`tokenMu`・`waiterMu`・`runtimeMu`）と対応 map 3 本（`jobTokens`・`jobWaiters`・`completedJobs`・`taskRuntimes`）を内包する。3 つのレジストリ（broker token 管理、job completion waiter、task runtime 追跡）は互いに依存せず、独立した型として切り出せる。テスト時に partial mock が困難な構造。

- **`dispatcher/sandbox_builder.go:548-589` — `contextFiles` のシリアライズ責務**  
  `contextFiles()` が task.yaml / instructions.yaml / environment.yaml の生成・マーシャリングまで担っており、独自の struct 定義（`environmentDoc`、`workspaceProjectEntry`）も同ファイルに持つ。`sandbox_context.go` として分離すると `sandbox_builder.go` が ~400 LOC に収まる。

- **`dispatcher/worktree_manager.go`（562 LOC）— 複数責務の混在**  
  worktree の作成・削除・ロック・HEAD ガードが 1 ファイルに詰まっている。`worktree_store.go`（105 LOC）が DB 側を別ファイルに持っているのと対称性がとれていない。HEAD 検証ロジック（`EnforceHeadOnBaseBranch`）は独立ファイルにする候補。

---

### initwizard （331 LOC）

**役割**: `boid init` 対話型ウィザード。プロジェクト初期化、kit 検出・適用。単一ファイル。

**リファクタ候補**: 特になし。`wizard.go` 331 LOC は機能数に対して妥当。

---

### kit （123 LOC）

**役割**: kit 検出 (`detect.go` 80 LOC) と requirements チェック (`requirements.go` 43 LOC)。

**リファクタ候補**: 特になし。

---

### logrotate （150 LOC）

**役割**: サイズベースのログローテーション（`io.Writer` + `io.Closer` 実装）。単一ファイル。

**リファクタ候補**: 特になし。

---

### notify （78 LOC）

**役割**: デスクトップ通知（`notify.go` 78 LOC）。`notify-send` を exec するシンプルなラッパー。

**リファクタ候補**: 特になし。

---

### orchestrator （6,026 LOC、refname サブパッケージ含む）

**役割**: タスクライフサイクル、状態機械、プロジェクト/kit メタ読み込み、hook dispatch、payload マージ、worktree ロック。boid の中核ドメインロジックを担う最大パッケージ。

**主要ファイル**:
- `orchestrator/spec_loader.go` 1,020 LOC
- `orchestrator/store.go` 549 LOC
- `orchestrator/spec_types.go` 496 LOC
- `orchestrator/coordinator.go` 496 LOC
- `orchestrator/planner.go` 238 LOC
- `orchestrator/machine.go` 218 LOC

**リファクタ候補**:

- **`orchestrator/spec_loader.go` — load / validate / merge の混在**  
  1,020 LOC の単一ファイルに 3 種の責務が同居している。
  1. **ロード**: `ReadProjectMeta`, `ReadProjectMetaWithKits`, `ReadKitMeta`, `ReadProjectLocalMeta` — YAML を読んでオブジェクトに変換する
  2. **バリデーション**: `rejectRemovedBehaviorFields` (155-187 行)、`validateHookKind` (292-300 行)、`validateProjectLocalFields` (877-891 行)、`validateProjectLocalMeta` (893-912 行)
  3. **クローン/マージユーティリティ**: `cloneProjectMeta`, `cloneTaskBehaviorMap`, `mergeBindMounts`, `unionBindMountSlices`, `mergeStringMaps`, `mergeHostCommands`, `cloneBindMounts` (914-1008 行)  
  分割案: `spec_validate.go`（バリデーション群 ~150 LOC）、`spec_merge.go`（マージ/クローン群 ~200 LOC）、`spec_loader.go`（ロードフロー ~670 LOC）。

- **`orchestrator/spec_types.go` — 型定義と非自明ロジックの混在**  
  `HostCommands.UnmarshalYAML`（79-99 行）・`Instructions.UnmarshalJSON`（148-178 行）・`RawPayload.UnmarshalYAML/JSON` (286-319 行) など、YAML/JSON のフォーマット変換ロジックが型定義ファイルに埋まっており、見つけにくい。型定義ファイルにロジックを置くこと自体は Go では一般的だが、`RawPayload` の dual-format marshal は非自明であり専用コメントを補強すべき。

- **`orchestrator/coordinator.go:496` — `json.Unmarshal` 未使用の raw map 変換**  
  既知の残課題（`project_payload_patch_json_followups.md` #3）: `coordinator.go:495` 付近の `coordinator.go` で `json.Unmarshal` 化が未完。`normalizeYAMLKeys` による YAML 非文字列キーの正規化に依存しているが、呼び出し元を `json.Unmarshal` ベースに変えることで依存を除去できる。

---

### qrterm （64 LOC）

**役割**: QR コードをターミナルに表示するユーティリティ。単一ファイル。

**リファクタ候補**: 特になし。

---

### sandbox （3,707 LOC）

**役割**: unshare/pasta ベースの Linux ユーザー名前空間サンドボックス実装。boid shim パース、broker（コマンド委譲）、git builtin プロキシ、mount spec 生成。

**主要ファイル**:
- `sandbox/boid_shim.go` 734 LOC
- `sandbox/broker.go` 592 LOC
- `sandbox/git_builtin.go` 440 LOC
- `sandbox/git_shim.go` 365 LOC
- `sandbox/broker_streaming_linux.go` 224 LOC
- `sandbox/protocol.go` 221 LOC
- `sandbox/script.go` 215 LOC

**リファクタ候補**:

- **`sandbox/boid_shim.go` — サブコマンドパース肥大化**  
  `parseBoidRequest` を起点に `parseBoidJobDone`, `parseBoidTaskCreate`, `parseBoidTaskUpdate`, `parseBoidTaskNotify`, `parseBoidTaskShow`, `parseBoidExec`, `parseBoidActionSend`, `parseBoidJobList`, `parseBoidJobShow`, `parseBoidJobLog` など多数の parse 関数が 1 ファイルに詰まっている。サブコマンドが増えるたびに肥大化する構造。  
  分割案: `boid_shim_job.go`（job サブコマンド群）・`boid_shim_task.go`（task サブコマンド群）・`boid_shim_exec.go`（exec / action サブコマンド）に分離。各ファイルは ~200 LOC に収まる見込み。

- **`sandbox/broker.go` — 複数の dispatch 経路が同居**  
  boid builtin 委譲（`BoidExecutor`）、host command 委譲（`exec.Command`）、git builtin 委譲（`GitBuiltinExecutor`）の 3 経路が `handleRequest` 関数の中で分岐している（592 LOC）。各経路の mock が困難。テスタビリティ向上のために dispatch 判定ロジックを interface 経由で抽象化できる。

---

### server （1,772 LOC）

**役割**: UNIX ソケット + TCP サーバの起動、全コンポーネントの依存注入 (`wire.go`)、boid builtin コマンドのアダプタ (`boid_executor.go`)。

**主要ファイル**:
- `server/wire.go` 571 LOC
- `server/boid_executor.go` 419 LOC
- `server/api_store.go` 370 LOC
- `server/server.go` 241 LOC

**リファクタ候補**:

- **`server/wire.go` — 依存注入が肥大化方向**  
  現状 571 LOC で機能しているが、コンポーネント数が増えるほど肥大化する。Go では手書き DI が標準的なので構造問題ではないが、`buildProjectStore`（44-61 行）のような補助 builder 関数が増えると wire.go 自体が読みにくくなる。現時点では許容範囲。

- **`server/boid_executor.go` (419 LOC) — switch 巨大化の予兆**  
  `ExecuteBoidBuiltin` の `switch req.Op` (41 行) が全 boid builtin op をカバーしており、op 追加のたびに本ファイルが増える構造。現在は 419 LOC で問題ないが、sandbox/boid_shim.go のサブコマンド追加に連動して拡大する。

---

### skills （58 LOC）

**役割**: 埋め込み skill ファイルの展開。`//go:embed` で skill ディレクトリを同梱。

**リファクタ候補**: 特になし。

---

### timeline （341 LOC）

**役割**: タスク詳細の timeline グループ化ロジック。`timeline.go` が api と tui の両方から利用される。api-free 設計で importサイクル回避済み。

**リファクタ候補**: 特になし。

---

### tui （5,367 LOC）

**役割**: Bubble Tea ベースの TUI。タスク一覧、詳細、フォーム、インタラクティブ接続。

**主要ファイル**:
- `tui/task_list.go` 1,316 LOC
- `tui/task_detail.go` 841 LOC
- `tui/task_form.go` 427 LOC
- `tui/task_answer.go` 271 LOC
- `tui/task_detail_timeline.go` 241 LOC
- `tui/task_detail_payload.go` 163 LOC
- `tui/instructions_role_edit.go` 229 LOC

**リファクタ候補**:

- **`tui/task_list.go` (1,316 LOC) — 4 責務の混在**  
  以下の 4 つが 1 ファイルに同居しており、Bubble Tea の Model/Update/View 境界が不明瞭:
  1. データモデル・メッセージ型（`taskTickMsg`, `tasksMsg` 等、69-150 行）
  2. ポーリング・データ取得（`tickIntervalForTasks`, `fetchTasks`, `fetchProjects` 等）
  3. フィルタ・ソートロジック（`applyFilter`, `sortTasks`, `filterForTab` 等）
  4. テーブル描画（`renderTable`, `renderRow`, `buildColumns` 等）  
  分割案: `task_list_model.go`（struct + messages + Update コア）、`task_list_view.go`（View + 描画ヘルパー）、`task_list_fetch.go`（ポーリング + データ変換）。各ファイルは ~400-500 LOC に収まる見込み。

- **`tui/task_detail.go` (841 LOC) — 詳細画面の複合責務**  
  `task_detail_timeline.go` (241 LOC)・`task_detail_payload.go` (163 LOC) に一部は分離済みだが、残りの 841 LOC はタブ切り替え、インタラクション、フォーム状態管理、hook リスト描画を担う。`task_detail_tabs.go` (73 LOC) は既に存在するが本体ファイルとの責務境界が曖昧。

---

## 3. 横断的観察

1. **`api` → `sandbox` 層境界違反**  
   `api/broker.go:9` と `api/store.go:9` が `sandbox` を import している。設計意図では `api` は `orchestrator` より上位で `dispatcher` / `sandbox` より高レベルに位置するが、`sandbox.BuiltinPolicy` の型が api 層に漏洩している。`orchestrator` パッケージへの型移動、または `api` 独自の型定義 + `server/wire.go` での変換が必要。

2. **`api` → `dispatcher` 依存（`api/ws_attach.go:15`）**  
   WebSocket PTY attach ハンドラが `dispatcher.RuntimeSubscriber` / `dispatcher.RuntimeInputWriter` を直接フィールドに持つ。api パッケージは dispatcher の具体型から独立すべきで、インタフェースを `api/store.go` 側に定義する形が望ましい。

3. **環境変数名の分散管理**  
   `BOID_TASK_ID`, `BOID_JOB_ID`, `BOID_BROKER_SOCKET`, `BOID_BROKER_TOKEN`, `BOID_BUILTIN_SHIM` 等の文字列が `dispatcher/sandbox_builder.go` と `sandbox/boid_shim.go` に分散して直書きされている。片方を変更しても他方が変わらないとサイレント失敗する。`orchestrator` または専用 `envkeys` パッケージへの定数集約が望ましい。

4. **エラー型の不統一**  
   `api` 層では `*StatusError{Code, Message}` を使って HTTP ステータスコードを持たせているが、`orchestrator` 層の純粋なエラーが `api` サービス内で `*StatusError` に変換される箇所と、そのまま素通しになる箇所が混在している。エラー変換タイミングが不定で、caller が HTTP コードを正しく受け取れないケースがある。

5. **クローン/マージユーティリティの重複**  
   `orchestrator/spec_loader.go` の `mergeStringMaps`, `cloneBindMounts`, `mergeHostCommands` と、`dispatcher/sandbox_builder.go` の `cloneStringMap` が独立して実装されている。機能的には同一パターン（map の浅コピーとオーバーレイ）。共通ユーティリティとしての集約は過剰かもしれないが、`orchestrator` 側の実装が spec_loader に埋まっていて再利用困難な状況は解消すべき。

6. **`dispatcher/runner.go` のロック 3 本体制**  
   `tokenMu`（broker token 管理）、`waiterMu`（job completion waiter + completedJobs）、`runtimeMu`（task runtime 追跡）の 3 mutex が 1 struct に存在する。それぞれが独立したライフサイクルを持つため、誤ったロック取得順序によるデッドロックリスクがある（現状は問題なし）。独立した `tokenRegistry`, `waiterRegistry`, `runtimeRegistry` 型に分離するとロック範囲が明示化される。

7. **`sandbox/boid_shim.go` と `server/boid_executor.go` のコマンドセット同期問題**  
   boid shim 側（`parseBoidRequest`）と executor 側（`ExecuteBoidBuiltin` の switch）がサポートするオペレーションセットを独立して管理している。新しい boid builtin op を追加する際に両方の更新が必要だが、コンパイラが漏れを検出できない。`sandbox.BoidOp` 型の exhaustive switch を使うか、op テーブルを一元化することで対処できる。

8. **`orchestrator/spec_loader.go` のバリデーション実行タイミング**  
   `ReadProjectMeta` は raw YAML を `yaml.Unmarshal` で 2 回読んでいる（1 回目: `raw map[string]any` でフィールド名検査、2 回目: `meta ProjectMeta` で型付き decode）。ダブル Unmarshal は正当な理由（removed fields の検出）があるが、コメントがないと冗長に見える。

9. **`orchestrator/machine.go` のルール定義と実装の距離**  
   `NewMachine()` 内のルールコメント（116-158 行）がドメイン知識として非常に重要だが、ルール配列のインラインにしか存在しない。状態機械の振る舞いを説明するより整理された外部ドキュメント（`docs/` または ADR）があると保守性が上がる。これは docs 不足の問題であって機能バグではない。

10. **`tui` と `api` のデータ変換層の薄さ**  
    `tui` は `internal/client` を通じて api を呼ぶが、api の Response 型（`api.Job`, `api.Task` 等）をそのまま TUI の描画に使用している。View ロジックと API 型が密結合しており、api の型変更が TUI の描画ロジックに直接影響する。`tui` 専用の view model 型を持つことで境界を引ける。

---

## 4. 次のステップの提案

調査結果を boid task として割る場合の推奨分割を示す。()内は並列可能性を示す。

### グループ A: 層境界修正（単独実施推奨・他に先行）

| task | 内容 | 依存 |
|---|---|---|
| A-1 | `sandbox.BuiltinPolicy` を `orchestrator` へ移動し、`api` / `server` / `dispatcher` の import を整理 | なし |
| A-2 | `api/store.go` に `RuntimeSubscriber` / `RuntimeInputWriter` インタフェースを定義し `api/ws_attach.go` を decoupled 化 | A-1 完了後 |

### グループ B: ファイル分割（並列実施可能）

| task | 内容 | 依存 |
|---|---|---|
| B-1 | `orchestrator/spec_loader.go` を `spec_loader.go` / `spec_validate.go` / `spec_merge.go` に分割 | A-1 完了後 |
| B-2 | `sandbox/boid_shim.go` を `boid_shim.go` / `boid_shim_job.go` / `boid_shim_task.go` / `boid_shim_exec.go` に分割 | なし（B-1 と並列可） |
| B-3 | `api/web.go` を `web_tasks.go` / `web_management.go` / `web_auth.go` に分割 | なし（B-1, B-2 と並列可） |
| B-4 | `dispatcher/sandbox_builder.go` から `sandbox_context.go` を抽出 | なし |

### グループ C: 構造改善（B 完了後推奨・並列可）

| task | 内容 | 依存 |
|---|---|---|
| C-1 | `dispatcher/runner.go` の 3 レジストリを独立型に切り出し | B 完了後 |
| C-2 | `tui/task_list.go` を model / view / fetch の 3 ファイルに分割 | なし（B と並列可） |
| C-3 | 環境変数名定数を `orchestrator/envkeys.go` に集約 | A-1 完了後 |

### グループ D: 品質改善（いつでも着手可能）

| task | 内容 |
|---|---|
| D-1 | `api/task_notify.go:24` — `NotifyTask` 引数を `NotifyRequest` struct 化 |
| D-2 | `api/web.go:266` — `lookupProjectName` の N+1 修正（`GetProject` 単一呼び出しへ） |
| D-3 | `orchestrator/spec_loader.go` ダブル Unmarshal にコメント補強 |

---

*本レポートは候補抽出のみを目的とする。実際の修正は本タスクでは行わない。*
