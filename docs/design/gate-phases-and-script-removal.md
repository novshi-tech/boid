# Gate Phases と Script 機構の廃止 — 設計・実装計画

## 位置付け

本ドキュメントは単一の大規模変更を複数の実装セッションに分割して実行するための設計・計画書である。実装を担当するセッションは本書をエントリポイントとして、合意済みの決定事項・アーキテクチャ・フェーズ分割・テスト戦略をここから再構築できるように書かれている。

前提となる直近の変更:

- `edb8b63` (2026-04-11): 統合 state machine (DefaultMachine) に一本化。`OneShotMachine` / `OneShotFeedbackMachine` / `FeedbackLoopMachine` を廃止し、`tasks` trait の有無を駆動シグナルとする。
- `e7c265f` (2026-04-12): plan タスクの `TasksReady → done` ショートカットを撤廃し、`executing → verifying → done` パスに統一。`tasks` trait を `artifact` trait と対称に扱うよう `DefaultMachine` の executing 自動遷移を変更。

本ドキュメントの変更は上記の延長線にあり、とくに `e7c265f` によって顕在化した「plan バウンス時の done.py 退行バグ」と、それ以前から存在した「auto-merge の timing 順序バグ」を併せて解消する。

## 1. 背景と動機

### 1.1 現状アーキテクチャの概要

boid の state machine では現在、以下の3種類のハンドラが task 遷移に関与する。

| ハンドラ種別 | 発火タイミング | 並列性 | 実行コンテキスト | 担当ファイル |
|---|---|---|---|---|
| Hook | 状態内の主作業 (dispatch cycle) | sequential (readonly 以外) | sandbox + worktree lock | `internal/orchestrator/coordinator.go` |
| Gate | hook 完了後、advance 直前 | 常に parallel | sandbox, lock なし | 同上 |
| Script | `task_done` / `task_aborted` イベント | 独立 ephemeral task | 別 dispatch context | `internal/orchestrator/spec_scripts.go`, `internal/api/service.go` `fireScriptTriggers` |

hook と gate は `runDispatchLoop` 内で `Coordinator.DispatchAndAdvance` が順に実行する (`internal/orchestrator/coordinator.go:30-114`)。script は advance 完了後の後処理として `runDispatchLoop` の終端で `fireScriptTriggers` から起動される (`internal/api/service.go:766-767, 793-794`)。

### 1.2 現状の問題群

#### 問題 A: done.py gate の plan refactor 退行

`boid-kits/boid-tasks/gates/done.py` は `on: [executing]` + `consumes: [tasks]` で宣言された gate で、`tasks` trait を読んで `boid task create` でサブタスクを量産する。

`e7c265f` で plan タスクが `executing → verifying → done` パスを通るようになり、verifying で reviewer hook/gate がバウンス要因になれるようになった。このとき以下のシナリオが発生しうる:

```
1. plan executing → claude-code hook が tasks 書き出し
2. done.py gate (on: [executing]) 発火 → 古い分解でサブタスク作成 ← ①
3. advance → verifying
4. reviewer が finding 書き出し: verifying → reworking
5. claude-code hook (on: [executing, reworking]) 再発火 → tasks 更新 ← ②
6. reworking (self-loop until resolved) → done
```

②の時点で done.py は発火しない (`on: [executing]` のみ)。結果、①で作られた古い tasks を元にしたサブタスクだけが残り、reviewer が要求した修正が反映されない。**これは `e7c265f` で導入した reviewer サポートの価値を実質打ち消している退行である。**

#### 問題 B: auto-merge の timing 順序バグ

`boid-kits/github-auto-merge/scripts/auto-merge.sh` は `task_done` で発火し、`gh pr merge` と `boid task update` (artifact.auto-merge.pr.merged の書き込み) を行う。現状の `runDispatchLoop` での順序:

```
advance → done (payload に artifact.auto-merge.pr.merged はまだない)
  ↓
triggerDependentTasks(parent)  ← 不完全な payload で評価 (1回目)
  ↓
fireScriptTriggers → auto-merge script 起動
  ↓
auto-merge が gh pr merge して boid task update で artifact.auto-merge.pr.merged を書く
  ↓
UpdateTask 内で go TriggerDependents(parent) が再起動 (service.go:403-405)
  ← 完全な payload で評価 (2回目)
```

`depends_on_payload: "artifact.auto-merge.pr.merged"` を待つ兄弟タスクは1回目で弾かれ、2回目で通る。ぎりぎり動いているが、`triggerDependentTasks` が冪等な副作用を前提にした二重評価になっており、失敗時の可視性も低い。

興味深いことに、現行の `internal/api/service.go:390` のコメントは auto-merge を **「gate」** と呼んでいる。実装は script なのに名称が gate であることが、この違和感を過去の自分が言語化しきれなかった痕跡である。

#### 問題 C: 外部データ前処理パターンの表現不足

「Jira / Linear / GitHub から課題を取得し、その内容を元に agent が boid タスクを生成する」という pattern は boid の汎用オーケストレータとしての典型的ユースケースだが、現行アーキテクチャでは居場所がない。

- **hook に詰め込む**: agent hook が Jira fetch と agent 呼び出しを両方やる。関心分離ができず、host_command 設定や権限管理が雑になる。
- **別タスクに分離**: 収集タスクと生成タスクを `depends_on` で繋ぐ。表現は可能だが、「1つの仕事」が2つの task に分裂するので直感的でない。
- **gate で前処理**: 既存 gate は hook の**後**に走るので、agent hook の入力として使えない。

つまり「state に入った時点で、hook を動かす前に外部データを取り込んで payload に載せる」という場所が設計上存在しない。

#### 問題 D: script / ephemeral task 機構の複雑さ

script を実現するため、以下のコード群が boid 本体に存在する:

- `internal/orchestrator/spec_scripts.go` — `MatchScripts`, `BuildTriggeredScriptTask`, `ScriptFilter`, `ScriptTrigger`, `BuildScriptTask`, script script resolver
- `internal/orchestrator/spec_types.go` — `Script`, `ScriptTrigger`, `ScriptFilter`, `ValidScriptTriggerValues`
- `internal/api/service.go` — `fireScriptTriggers` とその call site 2 箇所
- `internal/orchestrator/model.go` — `Task.Ephemeral`
- `internal/orchestrator/script_trigger_test.go` — script 関連のテスト
- sandbox protocol — ephemeral 関連フィールド

これらが存在するのは「hook/gate とは別のタイミングで動く何か」が必要だったからだが、本来その「別のタイミング」とは単に `state entry` / `state exit` の2つのフェーズに過ぎない。state machine の古典的な概念で吸収できる。

#### 問題 E: rework 後の verifying 再検証の欠如

現行 `DefaultMachine` では reworking → done に直接遷移する (`NoUnresolvedFindings()` が真のとき)。これは「rework 後に verifying 状態の exit gates を再実行する」ことを不可能にしている。

- 問題 A の plan バウンス時に mergeable-check 等の「verifying 状態で行うべき検証」を rework 後に再実行したくてもできない
- reviewer hook を `on: [verifying]` で書いても、rework 後は素通りになるので「修正後の再レビュー」が自然に表現できない

