# Signal ingest v0 — 詳細設計

2026-08-26 起案。`docs/plans/signal-driven-review.md` (r3) の移行順 step 2 (core ingest)・
step 6 (trigger source) と、connector 実行に必要な最小の Pack runtime の実装設計。
envelope は `signal-envelope-inventory.md` で確定した v0 を前提とする。

方針: **新しい機構をほぼ作らない。** inbox の table 2 枚と CLI/op を足し、実行・スケジュール・
単発防止・失敗通知・GC はすべて既存機構 (trigger loop / trigger_runs / failStreak /
TaskGCStore / API gateway / SecretStore) に乗せる。

---

## 1. 全体配線

```text
project.yaml (メタプロジェクト)
  signals.sources[] ──(hydrate)──▶ 導出 trigger "signal:<pack>/<connector>"
  triggers[] (on: signals) ─────▶ 判断側 trigger
                │
                ▼ (既存 TriggerLoop が 1m 毎に評価)
   ┌─ 導出 trigger 発火 ──▶ exec job (project sandbox 内で connector 実行)
   │      connector: gateway で外部 fetch → `boid signal ingest` (JSONL)
   │                                │
   │                                ▼
   │                     signals / signal_cursors (SQLite、1 tx)
   │                                │
   └─ on: signals trigger ──▶ 未 ack Signal があり every 経過なら発火
                                    │
                                    ▼
                        scan script → `boid signal list` → 篩う
                                    → `boid signal claim <判断に回す id>`
                                    → 判断 → `boid signal ack`
```

- **導出 trigger** = ユーザが `triggers:` に書くものと同じ内部表現 (`orchestrator.Trigger`)
  を、daemon が `signals.sources` の 1 件から導出したもの (**source 1 件 = trigger 1 本**)。
  複数 trigger の組み合わせではなく、宣言からの自動生成。生成後は既存の trigger loop が
  ユーザ定義 trigger と区別なく評価・実行する。対比で言うと、**通常の trigger の `run` は
  プロジェクト内のスクリプトを指し、導出 trigger の `run` は Pack 内の connector を指す**
- **「1m 毎に評価」は新設のポーリングではない**。1m は既存 TriggerLoop の評価解像度
  (`orchestrator.TriggerSweepResolution`) で、毎分やるのは「due か」の判定 =
  trigger_runs の timestamp 比較 (+ `on: signals` では未 ack の存在確認 1 クエリ) だけ。
  project.yaml を毎分パースし直すわけではない — trigger 定義は hydrate 済み meta
  (キャッシュ) から読み、project.yaml の変更反映は従来どおり `boid project fetch` 時
- **connector の宣言場所はメタプロジェクトの `.boid/project.yaml`** とする (r3 doc からの
  精密化)。理由: connector job には sandbox の宿主 project が要る。workspace には project が
  無い場合があり、metaproject は現行 khi の trigger job が adapter を回している場所そのもの。
  inbox 自体は workspace 単位 (project → `project_workspaces` で解決)。
- **boid 内部シグナル (action 列) も inbox を通る** (2026-08-28、
  `docs/plans/boid-internal-signal-inbox.md` で決着・PR-1 実装済み)。当初は
  inventory §6 の推しどおり「scan script が `boid action list` を直読みする」
  現行維持だったが、メタプロジェクトが 2 個目に増える時点でこの責務境界違反
  (§3 の表が inbox・cursor・dedup を core の持ち物と定義しているのに、内部
  シグナルだけ workspace 側に 848 行の複製機構が残る) が複製されることが
  分かり、統合する判断に変わった。ingest は `orchestrator.CreateAction` が
  action の書き込みと同一 tx で行う (`pack: boid` を予約名として envelope
  に写像。詳細は同 doc §4)。

---

## 2. DB (migration 0046)

前提: 現在の最新 migration は `0045_card_sti_migration.sql`
(`internal/db/migrate/migrate.go:476-478`)。実装時点の次番号を使う。

```sql
-- 0046_add_signal_inbox.sql
CREATE TABLE IF NOT EXISTS signals (
  workspace_id TEXT NOT NULL,           -- workspaces.slug (FK は張らない: project_workspaces の前例)
  service      TEXT NOT NULL,           -- service instance 名
  connector    TEXT NOT NULL,           -- "<pack>/<connector>"
  id           TEXT NOT NULL,           -- envelope id (connector が生成する安定 event key)
  occurred_at  TEXT NOT NULL,           -- RFC3339 (tz-aware)
  identity     TEXT NOT NULL,
  url          TEXT NOT NULL DEFAULT '',
  author       TEXT NOT NULL DEFAULT '',
  title        TEXT NOT NULL DEFAULT '',
  received_at  TEXT NOT NULL,           -- daemon 時計
  attempts     INTEGER NOT NULL DEFAULT 0,
  acked_at     TEXT,
  PRIMARY KEY (workspace_id, service, connector, id)
);
CREATE INDEX IF NOT EXISTS idx_signals_pending
  ON signals(workspace_id, occurred_at) WHERE acked_at IS NULL;

CREATE TABLE IF NOT EXISTS signal_cursors (
  workspace_id TEXT NOT NULL,
  service      TEXT NOT NULL,
  connector    TEXT NOT NULL,
  cursor       TEXT NOT NULL DEFAULT '',
  updated_at   TEXT NOT NULL,
  PRIMARY KEY (workspace_id, service, connector)
);
```

- **dedup は PK そのもの**: `INSERT OR IGNORE` で同一 (workspace, service, connector, id) の
  再投入は no-op (採点表 Q10)
- サイズは 1 行を JSON エンコードした値に既存の `orchestrator.ValidateContentSize`
  (64KiB、`internal/orchestrator/content_size.go:76`) を適用 (未決だった size limit の決着)
