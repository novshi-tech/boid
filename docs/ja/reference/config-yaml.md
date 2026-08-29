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
boid config unset oauth_providers.freee          # oauth provider エントリ丸ごと削除 (同じ扱い)

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
- **制限**: `.` を含む forge id（例: カスタム id `"github.corp"`）、service 名
  （`services.<name>`）、oauth provider 名（`oauth_providers.<name>`）は
  `get`/`set`/`unset` の dotted-path 構文では指定できません（`.` がパス区切り
  と区別できないため）。そのような id/名前を扱う場合は `boid config apply -f` /
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
| `egress_proxy_port_low` | int | `0`（= 30000） | egress プロキシのポート採番帯の下限 |
| `egress_proxy_port_high` | int | `0`（= 32767） | egress プロキシのポート採番帯の上限 |

起動時に `defaultAllowedDomains`（Anthropic/OpenAI API・各言語パッケージレジストリ等）へ追記されます。
プロキシ許可リストの詳細は [サンドボックス内部](../architecture/sandbox-internals.md) を参照してください。

### egress プロキシのポートは workspace ごとに固定される

egress プロキシは workspace ごとに別ポートで listen しており、そのポート番号は
**daemon を再起動しても変わりません**（`docs/plans/egress-proxy-stable-port.md`）。
割り当てた番号は DB に永続化され、次回起動時に同じ番号で bind し直します。

これは、プロキシ URL を設定ファイルに焼くツールを壊さないための仕様です。
env の `HTTPS_PROXY` を読むツール（curl / git など）はポートが変わっても追随
しますが、たとえば `~/.npmrc` に `proxy=http://boid-egress:38865` と書かれて
いると、ポートが変わった瞬間に npm / pnpm だけが `ECONNREFUSED` を長いバック
オフでリトライし続け、「エラー」ではなく「ハング」として現れます。

`egress_proxy_port_low` / `egress_proxy_port_high` は、その採番帯を上書きしたい
場合にだけ設定します。**両方セットで指定**してください（片方だけはエラー）。
既定の 30000–32767 はカーネルのエフェメラルポート帯（通常 32768–60999、
`net.ipv4.ip_local_port_range`）より下に置いてあります。エフェメラル帯から固定
ポートを選ぶと、その番号がたまたま外向き接続の送信元ポートとして使われている
最中の再起動で bind に失敗するためです。`ip_local_port_range` を既定より下げて
いる環境では、重ならない帯へ移してください。

帯が埋まっている場合はエフェメラルポートにフォールバックし、警告を出した上で
起動は続行します（ポート固定は利便性の機能であり、egress の分離は許可リストと
workspace ごとの listener 分離が担っているため）。

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
  slack:
    base_url: https://slack.com/api
    allow_readonly_write: true   # readonly job token でも POST 等を許可する opt-in (既定 false)
    auth: { kind: bearer, secret_key: slack_bot_token }

services_floor:
  - myapp   # 全 workspace で有効になる service (allowed_domains の floor と同じ位置づけ)
