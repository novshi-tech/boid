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

- 記録 (`attrs_set` / `progress`) の廃止 — 監査ログとしての役割は残る (§4.7)
- khi の判断ロジック (`khi/app/write.py`・`.claude/skills/khi-sweep/`) の変更
- 外部 connector (slack/mentions ほか 2 本) の変更
- `boid task create --idempotency-key` と `khi/domain/childid.py` の重複解消 —
  独立した論点なので別 doc に譲る (§8「この doc が扱わないもの」)

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

### 2.3 責務境界の表との照合

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

### 2.4 「core の変更ゼロ」が計上していないもの

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
このとき §2.3 の 848 行は、**そのまま 2 個目のメタプロジェクトへ複製される**。

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

### 4.2 何を signal にするか — 絞りの分担

ここが本設計の主要論点。現行 khi の絞り (`khi/domain/screen.py` の `is_signal`、S-9) は
2 段ある。

| 軸 | 現行の判定 | 目的 |
|---|---|---|
| **対象軸** | 対象が「書ける status の card」に載っている action だけ通す | 記録を残せない相手を弾く |
| **actor 軸** | khi 自身 (sweep task / trigger の exec job) の書き込みを落とす | **自己参照による無限自走の遮断** |

#### 篩は workspace、という原則との整合

`signal-driven-review.md` §8.4 が篩を workspace の責務としたのは理由つきである:

> 明らかなゴミ (mail 等の低 S/N source) は script が ack して落とす。**ノイズの質は
> source と workspace ごとに違うため**、篩は core でなく workspace の責務とする

**S-9 の 2 段はこの理由に当てはまらない。** 「自分の書き込みを自分で拾って無限に自走する」
のを止めるのは、ノイズの質の問題ではなく**構造的な自己参照の遮断**であり、どのメタ
プロジェクトでも同じ形になる。むしろこれこそ §3 で複製される代表格である。

さらに actor 軸については、**core の方が正確に判定できる**。現行 khi は action の actor に
載った task id を `boid` で引き直し、behavior が `sweep` かどうかで判別している
(`is_signal` の docstring が「自分が起こした task の id を覚えない (S-5)」ためだと説明して
いる)。core は actor をスタンプした側なので、この引き直しが要らない。

#### 対象軸は ack への一本化で不要になる

現行が対象軸を必要としているのは、**記録を書けない相手を弾くため**である
(`is_signal` の docstring: `aborted` の task には `attrs_set` も `noted` も打てないので、
通すと記録が残せず永久に再検知する)。

処理済みの印が ack になれば (§4.4)、**印は signal 行に付くので相手の task status に一切
依存しない**。よって対象軸は「弾かないと壊れる制約」ではなくなり、純粋に
「見る価値があるか」の篩 = workspace の責務へ降格する。

#### 決定

| 軸 | 統合後の持ち主 |
|---|---|
| **actor 軸** (自己参照の遮断) | **core** — ingest 時に落とす |
| 対象軸 (card 宛かどうか) | **core** — ingest の範囲として (下記) |
| ノイズ判定 (`self_authored` 等) | workspace — 現行のまま `screen.py` に残る |

対象軸を core に置くのは「篩」としてではなく、**ingest の範囲**としてである。外部
connector が「自分宛のメンションだけ取る」「アサインされた課題だけ取る」と決めているのと
同じレイヤの判断であり、篩ではない。範囲を「card 型 task 宛の action」に置くと、
`api_gateway_request` のような大量の雑音 (LLM task 1 回で 100 行超、`cursor.py` の
docstring) が構造的に inbox へ入らない。

### 4.3 宣言の形

**`signals.sources[]` には載せない。** `SignalsConfig` に兄弟フィールドを 1 つ足す。

```yaml
signals:
  internal: true                         # 追加: boid 自身の action を signal にする
  sources:
    - connector: slack/mentions          # 既存: Pack connector (変更なし)
      service: slack-cloud
      every: 10m
```

`sources[]` に相乗りさせない理由は、その配列が**実装上「1 件 = 1 導出 trigger」に
展開される**ため (`internal/orchestrator/signal_trigger_derive.go` の
`deriveSignalTriggers`)。内部シグナルは定期実行ではなく action が立つ瞬間に ingest する
(§4.5) ので導出すべき trigger が無く、相乗りさせると「source なのに trigger を導出しない」
という特例が既存経路に刺さる。さらに同じ関数が `service` と `every` を空文字で拒む
(`signal_trigger_derive.go:59,62`) ため、外部サービスへ到達しない内部 source は
そのままでは通らない。**別フィールドにすれば既存の 3 本の検証にも導出経路にも触らない。**

- **この宣言が「どの project がメタプロジェクトか」を core に教える**。§4.2 の actor 軸
  「自分自身の書き込み」は、この宣言をした project の job / task を指す
- 1 workspace に複数の project が `internal: true` を宣言した場合は load 時に検証エラー
  (inbox は workspace スコープなので、2 個あると互いの書き込みを拾い合う)
