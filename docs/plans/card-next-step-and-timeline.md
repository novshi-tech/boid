# Card の次の一手 — 判断処理の統合と Web UI の再構成

2026-09-05 起案。ステータス: **設計方針は対話で合意、実装計画は提案。未実装。**

## 1. 目的と位置づけ

card を「次の一手を考え、実行し、結果を受けてまた考える場所」として揃える。
人が指示しても内部イベントが起きても同じ判断処理を使い、その経緯を card の一画面で追えるようにする。

実運用から出た friction:

- working 中に次の子の仕様を作れても Go できない。
- suggestion の語彙に Action と状態名が混在する。
- Shape の固定された入口・指示が、ユーザーのやりたいことと合わない。
- Shape と Sweep の判断は調査・子の仕様作成・提案という同じ仕事を重複して持つ。
- card や子の内部イベントが定期 Sweep を待ち、次の判断まで間が空く。
- 子の仕様と実行結果、子の表示とタイムラインが分断されている。
- 一覧で子が実際に実行中か分からない。長い履歴では日付も分からない。

元の [cross-project-issue-triage.md](cross-project-issue-triage.md) の成功条件
「提示された課題に応えていくだけで仕事が進む」を引き継ぐ。現在形の前提は
[suggestion-as-state-transition.md](suggestion-as-state-transition.md)、
[card-model-cleanup.md](card-model-cleanup.md)、
[signal-driven-review.md](signal-driven-review.md)、
[boid-internal-signal-inbox.md](boid-internal-signal-inbox.md)。
UI については [webui-detail-list-redesign.md](webui-detail-list-redesign.md) の後続となる。
本 doc と衝突する将来仕様は本 doc を優先する。既存の実装を説明する際は区別する。

## 2. 合意した方向と範囲

1. suggestion は **Action** を指定する。状態は Action を適用した結果。
2. working 中も Go できる。
3. card 直下の未実行・実行中の作業子は、合わせて一つまで。完了した子の履歴は複数持てる。
4. Shape と card ごとの自動判断を共通化する。対話とタスク dispatch の両方を提供する。
5. 外部 Sweep は新規 card の作成または既存 card への情報の振り分けまで。
6. card ごとの肉付け・子の仕様作成・提案は、内部イベントやユーザーの指示で起動する判断処理が行う。
7. 内部イベントは周期 Sweep を待たずに反応する。
8. 対話中はその card の自動判断を抑止し、到着した要求は終了後に扱う。リアルタイム注入は不要。
9. card 詳細はタイムラインを中心に統合。最新順、初期10件、古い履歴を追加表示。
10. 指示入力は上部。現在の提案・実行中の子は履歴件数で隠さない。日付セパレータを入れる。
11. UI のボタン・説明・状態表示・エラー・空状態は **英語**で統一する。

初期の「working 中も全 specced 子をまとめて Go」という合意は、後の単一作業子モデルで置き換えた。
複数子の選択 UI や順序指定 UI は作らない。並列化は必要な実行タスクの配下で行う。

今回の非対象: 自動 accept、汎用 DAG scheduler、複数ユーザー協調編集、
全 connector の刷新、対話中のイベント注入、実行タスク詳細全体の作り直し。

## 3. Action と「次の一手」

### 3.1 語彙

Action を英語の動詞にする方針は合意済み。以下の具体名は対話で提示した採用案。
card の状態名は変えない。

| UI / wire Action 案 | 意味 | 適用元 → 結果 |
|---|---|---|
| Go / `go` | 用意した作業子を実行する | parked → working、working → working |
| Start / `start` | 子を起動せず人が着手する | parked → working |
| Park / `park` | 保留する | working → parked |
| Complete / `complete` | 課題を完了にする | parked / working → done |
| Drop / `drop` | 取り下げる | parked → dropped |
| Reopen / `reopen` | 再開する | done / dropped → parked |

`working` Action を `start`、`done` Action を `complete` に変更する。
execution 型の既存 `start` / `done` は変更しない。型で適用する機械を選ぶ。
Go の補足は `Run the next task`、Start は `Start working manually` など英語にする。

状態の自己遷移も Action として履歴に残す。既存の「遷移可能なら適用可能」という判定を、
子の実行可否も含む Action の契約へ拡張する。直接 Go と suggestion accept は同じ処理を通す。
人の承認ゲートは維持する。判断タスクを起動する `Run` は作業子の `Go` と区別する。

