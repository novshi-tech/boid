# suggestion 状態遷移化 実装計画

2026-08-24 起案。設計は `suggestion-as-state-transition.md` (確定版、PR #984) — 本 doc は
それを**実装する順番と契約**を定める。設計の議論は蒸し返さない。設計 doc と食い違ったら
設計 doc が正。

対象 repo は 2 つ:

- **boid** (github.com/novshi-tech/boid) — PR-B / PR-1 / PR-2
- **khi** (bitbucket.org/Aolani-ondemand/khi-task-collector) — PR-K

実行スタイル: 別セッションで一気に実装する。PR は**直列**に積む (interface を変える PR を
並列にすると git は MERGEABLE でも意味衝突する)。各 PR は単体で CI green にする。
**本番デプロイは最後のカットオーバーで 1 回だけ** — 途中の PR が main に入っても
daemon は旧のまま動き続ける (デプロイは手動 `deploy-container.sh` のみ)。

## 0. 実行セッションへの規律

- **必ず branch を切ってから作業する。** main 直 commit の前科がある
- TDD: テストを先に書き、失敗を確認してから実装 (CLAUDE.md)
- コミット prefix: `feat:` / `fix:` / `refactor:` / `test:`、doc は `docs(plans):`
- **やらないこと**: action 履歴の rename (triage_done 行はそのまま) /
  ingestion identity 機構のスキーマ変更 / 自動 accept (§3.9、将来) /
  完了検知の能動監視 (穴 8、別課題) / khi-oracle スキルの分割 (別宿題)
- templ を触ったら `templ generate` は repo ルートから
- 新パッケージを internal/ に足したら architecture allowlist 登録 (落ちるジョブ名は
  "Unit tests")

## 1. PR 列と依存

```
PR-B (boid: machine 分離、純リファクタ)
  ↓
PR-1 (boid: card 機械 v2 + accept 適用 + sweep 整理)   ← コア
  ↓
PR-2 (boid: 一覧・通知・Web UI の suggestion 駆動化)
  ↓
PR-K (khi: verb 刷新・canonical 削除)
  ↓
カットオーバー (運用作業。コードではない)
```

順序の根拠と、崩すと壊れる箇所:

- PR-B が先: 挙動不変の土台。これ無しで v2 を書くと §2.5 の場外規律ごと書き換える
  ことになり diff が読めない
- PR-1 は機械と mover (sweep) が不可分: wake_* rule を消すと SweepWake が壊れるので、
  sweep の整理は同じ PR に入れる
- PR-2 は PR-1 の「適用型 answered」に依存: 先に UI だけ変えると accept が no-op の
  まま適用型に見える (現行バグの再生産)
- PR-K は boid 側 API 契約 (suggestion スキーマ・status 語彙) の確定後
- カットオーバーは全部揃ってから 1 回

## 2. PR-B: machine 分離 (純リファクタ)

**目的**: `NewMachine()` 1 個を `NewExecutionMachine()` / `NewCardMachine()` に分割し、
task_triage sidecar の有無で選ぶ。**rule の内容・名前・挙動は一切変えない。**

やること:

- `internal/orchestrator/machine.go` の rule を 2 分割。card 側に行くもの:
  captured/triaged/parked/ready/working を FromStatus に持つ全 rule、wake_*、dispatch、
  triage_done、reopen_triaged、drop、reopen (dropped→triaged)、attrs_set 系ループ。
  execution 側: start/done/fail/reopen (done/aborted→executing)/ask/answer/abort、
  job_failed、progress/done_request/fail_request、auto 遷移 3 本
- 帰属に注意する共有点: done/aborted は両機械に存在する状態。`reopen: done→executing`
  は execution 側、`reopen_triaged: done→triaged` は card 側。abort は execution 側のみ
  (card は abort を持たない — 現行と同じ)
- api 層に選択関数 `machineFor(task) (*StateMachine, error)` を作る。判定は
  task_triage sidecar の有無 (`GetTaskTriage` の ErrNoRows)。reopen ルーティング
  (`resolveReopenVariant`) が既にやっている判定と同じもの — 共通化する
- **`ApplyAction` の IsManualAction チェックは task ロード後に移す**
  (`workflow_action.go:42` — 現在は task ロード前に `DefaultMachine()` で判定して
  いる)。これにより「存在しない task + 未知 action」のエラーが 400 → 404 に変わる
  ケースが生じる。この種の差分を PR 本文に列挙すること
- `DefaultMachine()` の呼び出し元を全部棚卸しして (grep)、それぞれに正しい機械を配る。
  coordinator / dispatch loop / NotifyTask / CompleteJob は execution 機械で足りるはず
  だが、目視で確認する
- `resolveReopenVariant` は本 PR では**残す** (改名・意味変更をしない縛り)

受け入れ条件:

- 既存テスト全緑 (`go vet && go test ./...`、machine_test は 2 分割)
- machineFor の選択テスト (sidecar 有無 × 両機械)
- エラー応答が変わり得る箇所の列挙が PR 本文にあること —
  「挙動を 1 ミリも変えない」ではなく「意味を変えない」が基準 (設計 doc §3.8)

## 3. PR-1: card 機械 v2 + accept 適用 + sweep 整理 (コア)

**目的**: card 機械を遷移表 v2 (設計 doc §3.2) に置換し、accept を適用に繋げ、
機械遷移を capture だけにする。

### 3.1 card 機械の rule 置換

- 状態: parked / working / done / dropped の 4 つ
- Manual rule: `go: parked→working` / `working: parked→working` /
  `drop: parked→dropped` / `park: working→parked` / `done: working→done` /
  `reopen: done→parked` / `reopen: dropped→parked`
- 非遷移 Manual rule (attrs_set / child_added / child_specced / child_dropped /
  noted / answered) の FromStatus 列挙は {parked, working} に縮む (captured/triaged/
  ready が消えるため)。done への attrs_set 着地 (I-5b/I-5c、attrs_set_done.go) は
  存続 — khi が終端 card に観測を書く経路は残す
- 削除する rule: wake_triaged / wake_ready / wake_working / dispatch / triage_done /
  reopen_triaged / ready / triage
- `resolveReopenVariant` を削除 (card 機械の reopen が直接 done→parked を持つので、
  variant 分岐そのものが不要になる)

### 3.2 押し込み防御 (経路/actor ベース、穴 11)

card 機械の遷移 action (go/working/park/drop/done/reopen) は Manual:true になるため、
そのままだと khi が gateway 経由で `boid action send --type done` を押し込める。防御:

- ApplyAction で、card 機械の**遷移を起こす** action は
  `actor == ActorHuman` (Web UI / CLI) の場合のみ受理。accept 経由の適用は daemon が
  human 文脈で発行するので通る
- 非遷移 action (attrs_set 等) は従来どおり khi から打てる
- **着手前確認**: gateway 経由 action_send の actor 値が何になるか
  (`actor.go` / `boid_executor.go` の `BoidOpActionSend`)。ActorHuman になってしまう
  なら区別の口 (経路フラグ) を先に作る
- **escape hatch の担保** (設計 doc §3.2): この防御が人の直接操作を塞がないことを
  テストで固定する — 「全遷移が Web UI / CLI から suggestion 無しで打てる」

### 3.3 suggestion スキーマと accept 適用

- `attrs.suggestion = {verb, reason, params?}`。verb ∈ {go, working, park, drop,
  done, reopen} を daemon が検証 (boid 語彙なので境界を越えない)。park は
  `params.wake_at` / `params.wake_task_id` を運べる (park には wake 条件が要る)
- `answered {answer: accept}`: verb を読み、対応する遷移を適用してから suggestion を
  消す。action log には answered + 遷移 action の 2 行が残る (決定 13: event 追記が正)
- `answered {answer: reject}`: 現行どおり suggestion を消すだけ
- **accept(go) の順序**: (1) specced 子の task 化 + dispatch (現 `Dispatch` の中身) →
  (2) 成功後に parked→working を commit。失敗時は card は parked のまま・suggestion
  温存・呼び出しに同期エラーを返し、`dispatch_error` action を記録する。現行の
  「ready commit → 別 Tx で dispatch」と Tx 単位が変わる — ここが本 PR で一番設計の
  要る箇所。子 task の作成と親の遷移を同一 Tx にできない場合は「子作成成功 → 遷移
  commit、遷移 commit 失敗時は子を abort」の補償順でもよい。選んだ方式を PR 本文に書く

### 3.4 sweep の整理

- 削除: `SweepTriage` (auto-done) / `SweepReopen` (auto-reopen) /
  `auto_reopen.go` 一式 (フラップ予算・エピソード計数) / 決定 16 watchdog
  (`SweepCanonicalSourceBreaches` / `MissingCanonicalSourceGuidance` /
  `logCanonicalSourceBreaches`)。テストも一緒に消す
- `SweepWake` → **wake_due 事実記録係に改造**: ShouldWake 成立で `wake_due` action
  (非遷移、daemon self-record、child_dispatched と同じ書き方) を card に追記し、
  **tt.WakeAt / WakeTaskID を消す** (条件は消費される — 毎分連打しない冪等性を
  これで担保)。遷移はしない
- `QueueSweepLoop` は SweepWake (改) だけを呼ぶ形に縮む

### 3.5 status 語彙と入口

- `KnownTaskStatuses` に captured/triaged/ready を **legacy として残す** (読み取り・
  GC・表示用)。card 機械はこれらからの遷移を持たない。洗い替えで実データからは消える
- capture の初期 status: `task_resolve_or_capture.go:162` の captured → **parked**
- `task_create.go` の initial_status 語彙: captured/triaged を落とし
  pending / parked にする (破壊変更 — khi と同時カットオーバーなので可)

### 3.6 テスト

- 遷移表 v2 の網羅テスト (全辺 + 不許可辺の拒否)
- accept 適用: verb ごと、go 失敗系 (dispatch 失敗で parked のまま + エラー)、
  park params の wake 条件書き込み
- 防御: khi actor の遷移 action 直打ちが拒否される / 人の直接操作は全遷移通る
- wake_due: 記録される + WakeAt が消費される + 遷移しない

## 4. PR-2: 一覧・通知・Web UI の suggestion 駆動化

### 4.1 queue 述語 (store.go)

- **suggestion_verb を task_triage の列に昇格する** (db/migrate に migration 追加)。
  根拠: TaskTriage の doc comment 自身が「queue 述語は列にする」原則を書いている
  (urgency / wake_at がそうである理由と同じ)。attrs_set の fold 時に detail から列へ
  書き込み、answered の fold で列を消す — 書き手は fold 側の 1 箇所
- `"queue_next"` → `suggestion_verb IS NOT NULL` に置換。urgency は ORDER BY のみ
  (可視性を失う — 設計 doc §3.6)
- `"queue"` (広い superset) は削除候補 — pin しているテスト
  (`TestListTasks_Queue_ReturnsExactlyPreExecutionSet`) ごと整理。Web UI が使って
  いなければ消す、使っていれば "triage" に寄せる
- `"triage"` → `('parked','working')` + legacy 3 値 (洗い替え前の残骸が読めるように)
- `"done_triage"` は SweepReopen 専用だったので削除

### 4.2 通知 (queue_notify)

- rule 4 (queue 入り通知) を「suggestion が付いた」通知に置換。urgency 昇格通知
  (`notifyUrgencyRaised`) は並び順属性になった urgency に意味が残るか判断し、
  残さないなら削除

### 4.3 Web UI

- queue タブ = suggestion 付き card。verb と reason を主表示に (accept が「何を承認
  するのか」が読める形)。**go の accept は実行承認として重い見た目にする** (Go ボタン
  と同格)
- parked / working の一覧面を残す (escape hatch の目視面 — 設計 doc §3.6 の
  「queue は唯一の窓ではない」)
- 全 manual 遷移の直接操作ボタン (AvailableActions が card 機械 v2 から正しく出る
  ことを確認 — done/park/drop/reopen/go/working)
- accept/reject ボタンは適用型の文言に (「accept しても動かない」時代の見た目を残さない)

受け入れ: templ generate + 全テスト緑 + UI の手動確認項目リストを PR 本文に。

## 5. PR-K: khi 側 (khi-task-collector)

- `app/write.py`:
  - `park` — 直接 action 打ち → suggestion 化 (params に wake_at / wake_task_id)
  - `wake` verb 削除
  - `manual` → `working` suggestion に改名
  - `go` / `done` / `reopen` suggestion 新設
  - `drop` は現行 suggestion のまま (適用は boid 側で変わる)
  - `canonical` verb 削除 → `adapters/jira_issue.py` / `domain/canonical.py` ごと削除
  - someday 据え置き分岐 (write.py:620) と「someday は captured に留める」分岐
    (write.py:525) を削除 — urgency の可視性兼務が消えるため
- `domain/screen.py`: `wake_due` action 種別を篩に通す
- `app/trigger.py`: `open_triage_task_ids` は `triage_list()` 追従なので boid 側
  "triage" フィルタ変更に自動追従 — 前提をテストで固定
- instruction / prompt: done 判断基準 (このワークスペースでは何を完了の証拠と
  するか)、suggest の運用 (1 枚 1 提案・最新が勝つ)、verb 一覧の更新
- khi/tests 更新。**外部契約は「相手 (boid) が要求する値」でアサートする**
  (送っている値のアサートはバグを固定する)

## 6. カットオーバー (runbook)

1. **事前確認**: `podman ps --filter name=boid-job` — デプロイは実行中 job を全 reap
   する。動いていたら待つか止める
2. **旧 card の手仕舞い**: 現 daemon のまま、全 card (~9 枚) を drop / done へ終端
   させる。working の card は done 経由
3. **identity の解放**: 旧 card に link された identity を unlink する
   (`boid identity unlink` — shim の help に出ないが実在する。link は上書きを拒む)。
   ※ResolveOrCapture が terminal task の identity をどう扱うか次第で不要になる —
   §7 で実装前に確定させる
4. **boid デプロイ**: `deploy-container.sh` 直接実行の 3 点セット
   (COMPOSE_ROOT / CLI_TOKEN / --build) を忘れない
5. **khi デプロイ**: workspace 側の反映。project.yaml を変えた場合は push +
   `boid project fetch` (reload では反映されない)
6. **洗い替え**: khi に再取り込みさせ、card を新規生成 (parked で誕生)
7. **動作確認チェックリスト**:
   - 取り込み → parked で誕生し、khi の suggestion が queue に並ぶ
   - accept(go) → 子 dispatch + working / dispatch 失敗が同期エラーで見える
   - accept(park) → wake 条件が書かれ、成立後 wake_due → khi が再 suggest する
   - done suggest → accept → done / reopen suggest → accept → parked
   - 人の直接操作で全遷移が回る (escape hatch)
   - khi actor の遷移 action 直打ちが拒否される
8. **監視**: 最初の数日は queue と parked / working 一覧を毎日目視
   (穴 7 の楽観の検証。破れたら棚卸し面の設計に戻る)

## 7. 着手前に確定させる点 (PR-1 実装セッションで確定・実装済み)

1. **gateway 経由 action_send の actor 値** — `internal/server/boid_executor.go`
   の `BoidOpActionSend` は常に `orchestrator.ActorTask(TokenContext.TaskID)` を
   スタンプする（`ActorHuman` にはならない）。khi は trigger job 経由で呼ぶため
   `TaskID` が空になり、actor は文字列 `"task:"` になる — prefix ではなく
   `actor == orchestrator.ActorHuman` の等値判定で防御できる（§3.2 実装済み、
   `internal/api/workflow_action.go` の押し込み防御）。
2. **ResolveOrCapture の terminal-task 挙動** — 本 PR のスコープでは未確認のまま。
   `ResolveIdentity` が終端(done/dropped) card の identity をどう扱うかは
   カットオーバー runbook 実施時（§6 手順3）に確認する。PR-1 は
   `ResolveOrCapture` の初期 status を `captured` → `parked` に変えただけで、
   identity 解決ロジック自体には手を入れていない。
3. **wake_due が khi のトリガを起こす配線** — **事実誤認だった箇所を訂正**:
   khi のトリガは取り込みシグナルで発火するのではなく、`internal/api/trigger_loop.go`
   が `now - 直近 run の started_at >= every` の経過時間だけで判定するポーリング型
   （イベント購読の口は存在しない）。したがって `wake_due` 側からトリガを能動的に
   起こす配線は不要 — khi は自分のポーリング周期の中で `wake_due` action
   （またはそれが記録された card の状態）を読みに来る。PR-K 側の対応も不要。
   §3.4 の「事実記録係」は `wake_due` action を記録するところまでが daemon の
   責務で、それを khi がいつ読みに来るかは khi 側のポーリング頻度に委ねられる。
4. **accept(go) の Tx / 補償方式** (§3.3) — **補償順を採用**（同一 Tx にはしない）。
   `TaskCreator.CreateTask` が自前で `*sql.DB` を掴んで末尾で `ApplyAction("start")`
   の別 Tx を開くため、`db.go` の `SetMaxOpenConns(1)` の下でネストした Tx を
   開くとデッドロックする。実装は `internal/api/workflow_triage.go` の
   `acceptGo`: ① 子タスクの作成 + auto-start（非トランザクショナル）→
   ② 成功したら 1 つの Tx で parked→working 遷移 + child_dispatched 記録を
   コミット → ③ 子作成失敗時は card を parked のまま・suggestion 温存・
   `dispatch_error` action 記録・同期エラー返却 → ④ 遷移 Tx 失敗時は作成済みの
   子を best-effort abort + `dispatch_error` 記録 + 同期エラー。
5. **suggestion_verb 列の fold 書き込み点の網羅** — PR-1 では suggestion を列に
   昇格しない（列昇格は PR-2 のスコープ）。JSON blob (`detail.attrs.suggestion`)
   のままなので書き込み点は従来通り 2 箇所のみ: 書く側は `attrs_set` の fold
   （`orchestrator.FoldDetailAttrs`、`validateSuggestionAttr` で verb を検証
   してから）、消す側は `applyAnsweredSideEffect`
   （`orchestrator.StripDetailAttrs(..., "suggestion")`）。他に書き/消しする
   経路は無い。

## 8. スコープ外 (この一気実装に含めない)

- 自動 accept (設計 doc §3.9)
- 完了検知の能動監視 (穴 8) — done suggest の口はこの実装で開くが、khi が外を毎巡
  見に行く仕組みは別課題
- triage_done → done の action 履歴 rename (新規書き込みは PR-1 から `done`。
  履歴はそのまま)
- khi-oracle スキルの分割
