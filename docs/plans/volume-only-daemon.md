# volume-only daemon: Phase 6 の compose 部分の再設計

ステータス: **draft (2026-07-24 作成、実装未着手、 codex review round 1 反映済み)**。
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
- workspace / project 定義を **export/import YAML** で扱う (Kubernetes-like envelope、 workspace 内に
  project を nested)。
- config.yaml の編集は **CLI (`boid config ...`) + Web UI (`/settings`)** 経由 (host `vim` は不要)。
- userns backend / host daemon 起動経路は **新方式 cutover と同時に廃止** (段階撤去でなく単発切替、 §決定4
  由来の rollback 契約は撤回されるため段階撤去のメリットが消える)。
- daemon の永続 state を「daemon-only の private volume」と「job container に見せる staging area」に
  **明示的に分離** する (§論点 b で詳細、 codex Blocker 1 対応)。
- k8s 移行時に refactor の破壊が最小になる **seam を volume-only 実装段階で作る**
  (§論点 i、 codex Major 6 対応)。

### 非目的

- **k8s Helm chart 設計** — 本 doc は「compose deploy を host filesystem 非依存にする」までを扱う。
  k8s 用の Helm chart / operator は本 doc の作業が完了したあと (Phase 7 相当) で別途扱う。 ただし
  seam (§i) は本 doc で決める。
- **DB スキーマ変更 (schema-breaking)** — workspaces/projects の SQL schema 自体は現行のまま。
  ただし `projects.work_dir` は現行コード全体で「host filesystem 上の通常 checkout の絶対 path」の
  意味に広く使われているため (`grep -c work_dir internal/` = 29 usages)、 意味論を bare repo path に
  差し替える場合は **presentation 上 URL を表示**するよう変更する (§論点 a で詳細)。
- **secret rotation の実装** — 本 doc は on-first-boot generate のみ。
  rotation は現行の `boid web pair/revoke` (device credential 対象) では代替できない別問題:
  - `secret.key`: SQLite 内 secret store 全件の decrypt/re-encrypt が必要
  - `web_secret`: cookie 一斉 invalidation
  - CA: leaf 再発行、 旧 CA revoke/transition、 稼働 job との整合
  本 doc の scope 外で、 別 doc で扱う。 現状は「rotation 未提供、 対処必要な場合は volume 内 secret
  を手動削除 → 再生成」の運用契約になる。

---

## 全体像

```
[host user] --- CLI (HTTPS + Bearer token, decision 4 of cli-remote-connection.md)
                       \
                        +---> [daemon container] --- (engine socket via DooD) ---> [job container 1..N]
                       /       - private volume (daemon-only):
[host user] --- Web UI            /home/boid/.local/share/boid/{boid.db,secret.key,web_secret,tls/,install_id}
                (HTTP+WSS)     - staging volume (job-visible, size-bounded):
                                  /home/boid/staging/{spec/<job>/,tls/<job>/,broker-tls/<job>/}
                               - repo cache volume (workspace scoped, daemon-writable):
                                  /home/boid/repos/<workspace>/<project>.git   (bare)
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
[新規]  boid workspace export/apply YAML   ← 定義の source of truth
[新規]  boid config edit/get/set/apply     ← config.yaml を CLI 経由で編集
[新規]  CLI は既存 HTTPS + Bearer token (cli-remote-connection.md 決定 4/8) を default 経路化
[新規]  SecretProvider / RepositoryCache / JobMountDescriptor の 3 seam (§論点 i、 k8s 前準備)
```

---

## 論点 a: project モデル transition (dir → git URL)

### 現行

`boid project add <dir>` で host filesystem の既存 checkout を register する。
`projects.work_dir` に host absolute path を格納。 daemon 起動時に `project.yaml` を `work_dir/.boid/project.yaml`
から読む。 project.yaml 欠落時は `wire.go:226-249` で **hard delete** (今日の DB 全滅の直接原因)。

### 新方式

`boid project add <git-url> --workspace=<name> [--name=<project-name>]` で git remote URL を register する。
daemon は repo cache volume 内 `<repo_cache>/<workspace>/<project>.git` に **bare clone** を行い、
`projects.work_dir` の意味論は **URL への reference を持つ record identity** に変わる (詳細は下記
「work_dir の意味論変更」)。

`project.yaml` は bare repo の HEAD (default branch) から `git show HEAD:.boid/project.yaml` で読む。

### 事前 validation (add 時 vs dispatch 時)

`boid project add <git-url>` の同期処理:
1. URL syntax validation + slug/path traversal 検証 (`[a-z0-9][a-z0-9._-]*`)
2. workspace 存在確認
3. workspace 内 project name unique 検証
4. URL canonicalize (`git@host:owner/repo.git` ↔ `https://host/owner/repo.git` の正規化)
5. bare clone 実行 + `project.yaml` パース + schema validation
6. DB insert (以上すべて成功したときのみ commit)

**同期 validate**: nose 判断: 「add 時に fail-loud」が正しい (silent に broken project を DB に残さない)。
非同期にすると失敗の原因が add からズレて debug しにくくなる。

### auto-prune の撤去 (Blocker 4 対応)

`wire.go:226-249` の on-startup hard delete 経路は **撤去する**。 新方式では:

- DB row (`projects` テーブルの URL + workspace assignment) が **復元可能な source of truth**
- bare repo は **再 clone できる cache** (missing なら refetch)
- `project.yaml` 欠落 / parse error / auth failure / fetch failure と「DB row 削除」は **分離**

startup 挙動:
- bare repo が missing → **再 clone を試行** (auth 失敗などで clone できなければ project を
  `degraded` 状態にして起動継続、 dispatch は refused、 UI/CLI で表示)
- `project.yaml` の parse error → 同じく `degraded` 状態 (dispatch 拒否、 修復は git push で fix
  or 明示的 `boid project rm`)

明示的削除入口は `boid project rm` / `boid workspace delete` の 2 経路のみ。 startup logic からの
destructive delete は絶対に発火しない (自動ではリソースを消さない invariant)。

### CLI 契約変更

- `boid project add <dir>` → **removed** (dir 引数は受け付けない、 helpful error message で `<git-url>` 版を案内)
- `boid project add <git-url>` → **new** (workspace 指定必須、 project-name は URL から derive or `--name` で override)
- `boid project rm <id>` → 現行維持 + repo cache dir の削除 + 進行中 fetch/dispatch の中断
- `boid project list` → 現行維持、 出力は **URL 表示に切り替え** (`work_dir` の bare repo path は
  internal detail として非表示、 human-facing identity は URL に統一)
- `boid project fetch <id>` → **new** (bare repo の explicit refetch)

### `work_dir` の意味論変更 (Major 5 対応)

`projects.work_dir` は現行 internal/ 全体で 29 usages、 「host filesystem 上の通常 checkout の絶対 path」
の意味で広く参照される (dispatch 時の cwd、 project.yaml の base dir、 git operation の対象、 etc.)。

