# boid 内部シグナルの inbox 統合

2026-08-28 起草。`docs/plans/signal-driven-review.md` §12 に未決として積まれた 1 行に
答える doc。

> boid 内部シグナル (action 列) の経路 — inbox へ統合するか、現行どおり workspace の
> scan script が `boid action list` を直読みし続けるか
> (初期実装は後者を推す。`signal-envelope-inventory.md` §6)

**結論: 統合する。** 根拠は行数削減ではなく、現行が
`signal-driven-review.md` §3 の責務境界の表と矛盾していること、およびメタプロジェクトが
2 個目に増える時点でその矛盾が複製されることにある。

---

## 1. ゴール

1. **責務境界を実装に一致させる。** `signal-driven-review.md` §3 の表は cursor・dedup・attempts を core の
   持ち物と定義している。現行は workspace (khi) がそれを持っている
2. **メタプロジェクトが 2 個目に増えるときに複製される機構をゼロにする**
3. workspace 側の検知コードを「篩と判断」だけに縮める

### 非ゴール

- **人が後から「あの巡は何をしたか」を追えなくすること** —— `progress` に残すのは続ける。
  ただし**機械可読な記録の書式** (`khi_record` attrs / `khi-record v1 ` prefix) は用途ごと
  無くなる (§4.7・§5.2)
- khi の**判断の意味論** (verb の語彙・`khi-sweep` の判断方針) の変更 —— ただし
  `write.py` から記録の自動付与は消える (§5.3)
- 外部 connector (slack/mentions ほか 2 本) の変更

---

## 2. As Is — 現行 khi の検知構造

2026-08-27 のカットオーバー後、khi は 2 本の読みを持つ。

| 読み | 何を拾うか | 誰が attempts を持つか | 誰が栞を持つか |
|---|---|---|---|
| `boid signal list --state pending` | 外部シグナル (slack/jira/bitbucket) | **khi** (`--claim` を使わない) | 栞なし (pending が毎回返る) |
| `boid action list --since <cursor>` | boid 内部シグナル **と記録** | **khi** | **khi** (`domain/cursor.py`) |

### 2.1 action 列は khi にとって 2 役を兼ねている

`khi/domain/boid/action.py` の冒頭がそう書いている — 1 回の読みで 2 つを拾う。

- **シグナル** — この件について考え直す理由 (子の終端、人の書き込み)
- **記録** — 前の巡が何を処理したか (`attrs_set` / `progress` の payload)

**そして外部シグナルの決着記録も同じ action 列に載る。** slack の event を処理した
記録は、sweep task の `progress` action として boid 側に書かれる
(`khi/app/detect.py:145` のコメントが実例つきで説明している)。

### 2.2 だから栞と attempts が複雑になっている

この 2 役の兼務が、khi の 2 つの機構の形を決めている。

- **`domain/cursor.py` の `Scan.requires`** — 各栞の位置に「そこを通過するために settled
  であるべき event_key 群」を持たせ、未決着の記録を含む action の手前で止める。これが
  無いと栞が記録を跨いで進み、attempts が 0 に巻き戻る
- **`domain/attempts.py`** — ローカルにカウンタを置かず、毎巡 action 列の記録から数え直す
  (S-5)。これが成立するのは上の記録窓が保たれているから

つまり **cursor と attempts の複雑さの出どころは「シグナル」ではなく「記録」の側**。
統合の判断は「シグナルをどこから読むか」ではなく、**処理済みの印をどこに置くか**が本体
になる。

### 2.3 絞りは 2 段あり、それぞれ別のものを守っている

`khi/domain/screen.py` の `is_signal` (S-9) が、action を 2 つの軸で絞る。

| 軸 | 判定 | 何を守っているか |
|---|---|---|
| **対象軸** | 「まだ書ける status の card」に載っている action だけ通す | **card 宛だけに絞る** (+ 印を書けない card を弾く) |
| **actor 軸** | khi 自身 (sweep task / trigger の exec job) の書き込みを落とす | **自己参照による無限自走の遮断** |

#### 対象軸が守っているもの

主目的は **card 宛の action だけ通す**こと。`boid action list` は workspace の**全 task** の
action を返すので、これが無いと `api_gateway_request` のような大量の行が流れ込む
(LLM task 1 回で 100 行超)。

もう 1 つが「印を書けない card を弾く」。**現行の khi は処理済みの印を signal 側ではなく
card 自身に書く** —— `attrs_set` で attrs (`khi_record`) へ、`progress` で timeline へ。khi は
ローカル状態を cursor しか持たない (S-5) ので、印の置き場が boid 側の card しかないから
である。だから **印を受け付けない status の card 宛の action を通すと、記録が残らず次の巡で
同じ action をまた拾う** —— 永久に再検知し続ける。

