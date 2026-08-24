# suggestion を状態遷移に揃える (検討メモ)

2026-08-24 起案。**ドラフトのドラフト。** khi の第 3 世代を本番で回した初日の実戦から
出てきた検討で、まだ何も決まっていないし 1 行も実装していない。

`cross-project-issue-triage.md` (triage task 本編) と `ingestion-identity.md` (J-6:
suggestion / answered) の続き。両 doc の決定を**引き直す**提案なので、着手するなら
あちらの決定番号との突き合わせが要る。

## 1. ゴール

**人が triage task に対して決定論的にできることは、状態遷移しかない。**
だから機械の提案も状態遷移で表し、承認したらそのまま適用されるようにする。

いまは提案 (`suggestion`) と適用 (`ready` / `park` / `drop`) が別の仕組みになっていて、
承認しても何も起こらない。ここを 1 本にすると、副産物として状態そのものが減る。

派生して達成したいこと:

- **人が見る一覧の駆動輪を「機械が判断を求めているか」にする。** いまは「状態 × 急ぎ具合」
  という機械的な条件で、そこに「なぜ今これを見るべきか」が入っていない
- **daemon が人を通さずにカードを動かす経路を無くす。** いまは wake がそれをしている

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

### 2.2 カードの状態を変えるものが 3 系統に分かれている

| 誰が | 何を | 人の承認 |
|---|---|---|
| workspace (khi) の `park` / `wake` verb | boid の `park` / `wake` action を直接打つ | **無し (即時)** |
| workspace の `drop` / `manual` verb | `suggestion` を置くだけ | 承認待ち (だが承認しても動かない) |
| daemon の `SweepWake` | `parked` → `triaged`/`ready`/`working` | **無し** (コメント: 「no human in the loop」) |

同じ「カードの状態を決める」ことが、実行タイミングも承認の有無もばらばらになっている。

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

状態と急ぎ具合の積でしかない。2026-08-24 の実測では、実装が完了して PR まで出ている
ROOKPF-306 / ROOKPF-307 が `triaged` + `week` のまま queue に並び続けていた。
**queue に居ることは「機械が何か言いたい」を意味しない。**

`urgency` は「queue に出るか」(可視性) と「どの順で見るか」(優先度) を兼ねている。
khi 側にこの兼務が漏れていて、`someday` へ下げると queue からも Open タブからも消える
ため、下げようとしたら据え置く分岐が `write.py` に生えている。

## 3. To Be

### 3.1 suggestion = 状態遷移の提案

suggestion に載る verb を **boid 自身の状態遷移**に限る。workspace 語彙ではないので、
daemon が解釈しても境界を越えない —— J-7 が避けたかったものを避けたまま、accept を
適用に繋げられる。

accept したらその遷移を適用する。適用したら suggestion は消える (現行の
`applyAnsweredSideEffect` のまま。**適用型では「消す」が正しい振る舞いになる** ——
遷移した直後の提案は必ず stale だから)。

reject は「その提案は採らない」。カードは動かず、suggestion は消える。

**`ready` の accept が Go と同じ重さになるのは意図どおり。** むしろ Go はそうあるべき
なので、見た目もそれに合わせる。

### 3.2 状態を減らす

`ready` は状態としては消える。`ready` action が commit すると `Dispatch` が連鎖するので、
**正常系で `ready` に留まる時間は原理的にゼロ**。留まるのは dispatch が失敗したときだけで、
それは `workflow_action.go` が「意図した可視の失敗モード」と呼んでいる状態である。

`triaged` も消える。「浮上させたいが次の一手がまだ無い」は 2 つに分解できる:

- **人が手を動かす必要がある** → `working`。子が無いことが手動作業のサインになる。
  AI が作業中のものと同じ一覧に並ぶのはむしろ望ましい (2026-08-24 の VPN 切替カードが
  この形だった)
- **前提条件が満たされていない** → `parked`。`wake_at` / `wake_task_id` は前提条件を
  表す仕組みそのもの

workspace 側も `triaged` を必要としていない。khi の `open_triage_task_ids` は
`triage_list()` を status 無指定で引いた `pre-execution ∪ working` で、用途はシグナルの
絞り込み (`dropped` / `aborted` には `attrs_set` も `noted` も打てないので落とす)。
特定の status を判断対象として選ぶ処理は無く、判断は identity 経由のシグナル駆動で走る。

`captured` も、urgency が可視性を持たなくなるなら初期状態を `parked` にできる。

