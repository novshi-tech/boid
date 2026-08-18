# 判断スケジューラと取り込み identity (設計メモ)

2026-08-18。 **まだ実装しない。** `cross-project-issue-triage.md` (以下「本編」) の Phase 1
完了後に見つかった構造的な積み残しを、 独立した subsystem として切り出した検討。

workspace 側の対になる memo は khi-task-collector リポジトリの
`docs/plans/ingestion-identity.md`。 **本 doc が先で、 khi 側はこれを参照する側**である —
先に daemon の持ち分を決め、 workspace 側は「その持ち分をこう使う」として書く。

同日の第 1 版・ 第 2 版は「claims 方言の boid コア移植」「取り込み identity」という部品の
話から入っており、 決定事項の羅列で全体像が見えないと nose に指摘されたため、 **原則と
To-Be を先に立てる形へ全面的に組み直した** (第 3 版)。

---

## 原則

| # | 原則 | 帰結 |
|---|---|---|
| P-1 | **daemon は決定論的にふるまう。 LLM の判断は workspace 側に置く** | 決定 12 の再確認。 daemon にエージェントを置かない |
| P-2 | **外部からのシグナル取り込みは workspace 側** | credential 境界 (決定 11 / 論点 h) と、 原文を越境させない設計 (決定 3) の帰結 |
| P-3 | **判断のトリガは daemon が出す** | P-1 + P-2 から自動的に出る。 下記 |

P-3 は独立した思いつきではなく、 P-1 と P-2 に **決定 13 (台帳は daemon に一箇所)** を
合わせると出てくる。 論証は 2 段:

1. **反応型の起動条件は daemon にしか書けない。** 「新しい事実が来たのに誰も反応していない」を
   評価するには actions の台帳が要り、 台帳は決定 13 / 14 で daemon にしか無い。 workspace 側に
   置こうとすると自前の履歴を持つことになり、 それが今の `claims` = fold 二本問題そのもの
2. **周期型だけ workspace に残すと決定論が両側に散る。** 周期型 (「N 経った」) は原理的には
   workspace 側の cron でも書ける — 現に今の `run-cycle.sh` がそうで、 循環はしていない。
   ただし起動の判定が 2 箇所に分かれ、 流量制御・ 直列化・ バックオフを両側で二重に実装する
   ことになる。 実際 khi は今それを自作している

つまり P-3 は「循環するから」だけでは導けず、 **反応型は強制・ 周期型は集約の選好**である。
結論 (daemon に寄せる) は変わらないが、 論拠が違うので分けて書く。

**例外**: 人が明示的に始める対話セッション (khi の `judge:spec` 相当) は述語から起こさない。
P-3 は「daemon が起こす判断」の話であって、 「人が始める判断」を禁じるものではない。

この 3 つから、 daemon の役割が決まる: **daemon は判断のスケジューラになり、 workspace は
判断の実装になる。**

---

## To-Be

```
  daemon (決定論)                              workspace (意味)
  ────────────────────────────────           ──────────────────────────
                                        
  述語が真になる                              
        │                                     
        ├──▶ 判断ジョブを起こす ──────────▶  task_behaviors.judge:<kind>
        │      (project の behavior を        で LLM が判断する
        │       task として dispatch)                │
        │                                            │
        └───── actions を畳む ◀──────────────────────┘
                 (attrs_set / child_* /
                  judged / proposed)

              ▲
              └──── answered ──── 人 (Web UI)
```

`answered` だけは書き手が人 (Web UI) であって判断 task ではない。

取り込みも suggest も spec も、 将来増える判断も、 **すべてこの 1 本のループに乗る**。
今 khi が `run-cycle.sh` + `cycle_preflight.sh` で自作しているものの一般化である。

### 責務分解

| 工程 | 持ち主 | 理由 |
|---|---|---|
| 外部 API を叩く (窓・ cursor・ credential) | **workspace** | credential 境界。 daemon に渡らない (決定 11 / 論点 h) |
| チャネル方言 → 共通語の翻訳 | **workspace** | Jira statusCategory → `source_closed`、 Slack ts → identity key。 逆輸入 3 |
| 原文の保持 | **workspace** | 決定 3。 境界を越えるのは card 粒度 |
| 起票に値するかの篩い | **workspace の LLM** | 篩いは意味の判断。 P-1 より daemon には置けない |
| 次の一手の提案・ spec 執筆 | **workspace の LLM** | 同上 |
| 判断をいつ起こすか | **daemon** | P-3 |
| 配送先の解決 (キー → task) | **daemon** | 索引引きであって判断ではない |
| イベントの記録と畳み込み | **daemon** | 決定 13。 台帳は一箇所 |
| 決定論の rule (auto-done / wake / 呼び直し抑止) | **daemon** | 決定 12 |
| state 遷移の判断 (park / drop / Go / reopen) | **人 (Web UI)** | 決定 14 |
| **起票の確定 (captured → triaged)** | **未確定** | I-4 で新規 card が `captured` で着地するようになるため毎 card 発生する工程になる。 優先度の判断ではなく「既存 card の続報ではない」という同一性の確定なので、 決定 14 の park/drop/Go とは性質が違う。 下記 未確定 |
| 人への提示 (queue / card 表示) | **daemon** | 横断は daemon にしか作れない |

一行で言えば **daemon は「同一性」と「時系列」を持ち、 workspace は「意味」を持ち、
人は「判断」を持つ**。 daemon はイベントが**何であるか**を知らないまま、 **どれと同じか**
(identity) と **いつ何が起きたか** (actions) だけを扱う。

---

## 拡張ポイント: `judge:` 予約名前空間

daemon が判断の種類ごとにコードを持つと、 種類が増えるたびに boid を変更することになり、
**LLM が自律的に動く場が育たない**。 そこで daemon が持つのは次の 3 つだけにする:

| daemon が持つもの | 中身 |
|---|---|
| **判断ジョブ** | `(target, kind, 起動理由)`。 `kind` は daemon にとって**ただの文字列** |
| **述語** | いつ起こすか。 時計と DB だけで決まる (下記の 2 形) |
| **dispatch** | その project の `task_behaviors.judge:<kind>` を task として起こす |

