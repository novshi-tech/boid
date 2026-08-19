# トリガと取り込み identity (設計メモ)

2026-08-18 起案、 2026-08-19 更新 (第 4 版)。 **まだ実装しない。** `cross-project-issue-triage.md` (以下「本編」) の Phase 1
完了後に見つかった構造的な積み残しを、 独立した subsystem として切り出した検討。

workspace 側の対になる memo は khi-task-collector リポジトリの
`docs/plans/ingestion-identity.md`。 **本 doc が先で、 khi 側はこれを参照する側**である —
先に daemon の持ち分を決め、 workspace 側は「その持ち分をこう使う」として書く。

### 版の経緯

設計に唯一の正解は無く、 各版はその時点の前提での選択である。 後の版が前の版を否定している
のではなく、 **前提が増えたので選び直している**。 何を選び直したかと、 その理由を残す。

| 版 | 何を書いていたか | 次で選び直した点と理由 |
|---|---|---|
| 第 1 版 | claims / fold / cadence のうち汎用な部分を boid コアへ引き上げる | boid コアに既に fold があると分かった (事実確認)。 引き上げると三本目になるので、 向きを逆にした |
| 第 2 版 | 取り込み identity と、 未着イベント用の専用インボックス | 部品の話から入って全体像が見えないと指摘を受け、 原則と To-Be を先に立てる構成へ。 インボックスは、 篩いが workspace に残るなら要らないと分かり `captured` 着地に |
| 第 3 版 | daemon を判断のスケジューラとし、 `judge:` 予約 behavior 名前空間 + 述語 2 形 | `trigger` は task_behavior の性質ではないという指摘を受け、 トップレベルの `triggers` へ。 予約名前空間・ 述語・ probe・ 流量制御が不要になった |
| 第 4 版 (現在) | daemon は時計と single-flight だけを持ち、 走らせるのは workspace のスクリプト | — |

---

## 原則

| # | 原則 | 帰結 |
|---|---|---|
| P-1 | **daemon は決定論的にふるまう。 LLM の判断は workspace 側に置く** | 決定 12 の再確認。 daemon にエージェントを置かない |
| P-2 | **外部からのシグナル取り込みは workspace 側** | credential 境界 (決定 11 / 論点 h) と、 原文を越境させない設計 (決定 3) の帰結 |
| P-3 | **判断のトリガ (時計) は daemon が出す** | 下記。 第 3 版初稿より射程を縮めてある |

P-3 が言うのは「**何かが始まる起点を daemon が持つ**」ところまでで、 「LLM に用があるか」の
判定まで daemon が持つという意味ではない (第 3 版初稿はそこまで含めていた。 経緯と縮小の理由は
「トリガは周期型 1 形でよい」節)。

daemon が持つ必然性があるのは 3 つ — **スケジュール**、 **single-flight**、 **実行結果の記録**。
いずれもドメインに依存しない。 判定・ 流量・ 対象の選択は workspace のスクリプトが持ち、
それは決定論なので P-1 とは衝突しない。

**例外**: 人が明示的に始める対話セッション (khi の spec 整形など) はトリガから起こさない。
P-3 は「daemon が起こすもの」の話であって、 「人が始める判断」を禁じるものではない。

この 3 つから、 daemon の役割が決まる: **daemon は判断のスケジューラになり、 workspace は
判断の実装になる。**

---

## To-Be

```
  daemon                                    workspace
  ──────────────────────────────          ─────────────────────────────

  時計 (every: 10m)
        │
        ├──▶ トリガのコマンドを実行 ─────▶  scripts/*.py  (決定論)
        │      single-flight で直列化              │
        │                                          │ 用があるときだけ
        │                                          ▼
        │                                    boid task create
        │                                          │
        │                                          ▼
        │                                    LLM が判断する
        │                                          │
        └───── actions を畳む ◀────────────────────┘
                 (attrs_set / child_* / noted)

              ▲
              └──── answered / park / drop / Go ──── 人 (Web UI)
```

