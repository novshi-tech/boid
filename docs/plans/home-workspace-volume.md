# $HOME workspace volume 契約先行 実装計画

ステータス: **計画 (planned、着手待ち)** — Phase 1-3 (+ Phase 2.5) 完了により依存前提が全部揃った状態
作成日: 2026-07-09
更新日: 2026-07-17 (refresh: Phase 2.5 完了差分メモ + 規模見積もり + 行番号は関数名アンカーへ切替)
親ドキュメント: [container-based-boid.md](container-based-boid.md) — 移行戦略ステップ 4

---

## 目的

現行 userns backend の「/home tmpfs + kit additional_binding + adapter の選択的 bind」を、
**ホスト側 per-workspace ディレクトリを $HOME に rw bind mount する方式**に置換する。
コンテナ版 $HOME workspace volume と同じ契約 (workspace 永続・初期化スクリプト・
ホスト $HOME 非共有) を enforcement 差し替え前に dogfood する。

これで同時に解決するもの:

- go / npm / nuget 等のパッケージキャッシュ非永続 (毎 dispatch 全再取得)
- ハーネス認証状態のホスト $HOME 共有 (`~/.claude.json` 単一ファイル bind の
  rename 原子書き換え問題込み)
- session jsonl の workspace 単位永続化
- kit additional_binding 機構の退役 (2 段配線の削除)

スコープ外: context ファイルの RPC 化 (ステップ 5)、コンテナ enforcement (ステップ 6)。

---

## 決定事項 (2026-07-09、親ドキュメント反映済み)

1. **init 実行モデルはハイブリッド**: 非対話 init script は初回 dispatch 時に自動実行
   (flock 直列化 + バージョン付き完了マーカー)。対話ログイン (claude login 等) は
   専用コマンドを作らず通常の `boid agent claude` セッション内で行う —
   セッションが workspace home に書くので認証はそのまま永続する
2. **`~/.boid` は job スコープ tmpfs を重ねる**: ステップ 5 (context RPC 化) 完了までの
   衝突対策。workspace home bind の上に `$HOME/.boid` だけ tmpfs を重ね、
   現行 context ファイル契約を保つ
3. **quota / GC はサイズ可視化のみ**: 自動 prune なし。workspace 削除 = home 削除
4. **embedded skills は bind 廃止・コピー配布**: コンテナ版で $HOME が volume に
   なるとイメージ焼き込みが mount に隠されるため。drift 防止のため boid が
   dispatch 時にバージョンチェック付き sync (init script の仕事にしない)

---

## Phase 2.5 完了差分メモ (2026-07-17 refresh)

本計画は 2026-07-09 作成時点のコード構造を前提にしているが、その後
Phase 2.5 (workspace DB 一元化・kit 機構退役、
[workspace-db-consolidation.md](workspace-db-consolidation.md)) が 2026-07-17 に
完結し、いくつかの前提が動いた。要点だけ差し引くと:

- **kit 機構は既に退役済み** — `internal/kit/` パッケージ / `KitRegistry` /
  `MergeKitRuntime` / `MergeKitMetaIntoBehavior` / `boid-sandbox-configure`
  スキル / kit init コマンド / `WorkspaceMeta.Kits` フィールド は全部
  Phase 2.5 で消えた。本計画の PR4「kit additional_binding の退役」は
  実務が **フィールド (`AdditionalBindings`) と 2 段配線 (dispatch merge)
  の撤去** に純化される (kit テンプレート更新・kit-authoring docs 追随・
  `boid-sandbox-configure` の追随といった仕事は自然消滅)
- **workspace は DB 実体化 + API 経路化済み** — `~/.config/boid/workspaces/`
  yaml 直読みは Phase 2.5 で撤去され、Web UI / CLI どちらも API 経由で
  workspace を触るようになった。init.sh の置き場は本計画で
  `~/.config/boid/workspaces/<slug>/init.sh` としているが、**workspace
  リソースの一部として DB (または DB リンク付きの blob) に載せる方が
  Phase 2.5 の思想に整合する** (workspace 単位の環境非依存な設定として)。
  PR1 設計時に検討する
- **PR3 dogfood チェックリストは今も生きている** — CLI バイナリ bind が
  消える瞬間、生きている workspace の init.sh が未整備だとセッションが
  起動しなくなる問題は不変。マージ前準備の項として重要度そのまま
