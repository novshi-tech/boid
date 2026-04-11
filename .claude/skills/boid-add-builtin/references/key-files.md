# key-files: boid builtin 追加時の主要ファイル

## 参照元ファイル

| ファイル | 役割 |
|---------|------|
| `internal/sandbox/protocol.go` | `BuiltinPolicy` 型、`ExecRequest`、Op 型・定数の定義 |
| `internal/orchestrator/builtin_policy.go` | `DefaultBuiltinPolicies` / `policyFor` — policy テーブル |
| `internal/sandbox/broker.go` | `Handle()` / `Register()` / `allowsBuiltinOp` ヘルパ |
| `internal/sandbox/git_builtin.go` | policy チェックを冒頭に持つ handler の実装例 |
| `internal/orchestrator/spec_loader.go` | `validBuiltinCommands` マップ |
| `internal/orchestrator/planner.go` | `PlanHook` / `PlanGate` での `mergeBuiltinCommands` 呼び出し |

## builtin 実装に関わるキー型・関数

### `internal/sandbox/protocol.go`

```go
// builtin の op 許可セットを保持する型
type BuiltinPolicy struct {
    AllowedOps map[string]struct{}
}
func (p BuiltinPolicy) Allows(op string) bool

// 全 builtin リクエストのエントリポイント
type ExecRequest struct {
    Command string
    Token   string
    Cwd     string
    Boid    *BoidRequest  // boid builtin
    Git     *GitRequest   // git builtin
    // 新 builtin はここにフィールドを追加
}
```

### `internal/sandbox/broker.go`

```go
// token に紐づく policy があるか確認
func (e *tokenEntry) hasBuiltinPolicy(name string) bool

// 特定の op が policy で許可されているか確認
func (e *tokenEntry) allowsBuiltinOp(name, op string) bool

// tokenEntry — 登録時にスタンプされた policy を保持
type tokenEntry struct {
    Context         TokenContext
    Commands        map[string]CommandDef
    BuiltinPolicies map[string]BuiltinPolicy
    Git             *GitBinding  // git 用のスナップショット
    // 新 builtin の binding は必要な場合のみ追加
}
```

### `internal/orchestrator/builtin_policy.go`

```go
// role と builtin 名から policy を返すエントリポイント
func DefaultBuiltinPolicies(role Role, names []string) map[string]sandbox.BuiltinPolicy

// 個別 builtin の policy 関数を追加するスイッチ
func policyFor(role Role, name string) sandbox.BuiltinPolicy
```

## 既存 builtin の policy 決定根拠

### boid builtin

| role | 許可 op |
|------|---------|
| hook | `job_done`, `task_get` |
| gate (default) | `job_done`, `task_create`, `task_update`, `task_import` |

hook は agent が task を作成・更新しないよう制限している（read-only + 完了通知のみ）。

### git builtin

| role | 許可 op |
|------|---------|
| hook | なし（空 policy） |
| gate (default) | `fetch`, `push` |

hook からの broker 経由 git 操作は禁止。agent はホスト側リモートに直接アクセスすべきでない。

## テストファイルの場所

| テスト | ファイル |
|--------|---------|
| policy matrix | `internal/orchestrator/builtin_policy_test.go` |
| git handler | `internal/sandbox/git_builtin_test.go` |
| 新 builtin handler | `internal/sandbox/<name>_builtin_test.go` (新設) |