daemon が知っているのは「10 分ごとにこのコマンドを走らせる」ことだけで、 その中で何が
起きるかは関知しない。 スクリプトは daemon の状態を CLI で読み戻し、 **LLM に用があると
判断したときだけ** task を起こす。 用が無ければ task 行は 1 本も増えない。

取り込みも suggest も、 将来増える判断も、 **すべてこの 1 本のループに乗る**。 今 khi が
`run-cycle.sh` + `cycle_preflight.sh` で自作しているもののうち、 **cron と多重起動防止だけを
daemon が引き取る**形である。

### 責務分解

| 工程 | 持ち主 | 理由 |
|---|---|---|
| 外部 API を叩く (窓・ cursor・ credential) | **workspace** | credential 境界。 daemon に渡らない (決定 11 / 論点 h) |
| チャネル方言 → 共通語の翻訳 | **workspace** | Jira statusCategory → `source_closed`、 Slack ts → identity key。 逆輸入 3 |
| 原文の保持 | **workspace** | 決定 3。 境界を越えるのは card 粒度 |
| 起票に値するかの篩い | **workspace の LLM** | 篩いは意味の判断。 P-1 より daemon には置けない |
| 次の一手の提案・ spec 執筆 | **workspace の LLM** | 同上 |
| 何かを始める時計と直列化 | **daemon** | P-3。 スケジュール / single-flight / 実行結果の記録 |
| 「LLM に用があるか」の判定 | **workspace のスクリプト** | 決定論なので P-1 と衝突しない。 対象の選択・ 流量制御も同じ側 |
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

## 拡張ポイント: トップレベルの `triggers`

daemon が判断の種類ごとにコードを持つと、 種類が増えるたびに boid を変更することになり、
**LLM が自律的に動く場が育たない**。 そこで daemon が持つのは「**いつ、 何を走らせるか**」
だけにし、 それ以外は全部 workspace のスクリプトに委ねる。

`project.yaml` の**トップレベル**に `triggers` を置く:

```yaml
triggers:
  - name: intake
    every: 10m
    run: python3 scripts/intake_tick.py

  - name: sweep
    every: 10m
    run: bash scripts/sweep_tick.sh
```

daemon が読むのはこれだけ。 走らせるのは workspace のコマンドで、 **その中で何をするかは
daemon の関知するところではない** — 外部を見に行くのも、 daemon の状態を読み戻すのも、
「LLM に用がある」と判断して `boid task create` を打つのも、 全部スクリプトの側。

### なぜ `task_behaviors` に入れないか

第 3 版初稿は `judge:` を予約した behavior 名前空間を作り、 `task_behaviors` 側に
`trigger` を足す形にしていた。 nose の指摘で置き場所を変えた (2026-08-19):

- **`trigger` は「task をどう実行するか」ではなく「仕事がいつ始まるか」の性質**である。
  `task_behaviors` は前者の器なので、 後者を入れると器の概念的な強度が落ちる。 実際
  `trigger` は既存の behavior には 1 つも要らないフィールドで、 それが混ざっている印になる
- **`judge:` 予約名前空間は、 その混在を名前で区別するための回避策だった。** トップレベルへ
  出せば「daemon が起動してよいものか」を名前で区別する必要が無くなり、 予約語ごと不要になる
- **`judge:intake` のように「判断」と「取り込み」を 1 語へ押し込む必要も消える。** 押し込む
  必要があったのは名前空間で括る制約があったからで、 制約が無ければ名前は自由でよい
- 結果として **daemon と workspace の結合が緩む**。 daemon は「10 分ごとにこのコマンドを
  走らせる」以上のことを知らない

### `run` はパスではなくコマンド文字列

`run` は **`sh -c` に渡すコマンド文字列**とし、 スクリプトの「パス」を daemon が解決する
形にはしない。 理由は既存の契約と揃えるため — `.boid/hooks/*.sh` を外部ファイルとして
参照する script hook は 2026-07 に撤廃済みで
([script-hook-removal.md](script-hook-removal.md))、 残っているのは `hooks[].command` の
inline 文字列だけである。 撤廃の理由は「sandbox 内 clone は tracked file しか持ってこないので、
`.boid/` を gitignore している project で silent に ENOENT 落ちする」という契約問題だった。

