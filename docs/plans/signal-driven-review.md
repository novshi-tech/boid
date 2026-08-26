# Signal-driven review と Integration Pack

2026-08-26 起案。

ステータス: **方向性の設計メモ**。目標と責務境界には概ね合意しているが、review agent が
サービス固有の外部情報を必要に応じて取りに行き、現行 khi の sweep と同等以上の判断を
できるかは未検証である。実装計画へ進む前に、本 doc の shadow 検証を行う。

関連文書:

- `docs/plans/ingestion-identity.md` — trigger、identity、daemon/workspace 境界の現行設計
- `docs/plans/suggestion-as-state-transition.md` — suggestion と人の判断の境界
- `docs/plans/workspace-default-project.md` — workspace default と project 定義
- `docs/plans/api-gateway.md` — service registry と workspace-scoped credential 境界
- khi-task-collector の `docs/plans/signal-and-summary.md` — 現行 sweep の設計

---

## 1. 発端

`khi-task-collector` は Slack、Jira、Bitbucket 等の変化を集め、boid の card/task を
再評価し、summary、次の子 task、状態遷移の suggestion を作る workspace 固有の
メタプロジェクトである。

現在の責務境界では、新しい workspace に同様の収集・判断系を追加する負担が大きい。
実機の khi はおおよそ次の規模になっている。

- Python 本体: 約 4,100 行
- Python テスト: 約 5,460 行
- agent skill: 約 650 行
- 合計: 約 10,200 行

この中には、khi の判断方針ではなく、どの workspace でも必要になる機構が多く含まれる。

- action 列のページングと cursor
- event key の dedup
- attempts、retry、dead letter
- sweep の single-flight
- dirty な task の抽出と batch 化
- boid CLI 応答の型変換
- status ごとの書き込み可否検証
- summary、suggestion、child spec の差分更新
- 処理済み signal の記録
- sweep task の作成と終了管理

これらを workspace ごとに再実装する形は持続しない。一方、Jira、Slack、Bitbucket の
具体的な API 実装や reference skill をすべて boid binary に埋め込むと、core の肥大化、
SaaS API と boid 本体の release cycle の結合、全 job への不要な skill 配布を招く。

本 doc は、両者の間に次の境界を引く。

1. boid core は signal-driven review の汎用実行機構を持つ
2. サービス固有知識は versioned Integration Pack として配布する
3. workspace/project は判断方針だけを供給する

---

## 2. 目標

### 2.1 設計目標

> 外部・内部の変化を受け取り、boid 上の work item について再判断し、次の状態・作業を
> 提案する仕組みを、workspace 固有コードなしで構成できる。

既存 Integration Pack だけを使う workspace は、次だけで追加できる状態を目標にする。

- workspace の review 定義
- service instance と credential の binding
- source の選択・検索条件
- workspace の判断方針
- 必要に応じた project 固有の補足

Git repository、Python package、専用 write CLI、専用 test suite は要求しない。

### 2.2 汎用性の対象

本設計は「任意の SaaS workflow engine」を目指さない。対象は次に限定する。

> 外部・内部の observation を契機に、boid の task/card を reconcile する自動処理。

入力と判断方針は拡張可能にするが、出力は boid の task model と state machine に閉じる。
Integration Pack に独自 DB field、独自状態遷移、独自 review verb を追加させない。

### 2.3 非目標

- boid daemon が直接 SaaS credential を持って外部 API を呼ぶこと
- Jira/Slack/Bitbucket の意味を orchestrator package に埋め込むこと
- すべての外部データを共通 schema に正規化すること
- workspace が review protocol 自体を上書きすること
- review agent から外部サービスへ無承認で書き込むこと
- project 固有の任意 script を workspace 共通 trigger として実行すること
- 現行 khi のコードをそのまま core package へ移植すること

---

## 3. 中心となる責務境界

旧来の「credential は workspace にあるため、外部取り込み実装も workspace が持つ」という
境界は粗すぎる。次の 3 つを分ける。

