# config.yaml リファレンス

`~/.config/boid/config.yaml` は boid daemon のユーザ設定ファイルです（XDG 準拠）。
ファイルが存在しない場合はデフォルト値で動作します。

手で直接編集する代わりに `boid config get/set/unset/apply/edit`（後述）を使うと、
daemon の HTTP API 経由でスキーマ検証つきの編集ができます。volume-only daemon
（`docs/plans/volume-only-daemon.md`）移行後は config.yaml が daemon 自身の
named volume 内にあり、host からファイルを直接編集できなくなるため、こちらが
正式な編集経路になります。

設定変更の反映タイミング: `boid config set/unset/apply/edit` は config.yaml を
その場で書き換えますが、**どのキーも daemon の実際の挙動には即座には反映されません
— 反映には daemon の再起動が必要です**（`boid config` 節参照）。`get` はファイルを
読むだけなので再起動なしでいつでも使えます。手で config.yaml を直接編集した場合
（あるいは `boid config` 経由でない変更）も同様に `boid stop && boid start`
（container backend なら `docker compose -f build/container/compose.yml restart
daemon`）で反映してください。

以前は `sandbox.allowed_domains` / `notify.command` / `web.public_url` の 3 キーだけ
再起動不要のホットリロード対応でしたが、そのための仕組み（`Runner.AllowedDomains` の
getter 化、`Server.AllowedDomains()`、egress proxy listener の毎 dispatch 再取得）は
codex レビューで 4 ラウンドかけてもブロッカーが収束せず、最終的に `Server.Stop` と
in-flight dispatch のデッドロックまで作り込んでしまったため撤回し、全キー一律
「即座に保存・反映は再起動から」というシンプルな契約に統一しました
（nose 判断、PR #830 round 4）。

---

## boid config — CLI での編集

```bash
boid config get                                  # 全体を YAML で dump
boid config get sandbox.allowed_domains          # 個別キーの値を表示

boid config set sandbox.allowed_domains \
  .freee.co.jp .notion.com                       # 配列は複数引数で丸ごと置換
boid config set gateway.forges.github-enterprise.host git.example.com  # map はセグメント traversal

boid config unset web.public_url                 # キー削除（存在しない場合エラー）
boid config unset gateway.forges.github          # forge エントリ丸ごと削除
boid config unset services.myapp                 # service エントリ丸ごと削除 (同じ扱い)

boid config apply -f config.yaml                 # ファイルから全体 apply（デフォルトは If-Match 必須）
boid config apply -f config.yaml --force         # 現在の revision チェックをスキップして上書き
boid config edit                                 # $EDITOR（未設定なら vi）で編集
```

`gateway.forges.github`（built-in id）は host が `github.com` に固定されている
ため、`host` を明示的に変更しようとするとエラーになります。別ホストを使う場合は
上記のようにカスタム forge id（`github-enterprise` など）を追加してください。

- **検証**: 未知のキーは近いキー名のサジェスト付きで拒否されます。
  `sandbox.allowed_domains` の各エントリはホスト名として妥当な構文か
  （RFC 1035 準拠: ラベルは英数字と `-` のみ・63 文字以内、ホスト全体で 253
  文字以内）、`gateway.forges.<id>` は host/forge/secret_key が揃っているかも
  チェックされます。`boid config apply -f` / `edit` はクライアント側で先に
  バリデーションしてから daemon に送るため、明らかに壊れたファイルは daemon
  への往復なしに弾かれます。
- **`get`（引数なし）の出力**: daemon 上の config.yaml の内容をそのまま返します
  （defaults を展開した表示ではありません）。明示的に書いたことのないキーは
  `get`/`unset` から見ると「存在しない」扱いになります（それでも daemon は
  そのキーの組み込みデフォルト値で動作します — この一覧表の「デフォルト」列
  の通り）。
- **反映タイミング**: `set`/`unset`/`apply`/`edit` は成功すると config.yaml へ
  即座に書き込まれますが、変更が daemon の実際の動作に反映されるのは
  **次回再起動から**です。値が変わった leaf ごとに
  `[warning] <key> requires daemon restart to take effect.` という警告が返ります
  （例: `gateway.forges.github.secret_key requires daemon restart`、
  `sandbox.allowed_domains requires daemon restart`）。ホットリロード対応の
  キーは今はありません — `sandbox.allowed_domains` / `notify.command` /
  `web.public_url` もかつては即時反映（dynamic）でしたが、そのための機構が
  codex レビュー 4 ラウンドを経てもブロッカーが収束せず `Server.Stop` の
  デッドロックまで招いたため撤回し、全キー一律 restart-required にしました
  （nose 判断、PR #830 round 4 — 「反映経路が少ないほど壊れ方も少ない」という
  判断）。`sandbox.backend` は PR-4（`docs/plans/volume-only-daemon.md` §論点 e）で
  撤去済みです — container が唯一の sandbox backend になったため選択の余地が
  無くなりました。キー自体は `KindOpaque` として引き続き構造的に認識されます
  （古い `config.yaml` の読み込みを壊さないため）が、`boid config set/unset`
  では使えません（読み取り専用の廃止済みフィールドという専用エラーになります）。
  `boid config apply -f`/`edit` で丸ごとのドキュメントに含まれていても
  エラーにはならず、値は静かに無視されます（daemon 起動時ログに warning が
  一度出ます）。