この制約は元々、状態遷移を最短にする意図だったが、結果として verifying 状態の検証層としての力を弱めている。

### 1.3 概念的な根拠

UML statechart および Moore machine では、状態に対する action を以下の3種類に分類する。

- **entry action**: 状態に入ったときに1回実行される
- **do activity** (state activity): 状態に留まっている間に実行される
- **exit action**: 状態を離れるときに1回実行される

boid の現行ハンドラはこのモデルに自然にマップできる:

| FSM 概念 | boid 対応 |
|---|---|
| entry action | (現状存在しない → **新設する**) |
| do activity | hook |
| exit action | gate (現行の gates) |
| post-transition effect | script (現状) → entry action on destination state (提案) |

script が本質的に「別の状態機械インスタンスでの ephemeral 実行」を必要としないケース (auto-merge, done.py, Jira fetch 等すべて) は、destination state の entry action として表現するのが自然である。

## 2. 設計方針

### 2.1 Gate に phase フィールドを導入する

- 既存の `Gate` struct に `Phase` フィールドを追加
- 値は `"entry"` または `"exit"`、省略時は `"exit"` (後方互換)
- kit.yaml / project.yaml 上での記法:

```yaml
gates:
  - id: pr-verify
    on: [executing, reworking]
    phase: exit  # 省略時のデフォルト
    traits:
      consumes: [artifact]
      produces: [verification]

  - id: fetch-jira
    on: [executing]
    phase: entry
    traits:
      produces: [artifact]

  - id: auto-merge
    on: [done]
    phase: entry
    traits:
      produces: [artifact]
```

### 2.2 Entry gate は遷移直後、hook dispatch 前に発火する

- `runDispatchLoop` で advance が新状態を返した直後に `DispatchEntryGates` を呼ぶ
- entry gate の出力は payload にマージされ、次サイクルの hook/exit gate から見える
- 同じ dispatch cycle 内で「hook → exit gate → advance → entry gate (新状態)」の流れになる

### 2.3 Self-loop では entry gate を skip する

- `reworking → reworking` のような self-loop は「状態を離れていない」ので entry gate を再発火しない
- 実装上は `prevStatus == newStatus` での早期リターン

### 2.4 Done / Aborted 到達後も 1 回だけ entry gate が走る

- 現状の `runDispatchLoop` は advance で terminal に到達すると `return` で即終了する
- 修正: terminal 到達時も entry gate を走らせてから `triggerDependentTasks` を呼ぶ
- terminal 到達後の「次サイクルの hook dispatch」は走らせない (terminal は依然として「do activity なし」)

### 2.5 Script 機構は完全に廃止する

- boid 本体から `fireScriptTriggers`, `MatchScripts`, `BuildTriggeredScriptTask`, `ScriptFilter`, `ScriptTrigger`, `Script` 構造体を削除
- `Task.Ephemeral` も撤去 (script 専用フラグだったため)
- kit.yaml schema から `scripts:` セクションを削除
- 既存 script を使っていた kit は entry gate に移行する (本書 Phase 3)

### 2.6 Gate は純粋に宣言的な契約 (stdin → stdout)

- gate の中から `boid task reopen` / `boid task update` 等の action 系 CLI を呼ばせない
- gate は stdin から task JSON を受け取り、stdout に `payload_patch` を出すだけ
- 状態遷移の decision は **state machine が唯一の authority**
- gate は `next_action` のような transition 要求も発行しない。状態を動かしたければ payload (特に `verification.findings`) を書くだけ。あとは state machine が condition を評価して transition する
- 例外: `gh pr merge`, `gh pr view` のような「外部システムへの副作用/参照」は引き続き許可される (これは boid の状態遷移には影響しない)

この宣言的契約は entry gate と exit gate に等しく適用される。phase は発火タイミングが違うだけで、契約は同一。

### 2.7 State machine 側の変更: `reworking → verifying`

現行の `DefaultMachine` は reworking → done に直接遷移するが、これを **reworking → verifying** に変更する。

```go
// Before
{FromStatus: "reworking", ToStatus: "done", Condition: NoUnresolvedFindings()},

// After
{FromStatus: "reworking", ToStatus: "verifying", Condition: NoUnresolvedFindings()},
```

これは一見 entry gate と独立した変更に見えるが、以下の設計効果を生む:

1. **verifying が「done 前の必須関門」として定義される**: rework 後も必ず通過する
2. **verifying exit gate が rework 後に自然に再実行される**: mergeable-check のような verification を verifying 退場のみに置けば、rework 後に自動的に再走する
3. **reviewer hook を `on: [verifying]` で書いても rework 後に再レビューされる**: reviewer agent が「修正内容を再評価する」という自然なフローになる
4. **状態遷移が対称化される**: `work → verify → done` という単一パターンを executing 起点にも reworking 起点にも適用できる

verifying に exit gate も hook も存在しないタスクは、現行通り pass-through で即座に done に抜けるので、機能的な退行はない。コスト増は 1 dispatch cycle のみ。

この変更は独立したコミットとして先行させる (本書 Phase 0)。entry gate 機構導入のための前提となるが、単独でも意味のある改善である。

## 3. データ構造の変更

### 3.1 Gate struct (`internal/orchestrator/spec_types.go`)

```go
type GatePhase string

const (
    GatePhaseEntry GatePhase = "entry"
    GatePhaseExit  GatePhase = "exit"
)

type Gate struct {
    ID         string        `yaml:"id" json:"id"`
    On         OnValues      `yaml:"on" json:"on"`
    Phase      GatePhase     `yaml:"phase,omitempty" json:"phase,omitempty"` // default: "exit"
    Behavior   string        `yaml:"behavior,omitempty" json:"behavior,omitempty"`
    Traits     HandlerTraits `yaml:"traits" json:"traits"`
    Kit        string        `yaml:"-" json:"kit,omitempty"`
    ScriptPath string        `yaml:"-" json:"-"`
}
```

YAML unmarshal 時に空文字列の場合は `GatePhaseExit` にフォールバックする。

### 3.2 HandlerResult struct

既存の `HandlerResult` は変更不要。gate の出力は従来通り `PayloadPatch` のみで表現する。

### 3.3 EntryGateResult 型 (新設)

entry gate 用の coordinator 戻り値を別型として定義する。`DispatchResult` と混同しないようにするため。

```go
// coordinator.go
type EntryGateResult struct {
    Results      []HandlerResult
    FinalPayload json.RawMessage
}
```

`NextAction` / `RequestedAction` のようなフィールドは**持たない**。entry gate は payload_patch だけを返す宣言的契約。

### 3.4 削除する構造体 (Phase 4)