判断の種類を宣言するのは `project.yaml` 側。 **`judge:` で始まる behavior 名を予約語**とし、
これを「daemon が起動してよい判断の入口」の印にする。

```yaml
task_behaviors:
  judge:intake:                 # 外部ソースを取り込んで起票する判断
    trigger:
      every: 10m                # 周期型
      max_per_sweep: 1
    instruction: { ... }

  judge:suggest:                # card 1 枚に次の一手を出す判断
    trigger:
      on: task_input            # 反応型
      scope: triage             # 対象は triage task
      max_per_sweep: 5
    instruction: { ... }
```

- `judge:` の**後ろは完全に不透明**。 daemon は `intake` や `suggest` の意味を知らない
- `trigger` の中身だけが daemon の読む領域で、 `on:` は **closed set** (下記)。 `every:` は duration
- `instruction` は daemon が読まない。 中身は workspace の言語

判断の種類が増えても daemon は無変更。 workspace が `project.yaml` に behavior を 1 個足せば
新しい判断の場が増える。 `task_behaviors` は既存機構なので、 新しい概念を導入していない —
今 khi が `behavior: cycle` の 1 種類に全部を詰め込んでいるのを、 **種類ごとに分ける**だけ。

### 予約語にする理由

実装上の裏付け (Fable レビューで実測): behavior 名に**文字種の制約は無い** —
`LookupBehavior` は「verbatim 比較、 正規化なし」(`behavior_resolve.go:50`) で、 YAML の
plain scalar key はコロン+スペース以外のコロンを含められる。 したがって `judge:` の判定は
素朴な `HasPrefix` でよい。 `trigger` フィールドの追加も可能 — project.yaml は非 strict
パースで未知フィールドは黙って無視される (拒否されるのは `removedBehaviorFieldGuidance` の
明示リストのみ)。 ただし `TaskBehavior` は (i) legacy loader (ii) workspace default のマージ
(iii) workspaces テーブルへの JSON 永続化、 の 3 経路を持つので、 追加時は 3 箇所の追従が要る
(B-5 の見積もりに含める)。

`judge:` プレフィクスが無いと、 daemon は「この behavior は自分が起動してよいものか」を
区別できない。 通常の `dev` behavior を daemon が勝手に起こすのは事故なので、
**「daemon が起動してよい」を名前で明示する**。 `trigger` を持つ behavior に限る、 という
形でも同じ効果は出るが、 名前で見えるほうが `project.yaml` を読む人にも伝わる。

---

## 述語は 2 形でよい

種類ごとに述語を書くとやはり daemon が肥大するので、 汎用形に落とす。

### 反応型 (`on: task_input`)

> target に新しい事実が来ていて、 その kind がまだ反応していない

現行 khi の `speech_index()` (「最後に LLM が喋った時刻」対「最後に事実が来た時刻」) を
**kind で一般化しただけ**である。 `spoken` を「その kind の `judged` action があった時刻」に
すれば **kind ごとに独立して回り**、 自己トリガ防止 (LLM の発言が自分を再起動しない) も
一般化されたまま効く。

### 周期型 (`every: <duration>` [+ `probe:`])

> 前回の起動から N 経った [∧ probe が「ある」と答えた]

外部を見に行かないと事実が来ないもの (取り込み poll) 用。 これだけは反応型に落とせない。

**`probe` が要る理由 (Fable レビュー指摘)。** 「N 経った」だけで起こすと、 新着ゼロでも
10 分ごとに判断 task が起きる。 これは khi が 2026-08-15 の統合で**明文で潰した問題**の
巻き戻しになる:

> if nothing new, no ingest task is created this cycle (this is what lets cron run
> frequently without cluttering `boid task list` with empty-cycle tasks)
>
> — khi `scripts/poll_ingest.py`

第 3 版初稿は「poll_ingest はトリガが daemon に移れば役目が無い」としていたが誤りで、
poll_ingest の役目はトリガではなく **周期トリガを条件付きにする決定論の前置ゲート**である。
トリガが daemon に移っても役目は消えない。

そこで周期型に `probe` を許す。 daemon が workspace サンドボックスで**決定論のチェックを
走らせ、 exit code (と契約行) だけを読む**:

```yaml
  judge:intake:
    trigger:
      every: 10m
      probe: python3 scripts/poll_probe.py    # LLM は起きない。exit 0 = 用がある
```

- probe は `boid exec --readonly` 相当で走る (task を起こさない実行経路は既にある)
- 中で外部 API を叩くので、 credential と窓は workspace 側のまま (P-2 を守る)
- LLM は一度も起きないので P-1 も守る
- daemon は probe の**中身を知らない**。 読むのは exit code だけ

これは `cycle_preflight.sh` の一般化そのものである。 述語は正確には
**反応型 / 周期型 / 周期型+probe の 3 形**になる。

### `on:` と `scope:` の closed set

`on:` は Phase 1 では `task_input` の 1 つで足りる。 将来 `child_specced` など状態の形を
足す余地は残すが、 **daemon が意味を知らずに評価できるものに限る**。 `on:` に workspace の
語彙 (`jira_reopened` 等) を書けるようにはしない — 逆輸入 3 の境界を越える。

`scope:` は対象を絞る **status の集合**で、 これも closed set (`triaged` / `captured` /
`parked` / …)。 daemon は status を知っているので解釈できる。

### `inputs` に数える action の closed set

反応型述語の `inputs` を「`judged` と人の回答以外の全部」と置くと、 **ある kind の判断が
書いた action が別の kind にとっての新しい事実になり、 kind 間で相互トリガが起きる**
(Fable レビュー指摘)。 旧 `speech_index` は「LLM が喋った」を kind 非依存で数えていたので
自己トリガ防止が全 LLM 発話に効いていたが、 kind に分割するとその防御が自 kind にしか
効かなくなる。

したがって `inputs` は明示の closed set にする。 素案:

| action | `inputs` に数えるか |
|---|---|
| `attrs_set` / `child_*` で **Actor が `task:*`** のもの | **数えない** (判断が書いたものは事実ではない) |
| `attrs_set` / `child_*` で Actor が daemon / human のもの | 数える |
| `judged` / `proposed` | 数えない |
| `answered` | 数えない (人の回答は世界の事実ではない) |
| 遷移 action (park / ready / dispatch / triage_done 等) | **未確定**。 人が park を押すたびに suggest が起きるのが意図か副作用か |

`Actor` で切れるのがここでの効きどころで、 `Actor` が「どの実行単位か」しか表さない
(kind を区別できない) という弱点は、 この用途ではむしろ都合がよい — **判断 task 発の
書き込みを一括で除外できる**。

---

## 判断の記録: `judged` action

反応型述語には「LLM が見た」の記録が要る。 これを **第一級の action** にする。

```
judged { kind: "suggest", outcome: "changed" | "unchanged" | "error" }
```

- non-transitioning (`ToStatus: ""`)、 `Manual: true`。 `attrs_set` と同じ枠
- 判断 task が終わる前に必ず 1 本押す
- `spoken(kind)` = その kind の `judged` の最新時刻
- `inputs` = `judged` と人の回答以外の action の最新時刻

これで 3 つが同時に片付く:

1. **「読んだが変えなかった」が表現できる** (`outcome: unchanged`)。 現行 khi の
   `suggestion_reviewed` の一般形。 これが無いと、 変更ゼロの巡で action が 1 本も飛ばず、
   daemon から見て「まだ一度も反応していない」が永続 true になり、 毎サイクル呼び直される
2. **バックオフ**が kind に依存せず書ける (`outcome: error`)
3. **daemon が payload を詮索しなくてよい**。 第 2 版では「`suggestion` キーを含む
   `attrs_set` が喋った印」としていたが、 それは daemon が opaque blob の中身を見る形で
   境界が怪しかった上に、 変更ゼロの巡を取りこぼしていた (Fable レビュー指摘)

### 提案の記録: `proposed` action

`judged` / `answered` を第一級にしたのと同じ論法を、 提案そのものにも延ばす。

```
proposed { verb, action, reason, basis, wake }
```

第 3 版初稿は提案を `attrs.suggestion` (opaque key) に置いたままにしていたが、 それだと
**自律 Go の gate が矛盾する** — gate の条件例が「`suggestion.verb` が go」「`basis` が
新しい」なのに、 解釈するキーの closed set では `attrs.suggestion` を「有無のみ、 中身は
読まない」としていた (Fable レビュー指摘)。

`proposed` を第一級 action にすると:

- **daemon が読むのは自分の action のフィールド**であって、 opaque blob ではない。
  `verb` は既に daemon の語彙 (park / drop / wake / go)。 逆輸入 3 の境界を越えない
- daemon は副作用として `detail.suggestion` へ射影する (Web UI が表示するため)。
  `answered` が落とす
- 結果として **opaque blob から daemon が解釈するキーは `observed.source_closed` の 1 つに
  戻る** — 第 2 版で 2 個目を足そうとしていた `suggestion` が不要になる
- 自律 Go の gate (段階 2) が読むものも第一級のフィールドになる

### 却下の記録: `answered` action

判断ではなく**人の回答**の記録。 決定 14 で判断を Web UI へ移したのに、 Web UI 側に回答を
記録する経路が無く、 **却下しても再提案の抑止が効かない**穴が現に開いている (下記「既に
開いている穴」)。

```
answered { answer: "accept" | "reject", verb: "...", basis: "..." }
```

副作用として `detail.attrs.suggestion` を落とす。 `inputs` には数えない (人の回答は世界の
事実ではないので、 却下のたびに同じ提案を作らせ直すループになる)。

**誰が押せるか。** `judged` は `Manual: true` なので、 任意の sandbox task と人が
`boid action send` で押せる。 バグった task が `judged{kind: suggest}` を押すとその kind が
一巡沈黙する。 実害は小さく、 **`Actor` で監査できる以上のことは守らない** (詐称防止の
仕組みは入れない) 方針とする。

**却下履歴の突合 (`verb` / `basis` の一致) は daemon に置かない。** それは suggestion の
中身を読むことになり境界を越える。 起こされた判断 task が `action_list` で自分の履歴を
読んで自己抑制する。

---

## 自律 Go への道筋

この取り組みの目的は生産性であり、 最終的に狙うのは **LLM が自律的に動ける場**である。
判断スケジューラはそのための器で、 到達点は「Go できそうなものは LLM が自分で Go する」。

| 段階 | 誰が Go を出すか | 状況 |
|---|---|---|
| 現在 | 人 (Web UI) | 決定 14 |
| 段階 1 | LLM が **提案**し、 人が承認 | `suggestion.verb: go`。 ほぼ実装済み |
| 段階 2 | daemon の**決定論 gate** が、 条件を満たす提案を自動で通す。 人は事後に見る | 本 doc の射程外だが、 器はここで作る |

段階 2 の gate は DB と設定だけで決まる形にする — 例えば「子が `specced` ∧ 対象 project が
auto-go を opt-in ∧ spec が readonly 相当 ∧ `proposed.basis` が既存の `answered` と一致
しない」。 読むのは **`proposed` action のフィールド** (J-7) であって opaque blob ではない
ので、 逆輸入 3 の境界を保ったまま書ける。 **判断そのものは
LLM が既に済ませており、 daemon は「その判断を人に見せずに通してよいか」だけを決定論で
決める**ので、 決定 12 に反しない。

これは **決定 15 (auto-done) の対称**である。 「全子 closed ∧ `source_closed`」で自動的に
終端させる rule が既にあるなら、 「spec がある ∧ 許可がある」で自動的に開始させる rule は
新しい思想ではない。

監査は既存機構で成立する: `Action.Actor` に「`task:X` が提案した」「daemon が自動 Go した」が
両方残るので、 事後に「なぜこれが勝手に走ったか」を辿れる。 人手 Go と同じ台帳に乗るのが
重要で、 自動化のたびに別系統の記録を作らない。