- 登録手順は定型どおり: `allMigrations()` へ `tableExists` skip 付きで追記 →
  `TestSchemaGolden` を `-update-golden` で再生成 → `TestMigrationFilesAllWired`

### store 層 (既存パターン踏襲)

`internal/orchestrator/signal_store.go` に `db.DBTX` を取る package 関数:

| 関数 | 契約 |
|---|---|
| `IngestSignals(dbtx, ws, service, connector, rows)` | **1 tx**: `INSERT OR IGNORE` → cursor を `max(rows.occurred_at)` へ **単調にのみ** 前進 (現在値より小さければ据え置き)。挿入とカーソル前進が同一 tx = crash しても取りこぼさない (Q13)。occurred_at と cursor の比較は RFC3339 を**時刻として parse して**行う (文字列比較は offset 混在 — jira は `+0900` で来る — で壊れる)。保存は UTC へ正規化した RFC3339 |
| `GetSignalCursor(dbtx, ws, service, connector)` | 無ければ空文字 (= 最初から) |
| `ListSignals(dbtx, filter)` | filter: ws (必須) / service / connector / state (pending・dead・acked・all) / limit。pending は `acked_at IS NULL AND attempts < max`、dead は `acked_at IS NULL AND attempts >= max`。occurred_at 昇順 |
| `ClaimSignals(dbtx, ws, limit, maxAttempts)` | **1 tx**: pending を昇順に limit 件 select → `attempts = attempts + 1` → 返す。dead は返さない (無限再配送の防止、khi の MAX_ATTEMPTS 相当。v0 は定数 5) |
| `AckSignals(dbtx, ws, ids)` | `acked_at = now WHERE acked_at IS NULL`。**既 ack は no-op (冪等、Q14)**、未知 id はエラーで列挙 (typo 検出)。照合は **(workspace_id, id)** で行い、service/connector を跨いで一致した行は全て ack する (id は複合 PK の一部で workspace 内一意ではないため、規則をここで固定する) |
| `HasPendingSignals(dbtx, ws, maxAttempts)` | trigger 述語用。**dead は数えない** — dead だけが残った workspace で trigger が永久に発火し続けるのを防ぐ |
| `GCSignals(dbtx, olderThan, dryRun)` | **pending (未 ack かつ attempts < max) の行は削除対象に絶対に入らない** — 対象は acked 行と dead 行 (未 ack かつ attempts >= max) のみで、cutoff はその 2 つにだけ効く (acked は acked_at、dead は received_at)。dead は 30 日間 `--state dead` で可視、それで拾われなければ他の 30 日 GC と同じ扱い。(記述訂正 2026-08-28: 旧記述「未 ack でも received_at が古ければ削除」は attempts < max の pending 行まで削除してしまう書き方だった — `internal/orchestrator/signal_store.go` の実装 (H1 修正済み) は当初からこの構造ゲート付きで、この表の記述が実装に追いついていなかっただけ) |

- 細い interface を `internal/api/store.go` に追加 (`SignalStore`)、`TaskRepository` に
  一行 wrapper (`GCTriggerRuns` の型)。GC は `TaskGCStore.GC` の既存 tx 内に
  `GCSignals` を追加し、`GCResult`・`gcResponse`・`cmd/gc.go` に counter を足す (定型 A)

---

## 3. `boid signal` CLI

### 3.1 host 側 (cobra、scopeRemote)

```text
boid signal list [--workspace <slug>] [--source <pack>/<connector>] [--service <name>]
                 [--state pending|dead|acked|all] [--limit N] [-o json]
boid signal ack  [--workspace <slug>] <id>...
```

- route: `GET /api/signals`・`POST /api/signals/ack`。handler は
  `internal/api/signal_handler.go` (card_handler の型)、`mountRoutes` で mount
- `--workspace` 既定は `default` slug。DTO は `internal/apiwire` に置く
  (cmd/client は internal/api を import しない規約)
- leaf command には `boid.scope` annotation (scope_annotations_test が強制)

### 3.2 sandbox 側 (shim builtin op)

| shim コマンド | op 定数 | 用途 |
|---|---|---|
| `boid signal list [--source ...] [--state ...] [--limit N] [--json]` | `signal_list` | scan script が読む。**副作用なし** |
| `boid signal claim <id>...` | `signal_claim` | **判断に回す行を名指しして attempts++** (2026-08-29 新設) |
| `boid signal ack <id>...` | `signal_ack` | 判断後の決着。冪等 |
| `boid signal ingest` | `signal_ingest` | connector 専用。stdin の JSONL を取り込む |
| `boid signal cursor` | `signal_cursor_get` | connector 専用。自 source の栞を返す |

- **`list --claim` は deprecated (2026-08-29)**。読み出しが返した行に一律 attempts++ する
  形は「読んだ」と「判断に回した」を区別できず、**読みが判断より広い consumer** (khi は
  合流を見込んで `MAX_TARGETS * 4` 行読み、判断するのは最大 `MAX_TARGETS`) が、次巡送りに
  しただけの行の attempts を焼く。5 巡で誰も判断していない signal が dead に落ちる。
  `list` (副作用なし) + `claim <id>...` (名指し) に分けた。**時間経過による失効には
  しない** —— 時計は「処理できない signal」と「処理する側が居なかった」を区別できず、
  workspace が 1 週間止まればその間に届いた signal を全部失効させる (`GCSignals` が
  pending 行を時間だけで消さないのは同じ理由で、実際に data-loss bug として直した経緯が
  ある)。互換のため `--claim` は当面残す —— daemon のデプロイと workspace 側の
  `boid project fetch` は別の手順なので、同時に切り替えられない。

