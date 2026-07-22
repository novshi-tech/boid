# Phase 6 実装計画: sandbox backend interface 化 + container backend 並走追加

ステータス: **draft (着手前)**。設計提案であり、決定事項は nose レビュー前提。
作成日: 2026-07-22
親ドキュメント: [container-based-boid.md](container-based-boid.md) — 移行戦略ステップ 6

> 改訂メモ (2026-07-22): 初版 draft に codex 設計レビューを実施し、Blocker 5 + Major 8 +
> Minor 2 を反映して改訂。特に interface の attach/stream 面、container からの trusted
> endpoint 到達 (loopback bind 問題)、PR7 の adapter writer 移行、Reconcile の割り切り、
> version skew と `sandbox.Spec` backend 非依存の誇張トーンダウンが主な変更。詳細は
> 「変更履歴」参照。

---

## 目的

契約先行 (contract-first) が Phase 1–5 で完了した。境界の意味論は現行 userns backend の
上で既にコンテナモデルに揃っている (git gateway + sandbox 内 clone / CLI リモート接続 /
workspace DB 一元化 + kit 退役 / $HOME workspace volume / shim 固定 dir + タスクコンテキスト
RPC)。**Phase 6 はいよいよ enforcement 層を差し替える最初の段**であり、契約は移行済みなので
**enforcement 差し替えに集中できる** (ただし後述のとおり、契約先行で扱ってこなかった
「起動経路そのもの」— attach ストリーム面・signal 意味論・trusted endpoint 到達・再起動
recovery — の再設計が必要になる)。

方針は strangler: **sandbox backend を interface 化し、現行 userns backend をその実装の
1 つに押し込んだ上で、container backend (docker) を横に足して並走させる**。一気切替はしない
(親ドキュメント基本方針)。各段階で config フラグ 1 個で userns backend に戻れる状態を維持する。

Phase 5 完了時点の申し送り (`next-session-phase5-5a-cutover` memory の宿題 1–2) が起点:

- 宿題 1: 残ステップ ⑥/⑦ の詳細計画。本 doc が ⑥ を担当し、⑦ (k8s / 別ホスト) は Phase 7 に
  切り出す (下記スコープ節)。
- 宿題 2: Phase 5b で維持された file fallback + `~/.boid` job-scoped tmpfs overlay の退役。
  退役条件 (decision 7 再改訂) を本 phase で成立させる (下記「決定事項」8)。

---

## スコープ

### 含む (移行戦略ステップ 6)

- **sandbox backend の interface 化**: 現行 userns 実行経路を新 interface の実装 1 つにリファクタ
  (振る舞い不変・inert)。interface は attach のライブストリーム面・signal 意味論・再起動時の
  reap まで含む (現状棚卸しで判明した通り、起動 transport は 3 経路 + WS attach 面に分散している)。
- **ローカル container backend (docker)**: **daemon はホストプロセスのまま**、docker API 越しに
  job コンテナを push 型で生成する。
- **同一ホスト前提の clone**: daemon とコンテナホストが同一ディスクなので、job コンテナは
  ホスト repo を直接 `git clone --reference` する。**mirror は不要** (別ホスト構成の論点なので Phase 7)。
- **egress の L3 トポロジ化**: nft + pasta (10.0.2.x) による強制を compose internal network +
  proxy gateway に置換。ポリシーデータ (workspace→許可ドメイン) は boid が持ち続け enforcement だけ差替。
- **container からの trusted endpoint 到達の再設計**: git gateway / egress proxy が現状
  loopback bind + pasta の 10.0.2.2 投影に依存している問題を解消する (下記決定 5)。
- **container backend 固有の運用契約の初期実装**: 診断成果物の回収 (daemon 側 fallback 込み)、
  孤児コンテナ/volume の reap + GC。
- **Phase 5 の負債退役**: `job_done` の file fallback (`~/.boid/output/payload_patch.json`) と、
  それを隔離する `~/.boid` job-scoped tmpfs overlay を退役 (決定 8)。writer 側 (claude adapter) の
  RPC 移行を前段に含む。

### 含まない (Phase 7 = 移行戦略ステップ 7 以降に切り出し)

- **daemon 自体のコンテナ化** (broker/proxy socket をコンテナに mount する構成) と、それに伴う
  **egress proxy / dockerproxy の broker 側再配置**。Phase 6 では両者はホスト側に留め到達させる。
- **k8s backend (operator パターン)** と別ホスト構成全般。
- **mirror + `git clone --reference` の mirror 更新ワーカー**: 別ディスク構成でのみ必要。
- **実行中 job の live 再吸着**: daemon 再起動時に走行中コンテナへ再アタッチして job を継続する機構。
  Phase 6 は現行契約 (再起動 = kill+fail+auto-reopen) を踏襲し、孤児コンテナの reap のみ行う (決定 6)。
- **リモートランナー (pull 型常駐 agent)**: 移行戦略ステップ 8。
- **DB (SQLite → Postgres/PVC)**: チーム共有の論点。

interface は Phase 7 (別ホスト / k8s) を後から足せる形で切る (メソッド境界に別ホスト前提を
持ち込まない) が、実装は Phase 6 スコープ (同一ホスト docker) に限る。

---

