# Runtime Review Findings

## Purpose

大規模リファクタリング後のレビューで確認した、
修正優先度の高い問題を記録する。

この文書は「何を直す必要があるか」の整理を目的とし、
実行順序や依存関係整理の段取りは
`docs/dependency-boundary-realignment-plan.md` に分離する。

## Context

現行アーキテクチャは概ね以下の責務分割を目指している。

- `api`
  - HTTP 入出力
  - アプリケーションサービス
- `orchestrator`
  - 状態遷移
  - dispatch 条件評価
  - 実行要求モデル
- `dispatcher`
  - job 実行
  - tmux / worktree / broker 連携
- `sandbox`
  - 実行境界
  - broker / proxy / shell script 生成

`go test ./...` と `go test -race ./...` は通過しているが、
今回見つかった問題の多くは
「単体テストでは守れているが、レイヤをまたぐ実行経路では守れていない」
という性質を持つ。

## Findings

### 1. Concurrent hook/gate execution reuses the same tmux window

Severity:
critical

Symptoms:

- readonly hook の並列実行時に後続ジョブが先行ジョブを kill しうる
- gate の並列実行でも同様の競合が起こりうる

Current behavior:

- `dispatcher.Runner` は hook/gate 実行時の tmux window 名を
  `task-<taskID[:8]>` に固定している
- 起動前に常に `KillWindow` を呼んでいる
- `Coordinator` は readonly hook と gate を並列実行する

Impact:

- 並列 dispatch が成立しない
- 一部 job が未完了のまま消える
- payload merge や auto-advance が不安定になる

Relevant areas:

- `internal/dispatcher/runner.go`
- `internal/orchestrator/coordinator.go`

Expected direction:

- job 単位で一意な実行 window を持つ
- task 単位 window cleanup と job 単位実行 window を分ける
- dispatcher 統合テストで並列 dispatch を再現できるようにする

### 2. Background dispatch loop inherits request context

Severity:
high

Symptoms:

- `start` action 後にレスポンス返却が終わると、
  request context の cancel に引きずられて
  background dispatch loop が停止しうる

Current behavior:

- action handler は `r.Context()` を service に渡す
- service はその context を goroutine の `runDispatchLoop` にそのまま渡す
- job wait 側は context cancellation を監視している

Impact:

- hook/gate 待機が途中で中断される
- payload persist や auto-advance が走らない
- 実運用で「開始されたが進まない task」を作る可能性がある

Relevant areas:

- `internal/api/action.go`
- `internal/api/service.go`
- `internal/dispatcher/orchestrator_adapter.go`
- `internal/dispatcher/runner.go`

Expected direction:

- request lifecycle と background workflow lifecycle を切り離す
- api 統合テストで「レスポンス返却後も dispatch が継続する」ことを守る

### 3. Gate execution path is inconsistent with sandbox mounts

Severity:
high

Symptoms:

- gate は「filesystem を持たない」設計意図なのに、
  実際には sandbox 内で `<workDir>/.boid/gates/<script>` を直接実行しようとする
- そのパスは gate role では mount されていない

Current behavior:

- sandbox plan は gate role で project dir / workspace dir / `.boid` mount を省略する
- inner script は gate script を project path 直下から実行する

Impact:

- gate 実行が実質失敗する
- gate の責務分離がコード上で破綻している
- 現行テストは mount しないことだけ見ており、実行可能性を守れていない

Relevant areas:

- `internal/sandbox/plan.go`
- `internal/sandbox/script.go`
- `internal/sandbox/script_test.go`

Expected direction:

- gate 実行方式を 1 つに固定する
- staging するのか、専用 mount を持つのか、stdin 実行に寄せるのかを決める
- sandbox 統合テストで「gate role が実際に script へ到達できる」ことを守る

### 4. Shell script generation uses raw string interpolation

Severity:
high

Symptoms:

- path に空白や shell metacharacter が入ると壊れうる
- payload / task JSON に quote が含まれると壊れうる
- 条件次第では shell injection の踏み台になりうる

Current behavior:

- setup / outer / inner script 生成で
  path, env, JSON, guard expression を生文字列で埋め込んでいる

Impact:

- 正常系でもスペース入り path で壊れる
- sandbox の安全性を shell quoting に依存してしまう
- テスト環境では通っても実運用入力で壊れるリスクが高い

Relevant areas:

- `internal/sandbox/script.go`
- `internal/sandbox/render.go`

Expected direction:

- shell quoting を共通 helper に集約する
- 可能なら script 生成責務をさらに小さく分ける
- sandbox 統合テストで path / JSON quoting を守る

### 5. Project reload may keep stale metadata after load failure

Severity:
medium

Symptoms:

- `project.yaml` が壊れたあと reload に失敗しても、
  旧 meta が store に残り続ける可能性がある

Current behavior:

- `ProjectStore.Load` は成功時のみ差し替える
- `LoadAll` は失敗を返すが、既存 meta の無効化や隔離をしない

Impact:

- 壊れた設定であることを operator が認識しづらい
- 実行時に旧 hook/gate 定義で動作し続ける可能性がある

Relevant areas:

- `internal/orchestrator/project_store.go`
- `internal/api/service.go`

Expected direction:

- reload semantics を明示する
- load failure 時に stale meta を残すのか、無効化するのかを決める
- orchestrator 統合テストで reload failure 時の挙動を固定する

## Testing Gaps Exposed By These Findings

現在不足しているのは、
個々の関数の正しさよりもレイヤ単位の統合検証である。

### API layer

- request context と background workflow の切り離し確認
- action 適用後の非同期 dispatch 起動確認

### Orchestrator layer

- hook/gate 並列 dispatch と auto-advance の一貫確認
- reload semantics の確認

### Dispatcher layer

- tmux window naming
- broker token lifecycle
- job wait と cleanup の相互作用

### Sandbox layer

- gate/hook 実行導線
- shell quoting
- readonly / worktree / proxy 境界

## Immediate Recommendation

依存関係整理の前に、
上記 5 件を「再現できる統合テスト」として固定しておくことを推奨する。

理由:

- 境界整理の途中で不具合の見え方が変わっても、
  期待挙動を失わずに済む
- 依存逆転の成果を
  「テストが書きやすくなった」だけでなく
  「既知不具合を再発させにくくなった」として評価できる