```go
// すべて削除:
// internal/orchestrator/spec_types.go
type Script struct { ... }
type ScriptFilter struct { ... }
type ScriptTrigger string
const (
    ScriptTriggerTaskDone    ScriptTrigger = "task_done"
    ScriptTriggerTaskAborted ScriptTrigger = "task_aborted"
)
var ValidScriptTriggerValues = ...

// internal/orchestrator/spec_scripts.go (ファイル全体またはほぼ全体)
func MatchScripts(...) ...
func BuildTriggeredScriptTask(...) ...
func BuildScriptTask(...) ...

// internal/orchestrator/model.go
type Task struct {
    // ...
    Ephemeral bool // 削除
}
```

`ResolveHookScript` / `ResolveGateScript` は残す (hook/gate の script path resolver として必要)。`ResolveScriptScript` は削除。

### 3.5 Kit YAML schema (`KitMeta` in `spec_types.go`)

```go
type KitMeta struct {
    TaskBehaviors      map[string]TaskBehavior `yaml:"task_behaviors"`
    Hooks              []Hook                  `yaml:"hooks"`
    Gates              []Gate                  `yaml:"gates"`
    // Scripts            []Script                `yaml:"scripts"` ← 削除
    BuiltinCommands    []string                `yaml:"builtin_commands"`
    HostCommands       HostCommands            `yaml:"host_commands"`
    // ...
}
```

`ProjectMeta.Scripts` および spec_loader での script 読み込みロジックも削除する。

## 4. Payload 上の形式

### 4.1 Gate output protocol

gate のプロセスは stdout に以下の YAML (または JSON) を吐く:

```yaml
payload_patch:
  # 任意の trait の書き込み。既存 gate と同じ
  artifact:
    pr:
      number: 42
      merged: true
```

`payload_patch` のみ。`next_action` 等の追加フィールドはなし。entry gate と exit gate は同じプロトコル。

gate は payload_patch を書かずに何もしないことも許可する (エラーを `verification.findings` に書くだけでも良い)。

### 4.2 典型例: auto-merge の 2 gate 構成

auto-merge 機能は 2 つの gate に分離される。

#### 4.2.1 mergeable-check (verifying 退場 exit gate)

```yaml
# github-auto-merge/kit.yaml
gates:
  - id: mergeable-check
    on: [verifying]
    phase: exit  # 省略可
    behavior: dev
    traits:
      produces: [verification]
```

処理内容:
- `gh pr view --json mergeable` で PR の mergeable ステータスを取得
- `CONFLICTING` なら conflict finding を書き出す
- それ以外 (`MERGEABLE`, `UNKNOWN`) では自身の verification subkey を空で更新する (古い conflict finding をクリアする効果)

conflict 検出時の出力:

```yaml
payload_patch:
  verification:
    mergeable-check:
      findings:
        - message: |
            PR #42 が base ブランチとマージコンフリクトしています。
            worktree で以下を実行してコンフリクトを解消してください:
              1. git merge origin/main
              2. コンフリクトを解消
              3. git add <files> && git commit
          status: open
```

この finding は `source_state=verifying` が `injectSourceState` (coordinator 側) で付与される。verifying 自動遷移ルール `AnyFindingUnresolvedForState("verifying") → reworking` により、task は reworking に戻る。

conflict 解消後の再実行:
- `reworking → verifying` (§2.7 の変更) により、rework 完了後に再び verifying 状態になる
- mergeable-check が再実行される
- mergeable 状態であれば自身の subkey を空 findings で上書きし、結果として verifying → done に抜ける

#### 4.2.2 auto-merge (done 入場 entry gate)

```yaml
# github-auto-merge/kit.yaml (続き)
gates:
  - id: auto-merge
    on: [done]
    phase: entry
    behavior: dev
    traits:
      produces: [artifact]
```

処理内容:
- `gh pr merge --merge --delete-branch` を実行
- 成功時: `artifact.auto-merge.pr.merged=true` を payload に書き込む
- 失敗時 (稀なレース: verifying 退場後に base ブランチへ別 commit が landed した): `artifact.auto-merge.pr.merged=false, error="late_conflict"` を書き込んで終了。task は done のまま。手動介入で対応する

成功時の出力:

```yaml
payload_patch:
  artifact:
    pr:
      number: 42
      url: https://github.com/org/repo/pull/42
      merged: true
```

失敗時 (late conflict) の出力:

```yaml
payload_patch:
  artifact:
    pr:
      number: 42
      url: https://github.com/org/repo/pull/42
      merged: false
      error: late_conflict
```

レースウィンドウについての議論は §10.3 参照。

### 4.3 典型例: Jira fetch (executing 入場 entry gate)

```yaml
# jira-collector/kit.yaml
gates:
  - id: fetch-jira
    on: [executing]
    phase: entry
    behavior: jira-ingest  # 例: 専用 behavior
    traits:
      produces: [artifact]
```

処理内容:
- `jira search` 等の CLI で対象 issue を取得
- 結果を `artifact.jira.issues` として書き込む

出力:

```yaml
payload_patch:
  artifact:
    jira:
      fetched_at: "2026-04-12T10:00:00Z"
      issues:
        - key: PROJ-123
          title: "..."
          description: "..."
        - key: PROJ-124
          title: "..."
```

同じ dispatch cycle 内で claude-code hook (または専用 hook) が走り、payload の `artifact.jira.issues` を読んで `boid task create` する。

### 4.4 典型例: create-subtasks (done 入場 entry gate)

```yaml
# boid-tasks/kit.yaml
gates:
  - id: create-subtasks
    on: [done]
    phase: entry
    behavior: [plan, auto_plan]  # ※ Gate.Behavior list 対応が必要 (Phase 3)
    traits:
      consumes: [tasks]
```

処理内容:
- 親タスクの payload から `tasks` trait を読む
- 各要素について `boid task create` を呼ぶ (外部副作用)
- payload_patch は空で返す

plan タスクは verifying → reworking (reviewer 指摘時) → verifying → done というバウンスを経て最終的に done に到達し、**その時点の最新の tasks trait でのみ** subtasks が生成される。reviewer の指摘を反映した分解で生成されるため、問題 A (§1.2) の退行が解消される。

## 5. Coordinator と Dispatch Loop

### 5.1 Coordinator.DispatchEntryGates

新メソッド。既存の `DispatchAndAdvance` に対して entry gate 専用の軽量版を追加する。

```go
// DispatchEntryGates runs entry-phase gates for the given task's current status.
// Unlike DispatchAndAdvance, this does NOT evaluate hooks/exit-gates or call sm.Advance.
// The returned result reflects only entry gate payload patches.
func (d *Coordinator) DispatchEntryGates(
    ctx context.Context,
    task *Task,
    meta *ProjectMeta,
) (*EntryGateResult, error) {
    matchedGates := d.Evaluator.EvaluateGates(task, meta.Gates, GatePhaseEntry)
    if len(matchedGates) == 0 {
        return &EntryGateResult{FinalPayload: task.Payload}, nil
    }

    gateResults, err := d.dispatchGates(ctx, task, matchedGates)
    if err != nil {
        return nil, fmt.Errorf("entry gate dispatch: %w", err)
    }

    payload := task.Payload
    exclusiveWriters := map[string]string{}
    for _, gr := range gateResults {
        if err := checkExclusiveCollision(gr.PayloadPatch, gr.ID, exclusiveWriters); err != nil {
            return nil, err
        }
        if len(gr.PayloadPatch) > 0 && string(gr.PayloadPatch) != "{}" {
            gr.PayloadPatch = injectSourceState(gr.PayloadPatch, string(task.Status))
            merged, err := MergePayloadPatch(payload, gr.PayloadPatch, gr.ID, gr.allowedTraitsFromGates(matchedGates))
            if err != nil {
                slog.Warn("entry gate payload merge failed", "gate_id", gr.ID, "error", err)
                continue
            }
            payload = merged
        }
    }

    return &EntryGateResult{
        Results:      gateResults,
        FinalPayload: payload,
    }, nil
}
```

