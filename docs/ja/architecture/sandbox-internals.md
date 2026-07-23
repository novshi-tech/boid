# サンドボックス内部実装

`boid` のサンドボックスがどう組まれているか、 hook を 1 つ動かすときに何が起きているかを記したページです。 [アーキテクチャ概要](overview.md) の sandbox 節を、ファイルとシステムコールの粒度で掘り下げています。

主な読者は `internal/sandbox/` に手を入れる contributor、サンドボックス絡みの不具合を追っている人、あるいは「なぜ**ホスト側**のホームディレクトリが見えないのか」 を最後まで知りたい人です (サンドボックス自身の `$HOME` は別物で workspace スコープです — 後述の「sandbox 内のプロセスからは」を参照)。

## ねらい

サンドボックスは 4 つの境界をまとめて作ります。

1. **ファイルシステム** — 書き込み可能な領域を sandbox 内 clone (project が可視でないジョブではプロジェクトルート) に絞る
2. **ネットワーク** — 組み込みリストと `config.yaml` の `sandbox.allowed_domains` に含まれるドメインしか出ていけない
3. **ユーザ ID** — ホストの root には触れない (rootless)
4. **コマンド** — host で動かすコマンドは kit の `host_commands` で宣言された分だけ通る

これら全てを Linux の標準機構 (mount namespace / user namespace / chroot / pasta / nftables) で組み合わせます。 Docker のような追加ランタイムは要りません。

## 起動の全体像

`boid` daemon が hook を 1 つ起動するとき、dispatcher が JSON 形式の spec ファイルをディスクに書き出し、`boid runner-outer` を起動します。プロセスチェーンは 5 段です。

```
+-------------------------------------------------------------+
| runner-outer  (host で実行)                                 |
|   JSON spec を読み込み                                      |
|   pasta を子プロセスとして exec ------+                     |
+----------------------------------------|--------------------+
                                         ▼
+-------------------------------------------------------------+
| pasta (ネットワーク namespace + ユーザ namespace)           |
|   -- boid runner-inner を起動 --------+                    |
+----------------------------------------|--------------------+
                                         ▼
+-------------------------------------------------------------+
| runner-inner  (pasta の user+net ns 内、 内側 uid 0)        |
|   nftables で egress を制限 (proxy 経由のみ許可)           |
|   clone(CLONE_NEWUSER|CLONE_NEWNS) で子を fork ----+       |
+------------------------------------------------------|------+
                                                       ▼
+-------------------------------------------------------------+
| runner-inner-child  (新規 user+mount ns 内、 uid 0)         |
|   $ROOT に bind mount で sandbox fs を組み立て              |
|   pivot_root で $ROOT を新しいルートに                      |
|   adapter.Run() でエージェントを exec                       |
+-------------------------------------------------------------+
```

実装は [`internal/sandbox/runner/`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/runner/) に集まっています (Phase 3-a で bash 3 本から Go ネイティブに置換)。

### 1. `runner-outer`

dispatcher が書き出した JSON spec を読み込み、pasta を次の引数で起動します:

```
pasta --config-net -4 \
    -a 10.0.2.0 -n 24 -g 10.0.2.2 \
    --dns-forward 10.0.2.3 \
    -t none -u none \
    -- boid runner-inner --spec <spec.json> --state <state.json>
```

`pasta` はユーザモードで動くネットワーク namespace ラッパで、サンドボックス内のプロセスから見たネットワークを完全に独立させます。 ホストの NIC 経由ではなく、 pasta が提供するゲートウェイ (`10.0.2.2`) と DNS フォワーダ (`10.0.2.3`) が経路です。 pasta の戻り後にホスト側 cleanup を行います (後述)。

### 2. `runner-inner`

pasta の user+net namespace の内側で動きます。 この時点では**内側 uid 0** です (uid_map: host uid 1000 → container uid 0。 uid 0 がなければ `CAP_SYS_ADMIN` が得られず後続の mount 操作が EPERM になるため)。

