# 取り込み identity と未着インボックス (設計メモ)

2026-08-18。 **まだ実装しない。** `cross-project-issue-triage.md` (以下「本編」) の
Phase 1 完了後に見つかった構造的な積み残しを、 独立した subsystem として切り出した検討。

workspace 側の対になる memo は khi-task-collector リポジトリの
`docs/plans/ingestion-identity.md`。 そちらは「khi の何が消えるか」を書いており、
本 doc は **boid コアに何が要るか**を書く。

## 発端: 決定 14 は fold を 1 つに減らしきれていない

決定 14 (state の正は daemon ただ 1 つ) は、 workspace 側の `decisions/*.jsonl` を退役
させた。 その決め手は khi 側 doc (`docs/note-events.md`) にこう記録されている:

> **2 つの fold を並べると、ロジックがズレたときに誰も気づかない** (決定 14 の決め手)

ところが退役したのは `decisions` だけで、 **`claims` + `fold_claims()` は残った**。
現状の khi はこう動いている:

```
claims (workspace の JSONL)  ──fold_claims──▶  card (workspace の畳んだ状態)
                                                    │
                                              daemon_sync (差分 reconcile)
                                                    ▼
actions (daemon のログ)  ──FoldDetailAttrs 等──▶  task_triage.detail
```

daemon 側は既に同じ構造を持っている:

- `internal/orchestrator/task_triage.go` の `FoldDetailAttrs()` / `AddDetailChild()` /
  `SpecDetailChild()` が fold の実体
- `machine.go` の `attrs_set` / `child_added` / `child_specced` が non-transitioning
  action として登録され、 届くたびに畳む
- 「決定13: event 追記を正、 state は導出」を `ParkedFrom` で実践している

つまり **fold は今も 2 つあり**、 `daemon_sync.py` (572 行) はその 2 つの間の reconcile を
している。 決定 14 の決め手はまだ完全には満たされていない。

## なぜ workspace 側の fold が消せなかったか

workspace が自前の畳んだ状態を持たざるを得ない理由は 2 つある。 どちらも
「workspace の都合」ではなく **daemon 側の口の不足**である。

### 理由 1: イベントの配送先を daemon が解決できない

外部ソースの観測は「Jira issue key」等のキーで飛んでくるが、 対応する triage task が
まだ存在しないことがある。 現行の action は `task_id` 必須なので、 **宛先未確定の
イベントを daemon は受け取れない**。 だから workspace 側が「キー → card」の索引を持ち、
着地させてから push する形になっている。

重要なのは、 **この配送先解決が判断ではない**こと。 khi の `scripts/match_card.py` は
冒頭でこう明言している:

> **照合は判断ではなく索引引き**。 2026-08-07 に同じ PR の card が 2 枚に分裂した事故は、
> これを LLM にやらせていたために起きた。

索引キーは `source.issue_key` / `source.thread_ts` / `related_jira_issues[]` の 3 つで、
**そのキーは既に daemon の中にある** (khi が `attrs_set` の opaque key として
`source` / `related_jira_issues` / `canonical_ref` を毎サイクル押している)。 加えて
`ref` 列は最初からこの用途で作られている (`internal/api/task_create.go:251` の
「jira issue_key / slack thread_ts / mail message-id」)。

決定 16 は既に **`ref` は canonical source のキーにする**と決めている。 足りないのは
**1 task が複数キーを持てないこと**と、 **キーから task を引く口が無いこと**だけ。

### 理由 2: actions の履歴を読む口が無い

workspace 側の `evaluate.py` は「呼び直しの抑止」を履歴から判定している:

- `speech_index()`: 「LLM が最後に喋った時刻」と「最後に事実が来た時刻」を分け、
  後者が新しいときだけ LLM を起こす (LLM の発言が自分自身の再トリガーにならない)
- `rejection_index()`: 却下された提案を `(slug, verb, basis)` で覚え、 同じ根拠の
  再提案を捨てる

どちらも actions ログがあれば daemon 側で計算できる。 が、 `internal/sandbox/protocol.go`
の `BoidOp` 一覧に **`action_list` 相当が無い**。 `action_send` で書けるのに読めない。
`task_triage_get` / `task_triage_list` が返すのは畳んだ後の `detail` だけで、 導出
フィールドとして露出しているのは `ParkedFrom` のみ。

