# State Machine

## Contents

- [Status Overview](#status-overview)
- [Unified Flow](#unified-flow)
- [Per-Status Guide](#per-status-guide)
- [Auto-Transition](#auto-transition)

## Status Overview

| status | 役割 | FS |
|--------|------|-----|
| executing | 指示に従い実装する | RW |

pending, done, aborted ではエージェントは起動されない。

## Unified Flow

すべてのタスクは単一の state machine で動作する。
状態は `pending → executing → done` の 3 つで、 失敗時のみ `aborted` で終端する。

```
pending → executing → done
              ↑
              │ reopen で done から戻れる
              │
done ─────────┘
```

## Per-Status Guide

### executing

instructions の指示に従って作業する。

- 指示に従って作業し正常終了 (exit 0) すれば、 hook trap が `boid job done` を発火して状態機械が進める
- 修正不可能なエラーに遭遇した場合は abort で打ち切る:
  `boid task abort <task_id> --code <reason> --message "<summary>"`

reopen で executing に戻された場合、 `Task.Instructions` 配列の最後の要素が新しい active 指示となる。 過去の指示は配列の前方に残るので、 文脈として参照できる。

## Auto-Transition

状態遷移はシステムが hook の終了イベントに基づいて自動判定する。
エージェントが明示的に遷移を指示する必要はない。

| 条件 | 遷移 |
|------|------|
| hook が exit 0 で終了 (`boid job done` 発火) | executing → done |
| `done` 入場直前に exit gate が exit 0 を返す | executing → done が確定 |
| `done` 入場直前に exit gate が exit 非 0 | 遷移ブロック (executing のまま残る) |
| 任意の状態で `boid task abort` | * → aborted |
