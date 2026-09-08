# Card の次の一手 — 判断処理の統合と Web UI の再構成

2026-09-05 起案・改訂。ステータス: **カードコマンドと単一実行枠の方針は対話で合意。実装契約は提案、未実装。**

実現可能性のセルフチェックを実施。既存コードで使える接続点と追加実装の必要箇所を §6 に記録した。
動く試作による確認はまだ行っていないため、§7 の最小縦断検証を後続実装の着手条件とする。

同日のレビュー（実コード照合）を反映して改訂。カードコマンドは既存 `triggers[]` の `run:` と同じ
script 方式で行き、起動機構は trigger run の拡張とする。関連付けは作成 op の中で書き、
readonly は継続先の定義で決め、自動起動の継続先と対象 card 状態を絞った（§4.3〜4.6）。
§6 に Shape launcher・task create の冪等性・GC・UI の action 名分岐の照合結果を追記した。

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
suggestion 語彙の現在の実装順序は [suggestion-as-state-transition-impl.md](suggestion-as-state-transition-impl.md)
（khi 側 PR-K を含む）。§3.1 の改名はその語彙を変えるので、khi 側の追随 PR を §8 に含める。
UI については [webui-detail-list-redesign.md](webui-detail-list-redesign.md) の後続となる。
本 doc と衝突する将来仕様は本 doc を優先する。既存の実装を説明する際は区別する。

## 2. 合意した方向と範囲

1. suggestion は **Action** を指定する。状態は Action を適用した結果。
2. working 中も Go できる。
3. card 直下の未実行・実行中の作業子は、合わせて一つまで。完了した子の履歴は複数持てる。
4. メタプロジェクトがカードコマンドを定義する。ラベル・スクリプト・呼ぶスキルは workspace 側で決める。
   対話と task dispatch を提供できるが、daemon に Discuss/Run という用途の分岐は持たせない。
5. 外部 Sweep は新規 card の作成または既存 card への情報の振り分けまで。
6. card ごとの肉付け・子の仕様作成・提案は、内部イベントやユーザー操作からカードコマンド経由で起動する。
7. 内部イベントは周期 Sweep を待たずに反応する。
8. 作業 task・コマンドが作る task・session を合わせ、card ごとの進行中の実行は最大一つ。
   占有中の自動起動要求は保持し、終了後に扱う。対話へのリアルタイム注入は不要。
9. card 詳細はタイムラインを中心に統合。最新順、初期10件、古い履歴を追加表示。
10. 指示入力は上部。現在の提案・実行中の子は履歴件数で隠さない。日付セパレータを入れる。
11. UI のボタン・説明・状態表示・エラー・空状態は **英語**で統一する。

初期の「working 中も全 specced 子をまとめて Go」は単一作業子モデルで置き換えた。
初版の「判断 task/session だけの排他」と「daemon 固有の Discuss/Run」も本改訂で置き換える。
カードコマンドの起動機構は既存 `triggers[]` の run 機構（readonly exec job、timeout、失敗通知、
手動起動口）を card 文脈付きで拡張する。宣言だけで継続先を作る方式は、Shape の「daemon が
指示を固定する」friction を薄く残すため採らない。新しい launcher subsystem も作らない。
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
人の承認ゲートは維持する。任意コマンドのラベルと、作業子の Go を UI 上で区別する。
Go をカードコマンド扱いにして実行承認を迂回しない。

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

**コマンドが作る task/session は、仕様から Go する作業子とは別の関連で持つが、実行枠は共有する。**
作業子の parent_id と command 実行の関連を混ぜない。コマンド用 task は親なしの execution task とし、
対象 card は専用の関連から取得する案とする。これなら作業子の集計・完了条件に判断 task が混入しない。
通常一覧での関連 task の表示もこの関連で整理し、履歴・診断からは参照可能にする。

| 制約 | 対象 |
|---|---|
| 次の一手の仕様は最大一つ | 未実行の open/specced 子。仕様を作る対話・判断と共存できる |
| 進行中の実行は最大一つ | Go による作業 task、コマンドが作る task、対話 session の合計 |

作業 task の pending/awaiting、session の入力待ちも枠を保持する。
card の working 状態は占有を意味しない。現在の関連先が終端かどうかで決まる。
作業中の card に別の対話を起動する機能は初期版に含めない。既存 task の質問・操作へ誘導する。

## 4. メタプロジェクト定義のカードコマンド

### 4.1 責務とスキルの関係

現在の Sweep は `/boid-task` から behavior の指示を読み、組み込み `sweep_targets.py` を実行する。
その `--judge-skill` が workspace 側の判断スキルを指定し、対象ごとの subagent がそれを読む。
`boid-metaproject` 自体は判断スキルではなく、構築・運用の手順と共通 Python を配布するもの。

この「workspace が入口を所有する」関係を維持する。新しい固定 `boid-card` スキルを全 workspace の
入口として強制しない。workspace スクリプトが既存の判断スキルを呼ぶことも、目的に応じて選ぶことも可能。
同一 workspace で手動・対話・自動が同じ方針や部品を使えるようにするのであり、daemon が判断手順を統一するのではない。

| 所属 | 責務 |
|---|---|
| daemon | 宣言済みコマンドの起動、文脈の受け渡し、実行の関連付けと寿命、イベントの保留 |
| メタプロジェクト | コマンドのラベルとスクリプト、task behavior/session instruction、判断スキルの選択 |
| boid 組み込みスクリプト | card の仕様検証・読み書き・差分ガード・記録などの共通処理 |
| workspace 判断スキル | 調査、成果物、完了条件、提案の方針。ユーザー指示に応じて動く |
| Integration Pack | サービス固有の照会・操作方法 |

### 4.2 宣言と入力（新設する契約の案）

メタプロジェクトの project.yaml に `card_commands` と、自動起動するコマンドの参照を宣言する。
以下の名前・書式は提案であり、現在の構文ではない。既存の `triggers` は外部 Sweep の定期起動に残す。
旧 `commands:` は既に無視されるため、旧機構の復活と解釈させない。

```yaml
card_commands:
  discuss:
    label: Discuss
    run: python3 scripts/card_discuss.py
  review:
    label: Run
    run: python3 scripts/card_review.py
card_events:
  command: review
```

daemon は discuss/review というキーを解釈しない。任意の安定キーで、UI のラベルも workspace 定義。
内部イベントの標準対応を `card_events.command` に渡す。複雑な条件式や複数 handler の競合解決は初期版に入れない。
コマンド未設定の project は既存機能を維持し、自動経路には参加しない。

UI は共通の入力欄と定義順のコマンドボタンを表示。空指示でも起動できる。
スクリプトは定義元のメタプロジェクトの sandbox で実行する。host shell で任意コードを実行する経路は新設しない。
card の project を定義元とし、全 workspace のメタプロジェクトから名前で探索・推測しない。
外部 Sweep の実行場所も同じ project を使う。

入力は broker で取得する構造化された command context に置く。取得口の仮称は
`boid card context`。card_id、request_id、command_key、ユーザー指示、起動理由を返す。
自由文を shell のコマンド文字列に展開しない。request ID の自己申告だけで操作権限を与えず、
job token に結びつく文脈を daemon が正として返す。機構は connector run と同じで、broker が
token entry から文脈を埋める（`internal/sandbox/broker.go` の Connector と同じ形）。
`BOID_CARD_ID` 等の環境変数を利便のために置いてもよいが、権限の根拠にはしない。
継続先の task/session にも daemon が同じ文脈を token に焼き込む。これが「由来の伝播」の実体で、
launcher が引数で card_id を渡す形にはしない。動的要素の吸収は launcher の有無に依存しない。

共通の固定 bootstrap が毎回「子を作れ」と強制しない。対話、調査だけ、何もしない判断も扱える。

### 4.3 コマンドの出力と session 作成

コマンドの `run:` は **card 文脈付きの trigger run** として起動する。既存 `triggers[]` と同じく
readonly 固定の exec job、`sh -c`、stdin closed、timeout、失敗の記録と連続失敗の通知、
`boid trigger run` 相当の手動起動口を流用する。trigger との違いは三つだけにする。
single-flight の単位が (project, trigger) ではなく card の実行枠になること、token に card/request
文脈が乗ること、終端が「launcher job の終了」ではなく「継続先の終端」になること。

launcher は短命で、**既存の作成 op を一回叩いて task または session を一つ作り、終了する**。
初期契約は一つの継続先を必須とし、launcher 自身で長時間の LLM 判断を実施する方式を増やさない。
判断が不要だったという結論は、作成した task が正常終了して返せる。

task 作成は既存 `boid task create` を使う。`ref` に request_id を載せれば、`internal/api/task_create.go`
の `FindTaskByRef(ref, parent, project)` / IdempotencyKey による get-or-create がそのまま
「同じ request で二つ目を作らない」を担う。root task でも効く。command task は親なし（`parent: root`）で作る。

session 作成は sandbox 内から行う op が無い（shim の agent op は stop のみ）ので、`boid agent start`
相当の broker op を新設する。HTTP 側の StartSessionResult は既に job_id / attach_url を返すので、
op はそれを呼ぶ薄い層にする。CLI の `boid agent <harness> --no-attach` は実装済みだが stderr に
`job_id=...` を出すだけなので、機械可読 stdout（`--output json`）を足す。

**関連付けは作成 op の中で書く。** task は create と同じ transaction で request → task を永続化する。
session は Dispatch が同期で job_id を返した直後に request → job を書く。Runner.Dispatch の前段に
hook を足す必要はない。Dispatch と関連書き込みの間で daemon が落ちた場合は、継続先の JobSpec/token に
request_id が入っているので、復旧走査が「launching のまま継続先が無い request」を job 側の文脈から
逆引きして結び直す（§4.4）。

**readonly は継続先の定義で決める。** task は behavior の `readonly`、session はコマンド定義で指定する。
launcher が readonly:true でも writable な behavior の task を作れるのは trigger の前例と同じで、
これを「呼び出し元を超えて昇格しない」と縛ると Run task が作れなくなる。利用 project だけを
token の許可範囲で検証する。

stdout の返却例は `{"kind":"session","job_id":"..."}` または `{"kind":"task","task_id":"..."}`。
ログは stderr。Web UI は daemon が検証済みの関連を取得して該当ページを開く。
任意 URL への redirect やログからの ID 推測は行わない。**stdout は UI への応答用であり、
関連付けの正本ではない。** 作成後に launcher が落ちても、応答を受け取れなくても再発行しない。
script が二つ目を作ろうとすれば op が拒否する。同一要求の再試行なら既存の関連先を返す。
launcher token に紐づく作成だけが枠を引き継ぐ。関連先 task がさらに子 task を作る通常の処理まで
「同じ request の二つ目」と誤判定しない。由来の伝播と枠を引き継ぐ権利は分ける。
Go も同じ実行枠を予約する。CLI で card 直下に直接作成する経路がこの制約を迂回しないようにする。

### 4.4 起動・終了の最小状態管理

request と card の現在の実行関連を保持する小さな永続 store を新設する案とする。
最低限、card_id、request_id、command_key（Go は作業実行由来として区別）、要求境界、
起動時定義、launcher_job_id、関連先 kind/id、状態、結果/エラーを保持する。
イベント要求は原因 ID を unique にして、同じ原因の再配達を重複排除する。
概念状態は queued → launching → attached → finished/failed。
card ごとに launching/attached は一つという制約を DB で保証する。
launcher は launching の枠を継続先へ引き渡し、別の実行枠としては数えない。

1. ユーザー要求/イベントを永続化し、空き枠を transaction 内で予約する。
2. launcher を起動。作成 RPC で task/session の関連を耐久化してから実行を開始する。
3. launcher 終了後も、関連 task/session が終わるまで枠は維持する。
4. task は task の終端、session は job の終端で解放。Run の hook job だけが終わっても解放しない。
5. 起動時に取り込んだ要求の境界を固定し、成功時はその範囲だけ完了にする。
   終了時に保留要求があれば一回にまとめて次を起動する。処理中に届いた要求を既処理にしない。
   失敗/中止は枠を解放しても判断成功とは扱わず、元の要求を retry 可能な失敗として残す。

DB transaction と外部プロセス起動は一つの atomic 操作にはできない。
そのため request の予約（枠の確保）は transaction で先に行い、継続先との関連は作成 op の中で書く（§4.3）。
起動前に task/job ID を予約する方式は採らない。代わりに継続先の token 文脈に request_id を焼き込み、
再起動時の走査は launching のまま関連先が無い request を job 側から逆引きして結び直す。
逆引きできる job が無ければ launcher が作成前に落ちたので、request を failed（retry 可能）に戻す。
既存 Go は CreateTask(auto-start) 後に親を更新するので、枠予約 → CreateTask（request 関連込み）→
親の遷移、の順に組み替える。この変更を最小縦断検証の対象とする。

失敗した request が retry 可能に残ったまま保留分をまとめて次を起動した場合、次の起動が成功すれば
失敗分も吸収したとみなして閉じる。判断は request 単位ではなく card 全体を読むためで、
失敗 request を個別に retry し直す義務は無い。明示的 Retry は保留も次の起動も無いときの手段。

request 行の GC は、pending/launching/attached を対象外とし、finished/failed は関連先 task/job と
同じ retention 規則で消す。card の履歴が request の結果概要を必要とする場合は、
request 行ではなく card 側の action payload に残す（§5.2）。

起動失敗は要求を失敗状態に残す。原因・履歴・明示的 Retry を提供し、無制限の高速再試行はしない。
継続先の生存が不明なら枠を勝手に空けず照合を再試行する。既存 trigger の「一定時間後に未解決枠を解放」を
そのまま流用すると二重実行を許すので採用しない。launcher 自体には短い timeout を持たせるが、
12時間超の作業 task に一律の短い期限は適用しない。

占有中の手動コマンド/Go は起動せず、現在の実行へのリンクを返す。入力内容は維持する。
自動要求だけを保留・集約する。対話はブラウザ切断と終了を混同せず、job が続けば枠も続く。
session に復帰/終了する汎用の UI を提供し、daemon 固有の Discuss モードは作らない。

**自動起動の継続先は task に限る。** session は誰も attach しなければ終端せず、無人 session が
枠を永久に握る。daemon は `run:` の中身を解釈しないので load 時には判定できず、
`boid agent start` op が「起動理由が内部イベントの request」から呼ばれた場合に拒否する。
人が押したコマンドは session を作ってよい。人発の session にも idle timeout は初期版では設けず、
占有の表示と終了操作で対処する。

### 4.5 共通記録処理を task/session 両方で使う

現行 `write.py` は以下の三つに依存しており、そのまま Discuss に流用できない。

- BOID_TASK_ID を処理記録の書き先（Sweep task）として必須にする。
- `task current --field readonly` によって書き込み可否を判定する。
- 全 verb に signals を要求し、一回の書き込み成功後に progress と ack を行う。

仕様の検証・差分ガード・child ID 等の共通処理は維持し、**操作の適用と、処理単位の終了を分離する**。
command context から対象 card、request、実行主体と書き込み可否を取得する。
session に偽の BOID_TASK_ID を与えず、readonly を任意の環境変数や CLI フラグで解除させない。
実行主体は request/task/job を区別して記録し、session 発の操作を human の Action とみなさない。

現状の readonly は三つの役割を兼ねている。(a) API gateway の非 safe メソッドの拒否、
(b) `boid-task` の supervisor/executor 選択、(c) write.py の report 強制
（`boid-metaproject/SKILL.md` §3、`write.py` の `_readonly_forces_report`）。
本 doc は (c) を command context の **card 書き込み権限**という独立した軸に移す。
判断 task は readonly:true + card 書き込み可、作業 task は behavior の readonly に従う、という組が
表現できるようにする。session job は TaskID を持たないので `boid task current` が使えず、
現行 write.py を session から呼ぶと readonly 判定で落ちて report 強制になる。これも context 化で解消する。

(b) は残る。Sweep は今も readonly:false + `boid-task` で動いており、default_instruction が
「最初の一手」を固定することで executor の既定手順（実装・commit・push）を実質回避している。
判断 task も同じ手を使えるが、それはスキル文言頼みで機構ではない。

記録した summary/spec/suggestion は card に、コマンドの経緯は実行関連に記録する。
同じ session から複数回書ける。一回の書き込みや Signal の ack で実行枠を解放しない。
作業を終えていない task の exit を成功ともみなさない。成功・失敗は関連先の lifecycle から得る。
人発コマンドは signals なしで動く。外部 Signal の ack は Sweep の handoff 成功に対応させる。

Run の task bootstrap は現在 `/boid-task` が readonly から supervisor/executor を選ぶ。
workspace で task behavior の instruction に判断スキルを指定できるだけでは、
writable な判断が「実装・commit・push」の既定手順を受ける矛盾が残る。
最小案は `boid-task` が active instruction に明示された workspace workflow への委譲を
汎用の実装手順より先に行い、文脈取得・ask・notify の lifecycle 契約は維持すること。
card 固有の behavior 名を adapter にハードコードしない。
session は既存 `--instruction` で同じ workspace スキルを読むよう指定できる。
この二つの入口を実 harness で検証し、成立しなければ bootstrap 選択の明示設定を追加する設計に戻す。
新しい組み込み判断スキルの強制だけで解決したことにはしない。

### 4.6 外部 Sweep と内部イベント

```text
External connector → inbox → Sweep: screen / capture / link
                                      ↓ durable card request
Card events / User command → reserve card slot → workspace launcher → task/session
                                                                    ↓ terminal
                                                         release slot → pending request
Go → reserve the same slot → work task → outcome → card event
```

Sweep は新規 card の最小情報（title、identity/source、起票理由・引き継ぐ文脈）を作成、
または既存 card に続報を結びつける。card/request の handoff が耐久化されてから元 Signal を ack する。
リトライで二重起票しない。続報の調査結果を捨てて後段で同じ調査を繰り返さない。

自動起動対象は新規 capture、外部 link/続報、作業子の終端、wake_due、人の card 更新/回答を基本とする。
Go 自体は枠を占有するだけで即時の再判断を起こさない。初期版の対象 action は実装時に明示表で固定する。
**対象 card 状態は parked / working に限る。** Complete/Drop も「人の card 更新」だが、
done/dropped の card には自動起動しない。終端 card への手動コマンドの提供範囲は別途（§10）。

**コマンド用 task/session の終了は枠の解放であり、それ自体は新たな判断要求にしない。**
そうしないと何もしない判断の終了でも永久に再起動する。
コマンド自身の summary/spec/suggestion/ack/progress も起動対象から除外する。
逆にメタプロジェクトの全書き込みを除外すると Sweep の capture/link を失うので、
起動由来（launcher/関連 execution と元 request）を daemon で追跡して区別する。

意味上の終了を持たないイベントを単純な updated_at 変化で検知しない。
作業子の終端・枠解放・判断要求の記録は整合する transaction 境界に揃え、
既存の best-effort な IngestActionSignal を唯一の起動根拠にしない。
旧内部 Signal と新要求の併用期間は原因 ID で重複排除する。

通常は commit 後に起動を試み、定期 Sweep を待たない。
通知欠落・daemon 再起動時の走査は復旧用に残す。異なる card の並行実行は可能だが、
既存 dispatcher の上限を確認し、利用可能枠が無い時には pending のまま保持する。
実行中の新着を対話に注入せず、終了後の再判断が「追加対応なし」になることも許容する。

## 5. Web UI

### 5.1 card 詳細

上から次の順に配置する。

1. タイトル、card 状態、現在の要約（長文は展開）。
2. 共通の指示入力欄とメタプロジェクト定義のカードコマンド。`Discuss` / `Run` は設定例。
3. 現在の提案・次の作業仕様・唯一の進行中の task/session を示す固定項目。
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
生 action 履歴は監査用に保持する。`internal/tui/` は撤去済みで、既存の status-group
timeline は execution 詳細（Web UI）のみが対象——これを壊さず、card 用の読みモデルを設ける。
過去の spec が履歴だけから復元できない場合は現在の保存情報を使い、
当時の完全な snapshot が存在するかのようには表示しない。

### 5.3 順序・追加読み込み・更新

履歴項目の発生時刻と安定 ID で降順に並べ、cursor で古い項目を追加する。
子の項目は作成時の位置を持ち、活動中だけ固定。終了しただけで履歴全体を並べ替え続けない。
ただし子の終端は終端時刻の位置に「finished」の軽い項目を出し、作成位置の子項目へリンクする。
これが無いと数日前に仕様を作った子の結果が最新 10 件に現れない。
提案の発生や回答などの独立イベントはその時刻を使う。

子 task の行は GC で消える（§6）ので、finished 項目と子項目の結果概要は child_closed の
action payload から描く。task 行が残っていればリンク先として使い、無ければリンクだけ落とす。

SSE 更新で指示の入力途中・展開状態・追加読み込み済み履歴・スクロール位置を失わない。
新着と Load older の競合でも重複・欠落を起こさない。親 card に子の状態更新を届け、
既存の parent action だけでは不足する進捗/awaiting の更新経路を補う。

### 5.4 日付

日付が変わる位置と先頭項目の前に `Sep 5, 2026` 形式のセパレータを表示。
通常項目は時刻のみ、固定項目は日付と時刻を明記する。
同じ日を Load older で継ぎ足す場合は区切りを重複させない。
日付境界・時刻は同一タイムゾーン。既存の task/job ページはサーバ側 `Local()` で描画している
（`web/templates/tasks.templ` の detail-time と timeline 時刻）。初期版はこれに合わせてサーバの
タイムゾーンで統一し、表示タイムゾーンを画面で確認可能にする。ブラウザのローカルタイムゾーンへ
移すのは epoch を出して JS で描く構造変更になるので、既存ページと一緒に別途行う。
対話画面内は通常どおり新しい発言を下へ追加する。

### 5.5 一覧

card 状態とは別に、唯一の作業子から `Ready to run` / `Running` / `Needs input` 等を表示する。
open で仕様未完成なら `Draft`、task が pending なら `Queued` とし、実行中と誤認させない。
コマンドの実行には定義されたラベルと task/session の状態を示す。
`Reviewing` / `Discussing` という用途を daemon が推測しない。
子の状態は実 task を正とし、JSON の dispatched だけで Running と断定しない。
一覧の読みは一括取得とし、行ごとの追加問い合わせを増やさない。

## 6. 実現可能性セルフチェック（checkout の実装で確認）

**結論: 成立を妨げる構造的な問題は見つかっていない。ただし UI とスクリプトの追加だけでは実現しない。**
session 起動の broker op、作成 op 内の関連付け、trigger run の card 文脈拡張、共通記録の session 対応、task bootstrap の検証が主要な追加作業。
コード読解で確認した結果であり、動く試作による担保は §7 の Gate A で行う。

