# $HOME workspace volume 契約先行 実装計画

ステータス: 計画 (planned)
作成日: 2026-07-09
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

### PR1: workspace home 管理の新設 (ensure + init 実行機構)

- 新パッケージ or `internal/dispatcher` 配下に workspace home resolver:
  ensure (mkdir) + init 判定 (マーカー読み比べ) + flock 実行 + マーカー書き込み
- フック位置: `internal/dispatcher/runner.go:189-278` の workspaceID 解決済み区間
  (`resolveWorkspaceProxy` と同列に `resolveWorkspaceHome` を足す)
- `SandboxRuntimeInfo` (`internal/dispatcher/sandbox_builder.go:22`) に
  `WorkspaceHomeDir` を追加
- init script の読み込みは `WorkspaceStore` (`~/.config/boid/workspaces/`) 側から。
  `WorkspaceLookup` interface (`runner.go:44`) が既存 seam
- この PR では**まだ mount には使わない** (配線だけ。挙動不変)
- テスト: init の直列化 (並行 ensure で 1 回)・script hash 変更で再実行・
  失敗時エラー・script 無し workspace の素通し

### PR2: sandbox builder の HOME 差し替え + `~/.boid` job tmpfs

挙動切替の中心。`internal/dispatcher/sandbox_builder.go` の HOME tmpfs 3 分岐のうち
2 つを差し替える:

- `projectVisibilityMounts()` step 2 (`sandbox_builder.go:465-469`):
  「homeDir に tmpfs」→「workspace home dir を homeDir に rw bind」。
  直後に **`homeDir + "/.boid"` に tmpfs を重ねる** mount を追加
  (context/output の job スコープ維持)。step 3 の project 再 bind は従来どおり
- project 不可視分岐 (`sandbox_builder.go:216-220`): 同様に差し替え
- **ProfileInit 分岐 (`sandbox_builder.go:193-214`) は変更しない**
  (kit init / workspace configure はホストをスキャンする用途で HOME を隠さない)
- runner 側は既存 `MountBind` 経路で足りる見込み (`runner_linux.go:350` の
  MountTmpfs は `.boid` overlay 用に残る)
- uid 所有権: rw bind の inner uid 0 ↔ host uid 1000 は既存 kit rw bind
  (GOMODCACHE 等) で実証済みのため新規リスクなし
- environment.yaml の filesystem 記述・notes
  (`sandbox_builder.go:955-963`, `986-991`) を新方式に合わせ更新
- テスト: builder unit (mount 列の検証)。同一 workspace の 2 job で
  $HOME ファイルが持続する / `~/.boid` は独立する e2e はここか PR6 で

### PR3: embedded skills sync + adapter bindings 退役

- skills sync: dispatch 時 (PR1 の home ensure と同じフック) に
  `~/.local/share/boid/skills/<name>` → `<wsHome>/.claude/skills/<name>` を
  バージョンチェック付きコピー (temp dir + rename で原子的に。最新なら no-op)。
  既存 `skills.DeployAll` (`internal/skills/deploy.go:18`) の隣に sync を実装
- adapter bindings の退役 (`internal/adapters/*/bindings.go`):
  - claude: `~/.claude` rw bind・**`~/.claude.json` 単一ファイル bind**・
    `~/.local/share/claude`・embedded skills bind → 全廃
  - codex: `~/.codex`・`~/.volta`・skills bind → 全廃
  - opencode: `~/.opencode`・`~/.config/opencode`・`~/.local/share/opencode`・
    `~/.local/state/opencode`・skills bind・ホスト `~/.claude/skills/*` 個別 bind → 全廃
  - CLI バイナリ系 (`~/.local/bin`・バイナリ親 dir) も退役 —
    ツールチェーンは workspace home 側に住む (親ドキュメントの明示的例外)。
    **init script が CLI を設置するまでセッション不能になるため、
    「コマンドが見つからない」ではなく init 未整備を指す fail-fast
    エラーメッセージを出す** (dogfood チェックリスト参照)
  - `Bindings()` interface は残し、$HOME 非依存の bind が将来要る時のために
    空実装を返す
- PATH: `buildPATH` (`sandbox_builder.go:749` 付近) を workspace home 基準
  (`$HOME/.local/bin` 等) に変更。追加 PATH は既存 `WorkspaceMeta.Env`
  (workspace.yaml の env) で workspace 作者が足せる
- env strip 系 (`CLAUDE_CODE_CHILD_SESSION` strip / `FORCE_SESSION_PERSISTENCE` /
  `IS_SANDBOX`、`internal/adapters/claude/run.go:262-295`) は**不変**
- テスト: sync の冪等性・バージョン更新時の差し替え・並行 dispatch 安全性

### PR4: kit additional_binding の退役

- 2 段配線の削除: hydrate マージ (`internal/orchestrator/project_store.go:162,179`
  の AdditionalBindings 分) + mount 化 (`sandbox_builder.go:238-239` の kit 側)
- スキーマは当面 parse 継続 + 無視 + 警告ログ (既存 kit.yaml を即壊さない)。
  次のメジャー整理で削除
- kit テンプレート更新 (`internal/skills/data/boid-sandbox-configure/templates/`):
  go-dev の GOMODCACHE/GOCACHE rw bind 等を削除 — 実 $HOME があるので
  go の既定パスがそのまま永続キャッシュになる。env 指定も大半不要化
- boid-sandbox-configure スキル本文・e2e fixtures (`e2e/fixtures/kits/*/kit.yaml`)・
  docs (kit-authoring) の追随
- テスト: builder unit の wantMounts 更新 (binding マージは
  sandbox_builder.go の 1 箇所集約済みなのでそこを直す)

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

- $HOME のパスは「ホスト実 home パスに被せる」方式 (`internal/dispatcher/home.go:10`)。
  bind に変えてもパス自体は変えない (sandbox 内外のパス一致は維持)
- shell adapter は `Bindings()` 未定義で kit 経路に乗っている
  (`sandbox_builder.go:139-148`、PR #594 退行の真因)。PR3/PR4 で shell 経路の
  退行テストを必ず踏む
- opencode の「ホスト `~/.claude/skills` 個別 bind」は tmpfs 前提の回避策なので
  PR3 で機構ごと消える。opencode がスキルを見つける経路が
  sync 後の実ディレクトリで機能するか実機確認
- session jsonl が workspace home に移ることで、診断時の参照先が
  `~/.claude/projects/...` (ホスト) から `homes/<slug>/.claude/projects/...` に変わる。
  診断手順のメモ更新
- 並行 job の同一 workspace home RW はローカル (単一ホスト・通常 FS) では
  許容と決定済み。k8s の RWX 問題はステップ 6 以降の論点のまま
