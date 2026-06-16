# サンドボックス内部実装

`boid` のサンドボックスがどう組まれているか、 hook を 1 つ動かすときに何が起きているかを記したページです。 [アーキテクチャ概要](overview.md) の sandbox 節を、ファイルとシステムコールの粒度で掘り下げています。

主な読者は `internal/sandbox/` に手を入れる contributor、サンドボックス絡みの不具合を追っている人、あるいは「なぜホームディレクトリが見えないのか」 を最後まで知りたい人です。

## ねらい

サンドボックスは 4 つの境界をまとめて作ります。

1. **ファイルシステム** — 書き込み可能な領域を worktree (もしくはプロジェクトルート) に絞る
2. **ネットワーク** — 組み込みリストと `config.yaml` の `sandbox.allowed_domains` に含まれるドメインしか出ていけない
3. **ユーザ ID** — ホストの root には触れない (rootless)
4. **コマンド** — host で動かすコマンドは kit の `host_commands` で宣言された分だけ通る

これら全てを Linux の標準機構 (mount namespace / user namespace / chroot / pasta / nftables) で組み合わせます。 Docker のような追加ランタイムは要りません。

## 起動の全体像

`boid` daemon が hook を 1 つ起動するとき、 `internal/sandbox.Prepare` が 3 枚のシェルスクリプトを `/tmp/boid-<job-id>-{outer,setup,inner}.sh` に書き出します。

```
+-------------------------------------------------------------+
| outer.sh  (host で実行)                                     |
|   pasta (network namespace) で囲んで                        |
|     unshare --mount  ------+                                |
+----------------------------|--------------------------------+
                             ▼
+-------------------------------------------------------------+
| setup.sh  (mount namespace 内、 host fs はまだ全部見えている)|
|   bind mount で $ROOT に worktree や kit ファイルを並べ      |
|   nftables で外向きパケットを drop (proxy 経由のみ許可)      |
|   exec unshare --user --map-user --root=$ROOT --            |
|     bash /tmp/inner.sh   ----+                              |
+------------------------------|------------------------------+
                               ▼
+-------------------------------------------------------------+
| inner.sh  ($ROOT 配下しか見えない rootless な環境)          |
|   env を設定し、 handler スクリプトを実行                    |
+-------------------------------------------------------------+
```

実装は [`internal/sandbox/script.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/script.go) と [`internal/sandbox/render.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/render.go) に集まっています。

### 1. `outer.sh`

```bash
pasta --config-net \
    -a 10.0.2.0 -n 24 -g 10.0.2.2 \
    --dns-forward 10.0.2.3 \
    -t none -u none \
    -- unshare --mount -- bash /tmp/boid-<id>-setup.sh
```

`pasta` はユーザモードで動くネットワーク namespace ラッパで、サンドボックス内のプロセスから見たネットワークを完全に独立させます。 ホストの NIC 経由ではなく、 pasta が提供するゲートウェイ (`10.0.2.2`) と DNS フォワーダ (`10.0.2.3`) が経路です。

その内側で `unshare --mount` を発行し、 mount namespace を切ります。これで以降の bind mount は host の他プロセスには見えません。

### 2. `setup.sh`

`unshare --mount` 直後の状態では、ファイルシステムはまだ host のものがそのまま見えています。 `setup.sh` がここで `$ROOT` (例: `/tmp/boid-root-XXXXXX`) を新しいルートとして組み立てます。

主要ステップ:

- **bind mount** — kit の `additional_bindings`、 worktree、 `/usr` や `/lib` 等のシステムディレクトリを `$ROOT` 配下に bind / rbind マウント。これがサンドボックス内から見えるファイルセットを決めます
- **ファイル書き出し** — kit / job 固有の設定ファイルを `$ROOT` 配下に置く
- **nftables ルール** — proxy ポート以外への外向きパケットを drop。 これで pasta 越しに出ていくときに、 proxy を使う場合のみ通す
- **シンボリックリンク** — `boid` shim を `/opt/boid/bin/<command>` のような形でリンクし、サンドボックス内から `boid` バイナリを呼べるようにする

最後に `setup.sh` は次のように `unshare` を再発行して、組み立て済みの `$ROOT` を新しいルートにしてから `inner.sh` を起動します。

```bash
exec unshare --user --map-user=1000 --map-group=1000 --root="$ROOT" -- /bin/bash /tmp/inner.sh
```

- `--user` で user namespace を切る (rootless 化)
- `--map-user=1000 --map-group=1000` で sandbox 内の UID/GID は 1000:1000 にマップ
- `--root=$ROOT` で `$ROOT` を sandbox プロセスのルートディレクトリに

これにより sandbox 内のプロセスからは:

- ホームディレクトリ・SSH 鍵・他プロジェクトは存在自体が見えない (`$ROOT` 配下に bind しなければそもそもパスが解決しない)
- 自分は UID 1000 として動いており、ホストの root へのエスカレーションパスはない

### 3. `inner.sh`

ここからは sandbox 内です。 環境変数 (`BOID_TASK_ID` 等の kit / behavior が宣言したもの) を `export` してから handler の argv を実行します。 タスクメタは stdin では渡されません — 全 hook は interactive (PTY) ジョブとして動作し、タスクコンテキストは `$HOME/.boid/context/{task,instructions,environment,payload}.{yaml,json}` のコンテキストファイル経由で参照します。 hook の終了コードはそのまま `exec` で置き換える形で返り、 `setup.sh` を経て `outer.sh` まで伝播します。

