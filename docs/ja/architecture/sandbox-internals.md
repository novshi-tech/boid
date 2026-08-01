# サンドボックス内部実装

`boid` のサンドボックスがどう組まれているか、 hook を 1 つ動かすときに何が起きているかを記したページです。 [アーキテクチャ概要](overview.md) の sandbox 節を、コンテナ起動パラメータとファイルレイアウトの粒度で掘り下げています。

主な読者は `internal/sandbox/` に手を入れる contributor、サンドボックス絡みの不具合を追っている人、あるいは「なぜ**ホスト側**のホームディレクトリが見えないのか」 を最後まで知りたい人です (サンドボックス自身の `$HOME` は別物で workspace スコープです — 後述の「sandbox 内のプロセスからは」を参照)。

## ねらい

サンドボックスは 4 つの境界をまとめて作ります。

1. **ファイルシステム** — 書き込み可能な領域を sandbox 内 clone (project が可視でないジョブではプロジェクトルート) に絞る
2. **ネットワーク** — 組み込みリストと `config.yaml` の `sandbox.allowed_domains` に含まれるドメインしか出ていけない
3. **ユーザ ID** — ホストの root には触れない (rootless)
4. **コマンド** — host で動かすコマンドは kit の `host_commands` で宣言された分だけ通る

これら全てを、boid daemon が Docker/Podman コンテナランタイムに委譲して実現します (PR-4 = volume-only cutover 以降、コンテナ backend が唯一の sandbox backend — `sandbox.backend` config は撤去済み)。 mount/network/user namespace の分離やルートファイルシステムの切り替えはコンテナランタイム自身が担い、boid 側で namespace 系 syscall を直接発行するコードはもうありません。

## 起動の全体像

