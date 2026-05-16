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

すべて既存の `notify --ask` 1 つで表現する。 区別はメッセージ内容 (将来 prefix を
整える余地はある):

| 概念上のイベント | 意味 | 実装 |
|---|---|---|
| `ask` | 質問 / 判断要求 | `boid task notify $BOID_TASK_ID --ask "..."` |
| `done_request` | 「完了したと思うので確認してください」 | 同上 (メッセージ本文で done_request だと示す) |
| `failure_report` | 異常 / 自発的 abort 要求 | 同上 (失敗内容をメッセージで伝える) |

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
2. 親 supervisor の monitoring loop (現状 sleep 60s 周期、 boid-supervisor skill
   参照) で:
   - `boid task list --parent $BOID_TASK_ID --status awaiting` で awaiting な子を
     列挙
   - 子の awaiting 内容を `boid task get <child> --field awaiting` で読む
   - 自分の文脈で **answer / reopen / abort / 上向き notify** を判断
3. 親 supervisor 自身が awaiting 中の場合は polling が止まっているので、 子 ask は
   親 resume 後に拾われる (X1 採用: payload キュー無しで polling-only)
4. parent chain の root (= parent_id 無し) のみ user 通知 (web UI / push) が発火

latency = `poll_interval × parent chain の階層数`。 階層は通常 1-2 で許容範囲、
必要があれば poll interval を縮めて緩和できる。

### user 通知スクリプトの発火条件

`notify --ask` 自体は task を `awaiting` にする操作で、 user への通知 (push,
web UI, 外部 webhook 等) は notify hook の発火で行われる。 本モデルでは notify
hook の発火条件を次の通り定義する:

- **parent_id 無し (root task) の notify --ask**: 既存どおり user 通知 hook を発火
- **parent_id あり (子 task) の notify --ask**: user 通知 hook は **発火させない**。
  親 supervisor が polling で気づくのが正しい配送経路

これにより 「親が agent supervisor のはずの子 ask が user の手元に push される」
誤配送を防ぐ。 実装は notify hook の発火条件 (project.yaml の notify hook
expression または daemon の hook 評価ロジック) で `parent_id` を条件として組み込む。

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

## Stop hook 改修案 (構造的強制 — コア変更)

「ask せずに stop で消える」 root cause を構造的に塞ぐ。

`boid-kits/claude-code/hooks/boid-stop-settings.json` (現状は無条件で
`boid agent stop $BOID_JOB_ID` を発火) を、 以下のチェック付き挙動に改修する:

```sh
# 擬似コード
if task has pending notify --ask:
  # 既に user/親に質問中。 触らず agent stop
  boid agent stop "$BOID_JOB_ID"
elif task has parent_id:
  # 親持ち子 task: done_request を親に自動発行してから stop
  boid task notify "$BOID_TASK_ID" \
    --ask "done_request: <task title> が完了したと思います。 確認お願いします。"
  boid agent stop "$BOID_JOB_ID"
else:
  # root task (parent_id 無し): 現状維持で auto-confirm
  boid agent stop "$BOID_JOB_ID"
```

これにより:

- 親持ち子 task は **自力で `done` 確定できなくなる** (= 構造的強制)
- root task は web UI で done を見て事後確認 (= 現状維持)
- agent が 「画面に質問書いて Stop」 してしまっても、 done_request が親に上がる
  ので親 supervisor が polling で気づける

### Stop hook が touch する情報

- `BOID_TASK_ID` / `BOID_JOB_ID` (既存 env、 追加不要)
- task の pending notify 有無 → daemon 経由で取得
  (`boid task get $BOID_TASK_ID --field awaiting`)
- task の parent_id → 同上 (`boid task get $BOID_TASK_ID --field parent_id`)

## backward compat

- **親無し executor**: parent_id が無い task は今までどおり自力 done で完了する。
  既存 「親無し dev task」 はそのまま動く
- **既存 supervisor**: monitoring loop に child awaiting 監視を追加するだけ。
  既存子の挙動は不変
- **kit**: Stop hook 改修は claude-code kit のみで完結。 他 kit があれば各 kit が
  別途同等の改修をする責任を負う
- **hook job (非対話 script)**: hook job は parent_id を持たないか、 持っていても
  「exit code で自律完了」 が現実的なので、 done_request の対象外
  (= Phase 3 / open question で別途検討)

## 段階移行プラン

| Phase | 内容 | 触る箇所 | 完了条件 |
|---|---|---|---|
| 1 | boid-supervisor skill の polling を child awaiting 監視に拡張、 親判定 (answer / reopen / abort / 上向き notify) の手順を明文化 | skill md のみ | skill 更新 + 動作確認 |
| 2 | boid-kits/claude-code Stop hook 改修 (parent あり時 done_request 自動発行) + notify hook 発火条件への parent_id ガード追加 | boid-kits + boid 本体 (notify hook 発火条件) | kit / 本体 更新 + 動作確認 |
| 3 (将来) | hook job も含めた universal model 検討、 必要なら daemon push 系の再検討 | boid 本体 | open question 解消後 |

**Phase 1 は boid 本体に触らない**。 Phase 2 は notify hook の発火条件のみ本体に
入る (kit のみでは parent_id ガードを表現しきれないため)。 Phase 3 は本ドキュメント
の直接の射程外。

## Open Question

1. **auto-confirm policy**: 親が agent supervisor の場合、 「特定パターンの
   done_request は自動承認」 を許すか。 許す場合の policy 記述場所は (skill 内
   logic / project.yaml / kit metadata)。 現案では agent skill 内ロジックに
   委ねる
2. **hook job との切り分け**: hook job の done を done_request 対象にするか。
   現案では除外 (exit code で自律完了) だが、 hook 連鎖が深くなった場合の責任
   所在は要再検討
3. **chain 途中の awaiting 山積み**: parent chain が長い escalate になると、
   chain 途中の supervisor が次々 awaiting 状態で並ぶ。 web UI で chain を可視化
   する責務がどこにあるか
4. **failure_report の意味付け**: 現案では `--ask` メッセージで failure を伝える
   が、 将来 action type を明示的に追加する余地は残す
5. **daemon push (候補 B/C) の再検討条件**: latency が許容できなくなる観測可能な
   状態に達したら再検討する。 具体的な閾値 (poll interval 10s まで縮めても
   解決しない遅延、 等) は実運用で判断

## 関連

- [[project_lifecycle_accountability_model]] — このモデルの設計合意 memo
- [[project_boid_weak_points]] #12 — 直接の起源 incident
- [[project_resume_skip_instructions]] — resume 時の context 喪失問題 (本モデルと
  関連するが直接の解にはならない)
- [[project_boid_task_purpose]] — boid task の人間中心設計
- [[feedback_dev_task_subagent]] — supervisor の subagent 禁止ガイド
- [[feedback_dev_task_granularity]] — executor 子タスクの粒度ガイド
