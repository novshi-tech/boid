# workspace DB 一元化 実装計画

ステータス: 計画 (planned)
作成日: 2026-07-16
更新日: 2026-07-16 (codex レビュー反映、blocker 3 件 + major 8 件を吸収)
親ドキュメント: [container-based-boid.md](container-based-boid.md) — 移行戦略 **Phase 2.5 (新設)**

---

## 目的

workspace 責務を machine-local な `~/.local/share/boid/workspaces/*.yaml` から DB に移し、
workspace 系操作を Web UI と CLI から一様な API 経由にする。同時に、
container backend 前提の設計 ((1) コンテナイメージ (2) host_command 参照 (3) env (4) allowed_domains) が
「環境依存要素ゼロ」であるという性質を活かして、**kit 機構を退役させ、
host_commands の実定義は broker 側 (最終的には broker コンテナイメージ) に集約する**。

この phase で同時に解決するもの:

- workspace 系操作のリモート実行 (Phase 3 CLI リモートの前提)
- Web UI での workspace 編集 (現状は yaml 直編集か CLI 経由のみ)
- workspace 定義の versioning (yaml export/import で git 追跡可能)
- kit 機構の複雑さ (kit registry / kit init / additional_bindings 合成 / kits[] merge) の除去
- workspace configure の LLM 対話フロー廃止 (プリセットイメージ選択に統合)

スコープ外:
- コンテナイメージ実利用 (Phase 6 container backend)
- host_command HTTP 経路化 (Phase 6 broker 別コンテナ化と一体で議論)
- $HOME workspace volume (Phase 4)

---

## 決定事項 (2026-07-16 nose)

1. **順序 A** で確定: workspace DB 一元化 → Phase 3 (CLI リモート)。
   Phase 3 の「二分類の残り」を先に消して、③ 実装時の分岐を減らす
2. **workspace は host_command の参照名リストのみ持つ**。フルセット定義
   (path / args_policy / allowed_flags / reject rules 等) は broker 側が保持。
   最終的には broker コンテナイメージに焼き込む — 移行期 (userns backend 並走中) は
   daemon プロセスが集約 config として保持
3. **workspace configure コマンドは廃止**。kit が提供している 2 つ (additional_bindings と
   host_commands) は、コンテナ移行後は image + workspace init script (Phase 4)
   + broker イメージが持つ host_command 事前既知集合 で置き換わる。
   したがって「ホスト環境のサブセットを workspace にコピーする」という
   configure/kit init の役割は消える。**移行期間 (Phase 2.5 完了 〜 Phase 4 / 6)
   は kit init を失うので新規 workspace 作成 UX は劣化するが、
   移行期のユーザ数は少ないためケア不要 (2026-07-16 nose 判断)**
4. **kit 機構を退役**。kit list / remove / init コマンドおよび
   `orchestrator.KitRegistry` / `orchestrator.MergeKitRuntime` 相当を撤去。
   **ただし `additional_bindings` は本 phase では撤去しない** — Phase 4
   ($HOME workspace volume 契約先行) で HOME 契約に置換されるまで、
   userns backend が現行の binding を必要とする。Phase 2.5 では
   `AdditionalBindings` を workspace の遺物フィールドとして保持し、
   dispatch 時の merge 経路も維持する
5. **yaml export/import** を CLI + API で提供 (bootstrap + git 追跡目的)
6. **web set-url / set-addr はローカル専用確定** (daemon 再起動が必要で、restart 自体
   ローカル専用のため、API 化してもリモートから完結できず片手落ち)
7. **container_image フィールドは schema に nullable で先置き**。Phase 6 まで
   dispatch 経路では無視、schema migration の再訪を避ける
8. **default workspace 概念を導入**。init 時に固定 slug `default` の workspace を
   1 件自動作成。project の workspace 未 assign 状態は「default に assign」で表現。
   workspace 削除時に assigned projects があれば default に再 assign する
