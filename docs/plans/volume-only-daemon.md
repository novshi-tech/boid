# volume-only daemon: Phase 6 の compose 部分の再設計

ステータス: **draft (2026-07-24 作成、実装未着手、 codex review round 1 + 2 + 3 + 4 反映済み)**。
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
                                                         /       - private volume (daemon-only、 job 見えない):
[host user] --- Web UI (HTTPS+WSS、 same pinned CA) -----/          /home/boid/.local/share/boid/{boid.db,secret.key,web_secret,tls/,install_id}
                                                                    /home/boid/.config/boid/config.yaml
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
    -- 'ready' | 'degraded' | 'apply-pending' | 'apply-error'
ALTER TABLE projects ADD COLUMN status_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN revision INTEGER NOT NULL DEFAULT 0;
```

**status 値**:

| status | 意味 | 遷移 |
|---|---|---|
| `ready` | 正常 | 初期、 fetch/apply 成功で戻る |
| `degraded` | bare repo missing / parse error 等 | on-startup / fetch 失敗で遷移 |
| `apply-pending` | apply の phase 2 完了、 phase 3 実行中 | apply phase 2 で set、 phase 4 で ready へ |
| `apply-error` | apply 中の partial failure | phase 3 失敗、 明示 recovery まで残る |

dispatch は `ready` のみ許可。

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
    // clone 失敗 → row を削除 (fail-open にせず fail-loud)
    db.Exec(`DELETE FROM projects WHERE id=?`, projID)
    return err
}

// 3. 確定 tx (短時間)
db.Exec(`UPDATE projects SET status='ready' WHERE id=?`, projID)
```

`status='reserving'` は phase 中の transient state (§g apply の `apply-pending` と類似)、 dispatch 対象外。

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

### `work_dir` の意味論変更 と `.WorkDir` audit (round 4 Major 8 対応で追加 4 箇所)

`.WorkDir` consumer (最新 audit):

| file | 用途 |
|---|---|
| `internal/api/project.go:37,73,78` | project list/create response |
| `internal/api/project_service.go:820` | project service layer |
| `internal/orchestrator/project_store.go:315,317` | on-startup Load |
| `internal/orchestrator/planner.go:116, 129` (round 4 追加 129) | planner の repo path + policy context |
| `internal/server/api_store.go:331` | API store |
| `internal/server/wire.go:274, 1071` (round 4 追加) | upstream backfill (274) + session/exec dispatch (1071) |
| `internal/sandbox/runner/runner.go:233,275` | runner Workspace field、 JobDone RPC |
| `internal/sandbox/realization/realization.go:194,229` | docker create Workdir |
| `internal/api/task_create.go:50,54,145,146,168` | branch classification / 展開 |
| `internal/api/task_notify.go:327,330` | gitFetchOrigin |
| `internal/dispatcher/session_job.go:177` | session job cwd |
| `internal/dispatcher/sandbox_builder.go:591` | sandbox builder cwd |
| `internal/dispatcher/gitgateway_wire.go:216` | git gateway |
| `internal/dispatcher/runner.go:602, 626` (round 4 追加 626) | dispatcher runner + peer path 列挙 |
| test | 各種 test |

**特に注意**: peer 列挙 (`runner.go:626`)、 policy context (`planner.go:129`) は URL を path 扱いすると
semantic bug。 全 consumer を `ProjectRef` + `RepositoryCache` 経由に refactor。

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

| volume | scope | 中身 |
|---|---|---|
| `boid_private` | daemon 専用、 job 見えない | `boid.db`, `secret.key`, `web_secret`, `tls/{ca.crt,ca.key,leaf/}`, `install_id`, `config.yaml` |
| `boid_staging` | daemon-writable、 job-readable/writable (subpath 単位) | 各 job 用 spec/state/TLS material/checkout |
| `boid_repos` | daemon-writable、 job-readable (subpath 単位、 read-only) | workspace/project ごとの bare mirror |
| `boid_homes_<workspace>` | workspace 単位、 job-writable | workspace HOME (Phase 4) |

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
2. **daemon 側は startup で label ベース discovery を fallback**: engine API `docker volume ls
   --filter label=io.boid.role=staging` で実名を確認、 `name:` 明示が効いてないケース (別 compose file、 手動
   deploy 等) を fail-hard で検出

