# 永続化レイヤ

`boid` の SQLite データベースに何が入っているか、どのテーブルがどの責務を持つかをまとめたページです。 [アーキテクチャ概要](overview.md) の補足にあたります。

主に DB に手を入れる contributor (新しい migration を書く、 schema 変更でインデックスを足す等) を想定しています。

## 概要

- 実装: [`modernc.org/sqlite`](https://gitlab.com/cznic/sqlite) (純 Go の SQLite 実装、 cgo 不要)
- データベースファイル: `~/.local/share/boid/boid.db` (XDG_DATA_HOME 配下)
- 開閉の責任: `internal/server.New` で開いて daemon の生存期間中保持。 daemon が終了すると閉じる
- 同時アクセス: `boid` daemon が排他的に持つ。複数 daemon は想定していない (UNIX socket 単一前提)

## テーブル一覧

`boid` は以下のテーブルを持ちます。スキーマ全文は [`internal/db/migrate/migrations/`](https://github.com/novshi-tech/boid/tree/main/internal/db/migrate/migrations) にあります。

| テーブル | 役割 | 1 行 = |
|---|---|---|
| `projects` | 登録済みプロジェクト | 1 プロジェクト |
| `project_workspaces` | プロジェクトとワークスペースの関連 | プロジェクト 1 件あたり 1 行 |
| `tasks` | タスクの本体 (最も大きいテーブル) | 1 タスク |
| `actions` | 状態遷移の監査ログ | 1 アクション |
| `jobs` | handler の実行記録 | handler 1 回の実行 |
| `worktrees` | タスク用に作成された git worktree | 1 worktree |
| `secrets` | 暗号化された secret 値 | 1 namespace × 1 key |
| `task_dependencies` | タスク間の依存エッジ | 1 依存関係 |
| `web_devices` | Web UI のペアリング済みデバイス | 1 デバイス |
| `web_pairing_codes` | 発行済みのペアリングコード | 1 コード |

## `tasks`

タスクの主テーブルです。 [概念](../guide/concepts.md#タスク-task) でいう task ドメインオブジェクトの永続化形です。

主要カラム:

| カラム | 型 | 役割 |
|---|---|---|
| `id` | TEXT PK | タスク ID (UUID) |
| `project_id` | TEXT FK → projects.id | 所属プロジェクト |
| `remote_id` | TEXT | 外部 issue tracker との対応 (任意) |
| `title` / `description` | TEXT | 表示用 |
| `status` | TEXT | `pending` / `executing` / `done` / `aborted` (旧 `verifying` / `reworking` は migration 0022 で `aborted` に強制遷移済み) |
| `behavior` | TEXT | このタスクの behavior 名 |
| `payload` | TEXT (JSON) | 現在の payload 全体 |
| `instructions` | TEXT (JSON) | Instruction の配列 (最後の要素が active、 reopen で append される) |
| `auto_start` | BOOLEAN | 作成時に自動 start するか |
| `traits` | TEXT (JSON 配列) | このタスクの behavior が宣言する trait |
| `readonly` / `worktree` | BOOLEAN | サンドボックスのモード |
| `branch_prefix` / `base_branch` | TEXT | worktree 設定 |
| `depends_on_payload` | TEXT | depends_on 先タスクの payload に関する条件 (JSON Path 風) |
| `ref` / `parent_id` | TEXT | 親タスク参照 (任意) |
| `created_at` / `updated_at` | DATETIME | 作成 / 更新時刻 |

**JSON カラムの役割**:

- `payload` — `artifact` などの trait を含む JSON ドキュメント。 trait の意味は [Payload trait リファレンス](../reference/traits.md)
- `instructions` — Instruction の配列。 配列の最後の要素が active、 reopen で append される
- `traits` — このタスクが扱う trait 名の配列 (behavior 由来)

`(parent_id)` には部分インデックス、 `(parent_id, ref)` にはユニークインデックスがあり、親子参照と ref 衝突を防いでいます。

## `actions`

タスクが受けたアクション (`start` / `done` / `abort` 等) と、それによる状態遷移の監査ログです。

| カラム | 型 | 役割 |
|---|---|---|
| `id` | TEXT PK | アクション ID |
| `task_id` | TEXT FK → tasks.id | 対象タスク |
| `type` | TEXT | アクション種別 |
| `payload` | TEXT (JSON) | アクションのパラメータ |
| `from_status` / `to_status` | TEXT | 遷移前後の status (タスク履歴を再構築する手がかり) |
| `created_at` | DATETIME | 発行時刻 |

`actions` は append-only で、上書きや削除を想定していません。 `boid task show` のタイムライン、 TUI / Web UI のヒストリ表示の元データです。

## `jobs`

handler (hook / gate) の 1 回の実行記録です。

| カラム | 型 | 役割 |
|---|---|---|
| `id` | TEXT PK | ジョブ ID |
| `task_id` | TEXT FK → tasks.id (NULLABLE) | 関連タスク。 `boid exec` 経由のスタンドアロン実行では NULL |
| `project_id` | TEXT FK → projects.id | プロジェクト |
| `handler_id` | TEXT | hook / gate の id |
| `role` | TEXT | `hook` または `gate` |
| `runtime_id` | TEXT | dispatcher が割り当てた runtime の ID |
| `interactive` / `tty` | INTEGER (bool) | PTY 接続の要否 |
| `status` | TEXT | `running` / `success` / `failed` |
| `exit_code` | INTEGER | プロセス終了コード |
| `output` | TEXT | stderr (ログ) 全文 |
| `execution_state` | TEXT | runtime 側の補助状態 |
| `created_at` / `updated_at` | DATETIME | 作成 / 更新時刻 |

`output` カラムには handler の stderr が丸ごと入ります (stdout は payload patch 解析後にこの内容に追加されない)。 大きなログを書く handler では DB サイズが膨らむため、 ストリーム制御は handler 側で行ってください。

`task_id` が NULLABLE なのは、 `boid exec` で hook/gate に紐付かないコマンドを動かしたときの記録もここに入るためです。

## `worktrees`

project トップで `worktree: true` を宣言したプロジェクトの executor タスクごとに作られる git worktree のメタです。

| カラム | 型 | 役割 |
|---|---|---|
| `id` | TEXT PK | worktree ID |
| `task_id` | TEXT FK → tasks.id (UNIQUE) | 1 タスク 1 worktree |
| `project_id` | TEXT FK → projects.id | プロジェクト |
| `path` | TEXT | host 上の worktree のパス |
| `branch` | TEXT | ブランチ名 |
| `base_branch` | TEXT | ベースブランチ |
| `created_at` / `cleaned_at` | DATETIME | 作成時刻 / 片付け時刻 |

`cleaned_at` が NULL = 現在 live な worktree。 削除済みでも行は残し、後追いの監査に使えるようにしています。

## `secrets`

API トークンなどを暗号化して保存します。鍵は `~/.local/share/boid/secret.key` にあり、 daemon が起動時に読み込みます。

| カラム | 型 | 役割 |
|---|---|---|
| `id` | TEXT PK | secret ID |
| `namespace` | TEXT | secret のネームスペース (project ごとに分離可能、既定は `default`) |
| `key` | TEXT | secret 名 |
| `value_encrypted` | BLOB | 暗号化済みの値 |
| `created_at` / `updated_at` | DATETIME | 作成 / 更新時刻 |

`(namespace, key)` でユニーク。 `boid secret set` / `boid secret get` 等のコマンドで操作します。

## `task_dependencies`

タスク間の依存エッジを表す多対多テーブルです。

| カラム | 型 | 役割 |
|---|---|---|
| `task_id` | TEXT FK → tasks.id | 待つ側のタスク |
| `depends_on` | TEXT FK → tasks.id | 待たれる側のタスク |

PK は `(task_id, depends_on)`。 `task.depends_on_payload` (JSON カラム) と組み合わせて 「先行タスクが done で、かつ payload にこの条件が立っているか」 を判定します。

## `web_devices` / `web_pairing_codes`

Web UI のデバイス認証用テーブルです (詳細は [Web UI](../guide/web-ui.md))。

```sql
web_devices(id, label, cookie_hash, created_at, last_seen_at, revoked_at)
web_pairing_codes(code_hash, label, created_at, expires_at, consumed_at)
```

cookie 値とペアリングコードはハッシュで保存し、 平文は DB に残りません。

## マイグレーション

`internal/db/migrate/` 以下に番号付きの SQL ファイルが並びます。

```
migrations/
├── 0001_initial.sql
├── 0002_add_jobs_handler_id.sql
├── ...
├── 0021_jobs_nullable_task_id.sql
└── 0022_drop_verifying_reworking.sql
```

特徴:

- マイグレーション履歴用のテーブル (`schema_migrations` のような) は **持っていません**。代わりに各マイグレーションの `skip` 関数が `columnExists` / `legacySchemaPresent` などでスキーマを直接確認し、すでに適用済みなら skip します
- 適用は daemon 起動時 (`server.New` → `migrate.Apply`)。各マイグレーションは個別の transaction で実行されます
- SQLite の制約上、 ALTER TABLE で削除できないカラムは `<table>_new` を作って INSERT、 旧テーブル DROP、 RENAME という手順を踏みます (例: `0005_add_secrets_namespace.sql`、 `0021_jobs_nullable_task_id.sql`)
- `ALTER TABLE ... DROP COLUMN` は SQLite 3.35+ でサポートされており、 純 Go SQLite の対応バージョンでは利用可能です (例: `0011_drop_tasks_start_gate.sql`)

新しいマイグレーションを足すときの慣例:

1. ファイル名は `NNNN_short_description.sql` (連番 4 桁)
2. 純粋な ADD なら `ALTER TABLE ... ADD COLUMN`、 削除や型変更が絡むなら `_new` テーブル経由で書き換え
3. `migrate.go` の `migrations` リストに登録し、 `skip` 関数で 「すでに適用済みか」 を判定する条件を書く
4. 既存環境の挙動を壊さないよう、デフォルト値は `NOT NULL DEFAULT ''` などで埋める

## 関連ドキュメント

- [アーキテクチャ概要](overview.md) — `internal/server` がどう DB を組み立てるか
- [Payload trait リファレンス](../reference/traits.md) — `tasks` テーブルの `payload` カラムに格納される trait の中身
- [`project.yaml` リファレンス](../reference/project-yaml.md) — `tasks` テーブルの `behavior` / `traits` カラムの出処
- [Concepts / Daemon](../guide/concepts.md#daemon) — daemon が DB を独占する理由