だから workspace は judge ゲートを作るために自前の履歴 (claims) を持つしかなかった。

## 提案: 外部キーを task の identity として扱う

外部データソースのキーを、 **ユーザに対する identity** と同じものとして扱う。 ユーザが
メールアドレスや GitHub アカウントを複数持ち、 どれからでも同じ本人に解決できるのと
同じ構造にする (nose 案)。

| 認証の世界 | ここでの対応物 |
|---|---|
| principal | triage task |
| identity | 外部キー `jira:ROOKPF-289` / `slack:1785803139.540489` |
| identity → principal の解決 | イベントの配送先解決 (索引引き、 判断ではない) |
| サインアップに使った identity | `ref` = 決定 16 の canonical source |
| あとから link した identity | `related_jira_issues` 由来のキー |
| 未登録 identity でのアクセス | **未着インボックス**行き。 起票するかは判断 |
| アカウント統合 | 分裂した card の identity 付け替え (管理操作) |

不変条件は 2 つ:

- 1 つの task は identity を **複数**持てる
- 1 つの identity は **高々 1 つの task** にしか紐付かない

### identity は名前空間付きの不透明文字列

`<namespace>:<key>` の形とし、 **daemon は中身を一切解釈しない**。 `jira` が何かも
`ROOKPF-289` の意味も知らず、 ただの文字列 → task_id の索引として扱う。 名前空間を
決めるのは workspace 側。

これは逆輸入 3 の境界 (チャネル固有の知識は workspace 側に置く) と、
`FoldDetailAttrs` の宣言:

> Deliberately a PURE fold with zero policy: it does not know or care what keys mean

を崩さないための形。 **多対一の索引を足しても daemon はチャネル知識ゼロのまま**でいられる。

副次的な効果として、 子 task の `Ref` (`Dispatch` が `children[i].ID` を入れている) と
外部キーを名前空間で分離できる。 現状は同じ `ref` 空間に内部 ID と外部キーが同居している。

## 既存の決定との関係

| 既存 | 本 doc の扱い |
|---|---|
| **決定 16** (`ref` は canonical source のキー) | **一般化する**。 `ref` は「起票に使った 1 本目の identity」= primary。 決定 16 の契約 (`source_closed` を報告できる source を必ず持つ) はそのまま |
| **決定 17** (`reopen_triaged`) | **引き金を与える**。 第 12 版は「done を探す経路は既に成立している (`FindTaskByRef` に status 絞りが無い)、 足りないのは押せる action だけ」としたが、 その action を**誰が何を見て押すか**が空いていた。 下記 I-5 が埋める |
| **論点 g** (洪水対策 = 同一 source ref の再 push を update 扱い) | **置き換わる**。 「1 つの ref で dedup」から「多対一の identity 索引 + binding のライフサイクル」へ一般化される |
| **論点 b** (定期起動の機構: host cron vs daemon 内 scheduler) | **judge の部分だけ内蔵側に倒れる**。 下記 B-5 |
| **論点 h** (source 既読化・ cursor は workspace 側) | **変わらない**。 外部 API を叩く窓は workspace のもの |
| **決定 12** (queue 評価は決定論のみ) | **守る**。 identity 解決も judge 述語もエージェント判断ゼロ |
| **決定 13** (event 追記を正、 state は導出) | **徹底する側**。 workspace 側の二本目のログを畳む |

## 決めたこと (workspace 側 memo と共通、 番号も共通)