## 前提と依存関係

- **ステップ 1–5 landed** (契約先行完了)。本 phase が前提にする契約:
  - **`/workspace/<name>` clone 先契約** (git-gateway-cutover): repo は $HOME 外の中立 path。
  - **`/run/boid/bin/<name>` shim symlink 契約** (Phase 5a): container ではイメージに焼く。
  - **$HOME = workspace 単位 volume 契約** (Phase 4): ホスト側レイアウト
    `~/.local/share/boid/homes/<slug>/` を再利用し bind mount。
  - **タスクコンテキストは broker RPC pull 契約** (Phase 5b): file mount 経路は撤去済み (fallback 除く)。
- **workspace DB の `ContainerImage` フィールド** (`internal/orchestrator/workspace_meta.go:78`、
  "reserved for the Phase 6 container" と明記) が本 phase の image 選択の入口。**現状 JobSpec /
  `sandbox.Spec` に流れていない**ので配線が要る (決定 2)。
- **既存 dockerproxy** (`internal/sandbox/dockerproxy/`、[docker-native-proxy.md](docker-native-proxy.md)
  Phase 1 landed): UNIX socket RPC。ただし socket bind だけでは sibling container 疎通は解けない (未解決論点)。
- **既存 egress ProxyManager** (`internal/sandbox/proxy_manager.go`): ホスト側 loopback listener。

---

## 現状棚卸し (backend の継ぎ目)

現状の sandbox 実行は 3 段: **dispatcher (host, go)** → **`sandbox.Spec` DTO** → **runner
(host→userns, syscall)**。継ぎ目は `sandbox.Spec` の JSON 境界に半分引かれているが、後述のとおり
`sandbox.Spec` は role 非依存ではあっても **backend 非依存ではない**。

### 起動経路が散らばっている (3 経路 + attach 面)

`Runner.launchSandbox` (`internal/dispatcher/runner.go:849`) が起動を 3 つに分けて持つ:

1. `SandboxPreparer.PrepareSandbox(spec sandbox.Spec) (*PreparedSandbox, error)`
   (`preparer.go:20`、実装 `sandbox_preparer.go:20`) — 「JSON を marshal するだけ」ではない。
   RootDir 作成・secret を含む spec file 書き込み・state/cleanup artifact の所有まで担う
   (`PreparedSandbox{SpecPath, StatePath, RootDir, StagingDir}`)。抽出時に cleanup 責務を落とさないこと。
2. `runnerCommand(prepared) → "boid runner-outer --spec … --state …"` (`runner.go:938-948`) —
   **userns entrypoint をハードコードしている唯一の箇所**。
3. `JobRuntime.Start(ctx, RuntimeStartSpec{Command}) (*RuntimeHandle, error)` (`runtime.go:65`、
   実装 `LocalRuntime` `runtime_local_linux.go:68`) — `bash -lc <Command>` を PTY/pipe で起動。

**attach / resize は単一経路ではなく 3 経路に散っている** (初版で見落とした重要点):

- **出力・入力ストリーム**は Phase 3 で WS に一本化済み (`internal/api/ws_attach.go`)。
  `JobRuntime.Attach` は使わず、`h.Subscriber.Subscribe(jobID)` / `h.Writer.WriteInput(jobID, …)` /
  `CloseInput` を **jobID キー**で叩き、`Runner` (`runtime_subscriber_export.go`) が DB で
  runtimeID を引いて `LocalRuntime` 固有メソッド (`SubscribeRuntime` = snapshot + ライブ channel、
  `WriteInputRuntime`、`CloseInputRuntime`) に type-assert 委譲する。snapshot・複数 subscriber・
  half-close の意味論はここにある。ローカル/リモート CLI attach 出力もこの WS を共用する。
- **WS 内 resize**: WS の `resize` メッセージが `h.Writer.ResizeRuntime(jobID, …)` (`ws_attach.go:123`) を叩く。
- **resize の別 HTTP route** (見落としの本命): `POST /api/jobs/{id}/resize` が
  `runtime.jobRuntime.Resize(job.RuntimeID, …)` を**直呼び**する (`internal/server/job_runtime_routes.go:54`)。
  これは `cmd/attach.go` の初期サイズ + SIGWINCH ハンドラ (`c.ResizeJob`) が使う **CLI の resize 経路**で、
  コメントに「survives unchanged / stays the CLI's resize path」と明記。runtimeID キーで backend を経由せず
  `JobRuntime.Resize` を直接呼ぶため、**container backend でここを付け替えないと CLI の resize が
  LocalRuntime に誤配送される** (既存の対話 attach 契約を壊す)。

→ **container backend はこの 3 経路すべてを session 経由に routing しないと WS/CLI attach が壊れる**
(Phase 3 のリモート attach が全滅する)。特に `job_runtime_routes.go` の直呼びは interface 抽出の対象に含める。

### JobRuntime は transport 抽象だが backend 非依存とは言い切れない

`JobRuntime` (Start/Attach/Resize/Wait/Stop/Signal) は「プロセスを起動し PTY/pipe を張る」
transport 抽象で docker の run/attach/wait/kill に近いが、そのままでは足りない:

