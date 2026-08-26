# Signal-driven review と Integration Pack

2026-08-26 起案 (r1: GPT-5.6-sol)。同日 r2 でレビュー指摘を反映、r3 で全面改稿。

ステータス: **方向性の設計メモ**。r1 が core に持たせていた review 実行機構
(workspace automation 定義・review scheduler・ReviewRun・mutation protocol) は、boid に
既にある trigger・task・behavior・skill の並行世界の複製 = 過剰設計と判断して撤回した
(経緯と対応表は §9)。core の責務は ingest と Integration Pack に絞る。実装計画へ進む前に
§10 の検証を行う。

関連文書:

- `docs/plans/ingestion-identity.md` — trigger、identity、daemon/workspace 境界の現行設計
- `docs/plans/suggestion-as-state-transition.md` — suggestion と人の判断の境界
- `docs/plans/workspace-default-project.md` — workspace default と project 定義
- `docs/plans/api-gateway.md` — service registry と workspace-scoped credential 境界
- khi-task-collector の `docs/plans/signal-and-summary.md` — 現行 sweep の設計
- `docs/plans/signal-envelope-inventory.md` — §10.2 机上検証の結果 (envelope v0 の根拠)
- `docs/plans/signal-ingest-detailed-design.md` — 移行順 step 2/6 の実装設計 (PR 分割と採点表の割り当て込み)

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
r3 での行き先の分類とともに示す。

| khi が再実装していた機構 | r3 での行き先 |
|---|---|
| action 列のページングと cursor | core ingest (source cursor) |
| event key の dedup | core ingest (inbox) |
| attempts、retry、dead letter | core ingest (connector 実行) |
| 処理済み signal の記録 | core ingest (ack) |
| sweep の single-flight | trigger のプロパティ |
| sweep task の作成と終了管理 | trigger (通常 task の dispatch) |
| dirty な task の抽出と batch 化 | workspace の scan script (残留。篩もここ) |
| boid CLI 応答の型変換 | 既存コマンドの出力整備で縮小 (残留) |
| status ごとの書き込み可否検証 | 機構が強制済み (機械遷移なし・readonly) のため消滅 |
| summary、suggestion、child spec の差分更新 | 既存コマンドの upsert 性で消滅 |

workspace ごとに再実装が要るのは上表の「残留」だけになり、それは機構ではなく方針と
糊である。一方、Jira、Slack、Bitbucket の具体的な API 実装や reference skill をすべて
boid binary に埋め込むと、core の肥大化、SaaS API と boid 本体の release cycle の結合、
全 job への不要な skill 配布を招く。

本 doc は次の境界を引く。

1. boid core は signal の ingest (connector 実行・inbox・cursor・dedup) と trigger を持つ
2. サービス固有知識は versioned Integration Pack として配布する
3. 判断は workspace の通常 task が行い、workspace は判断方針と少量の糊だけを持つ

---

## 2. 目標

### 2.1 設計目標

> 外部・内部の変化を受け取り、boid 上の work item を再判断して次の状態・作業を提案する
> 仕組みを、workspace 側は「判断方針と少量の糊」だけで構成できる。workspace に機構
> (paging、dedup、retry、lifecycle 管理) を再実装させない。

「workspace 固有コードなし」は目標にしない (r1 からの変更、理由は §9)。workspace には
メタプロジェクトが残り、判断方針・workspace 固有のワークフロースキル・inbox を
スキャンして判断タスクを起こす小さな script を持つ。排除するのはコードではなく
機構の再実装である。

既存 Integration Pack だけを使う workspace は、次で追加できる状態を目標にする。

- service instance と credential の binding
- source (connector) の選択・検索条件
- 判断方針と inbox スキャンの小さな script を持つメタプロジェクト
- 必要に応じた project 固有の補足

### 2.2 スコープ

本設計は「任意の SaaS workflow engine」を目指さない。対象は次に限定する。

> 外部・内部の observation を契機に、boid の task/card を reconcile する自動処理。

入力 (source) は Pack で拡張可能にするが、出力は boid の task model と state machine に
閉じる。Pack に独自 DB field、独自状態遷移を追加させない。

### 2.3 非目標