| 論点 / 根拠 | 使えるもの | 不足と対処 |
|---|---|---|
| `cmd/agent_session.go` | --no-attach と session API 呼び出し | stderr の ID を JSON stdout に整備。オプションを未実装と誤記しない |
| `internal/apiwire/store.go`、`internal/server/wire.go` | StartSessionResult の job_id/attach_url、既存 StartExec | card/request の関連は無い。作成時の関連と冪等性を追加 |
| `internal/sandbox/boid_shim.go`、`broker.go`、`internal/server/boid_executor.go` | workspace を token で認可する broker。connector 文脈を token entry から埋める既存パターン | agent 起動 op は無い。session 作成・context 読み・由来伝播を追加。readonly は継続先の定義で決め、launcher から継承しない |
| `internal/orchestrator/spec_loader.go` | project.yaml の load/hydrate/検証 | 旧 commands は無視される。card_commands/card_events を新設し schema/catalog も更新 |
| `internal/api/trigger_loop.go` | **card command の run はこれの拡張**。StartExec、readonly 固定、timeout、失敗記録と連続失敗通知、self-heal（`TriggerRunSelfHealGrace` 3 分）、`boid trigger run` | single-flight の単位を (project, trigger) から card 実行枠へ。終端を継続先の終端に。self-heal の時間解放は二重実行を許すので継続先照合に置き換える |
| `internal/dispatcher/runner.go` | Dispatch が job を永続化して起動、同期で job_id を返す | 起動前 hook は不要。JobSpec/token に request_id を焼き込み、復旧の逆引きに使う。通知は commit 後 |
| `internal/api/task_create.go` | `FindTaskByRef(ref, parent, project)` / IdempotencyKey の get-or-create。root task でも効く | ref に request_id を載せる。関連付けを同じ transaction に入れる |
| `internal/api/web.go` の Shape launcher、`session_behaviors.shape` | daemon が card から instruction を組んで StartSession する既存の session 起動口 | card command の最も近い先行実装。PR-2 で `boid agent start` op の HTTP 側に流用し、Gate B で Shape ボタンを撤去 |
| `internal/api/workflow_card.go` | Go、child_spec、子の終端 reconcile、冪等 create | Go は parked 二段階検査と先行 auto-start。枠予約→関連→起動へ改修。`promotedAttrVocabulary` の suggestion 語彙は手書きで `cardTransitionActions` と手同期 |
| `internal/skills/data/boid-metaproject/scripts/boidmeta/write.py` | 共通の検証・差分・書き込み | **PR-3 で実装:** `boid card context` の有無で経路を分岐、card 文脈があれば signals/BOID_TASK_ID を要求せず `card_write` だけを根拠にする。task ID/readonly/signals 依存の分離は完了、実 harness (session からの実呼び出し) は Gate A |
| `internal/adapters/{claude,codex,opencode}/run.go`、`boid-task/SKILL.md` | session instruction、task の既存 lifecycle | **PR-3 で実装:** `boid-task/SKILL.md` に workspace workflow への委譲節を追加 (adapter 側の配線は不要と判断、根拠は §10)。task は boid-task 起動。実 harness での成立確認は Gate A |
| `internal/orchestrator/signal_ingest_bridge.go` | 内部事実と workspace 解決 | best-effort、project 単位の自己除外。耐久要求と request の由来で補完 |
| `internal/api/web.go`、`web/templates/tasks.templ`、`internal/timeline/` | task/job ページ、SSE、仕様と実 task の対応 | command/read model、card 側の更新通知、stable cursor を追加。`detailPrimaryAction` / `actionPrimaryClass` は action 名だけで分岐し type を見ないので、card の `start` が primary 扱いにならないよう type で分ける。child_dropped の Web/CLI 操作は無い（action send のみ） |
| `internal/orchestrator/model.go` の子集計 | execution の階層構造 | command task を作業子に数えず単一枠を共有。reopen/直接作成でも迂回させない |
| `internal/orchestrator/store.go` の GCTasks | 終端 status + updated_at で削除 | 親の生死を見ない。生きている card の closed 子 task は 30 日で必ず消える。結果概要は child_closed の payload に持つ |

追加で考慮すべき境界:

- 作業子を card の履歴から直接 reopen する経路も枠を取得する。別の実行中に再開させない。
- Complete/Park/Drop は作業や session を暗黙に中止しない。初期案は占有中の状態変更を拒否し、
  先に既存の停止/終了操作を案内する。実行 task の awaiting はユーザーが回答できるように保つ。
- GC は pending/launching/attached の request・関連先を消さない。closed 子 task は既存 GC で
  30 日後に消えるので、長寿命 card の read model は初日から task 行に頼らず child_closed の
  payload の結果概要を正とする。新しい retention 年限の決定は別途。
- コマンド実行中の設定変更では、起動時の command_key・定義版・run 内容を保持する。
  待機中に定義が消えたら明示的失敗として表示し、別コマンドへ暗黙に振り替えない。
- task 作成 API の親自動補完で command task が作業子になる事故を防ぐ。明示的な command 関連を正とする。
- 認証・sandbox policy はプロセスの権限を制御し、skill の文言を enforcement とみなさない。
  user が押したコマンドの agent 書き込みも human accept にはならない。

khi 等の最新 workspace repo と本番 DB は未調査。判断スキルが新しい入口から使えるか、
独自の旧記録 API/環境変数依存が残っているかは Gate A で実物を確認する。

## 7. 実装順序とゲート

イベント基盤を先に作り込まず、**手動カードコマンドから二種類の継続先へ到達して一巡できるか**を先に検証する。
各 PR は既存動作を維持し、新経路は project の明示設定まで無効。schema 名は以下の契約を満たす範囲で調整可。

| 順番 | 内容 | 完了条件 |
|---|---|---|
| PR-1 | Action 語彙と working Go、単一作業仕様・共有実行枠の store | 全作成/Go/reopen 入口と UI の action 名分岐を棚卸し。予約と型別 Action をテスト。child_closed に結果概要を保存。既存複数子の診断と child drop の手段 |
| PR-2 | カードコマンド宣言、trigger run の card 文脈拡張、`boid agent start` op、op 内の関連付けと冪等性 | 固定した最小スクリプトで task/session を起動し、UI から返却先を開ける。二重作成不可。op 直後に launcher を殺しても関連が残る |
| PR-3 | 組み込み共通記録の context 対応、workspace workflow の接続 | session/task 両方で読み書き・正しい終了が動く。task bootstrap の委譲を検証 — **実装は完了、実 harness 検証は Gate A へ送った（§10 の「PR-3 で実装」を参照）** |
| Gate A | 実 workspace のコマンドと判断スキルを使う縦断検証 | 下記項目を通るまでイベント駆動化へ進まない |
| PR-4 | 内部イベントの耐久要求・起動通知・復旧、外部 Sweep handoff。PR-4a（queued/fold の既知バグ、マージ済み）/ PR-4b（内部イベント→queued 生成、マージ済み、下記参照）/ PR-4c（queued→launching dispatch、周期フォールバック、下記参照）に分割 — **3 本とも完了** | 自己ループ無し、枠占有中の保留、作業終了後の再判断。旧経路とは未併用 |
| PR-5 | card タイムライン読みモデルと一覧活動状態 | stable ID/cursor、関連 task/session、GC 後も読める概要。PR-2 後に着手可能 |
| PR-6 | 詳細 UI 統合、コマンド入力、最新10件・固定項目・日付・一覧/SSE | 汎用 command UI で期待する操作を行える。定義ラベルは英語 |
| Gate B / PR-7 | 移行・運用検証・旧 Shape/重複判断の撤去 | 少数 card で外部変化から次の Go まで通し、展開する |

### Gate A: 実装前提を確かめる最小の実例

1. 実 workspace の script/skill を調べ、どの指示でどのスキルを呼ぶかを記録する。
   新しい汎用スキルに置き換えて問題を隠さない。
2. 同じ card に対してメタプロジェクトの session コマンドを起動。
   workspace スキルを読み、summary/spec/suggestion を共通処理で書き、対話は継続できる。
3. session 終了後、task コマンドで同じスキル・共通処理を使う。boid-task が不要な
   commit/push 手順を強制せず、判断結果を記録して正しい task を完了する。
4. 両方で card の状態を勝手に変えず、spec を実作業として自動 dispatch しない。
5. 作成 op が返った直後に launcher を停止し、応答を失っても関連が残り、再試行で重複しない。
   Dispatch と関連書き込みの間で daemon を落とし、再起動時の逆引きで結び直せることも見る。
6. Go、手動コマンド、reopen を競合させ、最大一つの実行枠を守る。
7. session の browser 切断、task hook job の終了と task 未終端、daemon 再起動で枠が誤解放されない。

初期の実 harness 検証は利用中の一つで完走させ、対応をうたう他の adapter も切替前に検証する。
失敗時は PR-3 の context/委譲契約を修正し、成立が未確認のまま PR-4 を積まない。
PR-5 の読みモデルに合わせた静的 UI サンプルは早めに確認する。

### Gate A の実測 (2026-09-07)

**縦断の対象は nvt-tasks**（default workspace のメタプロジェクト）。khi にも同じ宣言を
入れたが、実際に撃ったのは nvt-tasks 側。以下は**実行して確認した**ことだけを書く。

**前提として入れた変更:**

- boid: `buildShapingInstruction` が `child_specced`/`child_added` を書き込み経路として
  名指しし、さらに `"khi の suggest 経由でのみ"` と workspace 名を焼き込んでいたのを外した
  （PR #1066）。§2 が「Shape の『daemon が指示を固定する』friction」と呼んでいたものの実体。
- nvt-tasks / khi-task-collector: `card_commands` に `discuss`（session、人発）と
  `judge`（task）を宣言し、`card_events.command: judge`、判断 task 用の `judge` behavior
  （`readonly: true`）を追加。判断スキル側は最小の手当てのみ（card 文脈経由では対象が
  `card_id` 1 件で event_key が無いこと、`signals` を渡さないこと、`skip`/`done-signal` が
  使えないこと、`--done` は親が打つこと）。
- **nvt-tasks の Shape は元々成立していなかった。** `CLAUDE.md` にも `nvt-sweep/SKILL.md`
  にも「整形」「Shape」「child_specced」「child_added」がゼロ件で、daemon の指示文を
  受け止めるものが何も無い。唯一のスキルは `context: fork` の subagent 専用。khi は
  `khi-shape` を自前で書いて穴を塞いでいたが、その代償が §1 の「Shape と Sweep の判断は
  同じ仕事を重複して持つ」だった。

**通った項目:**

1. 実 workspace のスキル調査。上記のとおり記録。
2. session コマンド。`boid card run <card> discuss` で session が立ち、`boid card context`
   を実際に叩いて card_id を取り、`.claude/skills/nvt-sweep/SKILL.md` を読み、
   `write.py` で summary / drop-child / complete を書いた。**`boid action send` の
   直呼びは 0 件。** 対話の継続も確認。session は `BOID_TASK_ID` を持たず、
   `card_write:false` なら report に倒れるので、**書き込みが実際に landed したこと自体が
   `card_write:true` が根拠になった証拠**（write.py の stderr 監査行は Claude Code の
   TUI がツール出力を畳むため transcript に残らない）。
3. task コマンド。`judge` の継続先が `readonly: true` のまま summary / spec / suggestion を
   書き、`--done` で正常終了。commit/push の強制は無し。**§4.5 の「判断 task は
   readonly:true + card 書き込み可」が実 harness で成立した**（PR-3 が Gate A に送った宿題）。
4. 状態を勝手に変えない。task 面でも session 面でも card は `parked` のまま。子は
   `specced` で止まり、Go を押すまで dispatch されない。
5. 作成 op 直後に launcher を止めても関連が残り、再試行で重複しない。`run:` の
   `boid agent start` の直後に `sleep` を置いた検証専用コマンドを一時的に宣言して窓を
   広げ、continuation が attached になった状態で launcher の container を
   `podman rm -f` した。行は `attached` + `session:<job>` のまま生き残り、reconcile を
   2 ティック（30 秒間隔）跨いでも解放されない。同じキーを再実行すると
   `occupied:true` + **同じ request_id・同じ session** が返り、2 本目の session は
   生まれなかった。
   daemon 停止側は、継続先がまだ無い `launching` の行を作って（`sleep` を op の
   **手前**に置いた版）daemon を落とすところまで通った。再起動後、起動時スキャンが
   その行を `failed`（retry 可）に落として枠を解放し、直後に別のコマンドが起動できた
   —— `RecoverLaunchingCardRequests`（`internal/orchestrator/card_request_release.go`）
   が実 harness で効いていることの確認。**逆引きで「結び直す」側は当てられていない**
   （下記「残る未検証」）。
6. Go とコマンドが 1 つの実行枠を共有する。Go は `card_requests` に
   `__go__` の行を作り、その間のコマンドは `occupied` + 作業 task へのリンクを返す。
   手動コマンド同士も同様で、入力した instruction は応答に echo され失われない。
   二重起動しない。**逆向きも通った** —— specced な子を持つ card でコマンドが枠を
   握っている状態で Go を押すと、`accept(go): ... single work slot is already occupied
   by an active card command` の 409 になる。`speccedIdx == -1` の早期 reject
   （`internal/api/workflow_card.go`）より奥の、枠のガードそのものに到達している。
7. launcher は継続先を作って即終了し（実測 2〜3 秒、exit 0）、枠は継続先が
   持つ。session から detach しても `attached` のまま（切断と終了を混同しない）。
   `boid agent stop` で job が終端して初めて `finished` に落ちる。Go の作業 task の
   終端でも同様に解放される。**hook job だけが終わって task が未終端の場合と daemon
   再起動も通った** —— 詳細は下の「終わり方で枠の扱いが 3 通りに分かれる」。

**見つけて直したもの（どちらもコード読解では出ず、実際に撃って初めて出た）:**

- **発見 1（PR #1067）: 判断コマンドが自分の握る実行枠で自分を塞いでいた。**
  `cardSlotOccupied` が「card 直下の 3 つの占有シグナルは同じ 1 つの枠」として畳んで
  おり、継続先自身の `card_requests` 行（`attached`）を占有と数えていた。結果、
  子が 0 件の card で `child_added` が 409 になり、**判断が次の一手を一度も記録できない**。
  §3.2 の表は制約を 2 本に分けていて、仕様の枠は「仕様を作る対話・判断と共存できる」と
  明記されている。`cardSpecSlotOccupied` / `cardSpecOrExecutionSlotOccupied` に分け、
  `child_added` は前者、`reopen` は後者を見るようにした。
- **発見 2（PR #1068）: その鏡写し。specced な子が card コマンドの起動を塞いでいた。**
  発見 1 を直して初めて「specced な子を持つ card」が生まれ、そこで露出した。
  `cardWorkChildOccupantTx` が `DetailOpenSlotChildID` を占有と数えており、何も
  走っていないのに `occupied` が返り、しかも JSON の子には指せる task 行が無いので
  `target_kind`/`target_id` が空の行き止まりになっていた（§4.4 は occupied 応答に
  「現在の実行へのリンク」を返すと決めている）。生きた子 task 行だけを実行の占有とし、
  detail の parse は fail-closed のために残した。

**終わり方で枠の扱いが 3 通りに分かれる（項目 7 を撃って初めて分かった）:**

判断 task の hook job を 3 通りの終わり方で終端させると、枠の扱いが変わる。

| hook job の終わり方 | task | card_requests |
|---|---|---|
| exit 0（`--done` 無し） | `auto_advance` で `done` | `finished`（解放）|
| container ごと強制削除（exit 143）| `aborted` | `failed`（解放）|
| `boid task ask` で待機中に `boid agent stop`（exit 0）| `awaiting` のまま | `attached`（保持）|

つまり **「hook job が終わったのに task が未終端」は `awaiting` でしか起きない** ——
それ以外の終わり方は task 側が必ず終端に落ちるので、枠は正しく解放される。
`awaiting` の場合だけ枠が保持され、reconcile を跨いでも誤解放されない。

その `awaiting` の状態で daemon を再起動したところ、task は
`abort`（`code: daemon_shutdown`）→ 自動 `reopen` → 再 dispatch され、agent が
**新しい question_id で質問をやり直した**。この間 `card_requests` は一貫して
`attached` のままで、枠は job ではなく task に付いて回る。回答を投げると task は
`done` になり、枠も解放された。

**残る未検証:**

- 項目 5 のうち「dispatch と関連書き込みの**間**で daemon を落とし、逆引きで結び直す」。
  この窓は `executeAgentStart`（`internal/server/boid_executor_agent_start.go`）の
  `StartSession` と `AttachCardRequestOwned` の**隣り合う 2 行の間**にあり、`run:` に
  `sleep` を挟んでも広がらない（sandbox 側ではなく daemon プロセス内なので）。
  手で当てられる幅ではないので、実 harness では**当てていない**。
  結び直し側は unit test が押さえている ——
  `TestRecoverLaunchingCardRequests_ReattachesFoundContinuation`、
  `_IgnoresStalePriorAttemptSessionJob`、
  `TestReconcileLaunchingCardRequests_LauncherTerminatedWithSessionContinuation_Attaches`。
  実 harness で確認したのは「継続先がまだ無い launching 行が起動時スキャンで
  解放される」側だけ。

**後片付け（実施済み）:**

- 検証専用の card コマンド（`gatea-pre` / `gatea-post`）は nvt-tasks から削除し、
  `boid project fetch` 済み。
- 検証用の捨て card `cc1af1df-a228-4f69-aa79-7e3ddb2e833c` は `dropped`。

## 8. 互換性・切替

- 過去の action 履歴の `working` / `done` は書き換えない。読み側で旧名を解釈する。
- 現在保存されている suggestion は card 型に限定して新名へ移す。
  旧 write CLI との短い互換期間では card の旧名を正規化して受ける。撤去条件を切替 runbook に記す。
  書き側は `promotedAttrVocabulary`、write.py の VERBS、khi の判断スキルの三か所が同時に変わる。
  khi 側の追随 PR を runbook に含める。
- 既存の複数未完了子を持つ card を事前に列挙する。実行中の仕事を停止・削除しない。
  実行中は完了を待ち、複数の未実行仕様は人が次の一つを選ぶ。残りの構想は保存してから整理する。
  選ぶ手段は child_dropped の action send で、Web/CLI に操作が無い。
  **PR-1 で決定: 列挙は `boid task diagnose-cards` CLI（読み取り専用、実行中の仕事には
  触れない）を新設し、解消は既存の `boid action send --type child_dropped` の
  コマンド例をこの CLI の出力自体に添える形にした。card 詳細 UI への drop ボタン追加は
  見送り — 対象件数が少なく（運用開始直後の一時的な移行作業）、UI 実装より
  診断→CLI 手動対応の方が早く着手できるため。card 詳細 UI 側で違反 card に理由と
  残っている子を表示する読みモデル統合は PR-5/PR-6 のタイムライン統合に合わせて行う
  （PR-1 では見送り、上記 CLI が代替する）。**
  解消前の card は新たな子追加・Go を制限し（child_added / accept(go) の該当箇所で実装済み）、
  理由と残っている子を表示するのは上記の通り後続 PR に持ち越す。
- 新 timeline は移行中の複数子も読める必要がある。新モデルの違反を非表示で隠さない。
- 既存の Shape session・稼働中の作業子も切替前に列挙し、終了を待つか関連を移す。
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
8. 作業 task/コマンド task/session のいずれも占有中は別実行を開始しない。
   終了後に保留分が動く。コマンドの終了だけでは新たな要求を作らない。ブラウザ切断では終了しない。
9. 任意ラベルの task/session コマンドが Signal なしで記録できる。workspace 越境・readonly 昇格を拒否する。
10. コマンドが card の Action を勝手に accept できない。旧 wire 互換は card 型にだけ効く。

### UI と運用

11. 仕様 → Go → 実行 → 結果を同じ子項目で確認できる。awaiting から回答画面に進める。
12. 10件より古い実行中項目/提案も固定表示される。固定と履歴で重複しない。
13. 日跨ぎ・年跨ぎ・タイムゾーン境界、同日追加、同時刻の複数項目で順序・区切りが安定する。
14. SSE と追加読み込みの競合で入力・展開・既読の履歴を失わない。
15. 一覧だけで作業中/コマンド実行中/入力待ちを判別できる。新規 UI 文言と設定例ラベルは英語。
16. 長い card と過去に複数子を持つ card を使って表示を確認。execution 詳細に回帰がない
    （`internal/tui/` は撤去済みのため検証対象は execution 詳細のみ）。
17. 作成後の応答欠落、launcher の異常終了、二つ目の作成、継続先 task の通常の子作成を区別できる。
18. 設定の削除/更新、GC、履歴のページ境界、進行中の子の reopen を検証する。

変更に対応する Go/Python のテスト、templ generate、関連パッケージ検証を各 PR で行う。
cutover 前には全体チェックと利用可能なブラウザ/E2E 環境で一巡を確認する。
本番評価は少数の実例で、イベント発生→判断開始の時間、提案への修正、失敗/再試行、
二重起動、API/token/実行時間を記録する。速度の具体的な目標値は baseline を測って決める。

## 10. 残る設計上の確定事項

- Action の具体英名は Go/Start/Park/Complete/Drop/Reopen 案を基準に最終確認。
- card_commands/card_events と context op の正式な schema/CLI 名、終了 callback の具体的な配線。
  権限は「card 書き込み権限を readonly と独立した軸で context に持つ」を **PR-3 で実装済み**
  (`card_commands.<key>.card_write` → `card_requests.launched_card_write` スナップショット →
  `boid card context` の `card_write` フィールド、詳細は下の「PR-3 で実装」を参照)。
- task bootstrap の workspace workflow 委譲が実 harness で成立するか（Gate A の必須項目、
  契約自体は PR-3 で `boid-task/SKILL.md` に書いた — 下記参照）。
- コマンドの retry 上限・launcher timeout の値、ユーザーへの失敗通知方法。
  trigger の `timeout` / 連続失敗通知を流用する前提で、値だけ決める。
- **PR-2d-5 で確定: 終端 card への手動コマンドは拒否する。** `RunCardCommandAsHuman`
  が card の status を全く見ておらず、`done`/`dropped` にもコマンドを撃てて実行枠を
  取れていた非対称を解消 — 自動起動の parked/working 限定（§4.6）と揃えた。
  実装は `acceptGo` と同じ 409 ガード。自動 reopen はせず必要なら Reopen を
  提案する原則は維持。