### 3.2 作業子の不変条件

- card 直下の `open`、`specced`、`dispatched` の合計が最大1件。
- 実行 task が pending / executing / awaiting の間も枠を占める。
- closed の子は枠を占めない。ただし成功・中止・取り下げの違いは履歴表示で失わない。
- Go は1件の specced 子にだけ作用する。子なし Go は拒否し、手作業には Start を使う。
- 二重クリック・再試行で同じ子を二重生成しない。既存の冪等生成を再利用する。
- 実行済み仕様を後から別の仕事へ書き換えない。次の仕事には新しい子 ID を使う。
- 将来の構想は description に置けるが、実行可能な複数の予定子としては保存しない。
- 制限は card 直下だけ。execution task の子や並列実行には適用しない。

UI だけで制限せず、child_added / child_specced / task create 等の全書き込み口を棚卸しし、
同じ transaction 内の検査で保証する。JSON 内の子と実際の parent_id の対応も検査対象。
子が終端した後の reconcile は既存処理を再利用する。

**判断用の task / session は作業子ではない。** card に対象として関連付けるが、
作業子枠・Go・作業子の完了判定には数えない。これを親子関係だけで表現しない。

## 4. 判断処理の統合

### 4.1 共通の仕事と入口

共通化するのは prompt の固定文ではなく、文脈の渡し方、利用できる操作、結果の記録契約。
card の最新 description、identity、履歴、現在と過去の子・成果、起動理由を読み、
必要なら外部を照会する。ユーザーの指示に応じて調査・整形・仕様作成・提案を行う。
毎回必ず子や suggestion を作る義務はない。対応不要という判断も正常な結果。

| 入口 | 入力 | 実行形態 |
|---|---|---|
| Discuss | card + 任意の指示 | 既存 session 基盤による対話 |
| Run | card + カスタム指示 | 既存 execution task 基盤による自律判断 |
| 内部イベント | card + 永続化された起動理由 | 同じ自律判断 |

workspace 固有の判断方針は一つの設定から参照する。共通スキルには読み書きの機構を置き、
特定サービスへの調査や特定の成果物作成を Shape の固定 bootstrap に埋め込まない。
session と task の harness/model は既存の設定機構を利用し、無理に同一プロセス形式にしない。

現在の `write.py` は全 verb に signals を要求し、書き込み後に ack する。
新契約では **ユーザー要求にも永続的な request ID を付ける**。Signal がない要求を扱え、
Signal 由来の場合だけ対応する ack を行う。偽の Signal ID を作って既存契約に合わせない。
task-less session の書き込み元・対象 card・workspace 認可もこの契約で追えるようにする。

### 4.2 外部 Sweep との分担

```text
External connector → Signal inbox → Sweep: screen / capture / link
                                             ↓ durable request(card)
Card creation / child completion / user input → card judgment → description / next task / suggestion
                                                                    ↓ human Go
                                                               execution task
                                                                    ↓ outcome
                                                               card judgment
```

Sweep は「この情報は課題か、どの card の続きか」を判断する。
作成時の最小情報は title、identity/source の入口、起票理由と引き継ぐ文脈。
作成または link が成功したら、その card の判断要求を確実に残してから元 Signal を ack する。
既存 card への続報も同じ経路。後段に渡す調査結果を捨て、同じ調査をやり直させない。

### 4.3 リアクティブ起動の最小構成（実装提案）

新しい常駐 agent や汎用イベント基盤は作らない。daemon に card 単位の小さな起動調整を置く。
既存 Signal 永続化を使える部分は使うが、現行の best-effort な action → Signal だけでは
唯一の起動理由を失えるため、**判断要求の耐久性は別途保証する**。

実装候補は card ID、要求 ID/世代、理由、起動元、task/job ID を保持する小さな永続 store。
要求追加を事実の書き込みと同一 transaction に載せ、commit 後に dispatcher を通知する。
再起動・通知欠落時には未処理要求を再走査する。定期走査は復旧用であり通常起動の待ち時間にしない。
正確な table/field/API 名は PR-3 の設計で決める。