- **Signal の意味論が userns 前提**: `boid agent stop` は `kill(-pgid, SIGUSR1)`
  (`runtime_local_linux.go:413-429`) で process group 全体に送り、runner は SIG_IGN で無視
  (`runner/runner.go:56-70`、execve 越しに継承)、adapter が受信して Claude だけ SIGTERM した後
  `job_done` する (`claude/run.go:433-440`)。docker の container signal は PID 1 に届くが、
  entrypoint が SIGUSR1 を無視すると消える。**container entrypoint に signal forwarding 経路が要る**。

### userns 固有の実装 (container backend で別実装になる部分) は 3 点に限局

1. **`sandbox.plan.BuildPlan(spec) *Plan`** (`internal/sandbox/plan.go:25`) — base rbind
   (`/bin /usr /etc /opt` …)、`/dev /proc /tmp`、nft drop policy、DNS stub `nameserver 10.0.2.3`。
   → container では **イメージの rootfs + internal network** に移る。
2. **`internal/sandbox/runner/runner_linux.go`** — clone(NEWUSER|NEWNS)+uid_map (170-179)、
   pivot_root/chroot (424)、mount namespace (302)、mount syscall (`applyMount` 340)、nft 適用
   (139-149)、pasta 起動 (47)。→ container runtime が代替。
3. **egress の L3 enforcement** — `BuildPlan` の nft drop + `applyProxyEnv`
   (`sandbox_builder.go:1210`) の `HTTP(S)_PROXY` 注入。→ compose internal network + proxy gateway に置換。

### `sandbox.Spec` は role 非依存だが backend 非依存ではない

`BuildSandboxSpec` (`sandbox_builder.go:183`) は role-aware switch を持たない点で role 非依存だが、
生成する `sandbox.Spec` は userns concrete な契約を含む:

- `ProxyPort` は nft+pasta 前提、`RootDir` / `CleanupPaths` / `Profile` は userns runner 前提、
  `Mount.Source` は **host path** (`spec.go`/`types.go`)。
- 特に `/workspace/<name>` は `cloneMounts` (`sandbox_builder.go:671`) が **ホスト runtime dir を
  bind** する形で組む。PR3 が `[]Mount` を素直に docker volume へ翻訳すると、計画の「container-local
  `/workspace`」にならず host bind になってしまう。
- `Mount.Guard` (shell test 式、`runner/runner.go:91` が評価) と `DetectType` (実行時 stat) は
  docker では表現できない。

→ **backend-neutral な「可視性要求」と userns concrete な mount plan を分ける realization 層**が
必要 (決定 1/PR3)。「Spec は backend 非依存」という初版の主張はこの点でトーンダウンする。

### backend 直交で流用できる部分

entrypoint 内のロジックの多くは backend 直交だが、**まだ関数抽出されていない**:

- ハーネス実行 `runAgent` → `registry.For(spec.HarnessType)` → `adapter.Run` と、broker job-done
  `postJobDone` → `brokerclient.JobDone` は **現状 `runner_linux.go:459-539` にある**。container
  entrypoint 向けの `runner/runner.go` への抽出は PR2 の作業 (初版の「移植可能ヘルパは分離済み」は不正確)。
- broker/dockerproxy socket は UNIX socket bind + RPC (`sandbox_builder.go:281-401`) — container でも
  ホスト socket を bind するだけで同型に流用可能 (TCP listener とは事情が違う、下記 egress 節)。

### 既存の抽象化の状況

- `JobRuntime` (runtime.go:65): transport 抽象。実装は `LocalRuntime` 一本。
- `SandboxPreparer` (preparer.go:20): 実質「Spec を書く」一本。
- **`sandbox.Sandbox` interface** (`internal/sandbox/sandbox.go:4`): **production dead** (呼び出しゼロ)
  だが `testutil/sandbox.go` に test mock 実装が存在する。継ぎ目には使わないが「完全な dead code」ではない。
- **container/compose backend の scaffold: 皆無**。sandbox backend は現状「userns 具象一本」。

### egress / dockerproxy / gateway の現行配線 (loopback bind 問題)

- **git gateway**: `net.Listen("tcp", "127.0.0.1:0")` (`server.go:295`)。sandbox 向け URL は
  `http://10.0.2.2:<port>` で、コメントに「sandboxes reach the host loopback via the slirp-provided
  10.0.2.2 alias」と明記。
- **egress ProxyManager**: 同じく loopback bind (`internal/sandbox/proxy.go:58-63`)、env は 10.0.2.2 投影。
- **docker には 10.0.2.2 投影がない**。docker の `host-gateway` は名前/IP mapping であって、
  loopback-only listener を bridge 側に公開しない。→ **container からは gateway clone も egress proxy も
  到達不能**。egress 以前に PR4 の gateway clone が失敗する (決定 5 / PR4 前提)。
- **dockerproxy**: `startDockerProxy(runtimeID)` (`runner.go:627`) が runtime dir に UNIX socket を
  bind し Serve、sandbox には `/run/boid/docker-proxy.sock` として bind。socket RPC 自体は backend 非依存だが、
  「job container が作る sibling container との疎通」は socket bind だけでは解けない (未解決論点)。

---

## 決定事項 (提案 — nose レビュー前提)