handler のプロトコル詳細は [Hook スクリプトプロトコル](../reference/hook-contract.md)。

## ネットワーク制御

ネットワーク境界は 2 段構えです。

### ① pasta (ネットワーク namespace)

pasta はユーザ権限で動くツールで、サンドボックス内に独立したネットワーク namespace を提供します。 ホストの物理 NIC は見えず、外向きの通信は pasta が host 側へリレーします。

### ② nftables による drop ルール

`setup.sh` 内で nftables ルールを書き、 proxy ポート以外への外向きパケットを drop します。 結果として:

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

## 後片付け

後片付けは **`outer.sh`** が pasta の戻り後に行います。 `setup.sh` 側にはトラップを置きません。

```bash
# outer.sh (抜粋)
...
exit_code=$?
...
rm -f "$pasta_stderr" 2>/dev/null || true
case "$root_dir" in
    /tmp/boid-root-*) rm -rf "$root_dir" 2>/dev/null || true ;;
    *) echo "[boid] WARNING: root_dir=$root_dir not under /tmp/boid-root-*, skipping cleanup" >&2 ;;
esac
rm -rf <staging-dirs...> 2>/dev/null || true
if [ "$exit_code" -eq 0 ]; then
    rm -f <outer.sh> <setup.sh> <inner.sh> 2>/dev/null || true
fi
exit $exit_code
```

ポイントは 3 つです。

1. **マウント解除はカーネルに任せる**。 setup.sh は `unshare --mount` の名前空間内で動いており、 そのプロセスが exit すれば名前空間が破棄されて配下の bind mount は全てカーネルに回収される。 `umount -R` を明示的に呼ぶ必要は無い (旧実装では `$ROOT` 自体が mountpoint ではないため `umount -R` が初手で失敗し、 配下が一切剥がれないバグになっていた)。
2. **`$root_dir` の削除は `unshare` の外で行う**。 outer.sh が動くのは pasta の親側 = ホストの mount 名前空間。 ここまで来れば sandbox 名前空間に居た bind mount は既に消えているので、 `rm -rf` がホストファイルに到達する余地が無い。
3. **`/tmp/boid-root-*` プレフィックスでなければ rm しない**。 `$root_dir` が意図しない値になっていても host を壊さない安全弁。

`exit_code != 0` のときは script ファイル (`*-outer.sh` `*-setup.sh` `*-inner.sh`) だけ保全して事後解析に使えるようにします (`internal/dispatcher/runner.go` の `cleanupSandboxAfterWait` 参照)。 `root_dir` / staging dir は名前空間破棄後はカラのスケルトンなので保全する意味が無く、 常に削除します。

過去にマウント越しの `rm -rf` でホストファイルが消えた事例があり (memory: "feedback: bind_rm_traverses_source")、 現在の実装は own ns / cross ns / chroot holder の 3 経路すべてで安全側に倒すようになっています。

## サンドボックス内から呼べる boid builtin 一覧

サンドボックス内のハンドラ (hook / exec) は `boid`、`git`、`fetch` の 3 つの builtin を呼ぶことができます。
いずれも自動的に注入されるため、 `project.yaml` / `kit.yaml` での宣言は不要です。

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

> **注記:** `task.reopen` だけが歴史的事情で `.` 区切りになっています。 他の op は `_` 区切りです。

### git builtin

全 role で同じ op セットが許可されます。

| Op | 対応 CLI | 用途 |
|---|---|---|
| `fetch` | `git fetch ...` | リモートから取得する |
| `push` | `git push ...` | リモートへ反映する |
| `push_delete` | `git push origin --delete <branch>` | リモートブランチを削除する |
| `clone_local` | `git clone --local ...` | ローカルリポジトリを clone する (peer ブランチ参照用) |

### fetch builtin

`boid fetch <url>` はサンドボックス内からプロキシ allowlist を通じて HTTP GET を行います。 `curl` / `wget` を `host_commands` で宣言せずに web リソースを取得したいときに使います。

| Op | 対応 CLI | 用途 |
|---|---|---|
| `fetch` | `boid fetch <url>` | アウトバウンドプロキシ経由で HTTP GET する |

### 設計上の注記

- **role 分岐なし** — `boid` / `git` / `fetch` ポリシーは `_ Role` で受け、全 role に同一 op セットを与えます。
  新しい builtin で role 固有の制限が必要になった場合のみ、 `policyFor` 内に `switch` を追加してください。
- **情報源** — `internal/orchestrator/policy.go` の `boidPolicy` / `gitPolicy` / `fetchPolicy` 関数が source of truth です。
- **サンドボックス側 enum 定義** — `internal/sandbox/protocol.go`
- **workspace / project 越えのアクセス** は broker (`internal/sandbox/broker.go` `handleBoidBuiltin`) が
  `entry.Context.AllowsProject(...)` 等で拒否します。 上記 op セットはこのチェックをバイパスしません。

## 関連ドキュメント

- [アーキテクチャ概要](overview.md) — sandbox レイヤの位置づけ
- [概念 / サンドボックス](../guide/concepts.md#サンドボックス-sandbox) — ユーザ視点での意味
- [Hook スクリプトプロトコル](../reference/hook-contract.md) — sandbox 内 handler の I/O
- [`project.yaml` リファレンス](../reference/project-yaml.md) — `host_commands` / `additional_bindings` / `capabilities` の宣言
- [Docker プロキシ移行ガイド](../guide/docker-proxy-migration.md) — docker kit (cetusguard) から native proxy への移行