| 観点 | 所有者 |
|---|---|
| credential と外部 API へ到達する実行コンテキスト | workspace |
| polling、dedup、review lifecycle 等の汎用機構 | boid core |
| API 固有の解析・探索知識 | Integration Pack |
| 何を重要とみなし、何を提案するか | workspace/project policy |
| task 状態・identity・履歴の正 | boid core |
| 最終判断 | 人 |

外部 connector と review agent は workspace の sandbox 内で実行し、既存 API gateway と
workspace-scoped job token を使う。したがって、実装コードを Integration Pack に移しても
credential 境界は変わらない。

---

## 4. 全体像

```text
                  ┌─────────────────────────────┐
                  │       Integration Pack       │
                  │ connector / resolver / skill│
                  └──────────────┬──────────────┘
                                 │ workspace sandbox
                                 ▼
External services ───────▶ normalized Signal
                                 │
                                 ▼
                         Signal inbox / dedup
                                 │
                                 ▼
                         Review scheduler
                                 │
                                 ▼
                ┌──────────── Review run ─────────────┐
                │ core protocol                      │
                │ + workspace policy                 │
                │ + project hints                    │
                │ + selected Integration skills      │
                │                                    │
                │ LLM が必要な外部データを追加照会   │
                └────────────────┬───────────────────┘
                                 │ typed, idempotent mutations
                                 ▼
             summary / identity / child spec / suggestion
                                 │
                                 ▼
                              人が判断
```

概念上、subsystem 全体は **reconciliation**、LLM が判断する処理は **review**、
1 回の実行記録は **review run** と呼ぶ。

review は現行 khi の sweep task を第一級機能として一般化したものだが、一般化するのは
source だけではない。起動条件、policy の供給、出力契約、retry/ack lifecycle も対象である。

---

## 5. Signal は「起動封筒」である

### 5.1 判断材料の完結形にしない

現行 khi の運用では、LLM が必要に応じて元の Slack thread、Jira issue、Bitbucket PR、
関連する別サービスを読めないと、提案精度が著しく落ちることが確認されている。

したがって、共通 Signal model は外部サービスを抽象化して隠すものではない。

> Signal は「いつ、何について調べ直すか」を伝える起動封筒であり、判断に必要な情報を
> すべて内包する snapshot ではない。

core は control plane だけを抽象化する。review agent は Integration Pack の skill と
service binding を受け取り、data plane ではサービス固有の API を意識して調査する。

### 5.2 Signal envelope 案

```yaml
id: slack-main:C123:1723456789.000100
occurred_at: 2026-08-26T10:00:00Z
title: PRレビューについてのメンション
preview: 修正内容を確認してほしい
source:
  pack: slack
  connector: mentions
  service: slack-main
resource:
  kind: message
  id: C123:1723456789.000100
  url: https://example.slack.com/archives/C123/p1723456789000100
identities:
  - slack:message:C123:1723456789.000100
hints:
  author: U123
  thread: "1723456789.000100"
```

core が解釈するのは次だけである。

- workspace/source instance と idempotency key
  (`id`。Connector が同一 event から必ず同じ値を生成する、取り込み dedup 用の安定キー)
- 発生時刻
- title/preview/body の開示済み部分
- source provenance (envelope の `source:` ブロック)
- 外部 resource への handle
- identity 候補
- opaque な hints

`resource.kind: message` や `hints.thread` の意味は core が解釈しない。Slack Pack の
resolver/skill が解釈する。

### 5.3 Adapter の責務

現在 khi 側にある外部 signal から共通 model への変換は Integration Pack の Connector に
移す。Connector は次を行う。

- API の pagination
- source cursor の解釈
- API 固有 response の parse
- 安定した event key の生成
- 最小限の preview と resource handle の生成
- 明らかな機械的 filter

Connector は次を行わない。

- その signal が仕事に値するかの意味判断
- 既存 task との意味的な同一性判断
- project/behavior の選択
- summary、child spec、suggestion の作成

---

## 6. Integration Pack

### 6.1 位置づけ

「core に寄せる」は、全 adapter/skill を Go binary に `go:embed` するという意味ではない。
boid が install、version pin、実行、mount を管理する Integration Pack に寄せる。