| # | 決定 | 根拠 |
|---|---|---|
| I-1 | 外部キーを identity として task に多対一でぶら下げる。 `ref` はその特例 | 多対一が実需 — Slack 起票の card に後から Jira のコメントが来る経路がある |
| I-2 | identity は `<namespace>:<key>` の不透明文字列。 daemon は解釈しない | 逆輸入 3 の境界を越えないため |
| I-3 | identity のスコープは **project** | 取り込まれるメタタスクはメタプロジェクトにしか紐づかない。 他プロジェクトへの展開は子 task (`spec.project`) が担い、 子は identity を持たない。 `(ref, parent_id, project_id)` unique (migration 0037) がそのまま使える |
| I-4 | 未着 identity のイベントは**インボックス**に積む。 自動で task 化しない | 起票に値するかは判断。 決定 14 の「判断は Web UI」に寄せる |
| I-5 | **done では identity を握り続ける**。 `observed.source_closed` が true → false に戻ったら `reopen_triaged` | 同じソースが動いたら普通は reopen したい (nose)。 決定 15 (auto-done) の鏡像で、 読むキーも同じ 1 個だけなので新しい policy が要らない |
| I-6 | **drop では identity を自動解放する** | drop は「この identity との関係を切る」判断そのもの。 握ったままだと、 捨てた件が動いても着地せず・ reopen もできず (`machine.go:371` の `reopen_triaged` は done/aborted からのみ)・ 同じキーで新規起票もできない、 という静かに詰む形になる |
| I-7 | 却下を **action** として記録する (`suggestion_answered` 相当、 non-transitioning / `Manual: true`)。 payload は `{answer, verb, basis}`、 副作用として `detail.attrs.suggestion` を落とす | 却下は attrs の更新ではなく出来事。 下記の穴を塞ぐ |
| I-8 | サンドボックスから actions を読む口 (`action_list` 相当の `BoidOp`) を足す | judge ゲートと却下履歴の判定材料を daemon の履歴で賄うため |

### I-5 は workspace 側の現行ルールの意図的な反転

khi の `match_card.py` は今、 終端 (`done` / `dropped`) の card を索引から除外している
— 「片付いた件に後から追記すると応答済みのものが動いてしまう」ため。 I-5 はこれを
引っくり返し、 「動いてしまう」を望ましい挙動と見る。 **退行ではなく意図的な方針変更**
なので理由ごと記録する。

identity モデルにすると、 この規則を「索引からフィルタする」形で書く必要が無くなる。
done は握り続ける (I-5)、 drop は解放する (I-6) という **binding のライフサイクル**として
表現され、 `ref` の unique 制約と衝突しない。

### 副産物: 暗黙則が不変条件になる

`match_card.py: build_key_index()` の既知の制約 —「複数の active card が同じ issue を
参照する場合、 観測は `updated_at` が最新の 1 枚にしか届かない」— は、 配送先が黙って
変わる形で事故のタネになっている (2026-08-09 に ROOKPF-289 で起きかけた)。

identity モデルでは「1 identity は 1 principal」が明示された不変条件になり、 2 枚目が
同じ identity を link しようとしたら **silent newest-wins ではなく拒否**になる。

## I-7 の動機: 今すでに開いている穴

khi の `fold_claims()` は「同じ根拠で却下済みの提案は出し直さない (`basis` が変わるまで)」を
実装しており、 入力は `suggestion_answered` claim である。 その書き手を確認すると、
khi の note-inbox スキルにこうある:

> 採用/却下は **Web UI** で出す (決定14)。 ここで答えた場合は
> `scripts/claim.py suggestion_answered` で却下履歴として残す

つまり **note-inbox で答えたときしか却下履歴が残らない**。 決定 14 で判断を Web UI へ
移したのに、 Web UI 側に `suggestion_answered` を生む経路が無い。 daemon 側を検索しても
`suggestion` は完全に opaque key 扱いで、 daemon は存在すら知らない。

結果として **Web UI で却下しても再提案の抑止が効かない** (次の巡で同じ提案が出し直される)
はずである。 I-7 はこれを構造的に塞ぐ。

### 境界の判断: 解釈するキーが 2 個目になる

`internal/orchestrator/triage_done.go` は、 daemon が opaque blob から読むキーは
`attrs.observed.source_closed` **ただ 1 つ**だと明言している (逆輸入 3 の境界)。
I-7 で `suggestion` が 2 個目になる。

**越境ではないと整理する** — 「エージェントが次の一手を提案し、 人が採否を答える」は
チャネル固有の知識ではなく、 triage という営みの共通語彙である (決定 16 が
`source_closed` を「common-language member」として選んだのと同じ基準)。 ただし
**意識して足した**ことをここに記録する。

## boid 側に要るもの

| # | もの | 見立て |
|---|---|---|
| B-1 | identity 索引テーブル + 解決 + link / unlink op。 `ref` はその特例として吸収 | 新規。 migration 1 本 + store + `BoidOp` |
| B-2 | 未着イベントのインボックス (project スコープ) と、 そこからの起票 / 破棄 | 新規。 Web UI を含む |
| B-3 | `action_list` 相当の `BoidOp` (workspace スコープ) | 小。 `ListActionsByTask` は既にある (`internal/api/task_service.go:413`) |
| B-4 | `suggestion_answered` action (I-7) と、 Web UI の却下からの発行 | 小〜中 |
| B-5 | judge 述語 + sweep | 中。 B-3 の後。 下記 |
| B-6 | `source_closed` 反転による auto-reopen (I-5) | 小。 `reopen` → `reopen_triaged` のルーティングは実装済み (`internal/api/triage_done.go:299`) |