`boid` daemon が hook を 1 つ起動するとき、`internal/dispatcher` の `containerBackend.Launch`（[`internal/dispatcher/container_backend.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/container_backend.go)）が `sandbox.Spec` を Docker Engine API の `container create` 呼び出しに翻訳し、host 側の docker/podman daemon に対して boid daemon 自身の兄弟コンテナ (sibling container、いわゆる docker-out-of-docker) を 1 つ起動します。

```
+-------------------------------------------------------------+
| boid daemon (bare-metal、または compose の daemon service)  |
|   containerBackend.Launch が sandbox.Spec を                |
|   docker `container create` + `start` に翻訳                |
+----------------------------------|---------------------------+
                                    v  Docker Engine API
+-------------------------------------------------------------+
| job container (image: build/container/Dockerfile がビルド   |
| する boid-runner image、`HostConfig.Init: true` で           |
| PID 1 = docker-init/tini)                                    |
|   ENTRYPOINT = `/usr/local/bin/boid runner-container`        |
|     --spec /run/boid/spec.json --state /run/boid/state.json |
|   spec.Files を書き込み → spec.Symlinks (host command shim) |
|   を再構成 → sandbox 内 clone (該当時) → adapter.Run() で   |
|   エージェントを exec                                        |
+-------------------------------------------------------------+
```

旧 userns backend にあった `runner-outer → pasta → runner-inner → runner-inner-child` という 5 段プロセスチェーン (`cmd/runner.go` の `runner-outer`/`runner-inner` サブコマンド、`internal/sandbox/runner/runner_linux.go` の `clone(CLONE_NEWUSER|CLONE_NEWNS)` + `pivot_root` パス) は PR-4 (`docs/plans/volume-only-daemon.md`、2026-07 cutover) で完全に撤去されました。 実装は host 側の [`internal/dispatcher/container_backend.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/container_backend.go)（launch / attach / resize / signal / reap を `backend.SandboxBackend` interface 経由で提供）と、コンテナ内で動く [`internal/sandbox/runner/runner_container_linux.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/runner/runner_container_linux.go) の `RunContainer`（`boid runner-container` の entry point）に分かれています。

### コンテナ起動パラメータ (`containerBackend.Launch`)

- **イメージ** — `build/container/Dockerfile` がビルドする boid-runner image。 `boid` バイナリは `/usr/local/bin/boid` にビルド時点で焼き込まれ (`COPY --from=builder`)、`ENTRYPOINT` は固定で `["/usr/local/bin/boid", "runner-container"]`。`Cmd` には `--spec`/`--state` の 2 引数だけを渡す
- **spec / state ファイル** — dispatcher が host 側に書き出した `runner-spec.json` (read-only) / `runner-state.json` (read-write) だけを、それぞれ `/run/boid/spec.json` / `/run/boid/state.json` として bind mount する。 job container から見える host filesystem はこの 2 ファイルのみで、project checkout やホームディレクトリの bind mount は無い (volume-only pivot の帰結。 `docs/plans/volume-only-daemon.md`)
- **volume** — [`internal/sandbox/realization/`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/realization/) が解決した volume / tmpfs を Docker の named volume / mount に変換する (`containerMounts`)。 workspace スコープの `$HOME` も named volume (後述)
- **ネットワーク** — workspace ごとに daemon が使い捨てで作る `internal: true` の docker network に接続する (後述「ネットワーク制御」)
- **PID 1** — `HostConfig.Init: true` (docker-init/tini) を指定。 SIGUSR1 の中継やゾンビ回収は tini が担い、boid 側で signal-forwarding loop を自前実装する必要はない
- **uid/gid** — image ビルド時に作成した非 root の固定 uid/gid (`boid` ユーザ) で起動する

### `boid runner-container` entrypoint (`RunContainer`)

job container の root filesystem は起動直後から既に image 自身のもの (sandbox root そのもの) なので pivot_root に相当する処理は無い。`RunContainer` が行う手順:

1. `spec.Files` を絶対パスへ書き込む
2. `spec.Symlinks` (project ごとに許可された host command の shim、`/run/boid/bin/<name> -> boid`) を再構成する — image には `boid` 本体の symlink しか焼き込まれていない (許可コマンドの集合は image ビルド時点では未知) ため、コンテナ起動の度に spec から作り直す
3. `spec.Clone.Enabled` なら sandbox 内 clone を実行する (git gateway 経由、host への broker dispatch は無し)
4. `spec.Env["PATH"]` を反映する
5. `adapter.Run()` (HarnessAdapter 経由) でエージェント (claude / codex / opencode) または shell hook を exec し、停止シグナル (SIGUSR1 → 子に SIGTERM) の中継・終了コード正規化を行う
6. 終了後、broker へ `boid job done` を送信する (後述「host commands と broker」)

sandbox 内のプロセスからは:

- **host 側**のホームディレクトリ・SSH 鍵・他プロジェクトは存在自体が見えない (spec/state の 2 ファイル以外、host filesystem への bind mount が無い)。 サンドボックス自身の `$HOME` は別物 — 下記参照
- コンテナの外 (host や他 job のコンテナ) へは、コンテナランタイム自身の namespace 分離により到達できない

サンドボックス内の `$HOME` は host 共有でも毎回まっさらな tmpfs でもなく、 **同一 workspace に dispatch される job 間で永続する、 read-write マウントされた workspace スコープの named volume** です (docs/plans/home-workspace-volume.md Phase 4)。 hook が `$HOME` 配下に書いたファイルは、 同じ workspace の後続の別 job からも見えます。 `$HOME/.boid` も同様に永続します — Phase 6 PR8 以前は dispatch 毎に job-scoped tmpfs を重ねて `$HOME/.boid/output/payload_patch.json` を job 間で隔離していましたが、 payload patch の唯一経路が broker RPC (`boid task update --payload-patch`) になったことでこのファイル経由の出力自体が撤廃され、 隔離用の tmpfs も不要になりました (詳細は [Hook スクリプトプロトコル / 出力](../reference/hook-contract.md#出力))。

タスクコンテキストは `boid task current` / `instructions` / `env` / `payload` — shim 経由で呼べる broker RPC — で取得します。 dispatch 時に一括生成する方式ではなく、必要になった時点で pull します。 hook のプロトコル詳細は [Hook スクリプトプロトコル](../reference/hook-contract.md)。

## ネットワーク制御

ネットワーク境界は 2 段構えです。

### ① workspace ごとの docker internal network

workspace ごとに daemon が使い捨てで `docker network create --internal --label boid.workspace=<slug>` するネットワークに job container を接続します (`ensureWorkspaceNetwork`、[`internal/dispatcher/container_backend.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/container_backend.go))。 `internal: true` のため、この network 上のコンテナには外部への default route がありません — job container が直接到達できるのは同じ network 上の boid daemon (自分自身) だけです。