- **PR-2d-5 で確定: force-release と abort の sibling 再起動セマンティクスを区別。**
  `ForceReleaseCardRequest`（運用者の明示的な「止めたい」意思）は fold されていた
  sibling を queued に戻さず failed にする — 直後の claim で運用者が止めたはずの
  card が再起動するのを防ぐ。一方 task の `aborted`（自動的な失敗で人の停止意思では
  ない）は従来通り `FailCardRequest` 経由で sibling を queued に戻し再試行を許す。
  両者を同じ関数に統合せず (`failCardRequest` の内部パラメータで分岐)、それぞれの
  意図の違いをコードでも保つ。
  **現状 fold 自体が本番未使用 (`ClaimQueuedCardRequests` の呼び出し元が無く、
  人発/Go はどちらも `launching` で直接 INSERT するため queued/folded 行は
  生まれない) なので、この分岐は PR-4 の内部イベント dispatch が実際に
  queued 行を作るまで dead code。**
  **PR-4a で対処済み:** fold は card 単位で command_key/cause_id を区別しない
  設計自体は維持したまま (§4.4 の「終了時に保留要求があれば一回にまとめて
  次を起動する」が意図どおり)、`ForceReleaseCardRequest` が巻き込んだ sibling
  の件数・command_key を返し `operator_notice`/`boid task release-card-request`
  の出力に含めるようにした (可観測性のみの対処)。`idx_card_requests_cause_unique`
  には `status != 'failed'` の述語を足し、`cause_id` 付きの行が `failed` に
  落ちても再配送で新しい行を作れるようにした。ただしこの緩和により
  `FinishCardRequest` の「同一 card の older failed request を吸収して
  finished にする」動作 (§4.4) が `cause_id` 付きの failed 行を巻き込むと
  index の UNIQUE 違反を起こしうることが判明したため、**吸収対象を
  `cause_id = ''` の行に限定**した — `cause_id` 付きの failed 行は
  吸収されず、failed のまま残る (GC は finished/failed を同じ規則で扱うので
  30 日後に削除される、§4.4 の保持規則どおり)。後続 PR がこの吸収を
  `cause_id` 付き行にも広げる場合は、この UNIQUE 制約を踏まえること。
  もう一つの含意: 同じ `cause_id` が failed 中に再配送されて新しい行が
  生まれた状態で古い failed 行を `RetryCardRequest` すると、UNIQUE 違反に
  なる。生の SQL エラーではなく `ErrCardRequestDuplicateCause` にマップして
  あるので、retry を UI や自動化に配線する側はこの失敗を「その原因は
  既に生きている要求として存在する」として扱うこと。
- **PR-4b で確定: 内部イベントから queued な `card_requests` 行を作る唯一の経路が
  `orchestrator.CreateAction`（`internal/orchestrator/store.go`）に定着した。**
  既存の `IngestActionSignal`（best-effort、warn のみ）の隣に第二の ingest ステップ
  `IngestCardEventRequest`（`internal/orchestrator/card_event_ingest.go`）を
  同一 tx 内に追加。§7 の PR-4a/PR-4b/PR-4c 分割のうち、queued 行を「作る」側を
  この PR が担当し、「捌く」側（claim/dispatch）は PR-4c に残る。

  **対象 action の allowlist（§4.6「初期版の対象 action は実装時に明示表で固定する」
  を実施）:**

  | action | 起動する | 根拠 |
  |---|---|---|
  | `child_closed` | ✅ | 作業子の終端 |
  | `wake_due` | ✅ | 起動条件の発火 |
  | `answered` | ✅ | 人の回答 |
  | `noted` | ✅ | 外部 link/続報・人の card 更新 |
  | `attrs_set` | ✅ | Sweep の続報 (summary)。自己ループ除外は type ではなく起動由来で行う（下記） |
  | `go`/`start`/`park`/`complete`/`drop`/`reopen` | ❌ | 状態遷移そのもの（§4.6） |
  | `child_added`/`child_specced`/`child_dropped` | ❌ | 判断の産物 (spec)（§4.6） |
  | `progress`/`child_dispatched`/`done_request`/`fail_request` | ❌ | §4.6 で progress 除外明記。残りは execution machine 共有の非遷移語彙 |

  card machine の全 18 action type について、allowlist の各エントリを個別に
  外す/足す mutation を当ててそれぞれ対応するテストが赤くなることを確認済み
  （`TestIngestCardEventRequest_ActionTypeAllowlist`、
  `internal/orchestrator/card_event_ingest_test.go`）。

  **自己ループ除外は起動由来（launcher/継続先の card_requests 行そのもの）で行う。**
  `IngestActionSignal` の project 単位の自己除外（`WithWriterProjectID`）とは別軸 —
  それを流用すると Sweep の capture/link を失う。`sandbox.TokenContext.CardID`/
  `CardRequestID`（PR-3 が `DispatchPlanner.PlanHook` 経由で task 継続先にも
  積んでいる）を `internal/server/boid_executor.go` の `ExecuteBoidBuiltin` が
  新設の `orchestrator.WithWriterCardRequestID` で goCtx に刻み、
  `IngestCardEventRequest` は書き手の card_requests 行が対象 card 自身の
  現在 launching/attached な行と一致する場合のみ自己ループとして無視する。
  別 card 宛の書き込み、既に finished/failed になった書き手の行、writer
  context 自体が無い書き込み（daemon 自己記録の `child_closed`、人発の
  Web UI/CLI 書き込み）はいずれも自己ループとして扱わない — 4通りとも
  実 DB テストで固定 (`TestIngestCardEventRequest_SelfLoop_*`,
  `_WriterRequestForDifferentCard_*`, `_WriterRequestFinished_*`,
  `_NoWriterContext_*`)。CardID 等価判定・live 状態判定・自己ループ分岐
  そのものを個別に外す mutation を当てて、それぞれ対応するテストが
  赤くなることを確認済み。

  **エラー方針: `IngestCardEventRequest` の失敗は `IngestActionSignal` と違い
  `CreateAction` のトランザクションを失敗させる。** 「対象外」(resolver 未配線 /
  action type 対象外 / card status 対象外 / card_events 未宣言 / 自己ループ) と
  `ErrCardRequestDuplicateCause`（原因 ID の再配達）だけが no-op で、それ以外の
  エラーは呼び出し元に伝播する。この方針を実装した上で `go test ./...` は
  全パッケージ green のまま（既存の呼び出し元・テストへの影響なし）—
  package-level `orchestrator.CreateAction` のシグネチャに `CardEventResolver`
  引数を追加したことに伴う call-site 更新（既存テスト ~25 箇所へ `nil` を
  1つ追加）はコンパイルを通すための機械的な変更で、動作の変更ではない。
  tx 失敗を実 DB で確認するテスト
  (`TestCreateAction_CardEventIngestHardError_RollsBackActionToo`) は
  `card_requests` テーブル自体を drop して genuine な DB エラーを起こし、
  action 行が rollback されることまで固定している。

  **`taskType != TaskTypeCard` ガードは現状の DB スキーマ下では到達不能
  （mutation testing で判明、記録のみ）。** `0045_card_sti_migration.sql` の
  CHECK 制約 `type != 'execution' OR status IN ('pending','executing','awaiting','done','aborted')`
  と `type IN ('card','execution')` の組み合わせにより、card 型でない task は
  parked/working という status を最初から持てない（隣接する
  `type != 'card' OR status IN (...)` の方は card 行を縛るもので、この結論の
  根拠にはならない）。よってこのアプリ層チェックを外す mutation を当てても
  既存テストは落ちない。`IngestActionSignal`（`actionTargetTypeAndProject`）との構造的対称、
  および将来のスキーマ変更に対する多層防御として残したが、DB 制約が変わらない
  限り実質 dead code である点は次段の実装者が把握しておくこと。

  **判明した制約: 外部 link（identity の束縛）は action 行を書かないので
  この経路では拾えない。** `write.py` の `link` verb →
  `TaskAppService.LinkIdentity`（`internal/api/task_identity.go`）は
  `CreateAction` を一切通らない。実際の Sweep は link の後に `summary`
  （= `attrs_set`）を書くので、内部イベント起動はそちらで拾われる —
  Sweep の capture/link 自体を internal event として拾うことは意図的に
  していない（外部 connector からの新規 capture は Sweep 自身が既存の
  `triggers[]` 定期起動または手動コマンドで処理する対象で、この PR の
  スコープ外）。

  **`CardEvents` は `ProjectStore.Get` 経由で実際に引けることを実行して
  確認した。** `json:"-"` タグは API 直列化のみに影響し、この in-process
  経路には効かない（`TestProjectStore_CardEventCommand_FromRealProjectYAML`
  が実 project.yaml → `ProjectStore.Load` → `CardEventCommand` の全経路を
  実行して確認）。

  **PR-4c への必須前提 1: claim 側で card status を再チェックすること。**
  ingest は action 適用時点の status を見るが、**その時点で parked/working
  だった card が同じ transaction 内で終端になる経路がある** —
  `internal/api/suggestion_accept.go` は `answered` を CreateAction し
  （この時点で card は working なので queued 行が作られる）、同じ tx の後段で
  受理された verb（`complete`/`drop`）の `UpdateTask` を走らせる。commit 後に
  残るのは「done な card に queued な card_events 要求」。`complete` の
  suggestion を人が accept するのは最も普通の操作なので稀ケースではない。
  ingest 側で先読みして潰す設計にはしていない（action 適用の途中結果に
  依存させると contract が壊れる）ので、**§4.6 の「done/dropped の card には
  自動起動しない」は claim 時の再チェックで担保する。** これを落とすと
  終端 card が自動起動する。

  **PR-4c への必須前提 2: force-release された card は、孤児継続先の
  書き込みで再起動しうる。** §10 の既存 KNOWN GAP（retry/force-release は
  前の継続先 session/task を止めない）との合わせ技。force-release で
  request は `failed` になるが継続先 job は生きており、その job がその後
  `attrs_set`（write.py の summary 書き戻し）を撃つと、自己ループ除外の
  live 判定（launching/attached のみ）を外れるため除外されず、新しい
  queued 行が生まれる。PR-4a が `ForceReleaseCardRequest` で sibling を
  queued に戻さず failed にした意図（「直後の claim で運用者が止めたはずの
  card が再起動するのを防ぐ」）を横から破る経路になる。**PR-4c は
  「force-release された card を次の人の操作まで claim しない」等の扱いを
  決めること。** 自己ループ除外を「failed になった書き手の行も除外する」側へ
  広げる案は、正当な retry 後の書き込みまで殺すので単純には採れない。

  **記録のみ（PR-4c で扱いを決めてよい）:**
  - Go の作業 task は `PlanHook` が意図的に CardID/CardRequestID を刻まない
    （`internal/orchestrator/planner.go` の Go 除外）ため、作業 task が card に
    `noted`/`attrs_set` を書くと自己ループ除外を受けず queued 行が積まれる。
    無限ループにはならない（枠が空くまで queued、その後の起動は自分の
    request で除外される）が、`child_closed` と二重に要求が積まれる。
    fold で吸収される想定なら追加対処は不要。
  - `ProjectStore.CardEventCommand` は非 hydrate の `Get` を使い、起動側の
    `RunCardCommandAsHuman` は `hydrateMetaForTriggers`（`GetWithWorkspace`）を
    使う非対称がある。`card_commands`/`card_events` は workspace hydration の
    対象外フィールドなので現状は同値だが、hydration の対象が広がると
    読み取り元が食い違う。

  この PR では `ClaimQueuedCardRequests` を呼ぶ経路を追加していない —
  queued 行が積まれるだけで、PR-4a が固定した「既存の 3 つの
  reconcile/recover 関数は queued 行に触らない」テスト
  (`TestQueuedCardRequests_UntouchedByReconcileAndRecovery` 等) も
  引き続き green。dispatch（claim → launch）は PR-4c の担当。
- **PR-4c で確定: queued 行を捌く dispatcher。** `ClaimQueuedCardRequests` の
  呼び出し元を実装し、queued→launching→launcher 起動の経路を繋いだ
  (`internal/orchestrator/card_request_dispatch.go` の
  `ClaimQueuedCardRequestsForDispatch`、`internal/api/card_request_auto_dispatch.go`
  の `dispatchQueuedCardRequest`)。起動タイミングは commit 後の即時試行を
  主経路とし、`tryDispatchQueuedCardRequest` をコミット点 6 か所から呼ぶ
  (`applyAction` の汎用パス、`applyAnswered`、`recordChildClosedOnParent`、
  `recordVanishedChildClosedOnParent`、`recordWakeDue`、
  `releaseCardRequestForTerminalTask`)。周期 `CardRequestDispatchLoop`
  (`internal/api/card_request_dispatch_loop.go`、30 秒間隔) は通知欠落・
  daemon 再起動時の復旧専用に位置づけた。

  **claim 側の status 再チェック: 実装した。終端のさせ方は「対象 card の
  queued 行を全て failed に落とす」。** `ClaimQueuedCardRequestsForDispatch`
  が claim の直前に card の現在 status を再読し、parked/working 以外なら
  `failAllQueuedCardRequests` で queued 行を一括 failed にして
  `ErrCardNotEligibleForDispatch` を返す。`suggestion_accept.go` の
  answered{accept: complete} が同一 tx 内で queued 行を作ってから card を
  done にするケースを、実 DB で end-to-end 固定した
  (`TestApplyAnswered_AcceptComplete_DrainsRaceQueuedRowInsteadOfLaunching`)。
  1 件だけでなく queued 中の全行を落とす判断にした理由: 対象が終端なら
  再判断してももう一度同じ結論になるだけで、1 件ずつ周期ループが拾い直すのは
  無駄。

  実装中に見つけた別バグ: `TaskRepository.ClaimQueuedCardRequestsForDispatch`
  の transaction wrapper が、`db.InTxDB` の「戻り値が non-nil ならロール
  バックする」という挙動により、この drain の副作用ごと巻き戻してしまって
  いた（`ErrCardNotEligibleForDispatch` を素朴に closure の戻り値として
  返すと drain の UPDATE も一緒に消える）。生の `*sql.DB` に対する直接呼び
  出しのテストでは各文が個別に auto-commit するため発覚せず、
  `TaskRepository` 経由（本番が実際に使う経路）で初めて再現した ——
  `TestTaskRepository_ClaimQueuedCardRequestsForDispatch_IneligibleDrainCommits`
  として固定。`orchestrator.IsCardRequestDispatchSkip` を新設し、「claim
  できなかった」系のエラーは wrapper 内で捕捉して commit させ、呼び出し元
  にはエラー値だけを返すようにした。

  **force-release 抑止の方式: 専用テーブル `card_force_release_barriers`
  (card_id 主キー) を新設した。** `ForceReleaseCardRequest` が対象 card の
  barrier 行を立て、`ClaimQueuedCardRequestsForDispatch` は claim 前に
  barrier の有無をチェックして存在すれば `ErrCardForceReleaseBarrierActive`
  を返し、queued 行はそのまま（failed にはしない — 一時的な抑止であって
  恒久的な失敗ではないため）。「人の操作」として数えたのは 3 つ: 人発カード
  コマンド (`RunCardCommandAsHuman`)、Go (`reserveGoCardRequest`)、明示的
  Retry (`RetryCardRequest`) — いずれも barrier を無条件にクリアする
  （Retry は対象 card の barrier のみ）。

  **Opus レビューで訂正: 「コマンド/Go は呼び出しがあった時点でクリアし、
  実際に枠を取れたかどうかは問わない」は不正確だった。**
  `RunCardCommandAsHuman` の barrier クリアは `s.Tx.WithinTx` の中で
  `ClearCardForceReleaseBarrier` → `cardWorkChildOccupantTx` の順に走る。
  `cardWorkChildOccupantTx` は対象 card が `done`/`dropped` なら
  `StatusError{Code: http.StatusConflict}` を返し、`WithinTx` はその
  エラーでトランザクション全体（先に呼んだ barrier クリアも含む）を
  rollback する。つまり終端 card への人発コマンドは barrier を解除しない。
  占有中（`ErrCardRequestSlotOccupied`）の人発コマンドは
  `cardWorkChildOccupantTx` 自体はエラーを返さないので、この場合の
  barrier クリアは意図どおり生き残る — 「実際に枠を取れたかどうかは
  問わない」が成立するのは占有ケースのみ。実害は無い（終端 card はそもそも
  自動 dispatch されない — §4.6 の「done/dropped の card には自動起動
  しない」対象外）が、記述と実装が食い違っていた。各経路を実 DB でテスト:
  `TestForceReleaseCardRequest_SetsBarrier`、
  `TestRunCardCommandAsHuman_ClearsForceReleaseBarrier`、
  `TestReserveGoCardRequest_ClearsForceReleaseBarrier`、
  `TestRetryCardRequest_ClearsBarrier`。migration 0054。

  **session 継続先の即時/周期: 周期のみとした。即時フックは追加していない。**
  `ReleaseCardRequestForTerminalTarget`（task 継続先の即時解放）は
  `ReleaseCardRequestForTerminalTargetWithCard` に拡張し、解放された card_id
  を呼び出し元に返すようにして task 側の即時 dispatch を実現した
  (`TestReleaseCardRequestForTerminalTask_CommitTriggersImmediateDispatch`)。
  一方 session 継続先の終端は既存の `ReconcileCardRequestSlots`（周期 30 秒）
  が引き続き唯一の解放経路で、そこに即時フックは足していない —— session
  終端に daemon 内で同期的に反応できるイベントが無く（job 終了は runner
  からの非同期通知）、新しい配線を増やすより、既に periodic dispatch sweep
  （同じ 30 秒間隔）が queued 行を拾う構造で十分と判断した。

  **自動起動の継続先を task に限る制約: 未着手ではなく既存実装で成立済みと
  確認した。** `internal/server/boid_executor_agent_start.go` の
  `executeAgentStart` が `cardRequestOrigin(row) == cardContextOriginEvent`
  なら session 起動を拒否する分岐を先行 PR の時点で既に持っていた。ガードを
  外す mutation を当てて `TestBoidOpAgentStart_EventOrigin_Rejected` が
  赤くなることを確認済み —— 新規実装は不要だった。

  **dispatcher の上限・並行性: 既存 dispatcher に明示的な同時実行上限は
  無いことを確認した**（docker-out-of-docker で job ごとに使い捨てコンテナ、
  ホストリソースが暗黙の上限）。

  **Opus レビューで訂正: 「利用可能枠が無いとき（StartExec が失敗した
  とき）は `FailCardRequest` で claim した行を failed に戻し」は 2 つの
  別物を混同していた。** 実際には経路が 2 つある。
  - **枠が占有されている**（`ClaimQueuedCardRequestsForDispatch` が
    `ErrCardRequestSlotOccupied` を返す、claim 自体が成立しない）場合は
    §4.6「利用可能枠が無い時には pending のまま保持する」どおり、行は
    **queued のまま**残る。
  - **claim には成功したが StartExec が失敗した**場合は別で、claim 済みの
    head 行は `FailCardRequest` で **永久に failed** になり、fold されて
    いた sibling だけが queued へ戻る
    (`TestDispatchQueuedCardRequest_StartExecFailure_FailsClaimAndRequeuesFold`)。

  §4.6 の契約6（「上限に当たったとき queued 行を失わない」）が現状満たされて
  いるように見えるのは、**container backend に同時実行上限が無いから
  StartExec がほぼ失敗しない**からに過ぎない。将来 dispatcher に同時実行
  上限を導入すると、上限に当たった瞬間の claim が StartExec の失敗として
  現れ、上のパスに落ちて head request が永久に failed になる — 「上限に
  当たったとき queued 行を失わない」契約はその時点で破られる。上限を導入
  する PR はこの区別（`ErrCardRequestSlotOccupied` 相当の queued 化 vs
  `FailCardRequest` による永久 failed 化）を意識すること。

  無制限の高速再試行にならない理由: 失敗は即座に再試行されず、次の commit
  契機か 30 秒周期まで待つ。同一 card への並行 claim は
  `idx_card_requests_active_unique` が守り、5 並行呼び出しで 1 件しか起動
  しないことを `-race` 付きで固定した
  (`TestDispatchQueuedCardRequest_ConcurrentCallersClaimOnlyOnce`)。

  **起動失敗の扱い: 既存の `FailCardRequest` の fold 復帰をそのまま再利用
  した。** 追加のリトライ上限は設けていない —— 周期ループの間隔（30 秒）
  自体が既に十分な back-off になっている。

  **スコープ外の外部 Sweep handoff ack 順序の調査結果: 現状で成立している
  ことを確認した。** `boid-metaproject/scripts/boidmeta/write.py` の
  `Executor.run()` は handler（capture/link/summary の実処理）→
  `notify_progress`（耐久化された記録）→ `inbox.ack` の順で呼び、
  `boid_store.py._run()` が非ゼロ終了で例外を送出するため、途中の失敗は
  後続（特に ack）を止める。`_record()` のコメントにも「記録の書き込み →
  ack の順を維持する、ack を先に打たない」と明記されており、実装もその
  通り。この PR での対応は不要（記録のみ）。

  **未着手（記録のみ）:**
  - 周期 `CardRequestDispatchLoop` の interval（30 秒）と
    `CardRequestLifecycleLoop` の interval（30 秒）は独立した値で、将来
    どちらかだけ変える設定を追加する場合は両者の関係を意識すること。
  - `dispatchQueuedCardRequest` が peek した commandKey と実際の claim 時の
    commandKey がズレた場合 (`ErrCardRequestCommandKeyChanged`) は今回の
    呼び出しでは再試行せず、次の commit か周期ループに委ねる。project.yaml
    の `card_events.command` を運用中に変更した直後の極めて狭い窓でのみ
    発生しうる。
  - **Opus レビューで判明、未記録だった事実: 人発コマンドは背後の queued な
    event 行を fold しない。** `RunCardCommandAsHuman` は claim 経路
    (`ClaimQueuedCardRequestsForDispatch`／`ClaimQueuedCardRequests`) を
    通らず `CreateCardRequest` で直接 `launching` 行を INSERT するので、
    その card に既に queued な内部イベント要求があっても fold されない。
    よって card は `launching`（人発）と `queued`（内部イベント）を
    同時に持ちうる — 実際に観測されている。PR-6 が「1 card あたり
    アクティブな要求は高々1件」のような前提を置く場合、この非対称を
    踏まえること。
  - **N3（Opus レビュー、記録のみ）: 人発経路との占有チェックの非対称。**
    `RunCardCommandAsHuman` は `cardWorkChildOccupantTx` が live な子 task
    を報告すると拒否するが、`dispatchQueuedCardRequest` には同等のチェック
    が無く `idx_card_requests_active_unique` だけに依存している。
    force-release + `RetryCardRequest`（barrier を clear する）の後、
    孤児の継続先はまだ生きていて card の live な子のままだが、その
    `card_requests` 行は `failed` — なので auto-dispatch は「人なら拒否
    される」2 本目のコマンドを claim して起動しうる。下流の
    `boid task create --parent <card>` のゲート（`cardChildSlotConflict`、
    `OpenChildCount > 0`）が 2 本目の継続先の実作成を止めるので、結果は
    二重実行ではなく「無駄なコンテナ 1 個と 60 秒後に失敗する request」に
    留まる。
  - **N4（Opus レビュー、記録のみ）: 高価な処理が安いリジェクトより先に
    走る。** `dispatchQueuedCardRequest` は status/barrier/占有チェックの
    **前**に `GetTask` + `hydrateMetaForTriggers`（`workspaceStore.Load`
    → workspace.yaml のディスク読み）をやっている。12 時間走る作業 task が
    枠を握っている card や、設計上ずっと queued のままの barrier ブロック
    card では、30 秒ごとに永久にディスクを読み続ける。

  **project meta が引けないときは queued のまま残す（B1 の対処で確定した契約）。**
  `dispatchQueuedCardRequest` は `hydrateMetaForTriggers` が nil を返す状態
  （project.yaml が今 `ProjectStore.metas` に無い — `boid project fetch` の失敗、
  parse エラー、起動順序）を **`ErrCardForceReleaseBarrierActive` と同じ
  「後でまた試す」skip** として扱い、行を `queued` のまま残す。
  `FailCardRequest` するのは **meta は引けたが `card_commands` にそのキーが
  無い**ときだけで、そのときは Warn ログを出す。
  当初の実装はこの 2 つを同一視して「command が project.yaml から削除された」
  として行を failed に落としており、YAML 構文エラー 1 つで 30 秒以内に
  その project の queued 行を持つ全 card が無言で drain され、yaml を直しても
  戻らなかった（手動 `RetryCardRequest` が必要）。§4.4 の「耐久要求」という
  前提そのものに反するので閉じた。
  **帰結として、load されない project の queued 行は無期限に残る。** これは
  「復旧不能かつ無言」より望ましい状態として意図的に受け入れたもので、meta が
  戻れば同じ行がそのまま dispatch される（実 DB で確認済み）。ただし残った行は
  上記 N4 のとおり毎 tick hydrate を 1 回ずつ呼ぶので、コストは行数に比例する。