### 1. 新 interface `SandboxBackend` / `SandboxSession` を導入し、起動 + attach 面を束ねる

`launchSandbox` が inline で持つ (PrepareSandbox + runnerCommand + JobRuntime.Start) と、別面に
散っている WS attach 面 (Subscribe/WriteInput/CloseInput/Resize) を、backend 選択の 1 抽象にまとめる。

```go
// internal/dispatcher (or internal/sandbox/backend)
type SandboxBackend interface {
    // Launch は 1 job 分の隔離境界・rootfs・mount・egress を realize し entrypoint を起動する。
    Launch(ctx context.Context, spec sandbox.Spec, opts LaunchOptions) (SandboxSession, error)
    // Adopt は launch 後に runtimeID から session を再構成する (WS attach / signal / stop の
    // 後続呼び出しは jobID→runtimeID 解決を経てここに入る)。DB には runtimeID + backend 種別を残す。
    Adopt(ctx context.Context, runtimeID string) (SandboxSession, bool)
    // ReapOrphans は daemon 再起動後に label で実行中コンテナ等を列挙して破棄する (決定 6)。
    ReapOrphans(ctx context.Context) error
}

type SandboxSession interface {
    ID() string
    // ライブ出力 (WS attach)。snapshot + 後続 chunk channel + unsubscribe。
    Subscribe() (snapshot []byte, ch <-chan []byte, cancel func(), ok bool)
    WriteInput(data []byte) error
    CloseInput() error
    Resize(size TerminalSize) error
    // 対話 attach (ローカル CLI)。
    Attach(ctx context.Context, req RuntimeAttachRequest) error
    Wait(ctx context.Context) (RuntimeExit, error)
    Stop(ctx context.Context) error
    // Signal は agent-stop の意味論を保つ (SIGUSR1 を pgroup / entrypoint forwarding 経由で
    // adapter に届け、graceful job_done させる。単純な container kill にしない — 決定 3)。
    Signal(ctx context.Context, sig syscall.Signal) error
}
```

- `LaunchOptions` は現行 `RuntimeStartSpec` の attach メタ (JobID/…/Interactive/TTY/StdinForward/
  DesiredID) を担う。`RuntimeAttachRequest`/`TerminalSize`/`RuntimeExit` は既存語彙を流用。
- **usernsBackend**: 現行の (PrepareSandbox + `boid runner-outer` + `LocalRuntime`) を合成。
  `JobRuntime`/`LocalRuntime`/`SandboxPreparer` は削除せず内部 transport として温存する。
  `Subscribe`/`WriteInput` 等は既存の `LocalRuntime.SubscribeRuntime`/`WriteInputRuntime` に委譲。
- **containerBackend**: 同じ `sandbox.Spec` を docker create の (image, volumes, env, workdir,
  network) に翻訳。session メソッドは docker attach/logs(stream)/wait/kill にマップ。
- backend 選択は config (`sandbox.backend: userns|container`、default `userns`)。`wire.go` で注入。
- **代替案 (不採用)**: `JobRuntime` をそのまま継ぎ目にする案。container ケースに「ホスト shell
  command 文字列 + `bash -lc`」の中間形が構造的に不要で leaky。かつ WS attach 面が `JobRuntime` の
  外 (Subscribe/WriteInput) に出ているため、`JobRuntime` だけ差し替えても attach が routing できない。

### 2. イメージ = 薄い OS base + boid バイナリ bind (version skew は大幅緩和だが「構造的解消」ではない)

- **イメージが持つのは OS レベルの土台のみ**。$HOME 配下のツールチェーン (go/volta/claude/codex/
  opencode) は workspace volume 側 (親ドキュメント「ツールはイメージ」の但し書き)。
- **runner entrypoint の boid バイナリはイメージに焼かず bind mount する** (現行 userns も
  `sandbox_builder.go:471` で bind)。これで daemon 更新のたびの再ビルドを避ける。
  **ただし「構造的に skew が消える」は言い過ぎ** (初版の誇張): daemon は起動時の executable inode を
  実行し続ける一方、`os.Executable()` の path 内容が atomic upgrade で新バイナリに差し替わると、
  後発 container は新バイナリを mount して「旧 daemon + 新 runner」の skew が再現しうる
  (`wire.go:450-456`)。**緩和策**: 起動中 daemon の実体を runtime 下に content-addressed copy として
  pin し、container はそれを mount する (or 互換 handshake を持つ)。この緩和を実装に含める。
- **container entrypoint は clone/pivot_root を行わない**: コンテナが namespace 隔離を提供するので、
  userns 段を skip し「mount/file/symlink 適用 + `runAgent` + `postJobDone` + signal forwarding」だけ
  実行する新 entrypoint (`boid sandbox-entry` 等) を切る。共有ロジックは runner_linux.go から抽出 (PR2)。
- **image 選択の入口**: workspace DB の `ContainerImage` (現状 spec に未接続) を JobSpec →
  `sandbox.Spec` → containerBackend に流す。default image + workspace override + digest/pull policy を
  決める (PR2/PR4 依存)。
