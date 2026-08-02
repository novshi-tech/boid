# Web UI

`boid` は CLI に加えて Web UI を提供します。既定で有効、`:8080` で listen します。 daemon プロセス自身から見た loopback (127.0.0.1 / ::1) からは認証不要、それ以外 (典型的には Cloudflare Tunnel 経由のスマホ) からはデバイスをペアリングしてからアクセスします。

> **注意 (compose daemon、既定の構成):** daemon は compose スタックのコンテナ内で動きます (`docs/plans/release-onboarding.md` 決定2)。ブラウザから見た `http://localhost:8080` は host の port publish 経由でコンテナに転送されますが、コンテナ自身が受け取るリクエストの送信元は **host の `127.0.0.1` ではなく** docker/podman のネットワークブリッジ経由の IP になるため、loopback 例外は発火しません。したがって compose daemon では **`boid web pair` によるペアリングが必須手順**です (詳細は [Getting started / 1. インストール](../getting-started/01-install.md#web-ui-をペアリングする))。以下の「loopback 例外」節の説明は、daemon プロセス自身から見て本当に loopback から届いたリクエスト (`--foreground` でホスト上に直接立てた daemon 等) にのみ適用されます。

## ローカルで開く

`boid start` した後、まず [`boid web pair`](../getting-started/01-install.md#web-ui-をペアリングする) でこのブラウザを認証してから、`http://localhost:8080` を開くとタスク一覧が表示されます。

listen アドレスは `boid web set-addr` で変更できますが、**標準の compose デプロイではこれだけでは host 側から見えるポート番号は変わりません** — `boid web set-addr` が書き換えるのはコンテナ**内部**の bind アドレスであり、`build/container/compose.yml` の `ports:` (`"127.0.0.1:8080:8080"`) は固定です。8080 以外に変更すると、コンテナの 8080 番に何も listen しなくなり、かつ新しいポートは host に公開されていないため、Web UI に到達できなくなります。host 側のポート番号自体を変えるには `build/container/compose.yml` を直接編集する必要があり (チェックアウトが要る開発者向けの手順)、`go install` だけのユーザには現時点で手段がありません。詳細と回避策は [Getting started / 3. Web UI をセットアップする](../getting-started/03-web-ui.md#listen-アドレスを変える-任意) を参照してください。

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

## セッション

セッションとは、 タスクに紐づかない実行中のジョブです (`boid agent` コマンドや Web UI の [New Session] で起動したもの)。 `tmux ls` に近いモデルで、 「今走っていて attach し直せる対話セッション」を表します。

### セッション一覧 (/sessions)

グローバルナビの **Sessions** リンクから開きます。**現在 running 状態のセッションのみ**を全プロジェクト横断で表示します。 完了したセッションは一覧から消えます (履歴は表示しません)。

各行をクリックすると `/jobs/{id}` のターミナル画面に遷移し、 エージェントの出力に再 attach できます。

### 新規セッション (/sessions/new)

一覧右下の **Create** ボタン、 または `/sessions/new` に直接アクセスします。

1. **プロジェクトを選択** — ドロップダウンで絞り込むとフォームが展開します
2. **Harness を選択** — `claude` / `codex` / `opencode` / `shell` のいずれかを選びます
3. **Instruction (任意)** — 最初のターンに渡すプロンプト。 空欄ならハーネスのデフォルト起動になります
4. **readonly チェックボックス** — チェックするとプロジェクトディレクトリが読み取り専用になります (既定: writable)
5. **Session name (任意)** — 一覧での表示ラベル
6. **Start session** ボタンで起動、 `/jobs/{id}/terminal` に自動遷移します

### CLI との対称性

```bash
boid agent claude -p <project>   # Web UI の New Session と同じ操作をターミナルから
```

## ページ

現在の Web UI は以下に対応しています。

- **タスク一覧** (status / behavior / project でフィルタ)
- **タスク詳細** (payload / job / インライン action)
- **セッション一覧** (running の task-less job を全プロジェクト横断表示)
- **新規セッション** (プロジェクト + ハーネスを選んで起動)
- **プロジェクト一覧・詳細**
- **ジョブ一覧・詳細** インラインインタラクティブ端末付き (xterm.js、`GET /api/jobs/{id}/attach/ws` で live attach)
- **ペアリング / ログイン** フロー

---

次: [トラブルシューティング](troubleshooting.md)
