# 3. Web UI をセットアップする

このページでは `boid` の Web UI を開けるところまで確認します。次の章で実際にタスクを走らせる際、 ターミナルだけでなくブラウザからもライブで進行状況を観察できるようにしておきます。所要時間は 3 分ほどです。

[2. プロジェクトを初期化する](02-init-project.md) で `demo` プロジェクトを登録済みである前提です。

## なぜ先に Web UI を見るか

`boid` の主な用途は AI エージェントへの長時間タスクの委譲です。 ターミナルに張り付かなくてもよいよう、 Web UI を片手間に開いておくと「いまどこを走っているか」「待ちなのか / 詰まっているのか」を一目で把握できます。 ローカル開発でも便利で、 スマホからアクセスできるようにしておくと外出中にも進捗を覗けます。

## ローカルで開く

[1. インストール](01-install.md) の `boid start` が済んでいれば daemon は `:8080` で HTTP を受け付けています。ブラウザで開いてください。

```
http://localhost:8080
```

[2. プロジェクトを初期化する](02-init-project.md) で登録した `demo` プロジェクトが表示され、 タスク一覧はまだ空のはずです。 同一マシン (loopback アドレス 127.0.0.1 / ::1) からのアクセスはペアリング不要です。

タスクを起こしていく次章では、 別ターミナルの `boid task watch` と並行してこのブラウザを開いておくのが便利です。

## listen アドレスを変える (任意)

既定 `:8080` が他のサービスと衝突する場合は変更できます:

```bash
boid web set-addr 127.0.0.1:5171
boid stop
boid start
```

`boid web set-addr` は `~/.config/boid/config.yaml` の `web.listen` を書き換えます。 daemon を再起動するまで反映されないので注意してください。

Web UI を完全に無効化したい場合は空文字を渡します:

```bash
boid web set-addr ""
```

## 他デバイスからアクセスする (任意)

スマホやサブマシンからも繋ぎたい場合、 公開 URL と ペアリングの 2 段階を踏みます。

1. URL を到達可能にする。 LAN アドレスでアクセスするか、 外から到達させたい場合は Cloudflare Tunnel を前段に立てるのが推奨です (手順は [Web UI ガイド](../guide/web-ui.md#cloudflare-tunnel) を参照)
2. マジックリンク用に公開 URL を 1 度だけ設定する:

   ```bash
   boid web set-url https://boid.example.com
   ```

3. ペアリングコードを発行し、デバイスのログイン画面に入力する:

   ```bash
   boid web pair
   ```

   コードは 5 分有効・単回使用です。

```bash
boid web devices                 # ペアリング済みデバイス一覧
boid web revoke <device-id>      # 1 デバイスを失効
boid web revoke-all              # 全部失効
```

このチュートリアルでは loopback アクセスだけで十分なので、 外部公開は飛ばして構いません。 詳細は [Web UI ガイド](../guide/web-ui.md) にまとめてあります。

## まとめ

このチュートリアルで触れた要素:

- ローカルから Web UI を開いた (loopback はペアリング不要)
- listen アドレスの変更方法 (`boid web set-addr`)
- 他デバイスからアクセスする場合の流れ (`boid web set-url` + `boid web pair`)

次の章では、 ここで開いた Web UI を見ながら Claude Code エージェントに小さなタスクを 1 本走らせます。

---

次: [4. 最初のタスク](04-first-task.md)