- **shim symlink 集合**: `hostCommandSymlinks` (`sandbox_builder.go:1078`) は job ごとの resolved
  command map から生成されるため、「共通 base に焼く」なら **global superset を焼く** か **起動時に
  volume/tmpfs へ生成** のどちらかに決める必要がある (job 別集合と焼き込みが一致しない問題)。
  Phase 6 は起動時生成 (現行 5a と同型) を既定とし、焼き込み最適化は後続。

### 3. Signal の意味論を保つ (agent-stop を container kill にしない)

- container entrypoint に signal forwarding 経路を設け、`Signal(SIGUSR1)` を PID 1 → adapter
  (process group) に届ける。runner 相当の中間段は SIG_IGN を維持し、adapter が受けて Claude を
  graceful に落として `job_done` する現行意味論を pin する。
- 単純な docker kill (SIGKILL/SIGTERM to PID 1) にすると agent-stop が「強制終了」に化けて成果が失われる。
  container backend の Signal 実装は「forwarding + adapter graceful」を e2e で検証する。

### 4. 診断成果物は「runner upload (正常系) + daemon 側 fallback capture (異常系)」の二段

- 正常系: runner が job 終了時に runner-state.json / transcript を broker RPC でアップロード。
- **異常系が本命** (初版の抜け): OOM / SIGKILL / entrypoint・setup failure / daemon loss では
  runner の終了 RPC は走らない。**container remove の前に daemon/backend が `docker logs` / `inspect` /
  runner-state を回収する fallback を必須**にする。runner upload は正常系の補助に留める。
- 既存の `boid job log` が読む永続場所 (現行は runtime dir) との対応を設計に含める
  ($HOME volume 置きは並行 job で混ざるため不可)。

### 5. container からの trusted endpoint 到達を PR4 前に決める (loopback bind 問題の解消)

- git gateway (`server.go:295`) と egress ProxyManager (`proxy.go:58`) は loopback bind + pasta の
  10.0.2.2 投影に依存しており、**docker container からは到達不能**。したがって「PR4 で hook job が
  一通り通る」は現状のままでは成立しない (gateway clone が egress 以前に失敗する)。
- **決定**: これらの TCP listener を **bridge から到達可能なアドレスに bind**し (docker bridge の
  host-gateway IP 等)、**アクセス元を job network に制限**し、**backend 別に sandbox 向け URL を
  生成**する (userns = 10.0.2.2 投影、container = bridge アドレス)。broker / dockerproxy は UNIX
  socket bind なので container に bind mount するだけでよく、問題は TCP の 2 listener に限る。
- ProxyManager 自体は Phase 6 ではホスト側に留める (sidecar 化 = 「broker 側再配置」は Phase 7)。
  ポリシー・live-swap 機構は不変、bind アドレスと到達経路だけ backend 差分にする。
- egress の L3 強制 (job network を internal にし外部到達を proxy のみにする、直 IP 拒否) は PR5。

### 6. 再起動 recovery は「現行踏襲 (kill+fail+auto-reopen) + 孤児 reap」に割り切る

- **live 再吸着はしない**。現行 startup は backend を見る前に全 running job を failed
  (`store.MarkStaleJobsFailed`)、executing/awaiting task を aborted にし
  (`MarkStale*TasksAborted`)、その後 auto-reopen (`FindDaemonShutdownAbortedTasks`) が再起動する。
  broker/gateway token も job context もメモリ上だけなので、走行中 container を拾い直しても旧 token
  では RPC/clone/job_done が成立しない。再吸着は Phase 7 (別ホスト recovery とセット)。
- Phase 6 の `SandboxBackend.ReapOrphans` は「label (`boid.job_id` 等) の付いた実行中コンテナ・
  volume・network を列挙して**破棄**する」reap 専用にする (`Adopt` は WS attach の runtimeID 解決用で、
  再起動 recovery には使わない)。これで孤児リソースが残らず、意味論は現行と一致する。
- **reap は startup の auto-reopen より前に完了させ、reap 失敗時は該当 task を reopen しない**。
  現行 startup は `MarkStale*` (`wire.go:424-441`) の後段で `FindDaemonShutdownAbortedTasks` →
  `reopen` (`wire.go:527-533`) を走らせる。docker container は boid daemon 再起動では終了しないため、
  reap しないまま auto-reopen すると**旧 agent (走行中 container) + 新 agent が共有 $HOME / workspace /
  task RPC に同時作用する二重実行**になる。この二重実行防止のため、container backend では
  `MarkStale*` と auto-reopen の間に `ReapOrphans` を挟み、reap が失敗した task は reopen をスキップする
  (現行 `cleanOrphanRuntimes` が userns の runtime dir に対してやっている「reopen 前に孤児を掃除」の
  container 版)。この順序契約は config 公開 (PR5) の前提であって PR6 の後回しにはできない。
- 現行 `runtimes/<id>/` dir GC (CLAUDE.md「自動 GC」) の container 版として周期 reap も足す。

### 7. `Wait` は単一所有者 + cleanup 順序を契約化する

- 現行は `watchRuntime` と `cleanupSandboxAfterWait` が同じ runtime を並行 `Wait` する
  (`runner.go:928-994`)。LocalRuntime は結果をキャッシュするので動くが、container session が
  診断回収と remove まで所有すると二重 wait/cleanup が競合する。
