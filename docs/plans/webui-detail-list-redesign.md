# Web UI を 2 実体に揃える — 詳細・一覧の再構築 (素案)

2026-08-25 起案。`suggestion-as-state-transition.md` の残課題 (Queue と Parked の
二重表示、穴 6 の queue 定義棚卸し、穴 7 の suggestion 無し card の可視性) を Web UI
側で引き取る。card 機械 v2 (4 状態・機械遷移ゼロ) と card モデル整理 PR-1〜3 (STI
直和化) が出荷済みであることを前提にする。

素案 v2 (2026-08-25): 初版への nose コメント 8 件 (artifact レビュー) を反映した。
主な変更 — card 詳細に表示優先順位 (モバイルファースト) を明記、子一覧を統合
ライフサイクルビューとして上位コンテンツに昇格、exec root 詳細に子ツリーを新設
(旧論点 7 決定)、Description 表出し決定 (旧論点 1)、urgency 読み手の実測 (論点 2)、
`"triage"` フィルタの正体の説明と rename 方針 (§3.6)。

## 1. ゴール

**一覧と詳細は「判断の場」である。開いた人が 3 秒で「これは何で・いまどこに居て・
何が判断を求めているか」を読めること。**

そのために表示規約を 3 つの原則に絞る:

1. **実体が違えば画面も違う。** card (判断の台帳) と実行タスク (プロセス) は
   machine を分けた (machineFor)。画面も同じ軸で分ける。1 枚のテンプレを
   hasTriage 分岐で共有し続けない
2. **モデルをそのまま描く。** suggestion は「状態遷移の提案」= 状態機械の辺に
   なった。だから辺として描く (`parked —go→ working`)。ラベル付き key-value の
   羅列でモデルを翻訳し直さない
3. **表示とソートを同じイベント集合で駆動する。** 「行に描かれる動き」=
   「updated_at を動かすイベント」= 「一覧でその行が浮上した理由」。3 つが同じ
   集合なら、一覧の並び自体が説明になる

ゴールでないもの: 機能追加はしない。card 機械・suggestion 機構・khi 側の契約は
一切変えない。これは表示と、表示を支える 1 つの永続属性 (updated_at) の再構築。

## 2. As Is (地図)

### 2.1 詳細ページ: 1 枚のテンプレを 2 実体で着回している

`web/templates/tasks.templ` の TaskDetail は上から:

| 部位 | 実体 | 中身 |
|---|---|---|
| メタ帯 (`:291`) | 共通 | status · behavior · project · updated_at。**card では behavior 枠に type 名 "card" を埋める** (`taskBehaviorLabel`, `:239` — コメント自身が「空欄にしないため」と書く埋め草) |
| suggestion カード (`:369`) | card 専用 | verb バッジ + reason + basis + Accept/Reject。ゲート 2 段 (CanApplyManualAction + SuggestionInapplicable)、3 PR ぶんの修正史がコメントに積層 |
| children リスト (`:465`) | card 専用 | detail blob の台帳 status (open/specced/dispatched/closed, `card.go:274`) のみ。**dispatch 済み子の生の task status (executing/awaiting) は出ない** — 「→ task」を踏むまで分からない |
| タブ (`:1238`) | ほぼ exec 専用 | Timeline + (Description / Payload / Instructions)。Timeline タブに SSE 購読 + ティッカーの JS 約 90 行が templ 直書き (`:1032`〜)。card にとって Payload / Instructions は常に空 |
| action bar (`:626`) | 共通 (分岐だらけ) | exec 系 (Start/Abort/Rerun/Reopen) と card 系 (Go/done primary, Shape, ケバブに working/park/done/drop) が hasTriage × status で分岐。**同じ遷移のボタンが suggestion セクションと 2 箇所にあり、confirm 文言を両側で揃える保守が発生** (コメント自身が PR #988 MEDIUM 1 を引く) |

死にかけ: TaskDetailJobsSection (`:990`) はページのどこからも描画されない
(タブ分岐に jobs が無い)。`jobs` 引数は TaskDetail → TabsSection → TabPanel と
素通しで運ばれて未使用。fragment `kind=jobs` (`internal/api/web.go:641`) も
ページからの呼び手が無い。コメントに存在しない "dependencies" タブへの言及
(`:1180`)。

### 2.2 一覧: タブ 4 枚 + ツリー

タブは Open / Closed / Queue / Parked (`components/filters.templ:78`)。述語は
`internal/orchestrator/store.go`:

- Open — 非終端 self ∪ 子孫救済 ∪ 祖先救済。**再帰 CTE 2 本**
  (open_descendants / open_ancestors, `store.go:411`) はツリー表示の孤児防止の
  ためだけに存在する
- Closed — 終端。唯一 updated_at DESC ソート (`store.go:497`)
- Queue (queue_next) — `suggestion_verb != ''` (`store.go:465`)。ソートは
  urgency 段 → created_at (`store.go:509`)
- Parked — `status = parked`

**Queue と Parked は述語が直交しているので、parked かつ suggestion 付きの card は
両方に出る。** 排他に見えるタブ UI の直観に反する — 本 doc の出発点。

ツリーは BuildTreeItems / BuildTreeItemsWithSuggestions (`internal/api/web.go`)
+ `components/task_tree.templ` (290 行)。一覧クエリに LIMIT は無い
(ページネーション不在)。

### 2.3 updated_at は suggestion で動かない

khi の書き込み (suggestion 含む) は attrs_set 系 action で、
`internal/api/workflow_action.go:401` が attrs_set / child_added /
child_specced / child_dropped / noted について **UpdateTask を意図的に
スキップ**する。理由は codex レビューが見つけた race — UpdateTask は全カラム
書き戻し (`store.go:590`) で、Tx 前に読んだ stale スナップショットの status を
書き戻すと並行遷移を踏み潰す。あの長大なコメントは全て **status 踏み潰し**の話で、
updated_at を動かさないこと自体は判断された形跡が無い (巻き添え)。歴史的にも
card 属性は 2026-08 まで別テーブル (task_triage sidecar) に書かれており、tasks
行の updated_at は物理的に動きようがなかった。STI 統合 (card モデル整理 PR-2) は
「意味を変えない純リファクタ」の規律で移植したため、この挙動も忠実に保存された
(`card.go:56` の doc comment)。

読み手も居なかった: card の一覧ソートは created_at と urgency で、updated_at を
読むのは Closed タブだけ。動かなくても観測されなかった。

### 2.4 実測の症状 (2026-08-24〜25 の運用から)

- Queue / Parked の二重表示 (§2.2)
- 子が 4 回 ask を上げたが親 card からは「dispatched」にしか見えなかった
  (awaiting バナーは子自身のページには出る — `:951` — が、親には何も出ない)
- Shape 後に suggestion が消えると card が Queue から消える (可視性が queue 述語に
  縛られているため — memory: shape-button-defers-to-workspace-skill)

症状は根拠として記録するが、**To Be はこのリストに 1:1 で答える形では組まない**
(問題リスト起点の再設計は項目に縛られる)。To Be は §1 の原則から導く。

## 3. To Be

### 3.1 部品 A: メタ 2 段規約 (両実体・両画面の共通規約)

メタ情報 4 種 (status / behavior / suggest / project) をラベルで説明するのを
やめ、**意味で 2 群に割って位置と記法に語らせる**:

- **素性の段 (静)** — 「これは何で、どこの持ち物か」。作成後不変。ラベル無しの
  パス表記:
  - card: `<project> / <kind>` (kind = signal | issue | theme, `card.go:38`)。
    埋め草 "card" は廃止
  - exec: `<project> / <behavior>`
- **動きの段 (動)** — 「いまどこに居て、何が起きているか / 求められているか」:
  - card, suggestion 無し: 状態バッジのみ (`parked`)
  - card, suggestion 有り: **遷移の辺として描く** — `parked —go→ working` +
    reason。**辺には verb を必ず載せる。** go と working は行き先が同じ working
    で、裸の矢印では区別が消える。go は specced 子を dispatch する実行承認、
    working はただの手動宣言 — この区別を消すと「手動のつもりの accept で AI が
    走る」事故に戻る (設計 doc §3.2 が別 verb にした理由そのもの)
  - exec: 状態 + 経過 (`executing · 03:12`) / 質問 (`awaiting · ⚠ 質問あり`)

**契約: 動きの段に描かれるイベント集合 = updated_at bump 集合 (部品 B) =
一覧で行が浮く理由。** この一致が原則 3 の実装形。

イメージ (card 詳細の頭):

```
ROOKPF-310 通知が二重に飛ぶ
rookpf / issue                       8/25 14:02
parked —go→ working
  子 2 件 specced 済み。実装に進められる
  [承認] [却下]
```

exec 詳細の頭:

```
PR #994 レビュー反映
boid / implement                     8/25 13:40
executing · 03:12 経過
```

### 3.2 部品 B: updated_at bump 方針

bump する (= 「動き」として描かれ、一覧で浮上する):

| イベント | 実装点 | 備考 |
|---|---|---|
| 全ての状態遷移 | 既存 (UpdateTask が bump 済み) | 人の直接操作・accept 経由とも |
| suggestion の付与 | attrs_set side-effect で **verb が (旧値から) 変化し、かつ空でないとき** | notifySuggestionArrived (`queue_notify.go`) が実際に発火する条件 (呼び出し側の suggestionVerbChanged ゲート + 関数自身の verb 非空チェック) と同じ判定極性。null-clear (撤回) はもちろん、同一 verb の再送 (khi の `_write_suggestion` は無条件で毎 judge cycle 送る) も bump しない |
| child_closed | 親 card への記録時 | 子の完了は親の判断材料が変わった瞬間 |

bump しない: observed / summary / urgency / link / skip / done-signal 等の
帳簿書き、noted、child_added / child_specced / child_dropped。**observed は khi
が高頻度で書き換える実績があり (memory: llm-dependent-step-is-not-convergence の
churn)、これを bump すると一覧が機械の帳簿書きで並び替わり続ける。**

実装形: status を運ばない単発文 `UPDATE tasks SET updated_at = ? WHERE id = ?`
を、side-effect と同一 Tx 内で打つ。skipTaskUpdate の race 修正
(`workflow_action.go:401`) と両立する — あの修正が防ぐのは stale status の
書き戻しで、この文は status を運ばない。**着手時にあのコメントの「the task row
genuinely has nothing new to persist」を書き換えること** — 新設計ではこの一文が
嘘になる (updated_at という persist すべきものが生まれる)。

### 3.3 部品 C: card 詳細ページ

**表示優先順位 (2026-08-25 nose レビュー反映)。** モバイルファーストで組む。
縦に長いページで下に沈んだものは無いのと同じなので、並び順そのものを設計する:
**「判断に要るもの」→「読むもの」→「履歴」**。timeline は既定で直近数件に
折り畳み、展開で全量 — 履歴が本文や子一覧を押し下げない。

構成 (上から):

1. タイトル + 素性の段 + 動きの段 (§3.1)。suggestion の承認 UI は動きの段に
   同居 — 承認とは描かれている辺を適用することだから。ゲート 2 段
   (CanApplyManualAction + SuggestionInapplicable) は現行のまま維持
2. **子一覧 (統合・最小表示)** — 上位コンテンツに昇格 (nose: 「かなり上位の
   コンテンツとしてうまく見せたい」)。現行は spec の台帳 status
   (open/specced/dispatched/closed) と実行タスクの生 status が完全に分離して
   いて、生 status はリンク先でしか見えない。統合の規則は「**1 子 = 1 行、
   現在地のチップ 1 個**」— 台帳 status が dispatched の間だけ、チップに生
   status (executing / awaiting / done / aborted) を差し込む。モデル上の直列
   (`open → specced → [executing → awaiting → …] → closed`) を行ごとに描く
   ことは**しない** — モック v1 で試して情報過多と判定 (2026-08-25 nose:
   「状態遷移をズラッと出すのはやりすぎ。**欲しいのは suggest だけ**」)。
   判断の駆動輪は card の suggestion (動きの段) であって、子の状態機械の詳細
   ではない。子行は チップ + タイトル + behavior/project + ⚠ 質問導線
   (`/tasks/{child}/questions/{qid}` へ親から 1 タップ) + spec 折り畳みまで。
   spec 折り畳みの中身は **description のみ** — instruction は毎回テンプレで
   冗長なため出さない (2026-08-25 nose。現行 TaskDetailChildrenSection は両方
   出している — `tasks.templ:494`)。
   並びは要注意順 (awaiting → executing → open/specced → closed)。実装:
   WebHandler が `parent_id` で子を 1 回引き (`store.go:911` の ListChildren
   相当)、TaskRef で detail blob の台帳と突き合わせる。§2.4 の「ask 4 回が
   見えなかった」への詳細ページ側の答え。端ケース (台帳 closed と task 終端の
   突き合わせ等) は論点 8
3. **本文** — card の Description。card にとって description は「取り込まれた
   内容」そのものなので、タブに隠さず本体に出す (決定済み — 旧論点 1。折り畳み
   既定のみ実装時に調整)
4. timeline — **既定で直近 N 件に折り畳み**、展開で全量。描画は現行の
   TaskDetailTimelineSection を流用。SSE 更新も流用
5. action bar — escape hatch (人の直接操作で全遷移、設計 doc §3.2 の担保) +
   Shape。suggestion 側の承認フォームとボタンが二重になる問題は、承認 UI を
   動きの段 (1) に一本化し、action bar は直接操作専用と役割を割ることで解消

出さないもの: Payload / Instructions タブ (exec 語彙、card では常に空)、
Jobs (card は job を持たない)。

### 3.4 部品 D: 実行タスク詳細ページ

card 詳細との関係 (2026-08-25 nose コメント反映): **URL は 1 本 (`/tasks/{id}`) の
まま、task.Type で layout を切り替える** — machineFor と同じ分岐軸。対比で言うと、
card 詳細が「判断の場」(動き・子・本文が主役、履歴は従) であるのに対し、exec 詳細は
「プロセスの観測窓」— timeline が主役のまま。

構成 (上から):

1. タイトル + 素性の段 + 動きの段 (§3.1。exec は状態 + 経過 / awaiting バナー)
2. **子ツリー (root のみ)** — 一覧からツリーを撤廃する代わりに、root (parent_id
   無し) のタスク詳細が自分の部分木を持つ (2026-08-25 nose 決定 — 旧論点 7:
   「ルートの詳細がツリーを持っていてくれた方がユーザーに優しい」)。supervisor 系の
   親子はここで見る。多階層は再帰表示。一覧の再帰 CTE 削除方針とは独立 — こちらは
   1 タスクの部分木を引くだけ
3. timeline (全量。現行の TaskDetailTimelineSection + SSE を流用)
4. Description / Payload / Instructions (現行タブ or 折り畳み。実装時に調整)
5. action bar (Start / Abort / Rerun / Reopen — 現行等価)

同時に死骸を掃除する:

- TaskDetailJobsSection と `jobs` 引数の素通し配線、fragment `kind=jobs`
- 存在しない "dependencies" タブへのコメント言及 (`:1180`)
- templ 直書き 90 行の SSE JS は静的 asset へ移す (論点 5)

### 3.5 部品 E: 一覧

- **タブ 4 枚とツリーを撤廃。** 1 本のフラットリスト、トップレベル
  (parent_id 無し) のみ。子は親の詳細 (§3.3) で見る
- **ソートは全ビュー updated_at DESC** (tie-break: id)。suggestion が付いた
  card は部品 B により付いた瞬間に浮上する。人が動かしたものも同様
- **デフォルトは全状態表示 + ページネーション。** フィルタは「アクティブのみ
  (非終端)」スイッチ 1 個。全表示がデフォルトなら、done card への reopen
  suggestion が非終端フィルタで隠れる穴はそもそも開かない — suggestion 付与の
  bump で浮上し、recency ソートが古い終端を自然に沈める。ページサイズは 50、
  LIMIT/OFFSET で開始 (論点 4)
- **行フォーマット** — 3 行構成 (2026-08-25 nose レビュー反映: モバイル幅で
  1 行に詰めると状況が必ず切れる。行数を増やしてでも状況を全文出す)。
  1 行目 = タイトル + 相対時刻、2 行目 = 素性パス + 子 rollup、3 行目 = 動き
  (遷移辺 or 状態 + 状況テキスト。**省略記号で切らず折り返す**、上限 2 行で
  clamp):

```
ROOKPF-310 通知が二重に飛ぶ                        12m
rookpf / issue                       子 3 · 進行 1 · ⚠ 1
parked —go→ working  実装子の spec が揃った。dispatch
して実装に進められる

card モデル整理 PR-4 supervisor                     2h
boid / drive                               子 2 · 完了 1
executing  PR-4b 実装が進行中 · 経過 03:12

ROOKPF-298 リリースノート整備                       3d
rookpf / theme                             子 2 · 完了 2
parked  次リリースの日程確定待ち
```

- **子 rollup**: 既存の集計カラム (total / done / aborted / 非終端,
  `store.go:192`) に **awaiting カウントを 1 本追加**し、「⚠ N」で質問持ちの
  子の存在を親の行に上げる。§2.4 の ask 不可視への一覧側の答え
- 副産物: 可視性が queue 述語から切り離れるので、「Shape 後に suggestion が
  消えて card が一覧から消える」既知問題は構造ごと軟化する (card は一覧に残り
  続け、沈むだけ)

### 3.6 部品 F: 述語・通知・khi との境界 (何が変わり、何が不変か)

| 対象 | 扱い | 根拠 |
|---|---|---|
| `"triage"` フィルタ (`store.go:448`) | 述語は**不変**・名前は rename (PR-4) | **status 名ではなく述語名** (nose の「triaged は無くなったのに変」への答え)。中身は `type='card' AND status IN ('parked','working')` = 「生きている card」で、旧 triaged 状態とは別物。外部から "triage" を送る呼び手は無い — `boid card list` が status 無指定のとき **server 側が埋める内部 default** (`card_read.go:135`) で、khi も無指定で呼ぶ (khi `app/trigger.py`、実物確認済み)。名前は triage task 時代の遺物 (card モデル整理 PR-3 wire rename の取り残し)。述語の中身 (khi の sweep 対象) は変えずに `"cards_live"` 等へ改名し、明示指定の互換 alias として旧名を当面受ける |
| notifySuggestionArrived (`queue_notify.go`) | **不変** | suggestion 付与イベント駆動で queue 述語に依存しない (実装確認済み) |
| khi 側 (suggestion の書き方・verb 語彙) | **不変** | 本再構築は表示 + updated_at のみ |
| attrs_set の urgency 語彙検証 (`workflow_card.go:289`) | **不変** | khi の書き込み契約。語彙はリテラル別持ちで、queue.go の定数削除に巻き込まれない |
| `"queue_next"` | 削除 | UI (Queue タブ) 専用。外部送信者はコード上に無い (grep 済み — server default は "triage" 側、CLI は値を焼き込まない)。API 直叩きで status=queue_next が来た場合の応答だけ実装時に決める (論点 3) |
| `"queue"` superset / `"done_triage"` | **削除済み (確認)** | 既に存在しない — suggestion 実装 PR-2 で削除済み (`card_handler.go:50` のコメントが記録)。穴 6 の棚卸しはこれで完了 |
| 再帰 CTE (open_descendants / open_ancestors) | 削除 | ツリー撤廃で存在理由が消える |
| BuildTreeItems 族 + task_tree.templ | 削除 | 同上 |
| urgency | 表示から落とす (確定寄り) | 読み手の実測は論点 2 に記載 — 残るのは `boid observe` の表示 1 行と CardView の projection のみで、khi は書くだけで読み返さない (実物確認済み) |

### 3.7 呼び出し関係 (部品の依存)

```
部品 B (bump) ──→ 部品 E (一覧: ソートが bump に依存)
部品 A (メタ規約) ──→ 部品 C (card 詳細) / 部品 D (exec 詳細) / 部品 E (行フォーマット)
部品 C/D の子表示 ──→ 部品 E の rollup (同じ「子の生 status」を詳細は列挙、一覧は集計)
```

一覧より詳細を先に作る (nose 決定)。理由: 現行カオスの半分は「もう片方の実体の
ための埋め草」で、実体分割 (C/D) が終わるとメタ規約 A が各画面で自然に確定し、
一覧 E はその縮小版を行に載せるだけになるから。

## 4. 実装順 (PR 分割)

1. **PR-1: 詳細ページの実体分割** — card / exec の 2 レイアウト + メタ 2 段
   (部品 A/C/D)。遷移機能は等価のまま表示のみ変更。死骸掃除 (Jobs 死骸、
   jobs 配線、dependencies コメント) を同梱
2. **PR-2: 子表示の統合** — card 詳細の統合ライフサイクルビュー (部品 C-2) +
   exec root 詳細の子ツリー (部品 D-2) + awaiting ⚠ / 質問直リンク
3. **PR-3: updated_at bump** — daemon 側 (部品 B)。attrs_set side-effect の
   suggestion 判定 + child_closed。race コメントの書き換えとテスト
   (「observed だけ畳んでも updated_at が動かない」を pin するテストを含む)
4. **PR-4: 一覧再構築** — タブ / ツリー撤廃、全状態 + ページネーション、
   updated_at ソート、行フォーマット、awaiting rollup カラム、削除
   (CTE / queue_next / BuildTreeItems / task_tree.templ)、述語棚卸しの確定

依存: PR-3 → PR-4 (ソートが bump 前提)。PR-1 / PR-2 は独立に先行可。

## 5. 開いている論点

(1・2・3・7 は 2026-08-25 の nose コメントと実測で解けた。経緯ごと残す)

1. ~~card の Description の置き場~~ → **決定: 表に出す** (nose: 「ちょうど不便だと
   思っていた。description は大事なのに何クリックかしないと表示できない」)。残るのは
   折り畳みの既定だけ — 取り込み内容は 64KiB まであり得るので、長文は先頭 N 行 +
   展開 (実装時に調整)
2. ~~urgency の完全落とし~~ → **実測済み、表示から落とすは安全確定。** boid 側の
   読み手: queue_next の ORDER BY (`store.go:509`、queue_next ごと削除)、
   UrgencyRank + 語彙定数 (`orchestrator/queue.go`、**production 呼び手ゼロ** —
   コメントとテストのみ、同時削除可。attrs_set の語彙検証はリテラル別持ちで無傷)、
   ツリー行のバッジ (`api/tree.go:124` + `task_tree.templ:260`、ツリーごと削除)、
   `boid observe` の表示 1 行 (`cmd/observe.go:67`)、CardView.Urgency
   (`card_read.go:178`)。khi 側 (実物確認済み): **書くだけで読み返さない** —
   capture verb で必須 (S-11: 起票と分けると打ち忘れる、2026-08-23 実測 8 件中
   5 件)、`urgency` verb で更新、status 非依存の 1 本 (`write.py:57` — 旧 someday
   据え置き分岐は削除済み)、resolve_identity 応答は urgency を返さない設計
   (`record.py:262`)。残: khi の capture 必須契約 (urgency を書く運用) 自体を
   緩めるかは khi 側の別課題として切り離す
3. ~~queue_next / queue / done_triage の外部利用棚卸し~~ → **ほぼ完了。**
   queue / done_triage は既に存在しない (suggestion 実装 PR-2 で削除済み)。
   queue_next の外部送信者も無い (§3.6)。残るのは API 直叩きで status=queue_next
   が来たときの応答 (エラーで案内 or 空) だけ
4. **ページネーション方式。** 現在の件数規模なら LIMIT/OFFSET で足りる見込み。
   updated_at DESC は挿入で順位が動くため厳密な連続性が要るなら keyset だが、
   「2 ページ目を見る頻度は低い」想定でまず OFFSET
5. **SSE JS の外部化を PR-1 に含めるか。** 含めると PR-1 が太る。表示等価の
   検証を優先し、PR-1 では現状維持・別 PR に切り出しも可
6. **3 行目 (動きの段) の状況テキストのソース。** suggestion がある行は reason。
   無い行の有力候補は card = khi の summary attr、exec = 直近の job / action
   label (モック v2 はこの形で試作)。表示は帳簿の読み出しだけなので bump 方針
   (§3.2) とは独立 — summary を出しても並びは動かない。正式決定はモック評価と
   運用で
7. ~~exec タスクの親子 (supervisor 系) もツリーを失う~~ → **決定: root の詳細が
   子ツリーを持つ** (§3.4-2。nose: 「ルートの詳細がツリーを持っていてくれた方が
   ユーザーに優しい」)
8. **子一覧統合ビューの端ケース。** 台帳 closed と子タスクの終端 (done/aborted)
   の突き合わせ、drop された子の表示、多段 (孫) をどこまで出すか (§3.3-2)

## 6. レビュワー採点表 (yes = 合格。根拠を引けない yes は no 扱い)

| # | 問い | y/n |
|---|---|---|
| 1 | ゴール (§1) は As Is の症状列挙 (§2.4) と独立に立っているか | |
| 2 | 表示要素それぞれに「誰が読み、何に答えるか」が書かれているか (§3.1, §3.3) | |
| 3 | go と working の区別が UI 上で保存されているか (§3.1 の辺ラベル) | |
| 4 | done card への reopen suggestion に、デフォルト設定の一覧から到達できるか (§3.5 全状態表示 + bump 浮上) | |
| 5 | 子の ask に一覧から 2 タップ以内で到達できるか (§3.5 rollup → §3.3 直リンク) | |
| 6 | updated_at bump が observed churn を除外しているか (§3.2) | |
| 7 | bump の実装形が skipTaskUpdate race と両立する根拠が書かれているか (§3.2) | |
| 8 | khi 側の変更が不要であることが契約として明記されているか (§3.6) | |
| 9 | `"triage"` フィルタが無変更か (§3.6) | |
| 10 | escape hatch (人の直接操作で全遷移) が維持されているか (§3.3 action bar) | |
| 11 | 削除対象が名前で列挙され、到達可能性 (外部利用の棚卸し) の検算手順があるか (§3.6, 論点 3) | |
| 12 | モバイル幅で「判断に要るもの」(動きの段 + 子の要注意) が first view に収まる並びになっているか (§3.3 表示優先順位, §3.4) | |

## 7. 実装ノート (Sonnet 実装 + Opus レビュー向け)

実装体制: 各 PR を独立の boid task として dispatch (実装 Sonnet)、レビューは Opus。
受け入れ条件の共通部分は「全既存テスト green + 各 PR に挙げる新規テスト + 挙動
差分の列挙 (PR 説明に)」。templ を触る PR は `templ generate` を**リポジトリ
ルートから**実行すること (生成物の drift は CI "Unit tests" が唯一のゲート)。
画面構成の正は本 doc §3 と「判断の場モック」artifact — 迷ったらモックに合わせる。

### PR-1: 詳細ページの実体分割

- 触る場所: `web/templates/tasks.templ` (TaskDetail / TabsSection / TabPanel /
  ActionBar / StatusSection を card 用・exec 用の 2 系列に分割)、
  `internal/api/web.go` の TaskDetail / TaskDetailFragment
- 分岐軸は `task.Type == "card"` (STI 列)。machineFor と同じ
- **罠 1: HX-Request のタブ swap 分岐** (`web.go:482` — `#tabs` を outerHTML
  swap)。card レイアウトにはタブが無いので、card は全面レンダに一本化するか
  fragment 対象を再定義する。どちらにしたかと挙動差分を PR 説明に列挙
- **罠 2: SSE 更新** (`kind=status` / `timeline` fragment + templ 直書き JS の
  refresh 対象 id)。両レイアウトで生かす。描画先 id を変えるなら JS も追随。
  JS の外部 asset 化は含めない (論点 5 — 別 PR)
- 死骸掃除を同梱: TaskDetailJobsSection、`jobs` 引数の素通し配線、fragment
  `kind=jobs`、"dependencies" コメント (`tasks.templ:1180`)
- 受け入れ: 遷移機能等価 (全 verb ボタンの POST 先・payload 不変)、
  既存テスト green

### PR-2: 子表示の統合

- card 詳細: `parent_id` で子を 1 回引き (`store.go:911` の ListChildren 相当)、
  TaskRef で detail blob の台帳と突き合わせ。チップ規則は §3.3-2 (台帳
  dispatched のときだけ生 status で置換)。awaiting の子は
  `orchestrator.GetAwaitingPayload` で qid を取り質問ページへ直リンク
- spec 折り畳みは description のみ (`tasks.templ:494` の instruction 表示を
  落とす)
- exec root の子ツリー: 取得は app 側の再帰 (ListChildren を深さ優先。規模は
  小さい)。**SQL の再帰 CTE は足さない** (一覧から消す CTE を詳細で復活させない)
- 論点 8 の既定 (実装が先に必要なぶんだけ決める): 台帳と生 status が矛盾したら
  生 status を優先表示、TaskRef 欠落など突き合わせ不能なら台帳のみ
- 受け入れ: awaiting の子を持つ card のフィクスチャで ⚠ バッジと質問リンクが
  描画されるテスト

### PR-3: updated_at bump

- 触る場所: `internal/api/workflow_action.go` の skipTaskUpdate ブロック
  (`:401` 周辺)。suggestion キーを畳み verb が非空のとき + child_closed の
  記録時に、**同一 Tx 内で** `UPDATE tasks SET updated_at = ? WHERE id = ?`
  (status を運ばない単発文 — §3.2 の race 両立根拠)
- child_closed の記録個所は `MarkDetailChildClosed` の呼び出し側 (子の終端処理)
  を grep して特定してから着手
- **コメント修正必須**: 「the task row genuinely has nothing new to persist」
  (`workflow_action.go:384`) は本 PR で嘘になる — 理由ごと書き換える
- pin するテスト: observed のみの attrs_set で updated_at が動かない /
  suggestion 付与で動く / null-clear (撤回) で動かない / attrs_set が status を
  書き戻さない (race 挙動不変)

### PR-4: 一覧再構築 (PR-3 マージ後に着手)

- `internal/orchestrator/store.go`: ORDER BY を全ビュー
  `updated_at DESC, id` へ / `"queue_next"` 分岐削除 + `queue.go` の
  UrgencyRank・語彙定数削除 (**attrs_set の urgency 検証は
  `workflow_card.go:289` のリテラルで独立 — 巻き込まない**) /
  taskChildCountCols (`:192`) に awaiting カウントを 1 本追加 / LIMIT+OFFSET /
  `"triage"` → `"cards_live"` rename + 旧名を互換 alias として受理
- Web 側: filters.templ のタブ撤去 → 「アクティブのみ」トグル 1 個 (既定は
  全状態) / `task_tree.templ` + BuildTreeItems 族削除 / 3 行 row (§3.5)。
  行の suggestion reason・summary は `ListTaskTriageByTaskIDs` のバッチで
  detail blob から取る (既存の BuildTreeItemsWithSuggestions と同じ経路)
- 再帰 CTE (open_descendants / open_ancestors) 削除
- 受け入れ: `"triage"` 述語の中身 (type='card' ∧ parked∪working) 不変を
  テストで pin / status=queue_next を明示指定した API 呼び出しの応答を決めて
  テスト (論点 3 の残り) / ページネーションの境界テスト
