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

## daemon の起動

```bash
boid start
```

`boid start` は detach した daemon を生成して即座に戻ります。出力には PID・UNIX socket パス・HTTP listen アドレス (既定 `:8080`) が表示されます。 `nohup` や `&`、 systemd の user unit は不要で、 `boid start` 自身が daemon 化します。

なお、daemon を必要とするコマンド (例: `boid task list`) を最初に呼んだ時点で自動起動するため、 `boid start` を省略していきなりコマンドを叩くこともできます。

## 動作確認

```bash
boid task list
```

新規インストール直後はリストが空であることが正常です。

ブラウザで `http://localhost:8080` を開くと Web UI が表示されます。loopback アクセスはペアリング不要なので、そのままタスク一覧が見えるはずです。

## daemon の停止

```bash
boid stop
```

サーバを停止する正しい方法は `boid stop` です。PID 指定で kill すると socket ファイルが残ることがあります。

## ファイルの配置場所

`boid` は [XDG Base Directory 仕様](https://specifications.freedesktop.org/basedir-spec/basedir-spec-latest.html) に従います。標準的な XDG 環境での既定パスは以下です:

| パス | 内容 |
|---|---|
| `~/.local/share/boid/boid.db` | SQLite データベース |
| `~/.local/share/boid/kits/` | インストール済みの kit |
| `~/.local/share/boid/runtimes/` | タスクごとの sandbox 実行用ディレクトリ (自動 GC) |
| `~/.local/share/boid/secret.key` | secret store の暗号鍵 (mode 0600) |
| `~/.local/share/boid/web_secret` | Web UI 署名鍵 (mode 0600) |
| `~/.local/state/boid/boid.log` | daemon ログ (ローテーション) |
| `~/.config/boid/config.yaml` | ユーザ設定 (任意) |
| `$XDG_RUNTIME_DIR/boid.sock` | UNIX socket (fallback は `/tmp/boid-<uid>.sock`) |

`~/.config/boid/config.yaml` は任意です。存在しない場合は既定値で動作します。

## 更新

`@latest` で `go install` し直し、daemon を再起動します:

```bash
go install github.com/novshi-tech/boid@latest
boid stop
boid start
```

再起動は必須です。実行中の daemon は古いバイナリを mmap しており、 `boid stop` を省略すると新しいコードがロードされません。

## アンインストール

```bash
boid stop
rm -rf ~/.local/share/boid ~/.local/state/boid ~/.config/boid
rm "$(go env GOPATH)/bin/boid"
```

最初の `rm` でタスク・シークレット・インストール済み kit を含むローカルデータがすべて消えます。再インストール時にデータを残したい場合は該当パスを除外してください。

---

次: [2. 最初のタスク](02-first-task.md)