`dispatchGates`, `injectSourceState`, `MergePayloadPatch`, `checkExclusiveCollision` は既存実装を流用する。

注目すべき点: action request の集約処理は**存在しない**。entry gate は純粋に payload_patch を書くだけ。

### 5.2 Evaluator.EvaluateGates の phase 対応

既存の `EvaluateGates` を phase 引数対応にリファクタする。

```go
func (e *Evaluator) EvaluateGates(task *Task, gates []Gate, phase GatePhase) []Gate {
    activeTraits, _ := ActiveTraitTypes(task.Payload)
    traitSet := make(map[TraitType]bool, len(activeTraits))
    for _, t := range activeTraits {
        traitSet[t] = true
    }

    var matched []Gate
    for _, g := range gates {
        gPhase := g.Phase
        if gPhase == "" {
            gPhase = GatePhaseExit // default
        }
        if gPhase != phase {
            continue
        }
        if !g.On.Contains(string(task.Status)) {
            continue
        }
        if g.Behavior != "" && g.Behavior != task.Behavior {
            continue
        }
        if !hasAllTraits(traitSet, g.Traits.Consumes) {
            continue
        }
        matched = append(matched, g)
    }
    return matched
}
```

既存の `DispatchAndAdvance` 内の gate 評価呼び出しは `GatePhaseExit` を明示する。

### 5.3 runDispatchLoop の書き換え (`internal/api/service.go:727-801`)

現状の loop:

```go
func runDispatchLoop(...) {
    for cycle := 0; cycle < maxCycles; cycle++ {
        result := DispatchAndAdvance(current, ...)
        // persist
        if result.NewStatus == "" {
            if terminal(current.Status) { finalize() }
            return
        }
        current.Status = result.NewStatus
        persist(current)
        if terminal(current.Status) {
            triggerDependents()
            fireScriptTriggers()
            return
        }
    }
}
```

新実装:

```go
func runDispatchLoop(...) {
    for cycle := 0; cycle < maxCycles; cycle++ {
        result := Coordinator.DispatchAndAdvance(ctx, current, meta, sm)
        // persist payload (hook + exit gate 結果)

        if result.NewStatus == "" {
            // 今サイクルで遷移なし。loop 終了
            if terminal(current.Status) {
                triggerDependents(current.ID)
            }
            return
        }

        prevStatus := current.Status
        current.Status = result.NewStatus
        persistStatus(current)

        // self-loop は entry gate を skip
        if prevStatus != current.Status {
            entryResult, err := Coordinator.DispatchEntryGates(ctx, current, meta)
            if err != nil {
                slog.Error("entry gate dispatch failed", "task_id", current.ID, "error", err)
                recordDispatchError(current.ID, err)
                return
            }
            if len(entryResult.FinalPayload) > 0 {
                persistPayload(current, entryResult.FinalPayload)
            }
        }

        cleanupWorktree(current.ID, current.ProjectID, current.Status)

        if terminal(current.Status) {
            // terminal 到達。entry gate は既に走っている
            if Lifecycle != nil { CleanupTaskWindow(current.ID) }
            triggerDependents(current.ID)
            return
        }
        // terminal でないなら次 cycle へ (新状態の hook + exit gate を走らせる)
    }
    slog.Warn("dispatch loop max cycles reached", ...)
}
```

要点:

- advance が新状態を返したら直後に entry gate を dispatch
- entry gate の結果は payload に merge して persist
- self-loop (prevStatus == newStatus) では entry gate skip
- terminal 到達後は `triggerDependentTasks` のみ (scripts は廃止、entry gate は terminal 到達時も 1 回走る)
- `sm.Apply` の追加呼び出しはない。state 遷移は `DispatchAndAdvance` 内の advance か `ApplyAction` (manual action) 経由のみ

### 5.4 max cycles について

max cycles は現状 10 のまま。新しい遷移パターンで最悪サイクル数は以下:

- 通常フロー: pending → executing → verifying → done (3 cycle)
- CI 失敗: executing → reworking → verifying → done (4 cycle)
- Conflict 検出: executing → verifying → reworking → verifying → done (5 cycle)
- 複数 rework: 追加 rework ごとに 2 cycle 増加

max 10 cycle あれば rework 3 回程度までは吸収できる。実務上 rework が 4 回以上連続するケースは agent の修正能力の限界を超えているので、そこで停止してユーザに通知するのが正しい挙動。

### 5.5 Worktree lock と entry gate

entry gate は exit gate と同様、worktree lock を取らない並列実行とする (`dispatchGates` が lock 外で動いている)。これは以下の理由による:

- entry gate は典型的に read-only な外部取得 (Jira fetch) か、外部システムへの side effect (gh pr merge) であり、worktree 内のファイルを触らない
- 将来 worktree を触る entry gate が必要になったら、その時に opt-in フラグを追加する

## 6. State Machine との関係

### 6.1 DefaultMachine の変更 (Phase 0 で実施)

`internal/orchestrator/machine.go` の `DefaultMachine()` で 1 行変更:

```go
// Before
{FromStatus: "reworking", ToStatus: "done", Condition: NoUnresolvedFindings()},

// After
{FromStatus: "reworking", ToStatus: "verifying", Condition: NoUnresolvedFindings()},
```

self-loop ルールと doc comment は併せて更新。

### 6.2 この変更のテスト影響

既存テストの更新 (Phase 0 で実施):

- `TestDefaultMachine_Reworking_AllResolved_Done` → `_Verifying` に改名、expected = verifying
- `TestDefaultMachine_Reworking_NoFindings_Done` → 同上の改名・期待値変更
- `TestDefaultMachine_Reworking_OpenFindings_SelfLoop` → 変更不要 (self-loop は変わらない)
- `TestDefaultMachine_Reworking_MixedSourceStates_AnyOpenBlocksDone` → 変更不要

新規テスト:

- `TestDefaultMachine_Reworking_AllResolved_TransitionsToVerifying` (改名後のテスト)
- `TestDefaultMachine_FullRework Cycle_ExecutingReworkingVerifyingDone`: integration 的に complete cycle を確認
- `TestDefaultMachine_ConflictResolutionCycle`: verifying → reworking → verifying → done の cycle

### 6.3 Entry gate 導入後の state machine への影響

entry gate 機構の追加は DefaultMachine 自体を変更しない。entry gate は state machine の外で動き、transition の決定は常に state machine に委ねられる。

