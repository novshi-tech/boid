# リファクタリング バックログ

2026-07-05 のコードベース棚卸し (dispatcher / sandbox / api・server・cmd / orchestrator の
4 方面並列調査 + 主要指摘の grep 裏取り) で出たリファクタリング候補のうち、
**自律タスクに委譲しなかった中〜大物**を将来の着手用に記録するメモ。

行番号は 2026-07-05 時点の main のもの。下記「委譲済みバッチ」が先に main へ入ると
ずれるため、**着手時に必ず再検証すること**。指摘の大半はサブエージェント調査由来で、
見出しレベルの事実 (デッドコード・重複箇所数など) は手元 grep で確認済みだが、
細部の行範囲まで全数検証はしていない。

## 委譲済みバッチ (このメモのスコープ外)

低リスク 5 件は supervisor タスク `refactor-deadcode-boilerplate-batch`
(ce3eb3d2, 2026-07-05 起票) で PR 化が進行中。重複着手しないこと:

1. `remove-depends-on-remnants` — migration 0026 で廃止済み depends_on 機構の API 層残骸撤去
2. `api-web-boilerplate-helpers` — web.go の redirect / エラー描画 / JobView 変換の集約
3. `dispatcher-small-cleanups` — DockerEnabled デッドフィールド・redockey リネーム・PRAGMA N+1
4. `sandbox-git-dead-path` — ExecRequest.Git 死に経路撤去・classify 系の配置修正
5. `orchestrator-notfound-sentinel` — not-found 文字列マッチのセンチネル化

---

## 大物 (独立プラン級。着手時は設計から)

### L1. `BuildSandboxSpec` の分割

`internal/dispatcher/sandbox_builder.go:91-447` (約 356 行の単一関数)。
env 組み立て / adapter binding 解決 / broker・server・docker proxy socket マウント /
project・worktree・peer・.boid マウントレイアウト / attachments / worktree モードの
argv 再マッピング / context ファイル生成 / stdin・stdout ルーティング /
boid バイナリ + git shim マウント / harness 解決、が全部同居している。

- 方向性: `buildSandboxEnv()` / `buildSandboxMounts()` / `buildContextFiles()` へ分割し、
  `BuildSandboxSpec` は調停役に絞る。マウント系は既存の `projectVisibilityMounts` と
  同じ粒度の副関数群にする。
- 追い風: `sandbox_builder_test.go` が約 1,750 行あり回帰網は厚い。
  Tier 1 #3 (PR #698) の入口別 builder unit テストも効く。