opt-in は **project 単位**。 最初から全部を自動にしない。

---

## As Is: fold が二本ある

以上が To-Be で、 ここからが「今どうなっていて、 何が足りないか」。

決定 14 (state の正は daemon ただ一つ) は workspace 側の `decisions/*.jsonl` を退役させた。
決め手は khi 側 doc にこう記録されている:

> **2 つの fold を並べると、 ロジックがズレたときに誰も気づかない** (決定 14 の決め手)

ところが退役したのは `decisions` だけで、 **`claims` + `fold_claims()` は残った**:

```
claims (workspace の JSONL) ──fold_claims──▶ card (workspace の畳んだ状態)
                                                   │
                                             daemon_sync (差分 reconcile)
                                                   ▼
actions (daemon のログ) ──FoldDetailAttrs 等──▶ task_triage.detail
```

daemon 側は既に同じ構造を持っている — `FoldDetailAttrs()` / `AddDetailChild()` /
`SpecDetailChild()` が fold の実体で、 `machine.go` の `attrs_set` / `child_added` /
`child_specced` が届くたびに畳み、 「決定 13: event 追記を正、 state は導出」を
`ParkedFrom` で実践している。 **fold は今も二本あり**、 572 行の `daemon_sync.py` は
その二本の間の reconcile をしている。

### workspace が自前の fold を持たざるを得なかった理由は 2 つ

どちらも workspace の都合ではなく **daemon 側の口の不足**である。

**理由 1: イベントの配送先を daemon が解決できない。** action は `task_id` 必須なので、
対応する triage task がまだ無いキーで飛んでくる観測を受け取れない。 だから workspace 側が
「キー → card」の索引を持ち、 着地させてから push する形になっている。

重要なのは **この配送先解決が判断ではない**こと:

> **照合は判断ではなく索引引き**。 2026-08-07 に同じ PR の card が 2 枚に分裂した事故は、
> これを LLM にやらせていたために起きた。
>
> — khi `scripts/match_card.py`

索引キーは `source.issue_key` / `source.thread_ts` / `related_jira_issues[]` の 3 つで、
**そのキーは既に daemon の中にある** (khi が `attrs_set` の opaque key として毎サイクル
押している)。 加えて `ref` 列は決定 16 以降この用途になっている
(`internal/api/task_create.go:251` の「jira issue_key / slack thread_ts / mail message-id」。
元は per-child dedup 用で、 root task へ開いたのは Phase 1 PR-4 / 論点 7)。

足りないのは **1 task が複数キーを持てないこと**と、 **キーから task を引く口が無いこと**
だけである。

**理由 2: actions の履歴を読む口が無い。** `internal/sandbox/protocol.go` の `BoidOp` 一覧に
`action_list` 相当が無く、 `action_send` で書けるのに読めない。 `task_triage_get` /
`task_triage_list` が返すのは畳んだ後の `detail` だけで、 導出フィールドとして露出して
いるのは `ParkedFrom` のみ。 だから workspace は呼び直し抑止のために自前の履歴 (claims) を
持つしかなかった。

---

## 取り込み identity

理由 1 を塞ぐ形。 外部データソースのキーを、 **ユーザに対する identity** と同じものとして
扱う (nose 案)。 ユーザがメールアドレスや GitHub アカウントを複数持ち、 どれからでも同じ
本人に解決できるのと同じ構造にする。

| 認証の世界 | ここでの対応物 |
|---|---|
| principal | triage task |
| identity | 外部キー `jira:ROOKPF-289` / `slack:1785803139.540489` |
| identity → principal の解決 | イベントの配送先解決 (索引引き) |
| 登録に使った identity | `ref` = 決定 16 の canonical source |
| あとから link した identity | `related_jira_issues` 由来のキー |
| 未登録 identity での到達 | `captured` な triage task として着地 (下記) |
| アカウント統合 | 分裂した card の identity 付け替え (管理操作) |

不変条件は 2 つ:

- 1 つの task は identity を **複数**持てる
- 1 つの identity は **高々 1 つの task** にしか紐付かない

identity は `<namespace>:<key>` の**不透明文字列**とし、 daemon は中身を一切解釈しない。
`jira` が何かも `ROOKPF-289` の意味も知らず、 ただの文字列 → task_id の索引として扱う。
名前空間を決めるのは workspace 側。 `FoldDetailAttrs` が明言している姿勢
(“a PURE fold with zero policy”) と逆輸入 3 の境界を、 多対一の索引を足しても崩さない
ための形である。

副次的に、 子 task の `Ref` (`Dispatch` が `children[i].ID` を入れている) と外部キーを
名前空間で分離できる。 現状は同じ `ref` 空間に内部 ID と外部キーが同居している (実害は
無い — スコープが `(ref, parent_id, project_id)` で分かれているため。 整理として)。

### 未着キーは `captured` に着地する。 専用の inbox は作らない

第 2 版は「未着イベントは daemon 側の**未着インボックス**に積み、 そこで人が起票判断を
する」としていた。 P-2 (取り込みは workspace 側) を立てるとこれは要らなくなる:

**篩いは意味の判断なので workspace 側の LLM に残る。** つまり pump が daemon へ押すのは
「workspace が起票に値すると判断したもの」だけで、 ゴミメールは **daemon に到達しない**。
流量は今の card 起票と同じ (= 既に回っている量) で、 メールを足しても増えるのは workspace
側の篩いの負荷であって daemon 側ではない。

すると未着キーの意味は「**起票に値すると判断されたが、 既存のどの identity にも当たら
なかったもの**」= **新規 card** になる。 これは本編が既に持っている状態でちょうど表せる:

> **不変条件の境界は captured → triaged** (起票の確定)。 captured (head-capture 直後、
> まだ形になっていない) は対象外
>
> captured: capture 直後、 triage 前。 主に head-capture 経由 (mail / Jira / Slack 経路は
> **workspace 内 triage を通るため** triaged で到着する、 決定 10)。 **Phase 0 では未使用**

