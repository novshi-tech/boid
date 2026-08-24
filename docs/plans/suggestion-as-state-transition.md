# suggestion を状態遷移に揃える (検討メモ)

2026-08-24 起案。khi の第 3 世代を本番で回した初日の実戦から出てきた検討。同日の
レビューと 3 巡の深堀り (実体の分離 → 遷移表 → 自動遷移の廃止 → canonical source の
降格) で主要な設計判断は確定した。まだ 1 行も実装していない。

`cross-project-issue-triage.md` (triage task 本編) と `ingestion-identity.md` (J-6:
suggestion / answered) の続き。両 doc の決定を**引き直す**提案であり、本版で明示的に
ひっくり返すものは以下。着手時にあちらの決定番号と突き合わせること:

- **決定 15** (auto-done: 全子 closed ∧ `observed.source_closed`) — 機械遷移そのものを廃止 (§3.3)
- **決定 16** (canonical source 必須契約 + watchdog) — 存在理由が消滅するため廃止 (§3.5)
- **決定 17 / I-5** (auto-reopen + フラップ予算) — 廃止 (§3.3)
- **逆輸入 2** の「done は承認不要で自動で落ちる」 — done も提案 + accept に統一 (§3.2)

## 1. ゴール

**人が triage task (以後 card) に対して決定論的にできることは、状態遷移しかない。**
だから機械の提案も状態遷移で表し、承認したらそのまま適用されるようにする。

いまは提案 (`suggestion`) と適用 (`ready` / `park` / `drop`) が別の仕組みになっていて、
承認しても何も起こらない。ここを 1 本にすると、副産物として状態そのものが減る。

派生して達成すること:

- **人が見る一覧の駆動輪を「機械が判断を求めているか」にする。** いまは「状態 × 急ぎ具合」
  という機械的な条件で、そこに「なぜ今これを見るべきか」が入っていない
- **card を動かす主体を 1 つにする。** 当初は「daemon の wake 経路を無くす」だったが、
  深堀りで踏み込んで**機械による遷移をゼロにする** (§3.2–3.3)。役割は 3 層に分離する:
  **daemon は事実を記録し、khi は提案を書き、人が accept で決める。** 将来の自動化は
  決定層の自動 accept (LLM 判断) として入る (§3.9) — 現行の auto 遷移のように決定層を
  迂回する横穴としてではなく

## 2. As Is

### 2.1 提案と適用が別系統

`suggestion` は `attrs.suggestion = {verb, reason}` として置かれ、Web UI に accept /
reject が出る。accept を押すと `answered` action が飛び、**やることは 1 つだけ**:

```go
case "answered":
    return applyAnsweredSideEffect(tx, newTask.ID)   // detail.attrs.suggestion を消す
```

`verb` は payload に載って action 行に残るが、daemon は解釈しない。accept と reject で
処理は同一。理由は J-7 に書かれている —— verb を suggestion と突き合わせるには workspace
語彙 (`manual` 等) を daemon が読むことになり、境界を越えるため。

結果として **accept は「読んだと記録して、画面から消す」ボタン**になっている。押しても
カードは動かない。しかも suggestion が消えるので、画面上は対応済みに見える。

### 2.2 カードの状態を変えるものが 5 系統に分かれている

| 誰が | 何を | 人の承認 |
|---|---|---|
| workspace (khi) の `park` / `wake` verb | boid の `park` / `wake` action を直接打つ | **無し (即時)** |
| workspace の `drop` / `manual` verb | `suggestion` を置くだけ | 承認待ち (だが承認しても動かない) |
| daemon の `SweepWake` | `parked` → `triaged`/`ready`/`working` | **無し** (コメント: 「no human in the loop」) |
| daemon の `SweepTriage` (auto-done、決定 15) | `working` → `done` | **無し** |
| daemon の `SweepReopen` (auto-reopen、I-5) | `done` → `triaged` (フラップ予算付き) | **無し** |

初版はこの表を 3 行で書いていた — daemon が人を通さず card を動かす経路を wake だけと
思い込んでいたが、レビューで auto-done / auto-reopen の 2 本が追加で見つかった。
同じ「カードの状態を決める」ことが、実行主体も承認の有無もばらばらになっている。

### 2.3 wake が 3 分岐しているのは、人を通さないから