- **policy の割り当て (重要)**: 一般の `boidPolicy` へ足すのは `signal_list`・
  `signal_claim`・`signal_ack` の 3 op **だけ**。`signal_ingest`・`signal_cursor_get` は宣言 (protocol / mirror /
  escape-manifest) はするが一般 policy へは**入れない** — 付与は PR-5 の connector 専用
  縮小 policy のみ。通常 job から ingest / cursor は常に拒否される (PR-3 の時点では
  「誰も呼べない op」として存在する)。checklist の手順 3 を機械的に適用しないこと
- `signal_ingest` の stdin は shim が上限 10MiB (既存 `PayloadPatchMaxBytes` と同値) で
  読み切って送る。超過はエラー (握り潰さない)
- workspace scoping は broker が job token から注入する (`card_list` の
  `internal/sandbox/broker.go:502-524` と同型)。**引数で workspace を指定させない**
- `ingest`/`cursor` の source/service は引数でなく env (`BOID_SIGNAL_SERVICE` /
  `BOID_SIGNAL_CONNECTOR`、§5) から取る — connector が他 source の栞を触れない
- readonly は builtin op を縛らない現行仕様のまま (khi の trigger job が readonly で
  task_create を打つのと同じ)。ack/ingest も readonly job から打てる
- **新 op の追い先 11 ファイル** (探索で確定した checklist): `protocol.go` (op 定数) /
  `policy_ops.go` (mirror) / `policy.go` (boidPolicy) / `boid_shim.go` (語→request) /
  `broker.go` (scoping) / `boid_executor.go` (実行) / `policy_translate_test.go` (mirror 表) /
  `policy_test.go` (wantOps、**length 比較で hard fail**) /
  `broker_op_escape_test.go` (**AST 列挙で hard fail**) / shim・broker・executor の各テスト

### 3.3 組み込みスキル `boid-signal`

`internal/skills/data/boid-signal/SKILL.md` を追加し、`deploy.go:13` の `go:embed` に
1 行足すだけで全 job へ read-only mount される (現行の配布機構)。内容は
list/claim/ack の使い方と「ack = この Signal について判断を書き終えた」の意味論。
**契約ではなく知識** (r3 §8.2)。

---

## 4. Trigger source `on: signals`

### 4.1 schema

`orchestrator.Trigger` (`internal/orchestrator/spec_types.go:408-425`) に field を 1 つ足す:

```yaml
triggers:
  - name: sweep
    on: signals        # 省略時 "schedule" (現行と完全互換)
    every: 2m          # signals でも必須 — 発火間隔の下限 = debounce
    run: python3 -m khi.app.scan
```

`ValidateTriggers` (`trigger_validate.go:26`) に `on ∈ {"", "schedule", "signals"}` を追加。
`every` の必須・下限 (1m) は両 kind 共通のまま。

### 4.2 述語

`SweepTriggers` の due 判定 (`internal/api/trigger_loop.go:414-439`) を拡張:

```text
due := now - latestRun.StartedAt >= every        # 現行 triggerIsDue のまま
if trig.On == "signals":
    due = due && HasPendingSignals(workspaceOf(project))
```

- **debounce はこの形で構造的に成立する**: every 窓内に何件 Signal が来ても発火は 1 回、
  発火後も未 ack が残っていれば (= 判断が crash した / 捌き切れなかった) every 経過後に
  再発火する。crash 回復と coalescing が同じ 1 行で出る
- single-flight は既存の `trigger_runs` partial unique index
  (`0043:71-72`、(project_id, trigger_name) で DB レベル強制) に**一切手を入れない**
- workspace 未所属 project の `on: signals` は常に not due (debug log のみ)
- skip streak (stuck 検知) は every ベースの現行式がそのまま働く
- workspace の解決は trigger loop が既に使っている hydrate (`hydrateMetaForTriggers` =
  `Meta.GetWithWorkspace`) の結果を使う。`HasPendingSignals` は `TaskWorkflowService` の
  store interface (`internal/api/store.go`) へ追加して配線する

---

## 5. Connector 実行 = 導出 trigger

### 5.1 宣言 (メタプロジェクトの project.yaml)

```yaml
signals:
  sources:
    - connector: slack/mentions      # <pack>/<connector>
      service: slack-api             # service instance 名
      every: 10m
      config:
        include_threads: true
```

`ProjectMeta` に `Signals` を追加。hydrate 時に source 1 件を**導出 trigger**
`{Name: "signal:<pack>/<connector>", Every: every, Run: exec "$BOID_CONNECTOR_EXEC"}` に
展開して既存の trigger 列へ足す (名前衝突は load 時に検証)。これにより:

- スケジュール・1m 解像度・**single-flight** — trigger_runs がそのまま担う
- **失敗の可視化** — connector の非ゼロ exit は既存 failStreak (3 回毎に通知、
  `trigger_loop.go:732-748`) に乗る。§8.1 の「上限超過は可視化」の実装がこれ。
  **StartExec 自体の dispatch 失敗 (pack 未導入・config schema 違反・pack version 曖昧
  など、§6.2 決着済み) も同じ failStreak に計上される**
  (`TriggerDispatchFailureExitCode`、PR-5 実装時に追加 — 当初 `fireTrigger` の
  fail-open 経路は dispatch 失敗を `slog.Warn` のみで `trigger_runs` 行を削除して
  次周期リトライするだけだったため、`TriggerLoop.trackFailStreak` (`Completed` だけを
  見る) には一切現れず、通知が飛ばないまま無期限リトライし続ける抜けがあった。
  `SweepTriggers` が dispatch 失敗時に `trigger_runs` 行の削除 (= fail-open の即時
  リトライ性は維持) と**別に**、`TriggerCompletionResult{ExitCode:
  TriggerDispatchFailureExitCode}` を `Completed` へ計上することで、既存の
  `trackFailStreak` 機構に無改造で乗せた)
