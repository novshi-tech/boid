# エージェント認識型 boid: ハーネス Adapter とロール簡素化

> Status: 設計合意 (2026-06-17)、未実装。
> 本ドキュメントは設計議論の結論を起こしたもの。コード参照は議論時点の調査に基づくため、
> 実装時に行番号は再確認すること。

## 背景と動機

### 設計ポリシー再確認

boid はハーネスを軽く保ち、ワークフローはエージェントスキルに極力委ねる方針。
PR 作成・CI/CD・マージなどの開発ワークフローは boid コアの前提に含めず、
instruction とエージェントスキルに委ねる。

### supervisor / executor 役割固定の実質

canonical な 2 値プリセット (supervisor / executor) を残してきた論拠は複数あった
(進捗可視化・FS 書込権限・マルチエージェントインタフェース・履歴) が、実装裏どり後に
絞ると、役割で実質的に違うのは以下の 2 軸のみ。

1. readonly フラグ (supervisor = true / executor = false)
2. default_instruction の中身

env / secrets / egress / worktree などはすべて project-level であり、役割では
切り替わらない。`TaskBehavior` 構造体 9 フィールドのうち、役割で structural に変わるのは
default_instruction だけ。

### かつての readonly 手動指定禁止判断

Phase 2-3 で `readonly` を `CreateTaskRequest` から削除した経緯がある。これは「2 つの
task_behavior プリセットを決定論的なガードレールにすべき」という当時の判断による。
本計画ではこの方針を意識的に上書きする。

### Claude 固有コードの漏出が限界

boid コアは当初「エージェントを単なる外部プロセスとして扱う、bash と区別しない」構想で
設計された。その結果、agent-stop の信号プロトコルや session resume の細部が boid 側に
漏れ出し、エージェントへの細かい制御が難しくなって、複雑なシグナル処理が boid コアに
散らばる事態を招いている。当初構想には無理があったと判断する。

### 将来要求: 真のマルチハーネス

- Claude のフロンティアモデルを常に使えるとは限らない時代に入りつつある。
- 廉価モデルを並列で走らせて結果を judge するループの実用性が上がる。
- これには Claude 以外のエージェント (codex / opencode 等) を真に plug-in できる
  必要があり、現状の Claude 前提コードはこれを妨げる。

## 現状 (コード上の事実)

### task_behavior の役割固定

- canonical name は `supervisor` / `executor` のみ許容。
- readonly は behavior 名から自動付与: `internal/orchestrator/behavior_resolve.go:174-186`
  の `applyCanonicalBehaviorOverrides` で supervisor → true、executor → false、
  それ以外 → false。
- `task.Readonly` フラグだけが dispatcher で参照される:
  `internal/dispatcher/sandbox_builder.go:338,352`。
- create payload に `readonly` を入れると静かに drop + WARN ログ:
  `internal/api/task.go:27-32` の `deprecatedTaskRowFields`、CLI 側は
  `cmd/task.go:270` の `deprecatedTaskRowSpecFields`。
- `TaskBehavior` 構造体 (`internal/orchestrator/spec_types.go:346-361`) のフィールドは
  traits / default_instruction / kits / commands / hooks / env / host_commands /
  additional_bindings / kit_roots の 9 個。役割で実質変わるのは default_instruction のみ。

### instruction 上書きは既に動く

- `CreateTaskRequest.Instructions json.RawMessage`: `internal/api/task.go:178`。
- マージ意味論: `internal/orchestrator/payload_merge.go:25-57`
  (0 件 = behavior default 維持、1 件 = フィールド毎マージで非空が勝つ、
  2 件以上 = 完全置換)。
- CLI: `boid task create -f spec.yaml` の YAML に `instructions:` を書けば通る
  (`cmd/task.go:280-309`)。
- `boid task update --instructions-file` で post-create も可能
  (`cmd/task.go:135,182`)。

### Claude 固有コードの漏出

実コード漏出 (実機能が claude 固有) と cosmetic 漏出 (コメント/ドキュメント文言のみ
claude 固有、実コードは generic) を区別して整理する。HarnessAdapter で吸収すべきは
実コード漏出が中心 (9 箇所 / 8 ファイル)。 cosmetic 漏出 (4 箇所 / 4 ファイル) は
adapter 抽出と独立に書き換え可能。合計 13 箇所 / 12 ファイル。

#### プロトコル層 (実コード漏出の中核 — 7 箇所 / 7 ファイル)

agent-stop の `SIGUSR1 → run-agent.py → SIGTERM child` 規約が以下に貫通している:

- `internal/api/store.go:46,153-160`
- `internal/api/task_notify.go:230-244`
- `internal/api/workflow.go:58`
- `internal/api/job.go:194-197`
- `internal/sandbox/script.go:97-102,172` (`trap '' USR1`)
- `internal/sandbox/boid_shim.go:156-159`
- `internal/server/boid_executor.go:56-60`

(`internal/dispatcher/runner.go:774` の `SignalJobRuntime` は関数自体は generic な
シグナル送信。 SIGUSR1 を呼ぶ側の規約が claude 寄りなので、 cosmetic 側で扱う。)

#### 実行特性層 (実コード漏出 — 2 箇所 / 1 ファイル)

- `internal/orchestrator/planner.go:84-90` の `Interactive: true` 強制理由が
  "claude --print が別 Max credit 食う"。
- `internal/orchestrator/planner.go:168-169` の `BOID_AGENT_SESSION_ID` が
  "claude session resume 用" として surface。

#### コメントレベル (cosmetic — 4 箇所 / 4 ファイル)

実コードは harness 不問だが、コメント/ドキュメント文言が claude 固有名を引用している
箇所。HarnessAdapter の導入とは独立に書き換え可能。

- `internal/dispatcher/runner.go:774` の "claude session gracefully" (関数
  `SignalJobRuntime` は generic、コメントだけ claude 言及)。
- `internal/dispatcher/sandbox_builder.go:230` の "claude code isatty TUI detection"。
- `internal/orchestrator/awaiting_payload.go:8-10` の `SessionID` doc が
  "claude --print session ID" 前提 (フィールドは generic な string)。
- `internal/api/web_service.go:107` の DuplicateTask コメントが `claude_code.sessions`
  trait 名を引用 (実コードは TaskService 経由で runtime payload 継承を避けてる、
  trait を直接読んでいない)。

### 親 → 子リスト API

- HTTP には未露出: `TaskHandler.Routes` (`internal/api/task.go:88-113`) に `/children` 無し。
- 内部 `TaskStore.ListChildren` (`internal/orchestrator/store.go:430`) は workflow
  内部のみで使用 (`internal/api/workflow_action.go:496`)。
- `TaskFilter` (`internal/orchestrator/store.go:33-39`) に parent_id 無し。
- 派生フィールド `OpenChildCount` / `DoneChildCount` 等は list で見えるが、子そのものは
  取れない。

### token / cost 会計

- DB マイグレーション全件に `token` / `cost` / `usage` 関連カラム 0 件。
- jobs テーブル (`internal/db/migrate/migrations/0001_initial.sql:48-58` + 拡張 ~0027)
  には exit_code / output / execution_state 等しか無い。

## 設計判断

### 1. canonical name 廃止、free naming + ルートテンプレ複数共存

- `applyCanonicalBehaviorOverrides` (`behavior_resolve.go:174-186`) を削除する。
- project.yaml の `task_behaviors` で任意 name を受け付ける
  (`supervisor` / `executor` 限定を撤回)。
- readonly の既定値は `false`。behavior 側で `readonly: true` を書けば readonly になる。
- default_instruction は behavior 側で書くか、create 時に上書きするかを選べる。

#### ルートテンプレ複数共存モデル

汎用スキル (設計判断 3) は behavior 名で動作分岐しない。違いは behavior エントリの
フィールド (`default_instruction` / `readonly` 等) だけで表現する。これにより
**ルートタスク用のテンプレ (= behavior エントリ) は project ごとに自由に並べられる**。
1 つの汎用スキルから複数のテンプレが呼ばれる構造 (skill 1 本 + テンプレ多数)。

用例: `research` / `dev` / `review` のように用途別テンプレを並べ、WebUI/CLI から
どれで起動するかを選ぶ。バイブコーダー向け配布
([[project-vibe-coder-provisioning]]) にも合う構造。

#### 未指定時 default 指定

project.yaml にトップレベルキー `default_task_behavior: <name>` を追加する
(`task_behaviors:` の構造そのものは無変更)。WebUI/CLI から behavior を省略して
ルートタスクを作ったとき、この default が選ばれる。

```yaml
default_task_behavior: dev      # 新規トップレベルキー

task_behaviors:                 # 構造は現状のまま
  dev:
    readonly: false
    default_instruction: ...
  research:
    readonly: true
    default_instruction: ...
```

`default_task_behavior` 未指定時の挙動:

- `task_behaviors` に `supervisor` があれば supervisor を暗黙的 default にする
  (互換維持)。
- `supervisor` も無く `default_task_behavior` も指定されていなければエラー。

API 側: `CreateTaskRequest.Behavior` は既に省略可能。サーバ側で省略時に default を
引く実装を追加する (Track A2 のスコープ)。