9. **host_command 名前衝突は migration fail**。複数 kit yaml が同名 host_command を
   定義していた場合はエラーで daemon 起動を止める (手で kit yaml を整理してもらう)
10. **プリセットイメージ提供者は user のみ**。初期スコープでは boid 提供のプリセット
    ライブラリは持たない (user が `workspace create` / `workspace import` で入れる)
11. **migration race は考慮しない** (boid は個人利用前提、複数 daemon 同時 migration の
    シナリオは想定外)
12. **`WorkspaceMeta.Kits` フィールドの fallback は残さない**。PR8 で即削除、旧 yaml
    環境の互換は保証しない
13. **Web UI 編集フォームは本 phase のスコープ外**。API 追加のみに留め、UI は別 phase
14. **workspace edit CLI は `--from-file` のみ**。個別フィールドフラグは実装しない
    (yaml 編集 → import が正規経路)
15. **`boid host-commands list` CLI を追加** (`/api/host_commands` の対応コマンド)
16. **downgrade 方針**: 移行期間のユーザ数が少ない前提で **rollback は cutover (PR3) 前まで** で
    保証、それ以降 (PR3〜PR7) に落ちた場合は「旧 workspace yaml + kits ディレクトリを
    残しているので DB を潰して boid を旧バージョンに戻せば復旧可能」を運用契約とする。
    dual-write / 一世代 backward compat は行わない (親文書「各段階で戻せる」の趣旨は
    「一世代前の binary に戻せる」ではなく「PR ごとに動作確認して問題があれば
    直前 PR に revert できる」で解釈する)
17. **API は PUT + `If-Match` (ETag) で楽観排他制御**、PATCH は採用しない
18. **CLI コマンド分類は三値必須** (`remote` / `local` / `neutral`)、cobra Annotations
    未設定は build fail (fail-open にしない)

---

## Schema

### 現行 (Phase 2.5 開始前)

```sql
CREATE TABLE projects (
  id         TEXT PRIMARY KEY,
  work_dir   TEXT NOT NULL,
  created_at DATETIME NOT NULL DEFAULT (datetime('now')),
  updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE project_workspaces (
  project_id    TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
  workspace_id  TEXT NOT NULL       -- workspaces slug (現状 FK 無し)
);
CREATE INDEX idx_project_workspaces_workspace_id ON project_workspaces(workspace_id);
```

workspace の実体は `~/.config/boid/workspaces/<slug>.yaml` (`DefaultWorkspaceDir()`)、
DB 側は「project ↔ workspace slug」の紐付けのみ持つ。

### Phase 2.5 (新設)

```sql
CREATE TABLE workspaces (
  slug                TEXT PRIMARY KEY,
  container_image     TEXT,                              -- nullable, Phase 6 まで無視
  host_commands       TEXT NOT NULL DEFAULT '[]',        -- JSON: []string of names
  env                 TEXT NOT NULL DEFAULT '{}',        -- JSON: map[string]string
  allowed_domains     TEXT NOT NULL DEFAULT '[]',        -- JSON: []string
  extra_repos         TEXT NOT NULL DEFAULT '[]',        -- JSON: []string
  capabilities        TEXT NOT NULL DEFAULT '{}',        -- JSON: Capabilities struct
  additional_bindings TEXT NOT NULL DEFAULT '[]',        -- JSON: []BindMount, Phase 4 で退役
  created_at          DATETIME NOT NULL DEFAULT (datetime('now')),
  updated_at          DATETIME NOT NULL DEFAULT (datetime('now'))
);

-- 既存 project_workspaces に FK 制約を後付け (可能なら)
-- SQLite は既存テーブルへの ALTER TABLE ADD CONSTRAINT がないため、
-- migration で新テーブル作成 → データコピー → rename の手順
-- (or PRAGMA foreign_keys = OFF で妥協)
```

設計判断:

- **PK は `slug`** (既存 `project_workspaces.workspace_id` が slug 相当のため、
  新テーブル追加で自然に FK 参照できる)
- **JSON カラム採用**: 環境変数・許可ドメイン・host_command 参照は個数少なく、
  正規化 (別テーブル) するメリットが薄い
- **`kits` カラム無し**: 本 phase で workspace の責務から外す
- **`additional_bindings` は temporary column**: Phase 4 で退役予定だが、
  本 phase では userns backend が現行 binding を必要とするため保持。
  Phase 4 で dispatch 経路の binding merge が消えた時点で column も DROP

### Meta struct 側の対応

`internal/orchestrator/workspace_meta.go` の `WorkspaceMeta`:

- `Kits []string` を **削除** (migration 後は無効、PR8 で撤去)
- `HostCommands []string` を **追加** (参照名リスト)
- `ContainerImage string` を **追加** (nullable, Phase 6 まで dispatch は参照しない)
- `AdditionalBindings []BindMount` を **保持** (Phase 4 で削除予定の遺物フィールド)
- `Env` / `Capabilities` / `AllowedDomains` / `ExtraRepos` は現行維持

---

## host_commands 実定義の集約先

移行期は daemon プロセス側の config として保持:

- 保持形式: 現行 kit yaml の `HostCommands` map をそのまま集約 (フォーマット踏襲)
- 保持場所: **`~/.config/boid/host_commands.yaml` 一元 config** (2026-07-16 決定)
- migration 時に既存 kit yaml を scan して集約 config を組み立てる (下記 migration 節)
- **編集経路**: 手で `~/.config/boid/host_commands.yaml` を編集 → `boid host-commands reload`
  で daemon に読み直させる。専用の GUI / CLI 追加編集コマンドは持たない
  (移行期の短さと個人利用ドメインから、YAML 直編集で足りる)
- **名前衝突**: 決定 9 (migration fail) に従う。集約後の一元 config でも同名エントリは
  reload エラー。個人利用ドメインでは kit yaml 数が少なく、衝突は稀な想定

Phase 6 移行:

- broker container image に host_commands 実定義を焼き込む
- workspace の参照名 → broker container 内の実定義解決に切り替え
- CLI から見た workspace の参照リスト語彙は不変 (今 Phase で決めた語彙をそのまま使う)

### workspace が持つ参照リストの解決規則

- `workspace.host_commands: [gh, aws, tf, ...]` — 実定義集合の名前を列挙
- workspace create / edit 時に broker 側実定義集合と照合、未知は 400 エラー
- dispatch 時にも fallback チェック (集約 config が更新されて名前が消えたケース)

---

## API 追加

| Method | Path | 用途 |
|---|---|---|
| GET | `/api/workspaces` | list、response の各 workspace に `revision` (updated_at ベース or 整数) を含める |
| GET | `/api/workspaces/{slug}` | show (meta + assigned projects)、`ETag` レスポンスヘッダを付ける |
| POST | `/api/workspaces` | create、body は yaml (Content-Type: `application/yaml`)、`slug` 未存在必須 |
| PUT | `/api/workspaces/{slug}` | 全文置換、`If-Match: <revision>` 必須 (楽観排他制御) |
| DELETE | `/api/workspaces/{slug}` | remove、`default` は削除禁止、他は default に再 assign (下記) |
| GET | `/api/workspaces/{slug}/export` | yaml export (body そのまま `application/yaml`) |
| POST | `/api/workspaces/import?mode=<create-only\|replace>` | yaml import、mode で意味論明示 |
| PUT | `/api/projects/{id}/workspace` | 既存 (assign)、body: `{workspace_id: string}` |
| DELETE | `/api/projects/{id}/workspace` | 既存 (clear = default assign) |
| GET | `/api/host_commands` | broker 側実定義集合の名前一覧 (Web UI / CLI の validation 用) |
| POST | `/api/host_commands/reload` | 集約 config を再読込 (手編集後の反映) |

