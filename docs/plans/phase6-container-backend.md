# Phase 6 実装計画: container backend への一気移行 (daemon 込み)

ステータス: **PR1–8 landed、PR9 (finale) は実装 PR 提出済み・レビュー/CI green 待ち (2026-07-23 時点)**。
各 landed PR の commit は「PR 分割案」節に pin 済み。PR9 自身は `e2e-container` CI job を real docker で
初めて動かし、そこで見つかった複数の実装ギャップ (§PR9 節に一覧) を修正 — job dispatch + git gateway 経由の
clone 試行まで到達したが、**clone 先ディレクトリへの書き込みが `permission denied` になる既知の残課題が
未解決**で、CI はまだ green になっていない (詳細は §PR9 節末尾)。次セッションでの実機調査が必要。
残作業は撤去 (旧デプロイ・userns backend・host daemon 起動経路) で、
[phase6-cutover-followups.md](phase6-cutover-followups.md) に計画 doc として分離済み (PR9 green 化後に着手)。
作成日: 2026-07-22
親ドキュメント: [container-based-boid.md](container-based-boid.md) — 移行戦略ステップ 6 (+ ステップ 7 の一部を前倒し)
rollback anchor: **v0.0.13** (契約先行 Phase 2.5–5 完了・container 移行前の最後の host daemon + userns リリース)

> 方針転換メモ (2026-07-22): 初版 draft は「daemon はホスト据え置き・enforcement だけ順次差し替え」
> で書いていたが、レビュー (下記変更履歴) を経て **daemon も含めて一気にコンテナ化する**方針に
> 転換した。理由と含意は「目的」「スコープ」節。旧版の daemon-on-host 前提で必要だった多くの
> 足場 (gateway/proxy の loopback bind 到達ハック、broker/dockerproxy の socket bind、boid バイナリの
> bind mount + version skew 対策、shim 同居問題) は、共有イメージ + compose ネットワークで**問題ごと消える**。

---

## 目的

契約先行 (contract-first) が Phase 1–5 で完了し、境界の意味論は現行 userns backend の上で
コンテナモデルに揃った (v0.0.13)。**Phase 6 は enforcement 層を container に差し替える段**。

**方針: 一気コンテナ化** (2026-07-22 nose 決定 = 論点 D)。sandbox の enforcement 差し替え
(userns→container) と daemon の deploy モデル (host→container) を**同時に**行い、単一ホストの
docker compose スタック (daemon + job + egress proxy) として動かす。

理由:

- ユーザは当面 nose + 同僚の 2 人で **blast radius が小さい**
- **顧客展開では速く container 化したい**ので、中間形態 (daemon-on-host) に投資したくない
- **host daemon 構成を恒久オプションとして残すと 2 系統の永続保守が重い** — 早期に container 一本化する

daemon-on-host を経由しないことで、旧版レビューで洗い出した Blocker/Major の相当数
(gateway/proxy 到達ハック、socket→TCP の二度手間、バイナリ bind と shim 同居の泥沼) が
**設計から消える** (下記「決定事項」で個別に対応関係を示す)。

### 移行中の安全網 = 「host 旧デプロイ」(deploy-level rollback)

strangler の並走は残すが、**config フラグのホットフリップではなく deploy 単位の rollback** にする:

- daemon をコンテナ化すると、userns backend を container daemon の中から動かすのは **nested userns**
  (privileged container が要り複雑) になる。よって「1 daemon の中で userns/container を config 切替」
  は採らない。
- 代わりに、移行期間中だけ **現行の host daemon + userns 構成 (v0.0.13) を旧デプロイとして温存**する。
  新は container daemon + container backend の compose スタック。rollback = **旧デプロイに切り戻す**
  (両者は同時起動しない。SQLite/workspace home 等の永続データは同一ホストパスを共有し、切替時に state を保つ)。
- **rollback は単なる compose stop → host daemon 起動ではない** (codex Blocker 1)。sibling job
  container は compose daemon を落としても走り続け、v0.0.13 host daemon には `ReapOrphans` が無いため
  stale task を auto-reopen して **旧 container + 新 userns agent の二重実行**になる。よって rollback
  契約を「①新規 dispatch 停止 → ②compose daemon の外側から install/stack label + 永続 ledger で job/子
  resource を停止・削除 → ③ゼロ件確認 → ④host daemon 起動」と定義する。新 daemon が起動不能でも回せる
  **deploy-level reaper** (daemon プロセス非依存の CLI or script) を必須とし、これ抜きの stop/start を
  rollback と呼ばない。
- container backend が dogfood で安定したら、**旧デプロイ・userns backend・host daemon 起動経路を撤去**する。
- **恒久 2 構成サポートはしない** (nose 決定)。userns backend は「撤去前提の短期 fallback」。

---

## スコープ

### 含む (Phase 6 = ステップ 6 + ステップ 7 の daemon/egress 部分を前倒し)

- **sandbox backend の interface 化** (現行 userns 経路を実装 1 つに抽出、振る舞い不変)。attach の
  ライブストリーム面・resize の 3 経路・signal 意味論・再起動 reap まで含む。
- **container backend** (job = 使い捨てコンテナ)。共有 versioned イメージ + docker API で生成。
- **daemon のコンテナ化** — SQLite/runtimes を host bind で永続化し (決定 4)、docker socket を mount して
  **sibling コンテナ (docker-out-of-docker)** として job を生成、broker/gateway を compose ネットワークで serve。
- **egress / dockerproxy の broker 側配置** (元ステップ 7 から前倒し) — egress proxy を compose
  internal network 上のコンテナに、dockerproxy も同スタックに。
- **broker/gateway/dockerproxy の TCP (mTLS) 化** — socket bind をやめ、compose ネットワーク越しの
  サービス到達に。
- **単一ホスト compose** 前提の clone — daemon コンテナに host repo を bind して `clone --reference`
  (mirror は不要)。
- **短期 fallback = host daemon + userns 旧デプロイ** (v0.0.13、移行中のみ、安定後撤去)。
- 診断回収 / 孤児 reap / file fallback 退役 等の運用契約。

### 含まない (Phase 7 = ステップ 7 の残り + 8)

- **k8s backend (operator パターン)** と別ホスト構成全般。
- **mirror + mirror 更新ワーカー** — 別ディスク構成でのみ必要。単一ホスト compose では host repo 直参照。
- **リモートランナー (pull 型)** — ステップ 8。
- **DB (SQLite → Postgres/PVC)** — チーム共有の論点。
- **実行中 job の live 再吸着** — Phase 6 は現行の再起動契約 (kill+fail+auto-reopen) 踏襲 + 孤児 reap のみ。
- **リソース制限 (cgroup) の設定語彙** (nose 決定 2026-07-22) — 現行スキーマに資源制限語彙は皆無で、boid が
  enforce する job/task timeout も存在しない (2026-07-22 実査)。userns に無い新能力を cutover の等価性検証に
  混ぜないため Phase 7 で語彙設計する。fork bomb 対策の `PidsLimit` default のみ実装時の任意項目として許す。
- **workspace HOME の named volume 化** (nose 決定 2026-07-22) — Phase 6 は host bind 維持 (決定 4)。
  named volume 化と owner 調整 (root one-shot init container での chown。driver opt では disk volume の
  owner 指定不可) は Phase 7 の k8s/別ホスト論点。
- **`boid workspace peers` (Phase 5b 宿題 3)** (nose 決定 2026-07-22) — backend swap と独立のため Phase 6
  から切り離し独立 backlog へ。peer advertise データは dispatch 時計算済みで inert (詳細は phase5 doc)。

一気とはいえ実装は単一ホスト docker に限る。interface とネットワーク契約は k8s (Phase 7) を後から
足せる形で切るが、k8s 実装自体はしない。

---

## 前提と依存関係

- **ステップ 1–5 landed** (v0.0.13)。本 phase が前提にする契約: `/workspace/<name>` clone 先 /
  `/run/boid/bin/<name>` shim / $HOME = workspace volume / タスクコンテキスト RPC pull。