```text
jira-cloud/
├── integration.yaml
├── connectors/
│   ├── assigned-issues
│   └── issue-updates
├── resolvers/
│   └── issue
└── skills/
    └── jira-api/
        ├── SKILL.md
        └── references/
```

### 6.2 Pack が提供する 3 能力

| 能力 | 役割 |
|---|---|
| Connector | 新しい変化を検出し Signal を出す決定的処理 |
| Resource resolver | Signal が指す canonical resource を再取得する決定的処理 |
| Reference skill | LLM が必要に応じて検索範囲を広げるための API 知識 |

resolver の必要性・粒度は shadow 検証で決める。最初は既存 API gateway と
`boid-api-skills` の reference skill で検証し、それで不十分な操作だけを型付き resolver
として昇格させる。

### 6.3 Manifest 案

```yaml
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata:
  name: jira-cloud
  version: 1.2.0

serviceProfiles:
  - name: jira-cloud
    endpoint:
      configurable: true
    credentials:
      - name: token
        injection: bearer

connectors:
  - name: assigned-issues
    executable: connectors/assigned-issues
    serviceProfile: jira-cloud
    configSchema:
      type: object
      properties:
        jql: {type: string}

skills:
  - name: jira-api
    path: skills/jira-api
    requiresServiceProfile: jira-cloud
```

core が持つのは manifest schema、connector execution protocol、selective skill mount、
pack version/digest、service profile の解決だけである。

### 6.4 配布

公式 container image には標準 Pack をプリインストールしてよい。ただし boid binary への
埋め込みとは分ける。

```text
boid image
├── /usr/bin/boid
└── /opt/boid/integrations/
    ├── jira-cloud/1.2.0/
    ├── slack/1.1.0/
    └── bitbucket-cloud/1.3.0/
```

- 通常の container 運用では追加 install 不要
- bare binary では将来 `boid integration install` を提供できる
- SaaS API の変更は Pack release で追従できる
- custom Pack は boid 本体を fork せず追加できる
- review job には利用する skill だけを read-only mount する
  (組み込みスキルと同様、dispatcher が job sandbox 構築時に agent のスキル探索パス配下へ bind mount する)

Pack install は daemon 管理者だけが実行できる。project.yaml や review agent が自分で
Pack を install/upgrade して権限を増やすことはできない。

---

## 7. Service profile と service instance

### 7.1 従来の自由形式 service は残す

現在の `config.yaml services:` は任意 HTTP API を扱う escape hatch として維持する。
Integration Pack の service profile は型付き convenience であり、既存 registry を
置き換えない。

```yaml
services:
  customer-jira:
    uses: jira-cloud/jira-cloud@1.2.0
    endpoint: https://example.atlassian.net
    credentials:
      token: JIRA_TOKEN

  internal-api:
    base_url: https://internal.example.com
    auth:
      kind: bearer
      secret_key: INTERNAL_TOKEN
```

Pack は credential の値を持たない。profile は必要な credential slot と注入方法を宣言し、
service instance が SecretStore の key を bind する。

### 7.2 論理名を skill に決め打ちしない

`jira-api` 等は慣例名であって固定名ではない。review run には capability から解決済み
service binding を渡す。

```json
{
  "jira-cloud": {
    "service": "customer-jira",
    "gateway_url": "$BOID_API_BASE/customer-jira"
  }
}
```

skill/connector は環境固有の論理名を埋め込まず、この binding を使う。

自由形式 service には profile がないため、この binding の対象外である。従来どおり
自身の service 名で gateway を参照する。`review_access.services` には両形式の service を
並べられる。

---

## 8. Review の定義

### 8.1 task_behaviors から分離する

review を通常の `task_behaviors.sweep` として workspace に定義させると、workspace が
protocol prompt、signal ack、batch、retry、完了管理、書き込み規約を再び持つことになる。

review は workspace の第一級定義として置く。汎用の `automations` + `kind` による
family 化は行わない。現時点で kind は review しか実在せず、実在しない 2 つ目の kind の
ための抽象は早すぎるためである。2 つ目の kind が実在した時点で括り直す。

