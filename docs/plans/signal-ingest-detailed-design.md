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
                        scan script → `boid signal list --claim` → 判断 → `boid signal ack`
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
- boid 内部シグナル (action 列) は v0 では inbox を通らない。scan script が
  `boid action list` を直読みする現行のまま (inventory §6 の推し通り)。

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
| `GCSignals(dbtx, olderThan, dryRun)` | acked が cutoff より古い行に加え、**未 ack でも received_at が cutoff より古い行を削除** (dead の永久残留を防ぐ。dead は 30 日間 `--state dead` で可視、それで拾われなければ他の 30 日 GC と同じ扱い) |

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
| `boid signal list [--claim] [--source ...] [--state ...] [--limit N] [--json]` | `signal_list` | scan script が読む。`--claim` は ClaimSignals (attempts++ 込み) |
| `boid signal ack <id>...` | `signal_ack` | 判断後の決着。冪等 |
| `boid signal ingest` | `signal_ingest` | connector 専用。stdin の JSONL を取り込む |
| `boid signal cursor` | `signal_cursor_get` | connector 専用。自 source の栞を返す |

- **policy の割り当て (重要)**: 一般の `boidPolicy` へ足すのは `signal_list`・`signal_ack`
  の 2 op **だけ**。`signal_ingest`・`signal_cursor_get` は宣言 (protocol / mirror /
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
  `trigger_loop.go:732-748`) に乗る。§8.1 の「上限超過は可視化」の実装がこれ
- 手動実行 — `boid trigger run <project> signal:slack/mentions` がタダで手に入る
- 履歴と GC — trigger_runs の既存 30 日 GC

### 5.2 job 環境

導出 trigger の発火は既存 `fireTrigger` → `StartExec` (exec job、readonly、project
sandbox)。`StartExecRequest` に **4 点**を追加する (env・bind と、下記「権限の絞り」の
縮小 policy・service allowlist。いずれも `SessionJobInput` → `BuildSessionJobSpec` への
pass-through field):

- env: `BOID_SIGNAL_SERVICE` / `BOID_SIGNAL_CONNECTOR` / `BOID_SIGNAL_CONFIG` (config の
  JSON) / `BOID_CONNECTOR_EXEC` (下記パス)
- bind: 解決済み Pack ディレクトリ → `/run/boid/integrations/<pack>` (read-only)。
  `Visibility.AdditionalBindings` の既存経路を使う

connector が呼ぶ service は workspace の enabled services に入っている必要がある
(検証は load 時に警告)。gateway 側の 403/502 は既存の 2 層検査のまま。

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
  取り込んだ分の栞は正しく進んでいる)
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
   (外部ライブラリ最小の規約)。検証失敗は**該当 connector の導出 trigger を作らない +
   起動ログにエラー** (採点表 Q19)

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
