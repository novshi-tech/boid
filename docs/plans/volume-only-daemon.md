# volume-only daemon: Phase 6 の compose 部分の再設計

ステータス: **draft (2026-07-24 作成、実装未着手)**。
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
  (host 側の事前 provisioning 不要、 現行 `install_id` の on-first-boot 挙動を全 secret に拡張)。
- workspace / project 定義を **export/import YAML** で扱う (Kubernetes-like envelope、 workspace 内に
  project を nested)。
- config.yaml の編集は **CLI (`boid config ...`) + Web UI (`/settings`)** 経由 (host `vim` は不要)。
- userns backend / host daemon 起動経路は **新方式 cutover と同時に廃止** (段階撤去でなく単発切替、 §決定4
  由来の rollback 契約は撤回されるため段階撤去のメリットが消える)。

### 非目的

- **k8s Helm chart 設計** — 本 doc は「compose deploy を host filesystem 非依存にする」までを扱う。
  k8s 用の Helm chart / operator は本 doc の作業が完了したあと (Phase 7 相当) で別途扱う。
- **DB スキーマ変更** — workspaces/projects の SQL schema 自体は現行のまま (project.work_dir だけは
  「host filesystem path」から「daemon-managed bare repo path」に意味論が変わる、 これは論点 b で扱う)。
- **secret rotation の UI 化** — 起動時 generate のみ扱う。 rotate/revoke の CLI/Web UI は既存の
  `boid web pair/revoke` を流用、 新規 API は今回の scope 外。
- **既存 host daemon 環境からの automated migration** — boid のデータは揮発性許容 (nose)、 移行は
  「host daemon 停止 → volume-only daemon fresh start → workspace/project を YAML から reimport」
  の手動手順で十分。

---

## 全体像

```
[host user] --- CLI (TCP profile) ------\
                                          \
                                           +---> [daemon container] ---> [job container 1..N]
                                          /       - named volume:
[host user] --- Web UI (HTTP + WSS) ----/            /home/boid/.local/share/boid
                                                     /home/boid/.config/boid
                                                     /home/boid/repos/<workspace>/<project>.git  (bare)
                                                   - secret.key/web_secret/tls/ は
                                                     on-first-boot generate
                                                   - host filesystem access は
                                                     podman/docker socket のみ
                                                     (job container 生成のため)
```

現行 (bind mount 経路) との差:

```
[削除]  ~/.local/share/boid/ を host bind mount
[削除]  ~/.config/boid/ を host bind mount
[削除]  host 側 project checkout dir を `boid project add <dir>` で register
[削除]  host daemon (userns backend) との socket 分離共存 / rollback 契約

[新規]  named volume 1 本 (daemon-owned) にすべて集約
[新規]  boid project add <git-url> --workspace=<name>   ← 引数意味論が変わる
[新規]  boid workspace export/apply YAML   ← 定義の source of truth
[新規]  boid config edit/get/set/apply     ← config.yaml を CLI 経由で編集
[新規]  CLI は TCP profile 経由 (localhost:<port> + mTLS)
```

---

## 論点 a: project モデル transition (dir → git URL)

### 現行

`boid project add <dir>` で host filesystem の既存 checkout を register する。
`projects.work_dir` に host absolute path を格納。 daemon 起動時に `project.yaml` を `work_dir/.boid/project.yaml`
から読む。

### 新方式

`boid project add <git-url> --workspace=<name> [--name=<project-name>]` で git remote URL を register する。
daemon は volume 内 `<data_dir>/repos/<workspace>/<project>.git` に **bare clone** を行い、
`projects.work_dir` は **daemon volume 内の bare repo path** を持つ (host filesystem を指さない)。

`project.yaml` は bare repo の HEAD (default branch) から `git show HEAD:.boid/project.yaml` で読む。
job dispatch 時は既存の worktree 経路 (Phase 4 の $HOME workspace volume) と integrate、
job container 側では従来通り `/workspace/<project-name>/` に worktree が checkout される (job 生成時に
`git worktree add` で必要 branch を切る)。

### CLI 契約変更