daemon 側 mount 生成:

```go
// 疑似コード
stagingVolName := "boid_staging"       // compose.yml の name: と一致
if actualName, err := engine.LookupVolumeByLabel(ctx, "io.boid.role=staging"); err == nil {
    if actualName != stagingVolName {
        return fmt.Errorf("staging volume name mismatch: expected %q, discovered %q. "+
                          "compose.yml の name: と一致するよう確認",
                          stagingVolName, actualName)
    }
}
// mount 生成
mounts := []MountSpec{{Source: stagingVolName, Target: "/run/boid/spec", Subpath: "spec/"+jobID}}
```

これで:
- 通常運用: compose.yml の `name:` で prefix 回避、 mount が hit
- 異常運用: label discovery で fail-hard

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

### volume ownership 初期化 (round 3 Major 9、 round 4 Major 4 対応で訂正)

uid 1000 daemon は root-owned volume を chown 不可 → **init container (root) で全 4 volume 初期化**。

**round 4 対応で訂正**: 
- round 3 draft の `boid_private_config` は未定義 (volume topology と不一致) → 撤回
- brace expansion (`{,.local,.config}`) は POSIX sh で展開されない → 明示 path 列挙

正しい compose.yml:

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
    entrypoint: ["/bin/sh", "-c"]
    command:
      - |
        set -eu
        chown -R 1000:1000 /home/boid/.local/share/boid
        chown -R 1000:1000 /home/boid/.config/boid
        chown -R 1000:1000 /home/boid/staging
        chown -R 1000:1000 /home/boid/repos
    user: "0:0"
    volumes:
      - boid_private:/home/boid/.local/share/boid
      - boid_private:/home/boid/.config/boid  # ← 同 volume 内の別 subpath は無理、 別 volume に
    # ↑ 上の記述は誤り、 正しくは:
    # boid_private の中に .local/share/boid と .config/boid を配置するなら
    # subpath mount を使う (compose の volume-subpath は engine 依存)。
    # 単純化: config も boid_private volume に統合、 daemon 側の path resolver で
    # $XDG_DATA_HOME=/home/boid/.local/share と $XDG_CONFIG_HOME=/home/boid/.config を
    # 同一 volume 内に置く (init container で両方 chown)。

  daemon:
    depends_on:
      init: { condition: service_completed_successfully }
    # ...
```

**採用**: `boid_private` volume 単一 (subpath 分けは daemon 内 path resolver で)、 config も data と同居。
理由: 単一 volume の方が backup/restore/break-glass が simple。 XDG env var で subpath 分ける。

- BOID_UID/GID は **固定 1000** (compose の build-arg で override 可能性を撤回、 volume-only では有害)

### bare repo の job 提供 (mirror clone + fetch/dispatch lock)

**mirror clone** + **symbolic HEAD 更新** (round 4 Major 3 対応で経路修正):

`git remote set-head origin --auto` は `refs/remotes/origin/HEAD` を設定するだけで、 mirror repository 自身の
`HEAD` は更新しない。 正しくは:

```bash
# 初回
git clone --mirror <url> <cache_path>

# 以降 refetch
git -C <cache> remote update --prune

