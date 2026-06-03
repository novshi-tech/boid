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
- `HostConfig.Privileged = true`
- `HostConfig.NetworkMode = "host"` / `PidMode = "host"` / `IpcMode = "host"`
- `HostConfig.CapAdd` の危険な capability
- `HostConfig.Devices` によるデバイスアクセス

これらを悪用すれば、サンドボックスのファイルシステム分離・ネットワーク分離を
コンテナ経由でバイパスできる。

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
それ以外は透過転送で十分であり、検査範囲は有界である。

### 検査必須エンドポイント

| エンドポイント | 危険な設定 |
|---|---|
| `POST /containers/create` | HostConfig (Binds, Mounts, Privileged, NetworkMode, PidMode, IpcMode, CapAdd, Devices 等) |
| `POST /networks/create` | HostNetworkMode 等の危険なドライバオプション |
| `POST /volumes/create` | DriverOpts に host パスを指定する外部ドライバ悪用 |
| `POST /build` (image build) | BuildKit の `--allow` / secret オプション |
| `POST /images/create` (pull) | 必要なら registry allowlist |

### 透過転送でよいエンドポイント (代表例)

- `GET /containers/{id}/logs`, `/containers/{id}/attach`
- `POST /containers/{id}/start`, `/{id}/stop`, `/{id}/wait`
- `POST /exec/{id}/start` (exec に危険オプションは少ない)
- `GET /info`, `GET /version`, `GET /_ping`
- `GET /images/...`, `DELETE /containers/...`

---

## ポリシースキーマ設計

### Go 型定義 (案)

```go
// DockerPolicy は docker kit のポリシー設定を表す。
// project.yaml の capabilities/docker に記述する。
type DockerPolicy struct {
    // AllowedBindPaths: HostConfig.Binds / Mounts で許可する host パスのプレフィックスリスト。
    // 空の場合は bind mount を全て deny する。
    // "$WORKTREE" はタスク worktree パスに展開される特殊値。
    AllowedBindPaths []string `yaml:"allowed_bind_paths"`

    // AllowPrivileged: true にすると Privileged=true を許可する。既定 false。
    AllowPrivileged bool `yaml:"allow_privileged"`

    // AllowHostNetwork: true にすると NetworkMode=host を許可する。既定 false。
    AllowHostNetwork bool `yaml:"allow_host_network"`

    // AllowedCapAdd: 許可する Linux capability のホワイトリスト。空は全 deny。
    AllowedCapAdd []string `yaml:"allowed_cap_add"`

    // AllowedRegistries: docker pull / FROM で許可するレジストリのプレフィックスリスト。
    // 空は制限なし（将来拡張）。
    AllowedRegistries []string `yaml:"allowed_registries"`
}
```

### project.yaml での表現 (案)

```yaml
capabilities:
  docker:
    allowed_bind_paths:
      - $WORKTREE   # タスク worktree 配下のみ bind 許可
    allow_privileged: false
    allow_host_network: false
    allowed_cap_add: []  # capability 追加は全て deny
```

`host_commands` と並ぶ `capabilities` セクションとして定義する。
kit が有効化されたとき、sandbox_builder がポリシーを読み込んで DockerProxy に渡す。

---

## bind mount の 2 系統を両方カバーする

Docker API には bind mount を指定するエンドポイント表現が **2 系統** ある。
片方だけ塞いでも残りから抜けられるため、両方を必ず検査する。

**系統 1: `HostConfig.Binds`**  
文字列配列。`-v` 相当の簡易記法。

```json
"Binds": ["/host/path:/container/path:ro"]
```

検査方法: 各要素を `:` で分割し先頭トークン (host パス) を `AllowedBindPaths` と照合。

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

検査方法: `Type == "bind"` の要素の `Source` フィールドを `AllowedBindPaths` と照合。
`Type` が `volume` / `tmpfs` / `npipe` の場合は bind ではないのでスキップ。

---

## 透過転送で丁寧に扱う必要があるもの

### API バージョンプレフィックス

Docker Engine API は `/v1.43/containers/create` のようにバージョンプレフィックスを持つ。
ルーティング時はプレフィックスを除去してパスを照合し、上流への転送時は元の URL をそのまま使う。
正規表現: `^/v\d+\.\d+(/.*)?$`