- **backend 内で一度だけ wait して exit future を fan-out**し、「診断回収 → job fallback 処理 →
  resource remove」の順序を契約化する。remove が診断回収より先に走ると決定 4 の fallback が空を掴む。

### 8. file fallback + `~/.boid` tmpfs overlay を退役 (Phase 5 宿題 2)。writer 移行が前段

Phase 5b decision 7 再改訂で「file fallback (`~/.boid/output/payload_patch.json`) と tmpfs overlay の
完全撤去は Phase 6 前後」とされた。退役の機序に注意:

- **退役は「コンテナ隔離」では達成できない**。container backend でも **$HOME は workspace 単位の
  共有 volume** なので、`$HOME/.boid/output/payload_patch.json` は依然として同 workspace の並行 job 間で
  共有される (コンテナ境界は $HOME を隔離しない — Phase 4 の設計どおり共有が意図)。退役は
  **file 経路を完全に消して RPC を唯一経路にする**ことで達成する。
- **reader だけ消すのは不十分** (初版の抜け): claude adapter は起動**前**に session ID を
  `writePayloadPatch` で `~/.boid/output/payload_patch.json` に書いており (`claude/run.go:317-348,398-419`、
  OOM でも jsonl 記録を残すため)、この file の**能動的 writer** でもある。`runAgent` は
  `Result.PayloadPatch` を捨て、最終的に `resolveJobOutput` の file read だけが broker に渡している
  (`runner_linux.go:497-538`)。したがって退役は **(a) adapter/shell hook/正規 doc の writer を RPC
  または明示的な runner→broker patch 送信に移行 → (b) reader/writer/placeholder/overlay を一括撤去**
  の順で行う。writer を残したまま reader を消すと session artifact が失われる。
- 移行完了後は PR7 自体は backend 非依存 (両 backend で同じ負債) なので PR1–6 と独立に先行 land 可能。

---

## 目標状態

- `SandboxBackend`/`SandboxSession` interface が導入され、`launchSandbox` と WS attach 面が
  backend 選択の 1 抽象に集約。userns backend は実装の 1 つ (振る舞い不変)。
- config `sandbox.backend: container` で job がホスト上の docker コンテナとして起動。イメージは
  薄い OS base + bind mount した (content-addressed pin 済みの) boid バイナリ + 起動時生成 shim。
  ハーネス/スキルの書き換えはゼロ。
- WS attach (Subscribe/WriteInput/CloseInput/Resize) と agent-stop (SIGUSR1 forwarding) が
  container backend でも動く。web terminal vt エミュレータ (サーバ側) の恩恵も維持。
- git gateway / egress proxy に container から到達でき (bridge bind + 制限)、egress は internal
  network + proxy で L3 強制。broker RPC・$HOME volume・`/workspace` clone・`/run/boid/bin` は現行契約のまま成立。
- 診断は正常系 runner upload + 異常系 daemon fallback capture の二段で、container 削除で消えない。
- 再起動は現行踏襲 (kill+fail+auto-reopen)、孤児コンテナ/volume/network は reap。
- file fallback + tmpfs overlay 退役 (writer は RPC 移行済み)。
- container backend 用 e2e が CI で回り、既存 blackbox-e2e が container でも green。
- **default は依然 userns** (opt-in で container を dogfood)。default 切替は dogfood 安定後の別判断。

---

## PR 分割案

strangler 順。前半 (PR1–3) は inert で個別 land 可。**PR4 は単体では insecure なので、config での
backend 選択公開は PR5 (egress) 完了まで行わない** (or PR4+PR5 を同一 cutover)。

### PR1: `SandboxBackend`/`SandboxSession` interface 導入 + userns backend 抽出 (inert・振る舞い不変)

- interface を **attach ストリーム面 (Subscribe/WriteInput/CloseInput/Resize) + Adopt + ReapOrphans +
  Signal 込み**で定義。現行 `launchSandbox` と `runtime_subscriber_export.go` の委譲を `usernsBackend` に集約。
- **resize の 3 経路すべてを session 経由にする**: WS 内 resize (`ws_attach.go`) に加え、
  `internal/server/job_runtime_routes.go` の `POST /api/jobs/{id}/resize` (現状 `jobRuntime.Resize` 直呼び) も
  `SandboxSession.Resize` (Adopt 経由) に付け替える。この付け替えを PR1 スコープに含める (漏らすと container
  job の CLI resize が誤配送される)。
- `JobRuntime`/`LocalRuntime`/`SandboxPreparer` は内部 transport として温存。
- **振る舞い不変**が契約。既存の全 unit/e2e (WS attach・CLI resize・agent-stop 含む) が無改変で green。

### PR2: container イメージ定義 + container entrypoint + boid バイナリ pin (inert・ビルドのみ)

- 薄い OS base イメージ + 起動時 shim 生成の足場。boid バイナリは content-addressed pin して mount する機構。
- container entrypoint (clone/pivot_root を skip、mount/file/symlink 適用 + `runAgent` + `postJobDone` +
  signal forwarding)。共有ロジックを runner_linux.go から `runner/runner.go` に抽出。
- まだ dispatch から呼ばれない (inert)。イメージビルド CI ジョブを足す。

### PR3: `sandbox.Spec` → docker 実行仕様の realization 層 (inert・単体テスト)