### ② daemon 内蔵 egress proxy

daemon container 自身が [`internal/sandbox/proxy_manager.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/proxy_manager.go) の ProxyManager を in-process で動かし、compose network 上に `boid-egress` という DNS alias で待ち受けます (`build/container/compose.yml`)。 job container には環境変数 `http_proxy` / `https_proxy` / `HTTP_PROXY` / `HTTPS_PROXY` に daemon 側アドレスが渡され (`applyProxyEnv`、[`internal/dispatcher/sandbox_builder.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/sandbox_builder.go))、許可リストに無いドメインへのリクエストは proxy が拒否します。 直接の TCP/UDP は internal network に default route が無いことで既に遮断されています。 daemon container 自身は別途 default bridge network にも所属しており、そちらで proxy 自身の上流フェッチや `docker pull` 用の外部インターネット接続を行います (`build/container/compose.yml` のトポロジ節を参照)。

proxy の実装は [`internal/sandbox/proxy.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/proxy.go) で、 daemon の goroutine として動きます。

#### proxy 許可リスト

許可ドメインは2層で構成されます。

1. **デフォルトリスト** — Anthropic/OpenAI API・各言語パッケージレジストリ・Docker Hub など、 `cmd/start.go` の `defaultAllowedDomains()` にハードコードされたエントリ
2. **ユーザ追加リスト** — `~/.config/boid/config.yaml` の `sandbox.allowed_domains` に列挙したエントリ。起動時にデフォルトリストへ追記される

```yaml
# ~/.config/boid/config.yaml
sandbox:
  allowed_domains:
    - ".github.com"       # ドット始まりはサフィックスマッチ
    - "api.example.com"   # ドットなしは完全一致
```

変更は `boid stop && boid start` で反映されます。

## Docker プロキシ (`capabilities.docker`)

`project.yaml` で `capabilities.docker: {}` を宣言すると、boid daemon がサンドボックスごとに **Docker プロキシ** を起動し、sandbox 内プロセスの Docker API アクセスを仲介します。実装は [`internal/sandbox/dockerproxy/`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/dockerproxy/) にあります。

```
サンドボックス内プロセス (docker CLI / SDK / TestContainers)
        |
        | DOCKER_HOST=unix:///run/boid/docker-proxy.sock
        v
[Docker Native Proxy] (内部 Unix socket)
        |
        | ポリシー評価 (policy.go)
        v
上流 Docker daemon (/run/user/<uid>/docker.sock 等)
```

### ルーティング: fail-closed 方式

リクエストの通過ルールは **fail-closed** です ([`server.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/dockerproxy/server.go)):

| リクエスト | 動作 |
|---|---|
| `GET` / `HEAD` (全エンドポイント) | 透過転送 (読み取り専用) |
| 明示許可リストに載っている mutating エンドポイント | 透過転送 |
| ボディ検査が必要な mutating エンドポイント | 検査後 ALLOW / DENY |
| `POST /build`, `POST /session` (image build) | 固定 deny |
| それ以外の未知 mutating エンドポイント | 既定 deny (fail-closed) |

