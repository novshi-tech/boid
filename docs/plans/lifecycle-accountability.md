# Lifecycle Accountability Model

## 背景

2026-05-15 task `316c6611` で次の事象が発覚した: agent が画面に質問文を書いて
`boid task notify --ask` を呼ばずに `boid agent stop` してしまい、 user に届かず
黙って task が `done` に遷移した ([[project_boid_weak_points]] #12)。

この個別事象の根本原因は、 boid の lifecycle に存在する **構造的非対称性** にある:

- 質問 (`ask`) は子から親 (user) に上向きで明示的に送る経路がある
- しかし完了 (`done`) は子が自己判定で確定する。 親への確認経路が無い

Unix の SIGCHLD / wait(2) では子の死は必ず親が reap する。 同じ対称性を boid の
lifecycle にも持ち込めば、 同型 bug を構造的に消せる + 「user は進行中セッションを
気にしなくていい」 という boid のコンセプトとも整合する。

関連:

- [[project_boid_weak_points]] #12 — root incident
- [[project_lifecycle_accountability_model]] — 設計合意の議事 memo
- [[project_resume_skip_instructions]] — resume 時に context が消える別系統症状
- [[project_boid_task_purpose]] — boid task は人間用 (横断把握 / 遅延実行)

## 設計目標

1. **対称性**: 子の terminate / question は **必ず親 (= 直近の owner / 最終的に
   user) が judge する**
2. **既存 primitive の再利用**: 新 action type / state machine 拡張は最小限。
   `notify --ask` を上向き通信の universal な単一 primitive にする
3. **owner を特別扱いしない**: escalate は再帰的な `notify --ask` で行い、
   「owner 専用 action」 は作らない (root user = parent_id 無しの top に到達するまで
   chain を遡るだけ)
4. **段階移行**: 既存 task / kit / project を壊さず、 opt-in から段階的に
   必須化する

## 提案モデル

### 上向きイベント (子 → 親)

すべて既存の `notify --ask` 1 つで表現する。 メッセージ本文の先頭 **prefix** で
イベント種別を識別する:

| prefix | 概念上のイベント | 意味 |
|---|---|---|
| `done_request:` | 完了通知 | 「完了したと思うので確認してください」 |
| `failure_report:` | 異常通知 | 異常 / 自発的 abort 要求 |
| (prefix なし) | 通常 ask | 質問 / 判断要求 |

例:

```
done_request: PR #123 を作成、 CI green を確認しました
failure_report: sandbox 内で go test が失敗、 未解決です
次の step の優先度を教えてください
```

親 supervisor は prefix で event 種別を分岐し、 confirm_done / reopen / answer 等の
判断ロジックを選ぶ。

### 下向きアクション (親 → 子)

すべて既存 primitive で表現する:

| 親の判断 | コマンド |
|---|---|
| ask への回答 | `boid task answer <child_id> <reply>` |
| done_request の承認 (= terminate `done`) | `boid action send --type done <child_id>` |
| done_request の却下 / 再実行 | `boid task reopen <child_id> -m "..."` |
| 強制終了 | `boid task abort <child_id>` |
| 自分も判断不能 → 上位に振る | `boid task notify $BOID_TASK_ID --ask "..."` |

「自分も判断不能」 は新 action ではなく、 親が **自分の親に対して notify --ask** を
発行するだけ。 owner を特別扱いしないので chain は再帰的に成立し、 最終的に
parent_id 無しの root に到達して user に届く。

### State machine

**変更なし**。 既存の `pending / executing / awaiting / done / aborted` で全ケースを
カバーできる。

- 子 ask 発行 → 子 task は既存どおり `awaiting` に遷移
- 親 supervisor は polling で child 状態を見て、 自分の文脈で answer / reopen /
  abort / 上向き notify を選ぶ
- escalate を再帰の notify で表現するため、 「owner 待ち」 専用 status / 新しい
  awaiting trait kind 等は不要

## 子 → 親 ルーティング (skill-level polling)

子 → 親の通信は、 **既存の supervisor monitoring loop の polling 拡張** で実現する。
daemon 側拡張は本フェーズの対象外。

### 動作仕様

1. 子 (supervisor / executor) が 「親に確認したい」 場合、
   `boid task notify $BOID_TASK_ID --ask "..."` を発行 → 既存挙動どおり子 task は
   `awaiting` に遷移
2. 親 supervisor の monitoring loop (現状 sleep 60s 周期、 boid-task skill の
   Supervisor Mode 参照) で:
   - `boid task list --parent $BOID_TASK_ID --status awaiting` で awaiting な子を
     列挙
   - 子の awaiting 内容を `boid task show <child> --field awaiting` で読む
   - 自分の文脈で **answer / reopen / abort / 上向き notify** を判断
3. 親 supervisor 自身が awaiting 中の場合は polling が止まっているので、 子 ask は
   親 resume 後に拾われる (X1 採用: payload キュー無しで polling-only)
4. parent chain の root (= parent_id 無し) のみ user 通知 (web UI / push) が発火

latency = `poll_interval × parent chain の階層数`。 階層は通常 1-2 で許容範囲、
必要があれば poll interval を縮めて緩和できる。

### user 通知の発火条件 (daemon hardcode)

`notify --ask` 自体は task を `awaiting` にする操作で、 user への通知 (push,
web UI, 外部 webhook 等) は notify hook の発火で行われる。 本モデルでは notify hook
の発火条件を **daemon が hardcode** で実装する:

- **parent_id 無し (root task) の notify --ask**: user 通知 hook を発火
- **parent_id あり (子 task) の notify --ask**: user 通知 hook は **発火させない**

これは routing の **hard 制約** であり、 project.yaml の hook expression DSL には
逃さない。 project 作者が条件を書き忘れて user に誤 escalate する可能性を排除する
ためである。

**含意**: 子タスクの ask に対しては基本的に **親 supervisor が answer しなければならない**。
これは skill 設計上の hard constraint として Phase 1 / Phase 2 で明記する。

### 異常子の検知と対処 (supervisor の責務)

monitoring loop で以下のケースを異常として検知する:

- **stuck executor**: `executing` 状態のまま長時間 (例: 10 分) active な job が
  無い子。 `boid task show <child> --field status` と `boid job list --task <child>`
  で最終 job の更新時刻を確認する
- **未報告 done_request**: 子が `done_request` を発行したが
  `payload.artifact.report` が空 / fields 不足の場合

対処:

- `boid task reopen <child> -m "<状況確認の指示>"` で子を立て直す
- または `boid task abort <child>` で強制終了
- または `boid task notify $BOID_TASK_ID --ask "..."` で上位に escalate

これにより 「agent が画面に質問書いて消えた」 「異常終了で放置」 のパターンは
supervisor が catch する。

### なぜ daemon push を採らないか

「子の ask 発行時に daemon が親 task の runtime に SIGUSR1 を送って起こす」
あるいは 「親 task の payload に pending_child_asks を積む」 等の daemon-mediated
push を検討したが採用しない。 理由:

- daemon が親 runtime に SIGUSR1 を送ると、 hook (`run-agent.py`) は claude を
  SIGTERM → claude 終了 → bash EXIT trap で `boid job done` → 次回 task 再開時に
  **新規 claude code が起動される**
- 新規 claude code instance には 「子から ask が来た」 という情報を
  `task.yaml` / `payload.yaml` / `instructions.yaml` に書き戻したうえで渡す
  必要がある。 signal は OS プロセス層の事象であって、 上位の Claude Code session
  に状態を運ぶ手段ではない
- skill 側にも 「自分の awaiting 起因が user 回答待ちなのか、 子からの ask なのか」
  を判別して扱うロジックが追加で必要
- これらを総合すると、 signal 1 本では完結せず、 既存 notify-awaiting 経路から
  本質的に並列の経路を設計し直すことになる

実装の練り込みが本設計の範囲を超えるため、 daemon push は採用しない。

## 親の診断手段

子の作業結果を親 supervisor が診断するために、 3 つの情報源を階層的に使う。

### Layer A (一次情報源): payload.artifact.report

task skill (`/boid-task` の executor mode) が `done_request` 発行前に必ず書く構造化レポート。
親 supervisor は `boid task show <child> --field payload.artifact.report` で structured
に読む。

提案 schema (free-form map なので daemon は schema を知らない):

```yaml
payload:
  artifact:
    report:
      summary: "<1-3 行で何をしたか>"
      evidence:
        pr_url: "https://github.com/.../pull/123"   # optional
        commit_sha: "abc1234"                        # optional
        worktree_branch: "boid/316c6611"             # optional
      verification:
        tests_passed: true                            # optional
        ci_status: "green"                            # optional
        manual_checks: ["..."]                        # optional
      caveats: ["..."]
      open_questions: ["..."]
```

executor skill 規約: `done_request` 発行前に必ず `report.summary` を書く。
`evidence` / `verification` は該当時に書く。

### Layer B (独立検証): git / gh による事実確認

子の local branch (`boid/<task_id8>`) は同一 git repo の `.git/refs` に存在するので、
親 sandbox から **push 不要** で確認できる:

```sh
git log main..boid/<id>
git diff main..boid/<id>
git show boid/<id>
```

PR を作成していれば `gh pr view` / `gh pr diff` / `gh pr checks` で追加確認する。
「push を強制」 する案は **採用しない** (workflow によっては push しないケースがある)。

### Layer C (shape diagnostics): transcript.log

`boid job log <child_last_job>` は **runtime の stdout/stderr (PTY 出力) の生キャプチャ**
で、 ANSI escape 等を含む raw terminal capture
(`internal/dispatcher/runtime_local_linux.go:60-135`)。

構造化されていないので 「何ができたか」 の判定には不向き。 「途中で stuck していな
かったか」 などの shape ベース確認 (size / tail 数百行 / 最終更新時刻) に使う。

Phase 1 完了条件に「supervisor sandbox から `boid job log` が読めることを E2E で
確認」 を含める。

## Stop hook 廃止

旧計画では Stop hook を 「parent あり時に done_request を自動発行」 に改修する案
だったが、 この案を **Stop hook 自体の廃止** に変更する。

`boid-kits/claude-code/hooks/boid-stop-settings.json` は Phase 2 で削除する。

### agent の終了経路

agent の終了経路 (Phase 2.c 以降):

1. **明示 notify**: agent が `boid task notify --done|--fail|--ask "..."` を発行 →
   daemon が `done` / `aborted` / `awaiting` に遷移 → claude SIGTERM
2. **暗黙終了**: agent が単に session を終わらせる → claude exit → bash EXIT trap →
   `boid job done` → **task は executing のまま残る**

case 2 のとき task が executing のまま放置されるが、 これは **異常状態** として
supervisor 側で検知・処理する責務になる (「異常子の検知と対処」 参照)。

### kit wrapper fallback の不採用

run-agent.py に fallback notify を入れる案も検討したが、 採用しない。 理由:

- 異常検知を kit 側に持たせると 「ハーネスは軽く」 の原則が崩れる
- supervisor の監視責務と重複する

## backward compat

- **親無し executor**: parent_id が無い task は今までどおり自力 done で完了する。
  既存 「親無し dev task」 はそのまま動く
- **既存 supervisor**: monitoring loop に child awaiting 監視と異常検知を追加する
  だけ。 既存子の挙動は不変
- **kit Stop hook**: Phase 2 で `boid-stop-settings.json` を削除する。 削除前は
  既存挙動 (無条件で `boid agent stop`) のままとし、 改修は経由しない
- **hook job (非対話 script)**: hook job は parent_id を持たないか、 持っていても
  「exit code で自律完了」 が現実的なので、 done_request の対象外
  (= Phase 3 / open question で別途検討)

## 段階移行プラン

| Phase | 内容 | 触る箇所 | 完了条件 |
|---|---|---|---|
| 1 | boid-task skill (Supervisor Mode) の polling を child awaiting 監視に拡張 (answer / reopen / abort / 上向き notify の手順を明文化)。 異常子検知ロジック (stuck executor / 未報告 done_request) を追加。 supervisor sandbox から `boid job log` が読めることを E2E で確認 | skill md のみ | skill 更新 + E2E 確認 |
| 2.a | boid-kits/claude-code Stop hook 削除 (`boid-stop-settings.json` 削除) | boid-kits | kit 更新 |
| 2.b | notify hook 発火条件への parent_id gate (daemon hardcode) | boid 本体 (notify hook 発火条件) | 本体 更新 + 動作確認 |
| 2.c | `notify --done` / `--fail` 導入: 子の終了報告を prefix ベースではなく state 遷移として直接扱う。 `fail: executing → aborted` / `reopen: aborted → executing` 追加、 親の confirm gate 廃止 (子 `--done` で即 done に遷移)、 SKILL.md を 4 mode に整理 | boid 本体 + skill md | 本体 + skill 更新 |
| 3 (将来) | hook job も含めた universal model 検討、 必要なら daemon push 系の再検討 | boid 本体 | open question 解消後 |

**Phase 1 は boid 本体に触らない**。 Phase 2.a/2.b は kit + notify hook 発火条件のみ。
Phase 2.c で state machine と CLI に触る (`--done`/`--fail` flag, `fail` action,
`reopen: aborted → executing`)。 Phase 3 は本ドキュメントの直接の射程外。

### Phase 2.c 詳細

子の終了報告は Phase 2.a/2.b までは `notify --ask "done_request: ..."` のように
prefix で意味付けしていたが、 これは

- supervisor agent が prefix を見落として `task answer` (= awaiting → executing の再 spawn)
  に流すと、 child agent が approval テキストを resume 時に受け取って "やる事が無い"
  まま idle 化し、 user の手動 `/exit` までクローズしない
- root 用と child 用の終了パスが二系統 (`agent stop` vs `notify --ask`) に分かれ、
  Stop hook 撤去 (2.a) 以降は前者が事実上機能しない

という欠陥を残していた。 Phase 2.c はこれを解消するために、 子の終了報告を
**直接 state 遷移にエンコードする** 設計に切り替える:

| 旧 | 新 |
|---|---|
| `notify --ask "done_request: ..."` → awaiting + 親が `action send --type done` で confirm | `notify --done "..."` → 直接 `done` |
| `notify --ask "failure_report: ..."` → awaiting + 親が `reopen` / `abort` | `notify --fail "..."` → 直接 `aborted` |
| `notify --ask "<question>"` (Q&A) | 不変。 `awaiting + question_id` を維持 |
| 親の `action send --type done` で awaiting → done に confirm | 不要 (子が `--done` で直接 done に落ちる)。 `awaiting → done` ルール自体は root user UI の経路として残置 |

**root supervisor の `--done` も silent done を許容**する。 root の場合は notify hook
が発火するので user に desktop notification は届く。 reopen が機能している以上、
awaiting + confirm の往復で user に "完了報告を読ませる" UX は値しない (cf.
[[feedback_design_simplicity]])。

state machine 追加ルール (2 行):

```
{Action: "fail",   FromStatus: "executing", ToStatus: "aborted",   Manual: true}
{Action: "reopen", FromStatus: "aborted",   ToStatus: "executing", Manual: true}
```

`done: executing → done` は既存 ("agent self-completion") を `--done` のターゲットとして
そのまま使う。

## Open Question

1. **auto-confirm policy**: 親が agent supervisor の場合、 「特定パターンの
   done_request は自動承認」 を許すか。 Phase 2.c 以降は子が直接 done に落ちるので
   この question 自体が形を変える: 「親が verify 後に reopen するか否か」 の判断を
   agent skill 内ロジックに委ねる、 という方針は不変
2. **hook job との切り分け**: hook job の done を done_request 対象にするか。
   現案では除外 (exit code で自律完了) だが、 hook 連鎖が深くなった場合の責任
   所在は要再検討
3. **chain 途中の awaiting 山積み**: parent chain が長い escalate になると、
   chain 途中の supervisor が次々 awaiting 状態で並ぶ。 **Layer A の report schema**
   で構造化されるため親の判断コストは下がるが、 web UI での chain 可視化は未解決。
   可視化の責務をどこに持つかは引き続き open
4. ~~**failure_report の意味付け**~~: Phase 2.c で `notify --fail` action として
   明示化されたため解消
5. **daemon push (候補 B/C) の再検討条件**: 本モデルでは polling-only で確定。
   latency が許容できなくなる観測可能な状態に達したら再検討する。 具体的な閾値
   (poll interval 10s まで縮めても解決しない遅延、 等) は実運用で判断

## 関連

- [[project_lifecycle_accountability_model]] — このモデルの設計合意 memo
- [[project_boid_weak_points]] #12 — 直接の起源 incident
- [[project_resume_skip_instructions]] — resume 時の context 喪失問題 (本モデルと
  関連するが直接の解にはならない)
- [[project_boid_task_purpose]] — boid task の人間中心設計
- [[feedback_dev_task_subagent]] — supervisor の subagent 禁止ガイド
- [[feedback_dev_task_granularity]] — executor 子タスクの粒度ガイド
