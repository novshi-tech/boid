# Hook/Gate 分離 — 残タスク

## 完了済み

- [x] Phase 1-3: Model 層 (Gate, Role, MergeMode, Readonly, payload 検証, gate 評価)
- [x] Phase 4: Broker Token Context, Role ベースポリシー
- [x] Phase 5: Inner Script リファクタ (Role 分岐, stdout キャプチャ)
- [x] Phase 6: Condition-based 状態マシン (sm.Advance)
- [x] Phase 7: AdvancedDispatcher (並列/逐次, hook→gate→advance)
- [x] Phase 8: WaitForJob/CompleteJob, JobHandler.Done シグナリング
- [x] DB スキーマ: hook_id → handler_id, role カラム追加
- [x] project.yaml: gate パース追加 (.boid/gates/)
- [x] Runner: ExecuteHook, ExecuteGate, WaitForJobCtx
- [x] CLI: --output-file フラグ, broker での --project 注入
- [x] API: AdvancedDispatcher ワイヤリング (ActionHandler, JobHandler, server.go)

## 残タスク

### ActionHandler.Apply の切り替え
- [ ] Apply 内で AdvancedDispatcher.DispatchAndAdvance を goroutine で起動
- [ ] 既存の Dispatcher.Dispatch を段階的に置き換え

### 自動遷移チェーン
- [ ] JobHandler.Done で AdvancedDispatcher の結果から再帰 dispatch
- [ ] MaxDepth でガード、エラー時のタスク aborted 処理

### Trait 再設計
- [ ] 現在の 5 trait を汎用的な少数 trait に再設計
- [ ] condition-based 遷移ルールを新 trait に合わせて定義
- [ ] OneShotMachine / FeedbackLoopMachine に condition ルール追加

### kit 統合
- [ ] ReadMetaWithKits で kit の gate をマージ
- [ ] kit.yaml での gate 定義パターン確立

### テスト
- [ ] E2E: readonly タスク → hook 並列 → gate 並列 → 自動遷移
- [ ] E2E: writable タスク → hook 逐次 → gate 並列 → 自動遷移 → 連鎖
- [ ] E2E: 手動 abort が任意の状態から動作
- [ ] E2E: feedback-loop の全サイクル
