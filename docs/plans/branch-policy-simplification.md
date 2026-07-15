# branch policy simplification (per-task branch 廃止・clone 時代への整合)

ステータス: 計画草稿 (2026-07-15、実装未着手)
作成日: 2026-07-15
親ドキュメント: [git-gateway-cutover.md](git-gateway-cutover.md) — cutover 後に判明した設計漏れの根治として位置付け
関連: [container-based-boid.md](container-based-boid.md), [dynamic-base-branch-overhaul.md](dynamic-base-branch-overhaul.md)

---

## 目的

現行の branch policy は **git worktree ベースの分離**を前提にした遺物になっている。git gateway cutover (v0.0.10) 以降は各 job が独立した fresh clone を持つため、**clone 自体が isolation 単位**となり、内部で per-task branch 名 (`boid/<id8>`) を切る必要は原則消失した。

本 plan は per-task branch (`boid/<id8>`) と fork point 概念を廃止し、**clone 内では常に `BaseBranch` を checkout する**最小 policy に一本化する。副産物として cutover 後に顕在化した「readonly supervisor が worktree=true の子を作れない」設計ミスマッチが自然消滅する。

---

## 前提となる決定事項

- **big-bang cutover (nose 2026-07-15)**: 段階移行 (readonly parent fallback 等) は採らず、 v0.0.11 で一気に切り替え
- **fork point 概念は完全廃止**: 「parent の未 push commit を child が継承」という semantics は clone モデルに存在しないので偽装をやめる
- **push 先 branch 名は task が明示 declare する方向へ**: 内部 checkout 名と push 先名を分離
- **BOID_PARENT_BRANCH env は再定義または廃止**: 現行の「親の worktree branch」意味論は消える
- **v0.0.11 として release**: v0.0.10 で cutover 完了直後の勢いで整理する

---

## 背景: なぜ worktree 時代の policy が clone 時代に合わないか

### worktree 時代 (cutover 前)

- host 側 project dir で `git worktree add` により per-task worktree を切っていた
- 同一 `.git` を共有し、worktree ごとに HEAD だけ独立させる仕組み
- **worktree の制約**: 同じ branch を複数 worktree で同時 checkout できない → **per-task branch 名 (`boid/<id8>`) が物理的に必要だった**
- parent の worktree branch を child が checkout することで inflight commit を継承できた (同一 .git だから見える)

### clone 時代 (cutover 後、v0.0.10 現行)

- 各 job は sandbox 内で完全に独立した fresh clone を持つ
- clone 同士は物理的に別ファイルシステム、gateway 経由でしか通信しない
- gateway は origin に push 済みの ref だけを advertise (`internal/sandbox/runner/clone.go:203-207`)
- **branch 名で isolation する必要が消失**: 同じ branch 名を別 clone で checkout しても衝突しない
- **inflight commit の継承は物理的に不可能**: fresh clone は origin の push 済み ref しか見えない

### 現行が抱えてる矛盾 (2026-07-15 顕在化)

- `internal/orchestrator/head_branch.go:32` `ComputeForkPoint` は「parent の `boid/<id8>` を child の fork point にする」
- `internal/orchestrator/jobspec.go:187` はコメントで **"which must already be pushed to origin"** と明記
- しかし canonical では `task_behaviors` の **supervisor は readonly=true (fail-safe)** ([[boid-canonical-task-behaviors]])
- readonly supervisor は sandbox が read-only + gateway token に write scope 無し → **自分の `boid/<id>` を push できない**
- 結果: **readonly supervisor が worktree=true の子を作る canonical フロー自体が cutover で壊れた**
- 実例: task `21ffd8f4-a249-4b81-886c-8269f2f797d5` が起動 4 秒で "resolve fork point ... not found in clone" で abort (parent `86beacc7`, readonly=true)

---

## 現状の branch policy (fact 要約)

主要関数と caller。全て `internal/orchestrator/head_branch.go` に集約。

### `ComputeHeadBranch(task) string`

- root task (`ParentID == ""`) → `task.BaseBranch`
- child task → `"boid/" + task.ID[:8]`
- caller: `BuildCloneDeclaration` (下記), `taskBusinessEnv` (planner.go:162, `BOID_PARENT_BRANCH` env の値)

### `ComputeForkPoint(parent) string`

- parent が nil → `""`
- parent が `Worktree == false` → `parent.BaseBranch`
- otherwise → `ComputeHeadBranch(parent)` (= `"boid/<pid8>"`)
- caller: `BuildCloneDeclaration` のみ

### `BuildCloneDeclaration(task, parent, baseBranchForkPoint) *CloneDeclaration`