**設計判断**:

- **PATCH 不採用**: yaml 全文渡しで PATCH だと merge/置換の意味論が曖昧、
  複数 CLI/UI 同時編集で last-write-wins になる。全文置換の PUT + `If-Match` (ETag)
  で楽観排他制御を明示する
- **body 上限**: 1 MiB (workspace yaml は数 KB 想定、DoS 防御)
- **未知フィールド**: reject (`strict` パース)、typo で silently 落とさない

---

## コマンド影響

| コマンド | 分類 | 移行後 |
|---|---|---|
| `workspace list` | API 化 | `GET /api/workspaces` |
| `workspace show <slug>` | API 化 | `GET /api/workspaces/{slug}` |
| `workspace remove <slug>` | API 化 | `DELETE /api/workspaces/{slug}` |
| `workspace create <slug>` | 新設 | `POST /api/workspaces` |
| `workspace edit <slug> --from-file <yaml>` | 新設 | `PUT /api/workspaces/{slug}` (`If-Match` 自動、`--force` で無視。yaml body、個別フィールドフラグは非対応) |
| `host-commands list` | 新設 | `GET /api/host_commands` — broker 側実定義集合の名前一覧 |
| `host-commands reload` | 新設 | `POST /api/host_commands/reload` — 手編集後の反映 |
| `workspace export <slug>` | 新設 | `GET /api/workspaces/{slug}/export` |
| `workspace import` | 新設 | `POST /api/workspaces/import` |
| `workspace assign / clear` | 既存 API | 変更なし |
| `workspace configure` | **廃止** | プリセットイメージ選択で不要化 |
| `kit list / remove / init` | **廃止** | 機構ごと退役 |
| `web set-url / set-addr` | ローカル専用確定 | 変更なし |

---

## マイグレーション

daemon 起動時に自動 migration を実行:

### 入力パス

- workspace yaml: **`DefaultWorkspaceDir()`** = `$XDG_CONFIG_HOME/boid/workspaces/`
  もしくは `~/.config/boid/workspaces/` (現行 `internal/orchestrator/workspace_store.go` と一致)
- kit yaml: `~/.local/share/boid/kits/<name>/*.yaml`
- 集約 config 出力先: `~/.config/boid/host_commands.yaml`

**入力パスは `DefaultWorkspaceDir()` を唯一の解決元とする** (実装コードとの重複解決避け)。
`XDG_CONFIG_HOME` を変えた e2e シナリオも追加すること。

### 実行手順 (atomicity 確保)

`schema_migrations` テーブル (現行 `internal/db/migrate/`) に本 migration の
状態を `staging` / `committed` の二段で持ち、crash recovery を可能にする。

**PR3 前段の schema 拡張** (通常の migration ファイル経路で入れる、workspaces テーブルは PR1 で先置き済み):

- `schema_migrations` に `state TEXT NOT NULL DEFAULT 'committed'` カラム追加
  (既存行はデフォルトで committed 扱い、後方互換)
- `schema_migrations` に `input_hash TEXT NOT NULL DEFAULT ''` カラム追加

本 migration (`workspace_db_consolidation`) の実行手順:

1. `schema_migrations` に `workspace_db_consolidation` が `state=committed` で存在すれば skip
2. `BEGIN IMMEDIATE` で write lock 取得
3. **Preflight (副作用なし)**:
   - 全 workspace yaml を parse、error があれば abort
   - 全 kit yaml を parse、error があれば abort
   - kit の `HostCommands` 実定義を集約 map に build。**同名で異定義なら abort**
     (同名で同定義は dedupe、決定 9 参照)
   - project → workspace 参照が全て解決可能か確認、切れた参照は abort
4. `schema_migrations` に `workspace_db_consolidation` を `state=staging`, `input_hash=<sha256>`
   で upsert (staging の残骸を検出可能に)
