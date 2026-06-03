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
| `POST /build` (image build) | URL パラメータ `networkmode=host` / BuildKit の `--allow` / secret |
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

1. **既定で安全 (secure-by-default)**: project.yaml に docker の設定を一切書かなくても、
   サンドボックスのセキュリティモデルを壊しうる機能は全て deny される。
   kit を有効化するだけで安全な既定が効き、プロジェクトごとの設定作業は要らない。
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
- 危険な SecurityOpt (seccomp/apparmor unconfined, no-new-privileges 無効化 等)
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
# docker セクションの記述は不要。kit を有効化するだけで安全な既定が効く。
capabilities:
  docker: {}   # セクション自体を省略してもよい
```

`host_commands` と並ぶ `capabilities` セクションとして将来の拡張余地は残すが、
初期段階で書くべき docker 設定は無い。kit が有効化されたとき、sandbox_builder が
（設定なしでも）安全既定を DockerProxy に適用する。

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

### image build の tar コンテキストストリーム

`POST /build` は multipart ではなく tar ストリームをリクエストボディで受け取る。
ボディサイズが大きくなりうるため、bodyPolicy 評価時はストリームをバッファリングしない。
`/build` エンドポイントは URL パラメータ (`dockerfile`, `buildargs` 等) で設定を受け取るため、
ボディを読まずにパラメータのみを検査する方針を採る (ボディは素通し)。

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
- JSON デコードは Docker daemon と同じ Go の `encoding/json` を使い、
  検査したいフィールドを持つ struct（または `map[string]json.RawMessage`）へ
  デコードする。未知フィールドは無視されるが、**HostConfig 全体を struct で
  受ける場合は、検査漏れフィールドが「無検査で通る」ことになるため、
  既知フィールドのみ allowlist で許可し、未知キーが来たら deny する** 設計
  （fail-closed）も検討する。

### 上流 Docker socket の動的解決（決め打ち禁止）

上流ソケットを `/run/user/<uid>/docker.sock` で決め打ちすると、環境差で壊れる。
daemon 起動時に以下の順で解決する:

1. ユーザ設定（config.yaml の明示指定）があればそれ
2. `DOCKER_HOST` 環境変数（`unix://` パス。TCP の場合は別途扱う / 非対応なら明示エラー）
3. rootless: `$XDG_RUNTIME_DIR/docker.sock` → `/run/user/<uid>/docker.sock`
4. rootful: `/var/run/docker.sock`

解決できなければ起動時に明示エラーとし、サイレントに誤ったソケットへ繋がない。

---

## サンドボックス統合と kit 移行手順

### 統合点 (現 docker kit との互換性)

現 docker kit はサンドボックスに cetusguard ソケットを bind-mount し、
`DOCKER_HOST` を差し替えることで機能している。ネイティブプロキシも
「daemon が立てた proxy socket を bind-mount し `DOCKER_HOST` を向ける」だけなので、
**統合点は現在と全く同じ** であり、kit 側の大規模改修は不要。

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

### proxy socket のアクセス制御

- proxy socket はサンドボックスごとに 1 本（socket per sandbox）とし、所有者を
  サンドボックス実行 uid、パーミッションを `0600`（または所有 uid のみアクセス可）
  に設定する。
- bind mount で当該サンドボックスにのみ可視化し、他サンドボックスからは到達不能にする。
- proxy → 上流 docker.sock の接続は daemon の権限で行い、サンドボックス内プロセスが
  上流 socket を直接握れないことを保証する。

### 移行手順

1. boid daemon が `DockerProxy` を起動し、proxy socket パスを生成
2. `sandbox_builder.go` が socket を bind-mount、環境変数を設定
3. docker kit の `additional_bindings` から cetusguard ソケットの記述を削除
4. `requires.commands: docker` の扱いは変えない (docker CLI は引き続き必要)
5. cetusguard の systemd unit は boid が管理しなくなるため、ユーザが手動で停止可能

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

### 単体テスト (内部ポリシー評価)

`internal/sandbox/dockerproxy/policy_test.go` として:

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
- 未知の mutating エンドポイント（例 `POST /some/new/api`）→ **deny（fail-closed）**
- `MaxBodyBytes` 超過のボディ → deny
- parser differential: 重複 `HostConfig` キーや大文字小文字を変えた攻撃ボディ → 見落とさない

### E2E テスト (e2e/scenarios/ 配下)

敵対的シナリオとして:

- `docker-proxy-bind-escape`: `-v /etc:/etc` で bind mount 脱出を試みる
- `docker-proxy-mount-escape`: `--mount type=bind,src=/etc,dst=/etc` で同様の脱出を試みる
- `docker-proxy-volume-bind-escape`: `--mount type=volume,volume-opt=device=/etc,volume-opt=o=bind`
  で volume driver 経由の脱出を試みる（系統 3）
- `docker-proxy-privileged`: `--privileged` でコンテナを起動しようとする
- `docker-proxy-host-network`: `--network host` で起動しようとする
- `docker-proxy-security-opt`: `--security-opt seccomp=unconfined` を試みる
- `docker-proxy-capadd`: `--cap-add SYS_ADMIN` を試みる
- `docker-proxy-device`: `--device /dev/sda` を試みる
- `docker-proxy-testcontainers`: TestContainers ベースのテストが Ryuk 無効化込みで正常完走する
- `docker-proxy-passthrough`: 通常の `docker run`, `docker ps`, `docker logs` が正常動作する

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
- daemon 側に proxy 管理ロジック追加 (起動・停止・socket パス管理・パーミッション)
- `sandbox_builder.go` に socket bind-mount と環境変数設定を追加
  （`DOCKER_HOST` 等 + **`TESTCONTAINERS_RYUK_DISABLED=true`**）
- **TestContainers (Ryuk) 互換性** を担保（`TESTCONTAINERS_RYUK_DISABLED=true` で Ryuk 無効化）
- 単体テスト + E2E (`docker-proxy-bind-escape` / `-volume-bind-escape` /
  `-privileged` / `-host-network` / `-security-opt` / `-capadd` / `-device` /
  `-testcontainers` / `-passthrough`)
- ✅ ここまで揃ってから docker kit の `additional_bindings` を
  cetusguard → native proxy に切り替える

### Phase 2 — 残りエンドポイントのボディ検査

- `POST /networks/create`, `POST /volumes/create`（系統 3 と整合）,
  `POST /build`（URL パラメータ `networkmode=host` 等）の検査追加
- `AllowedRegistries` 実装 (`POST /images/create` / build の FROM、必要に応じて)
- BuildKit (`/session` gRPC / HTTP2) 対応の検討
- E2E シナリオを networks / volumes / build / registry に拡張

### Phase 3 — kit 移行完了・cetusguard 廃止

- docker kit README から cetusguard 前提を削除
- セットアップドキュメント更新
- cetusguard 関連の設定ガイドを「移行済みなら不要」に書き換え
- rootless Docker の推奨手順を README に追記

---

## オープンクエスチョン

1. **bind mount の扱い** — 決着済み: 初期段階では `allowed_bind_paths` を設けず、
   **bind は一律 deny**。Ryuk の docker.sock bind は `TESTCONTAINERS_RYUK_DISABLED=true`
   で回避する。将来 bind 許可の実需要が出たら、その時点で最小限の許可リストを追加検討する。

2. **`POST /images/create` (docker pull) の registry allowlist**:
   Phase 1 では実装しない方針だが、エアギャップ環境ではニーズがある。
   Phase 2 以降で `AllowedRegistries` として追加するか検討。

3. **BuildKit gRPC API (`/session` endpoint)**:
   BuildKit は gRPC ストリームを使う場合がある。HTTP/2 の扱いを確認する必要がある。
   Phase 1 では BuildKit を使わない通常 build のみ対応し、Phase 2 以降で拡張。

4. **複数タスクが同時に docker kit を使う場合**:
   各サンドボックスが独立した proxy socket を持つ設計のため競合は起きないはず。
   upstream socket は uid 単位で共有されるが、**動的解決を Phase 1 必須項目に
   格上げ済み**（→「上流 Docker socket の動的解決」節）なのでこの懸念は解消。
   残課題は、同一 upstream への並行アクセス時の上限（同時接続数）程度。

5. **SecurityOpt 検査の粒度**:
   危険な値（`seccomp=unconfined` / `apparmor=unconfined` /
   `no-new-privileges=false` / `label=disable`）のみを deny するブラックリスト方式か、
   既知の安全な値以外を全て deny するホワイトリスト方式か。
   fail-closed の原則からはホワイトリストが望ましいが、互換性とのトレードオフ。

6. **HostConfig のデコード方針**:
   既知フィールドを持つ struct で受けると未知の危険フィールドが無検査で通る恐れがある。
   `map[string]json.RawMessage` で全キーを把握し、**既知の安全キー以外が含まれたら
   deny** する fail-closed 方式を採るか。Docker API のフィールド追加への追従コストと、
   過剰 deny による互換性低下のバランスを決める。