- root task or `worktree == false` → `{Branch: BaseBranch, CheckoutOnly: true, BaseBranchForkPoint: ...}`
- child task with `worktree == true` → `{Branch: "boid/<id8>", ForkPoint: ComputeForkPoint(parent), BaseBranchForkPoint: ...}`
  - cross-project guard: parent が別 project なら `ForkPoint` は空にして runner が BaseBranch にフォールバック
- caller: `planner.PlanHook` (planner.go:109)

### CloneDeclaration 消費側

- `internal/dispatcher/sandbox_builder.go:760-761`: `ForkPoint` / `BaseBranchForkPoint` を JobSpec.CloneSpec に写す
- `internal/sandbox/runner/clone.go:172-183`: runner が clone 後 `checkout -B <Branch> <ForkPoint>` する
- `internal/sandbox/runner/clone.go:189-199`: `BaseBranchForkPoint` は BaseBranch がまだ origin に無いとき (case-3) の start 点解決に使う

### env

- `BOID_PARENT_BRANCH` (planner.go:163): 親の `ComputeHeadBranch` の値。sub-supervisor が `git merge $BOID_PARENT_BRANCH` で使う想定 (docs/en/reference/hook-contract.md:52)
- `BOID_BASE_BRANCH`: task の base_branch (これは branch policy と独立、残す)

### docs

- `docs/en/reference/project-yaml.md:133` に既に「the child branch must be pushed before the sub-sup dispatches its next child」と明記されている (英語 doc は cutover 制約を認識済み、ただし readonly supervisor 制約は反映されていない)
- `docs/ja/reference/project-yaml.md:138`, `docs/en/reference/hook-contract.md:52`, `docs/ja/reference/hook-contract.md:52` も要更新

---

## 新設計

### 内部 checkout の統一

**clone 内では task の役割 (root/child/supervisor/executor) に関わらず `BaseBranch` を直接 checkout する**。

- `boid/<id8>` per-task branch は完全廃止
- fork point 概念は完全廃止 (`ForkPoint` フィールドを CloneDeclaration から削除)
- `BaseBranchForkPoint` (case-3 = BaseBranch が origin に未存在時の start 点解決) は残す (これは task 間の分離とは別軸)

### push 先 branch 名の扱い

**「内部 checkout branch」と「push 用 branch 名」は別の概念として明示分離する**:

- 内部 checkout branch = 常に `BaseBranch`
- push 用 branch 名 = task が declare (通常は `BaseBranch` そのまま、customize したければ instruction 内で `git push origin HEAD:refs/heads/<name>` を書く)

executor は clone 内で `BaseBranch` を checkout して作業し、最後に `git push origin HEAD` する。並列兄弟 executor が同じ `BaseBranch` に push すると衝突するが、これは **executor の責務で rebase/retry する** (現行 policy 下でも並列衝突は executor 契約なので変わらない、[[boid-canonical-task-behaviors]] 参照)。

**兄弟同士で真に isolate したい特殊ケース** (Slack meta-supervisor 経由の 3 issue 並列など) は、supervisor が child ごとに **異なる `base_branch` を明示指定** する運用に寄せる (例: BGO-214/215/216 それぞれに `base_branch: feature/BGO-214` 等)。これは今も可能な手段で、意図の明示化。

### `BuildCloneDeclaration` の新シグネチャ (実質的簡素化)

```go
func BuildCloneDeclaration(task *Task, baseBranchForkPoint string) *CloneDeclaration {
    if task == nil { return nil }
    return &CloneDeclaration{
        Branch:              task.BaseBranch,
        CheckoutOnly:        true,
        BaseBranchForkPoint: baseBranchForkPoint,
    }
}
```

parent 引数が不要になる (fork point 概念消滅の直接反映)。

### `ComputeHeadBranch` / `ComputeForkPoint` の扱い

- **`ComputeForkPoint` は削除** (call site が `BuildCloneDeclaration` のみ、そこも撤去)
- **`ComputeHeadBranch` は削除**: `BuildCloneDeclaration` は BaseBranch 直参照になるし、`taskBusinessEnv` の `BOID_PARENT_BRANCH` 用途も後述の通り再定義される

### `BOID_PARENT_BRANCH` env の再定義

現行: 親の `boid/<id8>` branch 名 (planner.go:163)。sub-supervisor が `git merge $BOID_PARENT_BRANCH` で inflight commit を取り込む用途。

新: `BOID_PARENT_BRANCH` は **親の `BaseBranch`** を返す (親が hydrate されている場合のみ)。「merge して継承」用途は、そもそも clone モデルでは親が push しない限り継承できないので、意味論は「親と同じ base branch で作業しているか?」の判定材料に格下げする。用途が薄いなら env ごと削除も検討。