- 一つの card で判断 task / 対話 session は最大一つ。
- 実行開始時に取り込む要求の境界を固定する。実行中の新しい要求は消さず、次回にまとめる。
- 成功時は取り込んだ範囲だけ完了にする。途中失敗では未処理が残る。
- 再実行は既存の差分ガード・冪等性を利用。失敗回数と最後のエラーを確認・再試行できる。
- 作成、外部続報の handoff、子の終端、wake_due、ユーザー指示を初期トリガ対象とする。
- 人による更新・accept/reject のうち判断対象にする action は明示列挙し、自己起動をテストする。
- 判断処理自身の summary / spec / suggestion / ack、細かな進捗は再起動理由にしない。
- 現行の「メタプロジェクト発を全除外」をそのまま使うと Sweep の capture/link まで落ちる。
  外部 handoff は明示的に要求を作り、判断自身の更新と区別する。
- 同じ原因から内部 Signal と直接要求の二本が生じても一回の要求として扱う。
- workspace 認可と card の所属を daemon で検証。LLM の意味判断は workspace に残す。

「リアルタイム」は定期 interval を待たず、commit 後に起動を試みること。
LLM の即時完了や、全イベントごとに別 task を起動することを意味しない。

### 4.4 対話との共存

シングルユーザーで、通常は静止した card に対話を始める。複雑な協調制御は導入しない。
対話中の要求は保持し、自動判断を起動しない。対話へのイベント注入・厳密な処理済み照合はしない。
終了後の再判断が「追加対応なし」になることは許容する。

ブラウザを閉じたことは session 終了と同一視しない。既存 job の終端で占有を解放する。
UI に `End discussion` と既存 session への復帰口を用意し、daemon 再起動時に job の生存と照合する。
判断 task が動いている時の Discuss は、初期版ではその実行へのリンクと待機理由を表示する。
自動キャンセルや途中の対話化は行わない。異常終了・起動失敗で枠が永久に残らないことを保証する。

## 5. Web UI

### 5.1 card 詳細

上から次の順に配置する。

1. タイトル、card 状態、現在の要約（長文は展開）。
2. 共通の指示入力欄と `Discuss` / `Run`。Run が判断を起こすことを説明する。
3. 現在の提案・次の作業子・進行中の判断を示す固定項目。
4. 最新順のタイムライン10件と `Load older`。

固定項目はタイムライン項目と同じ表示部品・同じ ID を使う。独立した子一覧を再設置しない。
履歴に同じ項目を重複表示せず、終了後は通常の時系列位置に戻す。
固定項目は10件の上限に含めない。件数は生 action 数ではなくユーザーが読む項目数。

### 5.2 一つの子を一つの項目として追う

仕様段階から実行中・終端まで安定した child ID の同じ項目を使う。
title、実行先 project、仕様・完了条件、状態、主要な進捗、結果をその場で読めるようにする。
実行 task が作られたら task_ref で結合する。詳細ログ・質問・成果物には既存画面へのリンクを残す。
進捗の細かな action を card の履歴に全展開せず、作業項目の中にまとめる。

タイムラインはユーザー指示、判断結果、提案と回答、作業子、重要な外部変化を表現する。
生 action 履歴は監査用に保持する。既存の execution/TUI 用 status-group timeline を壊さず、
card 用の読みモデルを設ける。過去の spec が履歴だけから復元できない場合は現在の保存情報を使い、
当時の完全な snapshot が存在するかのようには表示しない。

### 5.3 順序・追加読み込み・更新

履歴項目の発生時刻と安定 ID で降順に並べ、cursor で古い項目を追加する。
子の項目は作成時の位置を持ち、活動中だけ固定。終了しただけで履歴全体を並べ替え続けない。
提案の発生や回答などの独立イベントはその時刻を使う。

SSE 更新で指示の入力途中・展開状態・追加読み込み済み履歴・スクロール位置を失わない。
新着と Load older の競合でも重複・欠落を起こさない。親 card に子の状態更新を届け、
既存の parent action だけでは不足する進捗/awaiting の更新経路を補う。

### 5.4 日付

日付が変わる位置と先頭項目の前に `Sep 5, 2026` 形式のセパレータを表示。
通常項目は時刻のみ、固定項目は日付と時刻を明記する。
同じ日を Load older で継ぎ足す場合は区切りを重複させない。
日付境界・時刻は同一タイムゾーン。初期案はブラウザのローカルタイムゾーンを使い、
表示タイムゾーンも確認可能にする。実装前に既存の時刻表示方針と整合させる。
対話画面内は通常どおり新しい発言を下へ追加する。

