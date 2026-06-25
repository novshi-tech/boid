# 旧スキーマからの移行

## 廃止されたフィールド

`project.yaml` の以下のフィールドは新スキーマで廃止されました:

- top-level: `kits` / `env` / `host_commands` / `additional_bindings` / `secret_namespace` / `capabilities`
- behavior-level: `task_behaviors.<name>.kits`

これらは **workspace.yaml** (machine-local) または **legacy kit** に移行します。

## `boid project migrate <dir>` の使い方

```bash
# dry-run (何も書き換えない)
boid project migrate ~/src/myproject --workspace dev

# 実行
boid project migrate ~/src/myproject --workspace dev --apply

# secret collision がある場合の対応
boid project migrate ~/src/myproject --workspace dev --apply --on-collision skip
```

migrate コマンドは:

1. `project.yaml` の削除対象フィールドを検出する
2. kit は `~/.local/share/boid/kits/` へコピーし、workspace.yaml に kit ref を追記する
3. env / host_commands / additional_bindings を workspace.yaml へ移動する
4. `project.yaml` を新スキーマで書き直す (dry-run のときは変更しない)

## `project.local.yaml` の廃止

`project.local.yaml` も廃止されました。内容は `workspace.yaml` に集約されます。
`boid project migrate` が同時に吸い上げます。

旧 `project.local.yaml` が担っていた設定:

| 旧フィールド | 移行先 |
|---|---|
| `env` | `workspace.yaml` の `env` |
| `host_commands` | `workspace.yaml` の `host_commands` |
| `additional_bindings` | `workspace.yaml` の `additional_bindings` |
| `secret_namespace` | `workspace.yaml` の `secret_namespace` |

## オンボーディング 3 段について

初回セットアップは `boid init` (廃止) ではなく 3 段で行います。
詳細は `docs/ja/guide/onboarding.md` を参照してください。