- **`AdditionalBindings` フィールドの Phase 4 持ち越しは公式** —
  workspace-db-consolidation.md 冒頭に「PR8 で撤去予定だったものを
  Phase 4 に持ち越し」と明記済み。本計画 PR4 でそのまま引き取る

---

## 契約 (初期化スクリプトの語彙)

**boid が保証すること**:

- workspace home ディレクトリの存在 (dispatch 前に ensure)
- init script の実行タイミング: 初回 dispatch 時、および script 内容が変わった時
  (完了マーカーに script の hash を含める)
- 同時 dispatch でも実行は 1 回 (flock による直列化。待つ側は完了を待って続行)
- 実行環境: ホスト側 (trusted)・`HOME=<workspace home>` を設定した状態で実行。
  `BOID_WORKSPACE_SLUG` / `BOID_WORKSPACE_HOME` を env で渡す
- init 失敗時は dispatch を明示エラーで fail (黙って初期化無しで走らせない)

**script 作者が守ること**:

- 冪等であること (マーカー破損・script 更新での再実行に耐える)
- 中身は自由: ツールチェーン設置 (claude CLI / go / volta ...)、設定ファイル配置等。
  boid は中身に関知しない

**置き場 (trusted / sandbox 到達性)**:

- init script: `~/.config/boid/workspaces/<slug>/init.sh`
  (workspace.yaml と同じ config 側。sandbox からは不可視・不可書)
- script が無い workspace は「初期化不要」として素通し (マーカーだけ打つ)
- 完了マーカー・lock: `~/.local/share/boid/homes/<slug>.init.json` / `.lock`
  (**home ディレクトリの外**に置く。$HOME 内に置くと sandbox が改竄できてしまう)

---

## レイアウト

```
~/.local/share/boid/
  homes/<slug>/            # workspace home 実体 (sandbox の $HOME に rw bind)
  homes/<slug>.init.json   # 完了マーカー (script hash / boid version / 時刻)
  homes/<slug>.lock        # init 直列化用 flock
  skills/<name>/           # embedded skills の正本 (既存)
  runtimes/ worktrees/ kits/ ...  # 既存
```

- slug 検証は `ValidWorkspaceSlug` (`internal/orchestrator/workspace_slug.go:15`) を流用
- `WorkspaceID == ""` (workspace 未割当 project) は `default` に正規化して
  default workspace の home を使う (`runner.go:367-376` が "" を返すケース)
- `homes/` は runtimes/ と違い **GC 対象外** (workspace 永続)。
  掃除は workspace remove 連動のみ

---

## PR 分割

**行番号アンカーについて (2026-07-17 refresh)**: 本節の `runner.go:189-278`
などの行番号は 2026-07-09 作成時点の値。Phase 2.5 で該当ファイルは大きく動いた
ため、実装時は**関数名アンカー**で追うこと (対応表は各 PR の頭に注記した)。

### PR1: workspace home 管理の新設 (ensure + init 実行機構)

- 新パッケージ or `internal/dispatcher` 配下に workspace home resolver:
  ensure (mkdir) + init 判定 (マーカー読み比べ) + flock 実行 + マーカー書き込み
- **フック位置**: `Runner.Dispatch` (`internal/dispatcher/runner.go`) の
  workspaceID 解決済み区間 — 2026-07-17 現在では `resolveProjectRuntime` の
  直後 (`Runner.resolveWorkspaceProxy` を呼ぶすぐ手前) が同列位置。
  `resolveWorkspaceHome(workspaceID)` を新設して並べる
- **`SandboxRuntimeInfo`** (`internal/dispatcher/sandbox_builder.go` の型宣言。
  2026-07-17 現在は `CloneWorkspaceDir` 等が最後尾) に `WorkspaceHomeDir string`
  を追加。`Runner.Dispatch` の `rtInfo := SandboxRuntimeInfo{...}` 構築箇所も
  合わせて足す
- init script の読み込みは workspace リソース側から (Phase 2.5 完了差分メモ
  参照: DB 実体化との整合を PR1 設計時に検討。`WorkspaceLookup` interface が
  既存 seam)
- この PR では**まだ mount には使わない** (配線だけ。挙動不変)
- テスト: init の直列化 (並行 ensure で 1 回)・script hash 変更で再実行・
  失敗時エラー・script 無し workspace の素通し

