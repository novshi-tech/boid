# instructions トレイト設計・実装計画

## 背景と課題

### 課題 1: Hook ルーティングの粗さ

現在の Evaluator は、task の state と trait の有無だけで hook を選ぶ。
そのため、Claude Code と Codex の両方を kit として有効にしつつ、

- execution は Claude Code
- review は Codex

のように「同じ state に複数 kit hook があるが、payload 上の指示に応じて振り分ける」
という要件を表現できない。

### 課題 2: handler trait 契約の過積載

現在の `requires_traits` は、実質的に 2 つの責務を同時に持っている。

- handler が読む前提の trait
- handler が `payload_patch` で書いてよい trait

この 2 つは意味が異なる。特に `instructions` のような「読むだけで、書かない」trait を
素直に表現できない。

### 課題 3: プロンプトのハードコード

hook script にプロンプト構築ロジックが埋め込まれており、

- reviewer 向け指示
- consumer ごとの指示
- behavior ごとのプリセット
- 同一 consumer に対する複数観点の指示

を payload として渡す仕組みがない。

### 課題 4: `default_payload` の YAML 表現

`TaskBehavior.DefaultPayload` を `json.RawMessage` のまま持つと、
YAML 上の map/object をそのまま読み込めない。

```yaml
default_payload:
  instructions:
    reviewer:
      type: verification
      consumer: codex
      message: "レビューしてください。"
```

のような表現をそのまま受け付けるには、
YAML ノードを JSON バイト列へ変換する専用型が必要になる。

## 設計方針

- trait ベースの契約は維持する
- 新しい top-level trait `instructions` を導入する
- handler の trait 契約を `consumes` と `produces` に分離する
- consumer ルーティングは「`instructions` を consume する hook」にのみ適用する
- 同じ consumer に複数 instruction を割り当てられるようにする
- `instructions` は role 名をキーにした辞書で保持し、role 名は merge key と安定順序キーを兼ねる
- `default_payload` は YAML で記述可能にし、内部では canonical JSON に変換して保持する
- 後方互換レイヤは持たない。`requires_traits` は廃止し、新形式へ揃える

## データ構造

### Instruction 型

```go
// spec_types.go

type InstructionType string

const (
    InstructionTypeExecution    InstructionType = "execution"
    InstructionTypeVerification InstructionType = "verification"
)

type Instruction struct {
    Type     InstructionType `json:"type" yaml:"type"`
    Consumer string          `json:"consumer" yaml:"consumer"`
    Message  string          `json:"message" yaml:"message"`
}
```

### Dispatch 用の RoutedInstruction

payload 上の `instructions` は role 名をキーにした辞書だが、
hook へ渡すときは role 名を保持した配列に変換する。

```go
// spec_types.go

type RoutedInstruction struct {
    Role     string          `json:"role"`
    Type     InstructionType `json:"type"`
    Consumer string          `json:"consumer"`
    Message  string          `json:"message"`
}
```

## ペイロード上の形式

`instructions` は辞書形式で保持する。キーはユーザー定義の role 名であり、

- `default_payload` マージ時の merge key
- hook へ配列化する際の安定ソートキー

として使う。

```json
{
  "instructions": {
    "executor": {
      "type": "execution",
      "consumer": "claude-code",
      "message": "TDD で実装してください。"
    },
    "reviewer": {
      "type": "verification",
      "consumer": "codex",
      "message": "正しさと可読性をレビューしてください。"
    },
    "security-reviewer": {
      "type": "verification",
      "consumer": "codex",
      "message": "セキュリティ観点でレビューしてください。"
    }
  },
  "artifact": null,
  "verification": null
}
```

同じ `type` と `consumer` を持つ instruction が複数存在してよい。
hook 側には role 昇順で安定化した `[]RoutedInstruction` を渡す。

## handler trait 契約

### `consumes` / `produces`

`requires_traits` は廃止し、handler 側は明示的に I/O 契約を持つ。

```go
// spec_types.go

type HandlerTraits struct {
    Consumes []TraitType `json:"consumes,omitempty" yaml:"consumes,omitempty"`
    Produces []TraitType `json:"produces,omitempty" yaml:"produces,omitempty"`
}
```

意味は次の通り。