**判断**: 一旦は「親の BaseBranch を返す」に変更 + doc で「clone モデルでは merge 継承は成立しないので参考情報」と明記。次 phase で使用実態を見て削除判断。

---

## スコープ

### 削除するもの

| 対象 | ファイル | 削除理由 |
|---|---|---|
| `ComputeHeadBranch` 関数 | `internal/orchestrator/head_branch.go:14-23` | `boid/<id8>` per-task branch 廃止 |
| `ComputeForkPoint` 関数 | `internal/orchestrator/head_branch.go:32-40` | fork point 概念廃止 |
| `CloneDeclaration.ForkPoint` フィールド | `internal/orchestrator/jobspec.go` (CloneDeclaration struct) | 同上 |
| `CloneSpec.ForkPoint` (JobSpec 側) | `internal/orchestrator/jobspec.go:149,192` + `internal/dispatcher/sandbox_builder.go:760` | JobSpec に伝播しない |
| runner の ForkPoint 分岐 | `internal/sandbox/runner/clone.go:172-183` | `checkout -B <Branch>` は BaseRef 直で済む |
| `resolveCloneRef` 関数 | `internal/sandbox/runner/clone.go:208-219` | ForkPoint 解決の唯一の call site |
| `head_branch_test.go` の per-task branch / fork point 系テスト | `internal/orchestrator/head_branch_test.go` | 前提消失 |
| `clone_e2e_test.go` の fork point ケース | `internal/sandbox/runner/clone_e2e_test.go:150-160,201` | 同上 |

### 変更するもの

| 対象 | ファイル | 変更内容 |
|---|---|---|
| `BuildCloneDeclaration` シグネチャ | `internal/orchestrator/head_branch.go:62-84` | `parent` 引数削除、常に `{Branch: BaseBranch, CheckoutOnly: true}` |
| `planner.PlanHook` の呼び出し | `internal/orchestrator/planner.go:109` | `parent` 引数を渡さない |
| `taskBusinessEnv` の `BOID_PARENT_BRANCH` | `internal/orchestrator/planner.go:162-163` | `parent.BaseBranch` を返す (現行の `ComputeHeadBranch(parent)` から変更) |
| CloneDeclaration doc | `internal/orchestrator/jobspec.go:145-195` | ForkPoint 記述削除、CheckoutOnly が常時真になる旨明記 |
| SKILL.md "Creating child tasks" | `internal/skills/data/boid-task/SKILL.md:255-273` | `base_branch` 説明を更新、per-task branch 名の記述削除、「並列兄弟 executor は base_branch を分けろ」の明示 |
| SKILL.md "Handling Aborted" | 同上 :413-421 | fork point 系エラーが原理的に発生しなくなる旨反映 |
| project-yaml.md (ja/en) | `docs/{ja,en}/reference/project-yaml.md` | 「child branch を push してから次 child」記述削除 |
| hook-contract.md (ja/en) | `docs/{ja,en}/reference/hook-contract.md:52` | `BOID_PARENT_BRANCH` 意味論の再定義 |

### 残すもの

| 対象 | 残す理由 |
|---|---|
| `BaseBranchForkPoint` (case-3 fork point) | task 間分離とは独立、BaseBranch が origin 未存在時の start 点解決に必要 |
| `resolveCloneForkStart` (`clone.go:189-199`) | case-3 の解決ロジック、そのまま |
| `CheckoutOnly` フィールド | 意味変更なし (常に true になるだけ) |
| `BOID_BASE_BRANCH` env | branch policy と独立 |
| Cross-project child のガード | 親が別 project のケースでも `BaseBranch` 直 checkout なら問題なくなる。ガード自体は shrink |

---

## 移行手順

### Phase 1: コア変更 (1 PR)

1. `BuildCloneDeclaration` を新シグネチャに (parent 引数削除、常に CheckoutOnly)
2. `ComputeHeadBranch` / `ComputeForkPoint` を削除
3. `CloneDeclaration.ForkPoint` / `CloneSpec.ForkPoint` フィールド削除
4. runner の ForkPoint 分岐 (`clone.go:172-183`) と `resolveCloneRef` を削除
5. `taskBusinessEnv` の `BOID_PARENT_BRANCH` を `parent.BaseBranch` に変更
6. 関連 test 更新 (`head_branch_test.go` / `clone_e2e_test.go` / planner test)

### Phase 2: docs 更新 (同 PR or 別 PR)

- SKILL.md 2 節書き換え
- project-yaml.md (ja/en) 該当節書き換え
- hook-contract.md (ja/en) 該当行書き換え

