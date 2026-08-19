# トリガと取り込み identity (設計メモ)

2026-08-18 起案、 2026-08-19 更新 (第 4 版。 同日、 未確定 26 件を仕分けて着手前の 5 件を決定 → I-7 / I-8 / I-9 / J-9 / J-10)。 **まだ実装しない。** `cross-project-issue-triage.md` (以下「本編」) の Phase 1
完了後に見つかった構造的な積み残しを、 独立した subsystem として切り出した検討。

workspace 側の対になる memo は khi-task-collector リポジトリの
`docs/plans/ingestion-identity.md`。 **本 doc が先で、 khi 側はこれを参照する側**である —
先に daemon の持ち分を決め、 workspace 側は「その持ち分をこう使う」として書く。

## 1. この doc について

boid に **判断のトリガ**と**取り込み identity** という subsystem を足すための設計である。
**実装はまだ 1 行も無い。** 着手前に決めるべきことは全て決めてあり、 PR-1 から書き始められる
状態にある。

### 前提のシステム

boid は汎用パーソナル AI オーケストレータで、 登場人物は 3 つである。

| | 何か | 本 doc での役割 |
|---|---|---|
| **daemon** | boid 本体のプロセス。 task の状態機械と SQLite を持ち、 job を docker コンテナのサンドボックスで起こす。 Web UI も daemon が出す | **同一性**と**時系列**を持つ |
| **workspace** | daemon が管理する作業単位。 中に project (git repo) が入る。 **外部 API の credential を持てるのは workspace だけ**で、 daemon は外部システムへ到達しない | **意味**を持つ |
| **人** (nose) | Web UI から判断する | **判断**を持つ |

本 doc がずっと具体例にする **khi-task-collector** は workspace の 1 つである。 Slack の自分宛
メンション・ Jira の担当課題・ Bitbucket の PR コメントを定期的に取り込み、 起票に値するものを
daemon 側の **triage task** として起こしている。 これは既に動いている (本編 Phase 1)。

### 用語

本編 (`cross-project-issue-triage.md`) の用語節が正だが、 本 doc を読むのに要る最小限を再掲する。

| 用語 | 意味 |
|---|---|
| **triage task** | 「まだ着手していない課題 1 件」を表す task。 メタプロジェクト所属の task が実行前の状態 (`captured` / `triaged` / `parked` / `ready`) にあるもので、 固有フィールドは sidecar テーブル `task_triage` が持つ。 **旧称の「card」は本編 第 6 版の task 統合で廃止済み** — 本 doc に残る `card` はファイル名 (`match_card.py` 等) と当時の引用だけである |
| **action** | daemon 側のイベント。 **append-only**。 状態遷移を伴うもの (`triage` / `drop` / `dispatch`) と、 伴わないもの (`attrs_set` / `child_added`) がある |
| **fold** | append-only な action 列を畳んで「現在の状態」を作ること。 daemon 側は `FoldDetailAttrs()` 等が実体 |
| **claims** | workspace 側が持っている JSONL のイベントログ。 daemon の actions と**二重になっている**もの。 本 doc が退役させたい対象 |
| **Go** | 人による実行承認 = triage task を `ready` にする判断。 ここを経ずに実行 task は発生しない |
| **identity** | **本 doc で導入する概念**。 外部キーを task にぶら下げる索引で、 認証の identity → principal と同じ構造 |
| **決定 N / 論点 x** | **本編**の決定・ 未決事項の番号 (決定 3、 論点 e など) |
| **I-x / J-x** | **本 doc**の決定番号。 I が identity 系、 J が判断スケジューラ系。 本編の決定 N とは別系列 |

### この doc の歩き方

| 節 | 何が書いてあるか |
|---|---|
| 2〜3 | **なぜやるか**と、 いま何が壊れているか。 設計の動機 |
| 4 | 設計の骨格。 原則と、 daemon / workspace / 人の三分割 |
| 5〜7 | **3 つの仕組み** — identity 索引・ トリガ・ 判断の記録。 それぞれ独立に読める |
| 8 | この設計が向かう先 (自律 Go)。 2 と対になる |
| 9 | **決めたこと** (I-1〜J-10)。 各 PR の「やらないこと」はここを根拠にしている |
| 10 | **実装計画**。 型・ ファイル・ op の形・ 不変条件・ 検証まで。 実装者はここで手が動く |
| 11 | **守る境界**。 この設計で最も破られやすく、 破ると全体が崩れる線 |
| 12〜13 | 残っている未確定と、 付録 (版の経緯・ 本編側の doc 修正リスト) |

実装から入るなら **9 → 11 → 10** の順で足りる。 設計の妥当性を見るなら頭から読む。

なお本 doc は **workspace 側の memo と対**になっている (khi-task-collector リポジトリの
`docs/plans/ingestion-identity.md`)。 boid 側の実装に必要なことは全て本 doc に閉じている。

---

## 2. なぜやるか

### 仕組みの目的

boid が目指しているのは「**提示された課題に応えていくだけで仕事が進む**」状態である
(本編の成功の定義)。 自分から見に行く負担をゼロにする、 完全 push 型。

その先にあるのは、 **人が応える回数そのものが減っていく**ことである。 いま人がやっている判断の
うち機械的に決まるものを LLM が肩代わりし、 人は本当に判断が要るものだけを見る。 そのためには
**LLM が自律的に動ける場**が要る — 誰かが起こさなくても定期的に目を覚まし、 状況を見て、 用が
あるときだけ動く場である。

**この subsystem はその場を作る。** daemon が時計と直列化だけを持ち、 何を判断するかは workspace
のスクリプトが決める形にすると、 **判断の種類が増えても boid 側は無変更**で足せる。 到達点は
「Go できそうなものは LLM が自分で Go する」で、 そこへ至る段階は 8 節に書いた。

### 今回の射程

| 作るもの | それで何が起きるか |
|---|---|
| **identity 索引** | 外部キー (`jira:ROOKPF-289` 等) から task を引けるようになる。 workspace が持っている自前の索引が退役する |
| **`action_list`** | daemon の actions を読み戻せるようになる。 workspace が持っている自前の履歴が退役し、 **同じ状態を畳む fold が一本になる** |
| **`triggers`** | 「10 分ごとにこのコマンドを走らせる」を daemon が持つ。 workspace からホストの cron が無くなる |

3 つとも **daemon 側の口の不足を塞ぐ**もので、 workspace 側の作り直しではない。 いま workspace が
自前で持っているものは、 持ちたくて持っているのではなく **daemon に口が無いから仕方なく持って
いる**。 それが次節である。

---

## 3. いま何が起きているか

以上が To-Be で、 ここからが「今どうなっていて、 何が足りないか」。

決定 14 (state の正は daemon ただ一つ) は workspace 側の `decisions/*.jsonl` を退役させた。
決め手は khi 側 doc にこう記録されている:

> **2 つの fold を並べると、 ロジックがズレたときに誰も気づかない** (決定 14 の決め手)

ところが退役したのは `decisions` だけで、 **`claims` + `fold_claims()` は残った**:

```
claims (workspace の JSONL) ──fold_claims──▶ workspace 側の畳んだ状態
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

### fold が二本ある理由 — daemon 側の口の不足 2 つ

どちらも workspace の都合ではなく **daemon 側の口の不足**である。

**理由 1: イベントの配送先を daemon が解決できない。** action は `task_id` 必須なので、
対応する triage task がまだ無いキーで飛んでくる観測を受け取れない。 だから workspace 側が
「キー → triage task」の索引を持ち、 着地させてから push する形になっている。

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

### もう一つの自作 — 定期実行がホストの cron に依存している

トリガの話は上の 2 つと別系統だが、 同じ「daemon 側に無いので workspace が自作している」形で
ある。 khi はいま `run-cycle.sh` + `cycle_preflight.sh` を**ホストの cron** から回しており、
多重起動防止は flock で行っている。

- **cron はホストに紐づく**。 daemon をコンテナ化しても workspace のスケジュールだけホスト側に
  残り、 他ワークスペースへ展開するときは同じ配線を再実装することになる
- **flock はホスト単位でしか効かない**。 daemon が複数ホストに散る構成では多重起動を防げない

### 既に開いている穴: 却下しても再提案が止まらない

khi の `fold_claims()` は「同じ根拠で却下済みの提案は出し直さない (`basis` が変わるまで)」を
実装しており、 入力は `suggestion_answered` claim である。 その書き手は note-inbox スキルだけで:

> 採用/却下は **Web UI** で出す (決定14)。 ここで答えた場合は
> `scripts/claim.py suggestion_answered` で却下履歴として残す

つまり **note-inbox で答えたときしか却下履歴が残らない**。 決定 14 で判断を Web UI へ移した
のに、 Web UI 側に生む経路が無い。 daemon 側を検索しても `suggestion` は完全に opaque key
扱いで、 daemon は存在すら知らない。 結果として **Web UI で却下しても再提案の抑止が効かない**
はずである。

---

## 4. 設計の骨格

### 原則

| # | 原則 | 帰結 |
|---|---|---|
| P-1 | **daemon は決定論的にふるまう。 LLM の判断は workspace 側に置く** | 決定 12 の再確認。 daemon にエージェントを置かない |
| P-2 | **外部からのシグナル取り込みは workspace 側** | credential 境界 (決定 11 / 論点 h)。 **外部システムへ到達できるのは workspace だけ**という線が、 決定 3 の改訂後も残る本来の境界である (初稿はここを「原文を越境させない設計 (決定 3) の帰結」と書いていたが、 改訂で原文は越境するようになったので理由を差し替えた) |
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

### daemon は時計と直列化だけを持つ

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
| 原文の保持 | **daemon** (2026-08-19 変更) | 決定 3 改。 上限は開示ポリシー (論点 e) が決める。 workspace は「原文の保持」という責務を持たない |
| 起票に値するかの篩い | **workspace の LLM** | 篩いは意味の判断。 P-1 より daemon には置けない |
| 次の一手の提案・ spec 執筆 | **workspace の LLM** | 同上 |
| 何かを始める時計と直列化 | **daemon** | P-3。 スケジュール / single-flight / 実行結果の記録 |
| 「LLM に用があるか」の判定 | **workspace のスクリプト** | 決定論なので P-1 と衝突しない。 対象の選択・ 流量制御も同じ側 |
| 配送先の解決 (キー → task) | **daemon** | 索引引きであって判断ではない |
| イベントの記録と畳み込み | **daemon** | 決定 13。 台帳は一箇所 |
| 決定論の rule (auto-done / wake / 呼び直し抑止) | **daemon** | 決定 12 |
| state 遷移の判断 (park / drop / Go / reopen) | **人 (Web UI)** | 決定 14 |
| **起票の確定 (captured → triaged)** | **未確定** | I-4 で新規の triage task が `captured` で着地するようになるため毎件発生する工程になる。 優先度の判断ではなく「既存の task の続報ではない」という同一性の確定なので、 決定 14 の park/drop/Go とは性質が違う。 下記 未確定 |
| 人への提示 (queue / triage task の表示) | **daemon** | 横断は daemon にしか作れない |

一行で言えば **daemon は「同一性」と「時系列」を持ち、 workspace は「意味」を持ち、
人は「判断」を持つ**。 daemon はイベントが**何であるか**を知らないまま、 **どれと同じか**
(identity) と **いつ何が起きたか** (actions) だけを扱う。

---

## 5. 仕組み 1: 取り込み identity

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
| アカウント統合 | 分裂した task の identity 付け替え (管理操作) |

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
流量は今の起票と同じ (= 既に回っている量) で、 メールを足しても増えるのは workspace
側の篩いの負荷であって daemon 側ではない。

すると未着キーの意味は「**起票に値すると判断されたが、 既存のどの identity にも当たら
なかったもの**」= **新規の triage task** になる。 これは本編が既に持っている状態でちょうど表せる:

> **不変条件の境界は captured → triaged** (起票の確定)。 captured (head-capture 直後、
> まだ形になっていない) は対象外
>
> captured: capture 直後、 triage 前。 主に head-capture 経由 (mail / Jira / Slack 経路は
> **workspace 内 triage を通るため** triaged で到着する、 決定 10)。 **Phase 0 では未使用**

決定 10 の「workspace 内 triage を通るため triaged で到着する」の workspace 内 triage が、
まさに今 LLM がやっている篩いである。 篩いを LLM に残したまま、 **起票の確定 (どの task に
なるか) だけを daemon 側の判断に回す**なら、 これらの経路は `captured` で到着するのが本編の
設計通りということになる。 `triage` (captured → triaged) と `drop` (captured → dropped) の
遷移も machine に実装済み。

したがって:

- **新規テーブルは identity 索引の 1 本だけ**。 inbox 用のテーブルも Web UI も新規には要らない
- 「未着」の可視化 = `captured` な triage task の一覧
- 決定 3 とも衝突しない。 daemon が持つのは workspace が開示すると決めた title / summary で、
  今の起票と同じ (第 2 版のインボックス案は、 起票判断の材料として生本文を daemon 側へ
  置く必要があり、 決定 3 と正面衝突していた)

残る判断は「index が外れたが、 実は既存の task と同じ件ではないか」(例: Jira key を持たない
Slack スレッドが、 ある issue の話である) の統合で、 これは意味なので **LLM の判断**として
扱う (workspace 側で「統合」の判断を 1 つ立てる)。

`captured` の滞留対策だけは新規に要る: 終端状態ではないので既存の 30 日 GC に乗らず、
放っておくと溜まり続ける (下記 未確定)。

---

## 6. 仕組み 2: トリガ

### 拡張ポイント: トップレベルの `triggers`

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

#### なぜ `task_behaviors` に入れないか

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

#### `run` はパスではなくコマンド文字列

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

sandbox の `/bin/sh` は dash なので、 bashism を書くなら `bash scripts/x.sh` のように明示する。

**スクリプトは commit されている必要がある** (sandbox の clone は tracked file だけ)。
`scripts/` 配下は tracked なのでそのまま動くが、 開発ループは変わる (下記 未確定)。

### トリガは周期型 1 形でよい

v1 は **`every: <duration>` の周期型だけ**にする (nose 2026-08-19)。

#### 反応型は要らなくなった

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

#### 訂正 (PR-4 実装時, 2026-08-19): `action_list` には穴が 1 つある

上の論証 (「`action_list` があればスクリプトが反応型の判定を書ける」) には、 PR-1〜3 のレビューで
判明した穴が 1 つある。 **`task_create` / `BoidOpTaskResolveOrCapture` の新規作成は action を
1 本も残さない** (`action_list.go` 冒頭のコメントが明言している通り、 captured task の誕生
そのものは actions テーブルに何も書かない)。 したがって `action_list` だけでは「新しい
captured task が来た」を検出できず、 **`task_triage_list --status captured` の併用が必須**
である — `action_list` は「既存 task に何が起きたか」の読み口であって、 「task の存在そのものの
変化」の読み口ではない。

この穴は上の結論 (v1 は反応型を daemon に置かない) を変えない — スクリプトは
`action_list` **と** `task_triage_list` の 2 つを組み合わせれば反応型の判定を書けるので、
「daemon の述語が要る」という第 3 版の論拠には戻らない。 変わるのは「`action_list` 1 つで足りる」
という前提の精度だけである。

#### P-3 の縮小

以上により P-3 は「daemon が判断のトリガを全部持つ」から
**「daemon は時計と single-flight を持つ」**に縮む。 「LLM に用があるか」の判定は workspace 側へ
戻るが、 **決定論のまま戻る**ので P-1 (LLM の判断は workspace) とは別の話である。

第 3 版初稿では P-3 の論拠を「起動条件を判断で決めると循環する」に置いていたが、 循環を防いで
いるのは「決定論であること」であって「daemon にあること」ではない。 2 つを分けて考えると、
daemon が持つ必然性があるのは次の 3 つだけになる。

#### daemon が持つのは 3 つだけ

| # | daemon の持ち分 | なぜ daemon か |
|---|---|---|
| 1 | **スケジュール** | 時計を持つ主体が要る。 workspace 側の cron はホスト依存に戻る |
| 2 | **single-flight** | 前回がまだ走っているなら見送る。 **daemon にしか正しく実装できない** — workspace 側の flock はホスト単位でしか効かず、 コンテナ化された daemon や複数ホストで破れる |
| 3 | **実行結果の記録** | 落ちたか・ どれだけかかったか。 バックオフと通知の材料 |

この 3 つは**ドメインに一切依存しない**。 逆に「用があるか」「何個起こすか」「どの task か」は
全部 workspace のもの。

失うものもある: **多重起動防止・ バックオフ・ 流量制御のうち、 daemon が持たない部分は各
workspace が書く**ことになり、 他ワークスペースへ展開するときに再実装が要る。 上の 3 つを
daemon に置くのは、 その再実装を最小にするための線引きである。

---

## 7. 仕組み 3: 判断の記録

トリガのスクリプトが「LLM に用があるか」を判定するには、 **「LLM が見た」の記録**が要る。
見たが変えなかった巡で何も残らないと、 daemon の状態からは「まだ誰も反応していない」が
永続 true になり、 同じ task で毎巡 LLM が呼び直される (現行 khi の `suggestion_reviewed` が
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

#### 訂正 (PR-4 実装時, 2026-08-19): `answered` は sandbox からも撃てる

`Manual: true` の action は `boid action send --type answered` で **sandbox 内 (= task agent)
からも書ける**。 J-6 が想定していた「書き手は Web UI (= daemon 側)」は運用上の意図であって、
**boid op レベルでは強制されていない** — daemon は `answered` の書き手が人か機械かを検証しない。

`Action.Actor` を見れば `human` (Web UI 経由) と `task:<id>` (sandbox 経由) は区別できるので
監査上の実害は無いが、 **この PR (トリガ) で起こす判断 task が自分で `answered` を書き始めると、
J-7 の「却下履歴で自己抑制する」の履歴に人と機械が混ざる**。 判断 task が「自分が却下された」
ことを表現したいなら `noted` (payload 不透明、 J-5) を使うべきで、 `answered` を使うべきではない
— 使ってしまうと、 人が Web UI で本当に却下した履歴と機械が自己申告した履歴が同じ verb の下で
見分けがつかなくなる。

**`answered` の消費者 (khi のスクリプトや判断 task 自身) は `Actor` でフィルタする必要がある**
— `Actor == "human"` の `answered` だけを「人の却下履歴」として扱い、 それ以外
(`task:<id>` 等) は無視するか、 別の意味 (機械の自己申告) として扱うこと。

### 提案 (`proposed`) は v1 では足さない

第 3 版初稿は提案も第一級 action (`proposed`) にする案だった。 動機は「自律 Go の gate が
`suggestion.verb` や `basis` を読むのに、 opaque blob からは有無しか読まない、 と書いていて
矛盾する」というもの (Fable レビュー指摘)。

その矛盾が現れるのは **gate を実装する段階 4 (自律 Go)** であり、 v1 の射程外である。 v1 では
提案は今まで通り `attrs.suggestion` に置き、 「提案した」という事実は `noted` で記録する。
段階 4 に進むとき、 gate が読むものを第一級のフィールドへ昇格させる (`proposed` verb を足すか、
gate が読んでよいキーを closed set に加えるか) を決める。 **v1 の表面積を増やさないための
先送りであり、 論点は消していない**。

---

## 8. どこへ向かうか — 自律 Go への道筋

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

## 9. 決めたこと

| # | 決定 | 根拠 |
|---|---|---|
| I-1 | 外部キーを identity として task に多対一でぶら下げる。 `ref` はその特例 | 多対一が実需 — Slack 起票の task に後から Jira のコメントが来る経路がある |
| I-2 | identity は `<namespace>:<key>` の不透明文字列。 daemon は解釈しない | 逆輸入 3 の境界 |
| I-3 | identity のスコープは **project** | 取り込まれるメタタスクはメタプロジェクトにしか紐づかない。 他プロジェクトへの展開は子 task が担い、 子は identity を持たない。 `(ref, parent_id, project_id)` unique (migration 0037) と同じスコープを踏襲する (B-1 の (a) 案を採ると index 自体は消える) |
| I-4 | 未着キーは `captured` な triage task として着地させる。 **専用 inbox は作らない** | 篩いが workspace 側に残るので、 daemon に届く時点で既に「起票に値する」と判断済み (上記) |
| I-5 | **done では identity を握り続ける**。 `observed.source_closed` が true → false に戻ったら `reopen_triaged` | 同じソースが動いたら普通は reopen したい (nose)。 決定 15 の鏡像で、 読むキーも同じ 1 個だけ |
| I-5b | I-5 の前提として、 **done の triage task にも観測が着地できるようにする** | `attrs_set` の `FromStatus` は captured/triaged/parked/ready/working の明示列挙で **done を含まない** (`machine.go:441`、 論点 6-3 が「never `*`」と決めた)。 現状は観測を done の task へ畳む合法経路が無く、 I-5 の判定材料そのものが更新されない |
| I-5c | done の task に着地したイベントは、 reopen 条件を満たさなくても**必ず可視化する** | reopen の引き金は `source_closed` の反転だけなので、 **canonical source は closed のままで Slack スレッドにだけ続報が来た**ようなケースは、 identity を握っているので配送はされるが reopen せず**黙って沈む**。 (決定 16 の契約下では canonical source を持たない task は本来 done に到達しないので、 slack-only の done は契約違反ケースの防御にあたる) |
| I-6 | **drop では identity を自動解放する** | drop は「この identity との関係を切る」判断そのもの。 握ったままだと、 捨てた件が動いても着地せず・ 自動 reopen の経路も無く (`machine.go:371` の `reopen_triaged` は done/aborted からのみ。 手動 `reopen` dropped→triaged は `machine.go:355` にあるが誤破棄からの回復経路であって観測に反応しない)・ 同じキーで新規起票もできない、 という静かに詰む形になる |
| I-7 | **`tasks.ref` 列は子 dedup 専用として残し、 外部キー用途だけ identity テーブルへ移す**。 root task は `ref` を持たなくなる | `Ref` には用途が 2 つ同居していた — 子 task の dedup (`Dispatch` が `children[i].ID` を入れる内部 ID) と外部キー (決定 16)。 退役案は前者の代替が要り、 drop で空にする案は決定 16 と矛盾する。 同居を解消すればどちらも失わない (2026-08-19、 nose) |
| I-8 | **v1 は排他の identity 1 本だけ**。 非排他の reference (`related_jira_issues`) は workspace 側の助言キーのまま daemon へ押さない | I-1 の実需 (Slack 起票の task に後から Jira のコメント) は排他の identity で表現できる。 epic の重複参照は workspace が「無くても害はない助言キー」と定義している (2026-08-19、 nose) |
| I-9 | **イベント push は差分 reconcile を続ける**。 per-event push にはしない | 現行の冪等性の担保 (workspace 側の差分 push + self-healing) をそのまま使える。 per-event はイベント単位の冪等キーの設計を伴う (2026-08-19、 nose) |
| J-1 | トリガを **`project.yaml` のトップレベル `triggers`** に置く。 `task_behaviors` は変更しない | `trigger` は「どう実行するか」ではなく「いつ始まるか」の性質。 既存 behavior には 1 つも要らないフィールドなので、 混ぜると器の概念的な強度が落ちる |
| J-2 | `run` は **`sh -c` に渡すコマンド文字列**。 パス解決はしない | 既存契約と揃える (script hook の外部パス参照は 2026-07 に撤廃済み)。 `.py` も `.sh` も同じ形で書け、 失敗が普通のコマンド失敗として見える |
| J-3 | v1 のトリガは **周期型 (`every:`) だけ**。 反応型は後で | `action_list` があればスクリプトが反応型の判定を書ける。 待ちは最大 10 分で、 現行 khi と同じ粒度 |
| J-4 | daemon が持つのは **スケジュール / single-flight / 実行結果の記録** の 3 つだけ | ドメインに依存しない。 とくに single-flight は daemon にしか正しく実装できない (workspace の flock はホスト単位でしか効かない) |
| J-5 | 「LLM が見た」の記録は **payload が完全に不透明な汎用注記 action (`noted`)** にする | 消費者が daemon の述語ではなく workspace のスクリプトになったため、 daemon が中身を知る必要が無い。 判断の種類が増えても daemon は無変更 |
| J-6 | `answered { answer, verb, basis }` は専用 verb のまま。 副作用で `suggestion` を落とす | 人の操作は daemon 自身の UI から出る daemon の出来事であり、 他の人手操作と同じ語彙の並びに置きたい |
| J-7 | 却下履歴の突合は daemon に置かず、 スクリプトか判断 task が `action_list` で自己抑制する | `verb` / `basis` の一致判定は suggestion の中身を読むことになり境界を越える |
| J-8 | 自律 Go は **決定論 gate + project 単位 opt-in**。 判断は LLM、 通してよいかは daemon | 決定 15 (auto-done) の対称。 監査は `Action.Actor` で既存台帳に乗る。 gate が読む形の第一級化は段階 4 で決める |
| J-9 | **`captured` → `triaged` は LLM が提案し、 人が Web UI で押す**。 将来は J-8 と同じ器で自動化する | 「最初の時点では」人が押す (2026-08-19、 nose)。 提案は `description` 経由で人に見えるので **daemon は何も解釈せず**、 統合の窓も `captured` のまま残る。 自動化は J-8 の決定論 gate を再利用する形になり、 別系統を作らない |
| J-10 | `description` と action payload に**サイズ上限**を置き、 超えたら切り詰めずに**エラー**にする。 **値は 64 KiB (65,536B)** — description 側 (khi note 本文 32 枚実測、 max 15,584B) は約 4.2 倍、 action payload 側 (khi claims/attrs 実測、 畳んだ attrs 全体 max 22,572B) は約 2.9 倍のマージン。 実測の全文は §13 A-5 を参照 | 決定 3 改訂の帰結 — daemon が機械的に効かせられる開示の枠はサイズとフィールド粒度だけになった。 head / agent 発の source は外部に正本が無く daemon の写しが唯一なので、 黙って切ると復元できない。 エラーなら workspace が要約か分割かを選べる (2026-08-19、 nose)。 値は description / payload 両方を別々に実測した上で、 同じ定数を共用する判断込みで PR-2 実装時 (2026-08-19) に確定させた |

### I-5 は workspace 側の現行ルールの意図的な反転

khi の `match_card.py` は今、 終端 (done / dropped) のものを索引から除外している —
「片付いた件に後から追記すると応答済みのものが動いてしまう」ため。 I-5 はこれを引っくり返し、
「動いてしまう」を望ましい挙動と見る。 **退行ではなく意図的な方針変更**なので理由ごと記録する。

identity モデルにすると、 この規則を「索引からフィルタする」形で書く必要が無くなる。
done は握り続ける (I-5)、 drop は解放する (I-6) という **binding のライフサイクル**として
表現され、 `ref` の unique 制約と衝突しない。

#### 訂正 (PR-5 実装時, 2026-08-19): I-6 の手動 reopen は identity を復元しない

PR-1 レビューで判明していた事実を、 実装時に明文化しておく。 `drop` は `tx.UnlinkAllForTask` で
その task の identity binding を **全部消す** (I-6)。 誤って `drop` した task を手動
`reopen` (dropped→triaged、 `machine.go:355`) で救っても、 **binding は戻らない** —
`reopen` (dropped→triaged) は状態を戻すだけで、 identity を再 link する処理を一切持たない。

結果として、 誤破棄から回復した task は「見た目は triaged に戻ったが、 元の外部キーとは
もう紐づいていない」状態になる。 その外部キーは (I-6 の設計通り) 別の新規 task が
自由に link できる状態でもあるので、 **回復が遅れるほど「別の task に奪われている」
可能性が上がる**。 回復させるには workspace 側が identity link op を再度呼んで
明示的に再 link する必要がある — daemon 側でこれを自動化する経路は無い
(そもそも「どの identity を戻すべきか」は drop 時点の履歴からしか分からず、 それを
読むのは意味の判断に近づくため、 P-1 の線引き上も daemon には向かない)。

### 副産物: 暗黙則が不変条件になる

`match_card.py: build_key_index()` の既知の制約 —「複数の active card が同じ issue を参照する
場合、 観測は `updated_at` が最新の 1 枚にしか届かない」— は、 配送先が黙って変わる形で
事故のタネになっている (2026-08-09 に ROOKPF-289 で起きかけた)。 identity モデルでは
「1 identity は 1 principal」が明示された不変条件になり、 2 枚目の link 要求は
**silent newest-wins ではなく拒否**になる。

ただし引用元が例に挙げている **epic の子 task 群**は、 バグではなく正当な運用である
(workspace 側は `related_jira_issues` を「無くても害はない助言キー」と定義し、 複数 task の
重複参照を前提にしている)。 一律拒否にすると newest-wins が first-wins に変わるだけで
改善にならない。 **排他的な帰属 (identity) と非排他の言及 (reference) は性質が違う** —
昇格させてよいかは未確定。

---

## 10. 実装計画

### boid 側に要るもの

各項をブロックしている未確定は「未確定の仕分け」節のブロック関係表にまとめてある。

| # | もの | 見立て |
|---|---|---|
| B-1 | identity 索引テーブル + 解決 + link / unlink op。 `tasks.ref` は子 dedup 専用として残し、 外部キーだけこちらへ移す (I-7) | 新規。 migration 1 本 + store + `BoidOp`。 **ブロック解除済み** |
| B-2 | 未着キーからの `captured` task 自動生成 (I-4)。 `captured` の可視化。 push op の返り値契約 (解決された task_id、 新規 `captured` か既存かの区別、 identity 衝突時のエラー語彙) | **小**。 状態・ 遷移・ Web UI の triage ボタンは実装済みで、 J-9 により提案の解釈も要らない。 残るのは A-5 (サイズ上限) |
| B-3 | `action_list` 相当の `BoidOp` (workspace スコープ)。 I-9 により **since カーソル付きの一括読み**を既定とする (下記) | 小〜中。 `ListActionsByTask` は既にあるが per-task なので、 一括読みは新規 |
| B-4 | `noted` (J-5) と `answered` (J-6) の action 追加、 Web UI の回答からの発行 | 小 |
| B-5 | **トリガ実行機構** — `triggers` の読み取り、 スケジュール、 sandbox でのコマンド実行、 single-flight、 実行結果の記録 | 中。 本 doc の中核。 述語・ 流量制御・ dispatch 粒度を持たないぶん第 3 版初稿の見積もりより小さい |
| B-6 | `source_closed` 反転による auto-reopen (I-5) と、 done への着地経路 (I-5b) | 小。 `reopen` → `reopen_triaged` のルーティングは実装済み (`internal/api/triage_done.go:299`) |

#### B-2: push op は解決結果を返す

push op は「押した」だけでなく**解決結果を返す**契約にする:

- 解決された `task_id`
- 新規に `captured` を起こしたのか、 既存 task に着地したのか
- identity 衝突 (I-1 の「2 枚目は拒否」) が起きた場合のエラー語彙 — workspace はこれを
  受けて統合の判断へ回す

第 3 版はこれを**必須**としていた。 理由は「原文を現地に残す (決定 3) 以上、 続報が来たときに
どの note へ追記するかを workspace が知る必要がある」というもので、 workspace が
`task_id` → 自分の note (slug) を引けることまで要求していた。

**決定 3 の改訂 (2026-08-19) でこの要求は消えた** — 原文は description として daemon 側に載り、
workspace に追記先の概念が無くなったため。 返り値契約自体は「新規か既存か」を知りたい場面
(起票の通知、 統合判断への接続) で引き続き有用なので残すが、 **ブロッカーではなくなった**。

#### B-1: `tasks.ref` の去就は I-6 の前提

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

**決定 (2026-08-19)**: (c) — `ref` 列は子 dedup 専用として残し、 外部キー用途だけ identity
テーブルへ移す (I-7、 経緯は「未確定の仕分け」A-2)。 二重表現のドリフトは、 root task が `ref` を
持たなくなることで消える。

#### B-3: per-task だけだと tick が O(N) になる

`ListActionsByTask` は task 単位である。 workspace の tick が「新しい事実が来ていて、 まだ
反応していない task を選ぶ」をやるとき、 per-task の口しか無いと **全 triage task を列挙して
1 枚ずつ読む**形になり、 10 分ごとに task の数ぶんの brokered op が飛ぶ。 現行 khi はローカル
ファイルを舐めているので同じ走査が無料だった (Fable レビュー指摘)。

task が数十枚なら実害は無いが、 **「反応型はスクリプトが書ける」(J-3) という論証の実装コストの
前提**である。

**決定 (2026-08-19)**: I-9 (差分 reconcile を続ける) を選んだことで、 「押す前に現況と比べる」の
比較先が daemon の読み戻しになった。 per-task の口しか無いとこの走査が毎巡 O(N) の brokered op に
なるので、 **workspace スコープの actions-since-cursor 形を既定とする**。

#### B-5: 部品はほぼ揃っている

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

#### 破綻しそうなところ

daemon 側に残る懸念は 2 つだけになった。 残りは workspace のスクリプトの中の話である。

| 懸念 | どちらが持つか | 対策 | 現行 khi の実装 |
|---|---|---|---|
| トリガの重複起動 | **daemon** | single-flight (J-4) | flock + 実行中 `[cycle]` task の確認 |
| トリガのコマンドが落ちる / 返らない | **daemon** | 実行結果の記録。 落ち続けたことが見えること。 **タイムアウトは実装しなかった** (PR-4 実装後の Opus レビュー N-4、意識的な先送り — 12 節 B-5 の「実装時に見つかった懸念」節を参照。stdin を閉じない場合の永久ハングだけは別問題として直した) | preflight の step ごとのフェイルオープン / クローズ |
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

### 入れる順序 (段階 1〜4)

1. **B-1 + B-2** (→ PR-1 / PR-2) — identity 索引と `captured` 着地、 push op の返り値契約。 土台。
   間に **workspace 側の一括投入**が挟まる (「PR 分割」節の段取りを参照)
2. **B-3 + B-4** (→ PR-3) — `action_list`、 `noted` / `answered`。 workspace は読み口を付け替え、
   claims への書き込みを止める (物理退役はまだ)。 **読みと書きは 1 本にまとめる**
3. **B-5 + B-6** (→ PR-4 / PR-5) — トリガ実行機構と auto-reopen。 ここで workspace の cron と
   自前の履歴が消える
4. (射程外) 自律 Go の gate。 提案を第一級のフィールドへ昇格させるかもここで決める

第 2 版は「cadence を先にコアへ持ち上げる」順序を推していた。 その順で始めると、
**後で消える予定のものを boid コアへ移植する**ことになるので、 identity を先に置いた。

本編の PR-5 と同じ粒度 (型・ ファイル・ op の形と scoping・ 不変条件・ やらないこと・ 検証) で
書く。 **本 doc の PR 番号は本編 Phase 1 の PR-1〜5 とは別系列**である (本編 Phase 1 は完了済み)。
各見出しに対応する B-x を併記した。

着手前の 5 件 (A-1〜A-5) はすべて決定済みなので、 PR-1 から着手できる。

### PR 分割 — 5 本と、`tasks.ref` 退役の段取り

I-7 (`ref` は子 dedup 専用として残し、 外部キーだけ identity へ) は **1 つの PR では完了しない**。
daemon が既存 `ref` を機械的に identity へ写せないためである — identity は `<namespace>:<key>`
だが (I-2)、 既存の `ref` は `ROOKPF-289` のような裸のキーで、 namespace を補うには「どのチャネルの
キーか」を知る必要があり、 それは daemon には無い知識である (逆輸入 3)。

したがって投入は **workspace 側が自分の索引から link op で流し込む**形になり、 順序は次のとおり:

1. **PR-1** — `task_identities` を作り link / unlink / resolve を通す。 `tasks.ref` には触らない
2. **(workspace)** — khi が `match_card.py` の key index から namespace 付きで一括 link する
3. **PR-2** — push の宛先解決を identity 索引へ切り替える。 `ref` による root の get-or-create は
   fallback として残す
4. **PR-2 の後** — 切り替えが実地で回ったことを確認してから、 root task の `ref` を空にする
   migration を出す。 `idx_tasks_ref_parent_project` と子 task の `ref` は**そのまま残す**

「切り替えが回ったのを確認してから元を落とす」のは、 本編 PR-5 の khi 統合と同じ段取りである
(「fold 退役前の等価性検証」)。

---

### PR-1 (B-1): identity 索引

外部キー → task の索引を daemon に置く (I-1 / I-2 / I-3)。 **この PR だけでは挙動は変わらない** —
書き手も読み手もまだ居ない。 PR-2 と workspace の一括投入がそれぞれ乗る土台である。

**migration 0041**

- `task_identities` — `identity TEXT NOT NULL` / `project_id TEXT NOT NULL` /
  `task_id TEXT NOT NULL` / `created_at`
- `UNIQUE (project_id, identity)` — I-3 のスコープ。 **1 identity は高々 1 task** (I-1) を
  DB の制約として持つ。 `idx_tasks_ref_parent_project` (migration 0037) と同じスコープの踏襲
- `INDEX (task_id)` — 逆引き (task の identity 一覧、 drop 時の一括解放)
- `FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE` — 終端 task の 30 日 GC で
  binding も落ちる。 I-5 の「done では握り続ける」は実質 30 日の期限付きになるが、 task 行が
  消えた後も binding だけ残す意味が無いため、 これを受容する (仕分け B)
- **`tasks.ref` には触らない**。 上記の段取りのとおり

**store** (`internal/orchestrator/task_identity.go`)

- `LinkIdentity(dbtx, projectID, identity, taskID) error` — 既に**別の** task に紐付いていたら
  `ErrIdentityConflict`。 同じ task への再 link は成功 (冪等)
- `UnlinkIdentity(dbtx, projectID, identity) error`
- `UnlinkAllForTask(dbtx, taskID) error` — I-6 の drop 時解放
- `ResolveIdentity(dbtx, projectID, identity) (*Task, error)` — 未登録は `ErrTaskNotFound` と
  同型のセンチネル。 `FindTaskByRef` の並び
- `ListIdentitiesByTask(dbtx, taskID) ([]string, error)`

**drop での解放 (I-6)** は machine ではなく service 層に置く。 `machine.go` は純粋な遷移表で
副作用を持たないため、 `drop` 遷移が成立した直後の action 記録経路 (`TaskWorkflowService`) から
`UnlinkAllForTask` を呼ぶ。 done では**呼ばない** (I-5)。

**op** (`boid task identity ...`)

- `BoidOpTaskIdentityLink` / `BoidOpTaskIdentityUnlink` / `BoidOpTaskIdentityResolve`
- 追加手順は boid-add-builtin スキルの一式 (`protocol.go` / `broker.go` / `boid_shim.go` /
  `policy_ops.go` / `policy.go` / `boid_executor.go` + policy_test + drift test)
- **scoping は broker 側に書く**。 `BoidOpTaskCreate` と同型 — project 未指定なら
  `entry.Context.ProjectID` を埋め、 `resolveProjectRef` で解決してから
  `entry.Context.AllowsProject` を検査する。 **PR-4 / PR-5 のレビューが 3 回連続で指摘した
  クラス**がここに当たる (「brokered の scoping を executor 側にだけ書き、 broker 側が素通し」)。
  executor 側にも検査を置くのは二重化としては良いが、 **broker 側が権威**である
- 入出力は `BoidRequest` に `Identity string` を足して表す。 link は `{identity, task_id,
  project_id?}`、 unlink は `{identity, project_id?}`、 resolve は `{identity, project_id?}`。
  resolve の返りは **task の ID と status だけ** — task 全体は返さない (workspace は
  `task_triage_get` で取れる)。 未登録は「見つからない」を exit code で表し、 エラーにしない
  (get-or-create の判断は呼び手がする)
- **HTTP ルートは足さない。** 使い手は sandbox 内のスクリプトだけで、 ホスト側 CLI や Web UI から
  identity を触る用途が今は無い。 `boid task triage` が HTTP と brokered op の両方を持つのは
  Web UI が読むためで、 identity にはその使い手がまだ居ない (要るようになった PR で足す)

**不変条件**

- 1 つの identity は高々 1 つの task に紐付く。 2 枚目の link 要求は **silent newest-wins では
  なく拒否** (`ErrIdentityConflict`)。 これが `match_card.py: build_key_index()` の
  「`updated_at` が最新の 1 枚にしか届かない」という暗黙則を、 明示された不変条件へ変える
- 1 つの task は identity を複数持てる
- daemon は identity 文字列を**解釈しない**。 検証は「空でない」ことだけで、 namespace の有無も
  形式も見ない (I-2)
- done では保持し、 drop で解放する (I-5 / I-6)

**やらないこと**

- namespace の検証・ 正規化・ 妥当性検査 (I-2、 非目的)
- 非排他の reference (I-8)。 `related_jira_issues` は workspace 側に残る
- identity から task を作ること (PR-2)
- `tasks.ref` の root 用途の退役 (PR-2 の後)
- identity を認証・ 認可に使うこと (非目的)

**検証**

- 同じ `(project_id, identity)` を別 task へ link → `ErrIdentityConflict`。 **同じ task へ再 link
  すると成功する** (冪等) ことも pin する
- 別 project なら同じ identity 文字列を link できる (I-3 のスコープ)
- drop → 解放され、 同じキーで再び link できる (I-6)。 **done では解放されない** (I-5)
- task 行の削除 (GC) で binding が cascade で消える
- broker: 別 workspace の project を指す link / resolve が**拒否される** (executor 側だけでなく
  broker 単体のテストで)

---

### PR-2 (B-2): 宛先解決と `captured` 着地

観測の宛先を identity で解決し、 どの task にも当たらないキーは `captured` な triage task として
着地させる (I-4)。 **ここで khi が identity を使い始める。**

**op**: 既存の `action_send` / `task_create` を置き換えるのではなく、 **解決を前置する 1 本**を足す。

- `BoidOpTaskResolveOrCapture` — 入力は project + identity + 新規時の title / description。
  返りは `{task_id, created: bool}` (B-2 の返り値契約)
- `created: false` なら workspace は続けて既存の `action_send` で `attrs_set` を押す。
  **解決と記録を 1 op に混ぜない** — 記録の語彙 (`attrs_set` / `child_added` / …) は既に
  確立していて、 そこへ宛先解決を持ち込むと op が二重の責務を持つ
- identity 衝突時のエラー語彙は PR-1 の `ErrIdentityConflict` をそのまま返す。 workspace は
  これを受けて統合の判断へ回す
- 新規作成時は identity を **同一トランザクションで** link する。 作ったが link されていない
  task が残ると、 次の巡で同じキーがもう 1 枚作る

**`captured` で着地する** (I-4)。 状態も遷移も実装済みで、 `task_create` は既に `"captured"` を
受け付ける (`internal/api/task_create.go:75`)。

**サイズ上限 (J-10)**: `description` と action payload に上限を置き、 超えたら **エラー**。
切り詰めない。 **値は 64 KiB (65,536B)** — description 側 (khi note 本文実測 max 15,584B) は
約 4.2 倍、 action payload 側 (khi 畳んだ attrs 全体実測 max 22,572B) は約 2.9 倍のマージン。
実測の詳細と「同じ値を両方に使ってよい理由」は §13 A-5 を参照。 入口は 1 箇所ではないので、
`task_create` / `task_update` / `action_send` / `BoidOpTaskResolveOrCapture` のすべてで同じ
関数 (`orchestrator.ValidateContentSize`) を通す。 これとは別に、 `action_send`
(`ApplyAction`) を経由せず `actions.payload` を直接書く経路 (`boid task notify`
--progress/--done/--fail) も同じ関数を通すよう対応済み — エージェントが渡す自由文字列であり
「daemon 生成の固定フォーマット」ではないため、 4 箇所の入口と同じ扱いが必要だった。

**`captured` の可視化**: `captured` な triage task が Web UI の一覧に出ること。 **J-9 により
これ以外に daemon 側で要るものはほぼ無い** — `captured` → `triage` ボタンは Phase 1 PR-3 で
実装済み (`web/templates/tasks.templ:281`)、 提案は `desired_description()` の平文として人に
見えるので daemon は解釈しない。

**不変条件**

- 解決と新規作成は同一トランザクション (上記)
- 新規は必ず `captured` で着地する。 daemon が `triaged` まで進めることはない (J-9)
- daemon が opaque blob から読むキーは `attrs.observed.source_closed` の**ままで増えない**

**やらないこと**

- 提案の解釈 (J-9)。 `suggestion` は今までどおり不透明
- `captured` の TTL / 自動 drop (仕分け B: 可視化のみ)
- `triaged` への自動遷移 (J-9 の段階 2 以降)
- 統合 (「index が外れたが実は既存の task と同じ件」) の判断。 workspace 側の LLM の仕事
- root task の `ref` を空にすること (次の migration)

**検証**

- 未登録キー → `captured` な task が 1 枚できて identity が link される。 `created: true`
- 同じキーで再度呼ぶ → **同じ task を返し、 2 枚目を作らない**。 `created: false`
- 別 task に紐付いたキーで `LinkIdentity` が実際に `ErrIdentityConflict` を返した場合、
  そのエラーは (未 %w のまま) 呼び出し元まで伝播し、 途中で作った `task` はロールバックされて
  残らない。 **ただし実運用でこの分岐に到達するのは稀**: `internal/db/db.go` の
  `conn.SetMaxOpenConns(1)` により daemon 内の tx は完全直列化されるため、 「`ResolveIdentity`
  で未登録と判定 → その直後の `LinkIdentity` で衝突」という順序は同一 daemon 内では基本的に
  起こらない — 先に link した側の `WithinTx` が commit を終えてから次の呼び出しが tx を開くので、
  2 回目の呼び出しはそもそも `ResolveIdentity` の時点で既存 task を見つけ `created: false` を
  返す (これが通常系)。 この検証項目はエラー種別の**伝播経路**が正しいことの確認であり、
  「衝突が日常的に起きる」ことの確認ではない
- 新規作成に失敗したら identity も残らない (トランザクション境界)
- 上限超過 → エラーになり、 **切り詰められた行が残らない**
- `captured` な task が Web UI の一覧に出る

---

### PR-3 (B-3 + B-4): 読み口と判断の記録

段階 2。 **読みと書きを 1 本にまとめる** — workspace は「読み口を付け替え、 `claims` への書き込みを
止める」を一度にやるので、 片方だけ先に入れても khi 側が動けない。

**op**: `BoidOpActionList` (`boid action list`)

- **workspace スコープの一括読み**を既定とする (I-9 の連動、 B-3)。 入力は project (省略時は
  context) / since カーソル / limit / 任意の task_id 絞り、 返りは actions の配列と次のカーソル
- カーソルは actions が append-only であることに乗る (id または `(created_at, id)`)。 単調に
  進むので、 スクリプトは「前回の続きから」を自然に書ける
- scoping は `BoidOpTaskList` と同型 — 明示 project を検査し、 無指定時は `AllowedProjectIDs` を
  回して**決して無スコープで引かない**。 broker 側が権威 (PR-1 と同じ注意)
- 既定 limit を持つ (デフォルト値 + 上限)。 **「since カーソルで前進する読み」と「最新 N 件」は
  本質的に両立しない** ため、 実装は **昇順 (created_at 古い順) + since カーソル**を選ぶ —
  limit は 1 回の応答の行数を抑えるだけで、 retention (`noted` の蓄積対策) には効かない。
  `noted` の retention 自体は**未解決のまま残す** (仕分け B、 詳細は 12 節 B-3)。
  書き込み側の圧縮もしない (payload 不透明なので daemon には潰せない)
- `ListActionsByTask` (`internal/api/store.go:312`) は per-task なので、 一括読みは新規の
  store メソッドになる

**action の追加** (`internal/orchestrator/machine.go`)

- `noted` (J-5) — non-transitioning (`ToStatus: ""`)、 `Manual: true`。 `attrs_set` /
  `child_added` / `child_specced` を生成している同じループ (`machine.go:428`) に足し、
  FromStatus は `preExecutionStatuses` を踏襲する (論点 6-3: never `*`)
- `answered` (J-6) — 同じく non-transitioning、 `Manual: true`。 書き手は Web UI (daemon 自身)
- **`answered` の副作用 (`suggestion` を落とす) は fold 側に置く**。 `FoldDetailAttrs` が
  `attrs_set` を畳んでいるのと同じ場所で、 `answered` が `suggestion` キーを落とす形にする。
  理由は決定 13 (event 追記が正、 state は導出) — service 層で `detail` を別途書き換えると、
  同じ状態に書き手が 2 人いることになる

**Web UI からの発行** (J-6 の動機): 採用 / 却下のボタンが `answered` を送る。 これが
「既に開いている穴」(決定 14 で判断を Web UI へ移したのに、 Web UI 側に却下履歴を生む経路が無い)
の塞ぎである。

**不変条件**

- daemon は `noted` の payload を**一切解釈しない** (J-5)。 記録して `action_list` で返すだけ
- `answered.answer` は第一級のフィールド。 `verb` / `basis` は記録するが、 **daemon は突合しない**
  (J-7 — 突合は suggestion の中身を読むことになり境界を越える)
- 解釈するキーの closed set は `attrs.observed.source_closed` の 1 つのまま増えない

**やらないこと**

- 却下履歴の突合 (J-7)。 スクリプトか判断 task が `action_list` で自己抑制する
- `proposed` verb の第一級化 (段階 4)
- `noted` の圧縮・ 専用 GC。 **未解決のまま残す** — read 口の limit はページングの区切りに
  過ぎず、 retention には効かない (12 節 B-3 参照)
- workspace 側 `claims` の物理退役 (段階 2 では書き込みを止めるまで)

**検証**

- `noted` が任意形状の JSON payload を通し、 `action_list` でそのまま返る (daemon が中身を
  検証しないことを pin する)
- since カーソルが単調に進み、 同じ action を 2 度返さない
- scoping: project 無指定で**無スコープに引かない**
- `answered` が `detail.attrs.suggestion` を落とす
- Web UI の却下が `answered` を残す

---

### PR-4 (B-5): トリガ実行機構

本 doc の中核。 **段階 3 に入るので、 PR-1〜3 の実装で分かったことを織り込んでから着手する**。

**スキーマ** (`internal/orchestrator/spec_types.go`)

- `ProjectMeta` に `Triggers []Trigger` (`yaml:"triggers,omitempty"`) を足す。 **トップレベル**で
  あり `task_behaviors` は変更しない (J-1)
- `Trigger{Name, Every, Run}`。 `Run` は `sh -c` に渡すコマンド文字列 (J-2)
- **workspace envelope** は `decodeStrictNode` (`workspace_envelope.go`、 `KnownFields(true)`) で
  strict に読む。 workspace 側の default project 設定は `projects[]` の要素ではなく **`spec` 直下**に
  並ぶ形 (`WorkspaceEnvelopeProject` は `{name, url}` だけで、 `task_behaviors` / `base_branch` /
  `fork_point` は `workspaceEnvelopeSpecFields` の allowlist 側にある)。 したがって workspace レベルで
  `triggers` を書けるようにするなら **allowlist に `"triggers": true` を足し、 envelope 構造体にも
  フィールドを足す**の 2 手が要る。 足さないまま書くと **unknown field でエラーになる**ので、
  「project.yaml だけ」と決めるにしても意識的に決める (仕分け B)
- 旧 daemon は project.yaml を非 strict にパースするので `triggers` を**無警告で無視する**。
  受容する (`triggers` を書く project.yaml は新 daemon 前提)

**決定 (PR-4 実装時, 2026-08-19): workspace envelope には足さない。** `triggers` は
`workspaceEnvelopeSpecFields` / `WorkspaceEnvelopeSpec` に加えず、 workspace レベルでの
`triggers:` 記述は今後も unknown field として拒否され続ける。 理由:

- `task_behaviors` / `base_branch` / `fork_point` が workspace レベルのデフォルトとして意味を
  持つのは、 それらが「どの project に対しても一般化できるワークフローの形」だからである
  (`docs/plans/workspace-default-project.md`)。 一方 `trigger.run` は
  `python3 scripts/intake_tick.py` のような、 **特定 1 project の tracked tree にしか存在
  しないスクリプトパス**を指す。 workspace 内の複数 project が同じ `run:` を共有できる保証は
  何も無く、 むしろ「同じ workspace の別 project にたまたま同名のスクリプトが無い」ケースの方が
  普通である
- 同じ `run:` コマンドを複数 project へ機械的に展開すると、 撤廃済みの script hook 外部参照
  (`.boid/hooks/*.sh`) が「project によって存在したりしなかったりする」問題を起こしていたのと
  同じ形の契約問題を再現する — J-2 が「船着き場を daemon に持たせない」ために避けたかったのは
  まさにこの種の暗黙依存である
- 実需が無い: khi の実例 (`intake` / `sweep`) はどちらも project 固有のスクリプトであり、
  「複数 project で同じトリガ定義を使いたい」というユースケースが今のところ観測されていない

もし将来 workspace レベルでの共有が必要になったら、 `run:` をコマンド文字列のまま共有する
のではなく、 「workspace 内のどの project にも存在するスクリプト」という前提を明示的に検証
する仕組みとセットで再検討すること。

**スケジューラ** (`internal/api/trigger_loop.go`)

- `QueueSweepLoop` (`internal/api/queue_sweep.go:107`) と同型 — `Run(ctx)` が ticker を回し、
  `runOnce` が 1 巡ぶんを見る。 ctx に `ActorDaemon` を載せるのも同じ
- 1 巡で「`every` が経過していて、 かつ前回がまだ走っていない」トリガを起こす

**実行** — exec job を daemon 内から起こす

- `api.ExecDispatcher` (`sessionDispatcherAdapter.StartExec`、 `internal/server/wire.go`) が
  既にあり、 `BuildExecJobSpec` → `runner.Dispatch` を通る。 **新規なのは daemon 内のループから
  これを呼ぶ配線だけ**で、 job の組み立ても broker 登録も gateway 配線も既存経路に乗る
  (B-5 の表の「daemon 自身が exec job を起こす経路は新規」は、 正確にはこの配線を指す)
- `Argv` は `["sh", "-c", "exec 0</dev/null; " + trigger.Run]`。 sandbox の `/bin/sh` は dash
  なので bashism は `bash scripts/x.sh` と明示する側の責任。 `exec 0</dev/null; ` prefix は
  Opus レビュー N-4 対応 (2026-08-19) — container backend は `OpenStdin`/`AttachStdin` を
  無条件で立てるが daemon 発の trigger job には attach するクライアントが居ないため、 これが
  無いと stdin を読むコマンドが永久にハングする (実 podman で確認済み、 詳細は同節末尾)
- `Readonly: true` 固定 (仕分け B)。 boid op の allowlist は role にも readonly にも依存しない
  ので、 readonly のまま `task_create` / `action_send` が打てる

**single-flight と実行記録** (migration 0043)

- `trigger_runs` — `project_id` / `trigger_name` / `job_id` / `started_at` / `finished_at` /
  `exit_code`
- single-flight は「同じ `(project, trigger)` に `finished_at IS NULL` の行があれば見送る」。
  **daemon にしか正しく実装できない**部分である (J-4 — workspace 側の flock はホスト単位でしか
  効かず、 コンテナ化された daemon や複数ホストで破れる)
- 詰まり検出は N 連続見送りで通知、 失敗はフェイルオープン (次の巡も回す) + 連続失敗で通知
  (どちらも仕分け B)
- 手動 1 巡の口 (`boid trigger run <name>`) もここ (仕分け B)

**不変条件**

- daemon は `run` の中身を知らない (J-2 / 非目的)
- 同じ `(project, trigger)` が同時に 2 つ走らない
- トリガが task を作るかどうか・ 何個作るかに daemon は関知しない (J-4)

**やらないこと**

- 反応型トリガ (J-3。 `action_list` があればスクリプトが書ける)
- `probe` / `on:` / `scope:` / `max_per_sweep` / dispatch 粒度 (第 3 版で消えた)
- 時間帯窓 (仕分け B: スクリプトが時刻で自制する)
- ホスト側 cron の存続 (論点 b は内蔵側へ倒れた)

**検証**

- `every` の経過でちょうど 1 回起きる
- 前回が走っている間は見送る (single-flight)
- コマンドが非ゼロで落ちても次の巡が回り、 exit code が記録に残る
- **readonly の trigger job から `task_create` が通る** (この PR の前提そのものなので、
  推論ではなく実際に通して pin する)

#### 実装時に見つかった懸念 (PR-4 実装時, 2026-08-19): `noted` の頻度と dispatch loop

`TaskWorkflowService.ApplyAction` は **非遷移 action (`attrs_set` / `child_added` /
`child_specced` / `noted`) でも `s.runDispatchLoop` を goroutine で起動する**
(`internal/api/workflow_action.go`)。 `attrs_set` で既にそうなっていたので新規の問題ではないが、
このトリガ機構が 10 分周期でトリガを回すようになると、 各巡のトリガスクリプトが「見たが変えな
かった」を記録するために押す `noted` (J-5) の本数だけ、 このゴルーチンが追加で立つ —
「10 分周期 × triage task 数」のオーダーで hook 再評価が呼ばれる形になる。

**実測ではなくコードを読んで確認した内容**: `Evaluator.Evaluate` (`evaluator.go`) は
`task.Status != TaskStatusExecuting` なら即 `nil` を返す。 `noted` の対象になる triage task
(`captured`/`triaged`/`parked`/`ready`) は **どれも `executing` ではない**ので、 マッチする
hook は 0 件で確定し、 CPU コストはほぼ無視できる。 ただし `runDispatchLoop` は 0 件マッチでも
**`s.Tx.WithinTx` を最低 1 回 (GetTask + 条件付き UpdateTask) 実行する** — daemon の DB 接続は
`SetMaxOpenConns(1)` で完全直列化されているため、 `noted` を撃つたびに追加のラウンドトリップが
その直列化されたキューに 1 つ乗ることになる。

**判断 (2026-08-19)**: 「非遷移 action は dispatch loop をスキップする」という修正は
**この PR ではやらない**。 理由:

- `attrs_set` / `child_added` / `child_specced` の挙動まで一緒に変わるため、 hook が非遷移
  action で発火することに依存している経路が無いかを実物で確認する必要があり、 それは本 PR の
  スコープ (トリガ実行機構) を超える横断的な検証になる
- コストの実体は「hook 0 件確定 + 空の WithinTx 1 回」であり、 10 分周期という頻度を踏まえると
  実害が出るとは考えにくい (実測はしていない — 判断根拠はコードの読解のみ)

問題になった時点 (実際に daemon の応答が悪化した、 等) で「非遷移 action は dispatch loop を
スキップする」を再検討すること。 その際は `attrs_set` 等の既存経路への影響を先に実物で検証する。

#### Opus レビュー対応 (PR-4 実装後, 2026-08-19): single-flight のレース・詰まり検出の誤発火・その他

マージ前レビューで Blocker 2 件・要修正 10 件が見つかり、 同じブランチに追加コミットで直した。
判断が要った論点だけここに残す (機械的な直しは省略)。

**Blocker 1 (single-flight の check-then-act レース)**: 「列挙 (`ListInFlightTriggerRuns`) →
dispatch (`StartExec`、実機で数秒) → 記録 (`CreateTriggerRun`)」がすべて別トランザクションで、
`SetMaxOpenConns(1)` は個々の SQL 文を直列化するだけでこの Go 関数レベルの並びは一切守らない
— sweep tick と手動実行、 手動実行 2 連打、 daemon 2 プロセスのいずれでも実際に二重 dispatch した
(検証は実際の repro テストで固定、後述)。 **直し方**: `trigger_runs(project_id, trigger_name)` への
部分 UNIQUE インデックス (`WHERE finished_at IS NULL`) を追加し、`fireTrigger`
(`internal/api/trigger_loop.go`) を「行を先に INSERT (claim) → dispatch → job_id を UPDATE で
埋める」の順に並び替えた。 INSERT が UNIQUE 制約で弾かれたら `ErrTriggerRunInFlight`
(`internal/orchestrator/trigger_run.go`) を返し、 呼び出し側は通常の busy skip と同じ扱いにする —
これで J-4 が主張していた「workspace 側の flock はホスト単位でしか効かないが、 DB 行方式は
複数プロセスでも効く」が実装で裏付けられた (`TestSweepTriggers_TwoDaemonProcesses_OnlyOneDispatches`
が 2 つの独立した `*sql.DB` 接続で同じ DB ファイルを共有する形で実際に確認する)。
`job_id` は dispatch 前は空文字列を許容する形に変えた (`NOT NULL DEFAULT ''`、nullable 化ではなく
空文字列許容を選んだ — Go 側で `sql.NullString` を扱わずに済む)。
**dispatch 失敗時の後始末**は「行を CLOSE (exit code を入れて finished_at を埋める)」ではなく
**DELETE** にした — CLOSE すると `started_at` が実の値として残ってしまい、
既存の「dispatch 失敗はフェイルオープンで同一 tick に即リトライできる」
(`TestSweepTriggers_DispatchFailure_FailsOpenAndRetriesImmediately`、
このテストは今回も pin し続けている) という不変条件が壊れる (`triggerIsDue` が
`now.Sub(latest.StartedAt) >= every` を見るため、 CLOSE した行が残っていると elapsed=0 で
即リトライできなくなる)。
**migration の扱い**: 0043 は書き換えた (0044 を新設しなかった)。理由は、この PR 自体がまだ
どのタグにも入っておらず (`git describe` で確認、直近タグ `v0.3.11` より後ろの未タグコミット)、
main にもマージされていない 1 コミットの feature branch であるため — 0043 を適用した DB は
存在しえない。0044 を新設すると「未リリースの機能が未リリースのバグを踏んだまま出荷され、
別 migration で直された」という誤解を招く記録が残る。

**Blocker 2 (詰まり検出通知の誤発火)**: `due` を見る前に `busy` を見て `Skipped` に積んでいたため、
「`every` がまだ来ていないが前回がまだ走っている」 (`Interval=1m` で頻繁に起きる、 `every` が
sweep interval よりずっと長いと尚更) も見送りとしてカウントしていた。
`Interval=1m` / `every=10m` / 実行 5 分のケースで 3 分目に誤通知することを実測で確認した。
**直し方 (i)**: `SweepTriggers` の per-trigger ループで `triggerIsDue` を `busy` 判定より先に評価する
よう並び替えた (`due` が false ならそもそも `Skipped` に積まない)。
**(ii) 閾値を `every` 相対にするかどうか**: **やった**。
`TriggerStuckSkipThreshold` (生の sweep tick カウント) を撤去し、
`TriggerStuckOverrunMultiplier` (既定 3、`in-flight の継続時間 > 3 × trigger 自身の every` で通知) に
置き換えた。 理由: (i) だけでも報告されたケースは解消するが、 `Interval` と `every` の比が
崩れる (例 1m/10m) 限り、 「毎分 1 tick ずつ生カウントする」設計は同じ形の問題を再現しうる
(`every` が短いトリガと長いトリガで同じ生カウント閾値を使うのは無意味)。 継続時間ベースにすれば
トリガごとの `every` に自動的にスケールし、 かつ `TriggerLoop.trackSkipStreak` が in-memory
map (mutex 無し、単一ゴルーチン前提) のままで済む (tick カウンタを維持したまま `every` 相対に
するより shape がシンプルになった)。
`TestTriggerLoop_SkipStreak_NoFalsePositive_ExecutionShorterThanEvery` が
レビュアーの実測シナリオ (`Interval=1m`/`every=10m`/実行 5 分) を直接再現し、
通知 0 件であることを pin する。

**N-1 (job 行が引けない状態からの自己回復)**: `jobTerminalState` のエラー (job 行が GC で消えた等)
は今までどおり「次 tick に持ち越す」だけで、 自己回復経路が無かった (migration のコメントは
「フェイルクローズする」と書いていたが実装と矛盾していた — コメントを実装に合わせて直した)。
**直し方**: `reconcileInFlight` に `selfHealStaleTriggerRun` を足し、
`now.Sub(run.StartedAt) >= TriggerRunSelfHealGrace` (3 分、`TriggerStuckSkipThreshold` 時代の
「3 連続 tick」慣習を elapsed time で踏襲) が成立したら `CompleteTriggerRun` で
`TriggerRunSelfHealExitCode` (-1) を記録して強制終端する。
**tick カウンタではなく elapsed time にした理由**: `reconcileInFlight` は periodic sweep だけでなく
`RunTriggerNow` (HTTP ハンドラのゴルーチン) からも呼ばれるため、 tick カウンタを共有 map で持つと
mutex が要る (`TriggerLoop.skipStreak/failStreak` が mutex 無しで安全なのは、 それらが唯一の
sweep goroutine からしか触られないからで、 同じ前提は `reconcileInFlight` には無い)。
`run.JobID == ""` (SetTriggerRunJobID がまだ/決して届いていない) も同じ機構で救う。

**N-4 (コマンドのタイムアウト)**: **実装しない (意識的に先送り)**。 理由: トリガの `run:`
自体を daemon 側から強制終了する仕組みは、 コンテナの kill/reap や `every` との相互作用まで
含めた別スコープの設計判断になり、 この PR (single-flight/詰まり検出のバグ修正) の範囲を超える。
project.yaml のトリガ節に「daemon はタイムアウトを課さない、 `run:` 側で `timeout` コマンド等を
使うこと」を明記した (`docs/ja/reference/project-yaml.md` の `triggers[]` 節)。
**stdin wedge は事実だったので、 これは直した** (タイムアウト機構そのものとは別問題): 実 podman
コンテナで `OpenStdin:true/AttachStdin:true` かつ誰も書き込み/close しない stdin を再現したところ、
`cat` が SIGTERM でも止まらず永久にハングすることを確認した (`podman run -d -i ... sh -c
'cat'` を 5 秒後に inspect → `running` のまま)。`exec 0</dev/null; ` をコマンドの先頭に足すと
即座に正常終了することも確認した。`internal/api/trigger_loop.go` の `triggerRunArgv` で全トリガ
job のコマンドをこの prefix でラップするよう修正した。

**N-6 (`every` の実効解像度)**: **下限バリデーションを入れた**。
`orchestrator.TriggerSweepResolution` (= `time.Minute`) を新設し、 `ValidateTriggers` が
`every < TriggerSweepResolution` を project.yaml load 時に reject するようにした。
`internal/server/wire.go` の `TriggerLoop.Interval` もこの同じ定数を参照するようにして、
「実効下限」と「実際の sweep interval」が別々の値として drift しないようにした。

**N-2 (retention/index 不在)**: `idx_trigger_runs_inflight_unique` (Blocker 1 の部分 UNIQUE) に加え、
`ListInFlightTriggerRuns` の実クエリ形 (`WHERE finished_at IS NULL ORDER BY started_at, id`) を
専用にカバーする部分 index `idx_trigger_runs_inflight_started` も追加した (前者だけでは
`ORDER BY started_at` を効率よく満たせない)。 既存 30 日 GC (`orchestrator.GCTasks` を呼ぶ
`TaskGCStore.GC`) に `GCTriggerRuns` (`finished_at < cutoff` の DELETE、in-flight 行は年齢に
関わらず対象外) を同じトランザクションで追加した。

**N-3 (notify のタイムアウト)**: `TriggerLoop.notify` を `queue_notify.go`/`triage_done.go` と同じ
`context.WithTimeout(ctx, notifyTimeout)` でラップした。

**N-5/N-8 (テスト/コメントの実態訂正)**: `trigger_loop.go:193` 付近のコメントが指す存在しない
テスト名を実物 (`TestBoidBuiltinExecutor_TaskCreate_SucceedsFromWhatATriggerJobsSandboxWouldSend`,
`internal/server/trigger_readonly_task_create_test.go`) に直した。 そのテスト自身のコメントも
「本番の broker/executor 経路そのもの」という書き方を、 実際にカバーしているのは
`ExecuteBoidBuiltin` の単体呼び出しのみ (broker 登録・`validateBoidBuiltinCwd`・`boid_shim`
経由の実配線は通っていない) という実態に合わせて訂正した。 結論 (readonly は allowlist に
伝播しない) 自体はレビュアーが独立に確認しており正しいので実害は無い。

**N-9 (`started_at` の UTC 不整合)**: `CreateTriggerRun`/`CompleteTriggerRun` が呼び出し元の
`time.Time` をそのまま (タイムゾーン付きで) 保存していた (`StartedAt` は zero のときだけ `.UTC()`
していた) ため、 `TriggerLoop.runOnce`/`RunTriggerNow` の `time.Now()` (ローカル) がそのまま
DB に入っていた。 両関数とも呼び出し元の値に関わらず常に `.UTC()` する形に直し、 加えて
呼び出し側 (`runOnce`/`RunTriggerNow`) の `time.Now()` も `.UTC()` に揃えた (二重の防御)。

**N-10 (nil ガードの非対称)**: `SweepTriggers` の no-op ガードに `s.Jobs == nil` を追加した
(`Triggers`/`Projects`/`Meta` と同じ扱い)。`s.Exec == nil` は追加していない — こちらは既存の
per-trigger fail-open dispatch 失敗経路が既にカバーしており、 トップレベルガードに含めると
`reconcileInFlight` (既存の in-flight 行の解消) まで止まってしまうため。

---

### PR-5 (B-6): auto-reopen と done への着地

**done への着地 (I-5b)** — 現状 `attrs_set` の FromStatus は `preExecutionStatuses` だけで
`done` を含まないため (`machine.go:444`)、 I-5 の判定材料そのものが更新されない。 置き場所は
**service 層のガード** (仕分け B): `task_triage` 行を持つ task に限って done への `attrs_set` を
通す。 machine の rule に `done` を足すと論点 6-3 (通常 task の done に発火させない) を壊す。

**auto-reopen (I-5)**

- `orchestrator.ShouldAutoReopen` — 純関数。 `ShouldAutoDone` / `ShouldWake` と同型に書く
- 判定材料は `attrs.observed.source_closed` の **true → false 反転だけ**。 読むキーは増えない
- 遷移は既存の `reopen_triaged` (routing は `internal/api/triage_done.go` に実装済み)
- 評価契機は QueueSweepLoop — 決定 15 の `SweepDone` と同じ場所に並べる
- **フラップ対策**: 自動 reopen は 1 回だけで、 2 回目以降は通知して人に見せる (仕分け B)。
  回数は actions から数える (決定 13: state は導出、 専用カウンタ列を作らない)

**I-5c の可視化** — canonical source は closed のまま Slack にだけ続報が来た、 のような
「配送はされたが reopen しない」着地を**必ず可視化する**。 決定 16 の
`MissingCanonicalSourceGuidance` が「提示面が無いので sweep から log 出力に留める」という前例を
作っているので、 同じ形にするか queue へ出すかをこの PR で決める。

**不変条件**

- daemon が opaque blob から読むキーは `source_closed` の 1 つのまま
- `task_triage` 行を持たない done の通常 task には発火しない

**やらないこと**

- 自律 Go の gate (段階 4)
- 提案の第一級化 (段階 4)

**検証**

- `source_closed` の true → false で `done` → `triaged` になる
- 2 回目のフラップでは自動 reopen せず、 通知に落ちる
- triage 行を持たない done の task では発火しない
- done への `attrs_set` が、 triage 行を持つ task でだけ通る

#### 実装時の決定 (PR-5 実装, 2026-08-19)

**I-5b のガードの実装位置**: machine.go の rule には触れず、 `internal/api/workflow_action.go`
の `ApplyAction` が `sm.Apply` を呼ぶ**手前**で割り込む形にした
(`resolveAttrsSetDoneTransition`、 `internal/api/attrs_set_done.go`)。 判定は
`task_triage` 行の有無 (`GetTaskTriage` が `sql.ErrNoRows` を返すかどうか) で、
pre-Tx の読み (`s.TaskTriage`) と in-Tx の再検証 (`tx.GetTaskTriage`) の**両方**で同じ
関数を通す — park/attrs_set の他の分岐が持つ「pre-Tx で仮判定 → in-Tx で fresh 再検証」
の二段構えをそのまま踏襲した。 行が無い場合・ store 未配線の場合はどちらも
**素通りして `sm.Apply` の既存の reject に委ねる** (安全な方向へ倒す — 誤って通してしまう
方が危険なので)。 lookup 自体が失敗した場合 (`sql.ErrNoRows` 以外のエラー) は 500 として
呼び出し元に伝える — reject 扱いに握り潰さない。

**発見 1 (PR-1〜4 レビュー由来) への回答**: `answered` と違い `attrs_set` は今も
「行が無ければ作る」ままにした。 調べた結果、 これは穴にならないと判断した —
理由は 3 つ:

1. `attrs_set` の FromStatus 列挙 (`preExecutionStatuses`) には `pending`/`executing`/
   `awaiting`/`aborted` が無い。 `CreateTask` の既定 (`initial_status` 省略時) は
   `pending` なので、 **普通に作られた通常 task は `attrs_set` を一度も浴びずに一生を終える**
   — machine.go 上、 captured/triaged/parked/ready/working のどこにも入る遷移が無い
2. `parseAttrsSetPayload` は空オブジェクトを拒否するので、 `attrs_set` が作る行は
   **常に実データ入り** — `answered` の miss-case 修正が対処した「何も残さない空行」とは
   性質が違う
3. 唯一の理論的な穴は `CreateTask` の `initial_status=captured`/`triaged` が **project を
   問わず誰でも呼べる**ことに由来する — これを意図的に使えば「本当の triage task ではない」
   task を triage 語彙に乗せられる。 しかし乗せた瞬間からその task は `task_triage` 行に
   実データを持ち、 daemon 視点では「triage task として振る舞っている」以外に区別する
   材料が無い。 これは決定 13 (state は導出) の帰結として **受け入れる** — 行の有無以外の
   判定キー (project の allowlist 等) を足すことは、 P-1/逆輸入 3 が禁じる
   「daemon がプロジェクトの意味を知る」方向の越境になるため選ばなかった

**I-5c は log 側を選んだ**。 決定 16 の `MissingCanonicalSourceGuidance` が確立した
「提示面が無いので sweep から log に留める」前例と**同じ結論**だが、 評価するタイミングは
違う。 `MissingCanonicalSourceGuidance` は sweep ごとに再評価する**持続的な状態**なので
`logCanonicalSourceBreaches` の fingerprint-on-change が要るが、 I-5c は「この
`attrs_set` が着地した」という**離散イベント**そのものなので、 `attrs_set` が
実際に done task へ着地した瞬間 (`ApplyAction` の Tx コミット後、 `logAttrsSetOnDoneTriage`、
`internal/api/attrs_set_done.go`) に 1 回ログを出せば十分で、 fingerprint 機構は要らない
(イベントの発生頻度自体が workspace の取り込み周期で自然に絞られる)。 SweepReopen 側は
`source_closed==true` のケースを候補にすら入れない (何もしないので)。

**フラップ回数は actions から数える**。 専用カウンタ列は作らず、
`orchestrator.CountAutoReopens` (`internal/orchestrator/auto_reopen.go`) が
`task_id` の action 履歴を `Type=="reopen_triaged" && Actor==ActorDaemon` で filter
するだけの純関数。 **人間による手動 reopen (`Actor=human`) はカウントしない** — 人が
何度 reopen し直しても「フラップ」ではなく人の判断だから。 `SweepReopen` の外側ループが
やる pre-Tx の判定は `source_closed==false` かどうかだけ (Tx を開く価値があるかの安価な
足切り) — フラップ回数そのものの計算は `autoReopen` (`internal/api/triage_done.go`) の
中で `tx.ListActionsByTask` を呼んで **Tx 内 1 箇所だけ**で行う (`autoDone` の
「task_triage を fresh に読み直して判定を再実行する」という再検証の考え方は踏襲しつつ、
フラップ回数自体は pre-Tx 側で二重に計算していない)。

**フラップ通知の重複抑止**: `notify.command` は呼ぶたびに外部プロセスを実行する副作用の
ある操作なので、 `logCanonicalSourceBreaches` と同じ fingerprint-on-change が要る
(1 分ごとの sweep で同じ task が毎回通知を吐き続けたら実害が大きい)。
`TaskWorkflowService.lastFlappedReopen`
(`Notifier` と同じ構造体に置いた — sweep loop 側ではなく) が「今 flap 中の task 集合」を
保持し、 集合に**新しく入った** task だけ通知する。 QueueSweepLoop からの逐次呼び出し
1 本という前提 (`lastBreachFingerprint` と同じ前提) の上でのみ mutex 無しで安全。

**評価契機は既存の QueueSweepLoop に相乗り** — 新しいループは足していない。 doc の指定通り
`SweepReopen` を `SweepWake`/`SweepTriage` と同じ `runOnce` に並べただけで、
`InitialDelay`/`Interval` の新規調整は不要だった (10s/20s/30s の stagger 慣行に
新規エントリを足す判断そのものが発生しなかった)。

---

## 11. 守る境界

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

### 非目的

- **daemon にエージェントを置く**こと。 P-1。 gate も述語も決定論のまま
- **credential と外部到達性を daemon に集約する**こと。 決定 3 の改訂後も、 **境界として
  効いているのはここ**である。 原文が daemon に載るようになったのは、 その境界とは別の話
- **identity を認証・ 認可に使う**こと。 ここでの identity は配送先を決める索引であって、
  principal の権限とは無関係。 名前が示唆する以上のことをさせない
- **identity のチャネル知識を daemon に持たせる**こと。 名前空間の意味・ 正規化・ 妥当性検査は
  すべて workspace 側 (I-2)
- **判断の中身を daemon が知る**こと。 トリガが走らせるコマンドの中身も、 `noted` の payload も
  daemon は解釈しない (J-2 / J-5)
- **汎用注記を一般則として広げる**こと。 payload 不透明の判断は「daemon が汎用フレームワークで
  あること自体に意味がある」という条件付きのもので、 **workspace 側の語彙はきちんと構造化する**
  (「汎用化の線引き」節)

### 既存の決定との関係

| 既存 | 本 doc の扱い |
|---|---|
| **決定 3** (境界を越えてよい範囲) | **改訂した** (2026-08-19)。 「原文そのものは越えない」というハードな上限を外し、 上限を論点 e の開示ポリシーに一本化。 決定 14 の帰結 (「note は残す」) も連動して変わる (下記) |
| **決定 10** (mail/Jira/Slack は workspace 内 triage を通るので triaged で到着) | **一部を `captured` へ倒す**。 篩いは workspace に残るが、 起票の確定は daemon 側の判断に回る |
| **決定 12** (queue 評価は決定論のみ) | **徹底する側**。 identity 解決も gate も判断ゼロ。 トリガが走らせるスクリプトも決定論 (LLM は task の中でだけ動く) |
| **決定 13** (event 追記を正、 state は導出) | **徹底する側**。 workspace 側の二本目のログを畳む |
| **決定 14** (state の正は daemon) | **完成させる側**。 `claims` の退役でようやく fold が一本になる |
| **決定 15** (auto-done) | **鏡像を足す**。 I-5 の auto-reopen、 J-6 の auto-Go |
| **決定 16** (`ref` = canonical source のキー) | **一般化する**。 `ref` は「登録に使った 1 本目の identity」。 I-7 で (c) を採ったので **再定義が確定した** — root task は `ref` を持たなくなり、 canonical source は identity の 1 本目になる (付録の宿題) |
| **決定 17** (`reopen_triaged`) | **引き金を与える**。 第 12 版は「足りないのは押せる action だけ」としたが、 誰が何を見て押すかが空いていた |
| **論点 b** (定期起動の機構) | **内蔵側へ**。 daemon が時計を持ち、 workspace 側に cron は残らない。 外部 poll の窓 (cursor と頻度) は論点 h の通り workspace |
| **論点 g** (洪水対策 = 同一 source ref の再 push を update 扱い) | **置き換わる**。 多対一の identity 索引 + binding のライフサイクルへ一般化。 洪水自体は workspace 側の篩いで止まる |
| **論点 h** (source 既読化・ cursor は workspace 側) | **変わらない** |
| **逆輸入 3** (チャネル知識は workspace 側) | **守る**。 解釈するキーの closed set を上に明示した |

---

## 12. 残っている未確定

第 4 版で挙げた未確定 26 件を、 **いつ決めるか**で 3 分類した (2026-08-19)。 基準は「後から
変えたとき何が壊れるか」である。

| 分類 | 基準 | 件数 | 状況 |
|---|---|---|---|
| **A. 着手前に決める** | 決めないと migration / op の公開契約 / 状態遷移の骨格が決まらない。 後から変えると作り直しになる | 5 | **全件決定済み** → 9 節。 選択肢と決め手は 13 節 |
| **B. PR の中で決める** | 実装しながら決まる。 骨格は変わらない | 17 | 既定案あり。 異存が無ければそのまま実装する |
| **C. 後で決める** | v1 の射程外か、 運用してから決めたほうが良いもの | 5 | — |

**本節は B と C を並べる。** 本編側に残る 7 件は本編の記述を直す作業なので 13 節に置いた。

### B. PR の中で決める (17 件)

既定案に異存が無ければ、 そのまま実装計画に落とす。

#### B-1 (identity 索引) の中で

| 項目 | 既定案 |
|---|---|
| terminal task GC と identity binding の寿命 | **cascade delete**。 done は 30 日で GC されるので I-5 の「握り続ける」は実質 30 日の期限付きになるが、 task が消えた後も binding だけ残す理由が無い |
| identity の一括投入 (移行) | **daemon 側ではやらない。** namespace を補うにはチャネルの知識が要り、 それは daemon に無い (「PR 分割」節の段取り)。 workspace が自分の索引から link op で流し込む |

#### B-2 (`captured` 着地) の中で

| 項目 | 既定案 |
|---|---|
| `captured` の滞留対策 | **可視化のみ**。 TTL も自動 drop も置かない。 A-1 で (iii) を選ぶなら滞留そのものが発生しない |
| `description` に何を載せるか | 本文を足す。 導出フィールド (suggestion / 子一覧) は当面 `desired_description()` のまま置き、 Web UI 描画への移動は別 PR にする |

#### B-3 (`action_list`) の中で

| 項目 | 既定案 |
|---|---|
| `noted` の retention | **未解決。** 「since カーソルで前進する読み」(I-9 の連動) と「最新 N 件」は本質的に両立しないので、 read 口は昇順 + limit (ページングの区切り) に倒した — これは retention に何の効果も持たない。 書き込み側も圧縮しない (payload が不透明なので daemon 側で潰せない、 J-5 の対価)。 10 分周期で長寿命の task へ積まれ得るうえ、 終端でない task は 30 日 GC に乗らないので**行は増え続ける**。 それが問題になったら、 件数上限か期間で切るのは workspace 側の語彙を知っている khi が決める |

#### B-4 (`noted` / `answered`) の中で

| 項目 | 既定案 |
|---|---|
| 履歴の投入 (移行) | khi の `claims` から `answered` へ写す 1 回きりのスクリプト。 **これをやらないと移行直後に再提案の嵐になる** — J-6 が塞ごうとしている穴を移行自身が開ける |

#### B-5 (トリガ実行機構) の中で

| 項目 | 既定案 |
|---|---|
| 判断 task を通常の一覧に出すか | **出す**。 専用の kind は足さない。 判断はメタプロジェクトに閉じるので既存の project フィルタで足りる。 `boid task list` と Web UI が判断で埋まるようなら、 そのとき絞り方を足す — 何個作るかは workspace が決めるので、 まず実際の本数を見る |
| トリガの実行記録をどう持つか | **テーブル 1 本**。 single-flight と「落ち続けている」の検出の両方に要る |
| `triggers` の反映経路と version skew | `boid project fetch` に乗せる。 workspace envelope は `KnownFields(true)` の strict decode なので、 workspace の default project 定義に書けるようにするならスキーマ追加が必須。 旧 daemon の無警告無視は受容する (`triggers` を書く project.yaml は新 daemon 前提) |
| トリガ実行の readonly | **readonly 固定**。 事実確認済み — boid op の allowlist は role にも readonly にも依存しない (`internal/orchestrator/policy.go` の `boidPolicy(_ Role, pctx)`) ので、 readonly job からでも `task_create` / `action_send` は通る。 readonly が効くのは filesystem と gateway の POST だけである。 gateway の POST が要る処理をトリガに置きたくなったら、 そのとき方針を決める |
| single-flight の粒度と詰まり検出 | **トリガ単位**。 N 連続で見送ったら通知する |
| トリガ実行の失敗時方針 | **フェイルオープン** (次の巡も普通に回す) + 連続失敗で通知。 現行 khi が step ごとに使い分けているものは、 スクリプトの中に残る |
| トリガの時間帯窓 | **スクリプトが時刻で自制する**。 `every:` に窓は足さない (J-4 の「daemon はドメインに依存しない」に沿う) |
| 手動 1 巡の口 | **足す**。 デバッグに要る |

#### B-6 (auto-reopen) の中で

| 項目 | 既定案 |
|---|---|
| auto-reopen のフラップ対策 | **自動 reopen は 1 回だけ**。 2 回目以降は通知して人に見せる |
| I-5b の置き場所 | **service 層のガード** (`task_triage` 行の有無を見る)。 machine の `attrs_set` に done を足すと論点 6-3 (通常 task の done に発火させない) を壊す |

#### 段階 2 全体で

| 項目 | 既定案 |
|---|---|
| 無変更の観測をどちら側が抑止するか | **「押す前に現況と比べる」は workspace 側に残す**。 変わるのは比較先だけ (自前の fold → daemon の読み戻し)。 A-4 の (a) と同じ話の裏側である |

---

### C. 後で決める (5 件)

| 項目 | いつ | なぜ後でよいか |
|---|---|---|
| トリガのスクリプトが commit 必須になること | khi の移行時 | 開発ループの体感の話で、 boid 側の設計は変わらない。 抜け道を用意するかは実際に不便になってから |
| Web UI の本文の見せ方 | B-2 の後 | 本文が daemon に載ってから決めればよい |
| 開示ポリシー (論点 e) の実装時期 | workspace が 2 つめになるまで | 「khi = 本文まで」の 1 設定で足りる。 既定を保守的側に置く仕組みだけ忘れない |
| 判断 task の失敗の扱い | khi の実装時 | workspace 側の話。 `noted` を押さずに死んだ場合の検出はスクリプトが持つ |
| 本編への取り込み方 (決定番号の振り直し) | 実装が固まってから | 先にやると番号が動く。 I-4 / I-5 / I-6 / J-6 が決定 10 / 16 / 17 / 論点 g を書き換える点は変わらない |

---

## 13. 付録

### 着手前に決めた 5 件 — 選択肢と決め手

着手前に決めるべきとして切り出した 5 件は 2026-08-19 に全件決定した (→ I-7 / I-8 / I-9 /
J-9 / J-10)。 **どの案とどの案があって、 なぜこれを選んだか**を残す。 決定そのものは 9 節にある。

#### ブロック関係 — どの決定が何を止めていたか

| | B-1 identity 索引 | B-2 `captured` 着地 | B-3 `action_list` | B-4 `noted`/`answered` | B-5 トリガ | B-6 auto-reopen |
|---|---|---|---|---|---|---|
| A-1 `captured` → `triaged` を誰が押すか ✅ | | **●** | | | ○ | |
| A-2 `tasks.ref` の去就 ✅ | **●** | ○ | | | | |
| A-3 identity と reference の分離 ✅ | **●** | ○ | | | | |
| A-4 push の冪等性 ✅ | | **●** | ○ | | | |
| A-5 サイズ上限 ✅ | | **●** | | ○ | | |

● = 決まらないと着手できない / ○ = 決め方で形が変わりうる / ✅ = 2026-08-19 に決定

**この表から出た結論**: **B-1 は A-2 と A-3 の 2 件だけで着手できる。** A-1 (Fable が三巡
ずっと GO 条件にしてきた項目) は B-2 まで待てる — identity 索引テーブルと link / unlink /
解決 op は、 「未着キーが来たとき何をするか」に依存しないためである。

**現況**: 着手前の 5 件はすべて決まった。 A-1 の答えが「daemon 側に新規実装をほとんど要求
しない」形だったため、 **B-2 の見積もりは下がっている** (10 節の B-2 を参照)。

#### A-1. `captured` → `triaged` を誰がいつ押すか  [B-2 をブロック] ✅

I-4 で新規の triage task が `captured` で着地するようになるため、 毎件発生する工程になる。

| 案 | 誰が押すか | 代償 |
|---|---|---|
| (i) | 人が Web UI で押す | 今より 1 工程増える摩擦。 決定 14 と最も整合する |
| (ii) | 統合の判断が「統合先無し」と結論した後に押す | LLM が state 遷移を押す唯一の例外になる |
| (iii) | 取り込みの判断が起票と同時に即押す | 統合の判断が走る窓が無くなる |

**決定 (2026-08-19、 nose): 上記のどれでもない第 4 案 — (i) の押し方に LLM の提案を前置する。**
`captured` のうち実行可能そうなものについて LLM が `triaged` への遷移を**提案**し、 最後は人が
Web UI で押す。 「最初の時点では」であって、 **現在のアーキテクチャならさらに自動化できる**
という前提つき (→ J-9)。

これは J-8 (自律 Go) の段階表と同じ構造である — 「LLM が提案し人が承認する」段階を先に置き、
条件が固まったら決定論 gate が通す段階へ進む。 判断の種類ごとに器を分けずに済む。

実装上の含意 (2026-08-19 に実コードで確認):

- **daemon 側に新規に要るものはほぼ無い。** Web UI の `captured` → `triage` ボタンは Phase 1
  PR-3 で実装済み (`web/templates/tasks.templ:281`)。 残るのは `captured` な task が一覧に
  出ること (B-2 の可視化) だけである
- **daemon は提案を解釈しない。** Go 側で `suggestion` を読んでいる箇所は無く
  (`internal/orchestrator/task_triage.go` は「触らない」とコメントしているだけ)、 提案は
  `desired_description()` に畳まれた平文として人に見えている。 **解釈するキーの closed set は
  `attrs.observed.source_closed` の 1 つのまま**保たれる
- 提案を書くのは workspace 側 (取り込みの LLM)。 起票と同時に書けるので巡は増えない
- **統合の窓が残る**。 提案の付いた `captured` が並ぶので、 人が押す前に「これは既存の task と
  同じ件では」と気づける。 (iii) が失っていたものを、 摩擦を増やさずに保てる
- 自動化へ進むときは J-8 と同じ問題 (gate が提案の中身を読む以上、 提案は第一級のフィールドで
  ある必要がある) に当たる。 段階 4 でまとめて決める

#### A-2. `tasks.ref` の去就  [B-1 をブロック] ✅

I-6 (drop で解放 → 同じキーで新規起票できる) が現行の `ref` と衝突する件 (「B-1: `tasks.ref`
の去就は I-6 の前提」節)。 案は 3 つある。

| 案 | 中身 | 代償 |
|---|---|---|
| (a) | uniqueness を identity テーブルへ完全移譲し、 `tasks.ref` 列を退役させる | **子 task の dedup の代替が要る** (下記) |
| (b) | drop 時に `tasks.ref` を空にする | 決定 16 の「`ref` は後から変えられない」と矛盾するので、 決定 16 の再定義が要る |
| (c) | `ref` 列を**子 dedup 専用**として残し、 外部キー用途だけ identity テーブルへ移す | root task は `ref` を持たなくなる。 決定 16 を「canonical source は identity の 1 本目」へ再定義する |

**決定 (2026-08-19、 nose): (c)** (→ I-7)。 `Ref` には**用途が 2 つ同居している**:

- 子 task の dedup — `Dispatch` が `children[i].ID` を入れる。 これは内部 ID であって外部キー
  ではなく、 identity (`<namespace>:<key>` の不透明文字列) の概念にも当てはまらない
- 外部キー — 決定 16。 root task へ開いたのは Phase 1 PR-4 / 論点 7

(a) はこの前者の代替を作る必要があり、 (b) は決定 16 と矛盾する。 (c) は**同居を解消するだけ**で
どちらの用途も失わない。 I-1 の節が既に「子 task の `Ref` と外部キーを名前空間で分離できる」と
書いており、 (c) はそれを名前空間ではなく**列の分離**として実現する形である。

移行は「既存 root task の `ref` を identity テーブルへ写して列を空にする」1 回きりの migration。
`FindTaskByRef` の呼び手は get-or-create (`internal/api/task_create.go:260`) と
`orchestrator/store.go` の 2 箇所だけなので、 root の解決だけ identity 索引へ差し替えればよい。

#### A-3. identity と reference を分けるか  [B-1 をブロック] ✅

epic の子 task 群のような**正当な重複参照**を、 排他的な identity で表現するか別概念にするか
(「副産物: 暗黙則が不変条件になる」節)。

| 案 | 中身 | 代償 |
|---|---|---|
| (a) | v1 は identity だけ。 `related_jira_issues` は workspace 側の助言キーのまま daemon へ押さない | khi は当面 2 経路 (daemon の identity と自前の related 索引) を並べる |
| (b) | 最初から 2 概念を持つ (排他の identity / 非排他の reference で表を分ける) | 表が 2 本になり、 v1 の表面積が増える |

**決定 (2026-08-19、 nose): (a)** (→ I-8)。 I-1 の実需 (「Slack 起票の task に後から Jira の
コメントが来る」) は排他の identity だけで表現でき、 epic の重複参照は workspace 側が「無くても
害はない助言キー」と定義しているため。

**残る宿題**: (a) は **fold の一本化をその範囲だけ残す**。 決定 14 の決め手 (「2 つの fold を
並べると、 ロジックがズレたときに誰も気づかない」) に照らすと、 2 経路をいつまで並べるかの
条件は切っておきたい。 ただし残るのは索引であって fold ではない (`related_jira_issues` から
task を引く経路だけで、 state を畳んではいない) ので、 決定 14 が禁じた形そのものではない。
khi 側 memo と揃えて扱いを決める。

#### A-4. イベント push の冪等性 — per-event か差分 reconcile か  [B-2 をブロック] ✅

| 案 | 中身 | 代償 |
|---|---|---|
| (a) | v1 は差分 reconcile を続ける。 workspace が現況を読んでから差分だけ押す | **B-3 の read 口の要求が上がる** (下記) |
| (b) | per-event push にし、 イベント単位の冪等キーを op に足す | 冪等キーの設計が要る。 op の公開契約が変わる |

**決定 (2026-08-19、 nose): (a)** (→ I-9)。 現行の冪等性は workspace 側の reconcile (差分 push、
self-healing) が担保しており、 それをそのまま使える。 (b) にすると actions は append-only なので、
pump のクラッシュや再送で同じイベントが二重に積まれる。

**連動**: (a) を選んだことで **B-3 は「workspace スコープの一括読み」にする理由が立った**。
「押す前に現況と比べる」の比較先が自前の fold から daemon の読み戻しへ変わるので、 per-task の
口しか無いと read が 10 分ごとに task の数ぶん飛ぶ — B-3 の O(N) 問題そのものである。 B-3 の op
設計は per-task ではなく **since カーソル付きの一括読み**を既定とする。

#### A-5. `description` と action payload のサイズ上限  [B-2 をブロック] ✅

決定 3 の改訂で原文が `description` に載るようになり、 daemon が機械的に効かせられる開示の枠は
**サイズとフィールド粒度だけ**になった (「論点 e の enforcement は原理的に弱くなる」節)。
上限は後から足すと既存データが弾かれるので、 op の公開契約として最初に決める。

**切り詰めではなくエラーを推す**: head / agent 発の source は外部に正本が無く (「再取得できない
source がある」節)、 daemon の写しが唯一になる。 黙って切ると原文が壊れて復元できない。
エラーにすれば、 要約に落とすか分割するかを workspace 側が選べる。

**決定 (2026-08-19、 nose): 上限を置き、 超えたらエラー** (→ J-10)。 **値は PR-2 実装時に
実測して決めた: 64 KiB (65,536B)**。 上限が決まったことは論点 e の enforcement の限界
(「daemon が効かせられるのはサイズ / フィールド粒度まで」) を本編に書くときの材料にもなる。

**実測 (2026-08-19、 PR-2 実装時。 `internal/orchestrator/content_size.go` のコメントが
一次情報)**: `description` と action payload は書き込まれる内容の性質が違うため、 片方だけ
実測して両方に適用するのは筋が悪い。 **両方を別々に実測**した:

- description 側 — khi の note (取り込み済み 32 枚: jira 23 枚 / slack 8 枚 / frontmatter
  無しの手動メモ 1 枚) の本文バイト数分布:
  - jira (n=23): max 15,584B / median 1,259B / min 213B
  - slack (n=8): max 13,283B / median 6,302B / min 1,298B
  - 観測された最大は jira の 15,584B。 64 KiB はこれに対し **約 4.2 倍**のマージン
- action payload 側 — khi の claims / attrs データの実測:
  - claims JSONL の 1 レコード (n=109): max 5,537B / p95 2,571B / median 251B
  - 畳んだ attrs 全体・frontmatter 相当 (n=31): max 22,572B / median 1,039B
  - khi の `daemon_sync.py:497` (`send_action(task_id, "attrs_set", diff)`) は初回 push 時に
    desired_attrs 全体を 1 発の `attrs_set` payload として送るため、 単発 payload の現実的な
    最大は「畳んだ attrs 全体」の実測最大 22,572B。 64 KiB はこれに対し **約 2.9 倍**のマージン

**同じ 64 KiB を description / action payload の両方に適用してよい理由**: マージン倍率は
異なる (4.2 倍 vs 2.9 倍) が、 どちらも「1 回の書き込みで daemon に渡る自由記述/構造化
データの塊」という同じ形をしており、 フィールド粒度で別々の上限を持つほどの構造的な違いは
無い。 上限は「日常値を精密に予測する」ためのものではなく「無制限な肥大化を防ぐ安全弁」なので、
2 つの実測がどちらも同じオーダーに収まっている以上、 値を分けて管理するコストに見合わないと
判断した。 将来どちらかの分布が大きく変わったら (下記 mail 対応リスクと同様) この判断ごと
見直すこと。

未確定のまま残るリスク: khi 側は「mail を足したときに本文 + 添付テキストが載ることは未確定」
としている。 添付テキストの抽出結果は jira/slack の会話ログよりずっと大きくなり得るため、 mail
対応時にこの実測前提が破れる可能性がある。 そのときは新たに mail の分布を実測し、 この定数を
見直すこと — 黙って追認しない。

### 版の経緯

設計に唯一の正解は無く、 各版はその時点の前提での選択である。 後の版が前の版を否定している
のではなく、 **前提が増えたので選び直している**。 何を選び直したかと、 その理由を残す。

| 版 | 何を書いていたか | 次で選び直した点と理由 |
|---|---|---|
| 第 1 版 | claims / fold / cadence のうち汎用な部分を boid コアへ引き上げる | boid コアに既に fold があると分かった (事実確認)。 引き上げると三本目になるので、 向きを逆にした |
| 第 2 版 | 取り込み identity と、 未着イベント用の専用インボックス | 部品の話から入って全体像が見えないと指摘を受け、 原則と To-Be を先に立てる構成へ。 インボックスは、 篩いが workspace に残るなら要らないと分かり `captured` 着地に |
| 第 3 版 | daemon を判断のスケジューラとし、 `judge:` 予約 behavior 名前空間 + 述語 2 形 | `trigger` は task_behavior の性質ではないという指摘を受け、 トップレベルの `triggers` へ。 予約名前空間・ 述語・ probe・ 流量制御が不要になった |
| 第 4 版 (現在) | daemon は時計と single-flight だけを持ち、 走らせるのは workspace のスクリプト | — |

### 本編側の宿題

本 doc の決定が本編 (`cross-project-issue-triage.md`) の記述を書き換える箇所。 **いずれも本 doc の
実装をブロックしない**が、 放置すると本編と実装が食い違う。

#### 決定 16 の再定義 (I-7 の帰結)

I-7 で `tasks.ref` を子 dedup 専用にすると決めたので、 **root task は `ref` を持たなくなる**。
決定 16 は「`ref` = canonical source のキー」と書いているため、 「canonical source は **identity の
1 本目**」へ再定義する。 決定 16 の「後から変えられない」という性質は identity 側へ移り、
「1 identity は高々 1 task」(I-1) がその役割を引き継ぐ。 **PR-2 の後、 root の `ref` を空にする
migration と同じタイミング**で本編を直す。

#### 決定 3 改訂に伴うもの

決定 3 の改訂 (2026-08-19) は本編の 4 箇所にしか入れていない。 **旧決定 3 に依存した記述が本編に
多数残っている**ことと、 **改訂の中心論拠に反例クラスがある**ことが分かっているので、 両方を
ここに記録する。

改訂は本編の 4 箇所 (決定 3 の節・ 決定 8・ 非目的・ 第 13 版) にしか入れていない。 Fable の
レビューで、 **旧決定 3 に依存した記述が本編に多数残っている**ことと、 **改訂の中心論拠に
反例クラスがある**ことが分かった。 両方をここに記録する。

#### 再取得できない source がある (論拠の穴)

改訂の理由の 1 つは「本文は元システム (Jira / Slack / mail) から取り直せるので、 workspace 側の
写しは正本ではなくキャッシュ」だった。 **`source.type` には `head` と `agent` がある。**

- **head-capture** (nose が音声等で頭から直接入れた課題感、 UC-4) は外部に正本が無い。
  UC-4 は本文を workspace 側に着地させることで耐久性を担保していた
- **agent** 発のものも同様

note が退役すると、 これらの原文の**唯一の写しが daemon DB** になる。 「キャッシュにすぎない」
という論拠はこの class では成立せず、 決定 14 の受容代償を支えていた原則 2 (b)
(「render 済みの note が volume に残るので、 復元できなくても課題そのものは失われない」) の
fail-open も同時に崩れる。

**答え**: これらについては **daemon volume のバックアップを一次の耐久性とする**。 UC-4 は
既に「本文は nose 発なので daemon が一時保持しても compartment 問題は無い」としており、
compartment の観点では矛盾しない。 変わるのは「一時保持」が「正本」になる点なので、
決定 3 の節に明記し、 ストレージ表と原則 2 (b) も連動して直す (未確定)。

#### 本編に残る旧決定 3 の記述

以下は改訂と逆のことを現行文として言っている。 実装前の構想 doc なので全文改訂はしないが、
**少なくとも決定 3 の見出しと第 1 段落・ 決定 6・ UC-4・ 決定 14 の帰結**は直す必要がある
(未確定に置いた)。

| 箇所 | 残っている記述 |
|---|---|
| 決定 3 の見出しと第 1 段落 | 「本文は現地」「メール本文や課題の詳細は workspace 内に留まる」— **改訂注記の上に現行文として残っている** |
| 決定 14 の帰結 | 「**note は残す** (nose 判断): 本文は daemon 側に載らない (決定 3) ため」— 現行決定同士の正面矛盾 |
| 決定 6 | 「本文 (課題・ テーマ文書) はファイル」「card はファイル本文への `content_ref` を持つ」 |
| 概観 / 用語表 / 信頼境界表 / スキーマ案 | 「本文を含まない」「本文は各 workspace セッション経由」 |
| UC-1 / UC-3 / UC-4 | 「本文を `issues/` に保存」「本文は workspace に着地し daemon 側は card 粒度に戻る」 |
| ストレージ表 / 検証シナリオ S1 | 「課題・ テーマ本文 (note) — $HOME workspace volume」「daemon には流れない」 |

「本編で唯一の決定変更は決定 3」という第 13 版の書き方も、 **決定 14 の帰結の一部 (note の
存続と `project_card.py` の役割) が連動して変わる**以上、 正直ではない。 そこも直す。

#### 決定 4 (prompt injection) の再導出

改訂前は、 汚染された原文は workspace 内の note に留まり、 daemon が運ぶのは triage LLM が
書いた summary / spec (= 一段濾過済み) だった。 改訂後は **汚染された生の本文が daemon の
第一級 artifact (`description`) として、 Web UI・ 整形セッションの payload・ 実行 task の文脈へ
配送される**。

決定 4 の結論 (「最悪は誤った提案が並ぶだけで、 Go で止まる」) 自体は成立すると考えるが、
論証は旧前提の上に立ったままである。 とくに **未確定リスト自身が「本文は既定で畳んでおく」を
提案している**ので、 「人が読まない場所に payload を隠したまま Go し、 Go 後にそれが実行 agent の
一次入力になる」構図が生まれる。 従来も agent は credential で Jira / mail を直接読めたので
純増ではないが、 **「既定で畳む」と Go の判断材料はトレードオフ**であることを含めて、 決定 4 を
再導出して書く (未確定)。

#### 論点 e の enforcement は原理的に弱くなる

旧決定 3 のハードな線は「本文を運ぶフィールドが存在しない」という**構造的**な enforcement
だった。 改訂後、 daemon は opaque なテキストから「これは要約か原文か」を機械的に判別できない
(逆輸入 3 の closed set を守る限り意味検査はできない)。 つまり上限を論点 e に一本化した瞬間、
enforcement の実体は **workspace 側の自制 + せいぜいサイズ上限**に落ちる。 「新しい workspace の
既定は保守的側」も、 押してくるのは workspace 自身なので daemon 側では強制できない。

これは受け入れてよい弱まり方だと考えるが、 **論点 e に正直に書く**必要がある。 併せて
daemon が機械的に効かせられる唯一の枠として、 `description` と action payload の
**サイズ上限**は決めておく (未確定)。

#### honeypot 評価の格上げ

本編の信頼境界は「card だけでも 1 箇所に集まれば honeypot 性はある。 これは現行の task
タイトルの daemon 集中と**同レベル**であり受容する」と書いている。 改訂後の daemon DB は
全 workspace の**生本文**を持つ (決定 3 改自身が「認証情報が貼られていることがある」と認めて
いる) ので、 **「タイトル集中と同レベル」はもう成り立たない**。 デバイス認証 1 枚の突破・
端末盗難で全 workspace の本文が読める、 が新しい残余リスクである。 受容自体は妥当かも
しれないが、 評価の記述を格上げする (未確定)。

#### 決定 3 改訂の残り (本編側・ doc 作業)

実装をブロックしない。 サイズ上限だけ A-5 へ昇格させた。

- **本編の旧決定 3 記述の棚卸し**。 最低でも決定 3 の見出しと第 1 段落・ 決定 6・ UC-4・
  決定 14 の帰結。 残りは一括注記でもよい
- **再取得できない source (head / agent) の耐久性**。 daemon volume backup を一次とする、 で
  よければストレージ表と原則 2 (b) を連動改訂する
- **決定 4 の再導出**。 汚染原文が Go 後の実行文脈へ届く構図と、 「既定で畳む」と Go の判断
  材料のトレードオフ
- **論点 e の enforcement の限界を明記**。 daemon が機械的に効かせられるのはサイズ / フィールド
  粒度まで
- **信頼境界の honeypot 評価の格上げ**
- **非目的の「daemon 内 scheduler」と論点 b の更新**。 論点 k で方針が変わったのに、 非目的
  リストは daemon 内 scheduler を非目的のまま残しており、 同一 doc 内に現行方針が 2 つある
- **論点 g への前方参照**。 「dedup は同一 source ref の再 push を update 扱い」のままなので、
  identity 索引に置き換わる旨の注記が要る