- `traits.consumes`
  handler が読む前提の trait。Evaluator の発火条件にも使う
- `traits.produces`
  handler が `payload_patch` で書いてよい trait。merge validation に使う

### Hook / Gate の拡張

```go
// spec_types.go

type Hook struct {
    ID         string        `yaml:"id" json:"id"`
    On         string        `yaml:"on" json:"on"`
    Traits     HandlerTraits `yaml:"traits" json:"traits"`
    Requires   []string      `yaml:"requires" json:"requires"`
    Consumer   string        `yaml:"consumer,omitempty" json:"consumer,omitempty"`
    Kit        string        `yaml:"-" json:"kit,omitempty"` // provenance
    ScriptPath string        `yaml:"-" json:"-"`
}

type Gate struct {
    ID         string        `yaml:"id" json:"id"`
    On         string        `yaml:"on" json:"on"`
    Traits     HandlerTraits `yaml:"traits" json:"traits"`
    Kit        string        `yaml:"-" json:"kit,omitempty"` // provenance
    ScriptPath string        `yaml:"-" json:"-"`
}
```

`Hook.Consumer` は routing identity であり、`Hook.Kit` は provenance である。
この 2 つは別物として扱う。

- kit 由来 hook は、明示 `consumer` がなければ kit の解決済み consumer 名を継承する
- project 直定義 hook が `instructions` を consume する場合は、`consumer` を明示する
- `Kit` は誰がその hook を供給したかの記録であり、routing 判定には使わない

### YAML 例

kit 由来 hook:

```yaml
hooks:
  - id: run-agent
    on: executing
    traits:
      consumes: [instructions]
      produces: [artifact]

  - id: run-review
    on: verifying
    traits:
      consumes: [instructions, artifact, verification]
      produces: [verification]
```

project 直定義 hook:

```yaml
hooks:
  - id: local-reviewer
    on: verifying
    consumer: codex
    traits:
      consumes: [instructions, artifact, verification]
      produces: [verification]
```

### validation ルール

- `instructions` は `traits.produces` に入れてはならない
- `instructions` を `traits.consumes` に含む hook は、merge 後に `Consumer` が必須
- gate は当面 `instructions` を consume してはならない

## TaskBehavior の拡張

### `default_payload`

`default_payload` は YAML で自然に書ける必要があるため、
JSON バイト列を保持しつつ `UnmarshalYAML` を持つ専用型を導入する。

```go
// spec_types.go

type RawPayload json.RawMessage

func (p *RawPayload) UnmarshalYAML(node *yaml.Node) error {
    var v any
    if err := node.Decode(&v); err != nil {
        return err
    }
    b, err := json.Marshal(v)
    if err != nil {
        return err
    }
    *p = RawPayload(b)
    return nil
}

func (p RawPayload) RawMessage() json.RawMessage {
    return json.RawMessage(p)
}

type TaskBehavior struct {
    Name           string     `yaml:"name" json:"name"`
    Traits         []string   `yaml:"traits" json:"traits"`
    Readonly       bool       `yaml:"readonly" json:"readonly,omitempty"`
    Worktree       bool       `yaml:"worktree" json:"worktree,omitempty"`
    BranchPrefix   string     `yaml:"branch_prefix" json:"branch_prefix,omitempty"`
    BaseBranch     string     `yaml:"base_branch" json:"base_branch,omitempty"`
    DefaultPayload RawPayload `yaml:"default_payload" json:"default_payload,omitempty"`
}
```

### YAML 上の表現

```yaml
task_behaviors:
  impl:
    traits: [instructions, artifact, verification]
    default_payload:
      instructions:
        executor:
          type: execution
          consumer: claude-code
          message: "TDD で実装してください。テストを先に書くこと。"
        reviewer:
          type: verification
          consumer: codex
          message: "正しさと可読性をレビューしてください。"
        security-reviewer:
          type: verification
          consumer: codex
          message: "セキュリティ観点でレビューしてください。"
```

## kit consumer 名の解決

### KitRef

kit は短縮名ではなく、`ref` と `alias` を持つ構造体で扱う。

```go
// spec_types.go

type KitRef struct {
    Ref   string `yaml:"ref" json:"ref"`
    Alias string `yaml:"as,omitempty" json:"as,omitempty"`
}
```