5. **DB 更新** (同一 transaction 内):
   - 各 workspace を DB に write (Kits 展開込み)
   - `default` workspace の存在を保証 (無ければ空 workspace 作成)
   - `project_workspaces.workspace_id` の値が新 `workspaces.slug` に全て解決可能か再確認
6. **集約 config 出力**:
   - `~/.config/boid/host_commands.yaml.tmp` に書き込み → `fsync` → `rename` で
     atomic 化 (temp+rename パターン)
7. `schema_migrations` の `workspace_db_consolidation` を `state=committed` に UPDATE、
   transaction commit
8. 旧 workspace yaml / kits dir は削除しない (rollback 用に残す)

### crash recovery

再起動時:
- `workspace_db_consolidation` 行が `state=staging` で残っていれば
  → `input_hash` を再計算して DB 上の値と照合、
  一致なら roll-forward で再実行 (idempotent)、不一致なら abort して手動介入要
- config file の crash 時破損は temp+rename により回避 (中間状態が残らない)

### 名前衝突の扱い

kit A と kit B が同名の `host_command` を **異なる定義** で持つ場合、
preflight で abort → daemon 起動失敗。手で kit yaml を整理してもらう
(決定 9)。個人利用ドメインでは kit yaml 数が少なく、実際に起きるケースは稀な想定。

### e2e 前提

- migration on: 既存 workspace が全て正しく DB に載る
- migration on with kit conflict: 名前衝突で fail、`schema_migrations` の
  `workspace_db_consolidation` は `state=staging` を残さない
- migration on with corrupt yaml: parse error で fail、DB 変更ゼロ
- migration on empty environment: 初回インストール想定、`default` workspace のみ生成
- `XDG_CONFIG_HOME` を明示指定した shell から起動: 入力 dir が正しく解決される

---

## PR 分割案

各 PR で「read/write の権威が一つ」を守る (dual-write を避ける)。

| # | 内容 | 依存 |
|---|---|---|
| PR1 | **schema 追加のみ** (workspaces テーブル)、既存 code は yaml file の権威のまま。read/write は yaml、DB は空。deployable。`schema_migrations` 拡張 (state / input_hash) は PR3 で本格 migration atomicity と同時に入れる | — |
| PR2 | **host_commands 集約 preflight + config 生成** (kit yaml → `~/.config/boid/host_commands.yaml`)。集約 config を daemon が読む経路を追加、既存 kit yaml 経路も温存 (parity 検証段階) | PR1 |
| PR3 | **migration 本体** (daemon 起動時に workspace yaml + kit expand → workspace DB、`default` 保証、atomicity 手順)。migration 完了後は DB を workspace の権威に切り替え、yaml file は shadow (export 用) 降格。`WorkspaceStore` は DB backed に差し替え。`WorkspaceMeta.HostCommands []string` / `ContainerImage` 追加、`AdditionalBindings` は保持 | PR1-2 |
| PR4 | **workspace API 追加** (list/show/create/put/remove + `/api/host_commands` list/reload) + CLI 側 `list/show/remove/create/edit` を API 経由に差し替え。`default` 削除禁止 + 削除時 re-assign transaction 実装 | PR3 |
| PR5 | **yaml export/import** (API + CLI、mode=create-only\|replace) | PR4 |
| PR6 | **`workspace configure` / `kit init` / `kit list` / `kit remove` コマンド撤去**、`orchestrator.KitRegistry` / `MergeKitRuntime` の kit 集約経路撤去。`AdditionalBindings` merge は残す (Phase 4 まで userns backend が使う) | PR3-5 |
| PR7 | **`WorkspaceMeta.Kits` フィールド削除**、`Kits` を含む spec loader / dispatcher 経路のクリーンアップ (~500 行想定)。旧 workspace yaml / kits/ ディレクトリ削除 (次リリースで) | PR6 |
| PR8 (Phase 4 に持ち越し) | `AdditionalBindings` フィールド + `additional_bindings` カラム DROP、dispatch 経路の binding merge 撤去 | Phase 4 の HOME 契約先行 |

