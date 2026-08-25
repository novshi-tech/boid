# card モデル整理 — Task の明示的直和化 (STI) と命名統一

2026-08-25 設計。suggestion-as-state-transition (card 機械 v2、boid #986〜#990) の
カットオーバー完了を受けて、試行錯誤の過程で堆積した「地層」を一掃し、
ドメインモデルを設計 doc が既に到達している概念——**card**——に実装ごと揃える。

## 1. ゴール

Task を **明示的な直和型 Task = Card | ExecutionTask** として、型 (discriminator)・
スキーマ・命名・契約の全層で一貫させる。終わったと言える条件:

1. task が card か実行タスクかは **discriminator 1 個** (`tasks.type`) で決まり、
   sidecar row の有無や status からの推測が全コードから消えている。
2. card の行に実行系の値 (behavior 等) が **一切入らない**。逆も同じ。
   DB の CHECK 制約が型ごとのフィールド規律と status 語彙を宣言的に張っている。
3. 「card」という名前が DB・Go・HTTP・broker op・CLI・Web 文言まで通っている。
   「triage」は過程 (card が受ける行為) を指す語としてだけ残る。
4. captured/triaged/ready の legacy status がデータからもコードからも消えている。

これは傷の手当てのリストではない (問題リストを設計の錨にしない)。モデルを
直したとき、既知の傷が**結果として**消えることは §7 で検算する。

## 2. As-Is — 地層の由来

現状は 2 つの時代の重なりで、どちらの設計にも一致していない:

- **Phase 1 時代 (cross-project-issue-triage)**: 「triage task」という呼び名で、
  通常の task に `task_triage` sidecar (0035) を**片側だけ**足した。sidecar に
  した理由は migration コメントに明記されている——「Task は DTO を兼ねており
  列を足すと全 API/CLI/Web の JSON に自動露出するため」。つまり serialization の
  都合であって、ドメインモデルの判断ではない。
- **再設計時代 (suggestion-as-state-transition)**: 概念が「card = 判断の台帳」に
  生まれ変わり、機械も `NewCardMachine` / `NewExecutionMachine` に分割された。
  しかし新しい名前をもらえたのは新規コードだけで、外皮 (table 名・HTTP path・
  broker op・多くのファイル名) は triage のまま。

その結果の現在形:

- 実行系フィールド (behavior/readonly/branch_prefix/base_branch/instructions/
  payload/auto_start/traits) は **tasks テーブル直置き**で、card の行にも
  `ResolveBehavior` が解決した default 値一式が「嘘の値」として入る
  (task_resolve_or_capture.go — card の BaseBranch は未展開テンプレが入る
  文書化済みの地雷)。
- card 系フィールドは **task_triage sidecar** に入る。判別は「sidecar row が
  あるか」+ rowless card 救済のための status ベース fallback
  (api/machine_select.go の machineFor / isCardLifecycleStatus)。done/aborted は
  status では判別不能、と自分のコメントで白状している。
- 状態語彙は 1 つの TaskStatus enum に card 用・実行用・legacy 3 状態
  (captured/triaged/ready、pre-cutover row 読み出し専用) が同居。
- 命名は同一ファイル内でも triage と card が混在 (web/templates/tasks.templ)。

card ↔ 実行タスクの**型の移動は存在しない** (card 機械に executing へ到達する
辺は無く、reopen も card は parked へ、実行タスクは再実行へ、と型別に振り分け
ている)。個体が一生型を変えず、振る舞い (状態機械) が型で分かれる——
subtype モデリングの適用条件を両方満たす。だから「継承」が正解で、
現状はそのエンコードが欠けているだけ。

## 3. To-Be — ドメインモデル

### 3.1 型と discriminator

```
Task (共通コア)
 ├─ Card          … 判断の台帳。エージェントセッションは起きない
 └─ ExecutionTask … セッションを起こす実行タスク
```

- `tasks.type` 列 (TEXT NOT NULL、値は `card` | `execution`) を discriminator に
  する。**Single Table Inheritance**: テーブルは tasks 1 枚、subtype 固有列は
  相手型の行では NULL。
- どちらでもない・両方、は構造的に存在しない (discriminator の定義)。
  旧 machineFor の「ちょうど 1 個の sidecar」相当の invariant は消滅する。

STI を選んだ理由 (CTI = base + subtype 2 テーブル案との比較):

- 最ホットな read が**混在リスト** (task tree・open 一覧・queue_next) で、
  boid は ORM 無しの手書き SQL (orchestrator/store.go)。CTI は全 list 系に
  LEFT JOIN 2 本の税を人間が払い続ける。STI は単一テーブル読みのまま。
  queue_next が読む suggestion_verb も join 無しの素の WHERE になる。
- CTI の旨み (NULL の無い schema) の大半は、SQLite の CHECK 制約
  (§3.4) で join 無しに回収できる。
- 一気に切り替える方針 (§6) では migration の可動部品が少ない方が安全。
- CTI に振れる条件——subtype 固有列が増え続ける見込み——は現状無い
  (card 側 6 列・実行側 8 列)。将来 card 側の queue 述語列が繰り返し
  生えるようなら、その時に CTI 化を再検討する。

### 3.2 フィールド帰属表

| 帰属 | フィールド | 備考 |
|---|---|---|
| 共通コア | id, project_id, title, description, status, parent_id, ref, created_at, updated_at | ref は作成冪等キー (子タスク重複防止)。card は identity (task_identities) で冪等化するが、ref が core にあって害はない |
| Card | kind, urgency, wake_at, wake_task_id, suggestion_verb, detail | 現 task_triage の全列を畳み込み |
| ExecutionTask | behavior, traits, readonly, branch_prefix, base_branch, payload, instructions, auto_start | 現 tasks 直置きの実行系一式 |
| 監査して決める | remote_id | 用途は base_branch テンプレ展開・import 重複排除・親→子継承と実行寄り。card は task_identities を使い remote_id を持たない実態なら Execution へ。整形セッションが card に remote_id を書く運用が実在するなら core に残す |
| 監査して drop 候補 | datasource_id | 0001 の DB 列だが Go の Task struct に対応フィールドが無い。到達可能性を確認して死列なら table rebuild 時に落とす |

### 3.3 status 語彙の分割

- Card: `parked` / `working` / `done` / `dropped` (card 機械 v2 の 4 状態、変更なし)
- ExecutionTask: `pending` / `executing` / `awaiting` / `done` / `aborted`
- `done` の文字列共有は許容する。機械の選択は type で決まるので、status の
  重なりは判別に影響しない (旧 machineFor の done/aborted 曖昧性は type 列の
  導入で消える)。
- **captured / triaged / ready は廃止**。migration で既存行を parked に洗い替え
  (§6.2)、KnownTaskStatuses・IsPreExecutionStatus・IsPreDispatchEditableStatus・
  isCardLifecycleStatus の legacy 分岐をコードから撤去する。
  actions テーブルの履歴行 (from_status/to_status に旧値が残るもの) は
  **書き換えない**——action ログは台帳であり、歴史の改竄はしない。読み出し・
  表示は文字列として素通しなので影響しない。

### 3.4 DB invariant (CHECK 制約)

table rebuild (§6.2) で以下を宣言的に張る。意図を示す擬似 SQL:

```sql
type IN ('card', 'execution')

-- 型ごとの status 語彙
(type != 'card'      OR status IN ('parked','working','done','dropped'))
(type != 'execution' OR status IN ('pending','executing','awaiting','done','aborted'))

-- 相手型のフィールドは NULL (代表例。実列全部に張る)
(type != 'card'      OR (behavior IS NULL AND instructions IS NULL AND ...))
(type != 'execution' OR (kind IS NULL AND urgency IS NULL AND ...))
```

注意: 実行タスクの behavior は現行仕様で空文字が合法 (no-yaml project)。
「card では NULL / execution では NULL でない (空文字は可)」の区別を保つため、
実行系列は NULL 許容に変えた上で `type='execution' → IS NOT NULL` を張る。

### 3.5 Go のエンコード — tagged struct

Go にネイティブ継承は無い。interface + 具象 2 型 (compile-time で最も固い) は
`*orchestrator.Task` を受け渡す全域 (store/api/web/cmd/client) の signature 改修と
polymorphic JSON unmarshal・DB scan の手製化を要求し、portable client
(Mac/Win、型 import のみ許可) にも波及するため、**採らない**。

採るのは Go 流の直和エンコード = tagged struct:

```go
type TaskType string
const (
    TaskTypeCard      TaskType = "card"
    TaskTypeExecution TaskType = "execution"
)

type Task struct {
    ID        string   `json:"id"`
    Type      TaskType `json:"type"`
    ProjectID string   `json:"project_id"`
    Title     string   `json:"title"`
    // … 共通コアのみ …
    Card *CardAttrs `json:"card,omitempty"` // Type==card のとき非 nil
    Exec *ExecAttrs `json:"exec,omitempty"` // Type==execution のとき非 nil
}
```

- CardAttrs = 現 orchestrator.TaskTriage の改名 + 統合。ExecAttrs = 実行系一式。
- 「Type と非 nil 側が一致する」は store の scan/insert と CreateTask 検証の
  1 箇所ずつで守る (DB 側は CHECK が守る)。
- 呼び出し側は `task.Type` で switch。実行系フィールドへの素の参照
  (`task.Behavior`) は全てコンパイルエラーになるので、移行時に参照箇所の
  全数レビューが**型検査で強制される**——これがこの改修の隠れた検証機構。

### 3.6 機械選択の全域化

`machineFor` / `machineForDisplay` / `resolveReopenVariant` の判別は
`task.Type` の switch に置き換わる:

- TaskTriageStore lookup・sql.ErrNoRows の解釈・isCardLifecycleStatus fallback・
  fail-open/fail-closed の分岐は**全て削除**。machineFor は失敗しない純関数になる。
- SeedTaskTriage (best-effort seeding と rowless card 問題) は概念ごと消滅——
  card は生まれた瞬間から type='card' で、後から「card にする」行為が無い。

### 3.7 capture 経路の浄化

ResolveOrCapture は `ResolveBehavior` を呼ばなくなり、共通コア + CardAttrs
だけで card を作る。これに伴い:

- card の base_branch 未展開テンプレ地雷 (task_resolve_or_capture.go の長大な
  免責コメント) は根絶。
- behavior 解決の meta hydrate も capture 経路から消える (card に behavior は
  存在しないので)。

## 4. 命名統一 — rename 対応表

方針: **物 = card、過程 = triage**。「card を triage する」は正しい文なので、
過程・行為を指す箇所 (queue の文言等) の triage は残ってよい。物を指す triage は
全部 card に置換する。

| 現在 | 変更後 |
|---|---|
| `task_triage` テーブル | 廃止 (tasks に畳み込み) |
| `orchestrator.TaskTriage` | `orchestrator.CardAttrs` |
| `api.TaskTriageStore` / `TaskTriageView` | `CardStore` / `CardView` |
| `workflow_triage.go` / `triage_read.go` / `triage_handler.go` / `task_triage.go` | `workflow_card.go` / `card_read.go` / `card_handler.go` / `card.go` |
| `api.TriageHandler`・`/api/triage` mount | `CardHandler`・`/api/cards` |
| broker op `task_triage_get` / `task_triage_list` | `card_get` / `card_list` |
| shim `boid task triage [--list]` | `boid card [get|list]` |
| `GetTriage` / `ListTriage` (workflow service) | `GetCard` / `ListCards` |
| Web UI 文言の「triage task」相当 | 「card」に統一 (過程を指す triage は残す) |

据え置き (物ではなく過程・別概念を指すもの): `queue_sweep.go`、
`docs/plans/cross-project-issue-triage.md` (歴史記録)、khi 側の「triage」語彙の
うち行為を指すもの。

## 5. 境界の契約変更

**互換 alias は置かない** (決定: 一気にやる)。壊れる面と対応先:

1. **broker op / shim**: `task_triage_get|list` → `card_get|list`、
   `boid task triage` → `boid card`。消費者は khi (gateway 経由) と
   サンドボックス内スキル。khi 側 repo の同時 PR が必要 (§6.3)。
2. **HTTP `/api/triage`** → `/api/cards`。消費者は Web UI (同 repo なので同時) 。
3. **Task JSON の形**: flat → `{type, …core…, card:{…}, exec:{…}}`。
   消費者は CLI (client パッケージ、同 repo)・Web templ (同 repo)・khi の
   task JSON 読み取り箇所。card/exec の omitempty で相手型のノイズは出ない。
   0035 が sidecar を選んだ理由だった「DTO 自動露出」問題のうち、**「相手型の
   フィールドが紛れ込む」側は**この形で解消 (PR-2 レビューで表現を訂正 —
   旧稿は無条件に「解消」と書いていたが、下記5の通り detail blob の方は
   未解消のまま残っている)。
4. **status 語彙**: captured/triaged/ready が API validation から消える。
   これらを送る・filter に使う消費者は存在しない想定だが、khi 側 grep で確認。
5. **card の `detail` blob が全 task list/get レスポンスに乗るようになった**
   (PR-2 レビューで発見・**受け入れ済み契約変更として明記**、未対応のまま
   マージはしない §10 運用ルールにより doc 化が merge 条件)。
   単一テーブル化で `taskSelectCols` (`internal/orchestrator/store.go`) が
   card 専用列を含むようになり、GET /api/tasks・`boid task list --output
   json`・タスク詳細取得を含む**あらゆる** list/get 経路が card 行 1件ごとに
   `detail` (summary/source/content_ref/children/suggestion/observed を畳んだ
   JSON blob) を無条件で返すようになった。PR 前の main では
   `taskSelectCols` に card 系列が一切無く、`detail` は
   `GetTaskTriage`/`ListTaskTriageByTaskIDs` を明示的に呼ぶ経路 (`GetCard`/
   `ListCards`、web.go の queue_next 文脈同梱) だけが取得していた —
   0035 がサイドカーに分けた元の理由 (列追加が全レスポンスへ自動露出する)
   は、card についてはこの PR で実質的に解消していない。
   khi は毎巡 card を list するため影響範囲は大きい。

   サイズについて訂正 (PR-2 レビューラウンド3): 旧稿はここで
   `orchestrator.MaxContentBytes` (64KiB、`internal/orchestrator/
   content_size.go`) が detail の実質的な上限として効くと書いていたが、
   事実誤認だった。`ValidateContentSize("action payload", ...)`
   (`internal/api/workflow_action.go`) がキャップするのは **1回の
   attrs_set リクエストの payload** のみ。保存される detail 自体は
   累積 fold で、`FoldDetailAttrs` (`internal/orchestrator/card.go`) が
   attrs map へパッチをマージし続け、`AddDetailChild` (同ファイル) は
   children 配列へ**上限無く**追記し、`UpsertTaskTriage` (同ファイル) は
   detail の書き込み時にサイズチェックを一切行っていない。つまり 1回の
   リクエストは 64KiB に制限されても、複数回の attrs_set/add_detail_child
   を経た1枚の card の detail は 64KiB を軽く1桁超えうる — **累積には
   上限が無い**。

   **判断: それでも受け入れて契約変更として明記する** (根拠は実装コストの
   低さと非破壊性の2点のみ — 数字によるサイズ上限の主張はしない)。
   projection を切って detail を list から落とす選択肢もあったが、
   projection の新設は今すぐ必要な実装ではなく、後から `?fields=light`
   相当の projection を追加するのは何も壊さない非破壊変更として行えるため
   見送った。detail 自体に累積上限を設ける実装 (`FoldDetailAttrs`/
   `AddDetailChild`/`UpsertTaskTriage` へのサイズガード追加) も選択肢と
   してあるが、本 PR のスコープでは着手しない。khi 側の帯域またはレスポンス
   サイズが実際に問題になった場合の対応先 (projection または累積上限の
   どちらか) は PR-3 以降のフォローアップとする。
6. **`task_update` の payload/auto_start/instructions は card に対して 409 に
   なる**。旧 flat Task では Payload/AutoStart/Instructions が (execution
   専用の意味を持ちつつも) card 上でも構造的に存在するフィールドだったため、
   card への書き込みは黙って no-op マージされていた。PR-2 以降は
   `task.Exec == nil` の場合その項目のフィールドごと存在しない —
   `TaskAppService.UpdateTask` (`internal/api/task_service.go`) は payload・
   auto_start にそれぞれ明示的な `task.Exec == nil` ガードを追加し、
   instructions は `IsInstructionsEditable(task.Type, ...)` が同じ理由で
   card を弾く。3つとも 400 ではなく 409 (Conflict) で返す。khi が過去に
   card へ payload を書いていた運用があった場合、本 PR デプロイ時点でその
   呼び出しが 409 を踏むようになる — デプロイ前に khi 側の書き込み経路を
   確認しておくこと (PR-2 レビューで追記)。

## 6. 移行計画

「一気に」= 互換期間を置かない、であって 1 PR に全部詰める、ではない。
review 可能な 3 PR に割り、外部 (khi) と足並みの要る破壊は最後の 1 個に隔離する。

### PR-1: 内部命名 rename (無風)

Go 型・ファイル名・Web 文言を §4 の表どおりに改名。wire 名 (broker op・HTTP
path) と DB schema は**まだ触らない**。挙動変更ゼロの機械的 PR。
internal/ のパッケージ構成を変えた場合は architecture allowlist の更新を忘れない。

### PR-2: STI migration + tagged struct (本体)

migration 0045 (単一トランザクション、table rebuild):

1. tasks 新テーブルを CHECK 制約込みで作成 (§3.4)。
2. type の判定と移送: `task_triage` に row がある → card。無くても status が
   card 系 (captured/triaged/parked/ready/working/dropped) → card (rowless card
   救済の最終回)。それ以外 → execution。
3. legacy status 洗い替え: captured/triaged/ready → parked (card 側のみ。
   非終端なので parked が正しい着地。actions 履歴は触らない)。
4. card 行: task_triage の kind/urgency/wake_at/wake_task_id/suggestion_verb/
   detail を tasks の対応列へ移送し、実行系列は NULL。
   execution 行: 実行系列を移送し、card 系列は NULL。
5. `task_triage` テーブルを drop。0035/0040/0044 は歴史として残す
   (migration は追記のみ)。

コード側 (同 PR): Task struct の tagged 化 (§3.5)、machineFor 一族の type
switch 化 (§3.6)、capture 経路の浄化 (§3.7)、KnownTaskStatuses ほか legacy
status 分岐の撤去 (§3.3)、store.go の scan/insert/filter (notOpenSelfStatusSQLList・
queue_next 等) の単一テーブル化、GC の pre-execution carve-out を type 参照に。
`task.Behavior` 等の直参照はコンパイルエラーで全数洗い出されるので、
1 箇所ずつ「この呼び出し側はどちらの型を想定しているか」を判断して倒す。

### PR-3: wire rename + khi 同時カットオーバー

broker op・shim・`/api/cards`・CLI 表面の rename (§4 の残り) + **khi 側 PR**
(op 名・`boid card` 呼び出し・task JSON の新形へ追随)。デプロイは boid → khi の
順で同一メンテナンス窓において実施。デプロイ前に `podman ps --filter
name=boid-job` で実行中 job が居ないことを確認する (deploy は running job を
全部 reap する)。

### 移行前の実測 (migration を書く前にやる)

**実施結果 (2026-08-25, PR-2 実装セッション)**: 本番 DB への直接アクセスが無い
実装環境だったため、以下はコード監査で代替した。§9 の未決 3 項の決着根拠は
この節の実測結果そのもの。

- **本番 DB の status 分布**: 直接測れないが、migration 0045 は「実測 0 件でも
  安全」な設計 (§6.2-3 の 洗い替え + rowless card 救済) にしてあるので、
  ブロッカーにならない。念のため migration test
  (`internal/db/migrate/migrate_0045_card_sti_test.go`) に captured/triaged/ready
  の各パターン (sidecar 有り/無し) を fixture として仕込み、0 件でも 100 件でも
  同じロジックで決着することを確認した。
- **rowless card の実在パターン**: `internal/api/machine_select.go` (PR-2 直前、
  rename 後) の `machineFor` の doc comment 自体が実在を明言している ——
  「task_create.go's SeedTaskTriage is deliberately BEST-EFFORT at
  task-creation time ... leaves a real card genuinely rowless」。つまり
  best-effort insert が失敗すると sidecar 行の無い card が実際に生まれる経路が
  コードに書かれている。migration 0045 の 型判定優先順位2「row が無くても
  status が card 系なら card」はこの経路への安全網として設計通り必要。
  実測結果: rowless card は **実在しうる** (コード上の best-effort insert
  失敗パスとして) — 件数は本番 DB を見ないと出せないが、0 件でも 0 件でなくても
  migration の型判定ロジックは変わらない。
- **remote_id の帰属 — 決着: core (§9 参照)**: `internal/orchestrator/store.go`
  の `UpdateTask` 自身の doc comment (PR-2 着手前から存在する既存コメント) が
  「remote_id and auto_start have no status guard here — callers rely on being
  able to set them regardless of task status (**khi patches remote_id on
  working/parked tasks**; ...)」と明言している。working/parked は card 専用
  status であり、この一文は「khi が card に remote_id を書く運用が実在する」
  という本番挙動の直接証言 (コードコメントに残った実測)。加えて同じ doc
  comment が「remote_id and auto_start have no status guard here」とも
  明言している通り、`UpdateTask` は remote_id に一切 status ゲートを掛けて
  いない — `IsPreDispatchEditableStatus` がゲートしているのは Title と
  ProjectID の2つだけで (production 呼び出しは `task_service.go` の該当2箇所
  のみ)、remote_id はそもそもその対象外。つまり card の remote_id 書き込みは
  現に無条件で許容されている実装済みの経路であり、これを Execution 専用に
  倒すと khi の既存運用を壊す。→ **remote_id は共通コアに残す**。
  (旧稿はここで Web UI の task 編集フォームも card への remote_id 書き込み
  経路として挙げていたが、そのフォーム自体 `GetTaskEdit` が
  `Status != pending` を redirect するため card からは到達不能であり誤り
  — PR-2 レビューで指摘され訂正。結論 (core 帰属) 自体は上記の khi 運用実測
  + 無条件無ゲートという実装事実だけで独立に成立するため変更なし)
- **datasource_id の生死 — 決着: 既に死んでいる、drop 済み (§9 参照)**:
  `internal/db/migrate/migrations/0025_drop_tasks_datasource_id.sql`
  (`ALTER TABLE tasks DROP COLUMN datasource_id;`) が既に存在し、
  `internal/db/migrate/migrate.go` の migration チェーンに配線済みであることを
  確認した。つまり本設計 doc の §3.2 が書かれた時点で datasource_id は
  **既に tasks テーブルから消えている** (0025 は 2026-08-25 より前の別 PR で
  マージ済み)。Go の `orchestrator.Task` struct に対応フィールドが無いことも
  「死列」の根拠として §3.2 に書かれていた通り。→ **migration 0045 では
  datasource_id に一切言及不要** (rebuild 元の tasks テーブルに列自体が
  存在しないため、drop 判断すら発生しない)。
- **khi 側の `task_triage_*` op・task JSON フィールド読み取り箇所**: khi は別
  リポジトリで PR-3 の担当範囲 (本 PR-2 の指示スコープ外)。ただし PR-2 で
  変える JSON 形 (`{type, ..., card:{...}, exec:{...}}`) は §5-3 で明記された
  契約変更であり、broker op 名 (`task_triage_get`/`list`) と HTTP path
  (`/api/triage`) は PR-2 で**変更していない** (Q2 相当、PR-1 と同じく無風)。
  khi 側の JSON フィールド直読み (`.behavior` 等のフラット読み) は PR-3 の
  カットオーバー時に対応が必要— このリスクは §5 に既述の通り。

## 7. 検算 — 既知の傷が消えることの確認

モデル起点で設計した結果、以下が**帰結として**消える。1 つでも残るなら
設計に穴がある:

| 傷 | 消える理由 |
|---|---|
| card の行に嘘の behavior 一式 | ExecAttrs ごと存在しなくなる (CHECK で保証) |
| base_branch 未展開テンプレ地雷 | capture が ResolveBehavior を呼ばない |
| machineFor の status 推測・done/aborted 曖昧性 | type switch の純関数化 |
| rowless card と SeedTaskTriage の best-effort | 概念ごと消滅 |
| legacy 3 status のコード分岐 | データ洗い替え + enum 撤去 |
| 命名の二重化 | §4 の対応表で全層統一 |
| DTO 自動露出を理由にした sidecar | tagged JSON + omitempty で解消 |

## 8. テスト・検証

- 既存の機械テスト (machine_card_test.go 等) は card 機械 v2 の辺集合を変えない
  のでそのまま緑のはず——赤くなったら挙動を変えてしまっている。
- migration test: 混在 DB fixture (card with/without sidecar row・legacy status・
  実行タスク) → type 判定・移送・洗い替え・CHECK 成立を検証。
- 等価性: migration 前後で「open 一覧」「queue_next」「task tree」の件数と ID
  集合が一致すること (退役前の等価性検証の規律)。
- khi round: capture → attrs_set → accept(verb) → 遷移、の実弾 1 巡を
  カットオーバー直後に流す。
- `go build ./... && go test -race ./... && go vet ./...`、E2E はホスト側
  `./e2e/run-container.sh`。
- 機械的テストに加えて、レビュワー (LLM/人間) 用の採点表を §10 に置く。
  実装 PR のレビューは §10 を実際に採点して行う。

## 9. 決定済み / 未決

決定済み (2026-08-25 の設計セッション):

1. 互換 alias 期間は置かず一気に切り替える (PR は 3 分割、khi 同時)。
2. 概念は継承 (排他的 subtype)。Go のエンコードは tagged struct
   (interface + 具象 2 型は改修範囲と client 制約から却下)。
3. テーブル戦略は STI + type 列 + CHECK。CTI は subtype 列が増え続ける
   兆候が出たら再検討。
4. 命名は「物 = card、過程 = triage」で統一。

**決着済み (PR-2 実装時、2026-08-25 — 本番 DB に触れない実装環境だったため
コード監査で代替。§6.4 に実測結果と根拠を記載)**:

- `remote_id` の帰属 → **core (共通コア) に決着**。根拠: (1)
  `internal/orchestrator/store.go` の `UpdateTask` 自身の既存 doc comment が
  「khi patches remote_id on working/parked tasks」(working/parked は card
  専用 status) と明言しており、card への remote_id 書き込みが本番で実際に
  起きている運用であることの直接証拠になっている。(2) 同じ doc comment が
  「remote_id and auto_start have no status guard here」とも明言する通り、
  `UpdateTask` は remote_id に一切 status ゲートを掛けていない —
  `IsPreDispatchEditableStatus` がゲートしているのは Title/ProjectID のみ
  (production 呼び出しは `task_service.go` の該当2箇所のみ) — つまり card
  への remote_id 書き込みは現状すでに無条件で許容されている実装済みの経路。
  Execution 専用に倒すと (1) の khi 運用が壊れるため、**保守的判断 (既存動作
  を壊さない側) として core に残した**。実データでこの判断が覆る可能性: 低い
  (doc comment 自体が「本番で実際に起きている」と明言しており、かつ現行実装
  が既に無条件許容という後方互換前提で書かれているため)。
  (旧稿はここで根拠 (2) として Web UI の task 編集フォームが
  `IsPreDispatchEditableStatus` で remote_id をゲートしていると書いていたが、
  実際は同フォーム自体 (`GetTaskEdit`) が `Status != pending` を redirect
  するため card からは到達不能であり、かつ remote_id は上記の通りそもそも
  `IsPreDispatchEditableStatus` の対象外 — 二重に誤りだった。PR-2 レビューで
  指摘され訂正。結論は変わらない)。
- `datasource_id` の生死 → **既に死んでいた (drop 済み) と判明、決着不要に
  帰着**。`internal/db/migrate/migrations/0025_drop_tasks_datasource_id.sql`
  が既に tasks テーブルから datasource_id を drop 済みであることをコード監査
  (migrate.go の migration チェーンを直接確認) で発見した。本設計 doc の §3.2
  執筆時点でこの事実が反映されていなかった (見落とし)。migration 0045 の
  CREATE TABLE 文には datasource_id が最初から存在しないため、drop するか
  否かの判断自体が発生しない。
- type 値の表記 `execution` → **確定のまま実装 (追加決定なし)**。migration
  0045・CHECK 制約・tagged struct 全てで `execution` を使用した。
  `NewExecutionMachine` の命名と一致させる案がそのまま採用され、`work` 等への
  変更は提案されなかった。

## 10. 採点表 — レビュワー用 yes/no 判定リスト

機械的テスト (§8) が拾えない「設計への適合」を、LLM/人間レビュワーが
判定できる形に落とした採点表。運用ルール:

1. 全命題は**「yes = 合格」に極性統一**してある。否定疑問文は使わない。
2. 判定者は各命題に yes/no と**根拠 (file:line または diff hunk) の引用**を
   付ける。**根拠を引けない yes は no として扱う**。
3. no が 1 つでもあれば merge しない。直し方は 2 通りだけ——実装を直すか、
   設計判断を変えたのなら**先に本 doc の該当節を更新**してから実装を通す
   (doc と実装の乖離を作らない)。
4. 対象 PR のグループ + 「全体」グループを採点する。対象外グループは skip。

### PR-1 (内部 rename) 用

| # | 命題 | 根拠の在り処 |
|---|---|---|
| Q1 | diff に `internal/db/migrate/` 配下の追加・変更が含まれない | diff のファイル一覧 |
| Q2 | wire 文字列——broker op 定数 (`task_triage_get`/`task_triage_list`)・HTTP mount (`/api/triage`)・shim のコマンド語 (`task triage`)——が 1 つも変更されていない | sandbox/protocol.go・server/wire.go・sandbox/boid_shim.go |
| Q3 | Go の変更は識別子 rename・ファイル移動・コメント/表示文言の置換のみで、制御フロー (条件分岐・return・エラー処理) の追加/削除/変更を含まない | diff 全 hunk |
| Q4 | 過程を指す triage (queue_sweep.go・歴史 doc・「triage する」という行為の文言) が card に誤置換されていない | §4 の据え置きリストと diff の突き合わせ |
| Q5 | internal/ のパッケージ構成を変えた場合、architecture allowlist が同 PR で更新されている (構成変更が無ければ yes) | allowlist の diff |

### PR-2 (STI migration + tagged struct) 用

| # | 命題 | 根拠の在り処 |
|---|---|---|
| Q6 | migration は新規ファイル 1 本の追記で、既存 migration (0035/0040/0044 含む) を書き換えていない | migrations/ の diff |
| Q7 | table rebuild・type 判定・移送・status 洗い替え・`task_triage` drop が単一トランザクション内にある | migration 本体 |
| Q8 | type 判定の優先順位が「task_triage row 有 → card」→「無くても card 系 status → card」→「それ以外 → execution」の順で実装されている | migration 本体 (§6.2-2 と一致) |
| Q9 | captured/triaged/ready → parked の洗い替えは tasks.status のみで、actions の from_status/to_status を書き換えていない | migration 本体 |
| Q10 | 新 tasks の CHECK に (a) type 値域 (b) 型ごとの status 語彙 (c) 相手型フィールドの NULL 強制、の 3 種が全てある | CREATE TABLE 文 (§3.4 と一致) |
| Q11 | execution 行の behavior 制約は IS NOT NULL であり、空文字を禁止して**いない** (空文字は no-yaml project で合法) | CHECK 文 |
| Q12 | migration test に「sidecar row の無い card 系 status 行」「legacy status 行」「card/execution 混在」の fixture が全てある | migration test |
| Q13 | machineFor / machineForDisplay / resolveReopenVariant から sidecar lookup と status 推測が消え、`task.Type` の switch だけになっている | api/machine_select.go ほか |
| Q14 | ResolveOrCapture の本体に ResolveBehavior 呼び出しと behavior 用 meta hydrate が存在しない | api/task_resolve_or_capture.go |
| Q15 | KnownTaskStatuses に captured/triaged/ready が含まれず、参照分岐 (IsPreExecutionStatus / IsPreDispatchEditableStatus / isCardLifecycleStatus) が撤去または 4+5 状態語彙に縮退している | orchestrator/model.go |
| Q16 | orchestrator.Task に実行系フィールドの flat 定義が残っていない (CardAttrs/ExecAttrs へ完全移動) | orchestrator/model.go |
| Q17 | 「Type と非 nil 側の一致」の検証が DB scan 側と task 作成側の両方に存在する | store の scan・CreateTask 検証 |
| Q18 | SeedTaskTriage・その best-effort 呼び出し・「作成後に card 化する」経路が存在しない | grep SeedTaskTriage = 0 件 |
| Q19 | migration 前後の等価性 (open 一覧・queue_next・task tree の件数と ID 集合の一致) を検証するテストまたは記録された実測手順が存在する | test か PR description |

### PR-3 (wire rename + khi カットオーバー) 用

| # | 命題 | 根拠の在り処 |
|---|---|---|
| Q20 | 旧 wire 名 (`task_triage_get|list`・`/api/triage`・`boid task triage`) への互換 alias・redirect・fallback がコードに存在しない | grep 旧名 = 定数定義の削除のみ |
| Q21 | khi 側の追随 PR が存在し、khi の grep で旧 op 名・旧 flat JSON (`.behavior` 等の直読み) の残存が 0 件である | khi PR の diff |
| Q22 | デプロイ手順に「boid → khi の順・同一メンテナンス窓」「事前の実行中 job 確認 (`podman ps --filter name=boid-job`)」が明記されている | PR description か runbook |

### 全体 (どの PR でも採点)

| # | 命題 | 根拠の在り処 |
|---|---|---|
| Q23 | §7 検算表のうち当該 PR が担う行について、「消えたはずのもの」の削除が diff 上で実在する (行ごとに根拠を引く) | §7 × diff |
| Q24 | 未決 3 項 (remote_id 帰属・datasource_id 生死・type 値表記) が、PR-1 着手前までに §9 で決着済みへ更新されている | 本 doc §9 |
| Q25 | この PR が新設・変更した挙動には対応するテストがあるか、無い理由が PR 上に明記されている | diff とテストの対応 |
