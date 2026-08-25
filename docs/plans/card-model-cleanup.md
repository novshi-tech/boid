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
   0035 が sidecar を選んだ理由だった「DTO 自動露出」問題はこの形で解消。
4. **status 語彙**: captured/triaged/ready が API validation から消える。
   これらを送る・filter に使う消費者は存在しない想定だが、khi 側 grep で確認。

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

- 本番 DB の status 分布: `boid exec` 等で captured/triaged/ready の残存行数を
  数える (washover 完了済みなので 0 が期待値。0 でなくても §6.2-3 が拾う)。
- rowless card の実在数 (task_triage に row の無い card 系 status 行)。
- remote_id / datasource_id の実データ分布 → §3.2 の「監査して決める」2 項の決着。
- khi 側で `task_triage_*` op・task JSON フィールドを読む箇所の grep 一覧。

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

未決 (実測 §6.4 とセットで決める):

- `remote_id` の帰属 (core か Execution か) — 整形セッションが card に
  remote_id を書く運用の有無で決まる。
- `datasource_id` の生死。
- type 値の表記 `execution` の確定 (機械名 NewExecutionMachine に合わせた案。
  より短い `work` 等にするなら **PR-2 の前**に決める — PR-1 は Go の識別子・
  ファイル名の rename のみで `tasks.type` の値リテラルを一切導入しないため、
  この未決はブロッカーにならない。PR-2 の migration 0045・CHECK 制約・
  tagged struct が実際に type 値を書き込む最初の PR なので、そこが締切)。

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