- リスク: binding マージは配線退行の常習犯 (品質ゲート計画の Tier 1 #1 参照)。
  分割は「移動のみ・挙動不変」を PR 単位で守り、/boid-review の wiring レンズを通すこと。
- 同時に拾う小物: `projectVisibilityMounts` (sandbox_builder.go:464-469) の
  string×3 + bool×2 + map 位置引数を struct 化。

### L2. orchestrator の god パッケージ分割

`internal/orchestrator` は非テスト 44 ファイルが 1 パッケージに同居。境界は既に明確:

- spec 読み込み/パース系: spec_loader / spec_loader_legacy / spec_types / spec_payload /
  spec_resolve / behavior_resolve / payload_merge / awaiting_payload / conditions / base_branch_classify
- DB ストア系: store / project_catalog / repository / project_lock / gc_loop / gc_sandbox_tmp / db_adapters
- ステートマシン系: coordinator / machine / planner / evaluator / lifecycle / policy / jobspec
- workspace・kit メタデータ系: workspace_store / workspace_meta / workspace_slug /
  kit_registry / kit_name / project_store

方向性 (段階的に):

1. **最初の一歩 (低リスク)**: `spec_loader_legacy.go` (144 行) の全シンボルの実利用者は
   `cmd/project_migrate.go` のみ (grep 確認済み)。migrate 用サブパッケージへ移設して
   orchestrator の公開 API から legacy スキーマ知識を排除する。
2. 次に相互依存の少ない workspace/kit メタ系を切り出す。
3. 本丸の spec 系 / store 系 / ステートマシン系の 3 分割は、公開シンボルの
   使用箇所が server / api / dispatcher に広く波及するため独立 PR 系列で。

- 同時に解消したい命名混乱: `project_store.go` (インメモリ ProjectMeta キャッシュ) と
  `project_catalog.go` (Project 行の DB CRUD) は名前が紛らわしく別ドメイン。
  分割時に前者を workspace メタ系、後者をストア系へ振り分ける。
- 関連: `repository.go` (約 400 行) はほぼ全メソッドが store.go / project_catalog.go の
  パッケージ関数へ 1 行委譲するだけの二重 API。分割の際にフリー関数とレシーバ型の
  どちらかへ一本化する (DI 都合ならフリー関数を非公開化して Repository へ集約)。
- アンチゴール: workspace_store (YAML) / project_catalog (SQL) / kit_registry (FS 走査) は
  永続化バックエンドが異なる。CRUD の見た目が似ていても共通抽象を作らないこと。
- 制約: sqlite 依存で sandbox 内 build/test 不可のパッケージ群 (メモリ
  `sandbox-cannot-build-sqlite-packages` 参照)。import パス張り替えの検証は CI 委譲になる。

### L3. `workflow_action.go` の巨大メソッド分解

`internal/api/workflow_action.go`:

- `ApplyAction` (25-153, 約 130 行): state machine 適用・reopen ペイロード特殊処理 (69-95)・
  Tx・ロック解放・Hub broadcast・dispatch loop spawn・hook 評価 (137-146) が 1 メソッドに凝縮。
- `runDispatchLoop` (155-298, 約 145 行): ロック取得・dispatch cycle・ペイロード永続化・
  awaiting/terminal 検出・auto-advance が混在。

方向性: `ApplyAction` から `applyReopenInstruction(...)` / `evaluateMatchedHooks(...)` を抽出、
`runDispatchLoop` は cycle 内の永続化ブロック (209-237) を `persistHookPayload(...)` へ。

- **リスク最高**: 並行性・ロック・trait 消去のタイミングが正しさに直結。
  テスト整備 (coordinator 系テストの api 層版) とセットで着手すること。
- 前提: 委譲済みバッチの depends_on 撤去が先に入ると
  `triggerDependentTasks` 一式が消えて対象が軽くなる。マージ後に着手するのが得。

### L4. runner のエラーパス後始末を cleanup スタック化

`internal/dispatcher/runner.go` の `Dispatch` (118-318) と `launchSandbox` (687-765) に
`if cleanup != nil { cleanup() }` が 12 箇所コピペされ、`stopDockerProxy` /
`cleanupSandboxArtifacts` も各エラーパスで手動再掲。早期 return を足すたびに
後始末の追加漏れ = リソースリーク (broker token / docker proxy / job row) の罠になる。

方向性: defer ベースの「成功したら無効化するクリーンアップスタック」
(`committed = true` で skip する定石) へ集約。

- リスク: 巻き戻し順序が分岐ごとに微妙に異なる箇所があり、機械的置換では済まない。
  過去の bind mount 削除事故 (メモリ `stale-bind-mount-deletion-incident`) の系譜で、
  cleanup の実行タイミング変更はサンドボックス安全性に直結する。挙動表を作ってから着手。

---

## 中物 (PR 1〜2 本サイズ。すきま時間で拾える)

### M1. `boid_shim.go` のフラグパースをテーブル駆動化

`internal/sandbox/boid_shim.go` の parse 関数約 10 個 (60-580 行あたり) が同一の
for+switch 手書きパターンを反復、200 行超の重複。`-f`/`--file`, `-m`/`--message` の
flagName 選択も 4 箇所コピペ。`{names, dest}` のテーブル駆動パーサへ。
まず flagName 選択を `takeStringFlagValue` 内へ吸収するだけでも効く。

### M2. `handleBoidBuiltin` の分解

`internal/sandbox/broker.go:284-479` (約 195 行)。16 op 分の必須フィールド検証・
project 名前解決・task import のバッチ処理 (421-472) が 1 つの switch に同居。
op ごとの前処理をバリデータテーブルへ、少なくとも import ブロックを
`validateAndResolveImport(...)` へ抽出。
注意: builtin op を触る場合は policy_test.go の wantOps + dispatcher drift test の
更新が必要 (メモリ `sandbox-cannot-build-sqlite-packages` の末尾)。

### ~~M3. git builtin フラグ集合の二重管理解消~~ (obsoleted by git gateway cutover PR8-A)

`internal/sandbox/git_builtin.go` / `git_shim.go` は git gateway cutover PR8-A で
削除済み (2026-07-14)。対象コードが存在しないため本項は obsolete。

### ~~M4. worktree_manager の git exec ヘルパ集約~~ (obsoleted by git gateway cutover PR8-B)

`internal/dispatcher/worktree_manager.go` は git gateway cutover PR8-B で
削除済み (2026-07-14)。host worktree 割当機構自体が廃止 (sandbox 内 clone に置換)
されたため本項は obsolete。

### M5. `cmd/kit.go` と `cmd/workspace.go` の sandbox 起動シーケンス共通化

`runKitInit` (kit.go:67-200) と `runWorkspaceConfigure` (workspace.go:343-524) が
「DefaultHarness 解決 → skills.DeployAll → BuildInitJobSpec → BuildSandboxSpec →
PrepareSandbox → runner-outer fork+wait → secret scan」の長い手順を共有。
exec ラッパ変数 (`kitInitExecFn` / `workspaceConfigureExecFn`) はバイト単位で同一。
共通部だけ `launchInitSandbox(input, execFn)` へ。workspace 側の差分
(daemon 必須・workspace.yaml backup/restore・`BOID_WORKSPACE_PROJECTS` 注入) は
呼び出し側に残すのが安全。

### M6. `wire.go` `mountRoutes` の分割

`internal/server/wire.go:589-776` (約 190 行)。インライン health/shutdown/proxy/broker
ハンドラ・10 超の r.Mount・GC ループ構築・認証ミドルウェア + Web ルート group・
TCP ラッパ生成が 1 関数に同居。`mountInfoRoutes` / `mountAPIHandlers` / `mountWebRoutes`
へ分割。単純 JSON 系はテーブル登録化。chi のミドルウェア登録順序と group 境界に注意。

### M7. cmd/ 層の細かい定型句

- `client.NewUnixClient(client.DefaultSocketPath())` が cmd/task.go・project.go・
  workspace.go のほぼ全 RunE (30 箇所超) で反復 → `defaultClient()` ヘルパ。
- list/show/remove の「ref 解決 → Do → renderOutput」パターンが 3 ファイルで並走。
  薄いジェネリックヘルパで縮む。
- internal/api/task.go: `Decode → writeError → service → writeJSON/204` の定型に
  `decodeJSON(w, r, &req) bool` ヘルパ。

### M8. orchestrator の関数レベル整理 (L2 と独立に先行可)

- `ProjectStore.GetWithWorkspace` (project_store.go:90-202, 約 112 行):
  SecretNamespace 注入・degraded 分岐・workspace kit/env マージが混在。
  `injectWorkspaceKits` / `injectWorkspaceEnv` へ抽出、
  stripAliasMirrors→ループ→addAliasMirrors の重複イディオムを `mapBehaviors(out, fn)` で共通化。
- `ReadProjectMeta` (spec_loader.go:79-163): removed-field 拒否・deprecation 警告・
  interpolate 3 種・hook スクリプトパス解決・alias 正規化が混在。
  hook 解決ループと補間ブロックを別関数へ。
- `GCTasks` (store.go:291-379): table→GCResult フィールド振り分けの switch が 3 箇所反復。
  `table 名 → *int64` の対応表 1 つに統一。
- `NotifyTask(ctx, taskID, message, ask, questionID, progress, done, fail string)` の
  string 8 連発シグネチャが internal/api に 2 箇所 (+fake)。パラメータ struct 化。

---

## 見送り (アンチゴール)

- **dockerproxy/policy.go**: 網羅的な allow/deny テーブルで重複は少ない。触らない。
- **workspace_store / project_catalog / kit_registry の CRUD 共通化**: バックエンドが
  YAML / SQL / FS で異なる。見た目の類似で抽象を作らない (L2 の項参照)。
- **gofmt 差分**: ファイル末尾空行などバージョン揺れ由来。品質ゲート計画で
  「宣言つき見送り」済み。個別 PR で直さない。

## 着手時の共通注意

- sqlite 依存パッケージ (api / server / orchestrator / dispatcher 等) は sandbox 内で
  go build/test 不可。ローカル検証は sqlite-free な internal/sandbox のみで、残りは CI 委譲。
- リファクタ PR は「挙動不変」を明示し、/boid-review (wiring レンズ + claim 検証 +
  test-sync) を通す。equivalent 系の claim には diff 内の根拠を付ける。
- 大物 (L1-L4) は 1 PR に詰めず、移動のみ / 抽出のみの小 PR 系列に割ること。