主要ステップ:

- **nftables ルール** — uid 0 のうちに `CAP_NET_ADMIN` で egress ルールを書く (proxy ポート以外を drop)
- **`clone(CLONE_NEWUSER|CLONE_NEWNS)`** — `runner-inner-child` を新しい user+mount namespace に生成する。 uid_map は同じく `ContainerID=0, HostID=<euid>, Size=1`

### 3. `runner-inner-child`

新規の user+mount namespace 内で動きます (uid 0)。 ここで sandbox の fs レイアウトを組み立て、エージェントを起動します。

主要ステップ:

- **bind mount** — kit の `additional_bindings`、 sandbox 内 clone の runtime dir (project が可視な場合)、 `/usr` や `/lib` 等のシステムディレクトリを `$ROOT` 配下に bind / rbind マウント。 これがサンドボックス内から見えるファイルセットを決めます
- **`pivot_root`** — `$ROOT` を新しいルートに切り替える。 旧ルートは `/.old_root` にピボットしてから umount + rmdir する
- **シンボリックリンク** — `boid` shim を `/run/boid/bin/<command>` 等にリンク
- **`adapter.Run()`** — HarnessAdapter 経由でエージェント (claude / codex / opencode / shell) を exec し、停止シグナル (SIGUSR1 → 子に SIGTERM) の中継・終了コード正規化・broker job-done 送信を行う

sandbox 内のプロセスからは:

- **host 側**のホームディレクトリ・SSH 鍵・他プロジェクトは存在自体が見えない (`$ROOT` 配下に bind しなければパスが解決しない)。 サンドボックス自身の `$HOME` は別物 — 下記参照
- 自分は uid 0 (内側) で動いているが、 user namespace の外には出られずホストの root へのエスカレーションパスはない

