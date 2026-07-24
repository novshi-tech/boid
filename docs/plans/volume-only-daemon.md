# volume-only daemon: Phase 6 の compose 部分の再設計

ステータス: **draft (2026-07-24 作成、実装未着手、 codex review round 1 + 2 反映済み)**。
親ドキュメント: [phase6-container-backend.md](phase6-container-backend.md) の compose deploy 部分
(§決定4「host BIND mounts で shared state」) を置換する。
[phase6-cutover-followups.md](phase6-cutover-followups.md) §①-④ は本 doc の実装 PR 群で吸収される
(dogfood/userns 撤去/host daemon 撤去/config option fold はここに統合)。
発端インシデント: `phase6-dogfood-incident-and-pivot` (memory) — 2026-07-24 の dogfood 初回起動で
bind mount 経路の 3 段の壁が顕在化、 nose 判断で pivot。

---

## 背景と pivot 経緯

Phase 6 (PR #816-#826) は container backend (docker/podman を job runtime にする経路) を実装完了させ、
`sandbox.backend: container` config で opt-in できる状態まで持ってきた。 §①-④ (docs/plans/phase6-cutover-followups.md)
は「dogfood → userns 撤去 → host daemon 撤去 → config option fold」という段階撤去計画を定義していた。

2026-07-24 の dogfood 初回セッションで、 `scripts/deploy-container.sh` の実挙動を通そうとしたところ
以下 3 段の壁を連続で踏んだ:

1. **`/var/run/docker.sock` 非存在** (podman-only host) — compose.yml が DooD 前提で `/var/run/docker.sock` を
   bind mount しようとして `statfs no such file` で失敗。
2. **podman rootless の subuid mapping** — container 内 uid 1000 (`boid` user) が host uid ~100999 に
   mapping されるため、 host 側 mode 0600 の bind mount (`config.yaml` / `secret.key` / `web_secret` /
   `tls/` / `homes/`) が **owner mismatch で permission denied**。 daemon が load config 時点で exit。
3. **container 内から host filesystem 見えず → auto-prune で DB 大量消失** — compose daemon 起動時に
   `internal/server/wire.go:240` の「project.yaml が読めない (== dir が missing) → DB row を hard delete」
   経路が走り、 host filesystem に checkout してあった 18 project + cascade で tasks 479 / jobs 816 が
   全て削除された (workspaces 5 件のみ project 独立で残った)。

1 と 2 は「podman を primary target にする」ための追加 fix で塞げる (実際に fix branch で container 起動まで
は通した)。 しかし 3 は **「host filesystem を daemon container に持ち込む」という architecture 自体が
生む問題**であり、 fix の積み重ねで塞ぐ性質のものではなかった。

nose 判断 (2026-07-24 session): 「シークレットとかをバインドするってのはホスト側に必要なファイルがあることが
前提だから、 この将来的な目標 (k8s 上でお客様環境で調査タスク) と衝突する。 boid のデータは実は揮発性が
ほとんど。 workspace 定義と project の workspace assign だけが本質的に永続で、 project は git remote URL
+ workspace 指定で復元可能」。

これを受けて、 **Phase 6 の compose deploy 部分を「host bind mount 依存」から「daemon-owned named
volume」に根本再設計する** 方針が確定した。 §決定4 (host BIND mounts で shared state) と、 それを前提と
する rollback 契約は撤回される。

---

## 目的と非目的

### 目的

- daemon container の永続 state を **named volume に 1 本化**、 host filesystem 依存を完全に除去する。
- 現行 `~/.local/share/boid/` / `~/.config/boid/` の bind mount を撤廃、 相当の内容は volume 内で
  生成 / 管理される。
- project 登録経路を **git remote URL からの bare clone** に変える (host 側の checkout dir を register
  する現行 UX は廃止)。
- secret material (`secret.key` / `web_secret` / daemon CA) を **起動時 volume 内で generate**
  (host 側の事前 provisioning 不要、 現行 `install_id` / `LoadOrCreateKey` の on-first-boot 挙動を
  daemon の全 secret に拡張)。
- workspace / project 定義を **export/import bundle** で扱う (Kubernetes-like envelope、 workspace 内に
  project を nested、 init.sh 等の独立 file も bundle に含める)。
- config.yaml の編集は **CLI (`boid config ...`) + Web UI (`/settings`)** 経由 (host `vim` は不要)。
- userns backend / host daemon 起動経路は **新方式 cutover と同時に廃止** (段階撤去でなく単発切替、 §決定4
  由来の rollback 契約は撤回されるため段階撤去のメリットが消える)。
- daemon の永続 state を「daemon-only の private volume」と「job container に見せる staging area」に
  **明示的に分離** する (§論点 b で詳細、 codex Blocker 1 対応)。
- k8s 移行時に refactor の破壊が最小になる **seam を volume-only 実装段階で作る**
  (§論点 i、 codex Major 6 対応)。
- 公開 API (CLI HTTPS 経路) の **TLS trust bootstrap** を明示的に契約化する (§論点 c 内の新章、
  codex round 2 Blocker 1 対応)。

### 非目的

- **k8s Helm chart 設計** — 本 doc は「compose deploy を host filesystem 非依存にする」までを扱う。
  k8s 用の Helm chart / operator は本 doc の作業が完了したあと (Phase 7 相当) で別途扱う。 ただし
  seam (§i) は本 doc で決める。
- **project schema の schema-breaking 変更** — 本 doc は **additive migration のみを許容** する
  (`projects` テーブルに `project_name` / `status` / `status_reason` / `revision` を ADD COLUMN、
  `project_workspaces` に unique constraint 追加、 詳細は §論点 a)。 既存 column の型変更・削除・
  意味論反転 (schema-breaking) は本 doc の scope 外。 これは round 1 の「schema 変更しない」記述からの
  修正 (codex round 2 Blocker 2 対応: `degraded` 状態や name-based identity を実装するためには
  additive migration が不可避)。
- **secret rotation の実装** — 本 doc は on-first-boot generate のみ。
  rotation は現行の `boid web pair/revoke` (device credential 対象) では代替できない別問題:
  - `secret.key`: SQLite 内 secret store 全件の decrypt/re-encrypt が必要 (単独削除は decrypt error になる)
  - `web_secret`: cookie 一斉 invalidation
  - CA: leaf 再発行、 旧 CA revoke/transition、 稼働 job との整合
  本 doc の scope 外で、 別 doc で扱う。 dogfood 期間中の緊急対処は **break-glass procedure** を §論点 d
  で定義 (codex round 2 Major 7 対応)。

---

## 全体像

```
[host user] --- CLI (HTTPS + Bearer token + pinned CA)
                       \
                        +---> [daemon container] --- (engine socket via DooD) ---> [job container 1..N]
                       /       - private volume (daemon-only):
[host user] --- Web UI            /home/boid/.local/share/boid/{boid.db,secret.key,web_secret,tls/,install_id}
                (HTTPS+WSS,       /home/boid/.config/boid/config.yaml
                 same CA)      - staging volume (job-visible via subpath):
                                  /home/boid/staging/{spec/<job>/,tls/<job>/,broker-tls/<job>/,checkouts/<job>/}
                               - repo cache volume (workspace scoped、 daemon-writable):
                                  /home/boid/repos/<workspace>/<project>.git   (bare、 mirror refspec)
                               - workspace $HOME volume (Phase 4、 workspace 単位、 job-writable):
                                  /home/boid/homes/<workspace>/
```

現行 (bind mount 経路) との差:

```
[削除]  ~/.local/share/boid/ を host bind mount
[削除]  ~/.config/boid/ を host bind mount
[削除]  host 側 project checkout dir を `boid project add <dir>` で register
[削除]  host daemon (userns backend) との socket 分離共存 / rollback 契約
[削除]  wire.go:226-249 の on-startup destructive auto-prune (Blocker 4 対応)

[新規]  daemon 内 private volume + staging volume + repo cache volume + workspace HOME volume の 4 分離
[新規]  boid project add <git-url> --workspace=<name>   ← 引数意味論が変わる
[新規]  boid workspace export/apply bundle (YAML + init.sh 等の独立 file 含む)
[新規]  boid config edit/get/set/apply     ← config.yaml を CLI 経由で編集
[新規]  CLI は既存 HTTPS + Bearer token (cli-remote-connection.md 決定 4/8) を default 経路化
[新規]  daemon 公開 API の TLS trust bootstrap (self-signed CA + fingerprint pin、 §c 新章)
[新規]  SecretProvider / RepositoryCache / JobMountDescriptor の 3 seam (§論点 i、 k8s 前準備)
[新規]  additive DB migration (project_name / status / status_reason / revision) — §a
```

---

## 論点 a: project モデル transition (dir → git URL)

### 現行

`boid project add <dir>` で host filesystem の既存 checkout を register する。
`projects.work_dir` に host absolute path を格納。 daemon 起動時に `project.yaml` を `work_dir/.boid/project.yaml`
から読む。 project.yaml 欠落時は `wire.go:226-249` で **hard delete** (今日の DB 全滅の直接原因)。

現行 schema (`internal/db/migrate/migrations/0001_initial.sql` + `0028_add_projects_upstream_url.sql`):

```sql
CREATE TABLE projects (
    id           TEXT PRIMARY KEY,
    work_dir     TEXT NOT NULL,
    created_at   DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at   DATETIME NOT NULL DEFAULT (datetime('now')),
    upstream_url TEXT                                          -- 0028 で追加
);
CREATE TABLE project_workspaces (
    project_id   TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL
);
```

`project_name` / `status` / unique constraint は 存在しない。

### 新方式

`boid project add <git-url> --workspace=<name> [--name=<project-name>]` で git remote URL を register する。
daemon は repo cache volume 内 `<repo_cache>/<workspace>/<project>.git` に **bare clone (mirror 形式)** を
行い、 project 定義は DB (下記 schema) を source of truth とする。

`project.yaml` は bare repo の HEAD (default branch) から `git show HEAD:.boid/project.yaml` で読む。

### 新 DB schema (additive migration、 Blocker 2 対応)

**round 2 で「schema 変更しない」を撤回**。 `degraded` 状態や name-based identity を実装するには
additive migration が不可避。 新規 migration ファイルで:

```sql
-- migration NNNN_projects_add_name_and_status.sql
ALTER TABLE projects ADD COLUMN project_name TEXT;             -- workspace 内 unique な人間可読 name
ALTER TABLE projects ADD COLUMN status TEXT NOT NULL DEFAULT 'ready';
                                                                  -- 'ready' | 'degraded' | 'apply-error'
ALTER TABLE projects ADD COLUMN status_reason TEXT NOT NULL DEFAULT '';
                                                                  -- degraded/apply-error 時の理由 (human-readable)
ALTER TABLE projects ADD COLUMN revision INTEGER NOT NULL DEFAULT 0;
                                                                  -- optimistic concurrency 用 counter

-- unique 制約 (composite): 既存 project も project_name が NULL のうちは skip される
CREATE UNIQUE INDEX IF NOT EXISTS uniq_projects_workspace_name
    ON project_workspaces(workspace_id, ???);   -- ← name は projects 側にあるので JOIN 経由でしか unique 検証できない
                                                    -- 実装は upsert 時に app-side で workspace scope check
```

**注記**: `project_name` は `projects` テーブル側、 `workspace_id` は `project_workspaces` 側にあるので、
純粋な DB-level composite unique index は張れない (JOIN 越しの unique は index 不能)。 app-side で
upsert 時に `SELECT COUNT(*) FROM projects p JOIN project_workspaces pw ON p.id=pw.project_id
WHERE pw.workspace_id=? AND p.project_name=?` で verify + transaction 内で insert する形にする
(現行 workspace 内 dispatch の atomic pattern と同じ思想)。

より clean にするなら **`project_name` を `project_workspaces` に移動する schema-breaking な再設計**が
理想だが、 それは本 doc の非目的 (additive migration のみ)。 v1 は上記 app-side check で妥協、 将来の
schema-breaking migration で clean 化する。

### `status` の値と遷移

| status | 意味 | 遷移 |
|---|---|---|
| `ready` | 正常運転 (bare repo 存在、 project.yaml valid) | 初期値、 fetch 成功 / apply 成功で戻る |
| `degraded` | bare repo missing / fetch failure / project.yaml parse error 等、 dispatch 不能 | on-startup validate 失敗 / fetch 失敗 / apply 失敗 で遷移 |
| `apply-error` | `workspace apply` 中の partial failure (DB upsert 成功 → repo clone 失敗 等) | apply 実行中失敗 で遷移 |

**dispatch は `ready` のみで許可**、 他 status は refuse (エラー内容は `status_reason` で API/CLI/UI で表示)。
`degraded`/`apply-error` からの復旧は `boid project fetch <id>` (再 fetch) or `boid workspace apply -f`
(再 apply) or 明示的 `boid project rm` で削除。

### auto-prune の撤去 (Blocker 4 対応、 継続)

`wire.go:226-249` の on-startup hard delete 経路は **撤去する**。 新方式では:

- DB row (`projects` テーブルの URL + workspace assignment) が **復元可能な source of truth**
- bare repo は **再 clone できる cache** (missing なら refetch)
- `project.yaml` 欠落 / parse error / auth failure / fetch failure と「DB row 削除」は **分離**

startup 挙動:
- bare repo が missing → **再 clone を試行**、 失敗なら `status='degraded'` にして起動継続 (dispatch は
  refused、 UI/CLI で表示)
- `project.yaml` の parse error → 同じく `status='degraded'` (dispatch 拒否、 修復は git push で fix
  or 明示的 `boid project rm`)

明示的削除入口は `boid project rm` / `boid workspace delete` の 2 経路のみ。 startup logic からの
destructive delete は絶対に発火しない (自動ではリソースを消さない invariant)。

### 事前 validation (add 時 vs dispatch 時)

`boid project add <git-url>` の同期処理:
1. URL syntax validation + slug/path traversal 検証 (`^[a-z0-9][a-z0-9._-]*$`)
2. workspace 存在確認
3. workspace 内 project name 一意性 verify (app-side check、 上記 §DB schema 参照)
4. URL canonicalize (`NormalizeOriginURL` 経由、 `git@host:owner/repo.git` ↔ `https://host/owner/repo.git`)
5. bare clone 実行 (mirror refspec、 §論点 b 参照)
6. `project.yaml` パース + schema validation
7. DB insert (以上すべて成功したときのみ status='ready' で commit)

**同期 validate**: nose 判断: 「add 時に fail-loud」が正しい (silent に broken project を DB に残さない)。
非同期にすると失敗の原因が add からズレて debug しにくくなる。

### CLI 契約変更

- `boid project add <dir>` → **removed** (dir 引数は受け付けない、 helpful error message で `<git-url>` 版を案内)
- `boid project add <git-url>` → **new** (workspace 指定必須、 project-name は URL から derive or `--name` で override)
- `boid project rm <id>` → 現行維持 + repo cache dir の削除 + 進行中 fetch/dispatch の中断
- `boid project list` → 現行維持、 出力は **URL 表示に切り替え** + status 表示 (`work_dir` の bare repo
  path は internal detail として非表示、 human-facing identity は URL に統一)
- `boid project fetch <id>` → **new** (bare repo の explicit refetch、 degraded から ready への復旧経路)
- `boid project status <id>` → **new** (status / status_reason の詳細表示)

### `work_dir` の意味論変更 (Major 5 対応、 codex round 2 で consumer audit 拡張)

round 2 の指摘: 「`work_dir` grep だけでは `.WorkDir` consumer を拾えない」。 実 grep 結果:

`.WorkDir` を参照している箇所 (`internal/` 全体):

| file | 用途 |
|---|---|
| `internal/api/project.go:37` | project list response の WorkDir field |
| `internal/api/project.go:73,78` | project create API の WorkDir 要求 |
| `internal/orchestrator/project_store.go:315,317` | on-startup Load、 auto-prune の起点 (§撤去対象) |
| `internal/sandbox/runner/runner.go:233,275` | runner Workspace field、 JobDone RPC |
| `internal/sandbox/realization/realization.go:194,229` | docker create の Workdir field (job container 内 cwd) |
| `internal/api/task_create.go:50,54,145,146,168` | branch classification (`ClassifyBaseBranch`)、 branch 展開 (`ExpandBaseBranch`) |
| `internal/api/task_notify.go:327,330` | `gitFetchOrigin` 呼び出し (`git fetch` in filesystem repo) |
| `internal/dispatcher/session_job.go:177` | session job 経路の cwd |
| `internal/dispatcher/workspace_parent_dir_test.go` | test |

filesystem repo 前提の consumer が多数。 意味論変更の migration 戦略:

1. **新型 `ProjectRef` 導入**: `internal/orchestrator/project_ref.go` に:
   ```go
   type ProjectRef struct {
       ID           string
       Name         string       // project_name
       WorkspaceID  string
       UpstreamURL  string
       CachePath    string       // bare repo path (internal)
   }
   ```
   `work_dir` の string marker は使わない (`git://<workspace>/<project>` みたいな stringly-typed marker は
   codex 指摘の通り type safety が無く誤用の温床)。

2. **`RepositoryCache` interface 導入** (§論点 b + i 参照):
   ```go
   type RepositoryCache interface {
       Materialize(ctx, ref *ProjectRef) (checkoutHint, error)  // job dispatch 用の checkout hint
       Fetch(ctx, ref *ProjectRef) error
       ReadFile(ctx, ref *ProjectRef, path string) ([]byte, error)  // project.yaml 等
       DefaultBranch(ctx, ref *ProjectRef) (string, error)
       RefLookup(ctx, ref *ProjectRef, ref string) (commitSHA string, err error)
       Lock(ctx, ref *ProjectRef, mode LockMode) (unlock func(), error)
       Remove(ctx, ref *ProjectRef) error
       List(ctx) ([]*ProjectRef, error)
   }
   ```

3. **consumer 書き換え** (segment 単位):
   - `runner.go` / `realization.go`: `ProjectRef` を受け取り、 dispatch 時に `RepositoryCache.Materialize`
     で checkout hint を得る (staging volume 内の subpath、 §論点 b 参照)
   - `task_create.go`: `ClassifyBaseBranch` / `ExpandBaseBranch` を `RepositoryCache.RefLookup` /
     `DefaultBranch` 経由に refactor (filesystem 直参照を排除)
   - `task_notify.go`: `gitFetchOrigin` は `RepositoryCache.Fetch(ref)` に置換
   - `session_job.go`: cwd を `Materialize` の checkout hint から取得
   - `project_store.go` の startup Load: `project.yaml` を `ReadFile` 経由で読む、 auto-prune 経路は
     `status='degraded'` へ遷移に置換
   - `api/project.go`: request/response schema を `ProjectRef` ベースに (WorkDir field は URL 表示に)

4. **DB 側の `work_dir` column**: **廃止せずに残す** (additive migration の invariant)、 ただし v1 以降は
   `upstream_url` が正となる。 `work_dir` は「legacy field、 URL の canonical form」を保持する形で
   backward compat。 v2 (schema-breaking migration) で drop する option。

5. **presentation の統一**: `boid project list` / API response は **URL + name + status** を返す
   (`work_dir` の内部値は internal detail、 出力しない)。

### migration path

現行の project は「host checkout を dir 登録」だが、 新方式では意味論が違うため **auto-migration しない**。
既存 project は `boid project rm` で削除し、 新方式で `boid project add <git-url>` から register し直す。
nose の指示: 「boid のデータは揮発性許容、 消失は困らない」 = migration 経路を作り込まない判断。

### 開発者ワークフローへの影響

現行の「host に checkout した local branch (未 push) で agent に作業させる」ユースケースは **失われる**。
nose 判断: 「boid の作業と host 側 checkout が衝突する問題の方が大きい」 = 意図的に廃止。
未 push branch で作業したい場合は、 開発者が事前に push してから boid に投げる形になる。

---

## 論点 b: daemon 管理 state と job container への配送 (Blocker 1, 2, 3 対応)

### 概要

daemon の永続 state は目的節の全体像図が示す通り 4 種類の volume に分離:

| volume | scope | 中身 | job container mount | k8s 対応 (§i 参照) |
|---|---|---|---|---|
| `boid_private` | daemon 専用、 job 見えない | `boid.db`, `secret.key`, `web_secret`, `tls/`, `install_id`, `config.yaml` | **不可** (絶対 mount しない) | PVC (RWO) + Secret |
| `boid_staging` | daemon-writable、 job-readable/writable (subpath 単位) | 各 job 用 spec/state/TLS material/checkout 一時領域 | 可 (job 単位で subpath mount) | PVC (RWX) or EmptyDir per-job |
| `boid_repos` | daemon-writable、 job-readable (subpath 単位、 read-only mount) | workspace/project ごとの bare repo (cache) | 可 (job dispatch 時に project 単位で subpath mount、 read-only) | PVC (RWO) + reference clone in job |
| `boid_homes_<workspace>` | workspace 単位、 job-writable | workspace HOME (Phase 4 の既存経路) | 可 (workspace 単位で丸ごと mount) | PVC (RWX or RWO) per-workspace |

### なぜ 4 分離か (Blocker 1 対応)

現行 container_backend の DooD 契約 (`container_backend.go:186-199, 1409-1433`):

> Launch is a DooD (docker-out-of-docker) backend: the container it creates is a SIBLING via the HOST's
> own docker daemon, not nested inside this daemon's own container, so a mount Source it hands the HOST's
> own docker daemon has to be a path the HOST filesystem actually has.

現行は `BOID_RUNTIME_DIR` を「source == target で host に bind」することでこれを回避してた。
volume-only 化するとその host-visible path が消えるので、 sibling job container に mount する
staging area を **daemon-owned named volume として、 job container に mount 可能な形で確保する**。

### staging volume の isolation primitive (Blocker 3 対応、 round 2 で明確化)

**採用: Docker `VolumeOptions.Subpath` (API v1.45+) + podman compat 確認 + fail-hard capability probe**。

round 1 の draft で「host mountpoint bind と Docker `VolumeOptions.Subpath` が同じ経路」と書いたのは
誤り。 これらは別方式:

- **host mountpoint bind**: `docker volume inspect boid_staging` の Mountpoint field を取得し、
  host filesystem 上の実 path を sibling container に bind (local volume driver の実装依存、 API v1.0 から
  ある)
- **Named volume + Subpath**: `docker create --mount type=volume,src=boid_staging,dst=/run/boid/spec,volume-subpath=spec/<jobID>`
  で subpath を Docker API に指定 (Docker API v1.45+ の機能、 Moby CHANGELOG:342 で追加)

**採用: 後者 (VolumeOptions.Subpath)** — 理由:

- host mountpoint bind は local driver 依存で、 CSI/other driver では動かない (k8s 移行時の壊れ源)
- Subpath は API 契約なので engine 差の隠蔽が clean
- daemon container 内で `Mountpoint` を取得する必要が無い (unprivileged で完結)

### capability probe と fail-hard 契約

対応 engine 確認:

- **Docker**: API v1.45+ (Docker Engine 27.0+、 2024-06 リリース) で正式サポート
- **Podman**: 実 compat API v1.45 相当を提供、 ただし挙動同一性は要 empirical 確認 (podman-4.9.3
  相当が dogfood target)

daemon 起動時に engine capability probe を実行:

```go
// 疑似コード
capabilities, err := engine.APIVersion(ctx)  // GET /_ping or /version
if err != nil {
    return fmt.Errorf("engine API version probe failed: %w", err)
}
if !supportsVolumeSubpath(capabilities) {
    return fmt.Errorf("engine does not support VolumeOptions.Subpath (API v1.45+); "+
                       "daemon-only volume backend cannot be used safely without subpath isolation. "+
                       "Aborting startup.")
}
```

probe 失敗 or 未対応 engine の場合は daemon 起動を **fail-hard** で拒否 (whole-volume mount に degrade
することは security boundary breakage につながるため許容しない)。

### 実装詳細

1. `boid_staging` を compose.yml で declare (named volume)、 daemon container に `/home/boid/staging`
   として mount (rw)
2. daemon 起動時に engine capability probe (上記)
3. daemon が job を dispatch する際:
   - `boid_staging/spec/<jobID>/runner-spec.json` に spec を書く (container 内 path)、 chmod 0644 で
     job から readable、 owner は uid 1000 (keep-id 不要な named volume なので identity mapping で自然に)
   - `boid_staging/tls/<jobID>/{cert,key,ca}.pem` に TLS material、 chmod 0600
   - `boid_staging/checkouts/<jobID>/<project>/` に per-job clone (§bare cache 経路参照)
   - sibling job container 生成時、 `--mount type=volume,src=boid_staging,dst=/run/boid/spec,volume-subpath=spec/<jobID>`
     で subpath mount
   - 同様に `tls/`、 `broker-tls/`、 `checkouts/` を必要 dst に mount
4. job 終了時 daemon が `boid_staging/spec/<jobID>/`, `.../tls/<jobID>/`, `.../broker-tls/<jobID>/`,
   `.../checkouts/<jobID>/` を削除

### staging volume の安全境界

- **DB / secret.key / CA private key / web_secret は絶対 staging に書かない** (`boid_private` volume 側で
  管理、 job container に mount しない、 invariant)
- job container 側から staging の他 jobID subpath への read はできない (`volume-subpath` は subpath より
  外に出られない、 これは docker/podman の semantics、 capability probe で担保)
- staging volume 内のファイル permission は daemon 側で厳密に (spec/state は 0644、 TLS material は 0600)
- volume ownership: daemon 起動時に `chown -R 1000:1000 /home/boid/staging` で volume mountpoint の
  ownership を確保 (`init:` container or entrypoint 前段で実行)

### repository (bare repo) の job への提供 (Blocker 2 対応、 Major 1 対応で refspec 契約明記)

draft round-1 の「bare repo → workspace_volume 内 worktree 直 mount」は **撤回する** (round 1 でも撤回済み)。
round 2 の指摘: bare cache の refspec 契約と `--dissociate` の trade-off がまだ未確定。

#### bare clone の refspec

**mirror clone を採用** (`git clone --mirror` or 等価な `git clone --bare` + 明示 refspec)。 理由:

- `git clone --bare` + `git fetch --all` だけでは refs の完全同期が保証されない (defaults は `refs/heads/*`
  のみで、 tags / notes / stash 等は落ちる可能性)
- mirror clone は `refs/*` を丸ごと mirror し、 upstream の symbolic HEAD も追従
- `git show HEAD:.boid/project.yaml` の HEAD は mirror clone の HEAD (= upstream の default branch) を
  常に指す (Major 1 の refspec 契約)

実装:
```bash
git clone --mirror <url> <cache_path>              # 初回
git -C <cache_path> remote update --prune           # 以降の refetch (mirror refspec が自動反映)
```

#### `--reference` vs `--dissociate` の trade-off (Major 1 対応)

draft round-1 で採用した `git clone --reference --dissociate` は codex 指摘の通り、 borrowed objects を
each job で repack/copy するため cache との objects DB 共有の利点をほぼ失う。

**採用: `--reference` を job lifetime 中は維持、 job 終了時に `git repack -ad` で独立化してから
staging を削除**:

```
# job 開始時
git clone --reference <bare_cache> <upstream_url> <checkouts/<jobID>/<project>/>
git -C <checkouts/<jobID>/<project>/> checkout <branch>

# job 実行中
# alternates を維持 (cache と objects DB 共有、 disk 節約)

# job 終了時 (cleanup)
git -C <checkouts/<jobID>/<project>/> repack -ad  # alternates 解除、 objects 独立化 (先に自身に repack)
rm -rf <checkouts/<jobID>/<project>/>              # 削除しても cache に影響ない
```

**注記**: job 終了時 cleanup 前に cache 側で `git gc --prune=now` が走ると job が壊れるので、 cache の
`git gc` は shared lock (§項目 c 参照) で job lifetime を wait する。

#### workspace shared lock (Major 5 対応、 続き)

bare repo に対して `flock` (advisory) を daemon 側で持つ:

- shared lock: fetch + job 用 clone (並行可能)
- exclusive lock: rm/mv/rename、 `git gc --prune=now`
- 実装: `RepositoryCache.Lock(ctx, ref, mode)` interface method

#### job dispatch フロー

1. daemon が bare repo (`boid_repos/<workspace>/<project>.git`) の shared lock を取得
2. daemon が `git -C <cache> remote update --prune` を実行 (fetch、 up-to-date 化、 auth は git gateway 経由)
3. staging volume 内 `boid_staging/checkouts/<jobID>/<project>/` を用意
4. daemon が job container 生成時、 entrypoint 前段で:
   `git clone --reference <bare_cache> <upstream_url> <checkouts/<jobID>/<project>/>` + `git checkout <branch>`
5. job container は `/workspace/<project-name>/` として checkout 済み worktree を見る (Phase 4 の
   $HOME workspace volume と同じ mount 経路)
6. job 終了時、 daemon が `git repack -ad` (alternates 解除) → `boid_staging/checkouts/<jobID>/` を削除
7. shared lock を release

**reopen 意味論**: 現行の「push 済みのみ保証」を維持 ([[container-based-boid-direction]] 参照)、 reopen 時は
`boid_staging/checkouts/<jobID>/` を消して再 clone する。

### project name / URL の validation (Major 5 対応、 継続)

- **project name slug**: `^[a-z0-9][a-z0-9._-]*$` (path traversal 排除、 大文字禁止、 dot-prefix 禁止)
- **workspace 内 project name 一意性**: app-side check + `revision` counter で optimistic concurrency
  (詳細は §a 「新 DB schema」参照)
- **URL canonicalization**: `NormalizeOriginURL` (現行 dispatcher 内) を通す。 例: `git@github.com:foo/bar.git`
  ↔ `https://github.com/foo/bar.git` は同一 canonical form に
- **workspace reassignment**: `boid project mv <id> --workspace=<new>` — bare repo は
  `boid_repos/<new>/<name>.git` に atomic rename、 進行中 fetch/dispatch はブロック (exclusive lock)
- **rename**: 同様に `boid project mv <id> --name=<new-name>` — bare repo dir を rename、 DB update
- **`boid project rm` の cleanup**: bare repo dir 削除 + staging 内 in-flight checkouts の cancel + DB row 削除

### repo cache の GC

- `boid project rm` で該当 bare repo dir を削除
- `workspace delete` で workspace 配下の repo dir を一括削除
- `git gc --prune=now` は daemon 側で日次 goroutine (exclusive lock で fetch/dispatch を待つ)
- disk 使用量の monitoring は `boid gc status` (新規) or Web UI で

### 未解決論点

- **branch policy** (`branch-policy-simplification.md`) との整合 — 現行は「project 単位 branch」で main/task
  branch 区別なし。 clone --reference 経由でも同じ policy が働くか (ローカル checkout は独立なので影響
  少ないはずだが要確認)
- **fetch depth**: 初回 clone を full にするか、 blob-filter partial clone (`--filter=blob:none`) で
  bandwidth 節約するか (別 evaluation、 mirror clone との互換性要確認)
- **staging volume の disk 圧迫**: per-job checkouts が積み上がる可能性 (job 終了時 delete するが failure
  path で delete 漏れると回収されない)、 startup reap (PR7 で入った経路) との統合
- **podman VolumeOptions.Subpath 対応の empirical 確認**: 現行 podman 4.9.3 で subpath mount が Docker API
  と同一 semantics かは実 test 必要 (fail-hard capability probe は静的に判定できないケースあり得る、
  実行時 test を dogfood 前提とする)

---

## 論点 c: CLI 到達経路 (既存 HTTPS + Bearer 契約を維持 + TLS trust bootstrap 追加) (Blocker 3, round 2 Blocker 1 対応)

### 契約の再確認

draft round-1 の「mTLS + pair 済み client cert + cli-profiles.yaml」は **完全に撤回**。 Phase 3 で既に
確立した契約 ([cli-remote-connection.md] 決定 4/8) を volume-only cutover でも維持する:

- **transport 判定**: URL scheme で `unix://` (local UNIX socket) / `https://` (TCP + TLS + Bearer)
- **auth**: `Authorization: Bearer <device-token>` (device token は `~/.config/boid/tokens/<profile>.json`
  に 0600 で保存)
- **profile 管理**: `~/.config/boid/config.yaml` の `profiles:` map
- **切替**: `BOID_PROFILE=<name>` env / `--profile <name>` flag / `default_profile`

### volume-only cutover 時の変更点

現行:
- **local 経路**: `unix:///run/user/<uid>/boid.sock` (host filesystem 上の socket、 CLI が直接 dial)
- **remote 経路**: `https://<host>:<port>` (Bearer)

新方式:
- **local 経路の unix socket は消える** (daemon socket は volume 内、 host からは見えない)
- **代わりに `https://localhost:<port>`** (daemon container が localhost に publish、 CLI は Bearer + pinned CA)
- **remote 経路**: 変更なし (現行と同じ HTTPS + Bearer)

つまり全 CLI が **HTTPS + Bearer 一択** になる (unix socket 経路自体が消滅)。

### TLS trust bootstrap (round 2 Blocker 1 対応、 新章)

**現状分析** (静的読解による):

- `internal/server/server.go:718,733`: 現行公開 API listener は `net.Listen` 後 `Serve` (plain HTTP)、
  TLS listener じゃない (「`s.tcpServer = &http.Server{Handler: tcpHandler}`」に `TLSConfig` 無し)
- `internal/client/client.go:200-220`: production HTTPS client (`newHTTPSClient`) は
  `httpClient.Transport: &bearerTransport{token: token, base: transport}` (base nil で `http.DefaultTransport`、
  つまり **system trust store 固定**)
- 現行の HTTPS 経路は「public CA 発行の cert が要る」 前提の設計、 self-signed cert の path が無い

**volume-only での要求**:

- daemon 公開 API を TLS 化 (Bearer alone は cleartext channel で盗聴されると token 漏洩)
- self-signed cert (daemon internal CA を再利用) + fingerprint pin で client 側 trust bootstrap

**設計決定**:

1. **公開 API 用 TLS listener と internal CA の共有**: daemon 起動時、 `internal/mtls/ca.go` の `LoadOrCreate`
   で作る CA (broker/gateway/dockerproxy と共有) を、 公開 API listener の leaf cert 発行にも使用。
   同一 CA を使うことで:
   - fingerprint pin は CA cert 単位 (leaf ではない、 leaf 再発行に強い)
   - internal mTLS (broker/gateway) と公開 API の trust anchor が統一される
2. **fingerprint 取得経路**: `boid ca fingerprint` CLI subcommand を新規追加。 出力は SHA256 hex 文字列。
   volume-only 環境では **`docker exec container_daemon_1 boid ca fingerprint`** で out-of-band 取得
   (container exec = 管理者、 認可の起点として自然、 decision 7 と同じ扱い)
3. **pin 保存先**: profile file に fingerprint field 追加。 現行 `~/.config/boid/tokens/<profile>.json` を
   `{"token": "...", "ca_fingerprint": "SHA256:..."}` に拡張 (backward compat: fingerprint 未設定は
   system trust store fallback、 warning ログ)
4. **pin 前 fail-closed**: `boid login <url>` は `--ca-fingerprint <SHA256:...>` 引数を **必須** に
   (未指定は error)、 TOFU (trust on first use) は disable。 operator は先に `docker exec` で
   fingerprint を取得してから `boid login` する運用:
   ```bash
   FP=$(docker exec container_daemon_1 boid ca fingerprint)
   boid login https://localhost:8443 --ca-fingerprint "$FP" --profile local
   # → pair code prompt → device token 交換 → profile + fingerprint 保存
   ```
5. **CA 再生成時**: 全 device 再 pair 要 (secret rotation と同じ扱い、 §論点 d の break-glass procedure に
   `boid ca fingerprint` を追加 → 全 client で `boid login` やり直し)

### 初回 pair の UX (Blocker 3 継続対応)

[cli-remote-connection.md] 決定 7 の既定 UX + fingerprint pin:

```bash
# 1. compose stack を上げる
./scripts/deploy-container.sh

# 2. daemon container 内で fingerprint と pair code を取得
FP=$(docker exec container_daemon_1 boid ca fingerprint)
docker exec container_daemon_1 boid web pair
# → 表示: pair code XXXXX-YYYYY (5 分有効)

# 3. host CLI で redeem
boid login https://localhost:8443 --ca-fingerprint "$FP" --profile local
# → pair code prompt → device token 交換 → ~/.config/boid/tokens/local.json 書き込み
#    (token + ca_fingerprint、 profile entry も config.yaml に追加)
```

この pair は「container exec できる人 = 管理者、 認可の起点として自然」(nose 決定)。

### bootstrap 経路の loopback exemption

現行 `api_middleware.go:31-35, 76-87` の loopback bootstrap exemption は「device が 0 件 + 真の loopback
(proxy 通ってない)」の間だけ Web UI を通す仕組み。 volume-only では:

- 初回起動 (device 0 件) の Web UI アクセスは loopback exemption で通る (Web UI で pair code 発行できる)
- CLI 側は初回 pair 前は使えない (Bearer + fingerprint 必須) → decision 7 の `docker exec` 経由になる

これは既存挙動の維持で、 draft round-1 の「loopback trust default」提案は撤回済み
(round 1 の codex 指摘: host 別 user から到達可能 / peer address が必ずしも loopback ではない / 恒久 trust は安全でない)。

### profile 名の derive (Minor 3 対応)

`login.go:377` の `deriveProfileNameFromURL`: URL host の first dot-separated label を lowercase で derive。
`localhost` (dot 無し) → whole host = `localhost` になる (`local` にはならない)。

正しい例:

```bash
# --profile 明示 (推奨)
boid login https://localhost:8443 --ca-fingerprint "$FP" --profile local

# --profile 省略時
boid login https://localhost:8443 --ca-fingerprint "$FP"
# → profile 名は "localhost" (URL host そのまま)、 tokens/localhost.json / profiles.localhost
```

### port 選定

現行 Web UI が `:8080` に listen (`web.default_addr`)。 CLI Bearer 経路も同じ port に相乗り (Bearer
middleware は既に共存実装済み)。 別 port には分けない (単一 port の方が operator にとってシンプル、
firewall 設定も 1 rule)。

**HTTPS 化**: 上記 TLS trust bootstrap 節参照。 compose daemon は self-signed cert (internal CA 発行) で
listen する形。

### `boid start` の意味論 (未解決論点、 §論点 e に集約)

unix socket が消えるので、 現行 `boid start` (host daemon 起動) は使えない。 CLI は `boid login` から
始まる契約になる。 `boid start` を「compose stack up」の wrapper にするか、 script のまま残すかは
§論点 e で詳細。

### auto-start trigger 経路の削除

現行 CLI が「socket が無ければ auto-start」する挙動があった ([[stale-boid-daemon-recurring]])。 volume-only
では daemon は compose 経由で外部管理、 CLI から start できない → auto-start 経路自体を削除
(fail-fast にする)。

### 未解決論点

- **TLS listener の HTTP/2**: 現行の tcpServer は HTTP/1.1、 TLS 化に伴う HTTP/2 有効化は要検討
  (別 PR で扱う可能性)
- **initwizard との合流点**: `internal/initwizard` は現行 project.yaml scaffold 用 (`wizard.go:27`)、
  connection wizard じゃない (Minor 1 対応で誤記述訂正)。 「initwizard の compose deploy 対応」は
  本 doc の scope 外、 別 doc で扱う

---

## 論点 d: secret ライフサイクル (on-first-boot generate + break-glass procedure)

### 対象

- `secret.key` (**SecretStore の AES-256 master key**、 SQLite `secrets` テーブルの AES-256 encrypted
  rows の decrypt/encrypt 用、 `secret_keyfile.go:9` の `LoadOrCreateKey` で既に load-or-create)
- `web_secret` (Web session cookie signing key、 現行既に load-or-create)
- daemon internal CA (`tls/ca.crt` + `tls/ca.key` の **2 file**、 `mtls/ca.go:65` の `LoadOrCreate` で
  既に load-or-create、 broker/gateway/dockerproxy/公開 API の TLS trust anchor)
- `install_id` (現行既に on-first-boot generate、 atomic write-temp+os.Link 経路 PR #822)

**codex round 1 の事実確認**: 上記全て既に「不在なら generate、 あれば load」の contract で実装済み。
私の draft round 1 で書いた「新規実装が必要」の記述は誤りで、 実際には既存経路が host bind mount で
読めないのが原因だった (mode 0600 + subuid mapping 問題)。 volume-only では:

- volume owner = container 内 boid user (uid 1000)、 keep-id や podman override 無しでも identity
  mapping (named volume は container filesystem 上、 subuid 問題無し)
- 既存 `LoadOrCreateKey` / 対応する load-or-create 関数群が **そのまま volume 内で動く** (改修不要)

### 変更点

新方式で必要な変更:
- volume path が volume 内 (`/home/boid/.local/share/boid/*`) になるだけ
- `install_id`、 `secret.key`、 `web_secret`、 daemon CA いずれも既存の load-or-create をそのまま使用
- 追加改修は不要 (**round 1 Major 3 対応**: 私の「新規実装コスト」記述は誤り)

### rotation の non-goal 化 (Major 3 対応、 継続)

secret rotation は本 doc の scope 外 (「非目的」節参照)。 現状 rotation 未提供の帰結:

- **`secret.key` の rotate は SecretStore 全件の decrypt/re-encrypt が必要** — 現行未実装、 単独削除は
  **不可** (下記 break-glass 参照、 round 2 Major 7 対応)
- **`web_secret` の rotate は cookie 一斉 invalidation** — 現行未実装
- **CA の rotate は leaf 再発行 + 旧 CA revoke/transition + 稼働 job との整合** — 現行未実装

**重要**: `boid web pair/revoke` は **device credential (bearer token)** を扱う経路で、 上記 secret material
の rotation とは別問題。 device revoke で secret rotate はできない (round 1 Major 3 対応の指摘)。

rotation が必要になった段階で別 doc で扱う。

### break-glass procedure (round 2 Major 7 対応、 新節)

**問題**: `secret.key` を単独削除しても SQLite の `secrets` テーブルの encrypted rows は残り、 新 key では
decrypt error になる。 `web_secret` 単独削除は cookie を無効化するが Bearer device token row (別 table)
は失効しない。 CA 単独削除も token revoke と同義でない (Bearer token は CA と独立)。

したがって「無効な secret material を消せば fresh regenerate できる」というのは **事実誤り** (round 2
Major 7 指摘)。

**dogfood 期間中の緊急対処用 break-glass procedure** (rotate ではない、 完全リセット):

```bash
# 1. daemon 停止
docker compose -f build/container/compose.yml down

# 2. DB backup (crash 時 restore 用)
docker run --rm -v boid_private:/data alpine cp /data/boid.db /data/boid.db.backup-$(date +%Y%m%d-%H%M%S)

# 3. DB の secrets テーブルを purge (encrypted rows を削除、 sqlite3 CLI で)
docker run --rm -it -v boid_private:/data alpine sh -c 'apk add sqlite && sqlite3 /data/boid.db "DELETE FROM secrets;"'

# 4. web_devices テーブルも purge (device pair を無効化、 fresh pair 必須にするため)
docker run --rm -it -v boid_private:/data alpine sh -c 'apk add sqlite && sqlite3 /data/boid.db "DELETE FROM web_devices;"'

# 5. secret material file を削除 (daemon が起動時に fresh generate する)
docker run --rm -v boid_private:/data alpine sh -c 'rm -f /data/secret.key /data/web_secret /data/tls/ca.crt /data/tls/ca.key'

# 6. daemon 再起動 (secret material fresh generate)
./scripts/deploy-container.sh

# 7. Web UI で pair code 発行 → CLI で `boid ca fingerprint` → `boid login` で再 pair
FP=$(docker exec container_daemon_1 boid ca fingerprint)
docker exec container_daemon_1 boid web pair
boid login https://localhost:8443 --ca-fingerprint "$FP" --profile local

# 8. 上位 SecretStore 消費者 (workspace secrets 等) の re-provision
# → 各 workspace の gateway.forges.<forge>.secret_key で参照している env var の値を再登録
```

**副作用**:
- SecretStore に保存してた API token / OAuth refresh token 等は全て消失 (fresh から入れ直し要)
- 全 device pair 失効、 全 CLI/Web で再 pair 要
- 進行中の job があれば grand-slam abort (CA 変更で内部 mTLS が切れる、 dogfood 期間の緊急 rescue なので
  in-flight job 消失は許容)

### migration

現行 host daemon で generate 済みの secret material は **volume-only cutover 時に破棄** (新規 generate)。 副作用:

- **device pair 全失効の原因**: **fresh DB により device rows が無くなるため** (Minor 4 対応で因果分離)。
  Web UI + CLI いずれも再 pair 要
- **既存 session cookie 全部無効の原因**: `web_secret` 消失により cookie signature verify fail
- **SecretStore 内 secret 全部破棄の原因**: `secret.key` 消失 → 新 key では encrypted rows decrypt 不可
- **内部 mTLS cert 全部再発行の原因**: CA 消失 → 新 CA で fresh cert 発行 (稼働 job 無いので実害無し、
  fresh install 相当)
- **git-gateway cert scoping / reap label filter の変化**: `install_id` 変わる → 新 install_id 基準で
  動き出す

nose 判断: 「boid のデータはクリティカルでない」 = 上記副作用は許容範囲。 開発者は初回 pair からやり直し。

### 未解決論点

- **k8s 移行時の secret 供給経路**: on-first-boot generate は開発環境で便利、 k8s では **initContainer で
  Kubernetes Secret から volume に copy** する経路も欲しい (§論点 i 参照)。 現在の「file が既にあれば読む、
  無ければ generate」 の contract は両経路をカバー可能 (initContainer が file 書けば load される)
- **atomic write-safety**: `install_id` は write-temp+os.Link で atomic 化済み (PR #822)。 `secret.key` /
  `web_secret` / CA (cert + key の 2 file 同時) は同様の atomicity が必要 (race での partial write を防ぐ)。
  実装 PR で確認 (Major 6 対応: `SecretProvider` interface の multi-file transaction 要件も含む)

---

## 論点 e: 現行 host daemon 経路 (userns backend) の廃止 (Major 1, 2 対応、 PR 分割 additive/inert 化)

### 現状

- `cmd/start.go` の `runDaemonParent` (bare `boid start` の double-fork 経路) が host daemon 起動
- `internal/dispatcher/userns_backend.go` (userns backend) が sandbox 実行
- `internal/sandbox/runner/runner_linux.go` の syscall 経路
- `sandbox.backend` config option (`userns` | `container`)

これらは phase6-cutover-followups.md §②-④ で「dogfood 安定後に段階撤去」する予定だった。

### 新方式での扱い

**volume-only cutover と同時に一気撤去**。 段階撤去のメリットは「dogfood 期間中の rollback 契約」だが、
volume-only では host daemon への rollback 経路自体が成立しない (data の bind mount 契約が撤回されるため)。

### PR 分割案 (round 2 Major 2 対応: intermediate main を安全に保つ再々設計)

round 1 で提案した PR-1〜PR-6 は「staging consumer が PR-3 以降で unused」「URL marker と auto-prune の
window が PR-4〜PR-5 で split-brain」「unix fallback split-brain が PR-6 まで残る」問題があった。 修正:

- **PR-1: seam interface の導入 (additive、 unused 状態でも harmless)**
  - `RepositoryCache` / `SecretProvider` / `JobMountDescriptor` の interface 定義 (§論点 i 参照)
  - `VolumeSecretProvider` (現行 `LoadOrCreateKey` の thin wrapper) 実装
  - 既存 code は変更なし、 wrapper 経由に refactor するだけ
  - 追加 interface が implementer/consumer 揃った状態で landed (unused でない)
- **PR-2: volume-only compose stack + staging consumer 同時導入 (feature-gated)**
  - `compose.volume-only.yml` を新規追加 (現行 `compose.yml` は残す)
  - `deploy-container.sh` に `--mode=volume-only` flag 追加 (default = 現行の bind mount モード)
  - `container_backend.go` に **staging volume 経由の mount 経路実装** (VolumeOptions.Subpath 使用、
    engine capability probe、 fail-hard)
  - engine=volume-only mode の e2e (`e2e-container-volume-only` job) を CI に追加、 `continue-on-error: true`
    で advisory
  - 現行動作維持 (default mode は bind mount)
- **PR-3: URL-aware startup + auto-prune 撤去 + additive schema migration**
  - `wire.go:226-249` の hard delete 経路を撤去、 `status='degraded'` 遷移に置換
  - additive migration (`projects.project_name` + `projects.status` + etc.) 実装
  - `RepositoryCache` の consumer (project_store の startup Load 等) 書き換え
  - `boid project add <git-url>` の新経路 追加、 現行 `boid project add <dir>` は deprecation warning 出しつつ動く
  - 現行動作維持 (URL 経路は opt-in、 auto-prune 撤去だけ default 挙動変化)
- **PR-4: CLI Bearer 経路 + TLS bootstrap + config CLI/Web UI**
  - `boid login` の TLS fingerprint pin 対応 (`--ca-fingerprint`)
  - `boid ca fingerprint` CLI 追加
  - daemon 公開 API listener を TLS 化 (`internal/mtls/ca.go` の CA 共有)
  - `boid config edit/get/set/apply` 実装
  - Web UI `/settings` page
  - reload API 実装 (下記 §f の PR-3.a〜PR-3.d は本 PR 群 = PR-4 の中で扱う)
  - 現行動作維持 (default_profile 未 seed 時は unix socket fallback 継続)
- **PR-5: workspace export/import + repo cache 実装** (feature-gated)
  - `boid workspace export/apply` 実装 + init.sh 等の bundle 対応 (§g 参照)
  - `RepositoryCache` の mirror clone 経路実装 (`git clone --mirror` + `--reference` + `--dissociate` 変更)
  - 現行動作維持
- **PR-6: cutover (default 切替 + unix fallback + host daemon 経路 + userns backend 撤去、 同時)**
  - `deploy-container.sh` の default を volume-only に切替
  - unix socket fallback を CLI から撤去 (auto-start trigger 経路も同時撤去)
  - `cmd/start.go` の `runDaemonParent` 削除、 `boid start` は「compose stack 起動」の thin wrapper に
  - `internal/dispatcher/userns_backend.go` + `LocalRuntime` + `SandboxPreparer` 削除
  - `internal/sandbox/runner/runner_linux.go` + `internal/sandbox/plan.go` 削除
  - `sandbox.backend` config option を撤去 (container 一択)
  - `boid project add <dir>` 経路を完全削除 (deprecation の履行)
  - userns 固有 e2e scenario 削除
  - CI の `e2e-container-volume-only` を `continue-on-error: false` に格上げ

### 各 PR landed 時点の main の可動性 (Major 2 対応、 split-brain 解消)

- **PR-1 landed**: 現行動作不変、 seam interface が追加されて implementer/consumer が同時に整うので unused
  でない (VolumeSecretProvider は既存 `LoadOrCreateKey` の wrapper で稼働、 他 interface も default 実装が
  現行経路の wrapper)
- **PR-2 landed**: 現行動作維持 + volume-only mode を明示的に選ぶと動く (staging consumer が同時実装、
  advisory CI で挙動 pin)
- **PR-3 landed**: **auto-prune 撤去が default で発火** (URL 経路は opt-in だが、 auto-prune 撤去は全 mode で
  適用、 destructive behavior の削減が volume-only cutover 前に実現)、 URL marker と旧 filesystem path
  loader の窓が発生しない (PR-3 で startup loader を `RepositoryCache` 経由に書き換えるため)
- **PR-4 landed**: 現行動作維持 + Bearer 経路の TLS bootstrap も動く (`--profile local` opt-in)
- **PR-5 landed**: 現行動作維持 + workspace export/import bundle 使える
- **PR-6 landed**: default が volume-only に切替、 unix fallback + userns + host daemon 経路 一斉撤去、
  split-brain window 消滅

### 未解決論点

- **既存 userns 経路の e2e coverage** の container 経路移植完了状況の再確認 — attach ストリーム /
  resize 3 経路 / agent-stop signal / reap-before-reopen の container backend 版が揃っているか
- **`boid start` の意味論**: PR-6 で削除するとき CLI wrap にするか script のまま残すか

---

## 論点 f: config.yaml 編集経路 (CLI + Web UI) (Major 2, round 2 Major 3 対応)

### CLI API

```bash
# 全体を dump / apply
boid config get                             # 全 config を YAML で stdout
boid config apply -f config.yaml            # file から apply (validation + reload)

# key-level
boid config get sandbox.allowed_domains     # dotted path で individual key
boid config set sandbox.allowed_domains ".freee.co.jp" ".notion.com"
boid config unset gateway.forges.github.secret_key

# EDITOR 経由
boid config edit                            # $EDITOR で開く → 保存で validate + reload
```

CLI subcommand は **authenticated HTTP API** (Bearer 経由) で daemon に到達 (Minor 4 対応: draft round 2 の
「broker RPC」記述は誤り、 CLI config API は通常の HTTP API 経路)。

### Web UI

- `/settings` page (Templ + form) で以下を UI 化:
  - `sandbox.allowed_domains` (add/remove)
  - `gateway.forges.<forge>.host` / `.secret_key` (追加 / 削除)
  - `notify.command`
  - `web.public_url`
- YAML raw edit も可 (advanced tab、 monaco editor などで)

**削除**: `default_harness` (Major 2 対応、 Phase 2.5 PR7 で撤去済み、 現行 `Config` に存在しない)。

### validation

`boid config apply -f` / `boid config set` / `boid config edit` は保存前に schema validation を実施:
- required field の存在確認
- enum value の validity (現行の各 config field の validation を CLI 経路にも通す)
- allowed_domains の syntax チェック
- gateway.forges の各 forge 定義の完全性

validation error は human-readable な位置 + 理由付きで返す。

### reload semantics (round 2 Major 3 対応: atomicity + rollback + subsystem snapshot consistency)

**現状の SIGHUP は不在** (daemon は SIGINT/SIGTERM のみ処理)、 config file の inotify 監視も不在。
dynamic reload は **新規実装が必要**。

#### 実装アーキテクチャ

1. **config source of truth を daemon in-memory に**: 起動時に volume 内 config.yaml を read、
   `*config.Config` を `sync.RWMutex` 付きで保持
2. **reload API 追加** (authenticated HTTP API):
   - YAML AST preservation で file に書き戻す (unknown key / コメント保持)
   - atomic write (temp file + rename、 `flock` で advisory lock)
   - `revision` counter を用意して concurrent update を検出 (ETag に相当、 `If-Match` header)
   - **subsystem snapshot consistency** (下記): 複数 subsystem を単一 snapshot で更新
   - subscriber (allowed_domains → egress proxy、 notify.command → notify service、 etc.) に notify
3. **subscriber pattern**:
   ```go
   type ConfigSubscriber interface {
       OnConfigReload(ctx context.Context, oldCfg, newCfg *config.Config) error
   }
   ```
   subscriber の一部が失敗した場合の rollback protocol は下記
4. **subsystem snapshot consistency (round 2 Major 3 対応)**:
   - allowed_domains は現行 `server/proxy/runner` の 3 subsystem に **値コピー** されている
     (`cmd/start.go:193`, `server.go:595`, `dispatcher/runner.go:117`)
   - reload 時に 3 subsystem を **同時に snapshot** (`sync.Locker` の acquire 順序を daemon 側で pin)、
     一部だけ更新される中間状態を許さない
5. **subscriber 失敗時の rollback**:
   - reload の flow: (a) 新 config を file に atomic write → (b) 新 config を parse → (c) subscriber
     の pre-check (dry-run validate) → (d) subscriber の apply (順次 or 並列)
   - (c) or (d) で失敗した subscriber があった場合: **file を旧 config で rewrite**、 in-memory を
     旧に戻し、 apply 済み subscriber に旧 config で再 apply (reverse order)
   - この rollback 自体が失敗するケース (二重 error) は fail-hard で daemon abort、 operator 介入要
     (SecretStore purge じゃなく、 file を手動 rollback)

#### restart-required key の catalog (Major 3 対応、 未分類 key 追加)

| category | keys | 反映タイミング |
|---|---|---|
| **dynamic** | `sandbox.allowed_domains`, `notify.command`, `web.public_url` | reload 即時 (次 dispatch から反映) |
| **restart-required** | `gateway.forges.*` (dispatch 中の gateway TLS cert 再発行が絡む)、 `web.http_addr` (listener 再構築)、 `gc.*` (GC goroutine の再起動が絡む)、 `task_ask.*` (RPC 契約変更) | 保存 → next daemon restart で反映、 保存時に warning |
| **removed on volume-only** | `sandbox.backend` | 保存拒否 (エラー: "removed in volume-only cutover") |

**注**: `web.http_addr` / `gc.*` / `task_ask.*` は round 2 で codex に「未分類」と指摘された key。 restart-
required category に追加、 dynamic 化は将来別 PR で検討 (実装コスト大)。

#### 実装コストの再評価 (round 2 Major 3 対応: 1 PR に収まらない)

上記を単一 PR ではなく **§e の PR-4 の中で複数 sub-PR に切る**:

- **PR-4.a**: config in-memory + reload API + revision counter (基盤)
- **PR-4.b**: YAML AST preservation + atomic write + subscriber pattern (subscriber failure rollback 含む)
- **PR-4.c**: `boid config` CLI サブコマンド (get/set/apply/edit)
- **PR-4.d**: `/settings` Web UI page
- **PR-4.e**: client/daemon config file split (host 側 profile file を分離)

PR-4 内部で 5 段の連続 landed。 各段 additive/inert (existing subsystem 動作不変で新経路を追加)。

### 未解決論点

- **key の nested type** (`sandbox.allowed_domains` は array、 `gateway.forges` は map) を dotted path で
  操作する構文設計 (`boid config set sandbox.allowed_domains[0] .freee.co.jp` か
  `boid config set sandbox.allowed_domains .freee.co.jp .notion.com` (multi-arg) か)
- **secret key の editing 経路**: `gateway.forges.<forge>.secret_key` は env var 名なので値自体は非機密、
  ただし env var の値 (実 token) は編集経路が別 (env / systemd unit / k8s Secret)、 CLI/Web UI の scope 外

---

## 論点 g: workspace/project export/import shape (Major 4 対応)

### bundle 形式 (round 2 Major 4 対応: `init_script` は独立 file)

round 1 で `spec.init_script: |...` として YAML 内 inline に書いたのは、 現行 `workspace_home.go:225-235`
の設計 (「plain host-config file, not a DB-backed workspace resource、 environment-dependent shell content」)
と衝突。 訂正: **`init.sh` は独立 file のまま export bundle に含める**:

Export の物理形式:
- default: **tar.gz bundle** (`workspaces.tar.gz`)、 中身:
  ```
  workspaces.yaml            # 全 workspace + HostCommands (--- 区切り複数 document)
  workspaces/
      default/
          init.sh            # workspace default の init script
      bm-next/
          init.sh
      ...
  ```
- alt: **file list を stdout に** (`boid workspace export --format=multi -o workspaces/`)、 上記構造を
  directory tree として展開

apply も対応:
- `boid workspace apply -f workspaces.tar.gz` (tar 展開 → 全部 apply)
- `boid workspace apply -f workspaces/` (directory 直指定)
- `boid workspace apply -f workspace.yaml` (単一 YAML file、 init.sh は含めない or 別 file 指定 `--init-script <path>`)

### YAML shape (改定版、 codex 指摘の抜け fields 追加)

Kubernetes-like envelope (`apiVersion` / `kind` / `metadata` / `spec`) を採用。 draft round-1 で
`container_image` / `extra_repos` / `HostCommands` / `init.sh` の抜けが codex に指摘された。 補完:

```yaml
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: default                          # 現行 workspaces.slug
  revision: 42                           # optimistic concurrency 用 (apply 時 If-Match)
spec:
  container_image: boid-runner:latest    # workspace の job container image (WorkspaceMeta.ContainerImage、 Phase 6 予約 field)
  host_commands:                         # 現行 WorkspaceMeta.HostCommands
    - atl
    - gh
  env:                                   # 現行 WorkspaceMeta.Env (container 内 job container の env)
    ATL_SITE: ubs
    DOTNET_CLI_TELEMETRY_OPTOUT: "1"
  allowed_domains: []                    # 現行 WorkspaceMeta.AllowedDomains
  extra_repos:                           # 現行 WorkspaceMeta.ExtraRepos (workspace-scoped read-only 追加 repo)
    - https://github.com/some/private-go-mod.git
  capabilities:
    docker: {}                           # WorkspaceMeta.Capabilities
  init_script: workspaces/default/init.sh
                                         # bundle 内相対 path (存在すれば apply 時に materialize、
                                         # 無ければ init.sh 無しで workspace 作成)
  projects:                              # workspace 内 project の nested 定義 (nose 提案)
    - name: rook-server
      url: git@bitbucket.org:Aolani-ondemand/rook-server.git
      # branch: main                     # optional, default = origin/HEAD
    - name: mera-ui
      url: git@bitbucket.org:Aolani-ondemand/mera-ui.git
    - name: blanc-db-if
      url: git@bitbucket.org:Aolani-ondemand/blanc-db-if.git

---

apiVersion: boid.dev/v1
kind: HostCommands       # daemon-global host_commands.yaml の内容も一緒に export/import
metadata:
  name: default          # 単一 instance だが k8s-like envelope に統一
  revision: 15
spec:
  commands:
    - name: atl
      path: /usr/local/bin/atl
      # ... (現行 host_commands.yaml と同じ shape)
    - name: gh
      path: /usr/local/bin/gh
      # ...
```

### `additional_bindings` の扱い (Minor 2 対応、 継続)

現行 `WorkspaceMeta.AdditionalBindings` は Phase 4 (home-workspace-volume) で退役済み、 field も削除済み
(silent-drop on YAML unknown key)。 volume-only では **shape に含めない**。 現行 DB に残ってる column は
空 JSON array として persist される (既存挙動)。 round 1 draft の記述残 (「kits 前提」)は削除済み。

### env の host path 依存

現行の workspace `env` には host filesystem path が大量に含まれている (例: `GOPATH: /home/nosen/go`)。
volume-only では container 内 valid path のみ有効:
- `GOPATH: /home/boid/go` (container 内 path、 workspace HOME volume 内)
- host tool の bind (`/home/nosen/.volta` 等) は不可、 相当機能は container image (workspace の container_image
  field) に焼き込む

export 時に host path を検出したら warning を出す (「この path は container 内で invalid」)。

### CLI API + apply 契約 (Major 4 対応、 round 2 でさらに詳細化)

```bash
# export
boid workspace export <name> -o workspace.tar.gz              # 単一 workspace + init.sh
boid workspace export --all -o all-workspaces.tar.gz          # 全 workspace + HostCommands
boid workspace export <name> --format=yaml -o workspace.yaml  # YAML only (init.sh 含まない)

# import (apply)
boid workspace apply -f workspaces.tar.gz                     # bundle 展開 + 差分 apply (upsert)
boid workspace apply -f workspaces/                           # directory 直指定
boid workspace apply --dry-run -f workspaces.tar.gz           # 変更内容 preview のみ (remote/auth validation は skip)
boid workspace apply --dry-run --check-remotes -f ...         # remote validation も含める (時間かかる)
boid workspace apply --prune -f workspaces.tar.gz             # YAML に無い既存 project を削除
boid workspace apply --force -f ...                           # HostCommands conflict を上書き
boid workspace delete <name>                                  # workspace + 属する project + init.sh 全削除
```

### apply の transaction boundary + recovery journal (round 2 Major 4 対応)

複数 subsystem (DB / repo clone / init script materialize / HostCommands) を 1 apply で扱うので
transaction boundary が critical:

**phase 分け** (各 phase idempotent、 失敗時は次 phase に進まない、 status で可視化):

1. **phase 1: validate** — bundle の schema validation、 workspace/project name の一意性 check、
   HostCommands conflict check (`--force` 無い場合は fail)。 DB 変更なし、 filesystem 変更なし
2. **phase 2: DB upsert (single SQL transaction)** — workspaces / project_workspaces / projects の
   upsert、 status='apply-pending' で record 登録。 revision counter (optimistic lock) チェック
3. **phase 3: filesystem operations** — repo bare clone (or fetch)、 init.sh materialize (host_config
   に配置)、 HostCommands file の rewrite
4. **phase 4: status transition** — 各 project の status を 'ready' に更新 (DB update)

各 phase の失敗ハンドリング:

- **phase 1 失敗**: apply 中止、 DB/filesystem 無変更、 CLI/API に validation error 返す
- **phase 2 失敗**: SQL transaction rollback、 DB 無変更、 apply-error 返す
- **phase 3 失敗** (repo clone / init.sh / HostCommands のいずれか):
  - 該当 project/workspace の status を 'apply-error' に更新 (partial state を **可視化** する状態、
    dangling ではない — 「partial state を残さない」invariant は「dangling な rows を残さない」の意味で、
    「明示的な error status を可視化する」のは compatible、 round 2 Major 4 対応の矛盾解消)
  - 他 project は続行 (workspace 内で 1 project の failure が全 workspace apply を止めない)
- **phase 4 失敗**: SQL update 失敗、 個別 project の status は 'apply-pending' で残る (次回 apply/fetch で
  自動 retry)

**recovery journal**: apply の各 phase 開始時に `apply_journal` テーブル (新規、 additive migration) に
記録:

```sql
CREATE TABLE IF NOT EXISTS apply_journal (
    id TEXT PRIMARY KEY,
    started_at DATETIME NOT NULL,
    phase TEXT NOT NULL,
    workspace_name TEXT,
    project_name TEXT,
    completed_at DATETIME,
    error TEXT
);
```

daemon 再起動時、 `completed_at IS NULL` の journal entry を探して abandoned apply を検出 → 該当
project/workspace の status を 'apply-error' にして journal entry を close。 これで daemon crash 時の
dangling state を防ぐ。

### apply 6 点契約 (round 2 Major 4 対応、 全解消)

1. **project identity/一意キー**: `(workspace_name, project_name)` の composite key。 URL は identity ではなく
   contents (URL 変更は「同一 project の URL 差し替え」)
2. **URL 変更時**:
   - **default (`reclone` mode)**: bare repo dir を rename、 新 URL で `git clone --mirror`、 旧 dir は
     24h 後 GC
   - `--url-change=fetch`: in-place で `git remote set-url` + `git fetch --all`
   - `--url-change=reject`: エラー拒否
3. **YAML に無い既存 project の扱い**: **default: 残す** (`--prune` flag で削除 opt-in)。 conservative
4. **DB upsert と clone filesystem side effect の失敗回復**: 上記 phase 分け + journal で対応
5. **concurrent workspace update**: `revision` counter で optimistic concurrency (apply 時 If-Match、
   衝突時は apply-error に落として operator に notify)
6. **`--dry-run` の scope**: default は phase 1 のみ (validation + DB 変更 diff)。 `--check-remotes` で
   phase 3 の repo clone を dry-run 実行 (auth 疎通確認)
7. **project.yaml 内 `id/name` と envelope の食い違い** (round 2 で追加): envelope が source of truth、
   `id` は無視 (自動生成)、 `name` は envelope の値で override
8. **重複 document / unknown kind**: apply 時に reject (envelope schema validation の一部)
9. **HostCommands conflict**: 既存 host_commands.yaml と diff 検出、 `--force` 無しは reject

### migration path (現行 5 workspaces)

nose との対話で確認済み: 現行 workspace 定義 (`bm-next` / `boid` / `default` / `khi` / `ubs`) の
`env` / `additional_bindings` は host path 依存が大量、 volume-only 化に伴い **clean start** で再構築。
volume-only cutover 時に:
1. 現行 DB から workspaces + projects を **手動 dump** (post-incident snapshot 経由 or fresh export)
2. 新方式 shape に nose が書き直し (container_image 選択、 env の container path 化、 init.sh 記述)
3. 空 volume で fresh start → `boid workspace apply -f workspaces.tar.gz` で reimport

### 未解決論点

- **`ContainerImage` は workspace ごと必須にするか** (default で `boid-runner:latest` を採用するか)
- **project の branch policy** を workspace レベルで override するか (`spec.projects[].branch_policy`)
- **secret 参照**: `gateway.forges.<forge>.secret_key` は現行 workspace scope でない (daemon-global config)、
  workspace 単位で forge auth を分離するかは別論点 (今回 scope 外)

---

## 論点 h: 移行手順 (新方式単発切替)

### big-bang cutover の意味 (rollback 用語整理、 round 1 Minor 3 対応)

**「rollback path 無し」の正確な意味**: 新 state を保った切戻しは無い (volume-only の named volume が
書き込んだ state を旧 host daemon に引き継ぐ経路は無い)。

一方 **disaster fallback は存在する**: PR revert + volume-only stack を down + 旧 host daemon を再起動して
6-27 backup restore で「Phase 6 直前の状態に戻る」ことは可能。 これは deploy 時失敗の緊急回避策として
意識しておく (通常 rollback ではない、 fresh install 相当への切戻し)。

### タイムライン

1. **本 doc レビュー完了** (round 3 完了 → nose 承認 → 本 doc landed)
2. **PR-1 (seam 導入) 実装 + landed** — 現行動作不変
3. **PR-2 (volume-only compose + staging consumer feature-gated) 実装 + landed** — 現行動作維持 + opt-in
4. **PR-3 (URL-aware startup + auto-prune 撤去 + additive migration) 実装 + landed** — auto-prune 撤去が
   default で発火 (destructive delete 消える、 全 mode で安全化)
5. **PR-4 (CLI Bearer + TLS bootstrap + config CLI/Web UI) 実装 + landed** — 5 段 sub-PR (§f 参照)、
   default_profile 未 seed 時は unix fallback 継続
6. **PR-5 (workspace export/import + repo cache 実装) 実装 + landed** — workspace export bundle 使える
7. **PR-6 (cutover: default 切替 + unix fallback + userns + host daemon 一斉撤去) 実装 + landed** — 単一 mode 化
8. **手動 cutover 実施** — nose の host:
   - 現行 host daemon 停止
   - **`~/.config/boid/host_commands.yaml` / workspace init.sh を bundle 形式に手動整理** (Phase 4 の
     workspace init.sh 群 + host_commands 定義から export bundle を手作り)
   - `./scripts/deploy-container.sh` で volume-only compose stack start
   - Web UI で pair code 発行、 `docker exec container_daemon_1 boid ca fingerprint` で CA fingerprint 取得、
     `boid login https://localhost:8443 --ca-fingerprint "$FP" --profile local` で initial pair
   - `boid workspace apply -f workspaces.tar.gz` で initial import
   - `boid task list` で疎通確認

### 手動 cutover 中の risk

- **fresh DB による device pair 全部失効**: DB に device rows が無くなるため、 Web UI + CLI いずれも
  再 pair 要 (`docker exec ... boid web pair` → `boid login`)
- **web_secret 消失により session cookie 全部無効**: 既存 browser session が invalidate される (再ログイン要)
- **secret.key 消失により SecretStore 内 secret 全部破棄**: SecretStore に保存してた API token / OAuth
  refresh token 等は復元不可 (fresh から入れ直し)
- **CA 消失により内部 mTLS の cert 全部再発行**: 新 CA が起動時 auto generate、 稼働 job 無いので実害無し
- **install_id 変わる**: git-gateway cert scoping 再発行、 reap の label filter が新 install_id 基準で動く
- **CI 側 (blackbox-e2e.yml) の整合**: PR-2 で `e2e-container-volume-only` job 追加、 PR-6 で
  `continue-on-error: false` 格上げ、 PR-6 で userns 系 e2e scenario 削除

### Podman socket は cutover checklist で明示 (round 2 Minor 5 対応、 PR-2 前 gate)

現行 `compose.yml:331-335` は `/var/run/docker.sock` 固定 bind。 podman-only host では既に問題化していたが、
本 doc の scope は **volume-only 化** のみで engine socket abstraction は含めない。

**cutover checklist に明示的 gate**:

> **PR-2 実施前の必須 gate**: engine socket abstraction (podman rootless の
> `/run/user/<uid>/podman/podman.sock` 対応) の follow-up PR を先に landed させること。 このため PR-2 の
> 実装着手は engine socket abstraction PR の landed 後とする。 該当 follow-up は本 doc の scope 外
> (今日 revert した fix/phase6-deploy-podman-socket branch の考え方を新 stack に再適用する形になる)。

---

## 論点 i: k8s 移行時の seam (Major 6 対応、 interface signature 拡張)

### 目的

k8s Helm chart 実装 (Phase 7) は本 doc の scope 外だが、 **volume-only 実装段階で「k8s と 1:1 対応する
seam」を作っておく**ことで将来の refactor を最小化する。

### 3 つの seam (round 2 Major 6 対応で interface 拡張)

#### 1. `SecretProvider` interface

round 1 の draft の thin wrapper (`GetOrCreate(name)`) では insufficient (codex round 2 Major 6 指摘):
`secret.key` は単一 32-byte file だが CA は cert + key の 2 file、 generator error / file mode / validation /
multi-file transaction を表現できない。

**拡張 interface**:

```go
type SecretMaterial struct {
    Name        string           // logical name (例: "ca")
    Files       map[string][]byte // 相対 filename → contents (例: {"ca.crt": ..., "ca.key": ...})
    FileModes   map[string]os.FileMode  // 相対 filename → mode (例: {"ca.crt": 0644, "ca.key": 0600})
}

type SecretProvider interface {
    // 単一 file (secret.key/web_secret/install_id) の load-or-generate
    GetOrCreateFile(ctx context.Context, name string, mode os.FileMode,
                     generator func() ([]byte, error)) ([]byte, error)

    // multi-file (CA cert + key) の load-or-generate、 file 群を atomic に扱う
    GetOrCreateMulti(ctx context.Context, name string,
                      generator func() (*SecretMaterial, error)) (*SecretMaterial, error)

    // validation (存在チェック + mode 確認 + content shape validation)
    Validate(ctx context.Context, name string) error
}
```

実装:
- **VolumeSecretProvider** (compose deploy): `<data_dir>/<name>/*` に file 群を配置、 atomic write は
  temp file + os.Rename で全 file の rename が完了してから publish (multi-file の atomicity)
- **KubernetesSecretProvider** (k8s deploy、 将来): initContainer で K8s Secret から
  `/etc/boid/secrets/<name>/` に copy 済みという assumption、 daemon は `GetOrCreateMulti` で file 群を
  read (generator は fall back path、 通常発火しない)

#### 2. `RepositoryCache` interface (§論点 a の refactor 用 + §論点 b の bare repo 経路実装用)

round 1 の draft (`Materialize`/`Fetch`/`Remove`/`List` のみ) では insufficient (codex round 2 Major 5 指摘):
default branch / project.yaml read / ref lookup / URL 変更 / lock lifetime が表現不可。

**拡張 interface**:

```go
type LockMode int
const (
    LockShared LockMode = iota   // 並行 fetch/dispatch 可能
    LockExclusive                 // rm/mv/rename/git gc 用
)

type ProjectRef struct {
    ID           string
    Name         string           // project_name
    WorkspaceID  string
    UpstreamURL  string
    CachePath    string           // bare repo path (internal)
    Status       string           // 'ready' | 'degraded' | 'apply-error'
    StatusReason string
    Revision     int
}

type CheckoutHint struct {
    // job dispatch 時、 job container に mount する path 情報を返す
    // (staging volume 内の subpath、 §論点 b 参照)
    HostVisiblePath   string   // daemon 内 path (staging volume の subpath)
    ContainerMountPath string  // job container 内 mount path (通常 /workspace/<project-name>/)
}

type RepositoryCache interface {
    // project 登録時、 URL から bare clone、 ProjectRef を返す
    Register(ctx context.Context, workspace, projectName, url string) (*ProjectRef, error)

    // 明示的 fetch (up-to-date 化)、 status を ready に戻す (fetch 成功なら)
    Fetch(ctx context.Context, ref *ProjectRef) error

    // bare repo 内の file を読む (project.yaml 等)
    ReadFile(ctx context.Context, ref *ProjectRef, path string) ([]byte, error)

    // default branch (upstream HEAD が指す branch 名)
    DefaultBranch(ctx context.Context, ref *ProjectRef) (string, error)

    // branch/tag/commit SHA の resolve
    RefLookup(ctx context.Context, ref *ProjectRef, refspec string) (commitSHA string, err error)

    // job dispatch 用の checkout materialize (staging volume に clone)
    Materialize(ctx context.Context, ref *ProjectRef, jobID, branch string) (*CheckoutHint, error)

    // job 終了時の cleanup (staging から delete)
    CleanupCheckout(ctx context.Context, jobID string) error

    // shared/exclusive lock (fetch/dispatch は shared、 rm/mv/gc は exclusive)
    Lock(ctx context.Context, ref *ProjectRef, mode LockMode) (unlock func(), err error)

    // URL 変更
    ChangeURL(ctx context.Context, ref *ProjectRef, newURL string, mode URLChangeMode) error

    // 削除
    Remove(ctx context.Context, ref *ProjectRef) error

    // 一覧
    List(ctx context.Context) ([]*ProjectRef, error)
}
```

実装:
- **VolumeRepositoryCache** (compose deploy): named volume 内 `<repo_cache>/<workspace>/<project>.git` に
  bare clone、 lock は `flock` (advisory)
- **KubernetesRepositoryCache** (k8s deploy、 将来): PVC 経由、 lock は k8s Lease で分散対応

#### 3. `JobMountDescriptor` interface (§論点 b の mount 経路実装用)

round 1 の draft (`DescribeMounts(jobID)` のみ) では insufficient (codex round 2 Major 6 指摘):
workspace/project、 read-only、 repo/home/staging、 cleanup、 EmptyDir init seed が表現不可。

**拡張 interface**:

```go
type MountSpec struct {
    Source        string           // volume 名 or path
    Target        string           // container 内 mount path
    Subpath       string           // volume-subpath (staging 経路)
    ReadOnly      bool
    InitSeed      *InitSeedSpec    // EmptyDir 等の初期化 (k8s 用、 compose では nil)
}

type InitSeedSpec struct {
    // initContainer 相当で volume に seed する内容 (k8s 用)
    Files map[string][]byte  // 相対 path → contents
}

type JobContext struct {
    JobID       string
    WorkspaceID string
    ProjectRef  *ProjectRef
    Checkout    *CheckoutHint
    TLSMaterial *SecretMaterial     // per-job cert (SecretProvider 経由で発行済み)
    BrokerTLS   *SecretMaterial     // broker 用 per-job cert
}

type JobMountDescriptor interface {
    // 与えられた JobContext に対して mount 記述群を返す
    DescribeMounts(ctx context.Context, jc *JobContext) ([]MountSpec, error)

    // job 終了時の cleanup (staging subpath 削除、 EmptyDir だと k8s が Pod 削除時に自動)
    Cleanup(ctx context.Context, jobID string) error
}
```

実装:
- **VolumeJobMountDescriptor** (compose deploy): named volume の subpath mount (docker/podman の
  `volume-subpath` 経路)
- **KubernetesJobMountDescriptor** (k8s deploy、 将来): PVC の subPath mount + EmptyDir で per-job 独立
  volume (initContainer で `InitSeedSpec` の内容を EmptyDir に copy)

### k8s 1:1 mapping の精緻化 (round 1 Major 6 対応、 継続)

| compose deploy | k8s deploy |
|---|---|
| `boid_private` named volume (`/home/boid/.local/share/boid`) | PVC (RWO) — DB / CA private key 含むので node affinity or RWX + strict subPath isolation |
| `boid_staging` named volume | 選択肢 A: PVC (RWX、 daemon Pod と job Pod 共有)、 選択肢 B: 各 job Pod で EmptyDir (spec/cert を initContainer で seed) |
| `boid_repos` named volume | 選択肢 A: PVC (RWX、 daemon fetches, job reads via subPath)、 選択肢 B: job init-job で bare clone を EmptyDir に materialize |
| `boid_homes_<workspace>` | PVC per workspace (Phase 4 の workspace $HOME contract そのまま) |
| secret material (`secret.key`, `web_secret`, CA) — on-first-boot generate | K8s Secret を initContainer で volume に seed、 daemon は generator を fall back として持つ |
| CLI listener (HTTPS + Bearer + fingerprint pin) | Service (ClusterIP or Ingress)、 Bearer は現行契約、 fingerprint pin は変わらず (K8s Secret から fingerprint 配布) |
| workspace HOME | PVC per workspace |

**注記**: PVC の subPath isolation は security boundary になる。 daemon Pod と job Pod が同 PVC を触る
経路では、 subPath の path 権限管理が critical (K8s subPath mount は subPath の外に breakout できない
挙動を提供、 これに依存する)。

### 実装 PR (§e の PR-1 相当)

PR-1: seam の導入 (additive) で:
- `internal/orchestrator/secret_provider.go` に `SecretProvider` interface + `VolumeSecretProvider` 実装
- `internal/orchestrator/repository/cache.go` に `RepositoryCache` interface + `VolumeRepositoryCache`
  実装 (現行 dir 経路の wrapper)
- `internal/dispatcher/job_mount_descriptor.go` に `JobMountDescriptor` interface + `VolumeJobMountDescriptor`
  実装 (現行 bind mount 経路の wrapper)

これらの interface が PR-2〜PR-6 の実装で使われ、 将来 `KubernetesXxx` を実装するときに compose 経路と
共通の consumer を持てる形になる。

### 未解決論点

- **PVC の accessMode**: RWX が必要な volume (staging / repos) の選定、 storage backend の要件
- **initContainer と daemon Pod の起動 sequencing**: secret seed → CA load → daemon start の順序管理
- **Service type**: ClusterIP + Ingress か LoadBalancer か、 mTLS termination の場所

---

## 未解決の設計論点まとめ

各章の「未解決論点」を集約 (実装 PR 前に nose 判断を得るべきもの):

**Blocker から昇格した設計決定** (実装 PR 前に本文で fix 済み、 但し実装時に再確認要):

- §a: **auto-prune 撤去 + `degraded` status** (Blocker 4)、 **additive migration `project_name` /
  `status` / `status_reason` / `revision`** (Blocker 2)、 **`work_dir` 意味論変更 → `ProjectRef` +
  `RepositoryCache` 経由** (Major 5)
- §b: **staging volume の subpath isolation は Docker `VolumeOptions.Subpath` + capability probe +
  fail-hard 契約** (Blocker 3)、 **bare cache は mirror refspec、 job は `--reference` + 終了時
  `git repack -ad`** (Major 1)
- §c: **HTTPS + Bearer 契約維持 + TLS trust bootstrap (internal CA 再利用 + `boid ca fingerprint` +
  `--ca-fingerprint` 必須)** (round 2 Blocker 1)
- §d: **break-glass procedure (DB secrets purge + secret material 削除 + 再起動)** (Major 7)
- §e: **PR 分割 additive/inert、 PR-3 で auto-prune 撤去、 PR-6 で cutover 一斉** (Major 1, 2)
- §f: **config reload の subsystem snapshot consistency + subscriber failure rollback + restart-required
  catalog** (Major 3)
- §g: **apply の transaction phase 分け + recovery journal + init.sh は bundle 内独立 file** (Major 4)
- §i: **`SecretProvider`/`RepositoryCache`/`JobMountDescriptor` の interface 拡張** (Major 6)

**継続 未解決 (PR-1 着手前に individual に nose 判断を得る)**:

- **論点 a**: project.yaml validate タイミング (add 時 sync vs 初回 dispatch)、 fetch depth (mirror full
  vs shallow/partial)
- **論点 b**: branch policy との整合、 staging volume の disk 圧迫 / GC 統合、 podman VolumeOptions.Subpath
  の empirical 挙動確認
- **論点 c**: TLS listener の HTTP/2 有効化、 initwizard の compose deploy 対応 (別 doc)
- **論点 d**: k8s Secret provider 経路の initContainer 契約、 atomic write の multi-file 一貫化
- **論点 e**: e2e coverage の container 経路移植完了状況、 `boid start` の CLI wrap 化
- **論点 f**: dotted path 構文 (array/map の扱い)、 secret env var 値の編集経路
- **論点 g**: `ContainerImage` default 化、 branch policy の workspace override、 forge auth の workspace scope
- **論点 h**: (この章に固有の未解決論点は無し、 他章に集約)
- **論点 i**: PVC accessMode 選定、 initContainer sequencing、 Service type

---

## codex review 対応 summary

### Round 1 (初回 draft、 622 lines) → Round 2 draft (1000 lines)

- Blocker 4: 全対応 (job state 配送 / bare repo worktree / CLI TLS / auto-prune 撤去)
- Major 6: 全対応 (PR 分割 / config reload / secret 事実修正 / workspace shape / project lifecycle / k8s seam)
- Minor 4: 全対応

### Round 2 (Blocker 3 / Major 7 / Minor 5) → Round 3 draft (本 doc、 予想 1500+ lines)

**Blocker 3 全対応**:
- 1: HTTPS + Bearer bootstrap の TLS trust → §c に TLS bootstrap 章追加 (internal CA 再利用、
  `boid ca fingerprint`、 `--ca-fingerprint` 必須、 fail-closed)
- 2: 現行 DB schema で `degraded` 状態表現不可 → 目的節から「schema 変更しない」を撤回、 §a に additive
  migration (`project_name` / `status` / `status_reason` / `revision`) の schema 定義追加
- 3: staging isolation の 2 方式混在 → §b で Docker `VolumeOptions.Subpath` (API v1.45+) に固定、 capability
  probe + fail-hard 契約明記

**Major 7 全対応**:
- 1: bare cache refspec と `--dissociate` → §b で mirror clone 採用、 job は `--reference` + 終了時
  `git repack -ad` に変更
- 2: PR 分割の intermediate main 破綻 → §e で PR-3 に auto-prune 撤去 + additive migration を先出し、
  PR-6 で unix fallback + userns + host daemon 一斉撤去
- 3: config reload 1 PR 分不足 → §f で 5 段 sub-PR (PR-4.a〜PR-4.e) に分割、 subsystem snapshot consistency +
  subscriber failure rollback protocol 追記
- 4: workspace apply の transaction boundary → §g で phase 4 段 + `apply_journal` recovery、 `init_script` を
  bundle 内独立 file に、 apply 契約 9 点全解消
- 5: `.WorkDir` grep + interface signature → §a に `.WorkDir` consumer 20+ 箇所 audit table、 §i の
  `RepositoryCache` interface に `DefaultBranch` / `ReadFile` / `RefLookup` / `Lock` 追加、 `ProjectRef` 型導入
- 6: `SecretProvider` / `JobMountDescriptor` の thin wrapper 不足 → §i で interface signature 拡張
  (`SecretProvider.GetOrCreateMulti`、 `JobMountDescriptor` に `JobContext` + lifecycle + `InitSeedSpec`)
- 7: manual secret deletion の recovery 手順不足 → §d に break-glass procedure 定義 (daemon 停止 → DB
  purge → file 削除 → 再起動 → 再 pair)

**Minor 5 全対応**:
- 1: initwizard 誤記述 → §c 「未解決論点」で「initwizard は project.yaml scaffold、 connection wizard 別 doc」明記
- 2: retired 概念記述残 → §b / §g で削除 (「kits 前提」記述削除、 additional_bindings は既に削除)
- 3: `boid login` の profile derive → §c で `--profile local` 明示、 `localhost` (dot 無し) → `localhost` の
  挙動記述
- 4: config reload の「broker RPC」→ §f で「authenticated HTTP API」に修正
- 5: Podman socket follow-up の PR-2 前 gate → §h に explicit checklist 追加

---

## 参考リンク

- [phase6-container-backend.md](phase6-container-backend.md) — Phase 6 本体 (全 9 PR landed)、 §決定4 は本 doc で撤回
- [phase6-cutover-followups.md](phase6-cutover-followups.md) — 段階撤去計画、 本 doc の PR 群で吸収
- [container-based-boid.md](container-based-boid.md) — Phase 6 の前提となる移行戦略 ①-⑦、 大枠は継続
- [home-workspace-volume.md](home-workspace-volume.md) — Phase 4 の workspace $HOME volume、 論点 b の
  workspace HOME volume 経路で参照。 `workspace_home.go:225-235` の init.sh 独立 file 契約は本 doc §g で継承
- [cli-remote-connection.md](cli-remote-connection.md) — Phase 3 CLI リモート接続、 論点 c の HTTPS + Bearer
  契約は本 doc でも維持、 TLS bootstrap は本 doc で追加
- [workspace-db-consolidation.md](workspace-db-consolidation.md) — Phase 2.5、 `default_harness` / kits 撤去の
  経緯 (§f / §g で参照)
- `container-git-gateway-design` (memory) — git gateway 実装、 論点 b の bare repo fetch 経路で参照
- `phase6-dogfood-incident-and-pivot` (memory) — 本 doc の pivot 経緯記録