```
wake_triaged : parked → triaged   (Manual:false)
wake_ready   : parked → ready     (Manual:false)
wake_working : parked → working   (Manual:false)
```

`machine.go` のコメントがその理由を書いている —— **どれが正しいかは「どの status から
park したか」で決まる**。triaged から park したものを ready で起こすと Go を通さずに実行
承認まで飛ぶので、機械が起点を記憶して正しい行き先を選ぶ必要がある。だから 3 つに
分かれ、`AvailableActions` にも出さない (人に選ばせない)。

### 2.4 一覧の条件に「見るべき理由」が入っていない

```go
func QueueEligible(status, urgency) bool {
    if status != ready && status != triaged { return false }
    return urgency ∈ {now, today, week}
}
```

ただしこの関数は **production から呼ばれていない** (呼び出し元は自身のテストのみ)。
本物の queue 述語は `store.go` の `queue_next` SQL (task_triage を INNER JOIN して
status ∈ {ready, triaged} ∧ urgency ∈ {now, today, week})。queue の定義を変えるとき
触るのは store.go の status フィルタ族 — `"queue"` (広い superset、テストが pin 済み)、
`"triage"`、`"queue_next"`、`"done_triage"` — と Web UI タブ・queue_notify である (穴 6)。

中身は状態と急ぎ具合の積でしかない。2026-08-24 の実測では、実装が完了して PR まで
出ている ROOKPF-306 / ROOKPF-307 が `triaged` + `week` のまま queue に並び続けていた。
**queue に居ることは「機械が何か言いたい」を意味しない。**

`urgency` は「queue に出るか」(可視性) と「どの順で見るか」(優先度) を兼ねている。
khi 側にこの兼務が漏れていて、`someday` へ下げると queue からも Open タブからも消える
ため、下げようとしたら据え置く分岐が `write.py` に生えている。さらに khi は
**captured を someday 置き場としても使っている** (「someday は書くだけで captured に
留める」 — 進めると triaged + someday でどこからも見えなくなるから)。urgency の兼務が
生んだ workaround が 2 つある。

### 2.5 状態機械は 1 つだが、実体は 2 つの島に割れている (2026-08-24 深堀り)

`machine.go` の rule を到達可能性で見ると、pending/executing/awaiting の島と
captured/triaged/parked/ready/working の島は、同じタスクの上ではほぼ繋がらない。
共有しているのは終端 (done/aborted) だけ。captured で生まれたカードは一生 executing に
ならず、pending で生まれたタスクは一生 triaged にならない。ただし厳密には rule レベルで
`reopen: done → executing` が done の card にも適用可能で、それを防いでいるのは api 層の
振り分けである — **この分離は machine 単体の性質ではなく、場外のガード込みでやっと
成立している**。その場外の規律は 4 つ:

- **reopen の分岐** — done のカードと done の通常タスクを machine は区別できない
  (`machine.go` の reopen_triaged doc comment 自身が "the machine cannot see" と書いて
  いる)。api.ApplyAction が task_triage sidecar の有無を覗いて reopen / reopen_triaged
  を選んでいる
- **`triage_done` という改名** — IsManualAction がアクション名を機械全体で共有するため、
  素直に `done` と名付けると実行系の done (Manual:true) と衝突して khi が完了を主張
  できる穴が開く。名前空間が 1 つだから必要になった回避
- **preExecutionStatuses の列挙規律** (論点6-3) — triage 系 verb は FromStatus に `"*"`
  を使うなという約束。破ると通常タスクの executing に attrs_set が刺さる。rule を
  足すたびに人間が思い出す必要がある
- **abort と drop のスコープ手動調整** — abort を実行系ステータスに限定し、drop の
  担当範囲と重ならないよう、1 つの機械の中で 2 つの縄張りを手で塗り分けている

境界の正体はもう一段深いところにある。通常タスクは**セッションを持つ主体**で、状態は
プロセスのライフサイクル (sandbox が走っているか、成功したか) を表す。card は
**khi と nose が読み書きする客体**で、状態は判断の台帳 (いま誰の番か、注意が要るか) を
表す。カード自体には知性が無い — 判断は khi のトリガ起動セッション (別のタスク) が行い、
Shape ボタンも session は workspace 側に立ててカードへ書き戻すだけ。**「カードには
エージェントセッションが起きない」のは実装の都合ではなく本質である。**

