# Dynamic Base Branch / Worktree Fork Model Overhaul

## 背景

2026-05-20 セッションで以下の問題が顕在化した:

1. mera-ui プロジェクトで `base_branch` 未指定の supervisor タスクが、
   project HEAD が main 以外 (`feature/BGO-170`) なのに **`main` から worktree が切られた**
   (task `0d567279-88b8-4a60-aa01-24ab5eb129b9`)。
2. docs (`docs/ja/reference/project-yaml.md`、 commit `e3575d9`) は「base_branch 省略時は
   project HEAD = current branch」 と書いてあるが、 コードは `""` → `"main"` の暗黙
   フォールバックのまま。 docs と実装が乖離している。
3. multi-level supervisor (sub-sup) のもとで executor 子タスクに依存関係がある場合、
   sub-sup が自分の branch に親の base を取り込まないと、 後発の依存子が古いスナップ
   ショットから fork され、 先発兄弟の merge 済み変更を見られない。

このセッションで仕様レベルから整理し直したので、 落ち着いてレビューできるよう
実装計画として固める。

レビュー (2026-05-20 〜 2026-05-21) を経て、 当初案から以下を変更:

- P4 (依存子 dispatch の遅延 / scheduler 改修) を **削除**。 sub-sup が dispatch
  順序を制御する方針に倒したため、 boid scheduler 側の race 制御は不要。
- マージ責務を skill に書き出す案は撤回。 project 側 instruction に flush する。
- 重複 root sup を task 作成時に弾く案は撤回。 「同一 branch を 1 active task に
  絞る」 lock 仕様に整理し、 後発タスクは FIFO queue で待たせる。 これを P2.5
  として独立ステップ化。

## 仕様 (合意済み)

### worktree=true 時のタスク種別と HEAD

| タスク種別 | HEAD | fork 元 | readonly |
|---|---|---|---|
| **root sup / root exec** | `base_branch` | n/a | sup=true / exec=false |
| **child sup / child exec** | `boid/<task_id8>` | **親タスクの HEAD branch** | sup=true / exec=false |

- root supervisor / root executor (= `parent_id == ""`) は base_branch を HEAD に
  そのまま乗る。 `base_branch == project HEAD branch` なら **project root** で
  動き、 不一致なら **base_branch を HEAD とする worktree** で動く。
- 「root executor が project root で動く」 のは意図どおり (worktree を merge する
  人がいないと boid の最後までサポートする目標と矛盾するため)。
- child タスクは必ず `boid/<id8>` worktree を持つ。 fork 元は **親タスクの HEAD
  branch**: 親が root sup/exec なら base_branch、 親が child sup/exec なら
  `boid/<parent_id8>`。 grandchild の場合も「直接の親」 のみを参照し、 祖父母までは
  辿らない (1 hop)。

### base_branch の解決

省略時 (`""`):

- **root task (`parent_id == ""`)**: `${current_branch}` 展開 (= daemon が見る
  project HEAD branch を task 作成時に解決して row に保存)。 現状の「空 → `main`
  フォールバック」 は廃止。
- **child task**: `parent.BaseBranch` を継承 (`${current_branch}` は呼ばない)。
  現状の継承ロジック (`internal/api/service.go:655`) をそのまま維持。
- 「親継承 → root の `${current_branch}` 展開」 の **優先順位** を明文化する
  (両方が同時に発火するケースは存在しない: child は親を持つ、 root は親を持たない)。

その他:

- `${TASK_REMOTE_ID}` / `${current_branch}` template は引き続きサポート。
- 親継承の invariant 「parent's resolved base wins」 はそのまま維持
  (child.BaseBranch = parent.BaseBranch)。 これは PR target として伝播する
  ために必要。
- detached HEAD で `${current_branch}` が解決できない場合は task 作成時に 400 で
  弾く。 ただし **root task で空 base のときのみ**。 child は親から継承するので
  detached でも問題にならない。

### worktree fork point は task.BaseBranch と分離した別概念

| 概念 | 値 | 用途 |
|---|---|---|
| `task.BaseBranch` (継承) | 親から継承 (= root の base) | PR target、 `BOID_BASE_BRANCH` env で executor に渡る |
| worktree fork point (動的) | 親 task の HEAD branch | `git worktree add` の start-point |

- task row に新フィールドは追加しない。 worktree 作成時に dispatcher が
  `task.ParentID` を引いて parent の HEAD branch を計算する。
- root sup/exec の fork point は base_branch そのもの。
- child の fork point は parent の HEAD branch。 parent が root なら base_branch、
  parent が child なら `boid/<parent_id8>`。