- **PR-2d-5 で一部対応: retry/force-release は前の継続先 (session/task) を止めない
  (KNOWN GAP、`boid_executor_agent_start.go` の孤児 session と同系統)。** 完全な
  停止処理はまだ実装していない — `boid task release-card-request` が解放前の
  target_kind/target_id を読み、生存中の継続先があった場合は operator_notice で
  警告するところまでで留めた。実際に停止する仕組み（`boid agent stop` 相当）は
  引き続き follow-up。
- 履歴 snapshot と GC の保持範囲、既存履歴の再構成限界。タイムゾーンは初期版サーバ TZ で確定（§5.4）。
- **PR-2d-4 で確定: 直接 `--parent <card>` 実行タスク作成パス（`createExecutionTask`）の
  read-then-write。`card_requests` 行を持たない create（`CardRequestID==""`）は
  再チェックと INSERT を同一 `WithinTx` に閉じ、RunCardCommandAsHuman/acceptGo の予約と
  完全に調停する（`TaskAppService.Tx`、`internal/api/task_create.go`）。
  当時 `CardRequestID!=""` の経路は対象外のままだった
  （**PR-2d-6 でこちらも同じ `WithinTx` に入れた — 下記参照**）。

  **PR-2d-5 で訂正: 「二重に `WithinTx` を開くとデッドロックする」という上記の
  理由づけは不正確だった — 実行して確認済み。**
  `TaskRepository.CreateTaskLinkedToCardRequest` は自分の `db.DBTX` が生の
  `*sql.DB` かどうかで分岐しており、tx に紐づいた repo（`apiTransactor.WithinTx`
  が渡す `apiTxStore.tasks` のように `*sql.Tx` を持つ repo）を通せば
  `db.InTxDB` を新たに開かず、既存 tx の中でそのまま `CreateTask`+
  `AttachCardRequest` を実行できる。
  `TestCreateTaskLinkedToCardRequest_TxBoundRepo_DoesNotDeadlockInAnOuterTx`
  (`internal/orchestrator/card_request_linked_tx_nesting_test.go`) が
  `SetMaxOpenConns(1)` 下の実 DB で外側 tx 内から呼んでもデッドロックしない
  ことを実行して確認している。よって `TxStore` に
  `CreateTaskLinkedToCardRequest` を生やして `apiTxStore` から
  委譲するだけなら、実際にはデッドロックしない。

  **PR-2d-6 で訂正: 上記「launcher の継続は必ず ROOT task なので `cardParent`
  はこの経路では常に nil」は不正確だった。** これは launcher/継続タスク自身が
  さらに子タスクを作る場合（そのタスクの `ParentID` は自分自身が
  `ParentID==""` の ROOT task）の話であって、acceptGo 自身が「予約した子を
  task 化する」最初の `TaskCreator.CreateTask` 呼び出しには当てはまらない。
  `internal/api/workflow_card.go` の acceptGo は `ParentID: taskID`
  （card 自身）かつ `CardRequestID: cardRequestID`（自分の予約）で呼んでいる —
  このケースは `cardParent != nil` になり、`createExecutionTask` の
  非トランザクショナルな `cardSlotConflictWithRequests` 事前チェックを
  通っていた。**`RunCardCommandAsHuman` はこれには当てはまらない** —
  その launcher 自身は `TaskCreator.CreateTask` を一度も呼ばず、継続の
  task 化は launcher job が後で発行する `boid task create` 経由
  （`internal/server/boid_executor.go`）で、そちらは `--parent <this card>`
  を明示的に拒否して ROOT task しか継続にできない（`ctx.CardRequestID` は
  `createReq.ParentID == ""` のときにしかスタンプされない）。

  **`idx_card_requests_active_unique` が「調停する」という表現も不正確
  だった。** この UNIQUE INDEX は `card_requests` テーブル自身の行にしか
  効かない — `CardRequestID==""` の直接 create は `card_requests` 行を
  一切取らないので、index がこの create を直接弾くことはできない。実際に
  効いていたのは `cardSlotConflictWithLister` が `ListCardRequestsByCard` で
  毎回フレッシュに読む「アクティブな `card_requests` 行があるか」チェックの
  方で、index はその読みが返す集合が「card あたり高々1行」であることを
  保証しているだけ。

  **PR-2d-6 で閉じた:** `createExecutionTask` の `atomicCardCheck` 分岐を
  `CardRequestID!=""` のケースにも拡張し、`cardParent != nil` かつ
  `s.Tx != nil`（wire.go は本番で常にこれを満たす）ならどちらの
  `CardRequestID` ケースも同一 `WithinTx` に通すようにした
  （`internal/api/task_create.go`）。フレッシュな再チェック
  （`cardSlotConflictWithLister`）と INSERT（`CardRequestID==""` なら
  `tx.CreateTask`、`CardRequestID!=""` なら `tx.CreateTaskLinkedToCardRequest`
  — PR-2d-5 が `TxStore` に生やしていたが `createExecutionTask` からは
  一度も呼ばれていなかったメソッド）が同じトランザクションに入るので、
  「非トランザクショナルな事前チェック」と「別ラウンドトリップの INSERT」の
  間の read-then-write の隙間は、acceptGo 自身の子 dispatch と並行する
  直接 `--parent <card>` create のペアについて構造的になくなった
  （`RunCardCommandAsHuman` はこの分岐を経路として使わない — 上記参照）。
  `internal/api/task_create_card_slot_atomic_test.go` の
  `TestCreateTask_AtomicPath_CardRequestIDCarrying_RoutesThroughOneTx` /
  `_RejectsWhenAnotherOccupantExists` が実 DB でこれを固定している。

  **`s.Tx` が nil のときは今も旧経路（非トランザクショナルな事前チェック→
  別ラウンドトリップの `CreateTaskLinkedToCardRequest` 呼び出し）に
  フォールバックする。** wire.go が本番で常に `Tx` を渡している前提が崩れ
  ない限り実害はないが、この前提自体は型で強制されているわけではない
  （nil を渡せば通ってしまう）。

  **PR-0 で閉じた: 上記2つの更新系 write port の非トランザクショナルな
  pre-check → 別ラウンドトリップ書き込み。** `TaskAppService.UpdateTask`
  （reparent 経路）と `RerunTask` を、`updateTaskWithCardSlotRecheck`
  （`internal/api/task_service.go`）経由で `createExecutionTask` の
  `atomicCardCheck` と同じ形に揃えた — `s.Tx != nil` なら fresh な
  再チェック（`cardSlotConflictWithLister`）と `tx.UpdateTask` を同一
  `WithinTx` に閉じ、`s.Tx == nil` なら既存の非トランザクショナル経路に
  フォールバックする。`internal/api/task_update_rerun_card_slot_atomic_test.go`
  が実 DB で「1回の `WithinTx` を通ること」「別の占有者（Go の予約）が
  いるとき 409 になること」を reparent/rerun それぞれについて固定している。

  **訂正: 「同じ invariant への非トランザクショナル pre-check → 別ラウンド
  トリップ書き込みが `createExecutionTask` 以外にあと2箇所ある」という当時の
  数え自体が不正確だった。** 上記の2箇所（`UpdateTask` reparent /
  `RerunTask`）は閉じたが、**同型の3箇所目が未着手のまま残っている**:
  `internal/api/task_create.go` の `createCardTask`（:176-186）——
  `s.cardSlotConflictWithRequests` による非トランザクショナルな pre-check の
  直後、別ラウンドトリップの `s.Tasks.CreateTask(task)` で card 型の子を
  card 直下に作る経路。card 型の子作成は現状 UI/CLI からの手動操作が主で
  acceptGo の自動 dispatch と競合する頻度は低く実務上のリスクは低いと
  見るが、閉じたわけではない。

  **PR-0 で閉じた: 所有権の詐称・誤認 TOCTOU
  （`BoidOpTaskCreate`/`BoidOpAgentStart` の所有権チェックから実際の attach
  までの区間）。** `orchestrator.AttachCardRequestOwned` を新設し、
  呼び出し側が主張する `launcher_job_id` を `UPDATE ... WHERE launcher_job_id
  = ?` として書き込みそのものに埋め込むことで、`GetCardRequest` の別の
  早い読みを信用する代わりに書き込み時点で所有権を再検証するようにした。
  `CreateTaskLinkedToCardRequest`（`internal/orchestrator/repository.go`）が
  `ownerJobID` を受け取って `AttachCardRequestOwned` に渡すよう連鎖し、
  `apiwire.CreateTaskRequest.CardRequestOwnerJobID`（`CardRequestID` と同じ
  `json:"-"` で client-settable ではない）が `BoidOpTaskCreate` の所有権
  チェックが読んだ `ctx.JobID` を運ぶ。`BoidOpAgentStart` 側も同様に
  `AttachCardRequestOwned(..., ctx.JobID, ...)` を直接呼ぶ。所有権不一致は
  新設の `ErrCardRequestOwnerMismatch` で、冪等な再試行の収束
  （`ErrCardRequestInvalidTransition` 経由の既存ロジック）とは別のエラーに
  区別される。`internal/orchestrator/card_request_owned_test.go` が実 DB で
  「launching だが launcher_job_id が別」の行への attach 拒否・正しい所有者
  なら成功・同じ target への再試行の収束の3点を固定し、
  `internal/server/boid_executor_agent_start_test.go` の
  `TestBoidOpAgentStart_OwnershipReclaimedBeforeAttach_Rejected` が
  executor 層での挙動も固定している。
- **未着手（PR-0 レビューが指摘、記録のみ）: `ErrCardRequestOwnerMismatch` の
  `boid task create` 経路での扱いが非対称。** `attachCardRequestIfNeeded` /
  `createExecutionTask` はこのエラーを専用分岐せず `StatusError{500,
  err.Error()}` に潰しており、枠を奪われた launcher は
  `boid_executor_agent_start.go` 側の明確な「もう所有していない」メッセージでは
  なく不透明な 500 を受ける（task 自体の INSERT は同一 tx で rollback される
  ので状態破損は無い）。
- **未着手（PR-0 レビューが指摘、記録のみ）:
  `TestUpdateTask_AtomicPath_*`/`TestRerunTask_AtomicPath_*` は tx 内の
  fresh 再読と tx 前の stale 読みを区別できない。** 「`WithinTx` 呼び出しが
  1回であること」と 409 の発生しか見ておらず、クロージャ内で使う `tx` を
  意図的に `s.Tasks`/`s.CardRequests`（tx 前の stale store）に差し替えても
  緑のまま通ってしまう。既存の create 経路のテストと同じ限界。所有権側
  （`AttachCardRequestOwned`）は mutation testing で証明済みだが、こちらは
  コード読解での確認に留まる。
- **未着手（PR-0 レビューが指摘、記録のみ）: `CardRequestID` と
  `CardRequestOwnerJobID` が一緒に travel することを型が強制していない。**
  `AttachCardRequestOwned` は owner が空だとランタイム 500 を返すので、
  将来 `CardRequestID` だけを設定する第三の producer が現れるとコンパイル
  エラーではなくランタイム失敗になる。`cardRequestClaim{id, ownerJobID}` の
  ような小さな struct で両者を型に載せる案がある。
- **既知の制約（この PR が持ち込んだものではない、記録のみ）:
  `updateTaskWithCardSlotRecheck` は tx の外で読んだ `task` snapshot を
  `TaskStore.UpdateTask` の全カラム上書きで書いている。**
  （`internal/api/store.go` に既記の stale-snapshot stomp。）tx は
  card-slot invariant を守るが snapshot 自体は守らない —
  `SetMaxOpenConns(1)` 下で BEGIN 待ちが read→write の窓をわずかに広げる。
- **PR-2d-5 で確定: jobs 行が非終端のまま固まった (daemon プロセスは生きているが
  launcher job の行だけ never-terminal になった、あるいは daemon SIGKILL 等) launching
  card_requests 行は、既知の制約として受け入れる。** 周期 self-heal
  (`ReconcileLaunchingCardRequests`) は launcher job 自身が `completed`/`failed`
  に達したことをトリガに動く設計なので、job 行が `running` のまま固まる
  (プロセスは死んでいるが行は更新されない) ケースは永遠に拾われない —
  daemon 再起動時の startup scan (`RecoverLaunchingCardRequests`) は job の状態を
  問わず無条件に走るので、再起動すれば解消する。恒久稼働 (systemd 等での自動再起動)
  を前提に許容し、再起動を待てない場合の逃げ道は既存の
  `boid task release-card-request` (運用者の force-release)。周期 self-heal 側に
  ランタイム/コンテナの生存確認を持たせる拡張や jobs 側の crash recovery を
  独自に足すのは本 PR の範囲外とする。
- **PR-3 で実装: card 書き込み権限の軸、write.py の command context 対応、boid-task の
  workspace workflow 委譲契約。実 harness での縦断検証は Gate A へ送った。**

  野瀬さんの判断（drive task で確認済み）: 動いている daemon が PR-2 より古く、
  サンドボックス内 shim が `boid card context` / `boid agent start` をまだ持たない
  (再デプロイは Gate A の前に別途行う) ため、**この PR では実装と unit/Go テストまでとし、
  session/task 実物からの縦断検証は行っていない。** 完了条件の「動く」「検証」のうち
  実 harness 部分は Gate A の必須項目として残る（§7 の PR-3 行を参照）。

  1. **card 書き込み権限:** `CardCommand.CardWrite`（project.yaml `card_commands.<key>.card_write`、
     既定 false）を新設し、`CardRequestDefinition.CardWrite` として他の `launched_*` 列と
     同じ「queued→launching の瞬間にスナップショット」対象にした（`card_requests.launched_card_write`
     列、migration 0052）。`cardContextResponse` に `card_write`（bool）を追加 — 値は daemon が
     server-side で決めて返し、環境変数・CLI フラグでの昇格経路は無い（`origin` と同じ契約）。
     Go (`CardRequestCommandKeyGo`) はこの軸を一切持たず、作業 task の card 書き込み可否は
     従来どおり behavior の `readonly` に従う。write.py 側は `card_ctx.get("card_write") is True`
     と厳密比較する（Opus レビュー指摘 — truthy 判定だと想定外の値が誤って開く余地があった）。
  2. **実行主体:** `cardContextResponse` に `actor`（`"task"`/`"session"`）を追加 — 呼び出し
     トークンの `TaskID` の有無だけから daemon が導出する（session job は `TaskID` を持たない、
     という既存の事実をそのまま利用）。**設計判断:** 「実行主体を区別して記録する」は
     `boid card context` がこの値を継続先に渡すところまでとし、boid 側の Action スキーマに
     actor/human フラグを追加するところまでは本 PR ではやらない — 理由は、その手の記録は
     §5 のタイムライン読みモデル（PR-5/6）に合わせて設計した方が手戻りが少ないため。
     write.py は取得した `actor` を stderr の監査痕跡（`[write] card context: actor=...`）に
     残すのみで、boid 側の書き込み先スキーマは変えていない。
     **既知の限界（Opus レビュー指摘、未対応）:** launcher exec job も session 継続先も
     `TaskID` を持たないので、両方とも `"session"` と報告される — launcher 自身が
     write.py を呼ぶ設計にはなっていないので実害は無いが、`actor` は今のところ
     「task か、それ以外か」の二値でしかない。
  3. **`boid card context` の exit code:** 「このジョブに card 文脈が無い」を表す exit code を
     ExitCode:1（汎用失敗）から専用の `NoCardContextExitCode`（4、既存の
     `IdentityNotFoundExitCode`/`IdentityConflictExitCode` と同じ並び）に変更した。理由は
     write.py の「card 文脈あり/無し/引けなかった」の3分岐が stderr 文字列の pattern match
     に頼らずに区別できるようにするため。broker.go・boid_executor.go 双方のガードと、
     既存の固定テスト（`TestBroker_BoidCardContext_NoCardContext_RejectedBeforeExecutor` 等）を
     追随させた。
  4. **write.py:** `boid card context` の有無で経路を分岐する（`_readonly_forces_report` の
     Sweep 経路はバイトレベルで無改変）。card 文脈があるときは signals も `BOID_TASK_ID` も
     要求せず、`card_write` だけを書き込み可否の根拠にする（`validate(..., require_signals=False,
     allow_signals=False)`）。**`signals` はフィールドごと拒否する**（当初は「必須にしない」
     だけで受理していたが、Opus レビュー指摘: 渡せてしまうと `_record` が inbox とは無縁の
     event_key を ack できてしまう — 人発コマンドがこの ack 経路に触れる理由が無いので、
     フィールド自体を「知らないフィールド」として拒否する側に倒した）。card context の
     取得自体が失敗したとき（引けなかった、ではなく本当に例外）は report にすら倒さず即座に
     拒否する — 「文脈なし」と「引けなかった」を混同すると、本来 card-command 経由の呼び出しが
     古い Sweep 専用経路（`BOID_TASK_ID` 必須）へ誤って落ちるため。`boid_store.card_context()`
     も exit=0 の空応答（`card_id`/`request_id` が欠けた `{}`）を「文脈あり、権限は不明」として
     素通ししないよう、欠けていたら例外にする（Opus レビュー指摘、現行の daemon 実装では
     到達しないが防御的に閉じた）。
  5. **boid-task/SKILL.md:** 「Workspace workflow delegation」節を新設 — active instruction が
     具体的な実行手順（スクリプト起動等）を明示していれば、Supervisor/Executor どちらの汎用
     フローよりそれを優先する契約を明文化した。Sweep が既にこの形（`readonly:false` +
     「最初の一手」を固定した `default_instruction`）で動いていたのを、Sweep 専用の慣習ではなく
     一般契約として書き下しただけで、新しい機構は足していない。**adapter 側
     (`internal/adapters/{claude,codex,opencode}/run.go`) の配線は変更していない** —
     behavior 名のハードコード分岐が元々存在せず（free naming 前提が既に守られていた）、
     委譲の判断は agent 自身が active instruction を読んで行うので、adapter が dispatch 時点で
     behavior 名から挙動を変える必要が無いため。
  6. **Opus レビューで発覚し、この PR 内で修正: task 継続先には card 文脈が一切
     届いていなかった。** launcher の exec job とその後の session job（`session_job.go`
     の `BuildSessionJobSpec` 経由）は `spec.CardID`/`spec.CardRequestID` を持つが、
     `DispatchPlanner.PlanHook`（`internal/orchestrator/planner.go`、task の hook job を
     JobSpec にする唯一の経路）はそれを一切見ておらず、task 側は `card_id`/`card_request_id`
     を自分の行に持たない（`card_requests.target_id` が逆に task を指すだけ）。
     結果、task 継続先（判断 task の本体）から `boid card context` を呼ぶと常に
     `NoCardContextExitCode` になり、write.py は Sweep 経路に落ちて `BOID_TASK_ID` は
     あるが `boid card context` は使えない半端な状態になり、`readonly:true` の判断 task は
     問答無用で report 強制のまま何も書けない — 「判断 task は readonly:true + card_write:true」
     という本 PR の柱そのものが task 側では成立していなかった。
     **修正:** `orchestrator.GetCardRequestByTaskTarget`（`target_kind='task' AND
     target_id=?` の逆引き）を新設し、`DispatchPlanner` に任意の `CardRequests`
     （`CardRequestByTaskLookup`）依存として追加、`PlanHook` が task 自身の
     card_requests 行を引いて `JobSpec.CardID`/`CardRequestID` に積むようにした
     （`internal/server/wire.go` は `taskLookup`／`DBTaskLookup` をそのまま渡す — 既存の
     `TaskRepository`/`DBTaskLookup` に同じメソッドを生やしただけで新しい store は
     増やしていない）。lookup 失敗は best-effort（card 文脈が無いだけに倒れ、dispatch 自体は
     失敗させない）。session/launcher 側は元々正しく配線されていたので変更していない。
     これで session actor に加えて task actor も実際に到達可能になった。
  7. **Opus 2人目レビューで発覚し、この PR 内で修正: 6. の task 継続先スタンプが
     広すぎた。** `PlanHook` の `GetCardRequestByTaskTarget` 呼び出しは `row != nil`
     しか見ておらず、(a) `CommandKey == CardRequestCommandKeyGo` の行 —
     ターゲットは Go で起動された作業 task 自身 — にもスタンプしてしまい、
     「Go はこの軸を持たない」（上の 1.）と実装が逆になっていた、(b) `status` を
     見ていないため `finished`/`failed` になった request の `card_write` が
     その task の以後の全 dispatch（`boid task reopen` 後を含む）に永続してしまう
     fail-open 方向の穴があった。**修正:** `PlanHook` に `CommandKey !=
     CardRequestCommandKeyGo` かつ `Status ∈ {launching, attached}` の
     フィルタを追加（`card_request_release.go` の Go 除外と同じ方針）。
     併発ドリフトとして、`internal/server/boid_executor.go` の
     `BoidOpTaskCreate` 側コメントが「TASK continuation の後続 create は
     このCardRequestIDを二度と持たない」と書いていたが、この 6. の変更で
     task 継続先の後続 hook job も同じ CardRequestID を持つようになっていたため
     誤りになっていた（SESSION の carve-out と同じ理由で、task 継続先が
     ROOT task を作るたびに所有権不一致の Warn が誤って出ていた）。
     コメントを実態に合わせ、`else if` の carve-out に
     `row.TargetKind == task && row.TargetID == ctx.TaskID` のケースを追加した。
     `internal/orchestrator/planner_test.go` /
     `internal/server/boid_executor_task_create_card_request_test.go` に
     それぞれ固定テストを追加済み。

**未着手 (記録のみ、この PR では閉じない):**

- **`card_write:true` の書き込み対象スコープが request の card 単体より広い。**
  `write.py` の `_refuse_terminal` は書き込み先が `_own_project()`（= その
  card request が属する project）と一致するかしか見ておらず、`card_write:true`
  は project 内の任意の card への書き込みを許してしまう（「この request の
  card だけ」ではない）。Sweep が元々持っていた権限範囲と同じなので今回の
  変更による新規の昇格ではないが、スコープが request 単位ではないことは
  未対応のまま残っている。
- **`write.py` が全呼び出しで `boid card context` を叩くようになった副作用。**
  host 側 CLI（`cmd/card.go`）には `card context` サブコマンドが無いため、
  sandbox 外から `write.py` を走らせると（現状は sandbox 専用運用なので実害は
  無いが）全て exit 1 になる。また broker RPC 呼び出し自体が失敗した場合、
  従来の Sweep は「report に倒して exit 0」だったのに対し新経路では exit 1 に
  変わっている（より安全側の変化ではあるが、挙動変化として未記録だったので
  ここに記録する）。