- `boid project add <dir>` → **removed** (dir 引数は受け付けない)
- `boid project add <git-url>` → **new** (workspace 指定必須、 project-name は URL から derive or `--name` で override)
- `boid project rm <id>` → 現行維持
- `boid project list` → 現行維持 (work_dir 表示は「host path」から「daemon-managed bare repo path」に見える文字列だけ変わる)

### migration path

現行の project は「host checkout を dir 登録」だが、 新方式では意味論が違うため **auto-migration しない**。
既存 project は `boid project rm` で削除し、 新方式で `boid project add <git-url>` から register し直す。
nose の指示: 「boid のデータは揮発性許容、 消失は困らない」 = migration 経路を作り込まない判断。

### 開発者ワークフローへの影響

現行の「host に checkout した local branch (未 push) で agent に作業させる」ユースケースは **失われる**。
nose 判断: 「boid の作業と host 側 checkout が衝突する問題の方が大きい」 = 意図的に廃止。
未 push branch で作業したい場合は、 開発者が事前に push してから boid に投げる形になる。

### 未解決論点

- **project.yaml の validate タイミング**: `boid project add <git-url>` 時点で bare clone → project.yaml 読解 →
  validation まで同期でやるか、 clone は非同期 + validation は初回 dispatch 時か。 現行の on-startup validate
  (auto-prune 経路の起点) を維持するか。
- **bare repo の initial fetch depth**: 全 branch 全 tag を fetch する (現行 clone 相当) か、 shallow / partial
  clone (blob-filter) で bandwidth 節約するか。 論点 b と絡む。

---

## 論点 b: daemon 管理 bare repository

### 場所

named volume 内 `<data_dir>/repos/<workspace-slug>/<project-name>.git/` に bare clone を配置する。
`<data_dir>` は現行 `~/.local/share/boid` に相当 (container 内 `/home/boid/.local/share/boid`)。

layout 例:
```
<data_dir>/
├── boid.db
├── secret.key
├── web_secret
├── install_id
├── tls/
├── kits/                     (現行維持 — kits は Phase 6 で退役予定だが移行期は共存)
├── homes/<workspace>/         (Phase 4 の workspace HOME volume 経路、 現行維持)
└── repos/
    ├── default/
    │   ├── rook-server.git/
    │   ├── mera-ui.git/
    │   └── ...
    ├── boid/
    │   ├── boid.git/
    │   └── boid-kits.git/
    └── ...
```

### fetch 経路

初回 register 時に `git clone --bare <git-url> <data_dir>/repos/<workspace>/<project>.git`。
以降は job dispatch 時 or `boid project fetch <id>` で `git fetch --all` を実行。
実装は既存の git gateway 経路 ([container-git-gateway-design.md]) を活用 (auth は既存の gateway.forges 経由)。

### job container との integrate

job dispatch 時のフロー (現行 Phase 4 + Phase 6):
1. daemon が bare repo から必要 branch を fetch (up to date 化)
2. workspace 用 named volume に対して `git worktree add <workspace_volume>/repos/<project>/<worktree-id> <branch>` で worktree 追加
3. job container を start、 `/workspace/<project-name>/` に worktree を bind mount (workspace_volume 内 sub-path として)
4. job 終了時に `git worktree remove` (branch 情報だけ bare repo に残る)

現行 Phase 4 の workspace $HOME volume は「workspace 単位の永続 $HOME」だが、 その中に project ごとの
worktree area を含める形にできる (workspace_volume 内の `/repos/<project>/` サブディレクトリ)。

### fetch cache 戦略

- 同一 workspace 内の複数 project は同じ workspace_volume に worktree を切るので、 project 間で objects
  DB を共有できる可能性 (`git worktree` は同一 bare repo 内なら objects 共有、 別 project 間は別 bare repo なので共有できない)
- bandwidth 節約したい場合は blob-filter partial clone (`git clone --filter=blob:none`) を検討 (別途 evaluation)
- reopen 意味論は既に「push 済みのみ保証」で確定 ([[container-based-boid-direction]] 参照)、 bare repo 経路
  でも変わらない

### 未解決論点

- **branch policy** (`branch-policy-simplification.md`) との整合 — 現行は「project 単位 branch」で main/task branch
  区別なし。 bare repo でも同じ policy が働くか。