### 同一 branch を 2 タスクが同時に持たない (lock 仕様)

「同一 working copy で複数タスクを実行しない」 を実現するため、 lock の粒度を
**(project, HEAD branch)** にする。 base_branch ではないことに注意 (`feature/X` を
base に取る child は HEAD が `boid/<id8>` でユニーク、 lock 上は別物)。

| タスク種別 | HEAD branch | lock key |
|---|---|---|
| root sup / root exec | `task.BaseBranch` | `<projectID>:<baseBranch>` |
| child sup / child exec | `boid/<task_id8>` | `<projectID>:boid/<task_id8>` |

含意:

- child task は HEAD が globally unique (task id 起源) なので構造的に contention
  しない。 acquire は走るが必ず即時成功。
- root task は同 project の同 base_branch の他 root と serialize される。
- 異なる base_branch の root sup 同士、 root sup と child exec 等は並行 OK。

ルール:

- task 作成は弾かない (validate しない)。
- task が executing 遷移するときに lock を acquire。 既存 lock 保持タスクがあれば
  FIFO queue で待つ (`WorktreeLocker` の既存 FIFO 実装をそのまま使う)。
- worktree 作成 (`r.resolveWorktree` @ `internal/dispatcher/runner.go:109`) は lock
  acquire 後に行われるので、 git の「same branch in 2 worktrees」 衝突は発生しない。
- lock は executing-lifetime held (awaiting 中も保持)。 awaiting 中に同 branch の
  別タスクを動かすと worktree が衝突するため、 release は terminal 遷移時のみ。
- 既存 `shouldHoldProjectLock` (`!readonly && !worktree` フィルタ) と hook 有無
  フィルタは廃止 (readonly root sup も HEAD branch を占有するため lock 対象)。

### 依存子の最新化フロー

依存関係制御は **boid コア機構には頼らない**。 sub-sup が子タスクの dispatch
順序と base 同期を制御する。

```
A (executor) → done → A の PR を base に merge (root or sub sup が gh API)
                         ↓
                   base が remote で進む
                         ↓
              sub sup が git fetch origin <base> && git merge origin/<base>
              で boid/<subid8> を更新 (host 側 git なので readonly でも通る)
                         ↓
              sub sup が B を dispatch → B の worktree が
              更新済み boid/<subid8> から fork → A の commit が見える
```

- マージのタイミング・対象・コマンドは **project 側 instruction の責務**。 PR-merge
  → fetch → merge にするか、 ローカルブランチを直 merge にするかは project 運用
  ポリシーで決まる。
- skill (boid-task の supervisor / executor mode) には merge 責務を articulate しない
  (instruction で書き分けるなら skill 側の記述は実体を持たない)。
- boid コア側の責務は **env を渡すこと** に限定 (`BOID_BASE_BRANCH` 既存 +
  `BOID_PARENT_BRANCH` 新規)。

