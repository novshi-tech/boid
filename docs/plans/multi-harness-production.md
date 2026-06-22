# マルチハーネス本番化: codex / opencode の対話セッション対応

> Status:
> - **Phase 0 (resume 実現可能性評価 + 対話/非対話モード調査)**: ✅ 2026-06-20 完了
> - **Phase 1 (codex)**: ✅ 2026-06-20 実装完了 (codex adapter 復活 + 対話モード TUI 起動)
> - **Phase 2 (opencode)**: ✅ 2026-06-20 実装完了 (opencode 対話モード TUI 起動)
>
> 前提 plan: `docs/plans/agent-aware-boid.md` (Phase 3-c で codex / opencode を
> 試作した、 Phase 3-e でうち codex は一旦撤去した)。

## 背景と動機

- agent-aware-boid Phase 3-c の codex / opencode adapter は試作で、 `codex exec` /
  `opencode run` を **非対話モード** + `defaultPrompt = "boid Phase 3-c smoke test:
  respond with one short line then exit."` で叩く実装になっている
- このため `boid agent codex -p <project>` で起動しても、 user は dummy prompt の応答が
  1 行返って終わるだけ (= 対話セッションとして全く成立していない)
- Phase 3-e (PR #604) で codex は一旦撤去した
- 本 plan のゴールは codex を復活させ、 opencode と並べて **「`boid agent` 対話セッション
  として使える」** レベルに上げる

## スコープ

**入る (= 「普通に使える」 の最小定義、 Phase 1 / Phase 2 共通)**:

- `boid agent <harness>` で **対話モード TUI が起動** する
  - 現状 `boid agent codex -p <project>` は非対話モード + dummy prompt で 1 turn 終わる
  - 修正後は `codex` (TUI、 sub-command なし) を sandbox 内で起動し、 user の terminal
    に PTY 直結
  - WebUI の xterm.js attach 経路でも対話可能
  - opencode 同等 (`opencode [project]` で TUI 起動)
- sandbox 内 builtin (`boid task create` / `git` / `fetch` / docker proxy 等) が
  対話セッション中に呼び出せる
  - Phase 3-a で確立した boid_shim 経路に乗っているため、 harness が何であれ動く想定
  - Phase 1-A で 1 件だけ実機確認

**入らない (= 明示的にスコープ外)**:

- **task hook 経路** (project.yaml の `agent: codex` で task root を回す)
  - 同じスキル (`/boid-task` 等) を共有すると、 skill 内の `boid task notify --await`
    で agent が終了し、 codex/opencode は resume できず詰む
  - 詰みを回避するには (a) skill 側で agent capability 判定、 (b) codex/opencode 専用の
    簡略 skill、 (c) resume を fragile 実装で取り込む、 などの選択肢があるが、 いずれも
    本 plan のスコープ外
  - 別 plan で「task ask ブロック式」 等の構造変更と合わせて再設計する
- **session 永続化 (resume)**: 対話モードでは JSON event 経路が使えず ~/.codex/sessions
  の最新 filename 拾い等の fragile fallback が必要。 対話セッションは user が直接対話する
  ので「途中で切れたら継続したい」 要求はあるが、 当面は新規セッションだけ対応
- **Q&A pause** (`boid task notify --await` → SIGUSR1 → resume): 対話セッションでは ask
  は本質的に不要 (user が直接 agent と対話している) のでスコープに含まれない
- **payload_patch.json 書き戻し**: 対話セッションでは業務 payload を boid に返す概念が
  ない (session 終了は user の意思)。 read 経路も不要
- **usage / token / cost 会計**: agent-aware-boid Phase 4 に押し下げ済。 ただし Phase 0
  実機検証で codex / opencode の usage が JSON event から取れることは確認済 (Phase 4
  着手時の作業を軽くする情報として残す)

## Phase 0 結果

### 対話 / 非対話モード調査

実機 (host) で codex / opencode を起動 / 1 turn ずつ実行して挙動を確認:

| | 対話モード (TUI) | 非対話モード |
|---|---|---|
| codex | `codex` (no sub-command、 TUI 起動) | `codex exec [--json] <prompt>` |
| opencode | `opencode [project]` (TUI 起動、 default サブコマンド) | `opencode run [--format json] <prompt>` |
| user 体験 | 進行が見える (xterm 上で TUI render) | 進行不可視 (JSON が流れるだけ or 完了まで待つ) |
| 本 plan での扱い | **対象**: `boid agent` の起動経路に統一 | スコープ外 |

### resume の実現可能性 (情報として残す)

非対話モード経由なら session ID 捕捉 + resume は実装可能。 ただし対話モードでは TUI が
stdout を占有するため、 ~/.codex/sessions / opencode.db のような状態ファイル経由 fallback に
なる (= fragile)。

| 項目 | codex (非対話) | opencode (非対話) |
|---|---|---|
| session ID event | first event `thread.started.thread_id` (UUID v7 風) | 各 event の `sessionID` (例 `ses_<base62>`) |
| resume CLI | `codex exec resume <id> "<prompt>"` (`--last` で最新も可) | `opencode run -s <id> --continue "<prompt>"` |
| usage event (Phase 4 用) | `turn.completed.usage` | `step_finish.tokens` + `cost` (float) |
| 状態保存先 | `~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl` | `~/.local/share/opencode/opencode.db` (`opencode session list` で一覧) |

codex resume は cache hit (`cached_input_tokens: 16640`) で履歴維持を実機確認、 opencode resume
は CLI フラグ存在のみ確認。 これらは将来 task hook 経路を扱う別 plan で活用する。

## 現状ギャップ (実装の所在)

調査日: 2026-06-20。 ファイル / 行番号は当該時点のもの。

### claude adapter (見本の所在)

| 機能 | 実装場所 |
|---|---|
| 対話モード起動 (claude TUI) | `internal/adapters/claude/run.go:147-168` `buildClaudeArgs()` (現状 claude は常に interactive) |
| signal 中継 / 143 → 0 | `internal/adapters/sigutil/sigutil.go:40-77` `ForwardAndWait()` |
| Bindings (claude bin / state) | `internal/adapters/claude/bindings.go:24-63` |

### codex adapter (削除前 = Phase 1 復活時の出発点)

`git show c97762d^:internal/adapters/codex/` 参照。 試作のため:

- 起動経路は常に **`codex exec <prompt>`** (非対話) で固定、 対話モードに切り替える経路なし
- `defaultPrompt` で「smoke test の dummy 文字列」 を投げており、 user の入力を受け取らない

実装済 (= revert で取り戻す):
- signal 中継 / Setsid / 143 → 0 (sigutil 共通化済)

### opencode adapter (現状)

codex 削除前と同じ非対話モード固定 + dummy prompt の構造。 違いは CLI フラグ:
- 起動: `opencode run [--format json] <prompt>`
- 対話モードは `opencode [project]` だが現状未配線

## 設計判断

### 1. RunContext から起動モードを決める

adapter の `Run()` 内で「対話 / 非対話」 を判断する基準を 1 箇所に絞る。 既存の
`RunContext.Instruction` フィールドがこれに使える:

- **Instruction が空** = `boid agent <harness>` セッション起動 → **対話モード** (TUI 起動)
- **Instruction が非空** = task hook 経路 → 本 plan ではスコープ外。 現状の `codex exec`
  / `opencode run` でも実行はできるが「動くだけ、 await で詰む可能性あり」 と明示する

Phase 1 では Instruction == "" を対話 TUI に分岐させる。 Instruction != "" は試作実装 (非対話
+ argv) のまま残し、 README に「task hook 経路は別 plan で本格対応」と注記する。

### 2. codex 復活は手動コピー (PR #604 の partial revert)

PR #604 を full revert すると claude-code kit 撤去 + docs 多数修正まで戻ってしまう。
復活範囲は限定的に:

- `internal/adapters/codex/` ディレクトリだけ削除前コードから手動コピー (出発点)
- `internal/sandbox/types.go HarnessCodex` 定数復活
- `internal/adapters/registry/registry.go` の codex case 復活
- `internal/orchestrator/planner.go harnessTypeForAgent` の `case "codex":` 復活
- `internal/api/session.go validateHarnessType` / `internal/api/store.go` /
  `internal/dispatcher/session_job.go` / `cmd/agent_session.go` の codex 選択肢復活
- `web/templates/sessions.templ` dropdown に option 復活 + regen
- 関連 docs (cli / project-yaml / concepts / web-ui / sandbox-internals) の codex 言及復活

復活 commit を 1 件に固め、 直後の commit で対話モード TUI 起動を載せる。 PR は 1 本だが
レビューは差分上 2 段階で読める。

### 3. builtin 経路は adapter 固有作業なし

sandbox 内 `boid task create` / `git` / `fetch` 等の builtin は boid_shim が broker socket
経由で host に dispatch する設計 (agent-aware-boid Phase 3-a で確立済)。 これは harness が
何であれ sandbox 内で builtin を呼べば動く。

**Phase 1-A の動作確認のみ実施**: codex 対話セッション中に user が「`git --version` 実行
してくれ」 や「`boid task get <id>` してくれ」 と頼んでみる、 軽い builtin 1 つで経路が通って
いることだけ確認する。 通っていなければ別途調査 (おそらく PATH / binding 問題)。

## 段階的着手案

各 phase は独立 PR。

### Phase 0: 実機検証 + 設計確定 [✅ 完了 2026-06-20]

- codex / opencode を host で 1 turn ずつ実機起動、 JSON event の形式を確認
- 対話モード / 非対話モードのトレードオフを整理 (上記)
- task hook 経路を本 plan スコープ外と決定 (詰み回避のため)
- await→resume を本 plan ではスコープ外と決定

### Phase 1: codex 復活 + 対話モード TUI 起動

1. **commit A**: codex adapter を Phase 3-c 時点のコードで復活 (`internal/adapters/codex/` +
   配線一式、 PR #604 の partial revert)
2. **commit B**: `Run()` の中で `rc.Instruction == ""` 判定 → 対話モードなら `codex`
   (no sub-command) を起動。 PTY はすでに dispatcher が allocate しているので
   `cmd.Stdin / Stdout / Stderr` を渡せばそのまま動く想定 (実装時に確認)
3. **commit C**: 単体テスト追加 (`buildArgs` の対話 / 非対話分岐)
4. **commit D**: docs (cli / project-yaml / concepts / web-ui) に codex 復活を反映、
   `docs/plans/agent-aware-boid.md` の Phase 3-e セクションに「Phase 4 と並行で再採用」と
   追記

**完了基準**:
- `boid agent codex -p <project>` で TUI が立ち上がり、 user が対話セッションできる
- WebUI xterm.js attach 経路でも対話可能
- sandbox 内から builtin (`git --version` 等) が user 指示で呼べる (= 経路が通っていること
  だけ確認、 子 task 作成までは目指さない)

### Phase 2: opencode 同等パリティ

Phase 1 で codex 用に確立したパターンを opencode に移植。 CLI フラグ差分:

- 対話モード: `opencode [project]` (TUI、 default サブコマンド、 project は positional)
- 非対話モード (試作のまま残置): `opencode run --format json <prompt>`

完了基準は Phase 1 と同じ (codex → opencode 読み替え)。

### Phase 3 (オプション): 共通化 refactor

対話モード起動 / signal 中継 / Setsid 設定で claude / codex / opencode の重複が目立つ
ようなら、 内部 helper として共通化を検討する。 重複がきれいな抽象に落ちなさそうなら
やらない。

## 互換性方針

- `agent: codex` を書いた既存 project.yaml: Phase 3-e で一時的に壊れていたが Phase 1
  完了で復活する (task hook 経路は「動くが await で詰む可能性あり、 別 plan で本格対応」
  と明示)
- `agent: opencode` を書いた既存 project.yaml: 既存挙動 (1 turn のみ dummy prompt 起動)
  は Phase 2 完了で「`boid agent opencode` で対話モード起動」 に格上げ
- claude adapter は本 plan で変更しない (見本のまま)

## 残課題 / 未決

- **[Phase 1]** codex 対話モード起動時、 `codex` TUI が PTY をどう要求するか (boid 側で
  すでに PTY allocate 済だが、 `codex` 内部で `isatty()` チェック等があるかは実装時に確認)
- **[Phase 1]** codex 対話セッションで `--dangerously-bypass-approvals-and-sandbox` 相当の
  指定が必要か (sandbox 内ですでに boid-side sandbox があるが、 codex 自身も confirm UI を
  挟むので user 体験のためには bypass 推奨)
- **[Phase 1]** sandbox 内 binding 一式が claude pattern と同じで足りるか (codex は
  `~/.codex/auth.json` 等の認証情報を read する、 `bindings.go` で binding 追加が必要)
- **[Phase 2]** opencode 対話モードの TUI 起動は `opencode [project]` だが、 project 引数を
  どう渡すか (`rc.Workspace` を positional に詰める)
- **[Phase 2]** opencode の binding (`~/.local/share/opencode/`、 `~/.config/opencode/`、
  `~/.local/share/opencode/auth.json` 等) を確定
- **[引き継ぎ]** task hook 経路で codex/opencode を使う計画 →
  `docs/plans/multi-harness-task-hook.md` (2026-06-22 Draft) で対応。 `task-ask-rpc.md`
  完了 (PR #613) で session-id resume の詰みが消えたので、 当時想定していた skill
  capability 判定 / resume 実装は不要になり、 adapter の bootstrap prompt と
  skill bind だけで対応できる見込み
- **[別 plan]** multi-harness の常時 CI E2E カバー
- **[別 plan]** agent-aware-boid Phase 4 (usage / token 会計) と本 plan の Phase 1 を
  並行で進められるかの依存関係整理 (現状は独立)