コマンド文字列にすれば:

- `python3 scripts/intake_tick.py` も `bash scripts/sweep_tick.sh` も同じ形で書ける
  (**スクリプトはそれなりに複雑になるので `.py` を書けることは要件** — nose)
- 拡張子から interpreter を推測する知識を daemon が持たずに済む
- 失敗したときは**普通のコマンド失敗**として見える。 設定のパス解決が silent に外れる形にならない

**スクリプトは commit されている必要がある** (sandbox の clone は tracked file だけ)。
`scripts/` 配下は tracked なのでそのまま動くが、 開発ループは変わる (下記 未確定)。

## トリガは周期型 1 形でよい

v1 は **`every: <duration>` の周期型だけ**にする (nose 2026-08-19)。

### 反応型は要らなくなった

第 3 版初稿は反応型 (`on: task_input` — 「新しい事実が来ていて、 まだ誰も反応していない」) を
daemon の述語として持たせる案だった。 当時の論拠は「反応型の評価には actions の台帳が要り、
台帳は決定 13 / 14 で daemon にしか無い」というもの。

`action_list` (I-8) を足すと、 **その台帳をスクリプトが読める**。 反応型の判定はスクリプトが
書けるようになり、 しかも判定自体は決定論なので P-1 にも触れない。 待ち時間は最大で
トリガの間隔ぶん (10 分) になるが、 現行 khi が同じ粒度で回っていて実用上の不足は出ていない。

したがって v1 では反応型を daemon に置かない。 レイテンシが問題になった時点で足せばよい。

この結果、 daemon 側から次のものが消える:

| 第 3 版初稿で daemon に置こうとしていたもの | v4 |
|---|---|
| `probe` (周期型の前置ゲート) | **不要**。 スクリプト自身が probe になる — 走って、 用が無ければ task を作らずに終わる |
| `on:` / `scope:` の closed set | **不要**。 スクリプトが `task list` / `action_list` で絞る |
| `max_per_sweep` | **不要**。 スクリプトが決める |
| dispatch の粒度 (per-target か batched か) | **不要**。 スクリプトが task を何個作るか決める |
| `inputs` に数える action の closed set | **不要**。 スクリプトの中の話になる |

`probe` は第 3 版で Fable レビューを受けて足したものだが、 トリガがスクリプトを起こす形なら
**周期スクリプトがそのまま probe になる**ので、 別建てにする理由が無くなった。

### P-3 の縮小

以上により P-3 は「daemon が判断のトリガを全部持つ」から
**「daemon は時計と single-flight を持つ」**に縮む。 「LLM に用があるか」の判定は workspace 側へ
戻るが、 **決定論のまま戻る**ので P-1 (LLM の判断は workspace) とは別の話である。

第 3 版初稿では P-3 の論拠を「起動条件を判断で決めると循環する」に置いていたが、 循環を防いで
いるのは「決定論であること」であって「daemon にあること」ではない。 2 つを分けて考えると、
daemon が持つ必然性があるのは次の 3 つだけになる。

### daemon が持つのは 3 つだけ

| # | daemon の持ち分 | なぜ daemon か |
|---|---|---|
| 1 | **スケジュール** | 時計を持つ主体が要る。 workspace 側の cron はホスト依存に戻る |
| 2 | **single-flight** | 前回がまだ走っているなら見送る。 **daemon にしか正しく実装できない** — workspace 側の flock はホスト単位でしか効かず、 コンテナ化された daemon や複数ホストで破れる |
| 3 | **実行結果の記録** | 落ちたか・ どれだけかかったか。 バックオフと通知の材料 |

この 3 つは**ドメインに一切依存しない**。 逆に「用があるか」「何個起こすか」「どの card か」は
全部 workspace のもの。