なお boid コアの依存関係制御機構 (旧 `ComputeTaskBlocked` / `task_dependencies`
DB schema 等) は廃止済み (PR #481 / #482 / #483)。 本計画はコア依存制御に
依存しない設計のため影響なし。

### モデル別の動作確認

| モデル | base_branch | root sup の場所 | child fork 元 | PR target | 機能性 |
|---|---|---|---|---|---|
| M1: single-sup multi PR | `main` | project root (case 1) | root sup の `main` | `main` | ✓ |
| M2: single-sup 1 PR | `${TASK_REMOTE_ID}`=PR-42 | worktree HEAD=PR-42 (case 2) | root sup の PR-42 | PR-42 (sup が main へ merge) | ✓ |
| M3: multi-sup 1 PR | PR-42 | worktree HEAD=PR-42 | grandchild は sub sup の boid/<subid8> | PR-42 | ✓ sub sup が PR-42 を自 branch に merge |
| M4: multi-sup multi PR | `main` | project root | grandchild は sub sup の boid/<subid8> | `main` | ✓ sub sup が main を自 branch に merge |

## 実装ステップ

### P1: 空 base_branch → 親継承 / `${current_branch}` 展開

**対象ファイル**:
- `internal/orchestrator/base_branch_classify.go:81-82` ― `""` → `"main"` フォールバック削除
- `internal/dispatcher/worktree_manager.go:34-36` (`resolveBaseBranch`) ― 同
- `internal/dispatcher/worktree_manager.go:71-75` (`ensureBaseBranchExists`) ― 同
- `internal/api/service.go:655` ― parent 継承後の空チェックを追加 (root のみ展開)
- `internal/api/service.go:660` 周辺 ― expansion gate を「空文字列でも展開する」 に変更

**変更方針**:

優先順位:

1. parent 指定あり (`req.ParentID != ""`): `task.BaseBranch = parent.BaseBranch`
   をそのまま採用。 空でも空のまま伝播 (親が空ということは root が空であり、 root
   が detached HEAD で弾かれていなければ親は展開済み → child の継承時点では空に
   ならないはず)。
2. parent 指定なし + base 指定なし: `gitCurrentBranch(proj.WorkDir)` で展開して
   row に保存。 detached HEAD なら 400。
3. parent 指定なし + base 指定あり: `ExpandBaseBranch` で template 展開
   (既存挙動)。

以降の dispatcher / worktree manager は空文字列を受け取らない前提でよい (= 既存の
`""` → `"main"` フォールバックは死コード化、 安全な assertion に置き換えるか削除)。

既存 utility (`orchestrator/branch_var.go:14` の `ExpandBaseBranch` /
`branch_var.go:44` の `gitCurrentBranch`) を流用する。

**テスト**:

- `TestClassifyBaseBranch_Case1_EmptyBaseBranchDefaultsToMain` を
  `TestClassifyBaseBranch_Empty_RejectedAtCreationTime` 系に書き換え (classify は
  空を受け取らない前提に)。
- `internal/orchestrator/base_branch_classify_test.go` の他テストも併せて棚卸し
  (`""` フォールバック前提のものを一括 grep して書き換え)。
- mera-ui 再現ケース (HEAD = feature ブランチ + 空 base) で root supervisor が
  project root (case 1) になることを assert する integration test 追加。
- detached HEAD で root task + 空 base → 400 を assert。
- detached HEAD で child task + 空 base → parent から継承して成功を assert。

### P2.5: branch 単位 lock への移行

**現状**: `ProjectLockManager` (`internal/orchestrator/project_lock.go`) は
projectID 単位で acquire し、 `shouldHoldProjectLock` で readonly / worktree=true
タスクを除外する。 「project root を hook で直書きするタスク同士」 を serialize
する設計。

**問題**: P2 後 (root sup が base_branch worktree を持つ) では、 readonly な root
sup も HEAD branch を占有するので、 同 base_branch の 2 つ目が git 制約で worktree
作成失敗する。 既存 lock では readonly がすり抜けるため救えない。

**変更方針**:

- `ProjectLockManager` を `BranchLockManager` (or `WorktreeBranchLockManager`) に
  rename。 意図 (project ではなく branch を守る) を明確化。
- key を projectID → `<projectID>:<headBranch>` の compound key に変更。 別 project
  が同名 branch を持つ場合の false contention を防ぐ。
- HEAD branch を引く utility を追加: `computeHeadBranch(task) string`
  - root task (`task.ParentID == ""`): `task.BaseBranch`
  - child task: `"boid/" + shortID(task.ID)`
  - P3 の forkPoint 計算とロジックを共通化 (parent の HEAD branch を引くときに
    parent に対して再帰的に `computeHeadBranch` を呼ぶ形にする)。
- `shouldHoldProjectLock` 撤廃: 全タスクで acquire を試みる。 child は unique key
  で常に即時成功 (mutex 1 回の overhead のみ、 実害なし)。
- hook 有無フィルタ (`ListHooksForStatus(...) > 0`) 撤廃: 守る理由が「HEAD branch
  の占有」 に変わったので、 hook の有無と無関係。
- Acquire 条件: `task.Status == executing` のみ残す (`internal/api/service.go:2019`)。
- Release タイミングは現状維持 (terminal 遷移時)。

**注意**:

- 既存 lock は in-memory なので daemon restart で消える。 移行時の interim state
  は実質ない (daemon 起動時の現状把握で再 acquire する経路が現状なし → 起動直後の
  executing タスクは lock 持たない状態で再開する。 新仕様でも同じ挙動でよい)。
- coordinator.go:38 の `Locker WorktreeLocker // deprecated` は本変更で正式に
  削除可能 (`Coordinator` から `Locker` フィールドごと撤去)。

**テスト**:

- 同一 project + 同一 base_branch の 2 つの root sup task が executing 遷移する
  と、 後発が前者の release まで待つことを assert (lock の FIFO 性)。
- 異なる base_branch の root sup 同士、 root sup + child exec が並行実行できる
  ことを assert。
- 同一親の child 2 つ (それぞれ `boid/<idA>` `boid/<idB>`) が並行実行できることを
  assert。
- readonly root sup が lock を取得することを assert (現状仕様との差分)。

### P2: root sup / root exec の worktree HEAD を base_branch 直接にする

**現状**: `WorktreeManager.Create` は無条件で `boid/<task_id8>` ブランチを切る
(`internal/dispatcher/worktree_manager.go:161` 付近)。

**変更方針**:

- `Create` に「supervisor / root-executor 用に base_branch を直接 checkout する」
  モードを追加する。 シグネチャ案:
  ```go
  func (m *WorktreeManager) Create(projectDir, projectID, taskID, baseBranch string, opts CreateOpts) (*Worktree, error)
  type CreateOpts struct {
      // CheckoutBranch を指定すると boid/<id8> ではなくその branch を worktree
      // HEAD にする。 root sup / root exec の case 2/3 用。
      CheckoutBranch string
      // ForkPoint は worktree 作成時の start-point。 デフォルトは baseBranch。
      // P3 で child の場合に parent の HEAD branch を渡す。
      ForkPoint string
  }
  ```
- `worktree_resolver.go` 側で `task.ParentID == ""` を判定して opts.CheckoutBranch
  に baseBranch を渡す。

**重複 root sup への対応**:

- task 作成時の validation は **行わない**。 P2.5 の branch lock が executing 遷移
  時に serialize する。
- worktree 作成 (`r.resolveWorktree` @ `runner.go:109`) は lock acquire 後に走るので、
  先発が release するまで後発の worktree 作成は始まらず、 git レベルの衝突も発生
  しない。
- 先発 task が terminal で worktree を cleanup したあと、 後発が同 path に worktree
  を作る (path は task id ベースなので衝突しない、 HEAD だけ共有)。

**注意**:

- worktree HEAD = base_branch にすると、 同じ base_branch を取る別の root task や
  project root と git worktree レベルで衝突する。 これは P2.5 の branch lock で
  serialize されるので衝突しない (lock 待ちで queue される)。
- `EnforceHeadOnBaseBranch` ガード (`worktree_manager.go:115`) は **残す**:
  task 作成と dispatch の間に project HEAD が手動で動かされた場合の検知用。 lock
  とは別の invariant。

**テスト**:

- root sup case 2 で worktree HEAD = base_branch になることを assert。
- 同 base の 2 つの root sup task を作って executing 遷移させると、 後発は前者の
  完了まで queue され、 完了後に worktree が作られることを assert (P2.5 と統合
  テスト)。
- root exec case 2 で worktree HEAD = base_branch になることを assert。

### P3: child の worktree fork 元を「親タスクの HEAD branch」 に変更

**現状**: `WorktreeManager.Create` は `baseBranch` (= task.BaseBranch) を fork 元と
してそのまま使う。 child は親の base から fork している。

**変更方針**:

- `worktree_resolver.go` の First-time creation ブロック (`worktree_resolver.go:79`
  前後) で:
  ```go
  if task.ParentID != "" {
      parent, _ := r.TaskLookup.GetTask(task.ParentID)
      forkPoint := computeHeadBranch(parent)  // P2.5 で導入する utility
      // Create に opts.ForkPoint = forkPoint を渡す
  }
  ```
- `WorktreeManager.Create` に `ForkPoint` (opts 経由) を追加。 デフォルト
  (空文字列) は `baseBranch` で root 用。 child は forkPoint = parent's HEAD branch。
- `ensureBaseBranchExists` / `resolveBaseBranch` は forkPoint 種別で経路を分岐:
  - forkPoint が remote-backed base (`main`, `feature/X` 等): 既存ロジック (remote
    fetch + ローカル ref 確認)。
  - forkPoint が `boid/<parent_id8>`: **ローカル branch 存在確認のみ**。 remote
    fetch は呼ばない (remote に存在しない local-only branch のため)。 親 task の
    worktree が既に作成済み = local branch があるはず、 なければ親 task が dispatch
    されていない異常状態として error。
- task.BaseBranch (= PR target) はそのまま保存し、 `BOID_BASE_BRANCH` env に渡す
  (既存挙動継続)。

**env の追加**:

- `BOID_PARENT_BRANCH` を新規追加。 値は親 task の HEAD branch
  (`computeHeadBranch(parent)`)。 root task では空。
- agent (sub-sup) は instruction に従って `git merge origin/$BOID_BASE_BRANCH` か
  `git merge $BOID_PARENT_BRANCH` かを書き分けられる。

**テスト**:

- M3 (multi-sup 1 PR): sub-sup の worktree が root sup の HEAD branch から、
  sub-sup の child exec の worktree が sub-sup の `boid/<subid8>` から
  fork されることを assert。
- M4 (multi-sup multi PR): 同上で base=main の場合。
- forkPoint が `boid/<parent_id8>` のとき、 remote fetch が呼ばれないことを assert
  (mock した git で fetch コマンドが出ないことを確認)。
- `BOID_PARENT_BRANCH` env が child の executor / hook に渡ることを assert。 root
  task では空であることを assert。

### P4: (削除)

当初案では「依存子 dispatch の遅延 / scheduler 改修」 を盛り込んでいたが、
sub-sup が dispatch 順序を制御する設計に倒したため不要。 boid scheduler 側の
race 制御は持ち込まない。

関連: boid コアの依存関係制御機構 (旧 `ComputeTaskBlocked` / `task_dependencies` DB
schema 等) は廃止済み (PR #481 / #482 / #483)。 本計画はコア依存制御に依存しない
設計のため影響なし。

### P5: docs 更新

**対象**:

- `docs/ja/reference/project-yaml.md` / `docs/en/reference/project-yaml.md` の
  base_branch / worktree セクションを実装に揃える。
  - 「空 base_branch = `${current_branch}` 等価 (root のみ。 child は親継承)」 を
    明文化
  - root vs child の HEAD / fork 元の違いを表で説明
  - 1 active task per (project × HEAD branch) の lock 仕様を新節として追加
  - base 同期 (sub-sup の merge 責務) は project 側 instruction で記述する旨を
    明記。 skill / コアは関与しない
- `docs/ja/guide/concepts.md` の worktree セクションも更新。
- `BOID_PARENT_BRANCH` / `BOID_BASE_BRANCH` env の説明を env 一覧 docs に追加。
- 既存テスト docstring (`TestClassifyBaseBranch_Case1_EmptyBaseBranchDefaultsToMain` 等)
  の意図コメントを新仕様に揃える。

## 実装順序の推奨

`P1 → P2.5 → P2 → P3 → P5` の順。

- P1 は他に影響しない単独 PR で先行可能。
- **P2.5 を P2 より先**: P2 で root sup を base_branch worktree に乗せた瞬間に
  同 base の 2 つ目が git エラーで落ちる window が開く。 lock を先に整備して
  おく方が安全。 P2.5 単独では「lock が取られるが contention が無い」 状態で
  merge できる (現状 root sup は project root 直実行で別 lock パスなので無害)。
- P3 は P2 の Create signature 拡張に依存。
- P5 は P1-P3 が安定したタイミングで一括反映。

## 関連する既存メモ

- `feedback_plan_no_worktree.md` ― supervisor の worktree=false は意図どおり、
  EROFS 罠の出自。 本計画の P2 で「root sup が project root or base_branch worktree」
  ルールを明文化することで補足が必要 (`worktree:true × readonly:true` の EROFS
  問題自体は別軸で残るので、 memory は陳腐化ではなく「補足追記」 が正しい)。
- `project_role_shift.md` ― boid 本体は workflow 統括に寄せ、 git 操作は agent 側に
  委ねる方針。 本計画の P3 (env 追加) と「マージ責務を instruction に flush」 は
  これに整合。

## 後続計画 (本計画スコープ外)

1. **boid コアの依存関係制御廃止**: ~~`ComputeTaskBlocked` / DB schema
   (`task_dependencies` 表) / API パラメータ / docs を順次廃止。~~ **廃止済み (PR #481 / #482 / #483)**。
2. **worktree 共有モデル**: 同一 base_branch の root sup を「同じ worktree path
   で serialize 実行」 する案。 現状の P2.5 は「異なる worktree path だが lock で
   serialize」。 共有モデルにすると worktree 作成 cost を 1 回に圧縮できるが、
   cleanup 責務が複雑化するため、 並行実行ユースケースが顕在化するまでは保留。
3. **executor が親 supervisor の branch を参照したい** ユースケース
   (rebase 等)。 P3 で `BOID_PARENT_BRANCH` env を追加するため、 executor からも
   読める。 専用の高レベル API が必要になれば後付け。

## 未解決の懸念

1. **multi-level で各 sub-sup が独自 PR を持ちたい場合** (cascading 1-sup-1-PR):
   親継承の invariant により child sup は parent.BaseBranch を継承するので、
   独自 PR を切れない。 user との議論で「PR は executor 単位か root sup にまとめる
   どちらか」 と方針決定済みなので、 cascading パターンは spec として disallow する。
   docs にも明記する。