- **PR-5a で確定: `FinishCardRequest`/`FailCardRequest`/`ForceReleaseCardRequest`
  が card_requests の終端を card 自身の action ログへ自己記録する契約。**
  §4.4 の「card の履歴が request の結果概要を必要とする場合は、request 行では
  なく card 側の action payload に残す」が未実装だった穴を塞いだ。レビューで
  2件の blocker（後述）が見つかり、同じ PR 内で対処した。

  1. **action type 3種:** `command_finished`（`FinishCardRequest`）/
     `command_failed`（`FailCardRequest`）/
     `command_force_released`（`ForceReleaseCardRequest`、運用者の明示的な
     force-release）。`card_requests.status` の値と1対1ではなく、書き込み関数
     と1対1。3つとも `machine_card.go` に `FromStatus: "*", Manual: false`
     （既定値）の非遷移ルールとして登録 — `IsCardTransitionAction` の6動詞
     閉集合には含まれない。**この非 Manual であることは実 DB/実コードで
     mutation テスト済み**（後述の BLOCKER 1）。`internal/timeline` の
     status-group timeline (execution 詳細専用) はこの3 type を実行して
     確認したうえで除外される
     (`TestBuild_ExcludesNonTransitioningActionsWithStampedStatus` に追加、
     ただし本番はこの3 type に `FromStatus`/`ToStatus` を一切スタンプしない
     ため、このテスト自体は防御的な多層防御であって実際の payload 形の
     カバレッジではない — コメントで明記した)。
  2. **payload の形（JSON）:** `request_id` / `command_key`
     （`card_requests.command_key`、常に生存）/ `launched_label`
     （`card_requests.launched_label` のスナップショット、queued のまま
     終端した行では空）/ `launcher_job_id` / `target_kind` / `target_id`
     （未 attach なら空）/ `origin`（`"human"`|`"event"`、
     `orchestrator.CardRequestOrigin(causeID)`）/ `cause_id`（`origin` は
     これを潰した派生値なので、内部イベントへの遡及リンクには生の
     `cause_id` が要る）/ `result`（finish のみ）/ `error`（fail のみ）/
     `reason`（force-release のみ、運用者の指定文言）/
     `force_failed_siblings`（force-release が巻き込んだ folded sibling の
     `{id, command_key}` 一覧、force-release のみ）。`Action.FromStatus`/
     `ToStatus` はどちらも空文字のまま。
     **`result` は daemon 側の固定文言（例:
     `"continuation reached a terminal successful state"`）であり、
     `child_closed` の `childResultSummary`（子の `artifact.report.summary`
     を掘る）に相当するものは持たない。これは意図した設計** — コマンドの
     実際の成果は、そのコマンドが card に書いた summary/spec/suggestion が
     それぞれ独立したタイムライン項目になるので、コマンド項目自身が成果
     テキストを持つ必要が無いため。
  3. **Go 除外:** `card_requests.command_key == CardRequestCommandKeyGo`
     （`__go__`）の行はどの type でも自己記録しない — Go の作業子は
     `child_closed` に既に結果概要を持つ。ガードは
     `recordCardRequestOutcome`（3つの書き込み関数が共有する内部ヘルパ）
     1箇所にあり、呼び出し元ごとに実装していない。`ForceReleaseCardRequest`
     経由（運用者が Go 予約を force-release する場合）も同じガードを通る
     ことを実測。
  4. **card_events allowlist を起こさない:** 3 type とも
     `cardEventIngestActionTypes`（`card_event_ingest.go`）に加えていない。
     自己記録は `orchestrator.CreateAction` に `resolver`/`cardEvents` とも
     `nil` で渡すので、`IngestCardEventRequest` は allowlist を見る前に
     resolver-nil ガードで no-op になる —
     **こちらが本番で実際に効いているガード。** allowlist 自体にも3 type を
     除外側として追加し実測している
     (`TestIngestCardEventRequest_ActionTypeAllowlist`) が、**このテストは
     nil ガードの後段にあるため本番は到達しない防御的な pin** であり、
     生きているガードは (4) の nil 渡し、pin されているだけの防御は
     allowlist 自体、と役割を区別すること。
  5. **`IngestActionSignal`（internal signal ingest）も同時にスキップされる —
     理由は (4) と同じ。** `resolver` を `nil` にしているので、こちらも
     allowlist を持たない `IngestActionSignal` 自体には到達しない。これは
     `child_closed`（`recordChildClosedOnParent` が `tx.CreateAction` 経由で
     `metaResolver`/`cardEventResolver` の両方を実際に wire 済みの
     `TaskRepository.CreateAction` を呼ぶので、両方の ingest を通る）とは
     **意図的に異なる**扱い。理由: `IngestActionSignal` には action type の
     allowlist が一切なく、metaproject を持つ workspace の card task 上の
     全 action を signal inbox（khi の sweep が読む）に流す。コマンド自身の
     終了をそこに流すと、§4.6 が禁じる「コマンドが終わった → また判断する」
     ループを sweep に直接与えてしまう — card_events 除外とまったく同じ
     理由による、もう一段の除外。
     **将来の地雷:** もし誰かがこの3 type を将来 `cardEventIngestActionTypes`
     に足し、かつ書き込みを（`nil` を渡さず）resolver 付きの
     `TaskRepository.CreateAction` 経由に変えると、自己ループガード
     （`writerHoldsCardsLiveRequest`、`card_event_ingest.go`）は救えない —
     このガードは書き手の card_requests 行が `launching`/`attached` の
     ときしか自己ループと判定しないが、`recordCardRequestOutcome` が走る
     時点で終端 UPDATE は既にその行を `finished`/`failed`/`fail` 済みに
     コミットしている。よって正しい writer context があってもガードは
     false を返し、「コマンド終了 → コマンドを queue → 終了 → …」の
     無限ループになる。**nil 渡しが唯一のガードであり、allowlist に頼らない
     こと。**
  6. **同一 tx:** `recordCardRequestOutcome` は呼び出し元から渡された
     `dbtx` にそのまま書く（別 tx を開かない）。自己記録の INSERT が失敗すると
     `FinishCardRequest`/`FailCardRequest`/`ForceReleaseCardRequest` 自体が
     エラーを返す（best-effort にしていない）ので、呼び出し元が tx で
     ラップしていれば request の終端 UPDATE も一緒に rollback される。
  7. **`ForceReleaseCardRequest` も自己記録する（レビューで方針転換、
     BLOCKER 2 として対処）。** 当初「運用者の force-release にも自己記録を
     持たせるかは未決」としたが、レビューで以下が判明したため実装した:
     force-release は barrier（`card_force_release_barriers`）を張って以後の
     自動 dispatch を抑止する副作用を持つが、その barrier 行には `card_id`/
     `created_at` しか無く、**読み口が daemon 内の1箇所
     (`ClaimQueuedCardRequestsForDispatch`) しか無い** — CLI/API/Web UI の
     どこからも「なぜこの card は自動起動しなくなったか」を読めず、
     `GCCardRequests` が30日で card_requests 行を消した後は完全に無言になる。
     `command_force_released` の payload に運用者の `reason` と force-fail
     した sibling の id/command_key を載せることで、この帰属を耐久化した。
  8. **GC:** `GCCardRequests` は card_requests 行を年齢だけで無条件に消すが、
     自己記録の action は card 自身の task_id に対して書かれているので、
     card が終端していない限り `GCTasks` には巻き込まれない — 実 DB で
     両方向（`GCCardRequests` 後に action が残ること／card 終端後の
     `GCTasks` で action ごと消えること）を確認した。
  9. **fold/retry が生む shape の事実（PR-5b の実装者が必ず踏む）:**
     - fold が N 件の queued request を吸収しても、自己記録は claim の
       head（`ClaimQueuedCardRequests` が昇格した1行）だけに書かれる —
       folded sibling は自分の `launched_*` スナップショットを一度も
       持たないので、独立した自己記録も持たない。
     - `RetryCardRequest` は同じ `id` を使い回すので、同一 `request_id` に
       複数の終端 action（例: 失敗 → retry → 再度失敗）が積まれうる。
       `request_id` は action の一意キーではない。
     - **よって PR-5b のタイムライン項目の安定 ID は `actions.id` を使う。**
       `request_id` は項目の同一性ではなく**相関キー**としてのみ使う
       （進行中の固定項目 ↔ 終端項目の対応、および retry 系列のまとめ）。
     - **コマンドには子の `child_added` に相当する起動時 action が存在しない。**
       launcher 経路（`RunCardCommandAsHuman` / `dispatchQueuedCardRequest` /
       `ClaimQueuedCardRequestsForDispatch`）は action を一切書かない。
       したがって §5.3 の「作成位置に項目を置き、終端時刻に軽い finished 項目を
       出して作成位置へリンクする」という**子のモデルはコマンドには適用できない**。
       コマンドは別モデルにする — **終端 action 1 件 = タイムライン項目 1 件**を
       終端時刻の位置に置き、進行中のものだけ live な `card_requests` 行から
       §5.1 の固定項目として描く。固定項目が終端したら、その終端時刻の位置に
       通常項目として現れる（§5.1 の「終了後は通常の時系列位置に戻す」は
       コマンドについてはこの意味になる）。
  10. **確認した呼び出し元（`FinishCardRequest`/`FailCardRequest` 側 9箇所 +
      `ForceReleaseCardRequest` 1箇所、実 DB テストで自己記録の有無を固定）:**
      `ReconcileCardRequestSlots`（task/session の成功・失敗）、
      `attachFoundContinuationOrFail`（`ReconcileLaunchingCardRequests` と
      `RecoverLaunchingCardRequests` の双方から、後者は Go 行にも到達するため
      Go 除外もこの経路で実測）、`ReleaseCardRequestForTerminalTargetWithCard`
      （成功・失敗）、`dispatchQueuedCardRequest`（未宣言コマンドで queued の
      まま fail する経路・claim 後の StartExec 失敗）、`RunCardCommandAsHuman`
      の StartExec 失敗、`acceptGo` の Go 予約解放（自己記録が無いことを確認）、
      `ForceReleaseCardRequest`（人発コマンド解放・Go 予約解放の両方）。
      **未対応のまま残る10番目の終端経路:** `failAllQueuedCardRequests`
      （`card_request_dispatch.go`、`ClaimQueuedCardRequestsForDispatch` が
      対象 card の status が parked/working でないと判定したときの queued
      行一括 fail）は `FailCardRequest` を経由せず raw UPDATE で終端させる
      ため自己記録が無い。価値は低い（その時点で card は既に
      done/dropped/消滅しているので、コマンドの結果概要としての価値が
      ほぼ無い）と判断し対処しなかったが、「全経路を自己記録した」わけでは
      ないことを記録しておく。

これらは §4 の契約・§6 の対処を前提に、Gate A と各実装 PR で確定する。
単一ユーザーの利用を前提に、対話注入・分散ロック・汎用 DAG scheduler は追加しない。
本 doc は実装の実測結果に追随させ、コード読解で確認したことと実行して確認したことを混同しない。

- **PR-5b で確定: card タイムライン読みモデル (Go 側のみ、描画・SSE は PR-6)。**
  `internal/timeline/card.go`（新規、既存 `Build`/`StatusGroup` とは並立する別関数群）。

  1. **置き場所:** `internal/timeline`（新規パッケージや `internal/api` ではない）。
     理由: `internal/api` は `web/templates` を import しており（レンダリング用）、
     `web/templates` は既に `internal/orchestrator` と `internal/timeline` を直接
     import しているため、`internal/api` に置くと将来 `web/templates` から
     読みモデルを直接使えない（import cycle）。`internal/timeline` に足せば
     この制約を素直に満たせる上、新規パッケージを増やさずに済む。
     既存 `Build` が「呼び出し元が解決済みの入力だけを受け取る純粋関数」
     なのに対し、card 用の `BuildCardTimeline`/`CardPinnedItems` は
     `db.DBTX` を受け取り自分で読む — 契約が異なるので `Build` 自体は
     一切変更していない（`TestBuild_*` は無改変のまま green）。
  2. **型:** `CardItem`（`ID`/`Kind`/`Time`/`CorrelationID`/`Pinned`/
     `Action`/`Child`/`Command`）。`Kind` は
     child/child_finished/command/suggestion/answered/summary/note/wake_due
     の8種。`Child`（`*CardChildDetail`）と `Command`（`*CardCommandDetail`）
     はそれぞれ子・コマンドの構造化フィールドを持ち、それ以外の5種は
     生の `*orchestrator.Action` を持つだけ（label 生成等は PR-6 の仕事）。
  3. **cursor の方式:** 「バッチで読み進める」ではなく「項目境界を別に持つ」側を
     選んだ。`BuildCardTimeline`/`CardPinnedItems` はどちらも card の
     action 全履歴を `orchestrator.ListActionsByTask` で一括取得し、
     項目境界へのグルーピング（child_added+child_specced→1つの子項目、
     child_closed/child_dropped→finished項目、attrs_set の
     suggestion/summary キー分割、コマンド終端 action→1項目、等）を
     メモリ上で行ってから、`(Time, ID)` のキーセット cursor
     (`orchestrator.EncodeActionCursor`/`DecodeActionCursor` をそのまま流用)
     で項目単位にページングする。理由: 1card の action 総量は個人利用の
     triage キュー規模で小さく、DB 側バッチ読みの複雑さに見合わない。
     wire cursor の**エンコーディング**（項目の (created_at, id) キーセット）は
     この選択に依存しない。ただし「切り替えても互換」はエンコーディングの
     話に限られる — **cursor が運ぶ値は合成された項目 ID であり、順序は
     導出項目上の DESC で、固定/履歴の分割は live な `task_triage` 状態に
     依存する。制約の全体は下の point 13 に書いた。この段落だけを読んで
     「後から自由に DB 側実装へ移せる」と取らないこと。**
  4. **固定項目と履歴の重複回避:** `loadCardTimelineState` が pinned/history
     両方の項目を一度に作り、`Pinned` フラグで分岐するだけ
     （`BuildCardTimeline` は `Pinned==true` を除外、`CardPinnedItems` は
     `Pinned==true` だけを返す）。同じ計算から作るので、固定表示されている
     項目が解決した後に**同じ ID** で履歴側に現れることが構造的に保証される
     （子: `!closed` の間 pinned、closed になった瞬間 history 側に
     同じ anchor action の ID で出現。suggestion: 現在アクティブな
     suggestion を持つ最新の `attrs_set{suggestion}` action だけを
     pinned、answered で解決されると同じ ID が history に戻る）。
     コマンドの pinned 表現だけは例外 — 進行中のコマンドには対応する
     action がまだ無いので `"pending-command:<request id>"` という別 ID を
     一時的に持ち、終端すると全く別の ID（終端 action の `actions.id`）で
     history に現れる（§10 PR-5a point 9 のコマンドモデルどおり）。
  5. **日付/TZ:** 読みモデルは `actions.created_at` をそのまま UTC
     `time.Time` として持ち、タイムゾーン変換は一切行わない。
     `web/templates/tasks.templ` の既存 execution 詳細タイムラインと同じ
     方針で、`.Local()` は描画時（PR-6）に呼ぶ。
  6. **GC 後も読める概要:** 子は `child_closed`/`child_dropped` action の
     payload から Result/Status/ClosingActionType を復元し、子タスクの
     行が消えていても `TaskExists=false` で描画を続ける。コマンドは
     `orchestrator.ParseCardRequestOutcomePayload`（既存の書き込み用
     private struct を export しただけ、新しい wire 形式は増やしていない）
     で終端 action の payload から CommandKey/Label/Result/Error/Reason
     等を復元する。`Instruction` だけは best-effort（生きている
     `card_requests` 行があれば読む、GC 後は空）——これは意図的な非対称で、
     完了条件が求める「概要が読める」を満たすのは Result/Error/Reason 等
     action payload 由来のフィールドの方であり、Instruction はそこに
     含まれていない（PR-5a の自己記録 payload に元々無いフィールド）。
     実 DB でどちらも「関連行を DELETE してから読み直しても項目が消えない・
     TargetExists/TaskExists が false になる」ことをテストで固定済み
     （`TestBuildCardTimeline_GCSurvival_ChildTaskRowDeleted`、
     `TestBuildCardTimeline_GCSurvival_CardRequestRowDeleted`）。
  7. **mutation テスト結果（各契約ごとに個別に mutation を当てて対応する
     テストが赤くなることを確認済み）:**

     | 契約 | mutation | 結果 |
     |---|---|---|
     | 固定項目が履歴に出ない（子） | `Pinned: !closed` を `Pinned: false` に | 赤 |
     | 固定項目が履歴に出ない（コマンド/子/suggestion 共通ガード） | `BuildCardTimeline` の `if it.Pinned { continue }` を削除 | 赤（2テスト） |
     | 同時刻タイの cursor 側 tie-break | `isOlderThanCursor` の `id < sinceID` を `false` に | 赤（項目が無言で欠落）|
     | 同時刻タイの sort 側 tie-break | `sortCardItemsDesc` の `a.ID > b.ID` を `false` に | 赤 |
     | GC 後も子の概要が読める | `TaskExists` を常に `true` に固定 | 赤 |
     | GC 後もコマンドの概要が読める | `CommandKey` を payload からでなく空文字に固定 | 赤 |
     | 子の作成位置≠finished位置 | 子項目の `Time` を anchor でなく closingAction から取る | 赤 |
     | item 単位 cursor の境界 (`HasMore`) | `len(history) > limit` を `>= limit` に | **最初は既存テストで見逃した（緑のまま）** — 境界一致 (残り件数==limit) を直接見る `TestBuildCardTimeline_HasMoreFalseWhenExactlyLimitRemaining` を追加してから赤に |

     最後の行は「たぶん赤くなるはず」で済ませず実際に当てた結果、既存の
     ページング系テストが境界値をカバーしていない空振りだと判明したケース
     — テストを追加してから再度同じ mutation を当てて赤を確認した。
  8. **既存への影響:** `internal/timeline` の `Build`/`StatusGroup`（execution
     詳細用）は無改変、既存テスト全 green。`internal/api/card_read.go` の
     `CardView`/`/api/cards` REST（`boid card get`/`list` が使う）も
     無改変。既存の card 詳細ページ（`TaskDetailCardBody`/`TaskDetail`）は
     この PR では未接続のまま（PR-6 で差し替え）。
  9. **PR-5c/PR-6 への申し送り:** 一覧活動状態（§5.5）はこの読みモデルを
     参照してよいが未接続。PR-6 は `Action` フィールドのラベル生成
     （suggestion/summary/answered/note/wake_due の各 kind）を自前で行う
     必要がある — この PR は生の payload を渡すところまでで、
     `timeline.BuildActionLabel` 相当の card 版ラベル関数は用意していない。
     加えて point 10〜13（下記）の `CardCommandDetail.Status`、queued/
     launching の非対称、SSE 未接続時のページング欠落、進捗の畳み込み
     未実装を踏まえること。
  10. **フレッシュレビューで発見・修正した実バグ: 進行中コマンドの固定項目が
      queued 行に masked されうる。** `CardPinnedItems` の実行中コマンド選択が
      `ListCardRequestsByCard`（`created_at ASC`）の**先頭一致で `break`**して
      おり、`idx_card_requests_active_unique` が launching/attached の重複だけを
      防いで queued には無制約なことと、PR-4c の「card は launching（人発）と
      queued（内部イベント）を同時に持ちうる — 実際に観測されている」という
      既知の非対称が組み合わさると、**古い未起動の queued 行が新しい実行中の
      launching/attached 行を隠す**バグがあった（実 DB で再現: `sweep` が
      queued のまま先に作られ、`discuss` が後から launching で作られると、
      固定表示は空の `sweep` を指し `discuss` が見えない）。
      **修正:** `pickActiveCardRequest`（`internal/timeline/card.go`）が
      creation 順ではなく **状態の優先順位 (attached > launching > queued)** で
      選ぶようにした。`TestCardPinnedItems_LaunchingWinsOverOlderQueued` /
      `_AttachedWinsOverQueued` が実 DB で固定。旧コメント
      「the single execution slot invariant: at most one such row」は
      queued を含めると成立しない誤った主張だったため削除した。
  11. **`CardCommandDetail` に `Status`（`orchestrator.CardRequestStatus`）を
      追加した。** Pinned のときだけ意味を持ち、queued/launching/attached を
      区別する — §5.5（一覧活動状態、PR-5c）が「task が pending なら
      Queued とし実行中と誤認させない」を実装するのに必須で、`Label` は
      queued 行では空なのでそれだけでは判別できなかった。terminal な履歴項目
      では空文字のまま（`Outcome` の方が正）。
  12. **N+1 を解消した。** 子 1 件ごとの `GetTask`、コマンド終端項目 1 件ごとの
      `GetCardRequest`+`GetTask` を、`orchestrator.ExistingTaskIDs`
      （新設、`SELECT id FROM tasks WHERE id IN (...)` 1 回）と、既に読んでいた
      `ListCardRequestsByCard` の結果を id で引く map（`requestsByID`）に
      置き換えた。実測（in-memory SQLite、300 children/約1200 actions）:
      **67.1ms → 3.6ms（約18.6倍）**。スキャン軸（action 総量）自体は安く、
      支配的だったのは N+1 だった。
      **閾値の記録:** 1 card あたり約 20,000 actions（年単位の運用相当）で
      スキャン軸も効き始め、ページ描画が概ね 0.5 秒に達する
      （フレッシュレビューの実測、in-memory SQLite = 楽観側。本番はコンテナ内
      file-backed DB でさらに重い）。「personal-scale だから大丈夫」だけを
      根拠にせず、card の action ログは非終端の間 GC されず単調増加すること、
      PR-4 の内部イベント駆動で生成レートが上がっていることを踏まえ、
      将来 DB 側バッチ読みへの切替が要る規模の目安として残す。
  13. **cursor の「実装方式に依存しない」という記述の訂正。** encoding
      （`(time, id)` の keyset 文字列）自体は安定だが、この PR の cursor は
      **合成された項目 ID**（`<uuid>:summary`、`<uuid>:suggestion`、
      `child:<id>`、`pending-command:<id>`）と、**導出項目**上の
      newest-first `(Time, ID)` 順序を運んでいる。将来 DB 側でバッチ読みに
      切り替える実装は、この ID 文字列と順序を正確に再現する必要があり、
      さらに固定/履歴の分割は live な `task_triage` 状態（どの子が
      open/specced か、どの suggestion が現在有効か）に依存していて SQL の
      行単位では導出できない — 結局 detail 全体の読みが要る。「cursor
      互換は壊れない」自体は達成可能だが、当初の記述より制約は強い。

  **記録のみ（この PR では対処しない）:**

  - **既存 flake の根因: 構造的で、同じテストファイル内の別テストが矛盾する
    前提を assert している。** `TestCreateTask_AtomicPath_RaceWithGoReservation`
    と `TestReserveGoCardRequest_DoesNotSeeADirectlyCreatedLiveChild`
    （どちらも `internal/api/task_create_card_slot_atomic_test.go`、
    PR-0 が最終更新）が、`idx_card_requests_active_unique` が子タスクを
    index しない同じ事実から逆の期待を導いている——前者は「direct create と
    go のうち exactly one が勝つ」を assert し、後者は「既に live な子が
    あっても `reserveGoCardRequest` は成功する」ことを前提にしている。
    goroutine のスケジューリング順によって create 側が先に live な子を
    作ってしまうと両方成功し、前者が構造的に失敗しうる。main
    （`3c1aeaa3`、本 PR 抜き）で `-race -cpu=1 -count=120` を実行すると
    41% の頻度で再現し、`-count=200`/`-count=400`（cpu 制限なし）では
    再現しない——CI の通常設定では滅多に出ないが、既存・スケジューラ依存で
    あり本 PR による回帰ではない。**修正案（次の担当者向け）:** (a)
    `TestCreateTask_AtomicPath_RaceWithGoReservation` の assertion を
    「両方 fail はしない／結果として live な占有者がちょうど1つ」まで
    緩めるか、(b) 2 つの goroutine を直列化してこのテストから真の並行性を
    抜く。本 PR のスコープ外。
  - **進捗の畳み込み（§5.2）は未実装。** `loadCardTimelineState` は
    `progress`/`child_dispatched`/状態遷移 action を単に無視しており、
    「作業項目の中にまとめる」対象にしていない。ただし今日
    `notify --progress` は子の**自分の** task に書き込み、親 card の
    action ログには一切書かない——畳み込む対象の action が card 側に
    そもそも存在しない。したがって「捨てている」は不正確で、正しくは
    「card 側に進捗 action の経路が無いので畳み込み対象が無い」。子の
    進捗を親のタイムラインに反映する経路自体（新しい action 種別や
    fan-out）を作るかどうかは PR-6 以降の判断。
  - **SSE 未接続の間のページング欠落 (再現済み)。** ページ1 を読んだ時点で
    pinned だった子が、Load older を押す前に終端すると、その子の
    「child項目」は自分の作成位置（= 現在の cursor より古い位置）に
    履歴として再登場するため、**以後どのページの newest-first スキャンにも
    含まれない**（新規に読み込む「新しい」ページには乗らず、cursor が既に
    その位置を通り過ぎている）。6 件中 4 件しか見えない、という形で
    フレッシュレビューが実際に再現している。PR-6 の SSE による先頭再描画が
    実質的にこれを埋める設計目算だが、その前提はこれまで文書化されていな
    かった——PR-6 は SSE の head 再描画がこの欠落を埋める前提で設計すること。
  - **同時刻タイの中で「最新の suggestion」の選択が非決定的になりうる。**
    `orchestrator.ListActionsByTask`（`store.go`）の `ORDER BY created_at`
    には `id` の tie-break が無い。`EncodeActionCursor` 自身の doc コメントが
    警告している「同時刻の複数行」ケースと同じ穴で、`activeSuggestionActionID`
    の選択（`suggestionActions` の最後の要素を取る）がその非決定性を継承する。
    `ListActionsByTask` は execution 詳細タイムライン等の複数箇所で共有されて
    いる低レベル関数なので、この PR の狭い要求のためだけに順序保証を足すのは
    見送った——実務上、同一 card への複数 `attrs_set{suggestion}` が
    完全に同時刻で衝突する頻度は極めて低い（dispatcher の一括 abort のような
    バルク書き込みパターンが suggestion には無い）。次に触る人向けの記録として
    残す。

