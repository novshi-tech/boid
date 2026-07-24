# volume-only daemon: Phase 6 の compose 部分の再設計

ステータス: **draft (2026-07-24 作成、実装未着手、 codex review round 1 + 2 + 3 + 4 + 5 反映済み)**。
親ドキュメント: [phase6-container-backend.md](phase6-container-backend.md) の compose deploy 部分
(§決定4「host BIND mounts で shared state」) を置換する。
[phase6-cutover-followups.md](phase6-cutover-followups.md) §①-④ は本 doc の実装 PR 群で吸収される。
発端インシデント: `phase6-dogfood-incident-and-pivot` (memory)。

---

## 背景と pivot 経緯

Phase 6 (PR #816-#826) は container backend を実装完了させ、 `sandbox.backend: container` config で opt-in
できる状態まで持ってきた。 2026-07-24 dogfood 初回で bind mount 経路の 3 段の壁 (docker.sock 不在 /
podman subuid mapping / auto-prune で DB 大量消失) を踏み、 nose 判断で **host filesystem 依存 architecture
を廃止、 volume-only daemon に pivot**。 §決定4 と rollback 契約は撤回。

---

## 目的と非目的

### 目的

- daemon 永続 state を named volume に集約、 host filesystem 依存を完全除去
- project 登録経路を **git remote URL からの bare clone** に変える
- secret material を 起動時 volume 内で generate
- workspace / project 定義を **export/import bundle** で扱う
- config.yaml 編集を **CLI + Web UI** 経由に
- userns backend / host daemon 起動経路は **新方式 cutover と同時に廃止** (rollback 契約撤回)
- daemon 永続 state を **private / staging / repos / workspace HOME の 4 volume 分離**
- k8s 移行時の seam を volume-only 実装段階で作る (§i)
- 公開 API の **TLS trust bootstrap** を明示契約化 (§c)

### 非目的

- **k8s Helm chart 設計** — Phase 7 で扱う。 seam (§i) は本 doc で決める
- **schema-breaking DB migration** — 本 doc は additive migration のみ許容 (`projects.project_name` /
  `status` / `status_reason` / `revision` の ADD COLUMN、 `apply_journal` テーブル追加)。 既存 column の
  型変更・削除・意味論反転は scope 外
- **secret rotation の実装** — 別 doc。 dogfood 期間中は §d の break-glass procedure で対処

---

## 全体像

```
[host user] --- CLI (HTTPS + Bearer + pinned CA cert) ---\
                                                         \
                                                          +--> [daemon container] --- (engine socket) --> [job container 1..N]
                                                         /       - private volume (daemon-only、 job 見えない、 mount root /home/boid/private):
[host user] --- Web UI (HTTPS+WSS、 same pinned CA) -----/          data/boid/{boid.db,secret.key,web_secret,tls/{gen-N/,current},install_id}
                                                                       ($XDG_DATA_HOME=/home/boid/private/data、 <data_dir> = data/boid)
                                                                    config/boid/config.yaml
                                                                       ($XDG_CONFIG_HOME=/home/boid/private/config)
                                                                 - staging volume (subpath 経由で job-visible):
                                                                    /home/boid/staging/{spec/<job>/,tls/<job>/,broker-tls/<job>/,checkouts/<job>/}
                                                                 - repo cache volume (workspace scoped、 subpath 経由で job-readonly):
                                                                    /home/boid/repos/<workspace>/<project>.git   (bare mirror)
                                                                 - workspace $HOME volume (Phase 4、 workspace 単位、 job-writable):
                                                                    /home/boid/homes/<workspace>/
```

---

## 論点 a: project モデル transition

### 現行

`boid project add <dir>` で host filesystem checkout register。 `projects.work_dir` に host absolute path。
project.yaml 欠落時は `wire.go:226-249` で **hard delete** (今日の DB 全滅原因)。

現行 schema (`0001_initial.sql` + `0028`):

```sql
CREATE TABLE projects (
    id TEXT PRIMARY KEY, work_dir TEXT NOT NULL,
    created_at DATETIME, updated_at DATETIME,
    upstream_url TEXT
);
CREATE TABLE project_workspaces (
    project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL
);
```

### 新方式

`boid project add <git-url> --workspace=<name> [--name=<project-name>]` で URL register。 daemon は
repo cache volume 内に **bare mirror clone**、 DB を source of truth に。

### 新 DB schema (additive migration)

```sql
-- migration NNNN_projects_add_name_and_status.sql
ALTER TABLE projects ADD COLUMN project_name TEXT;
ALTER TABLE projects ADD COLUMN status TEXT NOT NULL DEFAULT 'ready';
    -- 'reserving' | 'ready' | 'degraded' | 'apply-pending' | 'apply-error'
ALTER TABLE projects ADD COLUMN status_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN revision INTEGER NOT NULL DEFAULT 0;
```

**status 値** (5 種、 round 5 Major 2 対応で `reserving` を status model に統合):

| status | 意味 | 遷移 |
|---|---|---|
| `reserving` | `project add` の予約 tx 完了、 clone 実行中 | 予約 tx で insert、 確定 tx で ready へ。 crash 時は startup sweep で row 削除 (下記) |
| `ready` | 正常 | 初期、 fetch/apply 成功で戻る |
| `degraded` | bare repo missing / parse error 等 | on-startup / fetch 失敗で遷移 |
| `apply-pending` | apply の phase 2 完了、 phase 3 実行中 | apply phase 2 で set、 phase 4 で ready へ。 startup 時に残存していたら apply-error へ一括転記 (§g) |
| `apply-error` | apply 中の partial failure | phase 3 失敗 or crash recovery、 明示 recovery まで残る |

dispatch は `ready` のみ許可 (`reserving` を含む他 4 status は dispatch 対象外)。

### 一意性の直列化 (round 3 Major 1、 round 4 Major 2 対応)

現行 `internal/db/db.go:23-28` の DSN は `_time_format=sqlite` のみで `_txlock=immediate` 無し
(codex round 4 指摘)。 `sql.LevelSerializable` を渡すだけでは modernc.org/sqlite は `BEGIN IMMEDIATE` を
自動発行しない。

**対策 (2 段)**:

1. **DSN に `_txlock=immediate` を追加** (`db.Open` の DSN string に append):
   ```go
   dsn += "&_txlock=immediate"  // BEGIN IMMEDIATE を default に
   ```
2. **network I/O (git clone) を tx 外に**: 現行 DB は connection 1 個、 clone 中 tx 保持すると全 DB 塞ぐ。
   別 tx に分割:
   - **予約 tx** (短時間): project_name の一意性 verify + placeholder row を `status='reserving'` で insert
   - **git clone / project.yaml validate** (tx 外、 filesystem/network I/O)
   - **確定 tx** (短時間): placeholder row を `status='ready'` に update

疑似コード:

```go
// 1. 予約 tx
mu := workspaceMutex(workspaceID)  // per-workspace sync.Mutex
mu.Lock()
defer mu.Unlock()

reserveTx, _ := db.BeginTx(ctx, nil)  // DSN _txlock=immediate で BEGIN IMMEDIATE
var count int
reserveTx.QueryRow(`SELECT COUNT(*) FROM projects p JOIN project_workspaces pw
                     ON p.id=pw.project_id
                     WHERE pw.workspace_id=? AND p.project_name=?`,
                     workspaceID, name).Scan(&count)
if count > 0 { reserveTx.Rollback(); return ErrDuplicate }
reserveTx.Exec(`INSERT INTO projects (id, project_name, status, ...) VALUES (?, ?, 'reserving', ...)`, ...)
reserveTx.Exec(`INSERT INTO project_workspaces (project_id, workspace_id) VALUES (?, ?)`, ...)
reserveTx.Commit()

// 2. tx 外で clone + validate (long-running I/O)
if err := repoCache.Register(ctx, workspace, name, url); err != nil {
    // clone 失敗 → 予約 row を削除 (fail-open にせず fail-loud)。
    // この DELETE 自体が失敗した場合は row が 'reserving' のまま残るが、
    // 下記の startup sweep + GC reaper が回収する (project name の恒久占有はしない)
    if _, derr := db.Exec(`DELETE FROM projects WHERE id=?`, projID); derr != nil {
        slog.Error("reserving row cleanup failed; swept at next startup/GC",
                   "project_id", projID, "error", derr)
    }
    return err
}

// 3. 確定 tx (短時間)
db.Exec(`UPDATE projects SET status='ready' WHERE id=?`, projID)
```

**`reserving` の lifecycle (round 5 Major 2 対応)**: transient state を恒久残留させないための
recovery/timeout を status model に統合する:

- **startup sweep**: daemon 起動時 (§g の apply_journal recovery と同じ startup recovery phase) に
  `status='reserving'` の row を全て DELETE (+ 対応する repo cache の作りかけ dir を削除)。
  `reserving` は生きている `project add` request の処理中にのみ有効で、 daemon crash 時点で
  その request は client 側に error/切断として観測済みのため、 row を残す理由がない
- **GC reaper**: daemon 再起動が無い場合でも、 既存 24h GC cycle に `status='reserving' AND
  created_at < now()-1h` の sweep を追加 (clone がどれだけ遅くても 1h あれば確定 tx に到達する
  前提。 超えて残っているのは cleanup DELETE 失敗の残骸とみなす)。 これで clone 中 crash・
  cleanup DELETE 失敗のどちらでも project name が恒久占有されない
- 一意性 verify (予約 tx の COUNT) は `reserving` row も占有として数える (それが予約の目的)。
  stale な `reserving` が邪魔をするケースは上記 2 経路の sweep で解消される

**目的節 (§非目的) の「project_workspaces に unique constraint 追加」記述は round 3 で削除済み** (index 不能、
app-side + mutex + BEGIN IMMEDIATE で保証)。

### auto-prune 撤去

`wire.go:226-249` の on-startup hard delete 撤去。 startup 挙動:
- bare repo missing → 再 clone 試行、 失敗なら `status='degraded'`
- `project.yaml` parse error → `status='degraded'`

明示的削除入口は `boid project rm` / `boid workspace delete` のみ。

### CLI 契約

- `boid project add <dir>` → **removed**
- `boid project add <git-url>` → **new** (workspace 必須)
- `boid project rm <id>` → 現行 + repo cache dir 削除 + 進行中 fetch/dispatch 中断
- `boid project list` → URL + status 表示
- `boid project fetch <id>` → **new**
- `boid project status <id>` → **new**

### `work_dir` の意味論変更 と `.WorkDir` audit (round 5 Major 6 対応で機械列挙の完全版に差し替え)

audit 方法を明示する: `grep -rn '\.WorkDir' --include='*.go'` の非 test 全 hit (2026-07-24 時点、
`_test.go` / `testutil` 除外) + `ProjectWorkDir` field の伝搬先。 hit は 2 系統に分かれる —
**project entity の `.WorkDir`** (意味論が dir → URL 参照に変わる本体) と、 **`JobSpec.WorkDir`**
(project.WorkDir から派生した job cwd が入る field。 型は変わらないが値の出所が
`RepositoryCache.Materialize` の checkout path に変わる)。

**project entity `.WorkDir` consumer (全 hit)**:

| file | 用途 |
|---|---|
| `internal/api/project.go:37,73,78` | project list/create request・response |
| `internal/api/project_service.go:820` | upstream URL backfill (service layer) |
| `internal/api/task_create.go:50,54,145,146,168` | branch classification / `${current_branch}` 展開 |
| `internal/api/task_notify.go:327,330,331,342` | gitFetchOrigin / gitObjectExists / gitRemoteTip |
| `internal/orchestrator/project_store.go:315,317` | on-startup Load |
| `internal/orchestrator/project_catalog.go:24,350` | DB Save/Scan (persistence layer) |
| `internal/orchestrator/planner.go:116,129` | planner の repo path + `PolicyContext.ProjectDir` |
| `internal/server/wire.go:274,1071,1122` | upstream backfill (274) + session dispatch (1071) + exec dispatch (1122) |
| `internal/server/api_store.go:141,152,331` | `TokenContext.ProjectDir` (141) + ResolveHostCommands の projectDir (152) + 表示名 `filepath.Base` (331) |
| `internal/dispatcher/runner.go:602,626` | dispatcher runner + peer path 列挙 |
| `internal/dispatcher/gitgateway_wire.go:216` | git gateway の clone dir 名 (`projectDirName`) |
| `internal/dispatcher/session_job.go:24,89,111,123` (`ProjectWorkDir`) | session/exec job cwd + PolicyContext + clone declaration |
| `cmd/project.go:280,360` / `cmd/project_ref.go:75,108` / `cmd/workspace.go:278` | CLI 表示 |

**`JobSpec.WorkDir` consumer (派生値、 checkout path に差し替わる側)**:

| file | 用途 |
|---|---|
| `internal/dispatcher/sandbox_builder.go:591` | `RealizationSpec.WorkDir` |
| `internal/sandbox/runner/state.go:186` / `runner.go:233,275` | runner state dump / Workspace field / JobDone RPC |
| `internal/sandbox/realization/realization.go:194,229` | docker create Workdir |
| `internal/dispatcher/container_backend.go:945,980` | container Workdir passthrough |

**特に注意**: path/policy consumer — `TokenContext.ProjectDir` (`api_store.go:141`)、 ResolveHostCommands
(`api_store.go:152`)、 `PolicyContext` (`planner.go:129` / `session_job.go:89`)、 peer 列挙
(`runner.go:626`) — は URL を path 扱いすると semantic bug。 全 consumer を `ProjectRef` +
`RepositoryCache` 経由に refactor する。 実装 PR (PR-3) では上記 grep を再実行して線表を更新する
(行番号は本 doc 時点の snapshot)。

**新型 `ProjectRef`** (詳細 §i):

```go
type ProjectRef struct {
    ID, Name, WorkspaceID, UpstreamURL, CachePath string
    Status, StatusReason string
    Revision int
}
```

### migration path

現行 project は auto-migration しない (nose: 「boid のデータは揮発性許容」)。

### 未解決論点

- fetch depth (mirror full vs partial)

---

## 論点 b: daemon 管理 state と job container への配送

### 4 volume 分離

| volume | scope | mount root | 中身 |
|---|---|---|---|
| `boid_private` | daemon 専用、 job 見えない | `/home/boid/private` | `data/boid/{boid.db,secret.key,web_secret,tls/{gen-N/,current},install_id}` + `config/boid/config.yaml` (XDG env で directory 分離、 下記 ownership 初期化参照。 `<data_dir>` = `/home/boid/private/data/boid`) |
| `boid_staging` | daemon-writable、 job-readable/writable (subpath 単位) | `/home/boid/staging` | 各 job 用 spec/state/TLS material/checkout |
| `boid_repos` | daemon-writable、 job-readable (subpath 単位、 read-only) | `/home/boid/repos` | workspace/project ごとの bare mirror |
| `boid_homes_<workspace>` | workspace 単位、 job-writable | (job 内 `/home/boid`) | workspace HOME (Phase 4) |

### compose.yml の volume 実名 (round 4 Blocker 1 対応)

**問題**: Compose は volume に project 名を prefix する (`<project>_boid_private`)。 daemon 側で
`docker create --mount src=boid_staging,...` (裸の名前) を呼ぶと **別の空 volume が新規作成される**。

**対策 (2 段)**:

1. **compose.yml で top-level volume の `name:` を明示** (prefix 回避):
   ```yaml
   volumes:
     boid_private:
       name: boid_private            # ← 実 volume 名を pin (project prefix 回避)
     boid_staging:
       name: boid_staging
     boid_repos:
       name: boid_repos
     # boid_homes_<workspace> は workspace apply の phase 3 で dynamic 作成
   ```
2. **daemon 側は startup で label ベース discovery を必須 verification にする** (round 5 Major 1 対応で
   fallback ではなく全 failure path fail-hard に): engine API `docker volume ls --filter
   label=io.boid.role=staging` で実名を確認する。 not found / multiple matches / engine error /
   name mismatch の **4 つ全てを区別して fail-hard** (どれか一つでも裸の `boid_staging` 名で
   続行すると「別の空 volume を新規作成する」元の failure mode に戻ってしまうため、 discovery を
   スキップして続行する経路は作らない):

daemon 側 startup verification + mount 生成:

```go
// 疑似コード (startup、 job dispatch 前に 1 回)
const stagingVolName = "boid_staging"  // compose.yml の name: と一致
actualName, err := engine.LookupVolumeByLabel(ctx, "io.boid.role=staging")
switch {
case errors.Is(err, ErrVolumeNotFound):
    return fmt.Errorf("staging volume not found (label io.boid.role=staging): "+
        "compose stack が init service 込みで起動済みか確認 (docker volume ls --filter label=...)")
case errors.Is(err, ErrMultipleVolumesMatch):
    return fmt.Errorf("multiple volumes carry label io.boid.role=staging: %w — "+
        "過去 deploy の残骸 volume を削除して一意にすること", err)
case err != nil:
    return fmt.Errorf("staging volume discovery failed (engine error): %w", err)
case actualName != stagingVolName:
    return fmt.Errorf("staging volume name mismatch: expected %q, discovered %q. "+
        "compose.yml の name: と一致するよう確認", stagingVolName, actualName)
}
// verification 通過後のみ mount 生成 (repos / private も同型で io.boid.role=repos / private を verify)
mounts := []MountSpec{{Source: stagingVolName, Target: "/run/boid/spec", Subpath: "spec/"+jobID}}
```

これで:
- 通常運用: compose.yml の `name:` で prefix 回避、 discovery が一致を確認して mount が hit
- 異常運用 (label 無し / 重複 / engine error / 名前不一致): いずれも daemon 起動失敗として顕在化、
  空 volume への silent fallback は起きない

### DooD 制約と staging volume

現行 (`container_backend.go:186-199`) の DooD 契約:
> mount Source it hands the HOST's docker daemon has to be a path the HOST filesystem actually has.

volume-only では staging を **daemon-owned named volume + subpath mount** で確保。

### staging isolation は behavioral probe (Blocker 2 対応、 継続)

version probe 撤回、 **behavioral probe** (daemon 起動時、 一時 volume 作成 → sentinel A/B 書く →
subpath=a container を start → sentinel A 読める + sentinel B 読めない + volume root に到達不能 を verify):

```go
func behavioralSubpathProbe(ctx, engine) error {
    tempVol := fmt.Sprintf("boid-probe-%d", os.Getpid())
    engine.VolumeCreate(ctx, tempVol)
    defer engine.VolumeRemove(ctx, tempVol, true)

    seedVolume(engine, tempVol, "a/sentinel-a", "A")
    seedVolume(engine, tempVol, "b/sentinel-b", "B")

    result := runProbeContainer(engine, tempVol, "a")  // subpath=a mount
    if !result.CanReadSentinelA {
        return errors.New("subpath probe: sentinel A unreadable — subpath field ignored")
    }
    if result.CanReadSentinelB {
        return errors.New("subpath probe: sentinel B readable — whole volume mounted, isolation broken")
    }
    if result.CanEscapeToVolumeRoot {
        return errors.New("subpath probe: volume root accessible from subpath mount")
    }
    return nil
}
```

全 failure path (通信 / timeout / create / start / semantic mismatch) を fail-hard で daemon abort。

### volume ownership 初期化 (round 5 Blocker 1 対応で mount topology 確定)

uid 1000 daemon は root-owned volume を chown 不可 → **init container (root) で 3 static volume を
初期化** (`boid_homes_<workspace>` は下記の通り init container 対象外)。

**mount topology の確定** (round 5 Blocker 1 対応。 round 4 draft は init service が `boid_private`
しか mount しておらず staging/repos の chown が image layer に空振りする + 同一 volume の 2 path mount
が XDG 分離にならない、 の 2 点が誤りだった。 以下が訂正版):

- `boid_private` は **`/home/boid/private` の単一 mount point** に mount する。 data と config の分離は
  **mount ではなく volume 内 directory** で行う: `XDG_DATA_HOME=/home/boid/private/data` +
  `XDG_CONFIG_HOME=/home/boid/private/config` を daemon の env に設定すると、 既存の XDG 依存 path
  解決 (現行 compose.yml が Major 10 対応で既にやっているのと同じ機構) だけで
  data dir = `/home/boid/private/data/boid/`、 config = `/home/boid/private/config/boid/config.yaml`
  に落ちる。 **subpath mount も 2 重 mount も不要**
- `boid_staging` / `boid_repos` は `/home/boid/staging` / `/home/boid/repos` に mount
- **init service は 3 volume 全部を daemon と同じ path に mount して chown する** — mount 済みの
  named volume に対する chown なので実 volume に永続する (round 4 draft の「mount してない path を
  chown して image layer に作用するだけ」問題の訂正)

```yaml
volumes:
  boid_private:
    name: boid_private
    labels:
      io.boid.role: private
  boid_staging:
    name: boid_staging
    labels:
      io.boid.role: staging
  boid_repos:
    name: boid_repos
    labels:
      io.boid.role: repos

services:
  init:
    image: boid-runner:latest
    user: "0:0"
    entrypoint: ["/bin/sh", "-c"]
    command:
      - |
        set -eu
        mkdir -p /home/boid/private/data/boid /home/boid/private/config/boid
        chown -R 1000:1000 /home/boid/private /home/boid/staging /home/boid/repos
    volumes:
      - boid_private:/home/boid/private
      - boid_staging:/home/boid/staging
      - boid_repos:/home/boid/repos

  daemon:
    depends_on:
      init: { condition: service_completed_successfully }
    user: "1000:1000"
    environment:
      XDG_DATA_HOME: /home/boid/private/data
      XDG_CONFIG_HOME: /home/boid/private/config
    volumes:
      - boid_private:/home/boid/private
      - boid_staging:/home/boid/staging
      - boid_repos:/home/boid/repos
    # engine socket / networks / その他 env は現行 compose.yml と同様 (省略)
```

**`boid_homes_<workspace>` が init container 対象外の理由**: workspace apply の phase 3 で daemon が
engine API で動的作成し、 job container の `/home/boid` に mount される。 mount point `/home/boid` は
image 内に uid 1000 所有で存在する (`build/container/Dockerfile` の `useradd --create-home`) ため、
空 volume の初回 mount 時に engine の copy-up が image 側 ownership を引き継ぎ、 root chown は不要。
static 3 volume は mount point が image に存在しない path なので engine が root で dir を作る —
これが init container が必要な理由 (対比)。

- BOID_UID/GID は **固定 1000** (compose の build-arg で override 可能性を撤回、 volume-only では有害)

### bare repo の job 提供 (mirror clone + fetch/dispatch lock)

**mirror clone** + **symbolic HEAD 更新** (round 4 Major 3 対応で経路修正):

`git remote set-head origin --auto` は `refs/remotes/origin/HEAD` を設定するだけで、 mirror repository 自身の
`HEAD` は更新しない。 正しくは `ls-remote --symref origin HEAD` の結果を `symbolic-ref` に反映する。
**failure contract を明示する** (round 5 Major 7 対応 — shell pipeline の `xargs` 流しでは空結果が
silent no-op になるため、 実装は Go で分岐を明示):

```bash
# 初回
git clone --mirror <url> <cache_path>

# 以降 refetch
git -C <cache> remote update --prune
```

```go
// 疑似コード: fetch 成功後の symbolic HEAD 更新
out, err := git(ctx, cache, "ls-remote", "--symref", "origin", "HEAD")
if err != nil {
    // remote 到達不能 (fetch 直後だが network は落ち得る) → fetch operation 全体を失敗扱い、
    // fetch 失敗と同じ経路で project status='degraded' + error return
    return fmt.Errorf("ls-remote --symref: %w", err)
}
branch, ok := parseSymrefHead(out)  // "ref: refs/heads/<name>\tHEAD" 行を parse
if !ok || branch == "" {
    // 上流が symref を返さない (detached HEAD の upstream / 空 repo / unborn HEAD) —
    // silent skip にせず明示 warning + 既存 HEAD 維持 (HEAD 更新は fetch の付随処理なので
    // fetch 自体は成功扱い。 dispatch は branch 明示 or 既存 HEAD で解決できる)
    slog.Warn("upstream did not report a default branch; keeping existing HEAD",
              "project", ref.Name, "ls_remote_output_prefix", firstLine(out))
} else if err := git(ctx, cache, "symbolic-ref", "HEAD", "refs/heads/"+branch); err != nil {
    return fmt.Errorf("symbolic-ref HEAD update: %w", err)  // 局所 I/O error も fail-loud
}
```

### fetch/dispatch のロック契約 (round 4 Major 3 対応で interface 修正)

| operation | lock mode |
|---|---|
| `git remote update` (fetch) | **exclusive** |
| `git rev-parse <branch>` + job dispatch 用 clone | **shared** |
| `git gc --prune=now` | exclusive |
| `boid project rm` / `mv` | exclusive |

**lock downgrade** は `Lock() -> unlock func()` interface では表現できない (round 4 Major 3 指摘) → interface を
明示的に「acquire → release」の 2 phase に変更、 downgrade は「release + acquire (別 mode)」で表現:

```go
// §i の RepositoryCache interface (改訂):
type RepositoryCache interface {
    // ...
    AcquireLock(ctx context.Context, ref *ProjectRef, mode LockMode) (LockToken, error)
    ReleaseLock(ctx context.Context, token LockToken) error
    // lock downgrade は Release + Acquire (別 mode) の 2 step。 release → acquire の窓では
    // 他の exclusive op (rm / mv / git gc) が割り込み得るため、 shared 再取得後は
    // identity/path 再検証 (下記) が契約 (round 5 Major 3 対応)
}
```

**release → acquire の窓の契約 (round 5 Major 3 対応で訂正)**: round 4 draft の「downgrade 中の窓は
他 op が待つ」は**誤り** — exclusive lock を release した瞬間、 待っていた `boid project rm` / `mv` /
`git gc` が lock を取得できる。 したがって resolve 済みの SHA と cache path は、 shared 再取得後に
**そのまま使ってはいけない**。 atomic downgrade を lock 実装に足す代わりに (実装が重い)、 shared
再取得直後の **identity/path 再検証** を dispatch flow の必須 step とする:

1. cache path が存在すること (`os.Stat(ref.CachePath)`)
2. `git -C <cache> rev-parse --verify <resolvedSHA>^{commit}` が成功すること

どちらか失敗 → 窓の間に対象 repo が削除・移動・GC された → dispatch を **retryable error** で fail
し、 呼び出し側は fetch (step 1) からやり直す。 再検証が通れば、 以降は shared lock が rm/mv/GC
(いずれも exclusive) を排除するので checkout 完了まで対象は安定。

### `--reference` + cleanup

job 開始時:
```bash
git clone --reference <bare_cache> <upstream_url> <checkouts/<jobID>/<project>/>
git -C <...> checkout <resolved_sha>
```

job 終了時: **checkout dir を単純削除** (`git repack -ad` 撤回、 round 3 で確定):
```bash
rm -rf <checkouts/<jobID>/<project>/>
```

cache 側 `git gc --prune=now` は exclusive lock で job 終了まで待つ。 borrowed objects の GC race を防ぐ。

### job dispatch フロー

1. daemon が bare repo の **exclusive lock** を取得
2. `git remote update --prune`
3. symbolic HEAD update (`ls-remote --symref` + `symbolic-ref`、 failure contract は上記)
4. `git rev-parse <branch>` で commit SHA resolve
5. exclusive lock を release
6. **shared lock** を acquire
7. **identity/path 再検証** (上記契約): cache path stat + `rev-parse --verify <SHA>^{commit}`。
   失敗なら shared release + retryable error (step 1 からやり直し)
8. staging volume 内 `boid_staging/checkouts/<jobID>/<project>/` を用意
9. `git clone --reference <cache> <upstream> <checkouts/...>` + `git checkout <SHA>`
10. job container start (staging subpath mount)
11. job 終了時、 checkout dir 削除 + shared lock release

### 未解決論点

- branch policy との整合
- fetch depth (mirror full vs partial)

---

## 論点 c: CLI 到達経路 (HTTPS + Bearer + TLS trust bootstrap)

### 契約 (Phase 3 決定 4/8 継承)

- transport: `unix://` or `https://` (TCP + TLS + Bearer)
- auth: `Authorization: Bearer <device-token>`、 `~/.config/boid/tokens/<profile>.json`
- profile: `~/.config/boid/config.yaml` の `profiles:` map

volume-only では unix 消滅、 **HTTPS + Bearer + pinned CA cert** 一択。

### TLS trust bootstrap (round 4 Major 1 対応で疑似コード修正)

**現状**:
- `server.go:718,733`: 公開 API listener は plain HTTP
- `client.go:200-220`: HTTPS client は system trust store 固定
- `ca.go:228` (`IssueServerCert`): TLS handshake で送る chain は leaf 1 枚のみ
- `ca.go:325-336` (`ServerOnlyTLSConfig`): signature は `(hosts ...string) (*tls.Config, error)`、
  内部で `IssueServerCert(hosts...)` して cert を issue

**採用: CA cert 本体を OOB (export file) で運び、 client は RootCAs pin で verify**
(round 5 Minor 1 対応: 冒頭の「fingerprint pin で verify」記述は撤回 — fingerprint 経路自体を
下記方式 B で撤回しているため、 verify は `--ca-cert` で受け取った CA cert の RootCAs pin のみ)

#### 1. CA cert export CLI

```
boid ca export -o ca.pem       # SecretProvider 経由で <data_dir>/tls/current/ca.crt を PEM で出力
                                # (path は §d の generation directory 契約で解決、 flat path 読みはしない)
boid ca fingerprint             # CA cert の SHA-256 hex (診断・目視照合用)
```

#### 2. `boid login` の pin 経路

**方式 A: CA cert file 経由 (推奨、 唯一実装)** (round 5 Major 8 対応で §d の engine-agnostic helper
経由に統一、 `docker compose` 直叩き例は撤回):
```bash
./scripts/deploy-container.sh --exec 'boid ca export' > /tmp/boid-ca.pem
boid login https://localhost:8443 --ca-cert /tmp/boid-ca.pem --profile local
```

**方式 B (fingerprint 経路) は撤回** (round 4 Major 1 対応):
- 現行 server chain に CA cert 含まれない (`IssueServerCert` は leaf のみ)
- fingerprint 経路は「server が chain 内で CA cert を送る前提」だが成立しない
- `--ca-cert` 主経路のみ、 `--ca-fingerprint` は将来 (server chain に CA 追加する日) の予約

主経路の TLS verify:
- CA cert PEM を parse → self-signed / EKU 検証
- `tls.Config.RootCAs` に `x509.NewCertPool()` + `AddCert(caCert)` (round 4 Major 1 訂正: `*x509.Certificate`
  を pool に追加、 `*x509.CertPool` 型混同を排除)
- `ServerName: profile.URL.Hostname()` で SAN verify

#### 3. Profile schema 拡張

`~/.config/boid/tokens/<profile>.json`:

```json
{
  "token": "...",
  "ca_cert_pem": "-----BEGIN CERTIFICATE-----\n...\n-----END CERTIFICATE-----\n",
  "url": "https://localhost:8443"
}
```

`ca_cert_pem` は PEM 文字列、 parse は runtime で行う。 `ResolvedProfile.CACert` は
`*x509.Certificate` (`x509.CertPool` は毎 request でその都度作る、 pool の long-lived 保持は不要)。

#### 4. HTTPS transport への pinned CA 経路 (round 4 Major 1 対応で型修正)

```go
// 疑似コード
func NewClient(profile *ResolvedProfile) (*Client, error) {
    if profile.URL.Scheme == "https" {
        pool := x509.NewCertPool()
        if profile.CACert != nil {
            pool.AddCert(profile.CACert)  // *x509.Certificate を pool に追加
        }
        tlsConfig := &tls.Config{
            RootCAs:    pool,
            ServerName: profile.URL.Hostname(),
            MinVersion: tls.VersionTLS12,
        }
        transport := &http.Transport{TLSClientConfig: tlsConfig}
        return newHTTPSClient(profile.URL, profile.Token, transport)
    }
    // ...
}
```

#### 5. 公開 listener の TLS 設計 — LeafRenewer 経路に一本化 (round 5 Blocker 4 対応)

round 4 draft は §5 (static `Certificates` の `ServerOnlyTLSConfig`) と §6 (`GetCertificate` の別
`tls.Config`) を併記して最終 listener がどちらか不定だった。 **一本化**: 公開 API listener の
`tls.Config` は **`LeafRenewer.GetCertificate` 経路のみ** とし、 既存 `ServerOnlyTLSConfig`
(`ca.go:325-336`、 static Certificates) は broker / git-gateway 等の内部 listener 用途に現行のまま残す
(公開 listener では使わない)。

**startup 順序の契約** (round 5 Blocker 4 対応):
1. **初回 leaf issue は `NewLeafRenewer` 内で同期実行**し、 error は startup error として return
   (goroutine 内での初回 issue + error 破棄を撤回)
2. renewer 構築が成功した後にのみ listener を bind + Serve — listener が handshake を受ける時点で
   有効な cert が必ず store 済み (`GetCertificate` が `nil, nil` を返す窓は存在しない)

```go
// server.go の tcpServer 初期化 (疑似コード)
hosts := []string{}
if u, err := url.Parse(config.Web.PublicURL); err == nil && u.Hostname() != "" {
    hosts = append(hosts, u.Hostname())
}
hosts = append(hosts, "localhost", "127.0.0.1", "::1")

renewer, err := mtls.NewLeafRenewer(ca, hosts, 7*24*time.Hour)  // 初回 issue はここで同期実行
if err != nil {
    return fmt.Errorf("issue initial server certificate: %w", err)  // startup fail-hard
}
tlsConfig := &tls.Config{
    GetCertificate: renewer.GetCertificate,
    MinVersion:     tls.VersionTLS12,
}
s.tcpServer = &http.Server{Handler: tcpHandler, TLSConfig: tlsConfig}
go renewer.Run(ctx)                                     // renewal loop は listener 開始後の background
go func() { _ = s.tcpServer.ServeTLS(tcpLn, "", "") }() // cert は GetCertificate 経由
```

#### 6. Leaf 更新契約 (`LeafRenewer` 本体)

現行 `leafValidity = 30 * 24 * time.Hour` (`ca.go:50`)、 daemon restart まで更新されない →
daemon 内で自動 renewal:

```go
// internal/mtls/leaf_renewal.go (新規)
type LeafRenewer struct {
    ca      *CA
    hosts   []string       // startup で確定、 以後 immutable (web.public_url は restart-required、 §f)
    renewAt time.Duration  // 例: 7 * 24 * time.Hour (残 7 日切ったら renew)
    current atomic.Pointer[tls.Certificate]
}

// NewLeafRenewer は初回 leaf を同期 issue する。 失敗は error return (呼び出し側 = daemon startup が
// fail-hard)。 これにより listener 開始時点で必ず有効な cert が store 済み。
func NewLeafRenewer(ca *CA, hosts []string, renewAt time.Duration) (*LeafRenewer, error) {
    r := &LeafRenewer{ca: ca, hosts: hosts, renewAt: renewAt}
    cert, err := ca.IssueServerCert(hosts...)
    if err != nil {
        return nil, fmt.Errorf("mtls: initial server cert issue: %w", err)
    }
    r.current.Store(&cert)
    return r, nil
}

func (r *LeafRenewer) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
    cert := r.current.Load()
    if cert == nil {
        // NewLeafRenewer の同期 issue 契約上ここには到達しない。 defense-in-depth:
        // nil, nil を返すと handshake が「no certificate available」で不明瞭に落ちるため明示 error
        return nil, errors.New("mtls: server certificate not initialized")
    }
    return cert, nil
}

func (r *LeafRenewer) Run(ctx context.Context) {
    ticker := time.NewTicker(24 * time.Hour)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            leaf := r.current.Load()  // NewLeafRenewer 契約上 non-nil
            parsed, err := x509.ParseCertificate(leaf.Certificate[0])
            if err != nil {
                slog.Error("leaf renewal: parse current cert", "error", err)
                continue
            }
            if time.Until(parsed.NotAfter) >= r.renewAt {
                continue
            }
            newCert, err := r.ca.IssueServerCert(r.hosts...)
            if err != nil {
                // 失敗時は既存 leaf で継続 + 24h 後に retry。 renewAt=7d なので expiry までに
                // 最大 7 回の retry 猶予 (CA は volume 内 local なので失敗要因は実質 I/O error のみ)
                slog.Error("leaf renewal failed; serving existing leaf", "error", err,
                           "expires", parsed.NotAfter)
                continue
            }
            r.current.Store(&newCert)
            newParsed, _ := x509.ParseCertificate(newCert.Certificate[0])
            slog.Info("leaf certificate renewed", "expires", newParsed.NotAfter)
            // ↑ round 5 Minor 2 対応: 新 cert の NotAfter を出す (旧 parsed.NotAfter ではなく)
        }
    }
}
```

**効果**: uptime 30 日超えても leaf は自動 renewal、 CLI/Web UI は影響なし (CA cert が rotate されない限り
client 側 pin は変わらない、 leaf は再発行のみ)。

#### 7. SAN 決定

- `config.web.public_url` の hostname を SAN に
- `localhost` / `127.0.0.1` / `::1` を default で含める
- IP literal は `x509.Certificate.IPAddresses` field
- SAN (= `LeafRenewer.hosts`) は **startup 時に確定し以後 immutable**。 `web.public_url` の変更は
  restart-required (§f で確定) なので runtime の SAN 切替経路は存在しない — restart 後の
  `NewLeafRenewer` 同期 issue が新 SAN を反映する

### 初回 pair の UX

(round 5 Major 8 対応で §d の engine-agnostic helper 経由に統一 — `docker compose` 固定の手順は撤回、
podman host でも同一手順になる)

```bash
# 1. compose stack 起動
./scripts/deploy-container.sh

# 2. CA cert + pair code 取得 (§d の helper 経由、 engine 検出は deploy script 側)
./scripts/deploy-container.sh --exec 'boid ca export' > /tmp/boid-ca.pem
./scripts/deploy-container.sh --exec 'boid web pair'

# 3. host CLI で redeem
boid login https://localhost:8443 --ca-cert /tmp/boid-ca.pem --profile local
```

### profile 名の derive

`login.go:377`: URL host の first dot-separated label lowercase。 `localhost` → `localhost`。 example に
`--profile local` 明示。

### port 選定

- compose publish: `8443:8080` (host 8443 = HTTPS、 container 8080 = listener)
- example URL: `https://localhost:8443`

### 未解決論点

- TLS listener HTTP/2 (別 PR)
- initwizard は project.yaml scaffold (`wizard.go:27`)、 connection wizard は別 doc

---

## 論点 d: secret ライフサイクル

### 対象と現状

- `secret.key` (SecretStore の AES-256 master key、 `dispatcher/secret_keyfile.go:11` の `LoadOrCreateKey`)
- `web_secret` (Web session cookie signing、 既存 load-or-create)
- daemon internal CA (現行は flat `tls/{ca.crt,ca.key}` を cert/key **順次 WriteFile** する
  `mtls/ca.go:65` の `LoadOrCreate`、 round 3 Major 5 で atomic 化が必要と判明)
- `install_id` (既存 on-first-boot generate)

追加改修は volume 内で動作、 **ただし** CA は下記の generation directory 契約に置換する。

### CA storage layout は generation directory に一本化 (round 5 Blocker 3 対応)

§i の `SecretProvider.GetOrCreateMulti` が導入する generation directory 方式を **CA storage の唯一の
契約**とする。 round 4 draft は §i だけ generation 方式で §c/§d が flat path (`tls/ca.crt`) のまま
だったため、 「break-glass の `rm -f` が成功終了するのに実 CA は `tls/current` 経由で生き残り再発行
されない」という破綻があった。 flat path は全章から撤去する:

```
<data_dir>/tls/gen-N/ca.crt
<data_dir>/tls/gen-N/ca.key
<data_dir>/tls/current -> gen-N       (symlink、 atomic pointer)
    <data_dir> = /home/boid/private/data/boid (§b の XDG 解決)
```

- **読み経路は SecretProvider 一本**: `boid ca export` (§c) を含む全 consumer は
  `SecretProvider.GetOrCreateMulti("tls", ...)` 経由 (= `tls/current/` 解決) で読む。
  `mtls.LoadOrCreate` の flat path read/write は volume-only 移行で撤去 (SecretProvider を
  backend にした load-or-create に置換)
- **削除契約は directory 単位**: break-glass (下記 step 4) は `rm -rf <data_dir>/tls` で generation
  構造 (全 gen-N + current symlink) ごと削除する。 file 単位の `rm -f` は禁止 — current symlink が
  生き残ると restart 後も同じ CA を指し続けて再発行が走らないため。 `tls/` が無ければ次回起動で
  `GetOrCreateMulti` が gen-1 から再生成する (期待どおりの CA 再発行 + 再 pin)

### rotation の non-goal 化

secret rotation は本 doc 外。 `boid web pair/revoke` は device credential で secret rotation 別問題。

### break-glass procedure (round 4 Major 9 対応で engine-agnostic に)

**新規 CLI: `./scripts/deploy-container.sh --run 'cmd'`** (engine 検出 + Compose service 経由の統一 helper):

```bash
#!/usr/bin/env bash
# scripts/deploy-container.sh の一部 (--run flag)
# engine 検出 + Compose service 経由で任意 command 実行
case "$1" in
    --run)
        shift
        exec "${COMPOSE_CMD[@]}" run --rm --user root daemon sh -c "$*"
        ;;
    --exec)
        shift
        exec "${COMPOSE_CMD[@]}" exec daemon sh -c "$*"
        ;;
    ...
```

break-glass procedure:

```bash
# 1. compose stack down
./scripts/deploy-container.sh --down

# 2. DB backup (<data_dir> = /home/boid/private/data/boid、 §b の XDG 解決)
./scripts/deploy-container.sh --run \
    'cp /home/boid/private/data/boid/boid.db \
        /home/boid/private/data/boid/boid.db.backup-$(date +%Y%m%d-%H%M%S)'

# 3. DB purge (secrets + web_devices + web_pairing_codes の 3 table のみ、 fresh DB にはしない)
./scripts/deploy-container.sh --run \
    'sqlite3 /home/boid/private/data/boid/boid.db \
        "DELETE FROM secrets; DELETE FROM web_devices; DELETE FROM web_pairing_codes;"'

# 4. secret material 削除 — CA は directory 単位で rm -rf (generation 契約、 上記)。
#    file 単位の rm -f だと tls/current symlink が生き残り CA 再発行が走らない
./scripts/deploy-container.sh --run \
    'rm -f  /home/boid/private/data/boid/secret.key \
            /home/boid/private/data/boid/web_secret && \
     rm -rf /home/boid/private/data/boid/tls'

# 5. daemon 再起動
./scripts/deploy-container.sh

# 6. 新 CA cert + pair code → 再 pair
./scripts/deploy-container.sh --exec 'boid ca export' > /tmp/boid-ca.pem
./scripts/deploy-container.sh --exec 'boid web pair'
boid login https://localhost:8443 --ca-cert /tmp/boid-ca.pem --profile local

# 7. SecretStore 消費者 (workspace secrets 等) の re-provision
```

**効果**:
- podman/docker 双方対応 (deploy script 内で engine 検出済み)
- `compose.volume-only.yml` 選択も deploy script 側の統一 flag で
- container 名依存 (`container_daemon_1`) 撤去、 service 名 (`daemon`) で参照

### 副作用の因果 (round 5 Minor 3 対応で訂正)

- device pair 全失効: **step 3 の `web_devices` purge のため** (DB は fresh にしない、 purge は
  3 table のみ)
- session cookie 全部無効: `web_secret` 削除 → 再生成
- SecretStore rows 破棄: step 3 の `secrets` purge + `secret.key` 再生成 (旧 rows は新 key で
  decrypt 不可なので purge が正)
- 内部 mTLS 全面再発行: `tls/` 削除 → 新 CA。 leaf / client cert は全て新 CA 配下で発行し直し
- `install_id` は**維持される** (round 5 Minor 3 の事実誤認を訂正: 本手順は `install_id` を削除
  しないため「install_id が変わる」は成立しない。 install identity と orphan reap の
  install_id scope 連続性は保たれる — break-glass は credential reset であって install 再作成では
  ない、 が意図した契約)

### 未解決論点

- k8s Secret provider の initContainer 契約

---

## 論点 e: 現行 host daemon 経路廃止 (PR 分割 additive/inert)

### 撤去対象

`cmd/start.go` の `runDaemonParent` / `internal/dispatcher/userns_backend.go` /
`internal/sandbox/runner/runner_linux.go` / `sandbox.backend` config option。

### PR 分割 (round 3 で additive/inert 化、 round 4 で追加調整無し)

- **PR-1: seam interface 導入** (`RepositoryCache` / `SecretProvider` / `JobMountDescriptor` interface +
  Volume 実装)
- **PR-2: volume-only compose stack + staging + volume ownership init + behavioral probe** (feature-gated)
  - **前提 gate**: engine socket abstraction PR が先 landed (podman socket follow-up、 本 doc scope 外)
- **PR-3: URL-aware startup + auto-prune 撤去 + additive migration + repo cache 実装** (URL 経路と cache
  同 PR で公開、 split-brain 消失)
- **PR-4: CLI Bearer + TLS bootstrap + config CLI/Web UI** (5 段 sub-PR、 §f 参照)
  - PR-4.a: config in-memory + reload API + revision counter + atomic pointer swap
  - PR-4.b: YAML AST preservation + atomic write + `ConfigSubscriber` (Prepare/Commit/Rollback)
  - PR-4.c: `boid config` CLI
  - PR-4.d: `/settings` Web UI
  - PR-4.e: client/daemon config file split
- **PR-5: workspace export/import bundle** (feature-gated)
- **PR-6: cutover (default 切替 + unix fallback + host daemon + userns 一斉撤去)**

各 PR landed 時点の main 可動性は round 3 で確立、 round 4 対応で追加変更無し。

### 未解決論点

- e2e coverage の container 移植状況
- `boid start` の CLI wrap 化

---

## 論点 f: config.yaml 編集経路 (CLI + Web UI)

### CLI + Web UI

```bash
boid config get / apply / set / unset / edit
```

Web UI: `/settings` page。 CLI subcommand は **authenticated HTTP API** 経由。 `default_harness` 撤去済み。

### reload semantics (round 3 で Prepare/Commit/Rollback 化、 round 4 Major 5 で active/desired 分離詳細化)

**`ConfigSubscriber` interface**:

```go
type ConfigSubscriber interface {
    Prepare(ctx, oldCfg, newCfg *Config) error    // dry-run、 副作用なし
    Commit(ctx, newCfg *Config) error             // 副作用あり
    Rollback(ctx, oldCfg *Config) error           // reverse order で呼ばれる
}
```

### reload flow (round 4 Major 5 対応で file write failure rollback + active/desired merge 明記)

1. reload API 受信 → 新 config を parse + validate
2. **active/desired merge**: `desired = fromFile()`, `oldActive = current in-memory`
   - dynamic key: `newActive := merge(oldActive.static, desired.dynamic)`
   - restart-required key: `newActive.static = oldActive.static` (変更しない、 file には書く)
3. subscriber の `Prepare(oldActive, newActive)` を順次 (副作用なし、 lock 保持しない)
4. Prepare 全成功 → 各 subscriber の `Commit(newActive)` 順次
5. Commit i 番目失敗 → 0..i-1 に reverse order で `Rollback(oldActive)`
6. Commit 全成功 → file に atomic write (temp + rename、 flock 付き)
7. **file write 失敗の場合 (round 4 Major 5 対応)**: file rollback (temp file 削除) + subscriber に
   Rollback(oldActive) を reverse order + in-memory pointer 変更しない
8. file write 成功 → revision counter increment + in-memory pointer atomic swap (`*atomic.Pointer[Config]`)
9. pointer swap 後の失敗はあり得ない (`atomic.Pointer.Store` は失敗しない)

**Rollback 自体が失敗した場合の契約** (round 5 Major 5 対応 — `Rollback` は error を返す interface
なので、 step 5 / step 7 での Rollback 失敗時の処理を定義する):

- reload API は **元の失敗 (Commit error or file write error) と rollback error を合成して caller に
  return** (`errors.Join`)、 `slog.Error` で両方記録
- daemon は config subsystem を **degraded に mark**: 以降の reload API request は
  「config reload subsystem degraded — daemon restart required」で全て拒否する。 Rollback 失敗後は
  in-memory active config と subscriber 内部状態の整合が保証できないため、 「壊れた基準の上に次の
  reload を重ねる」ことを構造的に防ぐ
- **daemon abort (fail-hard) にはしない**: reload は online admin 操作であり、 進行中 job を巻き添えに
  する failure ではない。 degraded mark + restart 要求が bounded な復旧経路 (restart すれば file の
  内容から一貫した状態で再スタート)

**`web.public_url` は restart-required に確定** (round 5 Major 5 対応で「実装 PR で決定」の先送りを
撤回):

dynamic にすると Commit 後すぐ新 URL が active になる一方、 新 SAN の leaf 発行が「次回 renewal
cycle」(最大 24 時間後) まで遅れて「URL は変わったが serving cert が SAN 不一致」の窓が生じる。
Commit 内での同期 issue + swap で窓は消せるが、 §c の `LeafRenewer.hosts` immutable 契約を崩して
可変 hosts + issue 失敗時の config rollback 連鎖を持ち込むコストに見合わない。 restart-required
なら §c の「startup で hosts 確定 → `NewLeafRenewer` 同期 issue」経路だけで SAN 更新が完結する。

### restart-required key の catalog

| category | keys |
|---|---|
| **dynamic** | `sandbox.allowed_domains`, `notify.command` |
| **restart-required** | `gateway.forges.*`, `web.http_addr`, `web.public_url` (SAN 確定のため、 上記), `gc.*`, `task_ask.*` |
| **removed on volume-only** | `sandbox.backend` |

### PR-4 sub-PR 分割

上記 5 段。

### 未解決論点

- dotted path 構文 (array/map の扱い)
- secret env var 値の編集経路

---

## 論点 g: workspace/project export/import shape

### bundle 形式

**tar.gz bundle** (`workspaces.tar.gz`) with `workspaces.yaml` + `workspaces/<name>/init.sh`。

### YAML shape

Kubernetes-like envelope、 `spec.projects[]` nested、 `spec.init_script` は bundle 内相対 path。 詳細は
round 3 draft と同一 (追加変更無し、 round 4 で指摘無し)。

### apply の transaction + recovery (round 4 Blocker 2 対応で apply_journal 全面書き直し)

**apply_journal schema (改訂版)**:

```sql
-- migration NNNN_apply_journal.sql
CREATE TABLE IF NOT EXISTS apply_journal (
    id                 TEXT PRIMARY KEY,
    started_at         DATETIME NOT NULL,
    completed_at       DATETIME,                    -- NULL = in-progress or crashed
    phase              TEXT NOT NULL,               -- validate | db_upsert | filesystem | status_transition
    operation          TEXT NOT NULL,               -- operation catalog 9 種 (下記表と 1:1 対応)
    target_kind        TEXT NOT NULL,               -- workspace | project | hostcommands
    target_name        TEXT NOT NULL,
    desired_revision   INTEGER,
    target_url         TEXT,
    staging_path       TEXT,                        -- ".new" 側 (新 content の一時場所)
    backup_path        TEXT,                        -- ".old" 側 (旧 content の一時場所)
    target_path        TEXT,                        -- 最終 destination
    filesystem_step    TEXT NOT NULL DEFAULT '{}',  -- 完了済み step の下限 marker JSON (下記契約)
    terminal_status    TEXT,                        -- recovery の判定結果 (closeJournal が completed_at と同時に set)
    error              TEXT
);
CREATE INDEX IF NOT EXISTS idx_apply_journal_incomplete
    ON apply_journal(completed_at) WHERE completed_at IS NULL;
```

**round 4 Blocker 2 対応**: `staging_path` (`.new`) と `backup_path` (`.old`) を **別 field に分離**
(round 3 の単一 `temp_path` は意味論混在で誤り)。

#### operation catalog (9 種、 round 5 Blocker 2 対応で明示列挙)

apply の phase 3 が journal に記録する operation は以下の 9 種で全部 (switch と 1:1 対応、
過不足があれば下記 default の fail-hard が検出する):

| # | operation | 内容 | target_kind |
|---|---|---|---|
| 1 | `clone_new` | 新規 project の bare mirror clone (staging に clone → target へ rename) | project |
| 2 | `fetch` | 既存 project の `git remote update --prune` | project |
| 3 | `change_url` | upstream URL 変更 (`git remote set-url origin` + verify fetch) | project |
| 4 | `rename_repo` | project 改名に伴う cache dir rename (staging_path = 旧 path) | project |
| 5 | `rename_old` | URL 差替え等で既存 target repo を backup_path (`.old`) へ退避 (clone_new の前段) | project |
| 6 | `delete_repo` | project 削除に伴う cache dir 削除 | project |
| 7 | `write_initsh` | workspace init.sh の atomic 置換 | workspace |
| 8 | `rewrite_hostcommands` | host_commands.yaml の atomic 置換 | hostcommands |
| 9 | `create_home_volume` | workspace HOME volume の engine API 作成 (§b) | workspace |

#### recovery の基本方針 (round 5 Blocker 2 対応で確定)

- **自動 resume はしない**: recovery は「filesystem を一貫状態に復元する」ことだけを行い、 やり残した
  作業の再実行は **冪等な `boid workspace apply` の再実行** (operator 判断) に委ねる。 これにより
  「再実行の主体・queue・再開条件」という問題系を recovery に持ち込まない。 apply は bundle との
  収束処理なので再実行で必ず収束する
- **close の一元化**: 各 case は outcome を返すだけで、 case 内の `return nil` による journal close
  bypass はしない。 `completed_at` + `terminal_status` の set は switch の後、 呼び出し側の
  `closeJournal` が**単一 UPDATE** で行う
- **recovery action は全て冪等**: recovery 自体の途中 crash 後に再実行しても収束する (下記各 case の
  action は RemoveAll / 条件付き Rename / verify-then-set のみ)

```go
// daemon startup: apply_journal recovery
// scan 対象は completed_at IS NULL のみ (idx_apply_journal_incomplete)。 completed_at が set 済みの
// row は二度と recovery されない — これが二重 recovery 防止の一次 gate。
rows := q(`SELECT * FROM apply_journal WHERE completed_at IS NULL ORDER BY started_at`)
for _, entry := range rows {
    // terminal_status の check 規則 (switch の前、 round 5 Blocker 2 対応):
    // terminal_status と completed_at は closeJournal の単一 UPDATE で同時に set されるので、
    // 「terminal_status あり + completed_at NULL」は通常起きない。 観測されたら過去 recovery の
    // 部分 write (DB 異常) とみなし、 filesystem 復旧は再実行せず close だけやり直す (marker として
    // の defense-in-depth)。
    if entry.TerminalStatus != "" {
        closeJournal(entry, entry.TerminalStatus)
        continue
    }
    outcome, err := recoverOperation(entry)   // ↓ switch。 filesystem 状態の復元のみ行う
    if err != nil {
        return err                            // unknown operation / 消せない file 等は daemon abort
    }
    closeJournal(entry, outcome)              // terminal_status + completed_at を単一 UPDATE で set
    applyEntryStatus(entry, outcome)          // outcome が apply-error なら対象 project/workspace の
                                              // status='apply-error' + status_reason=outcome
}
// journal recovery 後の一括転記: status='apply-pending' のまま残っている project を
// 'apply-error' ("apply interrupted by daemon restart — re-run workspace apply") に落とす。
// apply-pending は生きている apply process の進行中にのみ有効な transient で、 startup 時点の残存は
// crash の証拠 (journal entry を書く前の phase 2↔3 境界 crash もここで拾う)。
// 同じ startup recovery phase で §a の status='reserving' sweep も行う。
```

```go
// recoverOperation: outcome ("recovered-ok" | "apply-error: <reason>") を返す。
func recoverOperation(entry *JournalEntry) (outcome string, err error) {
    switch entry.Operation {

    case "clone_new":
        // 部分 clone (staging) を破棄するだけ。 再 clone は re-apply に委ねる (上記方針)
        if rerr := os.RemoveAll(entry.StagingPath); rerr != nil {
            return "", fmt.Errorf("clone_new recovery: remove staging: %w", rerr)
        }
        return "apply-error: clone interrupted — re-run workspace apply", nil

    case "fetch":
        // fetch は冪等 + 部分 fetch object が cache に残っても無害 (次回 fetch で収束)。
        // filesystem 復元は不要だが、 apply としては完遂していないので re-run を促す
        return "apply-error: fetch interrupted — re-run workspace apply", nil

    case "change_url":
        // set-url は単一 config write で冪等。 現在値を verify して合わせ直す
        current, gerr := gitRemoteGetURL(entry.TargetPath)
        if gerr == nil && current == entry.TargetURL {
            return "recovered-ok", nil        // set-url は完了していた
        }
        if gerr == nil {
            if serr := gitRemoteSetURL(entry.TargetPath, entry.TargetURL); serr == nil {
                return "recovered-ok", nil    // 冪等な再実行で完遂
            }
        }
        return "apply-error: change_url interrupted — re-run workspace apply", nil

    case "rename_repo":
        // 旧 path (staging_path) → 新 path (target_path) の単一 atomic rename
        if fileExists(entry.TargetPath) {
            if rerr := os.RemoveAll(entry.StagingPath); rerr != nil {  // 旧 path の残骸掃除 (冪等)
                return "", fmt.Errorf("rename_repo recovery: cleanup old path: %w", rerr)
            }
            return "recovered-ok", nil
        }
        if fileExists(entry.StagingPath) {
            if rerr := os.Rename(entry.StagingPath, entry.TargetPath); rerr != nil {
                return "", fmt.Errorf("rename_repo recovery: redo rename: %w", rerr)
            }
            return "recovered-ok", nil        // rename をやり直して完遂
        }
        return "apply-error: rename_repo — both paths missing, repo lost (re-add project)", nil

    case "rename_old":
        // target → backup (`.old`) の単一 rename。 親 operation (clone_new) の journal close が
        // 無い限り旧 repo が正 → 必ず revert
        if !fileExists(entry.TargetPath) && fileExists(entry.BackupPath) {
            if rerr := os.Rename(entry.BackupPath, entry.TargetPath); rerr != nil {
                return "", fmt.Errorf("rename_old recovery: revert: %w", rerr)
            }
        }
        return "apply-error: apply interrupted mid-swap — old repo restored, re-run workspace apply", nil

    case "delete_repo":
        // 削除は冪等 → 残っていれば再削除して完遂扱い
        if fileExists(entry.TargetPath) {
            if rerr := os.RemoveAll(entry.TargetPath); rerr != nil {
                return "", fmt.Errorf("delete_repo recovery: %w", rerr)
            }
        }
        return "recovered-ok", nil

    case "write_initsh", "rewrite_hostcommands":
        // atomic 置換系 2 operation は同一の完全規則 (round 5 Blocker 2 対応: write_initsh の
        // `...` 省略を解消 + filesystem_step を判定に使用)。 詳細は下記「filesystem_step 契約」
        return recoverAtomicReplace(entry)

    case "create_home_volume":
        // engine VolumeCreate は同名 + 同 label なら冪等 (既存 volume に対して no-op 相当) →
        // 再実行 + verify で完遂
        if verr := engine.EnsureVolume(entry.TargetName, workspaceVolumeLabels(entry)); verr != nil {
            return "apply-error: home volume create failed — re-run workspace apply", nil
        }
        return "recovered-ok", nil

    default:
        // 未認識 operation は fail-hard: catalog (上表) と switch の drift を起動時に検出
        return "", fmt.Errorf("unknown operation in apply_journal: %q for entry %s. "+
                              "Aborting startup — journal schema mismatch or corrupted entry.",
                              entry.Operation, entry.ID)
    }
}
```

#### filesystem_step 契約 (atomic 置換系、 round 5 Blocker 2 対応)

`filesystem_step` は「**少なくともここまで完了した**」の下限 marker。 phase 3 の各 atomic step の完了
直後に journal row を UPDATE する (phase 2 tx の外の短い個別 write)。 step 実行後〜marker 書き込み前の
crash では marker が実際より少なく見えるため、 recovery は **marker を出発点に、 file 存在の verify で
先へ進んだ形跡を検出したら filesystem 側を信頼する**。

atomic 置換系 (`write_initsh` / `rewrite_hostcommands`) の step keys と順序:

```
{"staged": bool, "backed_up": bool, "swapped": bool, "cleaned": bool}
  staged     = staging_path (`.new`) への write + fsync 完了
  backed_up  = target → backup_path (`.old`) の rename 完了
  swapped    = staging → target の rename 完了
  cleaned    = backup 削除完了
```

```go
func recoverAtomicReplace(entry *JournalEntry) (outcome string, err error) {
    step := parseSteps(entry.FilesystemStep)
    // swapped の判定: marker、 または file 存在による verify。 verify 側は
    // 「backed_up 済み」を条件に含める — target 存在 + staging 不在だけだと
    // 「operation 開始前 crash で旧 target だけが存在する状態」を swap 完了と誤認するため
    // (round 5 Blocker 2 の指摘への直接対応)
    swapped := step.Swapped ||
        (step.BackedUp && fileExists(entry.TargetPath) && !fileExists(entry.StagingPath))
    switch {
    case swapped:
        // 新 content が target に入っている → 完遂扱い。 残骸を掃除 (冪等)。
        // removeIfExists = os.Remove の fs.ErrNotExist 許容版、 他 error は propagate
        if rerr := removeIfExists(entry.BackupPath); rerr != nil {
            return "", fmt.Errorf("atomic replace recovery: cleanup backup: %w", rerr)
        }
        if rerr := removeIfExists(entry.StagingPath); rerr != nil {
            return "", fmt.Errorf("atomic replace recovery: cleanup staging: %w", rerr)
        }
        return "recovered-ok", nil
    case step.BackedUp || (!fileExists(entry.TargetPath) && fileExists(entry.BackupPath)):
        // 退避済み + swap 未完 → backup から target を復元 + staging 破棄
        if !fileExists(entry.TargetPath) {
            if rerr := os.Rename(entry.BackupPath, entry.TargetPath); rerr != nil {
                return "", fmt.Errorf("atomic replace recovery: restore backup: %w", rerr)
            }
        }
        if rerr := removeIfExists(entry.StagingPath); rerr != nil {
            return "", fmt.Errorf("atomic replace recovery: cleanup staging: %w", rerr)
        }
        return "apply-error: swap interrupted — old content restored, re-run workspace apply", nil
    default:
        // staged 以前 (target 無傷) → staging 破棄のみ
        if rerr := removeIfExists(entry.StagingPath); rerr != nil {
            return "", fmt.Errorf("atomic replace recovery: cleanup staging: %w", rerr)
        }
        return "apply-error: interrupted before swap — re-run workspace apply", nil
    }
}
```

### apply の phase 4 段 (round 3 で確定、 round 4 対応なし)

1. phase 1: validate
2. phase 2: DB upsert (SQL tx) + apply_journal 「開始」記録 + status='apply-pending'
3. phase 3: filesystem operations (各 op が temp + fsync + atomic rename、 journal step 記録)
4. phase 4: status transition (project.status='ready' + apply_journal.completed_at)

### 未解決論点

- `ContainerImage` default 化
- branch policy の workspace override
- forge auth の workspace scope

---

## 論点 h: 移行手順

### タイムライン

1. 本 doc 承認 → landed
2. PR-1 (seam) → PR-2 (volume-only + staging + probe) → PR-3 (URL + cache + auto-prune 撤去) →
   PR-4 (Bearer + TLS + config) → PR-5 (workspace bundle) → PR-6 (cutover 一斉撤去)
3. 手動 cutover 実施 (nose の host):
   - 現行 host daemon 停止
   - workspace YAML 手作り
   - `./scripts/deploy-container.sh` (default volume-only)
   - `./scripts/deploy-container.sh --exec 'boid ca export'` → `boid login`
   - `boid workspace apply -f workspaces.tar.gz`
   - `boid task list` で疎通確認

### rollback 用語

- **「rollback path 無し」**: 新 state 保った切戻しは無い
- **disaster fallback**: PR revert + stack down + 旧 host daemon 再起動 + backup restore で fresh install 相当

### Podman socket は cutover checklist で PR-2 前 gate

**PR-2 実施前の必須 gate**: engine socket abstraction (podman rootless の `/run/user/<uid>/podman/podman.sock`
対応) の follow-up PR を先に landed。 本 doc scope 外。

---

## 論点 i: k8s 移行時の seam

### 3 seam (interface 完成版)

#### 1. `SecretProvider`

```go
type SecretMaterial struct {
    Name      string
    Files     map[string][]byte
    FileModes map[string]os.FileMode
}

type SecretProvider interface {
    GetOrCreateFile(ctx, name string, mode os.FileMode, generator func() ([]byte, error)) ([]byte, error)
    GetOrCreateMulti(ctx, name string, generator func() (*SecretMaterial, error)) (*SecretMaterial, error)
    Validate(ctx, name string) error
}
```

**multi-file atomic write** (round 4 Major 6 対応で generation directory + atomic pointer に変更):

```go
// 疑似コード for VolumeSecretProvider.GetOrCreateMulti
func (p *VolumeSecretProvider) GetOrCreateMulti(ctx, name string,
    generator func() (*SecretMaterial, error)) (*SecretMaterial, error) {

    // serialization 契約 (round 5 Major 4 対応): per-name mutex で同名 concurrent call を直列化。
    // nextGeneration() の採番と固定名 `.current.new` temp symlink は mutex 内でのみ触るので
    // same-name 競合は構造的に起きない。 daemon が boid_private volume の唯一の writer
    // (単一 process、 §b) なので in-process mutex で十分 — cross-process file lock は不要
    mu := p.nameMutex(name)  // map[string]*sync.Mutex (lazy init、 map 自体は p.mu で保護)
    mu.Lock()
    defer mu.Unlock()

    baseDir := filepath.Join(p.baseDir, name)      // 例: <data_dir>/tls (§b の XDG 解決)
    currentLink := filepath.Join(baseDir, "current")  // symlink to gen-N/

    // 1. 既存確認: current が指す generation dir を読む
    if target, err := os.Readlink(currentLink); err == nil {
        genDir := filepath.Join(baseDir, target)
        if material, err := loadMulti(genDir); err == nil {
            return material, nil
        }
        // load 失敗 (partial write の残骸) → 新 generation を作る
    }

    // 2. 新 material を generate
    material, err := generator()
    if err != nil { return nil, fmt.Errorf("generate: %w", err) }

    // 3. 新 generation directory を作成
    genNum := nextGeneration(baseDir)  // 既存 gen-N を数えて N+1
    genDir := filepath.Join(baseDir, fmt.Sprintf("gen-%d", genNum))
    if err := os.MkdirAll(genDir, 0o700); err != nil {
        return nil, fmt.Errorf("mkdir gen: %w", err)
    }

    // 4. 全 file を write + fsync (error は propagate、 round 4 Major 6 対応)
    for filename, contents := range material.Files {
        path := filepath.Join(genDir, filename)
        mode := material.FileModes[filename]
        if err := writeFileAndFsync(path, contents, mode); err != nil {
            os.RemoveAll(genDir)  // cleanup
            return nil, fmt.Errorf("write %s: %w", filename, err)
        }
    }

    // 5. gen dir 自体を fsync (directory metadata flush)
    if err := fsyncDir(genDir); err != nil {
        os.RemoveAll(genDir)
        return nil, fmt.Errorf("fsync gen dir: %w", err)
    }

    // 6. atomic pointer swap: current symlink を新 genDir に向ける
    //    (tempSymlink 作成 → rename が atomic)
    tempLink := filepath.Join(baseDir, ".current.new")
    os.Remove(tempLink)  // ignore error
    if err := os.Symlink(fmt.Sprintf("gen-%d", genNum), tempLink); err != nil {
        os.RemoveAll(genDir)
        return nil, fmt.Errorf("symlink temp: %w", err)
    }
    if err := os.Rename(tempLink, currentLink); err != nil {
        os.RemoveAll(genDir)
        os.Remove(tempLink)
        return nil, fmt.Errorf("swap symlink: %w", err)
    }

    // 7. parent (baseDir) fsync (rename の durability)。 失敗は propagate (round 5 Major 4 対応:
    //    「fsync error は propagate」契約と統一、 warning 握りつぶしを撤回)。 rename 自体は成功済み
    //    なので on-disk の current は old/new どちらを指していても「完結した generation」であり、
    //    error return で呼び出し側 (startup) が fail-hard しても次回起動の load (step 1) で収束する
    if err := fsyncDir(baseDir); err != nil {
        return nil, fmt.Errorf("fsync base dir after symlink swap: %w", err)
    }

    // 8. 古い generation は GC (別 goroutine、 N-1 以前を削除)
    go cleanupOldGenerations(baseDir, genNum)

    return material, nil
}

func writeFileAndFsync(path string, contents []byte, mode os.FileMode) error {
    f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
    if err != nil { return err }
    defer f.Close()
    if _, err := f.Write(contents); err != nil { return err }
    if err := f.Sync(); err != nil { return err }  // ← error propagate
    return f.Close()
}

func fsyncDir(dir string) error {
    d, err := os.Open(dir)
    if err != nil { return err }
    defer d.Close()
    return d.Sync()
}
```

**利点**:
- generation directory の atomic swap (symlink rename は POSIX atomic)
- 破損した gen-N は current が指さないので無視される (crash safe)
- fsync error は **parent dir fsync 含め全て propagate**、 silent failure なし (round 5 Major 4)
- 同名 concurrent call は per-name mutex で直列化 (round 5 Major 4)、 `.current.new` /
  `nextGeneration()` の競合は構造的に排除
- 既存 non-empty finalDir の atomic 置換問題 (round 4 Major 6 指摘) は symlink pointer 経路で解決

**注**: 初回 (current link 無し) は上記 flow で symlink を新規作成、 既存 (current 有り) は rename で
swap。 crash 時、 partial gen-N は cleanup goroutine で回収 (current が指さないので参照無し)。

#### 2. `RepositoryCache` (round 4 Major 3 + Major 7 対応)

```go
type LockToken interface {
    Mode() LockMode
    Ref() *ProjectRef
}

type RepositoryCache interface {
    Register(ctx, workspace, projectName, url string) (*ProjectRef, error)
    Fetch(ctx, ref *ProjectRef) error
    ReadFile(ctx, ref *ProjectRef, path string) ([]byte, error)
    DefaultBranch(ctx, ref *ProjectRef) (string, error)
    RefLookup(ctx, ref *ProjectRef, refspec string) (commitSHA string, err error)
    Materialize(ctx, ref *ProjectRef, jobID, resolvedSHA string) (*CheckoutHint, error)
    // CleanupCheckout は削除 (round 4 Major 7: JobMountDescriptor.Cleanup に統一)
    AcquireLock(ctx, ref *ProjectRef, mode LockMode) (LockToken, error)
    ReleaseLock(ctx, token LockToken) error
    ChangeURL(ctx, ref *ProjectRef, newURL string, mode URLChangeMode) error
    Remove(ctx, ref *ProjectRef) error
    List(ctx) ([]*ProjectRef, error)
}
```

**変更点** (round 4 + round 5):
- `CleanupCheckout` を interface から削除 (`JobMountDescriptor.Cleanup` に統一)
- `Lock() -> unlock func()` を `AcquireLock` / `ReleaseLock` の 2 phase に (downgrade を Release+Acquire で表現)
- downgrade の release → acquire 窓では他の exclusive op が割り込み得るため、 shared 再取得後の
  identity/path 再検証 (cache path stat + `rev-parse --verify <SHA>^{commit}`) が呼び出し側の契約
  (§b、 round 5 Major 3)

#### 3. `JobMountDescriptor`

```go
type JobContext struct {
    JobID           string
    WorkspaceID     string
    WorkspaceHomeDir string           // workspace HOME volume path
    ProjectRef      *ProjectRef
    Checkout        *CheckoutHint
    RunnerSpec      []byte             // init seed 用
    RunnerState     []byte             // init seed 用
    TLSMaterial     *SecretMaterial
    BrokerTLS       *SecretMaterial
    UID             int                // 固定 1000
    GID             int
}

type JobMountDescriptor interface {
    DescribeMounts(ctx, jc *JobContext) ([]MountSpec, error)
    // 全 staging subpath (spec/tls/broker-tls/checkouts) を一括削除
    Cleanup(ctx, jobID string) error
}
```

### k8s 対応表 (round 3 と同じ、 round 4 で変更無し)

| compose | k8s |
|---|---|
| `boid_private` | PVC (RWO) + Secret |
| `boid_staging` | PVC (RWX) or EmptyDir per-job |
| `boid_repos` | PVC (RWX、 subPath) or init-job で EmptyDir に materialize |
| `boid_homes_<workspace>` | PVC per workspace |
| secret material | K8s Secret を initContainer で volume に seed |
| CLI listener | Service (ClusterIP or Ingress) |

### 未解決論点

- PVC accessMode 選定
- initContainer sequencing
- Service type

---

## 未解決の設計論点まとめ

### Blocker から昇格した設計決定 (本文で fix 済み)

- §a: auto-prune 撤去 + 5-status (reserving/ready/degraded/apply-pending/apply-error、 reserving の
  startup sweep + GC reaper 統合は round 5 Major 2)、 additive migration、 `ProjectRef` +
  `RepositoryCache` 経由 (`.WorkDir` audit は grep 機械列挙の完全版、 round 5 Major 6)、
  一意性直列化 (DSN `_txlock=immediate` + workspace mutex + tx 分割)
- §b: behavioral subpath probe、 volume ownership 初期化 (init container が 3 static volume を
  daemon と同一 path に mount + chown、 `boid_private` は `/home/boid/private` 単一 mount + XDG env
  分離、 round 5 Blocker 1)、 compose.yml の `name:` 明示 + label discovery の全 failure path
  fail-hard (round 5 Major 1)、 mirror clone + symbolic HEAD failure contract (round 5 Major 7)、
  fetch/dispatch lock (2-phase + downgrade 窓の再検証契約、 round 5 Major 3)、 bare cache は
  cleanup 時 checkout dir 単純削除
- §c: CA cert OOB 経路 + pinned CA cert (`--ca-cert` 主経路、 fingerprint 経路撤回、 冒頭記述も
  round 5 Minor 1 で撤回)、 profile schema 拡張 (`ca_cert_pem`)、 `ResolvedProfile.CACert` は
  `*x509.Certificate`、 公開 listener は **`NewLeafRenewer` 同期初回 issue + `GetCertificate` 経路に
  一本化** (round 5 Blocker 4)、 SAN は startup 確定 + immutable (public_url + localhost + IP)、
  pair/login 例は engine-agnostic helper 経由 (round 5 Major 8)
- §d: break-glass procedure engine-agnostic (`deploy-container.sh --run/--exec` helper 経由)、
  **CA storage は generation directory (`tls/gen-N/` + `tls/current`) に一本化 + 削除は directory
  単位 `rm -rf tls/`** (round 5 Blocker 3)、 副作用の因果訂正 (install_id 維持、 round 5 Minor 3)
- §e: PR 分割 additive/inert
- §f: `ConfigSubscriber` Prepare/Commit/Rollback、 immutable snapshot pointer、 active/desired merge、
  file write failure rollback、 **Rollback 失敗時の degraded 契約 + `web.public_url` は
  restart-required に確定** (round 5 Major 5)
- §g: apply_journal 完成版 (operation catalog 9 種の表 + 全 case の restart 規則、 filesystem_step
  下限 marker 契約、 close 一元化、 terminal_status の switch 前 check、 自動 resume 不採用で冪等
  re-apply へ委譲、 round 5 Blocker 2)、 apply-pending / reserving の startup 一括転記
- §i: `SecretProvider.GetOrCreateMulti` は generation directory + atomic symlink pointer +
  **per-name mutex 直列化 + parent dir fsync error propagate** (round 5 Major 4)。 `RepositoryCache`
  は `AcquireLock`/`ReleaseLock` の 2 phase + 再検証契約、 `CleanupCheckout` は
  `JobMountDescriptor.Cleanup` に統一。 `JobContext` 拡張 (round 3)

### 継続 未解決 (round 5 Major 9 対応で gate 粒度を 3 群に分割)

#### 群 1: PR-1 着手前に nose 判断必須 (seam interface / dispatch 意味論に影響)

- **論点 a/b**: fetch depth (mirror full vs partial) — `RepositoryCache.Fetch`/`Materialize` の実装契約に影響
- **論点 b**: branch policy との整合 — `Materialize` の refspec 契約に影響

#### 群 2: 各 implementation PR 着手前に判断 (PR-1 の gate ではない)

- **PR-2 前**: staging volume の disk 圧迫 / GC 統合、 podman VolumeOptions.Subpath の empirical 挙動
  (PR-2 の behavioral probe で pin)
- **PR-4 前**: dotted path 構文 (array/map の扱い)、 secret env var 値の編集経路
- **PR-5 前**: `ContainerImage` default 化、 branch policy の workspace override、 forge auth の
  workspace scope
- **PR-6 前**: e2e coverage の container 移植、 `boid start` の CLI wrap 化

#### 群 3: Phase 7 / 別 doc (本 doc の実装 PR を gate しない)

- TLS listener HTTP/2 (別 PR、 論点 c)
- k8s Secret provider の initContainer 契約 (論点 d)
- PVC accessMode 選定、 initContainer sequencing、 Service type (論点 i、 Phase 7)

### Minor 未対応

現時点で未対応の Minor は無し (round 5 の 3 件も本文 fix 済み: §c 冒頭の fingerprint 記述撤回 /
renewal log は新 cert の NotAfter / break-glass 副作用の install_id 訂正)。

---

## codex review 対応 summary

### Round 1 (Blocker 4 / Major 6 / Minor 4) → Round 2 draft (1000 lines): 全対応 (2135367)
### Round 2 (Blocker 3 / Major 7 / Minor 5) → Round 3 draft (1525 lines): 全対応 (4a192c9)
### Round 3 (Blocker 3 / Major 9 / Minor 5) → Round 4 draft (1417 lines): 全対応 (36875a2)
### Round 4 (Blocker 3 / Major 9 / Minor 4) → Round 5 draft (1295 lines): 全対応 (988f2ab)

### Round 5 (Blocker 4 / Major 9 / Minor 3) → Round 6 draft (本 doc)

**Blocker 4 件本文 fix**:
- 1: init container の compose.yml 不成立 → §b で mount topology 確定: init service が 3 static
  volume 全部を daemon と同一 path に mount して chown (実 volume に永続)、 `boid_private` は
  `/home/boid/private` 単一 mount + `XDG_DATA_HOME`/`XDG_CONFIG_HOME` env で directory 分離
  (subpath mount / 2 重 mount 不要)、 自己言及の「誤り」例を訂正版 compose.yml に置換、
  `boid_homes_<workspace>` が init 対象外の理由 (image copy-up) も明記
- 2: apply_journal recovery 未完 → §g で operation catalog 9 種の表 (switch と 1:1)、 全 case の
  完全な restart 規則 (`write_initsh` の `...` 解消、 atomic 置換系 2 種は共通の完全規則)、
  `filesystem_step` を下限 marker として判定に使用、 close (`completed_at`+`terminal_status` の
  単一 UPDATE) を switch 後に一元化 (case 内 `return nil` 廃止)、 `terminal_status` の switch 前
  check 規則、 clone_new は自動 resume 不採用 (冪等 re-apply へ委譲、 再実行主体/queue 問題を排除)
- 3: generation-directory secret と CA path 不整合 → §d で generation directory
  (`tls/gen-N/` + `tls/current` symlink) を CA storage の唯一の契約に一本化。 `boid ca export` (§c)
  は SecretProvider 経由 `tls/current/ca.crt` 解決、 break-glass の削除は directory 単位
  `rm -rf tls/` (file 単位 `rm -f` 禁止 — current symlink 残存で CA 再発行が走らない破綻を排除)
- 4: LeafRenewer の listener readiness → §c で初回 issue を `NewLeafRenewer` 内の同期実行 + error は
  startup fail-hard、 listener bind は renewer 構築成功後のみ、 `GetCertificate` は nil 時に明示
  error (defense-in-depth)、 §5/§6 を `GetCertificate` 経路に一本化 (公開 listener での static
  `Certificates` / `ServerOnlyTLSConfig` 使用を撤回)

**Major 9 件本文 fix**:
- 1: volume label discovery fail-hard → §b で not found / multiple matches / engine error /
  name mismatch の 4 分岐全て fail-hard (discovery スキップ続行の経路を排除)
- 2: `reserving` status 統合 → §a で status model を 5 status に (migration comment + status 表)、
  startup sweep + 24h GC reaper (created_at 1h 超) + cleanup DELETE 失敗時の残骸回収を定義
- 3: lock release→acquire 窓 → §b で「他 op は wait」の誤りを訂正、 shared 再取得後の
  identity/path 再検証 (cache path stat + `rev-parse --verify <SHA>^{commit}`) を dispatch flow の
  必須 step に、 失敗は retryable error。 §i の interface コメントも同契約に更新
- 4: multi-file secret durability/concurrency → §i で parent dir fsync 失敗を propagate に変更
  (warning 握りつぶし撤回)、 per-name mutex による same-name concurrent call の直列化契約を追加
- 5: ConfigSubscriber rollback failure / SAN 切替 → §f で Rollback 失敗時の契約 (error 合成 return +
  config subsystem degraded mark + 以降 reload 拒否、 daemon abort はしない) を定義、
  `web.public_url` は restart-required に確定 (SAN 同期 issue の複雑さ回避、 §c の hosts immutable
  と整合)
- 6: `.WorkDir` audit 不完全 → §a で grep 機械列挙の完全版に差し替え (`wire.go:1122` /
  `api_store.go:141,152` / `task_notify.go:331,342` / `project_catalog.go:24,350` / cmd 系 3 file を
  追加、 project entity と `JobSpec.WorkDir` の 2 系統に分離、 audit 方法と snapshot 時点を明記)
- 7: symbolic HEAD failure contract → §b で ls-remote error は fetch 失敗扱い (degraded)、
  symref 空/parse 不能は明示 warning + 既存 HEAD 維持、 symbolic-ref 失敗は fail-loud (xargs での
  silent no-op を撤回)
- 8: engine-agnostic helper の未反映 → §c の login 例 (方式 A) と初回 pair UX を
  `deploy-container.sh --exec` 経由に統一 (`docker compose` 直叩き例を撤回)
- 9: 未解決まとめの gate 粒度 → 「PR-1 前必須 (seam/意味論)」「各 implementation PR 前」
  「Phase 7 / 別 doc」の 3 群に分割

**Minor 3 件全対応**:
- 1: §c 冒頭の「fingerprint pin で verify」→ RootCAs pin に訂正 (fingerprint 経路撤回と整合)
- 2: renewal 成功 log の `expires` → 新 cert の `NotAfter` (`newParsed.NotAfter`) に修正
- 3: break-glass 副作用の事実誤認 → 「fresh DB」を「3 table purge」に、 「install_id 変わる」を
  「install_id は維持される」に訂正 (手順は install_id を削除しない)

---

## 参考リンク

- [phase6-container-backend.md](phase6-container-backend.md) — §決定4 撤回
- [phase6-cutover-followups.md](phase6-cutover-followups.md) — 段階撤去計画、 本 doc に吸収
- [container-based-boid.md](container-based-boid.md) — 移行戦略 ①-⑦
- [home-workspace-volume.md](home-workspace-volume.md) — Phase 4 の workspace $HOME volume
- [cli-remote-connection.md](cli-remote-connection.md) — Phase 3 CLI リモート接続契約
- [workspace-db-consolidation.md](workspace-db-consolidation.md) — Phase 2.5
- `container-git-gateway-design` (memory) — git gateway 実装
- `phase6-dogfood-incident-and-pivot` (memory) — 本 doc の pivot 経緯記録