失うものもある: **多重起動防止・ バックオフ・ 流量制御のうち、 daemon が持たない部分は各
workspace が書く**ことになり、 他ワークスペースへ展開するときに再実装が要る。 上の 3 つを
daemon に置くのは、 その再実装を最小にするための線引きである。

## 判断の記録: 汎用の注記 action

トリガのスクリプトが「LLM に用があるか」を判定するには、 **「LLM が見た」の記録**が要る。
見たが変えなかった巡で何も残らないと、 daemon の状態からは「まだ誰も反応していない」が
永続 true になり、 同じ card で毎巡 LLM が呼び直される (現行 khi の `suggestion_reviewed` が
まさにこれを防いでいる)。

第 3 版初稿は `judged { kind, outcome }` という**専用の verb** を daemon の語彙に足す案だった。
判定がスクリプト側へ移ったことで、 **この record の消費者も daemon の述語ではなく workspace の
スクリプトになった**ので、 daemon が `kind` や `outcome` を解釈する必要が無くなる。

そこで **payload が完全に不透明な汎用の注記 action** 1 つにする (nose 2026-08-19):

```
noted { <workspace が決める任意の JSON> }
```

- non-transitioning (`ToStatus: ""`)、 `Manual: true`。 `attrs_set` と同じ枠
- **daemon は payload を一切解釈しない**。 記録して、 `action_list` で返すだけ
- 「見たが変えなかった」も「バックオフ中」も「この kind の判断を済ませた」も、 全部
  workspace が payload の形を決めて表現する

### 汎用化の線引き

汎用性を重視する設計は、 **ドメイン知識を構造化する労力から逃げている**ことがある。 「何でも
入る箱」を置けば設計は進んだ気になるが、 実際には決めるべきことを先送りしているだけ、 という
形になりやすい (nose の指摘、 2026-08-19)。

ここで汎用注記を選ぶのは、 **daemon が汎用フレームワークであること自体に意味がある**から
である。 判断の種類は workspace ごとに違い、 増える前提であり、 その語彙を daemon が知ると
「種類が増えると boid を変更する」構造に戻ってしまう。

したがってこの判断は**この位置に限った条件付きのもの**であり、 一般則ではない。
**workspace 側の語彙は逆にきちんと構造化すべき**で、 khi が `noted` の payload に何を入れるかは
khi 側で型として決める。 汎用注記を理由に workspace 側の設計を曖昧にしてよい、 という話ではない。

### 却下の記録: `answered` action

こちらは判断ではなく**人の回答**の記録で、 書き手は Web UI (= daemon 側) である。 決定 14 で
判断を Web UI へ移したのに Web UI 側に回答を記録する経路が無く、 **却下しても再提案の抑止が
効かない**穴が現に開いている (下記「既に開いている穴」)。

```
answered { answer: "accept" | "reject", verb: "...", basis: "..." }
```

副作用として `detail.attrs.suggestion` を落とす。 これを `noted` に寄せず専用 verb にするのは、
**人の操作は daemon 自身の UI から出る daemon の出来事**であり、 監査ログとして他の人手操作
(park / drop / Go) と同じ語彙の並びに置きたいため。

**却下履歴の突合 (`verb` / `basis` の一致) は daemon に置かない。** それは suggestion の中身を
読むことになり境界を越える。 トリガのスクリプトか、 起こされた判断 task が `action_list` で
履歴を読んで自己抑制する。

### 提案 (`proposed`) は v1 では足さない

第 3 版初稿は提案も第一級 action (`proposed`) にする案だった。 動機は「自律 Go の gate が
`suggestion.verb` や `basis` を読むのに、 opaque blob からは有無しか読まない、 と書いていて
矛盾する」というもの (Fable レビュー指摘)。