決定 10 の「workspace 内 triage を通るため triaged で到着する」の workspace 内 triage が、
まさに今 LLM がやっている篩いである。 篩いを LLM に残したまま、 **起票の確定 (どの card に
なるか) だけを daemon 側の判断に回す**なら、 これらの経路は `captured` で到着するのが本編の
設計通りということになる。 `triage` (captured → triaged) と `drop` (captured → dropped) の
遷移も machine に実装済み。

したがって:

- **新規テーブルは identity 索引の 1 本だけ**。 inbox 用のテーブルも Web UI も新規には要らない
- 「未着」の可視化 = `captured` な triage task の一覧
- 決定 3 とも衝突しない。 daemon が持つのは workspace が開示すると決めた title / summary で、
  今の card 起票と同じ (第 2 版のインボックス案は、 起票判断の材料として生本文を daemon 側へ
  置く必要があり、 決定 3 と正面衝突していた)

残る判断は「index が外れたが、 実は既存 card と同じ件ではないか」(例: Jira key を持たない
Slack スレッドが、 ある issue の話である) の統合で、 これは意味なので **`judge:` の判断**
として扱う。

`captured` の滞留対策だけは新規に要る: 終端状態ではないので既存の 30 日 GC に乗らず、
放っておくと溜まり続ける (下記 未確定)。

---

## 決めたこと

| # | 決定 | 根拠 |
|---|---|---|
| I-1 | 外部キーを identity として task に多対一でぶら下げる。 `ref` はその特例 | 多対一が実需 — Slack 起票の card に後から Jira のコメントが来る経路がある |
| I-2 | identity は `<namespace>:<key>` の不透明文字列。 daemon は解釈しない | 逆輸入 3 の境界 |
| I-3 | identity のスコープは **project** | 取り込まれるメタタスクはメタプロジェクトにしか紐づかない。 他プロジェクトへの展開は子 task が担い、 子は identity を持たない。 `(ref, parent_id, project_id)` unique (migration 0037) と同じスコープを踏襲する (B-1 の (a) 案を採ると index 自体は消える) |
| I-4 | 未着キーは `captured` な triage task として着地させる。 **専用 inbox は作らない** | 篩いが workspace 側に残るので、 daemon に届く時点で既に「起票に値する」と判断済み (上記) |
| I-5 | **done では identity を握り続ける**。 `observed.source_closed` が true → false に戻ったら `reopen_triaged` | 同じソースが動いたら普通は reopen したい (nose)。 決定 15 の鏡像で、 読むキーも同じ 1 個だけ |
| I-5b | I-5 の前提として、 **done の triage task にも観測が着地できるようにする** | `attrs_set` の `FromStatus` は captured/triaged/parked/ready/working の明示列挙で **done を含まない** (`machine.go:441`、 論点 6-3 が「never `*`」と決めた)。 現状は観測を done の task へ畳む合法経路が無く、 I-5 の判定材料そのものが更新されない |
| I-5c | done の task に着地したイベントは、 reopen 条件を満たさなくても**必ず可視化する** | reopen の引き金は `source_closed` の反転だけなので、 **canonical source は closed のままで Slack スレッドにだけ続報が来た**ようなケースは、 identity を握っているので配送はされるが reopen せず**黙って沈む**。 (決定 16 の契約下では canonical source を持たない task は本来 done に到達しないので、 slack-only の done は契約違反ケースの防御にあたる) |
| I-6 | **drop では identity を自動解放する** | drop は「この identity との関係を切る」判断そのもの。 握ったままだと、 捨てた件が動いても着地せず・ 自動 reopen の経路も無く (`machine.go:371` の `reopen_triaged` は done/aborted からのみ。 手動 `reopen` dropped→triaged は `machine.go:355` にあるが誤破棄からの回復経路であって観測に反応しない)・ 同じキーで新規起票もできない、 という静かに詰む形になる |
| J-1 | `judge:` を **予約された behavior 名前空間**とし、 daemon が起動してよい判断の入口を名前で明示する | daemon が通常の `dev` behavior を勝手に起こすのは事故。 `project.yaml` を読む人にも伝わる |
| J-2 | 述語は **反応型 (`on:`) と周期型 (`every:`) の 2 形**。 `on:` は closed set | kind に依存しない形に落とさないと daemon が肥大し、 判断の場が育たない |
| J-3 | `judged { kind, outcome }` を第一級の action にする | 「読んだが変えなかった」の表現・ バックオフ・ payload 詮索の回避が同時に片付く |
| J-4 | `answered { answer, verb, basis }` を action にする。 副作用で `suggestion` を落とす | 却下は attrs の更新ではなく出来事。 現に開いている穴を塞ぐ |
| J-5 | 却下履歴の突合は daemon に置かず、 判断 task が `action_list` で自己抑制する | `verb` / `basis` の一致判定は suggestion の中身を読むことになり境界を越える |
| J-6 | 自律 Go は **決定論 gate + project 単位 opt-in**。 判断は LLM、 通してよいかは daemon | 決定 15 (auto-done) の対称。 監査は `Action.Actor` で既存台帳に乗る |
| J-7 | 提案を `proposed { verb, action, reason, basis, wake }` の第一級 action にし、 daemon が `detail.suggestion` へ射影する | opaque blob から daemon が読むキーを `observed.source_closed` の 1 つに保ったまま、 自律 Go の gate が提案を読めるようにするため |

### I-5 は workspace 側の現行ルールの意図的な反転

khi の `match_card.py` は今、 終端 (done / dropped) の card を索引から除外している —
「片付いた件に後から追記すると応答済みのものが動いてしまう」ため。 I-5 はこれを引っくり返し、
「動いてしまう」を望ましい挙動と見る。 **退行ではなく意図的な方針変更**なので理由ごと記録する。

identity モデルにすると、 この規則を「索引からフィルタする」形で書く必要が無くなる。
done は握り続ける (I-5)、 drop は解放する (I-6) という **binding のライフサイクル**として
表現され、 `ref` の unique 制約と衝突しない。

### 副産物: 暗黙則が不変条件になる