- **並行編集の保護**: `set`/`unset` は daemon 側で 1 回のアトミックな
  read-modify-write として処理されるため、異なるキーへの同時 `set` が
  互いを打ち消し合うことはありません。`apply -f`/`edit` はドキュメント全体を
  差し替えるため、`get` 時点の revision を暗黙に `If-Match` として送り、
  そのあいだに daemon 側の config.yaml が変わっていた場合は失敗します
  （エラーメッセージが再実行 or `--force` を促します）。`--force` を渡すと
  revision チェックをスキップして無条件に上書きします。
- **スコープ外**: `boid config` は config.yaml そのものを編集します。
  `gateway.forges.<forge>.secret_key` はあくまで secret store への参照名で、
  そこが指す実際のトークン値（env var / secret store の中身）は編集しません
  — 値は引き続き `boid secret set <key> <value>` で設定してください。
- **制限**: `.` を含む forge id（例: カスタム id `"github.corp"`）は
  `get`/`set`/`unset` の dotted-path 構文では指定できません（`.` がパス区切り
  と区別できないため）。そのような id を扱う場合は `boid config apply -f` /
  `edit` を使ってください。

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

## log — ログレベル

```yaml
log:
  level: debug   # debug / info / warn / error（デフォルト: 未設定 = info）
```

| キー | 型 | デフォルト | 説明 |
|---|---|---|---|
| `level` | enum (`debug`/`info`/`warn`/`error`) | 未設定（実効値 `info`） | daemon プロセス全体の slog 出力レベル |

`level` を明示的に設定しない場合、Go の `log/slog` パッケージの組み込みデフォルト（`info`）がそのまま使われます —
これは本キー導入前の boid daemon の挙動と完全に同一です。不正な値（`debug`/`info`/`warn`/`error` 以外の文字列）は
`boid start` 起動時 / `boid config apply`・`edit` 時に**設定読み込みエラー**として拒否されます（`gc.interval` に不正な
duration 文字列を渡した場合や `gateway.forges.*.forge` に未知の forge 名を渡した場合と同じ扱い）。既定へのフォール
バックはしません。

### boid.log の行形式は変わらない

boid daemon はコード中のどこでも `slog.SetDefault` を呼ばず、独自の `slog.Handler`（`TextHandler`/`JSONHandler` 等）も
インストールしていません。そのため `slog.Info`/`slog.Debug` 呼び出しは常に slog 組み込みの
`defaultHandler`（標準 `log` パッケージ経由で出力する非公開ハンドラ）を通り、boid.log の行は今までどおり

```
2009/11/10 23:00:00 INFO msg key=value
```

の形式（日時 + レベル + メッセージ + `key=value` 属性）のまま変わりません。`log.level` の実装は
`log/slog` 標準ライブラリの `slog.SetLogLoggerLevel`（`log` パッケージへのブリッジのしきい値だけを変える関数、
Handler には一切触れない）を daemon 起動時に一度呼ぶだけで、行の書式そのものには影響しません。もし将来
`slog.SetDefault` で本物の `Handler` を差し込む変更が入った場合は、boid.log の行形式が
`time=... level=INFO msg=...` のような別形式に変わり、daemon の生死判定など既存の boid.log grep 手順が壊れる
ため、その変更は別途大きな決定として扱ってください（`internal/config/log_level.go` / `internal/daemon/log_level.go`
の doc comment 参照）。

反映には daemon の再起動が必要です（本ファイル冒頭の「設定変更の反映タイミング」参照）。

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

> **`backend` キーは撤去済み（PR-4、2026-07-25）:** container backend
> （Phase 6 `docs/plans/phase6-container-backend.md`）が唯一の sandbox
> backend になったため、`sandbox.backend`（`userns` | `container`）の
> 選択自体が無意味になりました。古い `config.yaml` に残っていても
> エラーにはならず、静かに無視されます。

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

`gateway.hosts` が残っている config.yaml でも `boid config get`/`apply -f`/`edit`
は問題なく動作します（`gateway.hosts` は読み取り専用の移行用フィールドとして
schema に認識されており、他のキーを変更する `apply`/`edit` を巻き込んで拒否
されることはありません）。ただし `boid config set/unset gateway.hosts...` で
直接編集することはできません — `gateway.forges.*` への移行、または
`apply -f`/`edit` によるドキュメント全体差し替えを使ってください。

