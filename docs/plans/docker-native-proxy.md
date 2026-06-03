# Docker ネイティブプロキシ 設計ドキュメント

ステータス: 設計中  
作成日: 2026-06-03

---

## 背景と解決する課題

現状の Docker API アクセス制御は boid-kits の docker kit が担っており、
外部ツール [cetusguard](https://github.com/hectorm/cetusguard) に依存している。

**課題 1 — セットアップ負担**

cetusguard を使うには、バイナリのインストール・`~/.config/cetusguard/rules.txt`
の作成・systemd unit の有効化という前提作業が必要で、新規ユーザがすぐ使える状態にならない。

**課題 2 — セキュリティの穴**

cetusguard は HTTP メソッドと URL パスのみを照合し、**リクエストボディを検査しない**。
docker kit の README 自身が「ボディ制約は不可」と明記している通り、
POST /containers/create のボディに含まれる以下の設定を一切ブロックできない:

- `HostConfig.Binds` / `HostConfig.Mounts` — host パスの bind mount
  （`Mounts` は `Type=volume` でも local driver の `o=bind,device=/host/path` で host を mount できる）
- `HostConfig.Privileged = true`
- `HostConfig.NetworkMode = "host"` / `PidMode = "host"` / `IpcMode = "host"`
- `HostConfig.UsernsMode = "host"` / `CgroupnsMode = "host"`
- `HostConfig.SecurityOpt` — `seccomp=unconfined` / `apparmor=unconfined` /
  `no-new-privileges=false` / `label=disable`（Privileged=false でも隔離を剥がせる）
- `HostConfig.CapAdd` の危険な capability（`SYS_ADMIN` は実質 privileged 相当）
- `HostConfig.Devices` / `DeviceCgroupRules` によるデバイスアクセス
- `HostConfig.Runtime` — runc 以外の代替ランタイム指定
- `HostConfig.Sysctls` / `CgroupParent` — 任意 sysctl / 任意 cgroup への配置

これらを悪用すれば、サンドボックスのファイルシステム分離・ネットワーク分離を
コンテナ経由でバイパスできる。**Privileged 単体ではなく、上記の組み合わせで実質
privileged 相当になる点に注意**（例: `SecurityOpt=seccomp=unconfined` +
`CapAdd=SYS_ADMIN`）。

**方針**

socket / API レベルの boid ネイティブ Docker プロキシを実装し、
リクエストボディを検査して危険な設定を deny する。boid daemon が管理・自動起動し、
cetusguard への外部依存を排除する。

---

## なぜ CLI 引数ポリシーでは不十分か

`docker` コマンドを `host_commands` に登録して `-v` / `--privileged` を
パターンマッチで deny する方法は、docker CLI から直接呼ぶ場合には機能する。

> 補足: そもそも `docker` を host_commands に登録すること自体が、ホスト直実行による
> proxy バイパスを招くため禁止（後述「docker への経路は proxy socket だけ」）。
> ここでは「仮に CLI 引数で防ごうとしても不十分」という論点として述べている。
しかし現 docker kit の主目的は **TestContainers** であり、
TestContainers は Docker Engine API を Unix ソケット経由で **直接** 呼ぶ。
`docker compose`・Compose V2 プラグイン・Docker SDK for Go/Python なども同様で、
CLI を経由しない。

したがって CLI 層のポリシーだけでは防御不能であり、
**socket / API レベルのプロキシが必須** である。

---

## アーキテクチャ概要

```
サンドボックス内プロセス (TestContainers / compose / docker CLI)
        |
        | DOCKER_HOST=unix:///run/boid/docker-proxy.sock
        v
/run/boid/docker-proxy.sock   ← boid daemon が立てた Unix ソケット
        |
        v
[Docker Native Proxy] (internal/sandbox/dockerproxy/)
  ├─ 透過転送エンドポイント (大多数)
  │     └─ Unix ソケット転送 → /run/user/<uid>/docker.sock (実 daemon)
  └─ 検査エンドポイント (少数の mutating API)
        ├─ BodyPolicy 評価
        │     ├─ ALLOW → 転送
        │     └─ DENY  → 403 + 理由をログ
        └─ Unix ソケット転送 → /run/user/<uid>/docker.sock
```

proxy socket のパスは `SandboxRuntimeInfo` に追加し、
`sandbox_builder.go` が `--bind-mount /run/boid/docker-proxy.sock:...`
でサンドボックスに差し込む。`DOCKER_HOST` / `CONTAINER_HOST` /
`TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE` 環境変数も自動設定する。

---

## 既存資産の再利用

| 既存資産 | 役割 | 今回の利用方法 |
|---|---|---|
| `internal/sandbox/proxy.go` | TCP HTTP プロキシ (ドメイン allowlist) | リクエスト検査型プロキシのパターンを踏襲 |
| `internal/sandbox/broker.go` | token + policy 検証・dispatch | ポリシー評価の構造 (tokenEntry / CheckPolicy) を参照 |
| `internal/sandbox/policy.go` | `BuiltinPolicy` / `CommandDef` | `DockerPolicy` 型設計の参考 |
| `internal/dispatcher/sandbox_builder.go` | サンドボックス構築・mount 組み立て | proxy socket の bind-mount 追加 |

ゼロからではなく、既存のリクエスト検査パターンを Docker API 用に特化させる形。

---

## 検査が必要なエンドポイントの切り分け

Docker Engine API は多数のエンドポイントを持つが、
**ボディ検査が必要なのは少数の mutating エンドポイント** に限られる。
GET 系の読み取りエンドポイントは透過転送で十分であり、検査範囲は有界である。

### 設計原則: 未知の mutating は fail-closed

「大多数を透過、少数を検査」という素朴な分類は **fail-open**（将来 Docker が
新しい危険エンドポイントを追加すると自動的に素通しになる）になりやすい。
本プロキシは逆向きの原則を採る:

- **GET / HEAD（読み取り）は既定で透過転送**。
- **POST / PUT / DELETE（mutating）は、明示的に「透過可」と判定したものだけ透過**。
  それ以外の未知の mutating エンドポイントは **既定 deny（fail-closed）** とし、
  ログに残す。新しい API を素通しさせるには allowlist への明示的追加を要する。

これにより、Docker API のバージョンアップで追加された未知の mutating API が
無検査で通る事故を防ぐ。

### 検査必須エンドポイント

| エンドポイント | 危険な設定 |
|---|---|
| `POST /containers/create` | HostConfig (Binds, Mounts, Privileged, NetworkMode, PidMode, IpcMode, UsernsMode, CgroupnsMode, **SecurityOpt**, CapAdd, Devices, DeviceCgroupRules, Runtime, Sysctls, CgroupParent) |
| `POST /containers/{id}/exec` | exec create の `Privileged` フィールド |
| `POST /networks/create` | HostNetworkMode 等の危険なドライバオプション |
| `POST /volumes/create` | DriverOpts に host パスを指定する外部ドライバ悪用 (`o=bind,device=`) |
| `POST /build` / `POST /session` (image build) | **image build は全 deny**（検査せず拒否。BuildKit の gRPC トンネルは検査不能） |
| `POST /images/create` (pull) | 必要なら registry allowlist |

> ⚠️ `POST /containers/{id}/exec` の検査対象は exec **作成** エンドポイントである。
> 「exec に危険オプションは少ない」というのは誤りで、exec create には `Privileged`
> フィールドがあり、コンテナによっては特権 exec が成立しうる。

### 透過転送でよいエンドポイント (代表例)

- `GET /containers/{id}/logs`
- `POST /containers/{id}/attach`（attach は GET ではなく **POST**。hijack 対象）
- `POST /containers/{id}/stop`, `/{id}/wait`
- `POST /exec/{id}/start`（exec の起動。危険オプションは create 側で検査済み）
- `GET /info`, `GET /version`, `GET /_ping`
- `GET /images/...`, `DELETE /containers/...`

> ⚠️ `POST /containers/{id}/start` を無条件透過にしない。古い API バージョンでは
> `start` のボディで HostConfig を渡せた（v1.24+ では無視されるはずだが、上流が
> 古い場合のフォールバックを塞ぐため、HostConfig 付き start を deny するか、
> プロキシが API バージョンを下限固定する）。

---

## ポリシー設計: secure-by-default + 最小設定面

### 設計原則

1. **既定で安全 (secure-by-default)**: `capabilities.docker` を宣言して docker proxy を
   有効にしても、危険機能の設定を細かく書く必要はなく、全て安全側（deny）に倒れる。
   有効化の一行だけでよく、プロジェクトごとの細かな設定作業は要らない。
2. **設定可能項目は需要ドリブンで最小限**: 初期段階では危険機能のオーバーライドを
   公開しない。「設定できる」こと自体が攻撃面・誤設定リスクになるため、デフォルトで
   足りるなら設定項目を作らない。実需要が確認された項目だけ後から最小限のフラグを足す。
3. **ファイルシステム bind は常に deny (設定項目を設けない)**:
   サンドボックスのファイルシステム分離を直接破るため、host パスの bind mount は
   一律禁止する。`allowed_bind_paths` のような許可リストは初期段階では用意しない。
   （TestContainers の Ryuk が要求する docker.sock bind は別問題であり、
   Ryuk 無効化で回避する。後述の「TestContainers (Ryuk) 互換性」を参照。）

### 既定で deny される機能 (初期段階はオーバーライド不可)

- ファイルシステム bind mount (系統 1/2/3 すべて)
- Privileged
- host namespace 共有 (network / pid / ipc / userns / cgroupns = host)
- SecurityOpt（**指定があれば値を問わず deny**。seccomp/apparmor unconfined や
  no-new-privileges 無効化が危険なため、初期段階は SecurityOpt 自体を一律拒否し、
  ブラックリスト/ホワイトリストの粒度問題を生まない）
- CapAdd (capability 追加)
- Devices / DeviceCgroupRules
- runc 以外の Runtime
- 任意の Sysctls / CgroupParent

これらは通常の `docker run` や TestContainers では使われないため、固定 deny でも
大多数のユースケースは影響を受けない。

### Go 型定義 (初期段階)

```go
// DockerPolicy は将来のオーバーライド用に予約された型。
// 初期段階では設定可能なフィールドを持たず、プロキシは上記「既定で deny される機能」を
// ハードコードされた安全既定として適用する。
//
// 将来、実需要が確認された項目だけ最小限のフィールドを追加する
// (例: エアギャップ環境向けの AllowedRegistries)。
// bind mount は許可項目を設けず、常に deny する方針を維持する。
type DockerPolicy struct {
    // 初期段階ではフィールドなし。
}
```

`MaxBodyBytes` のような運用パラメータはユーザ設定ではなくプロキシ内部の定数とし、
ポリシー設定面には露出しない。

### project.yaml での表現

```yaml
# capabilities.docker の宣言が docker proxy の有効化トリガー（opt-in）。
# 中身は通常空でよく、危険機能は全て安全既定で deny される。
capabilities:
  docker: {}
```

`capabilities.docker` の宣言自体が有効化スイッチであり、かつ将来のポリシー設定の
置き場所でもある（初期段階で書くべき設定は無い）。有効化の詳細は後述
「サンドボックス統合と有効化」を参照。

---

## bind mount の 3 系統を全てカバーする

Docker API には bind mount を指定するエンドポイント表現が **3 系統** ある。
どれか一つでも塞ぎ漏らすと残りから抜けられるため、全てを必ず検査する。

**系統 1: `HostConfig.Binds`**  
文字列配列。`-v` 相当の簡易記法。

```json
"Binds": ["/host/path:/container/path:ro"]
```

検査方法: `Binds` に要素が 1 つでも存在すれば deny する（初期段階は bind を一律禁止）。

**系統 2: `HostConfig.Mounts`**  
オブジェクト配列。`--mount` 相当のリッチ記法。

```json
"Mounts": [
  {
    "Type": "bind",
    "Source": "/host/path",
    "Target": "/container/path",
    "ReadOnly": true
  }
]
```

検査方法: `Type == "bind"` の要素が 1 つでも存在すれば deny する。
`Type` が `tmpfs` / `npipe` の場合は host パスを取らないのでスキップしてよい。
**`Type == "volume"` はスキップしてはならない**（系統 3 を参照）。

**系統 3: `HostConfig.Mounts` の `Type=volume` + local driver bind**

`Type=volume` でも、local volume driver に bind オプションを inline で渡すと、
実質的に host パスの bind mount になる。これは系統 1/2 の単純な `Type` 判定を
すり抜けるため、見落とすと致命的な抜け穴になる。

```json
"Mounts": [
  {
    "Type": "volume",
    "Target": "/container/path",
    "VolumeOptions": {
      "DriverConfig": {
        "Name": "local",
        "Options": { "type": "none", "device": "/host/path", "o": "bind" }
      }
    }
  }
]
```

検査方法: `Type == "volume"` で `VolumeOptions.DriverConfig.Options` に
`device`（host パス）または `o=bind` を含む要素があれば deny する。同様の検査を
`POST /volumes/create` の `DriverOpts`（同じ `device` / `o=bind` キー）にも適用する。

---

## 透過転送で丁寧に扱う必要があるもの

### API バージョンプレフィックス

Docker Engine API は `/v1.43/containers/create` のようにバージョンプレフィックスを持つ。
ルーティング時はプレフィックスを除去してパスを照合し、上流への転送時は元の URL をそのまま使う。
正規表現: `^/v\d+\.\d+(/.*)?$`

### exec / attach / logs のストリーミング (HTTP hijack)

`POST /exec/{id}/start` や `POST /containers/{id}/attach` は、
レスポンスヘッダ送信後に TCP コネクションを raw ストリームとして使用する (HTTP hijack)。
プロキシは `Hijacker` インタフェースで接続を乗っ取り、上流ソケットと双方向 `io.Copy` する。
既存の `proxy.go` の CONNECT トンネルパターンと同じ実装方針でよい。

### image build は許可しない（`POST /build`・`POST /session` を deny）

image build はサンドボックス（proxy）では一切許可しない。`POST /build`（legacy builder）も
`POST /session`（BuildKit の gRPC トンネル）も deny する。

理由: BuildKit は `/session` で接続を hijack し、その上に gRPC（HTTP/2 + protobuf）を
流すため、HTTP のパス・ボディ検査が効かない。`RUN --security=insecure`（privileged build）、
`--network=host`、secret / SSH マウント等が gRPC 越しに通っても検査できない。
**検査不能な mutating エンドポイントは fail-closed の原則どおり deny する**。
legacy builder の `POST /build`（tar ストリーム）も、整合のため同様に deny する。

どうしても image build が必要なプロジェクトは、proxy ではなく **host_commands で
`docker` の `build` サブコマンドだけを opt-in する**最終手段を取る（後述「docker への
経路は proxy socket だけ」を参照）。

### Unix ソケット転送

上流の Docker daemon は Unix ソケット (`/run/user/<uid>/docker.sock`) で待機している。
HTTP/1.1 の `net/http` クライアントは `Dial` をカスタムして Unix ソケットに接続する:

```go
transport := &http.Transport{
    DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
        return net.Dial("unix", upstreamSocket)
    },
}
```

上流ソケットのパスは決め打ちにせず、daemon 起動時に動的解決する（下記参照）。

### ボディ検査時の安全な取り扱い（パーサ差分の回避）

検査対象エンドポイントでも、**プロキシはボディを decode → re-encode して転送しては
ならない**。プロキシと Docker daemon の JSON 解釈差（重複キー、大文字小文字、
未知フィールドの扱い）を突いて、プロキシが見落とした設定を daemon に解釈させる
**parser differential 攻撃** を防ぐため、以下を守る:

- 受信した **生ボディ（bytes）をそのまま上流へ転送** する。検査は同じ bytes を
  読み取るだけで、改変・再構築しない。
- ボディ読み取りには `io.LimitReader` で **`MaxBodyBytes` の上限** を課す。
  超過したら 413/403 で deny（巨大ボディによる DoS / メモリ枯渇対策）。
- JSON デコードは Docker daemon と同じ Go の `encoding/json` を使い、検査対象の
  **既知の危険フィールドを列挙した struct** へデコードする。HostConfig には無害な
  フィールド（`Memory` / `CpuShares` / `RestartPolicy` 等）が多数あるため、
  「未知キーが来たら全て deny」までは**行わない**（互換性を著しく損なうため）。
  代わりに **危険フィールドのリストを網羅的に保守し、Docker API のバージョン更新時に
  レビューする** ことで検査漏れを防ぐ。エンドポイント単位の fail-closed（未知 mutating は
  deny）と、フィールド単位の「危険フィールド列挙」を組み合わせる方針。

### 上流 Docker socket の動的解決（決め打ち禁止）

上流ソケットを `/run/user/<uid>/docker.sock` で決め打ちすると、環境差で壊れる。
daemon 起動時に以下の順で解決する:

1. ユーザ設定（config.yaml の明示指定）があればそれ
2. `DOCKER_HOST` 環境変数（`unix://` パス。TCP の場合は別途扱う / 非対応なら明示エラー）
3. rootless: `$XDG_RUNTIME_DIR/docker.sock` → `/run/user/<uid>/docker.sock`
4. rootful: `/var/run/docker.sock`

解決できなければ起動時に明示エラーとし、サイレントに誤ったソケットへ繋がない。

複数サンドボックスが同一 upstream（uid 単位で共有）へ同時アクセスしても、docker daemon は
多重接続を許容するため、初期段階では接続プールや同時接続数の制限は設けない
（必要が生じたら将来追加する）。

---

## サンドボックス統合と有効化（docker kit の廃止）

### docker kit は廃止し、daemon ビルトインにする

現 docker kit が提供しているのは実質「cetusguard socket の bind-mount」と
「`DOCKER_HOST` 等の環境変数注入」だけで、docker CLI バイナリすら提供していない。

ネイティブプロキシをビルトイン化すると、この socket bind + env 注入は **daemon が
直接行える**。既存の HTTP proxy が `http_proxy` 環境変数をサンドボックスへ注入するのと
同じ仕組み（`sandbox_builder.go` の applyProxyEnv パターン）に乗る。cetusguard socket が
daemon の立てる native proxy socket に置き換わるだけ。

したがって **docker kit という中間層は不要になり、廃止する**。cetusguard への外部依存も
同時に消え、課題 1（セットアップ負担）が根本から解消する。

### 有効化: project.yaml で opt-in

docker proxy は「ホストの docker daemon への通り道」を開けるため、外向き通信を制限する
HTTP proxy よりも強い権限を与える。secure-by-default の方針に従い、**明示的に有効化した
プロジェクトだけ**で使えるようにする（daemon グローバル常時有効にはしない）。

```yaml
# project.yaml
capabilities:
  docker: {}     # このキーがあるプロジェクトでだけ docker proxy が有効
```

`capabilities.docker` が宣言されたサンドボックスに対してのみ、daemon は:

1. そのサンドボックス専用の proxy socket を立てる（socket per sandbox）
2. socket を bind-mount する
3. `DOCKER_HOST` / `CONTAINER_HOST` / `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE` を
   その socket に向け、`TESTCONTAINERS_RYUK_DISABLED=true` を設定する

宣言がなければ proxy socket は存在せず、サンドボックスはホスト docker に一切触れられない。

### docker への経路は proxy socket だけ（host_commands に docker を入れない）

サンドボックス内の docker アクセスは **すべて proxy socket（`DOCKER_HOST`）経由に
統一する**。CLI も SDK も TestContainers も同じ socket を叩く。

> ⚠️ **`docker` を host_commands に登録してはならない。** host_commands は
> **ホスト直実行**（hostcmd broker 経由でホスト側で実行）なので、docker をそこに入れると
> ホストの本物の docker.sock を直接操作でき、proxy を完全にバイパスする。bind escape も
> privileged もやり放題になり、サンドボックス分離を無効化する巨大な穴になる。

docker CLI を使いたい場合も、**サンドボックス内に置いた docker バイナリを
`DOCKER_HOST`=proxy socket 経由で実行する**のが正道。CLI バイナリの可視化方法
（ベース環境に含める / proxy 有効時に daemon が read-only で見せる 等）は実装時に
決めるが、いずれも「ホスト直実行ではなくサンドボックス内実行 + proxy 経由」を守る。
proxy 有効時に host_commands へ `docker` が指定されていたら警告またはエラーにする。

唯一の例外は **`docker` の `build` サブコマンド限定**の登録（`AllowedSubcommands: [build]`）。
image build は proxy で deny する（後述）ため、build が必須のプロジェクトはこれを
**リスク承知で opt-in する最終手段**として使える。ただし `docker build` もホスト直実行なので
`--network host` / `--secret` / Dockerfile の `RUN` 経由のリスクは残る（`run` の bind escape
ほど直接的ではないが無害でもない）。`run`/`exec` 等を含む docker 全体の登録は引き続き禁止。

### TestContainers (Ryuk) 互換性 ⚠️ 実用上の必須要件

本 kit の主目的は TestContainers だが、TestContainers は既定で **Ryuk という
reaper コンテナ** を起動し、Ryuk は **`docker.sock` を bind mount する**。
bind を一律 deny する本設計では、この docker.sock bind も deny されるため、
素のままでは TestContainers が壊れる。

ただし Ryuk が要求するのは **docker.sock への bind**（= docker への通り道）であり、
脱獄に使われる **ホストファイルシステムの bind** とは別物である。両者を混同して
「bind を許可する」方向に倒す必要はない。Ryuk 自体を無効化すればよい:

- **`TESTCONTAINERS_RYUK_DISABLED=true` を sandbox 環境変数に自動設定する**（Phase 1 必須）。
  Ryuk はテスト後にコンテナを掃除する reaper だが、boid サンドボックスは task 終了時に
  まるごと破棄されるため、Ryuk による reap はそもそも不要。これで
  **「ファイルシステム bind 全 deny」と「TestContainers が動く」を両立** できる。

この方針なら bind 許可リストを一切設けずに済む。「Phase 1 完成と同時に TestContainers が
動く」ことを E2E (`docker-proxy-testcontainers`) で担保する。

Ryuk を無効化した分のリソース掃除（コンテナの後始末）は boid が肩代わりする。
→ 別節「コンテナのライフサイクル管理 (Ryuk の内製化)」を参照。

### proxy socket のアクセス制御

- proxy socket はサンドボックスごとに 1 本（socket per sandbox）とし、所有者を
  サンドボックス実行 uid、パーミッションを `0600`（または所有 uid のみアクセス可）
  に設定する。
- bind mount で当該サンドボックスにのみ可視化し、他サンドボックスからは到達不能にする。
- proxy → 上流 docker.sock の接続は daemon の権限で行い、サンドボックス内プロセスが
  上流 socket を直接握れないことを保証する。

### kit 廃止・cetusguard 廃止の手順

1. daemon に docker proxy を実装し、`capabilities.docker` 宣言時に proxy socket を起動
2. `sandbox_builder.go` が socket を bind-mount し、`DOCKER_HOST` 等の env を注入
3. boid-kits の docker kit を廃止（socket bind と env 注入は daemon へ移管）
4. ユーザの project.yaml から `kits: [.../docker]` を外し、`capabilities: docker` に置換
5. cetusguard のバイナリ / rules.txt / systemd unit は不要になり、ユーザは手動で撤去できる
6. docker CLI を使っていたプロジェクトは host_commands の `docker` を外し（proxy バイパス防止）、
   proxy socket 経由での利用に移行する

---

## コンテナのライフサイクル管理 (Ryuk の内製化)

TestContainers の Ryuk を無効化する（前節）ため、その掃除役 ── job が起動した
コンテナ・ネットワーク・ボリュームの後始末 ── を boid が肩代わりする。
コンテナの実体は **サンドボックス内ではなくホストの docker daemon（rootless, uid 単位）
配下に作られる**ため、サンドボックスを破棄してもコンテナは残る。明示的な掃除が要る。

### スコープ: job (runtime_id) 単位

boid は 1 job = 1 サンドボックス = 1 runtime_id の粒度で動く
（`internal/dispatcher/runtime_local_linux.go`。`boid exec` も JobKind=exec の job）。
コンテナのライフサイクルもこの **runtime_id 単位**に紐付ける。これにより
TestContainers / hook / `boid exec` を区別なく同一ルールで掃除できる。

> job をまたいだコンテナの共有・永続化は、既定では掃除対象とする
> （サンドボックスの隔離境界を越えて生き残らせない）。永続の需要が出たら、
> 将来、明示的なオプトインを検討する。

### 識別: socket per sandbox

docker クライアント（TestContainers 等）は boid 固有の認証トークンを載せないため、
「daemon 単位の単一 proxy socket + token で job を識別」はできない。
**サンドボックスごとに別の proxy socket を渡し、socket = runtime_id で識別する**
（各サンドボックスが独立した proxy socket を持つ既存方針と一致）。

### ID 取得: レスポンスから拾う (リクエストは無改変)

生ボディ転送の原則（parser differential 回避）を崩さないため、
**リクエストボディにラベルを注入しない**。代わりに作成系エンドポイントの
**レスポンス**から ID を拾う:

- `POST /containers/create` → レスポンス JSON の `Id`
- `POST /networks/create` → `Id`
- `POST /volumes/create` → `Name`

これらのレスポンスは hijack されない通常の JSON であり、サイズも小さい。
proxy はレスポンスを読み取って ID を記録し、ボディは改変せず下流へ返す。

### 記録: runtime ディレクトリ内のファイル

proxy が対応表をメモリだけに持つと daemon 再起動で失われ、孤児コンテナを追えなくなる。
そこで拾った ID を runtime ディレクトリに永続化する:

```
~/.local/share/boid/runtimes/<runtime_id>/docker-resources.jsonl
```

1 行 1 リソース（`{type, id}` 形式）で追記する。これは既存 GC が runtime ディレクトリ
単位で動く構造（`internal/orchestrator/repository.go` の `cleanRuntimes`）と整合し、
daemon 再起動後も記録から掃除できる。**複数ユーザ環境でも「自分が記録したリソース」
だけを消す**ため、同一マシン上の他ユーザの docker コンテナを巻き込まない。

### 掃除: 同期 + safety net

- **同期掃除（job 完了時）**: 既存の `runner.cleanupSandboxAfterWait()`
  （`internal/dispatcher/runner.go`）に、記録ファイルを読んで stop + rm する処理を追加。
  削除順序は **コンテナ → ネットワーク → ボリューム**（依存順）。
- **safety net（daemon GC loop）**: 既存の GC loop が runtime ディレクトリを削除する前に、
  記録ファイルのリソースを掃除する。daemon クラッシュ等で同期掃除を取りこぼした
  孤児リソースを定期的に回収する。

### 失敗時も消す

既存挙動では job 失敗時に runtime ディレクトリを診断用に残すが、**コンテナは
成功・失敗を問わず stop + rm する**。rootless docker のディスク・メモリを
じわじわ食うのを避けるため。診断が要る場合はコンテナのログで取得する。

---

## 防御の多層化

プロキシの bodyPolicy が第一防衛線だが、
万が一プロキシを迂回された場合に被害を限定するため、補完的な対策を推奨する。

| 対策 | 種別 | 依存にするか |
|---|---|---|
| rootless Docker / rootless Podman の利用 | ランタイム | 推奨だが依存にしない |
| daemon 設定 `no-new-privileges: true` | ランタイム | 推奨だが依存にしない |
| seccomp / AppArmor プロファイル | ランタイム | 推奨だが依存にしない |
| pasta ネットワーク分離 (既存) | サンドボックス | 既存の仕組みに委ねる |

「依存にしない」理由: 依存にすると課題 1 (セットアップ負担) が再発する。
プロキシが動けばすぐ使える、というゴールを損なわない範囲で推奨に留める。
README での complementary mitigation 案内は引き続き行う。

---

## テスト戦略

**検証は docker daemon 非依存を基本とする。** proxy のポリシー判定・転送・GC 記録は
すべて mock upstream（固定レスポンスを返す Docker API 互換の fake unix socket）で検証でき、
本物の docker daemon を必要としない。クライアントは docker CLI ではなく **curl で Docker API を
直接叩く**ことで CLI 非依存・再現可能にする。実 daemon が要るのは実コンテナを立てる結合
テストのみで、これは optional とする（後述）。

### 単体テスト (ポリシー判定ロジック・依存なし)

`internal/sandbox/dockerproxy/policy_test.go` として、ポリシー判定を純粋関数で検証:

- `HostConfig.Binds` に bind 指定（パス不問）→ deny
- `HostConfig.Mounts` (Type=bind, パス不問) → deny
- `HostConfig.Mounts` (Type=volume, DriverConfig なし) → allow
- `HostConfig.Mounts` (Type=volume, local driver `device=/etc,o=bind`) → **deny**（系統 3）
- `Privileged=true` → deny
- `NetworkMode="host"` → deny
- `PidMode="host"` → deny
- `IpcMode="host"` → deny
- `UsernsMode="host"` / `CgroupnsMode="host"` → deny
- `SecurityOpt=["seccomp=unconfined"]` → deny
- `SecurityOpt=["no-new-privileges=false"]` → deny
- `CapAdd` に危険な capability (`SYS_ADMIN`) → deny
- `CapAdd` が空 → allow
- `Devices=["/dev/sda:/dev/sda"]` → deny
- `Runtime="sysbox-runc"`（allowlist 外）→ deny
- `POST /containers/{id}/exec` で `Privileged=true` → deny
- `POST /build`（legacy builder）→ deny
- `POST /session`（BuildKit gRPC トンネル）→ deny
- 未知の mutating エンドポイント（例 `POST /some/new/api`）→ **deny（fail-closed）**
- `MaxBodyBytes` 超過のボディ → deny
- parser differential: 重複 `HostConfig` キーや大文字小文字を変えた攻撃ボディ → 見落とさない

### 統合テスト (proxy 本体 × mock upstream・docker 不要)

`internal/sandbox/dockerproxy/` の Go テストで、proxy を起動し mock upstream
（固定レスポンスを返す Docker API 互換の fake unix socket）に繋いで検証:

- deny 系: 危険リクエストを proxy に送ると 403 が返り、**mock upstream には到達しない**
- transfer 系: 正当リクエストが mock upstream へ転送され、応答がそのまま返る
- ID 記録: `POST /containers/create` の mock 応答（固定 `Id`）から ID を拾い、記録ファイルへ追記される
- GC: job 完了相当で、記録ファイルのリソースに対し mock upstream へ stop/rm が発行される
- 生ボディ転送: リクエストボディが改変されず upstream に届く

### E2E テスト (サンドボックス統合 × mock upstream・docker 不要)

サンドボックスに proxy socket が bind され、`DOCKER_HOST` 経由で **curl が通る**ことを
mock upstream に対して検証する敵対的シナリオ:

- `docker-proxy-bind-escape`: `-v /etc:/etc` で bind mount 脱出を試みる
- `docker-proxy-mount-escape`: `--mount type=bind,src=/etc,dst=/etc` で同様の脱出を試みる
- `docker-proxy-volume-bind-escape`: `--mount type=volume,volume-opt=device=/etc,volume-opt=o=bind`
  で volume driver 経由の脱出を試みる（系統 3）
- `docker-proxy-privileged`: `--privileged` でコンテナを起動しようとする
- `docker-proxy-host-network`: `--network host` で起動しようとする
- `docker-proxy-security-opt`: `--security-opt seccomp=unconfined` を試みる
- `docker-proxy-capadd`: `--cap-add SYS_ADMIN` を試みる
- `docker-proxy-device`: `--device /dev/sda` を試みる
- `docker-proxy-build-denied`: `POST /build` / `POST /session` が拒否される（403）
- `docker-proxy-reap-on-success`: job 正常完了で、記録したコンテナへ stop/rm が発行される
- `docker-proxy-reap-on-failure`: job 失敗（exit≠0）でも、同様に stop/rm が発行される
- `docker-proxy-passthrough`: 通常の API（`/containers/json`, `/version` 等）が転送される

### 実 docker/podman 結合テスト (optional・実 daemon がある環境のみ)

mock では確認できない「実際にコンテナが立つ」経路だけは本物の daemon が要る。
**proxy は upstream が docker か podman かを区別しない**ため、podman の docker 互換 socket
（`podman system service`）でも代替できる。実 daemon が無い環境（本リポジトリの開発機は
podman remote client、CI も既定では docker/podman 無し）では **skip** する。

- `docker-proxy-testcontainers`: 実 daemon に対し TestContainers が Ryuk 無効化込みで完走する

CI で実結合まで回す場合は、ジョブに rootless docker か `podman system service` の
セットアップを追加し、上流 socket の動的解決でそのパスを拾わせる。

---

## 段階実装プラン

> **方針: 危険項目の検査は分割しない。** cetusguard → native proxy への
> **デフォルト切替は、コンテナ作成系の危険フィールドを全て塞いでから** 行う。
> 「ネイティブ proxy に切り替わった ＝ 検査されている」とユーザが信じる以上、
> 切替時点で検査がザルだと cetusguard 時代より危険な誤認を生むため。

### Phase 1 — 透過プロキシ + /containers/create フル検査 + 安全な切替

- `internal/sandbox/dockerproxy/` パッケージを新設
- Unix ソケット → Unix ソケットの透過転送プロキシ実装
- **fail-closed ルーティング**: GET/HEAD は透過、未知の mutating は既定 deny
- 上流 docker socket の **動的解決**（決め打ち禁止）
- 生ボディ転送 + `MaxBodyBytes` 上限（parser differential / DoS 対策）
- `POST /containers/create` の **フルボディ検査**:
  Binds / Mounts（系統 1/2/3）/ Privileged / NetworkMode / PidMode / IpcMode /
  UsernsMode / CgroupnsMode / **SecurityOpt** / **CapAdd** / **Devices** /
  DeviceCgroupRules / Runtime / Sysctls / CgroupParent
- `POST /containers/{id}/exec` の `Privileged` 検査
- `POST /containers/{id}/start` の HostConfig 付き start を deny（or API バージョン下限固定）
- **image build を deny**（`POST /build` / `POST /session`。BuildKit gRPC は検査不能のため fail-closed）
- daemon 側に proxy 管理ロジック追加 (起動・停止・socket パス管理・パーミッション)
- `sandbox_builder.go` に socket bind-mount と環境変数設定を追加
  （`DOCKER_HOST` 等 + **`TESTCONTAINERS_RYUK_DISABLED=true`**）
- **TestContainers (Ryuk) 互換性** を担保（`TESTCONTAINERS_RYUK_DISABLED=true` で Ryuk 無効化）
- **コンテナ GC（Ryuk の内製化）**: 作成系のレスポンスから ID を拾い
  `runtimes/<runtime_id>/docker-resources.jsonl` に記録、`cleanupSandboxAfterWait()` で
  同期掃除（成功・失敗とも消す）+ daemon GC loop で孤児リソースを回収
- 単体テスト + E2E (`docker-proxy-bind-escape` / `-volume-bind-escape` /
  `-privileged` / `-host-network` / `-security-opt` / `-capadd` / `-device` /
  `-testcontainers` / `-reap-on-success` / `-reap-on-failure` / `-passthrough`)
- ✅ ここまで揃ってから docker kit を廃止し、`capabilities.docker` による
  native proxy 有効化へ切り替える（cetusguard 依存を除去）

### Phase 2 — 残りエンドポイントのボディ検査

- `POST /networks/create`, `POST /volumes/create`（系統 3 と整合）の検査追加
- `AllowedRegistries` 実装（`POST /images/create` / pull）。初期段階は pull に registry
  制限を設けない（pull はイメージ取得のみで分離を破らないため）。エアギャップ等の
  需要が出た場合のみ追加する需要ドリブン拡張
- E2E シナリオを networks / volumes / registry に拡張

> image build（`POST /build` / `POST /session`）は Phase 1 で deny 確定のため、
> Phase 2 でも検査対象に追加しない（BuildKit gRPC の検査実装は行わない）。

### Phase 3 — docker kit 廃止・cetusguard 廃止

- boid-kits から docker kit を削除（socket bind + env は daemon へ移管済み）
- `capabilities.docker` での有効化方法をドキュメント化
- 既存ユーザの移行ガイド（`kits: [docker]` → `capabilities: docker`、cetusguard 撤去手順）
- docker CLI は proxy socket 経由で使う旨を明記（host_commands への docker 登録は禁止）
- rootless Docker の推奨手順をドキュメントに追記