### PR2: sandbox builder の HOME 差し替え + `~/.boid` job tmpfs

挙動切替の中心。`internal/dispatcher/sandbox_builder.go` の HOME tmpfs は
2026-07-17 現在**3 分岐+ 1 (projectVisibilityMounts 内) = 実質 4 箇所**に散らばる。
Phase 2.5 後の git gateway cutover で clone 分岐が独立して増えた:

| 分岐 | 現在位置 (2026-07-17) | Phase 4 での扱い |
|---|---|---|
| Clone 分岐 (`spec.Visibility.Clone != nil`) | `BuildSandboxSpec` 内、Clone 判定直下の `sandbox.Mount{Target: homeDir, Type: MountTmpfs}` | 差し替え対象 |
| projectVisible 分岐 | `projectVisibilityMounts()` の `2) tmpfs HOME on top of user's home` | 差し替え対象 (step 3 の project 再 bind は残す) |
| ProfileInit 分岐 | `BuildSandboxSpec` 内、`sandbox.ProfileInit` 判定下の `$HOME/.boid` だけ tmpfs | **変更なし** (ホスト scan 用途で HOME を隠さない) |
| default 分岐 (no project) | `BuildSandboxSpec` 末尾の `default: HOME に tmpfs` | 差し替え対象 |

作業内容:

- 差し替え対象 3 箇所で「HOME に tmpfs」→「workspace home dir を HOME に
  rw bind」。直後に **`homeDir + "/.boid"` に tmpfs を重ねる** mount を追加
  (context/output の job スコープ維持)
- runner 側は既存 `MountBind` 経路で足りる見込み。`runner_linux.go` の
  `MountTmpfs` 実装は `.boid` overlay 用に残る
- uid 所有権: rw bind の inner uid 0 ↔ host uid 1000 は既存 kit rw bind
  (GOMODCACHE 等) で実証済みのため新規リスクなし
- environment.yaml の filesystem 記述・notes (`sandbox_builder.go` の
  `buildEnvironmentYAML` / `convertBindings` 系) を新方式に合わせ更新
- テスト: `sandbox_builder_test.go` の wantMounts 更新 (mount 列テスト書き換え)。
  同一 workspace の 2 job で $HOME ファイルが持続する / `~/.boid` は独立する
  e2e はここか PR6 で

### PR3: embedded skills sync + adapter bindings 退役

- skills sync: dispatch 時 (PR1 の home ensure と同じフック) に
  `~/.local/share/boid/skills/<name>` → `<wsHome>/.claude/skills/<name>` を
  バージョンチェック付きコピー (temp dir + rename で原子的に。最新なら no-op)。
  既存 `skills.DeployAll` (`internal/skills/deploy.go`) の隣に sync を実装
- adapter bindings の退役 (`internal/adapters/*/bindings.go`。2026-07-17
  現在の実体は以下):
  - **claude** (`internal/adapters/claude/bindings.go`): `~/.local/bin` ro +
    `~/.local/share/claude` ro + `~/.claude` rw + **`~/.claude.json` 単一
    ファイル rw bind** + embedded skills bind → 全廃
  - **codex** (`internal/adapters/codex/bindings.go`): `~/.codex` rw +
    PATH の `codex` 親 dir + `~/.volta` + embedded skills → 全廃
  - **opencode** (`internal/adapters/opencode/bindings.go`): `~/.opencode` rw
    + `~/.config/opencode` rw + `~/.local/share/opencode` rw +
    `~/.local/state/opencode` rw + PATH の `opencode` 親 dir + embedded skills
    + **ホスト `~/.claude/skills/*` 個別 bind ループ** → 全廃
  - CLI バイナリ系 (`~/.local/bin`・バイナリ親 dir) も退役 —
    ツールチェーンは workspace home 側に住む (親ドキュメントの明示的例外)。
    **init script が CLI を設置するまでセッション不能になるため、
    「コマンドが見つからない」ではなく init 未整備を指す fail-fast
    エラーメッセージを出す** (dogfood チェックリスト参照)
  - `Bindings()` interface は残し、$HOME 非依存の bind が将来要る時のために
    空実装を返す
- PATH: `buildPATH()` (`sandbox_builder.go`) を workspace home 基準
  (`$HOME/.local/bin` 等) に変更。追加 PATH は既存 `WorkspaceMeta.Env`
  で workspace 作者が足せる