image build を deny する理由: BuildKit は `/session` エンドポイントで HTTP をハイジャックし gRPC を流すため、ボディ検査が不可能です。

### ボディ検査: 拒否される HostConfig 設定

`POST /containers/create` のボディは詳細に検査されます ([`policy.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/dockerproxy/policy.go))。以下の設定が含まれていると `403 Forbidden` が返されます:

| フィールド | 拒否条件 | エラーメッセージ |
|---|---|---|
| `HostConfig.Binds` | 要素が 1 つ以上 | `HostConfig.Binds: bind mounts are not permitted` |
| `HostConfig.Mounts` | `Type=bind` の要素が存在 | `HostConfig.Mounts: type=bind mount is not permitted` |
| `HostConfig.Mounts` | `Type=volume` + `VolumeOptions.DriverConfig.Options.device` | `HostConfig.Mounts: volume with device option (system 3 bind) is not permitted` |
| `HostConfig.Mounts` | `Type=volume` + `Options.o` に `bind` を含む | `HostConfig.Mounts: volume with o=bind option (system 3 bind) is not permitted` |
| `HostConfig.Privileged` | `true` | `HostConfig.Privileged: privileged containers are not permitted` |
| `HostConfig.NetworkMode` | `host` / `container:<id>` / `ns:<path>` | `HostConfig.NetworkMode: <値> is not permitted` |
| `HostConfig.PidMode` | `host` / `container:<id>` / `ns:<path>` | `HostConfig.PidMode: <値> is not permitted` |
| `HostConfig.IpcMode` | `host` / `container:<id>` / `ns:<path>` | `HostConfig.IpcMode: <値> is not permitted` |
| `HostConfig.UsernsMode` | `host` | `HostConfig.UsernsMode: host is not permitted` |
| `HostConfig.CgroupnsMode` | `host` | `HostConfig.CgroupnsMode: host is not permitted` |
| `HostConfig.SecurityOpt` | 要素が 1 つ以上（値を問わず） | `HostConfig.SecurityOpt: security options are not permitted` |
| `HostConfig.CapAdd` | 要素が 1 つ以上（capability 名を問わず） | `HostConfig.CapAdd: adding capabilities is not permitted` |
| `HostConfig.Devices` | 要素が 1 つ以上 | `HostConfig.Devices: device access is not permitted` |
| `HostConfig.DeviceCgroupRules` | 要素が 1 つ以上 | `HostConfig.DeviceCgroupRules: device cgroup rules are not permitted` |
| `HostConfig.Runtime` | `runc` 以外 | `HostConfig.Runtime: only runc runtime is permitted, got <値>` |
| `HostConfig.Sysctls` | 要素が 1 つ以上 | `HostConfig.Sysctls: sysctl settings are not permitted` |
| `HostConfig.CgroupParent` | 空文字列以外 | `HostConfig.CgroupParent: custom cgroup parent is not permitted` |

`POST /containers/{id}/exec` では `Privileged=true` を拒否します。
`POST /containers/{id}/start` ではボディに HostConfig が存在する場合を拒否します（旧 API の legacy 形式対策）。
`POST /networks/create` では `Driver=host` を拒否します。
`POST /volumes/create` では `DriverOpts.device` および `DriverOpts.o` に `bind` を含む場合を拒否します。

proxy はボディを **decode → re-encode せず、受信した生バイトをそのまま上流へ転送** します（parser differential 攻撃の回避）。

### コンテナ GC (Ryuk の内製化)

TestContainers の Ryuk reaper は docker.sock への bind-mount を要求しますが、本 proxy は bind を禁止しています。そのため `TESTCONTAINERS_RYUK_DISABLED=true` が自動設定され、Ryuk は無効化されます。その代わり boid が掃除役を担います。

- **ID 記録**: 作成系エンドポイント (`POST /containers/create`・`/networks/create`・`/volumes/create`) のレスポンスから ID を拾い、**クライアントへ返す前に** `<runtimes-dir>/<runtime_id>/docker-resources.jsonl` に fsync 付きで追記します ([`ledger.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/dockerproxy/ledger.go))。
- **同期掃除**: ジョブ完了時（成功・失敗とも）に `Reap()` が台帳を読み取り、コンテナ → ネットワーク → ボリュームの順で `stop` + `rm` を発行します ([`reap.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/dockerproxy/reap.go))。
- **GC による安全網**: daemon の 24 時間 GC loop が runtime ディレクトリを削除する前に台帳のリソースを掃除し、クラッシュ等で取りこぼした孤児リソースを回収します。

### job 間分離 (id スコープ検査)

rootless Docker の upstream daemon は同一ユーザの全 job で共有されます。proxy は台帳を使って **自分の job が作成したリソース ID だけにアクセスを制限** します:

- `/containers/{id}/` 系・`/networks/{id}/` 系・`/volumes/{name}/` 系・`/exec/{id}/` 系のエンドポイントは、id が自 job の台帳に存在する場合のみ透過します。
- 台帳にない id への操作は **404 で拒否**し、他 job のリソースの存在を漏らしません ([`server.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/dockerproxy/server.go))。