- envelope の `source.pack` に使う `boid` は予約名とし、Pack loader が同名の外部 Pack を
  読み込むことを禁じる (§4.6)

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
| `source.pack` | `boid` | 予約名 (§4.3) |
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

`attrs_set` / `progress` は**廃止しない**。役割が 2 つに分かれ、片方だけが消える。

| 記録の役割 | 統合後 |
|---|---|
| 次の巡が「もう見た」と分かる根拠 | **消える** (ack が担う) |
| 人が後から「あの巡は何をしたか」を追う監査ログ | **残る** (`khi/app/write.py` が書き続ける) |
| card の summary / attrs そのもの | **残る** (これは記録ではなく状態) |

消えるのは `khi/domain/record.py` の **decode 側** (検知が記録を読み返す経路) であり、
encode 側は残る。

---

## 5. workspace 側から消えるもの (実測)

| khi のモジュール | 本体 | テスト | 統合後 |
|---|---|---|---|
| `domain/cursor.py` | 117 | 150 | **全消** (栞が不要) |
| `adapters/file_cursor.py` | 57 | 73 | **全消** |
| `domain/attempts.py` | 72 | 105 | **全消** (core の attempts へ) |
| `domain/boid/action.py` | 135 | 139 | **全消** (action 列を読まない) |
| 小計 | **381** | **467** | **848 行** |
| `domain/record.py` の decode 側 | (327 の一部) | (369 の一部) | 縮小 |
| `app/detect.py` の `plan_boid` | (375 の一部) | (195+414 の一部) | 縮小 |
| `domain/screen.py` の `is_signal` | (138 の約半分) | (181 の一部) | core へ移動 |
| `adapters/boid_store.py` の `list_actions` | (522 の一部) | (653 の一部) | 縮小 |

**確実に消えるのが 848 行。** 縮小分を足すと 1,000 行を超える見込みだが、正確な数は実装時に
確定する。

行数より重要なのは **消えるものの質**。栞と attempts は khi で最も壊れやすかった機構で、
運用で踏んだバグはここから出ている — 「栞が自分自身を越えられない再検知ループ」
「栞が assigned のまま settle せず deadlock」の 2 件は
`signal-driven-review.md:428` が inbox の不変条件の設計根拠として引いているものであり、
**core の inbox はその再発を構造的に防ぐ形で既に作られている**。統合とは、その防御を
内部シグナルにも適用することに他ならない。

---

## 6. 実装の勘所

- **2 本の書き込み口を両方塞ぐ** (§4.5)。片方だけは無音の取りこぼしになる
- **ingest 失敗で action の書き込みを巻き戻さない。** 同一 tx に載せるが、signal の
  INSERT 失敗が card の状態更新を巻き戻すのは因果が逆。ingest 側のエラーはログに残して
  action は成立させる (取りこぼした signal は次に同じ card が動いたときの signal で
  カバーされる)
- **自己参照の遮断は fail-close で書く。** 「この action の actor がメタプロジェクト自身か
  判定できなかった」ときは ingest **しない**。判定できないまま通すと無限自走に倒れ、
  症状 (10 分ごとに sweep が起き続ける) から原因へ辿るのが難しい。逆向きの誤り
  (拾うべきものを落とす) は次の action で回復する
- **`signals.internal: true` を宣言していない workspace では一切 ingest しない。** default
  workspace が `nvt-tasks` に宣言を入れるまで、既存の挙動は 1 ビットも変わらない
- **GC は既存の 30 日ルールに乗せる** (`internal/api/gc.go`)。内部 signal 専用の保持期間を
  作らない

---

## 7. 移行

khi は本番稼働中なので、切り替えは並走で検証してから行う。shadow-a
(`signal-driven-review.md` §10.3) と同じ形を踏襲する。

1. **core 側を入れる** (PR-1〜2)。`signals.internal: true` を宣言しない限り誰の挙動も変わらない
2. **khi で並走**。`signals.internal: true` を宣言し、inbox に内部 signal が入り始める。khi は
   現行どおり action 列を直読みしたまま、**inbox の内容と自分の検知結果を突き合わせて
   ログに出す** (ack はしない)。ここで等価性を採点する (§9 グループ B)
3. **khi を inbox 一本読みに切り替える** (PR-3)。action 列の直読みをやめる
4. **消す** (PR-4)。§5 の 848 行を撤去
5. **`nvt-tasks` に載せる** — ここで初めて 2 個目のメタプロジェクトが立ち上がる。
   §3 の「複製」は発生しない

---

## 8. この doc が扱わないもの