- 手動実行 — `boid trigger run <project> signal:slack/mentions` がタダで手に入る
- 履歴と GC — trigger_runs の既存 30 日 GC

### 5.2 job 環境

導出 trigger の発火は既存 `fireTrigger` → `StartExec` (exec job、readonly、project
sandbox)。`StartExecRequest` に **3 点**を追加する (env と、下記「権限の絞り」の
縮小 policy・service allowlist。いずれも `SessionJobInput` → `BuildSessionJobSpec` への
pass-through field):

- env: `BOID_SIGNAL_SERVICE` / `BOID_SIGNAL_CONNECTOR` / `BOID_SIGNAL_CONFIG` (config の
  JSON) / `BOID_CONNECTOR_EXEC` (下記パス)

**bind mount は使わない (2026-08-27、nose 指摘で訂正)。** 当初案は解決済み Pack
ディレクトリを `/run/boid/integrations/<pack>` へ bind mount する形だったが、これは
container backend の DooD (docker-out-of-docker) モデルと相容れない —
`BindMount.Source` は daemon が host の docker/podman に渡す値であり、**daemon
コンテナ自身のファイルシステム上のパスとして解決されるとは限らない**
(`hostVisibleRuntimesDirFor` が `runtimes/` に対して既にやっている変換と同じ罠、
§12.2 (M1) 参照)。daemon と job container は同じ base image から起動される
(`build/container/compose.yml` 決定2) ので、`pack.Dir`
(`integrations.dir` 配下の絶対パス、`build/container/Dockerfile` が焼き込む場所)
は job container 自身のファイルシステムにも既に同じ path で存在する —
`BOID_CONNECTOR_EXEC` を `pack.Dir` 直下に設定するだけで、bind mount という運搬
自体が不要になる。

connector が呼ぶ service は workspace の enabled services に入っている必要がある
(検証は警告のみ、エラーにしない — 実装は `ProjectStore.GetWithWorkspace` から。
**「load 時」は project.yaml parse 時ではなく hydrate 時を指す** — trigger sweep が
1 分毎に project を hydrate するたび呼ばれるため、宣言漏れが解消されるまで警告は
毎分ログに出続ける。daemon 全体の floor は考慮しない、workspace 自身の `services:`
のみとの照合。詳細は `docs/ja/reference/project-yaml.md` の該当節)。gateway 側の
403/502 は既存の 2 層検査のまま。

connector job の権限は通常の exec job より**絞る** (nose レビュー指摘の採用):

- builtin op は `signal_ingest`・`signal_cursor_get` の 2 つだけを許可する。policy は
  job spec 単位で dispatch 時に stamp される既存機構なので、導出 trigger の StartExec が
  専用の縮小 policy を渡すだけ — `task_create` 等は broker が拒否する
- `fetch` builtin も渡さない (connector の外部到達は gateway で足りる)
- API gateway token の service allowlist は workspace の全 enabled services ではなく
  **宣言した service 1 本**に絞る (Registry への登録も job 単位なので渡す値の変更のみ)

これで connector (Pack 由来のコード) が boid の状態や他 service に触れる面が消える。
検査は採点表 Q27。

### 5.3 connector プロセス契約 (Pack conformance の中核)

- 入力: 上記 env。栞は `boid signal cursor` で取得
- 外部 fetch: `$BOID_API_BASE/$BOID_SIGNAL_SERVICE/...` のみ (直接の外部到達は
  egress 制約で元々できない)
- 出力: `boid signal ingest` の stdin へ JSONL。1 行 =
  `{id, occurred_at, identity, url?, author?, title?}` (source ブロックは daemon が
  env から合成する — connector は自分の instance 名を知らなくてよい、§7.2 の binding 相当)
- **栞の契約**: 「cursor より後だけを返す。外部検索の精度に任せず、取得後に
  `occurred_at <= cursor` を自分で落とす」(jira 分精度の実証済み教訓)。重複して返しても
  inbox の dedup で no-op なので、迷ったら広く読む側へ倒す
- ページングは ingest を複数回呼んでよい (1 回ごとに tx が切れる = 途中 crash でも
  取り込んだ分の栞は正しく進んでいる)。**ページングする connector は古い順
  (oldest-first) に ingest すること**。新しい順にしか返さない API (Slack の
  `conversations.history`、Jira の既定 JQL ソート等) は、ページ全件を集めてから
  昇順に並べ替えて ingest すること — `IngestSignals` は cursor を「その呼び出しで
  渡されたバッチの `max(occurred_at)`」へ進める実装であり、newest-first のまま
  ページ単位で ingest すると 1 ページ目で cursor が実質「今」まで進んでしまい、
  2 ページ目取得前に crash すると古いページが永久に失われる (PR-1 実装時に発見、
  Opus review 2026-08-26)
- 終了: 成功 0 / 失敗非ゼロ (握り潰さない — failStreak に乗せる)
- 実行 runtime: connector は宣言 project の sandbox で動くため、必要な runtime
  (python3 等) は**その project の実行 image が持っていること**。公式 Pack v0 は
  python3 前提 (= khi metaproject の image で動く)

---

## 6. Config と Pack loader (最小)

### 6.1 config.yaml

```yaml
integrations:
  dir: /opt/boid/integrations        # 既定値。bare binary はここを差し替える

services:
  customer-jira:
    uses: jira-cloud/jira-cloud@1.2.0
    endpoint: https://example.atlassian.net
    credentials:
      token: JIRA_TOKEN
```

- `ServiceConfig` に `Uses` / `Endpoint` / `Credentials map[string]string` を追加。
  `uses` 指定時は `base_url`/`auth` と排他 (`validateServiceConfig` で検証)