# 上流 default branch 変更を mirror HEAD に反映
git -C <cache> ls-remote --symref origin HEAD | \
    awk '$1 == "ref:" { sub("refs/heads/", "", $2); print $2 }' | \
    xargs -I{} git -C <cache> symbolic-ref HEAD refs/heads/{}
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
    // lock downgrade は Release + Acquire (別 mode) の 2 step、 atomic 化不要 (fetch/dispatch は
    // sequential で serialize されるため、 downgrade 中の窓は他 op が待つ)
}
```

fetch/dispatch は sequential (1 request 内で fetch → dispatch)、 downgrade 中の他 op は wait で問題無し。

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
3. symbolic HEAD update (`ls-remote --symref` + `symbolic-ref`)
4. `git rev-parse <branch>` で commit SHA resolve
5. exclusive lock を release
6. **shared lock** を acquire
7. staging volume 内 `boid_staging/checkouts/<jobID>/<project>/` を用意
8. `git clone --reference <cache> <upstream> <checkouts/...>` + `git checkout <SHA>`
9. job container start (staging subpath mount)
10. job 終了時、 checkout dir 削除 + shared lock release

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

**採用: CA cert 本体を OOB で運ぶ + fingerprint pin で verify**

#### 1. CA cert export CLI

```
boid ca export -o ca.pem       # <data_dir>/tls/ca.crt を PEM で出力
boid ca fingerprint             # CA cert の SHA-256 hex
```

#### 2. `boid login` の pin 経路

**方式 A: CA cert file 経由 (推奨、 唯一実装)**:
```bash
docker compose ... exec daemon boid ca export > /tmp/boid-ca.pem
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

#### 5. 公開 listener は `ServerOnlyTLSConfig` (round 4 Major 1 対応で signature 修正)

```go
// server.go の tcpServer 初期化
hosts := []string{}
if u, err := url.Parse(config.Web.PublicURL); err == nil && u.Hostname() != "" {
    hosts = append(hosts, u.Hostname())
}
hosts = append(hosts, "localhost", "127.0.0.1", "::1")

tlsConfig, err := ca.ServerOnlyTLSConfig(hosts...)  // ← variadic hosts、 内部で IssueServerCert
if err != nil {
    return fmt.Errorf("build server TLS config: %w", err)
}

s.tcpServer = &http.Server{
    Handler:   tcpHandler,
    TLSConfig: tlsConfig,
}
go func() { _ = s.tcpServer.ServeTLS(tcpLn, "", "") }()  // cert は TLSConfig 内
```

#### 6. Leaf 更新契約 (round 4 Blocker 3 対応で goroutine + GetCertificate 経路)

現行 `leafValidity = 30 * 24 * time.Hour` (`ca.go:50`)、 daemon restart まで更新されない。
**round 4 Blocker 3 対応: daemon 内で自動 renewal**:

```go
// internal/mtls/leaf_renewal.go (新規)
type LeafRenewer struct {
    ca         *CA
    hosts      []string
    current    atomic.Pointer[tls.Certificate]
    renewAt    time.Duration  // 例: 7 * 24 * time.Hour (7 日切ったら renew)
}

func (r *LeafRenewer) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
    return r.current.Load(), nil
}

func (r *LeafRenewer) Run(ctx context.Context) {
    // startup: 初回 issue
    cert, _ := r.ca.IssueServerCert(r.hosts...)
    r.current.Store(&cert)

    ticker := time.NewTicker(24 * time.Hour)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            leaf := r.current.Load()
            if leaf == nil { continue }
            parsed, _ := x509.ParseCertificate(leaf.Certificate[0])
            remaining := time.Until(parsed.NotAfter)
            if remaining < r.renewAt {
                // 新 cert を issue + atomic swap
                newCert, err := r.ca.IssueServerCert(r.hosts...)
                if err != nil {
                    slog.Error("leaf renewal failed", "error", err)
                    continue
                }
                r.current.Store(&newCert)
                slog.Info("leaf certificate renewed", "expires", parsed.NotAfter)
            }
        }
    }
}
```

listener 側:
```go
tlsConfig := &tls.Config{
    GetCertificate: renewer.GetCertificate,  // ← 静的 Certificates ではなく dynamic
    MinVersion:     tls.VersionTLS12,
}
```

**効果**: uptime 30 日超えても leaf は自動 renewal、 CLI/Web UI は影響なし (CA cert が rotate されない限り
client 側 pin は変わらない、 leaf は再発行のみ)。

#### 7. SAN 決定

- `config.web.public_url` の hostname を SAN に
- `localhost` / `127.0.0.1` / `::1` を default で含める
- IP literal は `x509.Certificate.IPAddresses` field

### 初回 pair の UX

