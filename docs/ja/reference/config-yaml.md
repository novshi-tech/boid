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
  forges:
    github:
      secret_key: gh-pat        # 省略可。デフォルト: github-pat
    bitbucket:
      secret_key: bb-token      # 省略可。デフォルト: bitbucket-token
    # カスタム forge id を足す例（GitHub Enterprise 等）:
    github-enterprise:
      host: github.corp.example.com
      forge: github              # github / bitbucket のいずれか（Basic 認証の username 規約を決定）
      secret_key: ghe-pat
```

`gateway.forges` は forge id（map key）ごとに credential 設定を持ちます。`github` と `bitbucket` は **built-in id** で、`host` / `forge` / `secret_key` にデフォルトが用意されているため、`config.yaml` に何も書かなくても最初から有効です（後述）。built-in 以外の id（`github-enterprise` など）はカスタム forge 扱いになり、`host` と `secret_key` を明示する必要があります。

| キー | 型 | デフォルト | 説明 |
|---|---|---|---|
| `forges.<id>.host` | string | built-in id のみデフォルトあり（`github`→`github.com`、`bitbucket`→`bitbucket.org`） | upstream の git ホスト名。カスタム id では必須 |
| `forges.<id>.forge` | string | built-in id のみデフォルトあり（id と同名） | `github` または `bitbucket`（Basic 認証の username 規約を決定）。カスタム id では必須 |
| `forges.<id>.secret_key` | string | built-in id のみデフォルトあり（`github`→`github-pat`、`bitbucket`→`bitbucket-token`） | secret store 参照キー（実 token は `boid secret set <key> <value>` で別途登録）。カスタム id では必須 |

**平文の PAT / token をここに書いてはいけません**。実 token は namespace `default` の secret store にのみ保存され、`secret_key` はそこへの参照名に過ぎません。

このブロックは git gateway（sandbox 内 credential レス git と上流フォージの間の認証注入リバースプロキシ）の per-forge 設定です。project の clone・fetch・push はすべて sandbox 内の git がこの gateway 経由で行います（詳細は [`project.yaml` リファレンス](./project-yaml.md#git-gateway--sandbox-内-clone)）。

### 内蔵デフォルト（github / bitbucket）

`gateway` ブロックを一切書かなくても、`DefaultConfig()` が `github` / `bitbucket` の2 forge を最初から埋めた状態を返します。つまり:

```bash
boid secret set github-pat <PAT>
```

を実行した瞬間から、`~/.config/boid/config.yaml` に何も書かずとも github.com に対する gateway が動作します（bitbucket も同様に `bitbucket-token` を set するだけ）。secret がまだ `boid secret set` されていない forge は、これまでどおり per-key miss として fail-open します（gateway 自体は落ちません）。

`config.yaml` で `secret_key` を変えたい場合だけ、該当 id の下に書けば上書きされます。

### 旧 `gateway.hosts` 記法（非推奨）

cutover 直後の schema だった `gateway.hosts` の配列形式も、**次回リリースまでの猶予**として引き続きパースされます。読み込み時に `slog.Warn` で deprecation warning を出し、内部的には `forges` map に変換されます。

```yaml
# 非推奨。次のリリースで削除予定 — gateway.forges に移行してください。
gateway:
  hosts:
    - host: github.com
      forge: github
      secret_key: gh-pat
```

`forges` と `hosts` を同時に書いた場合は **`forges` が優先**され、同じ host を指す `hosts` 側のエントリは無視されます（warning ログ付き）。

---

## default_harness (撤去済み)

`default_harness` キーおよびそれを解決していた `config.DefaultHarness()` / `SetDefaultHarness()`
(`internal/config/default_harness.go`) は Phase 2.5 PR7 (2026-07) で撤去されました。
このキーを読んでいた唯一の呼び出し元 (`boid kit init` / `boid workspace configure`) は
Phase 2.5 PR6 で既に撤去済みで dead configuration になっていたため、PR7 で config 側も
削除しました。`boid project init --agent <name>` の既定値は別の定数
(`initwizard.DefaultAgent`、既定 `claude-code`) で、この設定とは無関係でした。

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
