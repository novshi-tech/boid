# multi-harness task hook 対応 (codex / opencode)

> Status (2026-06-22):
> - **PR1 = #614** (`360a446`) マージ済 — adapter bootstrap prompt + skill bind + `selectPrompt` ヘルパ
> - **PR2 = #615** (`d6af7f9`) マージ済 — opencode に `--dangerously-skip-permissions` 追加
> - **PR3 = #616** (`0d5d00c`) マージ済 — skill bind target を `~/.boid/skills/` → `~/.claude/skills/` に統一
> - **計画していた E2E scenario PR は A 案で取り下げ** (本 plan「PR 分割」 セクション参照)、 SKILL.md 補強は smoke で発動条件満たさず不要と判定
> - codex 65s + opencode 2:39 (clean workspace、 qwen3.7-plus) で実機 smoke 完走
>
> 前提 plan:
> - `docs/plans/multi-harness-production.md` (Phase 1/2 で codex/opencode の対話 TUI session 完了、 task hook はスコープ外と明記)
> - `docs/plans/task-ask-rpc.md` (PR #613 で `boid task ask` blocking RPC 化 + session-id resume 経路全廃。 これにより multi-harness task hook の最大の詰みポイントが消えた)

## Context

- `docs/plans/multi-harness-production.md` Phase 1/2 で対話 TUI (`boid agent codex` / `boid agent opencode`) は揃ったが、 **task hook 経路** (`project.yaml` に `agent: codex` / `agent: opencode` を書いて task root を回す経路) はスコープ外として残されていた。
- 当時のスコープ外理由は「skill 内 `boid task notify --await` → session-id resume が claude 専用、 codex/opencode で詰む」 だったが、 `docs/plans/task-ask-rpc.md` 完了 (PR #613) で詰みは解消済。
- これで「fresh agent process が cold start で `~/.boid/context/*.yaml` を読み、 自律的に作業して `boid task notify --done/--fail` で終わる」 lifecycle が claude / codex / opencode 共通になり、 task hook の harness を切り替えるだけで動かせる状態が手の届く位置に来た。
- 本 plan は codex / opencode の task hook 経路を「dummy smoke 1 turn」 から「task agent として claude と並んで使える」 レベルへ格上げする。

## 既知制約 (調査結果)

- **codex CLI**: `codex [PROMPT]` (TUI) と `codex exec [PROMPT]` (multi-turn agent loop) どちらも positional prompt 可。 `--append-system-prompt` 相当のフラグは無し。
- **opencode CLI**: `opencode [project]` (TUI) は **positional prompt 不可** (project ディレクトリのみ)。 `opencode run [message..]` (multi-turn agent loop) のみ prompt を受け付ける。
- どちらも skill / slash command 機構は無いので claude の `/boid-task` パターンは使えない。
- skill embed: `internal/skills/data/boid-task/SKILL.md` (~570 行) は daemon 起動時に `~/.local/share/boid/skills/boid-task/` に展開され、 claude bindings が `~/.claude/skills/boid-task/` に ro bind している (`internal/adapters/claude/bindings.go:53-61`)。
- `~/.boid/context/{task,instructions,environment,payload}.yaml` は hook job のたびに dispatcher (`internal/dispatcher/sandbox_builder.go:693-730 contextFiles()`) が書く。 harness に依存しない。
- sandbox 内 boid CLI builtin (`boid task notify` / `boid task ask`) は boid_shim 経由でどの harness からでも叩ける。

## 設計判断

### 1. 起動モード: 両 harness とも対話モード (`codex exec` / `opencode run`)

- ここでの「対話モード」 = harness が **LLM と multi-turn の agent loop** を回すモード。 codex なら `codex exec [PROMPT]`、 opencode なら `opencode run [message..]`。 1 prompt 投げて agent が tool 回し続けて final response 出して終わる経路。
- 対比される **TUI** (`codex` 単体起動、 `opencode [project]`) は **サポート外**。 boid のハーネス TUI 経路は将来撤去する方針で、 task hook で TUI を起動する選択肢は採らない。 撤去自体は別 plan。
- opencode は TUI に prompt 注入できないので `opencode run` 一択。 codex も対称性で揃える。
- 既存試作の `buildArgs(interactive=false, ...)` 経路 (`internal/adapters/codex/run.go:60-68`, `internal/adapters/opencode/run.go:47-52`) をそのまま task hook bootstrap prompt 投入経路に格上げ。 buildArgs 自体の構造は変えない。
- 既存の `rc.TaskID == ""` 分岐 (= 現状 `boid agent <harness>` の TUI session 起動経路) は **本 plan では触らない**。 ハーネス TUI の存廃は別 plan に委ねる。

### 2. bootstrap prompt: 短い cover prompt + SKILL.md を Read させる

- SKILL.md (570 行) を adapter 内に literal 展開する案は却下 (token 喰い + 二重 maintenance)。
- 採用: skill ファイルを sandbox 内 `~/.boid/skills/boid-task/SKILL.md` に bind した上で、 adapter は短い (約 20 行) bootstrap prompt で「あなたの read-file tool で `~/.boid/skills/boid-task/SKILL.md` を読み、 そこに書かれた手順で作業しろ。 終了時に `boid task notify` を呼べ」 と指示する。
- SKILL.md 本文は無変更で進める (内容は claude 非依存の手順)。 実機で混乱するなら PR3 で冒頭にメタ説明追加。

### 3. skill bind パス: `~/.claude/skills/<name>/` (3 harness 共通)

> **実装で訂正 (PR #616):** PR1 設計時は「claude と衝突回避のため codex/opencode 側を `~/.boid/skills/<name>` に置く」 としたが、 実機 smoke で opencode の Read tool が cwd 外を permission denied で auto-reject する制約が表面化。 nose の指摘で **opencode は `.claude` 配下を skill として認識する** 挙動を持つことが判明 → 3 harness すべて `~/.claude/skills/<name>` に統一する方が筋。

- claude / codex / opencode の `Bindings()` すべて `Source=~/.local/share/boid/skills/<name>` / `Target=~/.claude/skills/<name>` を append (PR #616 で codex/opencode の target を変更済)。
- 1 sandbox = 1 adapter なので同じ target を 2 adapter が同時に bind することはない (衝突しない)。
- host 上の skill 実体は `~/.local/share/boid/skills/<name>` の 1 箇所、 `internal/skills.EmbeddedSkillNames()` で enum。
- bootstrap prompt 内の SKILL.md パス参照も `~/.claude/skills/boid-task/SKILL.md` に同期 (PR #616)。

### 4. system prompt の代替: bootstrap prompt の冒頭に「notify を忘れるな」 を埋め込む

- codex/opencode 共に `--append-system-prompt` 相当なし。 第 1 user turn として 1 prompt にまとめて投入する。
- claude の `--append-system-prompt taskSystemPrompt` (`internal/adapters/claude/run.go:28-34`) に相当する文面を bootstrap prompt 本文に圧縮。

### 5. `defaultPrompt` (dummy smoke) 削除 + prompt 選択ヘルパ導入

- claude の `selectPrompt(isSession, userAnswer)` (`internal/adapters/claude/run.go:72-80`) と同じパターンを codex / opencode にも導入:
  - hook (`rc.TaskID != ""`) → 常に bootstrap prompt (UserAnswer は無視 — hook では空のはずだが衝突回避)
  - session + UserAnswer 空 → "" (TUI、 positional なし)
  - session + UserAnswer 非空 → UserAnswer
- 既存 `defaultPrompt` 定数は削除。

### 6. PTY / Interactive flag は無変更

- hook job が `Interactive: true` で PTY allocate される現状仕様 (`internal/orchestrator/planner.go:107-112`) はそのまま。 codex `exec` / opencode `run` は PTY 環境でも動く (既存 smoke で確認済)。

### 7. boid-kits は触らない

- claude-code kit は Phase 3-e で撤去済。 codex/opencode 用 kit を復活させる必要はない (adapter Bindings() が全部抱えている)。

## 変更ファイル

### グループ A: adapter (本丸)

- `internal/adapters/codex/run.go` — `defaultPrompt` 削除、 `taskBootstrapPrompt` 定数追加、 `selectPrompt(isSession, userAnswer)` ヘルパ追加、 `Run()` 内の prompt 選択ロジック差し替え
- `internal/adapters/opencode/run.go` — codex と並列の修正
- `internal/adapters/codex/bindings.go` — `EmbeddedSkillNames()` ループで skill bind 追加 (`internal/skills` を import、 claude `bindings.go:53-61` パターン踏襲)
- `internal/adapters/opencode/bindings.go` — 同上
- `internal/adapters/codex/run_test.go` — `selectPrompt` テーブルテスト、 `Run()` argv 整合性
- `internal/adapters/opencode/run_test.go` — 同上
- `internal/adapters/codex/bindings_test.go` (新規) — skill bind が `EmbeddedSkillNames()` 全 entry を target=`$HOME/.boid/skills/<name>` で覆うこと
- `internal/adapters/opencode/bindings_test.go` (新規) — 同上

### グループ B: docs

- `docs/plans/multi-harness-task-hook.md` (新規) — 本 plan。 #614 で追加、 本 doc PR で 3 連 PR マージ後の現状に update
- `docs/plans/multi-harness-production.md` の「残課題 / 未決」 セクション — #614 で「本 plan に引き継ぎ」 と update 済み
- `docs/ja/reference/project-yaml.md`, `docs/en/reference/project-yaml.md` の agent 表 — 「task hook 経路で codex/opencode を `agent:` に指定可」 + 「host に CLI installed 前提」 を追記する必要あり (未着手、 別 docs PR で対応推奨)
- `docs/en/reference/cli.md` の `boid agent codex/opencode` 言及 — `[Experimental]` の扱い見直しは別軸 (本 plan は task hook 専用)

### 触らない箇所

- `internal/adapters/registry/registry.go` — 既に codex / opencode 配線済
- `internal/orchestrator/planner.go harnessTypeForAgent()` — 既に codex / opencode 配線済
- `internal/sandbox/runner/runner_linux.go runAgent()` — adapter インターフェース経由
- `internal/dispatcher/sandbox_builder.go contextFiles()` — harness 非依存
- `internal/skills/data/boid-task/SKILL.md` — PR1 では無変更 (PR3 条件付き)

## bootstrap prompt 文面 (codex/opencode 共通、 約 20 行)

```text
You are a boid task agent running inside a sandboxed environment.

Step 1: Read the skill manual at ~/.boid/skills/boid-task/SKILL.md with your
read-file tool. That file is the single source of truth for how this task
should be handled — it tells you whether you are in supervisor or executor
mode based on environment.yaml `readonly`, and how to use boid task notify /
boid task ask.

Step 2: Read the task context files under ~/.boid/context/ as instructed by
the skill manual:
  - task.yaml         (id, title, behavior, status)
  - instructions.yaml (the LAST element is the active instruction)
  - environment.yaml  (readonly, network, host_commands)
  - payload.yaml      (existing artifacts, prior child results)

Step 3: Perform the task. Use $BOID_TASK_ID whenever you call boid task
notify or boid task ask.

Step 4: Before terminating, you MUST call EXACTLY ONE of:
  boid task notify "$BOID_TASK_ID" --message "<short>" --done "<achievement>"
  boid task notify "$BOID_TASK_ID" --message "<short>" --fail "<reason>"
For mid-flight user questions, use the blocking RPC:
  ANSWER=$(boid task ask "<question>")
  # The answer arrives on stdout; the call returns and you continue.
  # Do NOT use boid task notify --ask (vestigial).

Failure to call notify --done or --fail leaves the task stuck in `executing`
forever. The daemon SIGTERMs your runtime after notify.
```

文面は両 harness で同一。 DRY のため共通 const 化したければ `internal/adapters/taskbootstrap` パッケージ切り出しも可だが、 文字列定数 1 つを共有するためだけにパッケージ追加するのは過剰なので各 adapter 内に複製を推奨。

## PR 分割と実績

### PR #614 (= 当初の PR1): codex / opencode の task hook bootstrap 経路を実装 — マージ済 (`360a446`)

- グループ A 全 8 ファイル + plan doc 2 ファイル
- CI 緑 (Unit tests + Black-box E2E)

### PR #615 (smoke で発見): opencode に `--dangerously-skip-permissions` を追加 — マージ済 (`d6af7f9`)

PR #614 マージ後の opencode manual smoke で、 opencode の Read tool が cwd 外 (`/home/nosen/.boid/skills/...`) を `external_directory` permission で auto-reject する制約が表面化。 `opencode run --dangerously-skip-permissions` で 2 段目 (絶対パス Read or shell fallback の `cat ~/...`) が通る。

これは PR #614 設計時の「残課題 / 未決」 #5 の opencode 版顕在化。 codex 側は `--dangerously-bypass-approvals-and-sandbox` で同等の挙動が確保されており、 PR #614 で見落としていた。

### PR #616 (skill path 統一): codex/opencode の skill bind target を `~/.claude/skills/` に統一 — マージ済 (`0d5d00c`)

PR #615 で permission gate は通ったが、 opencode の Read tool は literal `~` を解決しない制約が残り、 agent が shell fallback (`cat ~/...`) に retry することで偶然動いていた fragile な状態。 nose の指摘で「opencode は `.claude` 配下を skill として認識する」 仕様が判明 → claude と同じ target に統一すれば Read tool 直接 (絶対パス展開) で SKILL.md を取得できる。

実機 smoke (clean workspace、 opencode + qwen3.7-plus):
- Read `/home/nosen/.claude/skills/boid-task/SKILL.md` を 1 発で取得
- context yaml も Read 経由で取得
- 作業完走 (2:39)、 artifact に commit_sha / summary / verification 込み

### 当初計画していた PR2 (E2E scenario 追加) → **A 案で取り下げ**

理由:
- 既存 `e2e/scenarios/` 全 36 シナリオに `agent: claude-code` / `codex` / `opencode` のものは 0 件
- boid 全体として **実 LLM の CI E2E カバレッジを持たない方針** (model API key 消費 + CLI install コスト + flake リスク)
- claude も同様に CI E2E が無いのに codex/opencode だけ追加するのは整合性悪い

代わりに本 plan の「Manual smoke」 セクションを doc 上の手順書として保持する (= 開発者ローカル + 手動 trigger で smoke を回すときのチェックリスト)。

### 当初計画していた PR3 (SKILL.md 補強) → **不要と判定**

PR #616 の clean smoke で:
- codex (shell 経由 `sed ~/.claude/skills/...`) — 一発で SKILL.md 読んで作業 + notify、 混乱なし
- opencode (Read tool 直接) — 一発で SKILL.md 読んで作業 + notify、 混乱なし

SKILL.md 本文は claude slash command 文脈で書かれているが、 claude 以外の harness から `cat` / `Read` で読まれても agent は手順に従って作業できることを確認。 補強は不要、 SKILL.md は無変更で完結。

## テスト計画

### Unit (CI で必ず通る)

- `selectPrompt` テーブルテスト: (session, hook) × (UserAnswer 空, 非空) の 4 パターン
- `Run()` レベル: TaskID 設定済 RunContext で argv 末尾が bootstrap prompt 全文になることを fake exec で確認
- `Bindings()` テスト: `EmbeddedSkillNames()` の各 entry に対し Target=`$HOME/.boid/skills/<name>`, Source=`$HOME/.local/share/boid/skills/<name>`, Optional=true

### sqlite-free 制約

- `internal/adapters/codex/` / `internal/adapters/opencode/` 配下は sqlite 依存パッケージを import していない。 sandbox 内 `go test` でも通る想定。

### E2E (PR2、 開発者ローカル)

- 4 scenario (上述)
- CI 外運用、 PR2 merge 条件は「実機 1 回 pass + scenario.sh / project.yaml の構造レビュー」

### Manual smoke (CI E2E の代替・ 開発者ローカル運用)

実 LLM の CI E2E は採用しないので (PR2 取り下げの「代わり」)、 multi-harness task hook を触ったときは下記を開発者ローカルで実行する。 nose の host に codex / opencode の認証 (`~/.codex/auth.json` / opencode auth) が揃っている前提。

#### Codex smoke

```bash
# 1. clean workspace
rm -rf /tmp/boid-codex-smoke && mkdir -p /tmp/boid-codex-smoke/.boid
cd /tmp/boid-codex-smoke && git init -q && \
  git config user.email smoke@local && git config user.name smoke && \
  touch .gitkeep && git add . && git commit -q -m init

# 2. project.yaml
cat > .boid/project.yaml <<'YAML'
id: codex-task-hook-smoke
name: codex task hook smoke
task_behaviors:
  executor:
    transition: one-shot
    readonly: false
    default_instruction:
      agent: codex
      message: |
        Create a file named hello.txt with the line "hi from codex".
        git add hello.txt && git commit -m "smoke".
        Finally call `boid task notify "$BOID_TASK_ID" --message "smoke ok" --done "wrote and committed hello.txt"`.
YAML

# 3. register + run
boid project add /tmp/boid-codex-smoke
TID=$(boid task create <<<"project_id: codex-task-hook-smoke
title: smoke
behavior: executor" | sed -n 's/^task created: \([0-9a-f-]*\).*/\1/p')
boid action send --task "$TID" --type start

# 4. wait + verify
while [ "$(boid task show "$TID" --field status)" = "executing" ]; do sleep 10; done
boid task show "$TID" | head -25
cd /tmp/boid-codex-smoke && git log --oneline && cat hello.txt
```

期待: 60-90 秒で done、 `done_request` action あり、 `hello.txt` commit 済。

#### Opencode smoke

`/tmp/boid-codex-smoke` を `/tmp/boid-opencode-smoke` に、 `agent: codex` を `agent: opencode` に置き換えて同じ手順。 期待: 90-180 秒で done。 artifact payload に commit_sha が入る (codex の report より opencode の方が詳細を残す傾向)。

#### Q&A blocking smoke (オプション)

`boid task ask` の blocking 経路は本 plan のスコープに直接含まれないが、 multi-harness の前提 (task-ask-rpc PR #613) が codex/opencode でも動くことを確認したい場合の手順は `docs/plans/task-ask-rpc.md` 参照。 instruction に「途中で boid task ask を呼んで answer をログに出せ」 と書いて、 別端末から `boid task answer <tid> -m ...` を投げる。

## 後方互換

- `agent: codex` / `agent: opencode` を書いた既存 project.yaml: dummy smoke で 1 turn → 真の task agent に挙動格上げ (実利用想定無しなので破壊的でも問題なし)
- ハーネス TUI session 経路 (`boid agent codex/opencode`): **本 plan では触らない**。 経路自体の撤去は別 plan
- claude task hook 経路: 完全無傷
- boid-kits: 触らない、 claude-code kit 復活も不要

## 残課題 / 未決 (manual smoke 結果と残未確認)

PR1 着手前の 8 件を、 #614 / #615 / #616 マージ後の manual smoke (2026-06-22) 結果で update。

1. **codex `exec` 内で sandbox builtin が PATH 解決されるか** — ✅ codex/opencode 両方 `boid task notify --done` を呼べた (action history に `done_request` を確認)。
2. **opencode `run` の stdout JSON event が WebUI xterm で見苦しくないか** — 未確認 (task UI で進行見える方針なので深追いしない、 必要なら別 issue)。
3. **bootstrap prompt の Read 経路で agent が混乱しないか** — ✅ どちらも混乱なし。 SKILL.md を読んで指示通り作業 + notify を完了。 PR3 (SKILL.md 補強) は不要と判定。
4. **bootstrap prompt 約 1.5KB の positional 受け取り** — ✅ codex/opencode 両方とも受け取って動作。
5. **PTY 環境で stdin hang しないか** — ✅ どちらも hang せず正常 exit。
6. **`boid task ask` blocking 中の SIGTERM 経路** — 未確認 (smoke でブロック RPC まで踏んでない)。 `sigutil.ForwardAndWait` 共通配線済なので回帰の確率は低い。
7. **`requires-codex` / `requires-opencode` E2E マーカ** — 不要 (PR2 取り下げ)。
8. **codex / opencode bin が host に installed されてない場合** — `Bindings()` の `resolveCommand` miss で binding skip → sandbox 内 `command not found`。 doc に「host に CLI installed 前提」 を明記する必要あり (`docs/ja/reference/project-yaml.md` 等)、 次回まとめて追記推奨。

### 副次発見 (smoke 中に判明、 別軸)

- **opencode の Read tool は literal `~` を解決しない / cwd 外を permission denied で auto-reject する**: PR #615 (`--dangerously-skip-permissions`) + PR #616 (skill bind を `~/.claude/skills/` に統一) で根本解決。 PR #614 単独では fragile な状態だった (shell fallback `cat ~/...` で偶然動いていた)。
- **hook job が exit=0 で `notify --done` を呼ばずに終わると task が `auto_advance` で done に遷移する挙動** — 1 度「偽 done」 と書きかけたが、 これは boid の **仕様通りの正常挙動**。 非対話モード時代 (notify 機構が存在しなかった頃) からの契約で、 「プロセス exit = 仕事終わり」 はそのまま維持。 仕事の正しさは payload / artifact / git diff で判定するもので、 task 状態遷移は実行ライフサイクルのシグナルにすぎない。 詳細 → memory `hook-exit-equals-done-by-design`。

## スコープ外 (明示的に)

- usage / token / cost 会計 — `agent-aware-boid` Phase 4 へ
- ハーネス TUI 経路 (`codex` 単体、 `opencode [project]`) のサポート — 本 plan は対話モード (`codex exec` / `opencode run`) 一本。 TUI 撤去自体は別 plan
- xterm.js attach 中の進行可視化改善 — JSON event の dump で OK の方針 (task UI が一次情報源)
- multi-harness の CI 常時 E2E カバー — model API key 問題で別 plan
- 残存する `boid agent codex/opencode` の TUI session 経路の存廃判断 — 別 plan