- **workspace_volume 内の worktree area の GC** — 現行 kit go-dev の rw bind と同じで、 job 単位の worktree を
  removed した後に objects が残る (git gc タイミングをどうするか)。 workspace_volume の disk 使用量が
  monotonic に増える懸念。
- **peer clone (`--reference`)** ([[git-gateway-clone-perf-local-mirror-idea-rejected.md]]) の適用可否 — 却下されたのは
  「host mirror を参照 clone する」経路。 daemon volume 内 bare repo → worktree なら該当しない
  (同一 volume 内なので `--reference` 経由の origin/* 追跡ブランチ問題は発生しない)。

---

## 論点 c: CLI 到達経路 (socket → TCP profile default 化)

### 問題

named volume 化すると daemon socket (`$XDG_RUNTIME_DIR/boid.sock`) が **host から見えなくなる**
(volume は container-internal filesystem)。 現行 CLI は unix socket 直接 dial 前提のため、
host 側 `boid task list` が届かない。

### 解決策

Phase 3 (CLI リモート接続、 [cli-remote-connection.md]) で確立した **TCP profile 経路を default 化**。

- 現行 profile は「remote daemon 接続」用 opt-in の位置付け ([[next-session-cli-remote-connection]] 参照)
- 新方式では、 compose daemon がデフォルトで localhost:<port> に mTLS TCP listener を持つ (Phase 3 の broker/gateway
  TCP wire (PR #825) を CLI にも delegation)
- host 側 CLI は `~/.config/boid/cli-profiles.yaml` (host 側の localhost bind path) で TCP profile を default に
  (boid CLI が pair 済み cert を持つ形)
- 初回 pair は Web UI の QR/link 経路 ([[project-web-sessions]]) と同じ機構 (localhost からは loopback trust で
  pair 不要にする option もある、 論点は下記)

### 開発 UX

```bash
# 現行:
boid task list                # socket 直接 dial

# 新方式:
boid task list                # 内部で TCP profile 経由 (透過)
```

profile 選択は既存の `--profile` flag / `BOID_PROFILE` env / `default_profile` config で。 ローカル開発では
`default_profile: local` (localhost:8443 相当) を initwizard で seed。

### loopback trust

localhost からの接続を **mTLS 不要で通す** loopback trust mode を defaults にするか、 常に mTLS 必須にするか。
- loopback trust 賛成: 開発 UX 簡単 (pair 手続き不要)、 localhost binding は host user 以外届かない
- mTLS 必須賛成: 契約の均質性 (localhost / remote で挙動同じ、 マルチユーザ host での分離)

**推奨**: loopback trust mode を default とし、 opt-in で strict mTLS に上げられる形。 nose 判断待ち。

### 未解決論点

- **profile bootstrapping**: 初回起動時に `default_profile: local` を自動 seed するのは init wizard ([[project-kit-init-skill-plan]])
  との合流点 (initwizard で TCP profile 生成 → pair 完了まで一発)
- **port 選定**: 現行 Web UI (`8080`) と共用するか、 daemon-rpc 用に別 port か。 別 port の方が「Web UI を落として
  も CLI 生きる」で運用しやすいが、 単一 port の方が operator にとってシンプル
- **compose 停止時の CLI 挙動**: 現行は socket 無しで即エラー、 新方式では TCP 到達失敗 → 「daemon が起動して
  ないよ」エラー。 auto-start 経路 ([[stale-boid-daemon-recurring]] の警戒対象) は volume-only 化で自然消滅
  (daemon = compose service なので CLI から start できない)

---

## 論点 d: secret ライフサイクル (on-first-boot generate)

### 対象

- `secret.key` (HMAC 用、 install_id / session token 生成)
- `web_secret` (Web session cookie signing)
- daemon internal CA (`tls/`、 broker / gateway / dockerproxy の TCP mTLS 用)
- `install_id` (現行既に on-first-boot generate)

### 現行

`~/.local/share/boid/` に file として置かれ、 mode 0600 で保護。 host bind mount で daemon container から
読む。 dogfood インシデントの 2 段目で顕在化した「container からは owner mismatch で読めない」問題は
volume-only 化で消える (volume owner = container 内 boid user、 identity で読める)。

### 新方式

**空 volume 起動時に daemon が generate**、 現行 install_id と同じ挙動 (`install_id` は既に atomic write-temp+os.Link
経路で on-first-boot generate、 PR #822 で実装済み)。 実装は各 secret の初期化ロジックを:

```go
// 疑似コード
func loadOrGenerateSecret(path string, generator func() []byte) ([]byte, error) {
    if data, err := os.ReadFile(path); err == nil {
        return data, nil
    }
    data := generator()
    // atomic write-temp + os.Link で publish
    return data, atomicWriteFile(path, data, 0600)
}
```

これを `secret.key` / `web_secret` / daemon CA (private key part) に適用。 CA cert 部分は既存の
`internal/mtls/ca.go` の generate 経路をそのまま使う。

### migration

現行 host daemon で generate 済みの secret material は **volume-only cutover 時に破棄** (新規 generate)。
副作用:
- 既存 device pair (Web UI) はすべて invalidate → 再 pair 要
- 既存 session cookie も invalidate → 再ログイン要
- install_id は変わる → 「同一 host = 同一 install」の identity が変わる (現行 install_id を参照するもの:
  git-gateway の cert scoping、 reap の label filter)

nose 判断: 「boid のデータはクリティカルでない」 = 上記副作用は許容範囲。 開発者は再 pair する。

### 未解決論点

- **k8s 移行時の secret 供給経路**: on-first-boot generate は開発環境で便利、 k8s では initContainer で
  Secret を pre-seed する経路も欲しい可能性。 「file が既にあれば読む、 無ければ generate」 の contract は
  両方カバー可能 (initContainer が file 書けば load される)
- **rotate**: 本 doc では扱わない。 別途 `boid secret rotate` 相当の CLI/Web UI が必要になる (revoke API は
  現行 `boid web revoke` があるが、 secret.key 自体の rotate は未対応)

---

## 論点 e: 現行 host daemon 経路 (userns backend) の廃止

### 現状

- `cmd/start.go` の `runDaemonParent` (bare `boid start` の double-fork 経路) が host daemon 起動
- `internal/dispatcher/userns_backend.go` (userns backend) が sandbox 実行
- `internal/sandbox/runner/runner_linux.go` の syscall 経路
- `sandbox.backend` config option (`userns` | `container`)

これらは phase6-cutover-followups.md §②-④ で「dogfood 安定後に段階撤去」する予定だった。

### 新方式での扱い

**volume-only cutover と同時に一気撤去**。 段階撤去のメリットは「dogfood 期間中の rollback 契約」だが、
volume-only では host daemon への rollback 経路自体が成立しない (data の bind mount 契約が撤回されるため)。
段階撤去する意味が消えるので、 fresh cutover PR で以下を一度に削除:

- `cmd/start.go` の bare `boid start` daemon 起動経路 → foreground-only (compose 前提)
- `internal/dispatcher/userns_backend.go` + `LocalRuntime` + `SandboxPreparer`
- `internal/sandbox/runner/runner_linux.go` (userns syscall 経路) + `internal/sandbox/plan.go`
- `sandbox.backend` config option (container 一択なので廃止、 config parse で unknown key 扱い)
- userns 固有 e2e scenario (`docker-proxy-*`、 `git-gateway-*` で `requires-sandbox` marker があるもの) →
  container backend 相当が e2e-container job に揃ってから削除

### 実装順序 (PR 分割案)

**PR 分割案 A: 一発 cutover** (`chore/volume-only-cutover` 1 PR、 巨大):
- pros: rollback 契約撤回が atomic
- cons: PR review が困難、 CI failure 時の切り分け困難

**PR 分割案 B: 段階的 (compose 変更が先、 userns 撤去は後)**:
1. **PR-1**: volume-only compose stack + secret on-first-boot generate + config CLI/Web UI + workspace export/import (これで新方式で動く)
2. **PR-2**: `boid project add` の意味論変更 (dir → git URL) + bare repo 経路 (新方式でしか project 使えなくなる)
3. **PR-3**: CLI TCP profile default 化 + loopback trust
4. **PR-4**: userns backend + host daemon 経路 + `sandbox.backend` option 撤去
- pros: 各 PR が review 可能サイズ、 CI regression の切り分け容易
- cons: PR-3 完了までは userns backend が中途半端に生きてる状態

**推奨**: 案 B (段階的)。 PR-1 の landed 時点で volume-only stack は動くが host daemon 経路も生存、
PR-4 で userns を潰して single-mode 化。

### 未解決論点

- **既存 userns 経路の e2e coverage** の container-only 経路への移植状況の再確認 — attach ストリーム /
  resize 3 経路 / agent-stop signal / reap-before-reopen の container backend 版が揃っているか
- **`boid start` の意味論**: 撤去後、 `boid start` は「compose stack の up 相当」にする (script を CLI に取り込む) か、
  compose ラッパは script のまま残すか

---

## 論点 f: config.yaml 編集経路 (CLI + Web UI)

### 問題

named volume 内 config.yaml は host `vim` で編集できない (`podman exec ... vim` は possible だが摩擦大)。
nose の指摘: 「編集しにくくなるから、 CLI/Web UI で編集可能にする必要がある」。

### CLI API

```bash
# 全体を dump / apply
boid config get                             # 全 config を YAML で stdout
boid config apply -f config.yaml            # file から apply (validation + reload)

# key-level
boid config get sandbox.allowed_domains     # dotted path で individual key
boid config set sandbox.allowed_domains ".freee.co.jp" ".notion.com"   # scalar/array set
boid config unset gateway.forges.github.secret_key                     # key 削除

# EDITOR 経由
boid config edit                            # $EDITOR で開く → 保存で validate + reload
```

### Web UI

- `/settings` page (Templ + form) で以下を UI 化:
  - `default_harness`
  - `sandbox.allowed_domains` (add/remove)
  - `gateway.forges.<forge>.host` / `.secret_key` (追加 / 削除)
  - `notify.command`
  - `web.public_url`
- YAML raw edit も可 (advanced tab、 monaco editor などで)

### validation

`boid config apply -f` / `boid config set` / `boid config edit` は保存前に schema validation を実施:
- required field の存在確認
- enum value の validity (例: `default_harness` は claude/codex/opencode のいずれか)
- allowed_domains の syntax チェック
- gateway.forges の各 forge 定義の完全性

validation error は human-readable な位置 + 理由付きで返す (現行の config.Load の error より詳細)。

### reload semantics

config の key ごとに **dynamic reload 可能 vs restart-required** を分類:

| category | keys | 反映タイミング |
|---|---|---|
| **dynamic** | `sandbox.allowed_domains`, `default_harness`, `notify.command`, `web.public_url` | reload 即時 (次 dispatch から反映) |
| **restart-required** | `gateway.forges.*` (dispatch 中の gateway TLS cert 再発行が絡む) | 保存 → next daemon restart で反映、 保存時に warning |
| **removed on volume-only** | `sandbox.backend` | 保存拒否 (エラー: "removed in volume-only cutover") |

`boid config set` / `apply` は変更 key が restart-required だった場合に:
```
[warning] gateway.forges.github.secret_key requires daemon restart to take effect.
          Restart with: docker compose -f build/container/compose.yml restart daemon
```
を出す。 dynamic は silent に reload (info log は出す)。

### 実装

- daemon 内部で config の source of truth は「volume 内 config.yaml」
- `boid config *` CLI は broker RPC 経由で daemon に到達 (現行の CLI → daemon RPC 経路)
- reload の trigger は既存の SIGHUP + inotify or 明示 API (`daemon reload config`)
- Web UI は同じ RPC を叩く

### 未解決論点

- **key の nested type** (`sandbox.allowed_domains` は array、 `gateway.forges` は map) を dotted path で
  操作する構文設計 (`boid config set sandbox.allowed_domains[0] .freee.co.jp` か
  `boid config set sandbox.allowed_domains .freee.co.jp .notion.com` (multi-arg) か)
- **secret key の editing 経路**: `gateway.forges.<forge>.secret_key` は env var 名なので値自体は非機密、
  ただし env var の値 (実 token) は編集経路が別 (env / systemd unit / k8s Secret)、 CLI/Web UI の scope 外

---

## 論点 g: workspace/project export/import shape

### 目的

- workspace 定義 + それに属する project 群を **1 file の YAML** で dump/apply できる
- 他環境 (別 host / k8s cluster) への引き継ぎ、 backup 経路、 fresh install からの provisioning に活用

### shape

Kubernetes-like envelope (`apiVersion` / `kind` / `metadata` / `spec`) を採用。 scratchpad の
`proposed-workspace-export-shape.yaml` 参照。

要点:

```yaml
apiVersion: boid.dev/v1        # 将来の schema evolution 用の namespace
kind: Workspace
metadata:
  name: default                # 現行 workspaces.slug に相当

spec:
  host_commands:               # Phase 5 の host_commands 経路
    - atl
    - gh
  env:                         # container 内 job container の env vars
    ATL_SITE: ubs
    DOTNET_CLI_TELEMETRY_OPTOUT: "1"
  allowed_domains: []          # egress proxy allowlist (workspace 加算)
  capabilities:
    docker: {}                 # workspace-level capability toggles

  # ↓ workspace 内 project 定義 (nose 提案の要)
  projects:
    - name: rook-server
      url: git@bitbucket.org:Aolani-ondemand/rook-server.git
    - name: mera-ui
      url: git@bitbucket.org:Aolani-ondemand/mera-ui.git
    - name: blanc-db-if
      url: git@bitbucket.org:Aolani-ondemand/blanc-db-if.git
```

複数 workspace は YAML の `---` 区切りで 1 file にまとめられる (`kubectl apply -f` と同じ)。

### CLI API

```bash
# export
boid workspace export <name> -o workspace.yaml
boid workspace export --all -o all-workspaces.yaml       # 全 workspace + project

# import (apply)
boid workspace apply -f workspace.yaml                   # 差分 apply (upsert)
boid workspace apply --dry-run -f workspace.yaml         # 変更内容 preview のみ
boid workspace delete <name>                             # workspace + 属する project 全削除
```

### `additional_bindings` の扱い

現行 `workspaces.additional_bindings` は Phase 4 で退役方向。 volume-only では host filesystem に触らない
前提なので、 export shape には **含めない** (schema から除去)。 現行 DB に残ってる `additional_bindings`
は import 時に silent drop (validation warning は出す)。

### env の host path 依存

現行の workspace `env` には host filesystem path が大量に含まれている (例: `GOPATH: /home/nosen/go`、
`PATH: /home/nosen/.local/bin:${PATH}`)。 volume-only では container 内 valid path のみ有効:
- `GOPATH: /home/boid/go` (container 内 path)
- host tool の bind (`/home/nosen/.volta` 等) は不可、 相当機能は kits 経由 (image layer) で提供

export 時に host path を検出したら warning を出す (「この path は container 内で invalid、 kits 経由に変換要」)。

### migration path (現行 5 workspaces)

nose との対話で確認済み: 現行 workspace 定義 (`bm-next` / `boid` / `default` / `khi` / `ubs`) の
`env` / `additional_bindings` は host path 依存が大量、 volume-only 化に伴い **clean start** で再構築。
volume-only cutover 時に:
1. 現行 DB から workspaces + projects を **手動 dump** (post-incident snapshot 経由 or fresh export)
2. 新方式 shape に nose が書き直し (kit 選択の再検討、 env の container path 化)
3. 空 volume で fresh start → `boid workspace apply -f workspaces.yaml` で reimport

### 未解決論点

- **project の branch policy** を workspace レベルで override するか (`spec.projects[].branch_policy`) —
  現行 `branch-policy-simplification.md` の workspace scope 動作を確認要
- **capabilities.docker** の scope — workspace ごとに opt-in するか、 全 workspace default で有効か
- **secret 参照**: `gateway.forges.<forge>.secret_key` は現行 workspace scope でない (daemon-global config)、
  workspace 単位で forge auth を分離するかは別論点 (今回 scope 外)

---

## 論点 h: 移行手順 (新方式単発切替)

### big-bang cutover の理由

- rollback 契約は §決定4 と一体、 §決定4 撤回で rollback 契約も撤回されるため段階撤去のメリット消失
- boid のデータは揮発性許容 (nose)、 「fresh start でも困らない」
- 中間状態を保つ運用オーバーヘッド (compose daemon + host daemon 並存など) を作らない

### タイムライン

1. **本 doc レビュー完了** (今日 nose レビュー) → confirmed
2. **PR-1 実装 + landed** (volume-only compose + secret generate + config CLI + workspace export/import)
3. **PR-2 実装 + landed** (`boid project add <git-url>` 化 + bare repo 経路)
4. **PR-3 実装 + landed** (CLI TCP profile default 化)
5. **PR-4 実装 + landed** (userns backend + host daemon 経路 + `sandbox.backend` option 撤去)
6. **手動 cutover 実施** — nose の host:
   - 現行 host daemon 停止
   - 現行 5 workspaces の設定を新方式 YAML で書き直す (kits 前提の re-design)
   - `./scripts/deploy-container.sh` で volume-only compose stack start
   - `boid workspace apply -f workspaces.yaml` で initial import
   - `boid task list` で疎通確認

### 手動 cutover 中の risk

- **secret 全 regen** = 既存 device pair 全 invalidate → 全 device 再 pair 要 (Web UI で pair 手順再実施)
- **install_id 変わる** = git-gateway cert scope 再発行 (自動、 daemon 起動時)
- **既存 project の task/job 履歴消失** = 現行 DB は volume 外なので参照不可 (post-incident snapshot は残るが
  参照するには DB 直接 SELECT)
- **CI 側 (blackbox-e2e.yml)** は既に container backend を e2e-container job で走らせてるので、
  volume-only 化に伴う compose.yml 変更が e2e-container の setup と噛み合う必要がある (script が bind mount
  path を扱わなくなる)

### rollback

**rollback path 無し** (data 経路が bind mount とは別のため、 旧 host daemon に切戻しても新方式で
書いた state は見えない)。 「fresh start でも困らない」 = rollback は不要、 という nose 判断が
big-bang cutover の前提。

問題発生時は **PR revert + 新方式 compose stack を down + host daemon を再起動して 6-27 backup restore**
で「Phase 6 直前の状態に戻る」。 これは deploy 時失敗の緊急回避策として意識しておく。

---

## 未解決の設計論点まとめ

各章の「未解決論点」を集約:

- **論点 a**: project.yaml validate タイミング (add 時 sync vs 初回 dispatch)、 fetch depth (full vs shallow/partial)
- **論点 b**: branch policy との整合、 workspace_volume 内 worktree area の GC、 peer clone の適用可否
- **論点 c**: profile bootstrapping (init wizard 合流)、 port 選定 (Web UI 共用 or 分離)、 loopback trust
  default か opt-in か
- **論点 d**: k8s 移行時の secret 供給 (initContainer 経路)、 rotate CLI/Web UI (別 doc)
- **論点 e**: e2e coverage の container 経路移植完了状況、 `boid start` の意味論 (CLI wrap vs script)
- **論点 f**: nested type の dotted path 構文、 secret env var 値の編集経路
- **論点 g**: project の branch policy override、 capabilities.docker の scope、 secret 参照の workspace 分離
- **論点 h**: CI (blackbox-e2e.yml) との整合、 initial import 用の workspaces.yaml を誰が書くか (nose が手で書く前提)

これらは PR-1 着手前の設計 review round で individual に nose 判断を得る。

---

## 参考リンク

- [phase6-container-backend.md](phase6-container-backend.md) — Phase 6 本体 (全 9 PR landed)、 §決定4 は本 doc で撤回
- [phase6-cutover-followups.md](phase6-cutover-followups.md) — 段階撤去計画、 本 doc の PR 群で吸収
- [container-based-boid.md](container-based-boid.md) — Phase 6 の前提となる移行戦略 ①-⑦、 大枠は継続
- [home-workspace-volume.md](home-workspace-volume.md) — Phase 4 の workspace $HOME volume、 論点 b の worktree
  area で参照
- [cli-remote-connection.md](cli-remote-connection.md) — Phase 3 CLI リモート接続、 論点 c の TCP profile 経路
- `container-git-gateway-design` (memory) — git gateway 実装、 論点 b の bare repo fetch 経路で参照
- `phase6-dogfood-incident-and-pivot` (memory) — 本 doc の pivot 経緯記録
