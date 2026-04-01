# Dependency Boundary Realignment Plan

## Goal

リファクタリング後の境界をもう一段整理し、
レイヤごとの統合テストを独立して書ける状態へ寄せる。

この計画の主眼は、
`api -> orchestrator/dispatcher -> sandbox` の責務分割を保ちながら、
振る舞い依存を consumer-side interface へ閉じ込めることにある。

## Architectural Rule

この計画は以下の原則に従う。

### 1. Data model dependency is acceptable

パッケージ間で純粋データモデルを共有すること自体は許容する。

許容条件:

- 所有権が明確である
- provider 固有の実装手順を暗黙に要求しない
- fake 実装でも同じ意味で扱える

### 2. Behavior dependency must be isolated by consumer-side interface

呼び出し側が必要とする振る舞いは、
呼び出し側 package に置いた interface で受ける。

### 3. Composition root stays in server

具体実装の配線は `server` に集約する。
`api` / `orchestrator` / `dispatcher` は
他レイヤの具象実装を new しない。

## Why This Plan Is Needed

現在の実装は `orchestrator -> dispatcher` の整理が進んだ一方で、
`dispatcher -> sandbox` にはまだ具象依存が残っている。

代表例:

- `dispatcher.Runner` が `*sandbox.Broker` を直接持つ
- `dispatcher` が `sandbox.WriteSandboxScripts` を直接呼ぶ
- `dispatcher` が `sandbox.WrapperConfig` を直接構築する
- `dispatcher.CommandDef` / `dispatcher.BindMount` が
  `sandbox` 型 alias になっている
- `server` 側の broker register adapter も `*sandbox.Broker` に依存している

この状態でも動作はするが、
以下の点で統合テストと境界管理が曖昧になる。

- `dispatcher` テストが `sandbox` 実装詳細に引きずられる
- known issue の再現テストがどのレイヤ責務か判別しづらい
- 具象依存のままでは fake 実装差し替えが中途半端になる

## Scope

この計画は主に以下を対象とする。

- `dispatcher -> sandbox` の behavior dependency
- `server -> sandbox` の wiring dependency
- レイヤ単位統合テストの再設計

以下は今回の主目的ではない。

- `orchestrator` の state machine 設計変更
- `sandbox.CommandDef` など共有データ型の全面移管
- 新規トップレベル package の導入を前提にした大規模再編

## Current Pressure Points

### A. Broker behavior is provider-owned

現状では `dispatcher` が broker の生成済み具象を直接受け取る。

結果:

- token registration / unregister の期待値を
  `dispatcher` 単独で検証しにくい
- fake broker を作れても、
  型境界上は `sandbox` 実装知識が残る

### B. Script preparation is provider-owned

現状では `dispatcher` が
`sandbox.WrapperConfig` を組み立てて `WriteSandboxScripts` を呼ぶ。

結果:

- `dispatcher` が sandbox 実装詳細を知りすぎる
- `WrapperConfig` の変更が `dispatcher` へ波及する
- script generation と job dispatch の責務境界が曖昧になる

### C. Provider-owned data DSL may leak behavior

`BindMount` は比較的素朴な DTO だが、
`CommandDef` は policy DSL に近い面がある。

特に以下は「単なるデータ」に見えても
behavior leakage の可能性がある。

- `AllowedPatterns`
- `DeniedPatterns`
- `AllowedSubcommands`
- `ExtractSubcommandFn`
- `RequireCwd`
- `AllowedCwdPrefixes`

この点は interface 導入後に改めて監査する。

## Target State

最終的に目指す形は以下。

### Dispatcher owns required behavior contracts

`dispatcher` 側に必要最小限の interface を置く。

例:

- `CommandBroker`
  - command registration
  - unregister
  - socket path exposure
- `SandboxPreparer` または `ExecutionPreparer`
  - dispatch 実行に必要な生成物を準備する

### Sandbox becomes a provider implementation

`sandbox` は上記 interface の実装になる。
`dispatcher` は `sandbox` 具象型ではなく interface だけを見る。

### Shared data remains shared, but ownership is explicit

共有データは直ちに全移管しなくてよい。
ただし「どの package が canonical owner か」は明示する。

方針:

- `BindMount` は shared DTO として扱う
- `CommandDef` は sandbox policy DSL を運ぶ transport shape として扱う
- dispatcher / orchestrator は `CommandDef` を保持してもよいが、
  解釈責務は sandbox に置く
- provider-owned behavior DSL になっているものは後段で再評価する

## Current Status

2026-04-01 時点の branch 状態は以下。

- Phase 0 は完了
  - `docs/runtime-review-findings.md` を source of truth として固定済み
- Phase 1 は「再現テストを追加する」という意味では完了
  - ただし high priority issue の一部はまだ failing test のまま残っている
- Phase 2 は完了
  - `dispatcher` の broker dependency は consumer-side interface 化済み
- Phase 3 は完了
  - `dispatcher` の sandbox preparation dependency は consumer-side interface 化済み