残る状態:

| 状態 | 意味 |
|---|---|
| `parked` | 前提条件が揃っていない (初期状態を含む) |
| `working` | 誰かが手を動かしている (AI / 人。子が無ければ人の手動作業) |
| `dropped` | やらないと決めた |
| `done` | 終わった |

### 3.3 wake の 3 分岐が消える

行き先を人が承認するので、機械が起点を記憶する必要がなくなる。**いま何が揃っているかで
提案すればいい。**

- 子が specced で揃っている → `ready` を suggest (accept で走り出す)
- 人が手を動かす必要がある → `working` を suggest
- まだ条件が揃っていない → 提案しない (park のまま)

2.3 で挙げた危険 (Go を通さずに実行承認へ飛ぶ) は、**人を通さないことが前提だった**ので
成立しなくなる。

### 3.4 一覧は suggestion で駆動する

queue を「suggestion が付いているカード」の一覧に置き換える。**機械が人に判断を求めて
いるものだけが並ぶ。**

suggestion が付いていないカードは出ない。それは「機械がまだ何も言えていない」ことを
意味し、**出ないこと自体が情報になる**。

`urgency` は可視性を失い、**並び順だけの属性**になる。`someday` が「どこからも見えない」
になる問題も消える。

### 3.5 khi 側の verb との対応

khi の verb は 14 個あり、5 種類の異なることをしている。suggestion に載るのはそのうち
状態遷移だけ。

| khi verb | 整理後 |
|---|---|
| `drop` | `drop` を suggest (現行どおり、ただし accept で適用される) |
| `manual` | **`working` を suggest** (「人が手を動かす」= working の定義そのもの) |
| `park` | `park` を suggest (現行は即時実行。提案型に揃える) |
| `wake` | **廃止** (park から出る提案は 3.3 のとおり中身を見て決まる) |
| `spec` / `drop-child` | children 側の提案。suggestion ではない |
| `summary` / `urgency` / `canonical` / `observed` / `link` / `skip` / `done-signal` | 機械が黙って書く記録・分類。人の承認を要さない |

## 4. まだ検証していない穴

**この節が本 doc の主内容である。** 3 節は筋であって、詰まっているわけではない。

1. **suggestion は 1 枚に 1 個しか持てない** (`attrs.suggestion` が単数、上書きされる)。
   一覧の駆動輪にするなら、提案が同時に複数立つケースをどう扱うか決めることになる。
   「機械は 1 つに決めろ」で足りるかもしれない
2. **`captured` を廃止すると、「起票されたが未判断」と「条件待ちで寝ている」が同じ
   `parked` になる。** 区別が要るか。要るなら何で表すか
3. **`ready` を状態から外すと、dispatch 失敗の可視化が失われる。** 現行はカードが
   `ready` のまま止まることが唯一の症状で、それを意図的に残している
   (`workflow_action.go`)。代わりの見せ方が要る
4. **「子が無い working = 手動作業」を誰がラベルするか。** 機械が貼るのか、状態から
   導出するのか
5. **既存データの移行。** 本番に `captured` / `triaged` / `ready` のカードが実在する
   (2026-08-24 時点で triaged 6 枚、working 3 枚)
6. **`QueueEligible` の呼び出し元の棚卸し。** queue の定義を変えるとどこに波及するか
   まだ数えていない
7. **suggestion が付かないカードが見えなくなることの是非。** 3.4 では「出ないこと自体が
   情報」と書いたが、2026-08-24 に実際に起きたのは *機械が完了に気づけないまま week の
   まま queue に居座る* ことだった。suggestion 駆動にすると同じカードは**見えなくなる**。
   これは改善なのか、別の問題への先送りなのか
8. **完了検知そのものは本 doc では解いていない。** 7 の根にあるのは「sweep がシグナル
   駆動だけで、外の世界の状態を能動的に見に行かない」ことで、これは workspace 側の
   別課題。本 doc の整理はそれを解決しない

## 5. この整理が出てきた経緯 (根拠)

2026-08-24、khi の第 3 世代を本番で初めて Go した日の実測:

- respond の子が 4 回 ask を上げたが、通知も UI の導線も出なかった (別途修正済み)
- 重複起票されたカードに khi 自身が 71 秒後に `drop` を suggest したが、**accept しても
  drop されない**ことが分かった。ここが本 doc の出発点
- 実装が完了して PR まで出ている 2 件が、`triaged` + `week` のまま queue に並び続けていた