`match_card.py: build_key_index()` の既知の制約 —「複数の active card が同じ issue を参照する
場合、 観測は `updated_at` が最新の 1 枚にしか届かない」— は、 配送先が黙って変わる形で
事故のタネになっている (2026-08-09 に ROOKPF-289 で起きかけた)。 identity モデルでは
「1 identity は 1 principal」が明示された不変条件になり、 2 枚目の link 要求は
**silent newest-wins ではなく拒否**になる。

ただし引用元が例に挙げている **epic の子 card 群**は、 バグではなく正当な運用である
(workspace 側は `related_jira_issues` を「無くても害はない助言キー」と定義し、 複数 card の
重複参照を前提にしている)。 一律拒否にすると newest-wins が first-wins に変わるだけで
改善にならない。 **排他的な帰属 (identity) と非排他の言及 (reference) は性質が違う** —
昇格させてよいかは未確定。

---

## 既に開いている穴 (J-4 の動機)

khi の `fold_claims()` は「同じ根拠で却下済みの提案は出し直さない (`basis` が変わるまで)」を
実装しており、 入力は `suggestion_answered` claim である。 その書き手は note-inbox スキルだけで:

> 採用/却下は **Web UI** で出す (決定14)。 ここで答えた場合は
> `scripts/claim.py suggestion_answered` で却下履歴として残す

つまり **note-inbox で答えたときしか却下履歴が残らない**。 決定 14 で判断を Web UI へ移した
のに、 Web UI 側に生む経路が無い。 daemon 側を検索しても `suggestion` は完全に opaque key
扱いで、 daemon は存在すら知らない。 結果として **Web UI で却下しても再提案の抑止が効かない**
はずである。

### 意識しておくこと: 解釈するキーの closed set

`internal/orchestrator/triage_done.go` は、 daemon が opaque blob から読むキーは
`attrs.observed.source_closed` **ただ 1 つ**だと明言している (逆輸入 3 の境界)。
本 doc の後、 daemon が解釈するものは次に限る:

| 対象 | 読む深さ |
|---|---|
| `attrs.observed.source_closed` | 値 (bool)。 決定 15 / 16。 **opaque blob から読むのはこれ 1 つだけ** |
| `judged.kind` / `judged.outcome` | 第一級 action のフィールド。 ただし `kind` の**意味**は知らない |
| `proposed.verb` / `basis` 等 | 第一級 action のフィールド。 `verb` は既に daemon の語彙 |
| `answered.answer` | 第一級 action のフィールド |
| identity 文字列 | 完全に不透明。 名前空間も解釈しない |

第 2 版は「`suggestion` キーを含む `attrs_set` を『喋った』印にする」としており、 blob を
詮索する形だった。 `judged` (J-3) と `proposed` (J-7) を第一級にしたことで、 **opaque blob
から daemon が読むキーは決定 16 当時と同じ 1 つに戻った**。

---

## boid 側に要るもの

| # | もの | 見立て |
|---|---|---|
| B-1 | identity 索引テーブル + 解決 + link / unlink op。 **`tasks.ref` 列と `idx_tasks_ref_parent_project` の去就を同時に決める** (下記) | 新規。 migration 1 本 + store + `BoidOp` |
| B-2 | 未着キーからの `captured` task 自動生成 (I-4)。 `captured` の可視化と滞留対策。 **push op の返り値契約** (解決された task_id、 新規 `captured` か既存かの区別、 identity 衝突時のエラー語彙) | 小〜中。 状態と遷移は実装済み。 返り値契約は workspace 側が原文の追記先を決めるのに必須 (下記) |
| B-3 | `action_list` 相当の `BoidOp` (workspace スコープ) | 小。 `ListActionsByTask` は既にある (呼び出し `internal/api/task_service.go:413`、 実装 `internal/orchestrator/store.go:431`) |
| B-4 | `judged` (J-3) / `proposed` (J-7) / `answered` (J-4) の action 追加、 `proposed` → `detail.suggestion` の射影、 Web UI の回答からの発行 | 小〜中 |
| B-5 | **判断スケジューラ** — `judge:` behavior の発見、 述語 2 形の評価、 dispatch、 直列化、 流量制御 | 中〜大。 本 doc の中核 |
| B-6 | `source_closed` 反転による auto-reopen (I-5) と、 done への着地経路 (I-5b) | 小。 `reopen` → `reopen_triaged` のルーティングは実装済み (`internal/api/triage_done.go:299`) |

### B-2: push op は解決先を返さないといけない

workspace は**原文を現地に残す** (決定 3) ので、 続報が来たとき「どの note に追記するか」を
知る必要がある。 ところが第 3 版初稿は「workspace はどの card かを一切決めない」と書いており、
**追記先を知る手段が無くなっていた** (Fable レビュー指摘)。

したがって push op は「押した」だけでなく**解決結果を返す**契約にする:

- 解決された `task_id`
- 新規に `captured` を起こしたのか、 既存 task に着地したのか
- identity 衝突 (I-1 の「2 枚目は拒否」) が起きた場合のエラー語彙 — workspace はこれを
  受けて `judge:` の統合判断へ回す

さらに workspace 側は `task_id` から自分の note (slug) を引けないといけない。 slug ↔ identity
の対応をどちらが持つかは未確定 (workspace の frontmatter に持つのが素直だが、 identity 読み出し
op を使う案もある)。

### B-1: `tasks.ref` の去就は I-6 の前提

I-6 (drop で解放 → 同じキーで新規起票できる) は現行の `ref` と正面衝突する:

- dropped の task も `tasks.ref` を保持したままである
- `idx_tasks_ref_parent_project` (migration 0037) の unique 制約が生きている
- `FindTaskByRef` の SQL に **status 絞りが無い**
  (`WHERE t.ref = ? AND t.parent_id = ? AND t.project_id = ?`)

したがって同じ ref で `task create` を打つと get-or-create が **dropped の既存 task を返し**、
新規起票にならない。 成立させるには少なくともどちらかが要る:

- (a) uniqueness を identity テーブルへ完全移譲し、 `tasks.ref` 列を退役させる
- (b) drop 時に `tasks.ref` を空にする — ただし決定 16 の「`ref` は dedup キーで後から
  変えられない」と矛盾するので、 決定 16 側の再定義が要る

**二重表現のドリフト**をどう防ぐかを含め、 B-1 の migration 設計で最初に決める。

### B-5: 部品はほぼ揃っている

| 要るもの | 現状 |
|---|---|
| 周期的に述語を評価するループ | **ある**。 `QueueSweepLoop` (`queue_sweep.go`) が interval ticker → 純粋述語 → `ActorDaemon` で action、 という形で動いている |
| daemon が workspace の task を起こす | **ある**。 `Dispatch` が specced な子から実 task を作っている。 behavior 指定も project 指定もできる |
| 判断結果を受け取る | **ある**。 `action_send` |
| 誰が判断したかの記録 | **ある**。 `Action.Actor` |
| 判断の種類を宣言する場所 | **ある**。 `task_behaviors` |
| LLM が自分の履歴を読む | **無い** (B-3) |
| 「見たが変えなかった」の記録 | **無い** (B-4 / J-3) |
| 判断ジョブの台帳・ 直列化・ 流量制御 | **無い** (B-5 本体) |

論点 b (定期起動の機構: host cron vs daemon 内 scheduler) は、 **判断の起動については内蔵側に
倒れる**。 外部 API を叩く窓 (poll の cursor と頻度) は論点 h の通り workspace 側に残るが、
その poll を起こすのも `judge:intake` の周期型トリガになるので、 **workspace 側に cron は
残らない**。

### 破綻しそうなところ

いずれも致命的ではなく、 **今 khi が自作している対策の一般化**になる。

| 懸念 | 対策 | 現行 khi の実装 |
|---|---|---|
| 同一 target への重複起動 | `kind × target` で 1 本の直列化 | flock + 実行中 `[cycle]` task の確認 |
| 自己トリガ (無限呼び直し) | 反応型述語の `spoken` に `judged` を数える | `speech_index()` の `SPOKEN` |
| 流量とコスト | `max_per_sweep` と繰り越し | 「note-suggest の fork は 1 巡 5 枚まで」 |
| 失敗のバックオフ | `judged{outcome: error}` | `suggestion_reviewed` |
| **判断 1 回のコスト** | dispatch の粒度を決める (下記) | cycle 1 task の中で subagent fork (1 巡 5 枚) |
| **`judged` を押さずに死ぬ** | dispatch 側でタイムアウト / 起動回数で諦める | — (現行は task 単位で watchdog) |

**dispatch の粒度は決めておく必要がある** (Fable レビュー指摘)。 `max_per_sweep: 5` が
「5 個の独立 task を dispatch する」なのか「1 dispatch に 5 target を渡す」なのかで、
コストとレイテンシが変わる。 現行 khi は cycle 1 task の中で subagent を fork しており、
コンテナ起動・ clone・ セッション起動が 1 回で済んでいる。 per-target dispatch にすると
card 1 枚ごとにそれが走る。 併せて、 判断 task が通常 task と同じ一覧に混ざると
`boid task list` が判断で埋まる問題も出る (周期型 10 分なら 30 日で ~4,300 行)。

第 2 版は「cadence は自動で片付く、 独立した論点ではなかった」としていたが、 これも不正確
だった。 **片付くのではなく昇格する** — khi の `run-cycle.sh` + `cycle_preflight.sh` は
判断スケジューラのプロトタイプであり、 上の 4 つは全てそこで実証済みである。

---

## 既存の決定との関係

| 既存 | 本 doc の扱い |
|---|---|
| **決定 3** (境界を越えるのは card 粒度、 本文は現地) | **守る**。 第 2 版のインボックス案は衝突していたが、 I-4 の変更で解消 |
| **決定 10** (mail/Jira/Slack は workspace 内 triage を通るので triaged で到着) | **一部を `captured` へ倒す**。 篩いは workspace に残るが、 起票の確定は daemon 側の判断に回る |
| **決定 12** (queue 評価は決定論のみ) | **徹底する側**。 identity 解決も述語も gate も判断ゼロ |
| **決定 13** (event 追記を正、 state は導出) | **徹底する側**。 workspace 側の二本目のログを畳む |
| **決定 14** (state の正は daemon) | **完成させる側**。 `claims` の退役でようやく fold が一本になる |
| **決定 15** (auto-done) | **鏡像を足す**。 I-5 の auto-reopen、 J-6 の auto-Go |
| **決定 16** (`ref` = canonical source のキー) | **一般化する**。 `ref` は「登録に使った 1 本目の identity」。 B-1 で列の去就を決める際に再定義が要る可能性 |
| **決定 17** (`reopen_triaged`) | **引き金を与える**。 第 12 版は「足りないのは押せる action だけ」としたが、 誰が何を見て押すかが空いていた |
| **論点 b** (定期起動の機構) | **判断の起動は内蔵側へ**。 外部 poll の窓は workspace |
| **論点 g** (洪水対策 = 同一 source ref の再 push を update 扱い) | **置き換わる**。 多対一の identity 索引 + binding のライフサイクルへ一般化。 洪水自体は workspace 側の篩いで止まる |
| **論点 h** (source 既読化・ cursor は workspace 側) | **変わらない** |
| **逆輸入 3** (チャネル知識は workspace 側) | **守る**。 解釈するキーの closed set を上に明示した |

---

## 段階

1. **B-1 + B-2** — identity 索引と `captured` 着地。 土台
2. **B-3 + B-4** — actions の読み口、 `judged` / `answered`。 ここで workspace 側の
   claims / fold / match / daemon_sync が消える
3. **B-5 + B-6** — 判断スケジューラと auto-reopen。 ここで workspace 側の cron と
   呼び直し抑止が消える
4. (射程外) 自律 Go の gate

第 2 版の「cadence を先にコアへ持ち上げる」案でこれを始めていたら、 **消える予定のものを
boid コアへ移植する**ことになっていた。 順序は identity が先。