### 環境変数の自動設定

`capabilities.docker` 有効時、以下の環境変数がサンドボックスに自動設定されます ([`sandbox_builder.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/sandbox_builder.go)):

| 環境変数 | 値 |
|---|---|
| `DOCKER_HOST` | `unix:///run/boid/docker-proxy.sock` |
| `CONTAINER_HOST` | `unix:///run/boid/docker-proxy.sock` |
| `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE` | `/run/boid/docker-proxy.sock` |
| `TESTCONTAINERS_RYUK_DISABLED` | `true` |

### host_commands への docker 登録禁止

`capabilities.docker` が有効なプロジェクトで `host_commands` に `docker` をサブコマンド制限なしで登録しようとすると、ジョブ起動時にエラーになります ([`runner.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/runner.go) `validateDockerHostCommands`):

```
host_commands.docker: unrestricted docker access bypasses the docker proxy
(capabilities.docker is enabled); remove docker from host_commands or restrict
to specific subcommands (e.g. allow: [build])
```

`host_commands` への `docker` 登録はホスト直実行（proxy バイパス）になるためです。`AllowedSubcommands` または `AllowedPatterns` を指定すれば許可されます（例: `allow: [build]`）。

## host commands と broker

サンドボックスから host のコマンドを呼ぶには、 `boid` shim と broker のペアが要ります。

```
job container 内: boid <subcommand>   (image に焼き込まれた boid バイナリ自身)
                    |
                    | TCP + mTLS (workspace の docker network 経由)
                    v
daemon container: boid daemon の broker (internal/sandbox.Broker)
                    |
                    | コマンドポリシーを評価
                    v
daemon container: 許可されたコマンドを実際に exec
```

shim の実体は `boid` バイナリ自身です (サブコマンドとして shim の動作に切り替わる multi-call binary、`internal/sandbox/boid_shim.go`)。 image ビルド時に `/usr/local/bin/boid` として既に焼き込まれており、project ごとに許可された host command 名の symlink (`/run/boid/bin/<name> -> boid`) はコンテナ起動の度に `spec.Symlinks` から再構成されます (前述「`boid runner-container` entrypoint」)。 job container と daemon container は別プロセス (別コンテナ) のため、broker との通信は UNIX socket ではなく TCP + mTLS で行われます — daemon が job ごとに短命の client 証明書を発行し (`BOID_BROKER_TLS_*` env 経由)、`internal/sandbox/brokerclient` がそれを使って接続します (`docs/plans/phase6-cutover-followups.md` §⓪ broker TCP wire)。 `boid task update` や `boid job done`、 kit が `host_commands` 宣言した `gh` / `git push` 等は、すべてこの経路で daemon へ流れます。

broker は `internal/sandbox/broker.go` にあり、次の責務を持ちます:

- shim からの要求を受ける (job container から daemon container への TCP + mTLS 接続、または bare-metal 経路の UNIX socket)
- リクエストにくっついている **トークン** を見て、どの job が呼んでいるかを特定する
- そのジョブが許可されたコマンド・サブコマンド・引数パターンに合致するかを `policy.go` の `CheckPolicy` で判定する
- 許可されていれば daemon 側で実際に exec し、 stdout / stderr / 終了コードを shim に返す

トークンは sandbox 起動時に発行され、 sandbox 内の環境変数 `BOID_BROKER_TOKEN` 等で受け渡されます。 sandbox の外からはトークンを知ることができないため、たとえ broker のアドレスや mTLS 証明書のディレクトリパスが漏れても、別 job のコマンドを許可させることはできません。

host command は daemon 側で project の checkout ディレクトリではなく中立ディレクトリ (`os.TempDir()`) で実行されます。 stdin も渡りません。 repo 文脈が必要なコマンド (`gh` 等) は kit の `env:` に `${boid:repo_slug}` を書いて渡します (詳細は [`project.yaml` リファレンス](../reference/project-yaml.md) の「host command の実行契約」)。

## 後片付け

後片付けは job container の **停止・削除** に集約されます。 mount/network/user namespace の生成・破棄をコンテナランタイム自身が担うため、旧 userns backend にあった「mount namespace の破棄をカーネルに任せる」「`$ROOT` を own ns / cross ns のどちらから消すか」といった作り込みは不要になりました。

job container が終了すると `containerBackend` ([`internal/dispatcher/container_backend.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/container_backend.go)) が:

1. `ContainerRemove` (`RemoveVolumes: true`) でコンテナ本体と、そのコンテナ専用の匿名 volume を削除する (失敗時は `Force: true` で再試行する)
2. host 側に書き出した `spec.json` / `state.json` (および per-job TLS cert 用の一時ディレクトリ) を削除する — `spec.json` は broker token 等の secrets を含むため終了コードに関わらず必ず削除する
3. workspace ごとの docker network・named volume はここでは削除しない (同一 workspace の後続 job が再利用するため)

daemon 起動時には `containerBackend.ReapOrphans` が、daemon 再起動やクラッシュを跨いで残った孤児コンテナ・network・volume を `boid.*` label ベースで掃除します (`internal/server/wire.go` の `reapOrphansBeforeReopen` から呼ばれる)。

失敗時 (`exitCode != 0`) は `state.json` (`runner-state.json`) を削除せず保全し、事後解析に使えるようにします。 この JSON には起動フェーズの進行記録・spec (secrets は redact 済)・終了コードが含まれます。

## サンドボックス内から呼べる boid builtin 一覧

サンドボックス内のハンドラ (hook / exec) は `boid`、`fetch` の 2 つの builtin を呼ぶことができます。
いずれも自動的に注入されるため、 `project.yaml` / `kit.yaml` での宣言は不要です。

`git` は broker builtin ではありません。 job container の image に apt でインストールされた実バイナリとして動作し、
project の clone・fetch・push はすべて sandbox 内の git が git gateway (認証注入リバースプロキシ) 経由で行います
(daemon への broker dispatch は無し)。 詳細は [`project.yaml` リファレンス](../reference/project-yaml.md#git-gateway--sandbox-内-clone) を参照してください。

### boid builtin

role 分岐はなく、全 role で同じ op セットが許可されます。

| Op (sandbox protocol) | 対応 CLI | 用途 |
|---|---|---|
| `job_done` | `boid job done <id>` | 自 job の終了を daemon に通知する |
| `job_list` | `boid job list --task <id>` | task に紐づく job を列挙する |
| `job_show` | `boid job show <id>` | job の詳細を表示する |
| `job_log` | `boid job log <id>` | job 実行ログを取得する |
| `action_send` | `boid action send` | 手動アクションを発行する |
| `agent_stop` | `boid agent stop <job-id>` | 実行中のエージェント job に SIGUSR1 を送る |
| `task_create` | `boid task create` | サブ task を作成する |
| `task_get` | `boid task show <id> --field <path>` | task の 1 フィールドを dotted JSON path で取得する |
| `task_update` | `boid task update <id>` | task のフィールドを更新する |
| `task_import` | `boid task import` | task を一括 import する |
| `task.reopen` | `boid task reopen <id>` | done の task を executing に戻す |
| `task_list` | `boid task list` | workspace 内の task を列挙する |
| `task_notify` | `boid task notify <id>` | 通知または Q&A (`--ask`) を送信する |
| `task_answer` | `boid task answer` | awaiting → executing に遷移させる |
| `task_delete` | `boid task delete <id>` | task を削除する |
| `task_current` | `boid task current` | この task の id/title/description/status/behavior/readonly を取得する |
| `task_instructions` | `boid task instructions` | この job 自身の routed instruction を取得する |
| `task_env` | `boid task env` | `allowed_domains` + `host_commands` (サンドボックス内から観測できない情報) を取得する |
| `task_payload` | `boid task payload` | trait フィルタ済みの現在の payload を取得する |
| `task_attachments_list` | `boid task attachments list` | この task の添付ファイル名一覧を取得する |
| `task_attachments_get` | `boid task attachments get <name>` | 添付ファイル 1 件の中身を取得する |
| `project_behaviors` | `boid project behaviors <project-ref>` | project の task_behaviors 一覧を JSON で取得する (`ref` は同一 workspace 内の project のみ解決可能) |

> **注記:** `task.reopen` だけが歴史的事情で `.` 区切りになっています。 他の op は `_` 区切りです。 `task_current` / `task_attachments_list` / `task_attachments_get` は TaskID スコープ、 `task_instructions` / `task_env` / `task_payload` は JobID スコープです ([Hook スクリプトプロトコル](../reference/hook-contract.md) 参照)。

### fetch builtin

`boid fetch <url>` はサンドボックス内からプロキシ allowlist を通じて HTTP GET を行います。 `curl` / `wget` を `host_commands` で宣言せずに web リソースを取得したいときに使います。

| Op | 対応 CLI | 用途 |
|---|---|---|
| `fetch` | `boid fetch <url>` | アウトバウンドプロキシ経由で HTTP GET する |

### 設計上の注記

- **role 分岐なし** — `boid` / `fetch` ポリシーは `_ Role` で受け、全 role に同一 op セットを与えます。
  新しい builtin で role 固有の制限が必要になった場合のみ、 `policyFor` 内に `switch` を追加してください。
- **情報源** — `internal/orchestrator/policy.go` の `boidPolicy` / `fetchPolicy` 関数が source of truth です。
- **サンドボックス側 enum 定義** — `internal/sandbox/protocol.go`
- **workspace / project 越えのアクセス** は broker (`internal/sandbox/broker.go` `handleBoidBuiltin`) が
  `entry.Context.AllowsProject(...)` 等で拒否します。 上記 op セットはこのチェックをバイパスしません。

## 関連ドキュメント

- [アーキテクチャ概要](overview.md) — sandbox レイヤの位置づけ
- [概念 / サンドボックス](../guide/concepts.md#サンドボックス-sandbox) — ユーザ視点での意味
- [Hook スクリプトプロトコル](../reference/hook-contract.md) — sandbox 内 handler の I/O
- [`project.yaml` リファレンス](../reference/project-yaml.md) — `host_commands` / `additional_bindings` / `capabilities` の宣言
- [Docker プロキシ移行ガイド](../guide/docker-proxy-migration.md) — docker kit (cetusguard) から native proxy への移行