PR1-3 は「機構は変わるが挙動不変」、PR4-5 は「新機能追加」、PR6-7 は「退役 + 削除」。
PR3 の cutover 時点で権威が完全に DB 側に移る (dual-write 期間なし)。

---

## e2e 影響

- `workspace configure` を使うシナリオ → 撤去 or DB 直挿しに置換
- `kit init` を使うシナリオ → 撤去
- 新シナリオ: workspace API CRUD (list / show / create / edit / delete)、yaml export/import 往復
- 既存 workspace 経由 dispatch (`workspace assign` → task 実行) → 挙動不変を e2e で確認 (regression)

---

## default workspace の実装詳細

- **default 削除禁止**: DELETE handler で `slug == "default"` を明示的に拒否
  (400 or 403)。DB 側 CHECK 制約は入れない (migration 経路との干渉回避)
- **削除時の再 assign transaction**: 単一 SQLite transaction 内で
  `UPDATE project_workspaces SET workspace_id = 'default' WHERE workspace_id = <target>`
  → `DELETE FROM workspaces WHERE slug = <target>` の順に実行、
  途中エラーは rollback
- **空 default workspace の dispatch 挙動**: host_commands 空 + env 空 +
  allowed_domains は floor のみ、で dispatch が通ることを PR3 の e2e で
  必須確認 (現状 workspace 未 assign の project がある場合の挙動と同等)

## 未解決論点

(2026-07-16 の nose 判断で A-1〜A-6 / B-7〜B-10 + codex review 全解決。
決定事項節に反映済み。当面追加なし)

---

## Phase 5 との連続性メモ

Phase 5 で「タスクコンテキスト伝搬を boid コマンド (shim → broker RPC) に一本化」
することが container-based-boid.md で確定している。現行 `~/.boid/context/environment.yaml`
が説明する内容 = 「ネットワーク制限 + host command」であり、これは本 phase で
workspace が持つ設定 (`host_commands` 参照 + `allowed_domains` + `env`) と実質同一。

したがって `GET /api/workspaces/{slug}` の response 設計時に **「エージェント視点で
そのまま environment 情報として読める」形**を意識しておくと、Phase 5 で
`boid workspace env` 相当を実装する時に response をそのまま返せる (追加の
変換層が不要になる)。具体的な API response schema の詳細は Phase 5 で詰めるが、
本 phase の PR5 (workspace API 追加) 実装時にこの用途を想定した命名・階層にする。

---

## 親 phase との関係

`container-based-boid.md` の移行戦略に **Phase 2.5** として挿入:

- ① host command 契約 (完了)
- ② git gateway + branch policy (完了)
- **2.5 workspace DB 一元化 (本 phase)** ← workspace 責務再設計 + kit 退役
- ③ CLI リモート (2.5 前提で「二分類の残り」ゼロで進む)
- ④ $HOME volume (2.5 で kit additional_binding 退役済みの前提で軽くなる)
- ⑤ shim + context RPC
- ⑥ container backend + host_command HTTP transport + broker 別コンテナ

**Why:** container backend 前提で workspace 責務を洗い直すと環境依存要素がゼロで、
DB 一元化 + API 化が自然に成立する。同時に kit 機構 (host_commands / additional_bindings /
env / allowed_domains を workspace に注入する merge layer) の存在意義がほぼ消えるため、
「並走期間中に kit を残す」判断より「Phase 6 を待たず本 phase で退役」の方が
負債を早く畳める。

**How to apply:** この phase で確定した「workspace が持つのは image + host_command 参照 +
env + allowed_domains + extra_repos + capabilities のみ」という語彙を Phase 3 以降の
API / CLI / UI 設計の起点にする。kit 概念は本 phase 完了後は登場させない。
