# legacy 退役計画

2026-08-08 の棚卸し (grep 網羅探索 + 実ファイル確認) で見つかった「legacy / 後方互換 /
一回きり移行」コードの残存一覧と、退役の推奨順序。行番号は 2026-08-08 時点の
main (644e3c74)。着手時は必ず再検証すること。

方針の前例: `internal/api/workspace_homes.go:49-60` のコメントが「この repo には
API versioning も compatibility shim も無い」と明言しており、互換サーフェスは
早期退役がプロジェクトの既定方針。

## 退役順序 (依存関係順)

| # | 対象 | 難易度 | 前提条件 | 削減規模 |
|---|---|---|---|---|
| 1 | gateway.hosts 折り畳み + sandbox.backend KindOpaque | 小 | 手元 config.yaml の書き換え確認 | ~75 行 |
| 2 | `boid init` / `boid workspace import` 廃止スタブ | 小 | UX 判断のみ | ~120 行 |
| 3 | `commands:` WARN + task-row deprecated + JobTerminal 302 | 小 | なし (挙動不変) | ~100 行 |
| 4 | **project migrate 一式** (A-1/A-2/B-8/B-12) | 中 | 旧スキーマ project.yaml 保有インストールがゼロ | **~2,500 行 + kit resolver 配線** |
| 5 | **MigrateWorkspaceYAMLToDB 本体** (A-5/B-11/B-13) | 中 | schema_migrations に staging 行が無いこと | **~1,500 行** |
| 6 | `workspace import-home` 上流一式 (sentinel は残す) | 中 | pre-PR6 の host 側 homes ディレクトリがゼロ | ~1,500 行 |
| 7 | canonical behavior 名 (`supervisor`/`executor`) 互換 | 中 | 全 project.yaml の readonly 明示監査 | ~80 行 (**挙動変化あり**) |
| 8 | Instructions legacy map 形式 | 中 | **正規化 DB migration が先に必要** | ~40 行 |

最大のレバレッジは #4 と #5。合計 4,000 行超が落ち、refactoring-backlog.md L2
(orchestrator god パッケージ分割) に直接効く。

## 退役対象から明確に外すもの

- **`internal/dispatcher/workspace_home_migration_sentinel.go` の record 読み取り側**
  (`recoverInterruptedWorkspaceHomeMigration`、dispatch path から毎回呼ばれる):
  一回きりの移行装置ではなく「volume の中身が半端な状態で init.sh が走る」一般破損
  クラスの恒常ガード (sentinel.go:49-58、round 3 Blocker 1 として意図的に追加)。
  #6 で import-home CLI を退役しても、record を書く箇所が減るだけで読む側は残す。
- **`internal/api/web.go:237-245` の `parseTaskForm` urlencoded 分岐**: 「HTML form
  fallback で今も使われる」と記載。実利用の有無を確認するまで対象外。
- **B-10 `AssignDefaultWorkspaceToUnlinked`** (`internal/orchestrator/project_catalog.go:186-213`、
  `wire.go:240-251` から起動時実行): 冪等 SQL 1 本で実行コストほぼゼロ、
  「消さずに放置」の合理性が高い。消すなら DB migration での代替が本筋。

---

## 各項目の詳細

### #1a. `gateway.hosts` レガシー折り畳み

- 場所: `internal/config/config.go:413-421` (raw struct) / `:490-578` (折り畳みループ)、
  `internal/config/schema.go:138-156`
- 何の互換か: `gateway.forges` map 化以前の `gateway.hosts:` 配列スキーマ。built-in slot
  (github / bitbucket) と衝突時は legacy 側 secret_key を優先マージ。
- 導入: 2026-07-09 82a5da55 (git gateway PR4)。**「one release だけ残す」と明記済み**
  (config.go:415-418)、既に約 1 ヶ月経過。
- 依存者: コメントに固有名詞で「nose's actual config.yaml (hosts-only, GH_TOKEN /
  BB_TOKEN keys) must keep working for one release」(config.go:549-552)。
  実質、運用者本人の config.yaml のみ。
- 手順: 手元 config.yaml (compose なら boid_state volume 内) を
  `gateway.forges.<id>.*` 形式へ書き換え → 削除。**削除後は旧キーが
  `unknown config key` で daemon 起動失敗になるので書き換え確認が先**。

### #1b. `sandbox.backend` の KindOpaque

- 場所: `internal/config/schema.go:120-131`、`internal/config/config.go:472-480`
  (warn して捨てる)、`internal/daemon/log_level.go:17`
- 何の互換か: container が唯一の backend になり (volume-only-daemon PR-4) 無意味化した
  キー。値検証なしで受理し WARN。
- 依存者: 実コード読み取りゼロ。e2e run-container.sh の言及はコメント/文言のみ。
- 手順: #1a と同じ判断・同じ PR で。合計 15 行程度。