- Phase 5 は完了
  - `sandbox` concrete type への参照は `server` package の wiring にのみ残っている
  - `api` / `orchestrator` / `dispatcher` は `sandbox` 具象へ直接依存していない
- Phase 6 は完了
  - `dispatcher` の broker/token/tmux/cleanup 系テストは追加済み
  - `server` に最小限の cross-layer smoke test は追加済み
  - `api` / `orchestrator` / `sandbox` / `dispatcher` / `server` を跨ぐ `go test ./...` / `go test -race ./...` が通過している
- 現時点の最優先は追加の境界整理ではなく、
  Phase 1 で固定した既知不具合を green に戻すこと

このため、
以降の優先順位は
「未修正の runtime issue を先に潰す」
ことへ明示的に切り替える。

## Execution Plan

### Phase 0: Freeze Findings And Acceptance Criteria

目的:
既知問題と期待挙動を先に固定する。

Tasks:

1. `docs/runtime-review-findings.md` を source of truth とする
2. 各 issue に対応する統合テスト観点を確定する
3. 依存整理の完了条件を先に固定する

Completion criteria:

- 問題一覧が文書化されている
- テストで守るべき期待挙動が明文化されている

### Phase 1: Add Reproducer Tests Before Refactor

目的:
依存整理前に既知問題を再現できるようにする。

Tasks:

1. `dispatcher` 統合テストで並列 job 実行時の window 名競合を再現する
2. `api` 統合テストで request context cancel による background dispatch 中断を再現する
3. `sandbox` 統合テストで gate 実行 path 不整合を再現する
4. `sandbox` 統合テストで quoting 問題を再現する
5. `orchestrator` 統合テストで reload failure 時の挙動を固定する

Notes:

- この phase は bug fix 前提の failing test を含んでよい
- refactor の議論より先に「何を壊してはいけないか」を固定する

Completion criteria:

- 既知問題がテストケースとして存在する
- 少なくとも再現が自動化されている

### Phase 2: Isolate Broker Behavior Behind Dispatcher-Owned Interface

目的:
`dispatcher` から `sandbox.Broker` 具象依存を外す。

Tasks:

1. `dispatcher` に broker consumer-side interface を定義する
2. `Runner` の `Broker` フィールドを interface へ差し替える
3. `dispatcher/wire.go` を interface 注入前提へ変更する
4. `server` に `sandbox.Broker` -> `dispatcher` interface adapter を置く
5. `dispatcher` テストで fake broker を使えるようにする

Completion criteria:

- `dispatcher` が `*sandbox.Broker` を知らない
- token lifecycle を fake で検証できる

### Phase 3: Isolate Sandbox Preparation Behind Dispatcher-Owned Interface

目的:
`dispatcher` から `sandbox.WriteSandboxScripts` /
`sandbox.WrapperConfig` 具象依存を外す。

Tasks:

1. `dispatcher` に sandbox preparation interface を定義する
2. dispatch 実行に必要な spec を `dispatcher` 側で定義する
3. `sandbox` 実装は `dispatcher` spec から script / 実行生成物を作る adapter とする
4. `Runner.launchSandbox` を interface 経由に切り替える
5. `dispatcher` テストで fake preparer を使えるようにする

Completion criteria:

- `dispatcher` が `sandbox.WrapperConfig` を知らない
- script generation の結果を fake で差し替えられる

### Phase 3.5: Stabilize Known Runtime Issues Before More Boundary Cleanup

目的:
すでに追加済みの repro test を green に戻し、
以降の boundary/data ownership 整理を
安定した基準の上で進められるようにする。

Tasks:

1. `dispatcher` の tmux window 競合を job 単位 window で解消する
2. `api` の background dispatch loop が request context cancel を継承しないようにする
3. `orchestrator` の reload failure 時に stale meta を残さない挙動を固定する
4. `sandbox` の gate 実行経路を mount/copy 方針と一致させる
5. `sandbox` の shell quoting を修正し、
   path / payload / setup script がスペースや quote を含んでも壊れないようにする
6. `go test ./...` を回し、
   Phase 1 で追加した known issue repro test がすべて green であることを確認する

Completion criteria:

- `docs/runtime-review-findings.md` の high priority issue が
  failing test ではなく passing test として固定されている
- 既知不具合の修正前提で次の boundary cleanup へ進める

### Phase 4: Re-check Shared Data Ownership

目的:
shared data dependency と behavior leakage を見直す。

Tasks:

1. `BindMount` を shared DTO のまま残して問題ないか確認する
2. `CommandDef` が provider-owned policy DSL であることを前提に、
   その transport shape を dispatcher / orchestrator 側でどう保持するか明確化する
3. 必要なら `CommandDef` を
   「共有してよい plain data」と
   「provider 実装へ閉じるべき policy detail」に分ける
4. 分離が必要なら neutral spec package または consumer-owned model を検討する

Decision rule:

- fake 実装でも同じ意味で扱える mount data は shared DTO のままよい
- policy DSL は consumer-owned transport shape として鏡写しに留め、
  provider 固有アルゴリズム selector が漏れるなら再設計対象とする

Completion criteria:

- shared data と behavior DSL の境界が説明できる
- `dispatcher` のテストが `sandbox` 内部ルール理解なしに書ける

Current decision:

- `BindMount` は shared DTO のまま維持する
  - `Source` と `Mode` だけの素朴なデータであり、
    fake 実装でも同じ意味で扱える
- `CommandDef` は canonical shared model に昇格させない
  - `AllowedPatterns`
  - `DeniedPatterns`
  - `AllowedSubcommands`
  - `ExtractSubcommandFn`
  - `RequireCwd`
  - `AllowedCwdPrefixes`
    は `sandbox` の policy evaluator が意味を与える provider-side DSL である
- したがって当面は
  `orchestrator` / `dispatcher` / `sandbox` に
  明示的な transport shape を置き、
  境界で field-by-field conversion する方針を採る
- neutral spec package への移管は、
  `sandbox` 以外の provider が同じ DSL を本当に共有する必要が出た時点で再検討する

### Phase 5: Keep Server As Composition Root Only

目的:
`server` の役割を配線に限定する。

Tasks:

1. broker register adapter を interface ベースへ置き換える
2. `server` が `sandbox` 具象を知るのは wiring のみとする
3. `api` / `dispatcher` へ `sandbox` 具象を渡さない

Completion criteria:

- `server` だけが provider concrete type を new している
- 他レイヤは interface で受ける

Status:

- Complete
- 検証は
  `rg "sandbox\\." internal/server internal/api internal/orchestrator internal/dispatcher -g'*.go'`
  で確認できる

### Phase 6: Establish Independent Integration Test Suites

目的:
レイヤ単位で独立した統合テストが書ける状態を完成させる。

Tasks:

1. `api` 統合テスト
   - HTTP -> service -> tx -> repository
   - background dispatch lifecycle
2. `orchestrator` 統合テスト
   - evaluator
   - coordinator
   - state machine
3. `dispatcher` 統合テスト
   - broker token lifecycle
   - tmux interaction
   - job wait / cleanup
4. `sandbox` 統合テスト
   - broker
   - proxy
   - script generation
   - gate/hook execution導線
5. 最小限の cross-layer smoke test
   - `start action -> dispatch -> job done -> auto-advance`

Completion criteria:

- known issue の再発防止がレイヤ単位テストで担保される
- 各レイヤが他レイヤの具象なしで統合テストできる

Current status:

- Complete
- 追加済み:
  - `api`
    - background dispatch lifecycle の service-level repro test
  - `orchestrator`
    - evaluator / coordinator / state machine の統合系テスト
  - `dispatcher`
    - broker lifecycle / tmux window / wait / cleanup coverage
  - `sandbox`
    - broker / proxy / script generation / gate-hook 導線の統合系テスト
  - `server`
    - `start action -> dispatch -> job done -> auto-advance` の最小 cross-layer smoke test
  - `api` の background dispatch lifecycle repro coverage
  - `orchestrator` の reload failure semantics coverage
  - `sandbox` の gate path / quoting coverage
  - `server` の最小 cross-layer smoke coverage

## Definition Of Done

この計画は以下を満たしたら完了とみなす。

- `dispatcher` が `sandbox` 具象の振る舞いへ直接依存しない
- `server` が composition root としてのみ concrete type を配線する
- 既知の高優先度 issue が再現テストと修正でカバーされる
- 各レイヤに独立した統合テストがある
- `go test ./...` が通る

## Suggested Execution Order

1. Phase 0
2. Phase 1
3. Phase 2
4. Phase 3
5. Phase 3.5
6. Phase 5
7. Phase 4
8. Phase 6

理由:

- 当初は「再現テスト追加 -> interface 整理 -> bug fix」の順を想定していた
- 実際の branch は Phase 2 / 3 まで先行している
- この状態でさらに先へ進むと、
  failing repro test を抱えたまま boundary cleanup を重ねることになる
- したがって次の critical path は
  Phase 4 / 5 / 6 ではなく
  Phase 3.5 として既知不具合の修正を先に完了させること

## Verification Commands

- `go test ./...`
- `go test -race ./...`
- `rg "sandbox\\.Broker|\\*sandbox\\.Broker|WriteSandboxScripts|WrapperConfig" internal/dispatcher internal/api`
- `rg "sandbox\\." internal/dispatcher`
- `rg "sandbox\\." internal/api`
- `rg "sandbox\\." internal/orchestrator`

## Notes

- `sandbox.CommandDef` 依存は直ちに禁止としない
- ただし provider-owned policy DSL になっているなら再設計候補とする
- 今回の主目的は「共有データの完全排除」ではなく
  「behavior 依存の切り離し」と
  「統合テスト境界の明確化」である