既存 supervisor / executor を使う project.yaml は **当面動作させる**
(互換維持。詳細は「互換性方針」)。

### 2. readonly + instruction を create 時上書き可に

- **readonly**:
  - `CreateTaskRequest` に `Readonly *bool` を追加 (`internal/api/task.go:169`)。
  - `deprecatedTaskRowFields` / `deprecatedTaskRowSpecFields` から `"readonly"` を外す。
  - `CreateTask` に `if req.Readonly != nil { readonly = *req.Readonly }` を入れる
    (`internal/api/task_create.go:101-102` 付近)。
  - **過去の「決定論的ガードレール」判断を意識的に上書きする**。
  - 想定変更ファイル数 ~4 (ハンドラ・モデル・サービス・テスト)。
- **instruction**:
  - 既に動く (上述)。Track A 側の追加改修は不要。
  - 動的 instruction 生成パターンへの skill 側の転換は Track B (汎用スキル統合)
    の中で扱う。詳細は設計判断 3「動的 instruction 生成パターンへの転換」を参照。

### 3. 汎用スキルへの統合 (boid 本体)

boid 本体での canonical name 廃止 + free naming 解禁だけでは、ロール簡素化は実用に
ならない。現在 boid 本体には `internal/skills/data/boid-supervisor` と
`internal/skills/data/boid-executor` の 2 つが埋め込まれており、project.yaml の
behavior 名から特定のスキル群が暗黙に bind される運用になっている。本体側で
free naming にしても、新規 behavior 名に対応する汎用スキルが存在しなければ
エージェントは正しく動作しない。

統合スキル自体は boid 本体 (`internal/skills/data/`) に置く。boid-kits の
`claude-code` キット側 (kit.yaml の `additional_bindings`) は新スキルを
`~/.claude/skills` に bind するための配線追加が必要だが、これは軽い追従作業で、
クリティカルパスは boid 本体側のスキル統合作業。

#### 統合方針

- `boid-supervisor` と `boid-executor` を **汎用スキル 1 本に統合する** ことを
  ゴールとする (例: `boid-task` 等の名前は別途検討)。
- 役割の違い (計画立案 / 子タスク管理 / 実作業 / レビュー) はスキル内部で instruction
  や task コンテキスト (readonly フラグ、親子関係、project hint 等) に応じて動的に
  振る舞いを切り替える。
- 統合スキルは boid 本体の free naming に対応し、behavior 名から動作を分岐させない。
- 「skill 1 本 + ルートテンプレ多数」モデル (設計判断 1 参照) と整合する。汎用スキル
  は behavior 名を読まず、違いは behavior フィールド (default_instruction / readonly
  等) で表現される。これにより supervisor / executor を「テンプレ」として残しつつ、
  汎用スキル 1 本でまかなえる構造が成立する。

#### 周辺スキル棚卸し結果

Track B のスコープを明確化するため、`internal/skills/data/` の各スキルの扱いを
以下の通り整理する。

| スキル | 扱い | 備考 |
|---|---|---|
| `boid-supervisor` | **統合** | 汎用スキルの中核 (タスク内部用) |
| `boid-executor` | **統合** | 汎用スキルの中核 (タスク内部用) |
| `boid-plan` | **Legacy 廃止** | SKILL.md に deprecated 明記済、`/boid-supervisor` 旧名 alias |
| `boid-sandbox` | **Legacy 廃止** | レガシー dispatch shim |
| `boid-discuss` | **Legacy 廃止** | `boid-orchestrate` の存在を見落として作られた重複機能。機能は orchestrate に吸収 |
| `boid-orchestrate` | **維持 + 拡張** | タスク外部 entry point。CLI (`boid exec`) と Web UI Commands ボタンは本質的に同じ役割 (起動経路の違いだけ) なので両方サポートする形に拡張。さらに動的 instruction / readonly 生成も対応 |
| `boid-web` | **スコープ外** | サブシステム (サンドボックス内 web fetch) |

Track B 完了の中身は **「統合 (supervisor + executor → 汎用スキル) + Legacy 廃止
(plan / sandbox / discuss) + 更新 (汎用スキルへの動的 instruction 生成パターン
適用) + boid-orchestrate の拡張」** となる。

`boid-discuss` を廃止するには、Web UI 側で Commands ボタンが起動するスキルを
`boid-orchestrate` に切り替える追従が必要。orchestrate 拡張の完了後に廃止する。

#### 動的 instruction 生成パターンへの転換

現状の skill は project.yaml の `default_instruction` を全タスクで一律に使うのが
デフォルト挙動になっている。create 時 instruction 上書きの仕組み (設計判断 2) は
すでに存在するが、skill 側で積極利用するようには書かれていない。汎用スキルでは
以下のパターンをデフォルト挙動として採用する:

- **ルートタスクの instruction は project.yaml の `default_instruction` を使う** で
  従来通り。WebUI / CLI からタスクを作るときに毎回 instruction を全部指定させると
  煩雑なため、ルートタスクのブートストラップにはこの固定 instruction を残す。
- **子タスク以降の instruction は親 skill が動的生成** し、`boid task create` の
  `instructions` フィールドに渡す。子タスクの仕事内容・コンテキスト・読み取り
  範囲等を親が判断して都度組み立てる。これを skill レベルの推奨パターン
  (= デフォルト挙動) として明文化する。
- 結果として「ルート = project.yaml の固定 instruction でブート、子孫 = 親が動的に
  展開」という流れが skill の標準フローになる。

この転換は Track B の中で skill 統合と同時に進める。コア側の Track A2 (free naming
解禁) を待つ必要はないが、skill の書き直しを伴うので Track B の作業量に含めて
見積もる。

#### 想定される困難

これは本計画で**最も不確実性が高い部分**。「結構むずかしいかも」という見立てを
前提に進める。

- supervisor と executor のスキルは責務が大きく異なる (計画立案 + 子管理 vs 実作業)。
  単純なマージはできず、共通基盤 + 役割別モジュール、もしくはコンテキスト依存の
  分岐という構成になる可能性が高い。
- 既存スキルは長期間運用されチューニングが入っている。統合過程で能力が低下する
  リスクがある。
- 機能パリティの検証手段が現状薄い。既存の supervisor / executor 挙動を保ちつつ
  統合スキルへ移行できたかを判定する仕組みが必要。
- 段階的に進める必要があり、Phase 1 の中で時間とイテレーションを最も要する見込み。

#### 進め方の方針

- 両スキルの差分を棚卸しし、共通部分と分岐部分を切り分ける。
- 分岐部分のうち真に必要なもの (例: readonly 制約下の振る舞い) は task コンテキスト
  経由で取れる情報から自動判定する。
- 旧 `boid-supervisor` / `boid-executor` スキルは互換期間中残し、新汎用スキルと
  並走させる。
- 既存プロジェクトを順次新スキルに移行し、安定確認後に旧スキルを廃止する。
- boid-kits 側 (`claude-code` キット) の `additional_bindings` に新スキルを追加して
  `~/.claude/skills` に届くようにする。これは小さな追従作業として位置付け、
  デプロイ順序は kit 側を先行させる。

#### 能力パリティの判定基準 / 撤退条件

現時点で boid には skill の挙動を定量評価する基盤がない。本計画では、パリティ判定の
理想形 (E2E ベンチ・自動回帰スイート等) を整備する前に、**「日常の開発作業が普通に
できる」程度のラフな基準で先に進める** ことを許容する。

- 主要シグナルは、移行を試みた既存プロジェクトで実作業を一定期間回し、明らかな
  劣化 (失敗の増加・user 介入頻度の上昇・skill のフリーズ等) が観測されないこと。
- 定量評価基盤の整備は本計画の範囲外。先送りすることで Track B の着手が遅れる
  リスクを避ける。
- 移行後に問題が露見した場合は、旧スキル並走期間を活用してロールバックする。これが
  本計画の「互換期間中の安全網」。
- 評価が緩いことを前提に、移行は **早めに始めて長く観測する** 方針とする。完璧な
  事前検証よりも、運用しながら問題を拾う方を優先する。

**撤退条件・タイムボックスは意図的にラフのままにする**:

- スキル統合の良し悪しは人間が定性的に判断するしかなく、定量基盤の整備も範囲外で
  あるため、明確な期限や数値的撤退閾値を置いても判断材料が無く意味を持たない。
- 撤退判断は nose の主観に委ねる。「これは厳しい」と感じた時点で計画を見直す。
  Track A の成果は Track B の進捗とは独立に残るため、 Track B 単体の撤退は
  プロジェクト全体の頓挫を意味しない。
- 期限を切らないことのリスク (ダラダラと統合作業が続く) は、旧スキル並走による
  互換維持コストでしか発生せず、上流の Track A や Phase 2 の作業を妨げない構造に
  なっている。三重メンテ期間の上限 (互換性方針セクション参照) が事実上のソフト
  リミットとして機能する。

### 4. 親 → 子リスト API

- `TaskFilter` に `ParentID *string` を追加 (`internal/orchestrator/store.go:33`)。
- `ListTasks` の SQL に parent_id 条件を追加。
- `TaskHandler.List` で `q.Get("parent_id")` を読む (`internal/api/task.go:215`)。
- 想定変更ファイル数 ~2。
- 「判定 skill が並列 N 子の結果を読み比べる」配線の前提となる。