```yaml
reviews:
  task-review:
    enabled: true

    schedule:
      every: 10m

    runtime:
      agent: claude-code
      model: sonnet
      timeout: 30m
      batch_size: 8

    policy:
      disclosure: body
      instruction: |
        顧客からの依頼、レビュー待ち、障害対応を優先する。
        外部で完了済みなら done を提案する。
        相手への送信が必要なら respond task を提案する。

    sources:
      - connector: slack/mentions
        service: slack-main
        config:
          include_threads: true

      - connector: jira-cloud/assigned-issues
        service: customer-jira
        config:
          jql: assignee = currentUser() ORDER BY updated ASC

    review_access:
      services:
        - slack-main
        - customer-jira
        - customer-bitbucket
```

### 8.2 Source と review access を分ける

Signal の発生元だけを review job に許可すると cross-service 調査ができない。khi では
Slack の会話から Jira issue や Bitbucket PR を調べる実需がある。

- `sources`: review を起こす connector
- `review_access.services`: 判断時に read してよい service

既定の access は sources が使う service の union とし、workspace が明示的に追加できる。
review job は原則 readonly。外部への書き込みは通常の child task と人の承認へ分離する。

### 8.3 指示の 4 層

```text
Core review protocol（boid、上書き不可）
    +
Integration skills（Pack、対象 run に必要なものだけ）
    +
Workspace policy（review 定義、全対象共通）
    +
Project review hints（対象 project が判明した場合だけ）
```

#### Core protocol

- 対象を review context から取得する
- lease された subject だけを扱う
- 書き込みは typed review operation を使う
- workspace scope を越えない
- 各 subject を complete または fail する
- suggestion を人の承認なしに直接適用しない

workspace/project はこの protocol を置換できない。custom instruction は明確に区切って
後置する。

#### Workspace policy

- 何を仕事として扱うか
- urgency の判断基準
- いつ完了とみなすか
- 人へ返す条件
- project/behavior 選択の一般方針

#### Project review hints

対象 project が確定している場合だけ、その project の局所知識を加える。

```yaml
review:
  instruction: |
    UI変更は mera-ui、APIとDB変更は rook-serverへ振る。
    実装には executor、調査には research behaviorを使う。
  completion_evidence:
    - merged_pull_request
    - deployed_to_staging
```

新規 Signal は project が未確定なので、project instruction だけで review を起動しない。
workspace policy で関連性と所属先を判断し、候補または所属先が決まった後に project hints を
読む。

---

## 9. Review run

### 9.1 通常 task と同一視しない

現行 sweep task は LLM を起動するために通常 task を使っている。しかし review cycle は
人が片付ける work item ではなく、work item を再評価する system activity である。

目標モデルは `review_runs` + `Job` とする。

```text
workspace の review 定義
        │
        ▼
  ReviewRun ── Job ── LLM
        │
        ├── Signal/Subject を lease
        ├── mutation を記録
        └── complete / retry / dead-letter
```

run が lease する対象は、当該 review 定義の未 ack Signal から `batch_size` を上限に
選ぶ。複数の Signal が同一 task へ合流する場合に subject へどう畳むかは未決である
(§14)。

ReviewRun は少なくとも次を snapshot する。

- workspace revision
- review 定義の revision
- Integration Pack の version/digest
- policy instruction
- model/runtime 設定
- 対象 signal IDs
- service bindings と権限

設定更新は実行中 run の意味を変えず、次の run から反映する。現行 khi で問題になった
trigger と behavior の version skew をここで防ぐ。

実装初期に通常 execution task を内部生成して検証する余地はあるが、公開モデルとして
「review は通常 task」と固定しない。

### 9.2 起動

host 側の手動操作案:

```text
boid review run <workspace>/<review>
boid review status <workspace>/<review>
boid review runs <workspace>/<review>
boid review retry <run-id>
```

scheduler も `boid review run` と同じ application service を呼ぶ。手動実行だけ別経路に
しない。

将来は次の起点を同じ review queue へ接続できる。

- periodic connector poll
- boid task/action の内部変化
- 手動実行
- webhook 等の外部 push

---