新方式では意味論を **完全に変える**が、 全 consumer の一斉書き換えは risk 大。 移行戦略:

1. **abstraction 導入**: `internal/orchestrator/repository/` に `RepositoryCache` interface を切り出す
   (URL → bare repo path、 fetch、 subclone、 lifecycle)。
2. **`projects.work_dir` の値**: 「URL の内部 canonical form」文字列にする (例: `git://<workspace>/<project>`)。
   これは filesystem path ではないので誤用時に immediate fail する (`os.Stat(work_dir)` が ENOENT で落ちる)。
3. **consumer 書き換え**: `work_dir` を filesystem path として使ってた 29 箇所を `RepositoryCache` 経由に
   置換。 grep + type check で機械的に検出可能。
4. **presentation**: `boid project list` / API response で URL を返す (`work_dir` の string 表現は
   `internal detail` の marker として非公開扱い)。

上記は §論点 b の `RepositoryCache` seam 導入 (Major 6) と同時実装。

### migration path

現行の project は「host checkout を dir 登録」だが、 新方式では意味論が違うため **auto-migration しない**。
既存 project は `boid project rm` で削除し、 新方式で `boid project add <git-url>` から register し直す。
nose の指示: 「boid のデータは揮発性許容、 消失は困らない」 = migration 経路を作り込まない判断。

### 開発者ワークフローへの影響

現行の「host に checkout した local branch (未 push) で agent に作業させる」ユースケースは **失われる**。
nose 判断: 「boid の作業と host 側 checkout が衝突する問題の方が大きい」 = 意図的に廃止。
未 push branch で作業したい場合は、 開発者が事前に push してから boid に投げる形になる。

---

## 論点 b: daemon 管理 state と job container への配送 (Blocker 1, 2 対応)

### 概要

daemon の永続 state は目的節の全体像図が示す通り 4 種類の volume に分離:

| volume | scope | 中身 | job container mount | k8s 対応 (§i 参照) |
|---|---|---|---|---|
| `boid_private` | daemon 専用、 job 見えない | `boid.db`, `secret.key`, `web_secret`, `tls/`, `install_id`, `config.yaml` | 不可 | PVC (RWO) + Secret |
| `boid_staging` | daemon-writable、 job-readable/writable (subpath 単位) | 各 job 用 spec/state/TLS material 一時領域 | 可 (job 単位で subpath mount) | PVC (RWX) or EmptyDir per-job |
| `boid_repos` | daemon-writable、 job-readable (subpath 単位) | workspace/project ごとの bare repo (cache) | 可 (job dispatch 時に project 単位で subpath mount) | PVC (RWO) + reference clone in job |
| `boid_homes_<workspace>` | workspace 単位、 job-writable | workspace HOME (Phase 4 の既存経路) | 可 (workspace 単位で丸ごと mount) | PVC (RWX or RWO) per-workspace |

### なぜ 4 分離か (Blocker 1 対応)

現行 container_backend の DooD 契約 (`container_backend.go:186-199, 1409-1433`):

> Launch is a DooD (docker-out-of-docker) backend: the container it creates is a SIBLING via the HOST's
> own docker daemon, not nested inside this daemon's own container, so a mount Source it hands the HOST's
> own docker daemon has to be a path the HOST filesystem actually has.

現行は `BOID_RUNTIME_DIR` を「source == target で host に bind」することでこれを回避してた。
volume-only 化するとその host-visible path が消えるので、 sibling job container に mount する
staging area を **daemon-owned named volume として、 job container に mount 可能な形で確保する**。

**選択肢と評価**:

| 案 | 内容 | pros | cons | 採否 |
|---|---|---|---|---|
| a) staging volume 分離 (採用) | daemon が管理する named volume の subpath を、 job container に mount する。 volume 名は daemon が engine API で取得、 subpath は per-job 一意 | mount 経路が clean、 docker/podman 共通の subpath 挙動、 spec/state/cert 全部同経路 | daemon-side で subpath 管理 (lifecycle GC) が必要 | ✓ |
| b) engine API (docker cp) | daemon が job container 生成後に `docker cp` で必要 file を container 内 filesystem に copy | 完全に isolation | 起動時 latency 追加 (spec/cert copy)、 running container への `cp` は copy-on-write の race がある | 却下 |
| c) 全 volume に host bind mount 併用 | volume を named + host bind の hybrid にして host path から sibling も参照可能 | 現行 architecture を最小変更 | 「host filesystem 依存廃止」の設計目標に完全に反する | 却下 |

**採用: 案 a (staging volume 分離)**。

### 実装詳細 (案 a)

1. `boid_staging` を compose.yml で declare (named volume)、 daemon container に `/home/boid/staging`
   として mount (rw)
2. daemon 起動時に engine API で `boid_staging` volume の **mountpoint (host filesystem 上の実体 path)** を
   取得: `docker volume inspect boid_staging | jq -r .[0].Mountpoint`
   - 取得結果: 通常 `/var/lib/docker/volumes/boid_staging/_data` (docker) or `~/.local/share/containers/storage/volumes/boid_staging/_data` (podman rootless)
   - この host path は sibling job container に bind mount 可能
3. daemon が job を dispatch する際:
   - `boid_staging/spec/<jobID>/runner-spec.json` に spec を書く (container 内 path)
   - sibling job container 生成時、 host 側から見た `{mountpoint}/spec/<jobID>/` を job container 内
     `/run/boid/spec/` として bind mount (docker/podman 双方 subpath 挙動サポート、 podman は `--mount type=volume,src=boid_staging,dst=/run/boid/spec,volume-subpath=spec/<jobID>`)
   - per-job TLS material も同経路 (`boid_staging/tls/<jobID>/`、 `boid_staging/broker-tls/<jobID>/`)
4. job 終了時 daemon が `boid_staging/spec/<jobID>/`, `.../tls/<jobID>/`, `.../broker-tls/<jobID>/` を削除

### staging volume の安全境界

- **DB / secret.key / CA private key / web_secret は絶対 staging に書かない** (私 volume `boid_private` 側で
  管理、 job container に mount しない)
- job container 側から staging の他 jobID subpath への read はできない (`volume-subpath` は subpath より
  外に出られない、 これは docker/podman の semantics)
- staging volume 内のファイル permission は daemon 側で厳密に (spec/state は 0644 fine、 TLS material は
  0600、 job container からは volume-subpath で mount するので owner mapping は Phase 6 と同じ (uid 1000))

### repository (bare repo) の job への提供 (Blocker 2 対応)

draft round-1 の「bare repo → workspace_volume 内 worktree 直 mount」は **撤回する**。 codex の指摘通り
3 つの致命的問題があるため:

1. **`git worktree add <branch>` は同一 branch を複数 worktree に checkout 不可** → 同一 branch の
   並行 dispatch が lock 競合で fail