- boid daemon が直接 SaaS credential を持って外部 API を呼ぶこと
- Jira/Slack/Bitbucket の意味を orchestrator package に埋め込むこと
- すべての外部データを共通 schema に正規化すること
- 判断タスクから外部サービスへ無承認で書き込むこと
- core に判断用の実行機構 (run 記録・scheduler・専用 mutation protocol) を作ること (r1 撤回)
- core が判断用の protocol prompt を供給すること (契約は機構で強制する)
- 篩・batch 化を core の機構にすること (workspace の責務)
- 現行 khi のコードをそのまま core package へ移植すること

---

## 3. 中心となる責務境界

| 観点 | 所有者 |
|---|---|
| credential と外部 API へ到達する実行コンテキスト | workspace |
| connector 実行、inbox、cursor、dedup、trigger | boid core |
| API 固有の解析・探索知識 | Integration Pack |
| 篩・batch 化・判断タスクの起こし方 | workspace (メタプロジェクト) |
| 何を重要とみなし、何を提案するか | workspace/project policy |
| task 状態・identity・履歴の正 | boid core |
| 最終判断 | 人 |

外部 connector も判断タスクも workspace の sandbox 内で実行し、既存 API gateway と
workspace-scoped job token を使う。credential 境界は現行から変わらない。

---

## 4. 全体像

```text
                  ┌─────────────────────────────┐
                  │      Integration Pack        │
                  │      connector / skill       │
                  └──────────────┬──────────────┘
                                 │ workspace sandbox で実行
                                 ▼
External services ───────▶ Signal (envelope)
                                 │
                                 ▼
                    Signal inbox（core: dedup・栞）
                                 │
                                 ▼
              trigger: inbox updated（debounce / single-flight）
                                 │
                                 ▼
        workspace の判断タスク（メタプロジェクト内の通常 task）
          scan script が篩・batch 化して判断 agent を起こす
          ├── 読み: boid signal list・Pack skill・API gateway (live)
          └── 書き: 既存の boid コマンド（summary / suggestion / child …）
                                 │
                                 ▼
                              人が判断
```

core が所有するのは inbox までと trigger である。判断は現行 khi の sweep と同じく
workspace の通常 task であり、core は判断の実行機構を持たない。

---

## 5. Signal は「起動封筒」である

### 5.1 判断材料の完結形にしない

現行 khi の運用では、LLM が必要に応じて元の Slack thread、Jira issue、Bitbucket PR、
関連する別サービスを読めないと、提案精度が著しく落ちることが確認されている。

したがって、共通 Signal model は外部サービスを抽象化して隠すものではない。

> Signal は「いつ、何について調べ直すか」を伝える起動封筒であり、判断に必要な情報を
> すべて内包する snapshot ではない。

core は control plane だけを抽象化する。判断 agent は Integration Pack の skill と
service binding を受け取り、data plane ではサービス固有の API を意識して調査する。

### 5.2 envelope v0 = `boid signal list` の出力

envelope は新しい protocol ではなく、inbox を読む CLI の出力 schema である。
机上検証 (§10.2、`docs/plans/signal-envelope-inventory.md`) で現行 khi の本番実装・
実データと突き合わせて v0 を確定した。現行の Signal 型 (6 field) と同型であり、
r1 案にあった title (必須扱い)・preview・hints・resource.kind・identities[] (複数) は
使用実績が無いため落とし、現行が持つ author を加えた。

```yaml
id: slack:C043YAL7G15:1787711028.007819    # 必須。connector が同一 event から必ず同じ値を生成 (dedup キー)
occurred_at: 2026-08-26T02:23:48Z          # 必須。RFC3339 tz-aware。source cursor の材料
source:                                    # 必須。provenance
  pack: slack
  connector: mentions
  service: slack-api                       # service instance 名
identity: slack-thread:1787711028.007819   # 必須。機械的に導出できる帰属候補 (単数)
url: https://khi.slack.com/archives/...    # 任意 (強く推奨)。原文への入口
author: self | <生ID> | null               # 任意。篩の材料。self は「自分自身」の共有語彙
title: "..."                               # 任意。篩の材料。判断は依存しない
```

- dedup と栞 (cursor) の単位は **(service instance, connector, id)** の複合
- `identity` の語彙は **workspace 全体の共有語彙** — bitbucket connector が `jira:<KEY>` を
  出して PR コメントと Jira 課題を機械的に合流させるのが本番実績。Pack contract で
  identity を pack-scope に閉じてはいけない
- `author` の `self` 正規化 (生 ID と自分の識別子の比較) は connector の責務。落とすか
  どうかは workspace の篩が決める (§8.4)

