# API gateway: 1 service 複数 credential (account 修飾)

ステータス: PR-1 (path 分割・static kind の account 対応) マージ済み (#1037)。
PR-2 (credentialID・oauth2 の account 対応) マージ済み (#1038)。PR-3
(`boid secret oauth login --account`) マージ済み (#1039)。PR-4
(`services.<name>.require_account`) は本 PR で実装完了 — これで PR 分割は
全て完了。`internal/server/connector_exec.go` の base/account 分割は本 PR
のスコープに含めず、独立の後続タスクへ送った（下記「PR 分割」節参照）。

## 目的

1 つの `services.<name>` 定義に対して、複数の credential セットを切り替えて使えるようにする。

動機は default workspace で複数の Freee アカウントを扱う必要が出たこと。現行の
API gateway は「1 workspace × 1 service = 1 credential」に固定されており、同一
workspace から同じ service を別アカウントで叩く経路が存在しない。

## As-Is

sandbox から gateway を叩いたとき、credential が決まるまでは 3 段ある。

1. `parsePath` (`internal/apigateway/route.go:44`) が
   `/api/<job-token>/<service>/<tail>` を分解する
2. `Registry.Lookup` (`internal/apigateway/registry.go:88`) が job token から
   `Entry` を引く。`Entry.Namespace` (= workspace id) と、その job が触れる
   service 集合がここにある
3. `CredentialProvider.Resolve/Inject` (`internal/apigateway/credentials.go:239`)
   が `(Entry.Namespace, services.<name>.auth.secret_key)` で SecretStore を引き、
   注入する。`auth.kind: oauth2` の場合は
   `(Entry.Namespace, "oauth2:<provider>:refresh_token")`

`Entry.Namespace` は job token に焼き付いていて sandbox からは触れない。これが
workspace 間の credential 分離を構造的に担保している。一方 `<service>` は URL に
出るので sandbox が選べる。

credential セットを識別する軸は `(namespace, service)` の 2 つしかなく、
`namespace` は `meta.SecretNamespace = workspaceID`
(`internal/orchestrator/project_store.go:429`) で workspace と 1:1 に固定されて
いる。「1 workspace 1 アカウント」という前提がここに埋まっている。

## 検討済み・却下した代替案

### 却下 1: アカウントごとに service を増やす (`freee-ubs` service + `freee-ubs` provider)

実装ゼロで今日動くが、API gateway の設計意図と逆行する。namespace という軸が
存在する理由は「同じ service 定義を credential の差し替えで使い回す」ことにあり、
service を複製するならその軸自体が要らない。この設計は採らない。

### 却下 2: secret namespace を階層化する (`<ws>/ubs` + ヘッダ指定)

`Entry.Namespace` を親として、リクエストごとにサブ名前空間を継ぎ足す案。secret
store は namespace カラムが TEXT なので無改造で通るが、**継承のジレンマ**を抱える。

`oauth_providers.<name>.client_secret_key` は同じ namespace から引かれる
(`internal/apigateway/oauth2.go:611`)。この値はアカウントごとに変わるものではない
ので、サブ名前空間に切り替えると「親から継承する」か「全サブに複製する」かの
二択になる。継承ありを選ぶと、サブに `refresh_token` を入れ忘れた状態で切り替えた
とき **黙って親アカウントの credential で書き込む**。会計データでこれは許容できない。
継承なしを選ぶと client_secret をアカウント数だけ複製することになる。

加えて切り替えの粒度が「その workspace の secret 一式」になり、要求 (「この service
の credential を切り替えたい」) と一致しない。`boid secret list` は namespace 単位
なので、どんなアカウントが存在するかを一覧する手段も別途必要になる。

### 却下 3: config に名前付き credential セットを書く (`services.freee.accounts.ubs`)

どんなアカウントがあるか config を見れば分かる利点があるが、アカウント追加のたびに
config 編集 + daemon 再起動が要る。secret 投入だけで完結する下記の案のほうが運用が
軽く、認可の軸を今回追加しない (下記 D4) 以上、config に列挙する必然性がない。

### 却下 4: ジョブ起動時に namespace を固定する

`Entry.Namespace` を task/project 側で決める案。gateway は無改造で済むが、1 つの
ジョブの中で複数アカウントを跨げない (「両アカウントの残高を比べる」ができない)。

## To-Be

### 1. credential identity — secret key を account で修飾する

namespace は workspace のまま動かさない。切り替えるのは **secret store のキー側**。

| auth.kind | account 無し (現行) | account = `ubs` |
|---|---|---|
| oauth2 | `oauth2:freee:refresh_token` | `oauth2:freee@ubs:refresh_token` |
| oauth2 | `oauth2:freee:access_token_cache` | `oauth2:freee@ubs:access_token_cache` |
| bearer/basic/header/query | `<auth.secret_key>` | `<auth.secret_key>@ubs` |

`oauth_providers.<name>.client_secret_key` は **修飾しない** (下記 D7)。

この形の利点は 3 つ。

- 継承のジレンマが無い。namespace が動かないので client_secret は指定どおりの
  キーから引ける。account 付きのキーが存在しなければ素直に解決失敗する
- 粒度が要求と一致する。切り替わるのは当該 service の credential だけで、
  同じ workspace の他の secret には一切影響しない
- `boid secret list` に既定と account 付きが同じ一覧で並ぶ。アカウントの発見に
  専用コマンドが要らない

### 2. 経路 — path の service セグメントに埋める

```
$BOID_API_BASE/freee@ubs/api/1/deals?company_id=123456
```

ヘッダではなく path を採る理由は 2 つ。

- `RequestRecorder` (`internal/apigateway/recorder.go:14`) は
  `(taskID, method, service, path, status)` を task の action log に記録する。
  path に含めれば「どのアカウントに対して何をしたか」が recorder のシグネチャを
  変えずに監査証跡へ残る
- URL は組み立てるときに必ず意識するが、`-H` は落ちる

### 3. 部品と変更点

```
route.go        parsePath       service セグメントを (name, account) に分割
                                ↓ route{service, account, path}
server.go       ServeHTTP       認可・BaseURL・readonly は name で引く (D4)
                                credentials.Resolve/Inject に account を渡す
                                recorder には "name@account" を渡す (D8)
credentials.go  Resolve/Inject  static kind: secret_key を account で修飾
                                oauth2: credentialID を組んで oauth へ渡す
oauth2.go       AccessToken     credentialID で secret key / singleflight /
                                memCache のキーを組む (D6)
login.go        StartLogin      account を pendingLogin まで持ち回る
config          validate        service 名に "@" を禁止 (D1)
config          schema.go       services.<name>.require_account (D5) を
                                Schema に登録 — こちらが実際に必要だった変更
                                (PR-4 実装時、validate 側の追加だけで済むと
                                誤認していた。RequireAccount は単なる bool
                                フィールドで validateServiceConfig 側の検証は
                                無く、config CLI 経由で読み書きできるように
                                internal/config/schema.go の Schema に
                                エントリを足す方が本体だった — レビューで
                                発覚、#1040 で修正)
```

`OAuth2AccessTokenSource` interface は
`AccessToken(namespace, provider string)` から
`AccessToken(namespace string, cred credentialID)` へ変える。

`credentialID` を値型として導入するのは取り違え防止のため。現行の `oauth2.go` は
`cfg.Name` を「provider 設定の引きキー」と「secret key の構成要素」と
「singleflight/memCache キーの構成要素」の 3 役で使い回している。account を足す
とこの 3 役が分岐する (provider 設定の引きだけは素の名前) ので、**secret/cache 側
の identity を 1 つの値にまとめ、それを持ち回る**。文字列を組み立てる箇所が増えると
「一部だけ account 付き、他が素のまま」という事故が起きる。

```go
// credentialID identifies ONE credential set within a provider.
type credentialID struct {
    provider string // oauth_providers.<name> の引きキー。account では変わらない
    account  string // 空なら現行と完全に同じキーになる
}

func (c credentialID) secretPrefix() string // "freee" / "freee@ubs"
func (c credentialID) cacheKey(ns string) string // ns + "\x00" + secretPrefix()
```

## 決定事項

**D1. 区切り文字は `@`**

RFC 3986 の `pchar` に `@` が含まれるので path segment にそのまま書ける
(percent-encode 不要)。config load 時に service 名・provider 名・account 名が
`@` を含むことを拒否する。現行の service 名 validation
(`internal/config/validate.go:205` `validateServiceConfig`) は前後空白と空文字
しか見ていないので、ここに足す。

**D2. account 無しは現行と完全に同じ**

`freee` は今までどおり `oauth2:freee:refresh_token` を引く。既存の secret も
config も移行不要。account の有無で分岐する箇所は必ず「空なら現行の文字列を
そのまま組む」形にする。

**D3. 既定へフォールバックしない**

`freee@ubs` を指定して `oauth2:freee@ubs:refresh_token` が無ければ、fail-fast
pre-check (`Server.ServeHTTP` の `credentials.Resolve`) が 502 を返す。account
無しの credential へは決して落ちない。却下 2 で挙げた「黙って別アカウントに
書き込む」を構造的に防ぐのがこの設計の主目的なので、ここは緩めない。

**D4. 認可判定は base service 名で行う**

`Entry.Services[name]`・`BaseURLFor(name)`・`AllowsReadOnlyWrite(name)` はいずれも
account を落とした名前で引く。`freee@ubs` を workspace の enabled services に
列挙する必要はない。

account ごとの認可の軸は今回入れない (nose 判断 2026-08-29: 同一 workspace の
ジョブなら全アカウントに触れてよい)。入れるとすれば `Entry` に許可 account 集合を
持たせる形になるが、それは「1 service を credential 差し替えで使う」という設計意図
から service 複製へ逆戻りする方向なので、必要になった時点で改めて設計する。

**D5. `services.<name>.require_account` (既定 false)**

true の service は account 無しのリクエストを 400 で弾く。指定漏れが既定アカウント
へ落ちる事故を防ぐ安全弁。既定 false なので既存 service の挙動は変わらない。

`freee` にはこれを立てる (nose 判断 2026-08-29)。現行の `oauth2:freee:refresh_token`
は無指定でしか引けなくなるため、既定アカウントも account 付きへ移す。手順は下記
「freee の移行手順」を参照。

**D6. OAuth2 の singleflight / memCache キーに account を含める**

`AccessToken` の singleflight キーと memCache キーは現行
`namespace + "\x00" + provider` (`internal/apigateway/oauth2.go:492`, `:566`)。
ここに account が入らないと、**別アカウントのリクエストが同じキーに合流して他方の
access token を返す**。credential 混線であり、この変更で最も危険な箇所。
`credentialID.cacheKey` を唯一の組み立て口にして、生の文字列連結を残さない。

`normalizeNamespace` を `AccessToken` / `StartLogin` の 2 箇所だけで適用する既存の
規約 (`internal/apigateway/oauth2.go:253` のコメント) は維持する。account は
normalize 対象ではない (空は空のまま = 修飾なし)。

**D7. client_secret は account で修飾しない**

`oauth_providers.<name>.client_secret_key` は provider (= OAuth アプリ) 単位の値で、
アカウント単位ではない。同一 OAuth アプリに複数ユーザが認可する形を前提とする。
別アプリを使いたい場合は provider を分ける (既存機構で表現できる)。

**D7 補足: `grant: client_credentials` の provider では account 修飾に分離効果が無い**
(opus レビュー、item 3、2026-08-30 発見)。`refreshClientCredentials`
(`internal/apigateway/oauth2.go`) は refresh_token を一切読まず、D7 どおり
無修飾の `cfg.ClientSecretKey` だけでトークンを取得する。したがって
`myservice@typo` のような **存在しない account を指定してもリクエストは成功し**、
返るのは無修飾と同じアプリ資格情報になる。D3 の「他 account へフォールバックしない」
には違反しない (そもそも account 修飾された credential という概念自体がこの
grant には存在しない) が、この種の provider では `require_account: true` は
「account を書け」という記法上の強制以上の分離効果を持たない — singleflight/
memCache キー (D6) は account ごとに分かれるが、実際に取得されるトークンは
どの account 名でも同一になる。挙動は
`TestOAuth2TokenSource_ClientCredentialsGrant_WithAccount_QualifiesCacheButNotClientSecret`
(`internal/apigateway/oauth2_account_test.go`) で固定済み。freee は
`grant: authorization_code` (manual flow) なので、下記の freee 移行手順には
影響しない。

**D8. recorder には `name@account` をそのまま渡す**

`RequestRecorder` のシグネチャは変えない。`service` フィールドに `freee@ubs` が
入る。base service 名はそこから自明に読める。

**D9. `boid secret oauth login <service> --account <name>`**

`--account` (省略時は現行と同じ無修飾) を足す。`LoginManager.StartLogin` は
`(namespace, provider, redirectURI)` に account を加えた形になり、`pendingLogin`
まで持ち回って `persistGrant` の書き込み先キーに効かせる。

**D10. account 一覧の専用コマンドは作らない**

`boid secret list` に `oauth2:freee@ubs:refresh_token` が並ぶ。

**D11. account 名の文字集合**

英数字・`-`・`_` のみ、1〜64 文字。`@` `/` `:` は禁止 (キー構成とパス分割の両方を
壊すため)。`parsePath` で弾き、400 を返す。

## PR 分割

- **PR-1 (マージ済み、#1037)**: `parsePath` の `(name, account)` 分割 +
  `route.account` + config validation (D1, D11)。static auth kind
  (bearer/basic/header/query) の secret key 修飾まで。認可・BaseURL・
  readonly が base 名で引かれることをテストで固定する (D4)
- **PR-2 (マージ済み、#1038)**: `credentialID` 導入と oauth2 の account 対応
  (D6, D7)。singleflight/memCache キーの分離をテストで固定する
- **PR-3 (マージ済み、#1039)**: `boid secret oauth login --account` (D9)
- **PR-4 (本 PR)**: `services.<name>.require_account` (D5)。これで PR 分割
  は全て完了 — このプロジェクトの設計・実装は本 PR で完結する

PR-1 単体で static kind の service は使えるようになるが、freee は oauth2 なので
PR-2 まで到達して初めて実用になる。

**決定済み・未実装: connector exec 経路の account 未対応 (レビューで発見・
PR-2 で決定、2026-08-29。PR-4 でも実装しないと判断 — 独立の後続タスクへ送る)**。
`internal/server/connector_exec.go` の
`resolveConnectorExec` は `APIGatewayServices: []string{ref.Service}` と
`Env["BOID_SIGNAL_SERVICE"] = ref.Service` の両方に `ref.Service` の生文字列を
そのまま入れている。ここは workspace の `services` 一覧以外で `Entry.Services`
を作る唯一の経路であり、`signals.sources[].service` に `freee@ubs` と書くと
`Entry.Services` は `{"freee@ubs"}` になる。一方 `Server.ServeHTTP` の認可判定は
base 名 (`freee`) で引く (D4) ので、この経路だけ **常に 403** になる —
fail-closed なので危険ではないが、原因不明の 403 として観測される。

**方針: split する**。`route.go` の `splitServiceAccount`/`validateAccountName`
を小さな exported helper 経由で `resolveConnectorExec` から再利用し、
`ref.Service` を base/account に分割してから `APIGatewayServices` には base 名を、
`BOID_SIGNAL_SERVICE` には `ref.Service` (base@account のまま、D8 の
「recorder には name@account をそのまま渡す」と同じ扱い) を入れる形にする。
connector 経路専用の再実装はしない — 既存 `parsePath` 側のロジックをそのまま
借りる、小さく機械的な修正の見込み。

本 PR (PR-4) でもこの分割には手を入れていない — `signals.sources[].service`
に account 修飾を書く実運用の要求がまだ無く、この PR のスコープ (D5の
`require_account` 実装) と直接の依存関係も無いため、独立に着手できる
後続タスクとして残す。

**PR-4 で「account 修飾された service を指す設定さえ書かなければ影響を受けない」
という前提が崩れる点に注意** (opus レビュー、item 2、2026-08-30 発見)。
`apigateway.Server.ServeHTTP` は `entry.Services[rt.service]` による認可判定の
直後に `ServiceConfig.RequireAccount` を見て、account 無しのリクエストを 400 で
弾く (D5、`internal/apigateway/server.go`)。connector exec 経路は
`resolveConnectorExec` が組んだ job token の `Entry.Services` にも同じ
`RequireAccount` フラグが乗るので:

- `signals.sources[].service: freee` (無修飾・現行の書き方のまま) →
  `entry.Services["freee"]` は true (認可は通る) が、`freee` に
  `require_account: true` を立てた時点で **connector job の gateway 呼び出しが
  400** になる。account 修飾を一切書いていなくても壊れる
- `signals.sources[].service: freee@ubs` (account 修飾を書いた場合) → 上記の
  「決定済み・未実装」節の 403 (`Entry.Services` が `{"freee@ubs"}` になり、
  base 名 `freee` で引く認可判定と一致しない)

つまり **`require_account: true` を立てた service には connector exec の動く
経路が存在しない** — 修飾の有無に関わらず、400 (無修飾) か 403 (修飾あり) の
どちらかで必ず落ちる。現時点で `freee` を指す connector は無いので実害は無いが、
後続タスク (上記の split) の前提条件としてここに明記する。既存の connector exec
経路が実際に無傷なのは、`require_account: true` を **立てていない** service を
指す設定を書いている間だけ。

ドキュメント (`docs/ja/reference/config-yaml.md`) の更新は各 PR に含める。
**boid-api-skills 側の `freee-api` スキルは別リポジトリなので、この PR (PR-4)
には含められない** (opus レビュー、item 6、2026-08-30 訂正 — 旧記述は「各 PR
に含める」としていたが実態と合わない)。account 修飾した path の書き方
(`freee@ubs` 等) へスキル側を更新する作業は、下記「freee の移行手順」の前提
として別途必要 — 手順のステップ 6 (`require_account: true` を立てる) より前に、
スキルが呼ぶ URL が account 修飾済みになっている必要がある。

## freee の移行手順

`freee` で扱うアカウントは 2 つ。**既存の無修飾 credential
(`oauth2:freee:refresh_token` / `oauth2:freee:access_token_cache`) が `ubs`**、
新たに追加するのが `nvt`。D5 により `freee` は `require_account: true` になるので、
既存分も `oauth2:freee@ubs:...` へ移す。

**refresh_token をコピーしてはいけない。** freee は refresh token をローテーション
する provider (`docs/ja/reference/config-yaml.md` の「daemon 単一リフレッシャ」節)
なので、旧キーと新キーが同じ refresh_token を指した状態でどちらか一方が refresh
すると、もう一方が握っている値はその瞬間に無効になる。並行運用は成立しない。
UBS 側も **login し直して新しい grant を取る**。

無停止で進む順序:

1. PR-1〜PR-4 をデプロイする。この時点では `services.freee.require_account` は
   まだ false なので、無修飾のリクエストは従来どおり動く
2. `boid secret oauth login freee --account ubs` で UBS の grant を取り直す。
   旧キー (`oauth2:freee:...`) はまだ触らない — 別の authorization code から得た
   独立した grant なので、この時点では旧 grant も生きている想定
3. `boid secret oauth login freee --account nvt` で NVT の grant を取る
4. `$BOID_API_BASE/freee@ubs/...` と `$BOID_API_BASE/freee@nvt/...` の両方が通る
   ことを実ジョブで確認する。company_id が期待どおり別事業所を指すかまで見る
5. boid-api-skills 側の `freee-api` スキル (別リポジトリ、上記「PR 分割」節末尾
   参照) を、無修飾の `/freee/...` ではなく account 修飾した
   `/freee@ubs/...` / `/freee@nvt/...` を呼ぶよう更新し、デプロイする。次の
   ステップ 6 (`require_account: true`) より前に完了させること — さもないと
   ステップ 6 の直後からスキル自身が無修飾リクエストを送って 400 で落ち始める
6. `services.freee.require_account: true` を立てて daemon に反映する
7. `boid secret delete oauth2:freee:refresh_token` と
   `boid secret delete oauth2:freee:access_token_cache` で旧キーを消す

6 を 2〜5 より先にやると、その間の無修飾リクエスト (旧スキルからのものを含む) が
400 で落ちる。7 を 6 より先にやると、その間の無修飾リクエストが 502 で落ちる。
順序は守る。

なお 2 の「独立した grant」が freee 側で成立するか (同一ユーザ・同一 OAuth アプリ
で複数の refresh_token を同時に保持できるか) は実測で確認する。もし freee が
1 ユーザ 1 grant で旧 grant を失効させる挙動なら、2 の直後から無修飾リクエストが
502 になるため、2〜5 を一続きの作業として実施する。

UBS と NVT が **別の freee ログインユーザ**なら 2 と 3 は互いに独立で、この懸念は
2 (UBS の取り直し) にしか当たらない。同一ユーザが両事業所にアクセスできる形なら、
そもそも company_id の指定だけで足りていたはずなので、ここは別ユーザ前提で進める。

## レビュワー採点表 (yes = 合格)

| # | 問い | yes/no |
|---|---|---|
| 1 | account 無しのリクエストが、変更前と**同一の** secret key を引くことがテストで示されているか (D2) | |
| 2 | account 指定時に、account 無しの credential へフォールバックしないことがテストで示されているか (D3) | |
| 3 | `Entry.Services` / `BaseURLFor` / `AllowsReadOnlyWrite` が base service 名で引かれ、`freee@ubs` が 403/502 にならないことがテストで示されているか (D4) | |
| 4 | singleflight / memCache のキーに account が入り、別 account が別エントリになることがテストで示されているか (D6) | |
| 5 | secret key・cache key を組み立てる箇所が `credentialID` のメソッドに一本化され、生の文字列連結が残っていないか (D6) | |
| 6 | `client_secret_key` が account で書き換わらないことがテストで示されているか (D7) | |
| 7 | service 名・provider 名・account 名の `@` 拒否が config load / parsePath で効いているか (D1, D11) | |
| 8 | recorder に `name@account` が渡ることがテストで示されているか (D8) | |
| 9 | `require_account: true` の service が account 無しを 400 で弾き、既定 false の service の挙動が変わらないことがテストで示されているか (D5) | |
| 10 | login で取得した grant が `oauth2:<provider>@<account>:refresh_token` に書かれることがテストで示されているか (D9) | |