プロセスの機械と注意の機械は答える問いが違う。運用して感じる「同じタスクなのに別物」
という違和感の正体は、モデルが違うことではなく、**違う 2 つの実体が同じ enum・同じ
machine を着ていること**にある。本 doc の To-Be はこの分裂を解消するのではなく先鋭化
させる — card 側は parked/working/done/dropped だけの純粋な注意の機械になり、実行系の
語彙が完全に消える。だから実装形も分離に揃える (§3.8)。

## 3. To Be

### 3.1 suggestion = 状態遷移の提案

suggestion に載る verb を **boid 自身の状態遷移**に限る。workspace 語彙ではないので、
daemon が解釈しても境界を越えない —— J-7 が避けたかったものを避けたまま、accept を
適用に繋げられる。

accept したらその遷移を適用する。適用したら suggestion は消える (現行の
`applyAnsweredSideEffect` のまま。**適用型では「消す」が正しい振る舞いになる** ——
遷移した直後の提案は必ず stale だから)。

reject は「その提案は採らない」。カードは動かず、suggestion は消える。

### 3.2 card の状態機械: 4 状態、機械の行は capture だけ

| from | to | 引き金 | 誰 |
|---|---|---|---|
| (新規) | parked | capture (取り込み) | 機械 |
| parked | working | accept(go): specced 子を dispatch して開始 | 人 |
| parked | working | accept(working): 手動作業として開始 | 人 |
| parked | dropped | drop (直接 or accept) | 人 |
| working | parked | park (wake 条件付き、直接 or accept) | 人 |
| working | done | accept(done) / 直接 done | 人 |
| done | parked | reopen (直接 or accept) | 人 |
| dropped | parked | reopen (直接 or accept) | 人 |

不変量: **機械が起こせる遷移は capture だけ。機械は仕事を始めない (working に入れない)
し、捨てない (dropped に入れない) し、閉じもしない。** 前進 (parked → working) は必ず
人の accept を通る — Go ゲートはこの 1 本の辺に凝縮される。wake の 3 分岐が消える理由も
この表で自明になる: parked の出口は人の accept だけで、行き先は suggestion の verb が
決めるから、機械が park の起点を記憶する必要が構造ごと消える。

| 状態 | 意味 |
|---|---|
| `parked` | 前提条件が揃っていない (初期状態・reopen 後の再判断待ちを含む) |
| `working` | 誰かが手を動かしている (AI / 人) |
| `dropped` | やらないと決めた |
| `done` | 終わった |

- `ready` は状態としては消える (ready 遷移 + 機械 dispatch の 2 段が accept(go) の
  1 段になる)。`triaged` は working / parked に分解。`captured` は parked に吸収
  (someday 置き場 workaround も一緒に消える — §2.4)
- **card は `aborted` にも到達しなくなる。** job_failed は job を持たない card に無縁で、
  dispatch 失敗は下記のとおり同期エラーになる。card 機械は 4 状態で本当に閉じる
- **dispatch 失敗の可視化 (旧穴 3) はむしろ改善する**: Dispatch は同期呼び出し
  (`workflow_action.go` の ready 連鎖) なので、accept(go) を「dispatch 成功後に working へ」
  の順にすれば、失敗時は card が parked のまま・suggestion も残り・**accept 操作自体が
  その場でエラーを返す**。現行は Go が成功に見えて失敗は slog にしか出ず ready に無言
  滞留するので、症状の質が上がる。Tx 単位 (現行は ready commit と dispatch が別 Tx)
  は実装課題
- **go と working は別 verb にする**: accept の重さが違う (go は実行承認で AI が走る、
  working はただの手動宣言)。手動のつもりの accept で子が走り出す事故を防ぐ
- **accept(done) は決定 15 の「khi は完了を主張できない」原則と両立する**: khi にできる
  のは done の**提案**までで、適用は必ず人の accept。ソース無し手動カード (現行は
  source_closed が永久に満たせず閉じられない) にも閉じ道ができる。done の基準は
  daemon の固定式から khi の instruction に移り、**workspace ごとに柔軟に定義できる**
- **drop は parked からだけ**: 「Go を過ぎた card は drop できない」現行規律を維持。
  working の card を捨てたければ park → drop の 2 手