entry gate が payload に書き込んだ内容 (例: verification findings) は、**次の dispatch cycle** で hook/exit gate が見て、exit gate の出力と併せて advance 判定される。この流れは現行 exit gate と同じ仕組みで、追加の machine 変更は不要。

### 6.4 Done は引き続き terminal state である

- done を非 terminal にして「finding あり → reworking」のような auto-transition ルールを追加することは**しない**
- done へ戻る条件は manual action `reopen` (TUI / CLI 経由) のみ
- entry gate on done も transition を要求しない。実行失敗時は artifact に error を書いて終了するだけで、task は done のまま

これにより「done は終わった状態」という直感的セマンティクスを保つ。rare case (auto-merge の late conflict race) は手動介入で処理する。

## 7. 実装フェーズ

各フェーズは独立したコミット単位として実行可能。フェーズ境界で必ず `go test ./...` と `go vet ./...` が緑になる状態を保つ。

### Phase 0: DefaultMachine の `reworking → verifying` 変更

**目的**: entry gate 導入の前提となる state machine 変更。単独で意味のある改善なので先行させる。

**変更範囲**: `internal/orchestrator/machine.go`, `internal/orchestrator/machine_test.go`

**タスク**:

1. `DefaultMachine()` の reworking 自動遷移ルールを `reworking → verifying` に変更
2. doc comment を新しい遷移に合わせて更新
3. 既存テストの期待値更新 (§6.2)
4. 新規テスト追加 (§6.2)
5. 他のテスト (`internal/api/`, `internal/server/` 等) で reworking → done 直接遷移に依存しているものがないか grep で確認
6. `go test ./...`, `go vet ./...` 緑化

**完了条件**: 全テスト緑、rework cycle のシナリオ (E2E/integration 的なもの) が 1 cycle 増えるだけで機能的退行がないこと。

### Phase 1: boid 本体に entry gate 機構を追加 (additive)

**目的**: 既存 script 機構と共存する形で entry gate を実装。既存 kit は影響を受けないことを CI で確認する。

**変更範囲**: `internal/orchestrator/`, `internal/api/`

**タスク**:

1. `Gate` struct に `Phase` フィールドを追加。YAML unmarshal で default "exit"。
2. `GatePhase` 型と定数 `GatePhaseEntry`, `GatePhaseExit` を定義。
3. `EntryGateResult` 型を新設。
4. `Coordinator.DispatchEntryGates` メソッド実装。
5. `Evaluator.EvaluateGates` を phase 引数対応にリファクタ。既存 `DispatchAndAdvance` 内の呼び出しは `GatePhaseExit` を明示。
6. `runDispatchLoop` を書き換え: advance 後の entry gate dispatch → persist → terminal 判定。script trigger 呼び出しは残したまま (Phase 4 で削除)。
7. 単体テスト (TDD):
   - entry gate が advance 後に発火すること
   - self-loop では発火しないこと
   - 既存 exit gate が従来通り動くこと (後方互換)
   - phase 省略時に exit 扱いになること
   - entry gate の payload_patch が traits.produces で validation されること
   - entry gate の exclusive trait collision が検出されること
8. Integration テスト:
   - `runDispatchLoop` が entry gate 付き dispatch cycle を正しく回すこと
   - done 到達時に entry gate が 1 回だけ走ること (script トリガーも併走するが Phase 4 まではそのまま)
   - verifying → reworking → verifying → done の各 cycle で entry gate が適切に発火すること

**完了条件**: `go test ./...` 緑、`go vet ./...` clean、`go test -race ./internal/orchestrator/ ./internal/api/` 緑。既存 script ベースの auto-merge / done.py が引き続き動くこと (実際に実行して確認)。

### Phase 2: Dogfood 用の新 kit で entry gate を検証

**目的**: 実装した entry gate を、既存 kit に影響を与えない形で最初に dogfood する。新規 kit のみを触る。

**変更範囲**: `github.com/novshi-tech/boid-kits` repo (別リポジトリ)

**タスク**:

1. `boid-kits/` に新 kit ディレクトリを作成。例: `debug-entry-gate/` または最初から `jira-collector/` の MVP。
2. `kit.yaml` で最小の entry gate を宣言。例:
   ```yaml
   gates:
     - id: fetch-demo
       on: [executing]
       phase: entry
       traits:
         produces: [artifact]
   ```
3. 対応する gate script を `gates/fetch-demo.sh` として配置 (既存 path resolver を利用)。スクリプトは単純に固定の `payload_patch.artifact` を返す。
4. boid のテストプロジェクトで kit を enable し、task を流して entry gate が発火すること、payload に artifact が書き込まれることを確認。
5. 複数 entry gate の並列動作を簡易 script で手動検証。

**完了条件**: 新 kit が動作すること、既存 kit の動作に変化がないこと。

### Phase 3: 既存 script を entry/exit gate に移行

**目的**: `auto-merge` と `done.py` を新 gate 機構に置き換える。この段階でも script 機構は boid 本体に残っているため、移行は漸進的に行える。

**変更範囲**: `github.com/novshi-tech/boid-kits` repo + `github.com/novshi-tech/boid` repo (Gate.Behavior list 対応)

#### 3a. Gate.Behavior の list 対応 (boid 本体の追加変更)

現状 `Gate.Behavior string` は単一値のみ。`create-subtasks` gate で plan と auto_plan の両方をカバーするため以下の拡張が必要:

- `Gate.Behavior` を `OnValues` と同じ scalar/sequence 両対応にする (別名の型を定義するか、`OnValues` を流用する)
- `Hook.Behavior` も同じパターンで対応 (symmetry のため)
- `MatchScripts` の `Filter.Behavior` (ただし Phase 4 で削除される) は変更不要。script 側では `[plan, auto_plan]` にするなら 2 つのエントリを書く

この変更は Phase 1 の scope 肥大化を避けるため、Phase 3 の冒頭で実施する。boid 本体の変更と boid-kits の変更は同じ timing でリリースする。

**タスク**:

- `spec_types.go` の `Gate.Behavior` / `Hook.Behavior` を scalar/sequence 両対応型に変更
- yaml unmarshal のテスト追加
- `Evaluator.EvaluateGates` / `Evaluator.Evaluate` で behavior マッチングロジックを list 対応に更新

#### 3b. github-auto-merge の移行

元の `scripts/auto-merge.sh` を 2 つの gate に分割する。

**mergeable-check (verifying 退場 exit gate)**:

- `gates/mergeable-check.sh` として新規作成
- 処理: `gh pr view --json mergeable` のみ実行、conflict 検出なら finding 書き出し、それ以外は自身の subkey を空で更新
- `gh pr merge` は呼ばない
- `boid task update` / `boid task reopen` は呼ばない (全て stdout の payload_patch で表現)

**auto-merge (done 入場 entry gate)**:

- `gates/auto-merge.sh` として新規作成
- 処理: `gh pr merge --merge --delete-branch` 実行、成功/失敗を artifact に反映
- `boid task update` / `boid task reopen` は呼ばない

**kit.yaml の書き換え**:

```yaml
gates:
  - id: mergeable-check
    on: [verifying]
    phase: exit
    behavior: dev
    traits:
      produces: [verification]

  - id: auto-merge
    on: [done]
    phase: entry
    behavior: dev
    traits:
      produces: [artifact]
```

`scripts:` セクションを削除。

**共通ユーティリティ**: 2 つの script で `gh pr view` のパース等が共有される場合、kit 内に `lib/gh-pr-info.sh` のような共通ファイルを置いて source する。

#### 3c. boid-tasks の移行

`gates/done.py` → `gates/create-subtasks.py` にリネームし、done 入場 entry gate として書き換える。

ロジックは既存と同じ (親の `tasks` trait を読んで `boid task create` を呼ぶ)。`boid task create` は entry gate からでも呼び続けて良い (これは外部システム = boid server への副作用であり、実行中の task の状態を変えるものではない)。

**kit.yaml の書き換え**:

```yaml
gates:
  - id: create-subtasks
    on: [done]
    phase: entry
    behavior: [plan, auto_plan]  # Gate.Behavior list 対応 (3a) が必要
    traits:
      consumes: [tasks]
```

旧 `gates/done.py` に該当する旧エントリ (`on: [executing]`) は削除。

**完了条件**:

- auto-merge の正常マージパスが動作
- auto-merge の conflict パスで verifying → reworking → verifying → done → done 入場 auto-merge が動作 (全経路)
- plan task のサブタスク生成が動作 (verifying 経由 done 到達時に 1 回だけ発火)
- plan task が reviewer バウンス後に最終的な tasks trait のみでサブタスクが生成される (問題 A の退行バグ修正確認)

### Phase 4: Script 機構の boid 本体からの撤去

**目的**: Phase 3 で既存 kit の移行が完了したら、script 機構を boid 本体から完全に削除する。

**変更範囲**: `internal/orchestrator/`, `internal/api/`, `internal/sandbox/`, `cmd/`, docs

**タスク**:

1. `internal/orchestrator/spec_scripts.go` から script 関連コードを削除 (`MatchScripts`, `BuildTriggeredScriptTask`, `BuildScriptTask`, `ScriptFilter`, `ScriptTrigger`, `ValidScriptTriggerValues`, `ResolveScriptScript`)。`ResolveHookScript`, `ResolveGateScript`, `ValidHookOnValues`, `ValidGateOnValues` は残す。
2. `internal/orchestrator/spec_types.go` から `Script` 構造体と関連定数を削除。`KitMeta.Scripts` フィールドを削除。`ProjectMeta.Scripts` フィールドを削除。
3. `internal/api/service.go` から `fireScriptTriggers` メソッドと 2 箇所の呼び出しを削除 (`runDispatchLoop` 内)。
4. `internal/orchestrator/model.go` から `Task.Ephemeral` フィールドを削除。
5. `internal/orchestrator/script_trigger_test.go` を削除。
6. spec_loader から script の読み込みロジックを削除。
7. sandbox protocol から ephemeral 関連のフィールドを削除。
8. kit.yaml を読む loader で `scripts:` セクションがあれば schema validation エラーを返す。
9. DB migration で ephemeral task のクリーンアップを検討 (過去の ephemeral task が DB に残っている場合は drop column)。
10. docs 更新: CLAUDE.md や state machine 説明から script の記述を削除。
11. `docs/skills/boid-sandbox/` の payload_patch 説明から script 関連を削除し、entry gate 説明を追加。

**完了条件**: `go test ./...` 緑、`go vet ./...` clean、`grep -r "Script" internal/orchestrator/` で削除対象の残存がないこと、boid-kits の全 kit が新アーキテクチャで動作すること。

### Phase 5 (optional): docs / e2e / skill の体系的更新

- CLAUDE.md の並列 dev タスクと conflict 復旧手順セクションを、新 auto-merge (2 gate 構成) のフローに合わせて更新
- 既存の e2e scenario (rework-cycle など) を新 machine (`reworking → verifying`) に合わせて確認・必要なら更新
- `docs/skills/boid-sandbox/SKILL.md` に entry gate の payload_patch 記法を追記 (exit gate と同じだが、発火タイミングの違いを記述)

## 8. テスト戦略

### 8.1 単体テスト (TDD 必須)

実装前にテストを書いて失敗を確認するのが boid のコーディング規約 (CLAUDE.md)。以下は各 Phase で書くべき test の最小セット。

#### Phase 0 (`internal/orchestrator/machine_test.go`)

- `TestDefaultMachine_Reworking_AllResolved_TransitionsToVerifying` — 既存テストの改名、期待値 = verifying
- `TestDefaultMachine_Reworking_NoFindings_TransitionsToVerifying` — 同上
- `TestDefaultMachine_ReworkCycle_VerifyingReworkingVerifyingDone` — cycle 全体を integration 的に確認
- 既存 self-loop 系テストは変更不要

#### Phase 1 (`internal/orchestrator/coordinator_test.go`)

- `TestCoordinator_DispatchEntryGates_NoMatch` — 該当 gate なしで空結果
- `TestCoordinator_DispatchEntryGates_SingleGate` — 1 つの entry gate が payload_patch を返す
- `TestCoordinator_DispatchEntryGates_MultipleGates_Parallel` — 複数 entry gate が並列実行され payload マージされる
- `TestCoordinator_DispatchEntryGates_ExclusiveCollision` — exclusive trait の競合でエラー
- `TestCoordinator_DispatchEntryGates_EmptyOutput` — payload_patch 空の gate は payload に影響しない
- `TestCoordinator_DispatchAndAdvance_IgnoresEntryGates` — 既存の `DispatchAndAdvance` は entry gate を evaluate しないこと (後方互換確認)

#### Phase 1 (`internal/orchestrator/evaluator_test.go`)

- `TestEvaluator_EvaluateGates_EntryOnly` — phase フィルタが効くこと
- `TestEvaluator_EvaluateGates_ExitDefault` — phase 省略時に exit 扱い
- `TestEvaluator_EvaluateGates_MixedPhases` — entry/exit 混在時に正しく分離されること

#### Phase 1 (`internal/orchestrator/spec_types_test.go` または `spec_loader_test.go`)

- Gate YAML unmarshal で phase field のパース
- `phase: entry` / `phase: exit` / `phase: ""` の扱い
- `phase: invalid_value` で error

#### Phase 1 (`internal/api/service_test.go`)

- `TestRunDispatchLoop_EntryGateFiresAfterAdvance` — advance 後 entry gate 発火
- `TestRunDispatchLoop_EntryGateSkippedOnSelfLoop` — self-loop では skip
- `TestRunDispatchLoop_EntryGateOnDone_TriggersOnce` — done 到達で 1 回だけ発火
- `TestRunDispatchLoop_EntryGateFailure_RecordsDispatchError` — entry gate エラー時の error recording
- `TestRunDispatchLoop_FullConflictResolutionCycle` — verifying → reworking → verifying → done の full cycle が entry/exit gate と hook の組み合わせで正しく流れること (mock handler 使用)