YAML では文字列形式と構造体形式の両方を受け付ける。

```yaml
kits:
  - github.com/novshi-tech/boid-kits/claude-code
  - ref: github.com/novshi-tech/boid-kits/codex
    as: codex
```

### 解決ルール

```go
func resolveKitConsumer(ref KitRef) string {
    if ref.Alias != "" {
        return ref.Alias
    }
    parts := strings.Split(ref.Ref, "/")
    return parts[len(parts)-1]
}
```

例:

```text
github.com/novshi-tech/boid-kits/claude-code -> "claude-code"
codex                                        -> "codex"
local/go-dev                                 -> "go-dev"
```

### validation

loader は有効 kit 一式に対して解決済み consumer 名の一意性を検証する。
basename 衝突がある場合は、`as:` を要求してロードエラーにする。

### MergeKitMeta

`MergeKitMeta()` は provenance と routing identity を別々に設定する。

```go
func MergeKitMeta(base *ProjectMeta, kits []*KitMeta, kitConsumers []string) *ProjectMeta {
    // ...
    for i, meta := range kits {
        for j := range meta.Hooks {
            meta.Hooks[j].Kit = kitConsumers[i]
            if meta.Hooks[j].Consumer == "" {
                meta.Hooks[j].Consumer = kitConsumers[i]
            }
        }
        for j := range meta.Gates {
            meta.Gates[j].Kit = kitConsumers[i]
        }
        allHooks = append(allHooks, meta.Hooks...)
        allGates = append(allGates, meta.Gates...)
    }
    // ...
}
```

## Evaluator のルーティング変更

### InstructionType と State の対応

| InstructionType | 発火する TaskStatus |
|---|---|
| `execution` | `executing`, `reworking` |
| `verification` | `verifying` |

```go
func instructionTypeForStatus(status TaskStatus) InstructionType {
    switch status {
    case TaskStatusExecuting, TaskStatusReworking:
        return InstructionTypeExecution
    case TaskStatusVerifying:
        return InstructionTypeVerification
    default:
        return ""
    }
}
```

### `Evaluate()` の新ロジック

consumer ルーティングは、`instructions` を consume する hook にだけ適用する。
それ以外の hook は、kit 由来であっても従来通り trait 条件だけで発火する。

```go
func (e *Evaluator) Evaluate(task *Task, hooks []Hook) []Hook {
    activeTraits, _ := ActiveTraitTypes(task.Payload)
    traitSet := make(map[TraitType]bool, len(activeTraits))
    for _, t := range activeTraits {
        traitSet[t] = true
    }

    instType := instructionTypeForStatus(task.Status)
    consumers := extractInstructionConsumers(task.Payload, instType)

    var matched []Hook
    for _, h := range hooks {
        if h.On != string(task.Status) {
            continue
        }
        if !hasAllTraits(traitSet, h.Traits.Consumes) {
            continue
        }
        if consumesTrait(h.Traits, TraitInstructions) {
            if instType == "" {
                continue
            }
            if h.Consumer == "" {
                continue // loader validation 後は到達しない想定
            }
            if !consumers[h.Consumer] {
                continue
            }
        }
        matched = append(matched, h)
    }
    return matched
}
```

ポイント:

- `instructions` を consume しない hook は consumer で絞られない
- routing identity は `Hook.Consumer` を使う
- `Hook.Kit` は provenance であり、routing 条件には使わない

### `EvaluateGates()`

gate は当面 `instructions` を consume 不可とし、
`EvaluateGates()` は `traits.consumes` のみを見る。

### `extractInstructionConsumers()`

```go
func extractInstructionConsumers(payload json.RawMessage, instType InstructionType) map[string]bool {
    if instType == "" {
        return nil
    }
    var m map[string]json.RawMessage
    if err := json.Unmarshal(payload, &m); err != nil {
        return nil
    }
    raw, ok := m["instructions"]
    if !ok || string(raw) == "null" {
        return nil
    }
    var instructions map[string]Instruction
    if err := json.Unmarshal(raw, &instructions); err != nil {
        return nil
    }
    consumers := make(map[string]bool)
    for _, inst := range instructions {
        if inst.Type == instType {
            consumers[inst.Consumer] = true
        }
    }
    if len(consumers) == 0 {
        return nil
    }
    return consumers
}
```

