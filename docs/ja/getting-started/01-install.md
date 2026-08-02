# 1. インストール

このページで `boid` をマシンに導入し、起動を確認します。所要時間は 5 分ほどです。

## 前提条件

- **Linux**。 `boid` は Linux 専用です。macOS / Windows はサポートしていません
- **Go 1.24 以上**。CLI のインストールは `go install` 経由で行います
- **`$GOBIN` (または `$GOPATH/bin`) が `PATH` に通っていること**。 `go env GOBIN` の出力 (空なら `$HOME/go/bin`) が `PATH` に含まれているか確認してください
- **docker (+ compose plugin) または podman (+ podman-compose)**。 daemon は `docker compose` / `podman-compose` でコンテナとして動きます (`build/container/compose.yml`)。 サンドボックス実行も docker-out-of-docker 方式で同じ engine を使うため、engine のソケットに到達できることが必須です

## インストール

```bash
go install github.com/novshi-tech/boid@latest
```

バイナリが使えることを確認します:

```bash
boid --help
```

サブコマンド (`start`, `check`, `task`, `job`, `project`, `workspace`, `web`, `agent`, `secret`, `gc`, `stop`, ...) の一覧が表示されれば OK です。

## 前提チェック

```bash
boid check
```

engine (docker/podman) の到達性、compose plugin の有無、docker-out-of-docker の bind ソース socket、`podman.socket` の起動状態、daemon を起動する uid、ホスト arch と image arch の一致などをまとめて確認します。 何か問題が出た場合は、その項目を解消してから次に進んでください (よくあるのは compose plugin 未導入、または docker/podman ソケットに到達できないケースです)。

## daemon の起動

`boid` は CLI と daemon (タスクの永続化・実行・観測を担うプロセス) が分離しています。 daemon は **compose スタックとして動く 1 択の構成**です — `docker compose up -d` / `podman-compose up -d` 相当を `boid` CLI 自身が代行します (`build/container/compose.yml`、host mode)。

```bash
boid start
```

初回はこの CLI バイナリのバージョンに対応する image (`ghcr.io/novshi-tech/boid-runner:<version>`、公開 GHCR image なので `docker login` は不要) を pull し、compose スタックを起動し、daemon が health check に応答するまで待ちます。 チェックアウトが手元にあり `BOID_COMPOSE_ROOT` を設定している場合 (開発者向け) は代わりにローカルビルドが走ります — 詳細は [CLI リファレンス / Host mode](../reference/cli.md) を参照してください。

起動が終わると、次にやるべきことのガイダンスが表示されます:

```
boid server started (compose, cli: http://127.0.0.1:8442)

Next steps:
  1. boid web pair                                      # Web UI をこのブラウザとペアリング
  2. boid project add <git-url> --workspace=<name>      # プロジェクトを登録 (新規なら先に `boid project init`)
  3. boid workspace set-init-script <name> -f init.sh   # ツールの自動インストールスクリプトを登録
  4. boid agent claude -p <project>                     # 対話セッションでサインイン
```

daemon が止まっている状態でそれを必要とするコマンド (例: `boid task list`) を呼ぶと自動的に起動するため、 `boid start` を毎回打たなくても構いません (`BOID_NO_AUTOSTART=1` で無効化可能)。

## Web UI をペアリングする

compose daemon では、コンテナ自身から見た loopback (127.0.0.1) とホストから見た `http://localhost:8080` は別物のため、従来 (bare-host 時代) にあった「loopback からはペアリング不要」という例外は効きません。必ず一度ペアリングしてください:

```bash
boid web pair
```

表示されたコード / URL / QR のいずれかでブラウザから認証すると、`http://localhost:8080` (既定ポート、変更は `boid web set-addr <addr>`) で Web UI にアクセスできるようになります。

## 動作確認

```bash
boid task list
```

新規インストール直後はリストが空であることが正常です。

## サーバの停止

```bash
boid stop
```

`docker/podman compose down` 相当で compose スタックを停止します。コンテナを直接 `docker kill` する等の操作ではなく、必ず `boid stop` を使ってください。

## データの保存先

daemon の永続データ (SQLite DB、kits、secret 鍵、web 署名鍵、ログ等) は、すべて `boid_state` という named volume (`build/container/compose.yml`) の中にあります。**host 側の `~/.local/share/boid/` のような XDG パスは daemon からは見えません** — bind mount ではなく named volume を使っているのは、rootless podman の uid マッピングを host filesystem 越しに扱う複雑さを避けるためです。

host 側 (`boid` CLI を実行しているこのマシン) に実際に作られるファイルは、compose daemon のライフサイクル管理用の小さなものだけです:

| パス | 内容 |
|---|---|
| `~/.config/boid/cli-token` | CLI ↔ daemon container 間の共有シークレット (`boid start` 初回に自動生成、パーミッション 0600) |
| `~/.local/state/boid/compose/` | チェックアウトが見つからない場合に埋め込み `compose.yml`/`Dockerfile`/`deploy-container.sh` を展開する先、および CLI 間の排他ロックファイル |

daemon が起動 10 秒後に開始する GC ループは、以降 24 時間ごとに繰り返し、30 日より古いデータを複数のスコープにわたって削除します: `runtimes/<runtime_id>/` ディレクトリ、`/tmp/boid-*` 一時ファイル、DB 上の terminal タスク・アクション・ジョブレコード、失効済みデバイスのエントリが対象です。手動実行は `boid gc` で行えます。

## 更新

`@latest` で `go install` し直し、daemon を再起動します:

```bash
go install github.com/novshi-tech/boid@latest
boid stop
boid start
```

CLI のバージョンが上がると `boid start` が pull する image ref (`internal/version.DefaultContainerImage()`) も追従して変わるので、`boid stop && boid start` だけで CLI と daemon の image が揃った状態に更新されます。 バージョン間で DB マイグレーションが必要な場合は起動時にガイダンスが表示されます (詳細は [移行ガイド](../guide/migration.md))。

## アンインストール

```bash
boid stop
docker volume rm boid_state    # podman の場合: podman volume rm boid_state
rm -rf ~/.config/boid ~/.local/state/boid/compose
rm "$(go env GOPATH)/bin/boid"
```

`docker/podman volume rm boid_state` でタスク・機密値・インストール済みの拡張パッケージを含むローカルデータがすべて消えます。再インストール時にデータを残したい場合はこのコマンドを省いてください。

---

次: [2. プロジェクトを初期化する](02-init-project.md)