- **workspace DB の `ContainerImage`** (`internal/orchestrator/workspace_meta.go:78`、"reserved for the
  Phase 6 container") が image 選択の入口。**現状 spec に未接続**なので配線が要る (決定 2)。
- **既存 dockerproxy** (`internal/sandbox/dockerproxy/`、Phase 1 landed) と **egress ProxyManager**
  (`internal/sandbox/proxy_manager.go`) は、本 phase で **broker 側 (compose スタック内) に配置換え**する。
- **既存の deploy 機構** (post-merge git hook → `go install` → live host binary) は、daemon コンテナ化で
  「イメージビルド → daemon コンテナ再起動」に変わる (決定 4)。
- **host に docker engine (rootful) + compose v2 の導入が必要 (確定、nose 決定 2026-07-22)**。現状の
  開発/運用 host には docker が無く podman 4.9.3 のみ (2026-07-22 実査 — plan 全体が暗黙に docker 存在を
  仮定していた)。DooD / compose / label / attach の挙動を plan の前提どおり docker で検証するため
  cutover 前に docker engine を導入する。podman は併存可。rootless docker は Phase 7 ハードニング候補の
  まま。CI の ubuntu-24.04 は docker 標準搭載なので prod と CI のエンジンが揃う。

---

## 現状棚卸し (backend の継ぎ目)

現状の sandbox 実行は 3 段: **dispatcher (host, go)** → **`sandbox.Spec` DTO** → **runner
(host→userns, syscall)**。継ぎ目は `sandbox.Spec` の JSON 境界に半分引かれているが、`sandbox.Spec` は
role 非依存であっても **backend 非依存ではない**。

### 起動経路が散らばっている (3 経路)

`Runner.launchSandbox` (`internal/dispatcher/runner.go:849`) が起動を 3 つに分けて持つ:

1. `SandboxPreparer.PrepareSandbox(spec) (*PreparedSandbox, error)` (`preparer.go:20`、実装
   `sandbox_preparer.go:20`) — 「JSON marshal だけ」ではなく RootDir 作成・secret を含む spec file
   書き込み・state/cleanup artifact の所有まで担う (抽出時に cleanup 責務を落とさないこと)。
2. `runnerCommand(prepared) → "boid runner-outer --spec … --state …"` (`runner.go:938-948`) —
   **userns entrypoint をハードコードしている唯一の箇所**。
3. `JobRuntime.Start(ctx, RuntimeStartSpec{Command}) (*RuntimeHandle, error)` (`runtime.go:65`、実装
   `LocalRuntime` `runtime_local_linux.go:68`) — `bash -lc <Command>` を PTY/pipe で起動。

### attach/resize の routing seam が 3 つに散っている (stream 1 + resize ingress 2)

- **出力・入力ストリーム**は Phase 3 で WS に一本化 (`internal/api/ws_attach.go`)。`JobRuntime.Attach`
  は使わず、`Subscriber.Subscribe(jobID)` / `Writer.WriteInput(jobID)` / `CloseInput` を jobID キーで
  叩き、`Runner` (`runtime_subscriber_export.go`) が DB で runtimeID を引いて `LocalRuntime` 固有
  (`SubscribeRuntime`/`WriteInputRuntime`/`CloseInputRuntime`) に委譲。snapshot・複数 subscriber・
  half-close の意味論はここ。**live 出力の消費者は WS attach に加え Web UI の SSE follow**
  (`GET /api/jobs/{id}/log?follow=true`、`internal/api/job_log_sse.go:21`) もあり、**どちらも同じ
  `Subscriber.Subscribe(jobID)` を使う** (seam は同一 stream 1 本のまま、消費者が 2 系統。PR1 の付け替え
  検証対象に SSE follow を含めること)。
- **WS 内 resize**: `Writer.ResizeRuntime(jobID)` (`ws_attach.go:123`)。
- **resize の別 HTTP route**: `POST /api/jobs/{id}/resize` が `jobRuntime.Resize(job.RuntimeID)` を
  **直呼び** (`internal/server/job_runtime_routes.go:54`)。`cmd/attach.go` の初期サイズ + SIGWINCH
  (`c.ResizeJob`) が使う CLI の resize 経路。backend を経由しないため container backend でここを
  付け替えないと CLI resize が誤配送される。

→ 内訳は **stream 1 本 + resize ingress 2 本 (WS frame / HTTP route)** の計 3 seam
(`Writer.ResizeRuntime` は WS 側 adapter であって第三の外部 ingress ではない)。container backend は
この 3 seam すべてを session 経由に routing しないと WS/CLI attach が壊れる。

### JobRuntime は transport 抽象だが Signal が userns 前提

`boid agent stop` は `kill(-pgid, SIGUSR1)` (`runtime_local_linux.go:413-429`) を process group に送り、
runner は SIG_IGN で無視 (`runner/runner.go:56-70`、execve 越し継承)、adapter が受信して Claude だけ
SIGTERM して `job_done` する (`claude/run.go:439-443`)。docker の container signal は PID 1 に届くが、
entrypoint が SIGUSR1 を無視すると消える → container entrypoint に signal forwarding が要る (決定 3)。

### userns 固有の実装は 3 点に限局

1. `sandbox.plan.BuildPlan(spec)` (`internal/sandbox/plan.go:25`) — base rbind + `/dev /proc /tmp` +
   nft drop + DNS stub `10.0.2.3`。→ container では **イメージの rootfs + compose network** に移る。
2. `internal/sandbox/runner/runner_linux.go` — clone(NEWUSER)+uid_map / pivot_root / mount syscall /
   nft / pasta。→ container runtime が代替。
3. egress L3 (`BuildPlan` の nft + `applyProxyEnv` `sandbox_builder.go:1210`)。→ compose internal
   network + proxy に置換。

### `sandbox.Spec` は role 非依存だが backend 非依存でない

`ProxyPort` は nft+pasta、`RootDir`/`CleanupPaths`/`Profile` は userns runner、`Mount.Source` は host
path。特に `/workspace/<name>` は `cloneMounts` (`sandbox_builder.go:671`) が **ホスト runtime dir を
bind** する形。`Mount.Guard`/`DetectType` は docker で表現できない。→ backend-neutral な可視性要求と
userns concrete な mount plan を分ける realization 層が要る (PR3)。

### shim と boid バイナリの現状 (一気化で溶ける前提の把握)

boid バイナリは `/run/boid/bin/boid` に**単一ファイル bind** (`sandbox_builder.go:471`、`IsFile:true,
ReadOnly:true`)、shim は **相対 symlink** `/run/boid/bin/<name> → boid` (`hostCommandSymlinks:1090`)。
相対リンクは binary と shim の **同居**が前提。旧版 (daemon-on-host) ではこの binary を container に
bind mount する設計で、単一ファイル mount の inode 問題・version skew・shim 同居のどれをどう解くかが
論点だった。**本 phase (一気化) では共有イメージに焼き込むため、この論点群ごと消える** (決定 2)。

### 既存の抽象化と egress 配線

- `JobRuntime` (transport 抽象、実装 `LocalRuntime` 一本)、`SandboxPreparer` (spec 書き一本)。
- `sandbox.Sandbox` (`sandbox.go:4`) は **production dead** だが `testutil/sandbox.go` に test mock あり。
- container/compose backend の scaffold は皆無 (userns 具象一本)。
- **gateway/proxy は loopback bind**: git gateway は `net.Listen("tcp","127.0.0.1:0")`
  (`server.go:295`)、sandbox 向け URL は `http://10.0.2.2:<port>` (pasta の slirp 投影)。ProxyManager も
  同様 (`proxy.go:58`)。**docker には 10.0.2.2 投影がない**ため旧版はここに到達ハックが要ったが、
  **daemon をコンテナ化して job と同じ compose ネットワークに置けば、サービス名 (DNS) で到達でき
  ハック自体が不要になる** (決定 5)。
- dockerproxy は per-sandbox UNIX socket bind + RPC (`runner.go:627`、sandbox には
  `/run/boid/docker-proxy.sock`)。本 phase で broker 側 (compose スタック) に配置換え。

---

## 決定事項 (提案 — nose レビュー前提)

### 1. `SandboxBackend` / `SandboxSession` interface を導入し、起動 + attach 面を束ねる

```go
type SandboxBackend interface {
    Launch(ctx context.Context, spec sandbox.Spec, opts LaunchOptions) (SandboxSession, error)
    // Adopt: launch 後に runtimeID から session を再構成 (WS attach / resize / signal / stop の後続用)。
    Adopt(ctx context.Context, runtimeID string) (SandboxSession, bool)
    // ReapOrphans: daemon 再起動後の孤児回収。global error だけでは「reap 失敗 task を reopen
    // しない」を表現できないため、task 単位の結果型を返す (決定 6/8)。
    ReapOrphans(ctx context.Context) (ReapReport, error)
}

// ReapReport: どの job が回収できて / できなかったかを task 単位で返す (決定 6 の reap-before-reopen
// が「失敗 task だけ reopen 抑止」を実現するのに必須)。dockerproxy 子 resource は label を持たず
// per-server ledger 管理なので、primary job container の label 列挙とは別に永続 ledger 経由で追う (決定 8)。
type ReapReport struct {
    ReapedJobIDs  []string
    FailedJobIDs  []string
    GlobalError   error
}

type SandboxSession interface {
    ID() string
    // Subscribe: snapshot と live channel を原子的に (欠落なく) 返す現行契約。container backend では
    // 各 subscriber が個別に docker attach するのではなく「backend 内に 1 本の docker attach 所有者 +
    // daemon 側 buffer/spool + fan-out」を置き、複数 subscriber / 完了後 snapshot / TTY・非 TTY
    // multiplex を成立させる (決定 8/9)。
    Subscribe() (snapshot []byte, ch <-chan []byte, cancel func(), ok bool)
    WriteInput(data []byte) error
    CloseInput() error                                                      // stdin half-close は出力 stream を閉じない (現行契約)
    Resize(size TerminalSize) error                                         // resize 2 ingress (WS/HTTP) の集約先
    Wait(ctx context.Context) (RuntimeExit, error)
    Stop(ctx context.Context) error
    Signal(ctx context.Context, sig syscall.Signal) error                   // agent-stop 意味論 (決定 3)
    // 注: Phase 3 の WS 一本化後 `JobRuntime.Attach` に外部 caller は無いため、live attach 契約は
    // Subscribe/WriteInput/CloseInput/Resize に集約し、旧 Attach(ctx, RuntimeAttachRequest) は載せない。
}
```

- **usernsBackend**: 現行の (PrepareSandbox + `boid runner-outer` + `LocalRuntime`) を合成。
  `JobRuntime`/`LocalRuntime`/`SandboxPreparer` は内部 transport として温存 (撤去まで)。
- **containerBackend**: 同じ `sandbox.Spec` を docker create に翻訳。session は docker
  attach/logs(stream)/wait/kill にマップ。
- **resize の 2 ingress を session 経由に**: WS 内 resize (`ws_attach.go` の `Writer.ResizeRuntime`) と
  `POST /api/jobs/{id}/resize` (`job_runtime_routes.go` の `jobRuntime.Resize` 直呼び) を
  `SandboxSession.Resize` (Adopt 経由) に付け替える (PR1 スコープ)。
- 選択は config (`sandbox.backend: userns|container`) だが、実運用は**デプロイ単位** (userns=host 旧
  デプロイ / container=compose スタック) で分かれる。同一 daemon 内ホットフリップは nested userns の
  ため採らない (目的節)。

### 2. イメージ = daemon と job が共有する versioned base イメージに boid 焼き込み (論点 A/C 解消)

- **一気化の最大の恩恵**。daemon と job runner が **同じ versioned base イメージ**から起動し、boid
  バイナリと `/run/boid/bin` パス土台を**イメージに焼き込む**。ただし host command shim の**個々の
  symlink (`/run/boid/bin/<name> → boid`) は project.yaml の `host_commands` 宣言から動的生成**される
  (`sandbox_builder.go` の `hostCommandSymlinks`) ため、image build 時に全 `<name>` を焼くことはできない
  (codex Major 6)。→ **image に焼くのは boid バイナリ + `/run/boid/bin` ディレクトリまで**。個々の shim
  symlink は container entrypoint が起動時に検証済み `spec.Symlinks` から生成する (下記 entrypoint と一致)。
  - **boid 本体の version skew は定義上消える**: 「boid 更新 = イメージ再ビルド = daemon も job runner も
    同一版」。旧版で必要だった「バイナリ bind mount + content-addressed pin」も、単一ファイル mount の
    inode 問題も、相対 symlink と binary の同居問題も**全部不要**になる (現状棚卸し参照)。
    ただし **workspace override image を許すとこの skew 保証は崩れる**ので、override は「boid base からの
    派生 image」に限定し、runner protocol version / image label を launch 前に inspect して不一致を拒否
    する (決定 11)。
- **container entrypoint** は clone/pivot_root を skip (namespace 隔離は container が提供)、mount/file/
  symlink 適用 + `runAgent` + `postJobDone` + signal forwarding のみ。共有ロジックは runner_linux.go から
  `runner/runner.go` に抽出 (PR2)。entrypoint はイメージ内 boid を絶対 path で直接起動 (shim 不要)。
- **$HOME 配下のツールチェーン** (go/volta/claude/codex/opencode) は引き続き workspace volume 側
  (親ドキュメント「ツールはイメージ」の但し書き)。イメージに焼くのは OS 土台 + boid + `/run/boid/bin`
  パス土台のみ (個々の shim symlink は entrypoint 生成)。
- **image 選択**: workspace DB の `ContainerImage` (nullable) を JobSpec → `sandbox.Spec` →
  containerBackend に流す。未指定は default image。workspace override + digest/pull policy を決める。
- userns backend 側は撤去まで現行 (`/run/boid/bin/boid` 単一ファイル bind + 相対 symlink) を維持・触らない。

### 3. Signal の意味論を保つ (agent-stop を container kill にしない)

container entrypoint に signal forwarding を設け、`Signal(SIGUSR1)` を PID 1 → adapter (process
group) に届ける。中間段は SIG_IGN を維持し、adapter が受けて Claude を graceful に落として `job_done`
する現行意味論を pin。単純な docker kill にすると agent-stop が強制終了に化ける。

**PID1 は docker 同梱の docker-init (tini 相当) = `HostConfig.Init: true` で確定** (nose 決定 2026-07-22)。
根拠 (2026-07-22 実査): SIGUSR1 → agent SIGTERM の転送は adapter 内 `sigutil.ForwardAndWait`
(`internal/adapters/sigutil/sigutil.go:40-77`) が既に持ち、それが動くプロセス = entrypoint 化される
プロセス自身なので **forwarding の自作は不要**。現行は PID namespace 自体が無く (`CLONE_NEWPID` 不使用)
zombie reap 責務は誰も負っていないため、コンテナ化で新規に生じる責務は「孤児 zombie の reap」と「PID1 の
signal disposition」の 2 点のみ — これを docker-init に任せる (イメージ改変ゼロ)。Go 自前 PID1 の
reap loop は `cmd.Wait` と `waitpid(-1)` の競合という既知の罠があるため書かない。

### 4. daemon コンテナ化 (persistence / docker socket / deploy)

- **docker-out-of-docker (DooD)**: daemon コンテナに host の `/var/run/docker.sock` を mount し、job を
  **sibling コンテナ**として生成する (nested docker ではない)。job と daemon は同一 host docker の
  compose ネットワークを共有する。
- **DooD の path 境界** (codex Blocker 2): sibling を `docker create` するときの bind source は **host
  docker daemon が解釈する host 絶対 path** であって daemon コンテナ内の mount path ではない。daemon
  コンテナに bind された dir は sibling に自動再 export されない。よって host bind を使う workspace HOME /
  clone reference (`.git`) 等は **daemon コンテナ内と host で同一絶対 path になるよう volume を配置し、
  起動時に検証**する。`sandbox.Spec.Mount.Source` を **named volume 名 / host 絶対 path / container-local
  の 3 種に型分け**する realization を PR3 に含める (現行は workspace HOME 絶対 path を bind source に
  している。決定 10 の clone reference も同じ制約)。
- **persistence**: SQLite (tasks 管理 DB)・runtimes dir・workspace home・credential・web_secret を
  **host bind で永続化** (移行中は named volume を使わない — 同一ホストパス共有が rollback の前提のため)。
  **短期 fallback のため、これらは host 旧デプロイと同一ホストパスを共有**し、切り戻しで state を失わない。
- **schema 互換と相互排他 (確定、nose 決定 2026-07-22)**: v0.0.13 と現 HEAD の schema は同一 (2026-07-22
  実査) だが、migration 機構は**前方のみ・rollback 不可・天井チェック無し**で、旧バイナリは自分の知らない
  記録済み version を黙って無視して新しい DB を開く (`internal/db/migrate/migrate.go`)。過去には破壊的
  migration (0021/0025/0032 の DROP COLUMN・テーブル再作成) を厭わない運用実績があるため、明文ルール化する:
  ① **旧デプロイ撤去まで migration 追加は加法的変更 (ADD COLUMN / CREATE) に限定**し、DROP/RENAME/
  テーブル再作成は撤去後まで禁止 (repository 層は `SELECT *` 不使用・明示カラム列挙なので加法的なら旧
  バイナリが新 DB 上で動ける)。② **未知 migration version の検出で起動拒否する天井チェックを PR6 で追加**
  し「黙って開く」穴を塞ぐ。③ 二重起動ガードは現行 socket path 単位の `IsSocketAlive` のみで、**別 socket
  path の 2 デプロイは同一 DB を同時に開けてしまう**ため、compose daemon の server socket を **host 旧
  デプロイと同一 host path に bind** して既存ガードを相互排他として効かせる。
- **job container の UID/GID (確定、nose 決定 2026-07-22)**: container では namespace 隔離を container
  runtime が提供するため、現行の「inner uid 0 で CAP_SYS_ADMIN を確保」(userns の pivot_root/mount 用、
  `runner_linux.go:170-179`) の理由が消える。→ **job container は `--user <daemon uid>:<gid>` (通常
  1000:1000) で非 root 起動**し host owner と揃える。**workspace HOME は Phase 6 では現行どおり host 絶対
  path の bind mount を維持する** (nose 決定 2026-07-22 — persistence の同一ホストパス共有・上記 DooD path
  境界の同一 path 検証と整合し、owner は現行と同じ daemon uid 所有 `workspace_home.go:61-68` のまま追加
  作業なし。#813 の「dockerproxy policy で bind 不可 → named volume 化」は誤りだった: dockerproxy policy が
  縛るのは job が作る sibling であって、daemon が docker.sock 直で作る job container の bind は縛られない)。
  named volume 化はスコープ外 (Phase 7) 節参照。docker userns-remap は host docker daemon 全体設定を汚し
  他用途 container に波及するため**使わない**。claude CLI の inner uid 0 拒否対策 `IS_SANDBOX=1` は
  container でも継続。image には `--user` uid の /etc/passwd エントリを焼く (無いと os/user 系ツールが
  壊れる、PR2 注意点)。
- **docker socket の security posture (確定)**: docker.sock = root 相当。**生 socket は daemon コンテナ
  1 箱にのみ集約**し (DooD の宿命として受容)、job コンテナには渡さない (job は dockerproxy 経由のみ、
  現行同様。`validateDockerHostCommands` が既に無制限 `host_commands.docker` の dispatch を弾く
  `runner.go:755-768`)。dockerproxy 自体が既に docker API allowlist + `containers/create` の
  bind/Privileged/host-mode/CapAdd 拒否 + `/build` deny + fail-closed (`dockerproxy/policy.go`) という
  **socket-proxy 相当のハードニングを持つ**ので外部 socket-proxy を二重に挟まない。**rootless docker で
  daemon コンテナ自体を非 root 化するのは Phase 7 のハードニング候補**に回す (nested の複雑さと引き換えの
  ため初手ではやらない)。
- **broker が exec する host command の実体** (codex Major 5): broker を daemon コンテナに移すと host
  command (`gh` / `atl` / board / project-local script 等) の "host" は **daemon image** になる。これらは
  daemon プロセスが `exec.LookPath` で解決し直接 exec する (`host_commands.go` / `broker.go`) ため、
  image を「OS 土台 + boid のみ」にすると解決に失敗し「ハーネス/スキル書き換えゼロ」が崩れる。→
  **必要ツールの実体・設定・credential を daemon image (または別 tool layer) に provision** し、
  host_commands config の全 command 解決を daemon 起動時に検証する。project-local script の host-path
  mapping の扱いも決める (親 doc の broker tool layer)。この provisioning 作業を PR6 の明示スコープにする。
- **deploy 機構の変更**: 現行の post-merge git hook → `go install` → live host binary は、daemon
  コンテナ化で「イメージビルド → daemon コンテナ再起動」に変わる。移行中は旧 deploy 機構 (host 旧
  デプロイ用) と新機構が並存する。

### 5. egress / gateway / broker は compose ネットワーク + TCP (loopback ハック消滅)

- daemon が compose ネットワーク上のコンテナになるので、**gateway / broker / dockerproxy は
  サービス名 (DNS) + TCP (mTLS) で到達**する。旧版の loopback bind + 10.0.2.2 投影 + host-gateway
  ハック (旧決定 5) は**丸ごと不要**。
- **egress**: job コンテナを `internal: true` ネットワークに置き、外部到達を **egress proxy コンテナ**
  のみにする (L3 トポロジ強制)。**proxy 自身を `internal` network だけに置くと proxy も外部に出られず
  egress が全停止する**ため、`internal` (job 側) + 外向き network の **dual-home** にする (codex Blocker 3)。
  ProxyManager のポリシー (workspace→allowed_domains) と live-swap は流用し、proxy を compose スタック内の
  サービスとして配置 (「broker 側再配置」を本 phase で実施)。**共有 network に workspace ごとの listener を
  出すと job が別 workspace の緩い proxy endpoint を直接選べる**ため、**internal network は workspace 単位で
  分離する** (確定、nose 決定 2026-07-22)。job は自 workspace の internal network にのみ属し、別 workspace の
  proxy endpoint へは L3 で到達不能にする (呼出元 identity × workspace policy の突合には頼らない)。これを
  **cutover 前の security invariant** とする。**compose 表現 (確定、nose 決定 2026-07-22)**: workspace
  network は動的に増減するため compose file には書けない — compose は静的骨格 (daemon / egress proxy /
  外向き network) のみを宣言し、workspace internal network は daemon が runtime に label 付きで生成して
  egress proxy へ `docker network connect` で動的接続する。proxy は**接続が届いた network 側 local
  address で workspace を同定**して policy を強制紐付けする。
- **dockerproxy** も broker 側 (compose スタック) に配置。job の `DOCKER_HOST` は proxy サービスを指す。
  ただし現行の **per-job UNIX socket + per-server ledger による scope を共有 TCP service に置き換えると、
  docker capability を持たない job も service を直接呼べてしまう** (codex Blocker 4)。共通 client cert の
  mTLS は transport 認証にしかならないため、**per-job の短命 identity/token を発行し、server 側で
  `jobID → DockerEnabled → 専用 ledger` に紐付け**る (token は既存 broker/gitgateway の per-job capability
  token パターンを流用)。**配送様式 (確定、nose 決定 2026-07-22)**: gateway 式の URL path 埋めは
  `DOCKER_HOST` が path を持てないため**構造的に不成立** → **per-job 短命 client cert (mTLS) を broker 式の
  env で配送**し、entrypoint が container-local (`/run/boid/docker-tls`) に materialize して
  `DOCKER_CERT_PATH`/`DOCKER_TLS_VERIFY` を設定する (docker CLI / TestContainers とも native 対応)。
  無 capability job は接続時点で拒否し、resource ID
  scope と cleanup ledger を job ごとに永続化する (資格情報は共有 image に焼かない)。
- **sibling 疎通契約 (確定、nose 決定 2026-07-22)**: dockerproxy は `containers/create` に**作成 job の
  workspace internal network を強制注入** (NetworkingConfig 上書き) し、sibling を同一 workspace network に
  入れる。job → sibling の到達は **container IP + container port の直アクセス**。host への port publish は
  非サポート (internal network から host published port へは届かない)、bind mount 非サポートは継続。これに
  伴い dockerproxy は「検査して素通し」から **request body を書き換える proxy** になる (forward/hijack
  両経路、PR6)。現行 e2e は fake docker への policy 検証のみでデータプレーン未検証 (2026-07-22 実査) の
  ため、**実 docker で「job→sibling TCP 到達 / 別 workspace sibling へ非到達 / reap で消える」の 3 点を
  container e2e 要件** (PR9) とする。なお policy の hostConfig モデルは Memory/NanoCpus 等 resource 系
  field を持たず素通しだが、境界破壊ではないため deny しない (仕様として明記)。
- broker protocol は「sandbox 発接続」の向きのままなので、UNIX socket を mTLS gRPC/HTTP に差し替える
  だけで shim / task ask RPC / notify の意味論は無傷 (親ドキュメント broker transport swap)。
- **exec job の daemon API 経路 (`BOID_SOCKET`) と host CLI/Web の publish 経路** (codex Major 10) も
  移行対象。現行 exec job は daemon UNIX socket を `/run/boid/server.sock` に bind して `BOID_SOCKET` で
  叩くが、別コンテナからは socket bind できない。→ job からの boid 操作は broker token 経由に一本化する
  か、job-scoped credential 付き内部 TCP endpoint を定義する。daemon の UNIX listener は裸 router (無認証)・
  TCP listener は auth-wrapped という現行構成のため、**裸 UNIX router を compose network に誤って公開すると
  認証 bypass になる**。host-facing port / bind address / Bearer 認証 / public URL を明記する。

### 6. 再起動 recovery = 現行踏襲 (kill+fail+auto-reopen) + 孤児 reap

- **live 再吸着はしない**。現行 startup は running job を failed (`store.MarkStaleJobsFailed:176`)、
  executing/awaiting task を aborted にし、後段で auto-reopen (`wire.go:527` `FindDaemonShutdownAborted
  Tasks` → reopen)。token/context はメモリ上のみなので走行中 container を拾い直しても旧 token で
  RPC/clone が成立しない。再吸着は Phase 7。
- `SandboxBackend.ReapOrphans` は label (`boid.job_id` 等) 付きの実行中コンテナ・volume・network を
  列挙して破棄する reap 専用。
- **reap は auto-reopen より前に完了させ、reap 失敗 task は reopen しない**: docker container は daemon
  再起動では終了しないため、reap 前に auto-reopen すると **旧 agent (走行中 container) + 新 agent が
  共有 $HOME / task RPC に同時作用する二重実行**になる。`MarkStale*` と auto-reopen の間に reap を
  挟む (現行 `cleanOrphanRuntimes` の container 版)。
- 現行 runtime dir GC の container 版として周期 reap も足す。
- **install_id / `boid reap` の formalize (確定、nose 決定 2026-07-22)**: install ID は
  `~/.local/share/boid/install_id` に**平文 UUID を LoadOrCreate** (web_secret と同居・非秘密。現行コードに
  machine id 相当は存在しない — 2026-07-22 実査)。daemon が作る全 container/network/volume に
  `boid.install_id` / `boid.job_id` / `boid.workspace` label を付与する (dockerproxy 経由で job が作る子
  resource は label を持たないため従来どおり per-job ledger `docker-resources.jsonl` 管理)。`boid reap` は
  **daemon 非依存 subcommand** で、label 列挙 ∪ runtimes dir の ledger の**和集合**を停止・削除する (PR6)。

### 7. `Wait` は単一所有者 + cleanup 順序を契約化

現行は `watchRuntime` と `cleanupSandboxAfterWait` が同 runtime を並行 `Wait` する (`runner.go:928-994`)。
container session が診断回収と remove まで所有すると二重 wait/cleanup が競合する。**backend 内で一度
だけ wait して exit future を fan-out**し、「診断回収 → job fallback 処理 → resource remove」の順序を
契約化する (remove が診断回収より先だと決定 8 の fallback が空を掴む)。

### 8. 診断成果物は「種類ごとに置き場を分ける」(論点 E)

回収したいものは 3 種類で、サイズと「既に永続してるか」が違う:

- **session jsonl (harness 記録)**: `~/.claude/projects/…/<session>.jsonl` は **$HOME workspace volume
  に既に永続** (Phase 4)、session-ID キーで並行 job でも衝突しない。**送らず参照する** (claude adapter
  が `artifact.claude_code.sessions[]` に記録)。RPC に大量データを流さない。
- **runner-state.json (boid 診断・固定名)**: 小さいが固定パスで並行 job と衝突するため $HOME volume に
  置けない。→ **これだけ RPC / daemon 側 capture** で回収。
- **transcript (生 stdout/stderr・大)**: 2 用途を分ける (codex Major 7 —「tail のみ」と「`boid job log` の
  full 契約維持」は両立しないため)。
  - **full 永続 (`boid job log` 契約)**: 現行 `boid job log` は runtime dir の `transcript.log` 全量を読む。
    container remove 後は `docker logs` も消えるので、**daemon 側が attach stream を runtime storage へ逐次
    spool** して full を永続する (決定 9 の「1 本の docker attach 所有者 + fan-out」と同一経路)。
    `ReadJobLog`/`StatJobLog` の backend-neutral な保存先を PR7 で実装。**RPC に full を流さない**。
  - **silent-exit 判別 (`runtime.go:59`)**: これは **tail + size + exit code** の小メタで足りる。
    OOM/SIGKILL/setup failure/daemon loss で runner RPC が走らない異常系は、**container remove 前に daemon が
    `docker logs` / `inspect` で capture** (runner upload は正常系の補助)。
- container remove は **spool の close/fsync と runner-state 回収の後**に行う (決定 7 の順序契約)。
- **実装方針 (確定、nose 決定 2026-07-22): 新規設計ではなく現行 session 層の抽出・流用**。「attach 1 本
  所有 + メモリ buffer + fan-out + 同期 disk spool + ロック下の snapshot/live 原子接続」は
  `runtime_local_linux.go` の session 層 (readLoop / appendTranscript / subscribe) に**既に全部実装済み**
  (2026-07-22 実査) なので、これを backend-neutral に抽出し source を PTY/pipe → docker attach に差し替える。
  transcript.log の置き場は現行 runtimes dir (host bind) を継続。非 TTY の docker attach は stdout/stderr
  多重化 stream で来るため demux して**単一結合 stream に合流**する (現行契約 — TTY/非 TTY とも単一結合で
  stdout/stderr 分離は意図的に無い。分離は protocol change なのでしない)。無欠落 full の保証者は disk
  spool のみ・live channel は backpressure で遅い subscriber を切断、という現行意味論も維持。live 消費者は
  WS attach と Web UI SSE follow の 2 系統 (どちらも `Subscribe` 経由、現状棚卸し参照)。

### 9. file fallback + `~/.boid` tmpfs overlay を退役 (writer 移行が前段)

- 退役の機序は「コンテナ隔離」ではなく **RPC を唯一経路化**すること。container でも $HOME は workspace
  共有 volume なので `$HOME/.boid/output/payload_patch.json` は並行 job 間で共有され続ける (Phase 4 設計)。
- **reader だけ消すのは不十分**: claude adapter は起動前に session ID を `writePayloadPatch` で
  `~/.boid/output/payload_patch.json` に書く**能動 writer** (`claude/run.go:399-419`)。`runAgent` は
  `Result.PayloadPatch` を捨て `resolveJobOutput` の file read だけが broker に渡る (`runner_linux.go:497-538`)。
  → **(a) writer を RPC / 明示的 runner→broker patch 送信に移行 → (b) reader/writer/overlay を一括撤去**。
- backend 非依存 (両 backend で同じ負債) なので、writer 移行さえ済めば独立に先行 land 可能。

### 10. clone は単一ホスト compose で host repo 直参照 (mirror 不要)

job コンテナ内 runner が clone 直後に `git clone --reference <ro .git> <gateway URL> /workspace/<name>`
する。単一ホストなので mirror + mirror 更新ワーカーは不要 (別ホストの Phase 7 論点)。`/workspace` は
container-local (性能)、reference は ro bind。**ただし DooD では job (sibling) の bind source も host 絶対
path で解釈される**ため、reference の `.git` は **daemon コンテナ経由の又貸しではなく host 絶対 path で job
に直接 ro bind** する (決定 4 の path 境界)。又貸しが不可なら reference を諦めて gateway 側 server-side pack
に寄せる選択肢も含め、PR3/PR5 で clone レイテンシを計測して確定する。

### 11. backend 選択 = global container + workspace は image 選択 (論点 B)

`ContainerImage` は **「どの image を使うか」**の意味に純化する (userns/container の per-workspace 切替
スイッチにはしない)。backend は global (container、userns は撤去前提の短期 fallback デプロイ)。段階移行が
必要になったら再検討するが、初期は global 一本 + image 選択で足りる。**workspace override image は「boid
base からの派生」に限定**し、runner protocol version / image label / digest を launch 前に inspect して
不一致を拒否する (codex Major 6 — 任意 image を許すと決定 2 の version skew 保証が崩れる)。現行の
`ContainerImage` は契約情報を持たない単なる文字列なので、この検証の配線を足す。

### 12. registry credential の線引き = 「$HOME 永続の例外はハーネス認証のみ」を維持 (確定、nose 決定 2026-07-22)

- npm/nuget/pypi 等の**公開 registry は egress floor に既定で含まれる** (`defaultAllowedDomains`
  `cmd/start.go:68-92`)。private registry は workspace の `allowed_domains` **加算**で開ける (floor 縮小不可)。
- **registry token は $HOME volume に置かない** (credential 例外を広げない)。既存の `secret:` env 機構
  (暗号化ストア → dispatch 時 env 注入、`secret_scan.go` / `secret_store.go`) で **job-scoped に env 注入**
  し、`.npmrc` は `//<registry>/:_authToken=${NPM_TOKEN}` の **env 参照 (非秘密設定)** にする。init.sh の
  「credential を書かない」契約 (`docs/examples/workspace-home-init.sh`) も維持。**新機構ゼロ**で線引きが引ける。
- **private git 依存**: ExtraRepos は gateway 側の PermFetch 付与まで実装済みだが、**sandbox 内の git/go を
  gateway URL へ誘導する経路 (insteadOf 生成) は未実装** (2026-07-22 実査 — 旧記述「insteadOf→gateway で
  解決済み」は誤りだった。現行の self/peer clone は URL 直埋めで insteadOf を使わない)。誘導の配線
  (insteadOf 生成 or 明示 URL 案内) は未解決論点に残す。

---

## 目標状態

- `SandboxBackend`/`SandboxSession` interface が導入され、`launchSandbox` と attach/resize の 3 経路が
  backend 選択の 1 抽象に集約。userns backend は実装の 1 つ (振る舞い不変、撤去まで残す)。
- **compose スタック**が動く: daemon コンテナ (docker.sock + 永続 volume + TCP broker/gateway) +
  job コンテナ (共有イメージ、entrypoint = 焼き込み boid runner) + egress proxy コンテナ (internal network)。
- ハーネス/スキルの書き換えはゼロ (契約が Phase 1–5 で移行済み)。version skew・shim 同居・loopback 到達の
  旧論点は共有イメージ + compose ネットワークで消滅。
- WS/CLI attach・resize (3 経路)・agent-stop (SIGUSR1 forwarding) が container で動く。
- egress は internal network + proxy で L3 強制。broker/gateway/dockerproxy は TCP。clone は host repo 直参照。
- 診断は種類ごとに置き場分離、再起動は reap-before-reopen で二重実行なし、file fallback 退役。
- **短期 fallback**: host daemon + userns 旧デプロイ (v0.0.13) に deploy 単位で切り戻せる。安定後撤去。
- container e2e が CI で回る。

---

## PR 分割案

一気化で PR 数は増える。前半 (PR1–3) は inert。**config 公開 (container を実運用に載せる) は PR7 の
cutover まで行わない** (egress + daemon コンテナ化 + reap が揃うまで insecure なため)。

### PR1: `SandboxBackend`/`SandboxSession` interface 導入 + userns backend 抽出 (inert・振る舞い不変)

**landed**: #816 (`18cd5a9`, 2026-07-22)
- interface を attach ストリーム面 + Adopt + ReapOrphans + Signal + **resize 3 経路** (`ws_attach.go` +
  `job_runtime_routes.go` の直呼び) 込みで定義。`launchSandbox`/`runtime_subscriber_export.go` を
  `usernsBackend` に集約。`JobRuntime`/`LocalRuntime`/`SandboxPreparer` は内部 transport として温存。
- 既存の全 unit/e2e (WS attach・**Web UI SSE follow**・CLI resize・agent-stop) が無改変で green。

### PR2: 共有 base イメージ + container entrypoint + boid/shim 焼き込み (inert・ビルドのみ)

**landed**: #819 (`9e83a0f`, 2026-07-23)
- OS 土台 + boid バイナリ + `/run/boid/bin` ディレクトリ土台を焼いた base イメージ (Dockerfile。個々の
  shim symlink は entrypoint 生成 — 決定 2 と整合、image に `<name>` は焼けない)。dir は非 root
  (`--user` uid) の entrypoint が symlink を書けるよう owner/perms を仕込む (or tmpfs /run)。`--user` uid の
  /etc/passwd エントリも焼く (決定 4)。
- container entrypoint (clone/pivot skip、mount/file/symlink + runAgent + postJobDone + signal forward)。
  共有ロジックを runner_linux.go から抽出。まだ dispatch から呼ばれない。イメージビルド CI を足す。

### PR3: `sandbox.Spec` → docker realization 層 (inert・単体テスト)

**landed**: #817 (`5a233a9`, 2026-07-22)
- backend-neutral な可視性要求と userns concrete な mount plan を分離。`[]Mount`/Env/WorkDir を docker
  volumes/env に翻訳、**`/workspace/<name>` は container-local に着地** (host bind にしない)。
  `Guard`/`DetectType` の扱いを固定。

### PR4: broker/gateway/dockerproxy の TCP (mTLS) transport 追加 (socket と併存・inert 寄り)

**landed**: #818 (`f185c3d`, 2026-07-23)
- UNIX socket に加え TCP(mTLS) listener を足す。userns は socket 継続、container は TCP を使う準備。
  gateway の sandbox 向け URL 生成を backend 別に (userns=10.0.2.2 / container=サービス名)。

### PR5: `containerBackend` 実装 + image 選択配線 (config 非公開)

**landed**: #820 (`b633889`, 2026-07-23)
- PR3 翻訳 + docker API (create/start/attach/logs/wait/kill、sibling コンテナ via docker.sock) で
  `SandboxBackend` を実装。workspace `ContainerImage` を JobSpec→Spec→backend に流す。default/pull policy。
- 内部フラグ/テスト専用で「hook job が compose ネットワーク上で通る」ことを確認 (config 未公開)。

### PR6: daemon コンテナ化 + egress/dockerproxy の compose 配置

**landed**: #822 (`1107810`, 2026-07-23)
- daemon を compose サービス化 (docker.sock mount、SQLite/runtimes/home を host bind — 決定 4、TCP
  listener)。broker が exec する host command 実体の provisioning も含む (決定 4)。
- egress proxy コンテナ (dual-home、静的骨格 + workspace network は daemon が動的生成・connect — 決定 5) +
  job internal network + dockerproxy (per-job client cert 配送 + sibling の workspace network 強制注入 =
  request body rewrite — 決定 5) を compose スタックに。直 IP 拒否検証。
- DB schema 天井チェック (未知 migration version で起動拒否)・`install_id` 生成 + resource label 付与・
  `boid reap` subcommand・server socket の host 同一 path bind (相互排他) を実装 (決定 4/6)。
- deploy 機構 (イメージビルド → daemon 再起動) を整備。**host 旧デプロイと state 共有**の配線。
- **PR6 時点では config 非公開 = 実運用 dispatch には載らない**。startup reap / Wait 単一所有 / 永続
  transcript spool は PR7 でまとめて入るため、**PR6 の compose スタックを本番 deploy してはならない**
  (単体では再起動二重実行・log 消失が残る、codex Major 11)。

### PR7: 起動時 reap + 診断 + Wait 順序 + config 公開 (cutover)

**landed**: #823 (`711d7bb`, 2026-07-23)
- `ReapOrphans` を startup の `MarkStale*`↔auto-reopen 間に配線 (reap 失敗→reopen スキップ)。
- 診断 3 分類 (jsonl 参照 / runner-state RPC / transcript は daemon spool で full 永続 + tail は異常系
  メタのみ、決定 8)。Wait 単一所有 + 順序契約。ここで container backend を実運用に公開 (egress + reap +
  daemon 化が揃い安全)。
- **config 公開 (cutover) の gate**: container e2e green + rollback rehearsal (deploy-level reaper 込み)
  の完了を前提にする (codex Major 11 — 検証前に公開しない)。
- e2e: allowed_domains + 「再起動で孤児残らず二重実行なし」を container で green。

### PR8: file fallback + `~/.boid` tmpfs overlay 退役 (backend 非依存・先行可)

**landed**: #821 (`bdb5bbf`, 2026-07-23)
- claude adapter `writePayloadPatch` (と shell hook / 正規 doc) の writer を RPC / runner→broker patch に
  移行 → `resolveJobOutput` の file read・writer・placeholder・tmpfs overlay を一括撤去。両 backend で。
- writer 移行が済めば PR1–7 と独立に先行 land 可能。

### PR9: container e2e + dogfood + 旧デプロイ撤去 + doc (finale)

**status**: 実装 PR 提出済み (2026-07-23)、**CI green 待ち — 未 landed**。`e2e-container` job を real docker
で繰り返し実行し、見つかった実装ギャップを都度修正 (下記一覧) — job dispatch + clone 試行まで到達したが
clone 先ディレクトリの `permission denied` が未解決で、CI はまだ red。この節自体は PR9 の変更内容。

- container e2e: `.github/workflows/blackbox-e2e.yml` に **`e2e-container` job を追加** (`e2e/run-container.sh`
  が実体)。既存 `e2e` job (userns backend、fake docker) は無改変で並走。ubuntu-24.04 の標準搭載 docker +
  compose v2 で `scripts/deploy-container.sh` を呼び、実 compose スタックをビルド・起動して sibling 疎通
  3 要件 (決定 5) を real docker 相手に検証: ① job → 自 workspace の sibling へ到達、② job → 別 workspace の
  sibling へ非到達、③ `docker compose down` + `boid reap` で container/network/volume が `boid.install_id`
  ラベルごと全部消える。dispatch レイテンシは実行のたびログに `[e2e-container][latency]` 行で記録 (podman
  代理実測 ~150-165ms との比較材料、CI 実測は毎回の run ログ参照)。
- **container e2e job を実装する過程で、real docker に対して一度も検証されていなかった実装ギャップが
  複数見つかり、本 PR で修正した** (PR6/PR7 は unit test / fake docker のみでの検証だったため、real docker
  の初回実行で初めて表面化した — plan doc 自身が compose.yml の header comment で予告していた通り):
  - **`LaunchOptions.Workspace`/`DockerEnabled` が実 dispatch 経路で未配線だった** (`Runner.launchSandbox`
    の `backend.LaunchOptions{}` リテラルが両フィールドを一切セットしていなかった — userns backend は
    どちらも読まないため無症状だったが、container backend は `opts.Workspace`/`opts.DockerEnabled` に
    依存する全機能 (workspace network 分離・per-job docker capability) が本番では常に無効化されていた)。
    `internal/dispatcher/runner.go` の `Dispatch`/`launchSandbox` を修正し、`workspaceID`/
    `spec.Visibility.DockerEnabled` を実際に橋渡しするよう修正 (regression test:
    `internal/dispatcher/runner_launch_options_workspace_test.go`)。
  - **workspace 単位の network 分離 (決定5) が containerBackend に一切実装されていなかった** — job container
    は `NetworkingConfig` 無指定で docker の暗黙 default bridge に乗るだけで、workspace 間の L3 分離が
    存在しなかった。`internal/dispatcher/container_backend.go` に `ensureWorkspaceNetwork`
    (`Internal: true` な per-workspace network を idempotent に作成 + job container を `NetworkingConfig`/
    `HostConfig.NetworkMode` 両方でそこに固定接続。ensure 失敗は fail-closed) と、daemon 自身をその
    network へ self-connect する仕組み (`ContainerBackendOptions.SelfContainerID`、compose 環境では
    `$HOSTNAME` = 自コンテナ ID) を追加。後者が無いと job container が gateway/egress (同じ daemon
    コンテナ内) に到達できず、**project-visible dispatch が前提とする clone (`Visibility.Clone`) が
    ネットワーク分離を有効にした途端に全滅する**という、決定5単体では見えていなかった依存関係だった。
  - **per-job dockerproxy が作る sibling を、その job の workspace network に強制注入する配線が未接続**
    だった (`dockerproxy.Server.SetWorkspaceNetwork` 自体は PR6 で実装済みだが、呼び出し側が皆無)。
    `Runner`(`runner.go`)の `startDockerProxy` 呼び出し箇所で、container backend 選択時のみ
    `ds.proxy.SetWorkspaceNetwork(containerWorkspaceNetworkName(...))` を呼ぶよう配線 (userns backend は
    対応する network 基盤が無いため対象外、既存 e2e への影響ゼロ)。
  - **compose.yml に `XDG_RUNTIME_DIR` が抜けていた** — daemon 自身のソケットが bind-mounted な
    `BOID_RUNTIME_DIR` ではなく container 内の一時領域に着地し、決定4の「server socket の host 同一 path
    bind (相互排他)」契約と、host 側 CLI/e2e から daemon ソケットへ到達する経路の両方が壊れていた。
    `environment: XDG_RUNTIME_DIR: "${BOID_RUNTIME_DIR}"` を追加 (structural test:
    `build/container/compose_test.go` の `TestComposeDaemonHasXDGRuntimeDirEnv`)。
  - e2e-container job 自身が使うフィクスチャ git upstream (self-signed TLS) を daemon コンテナから到達
    させるための `extra_hosts: host.docker.internal:host-gateway` + `SSL_CERT_FILE` passthrough を
    compose.yml に追加 (CI 専用の懸念、production には影響しない inert な追加)。
  - **daemon コンテナが起動直後に crash していた** (実 docker で初めて判明した、PR6 の compose スケルトンが
    一度も実際に動かされていなかったことを示す最大の発見): `cmd/start.go` の `runDaemonChild` は host 旧
    デプロイの二重 fork 前提のまま — ①stdin/stdout/stderr を boid.log へ self-pipe dup2 redirect、②
    `syscall.Setsid()` — を無条件に実行していた。**`Setsid()` はプロセスが既に process group leader の
    ときに EPERM で失敗する**契約があり、container の entrypoint プロセス (tini の直接子) はまさにその
    状態に当たる。「setsid: operation not permitted」というエラーメッセージが①で redirect 済みの
    self-pipe に書き込まれ、goroutine 側の `logrotate.Writer` が (別の理由で) 書き込みに失敗して
    pipe の読み取り端を close → 次の書き込みで SIGPIPE (exit 141) というプロセス kill に化けたため、
    `docker logs`/`docker compose logs` には**何も表示されず**、診断が非常に難航した (`boid.log` も
    self-pipe が機能する前に作られないため存在しない)。`internal/daemon/daemon.go` に
    `BOID_LOG_STDOUT` env var (`daemon.ShouldLogToStdout()`) を追加し、container 実行時は
    ①self-pipe redirect と ②Setsid の両方を skip (どちらも「制御端末から detach する」という host
    daemon 二重 fork 固有の目的のためのコードで、container には最初から controlling terminal が無く
    無意味 — supervisor が既に stdout capture + process/session lifecycle を持つ)。診断を可能にした
    副産物として `XDG_STATE_HOME` (boid.log を bind-mounted `BOID_RUNTIME_DIR` へ host-visible にする) と
    `docker run` ベースの pre-flight (BOID_RUNTIME_DIR が BOID_UID:BOID_GID で書き込み可能かを compose
    抜きで即座に確認する `e2e/run-container.sh` の一手順) も追加 — こちらは permission 起因の仮説を
    早期に棄却できた点で有用だった。
  - **container backend の job container が userns 専用の boid バイナリ single-file bind を試みて失敗**
    していた: `BuildSandboxSpec` は `rt.BoidBinary`（= 実行中プロセス自身の `os.Executable()`）を無条件
    に `sandboxShimBinDir + "/boid"` へ bind mount しようとしていた。userns では sandbox がホストと
    mount namespace を共有する前提なので機能するが、container backend は共有イメージに boid を焼き込む
    済み（決定2）であり、しかも sibling create の bind source は「host の real docker daemon が解釈する
    host 絶対 path」（決定4）である一方 `rt.BoidBinary` は daemon 自身の**コンテナ内**パス
    (`/usr/local/bin/boid`) なので、`container create: ... bind source path does not exist:
    /usr/local/bin/boid` で毎回失敗していた。`SandboxRuntimeInfo.UsingContainerBackend` を追加し、
    container backend のときはこの bind を skip（host-command shim symlink 生成は両 backend で継続）。
  - **`BOID_BUILTIN_SHIM` が container backend の job container 自身の entrypoint (`boid
    runner-container`) を誤って shim 経路へ misroute していた** — `main.go` の
    `shouldRunBoidBuiltinShim` は「argv0 の basename が `boid`」かつ「`BOID_BUILTIN_SHIM=1`」なら
    `RunBoidShim` へルーティングする。この env var は本来「サンドボックス内で hook script が
    `boid task update` のような**入れ子の** boid 呼び出しをするときだけ」効くべきものだが、userns
    backend では `spec.Env` が runner-outer/-inner/-inner-child チェーンには適用されず最終 exec にしか
    効かないため、この区別が偶然成立していた。container backend は `docker create` の `Config.Env` が
    コンテナの PID1（= entrypoint 自身 = `boid runner-container`）に最初から効くため、job が
    `capabilities.docker` を宣言している限り entrypoint 自身が shim 経路に誤爆し
    「unsupported boid subcommand "runner-container"」で毎回落ちていた。`main.go` に
    `isReservedRunnerSubcommand`（`runner-outer`/`runner-inner`/`runner-inner-child`/
    `runner-container` の 4 つ）を追加し、これらは `BOID_BUILTIN_SHIM` の値に関わらず常に通常の
    `cmd.Execute()` へ通すよう修正。
  - **`ContainerBackendOptions.UID`/`GID` が一度も配線されておらず、job container が常に 1000:1000
    (デフォルト値) で起動していた** — daemon 自身の実際の uid (compose の `user:
    ${BOID_UID}:${BOID_GID}`) と一致する保証が無く、host の実 uid が 1000 でない環境（GitHub Actions
    runner は 1001）では、daemon プロセスが作成した workspace home ディレクトリ (mode 0700、daemon の
    uid 所有) に job container 側 (uid 1000) がアクセスできず `permission denied` になっていた。
    `sandboxBackendForConfig` に `os.Getuid()`/`os.Getgid()`（daemon 自身の実行時 uid/gid）を
    `ContainerBackendOptions.UID`/`GID` として渡すよう修正 — 決定4の「job container は `--user <daemon
    uid>:<gid>`」を初めて実際に満たす。
  - 上記はいずれも「§決定2/§決定4/§決定5/§決定3 は書かれていたが実装・検証が伴っていなかった」ギャップ
    であり、real docker での e2e 検証を経て初めて発見・修正できた — PR6/PR7 レビューの想定通り、この
    PR が「本当に動くこと」の最終確認レイヤーだった。

**PR9 終了時点の既知の残課題 (未解決)**: 上記の修正を経て `e2e-container` job は「daemon 起動 → project
登録 → workspace 割当 → task dispatch → job container 起動 → git gateway 経由の clone 試行」まで到達する
ようになったが、**clone 先ディレクトリ (`/workspace/<project name>`、host-backed bind mount) への書き込みが
`permission denied` で失敗する**問題が残っている（`git clone` 自体は開始し `Cloning into
'...'` まで進むが `.git` の作成で失敗)。bind mount の source (`<RuntimesDir>/<job_id>/workspace`) は
daemon プロセス (今回の UID/GID 修正後は job container と同じ uid) が `os.MkdirAll(..., 0o755)` で
作成しており、既に判明した「daemon と job container の uid 不一致」系の問題ではなさそうだが、根本原因は
未特定 — docker の bind mount ターゲット auto-create 挙動、または `cloneDirNameForVisibility` が
project の表示名 (`meta.name`、空白を含みうる) をそのままディレクトリ名に使っている点との関連を含め、
次セッションでの実機調査 (`docker exec` で `/workspace` の実際の owner/permission を直接確認するなど) が
必要。[phase6-cutover-followups.md](phase6-cutover-followups.md) に追跡事項として記録。
- **旧デプロイ撤去計画**: [phase6-cutover-followups.md](phase6-cutover-followups.md) を新設し、host 旧デプロイ・
  userns backend・host daemon 起動経路・config `sandbox.backend` option の段階的撤去タイムラインを記録
  (**PR9 では実撤去はしない** — 撤去計画 doc の作成と、撤去可能な形の準備 (deprecation skeleton: 下記) のみ)。