- schema.go に leaf を追加 (set/apply 経路の unknown-key 検査対象にする)

### 6.2 解決タイミング

`config.Load()` は構文検証のみ (manifest の IO を config parse に持ち込まない)。
daemon 起動時 (`wire.go` の gateway 配線点) に新 package `internal/integrationpack` が:

1. `<integrations.dir>/<pack>/<ver>/integration.yaml` を列挙・parse
2. `uses:` を profile へ解決し、**既存の `apigateway.ServiceConfig` へ脱糖**する —
   profile の credential slot (`injection: bearer|basic|header|query` + header 名) を
   既存 `auth:` と同じ形に写す。**gateway 本体 (`internal/apigateway`) は一切変更しない**
3. 失敗 (pack 不在・version 不一致・slot 未 bind・未知 slot・endpoint 要求違反) は
   **起動エラー** — services の eager validation と同じ倒し方。§7.2 の照合検証の実装
4. connector の `config` は manifest の `configSchema` で検証する。v0 は JSON Schema の
   極小 subset (type/object/properties/required、値型は string/number/boolean) を自前実装
   (外部ライブラリ最小の規約)

   **検証タイミングの決着 (PR-5 実装時の判断、当初案からの変更)**: 上記 1〜3 (Pack
   manifest 自体の検証) は起動時に Pack registry 全体を一括で見るので daemon 起動エラーに
   できるが、`config` (project.yaml `signals.sources[].config`) は **project 単位の宣言**
   であり、daemon 起動時に全 project の project.yaml を横断的にスキャンする既存機構が
   無い。「該当 connector の導出 trigger を作らない」という当初案は、hydrate
   (`internal/orchestrator`) が Pack registry (`internal/integrationpack`) を知らない
   という層分離 (`internal/orchestrator` は `internal/integrationpack`/`internal/config`
   を import しない) と両立しない — Pack registry が実際に手に入るのは
   `sessionDispatcherAdapter.StartExec` (`internal/server/wire.go`) の時点のみ。

   よって v0 の実装は: 導出 trigger は (config の妥当性に関係なく) 常に生成される。
   config schema 検証は **StartExec 時点**で行い、失敗すれば `StartExec` がエラーを返す
   (`resolveConnectorExec`、`internal/server/connector_exec.go`)。この失敗は
   `fireTrigger` の既存 fail-open 経路 (次周期リトライ) に乗り、かつ
   `TriggerLoop.trackFailStreak` へも計上される
   (`TriggerDispatchFailureExitCode`、`internal/api/trigger_loop.go`) — 3 回連続で
   通知が飛ぶ、既存の failStreak 通知と同じ扱い。「起動ログにエラー」の代わりに
   「dispatch 失敗として毎周期ログ + failStreak 通知」になる。採点表 Q19 はこの形で
   引き続き満たされる (検証が行われ、失敗が可視化される、という命題自体は変わらない)。

- profile の credential slot が複数ある Pack と、`<ver>` ディレクトリ名が manifest の
  `version` と一致しない Pack は、v0 では**起動エラー** (未対応・不整合を黙って握らない。
  先頭 slot を勝手に取るような縮退をしない)
- `internal/integrationpack` は新 package なので architecture allowlist
  (`scripts/check-internal-architecture.sh` の**両方の配列**) へ登録
- Pack の skills mount (§6.4 の selective mount) は移行順 step 3 (公式 Pack 化) の範囲。
  本設計では connector 実行に必要な範囲だけを実装する

---

## 7. Pack と boid の契約 (Pack contract v1)

Pack が依存してよい boid の面と、boid が Pack に要求する面を 1 箇所に列挙する。
**この節が Pack 作者向け契約の正**であり、conformance test はこの節の機械検査可能な
項目を実装する。manifest の `apiVersion: boid.dev/v1` はこの契約の版を指す。

### 7.1 boid が Pack に提供するもの (Pack が依存してよい面)

| 提供物 | 内容 | 安定性 |
|---|---|---|
| mount 位置 | Pack 一式を `/run/boid/integrations/<pack>` へ read-only bind | v1 で固定 |
| connector 実行環境 | 宣言した project の sandbox の exec job。stdin は閉じられる | v1 で固定 |
| env | `BOID_SIGNAL_SERVICE` / `BOID_SIGNAL_CONNECTOR` / `BOID_SIGNAL_CONFIG` (configSchema 検証済み JSON) / `BOID_CONNECTOR_EXEC` + 既存の `BOID_API_BASE` 等 | **追加のみ**。削除・意味変更は契約版を上げる |
| 外部到達 | `$BOID_API_BASE/$BOID_SIGNAL_SERVICE/...` (credential 注入済み)。egress はこれ以外閉 | 既存 gateway 契約に従う |
| 取り込み口 | `boid signal ingest` (stdin JSONL、1 行 64KiB、複数回呼び出し可、行単位 dedup) / `boid signal cursor` (自 source の栞) | envelope field は**追加のみ** |
| 実行保証 | source ごとに single-flight。every 間隔・1m 解像度で起動。非ゼロ exit は失敗として記録・通知 (failStreak) され、次周期に再実行される | v1 で固定 |
| 権限範囲 | connector job から呼べる builtin op は `signal_ingest` / `signal_cursor_get` のみ。gateway は宣言した service 1 本のみ (§5.2) | v1 で固定 |

### 7.2 Pack が満たすべきもの (boid が要求する面)