## payload merge / validation の変更

### `TraitInstructions`

```go
const (
    TraitArtifact     TraitType = "artifact"
    TraitVerification TraitType = "verification"
    TraitTasks        TraitType = "tasks"
    TraitInstructions TraitType = "instructions"
)
```

`TraitPrompt` は廃止する。`instructions` が agent への指示伝達を完全に吸収するため、
`prompt` トレイトは不要になる。既存の `agent_prompt` ペイロードキーへの依存も
`instructions` に移行する。

### `ActiveTraitTypes()`

`instructions` が top-level に存在し、値が `null` でなければ active trait として検出する。
これは Evaluator の `traits.consumes` 判定に使う。

### `MergePayloadPatch()` の validation 対象

handler が書いてよい trait は `traits.produces` で決まる。
したがって、`ValidatePayloadPatch()` と `MergePayloadPatch()` の allowed list は
`consumes` ではなく `produces` を使う。

```go
func (hr *HandlerResult) producedTraits(hooks []Hook) []TraitType {
    for _, h := range hooks {
        if h.ID == hr.ID {
            return h.Traits.Produces
        }
    }
    return nil
}
```

Coordinator 側の呼び出しも次のように変える。

```go
merged, err := MergePayloadPatch(payload, hr.PayloadPatch, hr.ID, hr.producedTraits(matchedHooks))
```

gate についても同様に `Traits.Produces` を使う。

### merge mode

`instructions` は hook/gate の出力対象ではない。
よって `TraitMergeMode()` に `instructions` 用の特別扱いは不要である。

## タスク作成時の `default_payload` マージ

### `CreateTask()` の流れ

task 作成時に behavior の `default_payload` を request payload に先行マージする。

```go
func (s *TaskAppService) CreateTask(req CreateTaskRequest) (*orchestrator.Task, error) {
    meta, ok := s.Meta.Get(req.ProjectID)
    if !ok {
        return nil, &StatusError{Code: http.StatusBadRequest, Message: "project meta not loaded"}
    }

    payload := req.Payload
    if behavior, ok := meta.TaskBehaviors[req.Behavior]; ok {
        if len(behavior.DefaultPayload) > 0 {
            merged, err := mergeDefaultPayload(behavior.DefaultPayload.RawMessage(), payload)
            if err != nil {
                return nil, &StatusError{Code: http.StatusBadRequest, Message: "payload merge: " + err.Error()}
            }
            payload = merged
        }
    }

    task := &orchestrator.Task{
        ProjectID:    req.ProjectID,
        Title:        req.Title,
        Description:  req.Description,
        Behavior:     req.Behavior,
        RemoteID:     req.RemoteID,
        DataSourceID: req.DataSourceID,
        Payload:      payload,
    }
    if err := s.Tasks.CreateTask(task); err != nil {
        return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
    }
    return task, nil
}
```

### マージ戦略

1. `default_payload` をベースにする
2. request payload の top-level キーで上書きする
3. override が `null` の top-level キーは削除として扱う
4. `instructions` は role 名単位で上書きマージする（role 単位の置換）
5. `instructions.<role> = null` はその role の削除として扱う

### `mergeInstructions()`

```go
func mergeInstructions(base, override json.RawMessage) (json.RawMessage, error) {
    var baseMap map[string]json.RawMessage
    if len(base) > 0 && string(base) != "null" {
        if err := json.Unmarshal(base, &baseMap); err != nil {
            return nil, err
        }
    } else {
        baseMap = make(map[string]json.RawMessage)
    }

    var overMap map[string]json.RawMessage
    if err := json.Unmarshal(override, &overMap); err != nil {
        return nil, err
    }

    for role, overInst := range overMap {
        if string(overInst) == "null" {
            delete(baseMap, role)
            continue
        }
        // role 単位で置換（フィールド単位の deep merge は不要）
        baseMap[role] = overInst
    }

    return json.Marshal(baseMap)
}
```

## Hook への instructions 伝達

### `filterInstructions()`

Planner は task payload の `instructions` から、