- **deprecation skeleton**: `usernsBackend`/`LocalRuntime`/`SandboxPreparer`/`JobRuntime` の doc comment に
  `Deprecated:` 表記を追加 (振る舞い変更ゼロ、撤去は followup PR)。`boid start` の bare 起動 (非 --foreground /
  非 compose) 成功時に compose 移行を促す情報表示を追加 (`cmd/start.go`
  `printBareStartDeprecationNotice`、起動自体の挙動・終了コードは無改変)。
- ホスト側 dogfood (compose スタックの実運用) は nose 判断待ち — followups doc のタイムライン参照。

### 順序と依存
- PR1 独立・先行。PR2/PR3/PR4 は PR1 後に並行可 (inert/併存)。PR5 は PR2+PR3+PR4 依存。PR6 は PR5 依存。
  PR7 は PR6 依存 (cutover)。**PR8 は writer 移行さえ済めば独立に先行 land 可**。PR9 は全体後。

---

## 未解決論点

2026-07-22 の nose 決定で全論点が確定した (変更履歴参照)。残るのは決定済み方針の小さな実装残余のみ:

- **ExtraRepos の gateway URL 誘導の配線** (決定 12): sandbox 内の git/go を gateway に向ける経路
  (insteadOf 生成 or 明示 URL 案内) が未実装。private git 依存を実際に使う workspace が出る前に配線方式を
  決める。
