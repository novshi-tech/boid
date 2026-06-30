# supervisor quality gates (設計メモ)

boid 自身の開発ワークフローに品質ガードを足す設計メモ。 2026-06-30 に現状調査 (CI / 静的解析 / 結合テスト / supervisor フロー) を実施し、 **「機構ガード先行」** 方針に再構成した。

## なぜ書いたか (背景)

2026-06-29 に判明した「workspace kit の `additional_bindings` が claude/codex/opencode harness session で無音で死ぬ」 退行 ([[workspace-kit-bindings-2-tier-wiring]]、 PR #674 + #675 で修正) の振り返りから派生。

### 退行の正確な機序 (2026-06-30 調査で更新)

binding は **上流 (hydrate) → 下流 (sandbox builder) の 2 段**で配線される。 今回の退行は **両段で**起きていた:

- **下流**: Phase 3-c の `feat: codex / opencode adapter を追加` (`d464581`) で、 `sandbox_builder.go` の `expandedBindings` を `harnessBindings` で **排他置換**していた (`pathBindings = harnessBindings` / kit binding を append しない分岐)。 PR #674 (`4cd50c5`) で加算に修正。
- **上流**: API 出口 (`internal/api/project.go` の `GetProject` 再取得) も workspace kit マージ前の素 hydrate を返していた。 PR #675 (`33ac4cf`) で修正。

### なぜ当時 catch されなかったか

1. **binding 共存をアサートするテストが 1 つも無かった**。 `sandbox_builder_test.go` には「harness binding がある」「kit roots がある」を個別に見るテストはあったが、 **「harness あり かつ project/kit の `additional_bindings` も残る」を同時に検査するケースが存在しなかった**。 PR #674 でその 2 ケース (`TestBuildSandboxSpec_ProfileInit_HarnessKeepsAdditionalBindings` / `ProfileDefault_...`) が新設された = ガードできる形になったのは**事後**。
2. **設計前提が当時は正しかった**。 `d464581` は「kit は agent CLI plumbing しか積まない」前提で排他にした。 kit を環境固有 binding (`~/.volta` 等) の置き場に変えた **2026-06-26 の workspace+kit 再編は `d464581` より後**。 退行は潜在的で、 再編により初めて顕在化した。
3. **動作確認が 1-turn smoke だけ**だったため、 `additional_bindings` を持つ別 kit の巻き添えが見逃された。

つまり今回の bug は **「変更者が claim する範囲」を超えた挙動変化 (= 機構で catch すべきもの)** であって、 reviewer の判断力に期待するより **結合テスト・静的解析で自動的に落とす**方が ROI が高い。 これが下記の方針転換の根拠。

## 設計方針: 機構ガード先行

判断依存ガード (independent review) を足す前に、 **機構で自動的に落ちる仕組み**を厚くする。 2026-06-30 調査で以下が裏付けられた:

- レビューゲートを「すり抜けない機構」にするのは重い。 supervisor の merge は完全に skill + `.boid/project.yaml` のプロンプト駆動で、 daemon は merge も review も知らない。 「子が done 後に親側で review ジョブを差し込む」daemon 機構は存在せず、 ドキュメントにある `task.exit` ゲートはコード未実装 (`state-machine.md` と `dev-pr-flow` skill が言及するが grep ヒット 0)。 → 機構化は net-new 工事大。
- 静的解析は手薄すぎて伸びしろが大きい。 CI に lint が一個もなく、 `go vet` すら CI 未実行 (ドキュメント上の規律のみ)。 `scripts/check-internal-architecture.sh` という自前アーキガードが既にあるのに CI/フックから呼ばれず置物状態。
- 今回の退行クラスは結合テストでピンポイントに刺せる。 `BuildSandboxSpec` は `Runner.Run` の手前で binding を組む純関数的構造で、 実 mount も実 LLM も不要。

優先順位は機構ガード (Tier 0→1→2) を先行させ、 旧 G1 (強制レビューゲート) は Tier 3 に降格する。

## Tier 構造

### Tier 0 — 既存資産を CI に接続 (即効・最小工数)

書くのではなく**配線するだけ**。最小工数で即効。

- `scripts/check-internal-architecture.sh` を CI で実行 (禁止 import グラフ検査。 例 `internal/api → internal/db` 禁止、 `internal/db` は他 internal 依存禁止)。 既存だが CI/フック未接続。
- `go vet ./...` を CI ジョブに追加。 現状ドキュメント (`CLAUDE.md`, contributing, `/dev-pr-flow` skill) にあるだけで機械的 enforcement なし。

実装メモ:
- 唯一の workflow `.github/workflows/blackbox-e2e.yml` の `unit` ジョブに step 追加が最小。
- `check-internal-architecture.sh` は `go list` (= パッケージロード) を使うため、 CI (ホスト) では問題ないがサンドボックス内では sqlite 制約を受ける点に注意 ([[sandbox-cannot-build-sqlite-packages]])。 enforcement は CI 側に置く。

懸念: ほぼ無し。 既存 vet 違反・アーキ違反があれば初回だけ掃除が要る。

### Tier 1 — binding 回帰の結合テスト (今回の退行に直撃・本命)

2026-06-30 調査の seam 分析より、 **「hydrate 出力 → `BuildSandboxSpec` 入力」を貫く薄い結合テスト**が最も費用対効果が高い。 今回の退行が上下 2 段だったので、 両段を押さえる。

1. **上流マージの単体テスト新設 (現状ゼロの穴)**: `mergeBindMounts` (`internal/orchestrator/spec_loader.go:747`) と `GetProject` 返り `Meta.AdditionalBindings` が workspace kit マージ済かをアサート。 現状 `project_store_hydrate_test.go` は Env マージは見るが `AdditionalBindings` マージを見るテストが皆無 (= PR #675 で直した本体がノーガード)。
2. **上下 2 段を貫く結合テスト 1 本**: `project.yaml + workspace kit fixture → GetProject → 返り Meta の AdditionalBindings を assert → JobSpec に詰めて BuildSandboxSpec → Mounts に kit bind と harness bind が両立` を 1 テストで通す。 純関数 (実 LLM/実 mount 不要) なのでテスト容易性は高い。
3. **multi-harness × 複数 kit の e2e シナリオ追加**: `e2e/scenarios/` に codex/opencode harness を使うシナリオが現状ゼロ (今回の死角)。 fake harness で「kit init で作った binding が claude/codex/opencode 各 harness で見える」 matrix を 1 シナリオで ([[boid-e2e-from-sandbox-uses-ci]] のとおり requires-sandbox は CI のみ)。 旧プランの G4 はここに統合。

下流 (`sandbox_builder_test.go:1659,1715`) は PR #674 でガード済なので、 **新規に要るのは上流 (#1) と貫通 (#2) と e2e (#3)**。

#### 構造的ねじれ (中長期の宿題)

binding 組み立てロジック (`expandWorktreeBindings` / `additionalBindingMounts` / `mergeBindMounts`) は **純関数なのに、 同居パッケージ (`dispatcher` / `orchestrator`) が `internal/db` を引くせいでサンドボックス内 `go test` 不可** ([[sandbox-cannot-build-sqlite-packages]])。 上記 #1 #2 のテストも構造上ここに置くしかなく、 **CI (ホスト) でしか走らない**。

中長期改善: binding 組み立ての中核を **db 非依存の小パッケージ (例 `internal/sandbox` 配下 or 新規 sqlite-free pkg) へ切り出せば**、 回帰テストをサンドボックス内でも回せる層に降ろせる。 「純関数なのに db 同居でサンドボックス test 不可」というテスト容易性のねじれの解消。 急がないが、 binding 系を触る PR のついでに少しずつ寄せるのが筋。

### Tier 2 — golangci-lint 本格導入 (旧 G5 を昇格)

旧プランは G5 を「今回の bug には効かない / 低効果」としていたが、 機構ガードの底上げとして CI 追加の価値は高い。 errcheck / gosimple / unused / ineffassign / staticcheck 等。

実装メモ (サンドボックス制約が効く):
- 型情報込みの lint (golangci-lint / staticcheck) は `internal/db` を import するパッケージがサンドボックス内でビルド不可なため、 **サンドボックス内 pre-commit フックでのフル実行は現状の egress 制約下で不安定/不可** ([[sandbox-cannot-build-sqlite-packages]])。
- 現実的な構成: **(a) lint は CI ジョブ追加で担保**、 **(b) コミットフックは `gofmt -l` 等の構文のみで動く軽量チェックに限定**、 もしくは **(c) `host_commands` に golangci-lint を登録し broker dispatch でホスト側 (フルツールチェーン) で走らせる**。 (a)+(b) が無難。
- `.golangci.yml` は未導入なので新規。 初回は厳しすぎない linter セットから。

旧プランの懸念どおり、 lint は「意図的な分岐 + 間違った claim」 は catch できない。 だが Tier 1 と組で底上げになる。 別軸として並行可。

### Tier 3 — レビューゲート (旧 G1) / 配線図 memory (旧 G2) — 降格

機構 (Tier 0-2) で catch できない **「意図的分岐 + 誤った claim」クラス専用の最後の砦**。 今回の退行はもはや Tier 1 で落ちるので、 Tier 3 の主目的は「機構の網をすり抜ける挙動変化」に絞られる。

- **旧 G1 (強制レビューゲート)**: skill / プロンプト版で軽く始める。 supervisor `default_instruction` (`.boid/project.yaml:25-43`) の `gh pr merge` 前段に `/code-review` を挟む。 daemon 機構化 (`task.exit` ゲート新規実装、 `machine.go` / `coordinator.go` / `lifecycle.go` に net-new) は重いので当面見送り。
- **旧 G3 (互換性 claim の semantic check)** は G1 の review skill 内に統合 (独立 skill 化はコスト増)。 「同等」「互換」「Phase N の前提」 claim を含む変更は cover 範囲明示を必須化。 ← 今回の `d464581` の「claim は claude に同等」を突けたはずの観点。
- **旧 G2 (アーキ図 memory 事前整備)**: 主要配線パス (kit / workspace / dispatch / hook / hydration) の memory を整備し、 supervisor が planning 時に「触る配線図」を declare。 [[workspace-kit-bindings-2-tier-wiring]] はその先行例。 ただし memory は腐るので、 配線変更 PR では対応 memory も同 PR で更新するルール明文化が前提。

### 見送り (旧 G6)

mutation testing。 hot path 限定でも運用負担大。 当面見送り。

## 旧 G1-G6 との対応表

| 旧アイデア | 新 Tier | 扱い |
|---|---|---|
| G1 強制レビューゲート | Tier 3 | 降格。 skill 版で軽く、 機構化は見送り |
| G2 配線図 memory | Tier 3 | 降格。 G1 を補佐 |
| G3 互換性 claim semantic check | Tier 3 | G1 の review skill に統合 |
| G4 cross-feature e2e matrix | Tier 1-#3 | 昇格・統合。 multi-harness × kit binding の e2e |
| G5 golangci-lint | Tier 2 | 昇格。 機構底上げとして CI 追加 |
| G6 mutation testing | — | 見送り |
| (新) 既存スクリプト + vet の CI 接続 | Tier 0 | 新規・最優先 (即効) |
| (新) binding 上流ガード + 貫通結合テスト | Tier 1 | 新規・本命 (今回の退行に直撃) |
| (新) binding 純関数の sqlite-free 切り出し | Tier 1 | 新規・中長期 |

## 推奨着手順

1. **Tier 0** — 即やれる。 PR 1 本。 既存違反の掃除込みでも軽い。
2. **Tier 1 #1 + #2** — 今回の退行に直撃。 上流ガード + 貫通結合テストを 1 PR。
3. **Tier 1 #3** — multi-harness e2e シナリオ。 Tier 1 の #1#2 の後。
4. **Tier 2** — golangci-lint CI 追加。 並行可。 初回 linter セットは控えめに。
5. **Tier 1 構造的ねじれの解消** — binding 系を触る PR のついでに段階的に。
6. **Tier 3** — 機構が固まってから。 G1 skill 版 → (必要なら) G2 memory 整備。

## 未決事項

- Tier 0 の既存 vet / アーキ違反の量 (初回掃除のコスト見積もり)。
- Tier 1 #2 の貫通結合テストを `dispatcher` に置くか `orchestrator` に置くか (どちらも db import で CI 限定)。 純関数切り出し (構造的ねじれ解消) と同時にやるか別 PR にするか。
- Tier 2 の初回 linter セット (厳しすぎると既存コードで大量 finding → 形骸化)。 コミットフックを (b) 軽量に留めるか (c) host_commands 経由にするか。
- Tier 3 G1 の reviewer モデル / effort デフォルト (max は token 重い、 high で足りるか)、 false positive 時の NO-GO override 経路。
- Tier 3 G2 の「触る配線図」declare 形式 (memory ref のリスト? plan doc に明示?) と腐り対策の enforcement。

## 関連 memory

- [[workspace-kit-bindings-2-tier-wiring]] — 今回の退行のアーキ的整理 (2 段配線)。 Tier 1 が直撃する対象
- [[sandbox-cannot-build-sqlite-packages]] — Tier 1 の結合テスト配置と Tier 2 の lint 実行先を縛る制約
- [[boid-e2e-from-sandbox-uses-ci]] — Tier 1 #3 の e2e 実装条件 (requires-sandbox は CI のみ)
- [[boid-canonical-task-behaviors]] — supervisor / executor の責務分離 (Tier 3 G1 を挟む場所の context)
- [[evidence-first-infra-suspicion]] — 「primary evidence で判断」の習慣。 今回の振り返りの根底
- [[evaluation-no-anchor-stated-criteria]] — reviewer の検証バイアス (claim を額面通り受け取る) は評価バイアスの近縁。 Tier 3 G3 の動機
