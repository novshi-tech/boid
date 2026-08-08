# リファクタリング バックログ

2026-07-05 のコードベース棚卸し (dispatcher / sandbox / api・server・cmd / orchestrator の
4 方面並列調査 + 主要指摘の grep 裏取り) で出たリファクタリング候補のうち、
**自律タスクに委譲しなかった中〜大物**を将来の着手用に記録するメモ。

**2026-08-08 に再棚卸しを実施** (既存項目の生存確認 + dispatcher/sandbox・
api/server/apigateway/web・cmd/orchestrator/config/client の 3 方面新規発掘)。
既存項目の行番号・行数は 2026-08-08 時点の main (644e3c74) に更新済み。
新規候補は末尾の「2026-08-08 追補」節にある。それでも着手時は必ず再検証すること。

## 委譲済みバッチ (このメモのスコープ外)

低リスク 5 件は supervisor タスク `refactor-deadcode-boilerplate-batch`
(ce3eb3d2, 2026-07-05 起票) で PR 化済み (#706 #708-#711、2026-07-05 に全て main 入り):

1. `remove-depends-on-remnants` — migration 0026 で廃止済み depends_on 機構の API 層残骸撤去
2. `api-web-boilerplate-helpers` — web.go の redirect / エラー描画 / JobView 変換の集約
3. `dispatcher-small-cleanups` — DockerEnabled デッドフィールド・redockey リネーム・PRAGMA N+1
4. `sandbox-git-dead-path` — ExecRequest.Git 死に経路撤去・classify 系の配置修正
5. `orchestrator-notfound-sentinel` — not-found 文字列マッチのセンチネル化

---

## 大物 (独立プラン級。着手時は設計から)

### L1. `BuildSandboxSpec` の分割 【2026-08-08: 有効・悪化 (356→454 行)】

`internal/dispatcher/sandbox_builder.go:353-806` (454 行の単一関数。ファイル全体 1,908 行)。
env 組み立て / adapter binding 解決 / broker・server・docker proxy socket マウント /
project・worktree・peer・.boid マウントレイアウト / attachments / worktree モードの
argv 再マッピング / context ファイル生成 / stdin・stdout ルーティング /
boid バイナリ + git shim マウント / harness 解決、が全部同居している。

- 方向性: `buildSandboxEnv()` / `buildSandboxMounts()` / `buildContextFiles()` へ分割し、
  `BuildSandboxSpec` は調停役に絞る。マウント系は既存の `projectVisibilityMounts` と
  同じ粒度の副関数群にする。
- 追い風: `sandbox_builder_test.go` が 2,850 行あり回帰網は厚い。
  Tier 1 #3 (PR #698) の入口別 builder unit テストも効く。
- リスク: binding マージは配線退行の常習犯 (品質ゲート計画の Tier 1 #1 参照)。
  分割は「移動のみ・挙動不変」を PR 単位で守り、/boid-review の wiring レンズを通すこと。
- 同時に拾う小物: `projectVisibilityMounts` (sandbox_builder.go:1301-1305) の
  位置引数 struct 化 (string×5 + bool + map に増加)。追補 N9/N10 の struct 化と同一 PR 可。

### L2. orchestrator の god パッケージ分割 【2026-08-08: 有効・悪化 (44→50 ファイル)】

`internal/orchestrator` は非テスト 50 ファイルが 1 パッケージに同居。2026-07-05 の分類に
無い新顔も増えている (base_branch / branch_var / head_branch / home / host_commands_config /
policy_ops / project_bare_repo / project_explain / readonly / secret_scan / workspace_apply /
workspace_envelope* / workspace_migration / workspace_repository 等)。境界は既に明確:

- spec 読み込み/パース系: spec_loader / spec_loader_legacy / spec_types / spec_payload /
  spec_resolve / behavior_resolve / payload_merge / awaiting_payload / conditions / base_branch_classify
- DB ストア系: store / project_catalog / repository / project_lock / gc_loop / gc_sandbox_tmp / db_adapters
- ステートマシン系: coordinator / machine / planner / evaluator / lifecycle / policy / jobspec
- workspace・kit メタデータ系: workspace_store / workspace_meta / workspace_slug /
  kit_registry / kit_name / project_store

方向性 (段階的に):

1. **最初の一歩 (低リスク)**: `spec_loader_legacy.go` (194 行) の全シンボルの実利用者は
   `cmd/project_migrate.go` (+テスト) のみ (2026-08-08 再確認済み)。migrate 用
   サブパッケージへ移設して orchestrator の公開 API から legacy スキーマ知識を排除する。
   追補 N26 (`cmd/project_migrate.go` の internal/projectmigrate 化) と受け皿が重なるため、
   同じ新パッケージへ寄せる設計を先に決めること。
2. 次に相互依存の少ない workspace/kit メタ系を切り出す。
   サブパッケージ分割の前例として `internal/orchestrator/refname` が既にある。
3. 本丸の spec 系 / store 系 / ステートマシン系の 3 分割は、公開シンボルの
   使用箇所が server / api / dispatcher に広く波及するため独立 PR 系列で。

- 同時に解消したい命名混乱: `project_store.go` (インメモリ ProjectMeta キャッシュ) と
  `project_catalog.go` (Project 行の DB CRUD) は名前が紛らわしく別ドメイン。
  分割時に前者を workspace メタ系、後者をストア系へ振り分ける。
- 関連: `repository.go` (413 行 / 33 関数) はほぼ全メソッドが store.go / project_catalog.go の
  パッケージ関数へ 1 行委譲するだけの二重 API。分割の際にフリー関数とレシーバ型の
  どちらかへ一本化する (DI 都合ならフリー関数を非公開化して Repository へ集約)。
- アンチゴール: workspace_store (YAML) / project_catalog (SQL) / kit_registry (FS 走査) は
  永続化バックエンドが異なる。CRUD の見た目が似ていても共通抽象を作らないこと。
- 制約: sqlite 依存で sandbox 内 build/test 不可のパッケージ群 (メモリ
  `sandbox-cannot-build-sqlite-packages` 参照)。import パス張り替えの検証は CI 委譲になる。

### L3. `workflow_action.go` の巨大メソッド分解 【2026-08-08: 有効。depends_on 前提は解消済で着手可】

`internal/api/workflow_action.go` (全 476 行):

- `ApplyAction` (25-192, 168 行): state machine 適用・reopen ペイロード特殊処理・
  Tx・ロック解放・Hub broadcast・dispatch loop spawn・hook 評価が 1 メソッドに凝縮。
- `runDispatchLoop` (207-339, 133 行): ロック取得・dispatch cycle・ペイロード永続化・
  awaiting/terminal 検出・auto-advance が混在。

方向性: `ApplyAction` から `applyReopenInstruction(...)` / `evaluateMatchedHooks(...)` を抽出、
`runDispatchLoop` は cycle 内の永続化ブロックを `persistHookPayload(...)` へ。

- **リスク最高**: 並行性・ロック・trait 消去のタイミングが正しさに直結。
  テスト整備 (coordinator 系テストの api 層版) とセットで着手すること。
- ~~前提: 委譲済みバッチの depends_on 撤去が先に入ると軽くなる~~ →
  **2026-08-08: `triggerDependentTasks` / depends_on は internal/api から消滅済み。待ち条件なし。**

### L4. runner のエラーパス後始末を cleanup スタック化 【2026-08-08: 有効。Dispatch が 200→600 行に 3 倍化】

`internal/dispatcher/runner.go` の `Dispatch` (370-969, 600 行) と `launchSandbox`
(1408-1500) に `if cleanup != nil { cleanup() }` が 12 箇所コピペされ (417, 445, 494,
504, 533, 553, 617, 849, 963, 1413, 1453, 1489)、`stopDockerProxy` /
`cleanupSandboxArtifacts` も各エラーパスで手動再掲 (960 / 1451 / 1488)。早期 return を
足すたびに後始末の追加漏れ = リソースリーク (broker token / docker proxy / job row) の罠になる。

方向性: defer ベースの「成功したら無効化するクリーンアップスタック」
(`committed = true` で skip する定石) へ集約。

- リスク: 巻き戻し順序が分岐ごとに微妙に異なる箇所があり、機械的置換では済まない。
  過去の bind mount 削除事故 (メモリ `stale-bind-mount-deletion-incident`) の系譜で、
  cleanup の実行タイミング変更はサンドボックス安全性に直結する。挙動表を作ってから着手。
- 追補 N7 (`containerBackend.Launch` の手動巻き戻し 4 パス) は同型の問題の
  container_backend 版。同一 PR 系列に含めるのが得。

---

## 中物 (PR 1〜2 本サイズ。すきま時間で拾える)

### M1. `boid_shim.go` のフラグパースをテーブル駆動化 【2026-08-08: 有効・悪化 (parse 関数 10→22 個)】

`internal/sandbox/boid_shim.go` (全 1,024 行) の `parseBoid*` 関数 22 個 (119-982 行) が
同一の for+switch 手書きパターンを反復。`takeStringFlagValue` (609-618) は存在するが
呼び出しが 30 箇所超あるだけで骨格はコピペのまま。`-f`/`--file`, `-m`/`--message` の
flagName 選択も 4 箇所コピペ (301 / 683 / 712 / 963)。`{names, dest}` のテーブル駆動
パーサへ。まず flagName 選択を `takeStringFlagValue` 内へ吸収するだけでも効く。

### M2. `handleBoidBuiltin` の分解 【2026-08-08: 有効・悪化 (195→291 行、16→23 op)】

`internal/sandbox/broker.go:331-621` (291 行、case 20 個 / 23 op)。必須フィールド検証・
project 名前解決・task import のバッチ処理 (480 付近〜) が 1 つの switch に同居。
op ごとの前処理をバリデータテーブルへ、少なくとも import ブロックを
`validateAndResolveImport(...)` へ抽出。
注意: builtin op を触る場合は policy_test.go の wantOps + dispatcher drift test の
更新が必要 (メモリ `sandbox-cannot-build-sqlite-packages` の末尾)。
daemon 側の対 (追補 N2 `ExecuteBoidBuiltin`) と同時に設計すると二度手間が減る。

### ~~M3. git builtin フラグ集合の二重管理解消~~ (obsoleted by git gateway cutover PR8-A)

`internal/sandbox/git_builtin.go` / `git_shim.go` は git gateway cutover PR8-A で
削除済み (2026-07-14)。対象コードが存在しないため本項は obsolete。

### ~~M4. worktree_manager の git exec ヘルパ集約~~ (obsoleted by git gateway cutover PR8-B)

`internal/dispatcher/worktree_manager.go` は git gateway cutover PR8-B で
削除済み (2026-07-14)。host worktree 割当機構自体が廃止 (sandbox 内 clone に置換)
されたため本項は obsolete。

### ~~M5. `cmd/kit.go` と `cmd/workspace.go` の sandbox 起動シーケンス共通化~~ (obsoleted by kit 機構退役)

`cmd/kit.go` は Phase 2.5 PR6 (fb1f2224, kit 機構退役) で削除済み。
`runKitInit` / `runWorkspaceConfigure` / `kitInitExecFn` / `workspaceConfigureExecFn` /
`BuildInitJobSpec` はリポジトリ全体で 0 件。重複対象そのものが消滅したため obsolete
(2026-08-08 確認)。

### M6. `wire.go` `mountRoutes` の分割 【2026-08-08: 有効・大幅悪化 (190→382 行)。優先度上げてよい】

`internal/server/wire.go:1877-2258` (382 行。ファイル全体 2,258 行の末尾を占める)。
インライン health/shutdown/proxy/broker ハンドラ・10 超の r.Mount・GC ループ構築・
認証ミドルウェア + Web ルート group・TCP ラッパ生成が 1 関数に同居。
`mountInfoRoutes` / `mountAPIHandlers` / `mountWebRoutes` へ分割。単純 JSON 系は
テーブル登録化。chi のミドルウェア登録順序と group 境界に注意。
同ファイルの `buildRuntime` (追補 N1、700 行) と合わせて wire.go 全体の分割計画にするのが得。

### M7. cmd/ 層の細かい定型句 【2026-08-08: 前半 obsolete、decodeJSON はスコープ拡大】

- ~~`client.NewUnixClient(client.DefaultSocketPath())` の 30 箇所超反復 → `defaultClient()` ヘルパ~~
  → **obsolete**: cli-remote-connection Phase 3 PR1 で root の PersistentPreRunE が
  `*client.Client` を context 注入する方式へ移行済み。各 RunE は `client.FromContext` を使う。
- list/show/remove の「ref 解決 → Do → renderOutput」パターンが 3 ファイルで並走。
  薄いジェネリックヘルパで縮む。
- `decodeJSON(w, r, &req) bool` ヘルパ: **task.go 限定ではなく internal/api の
  10 ファイル・20 箇所** に `json.NewDecoder(r.Body).Decode` + `writeError(w, 400, ...)` の
  定型が存在 (project.go 6 / task.go 複数 / session.go / action.go / secret.go /
  oauth_login.go / job.go / gc.go / web.go / broker.go)。エラー文言も
  `"invalid request body"` / `"invalid request"` の 2 種に割れている。全ファイルへ展開する。

### M8. orchestrator の関数レベル整理 (L2 と独立に先行可) 【2026-08-08: 一部解消・一部悪化】

- `ProjectStore.GetWithWorkspace` (project_store.go:445-634, 190 行 — 当時 112 行から悪化):
  SecretNamespace 注入・degraded 分岐・workspace kit/env マージが混在。
  `injectWorkspaceKits` / `injectWorkspaceEnv` へ抽出、
  stripAliasMirrors→ループ→addAliasMirrors の重複イディオムを `mapBehaviors(out, fn)` で共通化。
- ~~`ReadProjectMeta` の分解~~ → **解消済み**: ロジックは `parseProjectMetaBytes`
  (spec_loader.go:63-156, 94 行) へ切り出され bare-repo ローダーと共有されている。
  補間ブロックの分離を続けるなら対象は `parseProjectMetaBytes`。優先度低。
- `GCTasks` (store.go:301-421, 121 行): table→GCResult の switch 反復は 3→2 箇所に縮小。
  `table 名 → *int64` の対応表 1 つに統一する案は引き続き有効 (優先度は下がった)。
- `NotifyTask(ctx, taskID, message, ask, questionID, progress, done, fail string)` の
  string 8 連発シグネチャが internal/api に 3 箇所そのまま (task.go:70 interface /
  task_notify.go:34 実装 / task_notify_test.go:26 fake)。パラメータ struct 化。
  本体 238 行の分解 (追補 N5) とは **PR を分ける** こと。

---

## 2026-08-08 追補 (新規候補)

3 方面並列調査 (dispatcher/sandbox、api/server/apigateway/web、cmd/orchestrator/config/client)
で発掘。番号は N1〜。行番号は 2026-08-08 時点の main (644e3c74)。

### 優先着手推奨 (実害実績あり・低リスク高効果)

#### N5A. bounded Adopt + `ctx.Err()` 分岐の 8 箇所コピペ集約 ★最優先

- `internal/dispatcher/runtime_subscriber_export.go:151-250` (Subscribe / WriteInput /
  ResizeRuntime / CloseInput) と `internal/dispatcher/runner.go:1594-1862` (StopJobRuntime /
  SignalJobRuntime / CanAttach / ResizeRuntimeID) の計 8 箇所が
  「`runtimeIDForJob` → `context.WithTimeout(..., sessionControlCallTimeout.Get())` →
  `sandboxBackend().Adopt` → `if !ok { if ctx.Err() != nil { slog.Warn } }`」を
  ログ文言だけ差し替えてコピペ。
- **PR #857 / #862 / eccd476e で「片方だけ直して残りが drift」を 3 回繰り返した実績あり**
  (`ResizeRuntimeID` の doc コメントが自白している)。再発防止効果が最大。
- 方向性: `withAdoptedSession(ctx, runtimeID, opName string, fn func(backend.SandboxSession) error)`
  へ集約。Canceled vs DeadlineExceeded の Warn/Debug 出し分けはヘルパ内に 1 回だけ。
  deadline の起点が `context.Background()` 系と呼び出し元 ctx 派生の 2 種ある点は引数で明示。
- 規模: 中 / リスク: 低〜中 (ログ文言変更のみ宣言が要る)。

#### N12A. `container_session.go` の分離 (移動のみ) — **完了 (2026-08-08, PR #914)**

- `internal/dispatcher/container_backend.go` (3,247 行) に `containerBackend` (555-2372) と
  `containerSession` (2373-3247) という責務も生存期間も違う 2 型が同居していた。
  既に `container_backend_workspace_init.go` 等へ切り出す慣習があるのにセッション層だけ残り。
- 対応: 2373-3247 を `container_session.go` へそのまま移動 (同一パッケージ内、宣言移動のみ)。
  内容の完全一致は AST 単位の diff (旧/新の宣言集合比較) で確認済み。テストファイル
  (`container_backend_test.go` → `container_session_test.go`) の分離は見送り (Adopt 系と
  session 系のテストが交互に並んでおり分離コストが高いため、リスク最小優先)。
- N1/N3 分割の下地になる。

#### N14A. `internal/api/store.go` の ports 分割 (移動のみ) — **完了 (2026-08-08, PR #914)**

- `internal/api/store.go` (504 行) は "store" という名前に反して 35 超の interface と DTO の
  雑多置き場だった。
- 対応: `ports_workspace.go` (project/workspace 関連) / `ports_task.go` (task/job 関連) /
  `ports_session.go` (session/exec 関連) へ機械的分割 (同一パッケージ内なので参照修正ゼロ)。
  `store.go` 自体は全内容移動後に削除。内容の完全一致は AST 単位の diff で確認済み。

### 巨大関数 (独立プラン級の新顔)

#### N1. `buildRuntime` (700 行) — 未記載物件の最大

- `internal/server/wire.go:1012-1707`。config 検証 / orphan sweep / stale 復旧 /
  各種レジストリ / dispatcher.Wire / sandbox backend / gateway credential provider /
  OAuth2 TokenSource 組み立てまで 1 関数。M6 (`mountRoutes`) と同ファイルで、あちらの 2 倍。
- 方向性: `buildRecoverySweeps` / `buildGateways` / `buildAppServices` 相当へ切り出し。
  大半は「変数を作って次へ渡す」直線コードで機械的に分けやすい。
- 規模: 大 / リスク: 中〜大 (起動順序が意味を持つ箇所あり)。M6 と合わせて wire.go 分割計画に。

#### N2. `ExecuteBoidBuiltin` (640 行の単一 switch)

- `internal/server/boid_executor.go:86-726` (25 op)。`&sandbox.ExecResponse{ExitCode: 1, ...}`
  が 99 箇所、"…unavailable" 依存性 nil チェックが 29 箇所。
- 方向性: op → `{requires, validate, run}` のハンドラテーブル化。まず `errResp(msg)` /
  `requireDeps(...)` の 2 ヘルパだけで 100 行超落ちる。
- 規模: 大 / リスク: 中。M2 (`handleBoidBuiltin`、sandbox 側の対) と同時設計が得。
  policy_test.go の wantOps + dispatcher drift test の同期必須。

#### N3. `containerBackend.Launch` (332 行) + N7 手動巻き戻し

- `internal/dispatcher/container_backend.go:982-1313`。TLS materialize / spec 書き出し /
  mount 実体化 / network 解決 / env 加工 / ContainerCreate〜start / transcript spool が同居。
- N7: 同関数の 4 エラーパス (1254-1310) が transcriptFile Close + removeHalfBuiltContainer +
  cleanupFiles を毎回手書き。**過去に fd leak を 2 箇所やった記録がコメントに残っている**。
  L4 と同型なので同一 PR 系列へ。
- 規模: 大 / リスク: 中〜高。先に N7 (巻き戻しの defer 化) を片付けてから分割が安全。

#### N4. `containerSession.waitLoop` (214 行)

- N12A (2026-08-08) で `internal/dispatcher/container_session.go:661-874` へ移動済み
  (旧 `container_backend.go:3011-3224`)。exit 分類 / transcript flush / subscriber close /
  diagnostics / ContainerRemove / artifact 削除が 1 goroutine 本体に直列。
  末尾の artifact 削除は Launch 側 `cleanupFiles` とほぼ同内容の二重管理。
- 方向性: `classifyExit` → `flushTranscript` → `publishExit` → `teardownArtifacts` の 4 段抽出。
- 規模: 中 / リスク: 中。

#### N2A. `Runner.ImportWorkspaceHome` (330 行)

- `internal/dispatcher/workspace_home_import.go:349-678`。lock / in-flight 登録 /
  migration record / tar peek / volume 作成 / extract / partial 破棄が直列。
- 規模: 大 / リスク: **高**。record の phase 順序 = クラッシュ耐性が正しさそのもので、
  doc コメントが挙動仕様書。分割は「コメントごと移動」に留める。急がない。

#### N3A. `Server.Start` (315 行、リスナ起動 7 ブロックコピペ)

- `internal/server/server.go:660-973` (対の `Stop` は 975-1049)。`net.Listen` →
  `http.Server` → `go Serve` → `slog.Info` が broker TLS / proxy×2 / gitgateway×2 /
  unix / tcp / cli で反復。`gatewayBindHost` 判断も 3 箇所に散在。
- 方向性: `startHTTPListener(name, network, addr, handler, tlsCfg)` + 起動済みスライスへ
  登録し `Stop` は逆順 Shutdown。停止順序の現状表を作ってから。
- 規模: 中 / リスク: 中。

#### N4A. `CreateProjectFromGitURL` (284 行、ロールバック手書き)

- `internal/api/project_service.go:446-729`。各失敗パスで `os.RemoveAll(bareRepoPath)` +
  `DeleteProject` + `Meta.Remove` を手動再掲。L4 と同型の問題の api 層版。
- リスク: 中。「bare repo を先、DB 行を後」の巻き戻し順序が過去インシデント修正で
  load-bearing。defer + committed 化でも順序表必須。

#### N5. `NotifyTask` 本体 (238 行)

- `internal/api/task_notify.go:34-271`。progress / ask / done / fail / FYI の 5 モードが
  1 メソッド。M8 のシグネチャ struct 化とは**別 PR** で分解する。
- 規模: 中 / リスク: 中 (状態遷移 + hook 発火 + runtime SIGTERM が絡む)。

#### N11A. `Config.UnmarshalYAML` (228 行) / N12B. `validateServiceConfig` (202 行)

- `internal/config/config.go:387-614`: 冒頭 30 行超の匿名 raw 構造体 (= Config の第二定義) +
  11 セクション適用が直列。`applyGC` / `applyGateway` / `applyServices` へ分割。
  gateway.hosts レガシー折り畳みは独立関数へ (将来まるごと削除できる形に)。
  リスク: config load の失敗/警告の順序保存を明示。
- `internal/config/apigateway.go:79-280`: auth.kind ごとの検証が 1 巨大 switch。
  `map[AuthKind]func(...) error` のバリデータテーブルへ。sqlite 非依存でローカル test 可。
- 規模: 各 中 / リスク: 中・低。

### 位置引数 struct 化 (取り違えが型検査を通らない系。まとめ PR 可)

- **N10A. `decodeWorkspaceMetaColumns` / `marshalWorkspaceMetaColumns` の 13 個位置引数**:
  `internal/orchestrator/workspace_repository.go:238` (引数 13) / `:431` (戻り値 13)、
  全部無名 string。エラー return の `"", "", ..., err` を 8 回コピペ。呼び出し 5 箇所は
  同一ファイル内。`workspaceMetaColumns` struct 化で 3 種の問題が同時に消える。sqlite 依存 → CI 委譲。
- **N10. `PrepareJobCheckout`**: `internal/dispatcher/checkout.go:117`、string 6 連続。
  `remoteURL` (per-job gateway token 含む秘匿値) と `baseBranchForkPoint` が隣接し、
  取り違え = token が fork point として git に渡る。本番呼び出し 1 箇所。
- **N9. `Runner.launchSandbox`**: `internal/dispatcher/runner.go:1408`、string 4 連続
  (desiredRuntimeID / workspace / workspaceSlug / workspaceHomeID)。テスト呼び出しが
  `(ctx, job, spec, nil, "", "", "", "", false)`。`launchSandboxInput{}` へ。呼び出し元 2 箇所のみ。
- **N11. apigateway の string 連発**: `internal/apigateway/registry.go:48,57` の
  `Register` / `RegisterToken` は `namespace` と `taskID` が隣接同型 (入れ替わると
  認証情報の誤解決、コンパイルは通る)。`recorder.go:15` `RequestRecorder` も string×4。
  `RegisterInput` / `RecordedRequest` struct へ。新設パッケージなので今のうちに。
- その他 (優先度低): `newBoidBuiltinExecutor` 7 引数 (boid_executor.go:71) /
  `sandboxBackendForConfig` (wire.go:459) / `brokerclient.SendJSONTLS` string 5 連続
  (brokerclient.go:93、dialTLS と二重) / `workspaceHomeInitialized` (workspace_home.go:581) /
  templ の `TaskDetail` string×3 隣接 (tasks.templ:805)。

### コピペ重複 (3 箇所以上)

- **N1A. client の raw HTTP メソッド 6 本**: `internal/client/client.go:886-1100`
  (GetRaw / GetRawWithAcceptAndRevision / PostStream / PostRaw / PutRawWithIfMatch /
  PostRawWithIfMatch)。`PutRawWithIfMatch` と `PostRawWithIfMatch` はメソッド定数以外同一。
  `doRaw(...)` へ集約。sqlite 非依存でローカル検証可。`PostStream` の chunked 特性と
  ETag 生値返却は保つこと。
- **N2B. `Do` / `DoContext` / `DoWithContentType` の三重化**: `internal/client/client.go:379-467`。
  差分は ctx 有無 / body 型 / Content-Type の 3 点のみ。`DoWithContentType` は ctx を
  取れない機能劣化も抱える。N1A と同一 PR 可。エラー文言は変えない (テストが文字列一致)。
- **N3B. `$EDITOR` テンポラリ編集ループの二重実装**: `cmd/config.go:405-471` と
  `cmd/workspace_init_script.go:443-509` が約 40 行逐語コピペ (後者の doc コメントが自認)。
  `editInTempFile(...)` へ。**差分を潰さないこと**: config 側は TrimSpace 比較、
  init-script 側はバイト完全一致比較 (ハッシュに効くため意図的)。
- **N4B. If-Match / --force / 412・428 分岐の 3 箇所並走**: `cmd/config.go:362-389` /
  `cmd/workspace.go:433-492` / `cmd/workspace_init_script.go:372-395`。conflict 判定が
  ヘルパ / 直書き / **分岐なし (workspace edit は generic に落ちる非対称)** と割れている。
  `ifMatchWrite(...)` 共通経路へ、文言はコールバック注入。規模: 中 / リスク: 中。
- **N6. docker TLS / broker TLS の 4 関数平行実装**: `internal/dispatcher/container_backend.go`
  の `materializeDockerClientCert` (2115-2144) / `materializeBrokerClientCert` (2190-2226)、
  `dockerTLSCertDir` / `brokerTLSCertDir`、`withDockerTLSEnv` / `withBrokerTLSEnv` が
  CN・validity・ディレクトリ名以外同一。3 系統目が生えたら破綻。
  `perJobCertMaterial{...}` を受ける 1 関数へ。テスト網は双方厚い。パス生成結果の pin を確認。
- **N6A. apigateway client_secret 解決 4 箇所**: `internal/apigateway/oauth2.go:608-615,
  649-652` / `login.go:404-410, 494-500`。文言までバイト同一 (1 つは「空値も拒否」変種)。
  `resolveClientSecret(namespace, cfg, required)` へ。
- **N7A. apigateway 拒否レスポンス 7 箇所**: `internal/apigateway/server.go:234-317`。
  `s.recorder(...)` + `http.Error(...)` の 2 行組。片方忘れると監査ログに穴。
  `deny(w, entry, rt, method, status, msg)` へ。新設パッケージなので今のうちに。
- **N8A. web/ project 表示名ヘルパ 6 実装**: 完全同一 3 本 (filters.templ:9 projectLabel /
  tasks.templ:180 projectOptionLabel / task_form.templ:13 taskNewProjectLabel) + 同型 3 本
  (detailProjectLabel / sessionProjectLabel / TreeItem.ProjectLabel)。
  `taskStatusClass` / `treeStatusClass` も body 完全一致。components へ 1 本化。templ 再生成必要。
- **N9A. web/ project select ブロック 3 箇所 + エラーバナー 3 クラス**:
  task_form.templ:92 / tasks.templ:827 / sessions.templ:133 の `<select>` と、
  `action-error` / `error-banner` / `login-error` の 3 クラスが同役割。
  `components.ProjectSelect` / `components.ErrorBanner` へ。CSS クラス名は据え置きが安全。
- **N8B. `runCheck` の allOK 手動伝播 8 箇所**: `cmd/check.go:90-256`。セクション追加時の
  `allOK = false` 書き忘れ = 診断が黙って通る罠。`checkSection` テーブル +
  `allOK &&= run(...)` へ。出力バイト不変を PR で明示。

### 責務混在・配置

- **N13A. `ProjectAppService` の workspace サービス兼務**: `internal/api/project_service.go`
  (2,065 行) の 1053-1858 が workspace 系メソッド。`WorkspaceAppService` が存在しない。
  `workspace_service.go` へ移設 (`s.Projects` / `s.mu` の扱いが設計判断。
  `projectMu` / `mu` の 2 ロック跨ぎメソッドがあり単純移動では済まない)。規模: 中 / リスク: 中。
- **N5B. `cmd/host.go` (820 行、cobra コマンド 0 個)**: host mode のトークン管理 /
  compose engine 検出 / flock / deploy / health ポーリングというデーモンライフサイクル基盤が
  cmd に同居。`internal/hostmode` へ移設。cmd 内非公開シンボル (effectiveBoidUID 等、
  cmd/check.go も使用) との線引きが先。付随: `resolveHostModeClient` /
  `resolveHostModeClientNoAutostart` (host.go:777-820) のコピペは移設と同時に統合。
- **N7B. `cmd/check.go` 埋め込み OCI レジストリクライアント (約 270 行)**:
  check.go:770-1040 の manifest/token/blob フェッチ群を `internal/ociref` 等へ。
  純関数群でテスト済み、移動は安全。`registryHTTPTimeout` の Background 根は維持。
- **N13. workspace home migration record ヘルパの散在 + 危険な呼び分け**:
  `workspace_home_migration_sentinel.go:402-465` (write/mark/remove) と
  `workspace_home_import.go:696-748` (discard/clearForAnAbsentHome) に「record を消す」
  関数が 3 命名で分散。**うち 2 つは経由必須の破壊事故ガード制約あり** (doc コメント明記)。
  sentinel 側へ集約 + phase を持つ record handle 型で直接呼び出しを塞ぐ。
  規模: 中 / リスク: 中 (sentinel_test.go 1,697 行の網を通す)。
- **N26. `cmd/project_migrate.go` (1,842 行) の internal/projectmigrate 化**: cobra 定義
  1 個以外は移行専用ドメインロジック (secret 再暗号化 / YAML ノード書き換え / shadow 管理)。
  **L2 手順 1 (spec_loader_legacy 移設) と受け皿が重なるため、先に受け皿設計を決める。**
  legacy 退役方針によっては migrate コマンドごと退役の可能性もあり、その場合は本項不要。
- **N9B. orchestrator workspace_migration.go の hash 3 兄弟 + 凍結ミラー**:
  `internal/orchestrator/workspace_migration.go:790-1112`。3 関数末尾の
  「json.Marshal → sha256 → hex」共通化 + 凍結ミラー構造体 (pr6/pr7、"do NOT modify") を
  `workspace_migration_legacy_hash.go` へ**移動のみ**で分離。
  **ミラー構造体はフィールド宣言順が JSON バイト互換の根拠。1 バイトも触らない**。
- **N10B. web/ templ 内インライン JS 500 行超**: 最大は settings.templ:195-513 (318 行)。
  lint/format 対象外になっている。`web/static/boid-settings.js` へ切り出し、
  `components/terminal.templ:37` の BuildID クエリ付き `<script src>` パターンに寄せる。
  templ 変数の受け渡しは `data-*` 属性経由へ。規模: 中 / リスク: 小〜中。

### 小物・ついで対応

- `ValidatePayloadPatch` (`internal/orchestrator/spec_payload.go:38-77`): 非テスト参照ゼロ。
  `MergePayloadPatch` 内に同等検証が残ることを確認してから削除。
- broker の未使用パラメータ `entry *tokenEntry` (`internal/sandbox/broker.go:764` /
  `broker_streaming_linux.go:55`): 将来の認可拡張の口の可能性あり、導入意図を git log で
  確認してから。単独 PR にする価値なし。
- デッドコード全般: 2026-08-08 の機械スキャンでは上記 2 件以外ゼロ。#912 (f3831779) の
  一括除去が効いており、この軸の新規バックログは不要。

### 見張り (今は着手しない)

- **workspace_home* 系 7 ファイル 4,990 行** (dispatcher 非テストの 27%): サブパッケージ化
  候補だが Runner メソッド依存が深く、境界がまだ動いている最中。
  「次に 1,000 行増えたら分割検討」の閾値付き見張りとする。
- **`api.Job` と `dispatcher.Job` の二重モデル** (`internal/api/job_model.go` /
  `internal/server/api_store.go:234-278` の双方向手写し変換): 層分離のための意図的二重化
  とも読める。着手前に「どちらを正とするか」の設計判断が要る。アンチゴール判定もありうる。

### 追補の推奨着手順

1. N5A (Adopt 8 箇所) — drift 事故 3 回の実績持ち。再発防止効果最大
2. N12A + N14A (宣言移動のみの分離 2 件) — リスク極小のウォームアップ
3. 位置引数 struct 化まとめ PR (N10A / N10 / N9 / N11 + L1 の projectVisibilityMounts)
4. M6 + N1 (wire.go 分割計画) — 2,258 行で限界近い
5. L1 / L3 / L4+N7 / N2+M2 は独立プラン化してから

---

## 見送り (アンチゴール)

- **dockerproxy/policy.go**: 網羅的な allow/deny テーブルで重複は少ない。触らない。
- **workspace_store / project_catalog / kit_registry の CRUD 共通化**: バックエンドが
  YAML / SQL / FS で異なる。見た目の類似で抽象を作らない (L2 の項参照)。
- **gofmt 差分**: ファイル末尾空行などバージョン揺れ由来。品質ゲート計画で
  「宣言つき見送り」済み。個別 PR で直さない。
- **gitgateway / apigateway の並行構造の共通化**: `token.go` に「leaf package に保つため
  意図的に複製」と明記あり。Entry の中身も別ドメイン。共通化しない (2026-08-08 追記)。
- **cmd/ の cobra コマンド定義テーブル化**: 宣言的スタイルの範囲内。テーブル化しても
  可読性が下がるだけ (2026-08-08 追記)。
- **adapters 3 種 (claude/codex/opencode) の bindings 共通抽象**: 各 43-55 行で中身は
  harness 固有。共通化しない (2026-08-08 追記)。
- **`cmd/project.go` `runProjectInit` (282 行)**: 実コードは約 70 行で残りは設計判断
  コメント。分割の価値が低く、コメント削除は情報損失。触らない (2026-08-08 追記)。

## 着手時の共通注意

- sqlite 依存パッケージ (api / server / orchestrator / dispatcher 等) は sandbox 内で
  go build/test 不可。ローカル検証は sqlite-free な internal/sandbox / internal/client /
  internal/config のみで、残りは CI 委譲。
- リファクタ PR は「挙動不変」を明示し、/boid-review (wiring レンズ + claim 検証 +
  test-sync) を通す。equivalent 系の claim には diff 内の根拠を付ける。
- 大物 (L1-L4、追補 N1/N2 等) は 1 PR に詰めず、移動のみ / 抽出のみの小 PR 系列に割ること。
- web/ の templ を触る PR は repo ルートからの `templ generate` 再生成を忘れない
  (メモリ `templ-generate-from-repo-root`)。