その矛盾が現れるのは **gate を実装する段階 4 (自律 Go)** であり、 v1 の射程外である。 v1 では
提案は今まで通り `attrs.suggestion` に置き、 「提案した」という事実は `noted` で記録する。
段階 4 に進むとき、 gate が読むものを第一級のフィールドへ昇格させる (`proposed` verb を足すか、
gate が読んでよいキーを closed set に加えるか) を決める。 **v1 の表面積を増やさないための
先送りであり、 論点は消していない**。
## 自律 Go への道筋

この取り組みの目的は生産性であり、 最終的に狙うのは **LLM が自律的に動ける場**である。
判断スケジューラはそのための器で、 到達点は「Go できそうなものは LLM が自分で Go する」。

| 段階 | 誰が Go を出すか | 状況 |
|---|---|---|
| 現在 | 人 (Web UI) | 決定 14 |
| 段階 1 | LLM が **提案**し、 人が承認 | `suggestion.verb: go`。 ほぼ実装済み |
| 段階 2 | daemon の**決定論 gate** が、 条件を満たす提案を自動で通す。 人は事後に見る | 本 doc の射程外だが、 器はここで作る |

段階 2 の gate は DB と設定だけで決まる形にする — 例えば「子が `specced` ∧ 対象 project が
auto-go を opt-in ∧ spec が readonly 相当 ∧ 提案の根拠が既存の `answered` と一致しない」。
ただし **gate が提案の中身を読む以上、 その提案は第一級のフィールドとして表現されている
必要がある** — 段階 4 に進むときに `proposed` verb を足すか、 gate が読んでよいキーを
closed set に加えるかを決める (上記「提案は v1 では足さない」)。 **判断そのものは
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
Slack スレッドが、 ある issue の話である) の統合で、 これは意味なので **LLM の判断**として
扱う (workspace 側で「統合」の判断を 1 つ立てる)。

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
| J-1 | トリガを **`project.yaml` のトップレベル `triggers`** に置く。 `task_behaviors` は変更しない | `trigger` は「どう実行するか」ではなく「いつ始まるか」の性質。 既存 behavior には 1 つも要らないフィールドなので、 混ぜると器の概念的な強度が落ちる |
| J-2 | `run` は **`sh -c` に渡すコマンド文字列**。 パス解決はしない | 既存契約と揃える (script hook の外部パス参照は 2026-07 に撤廃済み)。 `.py` も `.sh` も同じ形で書け、 失敗が普通のコマンド失敗として見える |
| J-3 | v1 のトリガは **周期型 (`every:`) だけ**。 反応型は後で | `action_list` があればスクリプトが反応型の判定を書ける。 待ちは最大 10 分で、 現行 khi と同じ粒度 |
| J-4 | daemon が持つのは **スケジュール / single-flight / 実行結果の記録** の 3 つだけ | ドメインに依存しない。 とくに single-flight は daemon にしか正しく実装できない (workspace の flock はホスト単位でしか効かない) |
| J-5 | 「LLM が見た」の記録は **payload が完全に不透明な汎用注記 action (`noted`)** にする | 消費者が daemon の述語ではなく workspace のスクリプトになったため、 daemon が中身を知る必要が無い。 判断の種類が増えても daemon は無変更 |
| J-6 | `answered { answer, verb, basis }` は専用 verb のまま。 副作用で `suggestion` を落とす | 人の操作は daemon 自身の UI から出る daemon の出来事であり、 他の人手操作と同じ語彙の並びに置きたい |
| J-7 | 却下履歴の突合は daemon に置かず、 スクリプトか判断 task が `action_list` で自己抑制する | `verb` / `basis` の一致判定は suggestion の中身を読むことになり境界を越える |
| J-8 | 自律 Go は **決定論 gate + project 単位 opt-in**。 判断は LLM、 通してよいかは daemon | 決定 15 (auto-done) の対称。 監査は `Action.Actor` で既存台帳に乗る。 gate が読む形の第一級化は段階 4 で決める |

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

## 既に開いている穴 (J-6 の動機)

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
| `noted` の payload | **読まない**。 記録して `action_list` で返すだけ |
| `answered.answer` | 第一級 action のフィールド (人の操作の記録) |
| identity 文字列 | 完全に不透明。 名前空間も解釈しない |
| `triggers[].every` / `run` | 設定。 `run` の中身 (コマンドが何をするか) は知らない |

