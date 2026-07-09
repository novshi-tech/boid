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

これらの設定は `Config.UnmarshalYAML` によって `config.yaml` から読み込まれます。
`GCConfig` 構造体フィールドには `yaml:"-"` タグが付いていますが、ロード時に独自デコード処理で明示的に適用されます。

> **注意:** `config.yaml` の `older_than` は **daemon の自動 GC ループ**にのみ反映されます。
> 手動実行の `boid gc`（および `POST /api/gc`）は **720h（30 日）のハードコード値**を使用し、config の値は参照しません。
> 一回限りの手動実行で閾値を変えたい場合は `boid gc --older-than <duration>` を使用してください。

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
| `http_addr` | string | `""` | HTTP サーバの listen アドレス |
| `public_url` | string | — | Cloudflare Tunnel 等で公開する場合の外部 URL |

> **デフォルトアドレスについて:** `config.DefaultConfig()` では `http_addr` は空です。実効デフォルトの `127.0.0.1:8080` は起動時に `cmd/start.go` のフォールバック処理で適用されます。

`http_addr` は `boid web set-addr <addr>` コマンドでも変更できます。

> **警告:** `boid web set-addr` および `boid web set-url` は YAML round-trip（`yaml.Marshal`）で `config.yaml` を書き換えるため、**ファイル内のコメントがすべて削除**されます。

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

---

## gateway — git gateway

```yaml
gateway:
  hosts:
    - host: github.com
      forge: github        # github / bitbucket のいずれか
      secret_key: gh-pat    # boid secret set gh-pat <PAT> で登録した key
    - host: bitbucket.org
      forge: bitbucket
      secret_key: bb-token
```

| キー | 型 | デフォルト | 説明 |
|---|---|---|---|
| `hosts[].host` | string | — | upstream の git ホスト名（例: `github.com`） |
| `hosts[].forge` | string | — | `github` または `bitbucket`（Basic 認証の username 規約を決定） |
| `hosts[].secret_key` | string | — | secret store 参照キー（実 token は `boid secret set <key> <value>` で別途登録） |

**平文の PAT / token をここに書いてはいけません**。実 token は namespace `default` の secret store にのみ保存され、`secret_key` はそこへの参照名に過ぎません。

このブロックは git gateway（sandbox 内 credential レス git と上流フォージの間の認証注入リバースプロキシ）の per-host 設定です。daemon 起動時に必ず gateway サーバ自体は立ち上がりますが、2026-07 時点ではまだどのジョブもこれを経由しません（`docs/plans/git-gateway-cutover.md` PR4 は inert な配線のみ、実際の clone は後続 PR）。

---

## default_harness — デフォルト harness

```yaml
default_harness: claude   # claude / codex / opencode のいずれか
```

| キー | 型 | デフォルト | 説明 |
|---|---|---|---|
| `default_harness` | string | — | `boid kit init` / `boid workspace configure` が起動する harness |

`boid kit init` 実行時に未設定の場合、対話プロンプトで聞いてこのキーに永続化します。環境変数 `BOID_DEFAULT_HARNESS` で一時 override できます（config より優先）。

詳細は [オンボーディング](../guide/onboarding.md) を参照してください。

---

## task_ask — ブロッキング Q&A

```yaml
task_ask:
  disconnect_grace: 30m   # 既定 30 分
```

| キー | 型 | デフォルト | 説明 |
|---|---|---|---|
| `disconnect_grace` | duration | `30m` | `boid task ask` で待機中（`awaiting`）のタスクに生きたエージェントが繋がっていない状態を、何分まで猶予してから回収するか |

`boid task ask` はハーネス非依存のブロッキング Q&A です。ハーネス（claude-code / opencode 等）は長時間の shell コマンドを概ね 2 分で kill するため、回答待ちの `boid task ask` が切断されることがあります。エージェントは同じ質問を再実行して `awaiting` に再アタッチできる（回答は DB に永続化されるため失われない）ので、切断だけではタスクを中断しません。`disconnect_grace` を過ぎてもエージェントが戻らず、回答も届いていない場合にのみ、daemon がそのタスクを `aborted` に回収します。短くすると死んだ待機タスクを早く片付けられますが、人手の回答が遅れるケースを誤って中断しやすくなります。
