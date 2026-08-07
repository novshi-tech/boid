# 汎用 API gateway (認証注入リバースプロキシ) + OAuth2 対応 計画

ステータス: PR1 (gateway 本体、static 注入のみ) マージ済み (2026-08-03、#898)。PR2 (OAuth2 TokenSource) マージ済み (2026-08-03、#899)。PR3 (login flow) マージ済み (2026-08-04)。PR4 (client_credentials grant、§6-補) は設計中・未着手 (2026-08-08)。
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
- 上記は全て authorization_code grant (delegated) 前提の記述。client_credentials
  grant (Service Principal / app-only) は refresh_token を持たないため永続化順序
  制約が発生せず、SecretStore に保持するのは access_token/expires_at のみになる
  — 詳細は §6-補。

### 6-補. client_credentials grant (Service Principal / app-only 認証) 対応

**未実装 (2026-08-08 論点追加)**。az の REST 移行 ([[host-commands-shrink-to-gh-decision]]) で
Azure DevOps / Application Insights / Storage を叩く際、**ユーザ本人の Entra ID 権限を
そのままエージェントに渡すのは権限が大きすぎる**ため、エージェント専用の Service
Principal (client credentials grant, RFC 6749 §4.4) を用意する方針 (2026-08-08 nose)。

現行の §6/§7 実装は **3-legged (delegated) 前提で一本化されている**: `OAuth2TokenSource`
の `refresh()` は `grant_type=refresh_token` のみを送り (`internal/apigateway/oauth2.go`
`callTokenEndpoint`)、`LoginManager` の3フロー (device/loopback/manual) はいずれも
「ユーザに一度認可させて refresh_token を発行させる」ための手段でしかない。
client_credentials は **ユーザ認可のステップ自体が無い** 2-legged 認証で、構造が別物:

- token endpoint に `grant_type=client_credentials` + `client_id` + `client_secret`
  (confidential client 必須) + `scope` を直接 POST するだけ。認可 code も
  redirect_uri も PKCE も関与しない
- `refresh_token` という概念が無い。access_token の有効期限が切れたら同じリクエストを
  投げ直すだけ — ローテーション型 refresh_token 特有の「書き込み順序厳守」制約
  (§6 永続化順序) がそもそも発生しない
- したがって `boid secret oauth login <service>` の出番が無い。必要な入力は
  `client_secret` の投入のみで、これは既存の `boid secret set` (namespace 付き) で
  そのまま足りる。**tenant は新フィールドでも secret でもなく、既存の
  `token_endpoint` の URL に含める** (`https://login.microsoftonline.com/<tenant-id>/
  oauth2/v2.0/token`) — Entra ID の client_credentials は `common`/`organizations`
  テナントを受け付けないため、tenant ID か検証済みドメインでの固定が必須になる
  (§Entra ID 特有の注意点、後述)

**設計方針 (Opus レビュー 2026-08-08 反映済み、実装時に最終化)**:

- `oauth_providers.<name>` に新フィールド `grant` (enum: `authorization_code`
  [既定・現状の3フロー全てが該当] / `client_credentials`) を追加。既存の `flow`
  フィールド (初回認可の**手段**: device/loopback/manual) とは直交する軸として
  **分離のまま保つ** (`flow` の4つ目の値にする案は採らない — `ValidLoginFlows`
  (`oauth2.go:122-131`) は「config 受理と `StartLogin` の switch が絶対に drift
  しない」ための仕組みであり、`client_credentials` を flow 値に混ぜると「config
  load では受理されるが `StartLogin` が必ず reject する値」が生まれてこの前提を
  壊す。型の消費者も別: `LoginFlow` は `LoginManager` 専用、`grant` は
  `OAuth2TokenSource.refresh()` というランタイム経路が読む)
- **`grant` と `flow` の排他は `StartLogin` 時点ではなく config load 時
  (`validateOAuthProviderConfig`) に拒否する**。既存の flow-conditional な必須
  チェック (`authorization_endpoint` 等) は全て config load 時にやっており、
  そこに揃える。`StartLogin` 側には defensive なガードを残すが、既存の default
  分岐が出す「flow を device/loopback/manual に設定しろ」というメッセージを
  そのまま client_credentials provider に出すと有害な誤誘導になるため、専用の
  エラーメッセージにする
- `grant: client_credentials` の場合 `client_secret_key` は必須 (public client では
  client_credentials grant 自体が成立しない — RFC 6749 §4.4.2)。**空文字が
  "設定されている" ケースは config load 時に fail-fast する** (未設定キーは
  `SecretStore.Get` が `sql.ErrNoRows` を返すので `refresh()` の `err != nil` で
  既に弾かれるが、空値だけは素通りして分かりにくい 502 になる)
- **分岐箇所は `AccessToken()` ではなく `refresh()`** (refresh_token の解決と
  エラー文言は `refresh()` 冒頭にある、`oauth2.go:537-557`)。client_credentials
  では refresh_token の存在チェックをスキップし `callTokenEndpoint` の POST body
  を `grant_type=client_credentials` に切り替える。先回りリフレッシュ・
  singleflight・access_token キャッシュの key 設計 (`namespace + provider`) は
  grant 非依存でそのまま使える。**ただし変更は「POST body 構築と persist の
  2箇所」には閉じない** — 少なくとも以下も伴う:
  - `callTokenEndpoint` には「scope は絶対に送らない」という明示的な設計判断が
    ある (`oauth2.go:822-827`、根拠は「PR2 の対象 provider が誰も scope を
    必要としない」)。Azure の client_credentials は `scope=.../.default` が
    **必須**なので、この判断ごと更新する
  - `persistGrant` に「refresh_token を書かない」を明示的に伝える経路を足す
    (現状は応答に refresh_token が無ければ自然にスキップされるが、RFC 6749
    §4.4.3 に反して refresh_token を返す IdP や provider 切り替え時の残留を
    考えると明示フラグが要る)
  - config 層一式: `internal/config/schema.go` の `oauth_providers.*.grant`
    (KindEnum + EnumValues)、`OAuthProviderConfig` の yaml フィールド、
    `validateOAuthProviderConfig`、`APIGatewayOAuthProviders` へのコピー、
    `ValidGrants` 相当の drift 防止ミラー変数、
    `docs/ja/reference/config-yaml.md` の provider 表 (380-387 行) と
    「リフレッシュでは scope を送らない」記述の更新
  - singleflight の存在理由が変わる点を明記する: §6 では「ローテーション型
    refresh_token のレース根絶」という**正しさ**の要件だが、client_credentials
    では refresh_token 自体が無いのでレースが起きようがなく、単なる効率最適化
    (token endpoint への重複リクエスト削減) に降格する
- **未決の運用論点 (論点表 #8 参照)**: `client_secret_key` は既存設計上
  namespace (workspace) 付きの SecretStore 解決 (`t.resolver(namespace,
  cfg.ClientSecretKey)`) — **暫定結論: 案A (namespace 付きのまま、複数 workspace
  で共有する SP は workspace ごとに同じ値を `boid secret set` する) を採る**。
  理由は論点表 #8 参照。access_token キャッシュも同じ namespace 分離に従う
  (共有 SP でも workspace 数だけ token endpoint を叩き、同一の app-only
  access_token が workspace 数だけ複製される — 案A を採る以上これは一貫した
  挙動として許容する)

#### Entra ID (Azure AD) 特有の注意点

- **tenant は `token_endpoint` の URL 自体に含まれる** (新フィールド不要)。
  `common`/`organizations` テナントは client_credentials で拒否されるため、
  tenant ID か検証済みドメインでの固定が必須
- **`scopes` は `https://<resource>/.default` 形式を 1 リソースにつき 1 個
  だけ**指定できる (v2 エンドポイントの client_credentials は複数リソースの
  scope を混在できない)。対象 API (Application Insights
  `https://api.applicationinsights.io/.default` 等) ごとに `oauth_providers`
  エントリを分ける必要がある点は §OAuth2 対応の他プロバイダと同型
- **Application permission にはテナント管理者の同意が必須**。§7 の
  「避けられない一回きりの手作業 (OAuth アプリ登録)」と同種の手作業が
  Service Principal 側にもあり、nose 自身がテナント管理者権限を持っているか
  が前提になる
- **client_secret には有効期限がある** (最大24ヶ月、既定はもっと短い)。切れた
  瞬間に対象 provider の全 job が 502 になる。§5 の「upstream 401 の notify」
  は upstream からの 401 専用の通知経路であり、token endpoint 側の
  `invalid_client` (client_secret 失効) はこれとは別経路 —
  `postFormToTokenEndpoint` は Azure の実診断情報 (`AADSTS*` コード, IdP の
  `error_description`) をサンドボックス側には返さずログにしか出さない設計
  (漏洩防止として正しい) ので、**運用手順として「daemon ログで `AADSTS*` を
  見る」ことを明記する** — SP の client_secret ローテーション手順は別途整備が要る
- provider の identity (tenant/client_id) を差し替えたら、memCache は config
  変更時の daemon 再起動 (`ReloadRestartRequired`) で消えるが、**secret store
  側の access_token キャッシュは残る**ので `boid secret unset` で明示的に消す
- 証明書ベースの client assertion (RFC 7523) や workload identity federation
  への対応は明示的にスコープ外とする (client_secret のみを初期スコープとし、
  必要になったら別途検討)
- Azure DevOps は PAT (Basic auth、既存 kind で対応可) でも SP トークン
  (`scope: 499b84ac-1321-427f-aa17-267ca6975798/.default`) でも叩けるが、
  **PAT はユーザ本人の権限で発行されるものなので「本人権限を渡さない」という
  本節の動機とは両立しない**。Azure DevOps も SP トークンに統一するか、
  PAT を使う範囲を「SP で代替不能な操作に限る」と明示的に切り分ける
  (§8 CLI整理表の az 行を参照、要更新)
- SP には最小権限 (readonly な app role) のみ付与を推奨。§3 の readonly 写像
  (task.readonly → GET/HEAD) はメソッドしか止めないため、権限最小化は
  Azure 側の role assignment でも二重にかけておく

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

**この節・下表は authorization_code grant (delegated) 限定**。client_credentials
grant (Entra ID Service Principal 等) には login flow という概念自体が無い —
§6-補参照。

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
| aws / gh | **host command 存続** | 署名系 (SigV4) / 独自認証エコシステム。gateway では原理的に代行不能 |
| az | **API gateway (client_credentials)** ([[host-commands-shrink-to-gh-decision]]) | ARM には踏み込まない。対象は Azure DevOps / Application Insights KQL / Azure Storage 分析。ユーザ本人の Entra ID 権限ではなくエージェント専用 Service Principal (client_credentials grant) を使う方針のため、§6-補の対応が前提 (2026-08-08 nose、未実装)。**Azure DevOps の PAT は使わない** — PAT はユーザ本人の権限で発行されるため「本人権限を渡さない」という動機と矛盾する。SP トークン (`scope: 499b84ac-1321-427f-aa17-267ca6975798/.default`) に統一する |
| atlassian コマンド | **MCP へ退役** | workspace 単位 $HOME 分離によりマルチアカウント認証が MCP で成立するようになった (Jira は workspace 単位 MCP で実現済み、詳細は運用メモ参照) |
| google コマンド (Gmail 等) | **MCP 不可・gateway (google-cli) 維持** | 当初「workspace 単位 $HOME 分離で MCP でもマルチアカウント成立」と見込んだが前提が誤りだった。claude.ai 経由の MCP Connector はアカウント単位のグローバル紐付けであり、boid workspace の $HOME 分離とは無関係 — workspace ごとに別 Google アカウントを繋ぐことはできない。ワークスペース別アカウントが要件の Gmail 等は gateway (google-cli 経由、SecretStore 管理) を維持する。gateway 化自体は本 repo 外の API スキル開発プロジェクトで行う |
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
- **PR4 (未着手、2026-08-08 追加): client_credentials grant** — §6-補。
  `oauth_providers.*.grant` フィールド、`AccessToken()` の grant 分岐、
  `LoginManager` のガード (client_credentials プロバイダへの login flow 拒否)。
  az (Service Principal) の REST 移行がこの PR に依存する。

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
| 8 | client_credentials の `client_secret_key` を namespace (workspace) 付きで解決するか、provider 単位グローバルにするか | **暫定結論 (2026-08-08、Opus レビュー反映): 案A** — namespace 付きのまま、複数 workspace で共有する SP は workspace ごとに同じ値を `boid secret set` する。global 自動 fallback (案B) は採らない: 同じ fallback が static な `secret_key` にも波及すると「未設定 workspace が黙って別 workspace のトークンを使う」というマルチアカウント設計 (config-yaml.md が明記する意図) の真逆になる。namespace 分離自体は client_credentials では実効的なセキュリティ境界ではない (共有 SP はどの workspace から取っても同じテナント権限のトークン) が、silent fallback を汎用機構として入れるコストの方が大きいため案A を優先。将来重複が実運用で痛くなったら**案C: `client_secret_namespace` を config に明示指定できるようにする** (暗黙 fallback ではなく明示参照) を検討。access_token キャッシュも同じ namespace 分離に従わせる (workspace 数だけ複製される想定) |