- **`PidsLimit` default の要否** (スコープ節): cgroup 語彙は Phase 7 送りだが、fail-safe default を PR5 で
  入れるかは実装時の任意判断。

PR9 で「dispatch レイテンシの docker 追試」はクローズした: `e2e-container` job が実行のたびに実 docker で
1 サイクル計測して `[e2e-container][latency]` ログ行に記録する形にした (podman 代理実測 ~150-165ms/cycle
との比較材料が毎 CI run で得られる)。判定 (pull 済み前提で問題なし) は変更なし。

---

## 関連ドキュメント

- 親: [container-based-boid.md](container-based-boid.md) — 移行戦略ステップ 6/7 / 目標アーキテクチャ。
- 直前段: [phase5-shim-and-task-context.md](phase5-shim-and-task-context.md) — decision 7 (file fallback +
  tmpfs overlay の退役条件、本 phase 決定 9 で成立)。
- [git-gateway-cutover.md](git-gateway-cutover.md) — `/workspace/<name>` clone 契約 / gateway (loopback
  bind の現状、本 phase で TCP 化)。
- [home-workspace-volume.md](home-workspace-volume.md) — $HOME volume レイアウト、session jsonl 永続。
- [docker-native-proxy.md](docker-native-proxy.md) — dockerproxy (Phase 1 landed)、本 phase で broker 側配置。
- [workspace-db-consolidation.md](workspace-db-consolidation.md) — `ContainerImage` フィールド (image 選択の入口)。
- 後続: [phase6-cutover-followups.md](phase6-cutover-followups.md) — PR9 で新設。host 旧デプロイ・userns
  backend・host daemon 起動経路・config `sandbox.backend` option の段階的撤去計画。

