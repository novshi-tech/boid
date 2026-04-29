# artifact.children.* 予約 namespace と virtual 評価

## 背景: フェーズ依存のユースケース

`plan` / `auto_plan` タスクは `create-subtasks` gate で子タスクを生成すると即座に `done` に遷移する。
次フェーズの `plan` タスクを「前フェーズの子タスクが全て完了するまで」待たせたい場合、
既存の `depends_on` + `depends_on_payload: "artifact.auto-merge.merged"` では表現できない。
親タスクの payload には子の完了情報が書き込まれないためである。

`artifact.children.*` 予約 namespace はこの問題を解決する。

## 予約 namespace `artifact.children.*` のルール

- **読み取り専用**: `depends_on_payload` のキーとして参照できる。
- **書き込み禁止**: API / kit / hook / gate 経由での payload 書き込みはエラーとして拒否される（400 Bad Request）。
- **virtual 評価**: キーの値は Task 構造体の派生フィールド（`TotalChildCount` / `DoneChildCount` / `AbortedChildCount`）から実行時に計算される。payload に実体は存在しない。

## 現在定義されているキーと計算式

| `depends_on_payload` キー | 計算式 | 意味 |
|---|---|---|
| `artifact.children.all_done` | `TotalChildCount > 0 && DoneChildCount == TotalChildCount` | 全子タスクが `done` |
| `artifact.children.all_resolved` | `TotalChildCount > 0 && (DoneChildCount + AbortedChildCount) == TotalChildCount` | 全子タスクが `done` または `aborted` |

## 使用例

```yaml
# Phase 2 は Phase 1 の全子タスクが done になってから start する
- title: "Phase 2"
  depends_on: ["phase1-task-id"]
  depends_on_payload: "artifact.children.all_done"
  auto_start: true
```

## 将来的にキーを追加する場合のガイドライン

1. `orchestrator/blocked.go` の `ResolvePayloadValue` 関数の `switch` に新しいケースを追加する。
2. キー名は必ず `artifact.children.` で始める。
3. 計算に必要なフィールドが `Task` 構造体に存在しない場合はフィールドを追加してから実装する。
4. 追加したキーのテストを `orchestrator/blocked_test.go` に記述する。
5. このドキュメントのキー一覧テーブルを更新する。

## kit 側で `artifact.children.*` を書いてはいけない理由

- 子タスク数はデータベース側でカウントされるシステム情報であり、アプリケーションコードが書き込むべき値ではない。
- kit が誤った値を書き込んでも検出できないため、virtual 評価によって常に正確な値が保証される。
- 書き込みを禁止することで「読み取り専用の計算済み値」という契約が明確になる。