`forges` と `hosts` を同時に書いた場合は **`forges` が優先**され、同じ host を指す `hosts` 側のエントリは無視されます（warning ログ付き）。

---

## services / services_floor — API gateway

```yaml
services:
  myapp:
    base_url: https://myapp-staging.example.com
    auth: { kind: bearer, secret_key: myapp_staging_token }
  myapp-ops:
    base_url: https://ops.example.com/api
    auth: { kind: header, header: X-Api-Key, secret_key: ops_key }
  bitbucket-api:
    base_url: https://api.bitbucket.org/2.0
    auth: { kind: basic, username: x-bitbucket-api-token-auth, secret_key: BB_TOKEN }
  legacy:
    base_url: https://legacy.example.com
    auth: { kind: query, query: api_key, secret_key: legacy_key }
  internal-staging:
    base_url: http://internal-staging.example.com   # TLS 未対応の内部環境
    allow_insecure: true                             # 明示的な opt-in が無いと config load エラー
    auth: { kind: bearer, secret_key: staging_token }

services_floor:
  - myapp   # 全 workspace で有効になる service (allowed_domains の floor と同じ位置づけ)
```

`services` は API gateway (`internal/apigateway`、sandbox 内 credential レス HTTP クライアントと任意の外部 API の間の認証注入リバースプロキシ) の service registry です。git gateway (`gateway.forges`) の姉妹機構で、git smart HTTP に限らない任意の HTTP API を対象にします。詳細設計は [`docs/plans/api-gateway.md`](../../plans/api-gateway.md) を参照してください。

| キー | 型 | デフォルト | 説明 |
|---|---|---|---|
| `services.<name>.base_url` | string | (必須) | upstream の base URL。sandbox からは見えない — sandbox が見るのは論理名 `<name>` だけ。`https` 以外のスキームは `allow_insecure: true` が無いと config load 自体が失敗する |
| `services.<name>.allow_insecure` | bool | `false` | `base_url` に `https` 以外のスキームを許可する明示的な opt-in。無いまま `http://` 等を指定すると config load エラー (内部テスト API 等 TLS が無い環境向けの意図的な抜け道であり、黙って許可はしない) |
| `services.<name>.auth.kind` | string | (必須) | `bearer` / `basic` / `header` / `query` / `oauth2` のいずれか |
| `services.<name>.auth.secret_key` | string | kind により必須 | secret store 参照キー (`bearer`/`basic`/`header`/`query` で必須。`oauth2` では未使用) |
| `services.<name>.auth.username` | string | `basic` のみ必須 | Basic 認証の username |
| `services.<name>.auth.header` | string | `header` のみ必須 | 注入するヘッダ名 |
| `services.<name>.auth.query` | string | `query` のみ必須 | 注入するクエリパラメータ名 |
| `services.<name>.auth.provider` | string | `oauth2` のみ必須 | OAuth2 provider 名 (**予約のみ、PR2 で実装** — `kind: oauth2` は schema として受理されるが、実際のリクエストは 502 で拒否される) |
| `services_floor` | []string | `[]` | 全 workspace に共通で有効化する service 名のリスト (`sandbox.allowed_domains` の floor と同じ additive 方式) |

**平文の token / API key をここに書いてはいけません**。実値は `boid secret set <key> <value>` で secret store に登録し、`secret_key` はそこへの参照名に過ぎません（`gateway.forges.*.secret_key` と同じ規約）。

sandbox からは `BOID_API_BASE` 環境変数 (`https://<gateway>/api/<job-token>`) が渡され、`$BOID_API_BASE/<service>/<path...>` の形で叩けます — base URL 差し替えだけで curl でも任意の SDK でも動くのが host command 方式に対する利点です。`task.readonly` (または `command.readonly`) な job には GET/HEAD 以外のメソッドが 403 になります。

**TLS trust の注意**: container backend (現行唯一の backend) では gateway listener が daemon 内蔵 CA で TLS を張るため、標準的な curl/Python 等はこの CA を自動では信用しません。`BOID_API_CA_FILE` (CA の PEM path) が env 注入されるので、`curl --cacert "$BOID_API_CA_FILE" "$BOID_API_BASE/..."` のように明示的に渡す必要があります。Node.js のみ `NODE_EXTRA_CA_CERTS` (Node が「既存の root CA に追加される」と保証する加算的な変数) が併せて注入されるため flag なしで動きます。詳細は `docs/plans/api-gateway.md` §1 参照。

workspace 単位の有効化は `services_floor` (daemon 全体) + workspace 自身の `Services` リスト (`boid workspace services add/remove/list`、[CLI リファレンス](./cli.md#workspace) 参照) の additive union です。`services_floor` に書いた名前が `services` に存在しない場合は起動時に warning が出ますが、config load 自体は失敗しません。

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
