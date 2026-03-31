# Internal Refactor Plan

## Goal

`internal` 配下のトップレベル構成を最終的に以下へ収束させる。

- `api`
- `server`
- `client`
- `orchestrator`
- `dispatcher`
- `db`
- `sandbox`

目標アーキテクチャの原則:

- `server` は composition root に限定する
- `api` は HTTP 入出力変換に限定する
- `orchestrator` は状態遷移、計画、消費側インタフェース定義を持つ
- `dispatcher` は dispatch 実行とその補助実装を持つ
- `sandbox` は実行境界を持つ
- 直通依存は共有モデルのみ
- メソッドのインタフェースは消費側で定義する

現状のトップレベルパッケージ:

- `api`
- `client`
- `db`
- `dispatcher`
- `orchestrator`
- `sandbox`
- `server`

## Working Style

このリファクタリングは以下の単位で進める。

1. 1フェーズずつ実装する
2. 各フェーズに明確な達成条件を置く
3. フェーズ完了後にテストと依存確認を行う
4. フェーズごとにコミットする

PR 前提ではなく、ローカルで段階的に積み上げる。

## Phase Status

- [x] Phase 0: Baseline And Guardrails
- [x] Phase 1: Package Mapping Fix
- [x] Phase 2: Shared Models First
- [x] Phase 3: Orchestrator Boundary Cleanup
- [x] Phase 4: Worktree Into Dispatcher
- [x] Phase 5: Secret Into Dispatcher
- [x] Phase 6: Host Command Policy Into Sandbox
- [x] Phase 7: Projectspec And Kit Into Orchestrator
- [x] Phase 8: Database Boundary Cleanup
- [x] Phase 9: Thin API And Thin Server
- [x] Phase 10: Final Convergence

## Definition of Done

各フェーズは以下を満たしたら完了とみなす。

- そのフェーズの達成条件を満たす
- コンパイルが通る
- 関連テストが通る
- 新しい禁止依存を増やしていない
- 次フェーズの前提が文書とコード上で明確

## Phase 0: Baseline And Guardrails

目的:
現状を固定し、以後のリファクタで境界の揺れを防ぐ。

達成条件:

- この計画ファイルがリポジトリに存在する
- 現在の `internal` 依存状況を確認できるコマンドを決める
- 最終的に残すトップレベルパッケージを固定する
- `internal/project` が空であることを確認済みとする

実装メモ:

- `go list` と `rg` で import グラフを追う
- 必要なら後続フェーズで依存チェック用スクリプトを追加する
- `scripts/check-internal-architecture.sh current` を baseline 確認に使う

完了確認:

- 本ファイルが存在する
- チーム内でこの進め方を採用することに合意している
- baseline 検査コマンドがリポジトリ上に存在する

## Phase 1: Package Mapping Fix

目的:
残存パッケージをどこへ吸収するかを固定する。

達成条件:

- 各残存パッケージの移管先が確定している
- 以後の実装で新規トップレベル package を増やさない

移管先:

- `projectspec` -> `orchestrator`
- `kit` -> `orchestrator`
- `worktree` -> `dispatcher`
- `secret` -> `dispatcher`
- `hostcmd` -> `sandbox` を優先、難しければ `dispatcher`
- `db` -> domain 所有 package ではなく、SQLite utility / transaction helper に縮退させる

完了確認:

- この対応表に変更がない
- 実装順が確定している
- `scripts/check-internal-architecture.sh target` で最終 package 目標を確認できる

## Phase 2: Shared Models First

目的:
ロジック移動の前に、共有モデルの帰属を固める。

達成条件:

- cross-package で共有する型を明示できる
- `projectspec` の純粋データ型の受け皿を決める
- `dispatcher` と `sandbox` の境界型を整理する
- 型 alias に頼った境界を減らし始める

対象:

- project meta / kit meta / task behavior 系
- dispatch request / result 系
- command / bind / policy 系

完了確認:

- 共有モデルとロジックの所在が混ざっていない

Phase 2 実施メモ:

- `dispatcher.CommandDef` は `sandbox.CommandDef` の alias とする
- `dispatcher.BindMount` は `sandbox.BindMount` の alias とする
- 実行境界の command / bind モデルは `sandbox` 所有とする

## Phase 3: Orchestrator Boundary Cleanup

目的:
`orchestrator` を消費側インタフェース定義の中心に寄せる。

達成条件:

- `orchestrator` が具体的な `dispatcher` 実装型に依存しない
- `DispatchPlanner` が `dispatcher` の具体型を直接返さない形へ寄せる
- `orchestrator` 側の interface が `dispatcher` の詳細を漏らさない

対象:

- `internal/orchestrator/planner.go`
- `internal/orchestrator/dispatch_adapter.go`
- `internal/orchestrator/types.go`

完了確認:

- `orchestrator` の外部依存がモデル中心になっている
- dispatcher 由来の詳細型露出が減っている

Phase 3 実施メモ:

- `DispatchPlanner` は `dispatcher.DispatchPlan` ではなく `orchestrator.DispatchRequest` を返す
- `dispatch_adapter` が `DispatchRequest -> dispatcher.DispatchPlan` の境界変換を担当する
- `dispatcher` 依存の詳細型は adapter 内部に閉じ込める

## Phase 4: Worktree Into Dispatcher

目的:
`worktree` を `dispatcher` 側責務へ寄せる。

達成条件:

- `worktree.Manager` 相当の責務が `dispatcher` 配下へ移る
- `server/worktree_adapter.go` の役割を不要にするか、`dispatcher` 側へ移す
- `worktree -> orchestrator` の逆依存を除去する

対象:

- `internal/worktree`
- `internal/server/worktree_adapter.go`

完了確認:

- `internal/worktree` を削除できる、または package 内身が空になる
- worktree cleanup / prepare が `dispatcher` 主導になる

Phase 4 実施メモ:

- `Worktree`, store, manager を `dispatcher` 配下へ移した
- `server/worktree_adapter.go` は `dispatcher.WorktreeManager` を使う薄い adapter に縮小した
- worktree cleanup 時の project lookup は `api` 側で行い、`dispatcher.WorktreeManager` は `projectDir` を受け取る

## Phase 5: Secret Into Dispatcher

目的:
`secret` を dispatch 実行補助として `dispatcher` に寄せる。

達成条件:

- `dispatcher` が必要な secret access を自身の内部実装として持つ
- `server` が `secret.Store` を直接扱わない
- API には必要最小限の secret interface のみを渡す

対象:

- `internal/secret`
- `internal/api/secret.go`
- `internal/server/server.go`

完了確認:

- `internal/secret` を削除できる、または package 外露出がなくなる

Phase 5 実施メモ:

- `SecretStore` と keyfile 処理を `dispatcher` 配下へ移した
- `api.SecretHandler` は具体型ではなく `SecretStore` interface を受ける
- `server` は `dispatcher.NewSecretStore` と `dispatcher.LoadOrCreateKey` を使う

## Phase 6: Host Command Policy Into Sandbox

目的:
実行ポリシーを `sandbox` 境界へ寄せる。

達成条件:

- `hostcmd` の policy ロジックが `sandbox` に統合される
- `hostcmd` package を削除できる
- command policy の型定義と評価ロジックの所在が一致する

対象:

- `internal/hostcmd`
- `internal/sandbox`

完了確認:

- `internal/hostcmd` が不要になる

Phase 6 実施メモ:

- `hostcmd` の alias API はすべて `sandbox` に吸収した
- policy / broker / protocol / shim のテストを `sandbox` package へ移した
- `internal/hostcmd` は削除済み

## Phase 7: Projectspec And Kit Into Orchestrator

目的:
プロジェクト定義と kit 解決を `orchestrator` の内部責務へ寄せる。

達成条件:

- `projectspec` のロジックが `orchestrator` 配下へ移る
- `kit` の registry / staging ロジックが `orchestrator` 配下へ移る
- `server` が `kit.Registry` を直接 new しない

対象:

- `internal/projectspec`
- `internal/kit`
- `internal/orchestrator/project_store.go`
- `internal/server/server.go`

完了確認:

- `internal/projectspec` と `internal/kit` を削除できる

Phase 7 実施メモ:

- `projectspec` の loader / payload / scripts / types を `orchestrator` 配下へ移した
- `kit` の registry / stage を `orchestrator` 配下へ移した
- wrapper file を削除し、外側の参照は `orchestrator` へ寄せた
- `internal/projectspec` と `internal/kit` は削除済み

## Phase 8: Database Boundary Cleanup

目的:
`db` を共有パッケージとして使う構造をやめる。

達成条件:

- `api`, `orchestrator`, `dispatcher` が `*db.DB` を直接保持しない
- store/repository interface は消費側に置く
- SQLite 実装は各責務側に閉じる

対象:

- `internal/db`
- `internal/api/*`
- `internal/orchestrator/*`
- `internal/dispatcher/*`

完了確認:

- `internal/db` を削除できる、または汎用ユーティリティに縮退している
- `db.DBTX` への広域依存が解消されている

Phase 8 実施メモ:

- `api` は `*db.DB` を保持せず、消費側 interface と transaction interface を受ける形へ変更した
- `orchestrator` / `dispatcher` の store は共有 utility としての `db.DBTX` を参照する
- `internal/db` は SQLite open / transaction helper に縮退した
- migration は `internal/db/migrate` へ分離した
- query/store テストは `internal/db` から各責務 package へ移設した
- `internal/db` は domain 所有 package ではなく低レベル utility package として扱う

## Phase 9: Thin API And Thin Server

目的:
`api` と `server` を最終形へ寄せる。

達成条件:

- `api` が具体実装型に触れない
- `server` が配線だけを持つ
- handler から状態遷移や cleanup の詳細を追い出す

対象:

- `internal/api/action.go`
- `internal/api/job.go`
- `internal/api/project.go`
- `internal/api/task.go`
- `internal/api/web.go`
- `internal/server/server.go`

完了確認:

- `api` の import が薄い
- `server.New` の責務が明確に縮小している

Phase 9 実施メモ:

- `ActionHandler` と `JobHandler` の状態遷移、dispatch loop、cleanup は `api.TaskWorkflowService` へ移した
- `api` handler は HTTP request/response の decode / encode と service error 変換に寄せた
- `ProjectHandler`, `TaskHandler`, `WebHandler` も service interface 経由に寄せた
- `server.New` は DB open / migration / core object 構築に集中し、route 配線と runtime 組み立ては `internal/server/wire.go` へ分離した
- `api/worktree.go` は削除し、worktree cleanup は service 内の workflow 処理として統合した

Phase 9 残件:

- `api` から `dispatcher` 具体型依存を除去済み
- broker register handler は `api` 配下へ移し、`server` は route mount のみを担当する
- `scripts/check-internal-architecture.sh current|target` と `go test ./...` が通ることを確認済み

## Phase 10: Final Convergence

目的:
不要 package を消し、最終構成へ収束させる。

達成条件:

- `internal` 直下に残る package が目標構成だけになる
- テストが通る
- 禁止依存のチェック手段がある

完了確認:

- `find internal -maxdepth 1 -type d` で以下のみが残る
- `internal/api`
- `internal/server`
- `internal/client`
- `internal/orchestrator`
- `internal/dispatcher`
- `internal/db`
- `internal/sandbox`

## Suggested Execution Order

1. Phase 0
2. Phase 1
3. Phase 2
4. Phase 3
5. Phase 4
6. Phase 5
7. Phase 6
8. Phase 7
9. Phase 8
10. Phase 9
11. Phase 10

## Current Remaining Work

- 現時点のフェーズ残件はなし

## Commit Policy

各フェーズは原則1コミットにまとめる。

コミット前チェック:

- `go test ./...`
- `scripts/check-internal-architecture.sh current`
- import 追加差分の確認
- 達成条件を満たしているかの目視確認

コミットメッセージ例:

- `refactor: fix internal package mapping`
- `refactor: isolate orchestrator boundary`
- `refactor: move worktree into dispatcher`

## Notes

- 初期開発段階では、大きな横断整理を避けずに進める
- ただしフェーズ跨ぎの変更は避ける
- 設計が揺れた場合は、先に本ファイルを更新してからコードを動かす

## Current Baseline

Phase 0/1 時点の `internal` 直下 package:

- `api`
- `client`
- `db`
- `dispatcher`
- `orchestrator`
- `project`
- `sandbox`
- `server`

補足:

- `internal/project` は削除済み
- `internal/worktree` は削除済み
- `internal/secret` は削除済み
- `internal/hostcmd` は削除済み
- `internal/projectspec` は削除済み
- `internal/kit` は削除済み
- 現状の baseline と最終 target の両方を `scripts/check-internal-architecture.sh` で検査する

## Final State

最終確認:

- `go test ./...` が通る
- `scripts/check-internal-architecture.sh current` が通る
- `scripts/check-internal-architecture.sh target` が通る
- `find internal -maxdepth 1 -type d` は `api`, `client`, `db`, `dispatcher`, `orchestrator`, `sandbox`, `server` のみを返す
- `db` は domain package ではなく SQLite utility / transaction helper package として残す