---

## 変更履歴

- 2026-07-22: 初版 draft 作成 (移行戦略ステップ 6、daemon-on-host・順次差し替え前提)。
- 2026-07-22: codex 設計レビュー 2 巡反映。interface に WS attach ストリーム面 + Adopt/ReapOrphans、
  Signal 意味論保存、trusted endpoint 到達 (loopback bind)、PR7 の adapter writer 移行、Reconcile の
  割り切り、`sandbox.Spec`/version skew のトーンダウン、resize 3 経路、reap-before-reopen を反映。
- 2026-07-22: **方針転換 (論点 D、nose 決定) — 一気コンテナ化 (daemon 込み) に再構成**。
  - daemon-on-host・順次を廃し、単一ホスト compose (daemon + job + egress proxy) の一気移行に。理由:
    blast radius 小 (ユーザ 2 名)・顧客展開速度・2 構成恒久保守回避。
  - **短期 fallback を「host 旧デプロイ (v0.0.13) への deploy-level rollback」に確定** (config ホット
    フリップは nested userns のため不採用、恒久 2 構成なし)。
  - 旧版の Blocker/Major の相当数が消滅: gateway/proxy の loopback 到達ハック (旧決定 5) → compose
    ネットワークのサービス DNS で不要 (決定 5)。boid バイナリ bind mount + version skew + shim 同居
    (論点 A/C) → 共有 versioned イメージへの焼き込みで解消 (決定 2)。socket→TCP の二度手間 → 最初から
    TCP (決定 5)。
  - **scope 前倒し**: daemon コンテナ化 (決定 4) と egress/dockerproxy の broker 側配置 (決定 5) を
    元ステップ 7 から Phase 6 に取り込み。Phase 7 は k8s / 別ホスト / mirror / remote runner に純化。
  - 論点 B は「global container + workspace ContainerImage = image 選択」に確定 (決定 11)。論点 E
    (診断 3 分類) は決定 8 に反映。

