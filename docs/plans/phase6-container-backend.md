# Phase 6 実装計画: container backend への一気移行 (daemon 込み)

ステータス: **draft (着手前)**。設計提案であり、決定事項は nose レビュー前提。
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
- **daemon のコンテナ化** — SQLite/runtimes を named volume に、docker socket を mount して
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
  half-close の意味論はここ。
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
する現行意味論を pin。単純な docker kill にすると agent-stop が強制終了に化ける。tini 相当を挟むか
自前 PID1 で forwarding するかは実装時判断 (未解決論点)。

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
- **persistence**: SQLite (tasks 管理 DB)・runtimes dir・workspace home volume・credential・web_secret を
  named volume / host bind で永続化。**短期 fallback のため、これらは host 旧デプロイと同一ホストパスを
  共有**し、切り戻しで state を失わない (schema 互換を移行中は保つ)。
- **docker socket の security posture**: docker.sock = root 相当。daemon コンテナに渡す前提のリスクを
  明示し、job コンテナには渡さない (job は dockerproxy 経由のみ、現行同様)。
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
  出すと job が別 workspace の緩い proxy endpoint を直接選べる**ため、network 分離の粒度 (workspace ごとの
  private network、または呼出元 identity × workspace policy の強制紐付け) を **cutover 前の security
  invariant** として決める (下記「ネットワーク分離の粒度」を任意論点から格上げ)。
- **dockerproxy** も broker 側 (compose スタック) に配置。job の `DOCKER_HOST` は proxy サービスを指す。
  ただし現行の **per-job UNIX socket + per-server ledger による scope を共有 TCP service に置き換えると、
  docker capability を持たない job も service を直接呼べてしまう** (codex Blocker 4)。共通 client cert の
  mTLS は transport 認証にしかならないため、**per-job の短命 identity/token を発行し、server 側で
  `jobID → DockerEnabled → 専用 ledger` に紐付け**る。無 capability job は接続時点で拒否し、resource ID
  scope と cleanup ledger を job ごとに永続化する (資格情報は共有 image に焼かない)。
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
- interface を attach ストリーム面 + Adopt + ReapOrphans + Signal + **resize 3 経路** (`ws_attach.go` +
  `job_runtime_routes.go` の直呼び) 込みで定義。`launchSandbox`/`runtime_subscriber_export.go` を
  `usernsBackend` に集約。`JobRuntime`/`LocalRuntime`/`SandboxPreparer` は内部 transport として温存。
- 既存の全 unit/e2e (WS attach・CLI resize・agent-stop) が無改変で green。

### PR2: 共有 base イメージ + container entrypoint + boid/shim 焼き込み (inert・ビルドのみ)
- OS 土台 + boid バイナリ + `/run/boid/bin/<name>` shim symlink を焼いた base イメージ (Dockerfile)。
- container entrypoint (clone/pivot skip、mount/file/symlink + runAgent + postJobDone + signal forward)。
  共有ロジックを runner_linux.go から抽出。まだ dispatch から呼ばれない。イメージビルド CI を足す。

### PR3: `sandbox.Spec` → docker realization 層 (inert・単体テスト)
- backend-neutral な可視性要求と userns concrete な mount plan を分離。`[]Mount`/Env/WorkDir を docker
  volumes/env に翻訳、**`/workspace/<name>` は container-local に着地** (host bind にしない)。
  `Guard`/`DetectType` の扱いを固定。

### PR4: broker/gateway/dockerproxy の TCP (mTLS) transport 追加 (socket と併存・inert 寄り)
- UNIX socket に加え TCP(mTLS) listener を足す。userns は socket 継続、container は TCP を使う準備。
  gateway の sandbox 向け URL 生成を backend 別に (userns=10.0.2.2 / container=サービス名)。

### PR5: `containerBackend` 実装 + image 選択配線 (config 非公開)
- PR3 翻訳 + docker API (create/start/attach/logs/wait/kill、sibling コンテナ via docker.sock) で
  `SandboxBackend` を実装。workspace `ContainerImage` を JobSpec→Spec→backend に流す。default/pull policy。
- 内部フラグ/テスト専用で「hook job が compose ネットワーク上で通る」ことを確認 (config 未公開)。

### PR6: daemon コンテナ化 + egress/dockerproxy の compose 配置
- daemon を compose サービス化 (docker.sock mount、SQLite/runtimes/home を volume、TCP listener)。
  broker が exec する host command 実体の provisioning も含む (決定 4)。
- egress proxy コンテナ (dual-home) + job internal network + dockerproxy (per-job token) を compose
  スタックに。直 IP 拒否検証。
- deploy 機構 (イメージビルド → daemon 再起動) を整備。**host 旧デプロイと state 共有**の配線。
- **PR6 時点では config 非公開 = 実運用 dispatch には載らない**。startup reap / Wait 単一所有 / 永続
  transcript spool は PR7 でまとめて入るため、**PR6 の compose スタックを本番 deploy してはならない**
  (単体では再起動二重実行・log 消失が残る、codex Major 11)。

### PR7: 起動時 reap + 診断 + Wait 順序 + config 公開 (cutover)
- `ReapOrphans` を startup の `MarkStale*`↔auto-reopen 間に配線 (reap 失敗→reopen スキップ)。
- 診断 3 分類 (jsonl 参照 / runner-state RPC / transcript は daemon spool で full 永続 + tail は異常系
  メタのみ、決定 8)。Wait 単一所有 + 順序契約。ここで container backend を実運用に公開 (egress + reap +
  daemon 化が揃い安全)。
