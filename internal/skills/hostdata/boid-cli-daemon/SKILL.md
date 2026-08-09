---
name: boid-cli-daemon
description: >
  boid daemon 自身のライフサイクル (start/stop/gc/check)、Web UI のデバイス
  ペアリング、host_commands レジストリをホスト側 (boid CLI を直接叩けるマシン上、
  job サンドボックスの外) から操作する。「boid を起動/停止して」「web UI に
  ペアリングして」「デバイスを失効して」「host_commands を再読込して」「gc を
  実行して」など boid 自身の運用操作を依頼されたときに使用。
---

# boid-cli-daemon — daemon / Web / host_commands 管理 (host 側)

このスキルは **boid job サンドボックスの外**、boid CLI をホストで直接叩く前提。
正確なフラグは `boid <command> --help` で確認する（このファイルはコマンド一覧
と手順、フラグの網羅表ではない）。詳しくは boid リポジトリ内なら
`docs/ja/reference/cli.md` を参照。

## コマンド一覧

### サーバライフサイクル

| コマンド | 役割 |
|---|---|
| `boid start` | compose スタックを起動 (`docker/podman compose up -d` 相当) |
| `boid stop` | compose スタックを停止 |
| `boid gc [--older-than DURATION] [--dry-run]` | 古い done/aborted タスクを GC (daemon は起動時からも自動 GC を回している)。`--dry-run` で削除せず対象一覧のみ表示 |
| `boid check` | host の前提コマンド・hook 依存をチェック |

### Web (デバイスペアリング)

| コマンド | 役割 |
|---|---|
| `boid web pair [--label LABEL]` | 5 分有効・単回使用のペアリングコードを発行 |
| `boid web devices` | ペアリング済みデバイス一覧 |
| `boid web revoke <id>` | 特定デバイスを失効 |
| `boid web revoke-all` | 全デバイスを失効 |
| `boid web set-url <URL>` | 公開 URL (`web.public_url`、マジックリンク用) を設定 |
| `boid web set-addr <ADDR>` | HTTP リッスンアドレス (`web.http_addr`) を設定。**反映には daemon 再起動が要る** |

### Host Commands

| コマンド | 役割 |
|---|---|
| `boid host-commands list` | daemon が把握している host_commands の名前一覧 |
| `boid host-commands reload` | `~/.config/boid/host_commands.yaml` を手で編集した後に daemon に再読込させる |

## 手順・落とし穴

### 新しいデバイスを Web UI にペアリングする

```bash
boid web pair --label "my-laptop"
# 5分以内に、表示されたコード/URL/QRコードでブラウザから登録する
```

loopback (127.0.0.1/::1) からは pairing 不要。外部公開 (Cloudflare Tunnel 等)
からは必須。

### `set-addr` した後は再起動が要る

```bash
boid web set-addr :9090
boid stop && boid start
```

`web set-addr` はコンテナ**内部**の bind アドレスを変えるだけで、標準の compose
デプロイでは host 側に公開されるポート (既定 8080) 自体は変わらない —
ポート番号を変えると Web UI に到達できなくなる点に注意。

### `host_commands.yaml` を編集したら reload を忘れずに

`host_commands` は workspace 側の参照名一覧 (`host_commands: [gh, aws]`) と、
daemon 側の実体定義 (`~/.config/boid/host_commands.yaml`) の二層構造。
定義を手で編集したら:

```bash
boid host-commands reload
boid host-commands list   # 反映確認
```

`boid kit init` のような自動生成コマンドは Phase 2.5 PR6 で撤去済みなので、
`host_commands.yaml` は今は手書き運用が前提。

### `gc` は daemon を自動起動しない

`boid gc` は daemon が起動していなければ「gc するためだけに daemon を起動する」
ことをしない設計 (annotationSkipAutostart)。daemon が動いていない状態で叩くと
即座にエラーになる — 先に `boid start` してから実行する。

## 関連スキル

- [`boid-cli-workspace`](../boid-cli-workspace/SKILL.md) — workspace / project / secret 管理
- [`boid-cli-task`](../boid-cli-task/SKILL.md) — task/job の作成・追跡