- 2026-07-22: **security 群 5 論点を nose 決定で確定** (未解決論点 → 決定 4/5/6・安全網へ格上げ)。
  Explore で現行実装の根拠を採取: dockerproxy は既に docker API allowlist + bind/Privileged 拒否 +
  fail-closed の socket-proxy 相当ハードニング (`policy.go`)、broker/gitgateway に per-job capability
  token パターン、startup reap は job orphan (`MarkStale*`) と docker orphan (GC ledger reaper) が別経路。
  - docker socket: 生 socket は daemon 1 箱集約 + job は dockerproxy 経由、rootless は Phase 7 (決定 4)。
  - job UID/GID: `--user` で host uid 揃え + named volume owner 調整、userns-remap 不使用 (決定 4)。
  - network 分離: workspace 単位 internal network + egress dual-home (決定 5)。
  - dockerproxy: 共有 TCP + per-job token (既存 broker/gitgateway token パターン流用) (決定 5)。
  - rollback reaper: label (`boid.job_id`/`install_id`) ∪ ledger 二重ソースの daemon 非依存 CLI
    (`boid reap`)、reap を `MarkStale*`↔reopen 間に移設 (決定 6・安全網)。
  - 未解決論点は「security 群 5 論点の実装残余」1 項に圧縮 (方針確定・formalize は着手時)。

