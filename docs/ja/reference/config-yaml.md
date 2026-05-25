# config.yaml リファレンス

`~/.config/boid/config.yaml` は boid daemon のユーザ設定ファイルです（XDG 準拠）。
ファイルが存在しない場合はデフォルト値で動作します。

設定変更は `boid stop && boid start` で反映されます。

---

## gc — ガベージコレクション

```yaml
gc:
  enabled: true       # false にすると自動 GC を無効化
  interval: 24h       # GC の実行間隔（デフォルト: 24h）
  older_than: 720h    # この期間より古いデータを削除（デフォルト: 720h = 30日）
```

| キー | 型 | デフォルト | 説明 |
|---|---|---|---|
| `enabled` | bool | `true` | 自動 GC の有効/無効 |
| `interval` | duration | `24h` | GC の実行間隔 |
| `older_than` | duration | `720h` | 削除対象とする最小経過時間 |

手動実行は `boid gc` で可能。

---

## web — Web UI

```yaml
web:
  http_addr: ":8080"                    # listen アドレス（デフォルト: :8080）
  public_url: "https://boid.example.com"  # 外部公開 URL（マジックリンク用）
```

| キー | 型 | デフォルト | 説明 |
|---|---|---|---|
| `http_addr` | string | `":8080"` | HTTP サーバの listen アドレス |
| `public_url` | string | — | Cloudflare Tunnel 等で公開する場合の外部 URL |

`http_addr` は `boid web set-addr <addr>` コマンドでも変更できます。

---

## notify — 通知

```yaml
notify:
  command: ["/home/you/bin/boid-notify.sh", "--title", "boid"]
```

| キー | 型 | デフォルト | 説明 |
|---|---|---|---|
| `command` | []string | — | `boid task notify` 呼び出し時に exec するコマンド |

空の場合、`boid task notify` は HTTP 501 を返して通知をスキップします（タスク実行には影響しません）。

詳細は [通知ガイド](../guide/notifications.md) を参照してください。

---

## sandbox — サンドボックス

```yaml
sandbox:
  allowed_domains:
    - ".github.com"       # ドット始まりはサフィックスマッチ
    - "api.example.com"   # ドットなしは完全一致
```

| キー | 型 | デフォルト | 説明 |
|---|---|---|---|
| `allowed_domains` | []string | `[]` | デフォルトの許可リストに追加するドメイン |

起動時に `defaultAllowedDomains`（Anthropic/OpenAI API・各言語パッケージレジストリ等）へ追記されます。
プロキシ許可リストの詳細は [サンドボックス内部](../architecture/sandbox-internals.md) を参照してください。