### 5. HarnessAdapter プラグイン化 (Phase 2 の本丸)

boid 内に HarnessAdapter インタフェースを導入し、エージェントを「知る」設計に転換する。

#### 構想転換の意味

- **これまで**: boid は agent を bash と区別しない外部プロセスとして扱う。
- **これから**: boid はサポート対象エージェント (claude / codex / opencode / ...) を
  名前付きで知り、adapter プラグインを通じて細かい制御を行う。
- 当初構想に無理があった整理: 結果として SIGUSR1 等の signal 規約・session resume 細部・
  session ID 形式が boid コアにあちこち漏れている。

#### インタフェース案 (議論時点のスケッチ。Phase 2 着手時に再設計)

```go
type HarnessAdapter interface {
    // 走っている agent プロセスを止める (現状 SIGUSR1 → run-agent.py → SIGTERM)
    StopAgent(ctx context.Context, jobID string) error

    // session 再開時に start hook へ渡すフラグ・環境
    ResumePayload(sessionID string) (args []string, env map[string]string)

    // PTY 要求 (claude --print 課金枠など、harness 特有の billing/UX 制約)
    Interactive() bool

    // hook 環境から session ID を取り出す方法
    SessionIDFromHookEnv(env map[string]string) string

    // 直前の run の使用量を取得 (返却型 Usage は Phase 2 中に確定。設計判断 6 参照。
    // 最大公約数 fixed 型 / harness 固有 JSON / ハイブリッドのいずれかを選ぶ)
    Usage(ctx context.Context, jobID string) (Usage, error)

    // (将来) inter-task messaging primitive
}
```

このスケッチは実コード漏出 9 箇所 (プロトコル層 7 + 実行特性層 2) を全てカバーする
想定:

- プロトコル層 (SIGUSR1 系 7 箇所) → `StopAgent` で吸収 (boid コアは StopAgent を
  呼ぶだけ、SIGUSR1 規約は adapter 内に閉じる)
- `planner.go:84-90` (Interactive 強制) → `Interactive()` で吸収
- `planner.go:168-169` (`BOID_AGENT_SESSION_ID` env) → `ResumePayload(sessionID) → env`
  で吸収 (adapter が env キー名と意味を生成、 boid コアは generic に env を渡す)

`Usage()` は Phase 3 の token / cost 会計 (設計判断 6) の入口として Phase 2 着手時から
インタフェースに含める。返却型の確定は Phase 2 中に行うが、メソッド形だけ先に決めて
おくことで Phase 3 着手時の「会計をどこで吸収するか」の議論を省く。

trait map から session ID を直接抽出するメソッドは不要 (実コードに trait アクセスは
無く、コメント中の言及のみで cosmetic 側で処理する)。

#### 実装の進め方

- `adapters/claude/`, `adapters/codex/`, `adapters/opencode/` を 1 ファイルずつ用意。
- 既存の SIGUSR1 規約を `adapters/claude/` に閉じ込める。
- boid コアは adapter を呼ぶだけにする。
- 切り替えは実コード漏出 9 箇所 (8 ファイル) + cosmetic 漏出 4 箇所 (4 ファイル)、
  計 13 箇所 / 12 ファイル横断の本気仕事。
- まず claude adapter で既存挙動を完全再現してから、他 harness を試作する。

### 6. token / cost 会計

#### スコープ

- jobs テーブルに input_tokens / output_tokens / model / cost (任意) を追加
  (新規マイグレーション 1 本)。
- HarnessAdapter 経由で adapter 側が usage を収集して DB に書く。
- Web UI に集計表示。
- 廉価モデル並列評価ループの「コスト勘案 judge」の前提。
- 着手は廉価モデル並列評価を本格運用する直前。

#### schema 粒度・責任分界の決定タイミング

harness ごとに usage で取れる粒度は異なる:

- **claude**: messages per turn、cache hit / miss、input / output 別、model
- **codex**: 未調査
- **opencode**: 未調査

DB schema をどう切るか (最大公約数 fixed columns / 拡張可能 JSON / ハイブリッド) は
**Phase 2 着手時** に決める。 Phase 2 の HarnessAdapter インタフェース設計と並行
させることで、 Phase 3 着手時にはマイグレーション / 集計実装に直接入れる状態を作る。

Phase 2 中に詰める論点:

- HarnessAdapter の `Usage()` (仮) 返却型: 最大公約数で共通化するか、harness ごとの
  差分を JSON で保持するか
