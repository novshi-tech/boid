---
name: boid-add-builtin
description: >
  boid オーケストレータに新しい builtin command (例: oci, net) を追加する際の手順チェックリスト。
  builtin command の追加、policy テーブルへの登録、broker dispatch の追加、テスト実装の一連の作業をガイドする。
  Use when a team member wants to add a new builtin command to the boid orchestrator.
---

# boid builtin command 追加手順

新しい builtin を追加するときは、以下の **8 ステップ** を順番に行う。
各ステップで参照すべき既存実装例を併記している。

コード詳細・ファイルパス早見表は [references/key-files.md](references/key-files.md) を参照。

---

## ステップ 1 — protocol 追加

`internal/sandbox/protocol.go` に追加する:

```go
// 新しい Op 型と定数
type OciOp string

const (
    OciOpRun OciOp = "run"
)

// ExecRequest に新フィールドを追加
type ExecRequest struct {
    // ... 既存フィールド ...
    Oci *OciRequest `json:"oci,omitempty"`
}

// Request 型を定義
type OciRequest struct {
    Op    OciOp  `json:"op"`
    Image string `json:"image,omitempty"`
    // ...
}
```

参照例: `BoidOp`, `GitOp` の定義パターン。

---

## ステップ 2 — validBuiltinCommands への登録

`internal/orchestrator/spec_loader.go` の `validBuiltinCommands` マップに追加:

```go
var validBuiltinCommands = map[string]struct{}{
    "git":  {},
    "boid": {},
    "oci":  {}, // 追加
}
```

これを忘れると project.yaml / kit.yaml で `builtin_commands: [oci]` と書いたときにバリデーションエラーになる。

---

## ステップ 3 — handler 実装

`internal/sandbox/oci_builtin.go` を新設し、以下の制約を守る:

```go
func handleOciBuiltinRequest(req *ExecRequest, entry *tokenEntry) *ExecResponse {
    // 冒頭で必ず policy チェック（これを省くと op 制限が無効になる）
    if !entry.hasBuiltinPolicy("oci") {
        return &ExecResponse{ExitCode: 1, Stderr: "command not allowed: oci"}
    }
    // cwd 検証（git_builtin.go の validateGitBuiltinCwd を参考に）
    if err := validateOciBuiltinCwd(req.Cwd, entry); err != nil {
        return &ExecResponse{ExitCode: 1, Stderr: err.Error()}
    }
    // op 制限チェック（role に基づく分岐は書かない — policy テーブルに集約）
    if !entry.allowsBuiltinOp("oci", string(req.Oci.Op)) {
        return &ExecResponse{
            ExitCode: 1,
            Stderr:   fmt.Sprintf("oci op %q not allowed for role %s", req.Oci.Op, entry.Context.Role),
        }
    }
    // observability: 実行時に role を slog に載せる（読み取りのみ、判定に使わない）
    slog.Info("oci builtin run requested", "role", entry.Context.Role)
    // ... 本体処理 ...
}
```

**禁止**: `entry.Context.Role == "hook"` のような role 直接参照による分岐。
role 判定は planner の `DefaultBuiltinPolicies` で完結しており、broker はそれを参照するのみ。

参照例: `internal/sandbox/git_builtin.go` の `handleGitBuiltinRequest`。

---

## ステップ 4 — policy 定義 (最重要)

`internal/orchestrator/builtin_policy.go` を編集する:

```go
func policyFor(role Role, name string) sandbox.BuiltinPolicy {
    switch name {
    case "boid":
        return boidPolicy(role)
    case "git":
        return gitPolicy(role)
    case "oci":
        return ociPolicy(role) // 追加
    default:
        return sandbox.BuiltinPolicy{}
    }
}

func ociPolicy(role Role) sandbox.BuiltinPolicy {
    switch role {
    case RoleHook:
        // hook からのコンテナ実行は禁止。
        // agent がホスト側リソースを直接操作しないようにするため。
        // 関連: git builtin の hook 制限と同じ設計思想。
        return sandbox.BuiltinPolicy{}
    default: // RoleGate or empty → gate 相当
        // gate は検証・ビルド目的でコンテナ実行が必要なため run を許可。
        // default (空 role) は gate と同じにするのが慣例（テスト互換性のため）。
        return sandbox.BuiltinPolicy{AllowedOps: map[string]struct{}{
            string(sandbox.OciOpRun): {},
        }}
    }
}
```

**必須事項**:
- 各 case に **根拠コメント** を書く（なぜ許可 or 禁止なのか）
- `default` case (空 role) は **gate と同じ policy** にする（テスト互換の慣例）
- セキュリティ上の懸念や関連 issue があれば併記する

---

## ステップ 5 — planner 連携

`PlanHook` / `PlanGate` が `mergeBuiltinCommands` で builtin を自動注入している場合のみ対応が必要。
project.yaml / kit.yaml の `builtin_commands:` で宣言している場合は **不要**。

