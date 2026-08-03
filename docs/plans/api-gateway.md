# 汎用 API gateway (認証注入リバースプロキシ) + OAuth2 対応 計画

ステータス: PR1 (gateway 本体、static 注入のみ) マージ済み (2026-08-03、#898)。PR2 (OAuth2 TokenSource) マージ済み (2026-08-03、#899)。PR3 (login flow) 実装完了・レビュー中 (2026-08-03)。
作成日: 2026-08-03
親ドキュメント: [git-gateway-cutover.md](git-gateway-cutover.md) — 本計画は git gateway の認証注入モデルを git 以外の HTTP API へ汎用化する。認証情報一元管理の観点では [host-command-contract.md](host-command-contract.md) の後継でもある。

---

## 目的

host command 機構が実質的に提供している 2 つの効果を、コマンド実装なしの
config 宣言だけで任意の HTTP API に対して提供する:

1. **workspace の許可リストにない外部ドメインへの通信を選択的に許可する**
2. **シークレットをサンドボックスに曝露することなく、認証付き通信を可能にする**

git gateway (`internal/gitgateway/`) はこれを git smart HTTP 限定で既に実現している。
本計画はその姉妹品として、daemon 側に汎用の認証注入リバースプロキシ
(以下 **API gateway**) を建てる。

動機は費用対効果: atl / freee / msgraph のようなサービス固有コマンドはまだ良いが、
**自社開発アプリのテスト・運用 API をそのつどコマンド化して host command として
配置するのは現実的でない**。API gateway なら自社アプリが増えても
config 1 ブロック + secret 1 個で済む。

さらに、CLI 群整理の元動機が認証情報の一元管理だったため、認証を boid が
引き受けることで多くのサービス固有コマンドが退役可能になる (§CLI 整理への波及)。

---

## 検討済み・却下した代替案

- **egress proxy (`internal/sandbox/proxy.go`) の MITM 化**: 現行 egress proxy は
  HTTPS を CONNECT トンネル (hijack した素の TCP パイプ) で素通ししており、
  ヘッダ注入には boid 独自 CA によるTLS 終端が必要になる。CA の信頼配布が
  ツールチェーンごとに異なり (git / Node / Go / Python / curl)、cert pinning
  クライアントは壊れ、proxy が全トラフィックを復号できる箱に格上げされる。
  技術的には可能だが費用対効果とセキュリティ境界の両面で却下 (2026-08-03 nose 合意)。
- **Web UI を OAuth redirect URI にする案**: boid はパーソナルオーケストレータで
  ユーザごとに `web.public_url` が異なるため、認可サーバ側への redirect URI 登録が
  利用者ごとの手作業になる。公開エンドポイントを redirect 先にする分の攻撃面増加も
  ある。loopback redirect (RFC 8252 §7.3) を採用して却下 (2026-08-03 nose 合意、
  §初回認証)。
- **`boid fetch` builtin の拡張 (`boid api <service> <path>`)**: broker 経由なので
  配線は最安だが、boid CLI 経由でしか叩けず curl / SDK / テストコードから
  透過的に使えない。そもそも `boid fetch` は sandbox 内で無効化されている
  WebFetch ツールの代替 (web ページ読み取り、`docs/plans/sandbox-web-access.md`)
  であり、API 呼び出しとは目的が別物 — 拡張・統合の対象にしない。
  糖衣 `boid api` 自体も作らない (論点 6 で確定)。

---

## 前提となる決定事項 (2026-08-03 nose 合意)

- **方式は git gateway と同型の reverse proxy**。sandbox → gateway は workspace
  network 内の平文 HTTP (または既存の mTLS listener 相当)、gateway → upstream は
  daemon 発の HTTPS。upstream への接続が daemon 発である時点で workspace の
  allowed_domains は関与せず、ゲートは service registry 側の宣言に移る。
- **service 定義は daemon 側 config.yaml、workspace 単位の有効化も daemon 側設定**。
  project.yaml には置かない — project.yaml はリポジトリ由来であり、そこに
  credential アクセス権限を書けるようにすると「repo に push できる人 =
  認証情報を使える人」になり信頼境界が repo 側に漏れる。有効化の形は
  workspace 単位 allowed_domains (floor + workspace 加算) の鏡写しとする。
- **host command は退役させない**。線引きは技術的必然による:
  注入可能な認証 (Bearer / Basic / 固定ヘッダ / query param) は gateway、
  リクエスト署名系 (AWS SigV4 等、secret なしでは署名不能) と独自認証
  エコシステム持ち CLI (aws sso / az login / gh) は host command 存続。
- **`task.readonly` を HTTP メソッドに写像する**: readonly task の job は
  GET / HEAD のみ許可。テスト・運用 API 相手の事故防止の第一ゲート。
- **OAuth2 の refresh は daemon を単一リフレッシャとする** (§OAuth2 対応)。
- **初回認証は device flow / loopback リモート CLI / manual paste の三段構え**
  (§初回認証)。
- **Google の device flow は認可可能な scope がかなり絞られている**
  (nose 実体験でハマった落とし穴)。device flow が「ある」ことと「必要な scope で
  使える」ことは別 — Google は device flow を持つが loopback 経路を既定とする。

---

## 本計画で確定する設計

### 1. ルーティングと sandbox からの見え方

git gateway の `/j/<job-token>/...` (`internal/gitgateway/route.go`) と並ぶ
route namespace を切る:

```
/api/<job-token>/<service>/<path...>[?query]
```

- `<service>` は config 宣言された論理名。upstream の実 URL は sandbox からは
  見えない (見せる必要もない)。
- sandbox からの利用イメージ:

```
curl "$BOID_API_BASE/myapp/v1/users"
```

- job への配布形態は `BOID_API_BASE=https://boid-gateway:<port>/api/<job-token>` の
  env 注入、または `boid job env` 的な sandbox 内 introspection コマンド (論点 2、
  半確定)。token 埋め込み URL 方式自体は git gateway の clone URL と同じ前例。
  curl でも SDK でも base URL 差し替えだけで動くことが host command 方式に対する
  本質的な利点 — **ただし container backend では TLS 証明書の trust に一段注意が要る**
  (実装時の codex レビューで判明、下記注記参照)。
- **TLS trust の注記 (PR1 実装時に判明)**: container backend では gateway listener
  (git gateway と共有) が daemon 内蔵 CA で署名された TLS を使うため、標準的な
  curl/Python 等は `BOID_API_BASE` に素の `curl` を投げるだけでは証明書検証に失敗する。
  `BOID_API_CA_FILE` (daemon CA の PEM path) を env 注入するので
  `curl --cacert "$BOID_API_CA_FILE" "$BOID_API_BASE/..."` や
  `requests.get(url, verify=os.environ["BOID_API_CA_FILE"])` のように明示的に渡す
  必要がある。Node.js のみ `NODE_EXTRA_CA_CERTS` (Node 公式ドキュメントで「既存の
  root CA 群に追加される」と明記された加算的な変数) を注入しているため
  flag 無しで動く。`SSL_CERT_FILE`/`CURL_CA_BUNDLE` 等の一括置換系変数は
  他の https 通信 (pypi.org 等) の trust を壊すため意図的に使わない。
  「base URL 差し替えだけで動く」は http (TLS 無し) の service、または Node SDK
  に限っては文字通り成立するが、curl/Python 等 + TLS service の組み合わせは
  1 flag の追加が必要、という限定付きの利点として理解すること。
- `boid-gateway` は egress proxy の dotless 許可リスト
  (`internal/sandbox/proxy.go` `isRefusedDotlessTarget`) に既に載っている
  boid インフラ名で、no_proxy 配線も既存のものをそのまま使う。

### 2. service registry (config.yaml)

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
  freee:
    base_url: https://api.freee.co.jp
    auth: { kind: oauth2, provider: freee }   # §OAuth2 対応
```

- `auth.kind` の初期セット: `bearer` / `basic` / `header` / `query` / `oauth2`。
- `secret_key` は git gateway の `HostForgeConfig.SecretKey` と同じく
  SecretStore への参照のみ。平文は config に置かない。
- secret 解決は既存の workspace namespace 機構
  (`internal/gitgateway/credentials.go` の `SecretResolver(namespace, key)`) を
  そのまま通す — workspace ごとに同じ service 名で別 credential を持てる
  (マルチアカウントの基盤)。

### 3. 認可モデル

三段のゲート:

1. **job token** — 既存 `gitgateway.Registry` と同型の register / authorize /
   unregister ライフサイクル (dispatch 時登録、job 完了時抹消)。token から
   workspace namespace を引く経路も同じ。
2. **workspace 単位の service 有効化** — daemon 側 workspace 設定。
   allowed_domains の floor + 加算方式を鏡写しにする (floor には全 workspace 共通で
   許可する service を置ける)。job token 登録時に workspace の有効 service 集合を
   token entry に焼き込む。
3. **`task.readonly` → メソッド制限** — readonly なら GET / HEAD のみ。

将来拡張 (初期スコープ外): service ごとの method / path prefix allowlist。
confused deputy 対策の絞り込みは後から足せる形にしておき、初期は
service 単位 + readonly 写像で開始する。

### 4. credential 注入: CredentialProvider → TokenSource 一般化

git gateway の `CredentialProvider` は「host → (username, token) を解決して
`SetBasicAuth`」に特化している。API gateway では注入部を kind 別に一般化する:

- `bearer` → `Authorization: Bearer <secret>`
- `basic` → `SetBasicAuth(username, secret)`
- `header` → `<header>: <secret>`
- `query` → クエリパラメータ付与
- `oauth2` → `Authorization: Bearer <access_token>` (access_token は TokenSource が
  供給、§OAuth2 対応)

static kind 群は SecretStore 読みの薄い wrapper であり、`Resolve` (pre-check) +
`Inject` (apply) の 2 段構成・fail-fast 502 の意味論は
[gitgateway-credential-fail-fast.md](gitgateway-credential-fail-fast.md) の
確立済みパターンを踏襲する。

### 5. 転送の衛生

- **無バッファストリーミング転送** — `httputil.ReverseProxy`、git gateway と同じ。
- **path 正規化** — `..` / 絶対 URL 混入で `base_url` の外に出られないことを
  parse 層で保証する (URL エンコード済み `%2e%2e` 含む)。
- **inbound ヘッダ剥がし** — sandbox 発の `Authorization` / `Cookie` /
  `Proxy-Authorization` は upstream に転送しない (注入とバッティングさせない +
  sandbox が別 credential を密輸する経路を塞ぐ)。
- **SSE 対応** — `FlushInterval: -1` (負値) で即時 flush。テスト・運用 API は
  SSE を使いがちなので初期スコープに含める。
- **upstream 401 の notify** — git gateway の `NotifyUpstreamAuthFailure` 相当を
  流用 (token 失効の可視化)。

### 6. OAuth2 対応

#### daemon 単一リフレッシャの原則

refresh token をローテーションするプロバイダ (freee が代表) では、複数
クライアントが独立に refresh すると古い token を握った側が死ぬレースがある。
リフレッシュする主体を daemon ただ一つに絞ることで、このレースを構造的に消す。
認証一元管理の動機に最も直接効く部分。

#### TokenSource の挙動

- SecretStore (workspace namespace 付き) に保持するもの: refresh_token、
  access_token、expires_at。provider 定義 (token endpoint、client_id、scopes、
  client_secret があれば) は config 側。
- **先回りリフレッシュ**: expires_at より margin 手前で更新。同時リクエストは
  singleflight で 1 回にまとめる。
- **永続化順序の厳守**: token endpoint 応答を受けたら**まず新 refresh_token を
  DB に書き、書き込み完了後に使用する**。ローテーション型では、書き込み前に
  crash すると grant ごと失う — ここだけは絶対に順序を崩さない。
- **upstream 401 での reactive refresh + リトライは初期スコープ外**:
  無バッファストリーミングでは body を再送できない。先回りリフレッシュのみで
  運用開始し、必要になったら「小 body に限り buffer してリトライ」を別途検討。

### 7. 初回認証 (`boid secret oauth login <service>`)

#### 役割分担: CLI = ブラウザ側、daemon = Web サーバ側

CLI リモート接続 ([cli-remote-connection.md](cli-remote-connection.md)) を前提に、
**ブラウザのあるデスクトップで CLI を実行し、headless の daemon に grant を
届ける**。役割分担はブラウザ / Web サーバの関係と同型になる:

1. CLI が daemon に「service X のログイン開始 (workspace 指定込み)」を要求。
   daemon が **PKCE verifier (RFC 7636) を生成して手元に保持**し、challenge と
   state を含む認可 URL の素材を CLI に返す。
2. CLI が loopback の動的ポート (`127.0.0.1:0`) で listen し (RFC 8252 §7.3)、
   redirect_uri を付けた認可 URL でブラウザを開く。
3. ユーザがブラウザで同意 → 認可 code が CLI の loopback listener に着地。
4. CLI は code を既存の認証済みチャネルで daemon に転送するだけ。
5. daemon が verifier を添えて code 交換し、refresh_token をその場で
   workspace namespace の SecretStore に永続化。

この形の性質:

- **デスクトップ機には平文トークンも client_secret も一度も存在しない**。
  経由するのは認可 code のみで、daemon が握る verifier なしには交換できない。
- state 検証は daemon 側の pending login session と突き合わせる (CSRF 対策)。
- 初回取得とリフレッシュの token endpoint 呼び出しが daemon 側で一本化される。

#### flow の三段構え

優先順位: **device flow → loopback リモート CLI → manual paste**。

| プロバイダ | device flow (RFC 8628) | 既定経路 | 備考 |
|---|---|---|---|
| Microsoft | あり | device | headless 単体で完結 |
| GitHub | あり | device | 同上 |
| Google | あり (**scope 制限が厳しい**) | **loopback** | nose 実体験: 必要 scope が device flow で認可不能でハマった |
| Atlassian (3LO) | なし | loopback | |
| freee | なし | **manual (OOB)** | **PKCE 非対応・loopback redirect 非対応** (2026-08-03 nose レビュー)。`urn:ietf:wg:oauth:2.0:oob` を redirect URI に登録すると認可後にブラウザへ code を直接表示する仕様のみ。ローテーション型 refresh token の代表格 |

- manual paste は最後の保険であると同時に、**freee のような OOB
  (`urn:ietf:wg:oauth:2.0:oob`) しか持たないプロバイダの正規経路**でもある:
  認可 URL を表示 → プロバイダが認可後の画面に code を直接表示 → ユーザが
  CLI に貼り付け → CLI が daemon に転送、以降は loopback 経路と同じ
  (daemon が code 交換 + 永続化)。redirect が発生しないため state 検証と
  loopback listener の機構は使わない。
- **PKCE 非対応プロバイダ (freee) では「daemon が verifier を握るので code
  単体は無価値」という §役割分担 の防護が成立しない**。代わりに confidential
  client の client_secret を daemon 側にのみ置くことで同等の性質 (code を
  見てもデスクトップ側では交換不能) を担保する。公開クライアント + PKCE
  優先の原則は対応プロバイダに限る。
- device flow の polling は daemon 側で行う (token endpoint を叩くのは常に
  daemon、の原則に揃える)。CLI は user_code / verification URL の表示のみ
  (`internal/qrterm` で QR 表示も可)。
- 避けられない一回きりの手作業: プロバイダごとの OAuth アプリ登録
  (client_id 取得)。public client + PKCE で client_secret 不要なプロバイダは
  それを優先する。

#### PR3 実装時の追加決定事項 (2026-08-03)

計画時点で「実装時に最終化」としていた点、および実装中に判明した追加の設計判断:

- **config.yaml スキーマ**: `oauth_providers.<name>.flow` (enum: device/
  loopback/manual) + flow ごとに必須の `authorization_endpoint`
  (loopback/manual) / `device_authorization_endpoint` (device)。**`flow` は
  任意項目** — PR2 時点の config.yaml (token_endpoint/client_id/
  client_secret_key のみ、refresh_token は `boid secret set` で手動投入)
  はこのフィールド無しでも変更なく動き続ける。login flow を使うプロバイダ
  だけ明示的に追加する。
- **`authorize_params` (map[string]string)**: 認可リクエストに追加する
  provider 固有パラメータの汎用エスケープハッチ。動機は Google:
  `access_type=offline` を付けないと refresh_token がそもそも発行されず、
  かつ一度同意済みだと `prompt=consent` を付けない限り 2 回目以降の
  ログインで refresh_token が再発行されない。Google 固有の挙動を
  gateway 本体にハードコードせず config 側の宣言に押し出した。
  `client_id`/`redirect_uri`/`state`/`code_challenge` 等プロトコル予約名は
  config load 時に拒否 (PKCE/state の機構を上書きされる事故を防止)。
- **CLI は毎回ローカル listener を開いてから daemon に問い合わせる**:
  `boid secret oauth login <service>` はどの flow になるか事前に分からない
  ため、まず `127.0.0.1:0` で listen してから `redirect_uri` 込みで
  daemon に開始リクエストを送る (device/manual では単に使われず即座に
  close される)。往復を 1 回に抑えるための簡略化。
- **refresh_token 未取得は login 失敗として扱う**: token endpoint の応答に
  `refresh_token` が無く、かつ該当 (namespace, provider) に既存の
  refresh_token も無い場合、access_token 単体の取得は
  「daemon が二度とリフレッシュできない grant」を意味するため
  `boid secret oauth login` 自体を失敗として報告する (access_token
  自体は取得できているにもかかわらず)。Google の「2 回目以降は
  refresh_token を返さない」挙動を `authorize_params` で回避できなかった
  場合に、原因不明のまま数十分後に静かに失敗する事故を防ぐための
  フェイルファスト。
- **daemon 単一 `LoginManager` は既存の `OAuth2TokenSource` インスタンスを
  再利用**: login で得た grant の永続化 (`persistGrant`) はリフレッシュと
  完全に同じ経路・同じ memCache を共有する — login 直後の最初のリクエスト
  が別プロセス内状態の不整合で無駄な再リフレッシュを起こさない。

### 8. CLI 整理への波及 (退役方針)

認証を boid が引き受けた後の各サービスの行き先:

| 対象 | 行き先 | 理由 |
|---|---|---|
| aws / az / gh | **host command 存続** | 署名系 (SigV4) / 独自認証エコシステム。gateway では原理的に代行不能 |
| google / atlassian コマンド | **MCP へ退役** | workspace 単位 $HOME 分離によりマルチアカウント認証が MCP で成立するようになった |
| bitbucket | **API gateway 直叩き** | MCP なし。git gateway が SecretStore に持つ API token は REST でも Basic auth で使える見込み — service エントリ 1 個で済む可能性が高い |
| freee / board | **API リファレンスのエージェントスキル + gateway** | コマンド実装よりスキルの保守がはるかに軽い。freee は daemon リフレッシャの恩恵を最も受ける。スキルは boid 非依存の一般スキルとして書き、gateway 情報との統合解釈は実験で確かめる (論点 7) |
| 自社開発アプリ | **gateway (本計画の主目的)** | config 1 ブロック + secret 1 個で追加完了 |

**MCP 経路の留意点** (退役判断の軸として明記): MCP の認証トークンは workspace
HOME 内、つまり**サンドボックスから読める場所**に置かれる。本計画の目的 2
(secret 非曝露) は MCP 方式では達成されない。scope の狭いトークンは MCP で
割り切り、漏洩時の痛みが大きい credential は gateway 経由に寄せる。

---

## PR 分割

各 PR が独立して価値を出す順に積む:

- **PR1: gateway 本体 (static 注入のみ)** — route namespace `/api/`、service
  registry config、Registry 認可、workspace 有効化、readonly → メソッド写像、
  転送衛生、`BOID_API_BASE` env 注入。`bearer` / `basic` / `header` / `query` の
  4 kind。これだけで自社アプリ API と bitbucket 直叩きが動き始める。
- **PR2: OAuth2 TokenSource** — `oauth2` kind、daemon 単一リフレッシャ、
  先回り refresh + singleflight、永続化順序。初回 grant は当面
  `boid secret set` で refresh_token を手動投入して動作させる (login flow を
  待たずに freee 等の dogfood が始められる)。
- **PR3: login flow** — `boid secret oauth login <service>`、device / loopback /
  manual の三段、pending session 管理、state / PKCE。

---

## リスク・スコープ外

- **署名系認証 (SigV4 等) は恒久的にスコープ外** — host command の存続領域。
- **confused deputy の残余リスク**: gateway は「エージェントに認証済み API を
  透過的に渡す」機構なので、prompt injection されたエージェントも同じ力を持つ。
  初期の緩和は workspace 有効化 + readonly 写像。method / path allowlist は
  必要が観測されてから足す。
- **WebSocket / gRPC** はスコープ外 (必要が出たら別途)。
- **MCP / スキルへの実退役作業** は gateway 稼働後に別トラックで行う。
- **レート制限・クォータ管理** はスコープ外 (upstream 側の責務とみなす)。

---

## 論点表

2026-08-03 nose レビューで 1 / 3 / 4 / 5 / 6 は確定。2 は方向のみ確定、7 は実験項目として追加。

| # | 論点 | 結論 |
|---|---|---|
| 1 | listener を git gateway と同居させるか別ポートか | **確定: 同居** (path prefix `/j/` と `/api/` で分岐)。mTLS listener も流用 |
| 2 | service 一覧・使い方 (と `BOID_API_BASE`) をエージェントにどう提示するか | **半確定: `boid job env` 的な job スコープ introspection コマンドの方向** — URL に job token が入る以上コマンドが自然 (nose)。ただし**環境情報が複数のコマンドに分散する懸念**があり、既存 introspection 語彙 (`boid project behaviors` 系) への統合、または env 注入 (`BOID_API_BASE` を job プロセス環境へ、新語彙ゼロ) との併用を実装時に最終化。#7 の実験結果と合わせて決める |
| 3 | audit log / timeline 記録の粒度 | **確定**: method + service + path + status を timeline に。body は記録しない |
| 4 | OAuth provider 定義 (token endpoint / client_id / scopes) の置き場 | **確定**: config.yaml の `oauth_providers:` ブロック。client_secret のみ SecretStore 参照 |
| 5 | 有効化設定の CLI 語彙 | **確定**: `boid workspace services add/remove/list <ws> <service>` 系。allowed_domains の既存語彙に揃える |
| 6 | `boid api <service> <path>` 糖衣 builtin を足すか | **確定: 作らない**。`boid fetch` は WebFetch 代替 (web 読み取り) で目的が別物、拡張・統合の対象にもしない |
| 7 | (実験) サービスカタログのスキル統合 | boid 非依存の一般スキル (例: freee API リファレンス) に boid の gateway 情報 (base URL 差し替え + service 名) を追加で渡したとき、エージェントが両者を統合解釈できるかは**やってみないと分からない** (2026-08-03 nose)。PR1 稼働後に実験して #2 と併せて決める。最悪 project ローカルの CLAUDE.md に統合方法を書けば成立する見込み |