- jobs テーブルの schema 形: fixed columns 中心 / extra JSON 列での harness 固有
  データ保持 / ハイブリッド
- 各 harness で実際に取れる usage の調査 (claude / codex / opencode)

これを決めないまま Phase 3 着手すると「最大公約数 schema で書いた後に harness 固有
データの保存方針で揉める」事態を招きうる。 Phase 2 完了時点で usage 返却型と
schema 方針を確定させておく。

### 7. エージェント認識型 boid: 応用

HarnessAdapter で「boid は知ってるエージェントを起動・操作できる」状態になると、
boid 自身の操作を agent に委ねるセッションを boid 自体のために起動可能になる。本計画では
基盤までを扱い、各応用は別 plan に切り出す前提とする。

想定される応用:

- **スタックタスク救出**: 監視 agent が長時間進捗のないタスクを検出して救出指示を出す。
- **セッション分析・スキル動的改善**: 終了タスクの jsonl を読み、エージェント評価・
  skill 改善案を生成する。
- **環境駆動プロジェクト初期化**: `boid init` の代わりに init agent が user 環境を調べて
  適切な project.yaml を構成する。多くの設定を project.yaml から追い出し、user / 環境ごとに
  動的設定できるようにする。バイブコーダー向けセットアップ
  (`docs/plans/vibe-coder-provisioning.md`) の単純化につながる。
- **タスク間メッセージング**: 現状 HITL は `task answer` (awaiting 限定) / `boid attach`
  (PTY 乗っ取り) / `task reopen` (done 限定) に限られるが、adapter ベースで外部からの
  自由メッセージ差し込みを実装可能になる。

これらは「boid 自身がエージェントを知っている」ことで初めて自然に書ける機能群。

## 段階的着手案

各 phase は独立 PR セットとして扱う。phase 内の項目は並行・順次どちらでもよい。

**実装が確定しているスコープは Phase 0-2 まで**。Phase 3 以降は Phase 2 (HarnessAdapter
抽出) の実装と運用を評価したうえで、改めて計画を見直す。Phase 2 完了後に得られる知見
(adapter 抽象の使い勝手・実装コスト・他 harness 対応の現実性・既存挙動再現の難度等) に
よって、Phase 3 のスコープや優先度、Phase 4 以降の応用 plan の取捨選択が変わる可能性が
高いため、本計画では確定させない。

### Phase 0: 設計確定

本ドキュメント承認、関連メモリの整理に加え、Phase 1 着手前に以下を済ませる:

1. 設計判断 1-7 の最終承認 (本ドキュメント Status: 設計合意 → 着手可)。
2. **`boid-supervisor` / `boid-executor` の差分棚卸し** (Track B-1 を Phase 0 中に
   先行着手)。 Track B の見積もり精度を上げ、「結構むずかしいかも」のかかり具合を
   早期に評価する。棚卸し結果は本ドキュメントに追記するか別メモに残す。
3. 残課題セクションのうち「Phase 0 で詰める」とラベルされたものを処理。
4. boid-kits 側 (`claude-code` キット) のオーナーと並走スケジュール調整 (Track B
   完了時に `additional_bindings` 追加 PR が出せる状態を作る)。

### Phase 1: ロール簡素化 (3 トラック構成)

Phase 1 は依存関係の異なる 3 トラックに分ける。Track A1 は他のトラックと完全独立で
先行 PR セットとして切り出せる。Track A2 (free naming 解禁) は既存 project.yaml の
互換性解釈に影響するため、Track B (汎用スキル統合) の着地と同時にリリースする。
Track B は supervisor / executor の枠を残したまま検証でき、Track A2 と独立に進める
ことができる。

#### Track A1: コア改修・互換破壊なし (先行 PR セット)

互換性に一切影響せず、Track B の進捗を待たずに先行リリース可能なトラック。
supervisor / executor を使う既存 project.yaml はそのまま動作する。

1. readonly create 時上書き復活 (~4 ファイル)。`CreateTaskRequest` に
   `Readonly *bool` を追加し、`deprecatedTaskRowFields` /
   `deprecatedTaskRowSpecFields` から `"readonly"` を外す。
2. parent_id で children listing (~2 ファイル)。`TaskFilter` に `ParentID` を
   追加し、HTTP API (`TaskHandler.List`) でも露出する。「判定 skill が並列 N 子の
   結果を読み比べる」配線の前提となる。

#### Track A2: free naming 解禁 (Track B と同時着地)

1. canonical name 廃止 + 任意 behavior 名の受付。`applyCanonicalBehaviorOverrides`
   削除、readonly 既定値 false 化。
2. **`default_task_behavior` トップレベルキー導入** + CreateTask での省略時 default
   引き実装。`supervisor` 存在時は暗黙的 default にする fallback を含む (設計判断 1
   参照)。