| 要求 | 内容 | 検査 |
|---|---|---|
| manifest | `integration.yaml` が §6 の schema に適合し、`apiVersion` が既知の版である。**未知 field はエラー** (strict decode。黙って無視しない) | loader (起動時) |
| profile | 値 (endpoint 実値・credential) を持たない。credential slot と注入方法の宣言のみ | loader |
| connector: 栞 | cursor より後だけを返す (外部検索の精度に任せず、取得後に `occurred_at <= cursor` を自分で落とす)。重複は許容される (dedup が受ける) | conformance test + shadow-a |
| connector: 出力 | JSONL の必須 field (id / occurred_at / identity) を満たす。id は同一 event から決定的に生成する | `ingest` が行単位で検証・拒否 |
| connector: 副作用 | 外部サービスへ書き込まない。gateway 以外へ到達しない | egress 機構 + レビュー |
| connector: 終了 | 成功 0 / 失敗非ゼロ。部分成功でも ingest 済み分は有効 (再実行で収束する作りにする) | conformance test |
| skill | source 側の知識のみを扱い、boid コマンドへの言及を含まない | conformance test (grep) |
| 拡張禁止 | state machine・DB schema・boid の出力 model に触れない (そもそも到達手段が無い) | 構造 (採点表 Q16-18) |

### 7.3 契約の進化規則

- boid 側の提供面 (env・envelope field・CLI) は**追加のみ**。削除・意味変更をするときは
  `apiVersion` を上げ、loader は未知の apiVersion の Pack を読み込まずエラーにする
  (前方互換を装わない)
- Pack は必要とする契約版を manifest の `apiVersion` で宣言する
- conformance test は boid リポジトリに置き、公式 Pack は CI で常時通す (採点表 Q22)。
  custom Pack の作者も同じテストを手元で回せる
- manifest の strict decode と apiVersion 検査は PR-4 に含める。connector/skill 側の
  conformance test は公式 Pack 化と同時に実装する

---

## 8. `boid task create --idempotency-key` (独立 PR)

- `tasks.idempotency_key TEXT` (nullable) + partial unique index
  `(project_id, idempotency_key) WHERE idempotency_key IS NOT NULL` (migration は実装時の次番号)
- 衝突時はエラーでなく**既存 task の id を返して exit 0** (再実行が収束する、の実装)
- CLI flag と `task_create` op の request field を追加。子タスク重複生成バグの再発防止

**既存の `task_resolve_or_capture` との違い** (nose レビュー質問への回答): あちらは
task_identities (外部世界の identity) をキーに card を引き当てる/無ければ起票する口で、
identity の link 意味論 (drop で解放・reopen の再燃経路) が付いてくる。idempotency key は
**外部 identity を持たない task** — 典型は判断が立てる子 task — の重複防止で、キーは
workspace 内部の安定キー (例: 親 card id + 子の世代キー)、link 意味論は無い。identity を
`child:<id>` のような合成値で流用すると、identity 表が「外部イベントの帰属」という
本来の意味を失い、drop の identity 解放と衝突するため、別の口として足す。

---

## 9. 本体 doc への反映 (この設計で決着した未決)

| §12 の未決 | 決着 |
|---|---|
| `boid signal` CLI の最終形 | §3 (list/claim/ack + connector 用 ingest/cursor、workspace は token/flag) |
| inbox の GC | acked 30 日で既存 GC tx に相乗り (§2) |
| Connector の実行詳細 | 導出 trigger + project sandbox exec job + ingest 単位 tx (§5) |
| Signal 1 件の size limit | 既存 ValidateContentSize (64KiB) を行単位に適用 (§2) |
| (新規に確定) source の宣言場所 | メタプロジェクトの project.yaml `signals.sources` (§1) |
| (新規に確定) Pack repo の置き場 | `boid-api-skills` を Pack repo へ発展。boid repo に Pack の中身は置かない。v0 配布は host checkout の bind mount (§10) |

残る未決: Pack の image 同梱・`boid integration install`・signing (v0 は bind mount で
回避)、kit との関係、resolver、scan script テンプレ、Web UI 表示、service profile の
複数 header/OAuth2 表現 (v0 の脱糖は単一 slot のみ)。

---

## 10. PR 分割と採点表の割り当て

本体 doc §14 のルール 4 に従い、C〜E を PR 単位へ割り直す。E (Q23-25) は全 PR で採点。

| PR | 内容 | 主に検査する命題 |
|---|---|---|
| **PR-1** | migration 0046 + signal store + GC 相乗り | Q10 (dedup no-op)、Q13 (at-least-once tx) |
| **PR-2** | host API/CLI (`/api/signals`、`boid signal list/ack`) | Q14 (ack 冪等) |
| **PR-3** | shim op 4 種 + `boid-signal` 組み込みスキル | Q14、(op checklist 11 ファイル) |
| **PR-4** | config (`integrations.dir`・`uses:`) + `internal/integrationpack` + 脱糖 + 検証 | Q16、Q17、Q19、Q21 の骨格 |
| **PR-5** | 導出 trigger + connector 実行 (env/bind/縮小 policy/プロトコル) | Q11 (栞 self-exceeding)、Q12 (失敗可視化 = failStreak)、Q18、Q20、Q27 (権限の絞り) |
| **PR-6** | trigger `on: signals` 述語 | Q15 (debounce/single-flight テスト) |
| **PR-7** | `boid task create --idempotency-key` | Q26 (新設: 下記) |
| **PR-8** | 公式 Pack (slack/jira/bitbucket) + conformance test | Q11、Q21、Q22 |

- 依存: PR-1 → {2,3} → 5、4 → 5、6 は 1 のみに依存、8 は 5 の後。7 は独立。並列 PR の
  interface 衝突に注意 (PR-4 と PR-5 の integrationpack 契約は PR-4 が先)
- **Q26 を本体 doc §14 に追加する**: 「`boid task create` の idempotency key は
  (project, key) で一意であり、同一 key の再実行が新規作成せず既存 task の id を返す
  テストがある」