第 2 版は「`suggestion` キーを含む `attrs_set` を『喋った』印にする」形で、 daemon が blob を
詮索していた。 判定が workspace のスクリプトへ移り、 記録を `noted` (payload 不透明) にした
ことで、 **opaque blob から daemon が読むキーは決定 16 当時と同じ 1 つに保たれている**。

---

## boid 側に要るもの

| # | もの | 見立て |
|---|---|---|
| B-1 | identity 索引テーブル + 解決 + link / unlink op。 **`tasks.ref` 列と `idx_tasks_ref_parent_project` の去就を同時に決める** (下記) | 新規。 migration 1 本 + store + `BoidOp` |
| B-2 | 未着キーからの `captured` task 自動生成 (I-4)。 `captured` の可視化と滞留対策。 **push op の返り値契約** (解決された task_id、 新規 `captured` か既存かの区別、 identity 衝突時のエラー語彙) | 小〜中。 状態と遷移は実装済み。 返り値契約は workspace 側が原文の追記先を決めるのに必須 (下記) |
| B-3 | `action_list` 相当の `BoidOp` (workspace スコープ) | 小。 `ListActionsByTask` は既にある (呼び出し `internal/api/task_service.go:413`、 実装 `internal/orchestrator/store.go:431`) |
| B-4 | `noted` (J-5) と `answered` (J-6) の action 追加、 Web UI の回答からの発行 | 小 |
| B-5 | **トリガ実行機構** — `triggers` の読み取り、 スケジュール、 sandbox でのコマンド実行、 single-flight、 実行結果の記録 | 中。 本 doc の中核。 述語・ 流量制御・ dispatch 粒度を持たないぶん第 3 版初稿の見積もりより小さい |
| B-6 | `source_closed` 反転による auto-reopen (I-5) と、 done への着地経路 (I-5b) | 小。 `reopen` → `reopen_triaged` のルーティングは実装済み (`internal/api/triage_done.go:299`) |

### B-2: push op は解決先を返さないといけない

workspace は**原文を現地に残す** (決定 3) ので、 続報が来たとき「どの note に追記するか」を
知る必要がある。 ところが第 3 版初稿は「workspace はどの card かを一切決めない」と書いており、
**追記先を知る手段が無くなっていた** (Fable レビュー指摘)。

したがって push op は「押した」だけでなく**解決結果を返す**契約にする:

- 解決された `task_id`
- 新規に `captured` を起こしたのか、 既存 task に着地したのか
- identity 衝突 (I-1 の「2 枚目は拒否」) が起きた場合のエラー語彙 — workspace はこれを
  受けて統合の判断へ回す

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
| 周期的に何かを回すループ | **ある**。 `QueueSweepLoop` (`queue_sweep.go`) が interval ticker → 純粋述語 → `ActorDaemon` で action、 という形で動いている。 トリガ実行機構はこれと同じ骨格 |
| sandbox でコマンドを走らせる | **ある**。 exec job (`BuildExecJobSpec`、 task を持たない session job)。 ただし **daemon 自身が exec job を起こす経路は新規** — 現状の呼び出し元は CLI だけ |
| スクリプトから task を作る | **ある**。 `task_create` op |
| スクリプトから daemon の状態を読む | **一部ある**。 `task_triage_get` / `task_triage_list` はあるが `action_list` が無い (B-3) |
| 「見たが変えなかった」の記録 | **無い** (B-4 / J-5) |
| トリガの台帳・ single-flight・ 実行結果の記録 | **無い** (B-5 本体) |

論点 b (定期起動の機構: host cron vs daemon 内 scheduler) は **内蔵側に倒れる**。 外部 API を
叩く窓 (poll の cursor と頻度) は論点 h の通り workspace 側に残るが、 その poll を起こすのも
daemon のトリガになるので、 **workspace 側に cron は残らない**。

