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
| PR-4 | 内部イベントの耐久要求・起動通知・復旧、外部 Sweep handoff | 自己ループ無し、枠占有中の保留、作業終了後の再判断。旧経路とは未併用 |
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
  queued 行を作るまで dead code。** PR-4 でワイヤされた後に残るオープン課題:
  fold は card 単位で command_key/cause_id を区別しないため、無関係な
  request を巻き込んで force-fail してしまう (`operator_notice` は sibling
  数を報告しない) ことと、`idx_card_requests_cause_unique` に status 述語が
  無いため `cause_id` 付きの sibling が `failed` に落ちると再配送が永久に
  ブロックされること (`FailCardRequest` の再試行なら起きない) — どちらも
  PR-4 側で対処すること。
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

これらは §4 の契約・§6 の対処を前提に、Gate A と各実装 PR で確定する。
単一ユーザーの利用を前提に、対話注入・分散ロック・汎用 DAG scheduler は追加しない。
本 doc は実装の実測結果に追随させ、コード読解で確認したことと実行して確認したことを混同しない。