### B-5: judge ゲートは既存の述語群と同じ形

`evaluate.py: speech_index()` の判定は **時計と DB だけで決まる純粋述語**であり、
daemon には既に同じ形のものが並んでいる:

- `orchestrator.ShouldWake()` (`queue.go`)
- `orchestrator.ShouldAutoDone()` (`triage_done.go`)
- `orchestrator.WatchdogGuidance()` (`watchdog.go`)
- これらを回す判断ゼロの周期 sweep — `api.SweepWake()` (`queue_sweep.go`、 `ActorDaemon` を刻む)

B-3 で actions が読めるようになるなら、 judge の判定材料は全部 daemon の中にある。

**`Action.Actor` は識別に使えない**ことに注意 — workspace の押し込みは観測も提案も同じ
ingestion task から出るので `Actor` は全部 `task:<id>` に潰れる。 代わりに **payload で
識別する**: `suggestion` キーを含む `attrs_set` が「エージェントが喋った」、 それ以外の
`attrs_set` が「事実が来た」。 daemon は依然として値を解釈しない (キーの有無だけ見る)。

### workspace 側の cadence は自動で縮む

khi は現在 `run-cycle.sh` (188 行) + `cycle_preflight.sh` (150 行) で「決定論パートを
boid task 抜きで毎サイクル回し、 LLM に用があるときだけ task を起こす」足場を組んでいる。
B-1〜B-5 が入ると preflight の 5 ステップのうち `daemon_sync` が消え `evaluate` が
daemon 側へ行き、 **残りに gate ロジックが 1 つも無くなる**。 「LLM に用があるか」を
判定するのも task を起こすのも daemon 側になる (`auto_start` / `queue_notify` は既にある)。

論点 b (host cron vs daemon 内 scheduler) のうち、 **判定と起動だけが内蔵側に倒れる**。
外部 API を叩く窓 (poll の cursor と頻度) は論点 h の通り workspace 側に残る。

## 段階

1. **B-1 + B-2** — identity と インボックス。 土台
2. **B-3 + B-4** — actions の読み口と却下 action。 ここで workspace 側の claims / fold /
   match / daemon_sync が消える
3. **B-5 + B-6** — judge 述語と auto-reopen。 ここで workspace 側の `evaluate.py` と
   cycle の足場が消える

## 非目的

- **identity を認証・認可に使わない**。 ここでの identity は「イベントの配送先を決める
  索引」であって、 principal の権限とは無関係。 名前が示唆する以上のことをさせない
- **identity のチャネル知識を daemon に持たせない**。 名前空間の意味・ 正規化・ 妥当性検査は
  すべて workspace 側 (I-2)
- **未着インボックスからの自動起票**。 起票は判断であり、 決定 14 の線を越える (I-4)

## 未確定

- **auto-reopen のフラップ対策** (I-5)。 ソースが閉じ / 開きを繰り返すと task が ping-pong
  する。 「自動 reopen は 1 回だけ、 2 回目以降は人に見せる」等が要りそう
- **インボックスの UI と滞留の扱い**。 未着イベントが溜まり続けたときに誰がどう気づくか
  (watchdog に載せるか、 件数を出すだけか)。 論点 g の洪水対策がここに移る
- **identity の link を誰が主張してよいか**。 khi の `related_jira_issues` は今 LLM が
  書いている。 identity として登録するなら「1 identity 1 principal」に反する link 要求が
  来たときの応答 (拒否して人に見せる、 が素案) を決める必要がある
- **未着イベントの保持期間と GC**。 インボックスは新しい永続データであり、 既存の GC
  (30 日ルール) に載せるかを決める
- **移行手順**。 既存 triage task の `source` / `related_jira_issues` / `ref` からの
  identity 一括投入と、 workspace 側 claims の退役順序
- **本編への取り込み方**。 本 doc を独立 subsystem のまま進めるか、 Phase 2 の項目として
  本編に畳むか