### 3.3 機械遷移ゼロ: sweep 3 兄弟の行方

auto 遷移は**仕様から落とす** (2026-08-24 nose 決定 — 「自動遷移は思い切って仕様から
落として、将来 LLM 判断による suggest の自動 Accept を実装する方向」)。

- **SweepTriage (auto-done): 廃止。** done は khi が suggest する。判断材料は khi が
  全部持っている — child_closed は daemon が action log に自己記録済みで khi の
  シグナル網に入っており、source_closed はそもそも khi 自身が書く
- **SweepReopen (auto-reopen): 廃止。** source の再オープンを検知して observed を書き
  換えるのは khi 自身なので、その場で reopen を suggest すればよい。`auto_reopen.go` の
  フラップ予算 (エピソード計数一式) は不要になり削除 — 人が accept するならフラップは
  人が止める
- **SweepWake: 唯一の生き残り。ただし事実記録係に降格。** wake_at は時計の話で、
  シグナル駆動の khi は時計を持たないため daemon の sweep 自体は残る。だが仕事は
  「wake 条件成立」を事実 action (仮称 `wake_due`) として card に追記するだけになる。
  遷移も提案もしない。その action が khi のトリガを起こし、khi が中身
  (子が specced か、手動が要るか) を見て suggest する

### 3.4 suggestion の書き手は khi 単独

daemon は suggestion を書かない (事実の記録まで)。書き手が khi 1 人なら、上書きは
「khi が自分の判断を更新した」の意味しか持たず、**最新が勝つ**で正しい — 旧穴 1
(単一スロットの衝突) はこれで消える。「1 枚に 1 提案、機械は 1 つに決めろ」は規約として
維持する。

boid コアは機構だけを持ち、判断は全部 workspace 側 — boid の設計スタンス
(ポリシーと enforcement の分離) とも一致する。

### 3.5 canonical source は必須契約から workspace 運用へ降格

canonical source の存在理由は auto-done ただ 1 本だった。因果鎖はこう:
auto-done の式 (決定 15) が `observed.source_closed` を要求 → 終了概念を持たない source
(Slack スレッド / mail / head) の card が永久に working に積もり GC にも到達しない →
「source_closed を報告できる source を必ず作る」契約 (nose 案 2026-08-13、khi が Jira
issue を起票する S-10) → 決定 16 の watchdog (`queue_sweep.go` — warning 文自身が
「これらは done 自動落ちに到達できません」と書いている)。

auto-done を落とすとこの鎖は丸ごと崩れる:

- **必須契約 (決定 16) は廃止。** watchdog / `SweepCanonicalSourceBreaches` /
  `MissingCanonicalSourceGuidance` 一式が削除対象
- **「タスクの一意特定キー」の役割は canonical には元々無い。** khi の
  `domain/canonical.py` は冪等キーを boid の task id から導出しており
  (`task_label = "khi-task-" + task_id` を Jira label に焼き込む)、依存の向きは
  task → canonical。シグナルの紐付け・重複排除は **ingestion identity** の仕事
  (identity は task に複数ぶら下がる、canonical は 1 つ — S-15 link)。
  **identity 機構は本再設計のスコープ外・現状維持**
- **canonical は「外部チケットを立てる workspace 運用」として optional 化。** 対人可視性
  (チームの他者が見られる Jira issue) は auto-done と無関係の価値で、立てる運用なら
  done 判断の強い証拠にも、identity link 経由の signal routing にも使える。
  「あれば使える」に降格し、「無いと壊れる」ではなくなる

実証: 本番は 2026-08-24 時点で **canonical 全カード欠落のまま**回っており、決定 16 の
契約は一度も履行されていない。実害は「auto-done が一度も発火しない」だけ
(ROOKPF-306/307 が queue に居座った症状の根の一つ)。suggest-done 化は理想の放棄では
なく現実の追認である。

### 3.6 一覧は suggestion で駆動する

queue を「suggestion が付いているカード」の一覧に置き換える。**機械が人に判断を求めて
いるものだけが並ぶ。**

suggestion が付いていないカードは出ない。それは「khi がまだ何も言えていない」ことを
意味し、出ないこと自体が情報になる (ただし穴 7 の 3 類型に注意)。

`urgency` は可視性を失い、**並び順だけの属性**になる。`someday` が「どこからも見えない」
になる問題も、captured を someday 置き場に使う workaround も消える。