- 2026-07-22: **security 群確定 (#813) の決定 5 反映漏れを修正**。network 分離の粒度が決定 5 本文で
  未決の書き方のまま残っていたのを「workspace 単位 internal network」の確定記述に更新し、#813 で削除済みの
  未解決論点「ネットワーク分離の粒度」への dangling 参照を除去。dockerproxy token の broker/gitgateway
  パターン流用も本文に明記 (いずれも #813 の変更履歴・commit message には記録済みだった決定内容)。

- 2026-07-22: **残り未解決論点 10 項 + 新規 1 項 (host docker 不在) を nose 決定で全面確定**。根拠は現行
  実装の Explore 実査 5 本 (signal / attach・transcript / DB schema / dockerproxy・token / CI・credential・
  peers) + host 実測。
  - host engine: **docker engine (rootful) + compose v2 を cutover 前に導入** (現状 host は podman のみ、
    前提節に追加)。
  - signal forwarding: **`HostConfig.Init: true` (docker-init/tini 相当)** (決定 3)。転送は既存 sigutil、
    新規責務は zombie reap + PID1 disposition のみ。
  - transcript/attach: **現行 LocalRuntime session 層を抽出・流用**し source を docker attach に差し替え
    (決定 8)。live 消費者に Web UI SSE follow がある事実を棚卸しと PR1 に追記。
  - sibling 疎通: **workspace network 強制注入 + container IP 直 + publish/bind 非サポート**、dockerproxy
    は body rewrite proxy 化 (決定 5)。
  - schema 互換: **移行中 migration 加法限定 + 天井チェック (PR6) + socket 同一 path bind で相互排他**
    (決定 4)。
  - CI: **e2e-container job + `requires-docker` marker、userns e2e 並走** (PR9)。
  - dispatch レイテンシ: **podman 代理実測 ~150-165ms/cycle でクローズ** (docker 追試のみ残)。
  - cgroup 語彙 / `boid workspace peers`: **Phase 6 スコープ外へ** (スコープ節)。
  - registry credential: **`secret:` env + allowed_domains 加算、$HOME 永続例外は拡大しない** (決定 12 新設)。
  - security 実装残余: token 配送 = **per-job client cert を env 配送** (URL path は `DOCKER_HOST` 制約で
    不成立) / **install_id file + resource label + `boid reap` (label ∪ ledger)** / egress compose =
    **静的骨格 + 動的 network connect** (決定 5/6)。
  - **#813 の決定 4 訂正 (nose 確認済み)**: workspace HOME の named volume 化は誤り (dockerproxy policy が
    縛るのは sibling のみで daemon 直作成の job container bind は縛られない + persistence の同一ホストパス
    共有と矛盾) → **Phase 6 は host bind 維持**、named volume 化 + owner 調整は Phase 7 へ。
  - 旧記述の訂正: 「git 依存は insteadOf→gateway で解決済み」は誤り — **insteadOf は未実装**、ExtraRepos の
    PermFetch のみ実装済み (決定 12、URL 誘導の配線は実装残余)。
  - PR2 の「shim symlink を焼いた base イメージ」記述が決定 2 (dir 土台のみ焼く・symlink は entrypoint
    生成) と矛盾していたのを整合させた。

- 2026-07-23: **Phase 6 全 9 PR landed、実装完結 (PR9 finale)**。PR1–9 の landed commit をそれぞれの
  節に pin (#816–#823)。PR9 で `e2e-container` CI job・旧デプロイ撤去計画 doc
  ([phase6-cutover-followups.md](phase6-cutover-followups.md))・deprecation skeleton・親 doc/CLAUDE.md 更新を
  実施。container e2e の実装過程で real docker に対して一度も検証されていなかった実装ギャップ
  (LaunchOptions.Workspace/DockerEnabled 未配線・workspace network 分離未実装・dockerproxy
  SetWorkspaceNetwork 呼び出し未接続・compose.yml の XDG_RUNTIME_DIR 欠落) が複数見つかり、本 PR で修正
  — 詳細は PR9 節本文。ステータスを draft → landed に更新。