- **PR-5c で確定: 一覧の活動状態（§5.5、Go 側の読みモデル + 一覧行の算出まで。
  UI 統合は PR-6）。**

  1. **状態語彙の確定表。** PR-6 はこの表と矛盾しない UI を作ること。

     | 軸 | 入力条件 | 表示文字列 |
     |---|---|---|
     | 作業子 | 有効な子なし（open/specced/dispatched の子が JSON に無い） | （非表示）|
     | 作業子 | 子が `open` | `Draft` |
     | 作業子 | 子が `specced` | `Ready to run` |
     | 作業子 | 子が `dispatched`、実 task が `pending` | `Queued` |
     | 作業子 | 子が `dispatched`、実 task が `executing` | `Running` |
     | 作業子 | 子が `dispatched`、実 task が `awaiting` | `Needs input` |
     | 作業子 | 子が `dispatched`、実 task が `done`/`aborted`、または実 task 行が無い | （非表示、下記参照） |
     | コマンド | アクティブな `card_requests` 行なし | （非表示）|
     | コマンド | 行が `queued`（`launched_label` 未スナップショット） | `<command_key>: Queued` |
     | コマンド | 行が `launching` | `<launched_label>: Launching` |
     | コマンド | 行が `attached`、target が session、または target が task で実 task が `executing`/不明 | `<launched_label>: Running` |
     | コマンド | 行が `attached`、target が task で実 task が `pending` | `<launched_label>: Queued` |
     | コマンド | 行が `attached`、target が task で実 task が `awaiting` | `<launched_label>: Needs input` |

     コマンド軸の `attached` は当初 `card_requests.status` のみで `Running`
     固定にしていたが、レビューで「§5.5 が要求する『task/session の状態』を
     見せていない」と指摘され、target が task の場合は実 task の状態
     （pending/executing/awaiting）を見る形に直した。session target は
     実装を見送った（下記 point 4 参照）。

     **語彙は `liveTaskActivityWord`（`web/templates/list_activity.go`）
     1本に寄せてある。** 当初は両軸が同じ `TaskStatus`→3文字列の switch を
     別々に手書きしており、片方だけ改名しても全テストが緑で通る状態だった
     （§6 の表が `promotedAttrVocabulary` について警告している「手書きで
     手同期」と同型）。共有 helper に寄せた上で
     `TestWorkAndCommandAxes_ShareTheSameLiveTaskWords` が両軸の対応を
     1本のテストで縛っている——helper の文字列を書き換えると
     **両軸のテストが同時に赤くなる**ことを実測で確認済み。

     **終端・不在の task target は「何も言わない」（空）。** 当初は
     `default: return "Running"` で、`done`/`aborted` の target や GC で
     消えた target まで `Running` と表示していた。作業子軸は同じ状況を
     非表示にしており（「`Running` と誤認させるより何も言わない方が安全」）、
     コマンド軸だけ逆の判断をしていたのを揃えた。**session target のみ
     `Running`** — 実 task 行が存在せず、`card_requests` が attached である
     こと自体が唯一の生存情報だから。

     作業子・コマンドは独立した2軸で、両方同時に非空になりうる（例:
     specced な子を持つ card で discuss セッションが起動中）。一覧行には
     両方をそのまま並べて出す（`web/templates/task_list_row.templ` の
     `cardActivityBadges`、CSS class `list-row-activity-work` /
     `list-row-activity-command`）。`Reviewing`/`Discussing` のような用途の
     推測はしていない——コマンド側の文字列は `launched_label`（project.yaml
     の生の定義値のスナップショット）か `command_key`（同じく生の識別子）
     をそのまま出すだけで、daemon が意味を補完する箇所は無い。

     `dispatched` だが実 task が `done`/`aborted`、または実 task 行が
     見当たらない場合は非表示にした（`Running` 等にフォールバックしない）。
     この組み合わせは§3.2の invariant 下では transient（reconcile が
     `child_closed` を書いて JSON 側の子を `closed` に倒すまでの短い窓）
     のはずで、実 task が権威だが不整合な間は「何も言わない」方が
     「まだ動いている」と誤認させるより安全という判断。

  2. **`launched_label` が空のとき（queued 行）の表示: `command_key` を
     出す方を選んだ。** ラベル無しで状態だけ出す案は「どのコマンドが
     待っているか」が一覧から分からず診断性が落ちるため採らなかった。
     `command_key` は project.yaml の生の識別子で、意味の翻訳や補完は
     一切していない（`CommandActivityLabel`、`web/templates/list_activity.go`）。

  3. **launching + queued 共存時: 一覧は `PickActiveCardRequest`
     （`internal/orchestrator/card_request.go`、PR-5b の `pickActiveCardRequest`
     を昇格・export したもの）の優先順位（attached > launching > queued）で
     選んだ1件だけを出す。両方は出さない。** PR-5b がタイムラインの pinned
     項目選択のために作った関数をそのまま共有しており、一覧とタイムライン
     詳細（PR-6）が同じ card に対して異なる「アクティブなコマンド」を
     指す事態を構造的に防ぐ。

  4. **クエリ本数と N+1 回帰ガード。** `WebHandler.TaskList` はこの PR で
     2本のクエリを追加した（`orchestrator.TaskStatusesByIDs`、
     `orchestrator.ListActiveCardRequestsByCardIDs` —— どちらも
     ページ全体の card 行に対して1回、`existingTaskIDsChunk`（500）を
     超えない限りチャンク化されない）。既存の `ListTasks`/`triageByTaskID`
     （`ListTaskTriageByTaskIDs`）の2本と合わせ、**一覧ページ全体で固定
     4本**（実測、`TestWebHandlerTaskList_ActivityState_QueryCountDoesNotScaleWithRowCount`
     が3 card/30 card どちらも4本であることを直接 assert する）。
     `ActiveCardRequestsByCardIDs` を先に呼び、その結果（`attached` な
     `card_requests` 行のうち target が task のもの）の `TargetID` を
     `TaskStatusesByIDs` の**同じバッチに合流**させている——コマンド軸が
     実 task の状態を見る (point 1) ようになった後もクエリは4本のまま
     （フレッシュレビューで「target ごとに追加のバッチ取得が要る」と
     自己申告していたが誤りで、既存バッチに混ぜるだけで済んだ）。
     回帰ガードは `internal/api/web_task_list_activity_n1_test.go`
     の `TestWebHandlerTaskList_ActivityState_QueryCountDoesNotScaleWithRowCount`
     —— `db.DBTX` を実クエリ回数を数える wrapper (`countingDBTX`) でラップし、
     実 DB 上で3 card と30 cardの2回 `TaskList` を呼んで、発行された
     Query/QueryRow/Exec の合計本数が一致し、かつ4本ちょうどであることを
     assert する。`cardActivityStates`（`internal/api/web.go`）を「dispatched
     な子ごとに `TaskStatusesByIDs` を1件ずつ呼ぶ」実装に書き換えるmutation
     を当ててこのテストが赤くなることを確認済み（3 card=6クエリ、
     30 card=28クエリで不一致検出）。コマンド軸の task target 合流を追加
     した後も4本のままであることは
     `TestWebHandlerTaskList_CommandAttachedToAwaitingTask_RendersNeedsInput`
     が実 DB で固定している。

  5. **PR-5b との共有: `PickActiveCardRequest` のみ。** 型/ロジック共有の
     判断は以下のとおり。
     - **共有した:** `pickActiveCardRequest`（優先順位選択）を
       `internal/orchestrator/card_request.go` の exported
       `PickActiveCardRequest` に昇格し、`internal/timeline/card.go`
       （PR-5b）と一覧側の新設 `orchestrator.ListActiveCardRequestsByCardIDs`
       の両方がこれを呼ぶ。挙動は変えていない
       （`TestCardPinnedItems_LaunchingWinsOverOlderQueued` 等 PR-5b の
       既存テストは無改変のまま green）。
     - **共有しなかった: `CardChildDetail`/`BuildCardTimeline`。**
       §5.5 の要求は「一覧は一括取得」で、`BuildCardTimeline` は
       1 card の全 action 履歴を読む関数（PR-5b §10 point 9 が明記した
       とおり "card ごとに呼ぶと N+1 どころではない"）。一覧の作業子判定は
       代わりに `orchestrator.TaskTriageChild`（生の JSON 型、`card.go`）
       と新設 `orchestrator.TaskStatusesByIDs`（実 task の状態のみを
       バッチ取得）を直接組み合わせる軽量な経路（`web/templates/list_activity.go`
       の `ActiveChildFromDetail`/`WorkActivityLabel`）にした。GC 後の
       概要保持（`CardChildDetail.HasResult`/`Result` 等）のような重い
       契約は一覧には不要——一覧が見せるのは「今アクティブか」だけで、
       終端した子の結果概要は一覧の対象外（詳細ページ・PR-6 の仕事）。
     - **型の重複は許容した。** `web/templates/list_activity.go` の
       `CardActivityState` は `internal/timeline` の `CardCommandDetail`
       とは別の、一覧専用の薄い型（`WorkLabel`/`CommandLabel` の2フィールド
       のみ）。共通化すると `internal/timeline` → `web/templates` の依存が
       生まれ、PR-5b が §10 point 1 で明記した「`internal/api` は
       `web/templates` を import しており、`web/templates` は
       `internal/orchestrator`/`internal/timeline` を直接 import している」
       という既存の依存方向と矛盾しない位置に一覧専用ロジックを置くには
       `web/templates` 内に閉じるのが素直だった。

  6. **子の状態と実 task の突き合わせは一覧専用に実装し、
     `CardChildDetail` は流用しなかった。** 理由は上記5の通り
     （`BuildCardTimeline` を一覧で呼ぶと N+1 どころではない）。判定ロジック
     自体（open→Draft、specced→Ready to run、dispatched は実 task 優先）は
     `WorkActivityLabel`（`web/templates/list_activity.go`）に一本化した。

  7. **既存の一覧テストギャップ: 今回のスコープに隣接する分だけ埋め、
     残りは埋めていない。** `docs/plans/webui-detail-list-redesign.md`
     followup が指摘した既存ギャップのうち、この PR は新設ロジックの
     テスト（後述のmutation結果表）に集中し、以下は**未着手のまま**:
     `taskListRowMovement` の exec executing 経過表示・awaiting の
     「⚠ 質問あり」・bare status のデフォルト分岐、`rowIdentityLabel`/
     `cardIdentityLabel`/`execIdentityLabel`、`relativeTimeLabel` の
     境界値（59s/60s、59m/60m、23h/24h）。理由: この PR が触った分岐は
     `taskListRowMovement` の**末尾に追加した新しい `if` ブロック**
     （`cardActivityBadges` の呼び出し）のみで、既存の分岐自体は無改変。
     「触る範囲に隣接するものは埋める」の対象として、新設した
     `CardActivityState`/`ActiveChildFromDetail`/`WorkActivityLabel`/
     `CommandActivityLabel`/`BuildCardActivityStates` の全分岐と、
     `taskListRowMovement`/`BuildListRows` への新規追加分（Activity
     フィールドの伝播、exec 行が活動バッジを出さないことの pin
     `TestTaskListRowMovement_ExecTask_NeverRendersActivityBadges`）は
     埋めた。既存の無改変分岐は次の担当者向けに残す。

  8. **mutation テスト結果（全て「変異を当てる → grep/diff で実際に
     コードが変わったことを確認 → テスト実行」の順で実施。誤って
     mutation が無効化されたまま緑を確認する事故は、Python スクリプトの
     assertion が実際の行内容と食い違って例外を投げたことで1回検出できた
     — 詳細は本 PR の実装記録）:**

     | 契約 | mutation | 適用確認 | 結果 |
     |---|---|---|---|
     | 作業子 `Draft` | `return "Draft"` → `"XDraft"` | grep で旧文字列0件 | 赤 |
     | 作業子 `Ready to run` | 同様に `"XReady"` | grep 0件 | 赤 |
     | 作業子 `Queued`（dispatched/pending） | `"Queued"` → `"XQueued"`（行番号指定+diff確認） | diff 確認 | 赤 |
     | 作業子 `Running`（dispatched/executing） | 同様 | diff 確認 | 赤 |
     | 作業子 `Needs input`（dispatched/awaiting） | 同様 | diff 確認 | 赤 |
     | 作業子: dispatched だが実 task 終端/不在は非表示 | `default: return ""` → `return "Running"` | diff 確認 | 赤（2テスト）|
     | コマンド `queued` ラベル | `": Queued"` → `": XQueued"` | diff 確認 | 赤 |
     | コマンド `launching` ラベル | 同様 | diff 確認 | 赤 |
     | コマンド `attached`→`Running` | 同様 | diff 確認 | 赤 |
     | コマンド: `launched_label` 空時に `command_key` へ fallback | fallback の `if`ブロックを削除 | diff 確認（1回目は行番号ずれで assertion 例外により無適用を検知、修正して再実施） | 赤 |
     | `ActiveChildFromDetail` の非closed選択 | `!=` を `==` に反転 | diff 確認 | 赤（2テスト）|
     | `BuildCardActivityStates` の両軸空省略 | 省略 `if`ブロックを削除 | diff 確認 | 赤 |
     | `cardActivityBadges` の exec 行非表示ガード | `.templ` の `if row.Task.Type == ...` を `if true` に（`templ generate` 再生成込み） | diff 確認 + `templ generate` 実行 | 赤 |
     | `cardActivityBadges` の空文字非表示ガード | `!= ""` を `== "XNEVER"` に | diff 確認 + `templ generate` | 赤 |
     | `PickActiveCardRequest` 優先順位 | attached/queued の rank値(3/1)を入れ替え | diff 確認 | 赤（2テスト）|
     | `PickActiveCardRequest`/`ListActiveCardRequestsByCardIDs` の Go 除外 | `CommandKey == CardRequestCommandKeyGo` 分岐を削除 | diff 確認 | 赤（2テスト）|
     | N+1 回帰ガード | `cardActivityStates` を子ごとに `TaskStatusesByIDs` を呼ぶループへ書き換え | diff 確認 | 赤（3 card=6クエリ vs 30 card=28クエリ）|

     全 mutation は当てた直後に `diff`（または grep でトークン数0件）で
     実際にファイルが変わったことを確認してからテストを実行し、その後
     元ファイルへ復元してから次の mutation・最終ビルドに進んだ。

     **フレッシュレビュー対応で追加した分（同じ手順）:**

     | 契約 | mutation | 適用確認 | 結果 |
     |---|---|---|---|
     | コマンド軸: session/非task target は `Running` 固定 | fallback の `return "Running"` を `"XRunning"` に | diff 確認 | 赤 |
     | コマンド軸: `TargetKind` の task/非task 分岐そのもの | `!=` を `==` に反転 | diff 確認 | 赤 |
     | コマンド軸 `Queued`（attached/task/pending） | `return "Queued"` を `"XQueued"` に | diff 確認 | 赤 |
     | コマンド軸 `Running`（attached/task/executing） | 同様 | diff 確認 | 赤 |
     | コマンド軸 `Needs input`（attached/task/awaiting） | 同様 | diff 確認 | 赤 |
     | コマンド軸: 未知 status は `Running` へ fallback | default を `"Needs input"` に | diff 確認 | 赤 |
     | `cardActivityStates` のバッチ合流（command target を `TaskStatusesByIDs` に混ぜる行を削除） | 対象の `for` ブロックを削除 | diff 確認 | 赤（ラベル誤り + クエリ本数 3 に減少の両方を検出）|
     | badge の描画位置（status/suggestion 本文より前） | TDD で確認 — 位置 assert のテストを先に書いて赤を確認してから実装、実装後に green | `git diff`（.templ + `templ generate` 再生成込み） | 赤→（実装後）緑 |

     **正直な報告: `ListActiveCardRequestsByCardIDs` の `ORDER BY created_at
     ASC, id ASC` 追加（nice-to-have）は mutation で赤くならなかった。**
     `ORDER BY` を外しても `TestListActiveCardRequestsByCardIDs_SameRankTieBreak_OldestWins`
     は `-count=10` で安定して緑のまま。**理由は当初「SQLite が ROWID 順で
     返すから」と書いていたが、これは誤り** —— `EXPLAIN QUERY PLAN` を取ると
     `SEARCH card_requests USING INDEX idx_card_requests_card_status_created
     (card_id=? AND status=?)` で、複合 index `(card_id, status, created_at)`
     が既に created_at 順を供給している。`PickActiveCardRequest` は同ランク内
     でしかタイにならないので、この index がある限り `ORDER BY` の有無は
     結果を変えない。追加そのものは構造的に正しい（index に暗黙依存せず、
     規則が SQL に書かれる）が、「テストが担保している」とは言えない。次にこのクエリを SQL 側で
     書き換える人は、この tie-break の正しさをテストではなくコード
     レビューで守ること。

  9. **plan doc の記述と実コードのズレで気づいたもの（フレッシュレビューで
     訂正）。** §5.5 は「定義されたラベルと task/session の状態を示す」と
     書いており、コマンド側にどちらを見せるべきかは明記していなかった。
     当初は `card_requests.status` だけを見せていたが、レビューで
     「attached の対話 task が awaiting でも `Running` と出て、回答待ちが
     隠れる」という §5.5 が最も避けたい誤認そのものの実害が指摘され、
     point 1 の形に訂正した。**「target ごとに追加のバッチ取得が要る」と
     いう当初の見送り理由は task target については誤りだった** ——
     `CardRequest.TargetID`（target が task のとき）を既存の
     `TaskStatusesByIDs` バッチにそのまま合流させるだけで済み、クエリは
     増えていない（point 4）。**session target だけは実際に別取得が要る
     ので今回も見送った** ——session の生死は job テーブル側の関心事で、
     この PR のスコープ外（`CardCommandDetail.TargetExists` の既存の
     doc comment と同じ切り分け）。よって session target の `attached` は
     `card_requests.status` 止まりで `Running` 固定のまま。PR-6 が session
     target の実状態まで見せたくなった場合は、job テーブル側のバッチ
     取得を新設すること。

  10. **badge の描画位置と CSS（フレッシュレビューで発見・修正）。**
      `.list-row-line3` は `max-height: calc(2 * 1.4em); overflow: hidden`
      で2行クランプする（既存 CSS、`web/static/style.css`）。activity
      badge を suggestion の reason/summary の**後**に追加していたため、
      それらが2行に達する card（khi では普通の行）で badge が丸ごと
      クリップされ**見えなくなっていた**。`cardActivityBadges` の呼び出しを
      `taskListRowMovement` の**先頭**（status/suggestion 本文より前）に
      移して解消した
      （`TestTaskListRowMovement_CardActivity_RendersBeforeStatusContent`
      が HTML 中の出現位置を直接 assert）。CSS class
      `list-row-activity`/`list-row-activity-work`/`list-row-activity-command`
      は当初未定義だったので `flex-shrink: 0` と最小限の色分け（`--accent`/
      `--muted`、既存のライト/ダーク両対応変数を流用）だけ追加した——
      **最終的な見た目・レイアウトの作り込みは PR-6 の仕事**であり、この
      PR が当てたのは「隠れない」を満たす最小限のスタイルに留まる。

  11. **同順位タイの決着規則: `ListActiveCardRequestsByCardIDs` に
      `ORDER BY created_at ASC, id ASC` を追加した（フレッシュレビュー
      指摘）。** 追加前は素の `SELECT` で行順が未規定だったため、同じ card に
      `queued` 行が2件ある場合に `PickActiveCardRequest`（strict `>` な
      ので同ランクはスライス先頭が勝つ）の選択が SQLite の物理格納順に
      依存し、PR-5b が使う `ListCardRequestsByCard`（`ORDER BY created_at
      ASC, id ASC` 済み）の詳細ページ側の選択と食い違いうる状態だった。
      同じ `ORDER BY` を足して構造的に揃えた。point 3 の「構造的に防ぐ」は
      この意味で成立する（タイの決着規則も含めて揃っている）。

  12. **既知の劣化（記録のみ、対応しない）: 旧データで非 closed な子が
      複数ある card は作業子軸が誤報しうる。** `ActiveChildFromDetail` は
      「JSON 内の最初の非 closed 子」を返す。§3.2 の invariant 下では
      高々1件のはずだが、PR-1 が「既存複数子の診断」を用意した経緯どおり
      本番にはこの invariant 成立前のデータが残りうる。実 DB で確認:
      `children=[{open}, {dispatched→awaiting}]` の card は `Draft` と表示され
      `Needs input`（本来最も注意を引くべき状態）が隠れる。優先順位を
      変える実装はしていない——対象は運用開始直後の一時的な移行データで、
      `boid task diagnose-cards`（PR-1）が既に列挙・解消の手段を提供して
      いるため。