自動注入する場合 (`internal/orchestrator/planner.go`):

```go
// PlanHook
BuiltinPolicies: DefaultBuiltinPolicies(RoleHook,
    mergeBuiltinCommands(meta.BuiltinCommands, []string{"boid", "oci"})),
// PlanGate
BuiltinPolicies: DefaultBuiltinPolicies(RoleGate,
    mergeBuiltinCommands(meta.BuiltinCommands, []string{"boid", "oci"})),
```

---

## ステップ 6 — broker dispatch 分岐

`internal/sandbox/broker.go` の `Handle()` に分岐を追加:

```go
func (b *Broker) Handle(req *ExecRequest) *ExecResponse {
    // ...
    if req.Command == "oci" {
        if entry.hasBuiltinPolicy("oci") {
            return handleOciBuiltinRequest(req, entry)
        }
        if def, ok := entry.Commands["oci"]; ok {
            return b.execCommand(req, def)
        }
        return &ExecResponse{ExitCode: 1, Stderr: "command not allowed: oci"}
    }
    // ...
}
```

**binding キャプチャが必要な場合** (`Register()` 冒頭に追加):

```go
if entry.hasBuiltinPolicy("oci") {
    var err error
    entry.Oci, err = captureOciBinding(ctx.ProjectDir)
    logOciBindingSnapshot(ctx, entry.Oci, err)
}
```

git の `captureGitBinding` はエージェントがリモート URL を書き換えても信頼済みスナップショットを使う設計。
外部 URL / リソース参照の改ざんを防ぐ必要がある場合のみキャプチャを追加する。

---

## ステップ 7 — テスト

### 7a. `builtin_policy_test.go` に matrix テストを追加

```go
// hook×oci は AllowedOps が空であること。
func TestDefaultBuiltinPolicies_HookOciIsEmpty(t *testing.T) {
    policies := DefaultBuiltinPolicies(RoleHook, []string{"oci"})
    if len(policies["oci"].AllowedOps) != 0 {
        t.Errorf("hook×oci AllowedOps should be empty, got %v", policies["oci"].AllowedOps)
    }
}

// gate×oci は {run} を含むこと。
func TestDefaultBuiltinPolicies_GateOciHasRun(t *testing.T) {
    policies := DefaultBuiltinPolicies(RoleGate, []string{"oci"})
    if !policies["oci"].Allows(string(sandbox.OciOpRun)) {
        t.Error("gate×oci should allow run")
    }
}

// empty role は gate と同じ policy であること。
func TestDefaultBuiltinPolicies_EmptyRoleEqualsGate_Oci(t *testing.T) {
    gate := DefaultBuiltinPolicies(RoleGate, []string{"oci"})
    empty := DefaultBuiltinPolicies("", []string{"oci"})
    if !opsEqual(gate["oci"].AllowedOps, empty["oci"].AllowedOps) {
        t.Error("default oci policy should equal gate oci policy")
    }
}
```

### 7b. `oci_builtin_test.go` を新設

最低限検証すべき項目:

- 禁止 op が拒否される（空 policy または該当 op を含まない policy）
- 許可 op が通る
- cwd が未設定 / 範囲外の場合にエラーになる

参照例: `internal/sandbox/git_builtin_test.go` のテストヘルパー (`initGitRepo`, `gateGitPolicies` 等)。

---

## ステップ 8 — セキュリティチェックリスト

新 builtin が外部通信やホストリソースアクセスを伴う場合に確認する:

- [ ] **最小権限**: hook に過剰な権限を与えていないか。hook は read-only / 通知のみが原則
- [ ] **ワークスペース分離**: `entry.Context.AllowedProjectIDs` など既存の workspace 分離を尊重しているか
- [ ] **secret 漏洩**: エラーメッセージや slog に secret / 認証情報が含まれていないか
- [ ] **信頼済みスナップショット**: 外部 URL やリソース参照をエージェントが書き換えられないか（git の `captureGitBinding` パターン参照）
- [ ] **ホストアクセス最小化**: 使用するホストリソース（ファイル, ネットワーク, プロセス）を最小化しているか
- [ ] **default role テスト**: 空 role の policy が gate と同一であることを `EmptyRoleEqualsGate` 相当のテストで確認

---

## 設計原則

| 原則 | 理由 |
|------|------|
| broker は role を知らない | role 判定は `DefaultBuiltinPolicies` に集約。broker は policy テーブルのみ参照 |
| policy は登録時にスタンプ | dispatch 時の role 判定を排除し、broker をシンプルに保つ |
| `default` = gate | テスト互換。production では Role は必ず設定されるが、空の場合に最も権限の多い role (gate) と同じにする |
| hook は fetch/push 禁止 | agent がホスト側リモートに直接アクセスすべきでない（git, oci とも同じ原則） |