---

## 非目的

- **daemon にエージェントを置く**こと。 P-1。 gate も述語も決定論のまま
- **daemon が原文を保持する**こと。 決定 3 を維持する
- **identity を認証・ 認可に使う**こと。 ここでの identity は配送先を決める索引であって、
  principal の権限とは無関係。 名前が示唆する以上のことをさせない
- **identity のチャネル知識を daemon に持たせる**こと。 名前空間の意味・ 正規化・ 妥当性検査は
  すべて workspace 側 (I-2)
- **判断の中身を daemon が知る**こと。 `kind` は不透明のまま (J-1 / J-2)

---

## 未確定

### 設計を左右するもの

- **`captured` → `triaged` を誰がいつ押すか**。 I-4 で新規 card が `captured` で着地する
  ようになるため、 毎 card 発生する工程になる。 候補は 3 つ: (i) **人が Web UI で押す** —
  今より 1 工程増える摩擦 (ii) **`judge:link` が「統合先無し」と判断した後に押す** —
  LLM が state 遷移を押すことになるが、 これは優先度の判断ではなく**同一性の確定**なので
  決定 14 の park/drop/Go とは性質が違う、 と整理できるかどうか (iii) **push した
  `judge:intake` 自身が即押す** — `judge:link` が走る窓が無くなる。
  現行実装の自然な順序は (iii) (`queue_notify.go:62` が「khi creates a card as captured,
  sends `triage`」と書いている)。 **B-2 の実装と `judge:link` の成立性が両方これに従属する**
- **`tasks.ref` 列を退役させるか、 drop 時に空にするか** (B-1)。 決定 16 の再定義を伴う
- **判断ジョブの dispatch 粒度** (B-5)。 per-target か batched か。 コストとレイテンシに直結
- **判断 task を通常 task と同じ一覧に出すか**。 出すと `boid task list` と Web UI が
  判断で埋まる
- **`inputs` に数える action の closed set の確定**。 特に遷移 action (park / ready 等) を
  数えるか。 数えると人が park を押すたびに判断が起きる
- **kind 間の連鎖の収束条件**。 A の判断が書いたものが B の入力になる構成で振動しないこと。
  `Actor` による一括除外で足りるかを検証する
- **判断ジョブの台帳をどう持つか**。 実体のあるテーブルにするか、 述語から毎回導出するか
  (導出なら「起こした」の記録は `judged` と dispatch した task 自体で足りる可能性がある)
- **`judge:` behavior の発見経路**。 `project.yaml` の reload / fetch と、 daemon が
  知っている trigger 定義の同期。 project.yaml の変更が `boid project fetch` を要する現状と
  どう噛み合うか
- **`captured` の滞留対策**。 終端でないので 30 日 GC に乗らない。 TTL か自動 drop か、
  可視化して人に落とさせるか
- **identity と reference の分離**。 epic の子 card 群のような正当な重複参照を、 排他的な
  identity で表現するか別概念にするか

### 挙動として決めておくもの

- **auto-reopen のフラップ対策** (I-5)。 ソースが閉じ / 開きを繰り返すと task が ping-pong
  する。 「自動 reopen は 1 回だけ、 2 回目以降は人に見せる」等
- **I-5b の置き場所**。 done への着地を machine の rule に足すか、 `task_triage` 行の有無を
  見られる service 層のガードにするか。 論点 6-3 (通常 task の done に発火させない) を
  壊さないこと
- **terminal task GC と identity binding の寿命**。 done / dropped の task は 30 日で GC
  される。 I-5 の「done では握り続ける」は実質 30 日の期限付きで、 GC 後は同じキーが事実上
  解放される。 それでよいのか、 identity テーブルを cascade させるのか
- **イベント push の冪等性**。 現行の冪等性は workspace 側の reconcile (差分 push、
  self-healing) が担保している。 per-event push にすると actions は append-only なので、
  pump のクラッシュ・ 再送で同じイベントが二重に積まれる。 イベント単位の冪等キーが要る
- **`description` の組み立ての引き取り手**。 workspace 側の `desired_description()` は
  summary / canonical URL / suggestion / 子一覧を平文に畳んで task の `description` に
  書いている (「summary が attrs の中にしか無いと card を開いても何の件か分からず Go の
  判断ができない」— 2026-08-14 の実害)。 Web UI が `detail.attrs` から直接描画するか、
  workspace が押し続けるか
- **判断 task の失敗の扱い**。 `judged` を押さずに死んだ場合、 述語は永遠に真のままになる。
  dispatch 側でタイムアウトを見るか、 起動回数で諦めるか
- **無変更の観測をどちら側が抑止するか**。 workspace が毎巡同じ観測を押すと `inputs` が進み、
  反応型述語が毎サイクル真になって `judged` の効果が消える。 現行は workspace 側の reconcile
  (差分だけ push) が担っており、 その比較先が自前の fold から daemon の読み戻しに変わる。
  **「押す前に現況と比べる」は残る** — 消えるのはローカルの fold のほうだけ、 という整理を
  workspace 側 memo と揃える
- **`probe` の実行コストと失敗時の方針**。 周期型 + probe は 10 分ごとに sandbox job を
  起こす (現行 `cycle_preflight.sh` と同じコスト)。 probe が落ちたときフェイルオープン
  (起こす) かフェイルクローズ (見送る) かは、 現行 khi が step ごとに使い分けている

### 移行

- **identity の一括投入**。 既存 triage task の `source` / `related_jira_issues` / `ref` から
- **履歴の投入**。 過去の却下履歴を写さないと、 **J-4 が塞ごうとしている穴を移行自身が一度
  開ける** (移行直後に再提案の嵐になる)。 子の到達点も同様
- **本編への取り込み方**。 独立 subsystem のまま進めるか Phase 2 の項目として畳むか。
  I-4 / I-5 / I-6 / J-6 は事実上 決定 10 / 16 / 17 / 論点 g を書き換えるので、 畳む時点で
  **決定番号を振り直さないと**「決定は本編・ 詳細は別 doc」の構造が崩れる
