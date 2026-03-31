# Dispatcher Orchestrator Dependency Inversion Plan

## Goal

`orchestrator -> dispatcher` の依存を解消し、
`dispatcher -> orchestrator` の方向へ整理する。

この変更により、`dispatcher` を
「orchestrator の実行要求を受け取り、sandbox / runner へ橋渡しする層」
として明確化する。

## Current State

現状では `internal/orchestrator/dispatch_adapter.go` が
`dispatcher.DispatchPlan` と `dispatcher.JobCompletionResult` を直接扱っており、
`orchestrator` から `dispatcher` への依存が残っている。

依存は adapter 1 ファイルに閉じ込められているが、
責務としては `dispatcher` 側へ寄せる方が自然である。

## Target State

最終的な責務分担は以下とする。

- `orchestrator`
  - 状態遷移
  - 実行要求 `DispatchRequest` の生成
  - 実行インタフェース定義
- `dispatcher`
  - `orchestrator.DispatchRequest` の受け取り
  - `DispatchRequest -> sandbox / runner` 実行計画への変換
  - job 実行と完了待機
- `server`
  - 配線のみ

## Architectural Rule

### Dispatcher が依存してよい orchestrator 要素

- `orchestrator.DispatchRequest`
- `orchestrator.JobCompletion`
- `orchestrator.HookExecutor`
- `orchestrator.GateExecutor`
- `orchestrator.JobWaiter`
- `orchestrator.Role`
- `orchestrator.CommandDef`
- `orchestrator.BindMount`

### Dispatcher が依存してはいけない orchestrator 要素

- repository / store 実装
- state machine 実装
- project/meta load ロジック
- task transition / evaluator の内部ロジック

この依存は「境界モデルと消費側 interface のみ」に限定する。

## Definition of Done

この計画は以下を満たしたら完了とみなす。

- `internal/orchestrator` から `internal/dispatcher` import が消えている
- `dispatch_adapter` 相当の翻訳責務が `internal/dispatcher` 配下に移っている
- `dispatcher` が `orchestrator.HookExecutor`, `GateExecutor`, `JobWaiter` を実装する
- `server` が新しい依存方向で配線できている
- `go test ./...` が通る
- `scripts/check-internal-architecture.sh current` が通る
- `scripts/check-internal-architecture.sh target` が通る

## Phase 1: Boundary Fixed

目的:
依存逆転後の境界を先に固定し、実装のぶれを防ぐ。

### Tasks

1. `DispatchRequest` は `orchestrator` 所有の canonical request model として維持する
2. `JobCompletion` は `orchestrator` 所有の完了結果型として維持する
3. `dispatcher` が依存してよい `orchestrator` 型を文書で固定する
4. `dispatcher` が依存してはいけない `orchestrator` 実装を文書で固定する

### Completion Criteria

- この文書がリポジトリに存在する
- 許可依存 / 禁止依存が明文化されている

## Phase 2: Move Translation Layer Into Dispatcher

目的:
翻訳責務を `orchestrator` から `dispatcher` へ移す。

### Tasks

1. `internal/orchestrator/dispatch_adapter.go` の責務を棚卸しする
2. `internal/dispatcher` に新しい adapter を追加する
   - 例: `internal/dispatcher/orchestrator_adapter.go`
3. `DispatchRequest -> DispatchPlan` 変換を `dispatcher` へ移す
4. `WaitForJobCtx -> orchestrator.JobCompletion` 変換も `dispatcher` へ移す

### Completion Criteria

- `dispatch_adapter` 相当コードが `dispatcher` 配下にある
- `orchestrator` は `dispatcher.DispatchPlan` を知らない

## Phase 3: Reframe Dispatcher As Orchestrator Request Consumer

目的:
`dispatcher` を `orchestrator` 実行要求の消費者として再定義する。

### Tasks

1. `dispatcher` に `HookExecutor` 実装を置く
2. `dispatcher` に `GateExecutor` 実装を置く
3. `dispatcher` に `JobWaiter` 実装を置く
4. 内部的には `DispatchRequest` から既存 runner / sandbox 実装へ橋渡しする

### Notes

外部境界では `orchestrator.DispatchRequest` を正準入力とする。
`dispatcher.DispatchPlan` が必要なら内部具体型として残してよい。

### Completion Criteria

- `dispatcher` が `orchestrator` interface を実装している
- `server` から見た配線が単純になっている

## Phase 4: Update Server Wiring

目的:
新しい依存方向で composition root を組み直す。

### Tasks

1. `server/wire.go` で planner を組み立てる
2. `dispatcher` の adapter に planner と runner を注入する
3. `Coordinator` には `dispatcher` 側 adapter を渡す
4. `orchestrator.NewDispatchAdapter(...)` 呼び出しを削除する

### Completion Criteria

- `server` が唯一の配線点になっている
- `orchestrator` から `dispatcher` への直接参照がなくなっている

## Phase 5: Cleanup

目的:
旧構成の残骸を取り除き、最終状態に収束させる。

### Tasks

1. `internal/orchestrator/dispatch_adapter.go` を削除する
2. 不要になった helper / 変換関数を削除する
3. docs を更新する
4. architecture check の観点を確認する

### Completion Criteria

- `rg "github.com/novshi-tech/boid/internal/dispatcher" internal/orchestrator` が 0 件
- テストが通る
- docs とコードの責務分担が一致する

## Suggested Execution Order

1. Phase 1
2. Phase 2
3. Phase 3
4. Phase 4
5. Phase 5

## Verification Commands

- `go test ./...`
- `scripts/check-internal-architecture.sh current`
- `scripts/check-internal-architecture.sh target`
- `rg "github.com/novshi-tech/boid/internal/dispatcher" internal/orchestrator`
- `rg "DispatchRequest|JobCompletion" internal/dispatcher`

## Risks

- `dispatcher` が `orchestrator` 実装詳細まで飲み込んで責務過多になる
- planner と adapter の責務が混ざる
- `DispatchRequest` と `DispatchPlan` の二重モデルが中途半端に残る

## Guardrails

- `dispatcher` が依存するのは `orchestrator` の境界モデルまでに限定する
- planner は引き続き `orchestrator` 側に置く
- `server` 以外で wiring を持たない

## Commit Split Suggestion

1. `refactor: move dispatch adapter into dispatcher`
2. `refactor: invert dispatcher dependency on orchestrator`
3. `docs: sync dispatcher orchestrator dependency design`