- env strip 系 (`CLAUDE_CODE_CHILD_SESSION` strip / `FORCE_SESSION_PERSISTENCE` /
  `IS_SANDBOX`、`internal/adapters/claude/run.go`) は**不変**
- テスト: sync の冪等性・バージョン更新時の差し替え・並行 dispatch 安全性。
  各 adapter の `bindings_test.go` は退役 or 空実装検証に整理

### PR4: `AdditionalBindings` フィールド撤去 (Phase 2.5 で kit 撤去済みの残務)

**Phase 2.5 完了差分メモ参照**: kit 機構本体 (`internal/kit/` /
`KitRegistry` / `MergeKitRuntime` / kit テンプレート / kit-authoring docs /
`boid-sandbox-configure`) は既に Phase 2.5 で消えている。本 PR は
「workspace レベルにだけ生き残った `AdditionalBindings` フィールド (kit 遺物)
の撤去」に純化される。

2026-07-17 現在の残存箇所 (grep 済み):

- **フィールド定義**: `internal/orchestrator/workspace_meta.go` の
  `WorkspaceMeta.AdditionalBindings` (「Phase 4 で撤去予定」と doc 注記済み)
- **strict parse**: `internal/orchestrator/workspace_meta_strict.go` の
  `bindMountStrict` 変換
- **expand (interpolation)**: `internal/orchestrator/workspace_meta.go` の
  `cloneBindMounts` / `interpolateBindMounts`
- **dispatch merge**: `internal/orchestrator/project_store.go` の
  `if len(ws.AdditionalBindings) > 0 { ... }` ブロック (`out.AdditionalBindings`
  + 全 TaskBehavior へマージ)
- **planner**: `internal/orchestrator/planner.go` の
  `AdditionalBindings: behavior.AdditionalBindings` 引き回し
- **legacy loader**: `internal/orchestrator/spec_loader_legacy.go` (2 箇所)
- **sandbox_builder 側の消費**: `additionalBindingMounts(expandedBindings)`
  + `KitRoots` ループ (shell adapter legacy 経路)。project レベルの
  `Visibility.AdditionalBindings` は project.yaml 経路で残るので、workspace
  由来分の flow だけを断つ形が正解
- **strict テスト等**: `workspace_kits_materialize_test.go`
  (kit-materialized 経路のピン、既に dead code なので機構ごと退役)

作業:

- yaml スキーマは parse 継続 + 無視 + 警告ログ (既存 workspace 設定を
  即壊さない)。次のメジャー整理で完全削除
- `KitRoots` (shell adapter legacy 経路) も同時に退役検討 —
  Phase 2.5 で kit 機構自体消えたので存在意義が形式化している
- テスト: `workspace_kits_materialize_test.go` 全廃、`project_store_hydrate_test.go`
  の BLOCKER 2 系ピン削除、`sandbox_builder_test.go` の wantMounts 整理

### PR5: サイズ可視化 + workspace remove 連動

- `boid workspace show <slug>` と `boid gc` の出力に workspace home サイズを追加
  (du 相当。自動 prune はしない)
- `boid workspace remove <slug>` で home dir も削除 (確認プロンプト付き)。
  default workspace の home は保護
- workspace.yaml が消えて home だけ残った孤児はレポートのみ (削除しない)

### PR6: e2e + docs

- e2e シナリオ: (1) 同一 workspace の連続 2 job で $HOME 書き込みが持続、
  (2) 並行 job で `~/.boid` が混ざらない、(3) init script が 1 回だけ走る +
  script 変更で再実行、(4) init 失敗で dispatch が明示エラー
- docs: config-yaml.md (該当あれば)・web-ui 系ではなく guide 側に
  workspace セットアップ手順 (init.sh の書き方 + `boid agent claude` での初回ログイン)

依存関係: PR1 → PR2 → PR3 → PR4 の順が本線。PR5 は PR1 以降いつでも。
PR6 は PR2 以降段階的に。各 PR は単体 revert 可能で、PR2 が挙動切替の中心。

---

## dogfood チェックリスト (PR3 マージ前にホスト側で準備)

PR3 で CLI バイナリ bind が消えるため、**生きている workspace の init.sh を
先に用意しないとセッションが起動しなくなる**:

1. 各 workspace の `~/.config/boid/workspaces/<slug>/init.sh` を作成
   (最低限: claude CLI のインストール。必要に応じ go / volta / codex / opencode)
