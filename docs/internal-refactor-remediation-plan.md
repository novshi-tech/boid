# Internal Refactor Remediation Plan

## Goal

リファクタリング完了後レビューで見つかった残課題を解消し、
`docs/internal-refactor-plan.md` の意図と実装を一致させる。

対象は以下の 3 点に絞る。

- `api` から `dispatcher` 具体型を追い出す
- `server` を composition root にさらに寄せる
- 実装とドキュメントの API 契約を同期する

## Scope

この追加計画では新規機能は扱わない。
既存リファクタリングの仕上げとして、境界の薄さと責務分離の不足を是正する。

## Review Findings Summary

レビューで確認した主な論点:

- `api` が `dispatcher.Job` と `dispatcher.JobCompletionResult` を直接扱っている
- `server` に broker register API の request/response 型と HTTP ハンドラが残っている
- secret API の docs と実装が path parameter と query parameter で不一致

## Definition of Done

この是正計画は以下を満たしたら完了とみなす。

- `internal/api` から `internal/dispatcher` import が消えている
- `api` が `dispatcher` 具体型を公開していない
- `server.go` が HTTP endpoint 固有の request/response と handler ロジックを持たない
- secret API の docs, CLI, handler 実装が同一契約で揃っている
- `go test ./...` が通る
- `scripts/check-internal-architecture.sh current` が通る
- `scripts/check-internal-architecture.sh target` が通る

## Phase A: Purify API Boundary

目的:
`api` を消費側 interface と API 所有モデルに寄せ、`dispatcher` 具体型依存を除去する。

### Tasks

1. `api` 所有の job model を追加する
2. `WorkflowService`, `JobStore`, `JobLifecycle`, `TaskDetailView` を `api` model ベースに置き換える
3. `api/service.go` から `dispatcher.JobStatus*` 依存を除去する
4. `api/job.go` と `api/web.go` を `api` model で閉じる
5. `server/api_store.go` に `dispatcher` との変換 adapter を集約する
6. `internal/api` のテストを `api` model ベースへ更新する

### Target Files

- `internal/api/store.go`
- `internal/api/service.go`
- `internal/api/job.go`
- `internal/api/web.go`
- `internal/api/service_test.go`
- `internal/server/api_store.go`

### Completion Criteria

- `rg "internal/dispatcher|dispatcher\\." internal/api` が 0 件
- `TaskDetailView.Jobs` が `api` 所有型になる
- `WorkflowService.CompleteJob` の返り値が `api` 所有型になる
- `JobLifecycle.CompleteJob` が `api` 所有 completion model を受ける

## Phase B: Shrink Server To Composition Root

目的:
`server` に残っている HTTP 詳細を外へ出し、配線と lifecycle に責務を限定する。

### Tasks

1. broker register API を `internal/api` へ移す
2. broker 登録に必要な最小 interface を `api` 側に定義する
3. `server/wire.go` は handler 生成と route mount のみ行う形にする
4. `server.go` から endpoint 固有 request/response struct と JSON 処理を除去する
5. 不要な accessor があれば見直す

### Target Files

- `internal/server/server.go`
- `internal/server/wire.go`
- `internal/api/store.go`
- `internal/api/broker.go` または同等の新規ファイル

### Completion Criteria

- `internal/server` に broker register 用の HTTP 契約型が残っていない
- `server.go` が DB, runtime, listener, lifecycle 管理に集中している
- `api` handler が `sandbox.Broker` 具体型を知らない

## Phase C: Sync Docs And API Contract

目的:
実装と `docs` のズレをなくし、レビュー時の解釈差分を解消する。

### Tasks

1. secret API の最終契約を決める
2. handler と CLI をその契約へ統一する
3. `docs/tasks-hostcmd-git.md` を実装と一致させる
4. 必要なら `docs/internal-refactor-plan.md` の残件記述を更新する

### Recommended Direction

secret API は path parameter へ寄せる。

- `DELETE /api/secrets/{key}`
- `GET /api/secrets/{key}/value`

理由:

- ドキュメント記載と一致させやすい
- REST 形式として自然
- CLI からも扱いやすい

### Target Files

- `internal/api/secret.go`
- `cmd/secret.go`
- `docs/tasks-hostcmd-git.md`
- `docs/internal-refactor-plan.md`

### Completion Criteria

- docs, handler, CLI が同一 endpoint を使う
- secret API の path 設計に揺れがない

## Execution Order

1. Phase A
2. Phase B
3. Phase C

この順序を推奨する。
Phase A が境界整理の本体で、Phase B はその上に乗る責務分離、
Phase C は最終的な契約固定として扱う。

## Verification Commands

実装後は以下で確認する。

- `go test ./...`
- `scripts/check-internal-architecture.sh current`
- `scripts/check-internal-architecture.sh target`
- `rg "internal/dispatcher|dispatcher\\." internal/api`
- `rg "type .*Request|type .*Response|json:\\\".*\\\"" internal/server`
- `rg -n "/api/secrets" cmd internal docs`

期待値:

- `internal/api` に `dispatcher` 依存が残らない
- `internal/server` に endpoint 固有の契約型が残らない
- secret API の docs, CLI, handler が一致する

## Commit Split Suggestion

コミットは以下の分割が妥当。

1. `refactor: remove dispatcher types from api boundary`
2. `refactor: move broker registration handler out of server`
3. `docs: sync secret api contract with implementation`

## Notes

- この計画は既存の `docs/internal-refactor-plan.md` を補完する追加文書として扱う
- 実装途中で方針が変わる場合は、先にこの文書を更新してからコードを変更する