```

`services` は API gateway (`internal/apigateway`、sandbox 内 credential レス HTTP クライアントと任意の外部 API の間の認証注入リバースプロキシ) の service registry です。git gateway (`gateway.forges`) の姉妹機構で、git smart HTTP に限らない任意の HTTP API を対象にします。詳細設計は [`docs/plans/api-gateway.md`](../../plans/api-gateway.md) を参照してください。

| キー | 型 | デフォルト | 説明 |
|---|---|---|---|
| `services.<name>.base_url` | string | (必須) | upstream の base URL。sandbox からは見えない — sandbox が見るのは論理名 `<name>` だけ。`https` 以外のスキームは `allow_insecure: true` が無いと config load 自体が失敗する |
| `services.<name>.allow_insecure` | bool | `false` | `base_url` に `https` 以外のスキームを許可する明示的な opt-in。無いまま `http://` 等を指定すると config load エラー (内部テスト API 等 TLS が無い環境向けの意図的な抜け道であり、黙って許可はしない) |
| `services.<name>.allow_readonly_write` | bool | `false` | readonly な job token (`task.readonly`/`command.readonly`) でもこの service への GET/HEAD 以外のメソッドを許可する opt-in。既定は fail-closed で readonly job は 403。**config.yaml (daemon 側) にしか置けない** — project.yaml / task_behaviors には無い。repo 側から書き込み許可を付与できてしまうと readonly ゲートの意味が無くなるため (prompt injection されたエージェントが自分で自分に書き込み権限を与えられてしまう) |
| `services.<name>.auth.kind` | string | (必須) | `bearer` / `basic` / `header` / `query` / `oauth2` のいずれか |
| `services.<name>.auth.secret_key` | string | kind により必須 | secret store 参照キー (`bearer`/`basic`/`header`/`query` で必須。`oauth2` では未使用) |
| `services.<name>.auth.username` | string | `basic` のみ必須 | Basic 認証の username |
| `services.<name>.auth.header` | string | `header` のみ必須 | 注入するヘッダ名 |
| `services.<name>.auth.query` | string | `query` のみ必須 | 注入するクエリパラメータ名 |
| `services.<name>.auth.provider` | string | `oauth2` のみ必須 | `oauth_providers.<name>` を参照する OAuth2 provider 名 (下記)。config load 時に参照先の存在はクロスチェックしない — 未宣言の provider を指すと request 時に 502 (`apigateway: oauth2 provider "..." is not configured`) になる |
| `services_floor` | []string | `[]` | 全 workspace に共通で有効化する service 名のリスト (`sandbox.allowed_domains` の floor と同じ additive 方式) |

**平文の token / API key をここに書いてはいけません**。実値は `boid secret set <key> <value>` で secret store に登録し、`secret_key` はそこへの参照名に過ぎません（`gateway.forges.*.secret_key` と同じ規約）。

sandbox からは `BOID_API_BASE` 環境変数 (`https://<gateway>/api/<job-token>`) が渡され、`$BOID_API_BASE/<service>/<path...>` の形で叩けます — base URL 差し替えだけで curl でも任意の SDK でも動くのが host command 方式に対する利点です。`task.readonly` (または `command.readonly`) な job には GET/HEAD 以外のメソッドが 403 になります。

**TLS trust の注意**: container backend (現行唯一の backend) では gateway listener が daemon 内蔵 CA で TLS を張るため、標準的な curl/Python 等はこの CA を自動では信用しません。`BOID_API_CA_FILE` (CA の PEM path) が env 注入されるので、`curl --cacert "$BOID_API_CA_FILE" "$BOID_API_BASE/..."` のように明示的に渡す必要があります。Node.js のみ `NODE_EXTRA_CA_CERTS` (Node が「既存の root CA に追加される」と保証する加算的な変数) が併せて注入されるため flag なしで動きます。詳細は `docs/plans/api-gateway.md` §1 参照。

