# boid 品質ゲート設計

boid リポジトリ全体の品質ガードを設計するメモ。 旧名 `supervisor-quality-gates.md`。 きっかけは 2026-06-29 の binding 退行だが、 スコープは**リポジトリ全体の品質ゲート**であり、 特定の退行の再発防止に限定しない。 2026-07-02 にサーフェス棚卸し (CI / テスト空白 / web / DB / e2e / docs / deploy) を実施して全面再構成した。

## 設計方針: 機構ガード先行

判断依存ガード (independent review) を足す前に、 **機構で自動的に落ちる仕組み**を厚くする。 根拠 (2026-06-30 調査):

- レビューゲートを「すり抜けない機構」にするのは重い。 supervisor の merge は完全に skill + `.boid/project.yaml` のプロンプト駆動で、 daemon は merge も review も知らない。 ドキュメントにある `task.exit` ゲートはコード未実装。 → 機構化は net-new 工事大。
- 静的解析は手薄すぎて伸びしろが大きい。 CI に lint が一個もなく、 `go vet` も `go test -race` も CI 未実行 (ドキュメント上の規律のみ)。 `scripts/check-internal-architecture.sh` という自前アーキガードが既にあるのに CI/フックから呼ばれず置物状態。
- 退行クラスの多く (binding 配線、 policy drift、 templ drift) は結合テスト・機械チェックでピンポイントに刺せる。

優先順位は機構ガード (Tier 0→1→2) を先行させ、 レビューゲートは Tier 3 に置く。

## サーフェス × ゲート対応表

品質ゲートが守るべき面の棚卸し。 「ゲートの機構 (Tier)」 と直交する軸として、 **どの面を何で守るか / 意図的にスコープ外か**を明示する。 無言の欠落と宣言された非目標は別物。