### #2. 廃止スタブ 2 件

- `cmd/init.go` (全 52 行): RunE がガイダンスを出して必ず失敗する `boid init` スタブ
  (Phase 2.5 PR6 でスタブ化、2026-08-02 に案内文更新)。
- `cmd/workspace.go:126-172, 198-200, 1112-1173`: `boid workspace import` 廃止スタブ +
  「パースを通すためだけ」の死にフラグ 3 本 (`--mode` / `--force` / `--slug`)。
- 判断軸: 「壊れた入り口をエラーで正しい入り口へ誘導する」UX パターンとして残す選択も
  あり得る。消すなら 2 件同時 + `docs/ja/reference/cli.md` 更新。

### #3. 挙動不変の warning / リダイレクト系

- **`commands:` キー WARN**: `internal/orchestrator/spec_loader.go:85, 158-181`。
  Phase 3-d (2026-06-19) で撤去済み入口。値は完全無視。`removedTopLevelKeys` へ移して
  hard error 化するか丸ごと削除するかの二択。
- **task-row deprecated フィールド** (`worktree` / `branch_prefix` / `base_branch`):
  `internal/api/task.go:19-56` (呼び出し 4 箇所) + `cmd/task.go:274-301`。
  2026-05-14 導入でリスト中最古クラス。struct にフィールドが無いので削除しても
  挙動不変、WARN が消えるだけ。注意: CLI 側 strip を消すと古い YAML が unknown field
  エラーになる可能性 — strict デコードかどうかを削除前に確認。
- **`/jobs/{id}/terminal` 302**: `internal/api/web.go:761-767`。2026-04-24 導入。
  依存者はブックマーク/外部リンクのみ。

### #4. project migrate 一式 (最大レバレッジその1)

一蓮托生の 4 点セット。まとめて 1 系列で退役する:

- **`cmd/project_migrate.go` (1,842 行)**: 旧 project.yaml → workspace.yaml + DB 変換。
  ファイル冒頭 doc comment (60-76 行) が自己否定済み — `--apply` は
  `--legacy-bare-metal` 明示が必須で、compose 構成では config/DB が boid_state volume
  内にあり **CLI から構造的に到達不能**。実用は dry-run 表示のみ。
- **`internal/orchestrator/spec_loader_legacy.go` (194 行)**: 全シンボルの利用者は
  project_migrate のみ (2026-08-08 grep 確認)。refactoring-backlog.md L2 手順 1
  (migrate 用サブパッケージへ移設) は、退役するならそもそも不要になる。
- **`project.local.yaml` 読み取り**: 現行 `ReadProjectMeta` は既に読まず、読むのは
  legacy loader のみ (`spec_loader_legacy.go:99-132`)。完全に運命共同体。
- **workspace assign の legacy kits 解決** (`cmd/workspace.go:600-800`
  `extractLegacyWorkspaceKitRefs` ほか): shadow yaml の `kits:` をクライアント側で
  解決する経路。これが消えるとサーバ側の kit resolver 保持
  (`internal/api/config.go:155` / `internal/server/server.go:584` /
  `internal/api/project_service.go:1090-1095`) も連鎖して落とせる。
  `docs/ja/reference/cli.md:223` の「残る 2 経路」のもう片方が project migrate。

退役時の付随作業:
- `internal/orchestrator/spec_loader.go:266-267` の removed-field 拒否ガイダンスが
  `boid project migrate --help` を名指し → 手作業手順 or
  `docs/ja/guide/migration.md` へのリンクに書き換え。
- `docs/ja/reference/cli.md:24, 88` / `docs/ja/guide/migration.md` の更新。
- 前提条件の確認方法: 旧スキーマ project.yaml を持つ実運用インストールが
  残っていないこと (現況、運用者は本人のみなので手元確認で足りる)。

### #5. MigrateWorkspaceYAMLToDB 本体 (最大レバレッジその2)

- 場所: `internal/orchestrator/workspace_migration.go` (1,181 行) 全体。
- 重要な発見: `internal/server/wire.go:101` は workspaceDir に空文字を渡し、preflight は
  `DefaultWorkspaceDir()` へフォールバック (workspace_migration.go:230-237)。新規
  インストールでは空ディレクトリ → 「0 workspace を移行して committed」を書くだけ。
  **2026-07-16 (Phase 2.5 PR3) 以前から継続しているインストールにしか意味がない**。
- 凍結ミラー構造体 `pr6WorkspaceMeta` (860-895) / `pr7WorkspaceMetaWithBindings`
  (986-1020) + hash 3 兄弟は、staging 中クラッシュ→バイナリ更新→再起動のためだけの
  crash-recovery 互換。実行されるのは「PR6/PR7 世代バイナリで migration 中にクラッシュ
  したまま再起動していないインストール」のみ。