- **config 公開 (cutover) の gate**: container e2e green + rollback rehearsal (deploy-level reaper 込み)
  の完了を前提にする (codex Major 11 — 検証前に公開しない)。
- e2e: allowed_domains + 「再起動で孤児残らず二重実行なし」を container で green。

### PR8: file fallback + `~/.boid` tmpfs overlay 退役 (backend 非依存・先行可)
- claude adapter `writePayloadPatch` (と shell hook / 正規 doc) の writer を RPC / runner→broker patch に
  移行 → `resolveJobOutput` の file read・writer・placeholder・tmpfs overlay を一括撤去。両 backend で。
- writer 移行が済めば PR1–7 と独立に先行 land 可能。

### PR9: container e2e + dogfood + 旧デプロイ撤去 + doc
- container e2e を CI で回す配線 (docker socket 可用性、compose、sibling 疎通)。dispatch レイテンシ実測。
- ホスト側 dogfood (compose スタック)。安定確認後、**host 旧デプロイ・userns backend・host daemon
  起動経路の撤去計画**を実行 (別 PR 群に分割可)。plan/親 doc の完結記録更新。

### 順序と依存
- PR1 独立・先行。PR2/PR3/PR4 は PR1 後に並行可 (inert/併存)。PR5 は PR2+PR3+PR4 依存。PR6 は PR5 依存。
  PR7 は PR6 依存 (cutover)。**PR8 は writer 移行さえ済めば独立に先行 land 可**。PR9 は全体後。

---

## 未解決論点

Phase 6 スコープ内で、決定まで要らず着手時に詰める論点 (Phase 7 専用 = mirror ワーカー / RWX 並行 RW /
DB 移行 / k8s / live 再吸着 はここに挙げない)。

- **docker socket の security 運用**: daemon コンテナに docker.sock を渡す前提のリスク受容と、job には
  渡さない境界 (dockerproxy 経由のみ) の担保。rootless docker / socket proxy でのハードニング可否。
- **container 内 entrypoint の signal forwarding 実装**: tini 相当を挟むか自前 PID1 か (決定 3)。
- **bind mount / volume の UID/GID**: $HOME (host owner RW bind)・docker.sock (owner-only) の ownership。
  container を root で動かすか host UID か、image 内ユーザとの整合、rootless/remap 対応。
- **短期 fallback の state 共有と schema 互換**: host 旧デプロイと compose daemon が同一ホストパスの
  SQLite/home を共有する際の、移行中の DB schema 互換維持と切替手順 (決定 4)。
- **dockerproxy の sibling container 疎通**: proxy は docker API 検査転送のみで、job と TestContainers が
  作る sibling のデータプレーン接続・path mapping はしない。bind mount 拒否のため container-local
  `/workspace` を sibling に渡す用途は不成立。internal network 下での公開 port 到達・network join・bind
  非対応時の契約を container e2e 要件として決める。
- **ネットワーク分離の粒度 (cutover 前の security invariant に格上げ、決定 5)**: internal network を
  workspace ごとに分けるか (別 workspace 間 L3 到達 / job が別 workspace の緩い proxy endpoint を選べる
  問題)。同一 workspace 内 job 間到達の要否。egress proxy の dual-home 構成と、呼出元 identity × workspace
  policy の強制紐付けをここで確定する。
- **rollback の deploy-level reaper 実装 (安全網、codex Blocker 1)**: 新 compose daemon が停止/起動不能でも
  生きている sibling job container を停止・削除できる daemon 非依存の reaper (CLI/script) をどう実装するか。
  依拠する label / install ID / 永続 ledger の source を確定する。
- **dockerproxy の per-job 認可と子 resource reap (決定 5/8、codex Blocker 4)**: 共有 TCP service で job
  identity/token をどう発行・検証するか。TestContainers が作る子 container/network/volume は label を
  持たないため reap は per-job 永続 ledger に依存する。ledger の永続先と daemon 再起動跨ぎの整合。
- **daemon 側 transcript spool / attach fan-out の実装 (決定 8/9、codex Major 7/9)**: 1 本の docker attach
  所有者から複数 subscriber へ fan-out しつつ full を runtime storage に spool する仕組み。snapshot と live
  の無欠落接続・完了後 replay・TTY/非 TTY multiplex・container remove との順序。
- **リソース制限 (cgroup)**: cpu/メモリ/pids/ディスクは container で自然に得られる新能力。workspace/job
  設定の語彙にどう出すか (Phase 6 では最小 or 見送り可)。
- **container backend の e2e を CI でどう回すか**: docker socket 可用性、compose、既存 blackbox-e2e との
  関係、sandbox 内から回せない制約。
- **dispatch レイテンシ実測**: コンテナ create/start の実測。イメージ pull 済み前提の妥当性。
- **npm private registry 等の credential 線引き**: git 依存は insteadOf→gateway で解決済み。registry
  token を $HOME volume に置くと credential 例外が広がる。明示的決定が要る。
- **`boid workspace peers` (Phase 5b 宿題 3)**: peer advertise の CLI 化。backend swap とは独立、取り込みは着手時判断。

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
