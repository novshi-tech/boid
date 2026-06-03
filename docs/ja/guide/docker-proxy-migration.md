# Docker プロキシ移行ガイド

docker kit (cetusguard ベース) から boid ネイティブ Docker プロキシ (`capabilities.docker`) への移行手順です。

## 背景

従来の docker kit は外部ツール [cetusguard](https://github.com/hectorm/cetusguard) に依存しており、ユーザがバイナリのインストール・ルールファイルの作成・systemd unit の有効化という前提作業を行う必要がありました。また cetusguard は HTTP メソッドと URL パスのみを照合し、**リクエストボディを検査しない**ため、`HostConfig.Privileged` / `HostConfig.Binds` / `HostConfig.NetworkMode=host` 等の危険な設定をブロックできませんでした。

boid ネイティブプロキシはこれらの問題を解消します:

- boid daemon が自動で起動・管理するため外部セットアップが不要
- リクエストボディを検査し、危険な `HostConfig` 設定を拒否する
- job 間の id スコープ検査でリソース操作を自 job 分のみに制限する
- TestContainers の Ryuk を自動無効化し、代わりに boid がコンテナを後始末する

**docker kit はまだ削除されていません。** しかし新規プロジェクトには native proxy の使用を推奨します。既存プロジェクトは本ガイドの手順で移行できます。

## 移行手順

### 1. `project.yaml` の更新

`kits` リストから docker kit を外し、`capabilities.docker` を追加します。

**変更前:**

```yaml
kits:
  - github.com/novshi-tech/boid-kits/claude-code
  - github.com/novshi-tech/boid-kits/docker   # ← 削除

task_behaviors:
  executor:
    ...
```

**変更後:**

```yaml
kits:
  - github.com/novshi-tech/boid-kits/claude-code

capabilities:
  docker: {}   # ← 追加

task_behaviors:
  executor:
    ...
```

変更後に `boid project reload` を実行して反映します。

### 2. `host_commands` の確認

`capabilities.docker` が有効なプロジェクトで `host_commands` に `docker` をサブコマンド制限なしで登録していると、ジョブ起動時にエラーになります。

```
host_commands.docker: unrestricted docker access bypasses the docker proxy
(capabilities.docker is enabled); remove docker from host_commands or restrict
to specific subcommands (e.g. allow: [build])
```

`host_commands` に `docker` が登録されている場合は次のいずれかの対応をしてください:

- **削除する（推奨）**: proxy socket 経由で docker CLI / SDK / TestContainers が使えるため、`host_commands` への `docker` 登録は通常不要です
- **サブコマンドを制限する**: image build 等どうしてもホスト直実行が必要な場合は `allow: [build]` のように制限します

```yaml
host_commands:
  docker:
    allow: [build]   # build サブコマンドのみ許可 (image build 用)
```

### 3. cetusguard の撤去

cetusguard は不要になります。次の手順で撤去してください。

**systemd user unit の停止と無効化:**

```sh
systemctl --user stop cetusguard.service
systemctl --user disable cetusguard.service
```

**unit ファイルの削除:**

```sh
rm ~/.config/systemd/user/cetusguard.service
systemctl --user daemon-reload
```

**ルールファイルの削除:**

```sh
rm -rf ~/.config/cetusguard/
```

**cetusguard バイナリの削除** (インストール先に合わせて):

```sh
# go install でインストールした場合
rm ~/go/bin/cetusguard

# ~/.local/bin にインストールした場合
rm ~/.local/bin/cetusguard
```

### 4. 動作確認

移行後にサンドボックスから Docker が使えるか確認します。`boid exec` でサンドボックスに入り、以下を実行してください:

```sh
# proxy 経由で Docker daemon に到達できるか確認
curl --unix-socket /run/boid/docker-proxy.sock http://d/_ping
# → "OK" が返れば正常

# または docker CLI で確認 (docker CLI がサンドボックス内に存在する場合)
docker info
```

## proxy 経由での docker CLI の使い方

サンドボックス内で docker CLI を使う場合、環境変数 `DOCKER_HOST` は boid が自動設定するため特別な設定は不要です。docker CLI バイナリがサンドボックス内の `PATH` にあれば、そのまま `docker ps` / `docker run` 等を実行できます。

```sh
# サンドボックス内で実行する場合 (DOCKER_HOST は自動設定済み)
docker ps
docker run --rm hello-world
```

TestContainers も `DOCKER_HOST` を自動で参照するため、コード変更なしに動作します。

## セキュリティに関する補足

boid proxy はリクエストボディを検査して危険な設定を拒否しますが、万一 proxy が迂回された場合の影響を限定するため、ホスト側 Docker daemon を **rootless** で動かすことを推奨します。

```sh
# rootless Docker のセットアップ
curl -fsSL https://get.docker.com/rootless | sh
# または
apt install docker-ce-rootless-extras   # Ubuntu/Debian
```

rootless Docker ではコンテナが user namespace 内で動くため、仮に proxy を迂回してコンテナが特権操作を試みても、host root へのエスカレーションが原理的に起きません。

proxy のセキュリティモデル全体は [サンドボックス内部実装 / Docker プロキシ](../architecture/sandbox-internals.md#docker-プロキシ-capabilitiesdocker) を参照してください。

## 関連ドキュメント

- [`project.yaml` リファレンス / capabilities.docker](../reference/project-yaml.md#capabilitiesdocker)
- [サンドボックス内部実装 / Docker プロキシ](../architecture/sandbox-internals.md#docker-プロキシ-capabilitiesdocker)