`identity` や `id` の中身 (`slack-thread:` が何を指すか) は core が解釈しない。Pack の
skill と、それを読む判断 agent が解釈する。

### 5.3 Connector の責務

外部 signal から共通 model への変換は Integration Pack の Connector が行う。

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

### 6.2 Pack が提供する能力

| 能力 | 役割 |
|---|---|
| Connector | 新しい変化を検出し Signal を出す決定的処理 |
| Resource resolver | Signal が指す canonical resource を再取得する決定的処理 |
| Reference skill | LLM が必要に応じて検索範囲を広げるための API 知識 |

resolver の必要性・粒度は検証で決める。最初は既存 API gateway と `boid-api-skills` の
reference skill で検証し、それで不十分な操作だけを型付き resolver として昇格させる。

Pack の skill は source 側の知識 — 判断中の読み方・調べ方と、child task が使う外部への
書き込み手順 — だけを扱い、boid コマンドには言及しない。boid の使い方は core の
組み込みスキルが担う (§8.2)。

Pack と boid の間の契約 (両側の義務・版管理・進化規則) は
`signal-ingest-detailed-design.md` §7 (Pack contract v1) が正である。

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

`serviceProfiles:` が §7 の service profile の定義である。Pack は接続の型だけを宣言し、
endpoint の実値や credential をここに書くことはできない。

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
- job には利用する skill だけを read-only mount する
  (組み込みスキルと同様、dispatcher が job sandbox 構築時に agent のスキル探索パス配下へ bind mount する)

Pack install は daemon 管理者だけが実行できる。project.yaml や判断 agent が自分で
Pack を install/upgrade して権限を増やすことはできない。

---

## 7. Service profile と service instance

### 7.1 定義場所と関係

profile は「接続の型」、instance は「実際の接続 1 本」である。定義場所も書き手も違う。

| | service profile | service instance |
|---|---|---|
| 何か | 接続の型。必要な credential slot と注入方法の宣言 | 実際の接続 1 本。endpoint と secret の bind |
| どこに定義 | Pack の `integration.yaml` の `serviceProfiles:` (§6.3) | daemon の `config.yaml` の `services:` (既存 registry) |
| 誰が書く | Pack 作者 (配布物の一部) | 運用者 (環境ごと) |
| 値 | 持たない (endpoint 実値・credential を書けない) | 持つ (endpoint と SecretStore key の bind) |

同じ profile から複数の instance を作れる (例: 顧客 Jira と自社 Jira は同じ
`jira-cloud` profile の別 instance)。gateway には instance 名で生える
(`$BOID_API_BASE/customer-jira`)。

分担は「仕様は profile、値は instance」である。注入方法 (`injection: bearer`) は
profile 側にあり、instance は slot へ SecretStore key を割り当てるだけで注入のやり方を
知らない。endpoint も profile が要否 (`configurable`) を宣言し、instance は値を埋める。
§6.3 の例が薄いのは最小例だからで、現実の profile はここが厚くなる — endpoint 固定の
サービス (Slack) は `configurable: false` + 既定値を宣言する、OAuth2 が要るサービスは
フローと token endpoint を宣言する (現行 gateway の OAuth2 対応を Pack 側へ移す)、
複数 header の表現は未決 (§12)。既存の自由形式 registry ではこの機械的仕様を運用者が
`auth:` として毎回書いており、それを Pack 作者へ移すことが profile の存在理由である。

### 7.2 service instance の定義例

次は `config.yaml` の `services:`、すなわち **service instance** の定義例である。
`customer-jira` と `internal-api` が instance 名になる。

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

- `uses:` — **インストール済み Pack が宣言する profile への参照**。書式は
  `<pack 名>/<profile 名>@<pack version>` で、例は Pack `jira-cloud` の中の profile
  `jira-cloud` (同名) を指す。install の指示ではない — install は daemon 管理者の操作
  (§6.4) であり、`uses:` はインストール済み Pack から選ぶだけ。未インストールの参照は
  設定エラーにする。version は Pack の版。例は instance 単位の pin を示しているが、
  pin の置き場所 (install 時 / instance / workspace) は未決 (§12)
- `endpoint:` — profile が `endpoint.configurable: true` と宣言した記入欄を埋める値
- `credentials:` — profile の credential slot (`token`) へ SecretStore の key
  (`JIRA_TOKEN`) を bind する。値そのものは SecretStore にあり、config.yaml にも
  Pack にも現れない