(`is_signal` の docstring が「`aborted` の task には `attrs_set` も `noted` も打てないので、
通すと記録が残せず永久に再検知する」と書いているのがこれ。同 docstring が「card 機械 v2 は
この status に到達しない」とも書いているとおり実害は理論上のものだが、フィルタの設計理由と
してはここに置かれている。)

#### actor 軸が守っているもの —— 自己参照のループ

sweep task が card に書く → その書き込みが action として残る → その action がシグナルに
なる → trigger が次の sweep を起こす → card を読んで、また書く。**外部に何も起きていなくても
回り続ける。** `screen.py` の docstring がこれを「素通しにすると、khi 自身が起こした sweep
task の完了がシグナルになり、10 分後の trigger が次の sweep を起こす無限自走になる」と
書いている。

止め方は actor を見て落とすこと:

| actor | khi の扱い | 根拠 |
|---|---|---|
| `human` / `daemon` | 通す | 人と daemon の書き込みは考え直す理由になる |
| `task:<id>` | その task を引き、behavior が `sweep` なら落とす | 自分が起こした task の id を覚えていない (S-5) ので、毎巡引き直すしかない |
| `task:` (id 空) | **決め打ちで落とす** | exec job には task が無く `ActorTask("")` になる。それを「workspace の trigger job = 自分」と見なしている |

**この 2 行が、統合で消える側と core へ移る側の境目になる** (§4.3)。

### 2.4 責務境界の表との照合

`signal-driven-review.md:113` の表:

| 観点 | 所有者 |
|---|---|
| connector 実行、**inbox、cursor、dedup**、trigger | **boid core** |
| 篩・batch 化・判断タスクの起こし方 | workspace (メタプロジェクト) |
| task 状態・identity・履歴の正 | boid core |

現行の khi を並べる (行数は実測、`+` の後はテスト):

| khi のモジュール | 行数 | やっていること | 表での所有者 |
|---|---|---|---|
| `domain/cursor.py` | 117 + 150 | 栞の前進条件 | **core** |
| `adapters/file_cursor.py` | 57 + 73 | 栞の永続化 | **core** |
| `domain/attempts.py` | 72 + 105 | 試行回数と dead letter | **core** |
| `domain/boid/action.py` | 135 + 139 | action → 検知の言葉 | (読み口) |
| **合計** | **381 + 467 = 848** | | |

**workspace が core の仕事を 848 行ぶん持っている。**

極めつけは `khi/adapters/inbox.py:12` と `khi/adapters/boid_store.py:378`。外部シグナルは
既に core の inbox にいるのに、khi は `--claim` を使わず自前の attempts で数え直している。
理由として書かれているのは「khi 既存の attempts 機構を引き続き使う」だけで、**なぜそちらが
良いかは書かれていない**。実際の理由は 2.2 — 記録が action 列にあるので、そこから数え直す
方が一貫していた。記録の置き場が変われば、この理由は消える。

### 2.5 「core の変更ゼロ」が計上していないもの

`signal-envelope-inventory.md` §6 が (b) を推した根拠は 1 行:

> 初期実装は (b) を推す — 今日動いている形のままで、core の変更ゼロ。

これは**副産物の発見として積まれた据え置き**であって、必要性を検討した結論ではない
(同 §6 の見出しがそう明示している)。そして「変更ゼロ」が指しているのは core 側のコストだけ
で、**workspace 側に 848 行の core 相当機構が残り続けるコストは計上されていない**。

据え置きとしては正しかった — 当時メタプロジェクトは 1 個で、複製のコストは発生していな
かった。**その前提がこれから変わる**、というのが次節。

---

## 3. なぜ今か

boid には現在メタプロジェクトが 2 つある。

| project | workspace | card パイプライン |
|---|---|---|
| `khi-task-collector` | khi | 稼働中 (2026-08-27 カットオーバー完了) |
| `nvt-tasks` | default | **持たない** |

`nvt-tasks` の `.boid/project.yaml` の冒頭コメントに、持たない理由が書いてある:

> khi ワークスペースの khi-task-collector と同じ位置づけだが、あちらが持つ card
> パイプライン (poll/observe/evaluate の自動サイクル) はまだ持たない。
> khi-task-collector が十分安定するまで統合しない方針のため。

その条件は満たされた。よって `nvt-tasks` に card パイプラインを載せる作業が次に来る。
このとき **§5 で数える 3,566 行が、そのまま 2 個目のメタプロジェクトへ複製される**
(§2.4 の 848 行はそのうち「責務境界の表が core と定義している」ぶんだけを数えた値)。

`signal-driven-review.md` §12 の未決リストには、もう 1 行こう書いてある:

> scan script の定型を組み込みスキル/テンプレとして配布するか