### Phase 3: e2e 検証

- 既存 e2e シナリオが `boid/<id8>` branch 名を assert していないか grep
- child task を作る e2e シナリオが `feature/*` 等の explicit base_branch を使っていることを確認
- 必要なら fixture 更新

### Phase 4: release

- CHANGELOG に breaking change として記載 (BOID_PARENT_BRANCH 意味変更)
- v0.0.11 タグ + Release

deprecation warning は入れない。cutover 直後で v0.0.10 の使用実績が短いこと、per-task branch 名を assert してる external code は現時点で確認されていないこと、破壊性が実質ゼロと判断できるため。

---

## 残ケースの検討

### 1. 並列兄弟 executor が同じ base_branch で push 衝突

現行も boid は調停していない (executor 契約で rebase/retry)。新案でも同じで、変わらない。
真に isolate したい場合は supervisor が **子ごとに異なる `base_branch` を明示指定** する運用に寄せる (例: BGO-214/215/216 で `feature/BGO-214`, `feature/BGO-215`, `feature/BGO-216`)。これは今も可能な手段、意図の明示化。

### 2. writable supervisor の inflight commit を child に渡したい

現行の `boid/<pid8>` 経由の暗黙継承は成立しなかった (fresh clone に未 push ref 見えず)。新案でも同じで、変わらない。
必要なら supervisor が明示 `git push origin HEAD:refs/heads/staging-*` してから child の `base_branch` にその名を指定する。意図の明示化。

### 3. root task の worktree branch

現行 CheckoutOnly path が担当。新案では CheckoutOnly が常時真になるだけで、root task の挙動は変わらない。

### 4. Cross-project child

現行の cross-project guard は「parent の boid/<pid8> は別 project の clone に無いので ForkPoint を空にする」ロジック。新案では ForkPoint 概念自体が消えるので guard 不要。

### 5. sub-supervisor が `git merge $BOID_PARENT_BRANCH` している既存 project

grep して実在するか確認要 (現時点で production 内 project.yaml に該当実装があるか未確認)。あれば個別に置換依頼 or fallback を残す判断。

---

## テスト

- `head_branch_test.go` を全面書き換え: parent 引数なし、常に BaseBranch を返すことを assert
- `clone_e2e_test.go` の fork point ケース (:150-160, :201) を削除、代わりに「child task も root task と同じく BaseBranch を checkout する」ケースを追加
- planner test で `BOID_PARENT_BRANCH` が親の BaseBranch を返すことを assert
- readonly supervisor + child (worktree=true) の e2e シナリオを追加 (今回の regression 再発防止)
- 既存の "readonly supervisor が children を持つ" e2e シナリオが green になることを確認

---

## リスク

- **BOID_PARENT_BRANCH の意味変更**: sub-supervisor が `git merge $BOID_PARENT_BRANCH` を使ってた場合、意図した挙動と変わる可能性。事前に grep で実利用を確認。
- **`boid/<id8>` を assume している external tool / dashboard**: web UI・timeline 表示等で branch 名を expose している箇所があるか確認。
- **e2e regression**: git-gateway 関連 6 シナリオが fork point 挙動を assert していないか確認 (`e2e/scenarios/git-gateway-*`)。
- **cross-project child が壊れる可能性**: cross-project guard の削除で予期せぬ挙動出るか、既存 test で担保できてるか要確認。

いずれも Phase 1 の実装前に grep + 既存 e2e 一巡で洗える範囲。

---

## 副次的なメリット

- **設計ミスマッチの根治**: readonly supervisor + worktree=true child が壊れる問題が自然消滅
- **概念の削減**: `boid/<id8>` / fork point / cross-project guard / `resolveCloneRef` が全て消える。head_branch.go / clone.go の LOC が実質半減
- **docs の簡素化**: 「child branch を push してから次 child」等の cutover 対応注意書きが不要に
- **意図の明示化**: 並列 executor の isolation は「base_branch を分ける」で表現する方が意図がコードから読める

---

## 未決事項

- **`BOID_PARENT_BRANCH` は残すか削除するか**: 一旦「親の BaseBranch」に再定義するが、使用実態を Phase 3 で調べて次 phase で削除判断
- **`base_branch` が declare されていない child** をどう扱うか: 現行は parent の base_branch を継承する挙動 (`api/task_create.go` 参照)。新案でも継続で問題ないはず、要確認
- **v0.0.11 と同時に他の post-cutover 改善もまとめて入れるか**: [[next-session-git-gateway-peer-clone-check]] の §3 (`.boid/` gitignore contract) や §4b (peer basename 衝突 suffix) と同時 release にするかは実装スケジュール次第