- `internal-api` — profile を持たない従来の自由形式 instance。任意 HTTP API を扱う
  escape hatch としてそのまま維持し、既存 registry を置き換えない

運用者が profile の中身 (integration.yaml) を読み込む必要はない。profile は自己記述的で
あり、instance の設定に必要な項目 (必須 slot、endpoint の要否) は boid が profile から
提示し、config 読み込み時に instance を profile と照合して検証する — 不足 slot・
未知 slot・必須 endpoint の欠落は設定エラーにする。提示の UX (`boid integration show` /
対話的な service 追加) は未決 (§12)。

### 7.3 論理名を skill に決め打ちしない

`jira-api` 等は慣例名であって固定名ではない。connector 実行と判断タスクには解決済み
service binding を渡す。key が profile の論理名、value が選ばれた instance と
gateway URL である。

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
自身の service 名で gateway を参照する。判断タスクが読める service の範囲は、既存の
workspace の service スコープのまま変えない。

---

## 8. Inbox、trigger、判断タスク

### 8.1 Inbox

inbox は core の永続 store で、Connector が生成した Signal を保持する。契約:

- dedup: 同一 idempotency key の再投入は no-op
- source cursor は処理済み Signal 自身を越えて前進する
  (同一 Signal の再検知が構造上起きない)
- 取り込み中のまま残る終着状態を持たない (connector 失敗は attempts として記録し、
  上限超過は可視化する)
- Signal の ack は判断側 (workspace) が行い、ack は冪等
- ack 済み Signal の保持期間は未決 (§12。既存の 30 日 GC に載せる想定)

上 2 つの不変条件は現行 khi の運用障害 (栞が自分自身を越えられない再検知ループ・
栞が処理中のまま残る deadlock) の再発防止であり、§14 の採点表で検査する。

### 8.2 CLI と組み込みスキル

判断タスクが inbox を読むための CLI を用意する。

```text
boid signal list [--source <pack>/<connector>] [--json]   # 未 ack の Signal
boid signal show <id>
boid signal ack <id>...                                    # 冪等
```

使い方は組み込みスキル (boid-task と同格) として配布する。これは知識であって契約では
ない。判断タスクが守るべき境界は prompt ではなく機構で強制する。

- 状態遷移は suggestion 経由のみ (card の状態機械に機械遷移はない)
- job token は workspace に閉じる
- 書き込み可否は task.readonly で決まる

r1 の「上書き不可の core review protocol prompt」はこの機構強制で置き換える (§9)。

### 8.3 Trigger source: inbox updated

既存 trigger 機構に「inbox が更新された」を発火条件として追加する。

- debounce: 短時間に連続する Signal をまとめて 1 回の発火にする
- single-flight: 前回起こした判断タスクが未終了なら次を起こさない
- 発火先は workspace が定義する通常の task (メタプロジェクトの scan script / behavior)

connector の定期実行も同じ trigger 機構に乗せる (source 1 件 = 導出 trigger)。source の
宣言はメタプロジェクトの project.yaml (`signals.sources`) で行い、connector はその
プロジェクトの sandbox で走る。詳細は `signal-ingest-detailed-design.md`。

### 8.4 判断タスクと篩

判断タスクは通常 task である。メタプロジェクトの scan script が `boid signal list` で
未 ack を取得し、篩と batch 化を行って判断 agent を起こす。

- 明らかなゴミ (mail 等の低 S/N source) は script が ack して落とす。ノイズの質は
  source と workspace ごとに違うため、篩は core でなく workspace の責務とする
- 判断 agent は Pack skill と API gateway で外部を live 照会し、既存コマンドで書く
- 途中で死んだ場合、summary/suggestion は upsert なので適用済みは残り、未 ack Signal が
  残るため trigger が再発火し、再実行は自然に収束する

### 8.5 既存コマンドへの小さな足し算

- `boid task create` に idempotency key (stable key) を追加する。子タスクの重複生成は
  実際に踏んだバグであり、「再実行で収束する」性質に対する唯一の既知の欠けである
- 必要が実証されたものだけを足す。判断専用の書き込み CLI 名前空間は作らない

---

## 9. r1 からの変更 — 何を削り、なぜか