- **`khi/domain/childid.py` (115 + 146) と `boid task create --idempotency-key` (#1012) の
  重複** — 内部シグナル統合とは独立した論点。子 id の採番と世代管理は検知ではなく書き込み
  側の関心事であり、統合しても消えない
- **khi の死にコード** — `adapters/{slack,jira,bitbucket,gateway}.py` (711) と
  `ports/source.py` (29)、`app/trigger.py` の `default_sources()`/`collect()`、および
  対応テスト 820 行。2026-08-27 カットオーバーでロールバック用に残置されたもので、
  呼び出し元が存在しない (grep で確認済み)。**本 doc の統合とは独立に削除してよい**
- **メタプロジェクト作成スキル** — §3 で統合の動機として参照しているが、設計は統合の
  完了後に行う (`signal-driven-review.md` §12「scan script の定型を組み込みスキル/
  テンプレとして配布するか」)

---

## 9. PR 分割

| PR | 側 | 内容 | 採点 |
|---|---|---|---|
| PR-1 | core | `signals.internal` の追加。parse・検証 (1 workspace 1 個)・`pack: boid` の予約 | Q1-Q4 |
| PR-2 | core | `CreateAction` での ingest。actor 軸の遮断、card 宛への範囲限定、envelope 写像 | Q5-Q12 |
| PR-3 | khi | `signals.internal: true` を宣言し、並走で等価性を観測 (ack しない) | Q13-Q15 |
| PR-4 | khi | inbox 一本読みへ切り替え。`--claim` を使う。`on: signals` trigger へ | Q16-Q18 |
| PR-5 | khi | §5 の 848 行を撤去 | Q19-Q20 |
| (独立) | khi | §8 の死にコード 1,600 行を削除 | — |

---

## 10. 採点表 — レビュワー用 yes/no 判定リスト

**極性は yes = 合格に統一。根拠を diff / 実データから引けない yes は no として扱う。**

### A. 前提 (この doc 自体の採点)

| # | 問い |
|---|---|
| Q1 | §2.3 の表の行数は実測か (khi repo で `wc -l` して一致するか) |
| Q2 | 「responsibility の表が cursor/dedup を core と定義している」は §3 の原文から引けるか |
| Q3 | 「(b) を推す根拠が core 側コストだけである」は `signal-envelope-inventory.md` §6 の原文から引けるか |
| Q4 | メタプロジェクトが 2 個目に増える予定は、`nvt-tasks` の実物 (project.yaml のコメント) から引けるか |

### B. PR-1〜2 (core ingest)

| # | 問い |
|---|---|
| Q5 | `signals.internal: true` を宣言していない workspace で、既存の action 書き込み経路の挙動が 1 ビットも変わらないことをテストが示しているか |
| Q6 | もう 1 本の書き込み口 (`dispatcher/store.go` の daemon 再起動時 abort) が範囲外である根拠が示されているか。加えて「子の abort で親 card に `child_closed` が立つか」を確認し、立たないなら別件として切り出したか (§4.5) |
| Q7 | 自己参照の遮断が fail-close か — actor を判定できないケースで ingest しないことをテストが示しているか |
| Q8 | メタプロジェクト自身の sweep task / trigger job が書いた action が inbox に入らないことを、実際の actor 文字列 (`task:<id>` と `task:`) でテストしているか |
| Q9 | `api_gateway_request` のような card 宛でない action が inbox に入らないことをテストが示しているか |
| Q10 | 同じ action が 2 度 ingest されても no-op であることをテストが示しているか (PRIMARY KEY dedup) |
| Q11 | signal の INSERT が失敗しても action の書き込みが成立することをテストが示しているか (§6) |
| Q12 | 1 workspace に 2 つの project が `signals.internal: true` を宣言したとき load が失敗するか |

### C. PR-3 (並走)

| # | 問い |
|---|---|
| Q13 | 並走中、khi の検知結果と inbox の内部 signal 列を突き合わせた実データが記録されているか |
| Q14 | 食い違いがあった場合、その原因が「core の絞りが違う」「khi の絞りが違う」のどちらか特定されているか |
| Q15 | 並走中に khi の既存挙動 (action 列直読み) が変わっていないか |

### D. PR-4〜5 (切り替えと撤去)

| # | 問い |
|---|---|
| Q16 | 切り替え後、khi の読みが `boid signal list` 1 本になっているか (`boid action list` の呼び出しが検知経路から消えたか) |
| Q17 | `--claim` を使っており、attempts が core 側で増えることを実データで確認したか |
| Q18 | `on: signals` trigger へ移行し、子 task の終端で sweep が起きることを実データで確認したか |
| Q19 | §5 の 848 行が実際に削除され、削除後もテストが通るか |
| Q20 | 撤去した機構に依存していた記述 (`khi-sweep` SKILL.md の「機構が持っているので気にしなくていいこと」節ほか) が追随して更新されているか |

### E. 全体 (どの段階でも)

| # | 問い |
|---|---|
| Q21 | この統合で workspace 側に**新たに**生まれた機構がゼロか (core へ移すつもりが両側に増えていないか) |
| Q22 | `nvt-tasks` に card パイプラインを載せるとき、§5 の 848 行に相当するものを 1 行も書かずに済むか |
| Q23 | attempts 上限が 3 から 5 に変わることによる運用上の変化 (dead になるまでの時間) が確認され、許容されているか |