- backend-neutral な可視性要求と userns concrete な mount plan を分離し、`[]Mount`/Env/WorkDir を
  docker volumes/env へ翻訳。**`/workspace/<name>` は container-local volume に着地させる** (host bind に
  しない)。`Guard`/`DetectType` は宣言時解決 or 無視の方針を固定。単体テストで pin。

### PR4: `containerBackend` 実装 + image 選択配線 (config 選択は非公開)

- PR3 翻訳 + docker API (create/start/attach/logs/wait/kill) で `SandboxBackend` を実装。
- workspace `ContainerImage` を JobSpec→Spec→backend に流す配線。default/override/pull policy。
- **決定 5 の trusted endpoint 到達 (bridge bind) を前提として先に入れる** — 入れないと gateway clone
  が通らず PR4 の手動確認が成立しない。
- **この時点では config で公開しない** (egress 前 + LLM credential 入り $HOME mount で insecure なため)。
  内部フラグ or テスト専用で hook job が通ることを確認。

### PR5: egress の L3 トポロジ + 起動時 reap + config 公開 (cutover)

- job コンテナを internal network に置き、ホスト ProxyManager (bridge bind) を唯一の egress 経路にする。
  直 IP 拒否を検証。`applyProxyEnv` の env は流用。dockerproxy socket を bind。
- **config 公開の前提として `ReapOrphans` (label スキャン) を startup 配線する**: `MarkStale*` と
  auto-reopen の間に reap を挟み (`wire.go:424-533`)、reap 失敗 task は reopen をスキップ (決定 6)。
  これを入れずに config 公開すると、再起動時に走行中 container を残したまま auto-reopen して二重実行になる。
- ここで `sandbox.backend: container` を config 公開 (egress + reap が揃い安全な状態になる)。
- e2e の allowed_domains 系 + 「daemon 再起動で孤児が残らず二重実行しない」を container で green に。

### PR6: 診断二段回収 + Wait 順序 + 周期 reap

- runner upload (正常系) + daemon 側 fallback capture (docker logs/inspect、異常系)。
- backend 内単一 wait + fan-out、「診断回収 → fallback 処理 → remove」順序契約。
- 起動時 reap (PR5 で配線済み) に加え、現行 runtime dir GC の container 版として**周期 reap** を足す。

### PR7: file fallback + `~/.boid` tmpfs overlay 退役 (backend 非依存・先行可)

- **前段**: claude adapter `writePayloadPatch` (と shell hook / 正規 doc) の writer を RPC / 明示的
  runner→broker patch 送信に移行。
- **本体**: `job_done` の file read (`resolveJobOutput`)、`writePayloadPatch` writer、placeholder、
  `~/.boid` tmpfs overlay (`sandbox_builder.go:823-833`) を一括撤去。両 backend で落とす。
- grep 静的テストに「file fallback path が消えた」pin を追加。writer 移行が済めば PR1–6 と独立に先行 land 可。

### PR8: container backend e2e + dogfood + doc

- container e2e を CI で回す配線 (docker socket 可用性、blackbox-e2e との関係、sibling container 疎通)。
- dispatch レイテンシ実測。ホスト側 dogfood (nose 判断)。plan/親 doc の完結記録更新。default 切替は別判断。

### 順序と依存

- PR1 独立・先行。PR2/PR3 は PR1 後に並行可 (inert)。PR4 は PR2+PR3+決定5 依存。PR5 は PR4 後 (cutover)。
  PR6 は PR4 後。**PR7 は writer 移行さえ済めば backend 非依存で先行 land 可** (Phase 5 宿題を早く畳める)。
  PR8 は全体後。

---

## 未解決論点

Phase 6 スコープ内で、決定まで要らず着手時に詰める論点。Phase 7 専用 (mirror ワーカー / RWX 並行 RW /
DB 移行 / subscription key と ToS / daemon コンテナ化 / broker 側再配置) はここに挙げない。

- **bind mount の UID/GID 契約**: `$HOME` は host owner の RW bind、broker/dockerproxy socket は
  owner-only。container を root で動かすと HOME に root-owned file が残り、host UID で動かすと image 内
  ユーザ・socket・rootless docker/userns-remap との整合が要る。numeric UID/GID、ownership 初期化、
  rootless/remap 対応を実装時に決める (Phase 6 のローカル成立条件)。
- **dockerproxy の sibling container 疎通**: proxy は docker API を検査転送するだけで、job container と
  TestContainers が作る sibling container のデータプレーン接続・path mapping はしない。bind mount は
  明示拒否されるため container-local `/workspace` を sibling に渡す用途は成立しない。internal network 下で
  公開 port へどう到達するか、許可 network への join、bind 非対応時の契約を **container e2e 要件**として決める。
- **trusted endpoint の bind アドレス具体** (決定 5 の実装レベル): docker bridge の host-gateway IP への
  bind + アクセス元 (job network) 制限 + 直 IP 拒否を両立する docker network 構成の詳細。
- **container 内 entrypoint の切り方** (決定 2/3): 既存 runner を `--mode container` で分岐か新 entrypoint か。
  signal forwarding の実装 (tini 相当を挟むか自前 PID1 か)。
