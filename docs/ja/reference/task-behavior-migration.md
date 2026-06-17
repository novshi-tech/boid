# task_behaviors 移行ガイド: canonical name 廃止と free naming

> Track A2 (2026-06-17) で導入された変更の移行手順です。

## 概要

従来は `task_behaviors` のキー名として `supervisor` と `executor` の 2 つのみが
有効な canonical name として扱われていました。Track A2 からこの制限を撤廃し、
**任意の名前 (free naming)** が使えるようになりました。

あわせて:

- **`readonly` の既定値が `true`** (fail-safe) になりました。writable な behavior には
  `readonly: false` を明示してください。
- **`default_task_behavior`** トップレベルキーが追加され、`boid task create` で
  `--behavior` を省略したときに使う behavior を指定できます。

---

## `supervisor` / `executor` の deprecated 扱い

これらの名前は引き続き動作しますが、**deprecated** 扱いになりました。
`ReadProjectMetaWithKits` の呼び出し時（デーモン起動・プロジェクト再読み込み時）に
1 回だけ WARN ログが出ます。

警告を抑止するには `BOID_NO_DEPRECATION_WARN=1` を設定してください。

---

## 移行手順

### Before (旧 canonical name)

```yaml
# .boid/project.yaml
id: my-project
name: My Project
worktree: true

task_behaviors:
  supervisor:
    default_instruction:
      agent: claude-code
      message: |
        ...
  executor:
    default_instruction:
      agent: claude-code
      message: |
        ...
```

### After (free naming)

```yaml
# .boid/project.yaml
id: my-project
name: My Project
worktree: true

default_task_behavior: plan   # ← 新規追加: boid task create のデフォルト

task_behaviors:
  plan:                        # ← "supervisor" を任意の名前に改名
    readonly: true             # ← readonly を明示 (省略時も true が既定)
    default_instruction:
      agent: claude-code
      message: |
        ...
  dev:                         # ← "executor" を任意の名前に改名
    readonly: false            # ← writable な場合は明示が必須
    default_instruction:
      agent: claude-code
      message: |
        ...
```

---

## readonly の既定値変化と移行

| situation | 旧 (Phase 3-1) | 新 (Track A2) |
|---|---|---|
| `supervisor` (明示なし) | readonly = true (自動) | readonly = true (既定値と同じ) |
| `executor` (明示なし) | readonly = false (自動) | **readonly = false (互換 override, WARN あり)** |
| 非 canonical (明示なし) | readonly = false | **readonly = true (fail-safe)** |
| 任意の名前, `readonly: false` 明示 | — | readonly = false |
| 任意の名前, `readonly: true` 明示 | — | readonly = true |

### `executor` を使い続ける場合

`readonly: false` を明示することで互換 WARN を抑止できます:

```yaml
task_behaviors:
  executor:
    readonly: false   # ← これを追加するだけで WARN が消える
    default_instruction:
      ...
```

---

## `default_task_behavior` の設定

`boid task create` (CLI) や Web UI の新規タスク作成で behavior を省略したとき、
`default_task_behavior` で指定した behavior が使われます。

```yaml
default_task_behavior: plan
```

**未指定の場合の挙動:**

1. `task_behaviors` に `supervisor` があれば暗黙的にそれを使う（WARN あり）
2. `supervisor` もなければエラー (`boid task create` が失敗)

---

## よくある移行パターン

### 単純なリネーム + default 設定

```yaml
default_task_behavior: plan

task_behaviors:
  plan:          # 旧 supervisor
    readonly: true
    ...
  dev:           # 旧 executor
    readonly: false
    ...
```

### 複数のルートテンプレートを並べる

```yaml
default_task_behavior: dev

task_behaviors:
  plan:
    readonly: true
    default_instruction: { agent: claude-code, message: "Plan the work..." }
  dev:
    readonly: false
    default_instruction: { agent: claude-code, message: "Implement the feature..." }
  review:
    readonly: true
    default_instruction: { agent: claude-code, message: "Review the PR..." }
```

`boid task create` で `--behavior review` のように名前を指定すれば任意のテンプレートを使えます。
省略時は `default_task_behavior: dev` が適用されます。