r1 は core に review の実行機構一式を持たせる設計だった。r3 でそれらを撤回した。
すべて boid に既にある機構の並行世界の複製だったからである。

| r1 の機構 | r3 の置き換え先 |
|---|---|
| workspace automation (`reviews:`) 定義 | 既存 task_behaviors + メタプロジェクトの scan script |
| review scheduler | 既存 trigger (source: inbox updated) |
| ReviewRun / `review_runs` | 通常の task / job 記録 |
| review mutation protocol + run token | 既存コマンド (upsert 性) + 機構による強制 |
| 上書き不可の core review protocol prompt | 機構による強制 + `boid signal` の組み込みスキル (知識) |
| `review_access` | workspace の service スコープ (既存 gateway のまま) |
| 指示の 4 層 | 既存の置き場所: 組み込みスキル / behavior instruction / Pack skill / project.yaml |
| core の篩の段 (r2 で未決化) | workspace の scan script |

根本原因は r1 の目標文「workspace 固有コードなしで構成できる」にある。これを文字通りに
最適化すると、workspace に残せない全機能へ core 側の住処を発明することになる。目標を
「workspace に機構を置かない。方針と糊だけ」に引き直すと (§2.1)、上表の全行が既存機構に
落ち、新設が要るのは ingest・Pack runtime・trigger source と少数の CLI だけになる。

この撤回によりメタプロジェクトは退役でなく縮退になる。khi には判断方針・workspace
固有のワークフロースキル・scan script が残り、workspace 横断タスクの対話セッションを
起こす起点としても存続する。

---

## 10. 実装前の検証

### 10.1 仮説

判断系を通常 task のまま維持したことで、r1 の仮説群の大半は「現行 khi が既に実証済み」
になった。残る仮説は 2 つで、互いに独立に検証できる。

| # | 仮説 | 検証 |
|---|---|---|
| A | Pack の connector と core ingest で、現行 khi collector と同等の Signal 列を作れる | shadow-a (LLM 不要のデータ比較) |
| B | サービス固有の調査知識を workspace prompt から Pack skill へ移しても、現行 sweep と同等以上の提案を維持できる | shadow-b (提案の比較) |

### 10.2 机上検証 (shadow の前)

khi の履歴から実 Signal を数件 (低 S/N の mail 想定を 1 件含む) 選び、envelope へ手書き
変換する。現行の判断が実際に参照している field を棚卸しし、envelope schema の必須/任意を
確定する。紙の上で書けないものは実装しても動かない。

**実施済み (2026-08-26)** — 結果は `docs/plans/signal-envelope-inventory.md`。本番の
sweep 82 本から実例 4 件 (slack 新規・jira 新規・bitbucket→jira 合流・mail 想定) を
変換し、envelope v0 を §5.2 に反映した。副産物として boid 内部シグナルの経路が未決と
判明 (§12)。

### 10.3 shadow-a: ingest の等価性

現行 khi collector を止めず、boid ingest (Pack connector) を並走させ、inbox に落ちる
Signal 列 (event key 集合と内容) を突き合わせる。判断系は一切関与しない。

- 並走開始時は両者の栞を同一時点に揃える (栞の巻き戻しは cutover 実績あり)
- 差分は 1 件ずつ原因を特定する (検索条件、cursor 解釈、filter の差)

### 10.4 shadow-b: 知識配置

同じ dirty 対象に対し、サービス固有 skill を Pack skill へ差し替えた判断タスクを
report のみ (書き込みなし) で並走させ、現行 sweep の提案と比較する。

- 対象ケース: cross-service 調査、本文非開示、外部完了済みの done 提案、材料不足の保留
- 評価: 提案の同等性、自発的な追加照会の有無、捏造の有無、API call 数・token・費用

### 10.5 失敗の読み方

| 症状 | 見直す境界 |
|---|---|
| shadow-a で Signal 列に漏れ・過剰がある | connector の検索条件・cursor 解釈 |
| envelope の field 不足で判断が劣化する | envelope schema (§10.2 へ戻る) |
| Pack skill があっても正しい調査手順を選べない | skill の記述・分割 (playbook の要否) |
| khi 固有の長い prompt がないと動かない | Pack/workspace への知識分解が不十分 |
| 提案は同等だが費用が跳ねる | 篩・batch 化・model 選択 (workspace 側) |

---

## 11. 想定する移行順