3. 既存 supervisor / executor 名は両対応 (互換期間中の deprecation warning ポリシー
   は「互換性方針」セクション参照)。

Track A2 を Track B 完了前にリリースすると、ユーザが free name の behavior を
書いてしまったとき汎用スキルがなく動作が破綻する。必ず Track B と同時着地にする。

#### Track B: 汎用スキル統合 (本計画の最大不確実性)

Track B 自体の検証は supervisor / executor の枠を残したまま実行できる。
canonical name で動かしたまま、内部の skill 実装だけ汎用 1 本に差し替え、
日常の開発作業が回るかを観測する形になる。Track A2 を待つ必要はない。

1. `boid-supervisor` / `boid-executor` の差分棚卸し (Phase 0 で着手可、設計判断 3
   参照)。
2. 共通基盤 + 役割別モジュール、または完全汎用 1 本への統合方針確定。
3. **動的 instruction 生成パターン** を汎用スキルのデフォルト挙動として実装
   (ルート = project.yaml 固定、子孫 = 親が動的生成。設計判断 3 参照)。
4. **`boid-orchestrate` の拡張**: CLI / Web UI Commands ボタン両対応 (boid-discuss
   の機能吸収) + 動的 instruction / readonly 生成への対応。
5. 段階的実装と並走検証 (旧スキルと新汎用スキルを互換期間中並列維持)。
6. 既存プロジェクトの順次移行、安定確認後に旧スキル廃止。
7. **Legacy 廃止**: `boid-plan` / `boid-sandbox` / `boid-discuss` を削除。Web UI
   側の Commands ボタン配線を `boid-orchestrate` 呼び出しに切り替える追従を含む。
8. boid-kits の `claude-code` キット側 `additional_bindings` に新スキルを追加
   (kit 先行デプロイ)。

Track B は「結構むずかしいかも」という見立てに従い、見積もりにバッファを取り、
能力低下の兆候を検出した時点で計画を見直す前提で進める。能力パリティの判定基準
は「設計判断 3」を参照。

### Phase 2: HarnessAdapter 抽出 (本気の仕事)

1. HarnessAdapter インタフェース定義 (**usage 返却型を含む** — Phase 3 の token
   会計 schema 方針も Phase 2 中に確定させる、設計判断 6 参照)。
2. `adapters/claude/` に既存 SIGUSR1 規約・session resume を閉じる。
3. boid コアから claude 固有コードを撤去 (実コード漏出 9 箇所 / 8 ファイルを adapter
   呼び出しに置換、cosmetic 漏出 4 箇所 / 4 ファイルのコメント書き換え。計 12
   ファイル横断)。
4. 各 harness の usage 粒度調査 (claude / codex / opencode) と jobs テーブル
   schema 方針の確定 (Phase 3 着手前提条件)。
5. (任意) codex adapter / opencode adapter 試作。

---

以下は Phase 2 評価後に再計画する想定。スコープ・優先度・実施可否はその時点で判断する。

### Phase 3 (暫定): token / cost 会計

1. jobs テーブル拡張 (Phase 2 で確定した schema 方針に基づく)、adapter で usage 収集、
   UI 集計。

### Phase 4 以降 (暫定): エージェント認識型応用 (別 plan に切り出す)

- スタックタスク救出 plan
- 環境駆動 init plan (`docs/plans/vibe-coder-provisioning.md` との統合検討)
- 動的 skill 改善 plan
- タスク間メッセージング plan

## 互換性方針

### 互換対象と廃止依存関係

各互換対象は、それぞれ前提条件が違う。一律ではなく対象ごとに判定する。

| 対象 | 互換中の振る舞い | 廃止前提 |
|---|---|---|
| canonical name (`supervisor` / `executor`) | project.yaml で従来通り書ける + deprecation warning 出力 | Track A2 リリース後、free naming で全 project が動作確認できていること |
| `default_task_behavior` 未指定 + `supervisor` 暗黙 fallback | yaml の default 省略時に supervisor が引かれる | canonical name と同時 (supervisor が yaml に書けなくなったら自然消滅) |
| 旧埋込スキル (`boid-supervisor` / `boid-executor`) | `internal/skills/data/` に残置、behavior 名で bind 可 | Track B 移行完了、汎用スキルで日常作業が回ること |
| `boid-discuss` | Web UI Commands ボタンから呼ばれる | `boid-orchestrate` 拡張完了 + Web UI 配線追従完了 |
| `boid-plan` / `boid-sandbox` | レガシー dispatch / 旧名 alias | 単独廃止可 (既に deprecated 扱い) |