### 5.5 一覧

card 状態とは別に、唯一の作業子から `Ready to run` / `Running` / `Needs input` 等を表示する。
open で仕様未完成なら `Draft`、task が pending なら `Queued` とし、実行中と誤認させない。
判断中も作業実行中と混同せず `Reviewing` / `Discussing` を示す（ラベルは実装時に調整可）。
子の状態は実 task を正とし、JSON の dispatched だけで Running と断定しない。
一覧の読みは一括取得とし、行ごとの追加問い合わせを増やさない。

## 6. 実装の接続点（2026-09-05 checkout で確認）

| 対象 | 現在の接続点 | 主な変更 |
|---|---|---|
| Action | `internal/orchestrator/machine_card.go`、`internal/api/suggestion_accept.go` | Action 語彙、working Go、accept と直接操作の一致 |
| 作業子 | `internal/orchestrator/card.go`、`internal/api/workflow_card.go` | 単一枠、Go の二段階状態検査、冪等生成 |
| Shape | `internal/api/web.go` の PostStartShapingSession/buildShapingInstruction | カスタム指示、共通判断文脈、対象 card と session の関連付け |
| session | `internal/dispatcher/session_job.go` | task-less job と判断要求の関連、既存の終了検知との接続 |
| Signal | `internal/orchestrator/store.go`、`signal_ingest_bridge.go` | commit 後起動、耐久要求、自己起動除外、handoff |
| Sweep/記録 | `internal/skills/data/boid-metaproject/scripts/boidmeta/` | capture/link と判断の分離、人発 request の記録、旧語彙の移行 |
| 復旧 | `internal/api/queue_sweep.go`、`trigger_loop.go` | 子の reconcile と既存 timeout を再利用、判断要求の復旧 |
| UI | `internal/api/web.go`、`web_service.go`、`web/templates/tasks.templ` | card 読みモデル、統合項目、一覧、入力、SSE |
| 履歴 | `internal/timeline/` | card 向け読みモデルとの役割分離、execution/TUI の互換維持 |

組み込みスキルを読むだけの調査で、ここに記した将来動作が既に実装されているとは扱わない。
khi 等の workspace repo と本番 DB は今回未調査。判断方針の切替・既存 card 数の調査は移行作業に含む。

## 7. 実装順序と PR 分割案

各 PR は単独で既存動作を維持できる形にする。新しい自動経路は切替まで無効にし、
旧 Sweep と新しい判断処理が同じ card を二重に書く期間を作らない。

| PR | 内容 | 完了条件 / 依存 |
|---|---|---|
| PR-1 | Action 語彙整理、working Go | 直接操作と accept が一致。旧 payload の互換方針を固定 |
| PR-2 | 単一作業子と移行診断 | 全書き込み口で一つの枠を守る。既存複数子を破壊しない。PR-1 後 |
| PR-3 | card 判断要求の永続化と起動調整 | 冪等要求、単一実行、commit 後起動、再起動復旧。自動経路は未切替 |
| PR-4 | 共通判断契約、Discuss/Run の backend | session/task 両方で同じ文脈・記録。人発要求、終了解放。PR-3 後 |
| PR-5 | Sweep 縮退と内部イベント切替 | capture/link handoff、自己ループ抑止、既存 card の続報。PR-2〜4 後 |
| PR-6 | card タイムライン読みモデルと API | 子の統合項目、安定 cursor、一覧用活動状態。PR-2 後、PR-3/4 の関連を取り込む |
| PR-7 | Web UI 統合 | 上部入力・固定項目・最新10件・日付・一覧・SSE。PR-4/6 後 |
| PR-8 | workspace 切替、運用検証、旧経路撤去 | 少数 card で一巡を確認後に展開。旧 Shape/Sweep の重複判断を除去 |

PR-3 着手時に session 終端通知、判断要求の table/transaction 境界、設定の名前を短く設計レビューする。
PR-6 着手時に実データから履歴項目と snapshot の充足を確認する。計画全体の再設計は前提にしない。
UI は PR-7 で初めて見せるのではなく、PR-6 の読みモデルに合わせた静的サンプルで早めに確認する。