### 破綻しそうなところ

daemon 側に残る懸念は 2 つだけになった。 残りは workspace のスクリプトの中の話である。

| 懸念 | どちらが持つか | 対策 | 現行 khi の実装 |
|---|---|---|---|
| トリガの重複起動 | **daemon** | single-flight (J-4) | flock + 実行中 `[cycle]` task の確認 |
| トリガのコマンドが落ちる / 返らない | **daemon** | 実行結果の記録とタイムアウト。 落ち続けたことが見えること | preflight の step ごとのフェイルオープン / クローズ |
| 呼び直しの抑止 | workspace | `noted` を読んで判定 | `speech_index()` |
| 流量とコスト・ task を何個作るか | workspace | スクリプトが決める | 「note-suggest の fork は 1 巡 5 枚まで」 |
| 判断 task が `noted` を押さずに死ぬ | workspace | 次の巡でスクリプトが検出する | — |

**トリガ 1 回のコストは sandbox job 1 個**である。 これは現行 `cycle_preflight.sh` と同じ
(10 分ごとに `boid exec --readonly` が 1 本走る) なので、 コストの増加は無い。 task を
何個作るかはスクリプトが決めるので、 「判断 1 枚ごとにコンテナが立つ」形にするかどうかも
workspace 側の選択になった。

第 2 版は「cadence は自動で片付く、 独立した論点ではなかった」と書いていた。 いま見ると
**片付くのではなく昇格する**が近い — khi の `run-cycle.sh` + `cycle_preflight.sh` は
この機構のプロトタイプで、 上の 5 つは全てそこで実証済みである。 ただし v4 で daemon が
引き取るのはそのうち 2 つだけになった。

---

## 既存の決定との関係

| 既存 | 本 doc の扱い |
|---|---|
| **決定 3** (境界を越えるのは card 粒度、 本文は現地) | **守る**。 第 2 版のインボックス案は衝突していたが、 I-4 の変更で解消 |
| **決定 10** (mail/Jira/Slack は workspace 内 triage を通るので triaged で到着) | **一部を `captured` へ倒す**。 篩いは workspace に残るが、 起票の確定は daemon 側の判断に回る |
| **決定 12** (queue 評価は決定論のみ) | **徹底する側**。 identity 解決も gate も判断ゼロ。 トリガが走らせるスクリプトも決定論 (LLM は task の中でだけ動く) |
| **決定 13** (event 追記を正、 state は導出) | **徹底する側**。 workspace 側の二本目のログを畳む |
| **決定 14** (state の正は daemon) | **完成させる側**。 `claims` の退役でようやく fold が一本になる |
| **決定 15** (auto-done) | **鏡像を足す**。 I-5 の auto-reopen、 J-6 の auto-Go |
| **決定 16** (`ref` = canonical source のキー) | **一般化する**。 `ref` は「登録に使った 1 本目の identity」。 B-1 で列の去就を決める際に再定義が要る可能性 |
| **決定 17** (`reopen_triaged`) | **引き金を与える**。 第 12 版は「足りないのは押せる action だけ」としたが、 誰が何を見て押すかが空いていた |
| **論点 b** (定期起動の機構) | **内蔵側へ**。 daemon が時計を持ち、 workspace 側に cron は残らない。 外部 poll の窓 (cursor と頻度) は論点 h の通り workspace |
| **論点 g** (洪水対策 = 同一 source ref の再 push を update 扱い) | **置き換わる**。 多対一の identity 索引 + binding のライフサイクルへ一般化。 洪水自体は workspace 側の篩いで止まる |
| **論点 h** (source 既読化・ cursor は workspace 側) | **変わらない** |
| **逆輸入 3** (チャネル知識は workspace 側) | **守る**。 解釈するキーの closed set を上に明示した |

---

## 段階

1. **B-1 + B-2** — identity 索引と `captured` 着地、 push op の返り値契約。 土台
2. **B-3 + B-4** — `action_list`、 `noted` / `answered`。 workspace は読み口を付け替え、
   claims への書き込みを止める (物理退役はまだ)