複製を人手でやる代わりにスキルで雛形化する、という話である。**統合しないまま雛形化すると、
スキルは責務境界違反を量産する道具になる。** 統合はスキル化の前提条件であって、
順番を入れ替えられない。

---

## 4. To-Be

### 4.1 全体像

```text
外部サービス ──[Pack connector]──▶ ┐
                                    ├──▶ Signal inbox (core) ──▶ trigger (on: signals)
boid 自身の action ──[core が直接]──▶ ┘                              │
                                                                     ▼
                                          メタプロジェクトの判断タスク
                                            読み: boid signal list 1 本
                                            書き: 既存の boid コマンド
                                            篩:  workspace 固有のノイズ判定のみ
```

変わるのは **左下の 1 本**と、**判断タスクの読みが 2 本から 1 本になること**。

### 4.2 絞りの分担 —— 2 段をそれぞれ誰が持つか

§2.3 の 2 段の絞りを、統合後は誰が持つか。ここが本設計の主要論点。

#### 篩は workspace、という原則との整合

`signal-driven-review.md` §8.4 が篩を workspace の責務としたのは理由つきである:

> 明らかなゴミ (mail 等の低 S/N source) は script が ack して落とす。**ノイズの質は
> source と workspace ごとに違うため**、篩は core でなく workspace の責務とする

**S-9 の 2 段はこの理由に当てはまらない。** 「自分の書き込みを自分で拾って無限に自走する」
のを止めるのも、「card 宛かどうか」を決めるのも、ノイズの質の問題ではない。どのメタ
プロジェクトでも同じ形になる —— むしろこれこそ §3 で複製される代表格である。

#### 対象軸 —— 「弾かないと壊れる」理由が消える

**処理済みの印が ack になれば (§4.4)、印は signal 行に付く。** 書き込み先の card の status に
一切依存しなくなるので、§2.3 の 2 つめ (印を書けない card を弾く) の理由が消える。

**残るのは「card 宛だけ通す」だけ。** そしてそれは篩ではなく **ingest の範囲**の話である
—— 外部 connector が「自分宛のメンションだけ取る」「アサインされた課題だけ取る」と決めて
いるのと同じレイヤの判断なので、core が持つ。範囲を「card 型 task 宛の action」に置けば、
`api_gateway_request` のような雑音は構造的に inbox へ入らない。

#### actor 軸 —— core の方が正確に判定できる

§2.3 の表の下 2 行 (毎巡の引き直しと、`task:` の決め打ち) が**両方消える**。理由と判定
ロジックは §4.3。

#### 決定

| 軸 | 統合後の持ち主 |
|---|---|
| **actor 軸** (自己参照の遮断) | **core** —— ingest 時に落とす |
| 対象軸 (card 宛かどうか) | **core** —— ingest の範囲として |
| ノイズ判定 (`self_authored` 等) | workspace —— 現行のまま `screen.py` に残る |

### 4.3 自己参照の遮断

**この設計で一番壊れやすいのがここ。** 他の部分は誤ると取りこぼす (次の巡で回復する) が、
ここを誤ると止まらなくなる。**ループの機序と現行 khi の止め方は §2.3 にある** —— ここでは
統合が何を変えるかだけを書く。

#### 統合が変えるのは代償の大きさ

ループそのものは統合で新しく生まれる危険ではない (§2.3)。変わるのは、遮断が効いていな
かったときに何を失うかである。

| | 現行 (schedule trigger) | 統合後 (`on: signals`) |
|---|---|---|
| 遮断が効いていないとき | trigger の exec job は毎巡走るが、対象ゼロなら sweep task を立てずに終わる | **pending signal が残っている限り毎巡発火し、sweep task (LLM) が立つ** |
| コスト | exec job 1 本 | **LLM 1 巡ぶん、永久に** |

`on: signals` の発火は `every` が下限なので暴走はしない。**止まらないだけである。**

#### core の止め方

**core は書き込み元を actor 文字列から推測しなくてよい。** sandbox から来る書き込みは
`internal/server/boid_executor.go` が `TokenContext` を持って処理しており、
`TokenContext.ProjectID` (`internal/sandbox/protocol.go:498`) がその書き込みを出した
project そのものだからである。

| 書き込み元 | ingest するか |
|---|---|
| `TokenContext.ProjectID` が `signals.sources[]` を宣言している project | **しない** (自己参照) |
| `TokenContext.ProjectID` がそれ以外の project | する |
| `human` / `daemon` (TokenContext を持たない) | する |

判定材料はこの 1 つで足り、メタプロジェクトとは `signals.sources[]` を宣言している
project のことなので、これ以上の宣言も要らない。複数の project が宣言していても、その
全部を落とせば閉じる。

これで §2.3 の表の下 2 行が両方消える:

- **引き直しが要らない** —— khi は actor の task id から behavior を毎巡引いている
- **決め打ちが要らない** —— ここが決定的。`task:` (id 空) は「exec job が書いた」しか
  意味せず、**どの project の exec job かは actor 文字列から永久に分からない**。khi が
  決め打ちしているのは、task が無い以上そこから引けるものが何も無いからである。
  **`TokenContext.ProjectID` は `TaskID` が空の exec job でも埋まっている** ので、
  core 側にはその行き止まりが無い

#### 判定できないときは落とす (fail-close)

`TokenContext` を持つ書き込みで `ProjectID` が解決できなかったときは **ingest しない**。
誤りの代償が非対称だから:

- **通す側の誤りはループに倒れる。** 症状 (毎巡 sweep が起きる) から原因へ辿るのが難しく、
  辿っている間も LLM のコストを払い続ける
- **落とす側の誤りは次の action で回復する。** その card に次の動きがあれば、そのときの
  signal で拾える

### 4.4 処理済みの印を ack に一本化する

現行、khi が「もう見た」と判断する根拠は 2 系統ある。

| シグナルの出どころ | 処理済みの印 |
|---|---|
| 外部 (inbox) | `boid signal ack` **と** 記録 (attempts を数えるため) |
| boid 内部 (action 列) | 記録のみ |

統合後は **ack 1 本**にする。

- **attempts / dead letter は core の機構に載せる** — `boid signal list --claim` が
  attempts を増やし、上限超過で dead になる (`MaxSignalAttempts`)
- **記録から数え直す機構は不要になる** — よって `domain/attempts.py` と、それを成立させる
  ための `domain/cursor.py` の `requires` 契約が両方消える
- **栞そのものが不要になる** — pending が毎回返る。内部 signal も外部と同じ扱い

#### attempts 上限の食い違い (要決定)

core は `MaxSignalAttempts = 5` (`internal/orchestrator/signal_store.go`)、khi は
`MAX_ATTEMPTS = 3` (`khi/domain/attempts.py`)。統合すると 5 になる。

3 → 5 は「諦めるまでに 20 分ぶん長く粘る」だけで、dead になった signal が消えるわけでは
ない (`--state dead` で見え、人が ack できる)。**core 側に揃える**のを推す — メタ
プロジェクトごとに上限を変える必要が実証されていない。

### 4.5 ingest の起こし方 (実装方式)

2 案ある。

| 案 | 形 | cursor | 取りこぼし | 発火の速さ |
|---|---|---|---|---|
| **(i) 書き込み時** | `orchestrator.CreateAction` の中で同一 tx で ingest | **不要** | 構造的にゼロ (同一 tx) | action が立った瞬間 |
| (ii) 定期 derive | daemon が周期的に action 列を舐めて ingest | **必要** | cursor の正しさ次第 | 周期ぶん遅れる |

**(i) を推す。** 理由は 3 つ。

1. **cursor が要らない。** (ii) は khi の栞問題を core へ移すだけで、§1 のゴール 1 は
   達成できても機構の総量は減らない
2. **dedup が PRIMARY KEY で効く。** action は不変で id を持つので、
   `(workspace_id, service, connector, id)` の `INSERT OR IGNORE` にそのまま乗る
   (`signal_store.go` の実装済みの不変条件)