```bash
# 1. compose stack 起動
./scripts/deploy-container.sh

# 2. CA cert + pair code 取得
docker compose -f build/container/compose.volume-only.yml exec daemon boid ca export > /tmp/boid-ca.pem
docker compose -f build/container/compose.volume-only.yml exec daemon boid web pair

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

- `secret.key` (SecretStore の AES-256 master key、 `secret_keyfile.go:9` の `LoadOrCreateKey`)
- `web_secret` (Web session cookie signing、 既存 load-or-create)
- daemon internal CA (`tls/ca.crt` + `tls/ca.key`、 `mtls/ca.go:65` の `LoadOrCreate` = **cert/key 順次 WriteFile**、
  round 3 Major 5 で atomic 化が必要と判明)
- `install_id` (既存 on-first-boot generate)

追加改修は volume 内で動作、 **ただし** CA の `LoadOrCreate` は round 3 Major 5 + round 4 Major 6 で
atomic 化必要 (§i `SecretProvider.GetOrCreateMulti` の generation directory + atomic pointer 経路)。

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

# 2. DB backup
./scripts/deploy-container.sh --run \
    'cp /home/boid/.local/share/boid/boid.db \
        /home/boid/.local/share/boid/boid.db.backup-$(date +%Y%m%d-%H%M%S)'

# 3. DB purge (secrets + web_devices + web_pairing_codes)
./scripts/deploy-container.sh --run \
    'sqlite3 /home/boid/.local/share/boid/boid.db \
        "DELETE FROM secrets; DELETE FROM web_devices; DELETE FROM web_pairing_codes;"'

# 4. secret material file 削除
./scripts/deploy-container.sh --run \
    'rm -f /home/boid/.local/share/boid/secret.key \
           /home/boid/.local/share/boid/web_secret \
           /home/boid/.local/share/boid/tls/ca.crt \
           /home/boid/.local/share/boid/tls/ca.key'

# 5. daemon 再起動
./scripts/deploy-container.sh

# 6. 新 CA fingerprint + pair code → 再 pair
./scripts/deploy-container.sh --exec 'boid ca export' > /tmp/boid-ca.pem
./scripts/deploy-container.sh --exec 'boid web pair'
boid login https://localhost:8443 --ca-cert /tmp/boid-ca.pem --profile local

# 7. SecretStore 消費者 (workspace secrets 等) の re-provision
```

**効果**:
- podman/docker 双方対応 (deploy script 内で engine 検出済み)
- `compose.volume-only.yml` 選択も deploy script 側の統一 flag で
- container 名依存 (`container_daemon_1`) 撤去、 service 名 (`daemon`) で参照

### 副作用の因果

- device pair 全失効: **fresh DB により `web_devices` rows が無くなるため**
- session cookie 全部無効: `web_secret` 消失
- SecretStore rows 破棄: `secret.key` 消失 → 新 key では decrypt 不可
- 内部 mTLS 再発行: CA 消失 → 新 CA
- `install_id` 変わる: git-gateway cert scoping 再発行

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

**deadlock 回避**: callback は subscriber の lock 外、 daemon 側は subscriber slice iteration のみ、
consumer は `atomic.Pointer.Load()` で lock-free。

**web.public_url を dynamic とするなら SAN/leaf 更新も同 transaction に含める** (round 4 Major 5 指摘):

`web.public_url` の変更は §c の SAN / leaf renewal に影響する:
- Prepare 段階: 新 URL から SAN リストを derive、 現行と diff、 変更あるなら Commit で `LeafRenewer.UpdateHosts()`
- Commit: `LeafRenewer.UpdateHosts(newHosts)` → 次回 renewal cycle で新 SAN の leaf を issue
- 既存 connection は旧 leaf のまま (問題なし、 handshake だけ new)

もしくは **`web.public_url` を restart-required に降格** して SAN 更新を daemon restart 経路に (簡単、
実装コスト削減)。 実装 PR で決定。

### restart-required key の catalog

| category | keys |
|---|---|
| **dynamic** | `sandbox.allowed_domains`, `notify.command` (、 `web.public_url` は上記の判断次第) |
| **restart-required** | `gateway.forges.*`, `web.http_addr`, `gc.*`, `task_ask.*`, (`web.public_url` の場合) |
| **removed on volume-only** | `sandbox.backend` |