### 3.7 khi 側の verb との対応

| khi verb | 整理後 |
|---|---|
| `drop` | `drop` を suggest (accept で適用される) |
| `manual` | **`working` を suggest** に改名 (「人が手を動かす」= working の定義そのもの) |
| (新設) | **`go` を suggest** (specced 子の dispatch + working。実行承認) |
| (新設) | **`done` を suggest** (auto-done の置き換え。基準は khi instruction = workspace 毎) |
| (新設) | **`reopen` を suggest** (auto-reopen の置き換え) |
| `park` | `park` を suggest (現行は即時実行 → 提案型に揃える) |
| `wake` | **廃止** (park から出る提案は wake_due 事実を見て khi が決める — §3.3) |
| `canonical` | **必須でなくなる** (§3.5)。起票運用を選ぶ workspace のみ残す |
| `spec` / `drop-child` | children 側の提案。suggestion ではない (現状維持) |
| `summary` / `urgency` / `observed` / `link` / `skip` / `done-signal` | 機械が黙って書く記録・分類 (現状維持。done-signal は done suggest の判断材料になる) |

### 3.8 実装形: machine を分ける。エンティティは分けない (2026-08-24 決定)

呼称: この実体は **card** と呼ぶ。一般名詞すぎる懸念はあったが、実際にやることを
書きつけるメモ帳そのものなので、一周回ってこれでよい (nose 決定)。

3 案を比較した:

- **A. エンティティごと分ける** (card テーブル新設) — 却下。card の子は通常タスクで
  `parent_id` が境界をまたいでいるし、action log / timeline / Web UI / notify を
  1 系統で共有していることに実利がある。二重化の痛みに対して得るのが型の綺麗さだけ
- **B. エンティティは共有のまま machine だけ分ける** — **採用**。task_triage sidecar の
  有無 (いまも事実上の型タグとして機能している) で StateMachine を選ぶ。ApplyAction の
  入口・tasks テーブル・action log は共有のまま
- **C. 先に状態削減をやり、分離は後で考える** — 順序が逆 (下記)

B により §2.5 の場外規律が原理的に消える:

- 名前空間が分かれるので `triage_done` は `done` に戻せる (ただし下記の注意)
- card 機械に executing が存在しなくなるので、FromStatus 列挙規律そのものが不要になる
- reopen の sidecar 覗き見は「この task はどちらの機械を使うか」という正面の機構に
  昇格する

**順序は B → 状態削減。** B は意味を変えない純リファクタとして切り出せる。機械が
分かれていれば、§3.2 の状態削減は「card 機械だけの変更」に局所化され、穴 5 (既存
データ移行)・穴 6 (queue 述語の波及) の範囲も読みやすくなる。

レビューで付いた注意書き:

- **`triage_done` → `done` の改名は B の純リファクタに含めない。** action 履歴には
  `triage_done` 行が実在し、履歴データは rename しない。読み手だった `auto_reopen.go`
  (isDoneEntryAction) は §3.3 で auto-reopen ごと消えるが、残る読み手 (timeline 表示・
  khi の action 種別読み) を棚卸ししてから、新規書き込みのみ `done` に切る
- **「挙動を 1 ミリも変えない」は言い過ぎで、正しくは「意味を変えない」。** machine を
  分けると未知 action のエラー経路 (IsManualAction の判定範囲) が微妙に変わる。B 単体
  PR の受け入れ条件は「全既存テスト green + エラー応答の差分列挙」とする
- **card 遷移の押し込み防御が名前ベースから経路ベースに変わる** (穴 11)。現行は
  triage_done を Manual:false + 別名にすることで khi の done 押し込みを防いでいた。
  新 card 機械では done が人の操作になるため、防御は「遷移 action は answered(accept)
  経由または actor=human のみ受理」という経路/actor ベースの設計に移す

### 3.9 将来: 決定層の自動 accept

自動化は決定層に入れる。suggestion (verb + reason) を LLM 判断で自動 accept する
ポリシー層で、機構 (提案 → 決定 → 適用) はそのまま、accept する主体だけが人から
ポリシーに差し替わる。先に手動で回すことで **accept / reject の履歴が action log に
貯まり、そのまま自動 accept ポリシーの評価データになる**。