- **PR-8 が Pack 側実装** (nose レビュー指摘: Pack が無いと機能しない)。**公式 Pack は
  boid repo に置かず、既存の `boid-api-skills` repo を Pack repo へ発展させる** (nose
  判断)。同 repo は既に 13 service の reference skill (80 files / 1.1MB) を持ち、Pack の
  `skills/` 部分そのもの — `integration.yaml` と `connectors/` を足して Pack layout に
  再構成する。boid repo 側に置くのは契約 (§7)・loader・conformance test の枠組みだけで、
  Pack の中身は持ち込まない (ファイル増と、SaaS API と boid の release cycle の結合を
  避ける)。connector の実装は現行 khi の adapter (slack 160 行 / jira 176 行 /
  bitbucket 243 行、いずれも gateway 経由の fetch-only) の移植で、変更点は栞の読み書き
  (`FileBookmarks` → `boid signal cursor`) と出力 (Signal 構築 → JSONL to
  `boid signal ingest`) が主。
  **PR-8 完了までこの機構は end-to-end では動かない** — 初回の実証は PR-8 + shadow-a
- **配布の v0**: container image への preinstall はまだやらない。`integrations.dir` を
  compose volume で host の Pack repo checkout へ bind mount する (Pack 更新 = git pull +
  boid 再起動。build の結合ゼロ)。image への同梱・`boid integration install`・signing は
  未決のまま後続の packaging とする
- shadow-a/shadow-b 自体はこの doc の範囲外 (本体 doc §10.3/§10.4)

---

## 11. 実装時の既知の注意

- `SetMaxOpenConns(1)`: 新しい loop は作らないので DB 競合の追加なし (trigger loop に相乗り)
- trigger loop の due→busy→fire の順序と fail-open (`trigger_loop.go:441-466`) は触らない
- `boid-add-builtin` スキルの checklist は現物と 5 点ズレている (op 数 16→32、
  escape-manifest テストの欠落ほか) — PR-3 のついでにスキルを現物へ追随させる
- `wiring-seams.md` の seam #6 (組み込みスキルの配布経路) も stale — 同上

---

## 12. PR-8 (公式 Pack 実装) 着手前に必ず解決すべき既知の問題

PR-5 (導出 trigger + connector 実行、§5) の独立レビュー (2026-08-26、Opus) で発見。
PR-5 自体のブロッカーではない (§5 は Pack が実在しなくても policy/wiring レベルで検証
できている) が、**PR-8 で実際に Pack を動かす前には必ず解決すること** — 現状のまま
PR-8 を進めると、以下のいずれかで初回デプロイが機能しない・原因不明の失敗になる。

### 12.1 (B1) `LoadPacks` がドットディレクトリを Pack として誤解釈し起動拒否になる

`internal/integrationpack/pack.go` の `LoadPacks` は `<integrations.dir>` 直下の
エントリを無条件に Pack ディレクトリとして扱う。§10 が指定する v0 配布形態
(`integrations.dir` を Pack repo の host checkout へ compose volume で bind mount) では、
その checkout のルートに `.git/` が存在する — `LoadPacks` はこれも「Pack ディレクトリ」
として `<dir>/.git/<version>/integration.yaml` を探しに行き、見つからず hard error で
daemon 起動を拒否する (再現確認済み)。**PR-8 の初回デプロイが起動不能になる**、実運用に
直結する問題。

対応方針 (未実装、PR-8 着手前に選ぶ): (a) `.` から始まるエントリを `LoadPacks` が
明示的にスキップする、(b) `integrations.dir` 自体を Pack repo のサブディレクトリ
(例: `<repo>/packs/`) に向ける運用にして `.git` を配下に含めない、のどちらか。(a) の方が
daemon 側だけで閉じる分シンプル。

### 12.2 (M1) Pack bind mount の host-visible path 境界が未検証 — 解消済み (2026-08-27)

当初 `sessionDispatcherAdapter` は `pack.Dir` (`integrations.dir` 配下の絶対パス、
daemon プロセス自身がファイル読み取りに使う path) をそのまま
`orchestrator.BindMount.Source` として渡し、container backend の DooD
(docker-out-of-docker) bind mount source に使っていた。これは daemon プロセスが
動いているコンテナの中の path であり、実際に docker socket 越しに job container を
起こす **docker daemon (ホスト)** から見た path とは限らない —
`hostVisibleRuntimesDirFor` (`internal/server/wire.go`) が `runtimes/` ディレクトリに
対して既にやっている「host から見える path への変換」と同じ罠。shadow-a 着手時
(公式 Pack を daemon image に焼き込み、実際に connector job を dispatch しようとして)
`mkdir /opt/boid: permission denied` として実際に踏んだ。

**対応 (nose 指摘): bind mount 自体を撤去した。** daemon と job container は同じ
base image から起動される (`build/container/compose.yml` 決定2) ので、`pack.Dir`
は job container 自身のファイルシステムにも既に同じ path で存在する — host-visible
path への変換という迂回路を用意するまでもなく、`BOID_CONNECTOR_EXEC` に `pack.Dir`
をそのまま使えば足りた。§5.2 を修正し、`resolveConnectorExec`
(`internal/server/connector_exec.go`) と `sessionDispatcherAdapter.StartExec`
(`internal/server/wire.go`) から bind 追加処理を削除した。

**ただしこれは「daemon と job が同じ image を共有する」という container backend の
前提に依存する。** workspace が `container_image` を override している場合、その
イメージに Pack が焼き込まれていなければ `BOID_CONNECTOR_EXEC` は ENOENT になる —
custom image を使う workspace で Pack connector を使いたければ、その image 自体にも
該当 Pack を焼き込む運用が前提になる (doc化のみ、未実装のバリデーションあり)。