## 10. Review mutation protocol

巨大な result envelope を最後に 1 回だけ返す形にはしない。LLM は逐次書き込みでき、途中で
落ちても適用済み操作は残り、再実行で no-op になる形を維持する。

Review job 内の CLI 案:

```text
boid review context
boid review subject show <subject-id>

boid review identity link <subject-id> <identity>
boid review summary set <subject-id> --file <file>
boid review child upsert <subject-id> --key <stable-key> --file <spec>
boid review suggest <subject-id> --verb <verb> --reason <reason>
boid review observe <subject-id> --source-closed=<bool>

boid review complete <subject-id>
boid review fail <subject-id> --reason <reason>
```

run token により次を強制する。

- lease された subject だけ
- 同一 workspace だけ
- core が許可した mutation だけ
- transition の直接実行は不可
- complete 後の追加書き込みは不可
- operation ごとの idempotency

途中で job が死んだ場合、適用済み mutation は残るが Signal は ack されない。lease expiry
または retry により再 review され、同じ mutation は no-op になる。

boid への書き込み手順を知るのは core review protocol 層だけである。Pack の skill が
扱うのは source 側の知識 — review 中の読み方・調べ方と、child task が使う外部への
書き込み手順 — であり、boid の review operation には言及しない。

---

## 11. 汎用性の担保

本節が担保するのは構造上の閉じ方である。generic にして現行 sweep の判断品質が出るか、
という実現性の担保は本節ではなく §12 の shadow 検証が担う。

### 11.1 拡張軸

```text
入力の拡張             判断の拡張
Integration Pack       Workspace / Project Policy
      │                         │
      └──── Signal ── Review ───┘
                          │
                    boid の固定出力
```

- 新しい source は Pack で追加する
- 同じ Pack を異なる workspace policy で再利用する
- 同じ workspace で同じ service profile の複数 instance を使える
- policy 変更のために connector を変更しない
- connector 変更のために core DB/state machine を変更しない

### 11.2 構造上の guard

- core package から Jira/Slack/Bitbucket package を import しない
- core schema にサービス固有 field を追加しない
- connector config は Pack の schema で検証する
- skill/connector に service instance の論理名を決め打ちしない
- core protocol prompt にサービス固有手順を入れない
- Pack の skill/playbook に boid の review operation への言及を入れない
  (boid への書き込み手順は core protocol 層が独占し、conformance test で検査する)
- Pack は state machine、review verb、DB schema を拡張できない
- Pack contract の conformance test を boid 側で提供する

### 11.3 実装着手後の受け入れ条件

1. 2つ目の workspace は YAML と policy だけで追加できる
2. 新しいサービスは Pack 追加だけで利用できる
3. 同じ Pack を異なる policy で再利用できる
4. 新しい policy のために connector を変更しない
5. Pack 追加時に core の DB schema/state machine/review command を変更しない
6. khi 固有語彙が core の型、SQL、protocol prompt に入らない

---

## 12. 実装前の shadow 検証

### 12.1 なぜ必要か

目標アーキテクチャの方向は妥当だが、最も重要な仮説は未検証である。

> normalized Signal、generic core protocol、Integration skill、workspace policy の組み合わせで、
> 現行 `/khi-sweep` と同等以上に外部世界を調査し、正しい提案を作れるか。

これを確認せずに signal inbox、Pack runtime、review run table を先に実装すると、review agent
が十分に判断できない大きな機構だけが残る危険がある。

### 12.2 仮説

| # | 仮説 |
|---|---|
| H1 | provenance と resource handle を持つ Signal から、agent が調べる対象を選べる |
| H2 | core protocol + workspace policy + Integration skill で現行 sweep と同等に動ける |
| H3 | 必要な service への read access があれば、Signal に全文を詰め込まず判断できる |
| H4 | サービス固有の調査手順を workspace prompt ではなく Pack 側へ置ける |
| H5 | 複数 service を横断する現行 khi の判断を generic review でも維持できる |

### 12.3 検証方法

shadow に先立ち、コードを書かない机上 walkthrough を行う。