workspace 単位の有効化は `services_floor` (daemon 全体) + workspace 自身の `Services` リスト (`boid workspace services add/remove/list`、[CLI リファレンス](./cli.md#workspace) 参照) の additive union です。`services_floor` に書いた名前が `services` に存在しない場合は起動時に warning が出ますが、config load 自体は失敗しません。

### credential account 修飾 (`<service>@<account>`) — 1 service 複数 credential

同じ `services.<name>` 定義を複数の credential セット (例: 複数の Freee 事業所) で
使い分けたい場合、sandbox 側は path の service セグメントに `@<account>` を付けて
呼びます。

```
$BOID_API_BASE/freee@ubs/api/1/deals?company_id=123456
$BOID_API_BASE/freee@nvt/api/1/deals?company_id=789012
$BOID_API_BASE/freee/api/1/deals?company_id=123456          # account 無し = 従来どおり
```

- **対応する auth.kind**: `bearer` / `basic` / `header` / `query` (static kind)
  と `oauth2` の両方。static kind で account を付けると、`auth.secret_key` を
  secret store から引く際のキーが `<secret_key>@<account>` に変わります
  (account 無しなら従来と完全に同一のキー)。`oauth2` kind で account を付けると、
  `oauth2:<provider>:refresh_token` / `oauth2:<provider>:access_token_cache`
  が `oauth2:<provider>@<account>:refresh_token` /
  `oauth2:<provider>@<account>:access_token_cache` に変わります —
  詳細は下記「daemon 単一リフレッシャ + 先回りリフレッシュ」節。
  `oauth_providers.<name>.client_secret_key` は例外で、account を付けても
  **修飾されません** (下記参照) — provider (OAuth アプリ) 単位の値であって
  account 単位ではないためです。
- **account 名の文字種**: 英数字・`-`・`_` のみ、1〜64 文字。それ以外の文字
  (`@`・`/`・`:` を含む) や、空の account 名、`@` が 2 個以上あるパスは
  **400** で拒否されます (path の形自体が不正な場合の 404 とは区別されます)。
- **フォールバックしない**: `<secret_key>@<account>` が secret store に無い
  場合、`<secret_key>` (account 無しのキー) へは決して落ちません。502 に
  なります — 意図しない account の credential で書き込んでしまう事故を
  構造的に防ぐためです。
- **認可 (workspace の有効 service 一覧) は account 無しの base 名で判定**:
  workspace の `services` リストには `freee` とだけ書けば、`freee@ubs` /
  `freee@nvt` のどちらも通ります。account ごとに個別の認可設定は無く、同一
  workspace の job であれば同じ service の任意の account に触れます。
  `allow_readonly_write` などの service 単位の設定も base 名 (`freee`) の
  ものが account 修飾リクエストにそのまま適用されます。
- **監査ログ**: `boid task action list`/`boid job log` 等に記録される
  service 名は `freee@ubs` のように account 込みの形です。
- `services.<name>` / `oauth_providers.<name>` の名前自体に `@` を含めることは
  できません (config load エラー) — path 上で account の区切りとして予約
  されているためです。

設計の詳細・却下案・PR 分割は
[`docs/plans/api-gateway-credential-accounts.md`](../../plans/api-gateway-credential-accounts.md)
を参照してください。

### `services.<name>.uses` — Integration Pack の service profile から生成する

`base_url`/`auth` を手書きする代わりに、導入済みの Integration Pack (下記 `integrations.dir`) が宣言する service profile を参照して instance を作れます (docs/plans/signal-driven-review.md §7、docs/plans/signal-ingest-detailed-design.md §6)。

```yaml
services:
  customer-jira:
    uses: jira-cloud/jira-cloud@1.2.0   # <pack>/<profile>@<version>
    endpoint: https://example.atlassian.net
    username: alice@example.com          # profile の credential slot が usernameFrom: instance を宣言している場合のみ
    credentials:
      token: JIRA_TOKEN                  # secret store の key 名 (値そのものではない)
```

| キー | 型 | 説明 |
|---|---|---|
| `services.<name>.uses` | string (`<pack>/<profile>@<version>`) | 導入済み Pack の service profile への参照。`base_url`/`auth` と排他 (両方書くと config load エラー) |
| `services.<name>.endpoint` | string | profile が `endpoint.configurable: true` を宣言している場合のみ指定可 (指定必須)。それ以外では指定するとエラー |
| `services.<name>.credentials.<slot>` | string | profile が宣言する credential slot 名へ secret store の key を bind する。値そのものはここに書かない |
| `services.<name>.username` | string | profile の credential slot が Basic 認証 (`injection: basic`) かつ `usernameFrom: instance` を宣言している場合のみ指定可 (指定必須)。平文の設定値 (例: Jira Cloud の Atlassian アカウントのメールアドレス) であり secret ではないため `credentials:` とは別の top-level フィールドになっている。profile が固定 username (`username:`) を宣言している場合や basic 以外の injection では指定するとエラー |

daemon 起動時に `internal/integrationpack` が `uses:` を実際の `base_url`/`auth` へ脱糖して API gateway の service registry に登録する。参照する Pack が未導入・profile 未宣言・credential slot 不一致・(basic + `usernameFrom: instance` の場合の) username 未指定などは daemon の起動エラーになる (`services` の他エントリと同じ eager validation)。

**Basic 認証の username: profile 固定 vs instance 固有** — Pack の `integration.yaml` が `injection: basic` の credential slot に書くのは次の2通りのどちらかである (両方は書けない):

```yaml
# 例1: username が API 全体で固定 (Bitbucket Cloud の "x-bitbucket-api-token-auth" 規約)
credentials:
  - name: token
    injection: basic
    username: x-bitbucket-api-token-auth   # Pack が宣言する固定値。instance は何も書かない

# 例2: username が instance (テナント) ごとに違う (Jira Cloud のメールアドレス)
credentials:
  - name: token
    injection: basic
    usernameFrom: instance                 # instance 側の "username:" が値を供給する
```

例2の profile を使う instance は `services.<name>.username` を必ず指定する (前掲の `customer-jira` 例)。例1の profile を使う instance は `username:` を指定してはいけない (指定するとエラー — 埋める slot が無い)。

---

## `integrations` — Integration Pack registry

```yaml
integrations:
  dir: /opt/boid/integrations   # 既定値
```

| キー | 型 | デフォルト | 説明 |
|---|---|---|---|
| `integrations.dir` | string (絶対パス) | `/opt/boid/integrations` | 導入済み Integration Pack を daemon が探すディレクトリ。`<dir>/<pack名>/<version>/integration.yaml` の形で列挙される。ディレクトリ自体が存在しなくてもエラーにはならない (Pack 未導入の既定状態)。v0 の配布方式は Pack repo の host checkout をこのパスへ compose volume で bind mount する運用 (詳細は `docs/plans/signal-ingest-detailed-design.md` §6.4/§10)。**再起動が必要** (`Reload: ReloadRestartRequired`) |

`services.<name>.uses` (上記) の解決元となる registry で、`boid signal` 系の connector 実行 (project.yaml `signals.sources[]`、[project.yaml リファレンス](./project-yaml.md#signalssources) 参照) が使う Pack もここから解決される。

---

## oauth_providers — API gateway の OAuth2 provider 定義

```yaml
oauth_providers:
  freee:
    token_endpoint: https://accounts.secure.freee.co.jp/public_api/token
    client_id: <freee アプリの client_id>
    client_secret_key: freee_oauth_client_secret   # secret store 参照 (confidential client のみ)
    scopes: [read, write]
    flow: manual                                   # OOB — freee は PKCE 非対応
    authorization_endpoint: https://accounts.secure.freee.co.jp/public_api/authorize
  google:
    token_endpoint: https://oauth2.googleapis.com/token
    client_id: <Google OAuth client の client_id>
    scopes: [https://www.googleapis.com/auth/calendar.readonly]
    flow: loopback
    authorization_endpoint: https://accounts.google.com/o/oauth2/v2/auth
    authorize_params:
      access_type: offline   # refresh_token を発行させるために必須 (Google 固有)
      prompt: consent        # 2 回目以降のログインでも refresh_token を再発行させる
  github:
    token_endpoint: https://github.com/login/oauth/access_token
    client_id: <GitHub OAuth App の client_id>
    flow: device
    device_authorization_endpoint: https://github.com/login/device/code
  az:
    # client_credentials grant (Service Principal / app-only 認証) の例。
    # tenant は token_endpoint の URL 自体に含める (common/organizations は
    # client_credentials で拒否されるため、tenant ID か検証済みドメイン固定が必須)。
    token_endpoint: https://login.microsoftonline.com/<tenant-id>/oauth2/v2.0/token
    client_id: <Service Principal の client_id>
    client_secret_key: az_sp_client_secret   # secret store 参照 (client_credentials では必須)
    scopes: [https://api.example.com/.default]  # v2 エンドポイントは 1 リソース 1 scope のみ
    grant: client_credentials                # flow とは併用不可 (login flow という概念が無い)

services:
  freee:
    base_url: https://api.freee.co.jp
    auth: { kind: oauth2, provider: freee }
```

`oauth_providers` は `services.*.auth.kind: oauth2` が参照する OAuth2 provider 定義です (docs/plans/api-gateway.md §6/§論点4)。1 provider が複数 service から共有されうる (例: freee 会計 API と freee 人事労務 API が同じ OAuth2 grant を使う場合、両方の service の `auth.provider` に同じ provider 名を書けば、トークンは 1 つの refresh_token グラントに集約される)。

| キー | 型 | デフォルト | 説明 |
|---|---|---|---|
| `oauth_providers.<name>.token_endpoint` | string | (必須) | provider の OAuth2 token endpoint (RFC 6749 §3.2)。スキームは常に `https` 必須 — `services.*.base_url` と異なり `allow_insecure` のような抜け道は無い (token endpoint は常に実在の外部 OAuth2 provider 宛であり、TLS 無し内部テスト API のようなユースケースが無いため) |
| `oauth_providers.<name>.client_id` | string | (必須) | OAuth2 client_id。RFC 6749 §2.2 の分類上 secret ではないため平文で書く |
| `oauth_providers.<name>.client_secret_key` | string | 省略可 | confidential client の client_secret を指す secret store 参照キー。public client (PKCE、client_secret 無し) では省略する |
| `oauth_providers.<name>.scopes` | []string | `[]` | 認可 URL (loopback/manual) / device authorization request (device) に送る scope。`grant: authorization_code` (既定) のリフレッシュ (`grant_type=refresh_token`) では送らない — 一方 `grant: client_credentials` の場合は毎回のリクエストに送る (Azure AD 等が必須とするため。下記参照) |
| `oauth_providers.<name>.flow` | string (enum) | 省略可 (`""`) | `boid secret oauth login <service>` (PR3、下記) が使う初回認証フロー: `device` / `loopback` / `manual`。未設定の場合 `boid secret oauth login` はエラーになるが、`boid secret set` での refresh_token 手動投入は引き続き可能 — 既存の PR2 時点の config.yaml がこのフィールド無しでも壊れず動き続けるための互換性 |
| `oauth_providers.<name>.authorization_endpoint` | string | `flow: loopback`/`manual` で必須 | provider の OAuth2 authorization endpoint (RFC 6749 §3.1)。`https` 必須 |
| `oauth_providers.<name>.device_authorization_endpoint` | string | `flow: device` で必須 | provider の RFC 8628 §3.1 device authorization endpoint。`https` 必須 |
| `oauth_providers.<name>.authorize_params` | map[string]string | 省略可 | 認可リクエストに追加するパラメータ (loopback/manual は authorize URL のクエリ、device は device authorization request の form field)。`client_id`/`redirect_uri`/`state`/`code_challenge` 等プロトコル予約名は使用不可 (config load 時にエラー)。Google の `access_type`/`prompt` が代表例 — 上記参照 |
| `oauth_providers.<name>.grant` | string (enum) | `authorization_code` | このプロバイダが使う RFC 6749 grant: `authorization_code` (既定、3-legged/delegated — 上記 `flow` 三段のいずれかで取得した refresh_token を使い続ける) / `client_credentials` (RFC 6749 §4.4、2-legged/app-only、Service Principal 向け)。`client_credentials` を指定した場合、`client_secret_key` は必須 (confidential client 必須、RFC 6749 §4.4.2) かつ `flow` は設定不可 (client_credentials に login flow という概念は無い) — いずれの違反も config load 時にエラーになる |

**平文の client_secret をここに書いてはいけません**。`client_secret_key` は secret store への参照名に過ぎず、実値は `boid secret set <key> <value>` で登録します。

### `grant: client_credentials` (Service Principal / app-only 認証)

`grant: client_credentials` を指定した provider は、`flow` の3段構え (device/loopback/manual) を一切使いません — ユーザ認可のステップ自体が無い2-legged認証だからです。`refresh()` は token endpoint に `grant_type=client_credentials` + `client_id` + `client_secret` (`client_secret_key` から解決) + `scope` を直接 POST するだけで、`refresh_token` という概念も無いため `boid secret oauth login` の対象にもなりません (`LoginManager.StartLogin` を呼ぶと専用のエラーで即座に失敗します)。

必要な secret store への投入は `client_secret_key` の実値のみです:

```bash
boid secret set -n <workspace-namespace> az_sp_client_secret
```

Azure AD (Entra ID) の client_credentials では `scope` に `https://<resource>/.default` 形式を指定する必要があります (v2 エンドポイントは1リソースにつき1つの scope しか受け付けないため、対象 API ごとに `oauth_providers` エントリを分けます)。`client_secret` には有効期限があるため、失効時は token endpoint 側の `invalid_client` (`AADSTS*` コード) がデーモンログにのみ出力されます — サンドボックス側には upstream の実 URL やエラー詳細は返しません。

### daemon 単一リフレッシャ + 先回りリフレッシュ

refresh token をローテーションする provider (freee が代表) では、複数クライアントが独立に refresh すると古い token を握った側が失敗するレースがあります。API gateway はこのレースを構造的に避けるため、daemon 内の単一の `OAuth2TokenSource` (`internal/apigateway/oauth2.go`) だけが token endpoint を叩きます:

- **先回りリフレッシュ**: `expires_at` の 5 分手前 (既定値、`OAuth2TokenSource.RefreshMargin`) になったら次回アクセス時に自動でリフレッシュします。upstream からの 401 を見てからリアクティブにリフレッシュ・リトライする経路は PR2 のスコープ外です (無バッファストリーミング転送では body を再送できないため)。
- **同時リクエストの集約**: 同じ (workspace namespace, provider) への同時アクセスは 1 回の token endpoint 呼び出しに集約されます (singleflight)。
- **永続化順序**: token endpoint から応答を受けたら、まず (ローテーションされていれば新しい) `refresh_token` を secret store に書き込み、その書き込みが成功して初めて `access_token` を使用・キャッシュします。ローテーション型 provider は古い `refresh_token` を新しいものへの交換と同時に無効化するため、新しい値の永続化に失敗した状態でそのまま使うと grant 自体を失います。

secret store に保持される値は namespace ごと (workspace ごと) に以下の 2 キーです (`internal/apigateway.OAuthSecretKey` の命名規約、直接 `boid secret get/set` で参照可能):

- `oauth2:<provider>:refresh_token`
- `oauth2:<provider>:access_token_cache` (リフレッシュ結果のキャッシュ、`{"access_token":"...","expires_at":<Unix秒>}` の JSON。access_token と expires_at を1キーにまとめているのは、2キーに分けると片方だけの書き込み失敗で不整合な組が残るのを防ぐため — codexレビューで指摘)

**account 修飾時のキー形** (上記「credential account 修飾」節、
docs/plans/api-gateway-credential-accounts.md D6): `$BOID_API_BASE/freee@ubs/...`
のように account を付けてリクエストすると、この 2 キーは
`oauth2:<provider>@<account>:refresh_token` /
`oauth2:<provider>@<account>:access_token_cache` になります
(`internal/apigateway.credentialID.secretPrefix()` が `<provider>@<account>`
を組み立てる唯一の箇所)。account 無しの場合と完全に独立したキーなので、
`boid secret oauth login freee` (account 無し) と将来の
`boid secret oauth login freee --account ubs` は互いに影響しません。
singleflight・in-process キャッシュ (`memCache`) のキーも同様に
account ごとに分離されており、あるアカウントのリフレッシュが別アカウントの
access token を返すことはありません。フォールバックも一切ありません — 上記
「credential account 修飾」節の D3 と同じく、`oauth2:freee@ubs:refresh_token`
が無ければ `oauth2:freee:refresh_token` へは決して落ちず、502 になります。

一方 `oauth_providers.<name>.client_secret_key` は account を付けても
**修飾されません** — 同じ OAuth アプリ (provider) を複数アカウントで共有する
前提のため、`client_secret_key` の解決キーは account の有無にかかわらず
常に config.yaml に書いたとおりの値です。

### `boid secret oauth login <service>` — 初回認証 (PR3)

`oauth_providers.<name>.flow` を設定した provider は `boid secret oauth login <service>` (`<service>` は `services.<name>` の名前、`oauth_providers` の provider 名ではない点に注意) で初回の refresh_token grant を取得できます。役割分担は「CLI = ブラウザ側、daemon = Web サーバ側」(docs/plans/api-gateway.md §7): PKCE verifier / state / device_code は daemon 側 (`internal/apigateway/login.go` の `LoginManager`) のみが保持し、デスクトップ機 (CLI を実行する側) には平文トークンも client_secret も一度も渡りません。

flow は 3 種類、`oauth_providers.<name>.flow` の値でどれが動くか決まります:

- **`device`** (Microsoft/GitHub 等): `boid secret oauth login <service>` が user_code と verification URI を表示するだけで、実際の token endpoint ポーリングは daemon が裏で行います。表示された URL を別の端末・スマホで開いて code を入力すれば、CLI 側は自動的に完了を検知します。
- **`loopback`** (Google/Atlassian 等): CLI がローカルの動的ポート (`127.0.0.1:0`, RFC 8252 §7.3) で listen し、表示された authorize URL をブラウザで開いて同意すると、そのブラウザからの redirect が CLI のローカル listener に着地して自動完結します。ブラウザが開ける環境で CLI を実行する必要があります。
- **`manual`** (freee 等、OOB のみのプロバイダ): 表示された authorize URL をブラウザで開いて同意すると、画面に code が直接表示されるので、それを CLI のプロンプトに貼り付けます。

```bash
boid secret oauth login freee --namespace <workspace-namespace>
```

`--namespace` (既定 `default`) は `boid secret set/get` と同じ workspace namespace の指定です。`--timeout` (既定 5 分) は「ユーザーがブラウザ/別端末での認証を完了するまで CLI がどれだけ待つか」で、daemon 側のセッション有効期限 (loopback/manual は 10 分、device は provider が申告した `expires_in`) とは独立です。

### 初回 grant の手動投入 (login flow を使わない場合)

`boid secret oauth login` を使わず、`boid secret set` で `refresh_token` を直接投入することでも動作確認・dogfood ができます (`flow` が未設定の provider ではこちらが唯一の経路です):

```bash
boid secret set -n <workspace-namespace> oauth2:freee:refresh_token
# プロンプトで refresh_token の値を貼り付け
```

`access_token`/`expires_at` は未設定のままで構いません — 最初のリクエストで `OAuth2TokenSource` が自動的にリフレッシュして埋めます。`client_secret_key` を設定した場合は、それも別途 `boid secret set` で登録してください。

### workspace 単位・命名の考え方

`oauth_providers` は daemon 全体で共有される provider 定義 (token endpoint / client_id / client_secret_key の参照) です。secret store 側の実値 (`refresh_token`/`access_token`/`expires_at`) は git gateway/services と同じ workspace namespace 機構でスコープされるため、同じ provider 定義を複数 workspace で共有しつつ、workspace ごとに別アカウントの refresh_token を持たせることができます (マルチアカウントの基盤)。

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