### exec / attach / logs のストリーミング (HTTP hijack)

`POST /exec/{id}/start` や `GET /containers/{id}/attach` は、
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

---

## サンドボックス統合と kit 移行手順

### 統合点 (現 docker kit との互換性)

現 docker kit はサンドボックスに cetusguard ソケットを bind-mount し、
`DOCKER_HOST` を差し替えることで機能している。ネイティブプロキシも
「daemon が立てた proxy socket を bind-mount し `DOCKER_HOST` を向ける」だけなので、
**統合点は現在と全く同じ** であり、kit 側の大規模改修は不要。

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

- `HostConfig.Binds` でワークツリー外パスを指定 → deny
- `HostConfig.Binds` でワークツリー内パスを指定 → allow
- `HostConfig.Mounts` (Type=bind) でワークツリー外パスを指定 → deny
- `HostConfig.Mounts` (Type=bind) でワークツリー内パスを指定 → allow
- `HostConfig.Mounts` (Type=volume) → bind 検査をスキップ → allow
- `Privileged=true` → deny
- `NetworkMode="host"` → deny
- `PidMode="host"` → deny
- `IpcMode="host"` → deny
- `CapAdd` に危険な capability → deny
- `CapAdd` が空 → allow

### E2E テスト (e2e/scenarios/ 配下)

敵対的シナリオとして:

- `docker-proxy-bind-escape`: `-v /etc:/etc` で bind mount 脱出を試みる
- `docker-proxy-mount-escape`: `--mount type=bind,src=/etc,dst=/etc` で同様の脱出を試みる
- `docker-proxy-privileged`: `--privileged` でコンテナを起動しようとする
- `docker-proxy-host-network`: `--network host` で起動しようとする
- `docker-proxy-passthrough`: 通常の `docker run`, `docker ps`, `docker logs` が正常動作する

---

## 段階実装プラン

### Phase 1 — 透過プロキシ + /containers/create ボディ検査

- `internal/sandbox/dockerproxy/` パッケージを新設
- Unix ソケット → Unix ソケットの透過転送プロキシ実装
- `POST /containers/create` のみボディ検査 (Binds / Mounts / Privileged / NetworkMode / PidMode / IpcMode)
- daemon 側に proxy 管理ロジック追加 (起動・停止・socket パス管理)
- `sandbox_builder.go` に socket bind-mount と環境変数設定を追加
- docker kit の `additional_bindings` を cetusguard → native proxy に切り替え
- 単体テスト + `docker-proxy-bind-escape` / `docker-proxy-passthrough` E2E

### Phase 2 — 残りエンドポイントのボディ検査

- `POST /networks/create`, `POST /volumes/create`, `POST /build` (URL パラメータ) の検査追加
- `CapAdd` / `Devices` ポリシー追加
- `AllowedRegistries` 実装 (必要に応じて)
- E2E シナリオを privileged / host-network / CapAdd に拡張

### Phase 3 — kit 移行・cetusguard 廃止

- docker kit README から cetusguard 前提を削除
- セットアップドキュメント更新
- cetusguard 関連の設定ガイドを「移行済みなら不要」に書き換え
- rootless Docker の推奨手順を README に追記

---

## オープンクエスチョン

1. **`allowed_bind_paths` のデフォルト値**: 空 (全 deny) か `$WORKTREE` か。
   利便性と安全性のトレードオフ。推奨は `[$WORKTREE]` (タスク worktree 内のみ許可)。

2. **`POST /images/create` (docker pull) の registry allowlist**:
   Phase 1 では実装しない方針だが、エアギャップ環境ではニーズがある。
   Phase 2 以降で `AllowedRegistries` として追加するか検討。

3. **BuildKit gRPC API (`/session` endpoint)**:
   BuildKit は gRPC ストリームを使う場合がある。HTTP/2 の扱いを確認する必要がある。
   Phase 1 では BuildKit を使わない通常 build のみ対応し、Phase 2 以降で拡張。

4. **複数タスクが同時に docker kit を使う場合**:
   各サンドボックスが独立した proxy socket を持つ設計のため競合は起きないはずだが、
   実 daemon が同一 (rootless docker は uid 単位) なので、upstream socket のパスが
   固定か動的かを確認する。