3. **`on: signals` trigger が即発火できる。** 現行 khi は `every: 10m` の素の schedule で
   回っており、`on: signals` (#1010) を使えていない — boid 内部の変化が inbox に無いため
   使うと取りこぼすからである。統合するとこの制約が消え、**子 task が終わった瞬間に
   sweep が起きる**

#### 書き込み口は 2 本ある — 片方は範囲外だが、確認した上で外すこと

`actions` テーブルへの `INSERT` は 2 箇所にある。

| 場所 | 何を書くか | 内部シグナルとして |
|---|---|---|
| `internal/orchestrator/store.go:689` `CreateAction` | 通常の action 全部 (呼び出し元 25) | **ここに入れる** |
| `internal/dispatcher/store.go:229` | daemon 再起動時、実行中 task を `aborted` に倒す `abort` action | **範囲外** |

後者が範囲外なのは、書く相手が**実行中だった exec task** であって card ではないため
(§4.2 の対象軸で落ちる)。card が dispatch されることは無いので、この経路に card 宛の
action は流れない。

**ただし「範囲外」で済ませずに、1 つ確認すること** — daemon 再起動で子 task が
`aborted` になったとき、親 card に `child_closed` が立つか。立つなら `CreateAction`
経由なので拾える。立たないなら khi は子の終端を見逃すが、**それは統合とは独立に現行から
存在する欠けである**。統合で直すのではなく、別件として切り出す (直したつもりで直って
いない、が一番まずい)。

### 4.6 envelope への写像

`signal-envelope-inventory.md` §5 の schema v0 に載せる。

| envelope | 内部シグナルでの値 | 備考 |
|---|---|---|
| `id` | action の id | action は不変なので dedup が効く |
| `occurred_at` | action の `created_at` | |
| `source.pack` | `boid` | 予約名。Pack loader が同名の外部 Pack を読み込むことを禁じる |
| `source.connector` | `actions` | |
| `source.service` | (空) | 外部サービスへ到達しないため |
| `identity` | 対象 card の task id | 現行 khi の Target と同じ指し方 |
| `url` | Web UI の card 詳細 URL | 任意。判断が原文へ跳ぶ入口 |
| `author` | action の actor (`human` / `daemon` / `task:<id>`) | workspace の篩の材料 |
| `title` | action の type | 篩の材料。判断は依存しない |

`identity` が task id そのものである点は外部シグナル (`jira:ROOKPF-309` 等) と形が違うが、
identity は「workspace 全体の共有語彙」(§3.3) であり pack-scope に閉じない、という既存の
契約の範囲内である。判断側は現行どおり task id を受け取る。

### 4.7 記録に残る役割

現行の「記録」は 3 つの仕事を兼ねている。**印が ack になると、残るのは 1 つだけ。**

| 記録の役割 | 統合後 |
|---|---|
| 次の巡が「もう見た」と分かる根拠 | **消える** —— ack が担う |
| 試行回数を数える材料 | **消える** —— core の attempts が担う |
| 人が後から「あの巡は何をしたか」を追う監査ログ | **残る** —— ただし形が変わる (下記) |
| card の summary / attrs そのもの | 残る (これは記録ではなく**状態**) |

**残る監査ログは、機械可読である必要が無くなる。** 現行の記録は
`khi/domain/record.py` が `khi_record` attrs と `khi-record v1 ` prefix 付き progress として
**次の巡の自分が読み返すために**構造化している。読み返す相手が消えれば、**書式も消える**
—— 人が読む progress を `boid task notify --progress` で残せば足りる。

これが §5.2 で `domain/record.py` (327 + 369) を「decode 側だけ縮小」ではなく丸ごと
消える側に置いた理由である。

---

## 5. 統合後、workspace 側に何が残るか

**数え方を変えた。** 初版は現行のモジュールを 1 つずつ見て「消える / 縮小」を付けていたが、
それだと**現行の構成そのものが暗黙の前提として残る**。実際それで `app/trigger.py` (498 行)
が表から丸ごと抜けていた —— 「trigger は残るもの」と置いていたからである。ここでは
**ゼロから組んだら何が要るか**を先に決め、現行をそこへ写像する。

### 5.1 統合後の sweep に要る機能

| 機能 | 実体 | 現在の置き場 |
|---|---|---|
| signal を読む | `boid signal list --claim` 1 発 | `adapters/inbox.py` |
| identity で集約する | 対象 1 件 = card 1 枚に畳む | `app/trigger.py` の `merge_targets` |
| 自分宛でないノイズを落とす | `self_authored` 等、**workspace ごとに違う篩** | `domain/screen.py` の一部 |
| 対象一覧を組む | subagent へ渡す形にする | `app/trigger.py` の `instruction` |
| 判断する | LLM | `.claude/skills/khi-sweep/SKILL.md` |
| boid へ書く | verb → boid コマンド | `app/write.py` |
| ack する | `boid signal ack` 1 発 | `adapters/inbox.py` |

**この一覧に「栞」「試行回数」「処理済みの印」「action 列を読む」が無い。** それが統合の
中身である。

### 5.2 丸ごと消えるモジュール

| モジュール | 本体 | テスト | なぜ消えるか |
|---|---|---|---|
| `app/trigger.py` | 498 | 824 | 5 つの仕事のうち 4 つ (並走防止・検知・記録・栞) が core へ移り、残る「sweep を起こす」は trigger の `run:` 数行になる |
| `domain/record.py` | 327 | 369 | **「処理済みの印」そのもののモジュール。** encode の呼び出し元は write と trigger、decode の呼び出し元は `boid/action.py` だけ。印が ack になれば全部消える |
| `adapters/inbox.py` | 196 | 292 | envelope → `Signal` のマッピングが要らなくなる (下記)。読みと ack は CLI 2 発 |
| `domain/boid/action.py` | 135 | 139 | action 列を読まない |
| `domain/cursor.py` | 117 | 150 | 栞が無くなる |
| `domain/signal.py` | 117 | 95 | `mark_of`/`time_of_mark` は栞用。`Signal` 型の source-prefix 不変条件も、envelope の id をそのまま使えば不要 |
| `domain/attempts.py` | 72 | 105 | core の attempts / dead へ |
| `adapters/file_cursor.py` | 57 | 73 | 栞の永続化 |
| **合計** | **1,519** | **2,047** | **3,566 行** |

初版が挙げていた 848 行の **4.2 倍**である。差は「現行のモジュールを出発点にしたか、
要る機能を出発点にしたか」だけで、統合の中身は何も変えていない。

### 5.3 大きく縮むモジュール

| モジュール | 本体 | テスト | 残るもの |
|---|---|---|---|
| `app/detect.py` | 375 | 609 | `plan_boid` (action 列の計画) が消える。`plan_candidates` の篩は残る |
| `domain/screen.py` | 138 | 181 | `is_signal` (S-9 の 2 段) が core へ。`self_authored` は workspace に残る |
| `adapters/boid_store.py` | 522 | 653 | `list_actions` と action 系が消える |
| `app/write.py` | 1008 | 1515 | verb の意味論は変わらないが、**記録の自動付与 (`encode_attrs`/`encode_progress`) が消える** |

### 5.4 別扱いだが、ゼロベースなら消える

| モジュール | 本体 | テスト | 行き先 |
|---|---|---|---|
| `domain/childid.py` | 115 | 146 | `boid task create --idempotency-key` (#1012) と同じ仕事。初版は「独立した論点」として §8 に逃がしていたが、ゼロから組むなら子 id を自前で採番する理由が無い |

### 5.5 消えるものの質

行数より重要なのはここ。栞と attempts は khi で最も壊れやすかった機構で、運用で踏んだ
バグはここから出ている —— 「栞が自分自身を越えられない再検知ループ」「栞が assigned の
まま settle せず deadlock」の 2 件は `signal-driven-review.md:428` が inbox の不変条件の
設計根拠として引いているものであり、**core の inbox はその再発を構造的に防ぐ形で既に
作られている**。統合とは、その防御を内部シグナルにも適用することに他ならない。

### 5.6 前提の見直し — 初版が現状に引っ張られていた 4 点

| # | 初版の前提 | 実際 |
|---|---|---|
| 1 | `app/trigger.py` は残る (表に載せてすらいなかった) | 役割の 4/5 が core へ移り、残りは trigger の `run:` 数行 |
| 2 | `domain/record.py` は decode 側だけ消える | encode 側も消える —— 記録は「処理済みの印」そのもので、印が ack になれば書く理由が無い |
| 3 | `domain/signal.py` の `Signal` 型は残る | envelope をそのまま扱えば要らない。`inbox.py` が id に prefix を補っているハックも、この型の不変条件のためだけに存在する |
| 4 | `domain/childid.py` は独立した論点 | `--idempotency-key` と同じ仕事。ゼロベースなら残す理由が無い |
| 5 | 記録は「decode 側だけ消える」 | 読み返す相手 (次の巡の自分) が消えれば**書式そのものが要らない**。encode 側も消える (§4.7) |

**5 つとも同じ誤り方をしている** —— 現行のモジュールを 1 つずつ見て「これは残るか」と
問うたので、**モジュールが存在すること自体が残す理由**になっていた。要る機能から数え直すと
逆になる。

## 6. 先に決めること、実装の勘所

### 6.1 trigger.py を消すと決まらないもの

`app/trigger.py` の 5 つの仕事のうち 4 つは行き先が決まった (§5.2) が、**残る 1 つと、
消える仕事の代わりが決まっていない**。実装に入る前にここを埋める。

| 問い | 現行の答え | 統合後の候補 |
|---|---|---|
| **生きている sweep があるときに 2 枚目を立てない**のは誰か | `trigger.py` の `run_once` が先頭で `boid` に問い合わせて何もせず終わる | `on: signals` の single-flight は **trigger の重複**しか防がない —— sweep task が `every` を超えて生きていると次の発火が 2 枚目を立てうる。`boid task create --idempotency-key` (#1012) で畳むか、trigger の `run:` で 1 回問い合わせるか |
| **誰がいつ ack するか** | sweep が記録を card に書く → **次の巡の** trigger が記録を読んで settled を判定 → ack。1 巡またぐのは記録が「完了の証拠」だから | sweep が**判断を書いた直後に自分で** ack する。**順序を逆にしてはいけない** —— ack を先に打つと、crash した signal が pending から消えたまま誰も処理していない状態になる |
| **対象一覧を誰が組むか** | trigger が `instruction` に埋めて sweep に渡す (daemon 側から workspace の手順を指示するとバグる、の分担) | sweep task 自身が `boid signal list --claim` を読んで組む。trigger の `run:` は sweep を起こすだけになる |

3 つとも「trigger job (LLM なし) と sweep task (LLM あり) の分担をどう引き直すか」という
1 つの問いの側面である。

### 6.2 実装の勘所

- **2 本の書き込み口を両方塞ぐ** (§4.5)。片方だけは無音の取りこぼしになる
- **書き込み元 project を `CreateAction` まで運ぶ経路が要る。** §4.3 の判定材料は
  `TokenContext.ProjectID` だが、`CreateAction(dbtx, *Action)` はそれを受け取らないし、
  `Action.Actor` にも載らない (`task:` は project を持たない)。`orchestrator.WithActor` が
  actor を context で運んでいるのと同じ形にするか、シグネチャに足すかは実装時に決める。
  **25 ある呼び出し元のうち project を知らないものが残れば、そこは判定不能 = fail-close の
  対象**になる —— どれが該当するかを実装の最初に洗い出すこと
- **ingest 失敗で action の書き込みを巻き戻さない。** 同一 tx に載せるが、signal の
  INSERT 失敗が card の状態更新を巻き戻すのは因果が逆。ingest 側のエラーはログに残して
  action は成立させる (取りこぼした signal は次に同じ card が動いたときの signal で
  カバーされる)
- **`signals` を宣言した project が居ない workspace では一切 ingest しない。** ingest 先が
  存在しないので、判定するまでもなく何も起きない。default workspace が `nvt-tasks` に
  `signals.sources[]` を入れるまで、そちらの挙動は 1 ビットも変わらない。
  **逆に、既に宣言を持つ workspace (khi) では core を入れた時点から内部 signal が入り
  始める** —— これは §7 の並走そのものなので、意図された挙動である
- **GC は既存の 30 日ルールに乗せる** (`internal/api/gc.go`)。内部 signal 専用の保持期間を
  作らない

---

## 7. 移行

khi は本番稼働中なので、切り替えは並走で検証してから行う。shadow-a
(`signal-driven-review.md` §10.3) と同じ形を踏襲する。

1. **core 側を入れる** (PR-1)。**khi は既に `signals.sources[]` を宣言しているので、
   この時点から内部 signal が inbox に入り始める。** khi はまだ読まないので pending の
   まま溜まる (内部シグナル用のスイッチは無いので、「入れるが流さない」段階は存在しない)。
   **ロールバックの口は boid のデプロイを戻すこと** —— project.yaml 側にスイッチは無い
2. **khi で並走観測** (PR-2)。現行どおり action 列を直読みしたまま、**inbox の内部
   signal 列と自分の検知結果を突き合わせてログに出す** (ack はしない)。ここで等価性を
   採点する (§10 グループ C)
3. **切り替え直前に、溜まった内部 signal を一括 ack する。** 並走中に入った分は khi が
   action 列経由で**既に処理済み**なので、そのまま一本読みへ移ると過去の判断をやり直す
   ことになる。`boid signal list --source boid/actions` で拾って ack する。
   (並走中の pending は GC の対象外 —— `GCSignals` は acked と dead しか消さない
   ので、この一括 ack が唯一の回収経路になる)
4. **khi を inbox 一本読みに切り替える** (PR-3)。action 列の直読みをやめ、`--claim` を使う
5. **消す** (PR-4)。§5.2 の 3,566 行を撤去し、§5.3 の 4 モジュールを縮める
6. **`nvt-tasks` に載せる** — ここで初めて 2 個目のメタプロジェクトが立ち上がる。
   §3 の「複製」は発生しない

---

## 8. この doc が扱わないもの

- **khi の死にコード** — `adapters/{slack,jira,bitbucket,gateway}.py` (711) と
  `ports/source.py` (29)、`app/trigger.py` の `default_sources()`/`collect()`、および
  対応テスト 820 行。2026-08-27 カットオーバーでロールバック用に残置されたもので、
  呼び出し元が存在しない (grep で確認済み)。**本 doc の統合とは独立に削除してよい**
- **メタプロジェクト作成スキル** — §3 で統合の動機として参照しているが、設計は統合の
  完了後に行う (`signal-driven-review.md` §12「scan script の定型を組み込みスキル/
  テンプレとして配布するか」)
- **1 workspace に複数のメタプロジェクトが居るときの inbox の取り合い** — `SignalFilter` にも `AckSignals` にも project の概念が無いので、複数居ると
  同じ pending 列を読み合って先に ack した方が勝つ。**外部シグナルで既に成立している
  穴**であり、内部シグナルを足しても新しく生まれるものではない。解くなら inbox に
  「誰宛か」を持たせる話になり、それは signals 機構全体の設計変更なので本 doc の外

---

## 9. PR 分割

| PR | 側 | 内容 | 採点 |
|---|---|---|---|
| PR-1 | core | `CreateAction` での ingest。actor 軸の遮断、card 宛への範囲限定、envelope 写像、`pack: boid` の予約 | Q5-Q12 |
| PR-2 | khi | 並走で等価性を観測 (ack しない) | Q13-Q15 |
| PR-3 | khi | 一括 ack のうえ inbox 一本読みへ切り替え。`--claim` を使う。`on: signals` trigger へ | Q16-Q19 |
| PR-4 | khi | §5.2 の 3,566 行を撤去し、§5.3 を縮める | Q20-Q21 |
| (独立) | khi | §8 の死にコード 1,600 行を削除 | — |

---

## 10. 採点表 — レビュワー用 yes/no 判定リスト

**極性は yes = 合格に統一。根拠を diff / 実データから引けない yes は no として扱う。**

### A. 前提 (この doc 自体の採点)

| # | 問い |
|---|---|
| Q1 | §2.4 の表の行数は実測か (khi repo で `wc -l` して一致するか) |
| Q2 | 「responsibility の表が cursor/dedup を core と定義している」は §3 の原文から引けるか |
| Q3 | 「(b) を推す根拠が core 側コストだけである」は `signal-envelope-inventory.md` §6 の原文から引けるか |
| Q4 | メタプロジェクトが 2 個目に増える予定は、`nvt-tasks` の実物 (project.yaml のコメント) から引けるか |

### B. PR-1 (core ingest)

| # | 問い |
|---|---|
| Q5 | `signals` を宣言した project が居ない workspace で、既存の action 書き込み経路の挙動が 1 ビットも変わらないことをテストが示しているか |
| Q6 | もう 1 本の書き込み口 (`dispatcher/store.go` の daemon 再起動時 abort) が範囲外である根拠が示されているか。加えて「子の abort で親 card に `child_closed` が立つか」を確認し、立たないなら別件として切り出したか (§4.5) |
| Q7 | 自己参照の遮断が fail-close か — 書き込み元の project を解決できないケースで ingest しないことをテストが示しているか (§4.3) |
| Q8 | メタプロジェクト自身の sweep task / trigger job が書いた action が inbox に入らないことをテストが示しているか。加えて**判定が actor 文字列ではなく書き込み元の project で行われている**か — `task:` (id 空) からはどの project の exec job か分からないので、そこを見て落とす実装は khi の決め打ちを core へ持ち込んだだけになる (§4.3) |
| Q9 | `api_gateway_request` のような card 宛でない action が inbox に入らないことをテストが示しているか |
| Q10 | 同じ action が 2 度 ingest されても no-op であることをテストが示しているか (PRIMARY KEY dedup) |
| Q11 | signal の INSERT が失敗しても action の書き込みが成立することをテストが示しているか (§6) |
| Q12 | 1 workspace に `signals` を宣言した project が複数あっても壊れないか — その**全部**の job / task が自己参照として落ちることをテストが示しているか (「1 個」の制約は課さないので、複数居る前提で閉じている必要がある) |

### C. PR-2 (並走)

| # | 問い |
|---|---|
| Q13 | 並走中、khi の検知結果と inbox の内部 signal 列を突き合わせた実データが記録されているか |
| Q14 | 食い違いがあった場合、その原因が「core の絞りが違う」「khi の絞りが違う」のどちらか特定されているか |
| Q15 | 並走中に khi の既存挙動 (action 列直読み) が変わっていないか |

### D. PR-3〜4 (切り替えと撤去)

| # | 問い |
|---|---|
| Q16 | 切り替え前に、並走中に溜まった内部 signal を一括 ack したか (§7 手順 3)。ack せずに一本読みへ移ると、khi が action 列経由で既に処理済みの件を過去に遡ってやり直す |
| Q17 | 切り替え後、khi の読みが `boid signal list` 1 本になっているか (`boid action list` の呼び出しが検知経路から消えたか) |
| Q18 | `--claim` を使っており、attempts が core 側で増えることを実データで確認したか |
| Q19 | `on: signals` trigger へ移行し、子 task の終端で sweep が起きることを実データで確認したか |
| Q20 | §5.2 の 3,566 行が実際に削除され、削除後もテストが通るか。**`app/trigger.py` が消えているか** (初版の見積もりが落としていた最大のモジュール) |
| Q21 | 撤去した機構に依存していた記述 (`khi-sweep` SKILL.md の「機構が持っているので気にしなくていいこと」節ほか) が追随して更新されているか |

### E. 全体 (どの段階でも)

| # | 問い |
|---|---|
| Q22 | この統合で workspace 側にも core 側にも**新たに**生まれた宣言・設定・機構がゼロか (core へ移すつもりが両側に増えていないか) |
| Q23 | `nvt-tasks` に card パイプラインを載せるとき、§5.1 の 7 機能だけで済むか — §5.2 に相当するものを 1 行も書かずに済むか |
| Q24 | attempts 上限が 3 から 5 に変わることによる運用上の変化 (dead になるまでの時間) が確認され、許容されているか |
| Q25 | §6.1 の 3 つ (並走防止の置き場・ack を打つ順序・対象一覧を誰が組むか) が実装前に決まっているか。とくに **ack を判断の書き込みより先に打っていない**か — 逆順だと crash した signal が pending から消えたまま誰も処理していない状態になる |
