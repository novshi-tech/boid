# script hook 廃止と inline command 移行計画

ステータス: 実装完了 (2026-07-15、PR #757〜#762 全 landed、PR4 docs 更新は本コミット)
作成日: 2026-07-15
親ドキュメント: [git-gateway-cutover.md](git-gateway-cutover.md) — post-cutover 改善候補 §3 の解決策として

---

## 目的

`.boid/hooks/*.sh` を外部 file として参照する **script hook 機能を撤廃**し、
用途 A (production の task_behaviors + agent 実行) と用途 B (e2e の control-plane
アサーション) それぞれに専用の後継経路を用意する。副作用として git gateway cutover
post-cutover 改善 §3 (`.boid/` gitignore contract 問題) が自然消滅する。

用途 A の後継: 既に存在する **agent-kind hook + `synthesizeAgentHook`**
(`internal/orchestrator/evaluator.go:78-101`)。production dogfood
(`internal/.boid/project.yaml`) と init wizard (`initwizard/default_behaviors.tmpl`)
は既にこの経路のみを使っており、追加実装不要。

用途 B の後継: **`hooks[].command` inline 文字列フィールドの新設**。
33 script hook を全部 project.yaml に埋め込み可能なサイズに畳んで移行する
(docker-proxy 系 16 コピーは 1 fixture に集約するので実質書き換え本数は少ない)。

deprecation warning は入れない。production は既に script hook を使っていないので
破壊性が実質ゼロ (nose 2026-07-15 判断)。

---

## 前提となる決定事項

- **hook = HarnessAdapter 経由の agent/command 実行のみに一本化**
  (nose 2026-07-15、「hook script 自体を機能として削除すべき」)
- **e2e は inline command 新設で移行**
  (nose 2026-07-15、「hooks: に command: 新設が最小差分」を採用)
- **一発切替**: A (e2e 移行) → C (削除) → D (docs 更新) の順で進める。
  Phase B (deprecation warning) はスキップ (nose 2026-07-15)
- **shell adapter そのものは残す**: `boid exec` (task-less な session job) が
  引き続き Argv 経由で使う (`internal/dispatcher/session_job.go:229-242`
  `BuildExecJobSpec`)

---

## 現状の hook 実装 (fact 要約)

前セッション調査結果 (2026-07-15、`.claude/agents/Explore` による very thorough 調査)。
主張の根拠は全て `file:line` 引用。

### hook が JobSpec に落ちるまでの経路

1. `Hook` 型は `internal/orchestrator/spec_types.go:271-280` で定義。
   フィールドは `ID`/`Name`/`Kind`/`Traits`/`Requires`/`Agent`/`Kit`/`ScriptPath`。
   `ScriptPath` は `yaml:"-" json:"-"` の runtime-resolved フィールド。
2. `ScriptPath` の解決は `ReadProjectMeta`
   (`internal/orchestrator/spec_loader.go:120-140`) が behavior loop 中に
   `ResolveHookScript(<projectDir>/.boid/hooks, h.ID)`
   (`internal/orchestrator/spec_resolve.go:9-17`) を呼び、`<hookID>.sh` →
   `<hookID>.py` の順に `os.Stat` した絶対パスを埋める。
3. `DispatchPlanner.PlanHook` (`internal/orchestrator/planner.go:48-128`) が:
   - `event.Hook.ScriptPath == "" && Kind != HandlerKindAgent` を早期エラー
   - `harnessTypeForAgent(event.Hook.Agent)` で HarnessType を決定
     (`"claude-code"→claude` / `"codex"→codex` / `"opencode"→opencode` /
     **その他 (空含む) → "shell"**)
   - `argv = []string{event.Hook.ScriptPath}` を組む (ScriptPath 非空時のみ)
4. sandbox builder (`internal/dispatcher/sandbox_builder.go:373-387`) が
   clone-mode の場合 `argv[0]` を `<projectDir>/.boid/` プレフィックスから
   `sandboxCloneDir(...)+ "/.boid/" + rest` に remap
5. sandbox 内 clone (`internal/sandbox/runner/clone.go:71-133`) は tracked
   file のみ持ってくるので、host repo で `.boid/` が gitignored だと clone
   側に無い
6. shell adapter (`internal/adapters/shell/run.go:51-108`) が
   `exec.CommandContext(ctx, rc.Argv[0], rc.Argv[1:]...)` して fork。
   ファイルが無ければ `cmd.Start()` が ENOENT → `runner_linux.go:470-475` が
   stderr に log → `postJobDone` (`runner_linux.go:212-217`) が exitCode=1
   で JobDone → `internal/api/workflow_job.go:23-27,96` で `JobStatusFailed`
   → task が aborted に遷移

### 二経路の実利用状況

**用途 A: agent-kind hook (virtual synthesize)**

- `HandlerKindAgent` は `spec_types.go:229-236` で定義済み
- `synthesizeAgentHook` は `evaluator.go:78-101` で、behavior が hook を宣言
  しないとき active instruction から virtual agent hook を合成
- production dogfood (`internal/.boid/project.yaml:16-51`) は
  task_behaviors + `default_instruction` のみ、`hooks:` 宣言ゼロ
- init wizard (`initwizard/default_behaviors.tmpl:1-29`) も同様
- **`kind: agent` を YAML で明示宣言してる fixture / production project は 0 件**
  (grep hit なし、doc と constant のみ)

**用途 B: script hook (E2E control-plane)**

- E2E: `find e2e/scenarios -path "*/.boid/hooks/*.sh"` = **33 ファイル / 全 41
  シナリオ中 24 弱**、総 3,207 行
- 内訳:
  - `docker-proxy-*` 16 個: 同一 `run-docker-test.sh` (159 行) のコピー配布
  - `git-gateway-*` 6 個: gateway assertion (git push / ls-remote)
  - control-plane 系 4 個 (`builtin-task-create`, `idempotent-task-create`,
    `task-ask-blocking`, `job-done-explicit`): boid CLI を叩いて task
    lifecycle 進行を検証
  - assertion 系 (`readonly-hook-gate`, `git-peer-isolation`,
    `hook-attach-smoke/slow-hook.sh`): fs assertion, PTY smoke
- サイズ分布: 12〜159 行、中央値 ~30 行
- E2E 全体 41 シナリオ中 **`task_behaviors:` を使う 38 project.yaml のうち
  31 (82%) が `hooks:` 宣言**、`default_instruction` を宣言してる e2e は 0 件
  (実運用の agent は動かさず、control-plane を script で叩いてアサーションだけ
  している)

### dead code

- `HookFile` 型 (`spec_types.go:48-52`): 宣言以外の参照ゼロ
  (`grep -rn "HookFile" internal/` は宣言 1 行のみ)

---

## スコープ

### 削除するもの

| 対象 | 削除理由 |
|---|---|
| `Hook.ScriptPath` フィールド | 外部 script 参照経路の起点 |
| `spec_resolve.go` (`ResolveHookScript`) 全体 | ScriptPath 解決の唯一の call site |
| `spec_loader.go:120-140` の hook resolve loop | ScriptPath 埋め処理 |
| `planner.go:52-54, 82-85` の ScriptPath 分岐 | 未使用化 |
| `sandbox_builder.go:373-387` の `.boid/hooks/` argv remap | 未使用化 |
| `HookFile` 型 (`spec_types.go:48-52`) | 既に dead |
| `hook_survive_test.go` 全体 | ScriptPath 前提のテスト |
| `evaluator_test.go`/`planner_test.go` の ScriptPath 参照テスト | 前提消失 |

### 残すもの

| 対象 | 残す理由 |
|---|---|
| shell adapter (`internal/adapters/shell/run.go`) 本体 | `boid exec` (task-less session job) が使う |
| `Hook` 型 (ScriptPath 除く) | 用途 A/B 両方で残る |
| `HandlerKindAgent` + `synthesizeAgentHook` | 用途 A の後継経路 |
| task_behaviors + `default_instruction` | production の標準経路 |
| `harnessTypeForAgent` | 用途 A で agent-kind hook を harness にルーティング |
| planner の Instruction/Payload/Env 埋め込み | HarnessType 分岐後も共通 |

### 新設するもの

| 対象 | 目的 |
|---|---|
| `Hook.Command` フィールド (yaml: `command`) | inline shell command |
| planner 内の Command → argv 変換 | `argv = []string{"sh", "-c", cmd}` (詳細下記) |
| Hook validation: `Command` と `Agent` は排他 | agent-kind hook が command 持つと HarnessType 曖昧化 |

---

## 新機能: `hooks[].command` inline field 設計

### YAML 仕様

```yaml
task_behaviors:
  test:
    hooks:
      - id: assert-clone-cwd
        command: |
          set -eu
          test -d .git || { echo "not in git repo" >&2; exit 1; }
          echo "assert-clone-cwd ok"
```

`command:` は複数行文字列で受ける。`sh -c` に渡されるので shell の syntax は
全部使える (パイプ、ヒアドキュメント、`set -eu`、`$VAR` 展開)。

### 型定義

```go
type Hook struct {
    // ... existing fields
    Command string `yaml:"command,omitempty" json:"command,omitempty"`
    // ScriptPath field は削除
}
```

### validation ルール

1. `Kind == HandlerKindAgent` かつ `Command != ""` → error
   (agent-kind hook は command を持たない)
2. `Agent != ""` かつ `Command != ""` → error
   (agent hook と command hook は排他)
3. `Kind != HandlerKindAgent && Command == "" && Agent == ""` → error
   (以前は ScriptPath で救われていたケース。移行後は明示的に禁止)
4. Command 長さ制限: とりあえず設けない (project.yaml 側の可読性は運用判断)

### planner の変換 (`planner.go:82-85` の置換案)

```go
var argv []string
switch {
case event.Hook.Command != "":
    argv = []string{"sh", "-c", event.Hook.Command}
case event.Hook.Kind == HandlerKindAgent:
    // agent hook: HarnessAdapter builds its own argv, leave nil
default:
    // validation should have caught this earlier
    return nil, nil, fmt.Errorf("hook %q: no command or agent kind", event.Hook.ID)
}
```

### HarnessType の扱い

- Command hook: `harnessTypeForAgent("")` = `"shell"` (既存挙動、明示的に選択)
- Agent hook: `harnessTypeForAgent(event.Hook.Agent)` (既存挙動)

つまり `harnessTypeForAgent` は変更なし、planner の argv 分岐だけが増える。

### sandbox 側の見え方

- Command は sandbox 内で `sh -c` により shell process として起動
- cwd は sandbox 内 clone (`/workspace/<name>`)
- env は planner が埋め込む `BOID_BASE_BRANCH` 等が有効
- `.boid/hooks/` を参照しない → gitignore contract 問題消滅

### 環境変数のアクセス

現行 script hook が使ってた env (`BOID_BASE_BRANCH`, `BOID_PARENT_BRANCH`,
`BOID_USER_ANSWER`, `BOID_ASK_ID`) は planner が `spec.Env` に載せて
`shell.Adapter.Run` に渡す (`shell/run.go` が `cmd.Env` に merge)。
Command 経由でも同じ env が使える。

---

## E2E 移行方針

### 分類と方針

| カテゴリ | シナリオ数 | 方針 | 想定書き換え量 |
|---|---|---|---|
| `docker-proxy-*` | 16 | 1 fixture に集約 (`e2e/fixtures/docker-proxy-test.sh` 共通 script を用意し、各 hook は数行の `sh -c "SCRIPT=/e2e/fixtures/docker-proxy-test.sh CASE_ID=... source \$SCRIPT"` に畳む) | 1 fixture (159 行) + 16 hook stub |
| `git-gateway-*` | 6 | 各 hook 個別に inline command 化 (シナリオごとに assertion が微妙に違う) | 6 × ~30-50 行 |
| control-plane 系 | 4 | inline command 化。boid CLI 呼び出しは inline でも書ける | 4 × ~30-50 行 |
| assertion 系 | 3 | inline command 化 (`slow-hook.sh` の 12 行は最小、他も 30 行以下) | 3 × ~15-30 行 |
| **合計** | **29** | | fixture 159 + hook stub ~1,200 行 |

`docker-proxy-*` を 1 fixture に集約する経路は要検討 (後述リスク)。

### state-machine 進行の等価性

現行 script hook は `payload_patch.json` を produce することで task の trait
進行 (Artifact/Verification/Awaiting) を駆動している。inline command
経由でも同じ file を書けば同じ trait 進行が起きる — sandbox builder は
payload_patch の受け取り経路 (env `BOID_PAYLOAD_PATCH_PATH` or 標準位置) を
HarnessType や argv に依存せず提供するので、shell adapter に落ちる限り等価。

**確認要**: `payload_patch.json` のパス提供が実際に HarnessType に非依存か、
コードレベルで確認する (実装時に `session_job.go` と runner 経路を再チェック)。

### fixture 集約の代替案

`docker-proxy-*` 16 コピーの集約先候補:

- **案 A**: `e2e/fixtures/` 配下に共通 script を置き、各 scenario の
  hook で `command: |\n  bash /e2e/fixtures/docker-proxy-test.sh` みたいに呼ぶ
  - 問題: sandbox 内で `/e2e/fixtures/` を見せる bind が必要 → 新たな bind
    surface 追加になる
- **案 B**: 共通ロジックを sandbox 内 `.boid/hooks/` に置いてた延長で、
  各 scenario の `.boid/` に script を残す (削除対象から外す)
  - 問題: script hook 削除の主目的 (外部 script 依存の排除) が崩れる
- **案 C**: docker-proxy 系 16 シナリオを、`case_id` パラメタで振る舞い分岐
  する 1 大 command に inline 埋め込み
  - 問題: 159 行の script × 分岐 → project.yaml が肥大化。可読性大幅悪化
- **案 D**: 16 シナリオを 1 大 e2e シナリオに統合
  - 問題: シナリオ独立性が失われる、失敗時の切り分けが辛くなる

**推奨**: **案 A**。sandbox 内で見せる bind surface は `AdditionalBindings`
経由で e2e infrastructure 用に 1 個追加。実装コストは低い。案 A の bind 追加は
「e2e fixtures を read-only で見せる」だけなので workspace level ではなく e2e
run.sh 側の管理でよい。

### 移行順序

1. inline command 新機能を先に merge (PR1)
2. 各 e2e カテゴリを別 PR で移行 (PR2a/2b/2c/2d)、CI で回帰確認
3. 全 e2e が inline command で通ることを確認後、script hook 経路を削除 (PR3)
4. docs 更新 (PR4)

---

## PR 分割案

| PR | 内容 | 前提 | サイズ | Merged |
|---|---|---|---|---|
| **PR1** | `Hook.Command` field 新設、planner の argv 変換分岐、validation、unit test | なし | 小 | [x] #757 (`c7a11a5`, branch `refactor/hook-command-inline`) |
| **PR2a** | e2e `docker-proxy-*` 16 シナリオを fixture 集約 + inline command 化 | PR1 | 中 | [x] #758 (`a5618a0`, branch `refactor/hook-command-e2e-docker-proxy`) |
| **PR2b** | e2e `git-gateway-*` 6 シナリオを inline command 化 | PR1 | 中 | [x] #761 (`7374366`, branch `refactor/hook-command-e2e-git-gateway`) |
| **PR2c** | e2e control-plane 4 シナリオを inline command 化 | PR1 | 小 | [x] #760 (`d476180`, branch `refactor/hook-command-e2e-control-plane`) |
| **PR2d** | e2e assertion 3 シナリオを inline command 化 | PR1 | 小 | [x] #759 (`d6d9e45`, branch `refactor/hook-command-e2e-assertion`) |
| **PR3** | `Hook.ScriptPath` field 削除、`spec_resolve.go` 削除、planner/sandbox_builder の script 分岐削除、`HookFile` 型削除、旧 test 撤去 | PR1〜PR2d | 中 | [x] #762 (`7088f00`, branch `refactor/hook-scriptpath-removal`) |
| **PR4** | `docs/plans/git-gateway-cutover.md` §3 を「解消済み」に更新、CLAUDE.md に「hook は command inline or agent kind のみ」を明記、`docs/ja/reference/project-yaml.md` (無ければ新規) に `hooks[].command` の使い方 | PR3 | 小 | [x] docs 更新完了 (branch `docs/script-hook-removal-completion`、この PR 自体) |

PR2a〜PR2d は並列可能 (実際に順不同で merge された: #758→#759→#760→#761、つまり
PR2a→PR2d→PR2c→PR2b の順)。

---

## リスク

### R1: inline command の shell escape / quoting

YAML の複数行文字列は改行を保持するが、`$VAR` 展開が YAML 側と shell 側で
二重に効くケースがある (`$VAR` を literal で使いたい場面)。対応:

- YAML の block scalar (`|` 記法) を使う限り YAML 側での変数展開は起きない
- shell 側の `$VAR` は planner が渡す env でのみ展開される
- e2e 移行時に quoting 起因の failure が出たら都度対応

### R2: state-machine 進行の等価性

現行 script hook が `payload_patch.json` を書いて task lifecycle を駆動する
経路が、inline command でも等価に動くか。実装時に:

- shell adapter 経由で payload_patch.json path が env に渡されているか確認
- `boid exec` で inline command と等価なことを PoC (シナリオ 1 個で先行実験)

### R3: e2e 移行時のヒドゥン依存

現行 script は cwd や env や host path に暗黙依存してるかもしれない:

- cwd: 現行は `<projectDir>/.boid/hooks/` の絶対 path で exec されてたので
  cwd は sandbox の default (planner が Argv の argv[0] path を絶対で渡すため
  cwd 依存しない)。inline command は `sh -c` の default cwd = sandbox 内 clone
  (`/workspace/<name>`)。この差分で壊れる script があるかもしれない
- env: 上述 R1 参照
- fixture bind (`docker-proxy` case): 案 A の bind surface 追加が e2e infra で
  必要

### R4: 現存 user project の script hook 使用

nose 判断で deprecation warning なしの一発切替。production dogfood と init
wizard は既に非使用だが、outside user (もしいれば) が壊れる可能性はある。

- **緩和策**: release note に明記、`boid start` / `project add` 時に
  「`.boid/hooks/*.sh` を検出したら warn + docs link」を 1 リリース入れる
  だけの中間対応も可能 (PR3 の直前で 1 PR 追加)。ただし nose 判断でスキップ

### R5: `docker-proxy-*` 集約案の副作用

案 A (fixture bind 追加) を採用する場合、e2e run.sh が
`AdditionalBindings` に fixture dir を追加する経路が必要。既存の e2e infra
がどう bind を組んでるか実装時に確認。

### R6: shell dialect (`/bin/sh` = dash) と bash wrap recipe

**発見経緯**: PR2b (`git-gateway-*` inline command 化) の boid-review で判明。

**問題**: sandbox 内の `/bin/sh` は host の `/bin/sh` を rbind mount したもので
(`internal/sandbox/plan.go:52`)、多くの Linux ディストリでは `/bin/sh` が dash に
resolve される。`hooks[].command` は `sh -c <command>` として実行される
(`internal/orchestrator/planner.go` の `PlanHook`) ため、実体は dash 上での実行になる。
dash は POSIX sh のサブセットで、bash 拡張構文を **reject** する:

- `set -o pipefail` → dash は `set: Illegal option -o pipefail` で即座に non-zero
  exit する (`set -eu` 単体は POSIX 準拠なので dash でも動く)
- `[[ ... ]]` (bash 条件式) → dash 未対応
- 配列 (`arr=(...)`) など他の bash 拡張も同様に非対応

bash 前提で書いた script をそのまま `command:` に貼ると、サイレントに壊れるのではなく
即座に non-zero exit で fail する (fail-safe ではあるが原因が分かりにくく、
はまりやすい)。

**対処**: bash 依存の構文 (`pipefail`、パイプラインの失敗伝播、`[[ ]]` 等) が必要な
hook は、body を `bash <<'HEREDOC'` で wrap する:

```yaml
hooks:
  - id: my-hook
    command: |
      bash <<'BOID_HOOK_SCRIPT'
      set -euo pipefail
      # bash 依存の body はここに書く
      some_cmd | grep pattern
      BOID_HOOK_SCRIPT
```

ヒアドキュメント終端子は **シングルクォート** (`<<'BOID_HOOK_SCRIPT'`) にすること —
dash が body を byte-for-byte で bash の stdin に渡し、`$VAR` / `` ` `` の展開は
bash 側が一度だけ行う (クォート無しだと dash が先に展開してしまい二重展開になる)。

**実例**: `e2e/scenarios/git-gateway-push-smoke/workspace/app/.boid/project.yaml`
の `gateway-push` hook (PR2b で全 6 シナリオに同種の wrap + header comment を適用済み)。
単純な `set -eu` のみで pipe を使わない hook (control-plane / assertion 系) では
wrap は不要 — `set -eu` は POSIX 準拠なので dash でもそのまま動く。

---

## 完了判定

1. [x] PR1〜PR3 は main 済み (#757〜#762)。PR4 (本 docs 更新) は branch
   `docs/script-hook-removal-completion` で作業完了、レビュー待ち
2. [x] `find e2e/scenarios -path "*/.boid/hooks/*.sh"` = 0 件 (2026-07-15 確認)
3. [x] `grep -rn "ScriptPath" internal/` = 0 hit (2026-07-15 確認、`git grep` でも同様)
4. [x] `.boid/` を gitignore してる project でも hook dispatch が silent break しない
   — script hook 経路 (`.boid/hooks/*.sh` を argv に組んで exec する分岐) が
   PR3 で完全に削除されたため、`.boid/hooks/` を参照するコードパス自体が
   もう存在しない。残る hook 経路は `hooks[].command` (inline 文字列、
   project.yaml 自体に埋め込み) と `kind: agent` (virtual hook 合成) のみで、
   どちらも `.boid/` 配下の別ファイルを sandbox 内 clone から探しに行かない
   ため、gitignore 設定に関わらず dispatch が壊れようがない
5. [x] `docs/plans/git-gateway-cutover.md` §3 が解消済み扱いに更新 (本 PR)

---

## 関連 memory / doc

- [container-git-gateway-design](../../../.claude/projects/-home-nosen-src-github-com-novshi-tech-boid/memory/container-git-gateway-design.md) — cutover 全体像
- `docs/plans/git-gateway-cutover.md` — §3 の元記述 (post-cutover 改善)
- `docs/plans/agent-aware-boid.md` — Phase 3-d/3-e の HarnessAdapter 化背景
- 前セッション調査 (2026-07-15): script hook 実利用実態と削除実現可能性

---

## 次セッションでの最初の一手 (historical、全完了済み)

以下は着手前 (2026-07-15 計画確定時点) に書いた手順。PR1〜PR4 全て完了したため
今後のアクションは無い。実施記録として残す:

1. ~~plan doc (この文書) を nose に確認 → OK なら PR1 (inline command) から着手~~ 完了
2. ~~PR1 実装前に PoC: 現存 e2e シナリオ 1 個 (`hook-attach-smoke` あたり)
   を inline command に手で書き換え、payload_patch.json 経路が等価に動くか実験~~ 完了
3. ~~PoC 成功なら PR1 → PR2a/b/c/d → PR3 → PR4 で進める~~ 完了 (この順で #757〜#762 が landed)

## レビュー運用

各 PR の実装が完了したら、**新規サブエージェント経由で PR レビューを回す**
(nose 指示 2026-07-15)。少なくとも `/boid-review` (wiring / claim /
test 観点、`.claude/skills/boid-review/SKILL.md`) と `/code-review` の 2 種を
かける。特に注目すべき points:

- PR1: `Hook.Command` を追加する diff が spec_types / planner / adapter 呼び出しの
  三点全てに揃って入っているか (wiring 片落ち検出は boid-review の得意分野)
- PR2a〜PR2d: e2e 移行前後で task lifecycle 進行 (payload_patch → trait) が
  等価に動く証拠 (シナリオごとに payload_patch が期待通り書き込まれる assertion
  が残っているか)
- PR3: 削除された経路が dead reference を残していないか
  (`grep -rn "ScriptPath"` などで拾える範囲)、旧 test の撤去漏れがないか