- **PR-6a で確定: card 詳細ページの本体描画（コマンド入力・SSE fan-out は
  対象外、PR-6b/PR-6c へ）。**

  1. **配置順は §5.1 のとおり実装した。** タイトル・card 状態・現在の要約
     (`TaskDetailCardSummary`、`task_triage.detail.summary` の現在値) →
     指示入力欄の場所は空けず何も置いていない (PR-6b が担当) → 固定項目
     (`CardPinnedItems` をそのまま `CardPinnedSection` で描画) → 最新10件
     + `Load older` (`CardHistorySection`)。固定項目と履歴は同じ
     `CardTimelineItem` コンポーネントを使い、`Pinned` 引数だけで分岐する
     （別コンポーネントを作っていない）。

  2. **5種類の raw Action のラベル描画規則（`web/templates/card_timeline.templ`）。**
     読みモデルは `*orchestrator.Action` を生で返すだけなので、payload の
     実キー名を書き込み側から拾って自前でパースした:
     - `suggestion`: payload の `{"suggestion":{"verb","reason","params"}}`
       を `orchestrator.Suggestion` にデコードし、固定項目のときだけ
       既存 `TaskDetailSuggestionSection`（Accept/Reject フォーム込み、
       無改変で再利用）を呼ぶ。履歴側（既に superseded/answered）は
       ボタン無しの読み取り専用表示 (`cardSuggestionHistoryBody`) —
       同じ verb でも「今のカードの状態に対して実際に適用できるか」を
       決める `CanApplyManualAction`/`SuggestionInapplicable` の判定は
       *現在* 有効な提案にしか意味を持たないため、履歴項目にボタンを
       出すと過去の提案を誤って承認できてしまう。
     - `answered`: `{"answer","verb","basis"}` を decode し、
       `accept`→"Accepted"、`reject`→"Rejected" の固定英語ラベル。
     - `summary`: `{"summary": "..."}` の文字列をそのまま表示。
     - `noted`: 構造検証が無い任意 JSON なので、既存の `payloadAsYAML`
       （不正 JSON なら生バイト列にフォールバック、パニックしない）を
       再利用しているだけで新しい parser は書いていない。
     - `wake_due`: payload 無し。固定文言 "Wake condition due" のみ。
     いずれも英語ラベル。verb/reason/basis/summary/note 本文は
     templ の `{ expr }` 式（自動 HTML エスケープ）を通しており、
     `templ.Raw` 等のエスケープ回避経路は使っていない — 子タイトル
     フィールドに `templ.Raw` を差し込む mutation で実際に
     `TestCardDetail_MaliciousChildTitle_Escaped` が赤くなることを
     確認済み（下記 mutation 表）。

  3. **日付セパレータの実装方式と TZ をどこで当てたか。** 純粋関数
     `cardHistoryDateSeparators(items, priorDateKey string) []string` が
     items と同じ長さのスライスを返し、各 index に「その項目の直前に
     出すべきセパレータ文字列（無ければ空文字）」を持たせる。判定は
     `item.Time.Local().Format("2006-01-02")` の日付キー同士の比較のみで、
     タイムゾーン変換は `time.Time.Local()` の呼び出し1箇所（この関数と
     `cardItemClockLabel`/`cardItemPinnedStamp`）に閉じている——サーバの
     ローカル TZ で確定という §5.4 の決定どおりで、ブラウザ側 TZ への
     移行はしていない。画面上の確認手段として `CardHistorySection` に
     `Times shown in <zone> (UTC±HH:MM)` という固定表示
     (`cardTimelineTZLabel`、`time.Now().Zone()`) を追加した。
     「Load older で同じ日を継ぎ足しても区切りを重複させない」は
     `priorDateKey` 引数（前ページの最終項目の日付キー）を呼び出し側が
     引き継ぐことで実現し、これは HTTP レイヤーの `last_date` クエリ
     パラメータとして運ばれる（次項）。固定項目は常に日付+時刻を明記
     (`cardItemPinnedStamp`)、履歴項目は時刻のみ。

  4. **Load older の HTMX/フォーム的な作り。** サーバ側ページングで、
     JS の手書きコードは書いていない。`CardHistorySection` は履歴
     `<ul>` の**内側**の末尾に `<li class="card-timeline-load-older">`
     として Load older ボタン (`hx-get="/tasks/{id}/card-timeline?
     cursor=...&last_date=..." hx-target="closest li"
     hx-swap="outerHTML"`) を置く。クリックすると新設ハンドラ
     `WebHandler.TaskCardTimelineOlder` (`GET /tasks/{id}/card-timeline`)
     が次ページ分の `<li>` 群 + （まだ残りがあれば）新しい Load older
     の `<li>`、を返し、`hx-target="closest li"` + `hx-swap="outerHTML"`
     によって**そのボタンを囲む `<li>` 自体**がレスポンス全体に
     置き換わる——新しい項目は同じ `<ul>` 内の兄弟要素として着地し、
     クリックのたびに置き換わるのは常にこの1個の `<li>` だけなので
     ネストが深くなっていかない。
     **フレッシュレビューで発見・修正した実バグ:** 当初の実装は
     `hx-target="this"`（ボタン自身）かつ Load older の `<li>` を
     `</ul>` の**外**に置いていた。`hx-swap="outerHTML"` はボタンだけを
     置き換えるので、2ページ目以降の項目群がリストの外に着地し、
     クリックのたびに孤立した `<li>` の中にさらに `<li>` がネストして
     いく壊れた DOM になっていた——実際にレンダリングして HTML を
     ダンプし、`</ul>` の後に `<li>` が出ていることを目視で確認して
     修正した。`#task-status` の SSE 再描画（`outerHTML` で丸ごと置換）
     とは競合しない——`CardHistorySection` 自体が `#task-status` の外
     （`#card-timeline` という別コンテナ）にあり、既存 SSE スクリプトは
     `kind=status`/`kind=timeline` の2種類しか再取得しないため、この
     新設コンテナは現状 SSE で自動更新されない（§10 PR-5b が記録した
     「SSE 未接続の間のページング欠落」はこの PR ではそのまま——埋める
     のは PR-6c の仕事）。

  5. **既存 `TaskDetailChildrenSection`/`ChildRow`/`cardChildrenFromTriage`
     をどうしたか: 削除した。** 子一覧は固定項目・履歴の `CardItemChild`/
     `CardItemChildFinished` 項目に統合され、独立した子一覧セクションは
     再設置していない。連鎖的に `cardChildrenForDisplay`/`childRowRank`/
     `resolveChildProjects`/`triageChildrenFor`/`triageChildrenForDisplay`/
     `triageSuggestionFor`/`childrenOf`/`suggestionOf` も削除した。
     実装中に気づいた副産物: `triageChildrenFor`/`triageChildrenForDisplay`
     はこの PR に着手する前から本番コードのどこからも呼ばれていない
     死にコードだった（`cardChildrenFromTriage` は同じロジックを別経路で
     再実装しており、この2つを経由していなかった）——専用テスト
     `web_child_spec_display_test.go` だけがそれを呼んでいたので、
     このテストごと削除した。子の project 表示名解決ロジック自体は
     `WebHandler.resolveCardItemChildProjects`（`timeline.CardItem` を
     対象にした新設の同等品）として残っている。

  6. **awaiting の子への質問導線は維持したが、実装場所は変わった。**
     読みモデル (`timeline.CardChildDetail`) には live task の状態
     (awaiting かどうか) が無いので、`WebHandler.enrichPinnedChildLiveStatus`
     が「固定項目として出ている唯一の dispatched な子」1件だけを対象に
     `GetTaskDetail` で live 状態を追加取得する。§3.2 の単一作業枠の
     invariant により高々1回の追加問い合わせで済む。
     **フレッシュレビューで発見・修正した機能後退: 同じ関数が live
     status のチップ表示も担っていたのに、質問導線だけ持ち帰って
     status は捨てていた。** 旧 `ChildRow.DisplayStatus`（PR-2、
     webui-detail-list-redesign.md §3.3 item 2）は台帳 status が
     dispatched の間だけ生 status (executing/awaiting/done/aborted) を
     チップに差し込んでいたが、新実装は台帳の生 `dispatched` を出し
     続けていた。`timeline.CardChildDetail` に呼び出し側専用の
     `LiveStatus` フィールドを足し（読みモデル自体は書かない、project
     名解決と同じ「呼び出し側が別コピーで足す」パターン）、この関数が
     既に取得済みの `detail.Task.Status` をそこに書き込むよう変更した
     （`cardChildDisplayStatus` が `LiveStatus` を優先、無ければ台帳
     `Status` にフォールバック）。

  7. **既存を壊していないことの確認方法。** execution 詳細
     (`TaskDetailExecBody`/`TaskDetailExecStatusSection`) 及びその
     status-group timeline (`TaskDetailTimelineSection`) は無改変
     ——`TestWebHandler_TaskDetail_Exec*`（既存）がそのまま green。
     `TaskDetailLiveScript` と `/tasks/{id}/fragment` の `kind=status`/
     `kind=timeline` は両方とも既存のまま呼ばれ続ける（card の
     `kind=status` は新しい pinned セクションを含むよう中身だけ差し替え、
     `kind=timeline` は既存どおり `#task-timeline` を対象にするが、
     card 側にはその id を持つ要素がもう無い——**`#card-timeline` 自体は
     存在する**が、SSE のリフレッシュ対象は `#task-timeline` の方であり
     両者は別物——ので JS 側の `document.getElementById('task-timeline')`
     が見つからず何も置き換えない no-op になる。壊れてはいないが card
     にとって意味の無い呼び出しが残る、という記録）。
     **訂正（フレッシュレビュー指摘）:** 初版はここを「今回導入した
     `#card-timeline` が存在しないため」と書いていたが誤り——
     `#card-timeline` は `CardHistorySection` が実際にレンダリングして
     おり存在する。no-op になる理由は id の不一致（`kind=timeline` が
     探すのは `#task-timeline`）であって、コンテナの不在ではない。
     `go test ./...` は全パッケージ green（実行して確認）。

     **nice-to-have（フレッシュレビュー指摘、対処済み）: `kind=status`
     が毎回 `BuildCardTimeline` を無駄に呼んでいた。**
     `TaskDetailCardStatusSection` は `Pinned`/`AwaitingQuestionID` しか
     読まないのに、`cardTimelineView` 経由で history 用の
     `BuildCardTimeline`（card の全 action ログを読む）まで毎回走らせ、
     結果を丸ごと捨てていた——SSE の action/job イベント1回・閲覧者1人
     につきこの無駄なフルスキャンが発生する。`cardTimelineView` を
     pinned 専用の `cardPinnedView`（`CardPinnedItems` のみ呼ぶ）と
     history 込みの `cardTimelineView`（`cardPinnedView` を内部で
     再利用し、history だけ追加で埋める）に分割し、`kind=status` の
     フラグメントハンドラは軽い方を呼ぶよう変更した。

     **記録のみ（対処しない）: SSE の `outerHTML` 置換は開いた
     `<details>` を保持しない。** `#task-status` は action/job イベント
     のたびに丸ごと `outerHTML` で置き換わり、morph も
     `hx-preserve` も使っていないので、固定項目の子 spec 折り畳み
     （`<details class="detail-children-spec">`）を開いた状態は
     SSE 更新のたびに閉じる。PR-6b がまさに `#task-status` 内に
     テキスト入力を置く計画なので、入力途中の内容が同じ理由で失われうる
     ——§5.3 の「指示の入力途中・展開状態・スクロール位置を失わない」を
     PR-6b が満たすには、この置換方式自体を変える（部分更新 / 状態の
     クライアント側保持 / `hx-preserve` 等）必要がある。

  8. **mutation テスト結果。** 全て「sed でソースを書き換え →
     `git diff` で実際にコードが変わったことを確認 →
     `.templ` を触った場合は必ず `templ generate` を再実行してから
     生成物 (`_templ.go`) にも変異が反映されたことを確認 →
     `go test` を実行 → 赤を確認 → revert」の手順で実施した。
     `.templ` ファイルは `go test` が直接コンパイルする対象ではなく
     生成後の `_templ.go` だけが対象なので、`templ generate` を
     忘れると「変異は当たっているのに何も壊れない」という誤検知に
     なる——実際に一度この手順ミスで日付セパレータの mutation が
     見逃されかけ、`templ generate` 忘れに気づいて修正した。

     **フレッシュレビューで指摘された穴（BL1/BL2/BL5/BL6）: 純粋関数だけ
     テストして呼び出し側（レンダリング結果）を一度も assert していない
     契約が複数あった。** 日付セパレータは `cardHistoryDateSeparators`
     という純粋関数のテストだけが存在し、その戻り値を実際に描画する
     `cardHistoryItems` の `if seps[i] != ""` 分岐そのものにはテストが
     無かった——この分岐を丸ごと無効化しても（`if false && ...`）
     純粋関数のテストは無関係に green のまま。レンダリング結果に対して
     `card-timeline-date-sep` の出現回数・位置を assert するテストを
     `web/templates/card_timeline_test.go` に追加し、同じ mutation で
     赤くなることを確認してから記録した。追加テストは以下の表に含む。
     同様に `resolveCardItemChildProjects`（project 名解決）にも
     テストが無かったため、旧 `resolveChildProjects`（削除済み）が
     持っていた4本相当（id→name解決・解決不能時は生id維持・spec無し
     子は不変・**保存済み spec を mutate しない**）を復元した。

     | 契約 | mutation | 着弾確認 | 結果 |
     |---|---|---|---|
     | 固定項目が履歴に非重複 | `CardHistorySection` が `tl.History` の代わりに `tl.Pinned` も連結して渡す | diff 確認 | 最初は既存アサーションが `Contains` のみで見逃した（緑のまま）— 固定 suggestion の出現回数を厳密に数える assertion を追加してから再度当てて赤を確認 |
     | 10件上限に固定項目を含めない | `BuildCardTimeline` 呼び出しの limit 引数を `0`（既定10件）から `999` に変更 | diff 確認 | 赤 |
     | 日付セパレータのタイブレーク | `cardHistoryDateSeparators` の `if key != last` を `if key == last` に反転 | diff 確認 + `templ generate` 実行確認（初回は generate 忘れで見逃し、再実行して赤を確認） | 赤（5テスト） |
     | `TaskExists` false → 子タスクへのリンクが消える | `cardChildItemBody` の `if c.TaskExists` を `if true` に | diff 確認 | 赤（templ単体テスト・実DB経由のGC生存テスト両方） |
     | `TargetExists` false → コマンドのターゲットリンクが消える | `cardCommandItemBody` の `if cmd.TargetExists` を `if true` に | diff 確認 | 最初は実DB側の対応テストが無く見逃し（templ単体テストのみ赤）— コマンド版のGC生存テストを追加してから再度当てて両方赤を確認 |
     | `answered` ラベル (`Accepted`) | `cardAnsweredLabel` の `"Accepted"` を `"XAccepted"` に | diff 確認 | 最初は `strings.Contains(html,"Accepted")` が `"XAccepted"` を部分一致で拾ってしまい見逃し（緑のまま）— `>Accepted<` のタグ境界アサーションに直してから再度当てて赤を確認。同じ理由で `closed`/`dropped`/`discuss`/`queued`/`Wake condition due` の assertion も同様にタグ境界に固定してから該当 mutation を当てて赤を確認 |
     | `summary` 本文の抽出 | `cardItemSummaryText` を常に空文字を返すよう変更 | diff 確認 | 赤（templ単体テスト・実DB経由の10件上限テスト両方） |
     | コマンドラベルの `Label` 優先・`CommandKey` フォールバック | `cardCommandLabel` から `if cmd.Label != ""` 分岐を削除 | diff 確認 | 赤 |
     | エスケープ（子タイトル） | `cardChildItemBody` の `{ c.Title }` を `@templ.Raw(c.Title)` に | diff 確認 | 赤（実DB経由の HTTP レベルテストで確認） |
     | Load older の cursor | `TaskCardTimelineOlder` がクエリの `cursor` を無視して常に `""` を使う | diff 確認 | 赤（15件中10+5の重複・欠落を検出） |
     | 日付セパレータの**描画** (BL1、純粋関数でなく呼び出し側) | `cardHistoryItems` の `if seps[i] != ""` を `if false && seps[i] != ""` に | diff 確認 + `templ generate` | 赤（templ単体テスト3本・実DB経由の10+5ページングテスト1本の計4本） |
     | `last_date` の受信配線 (BL2、ハンドラ側) | `TaskCardTimelineOlder` の `lastDate := r.URL.Query().Get("last_date")` を `lastDate := ""` に | diff 確認 | 赤（page1+page2 のセパレータ合計が 1→2 になることを検出） |
     | `last_date` の送信配線 (BL2、emit 側) | `cardHistoryLoadOlder` の `if last.HasTime` を `if false && last.HasTime` に（常に `lastDateKey=""` のボタンを出す） | diff 確認 + `templ generate` | 赤（同上） |
     | Load older の DOM 構造 (BL3、実バグの回帰ガード) | 該当なし — 構造そのものが唯一の実装なので「戻す」mutation は書いていない。修正前の状態そのものが不正な DOM だったことをレンダリング結果の目視確認（`</ul>` の後に `<li>` が出ることを実際に確認）で固定した | 目視 diff | （回帰ガードの性質上、赤/緑ではなく構造の一致で確認） |
     | dispatched な子の生 status チップ (BL4) | `cardChildDisplayStatus` の `if c.LiveStatus != ""` を `if false` に | diff 確認 + `templ generate` | 赤（templ単体テスト・実DB経由の awaiting/executing 両テスト） |
     | awaiting 以外では質問リンクを出さないガード (BL5 point 2) | `enrichPinnedChildLiveStatus` の `if detail.Task.Status == orchestrator.TaskStatusAwaiting && detail.Task.Exec != nil` から前半条件を削除 | diff 確認 | 最初のフィクスチャ（空 payload）では見逃し（`GetAwaitingPayload` が空 payload に対して常に空を返すため、ガードの有無に関係なく green）— executing な子に stale な awaiting payload を残すフィクスチャに直してから再度当てて赤を確認 |
     | project 名解決が保存済み spec を mutate しない (BL5 point 1) | `resolveCardItemChildProjects` の copy-then-reassign を `it.Child.Spec.Project = name`（直接代入）に置換 | diff 確認 | 赤 |
     | `strings.Contains` の部分一致 (BL6) | `cardSuggestionHistoryBody` の verb バッジ文言確認を `>park<` のタグ境界に変更する**前**の状態（`Contains(html,"park")`）で `{ s.Verb }` を `"X"` に固定 | diff 確認 | 修正前は緑のまま見逃し（`badge-verb-park` という CSS クラス名自体が `"park"` を部分一致で満たしていた）— `>park<` に直してから同じ mutation を当てて赤を確認 |

     mutation を当てる前に必ず `git diff` （`.templ` は追加で
     `templ generate` 後の生成物差分）で変異が実際にコードへ入った
     ことを確認してから `go test` を実行し、結果を見たら
     `git checkout --` で元に戻してから次の mutation に進んだ。

  9. **PR-6b/PR-6c への申し送り。**
     - 指示入力欄・カードコマンドボタンの場所は空けてある
       （`TaskDetailCardStatusSection` の pinned セクションの前後どちらに
       置くかは PR-6b が決めてよい、この PR では何も描画していない）。
     - `#card-timeline` は現状 SSE 未接続。PR-6c が子→親 fan-out を実装
       する際、この新設コンテナへの反映方法（既存 `refresh(['status',
       'timeline'])` に3つ目の kind を足すか、別の仕組みにするか）を
       決めること。§10 PR-5b が記録した「SSE 未接続の間のページング
       欠落」（pinned だった子が Load older 前に終端すると以後のページに
       出てこない）はこの PR では未解決のまま — PR-6c の SSE 実装が
       前提として埋める設計であることに変わりない。
     - 進捗の畳み込み（子の action を親のタイムラインに反映する経路）は
       PR-5b の時点で「card 側に進捗 action が存在しない」という理由で
       見送られており、この PR でも同様（読みモデルに無いものは描けない）。
     - 既存の日本語文字列（一覧側の「⚠ 質問あり」「経過」等）は
       この PR のスコープ外のまま未着手。この PR で新規に追加した文字列は
       すべて英語（"No history yet." "Load older" "No more history."
       "Wake condition due" "Accepted"/"Rejected" "Summary" "Note" 等）。