- 連鎖して落とせるもの:
  - `ListProjectWorkspaceReferences` (`project_catalog.go:215-235`) — doc comment に
    「migration is long past になったら他の caller は不要」と自己申告済み。
  - `WorkspaceStore` の yaml モード (`workspace_store.go:15-38, 104-135`) — daemon 本体は
    必ず DB モード。2 モード分岐の削除で可読性の利得が大きい。
- 前提条件の確認方法: 全インストールの `schema_migrations` に
  `version='workspace_db_consolidation' AND state='staging'` の行が無いこと。
- 注意: 削除すると `schema_migrations` に committed 行自体が付かなくなるため、
  `wire.go:171-176` のリカバリガイダンス文言との整合を取ること。

### #6. `workspace import-home` 上流一式

- 場所: `cmd/workspace_import_home.go` (459 行) + `internal/dispatcher/workspace_home_import.go` +
  `internal/api/workspace_home_import.go` + `internal/dispatcher/workspace_home_tar.go`
- 何の互換か: pre-PR6 (volume 化前、2026-07-27 以前) のホスト側
  `~/.local/share/boid/homes/<slug>` を volume へ吸い上げる一回きり CLI。
- 連鎖して落とせるもの: `internal/humanize/humanize.go:73-78` `ApparentSize`
  (「import-home が使うから残している。使わないなら消せ」と明記、production caller ゼロ) /
  `internal/dispatcher/workspace_home.go:692` の legacy root 解決。
- **sentinel (`workspace_home_migration_sentinel.go`) 本体は残す** (冒頭の除外リスト参照)。
  退役されるのは record を「書く」CLI 側だけ。
- 合計 ~1,500 行。refactoring-backlog.md 追補 N2A (`ImportWorkspaceHome` 330 行の分割) は
  退役するなら不要になる。

### #7. canonical behavior 名 (`supervisor` / `executor`) 互換

- 場所: `internal/orchestrator/spec_loader.go:476, 482-511` /
  `behavior_resolve.go:31-37, 108, 173-186` / `spec_types.go:320-345, 417-421`
- 3 つの互換が絡む: (1) deprecation WARN、(2) **`executor` に明示 readonly が無い場合、
  fail-safe の true でなく歴史的 false を適用** (behavior_resolve.go:185)、
  (3) `default_task_behavior` 未設定時の `supervisor` フォールバック。
- **リスク**: (2) は削除で fail-open → fail-safe に反転する実挙動変更。消し忘れた
  project が readonly=true で書けなくなる。全 project.yaml の
  (a) 独自 behavior 名 + `default_task_behavior` 明示、(b) readonly 明示、の
  一斉監査が前提。**この repo 自身の `.boid/project.yaml` も監査対象**。
- 移行手順 doc: `docs/ja/reference/task-behavior-migration.md` (en 版も)。退役時に整理。

### #8. Instructions legacy map 形式

- 場所: `internal/orchestrator/spec_types.go:158-200` (`Instructions.UnmarshalJSON` が
  `{"main": {...}}` を受理) / `store.go:587-600` / `payload_merge.go:35`
- 何の互換か: verifying/reworking 状態撤去 (2026-05-01、リスト中最古) 時の
  **DB 永続化済み行**の形式互換。API wire 形式でもある。
- **他項目と決定的に違う点**: 依存者が既存 DB 行。退役には
  「map 形式の `tasks.instructions` 行を配列形式へ書き換える一回きり正規化 migration」
  を先に入れ、全インストールで committed になるのを待つ必要がある。
- コード削減は ~40 行と小さい。優先度最低で良い。

### その他の小物 (順序表外・ついで対応)

- **`HostCommandSpec.Stdin` deprecated** (`spec_types.go:74-77` /
  `host_commands_config.go:190`): `reject` 語彙導入 (2026-07-06) 前の `stdin: true`。
  「まだ parse される」と doc comment にあるが実効性の有無は未確認 — 削除前に実装確認。
- **`/api/proxy` back-compat サーフェス** (`server.go:190, 748` / `runner.go:147`):
  workspace 別 proxy listener 化以前の単一 port 露出。**消費側が grep で見つからず、
  誰が叩いているか不明。調査が先**。

## 進め方

- #1〜#3 は「挙動不変 or 運用者本人の確認のみ」で、それぞれ独立 PR 1 本サイズ。
  すきま時間で拾える。
- #4 と #5 は削減規模が大きく independent な系列。どちらも「削除のみ」の PR に分割し、
  /boid-review の wiring レンズを通す。sqlite 依存パッケージのため検証は CI 委譲
  (メモリ `sandbox-cannot-build-sqlite-packages`)。
- #7 は挙動変更を含むため、監査 → WARN を error 化 → 1 リリース置いて削除、の
  段階を踏む。