1. **机上検証** — 実 Signal の envelope 手書き変換と field 棚卸し (§10.2)
2. **core ingest** — inbox・cursor・dedup・`boid signal` CLI・組み込みスキル・
   connector 実行 (Pack runtime 最小)
3. **公式 Pack 化** — slack/jira/bitbucket の connector と reference skill を Pack へ
4. **shadow-a** — khi collector と並走し Signal 列を diff (§10.3)
5. **shadow-b** — 判断タスクの skill を Pack skill へ差し替えて提案を diff (§10.4)
6. **trigger source 追加と切替** — inbox updated trigger を追加し、khi の scan script を
   inbox 読みへ切替。旧 collector は並走のまま停止 → 削除
7. **khi 縮退** — ingest 実装と再実装機構を削除し、判断方針・スキル・scan script だけを残す

各段階で現行 khi と並走可能にし、一度に collector・trigger・skill を切り替えない。

---

## 12. 現時点の決定と未決

### 決定済みの方向

1. ingest (connector 実行・inbox・cursor・dedup) を core 機能化する
2. Signal は判断材料の完結形ではなく起動封筒とする (r1 から維持)
3. サービス固有 adapter/reference skill は Integration Pack に置く (r1 から維持)。
   公式 Pack は container image へプリインストール可能にするが binary embed しない
4. 判断は workspace の通常 task。core は判断の実行機構 (run 記録・scheduler・専用
   mutation protocol) を持たない (r1 撤回)
5. 書き込みは既存コマンド。契約は prompt でなく機構 (機械遷移なし・job token・readonly)
   で強制する
6. core は protocol prompt を供給しない。供給するのは `boid signal` の組み込みスキル (知識)
7. 篩・batch 化は workspace の scan script の責務
8. メタプロジェクトは存続する (khi は退役でなく縮退)
9. 実装は shadow-a (ingest 等価性)・shadow-b (知識配置) を Go 条件とする
10. envelope schema v0 を机上検証で確定した (§5.2、2026-08-26。根拠は
    `signal-envelope-inventory.md`)
11. CLI 最終形・inbox GC・connector 実行 (導出 trigger)・size limit・source 宣言場所は
    詳細設計で決着した (`signal-ingest-detailed-design.md` §9、2026-08-26)
12. Pack と boid の間の契約は `signal-ingest-detailed-design.md` §7 (Pack contract v1) を
    正とする — 両側の義務、apiVersion による版管理、追加のみの進化規則、conformance
    test の検査対象

### 検証・実装と並行して決めること

- boid 内部シグナル (action 列) の経路 — inbox へ統合するか、現行どおり workspace の
  scan script が `boid action list` を直読みし続けるか
  (初期実装は後者を推す。`signal-envelope-inventory.md` §6)
- Resource resolver を Pack contract に含めるか (据え置き)
- Pack の発見・配布・install/update/signing/version pin の具体方式
  (pin を instance の `uses:` で行うか install 時に固定するかを含む)
- Pack と既存 kit 機構の関係 (統合するか並置するか)
- service profile が複数 header・OAuth2 フロー等をどう表現するか
- instance 設定の発見・検証の UX (profile からの必須項目提示、対話的な service 追加)
- scan script の定型を組み込みスキル/テンプレとして配布するか
- Web UI における inbox・connector 失敗の表示

---

## 13. 一文での定義

> 本設計は、外部サービスの変化を Integration Pack と core ingest が標準化された Signal
> として inbox に集め、trigger が workspace の判断タスクを起こし、判断タスクが既存の
> boid コマンドで task を reconcile して人に提案する、という分業である。

---

## 14. 採点表 — レビュワー用 yes/no 判定リスト

機械的テストが拾えない「設計への適合」を、LLM/人間レビュワーが判定できる形に
落とした採点表。運用ルール:

1. 全命題は**「yes = 合格」に極性統一**してある。否定疑問文は使わない。
2. 判定者は各命題に yes/no と**根拠 (file:line、diff hunk、または shadow の記録) の
   引用**を付ける。**根拠を引けない yes は no として扱う**。
3. no が 1 つでもあれば通さない。直し方は 2 通りだけ——実装 (または検証) を直すか、
   設計判断を変えたのなら**先に本 doc の該当節を更新**してから通す
   (doc と実装の乖離を作らない)。
4. グループ A/B は shadow の Go/No-Go 判定で採点する。グループ C〜E は実装 PR の
   レビューで「該当グループ + 全体 (E)」を採点し、対象外グループは skip する。
   PR 分割が別 doc で確定した時点で、C〜E を PR 単位のグループへ割り直す。