1. 現行 khi の sweep prompt を §8.3 の 4 層へ手で分解する。分解できなければ、この
   時点で H4 を棄却し、知識の置き場所を再検討する。
2. khi の履歴から実 Signal を 2〜3 件選び、envelope への変換と、期待される mutation 列
   (identity link・summary・child upsert・suggest・complete) を手書きする。

紙の上で書けないものは、shadow のインフラを作っても動かない。この成果物はそのまま
core protocol の初稿と shadow の期待値になる。

その上で、現行 sweep を止めず、同じ対象に generic `review-v0` を dry-run で並走させる。

```text
同じ Signal / 対象
   ├── 現行 /khi-sweep       → 本番の提案・更新
   └── generic review-v0     → report のみ、外部/boid 書き込みなし
```

`review-v0` に与えるもの:

- 仮の immutable core review protocol
- normalized Signal と resource handle
- workspace policy
- 既存 `boid-api-skills`
- 現行と同じ API gateway の read access
- dry-run の review mutation reporter

履歴 replay だけでなく live shadow を行う。外部データが不足しており、agent が自発的に
調査を広げる必要があるケースこそ検証対象だからである。

### 12.4 ケース

- Slack thread から Jira/Bitbucket を調べる cross-service ケース
- Jira issue 単独で完結するケース
- 複数 source が同一 task に合流するケース
- project 未確定の新規 Signal
- 既存 project/task へ link するケース
- 外部で完了済みの done suggestion
- done/dropped 後の再燃
- 本文非開示で URL/metadata しかないケース
- API の一時失敗、権限不足、pagination
- 判断材料不足で保留すべきケース

### 12.5 評価項目

- 正しい外部 resource を読みに行ったか
- 必要な追加検索を自発的に行ったか
- 複数 service を正しく突合したか
- identity/link 先が妥当か
- summary と suggestion が現行同等以上か
- child spec がそのまま実行可能か
- 材料不足時に捏造せず fail/保留できたか
- protocol と workspace scope を守ったか
- API call 数、token、実行時間、費用

### 12.6 失敗の読み方

| 症状 | 見直す境界 |
|---|---|
| 調べるべき resource が分からない | Signal provenance/resource handle が不足 |
| skill があっても正しい調査手順を選べない | Pack に resolver/review playbook が必要 |
| cross-service の関連づけに失敗する | review access、identity、workspace policy が不足 |
| khi 固有の長い prompt がないと動かない | core/Pack/workspace への知識分解が不十分 |
| Signal に全文を詰めないと動かない | live retrieval の入口または skill が不足 |
| Pack が独自 mutation を必要とする | boid の出力 model が不足、または対象 scope が広すぎる |

shadow 検証が不合格なら、目標そのものを捨てるのではなく、どの層に不足した知識を置くかを
再検討する。実装を先行させない。

---

## 13. 想定する移行順

shadow 検証が Go になった後の概略順。PR 分割は検証後に別 doc で確定する。

1. **review-v0 shadow** — 現行 khi 上で generic protocol/policy/skill の成立性を確認
2. **typed review mutation** — khi の `write.py` から boid native operation へ置換
3. **Signal inbox/review lifecycle** — cursor、attempts、dead letter、lease を core へ移す
4. **Integration Pack runtime** — Connector、skill mount、service profile を導入
5. **公式 Pack 化** — Slack/Jira/Bitbucket adapter と `boid-api-skills` を Pack へ整理
6. **workspace review 定義** — project trigger/sweep behavior を第一級 review 定義へ移す
7. **ReviewRun 化** — 通常 sweep task 依存を `review_runs` + Job へ置換
8. **khi 退役** — khi-task-collector repo には固有 policy もコードも残さない

途中の各段階で現行 khi と並走可能にし、一度に writer、scheduler、adapter、prompt を
切り替えない。

---

## 14. 現時点の決定と未決

### 決定済みの方向

