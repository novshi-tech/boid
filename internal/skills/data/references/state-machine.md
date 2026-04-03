# State Machine

## Contents

- [Status Overview](#status-overview)
- [Behaviors](#behaviors)
- [Per-Status Guide](#per-status-guide)
- [Auto-Transition](#auto-transition)

## Status Overview

| status | 役割 | FS | 出力 trait |
|--------|------|-----|-----------|
| executing | 指示に従い実装する | RW | artifact |
| verifying | 成果物を検証する | RO | verification |
| in_review | 最終レビュー | RO | verification |
| collecting_feedback | 指摘に基づき修正する | RW | artifact |

pending, done, aborted ではエージェントは起動されない。

## Behaviors

`task.yaml` の `behavior` で遷移パターンが決まる。

### one-shot

```
pending → executing → done
```

executing で artifact を出力すると自動的に done へ遷移する。

### feedback-loop

```
pending → executing → verifying → in_review → collecting_feedback → done
                ↑          │                          │
                └──────────┘                          │
              (unresolved findings)                   │
                ↑                                     │
                └─────────────────────────────────────┘
                          (unresolved findings)
```

verification の findings がすべて resolved なら前進、
unresolved があれば executing に戻り修正する。

## Per-Status Guide

### executing

instructions.json の指示に従って作業する。

**rework の場合**: payload.json に既存の `verification` がある。
`status: "open"` の findings が修正すべき指摘事項。対応すること。

作業完了時、artifact trait を出力する。

### verifying

payload.json の `artifact` を検証する。
instructions.json にレビュー観点が記載されている。

指摘事項を findings として verification trait に出力する。
問題がなければ `status: "resolved"`、問題があれば `status: "open"` とする。

プロジェクトディレクトリは読み取り専用。コードの変更はできない。

### in_review

verifying と同様の最終レビュー段階。

### collecting_feedback

executing と同じ権限（RW）で、フィードバック対応の修正作業を行う。
payload.json の `verification` から `source_state: "collecting_feedback"` の
findings を確認し、`status: "open"` の指摘に対応する。

修正完了時、artifact trait を更新出力する。

## Auto-Transition

状態遷移はシステムが payload の内容に基づいて自動判定する。
エージェントが明示的に遷移を指示する必要はない。

| 条件 | 遷移 |
|------|------|
| artifact が non-null | executing → verifying (feedback-loop) / done (one-shot) |
| 全 findings が resolved | verifying → in_review |
| unresolved findings あり | verifying → executing |
| 全 findings が resolved | collecting_feedback → done |
| unresolved findings あり | collecting_feedback → executing |