- 現在の `InstructionType`
- hook の `Consumer`

に一致するものを抽出し、role 昇順に整列した `[]RoutedInstruction` を返す。

```go
func filterInstructions(payload json.RawMessage, instType InstructionType, consumer string) []RoutedInstruction {
    // 1. payload.instructions を map[string]Instruction として読む
    // 2. type と consumer が一致する role を集める
    // 3. role 昇順で sort
    // 4. RoutedInstruction{Role: role, ...} の配列にする
}
```

### DTO チェーン

instructions は `DispatchRequest` だけでなく、dispatcher / sandbox まで通す。

```go
// orchestrator/dispatch_request.go
type DispatchRequest struct {
    // ...
    PayloadJSON      string
    TaskJSON         string
    InstructionsJSON string
}

// dispatcher/plan.go
type DispatchPlan struct {
    // ...
    PayloadJSON      string
    TaskJSON         string
    InstructionsJSON string
}

// dispatcher/preparer.go
type SandboxSpec struct {
    // ...
    PayloadJSON      string
    TaskJSON         string
    InstructionsJSON string
}

// sandbox/script.go
type WrapperConfig struct {
    // ...
    PayloadJSON      string
    TaskJSON         string
    InstructionsJSON string
}
```

### `PlanHook()`

```go
instType := instructionTypeForStatus(task.Status)
myInstructions := filterInstructions(task.Payload, instType, event.Hook.Consumer)
if len(myInstructions) > 0 {
    instJSON, _ := json.Marshal(myInstructions)
    req.InstructionsJSON = string(instJSON)
}
```

### sandbox 内の受け渡し

hook role の inner script は、stdin の payload JSON に加えて、
環境変数 `BOID_INSTRUCTIONS` で自分宛の instruction 配列を受け取る。

```bash
export BOID_INSTRUCTIONS='[{"role":"reviewer","message":"..."}, ...]'
printf '%s' '<payload-json>' | /path/to/hook.sh
```

### kit script の擬似コード

`instructions` を consume する hook は、`instructions` が存在する前提で動く。
instructions がない場合は Evaluator で弾かれるため、hook 内フォールバックは不要。

```text
payload := read stdin as JSON
instructions := decode env["BOID_INSTRUCTIONS"] as []RoutedInstruction
task_title := boid task get $BOID_TASK_ID --field title
task_desc := boid task get $BOID_TASK_ID --field description

// instructions.message は作業の前提・方針を与える
// タスク固有のコンテキストは title, description, verification 等から組み立てる
sections := []
for _, inst := range instructions { // role 昇順で安定
    sections = append(sections, "## "+inst.Role)
    sections = append(sections, inst.Message)
}
sections = append(sections, "## Task")
sections = append(sections, task_title)
if task_desc != "" {
    sections = append(sections, task_desc)
}

prompt := strings.Join(sections, "\n\n")
exec claude/codex with prompt
```

同一 consumer に複数 instruction がある場合でも、全件を role 付きで受け取れる。

## validation まとめ

- `requires_traits` は禁止
- hook / gate は `traits.consumes` と `traits.produces` を持つ
- `instructions` は `traits.produces` に入れてはならない
- `instructions` を consume する hook は、merge 後に `Consumer` が必須
- gate は `instructions` を consume してはならない
- kit consumer 名は有効 kit 全体で一意でなければならない
- `default_payload` は YAML から canonical JSON へ変換して保持する

## 破壊的変更

後方互換レイヤは持たない。前提は次の通り。

- `requires_traits` は廃止
- `TraitPrompt` (`prompt` トレイト) は廃止。`instructions` に置き換え
- hook / gate 定義は新しい `traits` 形式へ移行する
- instructions ルーティング対象 hook は `consumer` を持つ
  - kit hook は kit consumer を継承
  - project 直定義 hook は YAML で明示
- `default_payload` は専用型で受ける
- 既存の `agent_prompt` ペイロードキーへの依存は `instructions` に移行する

## 実装ステップ

### Step 1: データ構造の追加