サンドボックス内の `$HOME` は host 共有でも毎回まっさらな tmpfs でもなく、 **同一 workspace に dispatch される job 間で永続する、 read-write bind された workspace スコープの volume** です (docs/plans/home-workspace-volume.md Phase 4)。 hook が `$HOME` 配下に書いたファイルは、 同じ workspace の後続の別 job からも見えます。 `$HOME/.boid` も同様に永続します — Phase 6 PR8 以前は dispatch 毎に job-scoped tmpfs を重ねて `$HOME/.boid/output/payload_patch.json` を job 間で隔離していましたが、 payload patch の唯一経路が broker RPC (`boid task update --payload-patch`) になったことでこのファイル経由の出力自体が撤廃され、 隔離用の tmpfs も不要になりました (詳細は [Hook スクリプトプロトコル / 出力](../reference/hook-contract.md#出力))。

タスクコンテキストは `boid task current` / `instructions` / `env` / `payload` — shim 経由で呼べる broker RPC — で取得します。 dispatch 時に一括生成する方式ではなく、必要になった時点で pull します。 hook のプロトコル詳細は [Hook スクリプトプロトコル](../reference/hook-contract.md)。

## ネットワーク制御

ネットワーク境界は 2 段構えです。

### ① pasta (ネットワーク namespace)

pasta はユーザ権限で動くツールで、サンドボックス内に独立したネットワーク namespace を提供します。 ホストの物理 NIC は見えず、外向きの通信は pasta が host 側へリレーします。

### ② nftables による drop ルール

`runner-inner` が uid 0 のうちに nftables ルールを書き、 proxy ポート以外への外向きパケットを drop します。 結果として:

- HTTP/HTTPS は環境変数 `http_proxy` / `https_proxy` で `10.0.2.2:<port>` を指定して proxy 経由でしか出られない
- proxy は許可リストに該当するホストだけ中継する
- 直接の TCP/UDP は遮断される

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
sandbox 内: boid <subcommand>     (shim バイナリ)
              |
              | UNIX socket (host 上)
              v
host: boid daemon の broker (internal/sandbox.Broker)
              |
              | コマンドポリシーを評価
              v
host: 許可されたコマンドを実際に exec
```

shim は sandbox 起動時に bind mount で sandbox 内に持ち込まれます (実体は `internal/sandbox/boid_shim.go` でビルドされる薄いバイナリ)。 `boid task update` や `boid job done`、 kit が `host_commands` 宣言した `gh` / `git push` 等は、すべてこの経路で host へ流れます。

broker は `internal/sandbox/broker.go` にあり、次の責務を持ちます:

- shim からの要求を UNIX socket で受ける
- リクエストにくっついている **トークン** を見て、どの job が呼んでいるかを特定する
- そのジョブが許可されたコマンド・サブコマンド・引数パターンに合致するかを `policy.go` の `CheckPolicy` で判定する
- 許可されていれば host 側で実際に exec し、 stdout / stderr / 終了コードを shim に返す

トークンは sandbox 起動時に発行され、 sandbox 内の環境変数 `BOID_BROKER_TOKEN` 等で受け渡されます。 sandbox の外からはトークンを知ることができないため、たとえ broker socket のパスが漏れても、別 job のコマンドを許可させることはできません。

host command は host 側で project の checkout ディレクトリではなく中立ディレクトリ (`os.TempDir()`) で実行されます。 stdin も渡りません。 repo 文脈が必要なコマンド (`gh` 等) は kit の `env:` に `${boid:repo_slug}` を書いて渡します (詳細は [`project.yaml` リファレンス](../reference/project-yaml.md) の「host command の実行契約」)。

## 後片付け

後片付けは **`runner-outer`** が pasta の戻り後に Go コードで行います。

```go
// runner-outer (抜粋)
cleanupRoot(spec.RootDir)          // /tmp/boid-root-* プレフィックス確認後に rm -rf
for _, p := range spec.CleanupPaths { os.RemoveAll(p) }
os.Remove(specPath)                // spec は secrets を含むので成否に関わらず削除
if exitCode == 0 { os.Remove(statePath) }  // state は失敗時のみ保全
```

ポイントは 3 つです。

1. **マウント解除はカーネルに任せる**。 `runner-inner` / `runner-inner-child` が動いていた mount namespace は pasta プロセスが exit した時点で破棄され、配下の bind mount はカーネルに自動回収される。 `umount -R` を明示的に呼ぶ必要はない。
2. **`$ROOT` の削除は namespace の外で行う**。 `runner-outer` が動くのはホストの mount namespace。 pasta が終了した後なので sandbox の bind mount は既に消えており、 `rm -rf` がホストファイルに到達する余地がない。
3. **`/tmp/boid-root-*` プレフィックスでなければ rm しない**。 `spec.RootDir` が意図しない値になっていてもホストを壊さない安全弁 (`cleanupRoot` が確認)。

`exitCode != 0` のときは `runner-state.json` (`/tmp/boid-<runtime_id>-runner-state.json`) を保全して事後解析に使えるようにします。 この JSON には起動フェーズの進行記録・spec (secrets は redact 済)・終了コードが含まれ、30 日後に GC で削除されます。 spec ファイルは成否に関わらず削除します (broker token 等の secrets を含むため)。

過去にマウント越しの `rm -rf` でホストファイルが消えた事例があり、現在の実装は own ns / cross ns の 2 経路すべてで安全側に倒すようになっています。

## サンドボックス内から呼べる boid builtin 一覧

サンドボックス内のハンドラ (hook / exec) は `boid`、`fetch` の 2 つの builtin を呼ぶことができます。
いずれも自動的に注入されるため、 `project.yaml` / `kit.yaml` での宣言は不要です。

`git` は broker builtin ではありません。 サンドボックス内の実バイナリ (`/usr` の base rbind 経由) として動作し、
project の clone・fetch・push はすべて sandbox 内の git が git gateway (認証注入リバースプロキシ) 経由で行います
(host への broker dispatch は無し)。 詳細は [`project.yaml` リファレンス](../reference/project-yaml.md#git-gateway--sandbox-内-clone) を参照してください。

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