- **PR-6b で確定: 共通の指示入力欄とカードコマンドボタン（§5.1 項目 2）。**
  SSE fan-out・進捗の畳み込みは対象外（PR-6c へ）。

  1. **配置: `#task-status` を分割し、指示入力欄をその外に出した（選択肢
     (a) の変種）。** PR-6a は「タイトル/状態/要約」と「固定項目」を
     `TaskDetailCardStatusSection` 内の1つの `#task-status`
     （SSE `kind=status` で丸ごと `outerHTML` 置換）にまとめていた。
     指示入力欄は要約の下・固定項目の上という順序指定があるため、
     `#task-status` に丸ごと入れたまま外に置く手が使えない。そこで
     固定項目を新設の `#task-pinned`（`TaskDetailCardPinnedSection`、
     新設の SSE fragment `kind=pinned` で個別に置換）に分離し、
     指示入力欄 (`CardCommandSection`) をその2つの兄弟として
     `#task-status` → `CardCommandSection` → `#task-pinned` の順に並べた。
     これで指示入力欄はどちらの SSE 置換対象の中にも入らず、
     入力途中を失わない。§5.3 の劣化は**発生しない**（選択肢 (b) は
     不採用）。`TaskDetailLiveScript` の `refresh()` 呼び出し4箇所
     （action/job リスナー、visibilitychange、pageshow）に
     `'pinned'` を追加、`TaskDetailFragment` に `kind=pinned` ケースを
     追加した（exec task には `#task-pinned` が無いので no-op）。
  2. **成功時: 任意 URL への redirect ではなく、card 自身のページへ
     redirect する。** `RunCardCommandAsHuman` の成功応答は
     `RequestID`/`LauncherJobID` しか返さず（`TargetKind`/`TargetID` は
     Occupied 応答専用）、この時点では継続先がまだ存在しない可能性がある
     ため、継続先 URL を組み立てる材料が無い。`LauncherJobID` を継続先だと
     誤認しない（フィールド名がそう警告している）。代わりに
     `PostCardCommand` は素の `redirectTask(w, r, id)`（= 自分自身の
     card ページ）を返す。card ページの固定項目 (`CardPinnedSection`)
     は既に `card_requests` 行の状態（queued/launching/attached）と
     `TargetKind`/`TargetID`/`TargetExists` を読んで描画するので、
     継続先が生まれ次第、SSE の `kind=pinned` 更新（またはページ再読込）
     で自動的にリンクが現れる — 「daemon が検証済みの関連を取得して
     該当ページを開く」を、新しい導線を作らずに PR-6a の読みモデルへ
     委ねる形で満たした。
  3. **Occupied 時: redirect せず、その場で同じページを直接レンダリング
     する。** 占有時に入力内容を失わないための手段として、redirect +
     URL クエリでの往復は採らなかった（instruction の上限が
     `cardCommandInstructionMaxBytes`＝10MiB で、URL に乗せるのは
     非現実的）。代わりに `WebHandler.PostCardCommand` は
     `RunCardCommandAsHuman` の Occupied 応答を受け取ったら
     `renderTaskDetailPage` を直接呼び、送信された instruction を
     `templates.CardCommandFormState.Instruction` としてそのまま
     textarea へ埋め戻す。`TargetKind`/`TargetID` が両方揃っている
     ときだけ「現在の実行へのリンク」を表示し、揃っていなければ
     （占有者がまだ `launching` で継続先が無い場合など）リンクを
     出さない — PR-2d-5/PR-4c が固定した「行き止まりリンクを出さない」
     契約を UI 側でも踏襲した。占有の告知は `.action-error` と別クラス
     （`.card-command-occupied`）で中立トーンの英文にし、エラーに
     見えないようにした。
  4. **非表示条件: 2つ。** `card_commands` が1件も宣言されていない
     project（`CardCommandOptionsForProject` が nil を返す）と、
     card が `done`/`dropped`（`cardCommandTerminalStatus`）のとき、
     `CardCommandSection` は何も描画しない。後者は
     `RunCardCommandAsHuman` 自体が終端 card に 409 を返す
     （PR-2d-5 で確定済み）ことと対にして、押せるのに 4xx が返る
     ボタンを表示しないようにした。
  5. **ラベルは `CardCommand.Label` をそのまま表示、`Discuss`/`Run` を
     固定名として扱わない。** `CardCommandOptionsForProject`
     （`internal/api/card_command_launcher.go`）が project.yaml の
     `card_commands` を `CardCommandsOrder` の宣言順で `[]CardCommandOption`
     に変換し、`CardCommandSection`（`card_timeline.templ`）が
     そのまま描画する。
  6. **mutation テスト結果。** `.templ` は毎回 `templ generate`
     を再実行し、生成物 (`_templ.go`) の差分を確認してから
     `go test` を実行、赤を確認したら手元のバックアップコピーで
     元に戻す手順で実施した（`git checkout --` は使わず、mutation
     適用前に取ったファイルコピーへの `cp` で復元 — 理由は次項）。

     | 契約 | mutation | 着弾確認 | 結果 |
     |---|---|---|---|
     | 未宣言 project で非表示 | `CardCommandSection` の `len(options) > 0` を `true` に | diff 確認 + `templ generate` | 赤 |
     | 終端 card で非表示 | `cardCommandTerminalStatus` を常に `false` に | diff 確認 + `templ generate` | 赤（done/dropped 両方）|
     | 宣言順 | `CardCommandOptionsForProject` の順序ループの前に `sort.Strings(meta.CardCommandsOrder)` を挿入 | diff 確認 | 赤（純粋関数のテストとレンダリング結果のテスト両方）|
     | ラベル | ボタンの `{ opt.Label }` を `{ opt.Key }` に | diff 確認 + `templ generate` | 赤（宣言順テスト・エスケープテスト両方）|
     | Occupied で入力保持 | `cardCommandFormInstruction` を常に `""` を返すよう変更 | diff 確認 + `templ generate` | 赤（2テスト）|
     | Occupied でリンク有無 | リンクの条件 `TargetKind==task && TargetID!=""` を `true` に固定 | diff 確認 + `templ generate` | 赤（「target 無しでリンクを出さない」テストが検出）|
     | 成功時の導線 | `redirectTask(w, r, id)` を `LauncherJobID` を使った `/jobs/...` への redirect に変更 | diff 確認 | 赤 |
     | エスケープ（instruction） | textarea の `{ cardCommandFormInstruction(form) }` を `@templ.Raw(...)` に | diff 確認 + `templ generate` | 赤 |
     | エスケープ（label） | ボタンの `{ opt.Label }` を `@templ.Raw(opt.Label)` に | diff 確認 + `templ generate` | 赤 |
     | 空 instruction で起動可能 | `PostCardCommand` に `instruction == ""` の拒否ガードを追加 | diff 確認 | 赤 |
     | 英語文言 | occupied 告知文を日本語に差し替え | diff 確認 + `templ generate` | 赤 |
     | SSE の `kind=pinned` 配線 | `refresh()` 呼び出し1箇所から `'pinned'` を削除 | diff 確認 + `templ generate` | 赤 |

     着弾確認は毎回 `git diff`（`.templ` は追加で `templ generate` 後の
     `_templ.go` 差分）で行い、ビルドはすべて通った状態で計測した
     （コンパイルを壊す mutation は無し）。1点、実装時に自分自身の
     mutation テスト手順のミスで機能を巻き戻しかけた事例がある:
     mutation 確認後の復元に `git checkout --` を使ったところ、
     まだコミットしていない実装そのもの（HEAD には存在しない新規
     コード）が消えてしまった。以降は mutation を当てる前に
     `cp` で作業コピーを保存し、復元も `cp` で行う方式に切り替えた
     — 未コミットの新規ファイルに対して `git checkout --` を
     「元に戻す」目的で使わないこと。
  7. **PR-6c への申し送り。**
     - `#card-timeline`（履歴）は本 PR でも引き続き SSE 未接続のまま
       （PR-6a からの持ち越し）。`#task-pinned` は本 PR で SSE
       接続したが、履歴側は対象外。
     - 子→親 fan-out の実装先は `kind=pinned`/`kind=timeline`
       のどちらに寄せるか、あるいは新しい kind を足すかは
       PR-6c が決めること。
     - `RunCardCommandAsHuman` は人発コマンド専用で、内部イベント発の
       queued 行（PR-4c）は今回の UI から見えない。一覧の活動状態
       （PR-5c）とは別に、card 詳細で「queued な内部イベント要求」を
       明示する UI は本 PR に含めていない — 固定項目の
       `CardCommandDetail`（`launching`/`attached` 優先で選ばれる、
       PR-5b の `pickActiveCardRequest`）が queued 行を拾わないケースは
       PR-5b の既知の非対称のまま。

- **PR-6c-1 で確定: 子→親 SSE fan-out（PR-6c を 6c-1/6c-2 に分割した1本目）。**
  UI・進捗の畳み込みは対象外（PR-6c-2 へ）。フレッシュな Opus レビューで
  NO-GO を受け、BLOCKER 2件・nice-to-have 6件を修正した2ラウンド目の結果。

  1. **埋めた経路 / 埋めなかった経路。** 最終的に5つの欠落を埋めた:
     (1) `recordChildClosedOnParent`/`recordVanishedChildClosedOnParent`
     （`workflow_card.go`/`queue_sweep.go`）が書く `child_closed` action を
     broadcast、(2) `ApplyAction` の子 executing→awaiting（ask）を親へ
     fan-out、(3) `TaskAppService` に `Hub` を新設し
     `progress`/`done_request`/`fail_request`（子自身にも親にも一切
     届いていなかった）を配線、(4) `CompleteJob` の子 job 完了/失敗を
     親へ fan-out、(5) **レビューで発覚: `task_ask.go`（`consumePendingAnswer`/
     `answerBlocking` の fast path）が書く `answer` action も同じく
     どこにも broadcast されていなかった** — 子 awaiting→executing の
     「戻り」方向。`recordAnswerAction` を `broadcastNotifyAction` 経由に
     変更して埋めた。
     既存 7 箇所のうち `persistFiredEvents`（fired_event は card の
     読みモデルに反映先が無い）、`suggestion_accept.go`・acceptGo の
     "go" self-broadcast（対象タスクが常に呼び出し元自身で、子→親の関係
     ではない）は変更していない — 対象外にした理由は「fan-out先に反映
     するデータが読みモデルに存在しない」か「そもそも子→親の関係を持つ
     broadcast ではない」のいずれか。
     **見つけ方の教訓 (レビュー指摘):** 欠落 (1)〜(4) は既存の
     `grep Broadcast` 棚卸し（本 doc 冒頭の7行の表）から出発できたが、
     欠落 (5) は原理的にこの方法では見つからない — `task_ask.go` は
     `Broadcast` を一度も呼んでいなかったので、呼び出し箇所を数える
     棚卸しにはそもそも現れない。「呼んでいる場所」だけでなく
     「状態遷移を書いているのに呼んでいない場所」（`s.Tasks.UpdateTask`
     + 生の `CreateAction` の組み合わせ）を別途洗う必要があった。
  2. **`Kind` の語彙: 新設した `child` と、既存 `action` の再利用の二本立て。**
     `recordChildClosedOnParent`/`recordVanishedChildClosedOnParent` が書く
     action は `TaskID` が最初から親自身なので、既存の `action` self-broadcast
     をそのまま親のチャンネルに投げるだけで足りる（新語彙不要）。
     一方 `ApplyAction`/`CompleteJob`/`NotifyTask` の fan-out は、親の
     action ログに存在しないイベント（子の action_id/job_id は親の
     テーブルには無い）を親のチャンネルへ転送するので、新設 `child` を
     使う。ブラウザ側は `child` 受信時に `refresh(['pinned'])` のみ呼ぶ
     — `#task-status` は子の情報を一切描画しないため。card の fragment
     kind は既存どおり `status`/`pinned` の2つのまま変更していない。
  3. **fan-out の判定を1箇所に寄せた場所: `internal/api/card_child_fanout.go`
     の `isCardTask`/`fanOutChildEventToParentCard`。** 子の `Task` と
     `TaskStore` を受け取り、親を引いて型を見てから broadcast するのはこの
     関数だけで、呼び出し元（`ApplyAction`/`CompleteJob`/
     `broadcastNotifyAction`）は判定条件を一切持たない。
     `recordChildClosedOnParent`/`recordVanishedChildClosedOnParent` は
     action の `TaskID` が最初から親なので `fanOutChildEventToParentCard`
     は使わないが、同じ `isCardTask` 述語を再利用して判定を分岐させている。
     **mutation で判明した記録のみの事実: この2箇所での `isCardTask` は
     現状の DB スキーマ下では到達不能（常に true）。**
     `orchestrator.UpsertTaskTriage` の UPDATE 文が `WHERE id = ? AND
     type = 'card'` を持つため、card 以外の task に task_triage 行を
     持たせること自体ができず、`recordChildClosedOnParent` が
     `GetTaskTriage` を通過できた時点で親は必ず card。§6 の別の
     到達不能ガード（`taskType != TaskTypeCard`）と同じ構造で、
     schema が変わらない限り dead code だが、防御として残した。
  4. **`progress` は子自身と親の両方に届ける。** 配線前は
     `TaskAppService` に `Hub` が無く、`progress`/`done_request`/
     `fail_request` は子自身の購読者にすら届いていなかった
     （出発点の欠落3）。`broadcastNotifyAction` を新設し、まず
     action 自身の `TaskID`（=子）へ既存 `action` Kind で self-broadcast
     してから、`fanOutChildEventToParentCard` で親（card のときのみ）へ
     `child` Kind を送る。子自身への配信も今回まとめて直した理由:
     親にだけ届いて子の詳細ページ自身が更新されないのは非対称で、
     `ApplyAction`/`CompleteJob` の他の action と同じ扱いに揃えるほうが
     一貫する。
  5. **`child` Kind の payload は `childEventPayload`（`card_child_fanout.go`）
     に1箇所へ寄せた。** レビュー指摘: 当初は4呼び出し元がそれぞれ
     `map[string]any{...}` を手書きしており、`ApplyAction` だけ
     `reason` の代わりに `new_status` を積む・`action_id`/`job_id` の
     どちらを積むかが呼び出し元ごとにバラバラ、という「§6 が
     `promotedAttrVocabulary` について警告しているのと同型の手書き手
     同期ハザード」になっていた。`childEventPayload(childTaskID, reason,
     idKey, idValue)` に統一し、`reason` は action 系なら `action.Type`
     （`ask`/`answer`/`progress`/`done_request`/`fail_request`）、job 系
     なら `"job_completed"`/`"job_failed"` の固定文字列。`new_status` は
     落とした — ブラウザ側は payload の中身を見ず、Kind 受信をきっかけに
     `/tasks/{id}/fragment?kind=pinned` を再取得するだけなので、状態の
     再現に payload の内容は使われていない。
  6. **PR-6c-2 への申し送り。** 子の `child_task_id` を含む `child` Kind の
     イベントが親に届くようになったので、PR-6c-2 はこれを使って
     (a) `#task-pinned` の SSE 置換前に入力途中の指示・展開状態を保持する
     部分更新、(b) N4（pinned だった子が終端すると履歴に一度も現れない
     問題）の head 再描画、を実装できる。**ただし、card コマンドの継続先
     (task/session) は fan-out の対象に含まれない**（下記 nice-to-have 1）
     — PR-6c-2 が「`child` が届けば継続先の出現も分かる」と誤解しないこと。
  7. **child_closed 経路の broadcast は `tryDispatchQueuedCardRequest` の
     後に出す（順序が契約）。** 一度「購読者に早く届くように」前に出したが、
     レビューで穴が判明して戻した — 前に出すと、その close が起動した
     `card_requests` 行がまだ launching に至っていない状態で購読者が
     `?kind=pinned` を取りに行き、**dispatch が落ち着いた後に再 broadcast する
     経路が無い**ため、内部イベント発のコマンドが次のイベントかリロードまで
     固定項目に現れない。レイテンシの差は無視できる（どちらもコミット後）ので、
     dispatch を先に済ませて「取りに行けば必ず見える」状態にしてから通知する。
  8. **回答経路の broadcast は audit 行の書き込み失敗でも出す。**
     `recordAnswerAction` の耐久的な事実は既にコミット済みの
     awaiting→executing の反転であって、`actions` 行ではない。書き込み失敗で
     黙ると B1 と同じ症状（card が子を awaiting のまま表示し、回答済みの質問へ
     リンクし続ける）が監査失敗時にだけ再発する。
     `TestAnswerTask_ActionWriteFails_StillBroadcasts` が固定している。

  **コミット境界の pin（レビュー BLOCKER 2 対応）: `recordChildClosedOnParent`/
  `recordVanishedChildClosedOnParent`/`ApplyAction`/`CompleteJob` 失敗時分岐
  の fan-out は、いずれも「`WithinTx` が返った**後**、`err == nil` の
  分岐でだけ broadcast する」実装だったが、それを固定するテストが
  無かった。既存の `TestApplyAction_NoBroadcastOnCommitFailure`/
  `TestCompleteJob_NoBroadcastOnCommitFailure`（`hub_broadcast_test.go`）は
  親を持たない task を使っていたため、fan-out を tx クロージャの中に
  移す mutation が生き残っていた（下記 mutation 表で確認）。
  `postCommitFailTransactor`（クロージャを実行してから失敗を返す
  `Transactor` ラッパー — `alwaysFailTx` はクロージャを一切実行しない
  ので別物）を使い、4経路すべてに「WithinTx が失敗したら子・親どちらの
  チャネルにも届かない」を固定するテストを追加した。**

  **`answer` action の commit 境界は別カテゴリ。** `task_ask.go` の
  `consumePendingAnswer`/`answerBlocking` は元々 `WithinTx` を経由しない
  （`s.Tasks.UpdateTask` の直接呼び出し）ので、上記と同じ形の
  commit-failure pin は対象外（そもそも「commit が失敗して broadcast だけ
  先に出る」という失敗モードが存在しない）。

  **見つけた plan doc とのズレ:** §6 の実現可能性チェック表が
  `internal/api/workflow_card.go`（子 dispatch）の broadcast 先を
  「`newTask.ID`（新規子）」と記していたが、実装を読むと `acceptGo` の
  この broadcast は go 遷移そのもの（card 自身の parked→working）の
  self-broadcast で、`newTask` は新規子ではなく card 自身を指す
  （`newlyDispatched` 側の `child_dispatched` action には broadcast が
  無い）。既存 7 箇所の広報先を洗い出す出発点として使ったが、この1件は
  子→親の欠落ではなく元から self-broadcast だった。

  **記録のみ（レビュー nice-to-have 1/2/6）:**
  - **card コマンドの継続先は構造的に fan-out できない。**
    `internal/server/boid_executor.go` の `executeAgentStart`/`boid task
    create` 経路は `--parent <this card>` を明示的に拒否するため、
    card コマンドが作る継続先 (task/session) は常に ROOT task
    （`ParentID == ""`）で作られる。`fanOutChildEventToParentCard` は
    `child.ParentID == ""` なら即 no-op なので、この継続先の生成・
    状態変化は今回の fan-out では一切親に届かない。加えて
    `card_requests` のライフサイクル遷移（queued→launching→attached→
    finished/failed）自体にも broadcast 呼び出しが無い。**PR-6b §10
    point 2 の「継続先が生まれ次第、SSE の `kind=pinned` 更新で
    自動的にリンクが現れる」は、この PR の後もまだ機構としては
    成立していない**（ページ再読込・`visibilitychange`/`pageshow` の
    定期 refresh に依存したまま）。PR-6c-2 が `child` Kind を前提に
    設計する際、この経路が対象外であることを踏まえること。
  - **既存7箇所の棚卸しが8箇所目を見落としていた。** `hubJobEventSink.
    JobCreated`（`internal/server/api_store.go`）が `job` を
    `job.TaskID` へ broadcast しており、fan-out 対象になっていない。
    今のところ無害 — 子が dispatch される瞬間は `acceptGo` 自身の
    card self-broadcast（parked→working の `action` Kind）が同じ
    タイミングで親のチャネルに届くため、ジョブ起動の可視化はそちらで
    間に合っている。ただし「7箇所」という前提の棚卸しでは原理的に
    見つからない箇所だった、という点は次の fan-out 追加時に踏まえること。
  - **hot path に同期 `GetTask` が増えた。** `CompleteJob` の成功分岐
    （子の job 完了）と `NotifyTask` の `progress`/`done`/`fail` 分岐は、
    fan-out 判定のために親を1回 `GetTask` する（購読者がゼロでも発生）。
    sqlite・単一 writer 構成では無視できる規模と判断し、対処はしていない。

  **mutation テスト結果。** 全て「sed/python でソースを書き換え →
  `diff` でバックアップと比較して着弾を確認（`.templ` は追加で
  `templ generate` 後の `_templ.go` 差分も確認）→ `go test` を実行 →
  赤を確認 → `cp` で復元」の手順で実施した。

  | 契約 | mutation | 着弾確認 | 挙動が変わったか |
  |---|---|---|---|
  | card 親のみ fan-out（共通述語） | `fanOutChildEventToParentCard` の `!isCardTask(parent)` 判定を削除 | diff | 赤（execution 親 2 本） |
  | ApplyAction の子→親 fan-out | 追加した `fanOutChildEventToParentCard` 呼び出しを削除 | diff | 赤 |
  | CompleteJob 成功時の子→親 fan-out | 追加した呼び出しを削除 | diff | 赤 |
  | CompleteJob 失敗時の子→親 fan-out | 追加した呼び出しを削除 | diff | 赤 |
  | NotifyTask progress の自己 broadcast | `broadcastNotifyAction` 呼び出し（progress 分岐）を削除 | diff | 赤 |
  | NotifyTask fail_request の自己 broadcast | `broadcastNotifyAction` 呼び出し（done/fail 分岐）を削除 | diff | 赤 |
  | NotifyTask の親への fan-out（自己 broadcast とは独立） | `broadcastNotifyAction` 内の `fanOutChildEventToParentCard` 呼び出しのみ削除 | diff | 赤（2本とも、自己 broadcast 側は緑のまま で分離確認済み） |
  | child_closed の親への broadcast | `recordChildClosedOnParent` の broadcast ブロックを `if false` に | diff | 赤 |
  | child_closed の card 判定（`isCardTask(parentTask)`） | 条件を `true` に固定 | diff | **変化なし** — 上記3項の理由で到達不能。既存の「execution 親では broadcast されない」テストは `GetTaskTriage` の `sql.ErrNoRows` 早期リターンで既に緑になっており、この mutation では何も証明していない |
  | vanished child_closed の親への broadcast | `recordVanishedChildClosedOnParent` の broadcast ブロックを `if false` に | diff | 赤 |
  | vanished child_closed の card 判定 | 未実施（上記と同一構造、同じ理由で到達不能と判断） |  |  |
  | ブラウザ `child` リスナーの存在 | `tasks.templ` からリスナー行を削除 | diff + `templ generate` | 赤（新設テストと既存 `TestCardDetail_LiveScript_RefreshesPinnedKind` の両方が5→4件で検出） |
  | `child` リスナーの refresh 対象 | `refresh(['pinned'])` を `refresh(['status'])` に | diff + `templ generate` | 赤 |
  | `answer`（fast path・re-ask 両方）の自己+親 broadcast | `recordAnswerAction` 内の `s.broadcastNotifyAction(task, action)` 呼び出しを削除 | diff | 赤（子・親の両テストとも） |
  | `answer` の親への fan-out のみ（自己 broadcast とは独立） | `broadcastNotifyAction` 内の `fanOutChildEventToParentCard` 呼び出しのみ削除 | diff | 赤（2本とも、子自身への self-broadcast は緑のままで分離確認済み） |
  | `recordChildClosedOnParent` の broadcast を tx 内に移動 | `if s.Hub != nil && isCardTask(parentTask) {...}` ブロックを `recorded = true` 直後（`WithinTx` クロージャの中）に移動 | diff | 赤（`postCommitFailTransactor` でクロージャは実行されるが `WithinTx` 自体は失敗を返す状況で broadcast が届いてしまうことを検出） |
  | `recordVanishedChildClosedOnParent` の broadcast を tx 内に移動 | 同上 | diff | 赤 |
  | `ApplyAction` の fan-out を tx 内に移動 | `fanOutChildEventToParentCard` 呼び出しをクロージャの `switch` 末尾（`return nil` の直前）に移動 | diff | 赤 |
  | `CompleteJob` 失敗時の fan-out を tx 内に移動 | 同様にクロージャ内へ移動 | diff | 赤 |

  **記録のみ: card 判定の mutation が「着弾したが挙動を変えなかった」
  唯一のケース。** 「テストが甘い」のではなく、DB スキーマの制約
  （3番参照）により child_closed 経路では親が card 以外になり得ないため。
  テストを「task_triage を無理やり非 card task に付ける」形に作り込む
  ことも検討したが、`UpsertTaskTriage` 自体がそれを拒否する（対象行が
  0件で `ErrTaskNotFound` になる）ため実 DB では再現不可能と判断し、
  この事実を記録するに留めた。
