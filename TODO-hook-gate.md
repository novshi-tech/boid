# Hook/Gate 分離 — 残タスク

## 完了済み

- [x] Phase 1-8: 基盤実装（全 Phase）
- [x] DB スキーマ: hook_id → handler_id, role カラム追加
- [x] project.yaml: gate パース追加
- [x] Runner: ExecuteHook, ExecuteGate, WaitForJobCtx
- [x] CLI: --output-file, broker --project 注入
- [x] API: AdvancedDispatcher ワイヤリング
- [x] Trait リネーム: prompt, artifact, verification, tasks
- [x] Exclusive trait 衝突検知
- [x] Condition ヘルパー + 状態マシンルール
- [x] ActionHandler.Apply → AdvancedDispatcher + runDispatchLoop
- [x] 自動遷移チェーン（runDispatchLoop が管理）
- [x] kit gate マージ対応
- [x] tmux セッション管理（タスク単位 window、rework 再利用、cleanup）
- [x] レガシー Dispatcher 除去

## 残タスク

### テスト
- [ ] E2E: readonly タスク → hook 並列 → gate 並列 → 自動遷移
- [ ] E2E: writable タスク → hook 逐次 → gate 並列 → 自動遷移 → 連鎖
- [ ] E2E: 手動 abort + verification failed → executing rework
- [ ] E2E: feedback-loop の全サイクル

### .boid/project.yaml 更新
- [ ] agent_prompt → prompt にリネーム（読み取り専用のため手動対応）
