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

> **注意:** `capabilities` と `host_commands` はもう `project.yaml` のフィールドではありません。 現行スキーマではどちらもロード時に reject されます — machine-local な実行環境は **workspace** に設定します (`boid workspace create/edit/import`)。 まだ project.yaml がこれらのフィールドを持ったままの旧スキーマの場合は、 先に `boid project migrate <dir> --apply` で workspace へ変換してください ([移行ガイド](migration.md) 参照)。

### 1. workspace の更新

docker kit への参照を workspace の `kits:` から外し、 `capabilities.docker` を workspace に直接追加します。 まず現在の中身を確認します:

```bash
boid workspace export <slug> > ws.yaml
```

> **注意 (Phase 2.5 PR7):** `WorkspaceMeta.Kits` フィールドはコードから完全撤去されているため、 `boid workspace export` の出力に `kits:` が含まれることはもう無く (DB backed workspace はそもそも `kits` カラムを持ったことがありません)、 `boid workspace edit --from-file` に `kits:` を含む body を渡すと reject されます。 以下の「変更前」は旧い (未移行の) workspace shadow yaml を手元で編集する場合の例示です — 実際の手順は「変更後」の内容 (`kits:` を含まない) を `--from-file` に渡すだけなので、 この移行ガイド自体の操作手順は PR7 の影響を受けません。

**変更前 (`ws.yaml`、 docker kit がまだ `kits:` に入っている状態):**

```yaml
kits:
  - docker   # ← legacy kit 参照。削除する

env:
  ...
```

**変更後:**

```yaml
capabilities:
  docker: {}   # ← 追加

env:
  ...
```

反映します:

```bash
boid workspace edit <slug> --from-file ws.yaml
```

| 旧 (`project.yaml`、 撤去済み) | 新 (workspace) |
|---|---|
| `kits: [..., docker]` (docker kit を project トップで参照) | workspace の `kits:` から docker kit 名を外し、 `capabilities: { docker: {} }` を直接設定 |
| `capabilities.docker: {}` (project.yaml トップレベル) | `capabilities.docker: {}` (workspace) — 形は同じ、置き場所だけ変わった |

### 2. `host_commands` の確認

`capabilities.docker` が有効な workspace で `host_commands` に `docker` をサブコマンド制限なしで登録していると、ジョブ起動時にエラーになります。

```
host_commands.docker: unrestricted docker access bypasses the docker proxy
(capabilities.docker is enabled); remove docker from host_commands or restrict
to specific subcommands (e.g. allow: [build])
```

`host_commands` は二層構造です — workspace が持つのは参照 **名前** のリストだけで、実際の定義 (`allow`/`deny`/`path` 等) は daemon 側の集約レジストリ `~/.config/boid/host_commands.yaml` にあります。 workspace の `host_commands` に `docker` という名前が入っていて、 レジストリ側のその定義がサブコマンド制限なしの場合は次のいずれかの対応をしてください:

- **削除する（推奨）**: proxy socket 経由で docker CLI / SDK / TestContainers が使えるため、`host_commands` への `docker` 登録は通常不要です。 workspace の `host_commands` リストから `docker` を外します
- **サブコマンドを制限する**: image build 等どうしてもホスト直実行が必要な場合は、 daemon 側レジストリの定義を `allow: [build]` のように制限します

```yaml
# ~/.config/boid/host_commands.yaml
host_commands:
  docker:
    allow: [build]   # build サブコマンドのみ許可 (image build 用)
```

```bash
boid host-commands reload
```

| 旧 (`project.yaml`、 撤去済み) | 新 |
|---|---|
| `host_commands.docker: { allow: [...] }` (project.yaml トップレベル) | workspace の `host_commands: [docker]` (参照名) + daemon 側の `~/.config/boid/host_commands.yaml` に `docker: { allow: [...] }` の実定義 |

詳細は [オンボーディング / host_commands を定義する](onboarding.md#host_commands-を定義する-daemon-側の集約レジストリ) を参照してください。

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
