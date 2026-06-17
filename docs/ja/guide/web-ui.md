# Web UI

`boid` は CLI / TUI に加えて Web UI を提供します。既定で有効、`:8080` で listen します。 loopback (127.0.0.1 / ::1) からは認証不要、それ以外 (典型的には Cloudflare Tunnel 経由のスマホ) からはデバイスをペアリングしてからアクセスします。

## ローカルで開く

`boid start` した後、ブラウザで `http://localhost:8080` を開くとタスク一覧が表示されます。

listen アドレスは `boid web set-addr` で変更できます:

```bash
boid web set-addr 127.0.0.1:5171
```

**Web UI を無効化することはできません。** アドレスを空文字に設定しても HTTP listener の起動は止まらず、 daemon は `:8080` にフォールバックします。 現時点では HTTP listener を完全に停止する手段はありません。

## 他デバイスから

`boid` は単一ユーザを前提にしています。ペアリングは事故的アクセスを防ぐためのもので、 daemon は自分のラップトップ上で動いているのと同程度の信頼ラインで運用してください。

手順は 3 つ:

1. URL を到達可能にする。 LAN アドレスで動かすか、スマホ向けには [Cloudflare Tunnel](#cloudflare-tunnel) で前段を作るのが推奨
2. 公開 URL を 1 度だけ設定する: `boid web set-url https://boid.example.com`。マジックリンクのレンダリングに使われます
3. `boid web pair` でコードを発行し、デバイスのログイン画面に入力する。コードは 5 分有効・単回使用

```bash
boid web pair                    # ペアリングコード発行
boid web devices                 # ペアリング済みデバイス一覧
boid web revoke <device-id>      # 1 デバイスを失効
boid web revoke-all              # 全部失効
```

デバイス cookie の寿命は 90 日 (ローリング) / アイドル 30 日。 CSRF は double-submit cookie で防御しています。

### ペアリングコードの形式

`WX7K-4QJP` (英数 8 桁にハイフン)。単回使用、 5 分有効、 IP あたり 5 回 / 5 分のレート制限。

### loopback 例外

`127.0.0.1` / `::1` からのリクエストはペアリングをスキップします。ただし `X-Forwarded-For` / `CF-Connecting-IP` / `Forwarded` ヘッダが付いていれば loopback として扱いません。 localhost にプロキシする Tunnel が誤って認証をバイパスする事故を防ぐためです。

## Cloudflare Tunnel

スマホから `boid` にアクセスする推奨構成は、ユーザ systemd で `cloudflared` を動かすことです。

### 前提

- Cloudflare アカウントと、 Cloudflare DNS で管理されたドメイン (例: `nosen.dev`)
- `cloudflared` のインストール (`apt install cloudflared` や Cloudflare 公式リポジトリ)

### 初回セットアップ

1. `cloudflared` を Cloudflare アカウントに認証

   ```bash
   cloudflared tunnel login
   ```

2. トンネル作成

   ```bash
   cloudflared tunnel create boid
   ```

   `~/.cloudflared/<tunnel-id>.json` に credentials が生成されます。

3. ルーティング設定。 `~/.cloudflared/config.yml` を作成:

   ```yaml
   tunnel: <tunnel-id>
   credentials-file: /home/<you>/.cloudflared/<tunnel-id>.json

   ingress:
     - hostname: boid.example.com
       service: http://127.0.0.1:8080
     - service: http_status:404
   ```

4. ホスト名をトンネルに紐付け

   ```bash
   cloudflared tunnel route dns boid boid.example.com
   ```

5. ユーザレベルの systemd unit として動かす (`~/.config/systemd/user/cloudflared-boid.service`):

   ```ini
   [Unit]
   Description=cloudflared tunnel for boid
   After=network-online.target

   [Service]
   ExecStart=/usr/bin/cloudflared tunnel run boid
   Restart=on-failure

   [Install]
   WantedBy=default.target
   ```

   有効化 + 起動:

   ```bash
   systemctl --user enable --now cloudflared-boid.service
   ```

6. マジックリンク用に公開 URL を `boid` に教える:

   ```bash
   boid web set-url https://boid.example.com
   ```

### スマホから

`https://boid.example.com` を開き、 `boid web pair` で発行したコードを入力するとデバイス cookie が設定されます。以降は失効させるか 90 日経つまで、そのデバイスから `boid` を操作できます。

### セキュリティ上の注意

- ペアリングは proper firewalling の代わりにはなりません。第三者が API を叩くのを防ぐだけのものです。公開 URL ガードを外したり HTTPS を無効化したりしないでください
- 念のため、 Cloudflare Access (メール / service token 認証) をトンネルの上に重ねるとより安全です
- 使わなくなったデバイスは revoke してください。 30 日より短いアイドルタイムアウトはありません

## ページ

現在の Web UI は以下に対応しています。

- **タスク一覧** (status / behavior / project でフィルタ)
- **タスク詳細** (payload / job / インライン action)
- **プロジェクト一覧・詳細**
- **ジョブ一覧・詳細** インラインインタラクティブ端末付き (xterm.js、`GET /api/jobs/{id}/attach/ws` で live attach)
- **ペアリング / ログイン** フロー

---

次: [トラブルシューティング](troubleshooting.md)
