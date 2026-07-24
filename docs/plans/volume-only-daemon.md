# volume-only daemon: Phase 6 の compose 部分の再設計

ステータス: **draft (2026-07-24 作成、実装未着手、 codex review round 1 + 2 + 3 反映済み)**。
親ドキュメント: [phase6-container-backend.md](phase6-container-backend.md) の compose deploy 部分
(§決定4「host BIND mounts で shared state」) を置換する。
[phase6-cutover-followups.md](phase6-cutover-followups.md) §①-④ は本 doc の実装 PR 群で吸収される。
発端インシデント: `phase6-dogfood-incident-and-pivot` (memory) — 2026-07-24 の dogfood 初回起動で
bind mount 経路の 3 段の壁が顕在化、 nose 判断で pivot。

---

## 背景と pivot 経緯

Phase 6 (PR #816-#826) は container backend (docker/podman を job runtime にする経路) を実装完了させ、
`sandbox.backend: container` config で opt-in できる状態まで持ってきた。 §①-④ (docs/plans/phase6-cutover-followups.md)
は「dogfood → userns 撤去 → host daemon 撤去 → config option fold」という段階撤去計画を定義していた。

2026-07-24 の dogfood 初回セッションで、 `scripts/deploy-container.sh` の実挙動を通そうとしたところ
以下 3 段の壁を連続で踏んだ:

1. **`/var/run/docker.sock` 非存在** (podman-only host) — compose.yml が DooD 前提で bind mount しようとして失敗
2. **podman rootless の subuid mapping** — container 内 uid 1000 が host uid ~100999 に mapping されて
   host 側 mode 0600 の bind mount が **owner mismatch で permission denied**
3. **container 内から host filesystem 見えず → auto-prune で DB 大量消失** — `internal/server/wire.go:240`
   の hard delete 経路が発火して projects 18 + cascade で tasks 479 / jobs 816 が全て削除

1 と 2 は追加 fix で塞げるが、 3 は **「host filesystem を daemon container に持ち込む」architecture 自体**
が生む問題。 nose 判断: 「host filesystem 依存を廃止、 volume-only daemon に pivot」。

§決定4 (host BIND mounts で shared state) と、 それを前提とする rollback 契約は撤回される。

---

## 目的と非目的

### 目的

- daemon container の永続 state を **named volume に 1 本化**、 host filesystem 依存を完全に除去
- 現行 `~/.local/share/boid/` / `~/.config/boid/` の bind mount を撤廃、 相当の内容は volume 内で生成 / 管理
- project 登録経路を **git remote URL からの bare clone** に変える (host 側の checkout dir を register
  する現行 UX は廃止)
- secret material (`secret.key` / `web_secret` / daemon CA) を **起動時 volume 内で generate**
- workspace / project 定義を **export/import bundle** で扱う (Kubernetes-like envelope、 workspace 内に
  project を nested、 init.sh 等の独立 file も bundle に含める)
- config.yaml の編集は **CLI (`boid config ...`) + Web UI (`/settings`)** 経由
- userns backend / host daemon 起動経路は **新方式 cutover と同時に廃止** (単発切替、 rollback 契約撤回)
- daemon の永続 state を「daemon-only の private volume」と「job container に見せる staging area」に
  **明示的に分離** (§論点 b、 codex Blocker 1 対応)
- k8s 移行時に refactor の破壊が最小になる **seam を volume-only 実装段階で作る** (§論点 i)
- 公開 API (CLI HTTPS 経路) の **TLS trust bootstrap** を明示契約化 (§論点 c、 CA cert OOB 経路 + pin)

### 非目的

- **k8s Helm chart 設計** — 本 doc は「compose deploy を host filesystem 非依存にする」まで、
  Helm chart / operator は Phase 7。 ただし seam (§i) は本 doc で決める
- **project schema の schema-breaking 変更** — 本 doc は **additive migration のみを許容**
  (`projects.project_name` / `status` / `status_reason` / `revision` を ADD COLUMN、
  `apply_journal` テーブルの追加)。 既存 column の型変更・削除・意味論反転は本 doc の scope 外
- **secret rotation の実装** — 本 doc は on-first-boot generate のみ、 rotation は別 doc。 dogfood 期間中の
  緊急対処は **break-glass procedure** を §論点 d で定義

---

## 全体像

```
[host user] --- CLI (HTTPS + Bearer token + pinned CA cert)
                       \
                        +---> [daemon container] --- (engine socket via DooD) ---> [job container 1..N]
                       /       - private volume (daemon-only):
[host user] --- Web UI            /home/boid/.local/share/boid/{boid.db,secret.key,web_secret,tls/,install_id}
                (HTTPS+WSS,       /home/boid/.config/boid/config.yaml
                 same CA)      - staging volume (job-visible via subpath):
                                  /home/boid/staging/{spec/<job>/,tls/<job>/,broker-tls/<job>/,checkouts/<job>/}
                               - repo cache volume (workspace scoped、 daemon-writable):
                                  /home/boid/repos/<workspace>/<project>.git   (bare mirror)
                               - workspace $HOME volume (Phase 4、 workspace 単位、 job-writable):
                                  /home/boid/homes/<workspace>/
```

---

## 論点 a: project モデル transition (dir → git URL)

### 現行

`boid project add <dir>` で host filesystem checkout を register。 `projects.work_dir` に host absolute
path。 project.yaml 欠落時は `wire.go:226-249` で **hard delete** (今日の DB 全滅の直接原因)。

現行 schema (`0001_initial.sql` + `0028_add_projects_upstream_url.sql`):

```sql
CREATE TABLE projects (
    id TEXT PRIMARY KEY, work_dir TEXT NOT NULL,
    created_at DATETIME, updated_at DATETIME,
    upstream_url TEXT   -- 0028 で追加
);
CREATE TABLE project_workspaces (
    project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL
);
```

### 新方式

`boid project add <git-url> --workspace=<name> [--name=<project-name>]` で URL register。 daemon は
repo cache volume 内に **bare mirror clone**、 project 定義は DB (下記 schema) を source of truth。

### 新 DB schema (additive migration、 Blocker 2 対応)

```sql
-- migration NNNN_projects_add_name_and_status.sql
ALTER TABLE projects ADD COLUMN project_name TEXT;
ALTER TABLE projects ADD COLUMN status TEXT NOT NULL DEFAULT 'ready';
    -- 'ready' | 'degraded' | 'apply-pending' | 'apply-error'
ALTER TABLE projects ADD COLUMN status_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN revision INTEGER NOT NULL DEFAULT 0;
```

**status 値** (round 3 で `apply-pending` 追加、 §g との整合):

| status | 意味 | 遷移 |
|---|---|---|
| `ready` | 正常 (bare repo 存在、 project.yaml valid) | 初期値、 fetch/apply 成功で戻る |
| `degraded` | bare repo missing / fetch failure / project.yaml parse error 等 | on-startup / fetch 失敗で遷移 |
| `apply-pending` | workspace apply の phase 2 完了、 phase 3 (filesystem ops) 実行中 | apply phase 2 で set、 phase 4 で ready へ |
| `apply-error` | apply 中の partial failure | apply phase 3 で失敗、 明示 recovery まで残る |

**dispatch は `ready` のみで許可**、 他 status は refuse。 復旧は `boid project fetch <id>` / `boid workspace apply -f` / 明示 `boid project rm`。

### 一意性の直列化 (round 3 Major 1 対応)

**app-side check の直列化保証**: 単純な `COUNT + INSERT` は race がある。 対策:

- **`BEGIN IMMEDIATE` transaction + workspace-scope mutex** (in-memory `sync.Mutex` map keyed by workspace_id)
- `BEGIN IMMEDIATE` で SQLite の write lock 取得 (以降の read/write は同 tx 内で consistent)
- workspace mutex は cross-tx race を防ぐ (SQLite tx 開始前に取得)
- transaction 全体を retry 対象 (SQLite BUSY で retry、 exponential backoff)

疑似コード:

```go
mu := workspaceMutex(workspaceID)  // sync.Mutex per workspace
mu.Lock()
defer mu.Unlock()

tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
// (sqlite3 driver は BEGIN IMMEDIATE を出す)
defer tx.Rollback()

var count int
tx.QueryRow(`SELECT COUNT(*) FROM projects p JOIN project_workspaces pw ON p.id=pw.project_id
             WHERE pw.workspace_id=? AND p.project_name=?`, workspaceID, name).Scan(&count)
if count > 0 { return ErrDuplicate }

tx.Exec(`INSERT INTO projects (...) VALUES (...)`)
tx.Exec(`INSERT INTO project_workspaces (project_id, workspace_id) VALUES (?, ?)`, ...)
tx.Commit()
```

**注**: 「非目的」で「project_workspaces に unique constraint 追加」と書いた記述と matter で不一致
(round 3 Major 1 指摘)、 constraint は付けられない (JOIN 越し unique)。 目的節から該当記述削除、
app-side + mutex + BEGIN IMMEDIATE で保証。

### auto-prune の撤去 (Blocker 4 対応、 継続)

`wire.go:226-249` の on-startup hard delete 経路は **撤去**。 startup 挙動:
- bare repo missing → **再 clone 試行**、 失敗なら `status='degraded'`
- `project.yaml` parse error → 同じく `status='degraded'`

明示的削除入口は `boid project rm` / `boid workspace delete` のみ。

### 事前 validation

`boid project add <git-url>` 同期処理:
1. URL syntax validation + slug/path traversal (`^[a-z0-9][a-z0-9._-]*$`)
2. workspace 存在確認
3. workspace-scope mutex acquire → BEGIN IMMEDIATE tx
4. workspace 内 project name 一意性 verify (上記直列化)
5. URL canonicalize (`NormalizeOriginURL`)
6. bare mirror clone 実行 (§論点 b 参照)
7. `project.yaml` パース + schema validation
8. tx commit (status='ready')

**同期 validate**: nose 判断: 「add 時に fail-loud」が正しい。

### CLI 契約変更

- `boid project add <dir>` → **removed**
- `boid project add <git-url>` → **new**、 workspace 指定必須
- `boid project rm <id>` → 現行維持 + repo cache dir 削除 + 進行中 fetch/dispatch 中断
- `boid project list` → URL + status 表示
- `boid project fetch <id>` → **new** (bare repo explicit refetch、 degraded 復旧経路)
- `boid project status <id>` → **new** (status / status_reason 詳細)

### `work_dir` の意味論変更 (Major 5 対応、 round 3 で consumer audit 拡張)

`.WorkDir` を参照している箇所 (`internal/` 全体、 round 3 で追加 6 箇所発見):

| file | 用途 |
|---|---|
| `internal/api/project.go:37,73,78` | project list/create response の WorkDir field |
| `internal/api/project_service.go:820` (round 3 追加) | project service layer |
| `internal/orchestrator/project_store.go:315,317` | on-startup Load (§撤去対象) |
| `internal/orchestrator/planner.go:116` (round 3 追加) | planner の repo path 参照 |
| `internal/server/api_store.go:331` (round 3 追加) | API store layer |
| `internal/sandbox/runner/runner.go:233,275` | runner Workspace field、 JobDone RPC |
| `internal/sandbox/realization/realization.go:194,229` | docker create の Workdir field |
| `internal/api/task_create.go:50,54,145,146,168` | branch classification / 展開 |
| `internal/api/task_notify.go:327,330` | gitFetchOrigin (filesystem git fetch) |
| `internal/dispatcher/session_job.go:177` | session job cwd |
| `internal/dispatcher/sandbox_builder.go:591` (round 3 追加) | sandbox builder の cwd |
| `internal/dispatcher/gitgateway_wire.go:216` (round 3 追加) | git gateway 経路 |
| `internal/dispatcher/runner.go:602` (round 3 追加) | dispatcher runner の repo lookup |
| `internal/dispatcher/workspace_parent_dir_test.go` | test |

**特に注意** (round 3 Major 7 対応): `filepath.Base(WorkDir)`, git origin 取得、 policy context、 peer 列挙は
URL を path 扱いすると semantic bug。 全 consumer を `ProjectRef` + `RepositoryCache` 経由に書き換え。

**新型 `ProjectRef`** (詳細 §論点 i):
```go
type ProjectRef struct {
    ID, Name, WorkspaceID, UpstreamURL, CachePath string
    Status, StatusReason string  // ready/degraded/apply-pending/apply-error
    Revision int
}
```

`RepositoryCache` interface で consumer は URL/name/branch/ref/file を型安全に扱う (§論点 i)。

### migration path

現行 project は **auto-migration しない** — 既存 project は `boid project rm` で削除 → `boid project add <git-url>`
で register し直す。 nose: 「boid のデータは揮発性許容」。

### 開発者ワークフローへの影響

「host に checkout した local branch (未 push) で agent に作業」ユースケースは **失われる**。 nose 判断:
「boid の作業と host 側 checkout が衝突する問題の方が大きい」。

---

## 論点 b: daemon 管理 state と job container への配送 (Blocker 1, 2, 3 対応)

### 4 volume 分離

| volume | scope | 中身 | job container mount |
|---|---|---|---|
| `boid_private` | daemon 専用、 job 見えない | `boid.db`, `secret.key`, `web_secret`, `tls/`, `install_id`, `config.yaml` | **不可** (invariant) |
| `boid_staging` | daemon-writable、 job-readable/writable (subpath 単位) | 各 job 用 spec/state/TLS material/checkout 一時領域 | 可 (job 単位で subpath mount) |
| `boid_repos` | daemon-writable、 job-readable (subpath 単位、 read-only) | workspace/project ごとの bare mirror | 可 (job dispatch 時に project 単位、 read-only) |
| `boid_homes_<workspace>` | workspace 単位、 job-writable | workspace HOME (Phase 4 の既存経路) | 可 (workspace 単位で丸ごと) |

### DooD 制約と staging volume (Blocker 1 対応)

現行 (`container_backend.go:186-199, 1409-1433`) の DooD 契約:
> Launch is a DooD backend: mount Source it hands the HOST's docker daemon has to be a path the HOST
> filesystem actually has.

現行は `BOID_RUNTIME_DIR` を host bind で回避。 volume-only 化するとその host-visible path 消滅。
sibling job container に mount する staging area を **daemon-owned named volume + subpath mount** で確保。

### staging isolation は behavioral probe (round 3 Blocker 2 対応)

**round 2 の draft で採用した「API version probe」は撤回**。 `/_ping`/`/version` が v1.45 返しても
podman compat API が `VolumeOptions.Subpath` を実際適用する保証なし (未知 field を silently 無視すると
job container に staging volume 全体が見えて security boundary 崩壊)。

**採用: behavioral probe** (daemon 起動時に実際に isolation semantics を確認):

```go
// 疑似コード
func behavioralSubpathProbe(ctx context.Context, engine EngineClient) error {
    // 1. 一時 volume 作成
    tempVolName := fmt.Sprintf("boid-probe-%d-%d", os.Getpid(), time.Now().UnixNano())
    if err := engine.VolumeCreate(ctx, tempVolName); err != nil {
        return fmt.Errorf("subpath probe: create temp volume: %w", err)
    }
    defer engine.VolumeRemove(ctx, tempVolName, true)

    // 2. daemon 側で volume に subpath A/B 両方に sentinel を書く
    //    (temporary helper container で volume を mount して echo)
    if err := seedVolume(ctx, engine, tempVolName, "a/sentinel-a", "A"); err != nil {
        return fmt.Errorf("subpath probe: seed sentinel A: %w", err)
    }
    if err := seedVolume(ctx, engine, tempVolName, "b/sentinel-b", "B"); err != nil {
        return fmt.Errorf("subpath probe: seed sentinel B: %w", err)
    }

    // 3. subpath=a で container を start、 内部から:
    //    (a) /probe/sentinel-a が読める (subpath A の中身が見える)
    //    (b) /probe/sentinel-b が「読めない」(subpath B は外)
    //    (c) /probe/.. で volume root に到達できない (subpath より外)
    result, err := runProbeContainer(ctx, engine, tempVolName, "a")
    if err != nil { return fmt.Errorf("subpath probe: run: %w", err) }

    if !result.CanReadSentinelA {
        return errors.New("subpath probe: sentinel A unreadable from subpath=a mount " +
            "(volume-subpath semantics broken, engine likely ignoring Subpath field)")
    }
    if result.CanReadSentinelB {
        return errors.New("subpath probe: sentinel B (other subpath) readable — " +
            "isolation broken, engine likely mounting whole volume instead of subpath")
    }
    return nil
}
```

**全 failure path (通信 fail / timeout / create fail / start fail / semantic mismatch) を fail-hard で
daemon abort**。 診断メッセージで原因区分は log level で分ける。

version probe は補助的な pre-flight (info log) として残す (「engine が v1.45 未満なら behavioral probe
も skip して即 fail-hard」の shortcut) が、 **security 判定の source of truth は behavioral probe**。

### 実装詳細

1. `boid_staging` を compose.yml で declare、 daemon container に `/home/boid/staging` として mount (rw)
2. daemon 起動時に behavioral subpath probe (上記)
3. daemon が job を dispatch する際:
   - `boid_staging/spec/<jobID>/runner-spec.json` (0644、 uid 1000 owner)
   - `boid_staging/tls/<jobID>/{cert,key,ca}.pem` (0600)
   - `boid_staging/checkouts/<jobID>/<project>/` (§bare cache 経路)
   - sibling job container 生成時、 `--mount type=volume,src=boid_staging,dst=/run/boid/spec,volume-subpath=spec/<jobID>`
   - 同様に `tls/`、 `broker-tls/`、 `checkouts/` を必要 dst に mount
4. job 終了時 daemon が subpath dir を削除

### staging volume の安全境界

- **DB / secret.key / CA private key / web_secret は絶対 staging に書かない** (invariant)
- job container 側から staging の他 jobID subpath への read はできない (behavioral probe で担保)
- staging volume 内 file permission は daemon 側で厳密に (spec 0644、 TLS material 0600)

### volume ownership 初期化 (round 3 Major 9 対応)

uid 1000 の daemon 自身は **root-owned volume を chown 不可**。 対応:

- compose.yml に **init container (uid 0)** を追加、 daemon service 起動前に 4 volume mountpoint を
  `chown -R 1000:1000` で初期化
- 対象 volume: `boid_private` / `boid_staging` / `boid_repos` / `boid_homes_*` すべて (staging だけ
  じゃなく)
- BOID_UID/BOID_GID は **固定 1000** にする (現行の可変 build-arg は volume-only では有害、
  named volume の uid が build-arg 依存だと deploy 間で差が出る)

`compose.yml` の init container 例:

```yaml
services:
  init:
    image: boid-runner:latest
    entrypoint: ["sh", "-c"]
    command: ["chown -R 1000:1000 /home/boid/{,.local,.config,staging,repos,homes}"]
    volumes:
      - boid_private:/home/boid/.local/share/boid
      - boid_private_config:/home/boid/.config/boid
      - boid_staging:/home/boid/staging
      - boid_repos:/home/boid/repos
      # workspace homes は per-workspace init (workspace apply の phase 3 で対応)
    user: "0:0"
  daemon:
    depends_on:
      init:
        condition: service_completed_successfully
    # ... (以下現行)
```

### bare repo の job 提供 (Blocker 2 対応、 round 3 Major 2 反映)

**mirror clone 採用**:

```bash
git clone --mirror <url> <cache_path>              # 初回
git -C <cache_path> remote update --prune          # 以降 refetch
```

**round 3 Major 2 対応**:
- **mirror の `remote update` は symbolic HEAD 更新を保証しない** — upstream の default branch 変更に
  合わせて local `HEAD` を明示的に fetch: `git -C <cache> remote set-head origin --auto` を追加
- **fetch writer の直列化**: fetch も **exclusive lock** (shared だと複数 fetch writer が同時進入)、
  job dispatch (checkout の base 参照) は **shared lock** に限定 (fetch と分離)
- **cache から resolved SHA を materialize**: job dispatch 時に fetch 直後の `git rev-parse <branch>` で
  commit SHA を確定、 job 用 clone は `git clone --reference <cache> --branch <sha> <upstream_url>` の
  代わりに **cache から SHA fetch → detached HEAD checkout** (cache と job の間の upstream 更新で
  desync を避ける)

fetch/dispatch のロック契約:

| operation | lock mode |
|---|---|
| `git remote update` (fetch) | exclusive (writer 直列化) |
| `git rev-parse <branch>` + `git clone --reference` (job dispatch 用) | shared |
| `git repack -ad` / `git gc --prune=now` | exclusive |
| `boid project rm` / `mv` | exclusive |

**注**: fetch 中は他 job dispatch を待たせる。 fetch の頻度を減らすため:
- `boid project fetch` は明示 CLI (auto fetch は on-dispatch 1 回だけ、 batching)
- workspace apply 内の fetch は phase 3 の一括処理 (workspace 内 project 全部を 1 exclusive lock で)

### `--reference` + `git repack -ad` の再検討 (round 3 Major 2)

codex round 3 指摘: 「`git repack -ad` は objects をコピーしても alternates file を削除しない、 また
直後に checkout 全体を削除するなら dissociate/repack 自体が不要」

**訂正: `git repack -ad` は撤回**、 job 終了時は **checkout dir を単に削除**する:

```bash
# job 開始時
git clone --reference <bare_cache> <upstream_url> <checkouts/<jobID>/<project>/>
git -C <checkouts/<jobID>/<project>/> checkout <resolved_sha>

# job 実行中: alternates を維持 (cache と objects DB 共有、 disk 節約)

# job 終了時: checkout dir を単に削除
# (alternates は削除自動、 独立化は不要 — cache が生きてる限り borrowed objects は cache 側で保持)
rm -rf <checkouts/<jobID>/<project>/>
```

cache 側 `git gc --prune=now` は job dispatch を待つ exclusive lock (job 終了まで待つ)、 borrowed objects
の GC race を防ぐ。 これで dissociate/repack 不要。

### job dispatch フロー

1. daemon が bare repo の **exclusive lock** を取得 (fetch のため)
2. `git remote update --prune` (up-to-date 化)
3. `git rev-parse <branch>` で commit SHA を resolve
4. lock を **shared lock** に downgrade (fetch 終了、 dispatch 用 clone を許可)
5. staging volume 内 `boid_staging/checkouts/<jobID>/<project>/` を用意
6. `git clone --reference <bare_cache> <upstream_url> <checkouts/...>` + `git checkout <resolved_sha>`
7. job container start (staging volume の subpath mount)
8. job 終了時、 checkout dir 削除 + shared lock release

**reopen 意味論**: 現行の「push 済みのみ保証」を維持、 reopen 時は checkout dir を消して再 clone。

### project name / URL の validation

- **slug**: `^[a-z0-9][a-z0-9._-]*$`
- **workspace 内 一意性**: 上記 §a の直列化契約 (`BEGIN IMMEDIATE` + workspace mutex)
- **URL canonicalization**: `NormalizeOriginURL`
- **workspace reassignment**: `boid project mv <id> --workspace=<new>` — bare repo dir を atomic rename、
  exclusive lock で fetch/dispatch を待つ
- **rename**: 同様
- **rm cleanup**: bare repo dir 削除 + staging 内 in-flight checkouts の cancel + DB row 削除

### 未解決論点

- **branch policy** との整合 (別途)
- **fetch depth** (mirror full vs partial)
- **staging volume の disk 圧迫 / GC 統合** (現行 startup reap との統合)
- **podman VolumeOptions.Subpath の empirical 挙動確認** (behavioral probe が pin する形になった)

---

## 論点 c: CLI 到達経路 (HTTPS + Bearer + TLS trust bootstrap) (round 3 Blocker 1 対応で TLS 経路完成)

### 契約の再確認

Phase 3 で確立した契約 ([cli-remote-connection.md] 決定 4/8) を維持:

- **transport**: URL scheme で `unix://` (現行 local) / `https://` (TCP + TLS + Bearer)
- **auth**: `Authorization: Bearer <device-token>` (`~/.config/boid/tokens/<profile>.json`)
- **profile**: `~/.config/boid/config.yaml` の `profiles:` map

### volume-only cutover 時の変更

- **local 経路の unix socket 消える** (daemon socket は volume 内、 host から見えない)
- **代わりに `https://localhost:<port>`** (daemon container が localhost publish、 CLI は Bearer + pinned CA cert)
- **remote 経路**: 変更なし

全 CLI が **HTTPS + Bearer + pinned CA** 一択 (unix socket 消滅)。

### TLS trust bootstrap (round 3 Blocker 1 対応、 完成版)

**現状分析** (静的読解による、 codex round 3 で全 verify 済み):

- `internal/server/server.go:718,733`: 現行公開 API listener は plain HTTP (`net.Listen` 後 `Serve`、
  `TLSConfig` 無し)
- `internal/client/client.go:200-220`: production HTTPS client は `httpClient.Transport` に system trust
  store 固定
- `internal/mtls/ca.go:228` (`IssueServerCert`): TLS handshake で送る chain は **leaf 1 枚だけ** (CA cert 含まず)
- `ca.go:50` (`leafValidity`): leaf は 30 日固定、 daemon restart まで更新されない
- profile schema (`internal/profiles/token.go:16, resolve.go:33, root.go:219`): `Token → ResolvedProfile → NewClient`
  は URL/token しか運ばない

**round 2 の draft の「`boid ca fingerprint` (digest だけ) + `--ca-fingerprint`」では不足**:
- fingerprint だけでは CLI が pinned CA を root pool に追加できない (verify に CA cert 本体が必要)
- pin を transport へ渡す経路も未定義

**採用: CA cert 本体を OOB で運ぶ + fingerprint pin で verify**:

#### 1. CA cert export CLI (新規)

```
boid ca export -o ca.pem       # CA cert PEM を stdout or -o file
boid ca fingerprint             # CA cert の SHA-256 hex (short-form)
```

`boid ca export` は volume 内 `<data_dir>/tls/ca.crt` を PEM で出力 (daemon-internal endpoint、 認証不要
の read-only、 loopback 経由 or `docker exec` 経由の 2 通り)。

#### 2. `boid login` の CA pin 経路

```bash
# 方式 A: CA cert file 経由 (推奨)
docker exec container_daemon_1 boid ca export > /tmp/boid-ca.pem
boid login https://localhost:8443 --ca-cert /tmp/boid-ca.pem --profile local
# → login flow で CA cert を verify (self-signed / SAN 一致 / EKU / validity)
# → profile の tokens/local.json に CA cert PEM を保存

# 方式 B: fingerprint 経由 (shortcut、 CA cert を後で fetch)
FP=$(docker exec container_daemon_1 boid ca fingerprint)
boid login https://localhost:8443 --ca-fingerprint "$FP" --profile local
# → login flow で TCP 接続 → server から chain を取得 → chain 内 CA cert の fingerprint 一致確認 →
#    以降 CA cert を profile に保存
```

**必須**: `--ca-cert` or `--ca-fingerprint` のいずれか (TOFU / 素の HTTPS trust は disable、 fail-closed)。

#### 3. Profile schema 拡張

`~/.config/boid/tokens/<profile>.json`:

```json
{
  "token": "...",
  "ca_cert": "-----BEGIN CERTIFICATE-----\n...\n-----END CERTIFICATE-----\n",
  "ca_fingerprint": "SHA256:abcdef...",
  "url": "https://localhost:8443"
}
```

`ca_cert` は PEM 全体、 `ca_fingerprint` は derive fields (redundant だが CI 等の verify に便利)。

#### 4. HTTPS transport への pinned CA 経路

`internal/profiles/resolve.go` の `ResolvedProfile` に `CACert *x509.CertPool` を追加、
`newHTTPSClient` (`client.go:207`) の `transport` parameter に custom pool を渡す:

```go
// 疑似コード
func NewClient(profile *ResolvedProfile) (*Client, error) {
    if profile.URL.Scheme == "https" {
        var caPool *x509.CertPool
        if profile.CACert != nil {
            caPool = x509.NewCertPool()
            caPool.AddCert(profile.CACert)
        }
        tlsConfig := &tls.Config{
            RootCAs:    caPool,           // pinned CA (nil なら system default だが、 volume-only は pinned 必須)
            ServerName: profile.URL.Hostname(),  // SAN verify
        }
        transport := &http.Transport{
            TLSClientConfig: tlsConfig,
            // ... (現行 dial 設定)
        }
        return newHTTPSClient(profile.URL, profile.Token, transport)
    }
    // ...
}
```

**注**: profile に `CACert` 無い場合 (旧 profile from Phase 3) は system trust store fallback + warning
log (backward compat)、 volume-only は必須。

#### 5. 公開 listener は `ServerOnlyTLSConfig` (mTLS じゃない)

`internal/mtls/ca.go` に `ServerOnlyTLSConfig` (client cert 検証しない、 server auth のみ) が既にあるはず
(round 3 で codex 言及、 grep で未確認だが gateway CA 経路で存在すると思われる、 実装 PR で verify)。
公開 API listener はこれを使う (`ServerTLSConfig` の mTLS は internal broker/gateway 用に限定)。

listener 実装:

```go
// server.go の tcpServer 初期化
serverCert, err := ca.IssueServerCert(config.Web.PublicURL, "localhost", "127.0.0.1", "::1")
if err != nil { return fmt.Errorf("issue public API cert: %w", err) }

tlsConfig, err := ca.ServerOnlyTLSConfig(serverCert)
if err != nil { return fmt.Errorf("build server TLS config: %w", err) }

s.tcpServer = &http.Server{
    Handler:   tcpHandler,
    TLSConfig: tlsConfig,
}
go func() { _ = s.tcpServer.ServeTLS(tcpLn, "", "") }()   // cert/key は TLSConfig に埋め込み済み
```

#### 6. SAN の決定

- `config.web.public_url` が設定されてれば host 部分を SAN に追加
- `localhost` / `127.0.0.1` / `::1` は default で SAN に含める (compose deploy の port publish 経由)
- IP literal 対応: `x509.Certificate.IPAddresses` field 経由 (`IssueServerCert` で ip.ParseIP で分岐)

#### 7. Leaf 更新契約 (round 3 Blocker 1 対応の未解決を fix)

現行 `leafValidity = 30 * 24 * time.Hour` (`ca.go:50`)、 daemon restart まで更新されない。 volume-only では:

- **daemon startup で毎回 IssueServerCert を呼ぶ** (現行既に fresh issue、 30 日 window 内で restart
  すれば実質更新される)
- **health check goroutine** で残存 validity 期間を監視、 7 日以内なら warning log (dogfood 期間の
  daemon restart 頻度が低くて validity 切れる可能性を検出)
- 実装 PR-4 で「daemon が uptime 30 日超えたら自動 restart or 明示 restart 要求」を検討 (別 doc)

#### 8. CA 再生成時 (break-glass)

CA 消失/再生成 → CA cert fingerprint 変更 → **全 device で再 pair 要** (`boid login` やり直し)。
これは §論点 d の break-glass procedure の一部。

### 初回 pair の UX (完成版)

```bash
# 1. compose stack を上げる
./scripts/deploy-container.sh

# 2. daemon container 内で CA cert + pair code を取得
docker exec container_daemon_1 boid ca export > /tmp/boid-ca.pem
docker exec container_daemon_1 boid web pair
# → 表示: pair code XXXXX-YYYYY (5 分有効)

# 3. host CLI で redeem
boid login https://localhost:8443 --ca-cert /tmp/boid-ca.pem --profile local
# → pair code prompt → device token 交換 → tokens/local.json 書き込み (token + ca_cert + url)
```

### bootstrap 経路の loopback exemption

現行 `api_middleware.go:31-35, 76-87` の loopback bootstrap exemption は「device 0 件 + 真の loopback」の
間だけ Web UI を通す仕組み。 volume-only では:

- 初回起動 (device 0 件) の Web UI アクセスは loopback exemption で通る
- CLI 側は初回 pair 前は使えない (Bearer + CA pin 必須) → `docker exec` 経由

### profile 名の derive (Minor 3 対応)

`login.go:377` の `deriveProfileNameFromURL`: URL host の first dot-separated label を lowercase で derive。
`localhost` (dot 無し) → whole host = `localhost`。 例:

```bash
boid login https://localhost:8443 --ca-cert /tmp/boid-ca.pem --profile local  # 明示 (推奨)
boid login https://localhost:8443 --ca-cert /tmp/boid-ca.pem                    # → profile 名は "localhost"
```

### port 選定

現行 Web UI が `:8080` に listen。 CLI Bearer 経路も同じ port に相乗り (Bearer middleware 既に共存)。
volume-only で TLS 化するので port 名は明示化:

- **compose publish**: `8443:8080` (host 側 8443 = HTTPS pinned CA、 container 内 8080 = listener)
- example の URL は `https://localhost:8443`

### `boid start` の意味論 (§論点 e に集約)

### auto-start trigger 経路の削除

現行 auto-start ([[stale-boid-daemon-recurring]]) は volume-only では削除 (daemon は compose 外部管理、
CLI から start できない → fail-fast)。

### 未解決論点

- **TLS listener の HTTP/2 有効化** (別 PR)
- **initwizard は project.yaml scaffold** (`wizard.go:27`)、 connection wizard じゃない (round 2 Minor 1 訂正)。
  「initwizard の compose deploy 対応」は本 doc scope 外、 別 doc

---

## 論点 d: secret ライフサイクル (on-first-boot generate + break-glass procedure)

### 対象

- `secret.key` (**SecretStore の AES-256 master key**、 `secret_keyfile.go:9` の `LoadOrCreateKey` 既存)
- `web_secret` (Web session cookie signing key、 既存 load-or-create)
- daemon internal CA (`tls/ca.crt` + `tls/ca.key` の **2 file**、 `mtls/ca.go:65` の `LoadOrCreate` 既存)
- `install_id` (既存 on-first-boot generate、 atomic write PR #822)

**追加改修は不要** (round 1 Major 3 対応で確認済み)、 volume 内で既存経路がそのまま動く。

### rotation の non-goal 化 (Major 3 対応、 継続)

secret rotation は本 doc scope 外。 現状 rotation 未提供 の帰結:
- `secret.key` の rotate は SecretStore 全件の decrypt/re-encrypt 必要
- `web_secret` の rotate は cookie 一斉 invalidation
- CA の rotate は leaf 再発行 + 旧 CA revoke/transition

`boid web pair/revoke` は device credential で secret rotation とは別問題。

### break-glass procedure (round 2 Major 7 対応、 round 3 Major 8 で修正)

**問題** (事実):
- `secret.key` 単独削除しても SQLite の `secrets` テーブル encrypted rows は残る (新 key で decrypt error)
- `web_secret` 単独削除は cookie 無効化するが Bearer device token row (`web_devices`) は失効しない
- CA 単独削除は token revoke と同義でない

**dogfood 期間中の緊急対処用 break-glass procedure** (round 3 Major 8: podman/docker 差 + compose service 経由に修正):

```bash
# 1. compose stack down (podman-compose fallback / docker compose 双方対応)
./scripts/deploy-container.sh --down       # deploy script に新規 flag、 内部で engine 検出

# 2. DB backup (crash 時 restore 用)
#    compose service 経由で daemon image を run、 volume 直接 access は避ける
docker compose -f build/container/compose.yml run --rm --user root daemon \
    sh -c 'cp /home/boid/.local/share/boid/boid.db /home/boid/.local/share/boid/boid.db.backup-$(date +%Y%m%d-%H%M%S)'

# 3. DB の secrets + web_devices + web_pairing_codes を purge
#    (round 3 Major 8: web_pairing_codes も未消費 code purge 対象に含める)
docker compose -f build/container/compose.yml run --rm --user root daemon \
    sh -c 'sqlite3 /home/boid/.local/share/boid/boid.db \
        "DELETE FROM secrets; DELETE FROM web_devices; DELETE FROM web_pairing_codes;"'

# 4. secret material file を削除 (daemon が起動時に fresh generate)
docker compose -f build/container/compose.yml run --rm --user root daemon \
    sh -c 'rm -f /home/boid/.local/share/boid/secret.key \
                  /home/boid/.local/share/boid/web_secret \
                  /home/boid/.local/share/boid/tls/ca.crt \
                  /home/boid/.local/share/boid/tls/ca.key'

# 5. daemon 再起動 (secret material + CA fresh generate)
./scripts/deploy-container.sh

# 6. 新 CA fingerprint + pair code 取得 → CLI で再 pair
docker compose -f build/container/compose.yml exec daemon boid ca export > /tmp/boid-ca.pem
docker compose -f build/container/compose.yml exec daemon boid web pair
boid login https://localhost:8443 --ca-cert /tmp/boid-ca.pem --profile local

# 7. 上位 SecretStore 消費者 (workspace secrets 等) の re-provision
#    → gateway.forges.<forge>.secret_key で参照している env var を再登録
```

**副作用**:
- SecretStore に保存の API token / OAuth refresh token 等は消失
- 全 device pair 失効
- 進行中 job は abort (CA 変更で内部 mTLS 切れる)
- CA fingerprint 変わる → 全 client の profile を再 pair (`boid login` やり直し)

**注意** (round 3 Major 8):
- `boid_private` volume は Compose project 名で prefix される (通常 `<project>_boid_private`)、
  `docker run -v boid_private:/data` は別 volume を新規作成する可能性あり
- 対策: **compose service 経由で run** (`docker compose ... run --rm --user root daemon`)、 project scope 継承
- container 名 `container_daemon_1` は Compose v2 / project 名 / podman で不安定、
  service 名 (`daemon`) で参照

### migration

現行 host daemon で generate 済みの secret material は volume-only cutover 時に破棄 (新規 generate)。

**副作用の因果分離** (round 1 Minor 4 対応):
- device pair 全失効の原因: **fresh DB により `web_devices` rows が無くなるため**
- session cookie 全部無効の原因: `web_secret` 消失により cookie signature verify fail
- SecretStore 内 secret 全部破棄の原因: `secret.key` 消失 → 新 key では encrypted rows decrypt 不可
- 内部 mTLS cert 全部再発行の原因: CA 消失 → 新 CA で fresh cert 発行
- git-gateway cert scoping / reap label filter の変化: `install_id` 変わる

### 未解決論点

- **k8s Secret provider 経路の initContainer 契約**
- **atomic write-safety (multi-file 一貫化)**: round 3 Major 5 の指摘に注意、 §論点 i の
  `SecretProvider.GetOrCreateMulti` で解決 (sibling temp directory を完成・fsync → directory 単位で publish、
  or generation directory + `current` symlink pointer で atomic swap)

---

## 論点 e: 現行 host daemon 経路廃止 (Major 1, 2 対応、 PR 分割 additive/inert)

### 現状

- `cmd/start.go` の `runDaemonParent` (bare `boid start` の double-fork)
- `internal/dispatcher/userns_backend.go`
- `internal/sandbox/runner/runner_linux.go`
- `sandbox.backend` config option

### 新方式: volume-only cutover と同時に一気撤去 (段階撤去のメリット消失、 rollback 契約撤回)

### PR 分割案 (round 2/3 修正、 additive/inert、 round 3 Major 3 で URL 経路を PR-3 に統合)

- **PR-1: seam interface 導入 (additive)**
  - `RepositoryCache` / `SecretProvider` / `JobMountDescriptor` interface (§論点 i)
  - `VolumeSecretProvider` / `VolumeRepositoryCache` (thin wrapper) / `VolumeJobMountDescriptor` 実装
  - 既存 code は wrapper 経由に refactor (behavior 不変)
  - unused code 状態にしない (implementer + consumer 揃った状態で landed)
- **PR-2: volume-only compose stack + staging consumer + volume ownership init 同時導入 (feature-gated)**
  - `compose.volume-only.yml` を新規追加、 init container (root で chown) 含む
  - `deploy-container.sh` に `--mode=volume-only` flag + `--down` flag (break-glass 用)
  - `container_backend.go` に staging volume 経由の subpath mount 実装 (behavioral probe + fail-hard)
  - CI に `e2e-container-volume-only` job (`continue-on-error: true` advisory)
  - 現行動作維持 (default = bind mount mode)
  - **前提 gate**: engine socket abstraction PR (別 doc の podman socket follow-up) が先に landed
- **PR-3: URL-aware startup + auto-prune 撤去 + additive migration + repo cache 実装** (round 3 Major 3 対応で PR-5 の cache を PR-3 に前倒し)
  - `wire.go:226-249` の hard delete 撤去、 `status='degraded'` 遷移に置換
  - additive migration (`projects.project_name` / `status` / `status_reason` / `revision` + `apply_journal`)
  - `RepositoryCache` の mirror clone 経路実装 (`git clone --mirror` + fetch exclusive lock + `--reference`)
  - `boid project add <git-url>` 新経路追加、 現行 `boid project add <dir>` は deprecation warning
  - **URL 経路と cache 実装が同 PR** で公開 (round 2 draft の split-brain 解消)
- **PR-4: CLI Bearer 経路 + TLS bootstrap + config CLI/Web UI**
  - `boid ca export` / `boid ca fingerprint` CLI 追加
  - `boid login --ca-cert` / `--ca-fingerprint` 実装
  - daemon 公開 API listener を TLS 化 (`ServerOnlyTLSConfig`)
  - `boid config edit/get/set/apply` 実装
  - Web UI `/settings` page
  - reload API 実装 (5 段 sub-PR: PR-4.a〜PR-4.e、 §f 参照)
  - 現行動作維持 (`default_profile` 未 seed 時は unix socket fallback 継続)
- **PR-5: workspace export/import bundle** (feature-gated)
  - `boid workspace export/apply` 実装、 tar.gz bundle + init.sh 独立 file 対応
  - apply の phase 4 段 + `apply_journal` の recovery (§g 参照)
  - 現行動作維持
- **PR-6: cutover (default 切替 + unix fallback + host daemon + userns 一斉撤去)**
  - `deploy-container.sh` の default を volume-only に切替
  - unix socket fallback を CLI から撤去 (auto-start trigger 経路も同時撤去)
  - `cmd/start.go` の `runDaemonParent` 削除、 `boid start` は「compose stack 起動」の thin wrapper に
  - `internal/dispatcher/userns_backend.go` + `LocalRuntime` + `SandboxPreparer` 削除
  - `internal/sandbox/runner/runner_linux.go` + `internal/sandbox/plan.go` 削除
  - `sandbox.backend` config option 撤去 (container 一択)
  - `boid project add <dir>` 経路完全削除
  - userns 固有 e2e scenario 削除
  - CI の `e2e-container-volume-only` を `continue-on-error: false` に格上げ

### 各 PR landed 時点の main 可動性

- **PR-1**: 現行動作不変、 seam wrapper 追加
- **PR-2**: 現行動作維持 + volume-only mode を明示的に選ぶと動く
- **PR-3**: **auto-prune 撤去が default で発火**、 URL 経路 + cache 実装が同 PR で完結 (split-brain 消失)
- **PR-4**: 現行動作維持 + Bearer + TLS bootstrap opt-in
- **PR-5**: 現行動作維持 + workspace bundle 使える
- **PR-6**: default が volume-only、 一斉撤去、 単一 mode 化

### 未解決論点

- 既存 userns 経路の e2e coverage の container 経路移植完了状況
- `boid start` の CLI wrap 化

---

## 論点 f: config.yaml 編集経路 (CLI + Web UI) (Major 2, 3 対応、 round 3 Major 4 で interface 修正)

### CLI API

```bash
boid config get                             # 全 config を YAML で stdout
boid config apply -f config.yaml            # file から apply (validation + reload)
boid config get sandbox.allowed_domains     # dotted path
boid config set sandbox.allowed_domains ".freee.co.jp" ".notion.com"
boid config unset gateway.forges.github.secret_key
boid config edit                            # $EDITOR で開く
```

CLI subcommand は **authenticated HTTP API** (Bearer 経由) で daemon に到達 (round 2 Minor 4 対応、
「broker RPC」ではない)。

### Web UI

`/settings` page で `sandbox.allowed_domains` / `gateway.forges.*` / `notify.command` / `web.public_url`。
YAML raw edit (monaco editor)。 `default_harness` は Phase 2.5 PR7 で撤去済みなので UI/CLI に入れない。

### validation

schema validation を保存前に実施、 error は位置 + 理由付き。

### reload semantics (round 3 Major 4 対応で ConfigSubscriber interface を Prepare/Commit/Rollback 化)

**round 2 draft の `OnConfigReload` だけの interface は撤回**。 codex round 3 指摘: dry-run/pre-check/rollback
を表現できない、 「順次 or 並列」のままでは reverse-order rollback 定義不可、 複数 subsystem lock 保持で
callback 呼ぶと deadlock 余地。

**改訂 interface**:

```go
type ConfigSubscriber interface {
    // Prepare: 新 config で apply 可能かを検証 (dry-run、 副作用なし)
    // 失敗すると reload 全体 abort
    Prepare(ctx context.Context, oldCfg, newCfg *config.Config) error

    // Commit: 新 config を実際に適用 (副作用あり)
    // 全 subscriber の Prepare 成功後に、 各 subscriber の Commit が順次呼ばれる
    Commit(ctx context.Context, newCfg *config.Config) error

    // Rollback: Commit 済み subscriber の一つが後の subscriber Commit で失敗した場合、
    // reverse order で呼ばれ、 旧 config へ戻す
    Rollback(ctx context.Context, oldCfg *config.Config) error
}
```

**Reload flow**:

1. reload API 受信 → 新 config を parse + validate
2. subscriber の `Prepare` を **順次** 呼ぶ (lock 保持しない、 各 subscriber は自分の内部 lock を独立に扱う)
3. Prepare が全 subscriber で成功 → subscriber の `Commit` を **順次** 呼ぶ
4. Commit が i 番目で失敗 → 0..i-1 番目の subscriber に `Rollback(oldCfg)` を **reverse order** で呼ぶ
5. Commit 全成功 → file に atomic write (temp + rename) + `revision` counter increment + in-memory swap
   (immutable snapshot pointer の atomic replace)

**deadlock 回避**:
- callback は subscriber 自身の lock 内で呼ばない (subscriber は自分の lock を Prepare/Commit/Rollback の
  中でだけ取得)
- daemon 側は subscriber slice の iteration のみ (subscriber lock 触らない)
- immutable snapshot pointer (`*atomic.Pointer[Config]`) で consumer は lock-free に current config 参照

**persisted desired config と active config の分離** (round 3 Major 4 対応):
- restart-required key を保存: 変更は file に書くが in-memory active config は変わらない
- consumer (subscriber) は active config を参照、 restart 後に file から新値を load
- `boid config get --active` (active、 現行動作) / `boid config get --desired` (file に書いてある値) で区別

### restart-required key の catalog

| category | keys | 反映タイミング |
|---|---|---|
| **dynamic** | `sandbox.allowed_domains`, `notify.command`, `web.public_url` | reload 即時 |
| **restart-required** | `gateway.forges.*`, `web.http_addr`, `gc.*`, `task_ask.*` | 保存 → next restart、 保存時 warning |
| **removed on volume-only** | `sandbox.backend` | 保存拒否 |

### 実装コストの再評価

§e の PR-4 内部で 5 段 sub-PR:
- **PR-4.a**: config in-memory + reload API + revision counter + atomic pointer swap
- **PR-4.b**: YAML AST preservation + atomic write + `ConfigSubscriber` (Prepare/Commit/Rollback)
- **PR-4.c**: `boid config` CLI サブコマンド
- **PR-4.d**: `/settings` Web UI page
- **PR-4.e**: client/daemon config file split

### 未解決論点

- **dotted path 構文** (array/map の扱い)
- **secret env var 値の編集経路** (CLI/Web UI scope 外)

---

## 論点 g: workspace/project export/import shape (Major 4 対応、 round 3 Blocker 3 で apply_journal 完成)

### bundle 形式

**tar.gz bundle** (`workspaces.tar.gz`)、 中身:

```
workspaces.yaml            # 全 workspace + HostCommands (--- 区切り)
workspaces/
    default/
        init.sh
    bm-next/
        init.sh
    ...
```

apply:
- `boid workspace apply -f workspaces.tar.gz` (展開 + 全部 apply)
- `boid workspace apply -f workspaces/` (directory 直指定)
- `boid workspace apply -f workspace.yaml` (単一 YAML、 `--init-script <path>` で init.sh 別指定)

### YAML shape

```yaml
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: default
  revision: 42
spec:
  container_image: boid-runner:latest
  host_commands: [atl, gh]
  env: {ATL_SITE: ubs, DOTNET_CLI_TELEMETRY_OPTOUT: "1"}
  allowed_domains: []
  extra_repos:
    - https://github.com/some/private-go-mod.git
  capabilities: {docker: {}}
  init_script: workspaces/default/init.sh
  projects:
    - name: rook-server
      url: git@bitbucket.org:Aolani-ondemand/rook-server.git
    - name: mera-ui
      url: git@bitbucket.org:Aolani-ondemand/mera-ui.git

---

apiVersion: boid.dev/v1
kind: HostCommands
metadata:
  name: default
  revision: 15
spec:
  commands:
    - name: atl
      path: /usr/local/bin/atl
    - name: gh
      path: /usr/local/bin/gh
```

### `additional_bindings` の扱い

Phase 4 で退役、 shape に含めない (round 2 Minor 2 対応)。

### env の host path 依存

現行 workspace の `env` の host path は container 内で invalid、 kits 経由 (image layer) に移す。 export 時
warning。

### CLI API + apply 契約

```bash
boid workspace export <name> -o workspace.tar.gz
boid workspace export --all -o all-workspaces.tar.gz
boid workspace export <name> --format=yaml -o workspace.yaml
boid workspace apply -f workspaces.tar.gz
boid workspace apply -f workspaces/
boid workspace apply --dry-run -f workspaces.tar.gz
boid workspace apply --dry-run --check-remotes -f ...
boid workspace apply --prune -f workspaces.tar.gz
boid workspace apply --force -f ...
boid workspace delete <name>
```

### apply の transaction + recovery (round 3 Blocker 3 対応、 apply_journal 完成版)

**apply_journal schema** (round 3 Blocker 3 対応で拡張):

```sql
-- migration NNNN_apply_journal.sql
CREATE TABLE IF NOT EXISTS apply_journal (
    id                TEXT PRIMARY KEY,
    started_at        DATETIME NOT NULL,
    completed_at      DATETIME,                  -- NULL = in-progress or crashed
    phase             TEXT NOT NULL,             -- validate | db_upsert | filesystem | status_transition
    operation         TEXT NOT NULL,             -- clone_new | rename_old | rewrite_hostcommands | write_initsh | delete_repo | fetch | rename_repo
    target_kind       TEXT NOT NULL,             -- workspace | project | hostcommands
    target_name       TEXT NOT NULL,             -- workspace slug or project name
    desired_revision  INTEGER,                   -- 変更後の revision
    target_url        TEXT,                      -- URL 変更/clone_new 用
    temp_path         TEXT,                      -- filesystem op の temp destination (rename 前)
    target_path       TEXT,                      -- filesystem op の final destination
    filesystem_step   TEXT NOT NULL DEFAULT '{}',  -- 完了済み step の JSON checkpoint
    error             TEXT
);
CREATE INDEX IF NOT EXISTS idx_apply_journal_incomplete
    ON apply_journal(completed_at) WHERE completed_at IS NULL;
```

**apply の phase 4 段** (round 3 で phase 3 を fsync + rename に細分):

1. **phase 1: validate** — bundle schema validation、 一意性 check、 HostCommands conflict check。 副作用なし
2. **phase 2: DB upsert (SQL transaction)** — workspaces / projects の upsert、 status='apply-pending' で
   record 登録、 revision counter check、 apply_journal に「開始」記録
3. **phase 3: filesystem operations (各 op が temp + fsync + atomic rename、 journal に step 記録)**:
   - **clone_new** (新規 project): `temp_path` (`<repo_cache>/<workspace>/.<project>.git.new`) に clone →
     fsync → `target_path` (`<repo_cache>/<workspace>/<project>.git`) に rename → journal step 更新
   - **rename_old** (URL 変更、 reclone mode): 既存 `target_path` を `temp_path` (`.<project>.git.old`) に
     rename → journal step 更新 (以降 clone_new と組み合わせ)
   - **rewrite_hostcommands**: 新 host_commands.yaml を `.new` に write + fsync → 既存を `.old` に rename +
     `.new` を target に rename (2 段 atomic swap)
   - **write_initsh**: 新 init.sh を `.new` に write + fsync + chmod → 既存を `.old` に rename → `.new` を
     target に rename
   - 各 step 完了時に `apply_journal.filesystem_step` を update
4. **phase 4: status transition** — project の status を 'ready' に更新 + apply_journal.completed_at set

**restart 時の recovery** (round 3 Blocker 3 対応、 detection じゃなく recovery):

daemon 起動時、 `completed_at IS NULL` の apply_journal entry を探して operation 別に処理:

```go
// 疑似コード
for _, entry := range incompleteJournalEntries {
    switch entry.Operation {
    case "clone_new":
        // temp_path が存在すれば削除、 operation を最初から再実行 (idempotent)
        os.RemoveAll(entry.TempPath)
        retryClone(entry)

    case "rename_old":
        // target_path が空 && temp_path (.old) が存在 → revert rename
        if !fileExists(entry.TargetPath) && fileExists(entry.TempPath) {
            os.Rename(entry.TempPath, entry.TargetPath)  // 旧 repo 復活
        }
        markApplyError(entry, "rename_old crash: reverted, re-apply required")

    case "rewrite_hostcommands":
        // .old backup file が存在すれば restore
        if fileExists(entry.TempPath) {  // temp_path here = .old backup
            os.Rename(entry.TempPath, entry.TargetPath)  // restore
        }
        markApplyError(entry, "hostcommands rewrite crash: restored, re-apply required")

    case "write_initsh":
        // .new や .old の残骸を掃除
        os.RemoveAll(entry.TempPath)
        markApplyError(entry, "initsh write crash: re-apply required")
    }
    // journal entry を close (completed_at set)
    closeJournalEntry(entry.ID)
}
```

**HostCommands の workspace status** (round 3 Blocker 3 対応):

HostCommands は daemon-global (workspace scope でない)、 対応:
- apply_journal の `target_kind='hostcommands'` として管理
- daemon-global に「HostCommands apply status」を持つ (新規 table or config field)

### apply 9 点契約 (round 2 で解消済み、 round 3 で追加なし)

上記 recovery で partial state / crash / concurrent update / URL 変更 の failure mode を fail-loud で
可視化。 詳細は round 2 draft と同じ。

### migration path

現行 5 workspaces の env/additional_bindings は volume-only 化に伴い clean start で再構築。

### 未解決論点

- `ContainerImage` default 化
- branch policy の workspace override
- forge auth の workspace scope

---

## 論点 h: 移行手順 (新方式単発切替)

### big-bang cutover の意味 (rollback 用語整理、 round 1 Minor 3 対応)

**「rollback path 無し」**: 新 state を保った切戻しは無い (volume-only の named volume が書いた state を
旧 host daemon に引き継ぐ経路は無い)。

**disaster fallback**: PR revert + volume-only stack down + 旧 host daemon 再起動 + 6-27 backup restore で
「Phase 6 直前の状態に戻る」ことは可能。 fresh install 相当への切戻し。

### タイムライン

1. 本 doc レビュー完了 (round 3 → nose 承認 → landed)
2. **PR-1 (seam 導入)** — 現行動作不変
3. **PR-2 (volume-only compose + staging + volume ownership init、 feature-gated)** — 現行動作維持 + opt-in
4. **PR-3 (URL-aware startup + auto-prune 撤去 + additive migration + repo cache 実装)** — auto-prune 撤去が
   default で発火
5. **PR-4 (CLI Bearer + TLS bootstrap + config CLI/Web UI)** — 5 段 sub-PR
6. **PR-5 (workspace export/import bundle)** — 現行動作維持
7. **PR-6 (cutover: default 切替 + 一斉撤去)** — 単一 mode 化
8. **手動 cutover 実施** — nose の host:
   - 現行 host daemon 停止
   - `~/.config/boid/host_commands.yaml` / workspace init.sh を bundle 形式に手動整理
   - `./scripts/deploy-container.sh` で volume-only compose stack start
   - `docker compose ... exec daemon boid ca export > /tmp/boid-ca.pem`
   - `docker compose ... exec daemon boid web pair` で pair code 発行
   - `boid login https://localhost:8443 --ca-cert /tmp/boid-ca.pem --profile local`
   - `boid workspace apply -f workspaces.tar.gz` で initial import
   - `boid task list` で疎通確認

### Podman socket は cutover checklist で明示 (PR-2 前 gate、 round 2 Minor 5 対応)

**PR-2 実施前の必須 gate**: engine socket abstraction (podman rootless の
`/run/user/<uid>/podman/podman.sock` 対応) の follow-up PR を先に landed させる。 本 doc scope 外、
今日 revert した fix/phase6-deploy-podman-socket branch の考え方を新 stack に再適用。

---

## 論点 i: k8s 移行時の seam (Major 6 対応、 round 3 Major 6 で JobContext 拡張)

### 3 つの seam

#### 1. `SecretProvider` interface

```go
type SecretMaterial struct {
    Name      string
    Files     map[string][]byte            // 相対 filename → contents
    FileModes map[string]os.FileMode       // 相対 filename → mode
}

type SecretProvider interface {
    GetOrCreateFile(ctx context.Context, name string, mode os.FileMode,
                     generator func() ([]byte, error)) ([]byte, error)
    GetOrCreateMulti(ctx context.Context, name string,
                      generator func() (*SecretMaterial, error)) (*SecretMaterial, error)
    Validate(ctx context.Context, name string) error
}
```

**multi-file atomic write** (round 3 Major 5 対応、 os.Rename の順次は atomic じゃない):

```go
// 疑似コード for VolumeSecretProvider.GetOrCreateMulti
func (p *VolumeSecretProvider) GetOrCreateMulti(ctx context.Context, name string,
    generator func() (*SecretMaterial, error)) (*SecretMaterial, error) {

    finalDir := filepath.Join(p.baseDir, name)
    if material, err := loadMulti(finalDir); err == nil {
        return material, nil
    }

    material, err := generator()
    if err != nil { return nil, err }

    // 1. sibling temp directory に全 file を write + fsync
    tempDir, err := os.MkdirTemp(p.baseDir, "."+name+".tmp.")
    if err != nil { return nil, err }
    defer os.RemoveAll(tempDir)  // 途中失敗時の cleanup

    for filename, contents := range material.Files {
        path := filepath.Join(tempDir, filename)
        mode := material.FileModes[filename]
        if err := os.WriteFile(path, contents, mode); err != nil {
            return nil, fmt.Errorf("write %s: %w", filename, err)
        }
        f, err := os.Open(path)
        if err != nil { return nil, err }
        _ = f.Sync()
        f.Close()
    }

    // 2. temp dir 自体を fsync (directory metadata が disk に flush される)
    if d, err := os.Open(tempDir); err == nil {
        _ = d.Sync()
        d.Close()
    }

    // 3. directory 単位で atomic rename (POSIX rename は directory を atomic に swap)
    if err := os.Rename(tempDir, finalDir); err != nil {
        return nil, fmt.Errorf("publish %s: %w", name, err)
    }

    return material, nil
}
```

**注**: 既存 file を上書きする場合は `rename` が atomically 置換 (POSIX 保証)、 partial write が観測されない。

#### 2. `RepositoryCache` interface

```go
type LockMode int
const (
    LockShared LockMode = iota   // job dispatch 用 clone
    LockExclusive                 // fetch / rm / mv / gc
)

type ProjectRef struct {
    ID, Name, WorkspaceID, UpstreamURL, CachePath string
    Status, StatusReason string
    Revision int
}

type CheckoutHint struct {
    HostVisiblePath    string  // daemon 内 path (staging subpath)
    ContainerMountPath string  // job container 内 mount path (/workspace/<name>/)
}

type URLChangeMode int
const (
    URLChangeReclone URLChangeMode = iota  // default
    URLChangeFetch                          // in-place remote set-url + fetch
    URLChangeReject
)

type RepositoryCache interface {
    Register(ctx context.Context, workspace, projectName, url string) (*ProjectRef, error)
    Fetch(ctx context.Context, ref *ProjectRef) error
    ReadFile(ctx context.Context, ref *ProjectRef, path string) ([]byte, error)
    DefaultBranch(ctx context.Context, ref *ProjectRef) (string, error)
    RefLookup(ctx context.Context, ref *ProjectRef, refspec string) (commitSHA string, err error)
    Materialize(ctx context.Context, ref *ProjectRef, jobID, resolvedSHA string) (*CheckoutHint, error)
    CleanupCheckout(ctx context.Context, jobID string) error
    Lock(ctx context.Context, ref *ProjectRef, mode LockMode) (unlock func(), err error)
    ChangeURL(ctx context.Context, ref *ProjectRef, newURL string, mode URLChangeMode) error
    Remove(ctx context.Context, ref *ProjectRef) error
    List(ctx context.Context) ([]*ProjectRef, error)
}
```

#### 3. `JobMountDescriptor` interface (round 3 Major 6 対応で JobContext 拡張)

```go
type MountSpec struct {
    Source     string
    Target     string
    Subpath    string
    ReadOnly   bool
    InitSeed   *InitSeedSpec
}

type InitSeedSpec struct {
    // initContainer 相当で volume に seed する内容 (k8s 用)
    Files map[string][]byte
}

type JobContext struct {
    JobID           string
    WorkspaceID     string
    WorkspaceHomeDir string           // workspace HOME volume path (round 3 Major 6 追加)
    ProjectRef      *ProjectRef
    Checkout        *CheckoutHint
    RunnerSpec      []byte             // runner spec bytes (init seed 用、 round 3 追加)
    RunnerState     []byte             // runner state bytes (init seed 用、 round 3 追加)
    TLSMaterial     *SecretMaterial    // per-job cert
    BrokerTLS       *SecretMaterial
    UID             int                // 通常 1000 (round 3 追加、 keep-id 前提)
    GID             int
}

type JobMountDescriptor interface {
    DescribeMounts(ctx context.Context, jc *JobContext) ([]MountSpec, error)
    // cleanup は staging subpath 削除 (compose 経路)、 EmptyDir なら k8s Pod 削除で自動
    Cleanup(ctx context.Context, jobID string) error
}
```

**checkout cleanup の ownership** (round 3 Major 6 で指摘): `RepositoryCache.CleanupCheckout` と
`JobMountDescriptor.Cleanup` の両方が checkout dir を持ってる矛盾を解消:

- **`JobMountDescriptor.Cleanup` が staging 全 subpath (spec/tls/broker-tls/checkouts) を一括削除**
- `RepositoryCache.CleanupCheckout` は **廃止** (checkout は staging の一部として `JobMountDescriptor` が
  ownership)
- `RepositoryCache` は bare repo (cache) の ownership のみ

### k8s 1:1 mapping

| compose deploy | k8s deploy |
|---|---|
| `boid_private` named volume | PVC (RWO) — DB / CA private key、 node affinity or RWX + subPath isolation |
| `boid_staging` | PVC (RWX、 daemon Pod と job Pod 共有) or EmptyDir per-job (initContainer で seed) |
| `boid_repos` | PVC (RWX、 daemon fetches, job reads via subPath) or job init-job で bare clone を EmptyDir |
| `boid_homes_<workspace>` | PVC per workspace |
| secret material | K8s Secret を initContainer で volume に seed |
| CLI listener (HTTPS + Bearer + pinned CA) | Service (ClusterIP or Ingress)、 K8s Secret から CA fingerprint |
| workspace HOME | PVC per workspace |

### 未解決論点

- PVC accessMode 選定
- initContainer sequencing
- Service type

---

## 未解決の設計論点まとめ

### Blocker から昇格した設計決定 (本文で fix 済み)

- §a: auto-prune 撤去 + `degraded`/`apply-pending`/`apply-error` status (Blocker 4)、 additive migration
  (Blocker 2)、 `ProjectRef` + `RepositoryCache` 経由の `work_dir` 意味論変更 (Major 5)、
  一意性の直列化契約 (`BEGIN IMMEDIATE` + workspace mutex、 round 3 Major 1)
- §b: **behavioral subpath probe** (round 3 Blocker 2)、 volume ownership 初期化 (init container で root、
  round 3 Major 9)、 mirror clone + fetch/dispatch lock 分離 + resolved SHA materialize (round 3 Major 2)、
  bare cache は cleanup 時 checkout dir を単純削除 (repack 撤回、 round 3 Major 2)
- §c: **CA cert OOB 経路 + pinned CA cert** (round 3 Blocker 1)、 `boid ca export`/`fingerprint` CLI、
  profile schema 拡張 (`ca_cert` field)、 HTTPS transport への pinned CA 経路、 `ServerOnlyTLSConfig` 使用、
  SAN 決定 + leaf 更新 monitoring
- §d: break-glass procedure (`compose exec` 経由で resource 選択安定化、 web_pairing_codes purge 追加、
  round 3 Major 8)
- §e: PR 分割 additive/inert、 PR-3 で URL 経路 + cache 実装同居 (round 3 Major 3)
- §f: `ConfigSubscriber` interface (Prepare/Commit/Rollback)、 immutable snapshot pointer、 persisted
  desired config と active config 分離 (round 3 Major 4)
- §g: **apply_journal 完成版** (desired_revision/URL/temp/target/operation/filesystem_step、 operation 別
  restart recovery、 apply-pending status 追加、 round 3 Blocker 3)、 phase 3 の temp + fsync + rename、
  HostCommands の apply_journal 経由管理
- §i: `SecretProvider`/`RepositoryCache`/`JobMountDescriptor` interface 拡張、 multi-file atomic write
  (sibling temp dir + directory rename、 round 3 Major 5)、 `JobContext` 拡張 (WorkspaceHomeDir /
  RunnerSpec / RunnerState / UID / GID、 round 3 Major 6)、 checkout cleanup ownership 統一 (round 3 Major 6)

### 継続 未解決 (PR-1 着手前に individual に nose 判断)

- **論点 a**: project.yaml validate タイミング (本文で決定済み: 同期 validate、 round 3 Minor 4 対応で
  未解決から除去)、 fetch depth (mirror full vs partial)
- **論点 b**: branch policy との整合、 staging volume の disk 圧迫 / GC 統合、 podman VolumeOptions.Subpath
  の empirical 挙動 (behavioral probe で pin する形になった)
- **論点 c**: TLS listener の HTTP/2 (別 PR)、 initwizard の compose 対応 (別 doc、 round 3 Minor は
  Phase 7 領域)
- **論点 d**: k8s Secret provider の initContainer 契約
- **論点 e**: e2e coverage の container 経路移植、 `boid start` の CLI wrap 化
- **論点 f**: dotted path 構文、 secret env var 値の編集経路
- **論点 g**: `ContainerImage` default 化、 branch policy の workspace override、 forge auth の workspace scope
- **論点 i**: PVC accessMode 選定、 initContainer sequencing、 Service type (Phase 7 領域、 別 doc 扱い)

### Minor 未対応 (round 3 で残るもの、 実装 PR 時 or 別 PR で修正)

- 章内 cross-reference の typo (「§項目 c」→ workspace shared lock 節へ、 「PR-3.a〜PR-3.d」→ 「PR-4.a〜PR-4.e」、
  「apply 6 点契約」→ 9 点)
- 未解決まとめの粒度改善 (Phase 7 領域を Minor で分離)
- example の port 明示 (`8443:8080` publish 想定を明記)

---

## codex review 対応 summary

### Round 1 (Blocker 4 / Major 6 / Minor 4) → Round 2 draft (1000 lines)
全対応 (詳細は 2135367 commit)

### Round 2 (Blocker 3 / Major 7 / Minor 5) → Round 3 draft (1525 lines)
全対応 (詳細は 4a192c9 commit)

### Round 3 (Blocker 3 / Major 9 / Minor 5) → Round 4 draft (本 doc)

**Blocker 3 件本文 fix**:
- 1: TLS trust bootstrap → §c で CA cert OOB 経路 + pinned CA cert + profile schema 拡張 + `ServerOnlyTLSConfig`
- 2: Subpath isolation → §b で behavioral probe (実 volume/container 作って sentinel 可視性確認) + 全 failure fail-hard
- 3: apply_journal recovery → §g で journal schema 拡張 (desired_revision/URL/temp/target/operation/filesystem_step)
  + operation 別 restart 規則 + apply-pending status 追加 + phase 3 の temp + fsync + rename

**Major 9 件本文 fix (targeted)**:
- 1: workspace name uniqueness 直列化 → §a で `BEGIN IMMEDIATE` + workspace mutex
- 2: mirror/ref/lock 契約 → §b で fetch exclusive lock + resolved SHA materialize + `git repack -ad` 撤回
- 3: PR-3 で URL 経路が cache 実装より先に公開 → §e で PR-3 に repo cache 実装を統合
- 4: config subscriber transaction protocol → §f で `ConfigSubscriber` interface を Prepare/Commit/Rollback 化
  + deadlock 回避 + immutable snapshot pointer + persisted vs active config 分離
- 5: multi-file secret atomicity → §i (`SecretProvider.GetOrCreateMulti`) で sibling temp dir + directory rename
- 6: `JobContext` 情報不足 → §i で WorkspaceHomeDir / RunnerSpec / RunnerState / UID / GID 追加 + cleanup ownership 統一
- 7: `.WorkDir` audit 不完全 → §a で追加 6 consumer table (project_service / sandbox_builder / gitgateway_wire /
  runner / api_store / planner)
- 8: break-glass example 不安定 → §d で `compose exec` 経由に統一 + web_pairing_codes purge 追加
- 9: volume ownership 初期化 → §b で init container (root) 追加 + 全 4 volume 対応 + BOID_UID 固定 1000

**Minor 5 件**: 大半は §未解決まとめ で分離 or 本文修正済み (章内 cross-reference の typo は Minor 対応の
連続 revise で漸次修正、 実装 PR 時に確定)

---

## 参考リンク

- [phase6-container-backend.md](phase6-container-backend.md) — Phase 6 本体、 §決定4 は本 doc で撤回
- [phase6-cutover-followups.md](phase6-cutover-followups.md) — 段階撤去計画、 本 doc の PR 群で吸収
- [container-based-boid.md](container-based-boid.md) — 移行戦略 ①-⑦、 大枠は継続
- [home-workspace-volume.md](home-workspace-volume.md) — Phase 4 の workspace $HOME volume、 論点 b
  で参照、 `workspace_home.go:225-235` の init.sh 独立 file 契約は本 doc §g で継承
- [cli-remote-connection.md](cli-remote-connection.md) — Phase 3、 論点 c の HTTPS + Bearer 契約は本 doc
  でも維持、 TLS trust bootstrap は本 doc で追加
- [workspace-db-consolidation.md](workspace-db-consolidation.md) — Phase 2.5、 default_harness / kits 撤去
- `container-git-gateway-design` (memory) — git gateway 実装、 論点 b の bare repo fetch 経路で参照
- `phase6-dogfood-incident-and-pivot` (memory) — 本 doc の pivot 経緯記録
