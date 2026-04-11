# State Machine

## Contents

- [Status Overview](#status-overview)
- [Unified Flow](#unified-flow)
- [Per-Status Guide](#per-status-guide)
- [Auto-Transition](#auto-transition)

## Status Overview

| status | 役割 | FS | 出力 trait |
|--------|------|-----|-----------|
| executing | 指示に従い実装する | RW | artifact |
| verifying | 成果物を検証する | RO | verification |
| reworking | findings を修正する | RW | artifact |

pending, done, aborted ではエージェントは起動されない。

## Unified Flow

すべてのタスクは単一の state machine で動作する。
`transition:` 指定は廃止されており、遷移は payload の内容に基づいてシステムが自動判定する。

### 動作パターン

**単純タスク**（verify gate が全 findings を resolved にする場合）:

```
pending → executing → verifying → done
```

**CI 系**（pr-verify gate が executing / reworking に反応する場合）:

```
pending → executing → reworking → done
```

**verify 系**（verifying gate が unresolved findings を返す場合）:

```
pending → executing → verifying → reworking → verifying → done
                                      ↑              │
                                      └──────────────┘
                                    (unresolved findings)
```

## Per-Status Guide

### executing

instructions.yaml の指示に従って作業する。

作業完了時、artifact trait を出力する。

### verifying

payload.yaml の `artifact` を検証する。
instructions.yaml にレビュー観点が記載されている。

指摘事項を findings として verification trait に出力する。
問題がなければ `status: "resolved"`、問題があれば `status: "open"` とする。

プロジェクトディレクトリは読み取り専用。コードの変更はできない。

### reworking

executing と同じ権限（RW）で、修正作業を行う。
payload.yaml の `verification` に `status: "open"` の findings があるので確認し、対応する。

修正完了時、artifact trait を更新出力する。

## Auto-Transition

状態遷移はシステムが payload の内容に基づいて自動判定する。
エージェントが明示的に遷移を指示する必要はない。

| 条件 | 遷移 |
|------|------|
| artifact が non-null かつ executing 由来の unresolved findings なし | executing → verifying |
| artifact が non-null かつ executing 由来の unresolved findings あり | executing → reworking |
| verifying 由来の unresolved findings あり | verifying → reworking |
| verifying 由来の unresolved findings なし | verifying → done |
| 全 findings が resolved | reworking → done |
| unresolved findings あり | reworking → reworking（継続） |