2. **linked worktree の `.git` は bare repo 側 `worktrees/<id>` を参照** → subdir mount では bare repo
   metadata 解決できず job 内 `git status/commit/push` が全部 fail
3. **workspace HOME 直下に worktree 置くと** 同 workspace の他 job から read/write 可能、 現行の per-job
   fresh clone 隔離より弱くなる

代替: **「bare repo は fetch cache として daemon 側で保持、 job は cache から `git clone --reference` で独立 clone」**
現行 `runner/clone.go:49-53, 85-124` の per-job fresh clone 思想を維持する。

具体的な job dispatch フロー:
1. daemon が bare repo (`boid_repos/<workspace>/<project>.git`) に `git fetch --all` (up-to-date 化、
   auth は git gateway 経由)
2. staging volume 内 `boid_staging/checkouts/<jobID>/<project>/` を用意
3. daemon が job container 生成時、 initContainer 相当 or entrypoint 前段で:
   `git clone --reference <bare repo path> --dissociate <upstream URL> <checkouts/<jobID>/<project>/>`
   の後 `git checkout <branch>` (`--dissociate` で objects を独立化、 bare repo 削除の影響を受けない)
   - **注**: `--reference` は同 volume 内 (daemon と job container が同 subpath mount で bare repo を
     read できる) なので、 host mirror 参照とは違う経路 ([[git-gateway-clone-perf-local-mirror-idea-rejected.md]]
     で却下された経路とは別、 volume 内なので origin/* 追跡ブランチの伝播問題は発生しない)
4. job container は `/workspace/<project-name>/` として checkout 済み worktree を見る (現行 Phase 4 と
   同じ mount 経路)
5. job 終了時、 daemon が `boid_staging/checkouts/<jobID>/` を削除

**reopen 意味論**: 現行の「push 済みのみ保証」を維持 ([[container-based-boid-direction]] 参照)、 reopen 時は
`boid_staging/checkouts/<jobID>/` を消して再 clone する (現行と同じ)。

### project name / URL の validation (Major 5 対応)

- **project name slug**: `^[a-z0-9][a-z0-9._-]*$` (path traversal 排除、 大文字禁止、 dot-prefix 禁止)
- **workspace 内 project name 一意性**: DB unique constraint (`(workspace_slug, project_name)` composite unique)
- **URL canonicalization**: `NormalizeOriginURL` (現行 dispatcher 内、 host command 契約経由で defined)
  を通す。 例: `git@github.com:foo/bar.git` ↔ `https://github.com/foo/bar.git` は同一 canonical form に
- **workspace reassignment**: `boid project mv <id> --workspace=<new>` — bare repo は `boid_repos/<new>/<name>.git`
  に atomic rename、 進行中 fetch/dispatch はブロック (advisory lock)
- **rename**: 同様に `boid project mv <id> --name=<new-name>` — bare repo dir を rename、 DB update、 dispatch lock
- **`boid project rm` の cleanup**: bare repo dir 削除 + staging 内 in-flight checkouts の cancel + DB row 削除
- **同時 fetch/dispatch locking**: bare repo に対して `flock` (advisory) を daemon 側で持つ。 fetch と dispatch
  の checkout は shared lock、 rm/mv/rename は exclusive lock

### repo cache の GC

- `boid project rm` で該当 bare repo dir を削除
- `workspace delete` で workspace 配下の repo dir を一括削除
- `git gc` は daemon 側で日次 goroutine (現行の `runtimes/` GC と統合)
- disk 使用量の monitoring は `boid gc status` (新規) or Web UI で

### 未解決論点

- **branch policy** (`branch-policy-simplification.md`) との整合 — 現行は「project 単位 branch」で main/task
  branch 区別なし。 clone --reference 経由でも同じ policy が働くか (ローカル checkout は独立なので影響
  少ないはずだが要確認)
- **fetch depth**: 初回 clone を full にするか、 blob-filter partial clone (`--filter=blob:none`) で
  bandwidth 節約するか (別 evaluation)
- **staging volume の disk 圧迫**: per-job checkouts が積み上がる可能性 (job 終了時 delete するが failure
  path で delete 漏れると回収されない)、 startup reap (PR7 で入った経路) との統合

---

## 論点 c: CLI 到達経路 (既存 HTTPS + Bearer 契約を維持) (Blocker 3 対応)

### 契約の確認

draft round-1 の「mTLS + pair 済み client cert + cli-profiles.yaml」は **完全に撤回**。 codex の指摘通り、
Phase 3 で既に確立した契約 ([cli-remote-connection.md] 決定 4/8) を volume-only cutover でも維持する:

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
- **代わりに `https://localhost:<port>`** (daemon container が localhost に publish、 CLI は Bearer で通す)
- **remote 経路**: 変更なし (現行と同じ HTTPS + Bearer)

つまり全 CLI が **HTTPS + Bearer 一択** になる (unix socket 経路自体が消滅)。 これは compose stack の
`ports:` 定義で `127.0.0.1:<port>` に publish する形。

### 初回 pair の UX

[cli-remote-connection.md] 決定 7 の既定 UX をそのまま採用:

```bash
# 1. compose stack を上げる
./scripts/deploy-container.sh

# 2. daemon container 内で pair code 発行
docker exec container_daemon_1 boid web pair
# → 表示: pair code XXXXX-YYYYY (5 分有効)

# 3. host CLI で redeem
boid login https://localhost:8443
# → pair code を prompt で入力 → device token 交換 → ~/.config/boid/tokens/local.json 書き込み
# → ~/.config/boid/config.yaml の profiles.local に URL を書く
```

この pair は「container exec できる人 = 管理者、 認可の起点として自然」(nose 決定)。

### bootstrap 経路の loopback exemption

現行 `api_middleware.go:31-35, 76-87` の loopback bootstrap exemption は「device が 0 件 + 真の loopback
(proxy 通ってない)」の間だけ Web UI を通す仕組み。 volume-only では:

- 初回起動 (device 0 件) の Web UI アクセスは loopback exemption で通る (Web UI で pair code 発行できる)
- CLI 側は初回 pair 前は使えない (Bearer 必須) → decision 7 の `docker exec` 経由になる

これは既存挙動の維持で、 私の draft round-1 の「loopback trust default」提案は撤回する
(codex の指摘: host 別 user から到達可能 / peer address が必ずしも loopback ではない / 恒久 trust は安全でない)。

### profile bootstrap (init wizard との合流)

`./scripts/deploy-container.sh` は compose stack 起動のみ。 初回 pair は上記 `docker exec` 経路を
operator に案内する形。 `initwizard` (現行 `internal/initwizard/`) の enhancement は本 doc の scope 外
(別 doc で init wizard の compose deploy 対応)。

`~/.config/boid/config.yaml` に `default_profile: local` を毎回 seed する自動化は行わない (`boid login`
実行時に profile entry が書かれる、 それが `default_profile` の source of truth)。

### port 選定

現行 Web UI が `:8080` に listen (`web.default_addr`)。 CLI Bearer 経路も同じ port に相乗り (Bearer
middleware は既に共存実装済み)。 別 port には分けない (単一 port の方が operator にとってシンプル、
firewall 設定も 1 rule)。

**HTTPS 化**: 現行 Web UI は plain HTTP (localhost bind 前提)。 CLI Bearer 経路のために TLS 必須にする
かは別論点 (Phase 3 の cli-remote-connection.md 決定 4 で「`https://`」となってるので、 volume-only では
compose daemon が self-signed TLS cert で listen する形が自然、 CLI は初回 pair 時に cert fingerprint
を pin する)。 詳細は実装 PR で詰める。

### 未解決論点

- **`boid start` の意味論**: unix socket が消えるので、 現行 `boid start` (host daemon 起動) は使えない。
  CLI は `boid login` から始まる契約になる。 `boid start` を「compose stack up」の wrapper にするか、
  script のまま残すか (Major 1 対応で §論点 e に詳細)
- **auto-start trigger 経路の削除**: 現行 CLI が「socket が無ければ auto-start」する挙動があった
  ([[stale-boid-daemon-recurring]])。 volume-only では daemon は compose 経由で外部管理、 CLI から
  start できない → auto-start 経路自体を削除 (fail-fast にする)

---

## 論点 d: secret ライフサイクル (on-first-boot generate)

### 対象

- `secret.key` (**SecretStore の AES-256 master key**、 内部 secret store の decrypt/encrypt 用、
  `secret_keyfile.go:9` の `LoadOrCreateKey` で既に load-or-create)
- `web_secret` (Web session cookie signing key、 現行既に load-or-create)
- daemon internal CA (`tls/`、 broker / gateway / dockerproxy の TCP mTLS 用、 現行既に load-or-create)
- `install_id` (現行既に on-first-boot generate、 atomic write-temp+os.Link 経路 PR #822)

**codex の事実確認**: 上記全て既に「不在なら generate、 あれば load」の contract で実装済み。 私の draft
round-1 で書いた「新規実装が必要」の記述は誤りで、 実際には既存経路が host bind mount で読めないのが
原因だった (mode 0600 + subuid mapping 問題)。 volume-only では:

- volume owner = container 内 boid user (uid 1000)、 keep-id や podman override 無しでも identity
  mapping (named volume は container filesystem 上、 subuid 問題無し)
- 既存 `LoadOrCreateKey` / 対応する load-or-create 関数群が **そのまま volume 内で動く** (改修不要)

### 変更点

新方式で必要な変更:
- volume path が volume 内 (`/home/boid/.local/share/boid/*`) になるだけ
- `install_id`、 `secret.key`、 `web_secret`、 daemon CA いずれも既存の load-or-create をそのまま使用
- 追加改修は不要 (**Major 3 対応: 私の「新規実装コスト」記述は誤り**)

### rotation の non-goal 化 (Major 3 対応)

secret rotation は本 doc の scope 外 (「非目的」節参照)。 現状 rotation 未提供の帰結:

- **`secret.key` の rotate は SecretStore 全件の decrypt/re-encrypt が必要** — 現行未実装、 manual に
  volume 内 file 削除 → 全 secret 破棄 (regenerate は空 secret store から) が唯一の運用
- **`web_secret` の rotate は cookie 一斉 invalidation** — 現行未実装、 manual に file 削除 → 全 device 再 pair 要
- **CA の rotate は leaf 再発行 + 旧 CA revoke/transition + 稼働 job との整合** — 現行未実装、 manual に
  file 削除 → 全 device 再 pair 要 + 進行中 job の broker/gateway 疎通が切れる

**重要**: `boid web pair/revoke` は **device credential (bearer token)** を扱う経路で、 上記 secret material
の rotation とは別問題。 device revoke で secret rotate はできない (codex の Major 3 指摘の混同回避)。

rotation が必要になった段階で別 doc で扱う。 現状は「rotation 未提供、 必要時は volume 内 file 手動削除
→ 再起動 → 上記副作用を受け入れる」の運用契約 (dogfood 期間はこの制約で問題無い想定)。

### migration

現行 host daemon で generate 済みの secret material は **volume-only cutover 時に破棄** (新規 generate)。 副作用:

- 既存 device pair (Web UI + CLI) は **fresh DB により device rows が無くなるため** 全て invalidate → 再 pair 要
- `web_secret` 消失により既存 session cookie 全部無効 → 再ログイン要
- `secret.key` 消失により既存 SecretStore の secret 全部破棄 (中身が復号できなくなる)
- CA 消失により内部 mTLS 通信の cert 全部再発行 (daemon 起動時 auto、 稼働 job には影響なし = fresh install なので job も無い)
- `install_id` 消失で「同一 host = 同一 install」の identity 変わる → git-gateway cert scoping / reap
  label filter が新 install_id 基準で動き出す

nose 判断: 「boid のデータはクリティカルでない」 = 上記副作用は許容範囲。 開発者は初回 pair からやり直し。

### 未解決論点

- **k8s 移行時の secret 供給経路**: on-first-boot generate は開発環境で便利、 k8s では **initContainer で
  Kubernetes Secret から volume に copy** する経路も欲しい (§論点 i 参照)。 現在の「file が既にあれば読む、
  無ければ generate」 の contract は両経路をカバー可能 (initContainer が file 書けば load される)
- **atomic write-safety**: `install_id` は write-temp+os.Link で atomic 化済み (PR #822)。 `secret.key` /
  `web_secret` / CA は同様の atomicity が必要 (race での partial write を防ぐ)。 実装 PR で確認

---

## 論点 e: 現行 host daemon 経路 (userns backend) の廃止 (Major 1 対応、 PR 分割 additive/inert 化)

### 現状

- `cmd/start.go` の `runDaemonParent` (bare `boid start` の double-fork 経路) が host daemon 起動
- `internal/dispatcher/userns_backend.go` (userns backend) が sandbox 実行
- `internal/sandbox/runner/runner_linux.go` の syscall 経路
- `sandbox.backend` config option (`userns` | `container`)

これらは phase6-cutover-followups.md §②-④ で「dogfood 安定後に段階撤去」する予定だった。

### 新方式での扱い

**volume-only cutover と同時に一気撤去**。 段階撤去のメリットは「dogfood 期間中の rollback 契約」だが、
volume-only では host daemon への rollback 経路自体が成立しない (data の bind mount 契約が撤回されるため)。

### PR 分割案 (Major 1 対応: intermediate main を壊さない additive/inert 化)

draft round-1 の PR 分割案は「PR-1 landed 時点で compose deploy 操作不能」の状態を作っていた。 codex 指摘
を反映して additive/inert 化する:

**改定 PR 分割** (各 PR landed 時点で main が壊れない):

- **PR-1: seam の導入 (additive)**
  - `RepositoryCache` interface (§b) + `SecretProvider` interface (§i) + `JobMountDescriptor` interface (§i) の追加
  - 既存 code は変更なし (interface だけ切り出し、 現行 implementation を interface satisfy させる wrapper 追加)
  - 現行動作は不変、 追加 interface が使われるのは PR-3 以降
- **PR-2: volume-only compose stack (feature-gated)**
  - `compose.volume-only.yml` を新規追加 (現行 `compose.yml` は残す)
  - `deploy-container.sh` に `--mode=volume-only` flag 追加 (default = 現行の bind mount モード)
  - secret on-first-boot generate 経路の verify (既に load-or-create 済みだが staging volume との整合を含めて test)
  - CI (blackbox-e2e.yml) に `e2e-container-volume-only` job 追加 (`continue-on-error: true` で advisory)
- **PR-3: CLI Bearer 経路 default 化 + config CLI/Web UI**
  - `boid login` / `boid config edit/get/set/apply` の実装 (§c、 §f)
  - CLI が unix socket と HTTPS Bearer 両方サポート (現行契約維持、 default_profile 未 seed 時は unix 継続)
  - Web UI の `/settings` page
  - 現行動作不変 (`default_profile` を明示 seed するまで)
- **PR-4: project add URL 化 + workspace export/import** (feature-gated)
  - `boid project add <git-url>` の新経路 追加、 現行 `boid project add <dir>` は deprecation warning 出しつつ動く
  - `boid workspace export/apply` の実装 (§g)
  - `RepositoryCache` を使った bare repo 経路の実装 (§b)
  - 現行動作維持
- **PR-5: cutover (default 切替 + auto-prune 撤去)**
  - `deploy-container.sh` の default を volume-only に切替
  - `wire.go:226-249` の destructive auto-prune 撤去、 `degraded` 状態への遷移経路実装 (§a)
  - `boid project add <dir>` 経路を完全削除 (deprecation の履行)
  - `sandbox.backend` config option を撤去 (container 一択)
  - CI の `e2e-container-volume-only` を `continue-on-error: false` に格上げ
- **PR-6: userns backend + host daemon 経路撤去**
  - `internal/dispatcher/userns_backend.go` + `LocalRuntime` + `SandboxPreparer` 削除
  - `internal/sandbox/runner/runner_linux.go` + `internal/sandbox/plan.go` 削除
  - `cmd/start.go` の `runDaemonParent` 削除、 `boid start` は「compose stack 起動」の thin wrapper に
  - userns 固有 e2e scenario 削除 (container backend 相当が e2e-container-volume-only に揃ってから)

**各 PR landed 時点の main の可動性**:
- PR-1 landed: 現行動作不変、 seam interface が追加されただけ
- PR-2 landed: 現行動作維持 + volume-only mode を明示的に選ぶと動く
- PR-3 landed: 現行動作維持 + `boid login` で HTTPS Bearer 経路も使える
- PR-4 landed: 現行動作維持 + URL 版 project add も使える
- PR-5 landed: **default が切り替わる** (ここでの cutover が事実上の transition)
- PR-6 landed: userns 撤去、 単一 mode 化

### 未解決論点

- **既存 userns 経路の e2e coverage** の container-only 経路への移植状況の再確認 — attach ストリーム /
  resize 3 経路 / agent-stop signal / reap-before-reopen の container backend 版が揃っているか
- **`boid start` の意味論**: PR-6 で削除するとき CLI wrap にするか script のまま残すか

---

## 論点 f: config.yaml 編集経路 (CLI + Web UI) (Major 2 対応)

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

### Web UI

- `/settings` page (Templ + form) で以下を UI 化:
  - `sandbox.allowed_domains` (add/remove)
  - `gateway.forges.<forge>.host` / `.secret_key` (追加 / 削除)
  - `notify.command`
  - `web.public_url`
- YAML raw edit も可 (advanced tab、 monaco editor などで)

**削除**: `default_harness` (Major 2 対応、 codex 指摘の通り Phase 2.5 PR7 で撤去済み、 現行 `Config` に
存在しないので UI/CLI にも入れない)。

### validation

`boid config apply -f` / `boid config set` / `boid config edit` は保存前に schema validation を実施:
- required field の存在確認
- enum value の validity (現行の各 config field の validation を CLI 経路にも通す)
- allowed_domains の syntax チェック
- gateway.forges の各 forge 定義の完全性

validation error は human-readable な位置 + 理由付きで返す。

### reload semantics (実装コスト詳細、 Major 2 対応)

**現状の SIGHUP は不在** (daemon は SIGINT/SIGTERM のみ処理、 `grep syscall.SIG. cmd/start.go` で確認)。
config file の inotify 監視も現行実装されてない (fresh 起動時に一回 read で済ませてる)。

したがって dynamic reload は **新規実装が必要**、 内部設計は:

1. **config source of truth を daemon in-memory に**: 起動時に volume 内 config.yaml を read、
   `*config.Config` を lock 付きで保持
2. **reload API 追加**: `boid config apply/set/unset` は broker RPC 経由で daemon に到達、 daemon side が:
   - YAML AST preservation で file に書き戻す (unknown key / コメント保持)
   - atomic write (temp file + rename、 flock で lock)
   - revision counter を用意して concurrent update を検出 (ETag に相当)
   - in-memory `Config` を replace (lock 内で swap)
   - reload subscriber (allowed_domains → egress proxy、 notify.command → notify service、 etc.) に
     notify (channel or observer pattern)
3. **restart-required key の警告**: `gateway.forges.*` は dispatch 中の gateway TLS 再発行が絡むので
   restart 要 (実装コスト大なので v1 は restart-required 扱い)、 保存時に warning
4. **client-side profile file と daemon config の分離**: 現行 host CLI profile は `~/.config/boid/config.yaml`
   に混在 (Phase 3 の cli-remote-connection.md 決定 1)。 volume-only で daemon config が volume 内に
   移るため、 host 側 `~/.config/boid/config.yaml` は **client-only profile file** に純化する必要
   (daemon-side keys と client-side keys の split)

### 実装コストの再評価 (Major 2 対応)

上記を独立 PR 群に切ることを推奨:

- **PR-3.a**: config in-memory + reload API + YAML AST preservation (基盤)
- **PR-3.b**: `boid config` CLI サブコマンド (get/set/apply/edit)
- **PR-3.c**: `/settings` Web UI page
- **PR-3.d**: client/daemon config file split (host 側 profile file を分離)

PR-3 は単一 PR ではなく上記 4 段の連続 landed になる可能性 (§論点 e の PR 分割案では PR-3 を包括的に
描いてるが、 内部でさらに分解される)。

### reload semantics 表

| category | keys | 反映タイミング |
|---|---|---|
| **dynamic** | `sandbox.allowed_domains`, `notify.command`, `web.public_url` | reload 即時 (次 dispatch から反映) |
| **restart-required** | `gateway.forges.*` (dispatch 中の gateway TLS cert 再発行が絡む) | 保存 → next daemon restart で反映、 保存時に warning |
| **removed on volume-only** | `sandbox.backend` | 保存拒否 (エラー: "removed in volume-only cutover") |

### 未解決論点

- **key の nested type** (`sandbox.allowed_domains` は array、 `gateway.forges` は map) を dotted path で
  操作する構文設計 (`boid config set sandbox.allowed_domains[0] .freee.co.jp` か
  `boid config set sandbox.allowed_domains .freee.co.jp .notion.com` (multi-arg) か)
- **secret key の editing 経路**: `gateway.forges.<forge>.secret_key` は env var 名なので値自体は非機密、
  ただし env var の値 (実 token) は編集経路が別 (env / systemd unit / k8s Secret)、 CLI/Web UI の scope 外

---

## 論点 g: workspace/project export/import shape (Major 4 対応)

### shape (改定版、 codex 指摘の抜け fields 追加)

Kubernetes-like envelope (`apiVersion` / `kind` / `metadata` / `spec`) を採用。 draft round-1 で
`container_image` / `extra_repos` / `init.sh` / `host_commands.yaml` の抜けが codex に指摘された。 補完:

```yaml
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: default                          # 現行 workspaces.slug
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
  init_script: |                         # workspace HOME 初期化スクリプト (Phase 4 の init.sh 経路)
    #!/bin/bash
    # go/volta/claude/... の初期セットアップ (workspace 初回起動時 1 回だけ実行)
    ...
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
spec:
  commands:
    - name: atl
      path: /usr/local/bin/atl
      # ... (現行 host_commands.yaml と同じ shape)
    - name: gh
      path: /usr/local/bin/gh
      # ...
```

`HostCommands` kind は daemon-global (単一 instance)、 `boid workspace export --all -o all.yaml` で
Workspace 群と一緒に dump される (単一 file 内 `---` 区切りで複数 document)。

### `additional_bindings` の扱い

現行 `WorkspaceMeta.AdditionalBindings` は Phase 4 (home-workspace-volume) で退役済み ([workspace_meta.go:108])、
既に field も削除済み (silent-drop on YAML unknown key)。 volume-only では **shape に含めない**、 現行
DB に残ってる column は空 JSON array として persist される (既存挙動)。

**Minor 2 対応**: 私の draft round-1 で kits と additional_bindings を「現行維持」と書いた記述は誤り
(kits は Phase 2.5 PR6/PR7、 additional_bindings は Phase 4 PR4 で退役済み)。 該当記述削除。

### env の host path 依存

現行の workspace `env` には host filesystem path が大量に含まれている (例: `GOPATH: /home/nosen/go`)。
volume-only では container 内 valid path のみ有効:
- `GOPATH: /home/boid/go` (container 内 path、 workspace HOME volume 内)
- host tool の bind (`/home/nosen/.volta` 等) は不可、 相当機能は container image (workspace の container_image
  field) に焼き込む

export 時に host path を検出したら warning を出す (「この path は container 内で invalid」)。

### CLI API + apply 契約 (Major 4 対応)

```bash
# export
boid workspace export <name> -o workspace.yaml           # 単一 workspace + 属する projects
boid workspace export --all -o all-workspaces.yaml       # 全 workspace + HostCommands (---区切り)

# import (apply)
boid workspace apply -f workspace.yaml                   # 差分 apply (upsert)
boid workspace apply --dry-run -f workspace.yaml         # 変更内容 preview のみ (remote/auth validation は skip)
boid workspace apply --dry-run --check-remotes -f ...    # remote validation も含める (時間かかる)
boid workspace delete <name>                             # workspace + 属する project 全削除
```

### apply の詳細契約 (codex 指摘 6 点全解消)

1. **project identity/一意キー**: `(workspace_name, project_name)` の composite key で match。 URL は
   identity ではなく contents (URL 変更は「同一 project の URL 差し替え」として扱う)
2. **URL 変更時**:
   - **default (`reclone` mode)**: bare repo dir を rename (履歴 log)、 新 URL で `git clone --bare`、
     旧 dir は 24h 後 GC
   - `--url-change=fetch`: in-place で origin URL 変更 + `git fetch --all` (履歴保持、 URL 変更が
     mirror 系のみ想定)
   - `--url-change=reject`: エラーで拒否
3. **YAML に無い既存 project の扱い**: **default: 残す** (`--prune` flag で削除 opt-in)。 workspace
   単位の複数 apply で偶発的な削除を防ぐため conservative
4. **DB upsert と clone filesystem side effect の失敗回復**: DB は transaction、 filesystem 側の失敗は
   status を `apply-error` に落として fail-loud (partial state を残さない)
5. **concurrent workspace update**: workspace 単位で advisory lock (`flock`)、 revision counter (ETag 相当)
   で optimistic concurrency control
6. **`--dry-run` の scope**: default は schema validation + DB 変更 diff のみ (remote/auth は触らない)。
   `--check-remotes` で remote 疎通確認も含める (時間コスト明示)

### migration path (現行 5 workspaces)

nose との対話で確認済み: 現行 workspace 定義 (`bm-next` / `boid` / `default` / `khi` / `ubs`) の
`env` / `additional_bindings` は host path 依存が大量、 volume-only 化に伴い **clean start** で再構築。
volume-only cutover 時に:
1. 現行 DB から workspaces + projects を **手動 dump** (post-incident snapshot 経由 or fresh export)
2. 新方式 shape に nose が書き直し (container_image 選択、 env の container path 化、 init_script 記述)
3. 空 volume で fresh start → `boid workspace apply -f workspaces.yaml` で reimport

### 未解決論点

- **`ContainerImage` は workspace ごと必須にするか** (default で `boid-runner:latest` を採用するか)
- **project の branch policy** を workspace レベルで override するか (`spec.projects[].branch_policy`)
- **secret 参照**: `gateway.forges.<forge>.secret_key` は現行 workspace scope でない (daemon-global config)、
  workspace 単位で forge auth を分離するかは別論点 (今回 scope 外)

---

## 論点 h: 移行手順 (新方式単発切替)

### big-bang cutover の意味 (rollback 用語整理、 Minor 3 対応)

**「rollback path 無し」の正確な意味**: 新 state を保った切戻しは無い (volume-only の named volume が
書き込んだ state を旧 host daemon に引き継ぐ経路は無い)。

一方 **disaster fallback は存在する**: PR revert + volume-only stack を down + 旧 host daemon を再起動して
6-27 backup restore で「Phase 6 直前の状態に戻る」ことは可能。 これは deploy 時失敗の緊急回避策として
意識しておく (通常 rollback ではない、 fresh install 相当への切戻し)。

### タイムライン

1. **本 doc レビュー完了** (round 1 完了 → codex review 反映 round 2 → 本 doc landed)
2. **PR-1 (seam 導入) 実装 + landed** — 現行動作不変
3. **PR-2 (volume-only compose feature-gated) 実装 + landed** — 現行動作維持 + opt-in
4. **PR-3 (CLI Bearer default + config CLI/Web UI) 実装 + landed** — 現行動作維持 + 新経路 opt-in
5. **PR-4 (project add URL 化 + workspace export/import) 実装 + landed** — 現行動作維持 + 新経路 opt-in
6. **PR-5 (cutover: default 切替 + auto-prune 撤去) 実装 + landed** — ここで transition
7. **PR-6 (userns backend + host daemon 経路撤去) 実装 + landed** — 単一 mode 化
8. **手動 cutover 実施** — nose の host:
   - 現行 host daemon 停止
   - 現行 5 workspaces の設定を新方式 YAML で書き直す (kits 前提の re-design、 container_image 選択)
   - `./scripts/deploy-container.sh` で volume-only compose stack start
   - `boid workspace apply -f workspaces.yaml` で initial import
   - `boid login https://localhost:8443` で initial pair (decision 7 の `docker exec` pair code 経由)
   - `boid task list` で疎通確認

### 手動 cutover 中の risk (Minor 4 対応で因果を分離記述)

- **fresh DB による device pair 全部失効**: DB に device rows が無くなるため、 Web UI + CLI いずれも
  再 pair 要 (`docker exec ... boid web pair` → `boid login` で redeem)
- **web_secret 消失により session cookie 全部無効**: 既存 browser session が invalidate される (再ログイン要)
- **secret.key 消失により SecretStore 内 secret 全部破棄**: SecretStore に保存してた API token / OAuth
  refresh token 等は復元不可 (fresh から入れ直し)
- **CA 消失により内部 mTLS の cert 全部再発行**: 新 CA が起動時 auto generate、 稼働 job 無いので実害無し
- **install_id 変わる**: git-gateway cert scoping 再発行、 reap の label filter が新 install_id 基準で動く
- **CI 側 (blackbox-e2e.yml) の整合**: PR-2 で `e2e-container-volume-only` job 追加、 PR-5 で
  `continue-on-error: false` 格上げ、 PR-6 で userns 系 e2e scenario 削除

### Podman socket は cutover checklist で明示 (Minor 1 対応)

現行 `compose.yml:331-335` は `/var/run/docker.sock` 固定 bind。 podman-only host では既に問題化していたが、
本 doc の scope は **volume-only 化** のみで engine socket abstraction は含めない。 cutover checklist に:

> volume-only 化と engine socket abstraction (podman rootless の `/run/user/<uid>/podman/podman.sock` 対応)
> は別 PR で扱う。 本 doc の PR-2 以降を podman-only host で dogfood する前に、 engine socket abstraction
> の follow-up PR が必要。

を明記。 該当 follow-up は本 doc の scope 外 (今日 revert した fix/phase6-deploy-podman-socket branch の
考え方を新 stack に再適用する形になる)。

---

## 論点 i: k8s 移行時の seam (Major 6 対応、 新章)

### 目的

k8s Helm chart 実装 (Phase 7) は本 doc の scope 外だが、 **volume-only 実装段階で「k8s と 1:1 対応する
seam」を作っておく**ことで将来の refactor を最小化する。 codex 指摘: 「今の段階で `SecretProvider`、
repo/workspace materializer、 job mount descriptor の seam を作ると将来の fatal refactor を避けられる」。

### 3 つの seam

#### 1. `SecretProvider` interface

```go
type SecretProvider interface {
    // 名前付き secret material を byte で提供する。
    // 呼び出し側 (daemon 起動時 load 経路) は「無ければ generate、 あれば return」を意識せず、
    // provider 実装に委譲する。
    GetOrCreate(ctx context.Context, name string, generator func() []byte) ([]byte, error)
}
```

実装:
- **VolumeSecretProvider** (compose deploy): 現行 `LoadOrCreateKey` の思想を interface 化、
  `<data_dir>/<name>` を read、 無ければ generator 呼び出し + atomic write
- **KubernetesSecretProvider** (k8s deploy、 将来): initContainer で K8s Secret から
  `/etc/boid/secrets/<name>` に copy 済みという assumption、 daemon は GetOrCreate で file read
  (generator は fall back path、 通常発火しない)

#### 2. `RepositoryCache` interface

```go
type RepositoryCache interface {
    // URL を bare repo として materialize、 subclone に必要な reference path を返す。
    Materialize(ctx context.Context, workspace, project, url string) (referencePath string, err error)
    // bare repo に対して fetch --all を実行 (up-to-date 化)。
    Fetch(ctx context.Context, workspace, project string) error
    // 削除 (project rm / workspace delete)。
    Remove(ctx context.Context, workspace, project string) error
    // 一覧 (GC / status 表示用)。
    List(ctx context.Context) ([]Repository, error)
}
```

実装:
- **VolumeRepositoryCache** (compose deploy): named volume 内 `<repo_cache>/<workspace>/<project>.git`
  に bare clone
- **KubernetesRepositoryCache** (k8s deploy、 将来): PVC 経由 (RWX or per-workspace RWO)、 or
  init-job で bare clone を PVC に materialize

#### 3. `JobMountDescriptor` interface

```go
type JobMountDescriptor interface {
    // 与えられた jobID に対して、 job container に mount すべき volume / bind の記述を返す。
    // 実装は「daemon 側の staging volume の subpath を job container のどこに mount するか」を決める。
    DescribeMounts(ctx context.Context, jobID string) ([]MountSpec, error)
}
```

実装:
- **VolumeJobMountDescriptor** (compose deploy): named volume の subpath mount (docker/podman の
  `volume-subpath` 経路、 `/home/boid/staging/spec/<jobID>` → job container 内 `/run/boid/spec` など)
- **KubernetesJobMountDescriptor** (k8s deploy、 将来): PVC の subPath mount (K8s Pod spec の
  `volumes[].persistentVolumeClaim` + `volumeMounts[].subPath`)、 or EmptyDir で per-job 独立 volume

### k8s 1:1 mapping の精緻化 (codex Major 6 対応)

draft round-1 の「named volume → PVC 1:1」表現は正確ではない。 精緻化:

| compose deploy | k8s deploy |
|---|---|
| `boid_private` named volume (`/home/boid/.local/share/boid`) | PVC (RWO) — DB / CA private key 含むので node affinity or RWX + strict subPath isolation |
| `boid_staging` named volume | 選択肢 A: PVC (RWX、 daemon Pod と job Pod 共有)、 選択肢 B: 各 job Pod で EmptyDir (spec/cert を initContainer で seed) |
| `boid_repos` named volume | 選択肢 A: PVC (RWX、 daemon fetches, job reads via subPath)、 選択肢 B: job init-job で bare clone を EmptyDir に materialize |
| `boid_homes_<workspace>` | PVC per workspace (Phase 4 の workspace $HOME contract そのまま、 workspace 内 job は同 Pod) |
| secret material (`secret.key`, `web_secret`, CA) — on-first-boot generate | K8s Secret を initContainer で volume に seed、 daemon は generator を fall back として持つ |
| CLI listener (HTTPS + Bearer) | Service (ClusterIP or Ingress)、 Bearer は現行契約のまま |
| workspace HOME | PVC per workspace |

**注記**: PVC の subPath isolation は security boundary になる。 daemon Pod と job Pod が同 PVC を触る
経路では、 subPath の path 権限管理が critical (K8s subPath mount は subPath の外に breakout できない
挙動を提供、 これに依存する)。

### 実装 PR (§e の PR-1 相当)

PR-1: seam の導入 (additive) で:
- `internal/orchestrator/secret_provider.go` に `SecretProvider` interface + `VolumeSecretProvider` 実装
- `internal/orchestrator/repository/cache.go` に `RepositoryCache` interface + 空 impl (現行 dir 経路の
  wrapper)
- `internal/dispatcher/job_mount_descriptor.go` に `JobMountDescriptor` interface + 現行 bind mount 経路の
  wrapper

これらの interface が PR-3〜PR-5 の実装で使われ、 将来 `KubernetesSecretProvider` を実装するときに
compose 経路と共通の consumer を持てる形になる。

### 未解決論点

- **PVC の accessMode**: RWX が必要な volume (staging / repos) の選定、 storage backend の要件
- **initContainer と daemon Pod の起動 sequencing**: secret seed → CA load → daemon start の順序管理
- **Service type**: ClusterIP + Ingress か LoadBalancer か、 mTLS termination の場所

---

## 未解決の設計論点まとめ

各章の「未解決論点」を集約 (実装 PR 前に nose 判断を得るべきもの):

- **論点 a**: branch policy との整合、 fetch depth (full vs partial)
- **論点 b**: staging volume の disk 圧迫 / GC 統合、 clone --reference の branch policy 経路
- **論点 c**: HTTPS 化の cert pin 方式、 `boid start` の意味論、 auto-start trigger 削除
- **論点 d**: k8s Secret provider 経路の initContainer 契約、 atomic write の一貫化
- **論点 e**: e2e coverage の container 経路移植完了状況、 `boid start` の CLI wrap 化
- **論点 f**: dotted path 構文 (array/map の扱い)、 secret env var 値の編集経路
- **論点 g**: `ContainerImage` default 化、 branch policy の workspace override、 forge auth の workspace scope
- **論点 h**: (この章に固有の未解決論点は無し、 他章に集約)
- **論点 i**: PVC accessMode 選定、 initContainer sequencing、 Service type

これらは PR-1 着手前の設計 review round で individual に nose 判断を得る。

---

## codex review round 1 反映済み Blocker/Major/Minor 対応

**Blocker 対応 (全 4 件)**:
- 1: job container への state 配送 → §b で 4 volume 分離 + staging volume 経由の subpath mount 経路採用
- 2: bare repo → worktree の実行契約破綻 → §b で「bare repo は cache、 job は `git clone --reference` で
  独立 clone」に変更 (現行 fresh clone 思想維持)
- 3: CLI TLS/auth が親 doc と矛盾 → §c で「既存 HTTPS + Bearer 契約 (cli-remote-connection.md 決定 4/8) 維持」
  に完全書き換え、 mTLS / loopback trust default は撤回
- 4: destructive auto-prune の残置 → §a で「on-startup hard delete 撤去、 `degraded` 状態遷移で fail-loud、
  明示的削除入口は rm / delete のみ」を明記

**Major 対応 (全 6 件)**:
- 1: PR-1 landed 時 intermediate main が壊れる → §e で PR 分割を additive/inert に再設計 (PR-1〜PR-6 各段で
  main 可動性を維持、 default 切替は PR-5)
- 2: config live reconfiguration の実装コスト過小評価 + `default_harness` 撤去済み / SIGHUP 不在 の事実誤認 →
  §f で事実修正 + reload 実装 (config in-memory + reload API + AST preservation) を PR-3.a〜PR-3.d に分割
- 3: secret rotation と device revoke の混同 + `secret.key` が HMAC でなく AES-256 の誤認 → §d で
  事実修正 + rotation は non-goal で「未提供、 web revoke では代替不可、 別設計要」明記
- 4: workspace export shape 不完全 → §g で `container_image` / `extra_repos` / `init_script` / `HostCommands`
  kind 追加、 apply 契約 6 点全解消
- 5: project repository lifecycle 契約不足 → §a で slug/path validation、 URL canonicalization、
  reassignment、 rm cleanup、 concurrent lock、 `work_dir` 意味論変更 (`RepositoryCache` abstraction 経由の
  migration 戦略) 明記
- 6: k8s との 1:1 mapping 精緻化 → §i (新章) で `SecretProvider` / `RepositoryCache` / `JobMountDescriptor`
  の 3 seam を PR-1 で導入、 k8s deploy の各 volume 対応表を精緻化

**Minor 対応 (全 4 件)**:
- 1: Podman socket は cutover checklist で明記 → §h に「engine socket abstraction は別 PR、 podman host での
  dogfood 前に必須」明記
- 2: retired 済み概念 (kits / additional_bindings) の記述削除 → §b / §g で該当記述削除
- 3: rollback 用語矛盾 → §h で「新 state 保持切戻し無し / disaster fallback あり」に分離
- 4: secret regeneration の副作用因果を分離 → §d で web_secret / secret.key / CA / install_id 各項目
  別に因果を記述

---

## 参考リンク

- [phase6-container-backend.md](phase6-container-backend.md) — Phase 6 本体 (全 9 PR landed)、 §決定4 は本 doc で撤回
- [phase6-cutover-followups.md](phase6-cutover-followups.md) — 段階撤去計画、 本 doc の PR 群で吸収
- [container-based-boid.md](container-based-boid.md) — Phase 6 の前提となる移行戦略 ①-⑦、 大枠は継続
- [home-workspace-volume.md](home-workspace-volume.md) — Phase 4 の workspace $HOME volume、 論点 b の
  workspace HOME volume 経路で参照
- [cli-remote-connection.md](cli-remote-connection.md) — Phase 3 CLI リモート接続、 論点 c の HTTPS + Bearer
  契約は本 doc でも維持
- [workspace-db-consolidation.md](workspace-db-consolidation.md) — Phase 2.5、 `default_harness` / kits 撤去の
  経緯 (§f / §g で参照)
- `container-git-gateway-design` (memory) — git gateway 実装、 論点 b の bare repo fetch 経路で参照
- `phase6-dogfood-incident-and-pivot` (memory) — 本 doc の pivot 経緯記録