### A. shadow-a の Go 判定 — ingest の等価性 (§10.3)

| # | 命題 | 根拠の在り処 |
|---|---|---|
| Q1 | 対象 source (slack/jira/bitbucket) それぞれで、並走期間中の Signal 列 (event key 集合) が現行 khi collector と一致するか、全差分の原因が特定・記録されている | diff 記録 |
| Q2 | 並走は live で行われ、履歴 replay のみではない | 並走記録 |
| Q3 | envelope に、§10.2 の棚卸しで「判断が参照している」と確定した field が全て載っている | 棚卸し表と schema の突き合わせ |
| Q4 | 並走開始時に両者の栞を同一時点に揃えた手順が記録されている | 手順記録 |

### B. shadow-b の Go 判定 — 知識配置 (§10.4)

| # | 命題 | 根拠の在り処 |
|---|---|---|
| Q5 | 同一対象への現行 sweep と Pack skill 版の提案比較記録があり、同等以上と判定されている | 比較記録 |
| Q6 | サービス固有の調査手順が判断タスクの workspace prompt に残っておらず、Pack skill 側だけにある | prompt と skill の突き合わせ |
| Q7 | Signal に本文がないケースで、agent が resource handle から外部を自発的に live 照会した実測がある | 該当 run の記録 |
| Q8 | 材料不足のケースで捏造せず保留した実測がある | 該当 run の記録 |
| Q9 | 評価に API call 数・token・実行時間・費用の実測が含まれている | 計測記録 |

### C. core ingest の実装 guard (§8)

| # | 命題 | 根拠の在り処 |
|---|---|---|
| Q10 | 同一 idempotency key の再投入が no-op になるテストがある | inbox テスト |
| Q11 | source cursor が処理済み Signal 自身を越えて前進し、同一 Signal の再検知が構造上起きないことを示すテストがある | cursor テスト |
| Q12 | connector 失敗が attempts として記録され、上限超過が CLI または Web UI から可視である | 実装とテスト |
| Q13 | crash 時に Signal を取りこぼさない (at-least-once) ことがテストで示されている | transaction 境界のテスト |
| Q14 | `boid signal ack` は冪等で、二重 ack がエラーにならないテストがある | CLI テスト |
| Q15 | trigger の debounce と single-flight にテストがある | trigger テスト |
| Q26 | `boid task create` の idempotency key は (project, key) で一意であり、同一 key の再実行が新規作成せず既存 task の id を返すテストがある | task_create 実装とテスト |

### D. Pack runtime の guard (§6・§7)

| # | 命題 | 根拠の在り処 |
|---|---|---|
| Q16 | core package から Jira/Slack/Bitbucket 固有 package の import が 0 件である | import グラフ / grep |
| Q17 | core の DB schema にサービス固有 field が存在しない | migration の diff |
| Q18 | daemon が SaaS credential を保持して外部 API を直接呼ぶ経路が存在しない (connector の外部到達は workspace sandbox + API gateway 経由のみ) | connector 実行経路のコード |
| Q19 | connector config は Pack manifest の schema で検証され、検証失敗が該当 connector の起動を止める | loader とそのテスト |
| Q20 | skill/connector に service instance の論理名が決め打ちされておらず、解決済み service binding 経由で参照している | Pack 実体と binding 受け渡しコード |
| Q21 | Pack の skill は source 側の知識のみを扱い、boid コマンドへの言及を含まない | conformance test |
| Q22 | Pack contract の conformance test が boid 側に存在し、公式 Pack 全てがそれを通る | テストと CI |
| Q27 | connector job の builtin op は signal 系 (ingest / cursor) のみに制限され、他の op と宣言外 service への gateway 到達が拒否されるテストがある | policy とテスト |

### E. 全体 (どの段階でも採点)

| # | 命題 | 根拠の在り処 |
|---|---|---|
| Q23 | この PR が新設・変更した挙動には対応するテストがあるか、無い理由が PR 上に明記されている | diff とテストの対応 |
| Q24 | khi 固有語彙が core の型・SQL・組み込みスキルに入っていない | grep |
| Q25 | 当該段階の切替後も現行 khi と並走可能であり、collector・trigger・skill の一斉切替をしていない (§11) | PR description と切替手順 |