## 8. 互換性・切替

- 過去の action 履歴の `working` / `done` は書き換えない。読み側で旧名を解釈する。
- 現在保存されている suggestion は card 型に限定して新名へ移す。
  旧 write CLI との短い互換期間では card の旧名を正規化して受ける。撤去条件を切替 runbook に記す。
- 既存の複数未完了子を持つ card を事前に列挙する。実行中の仕事を停止・削除しない。
  実行中は完了を待ち、複数の未実行仕様は人が次の一つを選ぶ。残りの構想は保存してから整理する。
  解消前の card は新たな子追加・Go を制限し、理由と残っている子を表示する。
- 新 timeline は移行中の複数子も読める必要がある。新モデルの違反を非表示で隠さない。
- 旧内部 Signal の未処理分を要求へ移す時は原因 ID で重複排除。意味判断を経ず一括 ack しない。
- 旧 Sweep の判断段を停止してから新自動経路を有効化する。外部 connector は継続可能。
- 問題時は新規自動起動を止め、要求を保持する。進行中の作業子には介入しない。
  schema の強制巻き戻しではなく旧 wire 互換を使う。旧判断との同時稼働は復旧策にしない。

## 9. 検証計画

### モデルと起動

1. parked Go と working Go が一つの子だけを起動。再クリック/再試行で重複しない。
2. open/specced/dispatched を持つ card への追加を全 API/CLI 経路で拒否。execution の並列子は許可。
3. 子なし Go、仕様不備、起動失敗で suggestion や仕様を失わない。
4. 子の done/aborted を受けて次の判断が起動し、結果を読んで新しい一手を用意できる。
5. 外部の新規/既存 card の両経路で handoff が残る。書き込みと ack の間の失敗から復旧できる。
6. 通知欠落、daemon 再起動、判断起動失敗、判断中の追加要求を注入し、欠落・二重実行を検査する。
7. 判断自身の summary/spec/suggestion は無限再起動を起こさない。Sweep capture は抑止されない。
8. 対話中は自動判断せず、終了後に保留分が動く。ブラウザ切断だけでは終了しない。
9. 人発 Run と Discuss が Signal なしで記録できる。workspace 越境を拒否する。
10. 判断処理が card の Action を勝手に accept できない。旧 wire 互換は card 型にだけ効く。

### UI と運用

11. 仕様 → Go → 実行 → 結果を同じ子項目で確認できる。awaiting から回答画面に進める。
12. 10件より古い実行中項目/提案も固定表示される。固定と履歴で重複しない。
13. 日跨ぎ・年跨ぎ・タイムゾーン境界、同日追加、同時刻の複数項目で順序・区切りが安定する。
14. SSE と追加読み込みの競合で入力・展開・既読の履歴を失わない。
15. 一覧だけで作業中/判断中/入力待ちを判別できる。すべての新規 UI 文言が英語。
16. 長い card と過去に複数子を持つ card を使って表示を確認。execution 詳細と TUI に回帰がない。

変更に対応する Go/Python のテスト、templ generate、関連パッケージ検証を各 PR で行う。
cutover 前には全体チェックと利用可能なブラウザ/E2E 環境で一巡を確認する。
本番評価は少数の実例で、イベント発生→判断開始の時間、提案への修正、失敗/再試行、
二重起動、API/token/実行時間を記録する。速度の具体的な目標値は baseline を測って決める。

## 10. 実装時に確定する点

- Action の具体英名は Go/Start/Park/Complete/Drop/Reopen 案を基準に最終確認。
- 統合判断の設定名、要求 store/API 名、session の終了フック。
- Complete/Park を実行中の子がある時にどう扱うか。既存挙動を調べ、暗黙に子を中止しない。
- 終端 card の Discuss/Run の提供範囲。自動で reopen はせず、必要なら Reopen を提案する原則は維持。
- 失敗した判断要求の retry 上限・通知・手動 retry の最小 UI。
- 日付のタイムゾーン方針と、既存履歴から再構成できる情報の限界。

これらは合意した UX と矛盾しない範囲で決める。card 単位の判断を中心に置き、
まれな並行ケースのために対話注入・分散ロック・複雑なイベント消費追跡を導入しない。