1. 現行 khi の sweep を、source と lifecycle を一般化した review として core 機能化する
2. Signal は判断材料の完結形ではなく起動封筒とする
3. review agent はサービス固有知識を失わず、必要に応じて live data を取りに行く
4. サービス固有 adapter/reference skill は Integration Pack に置く
5. 公式 Pack は container image へプリインストール可能にするが binary embed しない
6. core protocol は boid、判断方針は workspace、局所知識は project が供給する
7. source service と review 時に読める service を分離する
8. Pack は input を拡張できるが、boid の output/state machine は拡張できない
9. review の実装方式は shadow 検証を Go 条件とする

### shadow 検証後に決めること

- 共通 Signal schema の必須/任意 field と size limit
- Signal から subject への畳み込み (同一 task へ合流する複数 Signal の単位化。
  現行 khi の dirty 抽出 + batch 化に相当)
- 大量・低 S/N の source (mail 等) に対する、Connector の機械的 filter と review の
  間の安価な篩の段
- Resource resolver を Pack contract の必須能力にするか
- API reference skill と review playbook を分けるか
- Connector process protocol と cursor/checkpoint の transaction 境界
- Pack の発見・配布・install/update/signing/version pin の具体方式
- service profile が複数 header 等をどう表現するか
- workspace review 定義 schema の最終形
- `review_runs` の schema と Job principal
- review batch 内の並列性と subagent 利用
- live shadow の期間、評価 rubric、Go threshold
- Web UI における review run、dead letter、失敗の表示

---

## 15. 一文での定義

> review とは、任意の source から届いた変化を契機に、Integration Pack を使って外部世界を
> 必要なだけ調べ、workspace/project の方針で boid task を reconcile する、boid 管理の
> sweep である。

---

## 16. 採点表 — レビュワー用 yes/no 判定リスト

機械的テストが拾えない「設計への適合」を、LLM/人間レビュワーが判定できる形に
落とした採点表。運用ルール:

1. 全命題は**「yes = 合格」に極性統一**してある。否定疑問文は使わない。
2. 判定者は各命題に yes/no と**根拠 (file:line、diff hunk、または shadow run の
   記録) の引用**を付ける。**根拠を引けない yes は no として扱う**。
3. no が 1 つでもあれば通さない。直し方は 2 通りだけ——実装 (または検証) を
   直すか、設計判断を変えたのなら**先に本 doc の該当節を更新**してから通す
   (doc と実装の乖離を作らない)。
4. グループ A は shadow 検証の Go/No-Go 判定で採点する。グループ B〜E は実装 PR の
   レビューで「該当グループ + 全体 (E)」を採点し、対象外グループは skip する。
   PR 分割が別 doc で確定した時点 (§13) で、B〜E を PR 単位のグループへ割り直す。

### A. shadow 検証の Go 判定用 (§12)

| # | 命題 | 根拠の在り処 |
|---|---|---|
| Q1 | review-v0 の実行は外部サービスと boid 状態への書き込みゼロで完了している (mutation は dry-run reporter への記録のみ) | shadow run のログと reporter 出力 |
| Q2 | §12.4 の各ケース分類に、履歴 replay ではなく live shadow の実測が少なくとも 1 件ある | ケース × run の対応記録 |
| Q3 | Signal に本文が含まれないケースで、agent が resource handle を辿って外部 resource を自発的に read した実測がある (H1/H3) | 該当 run の tool call 記録 |
| Q4 | cross-service ケースで、Signal の発生元と異なる service を agent が照会し正しく突合した実測がある (H5) | 該当 run の記録 |
| Q5 | サービス固有の調査手順が workspace policy に一切書かれておらず、Pack 側 skill の知識だけで調査が成立している (H4) | review-v0 の policy 文と skill 一覧 |
| Q6 | 判断材料が不足するケースで、agent が捏造せず fail または保留を選んだ実測がある | 該当 run の記録 |
| Q7 | 同一 Signal に対する現行 `/khi-sweep` と review-v0 の提案を突き合わせた比較記録があり、同等以上と判定されている (H2) | 比較記録 |
| Q8 | 評価に API call 数・token・実行時間・費用の実測が含まれている | 計測記録 |

### B. core 境界の guard (§2.3・§11.2)