フラップ制御は機械から消えたのではなく決定層に引っ越しただけ — 自動 accept を入れる
日には accept ポリシー側の課題として戻ってくる。

## 4. 穴

レビューと深堀り 3 巡で、初版の穴 8 個のうち 4 個は解けた。解けたものは解き方ごと
残し、開いているものに続番を振る。

**解けた穴:**

1. ~~suggestion の単一スロット~~ → 書き手を khi 単独にして解消 (§3.4)
2. ~~captured 廃止で「未判断」と「条件待ち」が同じ parked になる~~ → 「未判断」は状態
   ではなく suggestion の不在で表す (§3.6)。someday 置き場 workaround も消える。
   UI 上の区別が本当に不要かは運用で確認
3. ~~ready 廃止で dispatch 失敗が不可視になる~~ → accept(go) の同期エラー返しで現行
   より改善 (§3.2)。Tx 単位の設計は実装課題として残る
4. ~~「子が無い working = 手動作業」を誰がラベルするか~~ → ラベルの機械的消費者
   (auto-done の評価器と wake の行き先決定) が両方消えたため、machine-readable にする
   必要ごと消滅 (§3.2–3.3)。khi は自分が working を提案した文脈を覚えており、人は UI で
   子の有無を見れば足りる

**開いている穴:**

5. **既存データの移行。** 本番の captured / triaged / ready / aborted の card を
   parked / working へ写像する規則。triage_done の action 履歴は改名せずそのまま (§3.8)
6. **queue 定義変更の棚卸し。** 対象は `QueueEligible` (production 未使用) ではなく
   store.go の status フィルタ族 — `"queue"` (superset、テストが pin)、`"triage"`、
   `"queue_next"`、`"done_triage"` — と Web UI タブ・queue_notify
7. **suggestion の付かないカードの不可視。** 3 類型ある: (a) khi がまだ何も言えていない
   parked (意図どおり、出ないこと自体が情報)、(b) 閉じ忘れの stale working (auto-done
   廃止で新たに増える)、(c) wake 条件なしの初期 parked (khi の park verb は wake_at /
   wake_task_id 必須という現行不変量 [write.py] と衝突する)。(b)(c) を拾う棚卸し (UC-5)
   の守備範囲を設計すること
8. **完了検知そのものは未解決のまま。** khi が外の完了に気づく問題 (シグナル駆動 sweep
   の限界) は workspace 側の別課題。ただし契約が「canonical を立てて source_closed を
   観測」から「任意の証拠で done を suggest」に緩和され、達成しやすくはなった
9. **accept 総量。** park / done / reopen も accept 化するので人が捌く件数は増える。
   現在の規模 (working 3 枚) では問題にならないが、増えたときの逃がし先が §3.9
10. **wake_due 事実 action の khi 側配線。** khi の screen は action 種別で篩うため、
    新種別の追加が要る
11. **card 遷移 action の押し込み防御の再設計。** 名前ベース (Manual:false + 別名) から
    経路/actor ベースへ (§3.8)

## 5. この整理が出てきた経緯 (根拠)

2026-08-24、khi の第 3 世代を本番で初めて Go した日の実測:

- respond の子が 4 回 ask を上げたが、通知も UI の導線も出なかった (別途修正済み)
- 重複起票されたカードに khi 自身が 71 秒後に `drop` を suggest したが、**accept しても
  drop されない**ことが分かった。ここが本 doc の出発点
- 実装が完了して PR まで出ている 2 件が、`triaged` + `week` のまま queue に並び続けていた

同日、doc 自体のレビューと 3 巡の深堀りを行った:

- レビュー: daemon が card を動かす経路が実は 3 本あること、QueueEligible が production
  未使用であること、khi の park が wake 条件必須であることが判明
- 1 巡目: 遷移表を引いたら「機械は working / dropped に入れない」不変量が浮かんだ
- 2 巡目 (nose): 自動遷移 (auto-done / auto-reopen) を仕様から落とし、自動化は将来の
  自動 accept (決定層) に置く。穴 4 はこれで消滅した
- 3 巡目 (nose): auto-done が消えるなら canonical source の必須理由も消えるのでは、
  という仮説。調査で「一意特定キーの役割は最初から identity のもの (依存の向きは
  task → canonical)」と確認し、canonical は workspace 運用に降格した