- [ ] `InstructionType`, `Instruction`, `RoutedInstruction` を追加
- [ ] `TraitInstructions` を追加、`TraitPrompt` を削除
- [ ] `HandlerTraits` を追加
- [ ] `Hook` の `Traits`, `Consumer`, `Kit` を追加
- [ ] `Gate` の `Traits`, `Kit` を追加
- [ ] `RawPayload` を追加し `UnmarshalYAML` を実装
- [ ] `TaskBehavior.DefaultPayload` を追加
- [ ] `KitRef` と YAML 文字列/構造体両対応の Unmarshal を追加

### Step 2: kit consumer 解決

- [ ] `resolveKitConsumer()` を追加
- [ ] `ReadProjectMetaWithKits()` で kit ごとの consumer 名を解決
- [ ] consumer 名の重複を validate
- [ ] `MergeKitMeta()` のシグネチャを変更し consumer 名を受け取る
- [ ] kit hook の `Consumer` / `Kit` を設定
- [ ] kit gate の `Kit` を設定
- [ ] テスト更新

### Step 3: Evaluator の `consumes` / instructions routing

- [ ] `instructionTypeForStatus()` を追加
- [ ] `extractInstructionConsumers()` を追加
- [ ] `Evaluate()` を `Traits.Consumes` ベースに変更
- [ ] `instructions` を consume する hook にだけ consumer フィルタを適用
- [ ] `EvaluateGates()` を `Traits.Consumes` ベースに変更
- [ ] テスト: instructions あり → consumer マッチで選別
- [ ] テスト: instructions を consume しない hook は影響なし
- [ ] テスト: instructions を consume するが consumer 不一致 → 非発火

### Step 4: payload merge / validation の変更

- [ ] `ValidatePayloadPatch()` を `produces` ベースに変更
- [ ] `MergePayloadPatch()` の allowed traits を `Traits.Produces` から渡す
- [ ] `HandlerResult.allowedTraits()` を `producedTraits()` に置き換える
- [ ] gate 側も `Traits.Produces` を使う
- [ ] テスト: `instructions` を produces に置いたら validation error
- [ ] テスト: produces 外の trait を返したら merge error

### Step 5: `default_payload` マージ

- [ ] `mergeDefaultPayload()` を追加
- [ ] `mergeInstructions()` を追加
- [ ] top-level `null` を削除として扱う
- [ ] `instructions.<role> = null` を role 削除として扱う
- [ ] `TaskAppService` に `Meta` を追加
- [ ] `CreateTask()` に `default_payload` マージを追加
- [ ] テスト: YAML `default_payload` が読める
- [ ] テスト: instructions が role 単位で上書きマージされる
- [ ] テスト: override null で role/top-level が削除される

### Step 6: Planner / dispatcher / sandbox への instructions 伝達

- [ ] `filterInstructions()` を追加
- [ ] `DispatchRequest.InstructionsJSON` を追加
- [ ] `DispatchPlan.InstructionsJSON` を追加
- [ ] `SandboxSpec.InstructionsJSON` を追加
- [ ] `WrapperConfig.InstructionsJSON` を追加
- [ ] `PlanHook()` で hook consumer 宛 instruction を抽出してセット
- [ ] sandbox script 生成で `BOID_INSTRUCTIONS` を export
- [ ] テスト: instructions が DTO チェーン全体を通って hook に届く
- [ ] テスト: role 昇順で安定した配列になる

### Step 7: kit 更新

- [ ] Claude Code kit の hook 定義を新しい `traits` 形式へ移行
- [ ] Codex kit に verification hook を追加
- [ ] instructions を consume する hook script を `BOID_INSTRUCTIONS` 対応へ更新
- [ ] 同一 consumer の複数 instruction を連結できるようにする

### Step 8: E2E テスト

- [ ] 2 つの agent kit を有効化し、instructions で execution / verification が振り分けられることを確認
- [ ] 同一 consumer に複数 verification instruction を渡し、全件が届くことを確認
- [ ] `default_payload` と request payload の merge を確認
- [ ] alias なし basename 衝突が loader error になることを確認

## 未決事項

- rework 時の verification -> execution 引き継ぎは kit の責務とする
  - hook は stdin の payload と `BOID_INSTRUCTIONS` の両方を見て prompt を組み立てる
- gate に対する instructions routing は当面対象外
- `TaskBehavior.Traits` の役割整理は別設計で扱う