| # | 命題 | 根拠の在り処 |
|---|---|---|
| Q9 | core package から Jira/Slack/Bitbucket 固有 package の import が 0 件である | import グラフ / grep |
| Q10 | core の DB schema にサービス固有 field が存在しない | migration の diff |
| Q11 | daemon が SaaS credential を保持して外部 API を直接呼ぶ経路が存在しない (connector/review の外部到達は workspace sandbox + API gateway 経由のみ) | connector 実行経路のコード |
| Q12 | Pack が state machine・review verb・DB schema を拡張できる経路が存在しない | manifest schema と loader |
| Q13 | connector config は Pack manifest の schema で検証され、検証失敗が該当 review の起動を止める | loader とそのテスト |
| Q14 | skill/connector に service instance の論理名が決め打ちされておらず、解決済み service binding 経由で参照している (§7.2) | Pack 実体と binding 受け渡しコード |
| Q15 | Pack contract の conformance test が boid 側に存在し、公式 Pack 全てがそれを通る | テストと CI |

### C. Signal/Connector lifecycle (§5・§13-3)

Q17・Q18 は現行 khi の運用で実際に踏んだ障害 (栞が自分自身を越えられず再検知が
永久に続く、栞が処理中のまま残り source の検知だけ死ぬ) を契約要件へ昇格させた
もの。§14 で未決の transaction 境界がどう決まっても、この 2 つの不変条件は維持する。

| # | 命題 | 根拠の在り処 |
|---|---|---|
| Q16 | Signal の dedup は connector が生成する stable event key で行われ、同一 event の再投入が no-op になるテストがある | inbox 実装とテスト |
| Q17 | source cursor は処理済み Signal 自身を越えて前進し、同一 Signal が巡ごとに再検知され続ける状態が構造上起きない | cursor 前進の実装とテスト |
| Q18 | 処理中のまま残った Signal/subject は lease expiry により必ず再スケジュールされ、永久に「処理中」へ留まる終着状態が存在しない | lease 実装と expiry テスト |
| Q19 | cursor 前進と Signal 永続化の順序が定義され、クラッシュ時に Signal を取りこぼさない (at-least-once) ことがテストで示されている | transaction 境界のコードとテスト |
| Q20 | retry 上限を超えた Signal は dead letter へ移り、CLI または Web UI から可視である | dead letter 実装 |

### D. Review run と mutation protocol (§8〜§10)

| # | 命題 | 根拠の在り処 |
|---|---|---|
| Q21 | ReviewRun は §9.1 列挙の snapshot 項目 (workspace revision・review 定義の revision・Pack version/digest・policy instruction・model/runtime 設定・対象 signal IDs・service bindings) を全て記録する | run 作成コード |
| Q22 | review 定義の更新は実行中 run へ影響せず、次の run から反映されることを示すテストがある | run 実装とテスト |
| Q23 | run token で「lease 外 subject」「他 workspace」「未許可 mutation」「transition の直接実行」「complete 後の書き込み」が全て拒否され、それぞれにテストがある | 認可コードとテスト |
| Q24 | 各 review mutation は冪等で、同一 run の再実行が no-op になるテストがある | mutation 実装とテスト |
| Q25 | child upsert は stable-key により冪等で、再実行しても子 task が重複生成されない | child upsert 実装とテスト |
| Q26 | suggestion が人の承認なしに状態遷移として適用される経路が存在しない | suggestion 適用経路 |
| Q27 | review job の外部サービス access は review_access の解決結果に限定され、書き込み操作を含まない | job token の scope と gateway 側検証 |
| Q28 | 手動 `boid review run` と scheduler が同一の application service を経由している (§9.2) | 呼び出しグラフ |

### E. 全体 (どの段階でも採点)

| # | 命題 | 根拠の在り処 |
|---|---|---|
| Q29 | この PR が新設・変更した挙動には対応するテストがあるか、無い理由が PR 上に明記されている | diff とテストの対応 |
| Q30 | khi 固有語彙が core の型・SQL・protocol prompt に入っていない (§11.3-6) | grep |
| Q31 | 当該段階の切替後も現行 khi と並走可能であり、writer・scheduler・adapter・prompt の一斉切替をしていない (§13) | PR description と切替手順 |