2. PR2 マージ後・PR3 マージ前に各 workspace で `boid agent claude` を 1 回起動し
   セッション内でログイン (認証が workspace home に永続することを確認)
3. ホスト `~/.claude.json` のコピーはしない (決定事項。まっさらからログイン)

---

## 落とし穴・注意 (下調べからの引き継ぎ)

- $HOME のパスは「ホスト実 home パスに被せる」方式 (`hostHomeDir()` in
  `internal/dispatcher/home.go`)。bind に変えてもパス自体は変えない
  (sandbox 内外のパス一致は維持)
- shell adapter は `Bindings()` 未定義で kit 経路に乗っている
  (`BuildSandboxSpec` 内の adapter registry lookup + `expandedBindings`
  fallback、PR #594 退行の真因)。PR3/PR4 で shell 経路の退行テストを必ず踏む
- opencode の「ホスト `~/.claude/skills` 個別 bind ループ」は tmpfs 前提の
  回避策なので PR3 で機構ごと消える。opencode がスキルを見つける経路が
  sync 後の実ディレクトリで機能するか実機確認
- session jsonl が workspace home に移ることで、診断時の参照先が
  `~/.claude/projects/...` (ホスト) から `homes/<slug>/.claude/projects/...` に変わる。
  診断手順のメモ更新
- 並行 job の同一 workspace home RW はローカル (単一ホスト・通常 FS) では
  許容と決定済み。k8s の RWX 問題はステップ 6 以降の論点のまま

---

## 規模見積もり (2026-07-17 refresh)

行数は差分の粗い見積もり (削除多め PR はマイナスで)。日数は 1 人・レビュー
待ち含めない実装ゾロ目。

| PR | 内容 | 見積もり (差分 loc) | 日数 | リスク |
|---|---|---|---|---|
| PR1 | workspace home resolver 新設 + rtInfo 配線 | +700〜900 | 2〜3 日 | 低 (挙動不変) |
| PR2 | sandbox builder HOME 差し替え + `.boid` tmpfs 重ね | +200〜400 | 1〜2 日 | **中** (挙動切替の中心。builder unit test 大改) |
| PR3 | embedded skills sync + adapter bindings 退役 | -260 削除 +200 追加 = 実質 -60 (net 減) | 3 日 | **高** (init.sh 未整備で全 workspace 停止しうる。dogfood チェックリスト必須) |
| PR4 | `AdditionalBindings` フィールド撤去 | -300〜500 (削除中心) | 1〜2 日 | 低 (dead code 撤去) |
| PR5 | サイズ可視化 + workspace remove 連動 | +250〜400 | 1 日 | 低 |
| PR6 | e2e + docs | +300〜500 | 1〜2 日 | e2e 依存で時間読みにくい |

**トータル**: 6 PR、実装ゾロ目で **8〜14 日** (レビュー・修正込みで 2〜3 週間規模)。
Phase 2.5 (PR1-PR7、実質 1 週間強) より一回り重い、Phase 3 (CLI リモート、
PR1-PR5) と同程度の規模感。

**クリティカルパス**: PR1 → PR2 → PR3 が本線 (直列必須)。PR4 は PR3 と並行可
(依存無し)、PR5 は PR1 以降いつでも、PR6 は PR2 以降段階的。

**リスク集中は PR3**: adapter bindings 退役の瞬間、init.sh が空だと
セッションが起動不能になる。**PR3 マージ前に**:

1. dogfood チェックリスト (本 doc 内) を実行
2. workspace 単位の init.sh の書き方を guide 化 (最低限 claude CLI インストール、
   必要に応じ go / volta / codex / opencode)
3. PR2 マージ後・PR3 マージ前の窓で全 workspace の初回ログインを済ませる

**着手順の推奨**: 依存関係と規模を踏まえ、以下の順で走らせるのが自然:

1. PR1 (2-3 日、単独 landable、挙動不変なので待たずマージ可)
2. PR2 + PR6-a (e2e 一部) をペアで (2 日)
3. **窓を確保して dogfood** (init.sh 準備 + 初回ログイン)
4. PR3 + PR6-b (2-3 日)
5. PR4 + PR5 を並行 (2 日) — PR4 は独立に landable、PR5 も独立
6. PR6-c (残 e2e + docs 仕上げ、1 日)