| サーフェス | 現状 | 対応 |
|---|---|---|
| 配線パス (hydrate → sandbox builder の binding 等) | 下流のみ事後ガード (PR #674)。 上流・貫通はゼロ | **Tier 1 #1** (第一号 = binding) |
| broker policy / builtin op / escape guard | policy drift テストあり (`policy_translate_test.go`)。 op↔escape e2e の対応保証なし | **Tier 1 #2** |
| multi-harness (codex/opencode) | e2e シナリオゼロ | **Tier 1 #3** |
| 並行処理 (orchestrator/dispatcher/api) | `-race` は CLAUDE.md 記載のみ、 CI 未実行 | **Tier 0** |
| アーキ境界 (禁止 import) | スクリプトあり、 未配線 | **Tier 0** |
| web UI: templ 生成コード drift | `_templ.go` はコミット済、 同期チェックなし | **Tier 0** |
| DB マイグレーション | forward-only 27 本、 手書き columnExists のみ。 golden schema なし | **Tier 1 #4** |
| パッケージ単体テストの空白 (adapters ゼロ、 kit スタブ等) | カバレッジ計測もなし | **Tier 1.5** |
| lint (errcheck/staticcheck 等) | ゼロ | **Tier 2** (実装済 2026-07-02: govet/ineffassign/staticcheck/unused/**errcheck**。 gofmt は宣言つき見送り) |
| e2e インフラの信頼性 (flake) | リトライ機構なし。 1 flake で 60 分ジョブ全赤 | **前提 P** (Tier 0 と並行) |
| 意図的分岐 + 誤った claim | reviewer 依存 | **Tier 3** |
| web UI: 静的 JS (`web/static/*.js` 3 ファイル) | テスト・lint なし | **スコープ外** (面が小さい。 増えたら再考) |
| docs ja↔en 同期 | CI が `docs/**` を paths-ignore | **スコープ外** (機械検証のツール投資が割に合わない。 PR flow の規律で担保) |
| deploy 経路 (post-merge hook の `go install`) | CI 緑と非連動、 post-deploy smoke なし | **スコープ外・別トラック** (下記メモ) |
| web UI: e2e フロー (task 一覧 / session / terminal) | `web-auth` 1 本のみ | **スコープ外・当面** (実装変更が激しい。 vt-emulator 安定後に再考) |

deploy 経路のメモ: merge 即 live 反映の運用なので本来ゲートに値するが、 hook はマシンローカルで本プランの CI 工事とは別物。 安い緩和 (hook 内で `gh run list` の CI status 確認 → 緑のときだけ `go install`、 直後に `boid --version` smoke) は別トラックで検討。

## 前提 P — e2e flake 対策 (ゲートの信頼性そのもの)

**ゲートを増やすほど flake の被害も増える**。 「赤でも flake かも」 と思われた時点でゲートの効力は死ぬため、 Tier 積み増しの前提条件として扱う。 方針は 2 本立て: **既知 flake は根治 (prevention) が第一、 retry は未知 flake 用の可視化された保険**。 retry を「緑を出す道具」ではなく「flake を可視化しながら本物の赤だけ通す道具」に格下げして扱う。

- **prevention (第一)**: 既知 flake `daemon-restart-resume` は根治済 (**2026-07-02 PR #688**)。 真因は退場する daemon の `Server.Stop()` 末尾 `os.Remove` が後続 daemon の socket を消していた path-based 削除 (tmpfs inode 再利用が絡む race)。 fd ベース listener Close に委ねて撤去、 決定的 unit test `TestStopDoesNotRemoveForeignSocket` でガード ([[e2e-daemon-restart-resume-flake]])。 今後も個別 flake は「retry で包む前にまず根治」を原則とする。
- **retry (保険、実装済 PR #689)**: `e2e/run.sh` のループに **失敗シナリオの 1 回自動リトライ** を実装 (デフォルト 2 attempts、 `E2E_MAX_ATTEMPTS=1` で無効化)。 握りつぶさない設計を徹底: 各 retry は `[e2e][retry]` マーカーで stderr に出力、 **retry して通ったら FLAKE として明示報告** (「retry に頼るな・根治せよ」)、 全 attempt 失敗は run 全体を赤にする。 末尾に grep 可能なサマリ行 `[e2e][retry] summary: retried=N (...) failed=M (...)` を出す。
- retry 頻発シナリオの計測: 上記サマリ行を CI ログ集計で拾う (専用基盤は作らない)。 log された retry は全て「直すべき bug」として扱う運用。
- quarantine 機構 (既知 flake の別枠化) は retry で足りなければ検討。

## Tier 0 — 既存資産・既存規律の CI 接続 (即効・最小工数)

**実装済 (2026-07-02, PR #686)**。 配線と同時に、 初回 CI で炙り出た既存の潜在問題 3 件を掃除した (下記「初回掃除の実績」)。

書くのではなく**配線するだけ**。 「ドキュメントにはあるが機械 enforcement がない」 ものを全部 CI に落とす。

- `scripts/check-internal-architecture.sh` を CI で実行 (禁止 import グラフ検査)。
- `go vet ./...` を CI ジョブに追加。
- `go test -race ./...` を CI ジョブに追加。 CLAUDE.md に書いてあるのに CI 未実行という、 vet と完全に同じクラスの穴。 並行処理だらけの orchestrator/dispatcher が主戦場。
- **templ 生成コードの drift チェック**: `templ generate` 後に `git diff --exit-code` (または `templ generate --check` 相当)。 `.templ` と checked-in `_templ.go` のズレを CI で落とす。 sandbox 内で templ が使えない ([[sandbox-go-bin-mount]]) からこそ CI 側ゲートが必須の面。

実装メモ (実装後の実際):
- workflow `.github/workflows/blackbox-e2e.yml` の `unit` ジョブに vet / arch / templ drift の 3 step を追加。 `-race` は所要時間が伸びるので専用 `race` ジョブに分離して並列化した。
- templ は CI 上で go.mod と**同一 version** の `go install github.com/a-h/templ/cmd/templ@v0.3.1001` → **リポジトリルートから** `templ generate` → `git status --porcelain` で drift 判定 ([[templ-generate-from-repo-root]])。 version がズレると spurious diff、 生成 CWD がズレると FileName drift。
- `check-internal-architecture.sh` は `go list` を使うためサンドボックス内では sqlite 制約を受ける ([[sandbox-cannot-build-sqlite-packages]])。 enforcement は CI 側。

### 初回掃除の実績 (2026-07-02)

「初回だけ掃除が要る」 は的中。 ゲートを繋いで初めて見えた既存の潜在問題を同 PR で掃除した:

1. **arch スクリプトの許可リストが化石化**。 一度も CI で回っておらず 7 パッケージのまま (現在 17)。 全新規パッケージが `unexpected` 判定。 現状に更新。
2. **`internal/client` の禁止 import ルールが実態と乖離**。 「client は internal に一切依存するな」 だったが、 client は api の DTO / orchestrator のドメイン型を型として共有するのが正当 (循環なし)。 ルールを実態に合わせ、 振る舞いのみの backend 層 (server/db/dispatcher/sandbox) の hard ban に変更 ([[client-type-import-ok-behavior-ng]])。
3. **templ 生成コードが CWD 混在で不整合**。 一部ルート相対・ 一部 web/templates 相対 + `layout_templ.go` の stale 行番号。 全ファイルをルート相対に統一。

掃除 #2 の追い込み (別 PR): arch スクリプトは import グラフしか見えず、 api/orchestrator は「型 import」 と「振る舞い関数呼び出し」 を区別できない。 これを go/types でシンボル単位に解決する回帰テスト `internal/client/architecture_test.go` (`TestClientDoesNotDependOnBehavior`) を追加。 client が api/orchestrator の**パッケージレベル関数** (振る舞い) を参照したら落ちる (型・型のメソッドは許可)。 通常の `go test ./...` で回るため追加の CI 配線は不要。 net-new tooling (go/packages 依存を direct 昇格) なので厳密には Tier 1 相当。

## Tier 1 — 重要配線パスの回帰結合テスト (本命)

**「変更者が claim する範囲を超えた挙動変化」を機構で落とす**結合テスト群。 binding 退行はその第一号であって、 Tier 1 の定義ではない。 対象は 「複数パッケージを貫く配線で、 片端だけ変えると無音で壊れるパス」 全般。

### #1 binding 配線 (hydrate → BuildSandboxSpec 貫通)

2026-06-29 退行 ([[workspace-kit-bindings-2-tier-wiring]]、 付録参照) に直撃する本命。 退行が上下 2 段だったので両段を押さえる。

1. **上流マージの単体テスト新設 (現状ゼロの穴)**: `mergeBindMounts` (`internal/orchestrator/spec_loader.go:747`) と `GetProject` 返り `Meta.AdditionalBindings` が workspace kit マージ済かをアサート。 現状 `project_store_hydrate_test.go` は Env マージは見るが `AdditionalBindings` マージを見るテストが皆無 (= PR #675 で直した本体がノーガード)。
2. **上下 2 段を貫く結合テスト 1 本**: `project.yaml + workspace kit fixture → GetProject → 返り Meta の AdditionalBindings を assert → JobSpec に詰めて BuildSandboxSpec → Mounts に kit bind と harness bind が両立` を 1 テストで通す。 純関数 (実 LLM/実 mount 不要) なのでテスト容易性は高い。

下流 (`sandbox_builder_test.go:1659,1715`) は PR #674 でガード済。

### #2 builtin op ↔ escape guard の不変条件ゲート

boid のセキュリティモデルの中核。 policy drift テスト (`internal/dispatcher/policy_translate_test.go:35` の op constant drift 検査) は既にあるが 「名前のあるゲート」 として扱われていない。 加えて **「新しい builtin / docker-proxy op を足したら、 対応する escape 系テスト (unit or e2e) が必ず対になる」** ことを保証する集約チェックを足す。

- 最小実装: op 一覧 (policy table) と escape テストの対応を突き合わせる meta テスト。 新 op 追加時に 「対応テストを書くか、 明示的に免除リストに載せる」 を強制。
- [[sandbox-cannot-build-sqlite-packages]] の wantOps / drift test 更新規律を機構に昇格させるイメージ。

### #3 multi-harness × 複数 kit の e2e マトリクス

`e2e/scenarios/` は 37 本中 16 本が docker-proxy 系に偏り、 codex/opencode harness を使うシナリオがゼロ (binding 退行の死角)。 fake harness で 「kit init で作った binding が claude/codex/opencode 各 harness で見える」 matrix を 1 シナリオで ([[boid-e2e-from-sandbox-uses-ci]] のとおり requires-sandbox は CI のみ)。 旧プランの G4 はここに統合。

### #4 DB スキーマの golden 比較

マイグレーション 27 本は forward-only で、 検証は手で列挙した `columnExists` のみ。 assert を書き忘れたマイグレーションは素通りする。

- **golden schema テスト**: 全マイグレーション適用後の最終スキーマ (`sqlite_master` 相当のダンプ) を golden ファイルと比較。 新マイグレーションは golden 更新を強制されるので、 意図しないスキーマ変化が diff として可視化される。
- down 経路は存在しない (apply-only 設計) ので rollback テストは対象外。

### 構造的ねじれ (中長期の宿題)

binding 組み立てロジック (`expandWorktreeBindings` / `additionalBindingMounts` / `mergeBindMounts`) は純関数なのに、 同居パッケージ (`dispatcher` / `orchestrator`) が `internal/db` を引くせいでサンドボックス内 `go test` 不可 ([[sandbox-cannot-build-sqlite-packages]])。 #1 #2 のテストも構造上ここに置くしかなく、 CI (ホスト) でしか走らない。

中長期改善: binding 組み立ての中核を db 非依存の小パッケージへ切り出せば、 回帰テストをサンドボックス内でも回せる層に降ろせる。 急がないが、 binding 系を触る PR のついでに少しずつ寄せる。

## Tier 1.5 — パッケージ単体テストの空白解消

2026-07-02 棚卸しで見えた空白。 カバレッジ計測自体がないため空白が不可視になっている点も含めて対処する。

### 可視化 (先行)

- CI の unit ジョブに `-coverprofile` を足し、 総カバレッジをログ出力する。 **初回からハード床 (coverage floor) は張らない** — 床は数字合わせのテストを誘発して形骸化する。 まず可視化、 床は傾向が見えてから判断。

### 優先度つきパッケージ計画

| パッケージ | 現状 | 優先度・方針 |
|---|---|---|
| `internal/adapters` | **テストゼロ** | **高**。 harness adapter は Phase 3-b/3-c の退行多発地帯 (env strip [[phase3b-session-jsonl-not-persisted]]、 Bindings() 統合 [[phase3c-codex-opencode-prototype]])。 Bindings() の出力・env strip/inject・resume 引数組み立てを table test で固める |
| `internal/kit` | `detect_test.go` / `requirements_test.go` が **1 行スタブ** | **高**。 kit init は直近の主力機能 ([[project-kit-init-skill-plan]])。 detect / requirements の実テストを書く。 スタブ放置は 「テストがある」 という誤情報になる分ゼロより悪い |
| `internal/db` (top-level) | `db.go` 未テスト (migrate 配下のみ) | **中**。 Tier 1 #4 の golden schema と同時に着手 |
| `internal/notify` | 薄い (120 行) | **中**。 e2e もゼロの面なので unit 側で担保。 通知 payload 組み立てと failure path |
| `internal/logrotate` | 薄い (240 行) | **中**。 ローテーション境界とエラー時の挙動 (ログ喪失系は事故ると診断手段が消える) |
| `internal/config` / `internal/daemon` / `internal/client` | 薄い | **低**。 触る PR のついでに拡充。 plumbing 中心でロジック密度が低い |
| `internal/timeline` / `internal/initwizard` / `internal/qrterm` / `internal/skills` | 薄い | **低**。 同上 |

進め方: 高優先 2 つ (`adapters` / `kit`) はそれぞれ独立 PR で先行。 中以下は 「そのパッケージを触る機能 PR に同梱」 を規律とし、 専用 PR は立てない。

## Tier 2 — golangci-lint 本格導入

**実装済 (2026-07-02)**。 `.golangci.yml` 新規 + `blackbox-e2e.yml` に `lint` ジョブ追加 (`golangci/golangci-lint-action@v7`、 version `v2.1.6` 固定)。

linter セット (控えめ):
- `errcheck` (儀式系は `std-error-handling` preset で除外、 詳細後述) / `govet` / `ineffassign` / `staticcheck` (SA* バグ検出コア + S* 安全な簡略化) / `unused`。 gosimple は staticcheck v2 の S* に統合済みなので別途は入れない。
- staticcheck は `-ST1*` (stylecheck: 命名規約・コメント書式・重複 import 等) と `-QF1*` (quickfix 提案: De Morgan / tagged switch / 型省略) を除外。 バグ寄りに絞って形骸化を防ぐ。

初回掃除 (同 PR): この控えめセットでも既存コードで数件出たので同時に潰した — 未使用のテストヘルパー/フィールド/型 (`unused`)、 不要な `fmt.Sprintf` (S1039)、 空の if ブランチ (SA9003)、 未使用フィールド `dockerproxy.Server.mu`。

**errcheck は 2026-07-02 の follow-up で導入済**: 当初は「329 件出て、機械的 `_ =` 埋めが握りつぶすとバグになる少数まで隠す」「`exclude-functions` の `(io.Closer).Close` は具象型 `Close` にマッチせず型列挙は脆い」として初回セットから外していた。 その後の再調査で、 上記悲観は **errcheck 単体**の話で、 golangci-lint v2 の **`std-error-handling` プリセット** (旧 EXC0001 の正規表現) を使えば `Close` / `fmt.Fprint*` / `os.Remove(All)` / `os.Std(out\|err)` / `os.(Un)Setenv` を型列挙なしでまとめて除外できると判明。 これで非 test の findings が **381→61 件**に落ちた。

採った方針 (上記 (a) の精緻版):
- errcheck を enable + `exclusions.presets: [std-error-handling]` で儀式系を機械除外。
- `exclude-functions` に `(github.com/a-h/templ.Component).Render` (best-effort な HTML 描画、preset の正規表現に載らない) と `(*github.com/coder/websocket.Conn).CloseNow` (Close 亜種) を追加。
- test ファイルは `exclusions.rules` の `path: _test\.go` で errcheck 対象外。 setup の err 無視は低リスクで、 機械的 `_ =` 埋めがプランの嫌う形骸化 churn になるため (preset 後も test 側に 220 件残っていた)。
- 残る非 test ~37 件を個別に手当て: **実処理**が要るもの (DB pragma の `conn.Exec` はループで error 返却、 crypto/rand の `rand.Read` はトークン helper で entropy 失敗時 panic)、 **best-effort として明示無視** (`_ =` / `_, _ =`: ResponseWriter への Encode/Write、 proxy pump の io.Copy、 worktree cleanup の `exec.Cmd.Run`、 commit 後 no-op の `tx.Rollback`、 shutdown 時 `Serve`)。 既存の壊れた `//nolint: errcheck` (コロン後スペースで無効化されていた) も `_ =` に正規化。

**gofmt / フォーマット系も初回セットに含めない (2026-07-02 決定)**: gofmt 出力は Go のバージョンで揺れる (go1.25 は struct タグ整列アルゴリズムが変わり、 repo は go1.24 整形なので 66 ファイルに差分が出る)。 golangci-lint 同梱 formatter とローカル/CI toolchain がズレると spurious diff や `golangci-lint fmt` の巻き添え整形を招く。 フォーマットの enforcement が必要なら go.mod と同一 toolchain の `gofmt -l` を別ステップで足す方が堅い (Tier 0 系の話で、 本 Tier の範囲外)。

実装メモ (サンドボックス制約が効く):
- 型情報込みの lint は `internal/db` を import するパッケージがサンドボックス内でビルド不可なため、 enforcement は CI ジョブ一択 ([[sandbox-cannot-build-sqlite-packages]])。 `host_commands` 経由のホスト側実行は [[no-project-specific-kits]] の趣旨からも見送り。 (2026-07-02 補足: `storage.googleapis.com` が egress allowlist に載って以降は sandbox 内でも sqlite パッケージが build 可能になり、 lint も回せる。 ただし enforcement の正は引き続き CI。)
- **コミットフックは使わない (2026-07-02 決定)**: brokered git は `core.hooksPath=/dev/null` で git hook を意図的に無効化している (`internal/sandbox/git_builtin.go:400,628`。 sandbox 書込可能な `.git/hooks` × host 側実行 = escape になるため、 `git_shim.go:395` は `core.hookspath` 設定も弾く)。 つまりエージェント発コミット (= この repo のコミットの大半) に pre-commit は構造的に効かず、 hook ゲートは 「カバレッジの錯覚」 を生むだけ。 push 前の早期フィードバックは dev-pr-flow skill / task_behaviors の default_instruction に手順として書く (`gofmt -l` + ビルド可能サブセットの `go vet`)。 プロンプトが versioned で可視な分、 opaque な hook 失敗よりエージェントに効く。

lint は 「意図的な分岐 + 間違った claim」 は catch できないが、 Tier 1 と組で底上げになる。 別軸として並行可。

## Tier 3 — レビューゲート / 配線図 memory

機構 (Tier 0-2) で catch できない **「意図的分岐 + 誤った claim」クラス専用の最後の砦**。

- **強制レビューゲート (旧 G1)**: skill / プロンプト版で軽く始める。 supervisor `default_instruction` (`.boid/project.yaml:25-43`) の `gh pr merge` 前段に `/code-review` を挟む。 daemon 機構化 (`task.exit` ゲート新規実装) は重いので当面見送り。
- **互換性 claim の semantic check (旧 G3)** は review skill 内に統合。 「同等」「互換」「Phase N の前提」 claim を含む変更は cover 範囲明示を必須化。 ← binding 退行の `d464581` の 「claude に同等」 claim を突けたはずの観点。
- **配線図 memory (旧 G2)**: 主要配線パス (kit / workspace / dispatch / hook / hydration) の memory を整備し、 supervisor が planning 時に 「触る配線図」 を declare。 [[workspace-kit-bindings-2-tier-wiring]] はその先行例。 memory は腐るので、 配線変更 PR では対応 memory も同 PR で更新するルール明文化が前提。

## 見送り (宣言つき)

- **commit hook ゲート**: brokered git が hook を無効化するためエージェント発コミットに効かない (Tier 2 実装メモ参照)。 早期フィードバックは skill / default_instruction の手順で代替。
- **mutation testing (旧 G6)**: hot path 限定でも運用負担大。
- **静的 JS のテスト/lint**: 対象 3 ファイルで面が小さい。 JS が増えたら再考。
- **docs ja↔en 同期の機械検証**: ツール投資が割に合わない。 PR flow の規律 (両言語同時更新) で担保。
- **deploy 経路のゲート**: 本プランの CI 工事と別物 (マシンローカル hook)。 別トラックで安い緩和を検討 (対応表のメモ参照)。
- **web UI e2e フロー**: 実装変更が激しい時期 ([[project-web-terminal-vt-emulator]] Phase 2 未着手)。 安定後に再考。

## 旧 G1-G6 との対応表

| 旧アイデア | 新配置 | 扱い |
|---|---|---|
| G1 強制レビューゲート | Tier 3 | skill 版で軽く、 機構化は見送り |
| G2 配線図 memory | Tier 3 | G1 を補佐 |
| G3 互換性 claim semantic check | Tier 3 | G1 の review skill に統合 |
| G4 cross-feature e2e matrix | Tier 1 #3 | multi-harness × kit binding の e2e |
| G5 golangci-lint | Tier 2 | 機構底上げとして CI 追加 (**実装済 2026-07-02**、 errcheck も follow-up で導入済、 gofmt は宣言つき見送り) |
| G6 mutation testing | 見送り | — |
| (新) 既存規律の CI 接続 (arch script / vet / **race** / **templ drift**) | Tier 0 | 最優先 (即効) |
| (新) e2e flake 対策 | 前提 P | ゲート信頼性の前提。 Tier 0 と並行 |
| (新) binding 上流ガード + 貫通結合テスト | Tier 1 #1 | binding 退行に直撃 |
| (新) builtin op ↔ escape guard 不変条件 | Tier 1 #2 | セキュリティモデルの中核 |
| (新) DB golden schema | Tier 1 #4 | 手書き assert 依存の解消 |
| (新) パッケージ空白解消 + カバレッジ可視化 | Tier 1.5 | adapters / kit 先行 |
| (新) binding 純関数の sqlite-free 切り出し | Tier 1 宿題 | 中長期 |

## 推奨着手順

1. **Tier 0 + 前提 P** — 並行で即。 各 PR 1 本。 既存違反の掃除込みでも軽い。
2. **Tier 1 #1** — binding 上流ガード + 貫通結合テストを 1 PR。
3. **Tier 1 #2 / #4** — op↔escape 不変条件、 DB golden schema。 各 1 PR、 並行可。
4. **Tier 1.5 高優先** — `adapters` / `kit` の実テスト。 各 1 PR。 カバレッジ可視化は Tier 0 に同梱でも可。
5. **Tier 1 #3** — multi-harness e2e シナリオ。
6. ~~**Tier 2** — golangci-lint CI 追加。 並行可。~~ **実装済 (2026-07-02)**。
7. **Tier 1 構造的ねじれの解消** — binding 系を触る PR のついでに段階的に。
8. **Tier 3** — 機構が固まってから。 G1 skill 版 → (必要なら) G2 memory 整備。

## 未決事項

- ~~Tier 0 の既存違反の量 (初回掃除コスト見積もり)~~ **解決 (PR #686)**: vet / race はクリーン、 掃除は arch 許可リスト・ client ルール・ templ 生成不整合の 3 件 (「初回掃除の実績」 参照)。 いずれも同 PR 内で収まる軽量。
- ~~前提 P のリトライ実装位置 (`run.sh` のループ内 vs scenario 単位 wrapper) と retry ログの形式。~~ **解決 (PR #689)**: `run.sh` のシナリオ直列ループ内に実装 (scenario 単位 wrapper は増やさず既存 `run_scenario` 呼び出しを while で包むだけ)。 ログ形式は `[e2e][retry]` マーカー + 末尾 grep 可能サマリ。
- Tier 1 #1 の貫通結合テストを `dispatcher` に置くか `orchestrator` に置くか (どちらも db import で CI 限定)。 純関数切り出しと同時にやるか別 PR か。
- Tier 1 #2 の 「op ↔ テスト対応」 の突き合わせ方式 (naming convention で機械照合 vs 明示 manifest)。
- Tier 1.5 のカバレッジ可視化の出し方 (ログのみ vs PR コメント)。 床を張るかの判断時期。
- ~~Tier 2 の初回 linter セット。~~ **解決 (2026-07-02)**: govet / ineffassign / staticcheck (SA*+S*、 ST*/QF* 除外) / unused。 gofmt (バージョン揺れ) は宣言つき見送り (Tier 2 節参照)。
- ~~errcheck をいつ・どう入れるか (テキスト除外で ~30 件手当て vs 段階的に負債返済)。~~ **解決・導入済 (2026-07-02 follow-up)**: `std-error-handling` preset で儀式系を機械除外 + test 除外 + 残 ~37 件を個別手当て (実処理/明示無視)。 Tier 2 節参照。
- Tier 3 G1 の reviewer モデル / effort デフォルト (max は token 重い、 high で足りるか)、 false positive 時の NO-GO override 経路。
- Tier 3 G2 の 「触る配線図」 declare 形式 (memory ref のリスト? plan doc に明示?) と腐り対策の enforcement。

## 付録: きっかけになった binding 退行 (2026-06-29)

本プランの出発点。 Tier 1 #1 と Tier 3 の設計根拠なので記録を残す。

「workspace kit の `additional_bindings` が claude/codex/opencode harness session で無音で死ぬ」 退行 ([[workspace-kit-bindings-2-tier-wiring]]、 PR #674 + #675 で修正)。

### 退行の正確な機序

binding は **上流 (hydrate) → 下流 (sandbox builder) の 2 段**で配線される。 退行は**両段で**起きていた:

- **下流**: Phase 3-c の `feat: codex / opencode adapter を追加` (`d464581`) で、 `sandbox_builder.go` の `expandedBindings` を `harnessBindings` で**排他置換**していた (`pathBindings = harnessBindings` / kit binding を append しない分岐)。 PR #674 (`4cd50c5`) で加算に修正。
- **上流**: API 出口 (`internal/api/project.go` の `GetProject` 再取得) も workspace kit マージ前の素 hydrate を返していた。 PR #675 (`33ac4cf`) で修正。

### なぜ当時 catch されなかったか

1. **binding 共存をアサートするテストが 1 つも無かった**。 `sandbox_builder_test.go` には「harness binding がある」「kit roots がある」を個別に見るテストはあったが、 「harness あり かつ project/kit の `additional_bindings` も残る」を同時に検査するケースが存在しなかった。 PR #674 でその 2 ケース (`TestBuildSandboxSpec_ProfileInit_HarnessKeepsAdditionalBindings` / `ProfileDefault_...`) が新設された = ガードできる形になったのは**事後**。
2. **設計前提が当時は正しかった**。 `d464581` は「kit は agent CLI plumbing しか積まない」前提で排他にした。 kit を環境固有 binding (`~/.volta` 等) の置き場に変えた 2026-06-26 の workspace+kit 再編は `d464581` より後。 退行は潜在的で、 再編により初めて顕在化した。
3. **動作確認が 1-turn smoke だけ**だったため、 `additional_bindings` を持つ別 kit の巻き添えが見逃された。

つまりこの bug は **「変更者が claim する範囲」を超えた挙動変化 (= 機構で catch すべきもの)** であって、 reviewer の判断力に期待するより結合テスト・静的解析で自動的に落とす方が ROI が高い。 これが 「機構ガード先行」 方針の根拠。

## 関連 memory

- [[workspace-kit-bindings-2-tier-wiring]] — きっかけの退行のアーキ的整理 (2 段配線)。 Tier 1 #1 が直撃する対象
- [[sandbox-cannot-build-sqlite-packages]] — Tier 1 の結合テスト配置と Tier 2 の lint 実行先を縛る制約
- [[boid-e2e-from-sandbox-uses-ci]] — Tier 1 #3 の e2e 実装条件 (requires-sandbox は CI のみ)
- [[e2e-daemon-restart-resume-flake]] — 前提 P の動機になった既知 flake
- [[sandbox-go-bin-mount]] — templ が sandbox 内で使えない制約 (Tier 0 templ drift チェックを CI に置く理由)
- [[templ-generate-from-repo-root]] — Tier 0 templ drift の運用 (ルートから生成・ FileName ルート相対・ version 一致)
- [[client-type-import-ok-behavior-ng]] — Tier 0 初回掃除 #2 と client 振る舞いガードのアーキ不変条件
- [[phase3b-session-jsonl-not-persisted]] / [[phase3c-codex-opencode-prototype]] — `internal/adapters` の退行史 (Tier 1.5 で高優先の根拠)
- [[boid-canonical-task-behaviors]] — supervisor / executor の責務分離 (Tier 3 G1 を挟む場所の context)
- [[evidence-first-infra-suspicion]] — 「primary evidence で判断」の習慣
- [[evaluation-no-anchor-stated-criteria]] — reviewer の検証バイアス (claim を額面通り受け取る) は評価バイアスの近縁。 Tier 3 の semantic check の動機