### PR-4 sub-PR 分割

上記 5 段。

### 未解決論点

- dotted path 構文 (array/map の扱い)
- `web.public_url` の dynamic vs restart-required の最終判断 (実装 PR で)

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
    operation          TEXT NOT NULL,               -- 9 operation 列挙 (下記 switch と対応)
    target_kind        TEXT NOT NULL,               -- workspace | project | hostcommands
    target_name        TEXT NOT NULL,
    desired_revision   INTEGER,
    target_url         TEXT,
    staging_path       TEXT,                        -- ".new" 側 (新 content の一時場所)
    backup_path        TEXT,                        -- ".old" 側 (旧 content の一時場所)
    target_path        TEXT,                        -- 最終 destination
    filesystem_step    TEXT NOT NULL DEFAULT '{}',  -- 完了済み step JSON
    terminal_status    TEXT,                        -- recovery 後の status (apply-error 等)
    error              TEXT
);
CREATE INDEX IF NOT EXISTS idx_apply_journal_incomplete
    ON apply_journal(completed_at) WHERE completed_at IS NULL;
```

**round 4 Blocker 2 対応**: `staging_path` (`.new`) と `backup_path` (`.old`) を **別 field に分離**
(round 3 の単一 `temp_path` は意味論混在で誤り)。

**operation 9 種 と restart 規則** (round 4 Blocker 2 対応で 4 → 9 全部):

```go
switch entry.Operation {
case "clone_new":
    // staging_path (`.<project>.git.new`) を削除、 operation 再実行
    os.RemoveAll(entry.StagingPath)
    markApplyPending(entry, "clone_new resume")

case "rename_old":
    // target_path が空 && backup_path (`.old`) 存在 → revert
    if !fileExists(entry.TargetPath) && fileExists(entry.BackupPath) {
        os.Rename(entry.BackupPath, entry.TargetPath)  // 旧 repo 復活
    }
    markApplyError(entry, "rename_old crash: reverted")

case "rewrite_hostcommands":
    // staging_path (`.new`) と backup_path (`.old`) の両方をチェック
    if fileExists(entry.TargetPath) && !fileExists(entry.StagingPath) {
        // target = 新、 staging 消失 = swap 完了、 backup 残り: backup 削除
        os.Remove(entry.BackupPath)
        return nil  // success
    }
    if fileExists(entry.BackupPath) && !fileExists(entry.TargetPath) {
        // target 消失、 backup あり: target を backup から restore
        os.Rename(entry.BackupPath, entry.TargetPath)
    }
    if fileExists(entry.StagingPath) {
        // staging 残り: 未完了 → 削除
        os.Remove(entry.StagingPath)
    }
    markApplyError(entry, "rewrite_hostcommands crash: restored")

case "write_initsh":
    // rewrite_hostcommands と同じ pattern
    // (staging/backup の別 field を使うので target 復元可能)
    // ... (rewrite_hostcommands と同じ論理)

case "delete_repo":
    // target が存在すれば削除完了とみなさない → 再削除
    if fileExists(entry.TargetPath) {
        os.RemoveAll(entry.TargetPath)
    }
    // journal を close (delete は idempotent)
    return nil

case "fetch":
    // fetch は idempotent、 次 dispatch で自動 fetch されるので journal だけ close
    // (crash 中に部分 fetch した objects は cache に残るが問題無し)
    return nil

case "rename_repo":
    // target_path 存在 → rename 完了、 staging (旧 path、 backup 相当) 消去確認
    if fileExists(entry.TargetPath) {
        os.RemoveAll(entry.StagingPath)  // 旧 path 掃除 (存在すれば)
        return nil
    }
    // target 消失、 staging (旧 path) 存在 → rename 中断、 staging を target に rename
    if fileExists(entry.StagingPath) {
        os.Rename(entry.StagingPath, entry.TargetPath)
        return nil
    }
    // 両方消失 → データ消失、 apply-error にして operator に通知
    markApplyError(entry, "rename_repo: both paths missing, data lost")

default:
    // 未認識 operation は fail-hard (round 4 Blocker 2 対応)
    return fmt.Errorf("unknown operation in apply_journal: %q for entry %s. "+
                       "Aborting startup — journal schema mismatch or corrupted entry.",
                       entry.Operation, entry.ID)
}
```

**注**: 全 operation で journal entry を close (`completed_at` set) するのは switch の後、 各 case が正常
return したときのみ。 `default` (未認識) は daemon abort。

**terminal_status field** の意味: recovery で apply-error や success を決めた status を記録 (再起動時に
「これは既に recovery 済み」の marker として使う、 double-recovery を防ぐ)。

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

    baseDir := filepath.Join(p.baseDir, name)      // 例: /home/boid/.local/share/boid/tls
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

    // 7. parent (baseDir) fsync (rename の durability)
    if err := fsyncDir(baseDir); err != nil {
        // rename は既に成功、 fsync 失敗は warning (data 損失なし、 crash 後 replay で復元)
        slog.Warn("baseDir fsync failed after symlink swap", "error", err)
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
- fsync error は propagate、 silent failure なし
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

**変更点** (round 4):
- `CleanupCheckout` を interface から削除 (`JobMountDescriptor.Cleanup` に統一)
- `Lock() -> unlock func()` を `AcquireLock` / `ReleaseLock` の 2 phase に (downgrade を Release+Acquire で表現)

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

- §a: auto-prune 撤去 + 4-status (ready/degraded/apply-pending/apply-error)、 additive migration、
  `ProjectRef` + `RepositoryCache` 経由 (`.WorkDir` audit 全 consumer 反映済み、 round 4 で 4 箇所追加)、
  一意性直列化 (DSN `_txlock=immediate` + workspace mutex + tx 分割)
- §b: behavioral subpath probe、 volume ownership 初期化 (init container、 訂正版 topology)、
  compose.yml の `name:` 明示 (round 4 Blocker 1)、 mirror clone + fetch/dispatch lock (interface 2-phase)、
  bare cache は cleanup 時 checkout dir 単純削除
- §c: CA cert OOB 経路 + pinned CA cert (`--ca-cert` 主経路、 fingerprint 経路は撤回)、 profile schema
  拡張 (`ca_cert_pem`)、 `ResolvedProfile.CACert` は `*x509.Certificate`、 HTTPS transport pool 生成は
  runtime、 `ServerOnlyTLSConfig(hosts...)` signature 正しい呼び出し、 **leaf renewal goroutine +
  `tls.Config.GetCertificate` 経路** (round 4 Blocker 3)、 SAN 決定 (public_url + localhost + IP)
- §d: break-glass procedure engine-agnostic (`deploy-container.sh --run/--exec` helper 経由)
- §e: PR 分割 additive/inert
- §f: `ConfigSubscriber` Prepare/Commit/Rollback、 immutable snapshot pointer、 active/desired merge
  (`applyToActive`)、 file write failure rollback (round 4 Major 5)、 `web.public_url` の dynamic vs
  restart-required は実装 PR で判断
- §g: apply_journal 完成版 (`staging_path`/`backup_path` 別 field、 9 operation 全部 restart 規則、
  未認識 operation fail-hard、 terminal_status field)、 apply-pending status を §a に反映
- §i: `SecretProvider.GetOrCreateMulti` を **generation directory + atomic symlink pointer** に
  (round 4 Major 6)、 fsync error propagation、 parent dir fsync。 `RepositoryCache` は
  `AcquireLock`/`ReleaseLock` の 2 phase、 `CleanupCheckout` を interface から削除
  (`JobMountDescriptor.Cleanup` に統一、 round 4 Major 7)。 `JobContext` 拡張 (round 3)

### 継続 未解決 (PR-1 着手前に individual に nose 判断)

- **論点 a**: fetch depth (mirror full vs partial)
- **論点 b**: branch policy との整合、 staging volume の disk 圧迫 / GC 統合、 podman
  VolumeOptions.Subpath の empirical 挙動 (behavioral probe で pin する形になった)
- **論点 c**: TLS listener HTTP/2 (別 PR)
- **論点 d**: k8s Secret provider の initContainer 契約
- **論点 e**: e2e coverage の container 移植、 `boid start` の CLI wrap 化
- **論点 f**: dotted path 構文、 secret env var 値の編集経路、 `web.public_url` の dynamic vs restart-required
- **論点 g**: `ContainerImage` default 化、 branch policy の workspace override、 forge auth の workspace scope
- **論点 i**: PVC accessMode 選定、 initContainer sequencing、 Service type (Phase 7 領域)

### Minor 未対応

現時点で未対応の Minor は無し (round 3/4 対応で全部本文 fix or 集約先移動済み)。

---

## codex review 対応 summary

### Round 1 (Blocker 4 / Major 6 / Minor 4) → Round 2 draft (1000 lines): 全対応 (2135367)
### Round 2 (Blocker 3 / Major 7 / Minor 5) → Round 3 draft (1525 lines): 全対応 (4a192c9)
### Round 3 (Blocker 3 / Major 9 / Minor 5) → Round 4 draft (1417 lines): 全対応 (36875a2)

### Round 4 (Blocker 3 / Major 9 / Minor 4) → Round 5 draft (本 doc)

**Blocker 3 件本文 fix**:
- 1: Compose volume 実名 → §b で `name:` 明示 + label ベース discovery fallback
- 2: apply_journal recovery 未完 → §g で `staging_path`/`backup_path` 別 field、 9 operation 全部 restart 規則、
  未認識 operation fail-hard、 terminal_status field
- 3: leaf renewal → §c で `LeafRenewer` goroutine + `tls.Config.GetCertificate` 経路、
  24h ごと monitoring + 7 日切ったら fresh issue + atomic swap

**Major 9 件本文 fix**:
- 1: TLS 疑似コード 3 箇所矛盾 → §c で fingerprint 経路撤回 (`--ca-cert` 主経路のみ)、 `*x509.Certificate`
  型統一、 `ServerOnlyTLSConfig(hosts...)` 正しい signature
- 2: BEGIN IMMEDIATE → §a で DSN に `_txlock=immediate` 追加 + network I/O を tx 外に (予約 tx + 確定 tx の
  2 段)
- 3: git symbolic HEAD update → §b で `ls-remote --symref origin HEAD` + `git symbolic-ref` の正しい経路、
  §i の interface を `AcquireLock`/`ReleaseLock` の 2 phase に (downgrade 表現可能)
- 4: init container topology → §b で `boid_private_config` 撤回、 `boid_private` volume 単一に統合 (XDG env
  で subpath 分離)、 brace expansion 撤回 (明示 path 列挙)
- 5: ConfigSubscriber file write failure → §f で file write 失敗時の rollback protocol 明記、 active/desired
  merge (`applyToActive(oldActive, newCfg.dynamicFields)`) 手順追加、 `web.public_url` の判断は実装 PR で
- 6: multi-file secret atomicity → §i で generation directory + atomic symlink pointer に変更 (`gen-N` +
  `current` symlink)、 fsync error propagate、 parent dir fsync 追加、 既存 non-empty finalDir の置換問題解消
- 7: `RepositoryCache.CleanupCheckout` interface 上残存 → §i で interface からも削除
- 8: `.WorkDir` audit +4 → §a で 4 consumer 追加 (`server/wire.go:274,1071` / `orchestrator/planner.go:129` /
  `dispatcher/runner.go:626`)
- 9: break-glass podman 対応 → §d で `deploy-container.sh --run/--exec` helper 経由に統一 (engine 検出 +
  compose service 名参照、 container 名固定撤去)

**Minor 4 件全対応**:
- 1: behavioral probe の `/probe/..` 判定 field → §b で `CanEscapeToVolumeRoot` 追加
- 2: 「継続 未解決」の同期 validate 残 → 削除 (本文で決定済み)
- 3: Minor 未対応の typo と port 明示 → 本文で修正済み、 未解決まとめから削除
- 4: summary の「compose exec」→ 「Compose service 経由」に統一 (「exec」と「run」使い分け明記)

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
