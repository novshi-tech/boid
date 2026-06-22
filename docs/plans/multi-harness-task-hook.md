# multi-harness task hook 対応 (codex / opencode)

> Status: Draft (2026-06-22)
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

### 3. skill bind パス: `~/.boid/skills/<name>/`

- claude 既存 binding (`~/.claude/skills/<name>`) は触らず維持 (claude CLI の auto-discover を壊さない)。
- codex / opencode の `Bindings()` に **追加で** `Source=~/.local/share/boid/skills/<name>` / `Target=~/.boid/skills/<name>` を append。
- host 上の skill 実体は 1 箇所のまま、 sandbox 内 target だけ harness 別。 `internal/skills.EmbeddedSkillNames()` を共有して enum。

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

- `docs/plans/multi-harness-task-hook.md` (新規) — 本 plan
- `docs/plans/multi-harness-production.md` の「残課題 / 未決」 セクション — task hook を「本 plan に引き継ぎ」 と update
- `docs/ja/reference/project-yaml.md`, `docs/en/reference/project-yaml.md` の agent 表 — task hook 経路が正規対応した旨を追記 (PR2 マージ後)
- `docs/en/reference/cli.md` の `boid agent codex/opencode` 言及 — `[Experimental]` の扱いを実機検証次第で見直し (PR2 で判断)

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

## PR 分割

### PR 1: codex / opencode の task hook bootstrap 経路を実装 (boid 本体のみ)

- グループ A 全 8 ファイル + グループ B の plan doc 2 ファイル
- CI で機械的に検証可能な部分はここで完結

### PR 2: E2E scenario 追加 (boid 本体、 PR1 マージ後 + 実機検証で書く)

- `e2e/scenarios/codex-task-hook/` (executor、 1 ファイル作成 + commit + notify --done)
- `e2e/scenarios/opencode-task-hook/` (同等)
- `e2e/scenarios/codex-task-hook-readonly/` (supervisor、 子 task 1 件 + notify --done)
- (オプション) `e2e/scenarios/codex-task-hook-ask/` (`boid task ask` の codex 経由動作)
- `requires-codex` / `requires-opencode` マーカを `e2e/runner/` に追加 (既存 `requires-sandbox` の skip 機構を踏襲、 host に CLI 無い CI では skip)
- CI gate には乗せない (model API key 消費 + CLI install コスト)、 開発者ローカル + 手動 trigger で動かす

### PR 3 (条件付き): SKILL.md 補強

- PR2 実機検証で codex/opencode が SKILL.md Read 後に混乱する箇所が見つかれば、 冒頭にメタ説明 (「Read tool 経路で読まれる前提、 claude 以外でも同じ手順」) を 5-10 行追加。 不要なら skip。

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

### Manual smoke (PR1 マージ前)

実機 boid daemon 上で:

1. minimal project (`agent: codex`) で `boid task create` + start → done 遷移
2. `boid task ask` blocking が codex sandbox から動くか (`boid task answer` で release → 続行)
3. `~/.boid/skills/boid-task/SKILL.md` が sandbox 内で codex の Read tool で実際に読まれるか

## 後方互換

- `agent: codex` / `agent: opencode` を書いた既存 project.yaml: dummy smoke で 1 turn → 真の task agent に挙動格上げ (実利用想定無しなので破壊的でも問題なし)
- ハーネス TUI session 経路 (`boid agent codex/opencode`): **本 plan では触らない**。 経路自体の撤去は別 plan
- claude task hook 経路: 完全無傷
- boid-kits: 触らない、 claude-code kit 復活も不要

## 残課題 / 未決 (PR1 manual smoke + PR2 E2E で確認)

1. **codex `exec` 内で sandbox builtin が PATH 解決されるか** — boid_shim は sandbox PATH に注入されるが、 codex CLI の child shell が継承するかは要確認
2. **opencode `run` の stdout JSON event が WebUI xterm で見苦しくないか** — task 進行は task UI 側で見れば良いが、 xterm attach UX は劣化する。 `--format text` 相当があれば検討
3. **bootstrap prompt の Read 経路で agent が混乱しないか** — SKILL.md は claude slash command 文脈で書かれているため、 codex/opencode が困惑するなら PR3 で補強
4. **bootstrap prompt 約 1.5KB の positional 受け取り** — codex/opencode 共に問題ないはずだが実機で確認 (`getconf ARG_MAX` 余裕)
5. **PTY 環境で codex/opencode が stdin を user 入力と誤認して hang しないか** — 対話モードは positional 完結で stdin 読まないはずだが要確認
6. **`boid task ask` blocking 中の SIGTERM 経路で codex/opencode の child が正しく死ぬか** — `sigutil.ForwardAndWait` 共通配線済だが実機確認
7. **`requires-codex` / `requires-opencode` E2E マーカ実装** — `e2e/runner/` の `requires-sandbox` パターン踏襲、 PR2 着手時に確認
8. **codex / opencode bin が host に installed されてない場合** — `internal/adapters/codex/bindings.go` で `resolveCommand` が miss すると binding skip、 sandbox 内 `command not found`。 doc に「host に CLI installed 前提」 を明記

## スコープ外 (明示的に)

- usage / token / cost 会計 — `agent-aware-boid` Phase 4 へ
- ハーネス TUI 経路 (`codex` 単体、 `opencode [project]`) のサポート — 本 plan は対話モード (`codex exec` / `opencode run`) 一本。 TUI 撤去自体は別 plan
- xterm.js attach 中の進行可視化改善 — JSON event の dump で OK の方針 (task UI が一次情報源)
- multi-harness の CI 常時 E2E カバー — model API key 問題で別 plan
- 残存する `boid agent codex/opencode` の TUI session 経路の存廃判断 — 別 plan