### 12.3 (M2) HostCommands と同型の権限漏れが他フィールドにも残っている可能性

PR-5 実装時、`SessionJobInput.HostCommands` が connector job にもそのまま漏れる欠陥が
見つかり修正済み (§5.2 実装、`BuildSessionJobSpec` の `ConnectorPolicy` 分岐)。同型の
懸念が他にも残っている:

- **`DockerEnabled`**: メタプロジェクトが `capabilities.docker` を宣言していると、
  connector job にも per-sandbox docker proxy が生える (`Visibility.DockerEnabled`、
  `BuiltinPolicies` とは独立した機構)。connector が docker socket に触れる必要は
  無いはずで、意図しない権限拡大の可能性がある
- **`AdditionalBindings`**: project/kit が宣言する host bind 一式が connector job にも
  そのまま見える (PR-5 は Pack ディレクトリの bind を**追加**しただけで、既存の
  binding を落としていない)。意図的ならそう doc化すべきで、そうでなければ
  `HostCommands` と同じ扱いで空にする必要がある
- **egress allowlist**: §5.3 の「直接の外部到達は egress 制約で元々できない」という
  記述は不正確 — 実装は workspace 全体の `allowed_domains` proxy をそのまま使っており、
  gateway (`$BOID_API_BASE`) を経由しない到達 (workspace の allowlist に載っている
  他ドメインへの直接 HTTP) は塞がれていない。connector 専用の allowlist 縮小は
  実装されていない

対応方針 (未実装): PR-8 着手前に上記 3 点それぞれについて「意図的に許可する
(doc化する)」か「connector job 用に縮小する (実装する)」かを個別に決定すること。
`HostCommands` の修正 (`BuildSessionJobSpec` の `ConnectorPolicy` 分岐、
`internal/dispatcher/session_job.go`) と同じパターン (単一の enforcement point) を
踏襲すれば実装コストは大きくないはず。

### 12.4 conformance test framework (boid 側パート) レビューで判明した follow-up

`packconformance/`（Pack contract v1 conformance test framework、§7.2/§7.3、採点表
Q21/Q22）の Opus レビュー (2026-08-27) で判明。F1 (custom Pack 作者が `internal/` 配下の
package を import できない) と F2 (skill scan が manifest の `skills[].path` を見ず
`skills/` 決め打ち)・F5 (`.git` 誤読を防ぐ discovery ロジックに専用テストが無い) は
同 PR で修正済み。残りは次のいずれかで PR-8 (Pack 実装本体) 着手までに解決すること:

- **F3**: `LoadPacks` (`internal/integrationpack/pack.go`) が hard error にする
  `metadata.name`/`metadata.version` とインストールディレクトリ名の不一致チェックが
  `packconformance` には無い。`ConformancePack` は単一 Pack ディレクトリを直接受け取る
  設計 (§12.1 の B1 を踏まないための意図的な選択、`ConformancePack` 自身の doc comment
  参照) のため、ディレクトリ名との整合性検査は本来 `LoadPacks` 側の責務のままで良いが、
  conformance test 単体を手元で回した custom Pack 作者にはこの不一致が可視化されない
  (`boid` daemon 起動時に初めて発覚する)。追加するなら `ConformancePack` に
  optional な `expectedName`/`expectedVersion` 引数を足すか、`TestOfficialPacks` 側で
  ディレクトリ名 (`discoverPackDirs` が返す `<pack>/<version>` の実ディレクトリ名) と
  `manifest.Metadata` を突き合わせる形が自然
- **F4**: `extension_check.go` の `findExtensionViolations` の `filepath.WalkDir` に
  dot-directory skip が無い (`discoverPackDirs` は §12.1 対策で持っているが、こちらは
  持っていない)。F1 修正で `ConformancePack` が単一 Pack バージョンディレクトリを直接
  受け取る設計のままである限り Pack ディレクトリ自体に `.git` が含まれることは無いので
  現状は実害が無いはずだが、将来 `ConformancePack` の呼び出し形が変わる (例えば
  Pack repo のサブツリーをまるごと渡すような使い方が増える) と沈黙のまま影響が出る
  可能性がある。`discoverPackDirs` と同じ dot-directory skip をここにも合わせておくのが
  安全
- **F6**: `TestOfficialPacks` は 0 Pack ディレクトリ発見時に `t.Skip` ではなく
  `t.Log` してそのまま return している (意図的: PR-8 で最初の公式 Pack が着地するまで
  CI を赤くしないため — 本文参照)。CI の実行結果としては pass 扱いで問題ないが、
  `go test -v` の出力上は「実行されたが何も検査していない」テストと「本当に全部
  green だった」テストが区別しづらい。`t.Skip` に変えると `go test` の集計上は
  skipped 扱いになり pass/skip が視覚的に分かれるが、GitHub Actions の checks UI 上で
  skip がどう見えるかは要確認 (単純に置き換えると CI green の可視性が却って落ちる
  懸念がある)
- **F9**: `docs/plans/signal-ingest-detailed-design.md §12.1, "B1"` という形の
  citation が `packconformance` 内に複数箇所ある。§12.1 自体はこの doc に実在する
  (見出しは「### 12.1 (B1) `LoadPacks` がドットディレクトリを Pack として誤解釈し
  起動拒否になる」) が、citation の書式が「節番号 + カンマ区切りで略称」という
  この doc の他の箇所には無い独自形式になっており紛らわしい、という指摘。次に
  `packconformance` を触る際に `docs/plans/signal-ingest-detailed-design.md §12.1`
  （B1）のように見出しの略称をそのまま埋め込む形に揃えるか、コード側の citation
  文言を統一すること
