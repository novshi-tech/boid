# 1. インストール

このページで `boid` をマシンに導入し、起動を確認します。所要時間は 2 分ほどです。

## 前提条件

- **Linux**。 `boid` は Linux 専用です。macOS / Windows はサポートしていません
- **Go 1.24 以上**。インストールは `go install` 経由で行います
- **`$GOBIN` (または `$GOPATH/bin`) が `PATH` に通っていること**。 `go env GOBIN` の出力 (空なら `$HOME/go/bin`) が `PATH` に含まれているか確認してください

## インストール

```bash
go install github.com/novshi-tech/boid@latest
```

バイナリが使えることを確認します:

```bash
boid --help
```

サブコマンド (`start`, `task`, `job`, `project`, `web`, `kit`, `secret`, `gc`, `stop`, ...) の一覧が表示されれば OK です。

## サーバ (daemon) の起動

`boid` は CLI と並行してバックグラウンドで動くサーバプロセス (以降 daemon と呼びます) を持ち、タスクの永続化・実行・観測はこの daemon が担います。

```bash
boid start
```

`boid start` は子プロセスとして daemon を起動して即座に戻ります。出力には PID・UNIX ソケットのパス・HTTP のリッスンアドレス (既定 `:8080`) が表示されます。 `nohup` や `&`、 systemd の user unit は不要です — `boid start` 自体がプロセスを切り離します。

daemon が止まっている状態でそれを必要とするコマンド (例: `boid task list`) を呼ぶと、自動的に起動するため、 `boid start` を毎回打たなくても構いません。

## 動作確認

```bash
boid task list
```

新規インストール直後はリストが空であることが正常です。

ブラウザで `http://localhost:8080` を開くと Web UI が表示されます。同一マシン (loopback アドレス 127.0.0.1 / ::1) からのアクセスはデバイス認証 (ペアリング) 不要で、そのままタスク一覧が見えるはずです。

## サーバの停止

```bash
boid stop
```

サーバを停止する正しい方法は `boid stop` です。PID 指定で kill すると socket ファイルが残ることがあります。

## ファイルの配置場所

`boid` は [XDG Base Directory 仕様](https://specifications.freedesktop.org/basedir-spec/basedir-spec-latest.html) に従います。標準的な XDG 環境での既定パスは以下です:

| パス | 内容 |
|---|---|
| `~/.local/share/boid/boid.db` | タスク・ジョブ・プロジェクト等を保存する SQLite データベース |
| `~/.local/share/boid/kits/` | インストール済みの拡張パッケージ (kit) のソースツリー |
| `~/.local/share/boid/runtimes/` | タスクごとに作業ディレクトリを切るための一時領域 (一定期間で自動削除) |
| `~/.local/share/boid/secret.key` | API キー等の機密値を暗号化するための鍵 (パーミッション 0600) |
| `~/.local/share/boid/web_secret` | Web UI のセッション cookie 署名鍵 (パーミッション 0600) |
| `~/.local/state/boid/boid.log` | daemon の標準出力・エラーを書き出すログ (サイズ上限でローテーション) |
| `~/.config/boid/config.yaml` | ユーザによる任意の設定上書き |
| `$XDG_RUNTIME_DIR/boid.sock` | CLI と daemon を繋ぐ UNIX ソケット ( `XDG_RUNTIME_DIR` が無い環境では `/tmp/boid-<uid>.sock` ) |

`~/.config/boid/config.yaml` は任意です。存在しない場合は既定値で動作します。

## 更新

`@latest` で `go install` し直し、daemon を再起動します:

```bash
go install github.com/novshi-tech/boid@latest
boid stop
boid start
```

再起動は必須です。実行中の daemon プロセスは古いバイナリのコードをメモリ上に保持し続けるため、 `boid stop` を省略するとディスク上のバイナリが新しくなっても挙動は変わりません。

## アンインストール

```bash
boid stop
rm -rf ~/.local/share/boid ~/.local/state/boid ~/.config/boid
rm "$(go env GOPATH)/bin/boid"
```

最初の `rm` でタスク・機密値・インストール済みの拡張パッケージを含むローカルデータがすべて消えます。再インストール時にデータを残したい場合は該当パスを除外してください。

---

次: [2. 最初のタスク](02-first-task.md)