#### Phase 3 (boid-kits 側で E2E)

- auto-merge 正常パス: PR 作成 → verifying 通過 → done 到達 → 実マージ → artifact.auto-merge.pr.merged=true
- auto-merge conflict パス: PR 作成 → verifying で mergeable-check が conflict 検出 → reworking → agent が git merge 解消 → verifying 再実行 → mergeable 確認 → done → 実マージ
- create-subtasks: plan task で tasks 書き出し → reviewer で指摘 → rework → 更新された tasks で done 到達 → 最終 tasks でのみ subtasks 生成

### 8.2 E2E シナリオ

Phase 3 完了後、以下を e2e で確認:

- `e2e/scenarios/` に既存 rework-cycle シナリオがあれば、新 machine に合わせて修正 (1 cycle 増えるだけのはず)
- 新規 scenario として auto-merge conflict 復旧パスを追加 (optional)
- Jira-fetch-like な entry gate を dogfood kit で動かしてみる (optional)

E2E はサンドボックス内では実行できない (CLAUDE.md 参照) ので CI に任せる。

## 9. 移行・後方互換と関連リポジトリの変更

### 9.1 boid 本体の既存 kit との互換性

Phase 1 時点で既存 gate (phase 省略) は全て exit gate として扱われるため、現行 kit は一切変更不要。Phase 4 で script 機構を削除するまで、旧形式の kit も動く。

Phase 0 の `reworking → verifying` 変更は全 kit に影響するが、verifying 状態に exit gate/hook がない kit ではただ 1 dispatch cycle 増えるだけで機能的な差はない。

### 9.2 boid-kits repo の変更 (Phase 3 実施)

変更対象 kit:

- `github-auto-merge/` — script から 2 gate (exit on verifying + entry on done) へ分割移行
- `boid-tasks/` — gate (on: executing) から entry gate (on: done) へ移行
- その他 `scripts:` を使う kit があれば洗い出して移行 (`rg -l "scripts:" github.com/novshi-tech/boid-kits/`)

### 9.3 docs / skill / CLAUDE.md の更新

- `CLAUDE.md`: 「並列 dev タスクとコンフリクト」セクションの auto-merge 説明を 2 gate 版に更新。conflict 復旧手順の state 遷移を `verifying → reworking → verifying → done` に修正。
- `docs/design/instructions-trait.md`: entry gate の概念に触れる必要があれば追記 (必須ではない)。
- `docs/skills/boid-sandbox/` (もし存在すれば): payload_patch 記述例に entry gate の書き方を載せる (exit gate と同じ protocol)。
- kit スキャフォルドテンプレート: 旧 script 形式を entry/exit gate 形式に書き換え。

### 9.4 データ互換性

`Task.Ephemeral` を削除するため、既存 DB に ephemeral=true の task が残っていると起動時に migration が必要になる可能性がある。Phase 4 開始時点でクリーンな開発環境 (本 project はプレリリースで単一ユーザ) であれば migration は単純にカラムを drop するだけで済む。念のため `0015_drop_task_ephemeral.sql` の migration を Phase 4 に含める。

## 10. 付録: 決定事項の根拠

本書で採用された設計判断とその理由を集約する。将来の疑問「なぜこうなっているか」に答えるための section。

### 10.1 なぜ gate を宣言的契約のみに留めるか (action 発行を許さない)

Gate は現在の runDispatchLoop の内側で同期的に実行される。ここで `boid task reopen` 等を CLI 経由で発行したり、あるいは「next_action request」のような宣言的な transition 要求を発行させたりすると、state machine の役割境界が曖昧になる。

代替案として検討した「`next_action` protocol」は以下の複雑さをもたらす:

- `HandlerResult` / `EntryGateResult` への action field 追加
- 複数 gate 間の action request 競合解決ロジック
- `sm.Apply` の追加呼び出しと、それによる loop 内での state 変化の連鎖処理
- action の「許可セット」の議論と validation
- action 履歴 (manual reopen か gate-requested reopen かの区別) の可視化

これらはすべて、**state machine 側で condition 評価によって表現できる遷移を、gate 側から強制する**という目的のための複雑さである。

一方で、conflict 検出 → rework の典型ユースケースは:

- mergeable-check が verifying 退場で conflict finding を書く
- state machine の `AnyFindingUnresolvedForState("verifying")` ルールが発火
- `reworking → verifying` (§2.7 の変更) により、rework 完了後に自然に再検証される

この形で complete に表現できる。action request は不要。

結果として、entry gate / exit gate の契約は「stdin で task JSON を受け取り、stdout に payload_patch を吐く」という純粋関数寄りな形に留まる。テスト・リプレイ・推論が容易になり、state machine が唯一の遷移 authority を保つ。

### 10.2 なぜ done を terminal のまま保つか

alternative: done に auto-transition rule を追加して、verification findings at done level があれば reworking に戻す。

これを採用しない理由:

- 「done は終わった状態」という直感的セマンティクスを壊す。ユーザが TUI や API 経由で done になったタスクを見たとき、自動的に reworking に戻り続ける可能性があるのは予測しづらい
- auto-transition は condition 評価のたびに発火判定されるので、done 状態のままで一時的に reworking 要因を持つ payload を書けなくなる (例: post-merge の監査レコード等)
- auto-merge の late conflict race のような rare case は、手動介入で処理する方が運用上も明瞭

done に戻る経路は manual action `reopen` (TUI / CLI 経由) のみ。entry gate on done は payload を書くだけで transition しない。

### 10.3 なぜ mergeable-check を verifying 退場のみに置くか (reworking 退場に重複しないか)

代替案: `on: [verifying, reworking]` 両方に宣言して、rework 後も同じ gate が走るようにする。

これを採用しない理由:

- §2.7 の `reworking → verifying` 変更により、rework 完了後は必ず verifying を再通過する。そのとき同じ mergeable-check gate が verifying 退場で再実行される
- `on: [verifying]` のみで書くのが最もシンプルで、意図 (「verifying は最終検証関門」) が明確
- 2 箇所に書くと同じ gate が複数タイミングで発火し、どのタイミングの finding なのか `source_state` を見ないと区別できなくなる

`source_state=verifying` の finding は rework 後に同じ gate が再実行されると subkey が上書きされ、mergeable なら空になる。これが自然に「conflict が解消された」ことを state machine に伝える。

### 10.4 なぜ mergeable-check と actual merge を分離するか (1 つの done 入場 gate に統合しない)

代替案: `auto-merge` を done 入場 gate 1 つに統合し、その中で mergeable チェックと `gh pr merge` を両方やる。

これを採用しない理由:

- mergeable 判定は **検証 (verification)** であり、その役割は verifying phase が持つべき
- conflict が検出されたとき、自然に reworking に戻るのは state machine の仕事。entry gate on done から conflict で reworking に戻すには action request protocol が必要になり、§10.1 の議論に逆戻りする
- 関心分離: 「is it mergeable?」 と「do the merge」は性質の異なる操作。前者は idempotent な検証、後者は不可逆な side effect

2 gate に分離することで:

- verifying 退場で mergeable 検証 → conflict なら自然な rework フローに乗る
- done 入場で actual merge → 確定した done state で実行される副作用

という責務が明確になる。

### 10.5 `reworking → verifying` 変更のトレードオフ

`reworking → done` を直接から `reworking → verifying` に変えることで、全タスクの rework 後に verifying 状態を 1 cycle 追加で通過する。

- コスト: dispatch loop の 1 cycle 分のオーバーヘッド (ミリ秒オーダー)
- ベネフィット: verifying 状態の exit gates が rework 後に再走する (mergeable-check, 将来の reviewer hook 等)

verifying に何も配置していないタスクでは、verifying は pass-through なのでコストは実質 0。configuration が増えたタスクで自動的に恩恵を受ける。

機能退行なし、コスト無視できる、将来の拡張 (reviewer hook on verifying) にも効く。採用。

### 10.6 なぜ phase を Gate 構造体のフィールドにしたか (別セクションにしない理由)

alternative: kit.yaml に `entry_gates:` / `exit_gates:` セクションを別に定義する。

これを採用しない理由:

- Go 側で 2 つの slice を持つと `MergeKitMeta` や validation 等すべての場所で 2 回ずつ処理する必要がある
- kit.yaml 作者視点では「gate は gate」であり、phase だけが違うのは本質的な差異ではない
- yaml 上で `phase: entry` を書き加えるだけなので、書き心地の差も小さい

### 10.7 なぜ Phase 1 を additive にするか (script 機構を同時に消さないか)

いきなり script を消すと:

- boid 本体の変更と boid-kits の変更を同時にリリースしないと CI が壊れる
- runDispatchLoop の書き換えと script 撤去を同じ PR に入れると diff が巨大になり review / debug しづらい

Phase 分割により:

- Phase 0 で state machine 変更を単独で確定
- Phase 1 で新機構を追加して既存テストが通ることを確認 (= runDispatchLoop 書き換えの正しさを既存テスト群で検証)
- Phase 2 で新機構を dogfood して基本動作を確認
- Phase 3 で既存 kit を個別に移行 (1 kit ずつ動作確認)
- Phase 4 で script 機構を撤去 (この時点で全 kit は新機構で動いている)

各フェーズが独立した commit / PR になり、万一の rollback も小さい単位で可能。プロジェクトがプレリリースでユーザが 1 名 (= オーナー自身) なので「段階リリース」は不要だが、「作業単位としての分解」は diff サイズと検証容易性のために有効。

## 11. 懸念点と未確定事項

以下は実装開始時点で再検討すべき項目。本書時点では結論を保留している。

### 11.1 Entry gate の cold start 問題

task が pending から最初に start されたとき、executing に入る瞬間は「遷移」なので entry gate が走る。これは意図通り。

ただし、boid 起動時に既に executing/reworking/verifying 状態の task を resume する場合:

- 現状の runDispatchLoop は起動時 task に対して `DispatchAndAdvance` を呼ぶが、これは「現状態の hook + exit gate 発火」であり、entry gate は走らない
- 実装時に「前回の dispatch で entry gate が走ったか」を persist する必要があるかどうか要検討
- 初期実装では entry gate は idempotent を仮定して起動時には走らせない (= 最後の transition 時に走った想定) 方針にする。bug として現れたら再検討

### 11.2 Entry gate の失敗時のロールバック

entry gate が失敗したとき、既に persist された advance 後の状態はどうする?

- 現行の exit gate 失敗時は `recordDispatchError` でログを残し、状態は advance 後のままで loop を止める
- entry gate 失敗時も同じで良さそう (ロールバックしない)
- 失敗した entry gate が重要な処理 (auto-merge の gh pr merge) の場合は、artifact に error を書いて終わり。ユーザは TUI で error を見て手動介入する
- ただし「reworking に戻そうとしたら failed」というケース (例: create-subtasks で boid task create が失敗) では task が stuck する可能性がある。手動介入手順 (`boid task reopen` / `boid task retry` 等) を docs に残す

### 11.3 Entry gate の並列性と順序

複数 entry gate が同時に走るとき、payload_patch のマージ順序は?

- 現行 exit gate と同じく、並列実行後に順不同でマージ (exclusive trait は collision detection あり)
- `source_state` の付与 (`injectSourceState`) は entry gate でも同じく行う。entry gate が verifying 状態に入った直後に走った場合、source_state=verifying になる

### 11.4 Gate.Behavior list 対応と既存 hook/gate との整合性

§7 Phase 3a で触れた通り、`Gate.Behavior` / `Hook.Behavior` を scalar/list 両対応にする変更が必要になる。これは `OnValues` の実装パターン (`internal/orchestrator/spec_types.go:176-192`) を流用するだけで十分小さい変更。

既存 kit の `behavior: dev` (scalar) は引き続き動くこと、`behavior: [plan, auto_plan]` (list) が新しく書けること、両方を yaml unmarshal test で確認する。

### 11.5 `source_state=entry` のような区別が必要か

entry gate が書いた finding と exit gate が書いた finding を区別する必要があるか?

- 現状の `source_state` は task の state 名 (executing/verifying/reworking 等) を入れており、gate の phase は区別しない
- 区別の必要性: 不明。初期実装では区別せず、現行の state 名のみを source_state に入れる
- 将来必要になった場合は `source_phase` のような追加フィールドで対応

## 12. 参照

### 関連コミット

- `edb8b63` (2026-04-11): 統合 state machine 導入
- `e7c265f` (2026-04-12): plan タスクの `tasks` trait を `artifact` と対称化

### 関連ファイル (各フェーズで主に触る場所)

**Phase 0**:
- `internal/orchestrator/machine.go` — DefaultMachine の reworking rule
- `internal/orchestrator/machine_test.go` — 対応テスト

**Phase 1**:
- `internal/orchestrator/spec_types.go` — Gate struct 拡張, GatePhase 定義
- `internal/orchestrator/evaluator.go` — EvaluateGates の phase 対応
- `internal/orchestrator/coordinator.go` — DispatchAndAdvance の調整, 新設 DispatchEntryGates, EntryGateResult
- `internal/api/service.go` — runDispatchLoop 書き換え

**Phase 3**:
- `internal/orchestrator/spec_types.go` — Gate.Behavior / Hook.Behavior の list 対応
- `internal/orchestrator/evaluator.go` — behavior マッチングロジック更新

**Phase 4**:
- `internal/orchestrator/spec_scripts.go` — 大幅削除
- `internal/orchestrator/spec_types.go` — Script 関連削除
- `internal/orchestrator/model.go` — Task.Ephemeral 削除
- `internal/api/service.go` — fireScriptTriggers 削除

### 関連 kit (Phase 3 で移行)

- `github.com/novshi-tech/boid-kits/github-auto-merge/`
- `github.com/novshi-tech/boid-kits/boid-tasks/`
- その他 `scripts:` セクションを使う kit

### 理論的背景

- UML State Machine Diagram (onEntry / onExit action)
- Moore machine (output associated with state)
- Statechart hierarchical state machines (Harel, 1987)