### 出口条件

各対象の互換コードを削除する判定基準:

- **観測シグナル**: nose 管理下の各 project で、新スキル / 新設定により日常開発作業が
  **1 週間** 問題なく回ること。1 週間で答えが出る (劣化があればほぼ確実に検知できる)
  想定。これが定量基盤の代わりの一次シグナル。
- **削除タイミング**: 出口条件を満たしたら **定期 release を待たず即削除 PR** を出す。
  release 前にもう一度動作確認を行う。
- **判定者**: nose (定量基盤を持たない前提)。
- **時間下限**: deprecation warning を出してから少なくとも 1 週間以上経過してから削除
  する (即削除でも warning 期間ゼロにはしない)。

### deprecation warning と移行ガイド

- Track A2 着地時 (free naming 解禁時) に deprecation warning を `task_behaviors`
  解釈時に出力する。例: behavior 名が canonical (supervisor / executor) の場合のみ
  "deprecated, see docs for migration" の WARN ログ。Track A1 単独では canonical
  name の意味論は不変のため、警告は出さない。
- **発火頻度**: project reload / daemon 起動時に project ごと **1 回だけ** 出す
  (タスク作成のたびに出さない)。 noise を抑え、ユーザが気付ける程度の頻度に保つ。
- **抑止手段**: 環境変数 `BOID_NO_DEPRECATION_WARN=1` で完全抑止可能。互換期間が
  長引いてユーザが warning に飽きた場合の逃げ道。
- **強制再有効化**: 互換コード削除 PR の **1 週間前** から `BOID_NO_DEPRECATION_WARN`
  を無視して強制表示に切り替える (削除予告フェーズ)。これで「気付かないうちに動かなく
  なった」を防ぐ。
- `docs/ja/reference/` 配下に移行ガイドを追加する。
- 移行テスト: canonical name 経由でも free naming 経由でも同等に動くこと、を回帰
  テストで保証する。

### 既存ユーザ・並走運用への配慮

- 既存 project.yaml (supervisor / executor を使うもの) は **当面動作させる**。
  気に入って使っている同僚ユーザがいるため、即 break しない。
- **埋込スキルにも互換期間を取る**。Track B 完了までは旧 `boid-supervisor` /
  `boid-executor` を残し、新スキルと並走させ、プロジェクト単位で切り替え可能と
  する。boid-kits の `claude-code` キットには新旧両方の `additional_bindings` を
  同時に置く。
- 互換期間中の三重メンテ (旧 supervisor + 旧 executor + 新汎用) は、上記出口条件
  により最大 1-2 週間 + α で収束させる。**α の上限は 4 週間 (合計最大 6 週間)** とし、
  これを超える場合は本 plan を再開して延長判断 (撤退・スコープ縮小・追加検証) を
  行う。長期化させない。

## 残課題 / 未決

優先度ラベル:
- **[Phase 0]**: 本 plan 着手前に詰める
- **[Track A]**: Track A 着手時に詰める
- **[Track B]**: Track B 着手時に詰める
- **[Phase 2]**: HarnessAdapter 抽出着手時に詰める
- **[別 plan]**: 本 plan の範囲外、別 plan に切り出す

- **[Track B]** **`boid-supervisor` / `boid-executor` 統合 (Track B) の具体方針**。
  共通基盤 + 役割別モジュール構成か、コンテキスト依存分岐の汎用 1 本か。周辺スキル
  の境界整理は設計判断 3「周辺スキル棚卸し結果」で確定済 (Track B のスコープに統合
  + Legacy 廃止 + orchestrate 拡張を含む)。
- **[Track B]** 新スキルの命名 (`boid-task` 候補、他案も含めて Track B 着手時に決める)。
- **[Track B]** スキル統合に伴う既存プロジェクトの移行手順 (旧 / 新を behavior 名で
  切り替える運用ガイド等)。
- **[Track A]** 既存テストで canonical name 前提のものをどう扱うか (両対応テスト追加か、
  新名称への置換か)。
- **[Phase 2]** HarnessAdapter のインタフェース詳細 (上記スケッチは Phase 2 着手時に
  再設計)。`Usage()` の返却型確定もここに含む。
- **[Phase 2]** daemon-restart-resume の既知 E2E flake との関係 — HarnessAdapter 化で
  session continuation の意味論を整理する余地があるか。
- **[別 plan]** inter-task messaging を adapter 経由でどう実装するか (具体仕様)。
- **[別 plan]** 環境駆動 init agent の具体設計。
- **[別 plan]** 廉価モデル並列評価のワークフロー primitives (どこまで boid コアで提供
  するか、特に judge 結果集約 API)。
