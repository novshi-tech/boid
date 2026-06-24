# task ask の disconnect 耐性 + 回答 DB 永続化

> Status (2026-06-24): **Draft**。 実機検証完了・設計提案段階・未実装。
>
> 関連:
> - `boid task ask` blocking RPC (PR #613 で導入、 session-id resume 経路全廃)。 本 plan が手を入れる対象。
> - `docs/plans/multi-harness-task-hook.md` — codex/opencode を task hook で回す経路。 本問題の影響を直接受ける。
> - `docs/plans/multi-harness-production.md` — 「await→resume は codex/opencode で詰む」 を当時から認識 (`:40-49`)。
> - memory: `supervisor-awaiting-ask-host-action-aborts`。

## Context — 何が壊れているか

`boid task ask "<質問>"` は「ハーネス非依存の blocking Q&A」 として実装されている。 エージェントは foreground で `ANSWER=$(boid task ask "...")` を実行 (`internal/skills/data/boid-task/SKILL.md:119` 系) してブロックし、 broker が接続を握り続けたまま daemon の `AskTaskBlocking` (`internal/api/task_ask.go:34`) が回答を待つ。

設計意図は「**timeout は無い、 cancellation のみ**」 (decision C1, `task_ask.go:30`)。 実際 boid 側は端から端まで timeout ゼロ:

- daemon `http.Server` は timeout 未設定 (`internal/server/server.go:109`)
- broker にも deadline 無し。 blocking ask の間 `watchConnClose` (`internal/sandbox/broker.go:211`) が接続を監視し、 **read error (= 接続 close) のときだけ** `cancel()` する
- `BlockingAskRegistry.Wait` (`internal/api/blocking_ask_registry.go:69`) は `select { 回答チャネル | ctx.Done() }` のみ

問題は2つ:

1. **回答が DB に永続化されない。** 回答は in-memory チャネル (`BlockingAskRegistry.Notify`) でのみ配送される。 エージェントが切断した後に回答が来ても、 届け先が無く `Has(qid)` チェックで弾かれる (`task_ask.go:108`)。 `AwaitingPayload.PendingAnswer` (`internal/orchestrator/awaiting_payload.go:29`) というレガシー永続フィールドは現存し `internal/api/web.go:619` が今も読むが、 blocking RPC はこれをバイパスしている。
2. **接続が切れると即タスク abort。** `Wait` が ctx.Err() を返すと `abortDanglingAsk` (`task_ask.go:175`) がタスクを **aborted** にする。 「裏に生きたエージェントが居ない awaiting が永久に残るのを防ぐため」 (`task_ask.go:86-90`)。

この2つが組み合わさると、 **「ask 中の foreground コマンドが何らかの理由で死ぬ = タスクが死ぬ」** という極めて脆い構造になる。 そして foreground コマンドを殺す主因が、 **ハーネス側の command timeout** である。

## 実機検証結果 (2026-06-24, ホスト実測)

`/tmp/htest.sh` (15 秒ごとに heartbeat、 最大 5 分 = 300 秒) を各ハーネスの shell ツールから実行し、 ハーネスが long-running コマンドをいつ kill するか測定した。

| ハーネス | shell コマンドの既定 timeout | 設定機構 | 5 分ブロックの結果 |
|---|---|---|---|
| **claude-code** | **120 秒** (最大 600 秒) | `BASH_DEFAULT_TIMEOUT_MS` / `BASH_MAX_TIMEOUT_MS` env + 呼出毎 `timeout` param | ✗ 既定で kill (今回の実障害の原因) |
| **opencode** (`opencode run`) | **120 秒** | 呼出毎 timeout param (`shell tool terminated command after exceeding timeout 120000 ms` を実測) | ✗ ちょうど 120 秒で kill (heartbeat 9 行で停止、 `COMPLETED` 無し) |
| **codex** (`codex exec`, gpt-5.5) | 無し / ≥300 秒 | (ノブ未発見) | ✓ 300 秒完走、 exit 0 |

結論:

- **3 ハーネス中 2 つ (claude-code・opencode) が、 2 分超の blocking コマンドを既定で kill する。** 人間/supervisor の回答待ちは普通 2 分を超えるため、 blocking ask は常用ハーネスで構造的に詰む。
- timeout を制御する機構が **ハーネスごとに完全にバラバラ** (claude-code = env、 opencode = 呼出毎 param、 codex = ノブ無し)。 **単一の env ノブで全ハーネスを直すことは不可能。** `BASH_*_TIMEOUT_MS` 注入案は claude-code 専用で、 opencode はそれを読まない (実証済) ・codex には不要。
- 一番厳しいのが主役の claude-code と opencode で奇しくも両方 2 分。 codex だけ寛容。

> 実際の障害例: TUI 撤去 supervisor (task `4d57eda2` / PR #622) が host 作業を依頼する `boid task ask` でブロック → claude-code の 120 秒 Bash timeout で kill → 接続 close → `watchConnClose` → `abortDanglingAsk` → タスク abort。 仕事 (PR) は無傷だったが supervisor が落ちた。

## 設計目標

Q&A が **「エージェントの foreground コマンドが殺されても生き残る」** ようにする。 各ハーネスの command timeout の値・設定機構に一切依存しない (harness 非依存)。 session-resume は復活させない (削除は意図的)。

## 提案 — 核となる2変更 + 付帯

### 変更1: 回答を DB に永続化する

`AnswerTask` / `answerBlocking` (`task_ask.go:103`) が回答を **`AwaitingPayload.PendingAnswer` に書き込む** (レガシーフィールドを revive)。 in-memory `Notify` は「エージェントが今まさに繋がっている時の高速配送」 として残す (fast-path)。 これで回答は durable になり、 切断後に来た回答も拾える。

### 変更2: abort を disconnect から切り離す

`Wait` の ctx-cancel 時に `abortDanglingAsk` で即 abort するのをやめる。 接続が切れても **タスクは awaiting のまま**にする。 エージェントはハーネスループ内では生きている (殺されたのは1つの shell コマンドだけで、 model にはツール timeout エラーが返るだけ) ので、 再 ask / poll で回答を取りに来られる。

### 変更3: `boid task ask` を再 ask に対して冪等・復帰可能にする

同じタスクで `boid task ask "<同じ質問>"` を再度呼んだとき:

- 既に `PendingAnswer` がある → **即座に回答を返し** awaiting を解除 (→ executing)
- awaiting で回答未着 → 既存の質問に **再アタッチして再ブロック** (Wait を継続)

これで「ハーネスがコマンドを kill → model が同じ ask を再実行 → 回答取得 or 再ブロック」 のループが成立する。 各呼び出しはハーネス timeout までは生きるし、 再呼び出しのコストはゼロ。 B1 ガード (`ErrAskPending`, 同一タスクの二重 ask 拒否, `blocking_ask_registry.go:53`) は **「同一タスクの再アタッチは許可、 異なる新規質問の同時実行のみ拒否」** に緩める。

> shim 側 (`internal/sandbox/boid_shim.go` の `parseBoidTaskAsk`) は、 切断で終わった ask に対して **「再 ask せよ」 を示す専用 exit code** を返す。 SKILL.md は「ask がその code で終わったら同じ質問でもう一度叩け」 と指示する。

### 変更4 (新規・必須): 真に死んだ awaiting タスクの回収

変更2で「disconnect で即 abort」 をやめると、 **本当にエージェントが死んだ場合に awaiting が永久に残る**。 現状これを防ぐ手段は **無い**:

- 走行中 daemon の周期 reaper は存在しない (確認済)。
- 起動時の `MarkStaleExecutingTasksAborted` (`internal/dispatcher/store.go:184`, `wire.go:160`) は **daemon 起動時のみ・executing ステータスのみ**で、 awaiting を拾わない。

よって以下を追加する:

- (a) `MarkStaleExecutingTasksAborted` を **awaiting も対象に拡張** (daemon restart 時の回収。 in-memory registry はどのみち restart で全消えるので整合的)。
- (b) **grace 付き abort**: disconnect 時に即 abort せず、 猶予 (config 化、 既定は長め e.g. 30–60 分) 後に「まだ awaiting かつ 再 ask も回答も無い」 ならタスクを abort する deferred check を仕込む。 走行中 daemon でのゾンビ awaiting を bound する。
- 可視性については即 abort 不要: 子タスクは supervisor の監視ループが awaiting を観測、 root タスクは user に通知済。

## 決定事項 / トレードオフ

- **D1 — blocking fast-path を残すか、 純 poll にするか**: 推奨は **blocking + 復帰可能 (変更3)**。 fast-path の即応性を保ちつつ、 切断を無害化する。 純 poll (即 return + ループ) は token を余計に食い、 sleep ループ自体がまた kill されるが、 それが無害になる (再 poll で復帰) のが利点。 両者は同じ土台 (変更1+2) の上の表面形。
- **D2 — ゾンビ awaiting の bound**: 変更4 で startup 回収 + grace abort。 grace 値は config。
- **D3 — 回答受け渡しの latency**: エージェントが繋がっている間は in-memory Notify で即時。 切断後は次の再 ask 時に DB から取得 (最悪 1 ハーネス timeout 分の遅延)。 許容範囲。
- **D4 — qid 整合**: 再 ask 時、 awaiting payload に残る旧 qid と新規 Register の qid をどう突き合わせるか。 案: 回答の取得鍵を qid でなく **taskID** に寄せる、 もしくは再 ask は payload の既存 qid を再利用する。 → Open Question。

## 実装スケッチ (触る想定ファイル)

- `internal/api/task_ask.go` — `AskTaskBlocking` の再アタッチ分岐、 `abortDanglingAsk` → grace 化、 `answerBlocking` で `PendingAnswer` 書き込み
- `internal/api/blocking_ask_registry.go` — B1 ガードの緩和、 再アタッチ API
- `internal/orchestrator/awaiting_payload.go` — `PendingAnswer` の読み書きヘルパ整備 (`ClearPendingAnswer` の対) 
- `internal/sandbox/boid_shim.go` / `internal/server/boid_executor.go` — 「再 ask せよ」 exit code の往復
- `internal/dispatcher/store.go` — `MarkStaleExecutingTasksAborted` を awaiting に拡張 (or 新関数)
- grace abort のスケジューラ (daemon 内 goroutine + timer)
- `internal/skills/data/boid-task/SKILL.md` — 再 ask ループの指示。 opencode 向けに「大きい timeout を渡す」 副次緩和も検討
- config — grace 期間

## Open Questions

- D4 の qid 整合の具体。
- grace abort の既定値と config キー名。
- codex の真の command-timeout 天井 (≥300 秒は確定、 正確値は未測)。 設計には影響しないが参考に追加実測してもよい。
- opencode の呼出毎 timeout を SKILL.md から確実に大きくできるか (model 任せの prompt 工学なので fragile。 構造変更で不要になるのが理想)。
- 純 poll に倒す場合の専用 CLI verb (`boid task ask --poll <qid>` 等) の要否。

## Non-goals

- session-resume (harness `--resume`) の復活。 削除は意図的、 本 plan でも復活させない。
- 各ハーネスの command timeout 自体を変える試み。 制御不能かつ不要。