3. **B-5 + B-6** — トリガ実行機構と auto-reopen。 ここで workspace の cron と自前の履歴が
   消える
4. (射程外) 自律 Go の gate。 提案を第一級のフィールドへ昇格させるかもここで決める

第 2 版は「cadence を先にコアへ持ち上げる」順序を推していた。 その順で始めると、
**後で消える予定のものを boid コアへ移植する**ことになるので、 identity を先に置いた。

---

## 非目的

- **daemon にエージェントを置く**こと。 P-1。 gate も述語も決定論のまま
- **daemon が原文を保持する**こと。 決定 3 を維持する
- **identity を認証・ 認可に使う**こと。 ここでの identity は配送先を決める索引であって、
  principal の権限とは無関係。 名前が示唆する以上のことをさせない
- **identity のチャネル知識を daemon に持たせる**こと。 名前空間の意味・ 正規化・ 妥当性検査は
  すべて workspace 側 (I-2)
- **判断の中身を daemon が知る**こと。 トリガが走らせるコマンドの中身も、 `noted` の payload も
  daemon は解釈しない (J-2 / J-5)
- **汎用注記を一般則として広げる**こと。 payload 不透明の判断は「daemon が汎用フレームワークで
  あること自体に意味がある」という条件付きのもので、 **workspace 側の語彙はきちんと構造化する**
  (「汎用化の線引き」節)

---

## 未確定

### 設計を左右するもの

- **`captured` → `triaged` を誰がいつ押すか**。 I-4 で新規 card が `captured` で着地する
  ようになるため、 毎 card 発生する工程になる。 候補は 3 つ: (i) **人が Web UI で押す** —
  今より 1 工程増える摩擦 (ii) **統合の判断が「統合先無し」と結論した後に押す** —
  LLM が state 遷移を押すことになるが、 これは優先度の判断ではなく**同一性の確定**なので
  決定 14 の park/drop/Go とは性質が違う、 と整理できるかどうか (iii) **取り込みの判断が
  起票と同時に即押す** — 統合の判断が走る窓が無くなる。
  現行実装の自然な順序は (iii) (`queue_notify.go:62` が「khi creates a card as captured,
  sends `triage`」と書いている)。 **B-2 の実装と、 統合判断を挟めるかが両方これに従属する**
- **`tasks.ref` 列を退役させるか、 drop 時に空にするか** (B-1)。 決定 16 の再定義を伴う
- **判断 task を通常 task と同じ一覧に出すか**。 出すと `boid task list` と Web UI が
  判断で埋まる。 何個作るかは workspace が決めるが、 見せ方は daemon の話
- **トリガの実行記録をどう持つか**。 実体のあるテーブルにするか、 最終実行時刻だけ持つか。
  single-flight と「落ち続けている」の検出に何が要るかで決まる
- **`triggers` の反映経路**。 `project.yaml` の変更が `boid project fetch` を要する現状と、
  daemon が保持するスケジュールの同期。 トリガを止める / 変えるときの挙動も含む
- **トリガのスクリプトが commit 必須になること**。 sandbox の clone は tracked file だけなので、
  今 khi が使っている「ホストからスクリプト一式を tar で流し込んで commit 無しに次の cron から
  効かせる」開発ループが使えなくなる。 設計としてはホスト依存が消えて正しい方向だが、
  体感は落ちる。 開発時だけの抜け道を用意するかを決める
- **トリガ実行の readonly**。 決定論のスクリプトは読み取りと `task create` しかしないので
  readonly でよいはずだが、 gateway の POST が要る処理を将来トリガに置きたくなったときの
  方針を決めておく (現状 gateway は readonly な job token に GET/HEAD しか通さない)
- **single-flight の粒度と、 詰まったときの挙動**。 トリガ単位か project 単位か。 前回が
  返ってこないまま何巡も見送り続ける状態をどう検出して知らせるか
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
