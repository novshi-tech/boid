# 旧スキーマからの移行

## 廃止されたフィールド

`project.yaml` の以下のフィールドは新スキーマで廃止されました:

- top-level: `kits` / `env` / `host_commands` / `additional_bindings` / `secret_namespace` / `capabilities`
- behavior-level: `task_behaviors.<name>.kits`

これらは **workspace** (machine-local。 DB が権威、`~/.config/boid/workspaces/<slug>.yaml` は shadow) または、 移行の過程で生成される **legacy kit** に振り分けられます。 振り分け先の詳細は下記「`boid project migrate` の変換内容」を参照してください。

## `boid project migrate <dir>` の使い方

```bash
# dry-run (何も書き換えない)
boid project migrate ~/src/myproject --workspace dev

# 実行
boid project migrate ~/src/myproject --workspace dev --apply

# secret collision がある場合の対応
boid project migrate ~/src/myproject --workspace dev --apply --on-collision skip
```

### `boid project migrate` の変換内容

1. `project.yaml` の撤去対象フィールド (`kits` / `env` / `host_commands` / `additional_bindings` / `secret_namespace` / `capabilities`、 および behavior-level の `task_behaviors.<name>.kits`) を検出する
2. **(Phase 2.5 PR7 で変更)** 既存の `kits:` 参照 (`github.com/.../foo` のような ref) は、 名前検証 (`ValidKitName`) のみ行い、 migrate の dry-run/apply 出力に informational な note として表示される。 `WorkspaceMeta.Kits` フィールド自体が撤去されたため、 workspace へは一切引き継がれない — その kit が host_commands/env/additional_bindings を供給していた場合は、 移行後に手で workspace.yaml に追記すること
3. `host_commands` / `additional_bindings` のどちらかが非空なら、 その内容を同梱した **新規の legacy kit** を `~/.local/share/boid/kits/legacy-<slug>/kit.yaml` として生成する。 **(Phase 2.5 PR7 で変更)** この kit の host_commands 名リストと additional_bindings は、 kit 参照経由ではなく workspace の `host_commands:` / `additional_bindings:` に **直接** 追記される (project.yaml 自身のフィールドなので kit ディレクトリを介した再解決は不要)。 legacy kit の `host_commands` 定義自体は、 daemon 側の集約レジストリ `~/.config/boid/host_commands.yaml` にもマージされ (`workspace.host_commands` の名前参照が解決できるように)、 daemon に到達可能なら reload を指示する
4. `env` は workspace の `env` へ直接マージする (同一キーは新値、 つまり project.yaml 側が優先)
5. `capabilities.docker` は workspace の `capabilities.docker` へ直接マージする (project.yaml 側が設定していれば上書き)
6. `secret_namespace` が設定されていれば、 旧 namespace の secret を新 namespace (= workspace の slug そのもの) へコピーする。 **`secret_namespace` という別フィールドが workspace に生えるわけではない** — workspace は元々 slug 自体を secret のネームスペースとして使う設計であり、 移行が行うのは値のコピーだけ
7. `project.yaml` を新スキーマで書き直す (dry-run のときは何も書き換えない)

### workspace への反映 (daemon が動いている場合)

`--apply` は上記の変換結果をローカルの shadow yaml (`~/.config/boid/workspaces/<slug>.yaml`、 daemon が二度と読まない reviewable なアーティファクト) に書くだけでなく、 **動いている daemon の DB にも反映を試みます** (`pushMigratedWorkspaceToDaemon`):

- workspace slug が daemon にまだ無い場合: `POST /api/workspaces` で新規作成する
- 既存 slug の場合: 現在の内容を `GET /api/workspaces/<slug>` で取得し、 今回の migration が生成したフィールドとマージした上で `PUT /api/workspaces/<slug>` (`If-Match: <revision>`) で書き戻す (`mergeLegacyFieldsIntoWorkspace`)。 **マージの優先順位は「migration 側 (project.yaml から生成された値) が優先」** — `env` は同一キーなら migration 側の新値で上書き、 `capabilities.docker` は project.yaml 側が設定していれば上書きする。 legacy kit が生成された場合の `host_commands` (参照名) は union (既存の値を消さない)、 `additional_bindings` は Source が一致すれば migration 側が上書きする。 それ以外の既存フィールドはそのまま保持される
- `412 Precondition Failed` (revision 不一致 = 同時編集) を受けた場合は再取得してマージからやり直し、 最大 3 回リトライする
- daemon に到達できない場合、 または 3 回リトライしても解決しない場合は、 反映は shadow yaml にしか行われない。 コマンド出力に手動反映の手順 (`boid workspace import <file> --slug <slug>` または `boid workspace edit <slug> --from-file <file>`) が案内されるので、 その通りに実行すること — **`project.yaml` 自体の書き換えはこの反映結果とは無関係にすでに実行済み** であることに注意 (dry-run ではない限り)

## `project.local.yaml` の廃止

`project.local.yaml` も廃止されました。内容は workspace に集約されます。
`boid project migrate` が同時に吸い上げます。

旧 `project.local.yaml` が担っていた設定:

| 旧フィールド | 移行先 |
|---|---|
| `env` | workspace の `env` へ直接マージ |
| `host_commands` | workspace の `host_commands:` (参照名リスト) に直接追記 + daemon 側 `~/.config/boid/host_commands.yaml` に実定義をマージ (非空なら生成される legacy kit 経由、 Phase 2.5 PR7) |
| `additional_bindings` | workspace の `additional_bindings:` に直接追記 (Phase 2.5 PR7、 kit ディレクトリを介した再解決は不要) |
| `secret_namespace` | workspace に同名の別フィールドとして生えるのではなく、 **workspace の slug そのものが新しい secret namespace になる**。 移行が行うのは旧 namespace から新 namespace (= workspace slug) への secret 値コピーのみ |

## workspace DB 移行について (Phase 2.5、自動・手動操作不要)

`project.yaml` の schema 移行 (このページで説明している `boid project migrate`) とは別に、workspace の権威を yaml ファイルから DB (`workspaces` テーブル) に切り替える移行が Phase 2.5 (workspace DB 一元化) で入りました。こちらは **daemon 起動時に自動実行**され、手動操作は不要です:

- 既存の `~/.config/boid/workspaces/<slug>.yaml` を読み、`workspaces` テーブルへ一度だけ書き込む (`orchestrator.MigrateWorkspaceYAMLToDB`)
- 冪等 (2 回目以降は即 no-op) — `schema_migrations` テーブルに `workspace_db_consolidation` として記録される
- 途中で daemon が落ちた場合はクラッシュリカバリが働く (再起動時に同じ入力なら再開、入力が変わっていれば安全側で abort してエラーを出す)
- `default` workspace が存在しない場合はこの移行の中で自動生成される

移行後は `workspaces` テーブルが唯一の権威になり、`~/.config/boid/workspaces/*.yaml` は `boid workspace export` 用の shadow としてのみ残ります。詳細は `docs/plans/workspace-db-consolidation.md` を参照してください。

## kit 機構の退役について (Phase 2.5 PR6)

`boid kit init` (マシン単位の kit カタログ生成) と `boid workspace configure` (LLM 対話による workspace 設定生成)、および周辺コマンド (`boid kit list` / `boid kit remove`) は Phase 2.5 PR6 (2026-07) で撤去されました。

上の「使い方」節で説明した `boid project migrate` 自体の変換内容 (kit 生成・workspace.yaml への反映) は PR6 の影響を受けていません — 変わったのは生成された `kit.yaml` を後から**閲覧・削除する CLI が無くなった**点です。`~/.local/share/boid/kits/<name>/kit.yaml` は手で編集・削除してください。

workspace の中身を新規に用意する場合は、`boid workspace configure` の代わりに `boid workspace create` / `edit` / `import` (yaml 直接指定) を使います。詳細は [オンボーディング](../guide/onboarding.md) を参照してください。

## kit 機構の最終撤去 (Phase 2.5 PR7)

`WorkspaceMeta.Kits` フィールド (workspace.yaml の `kits:`) は Phase 2.5 PR7 (2026-07) でコードから完全撤去されました。影響:

- `POST` / `PUT` / `import /api/workspaces` に `kits:` キーを含む body を送ると 400 (`unknown field kits`) で reject される
- `boid project migrate` は legacy project.yaml の `kits:` 参照を名前検証・informational 表示のみ行い、 workspace への自動解決はしなくなった (上の「`boid project migrate` の変換内容」参照)。 migrate が生成する legacy kit 自体 (`host_commands` + `additional_bindings` を同梱) は変わらず生成され、 その内容は workspace の対応フィールドに直接追記される
- 唯一残る legacy `kits:` 対応経路は `boid workspace assign` の auto-create 補助 (手書き/e2e フィクスチャの workspace shadow yaml 向け) — クライアント側でインストール済み kit を解決してから (kits: を含まない) body を送信する
- **(訂正)** rollback 用に残置している shadow yaml (`~/.config/boid/workspaces/*.yaml`) や `~/.local/share/boid/kits/` ディレクトリは、 「DB 権威切り替え後は読まれない」わけではなく、 依然として次の 2 経路から読まれる依存が残っている — 「削除しても daemon の動作に影響はない」という案内は誤りなので撤回する:
  - shadow yaml: `boid workspace assign` の auto-create 経路 (直前の箇条書き参照) が、 assign 先の slug にまだ DB row が無い場合に `~/.config/boid/workspaces/<slug>.yaml` を読みに行く。 未 assign の workspace slug に対して今後も `assign` を使う可能性があるなら、 該当 slug の shadow yaml を消してはいけない
  - `~/.local/share/boid/kits/`: daemon 起動時のプリフライト (`internal/server/wire.go` の `buildProjectStore`) が、 `~/.config/boid/host_commands.yaml` が何らかの理由で失われていた場合の自己修復として、 このディレクトリ配下の kit.yaml から集約 host_commands を再構築する (`boid host-commands reload` 自体はこの再構築をしない — 既存ファイルの読み直しのみ)。 host_commands.yaml が失われる想定が無いなら影響は小さいが、 保証はできない
  - 削除してよいのは、 上記 2 条件 — 「今後 auto-create 経路 (未 assign slug への `workspace assign`) を使わない」かつ「host_commands.yaml が失われる想定が無い」— を両方確認できたときに限る

## オンボーディングについて

初回セットアップは `boid init` (廃止) ではなく、project 登録 + (任意) workspace 設定の 2 段で行います (`default` workspace で足りる場合は実質 1 段)。
詳細は `docs/ja/guide/onboarding.md` を参照してください。