- **shim 集合の焼き込み最適化** (決定 2): Phase 6 は起動時生成だが、global superset をイメージに焼く
  最適化の是非と更新 lifecycle。
- **ネットワーク分離の粒度**: internal network を workspace ごとに分けるか (別 workspace の job の L3 相互
  到達)。同一 workspace 内 job 間到達の要否。
- **リソース制限 (cgroup)**: cpu/メモリ/pids/ディスクは container で自然に得られる新能力。workspace/job 設定の
  語彙にどう出すか (Phase 6 では最小 or 見送り可)。
- **dispatch レイテンシ実測**: コンテナ create/start の実測。イメージ pull 済み前提の妥当性検証。
- **npm private registry 等の credential 線引き**: git 依存は insteadOf→gateway で解決済み。registry token を
  $HOME volume に置くと「sandbox 内 credential = ハーネス LLM 認証のみ」の例外が広がる。明示的決定が要る。
- **`boid workspace peers` (Phase 5b 宿題 3)**: peer advertise の CLI 化 (git-gateway-peer-fetch の skip 解除)。
  container モデルの peer 動的 clone 契約の前提だが backend swap 自体とは独立。取り込むか別建てかは着手時判断。

---

## 関連ドキュメント

- 親: [container-based-boid.md](container-based-boid.md) — 移行戦略ステップ 6 / 目標アーキテクチャ / 未解決論点。
- 直前段: [phase5-shim-and-task-context.md](phase5-shim-and-task-context.md) — decision 7 (file fallback +
  tmpfs overlay の退役条件、本 phase で成立)。
- [git-gateway-cutover.md](git-gateway-cutover.md) — `/workspace/<name>` clone 契約 / clone --reference /
  gateway (loopback bind の現状)。
- [home-workspace-volume.md](home-workspace-volume.md) — $HOME volume レイアウト
  (`~/.local/share/boid/homes/<slug>/`)、container で bind mount 再利用。
- [docker-native-proxy.md](docker-native-proxy.md) — dockerproxy (Phase 1 landed)、Phase 7 で broker 側再配置。
- [workspace-db-consolidation.md](workspace-db-consolidation.md) — workspace の環境非依存設定
  (`ContainerImage` フィールドが本 phase の image 選択の入口)。

---

## 変更履歴

- 2026-07-22: 初版 draft 作成 (移行戦略ステップ 6)。
- 2026-07-22: codex 設計レビュー反映で改訂。主な変更:
  - **Blocker 反映**: (1) interface に WS attach ストリーム面 (Subscribe/WriteInput/CloseInput/Resize) と
    Adopt/ReapOrphans を追加 (`JobRuntime.Attach` は attach の一面でしかないと判明、`ws_attach.go` /
    `runtime_subscriber_export.go` 実コードで確認)。(2) Signal を container kill にしない agent-stop 意味論保存
    (決定 3)。(3) container からの trusted endpoint 到達を PR4 前の決定に格上げ (gateway/proxy が loopback bind +
    10.0.2.2 投影、`server.go:295` で確認、決定 5)。(4) PR7 に claude adapter writer の RPC 移行を前段追加
    (`writePayloadPatch` が能動 writer と確認、決定 8)。(5) Reconcile を「現行踏襲 + 孤児 reap」に割り切り
    (`MarkStaleJobsFailed` 等で現行契約確認、決定 6)。
  - **Major 反映**: `sandbox.Spec` は backend 非依存でない旨のトーンダウン + realization 層 (PR3)、version skew
    「構造的解消」のトーンダウン + content-addressed pin 緩和 (決定 2)、PR4 の insecure backend を config 非公開に
    (PR4/PR5)、診断の daemon 側 fallback capture 二段化 (決定 4)、image 選択 (`ContainerImage`→spec) + shim 集合を
    PR に明記 (決定 2)、Wait 単一所有者 + cleanup 順序 (決定 7)、UID/GID と dockerproxy sibling 疎通を未解決論点に追加。
  - **Minor 反映**: `PrepareSandbox` は marshal だけでない (RootDir/secret/cleanup 所有)、portable helper は
    未抽出で PR2 の作業、`sandbox.Sandbox` は production dead だが test mock ありと正確化。
- 2026-07-22: codex 2 巡目 (改訂版の再レビュー) 反映。B2/B3/B4 は実装契約として解消確認、残 2 Blocker を修正:
  - **resize routing**: attach は 3 経路 (WS ストリーム / WS 内 resize / `POST /api/jobs/{id}/resize` の
    `jobRuntime.Resize` 直呼び) に散っており、CLI の resize は最後の HTTP route を通ると判明
    (`cmd/attach.go` + `job_runtime_routes.go:54`、実コード確認)。この route の session 経由化を PR1 スコープに追加。
  - **reap-before-reopen 順序**: docker container は daemon 再起動で終了しないため、`ReapOrphans` を PR6 に
    置くと PR5 の config 公開時に二重実行が起きる (`wire.go:424-533` の MarkStale→auto-reopen 間に reap 未挿入)。
    起動時 reap を PR5 (config 公開の前提) に前倒しし、reap 失敗 task は reopen スキップの順序契約を決定 6 に明記。
  - codex 総評「独立した新規 Major はなし、設計は収束」。
