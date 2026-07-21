# Phase 5 実装計画: shim 固定ディレクトリ化 + タスクコンテキスト RPC 化

ステータス: 構想 (draft) — 着手判断前
作成日: 2026-07-20
親ドキュメント: [container-based-boid.md](container-based-boid.md) — 移行戦略ステップ 5

---

## 目的

契約先行 (contract-first) の最後の段。 enforcement 差し替え (ステップ 6 以降の
container backend 追加) より先に、 現行 userns backend の上で
**サンドボックス境界の意味論を「ホスト共有 FS 前提」から
「コンテナ + broker RPC 前提」に揃える**。

サブトラックは 2 つで独立に着地可能:

- **5a**: shim をオリジナルコマンドの絶対パスに bind 上書きする方式から、
  固定ディレクトリ (`/opt/boid/bin` 等) + PATH 解決に移す。
  ステップ 2 (git gateway cutover) 完了で git shim が退役済みなため前提充足。
- **5b**: task/instructions/environment/payload のファイル配布と attachments bind を
  boid コマンド (broker RPC) 経由に置換する。 environment.yaml の中身も
  コンテナモデルで残る 2 項目 (allowed_domains + host_commands) に縮退させる。

どちらも単体で現行 boid の複雑さを削り、 かつ現行 userns backend で
dogfood してからコンテナ backend を差し替えるので、 enforcement 移行時に
「エージェント・ハーネス・スキルの契約が回るか」の未知が残らない状態を作る。

スコープ外: sandbox backend interface 化 と container backend 追加 (ステップ 6)、
egress / docker proxy の broker 側再配置 (ステップ 7)、 k8s backend (ステップ 7 後段)。

---

## 決定事項 (2026-07-20)

1. **shim の実体はイメージ / userns ともに boid multi-call binary の symlink 群**。
   現行同様 `argv[0]` で分岐、 実バイナリは 1 個。 配布方法だけが「絶対パス bind」
   から「固定 dir + symlink + PATH」に変わる
2. **broker 側 command lookup key はホスト絶対パスから short name (基本名) に変える**。
   `hostCommandMounts` (絶対パス→ boid バイナリ bind) が消える帰結として、
   shim → broker で照合できる安定 key は short name しかない。 policy 表・
   `EarlyRejectFromEnv` の `BOID_HOST_COMMAND_RULES` も同時に short name key に揃える
3. **タスクコンテキストの伝搬方向は「読み取りは pull, 書き戻しは既存 RPC を拡張」**。
   task/instructions/environment/payload の**取得**は新 CLI 経由の broker RPC で
   都度 pull する。 書き戻し (`task update`/`task notify`/`job done` payload_patch) は
   既存 RPC 語彙をそのまま使う (patch 直渡し版の追加のみ)
4. **environment.yaml は「消す」ではなく「縮退 + CLI 経由に配布路線変更」**。
   コンテナモデルでエージェントから観測不能な情報は 2 つのみ
   (`network.allowed_domains` と `host_commands[]`)。 それ以外
   (`sandbox.*`/`filesystem.*`/`worktree`/`notes` の大半) はセクションごと削除。
   `notes` に残っているサンドボックス癖の説明はスキル側テキストへ移す
5. **attachments は bind 廃止・「list + fetch」の CLI 契約に**。
   `~/.boid/attachments/` の read-only bind をやめ、 `boid task attachments list` と
   `boid task attachments get <name>` の 2 コマンドで明示 pull する。
   実体は broker RPC で bytes を返し、 CLI がローカル (job スコープの
   コンテナ FS or 一時 dir) に書き出す
6. **payload_patch.json は job_done RPC 直渡し版を追加、 fallback は残す**。
   現行 `runner-inner-child` の `postJobDone` が `~/.boid/output/payload_patch.json` /
   `StdoutCaptureFile` を読んで broker に送る経路は、 中で `bytes` に折り畳んだ
   後 `WorkflowService.CompleteJob` に流している。 新契約は
   「エージェントが `boid task update --payload-patch @-` (stdin) で送る」を
   一次経路にし、 file 経路は enforcement 差し替え時に一括退役する
   (5b 内では新経路の並走まで、 退役は Phase 6 前後)
7. **`~/.boid/context` tmpfs overlay は本 phase で退役**。 Phase 4 の
   ステップ 5 完了前の衝突対策 (workspace home bind の上に `$HOME/.boid` だけ
   tmpfs) は、 本 phase の完了で不要になる。 退役の順序は
   「新 CLI が着地 → スキル / adapter bootstrap 側の CLI 化 →
    context ファイル書き / tmpfs overlay 撤去」

---

## 現状棚卸し

### 5a: shim 配置の現状

- **配置ロジック**: `internal/dispatcher/sandbox_builder.go` の
  `hostCommandMounts` (~ L1010-1029) が各 host command の**絶対 host path**
  (例 `/usr/bin/gh`) に `rt.BoidBinary` (`os.Executable()` の結果) を
  read-only bind する。 `buildPATH` (~ L1085-1122) が各 shim の親ディレクトリを
  PATH に prepend
- **shim の実体**: 単一の boid multi-call binary。 bind 先ごとに `argv[0]` で分岐:
  - `argv[0] == "boid"` + `BOID_BUILTIN_SHIM=1` → `RunBoidShim`
  - それ以外 (`gh` 等) → `shimMain()` → `sandbox.EarlyRejectFromEnv` →
    `sandbox.ShimExec` (streaming ExecRequest を broker に送る)
- **broker 側 lookup key**: shim は `os.Executable()` で自分の bind mount target
  (絶対ホストパス) を取得し `ExecRequest.Command` に載せる。
  broker の `entry.Commands[req.Command]` が同じ key で hit する
- **git は退役済み**: `git-gateway-cutover` PR6/8 で `hostCommandMounts` からも
  broker builtin からも撤去済。 予約名として `ResolveHostCommands` /
  `validateBuiltinHostConflict` に残るのみ
- **同一データ源**: `ResolveHostCommands` (`internal/dispatcher/host_commands.go`
  L192-250) の戻り値 (絶対パス map) が **shim mount と broker policy 表の両方**に
  流れる。 両者が同じ resolved map を共有していることが照合の正しさの前提

### 5b: タスクコンテキスト伝搬の現状

サンドボックス内配置 (全て `$HOME/.boid/context/` の tmpfs overlay 上に
`sandbox.FileWrite` として materialize、 job 終了で消滅):

| ファイル | 内容 | 生成元 |
|---|---|---|
| `task.yaml` | id/title/status/behavior/description | `TaskSnapshot` (`orchestrator/jobspec.go`) |
| `instructions.yaml` | `[]RoutedInstruction` | `JobSpec.Instruction` (`orchestrator/planner.go`) |
| `environment.yaml` | sandbox/network/filesystem/host_commands/notes/workspace_projects/session | `buildEnvironmentYAML` (`sandbox_builder.go` L1397-) |
| `payload.json`/`.yaml` | trait filter 後の task.payload | `sandbox_builder.go` L1190-1198、 filter は `orchestrator.FilterPayloadByTraits` |
| `attachments/` (bind) | task 添付ファイル群 (RO) | `sandbox_builder.go` L360-378、 source は `<AttachmentsRoot>/tasks/<id>/attachments` |

env 経路 (併存): `BOID_TASK_ID` / `BOID_JOB_ID` / `BOID_MODEL` / `BOID_INVOKED_*` /
`BOID_INSTRUCTIONS` (JSON) / `BOID_BASE_BRANCH` / `BOID_USER_ANSWER` /
`BOID_WORKSPACE_SLUG`。 これは env なので Phase 5 でも残す (RPC で問い合わせるための
最小 handle として `BOID_TASK_ID` / `BOID_JOB_ID` は必須)。

ハーネス側の読者:

- **claude adapter** (`internal/adapters/claude/run.go` L173-192): `readSessionsFromPayload`
  が `rc.PayloadPath` (既定 `~/.boid/context/payload.json`) を直接開く。
  cold start 時の task 記述はスキル経由でエージェントが自力読み
- **codex / opencode adapter**: `taskBootstrapPrompt` (`codex/run.go` L58-85 /
  `opencode/run.go`) が「Step 2: `~/.boid/context/{task,instructions,environment,payload}.yaml`
  を読め」と指示するだけで Go 側の直接読みなし
- **shell adapter**: 何も読まない
- **契約集約**: `internal/adapters/adapter.go` L80-149 `RunContext.PayloadPath` /
  `OutputDir`

スキル / ドキュメント側の読者:

- `internal/skills/data/boid-task/SKILL.md` (Step 0 の必読 4 files 表、 readonly の
  モード分岐、 executor の `filesystem.project_dir` / `filesystem.writable` 確認、
  `network.restricted` / `tools` 参照、 計 7-8 箇所)
- `internal/skills/data/boid-task/references/data-model.md` (§ environment.yaml の
  スキーマ表)
- `internal/skills/data/boid-orchestrate/SKILL.md` (`/boid-task` の readonly 言及 1 箇所)
- `docs/ja/reference/hook-contract.md` / `docs/en/reference/hook-contract.md` (L19)
- `e2e/scenarios/git-gateway-peer-fetch/workspace/app/.boid/project.yaml` (`grep -E
  '^\s*clone_url:' "$HOME/.boid/context/environment.yaml"` している)

既存 broker RPC (`internal/sandbox/protocol.go` L59-78 の `BoidOp*`):

- `task_get --field <path>` → `TaskAppService.GetTaskField`。 payload の trait を
  dotted で個別に引ける。 現状 payload に触る唯一の broker 読み口
- `task_update --payload-file` → top-level shallow merge
- `task_notify --done/--fail/--ask/--progress/--message`, `task_ask` (blocking),
  `action_send`, `job_done`, `job_list/show/log`, `agent_stop`, `task_create/list/import/reopen/delete`

**新設が必要な RPC**: task 全体取得 / instructions 全体取得 / environment 全体取得
(縮退版) / payload 全体取得 / attachments list + get / job_done の patch 直渡し版。

書き戻し経路 (現行):

- `boid task notify` → broker → `TaskAppService.NotifyTask`
- `boid task update --payload-file` → `task_update` RPC
- **プロセス終了時**: `~/.boid/output/payload_patch.json` (agent が書く) →
  runner-inner-child `postJobDone` / `resolveJobOutput` (`runner_linux.go` L478-512) →
  `brokerclient.JobDone` → broker → `BoidOpJobDone` → `WorkflowService.CompleteJob`
- hook script fallback: `StdoutCaptureFile = /tmp/boid-output`
- **merge**: `orchestrator/coordinator.go` L446-448 `MergePayloadPatch` が YAML/JSON
  パース → trait allowlist で merge → `task.Payload` を差し替え

### environment.yaml の中身と縮退案

現状 `environmentDoc` (`sandbox_builder.go` L1357-1375):

| セクション | Phase 5b 後 |
|---|---|
| `readonly` (bool) | 削除。 task.yaml (task RPC) から取れる |
| `worktree: false` (BC 残置) | 削除 |
| `tools: []string` | 削除。 スキル側で固定表 or CLI (`boid tools list`) に |
| `network.restricted`/`egress`/`proxy_url`/`webfetch` | 削除 (コンテナモデルでは L3 トポロジ) |
| `network.allowed_domains` | **残す** (agent 側で観測不能) |
| `workspace_projects` (peer advertise) | 残す (peer 発見に必須)。 ただし別 CLI (`boid workspace peers`) に切り出す方向も検討 |
| `sandbox.*` (kind/pid_isolated/uid_inside) | 削除 (全部 hard-coded の見せかけ情報) |
| `filesystem.*` | 削除。 コンテナモデルでは「見たまんま」 |
| `host_commands[]` | **残す** (allow/deny/reject の説明が agent 側で観測不能) |
| `session.harness`/`display_name` | 削除。 環境変数 or 別 CLI に |
| `notes` (8 bullet の癖説明) | 削除、 スキル本文へ移設 |

残す 2 項目のみを返す最小 RPC / CLI (仮): `boid task env` (JSON/YAML)。

---

## 目標状態

### 5a 完了時

- サンドボックス内には `/opt/boid/bin/` (仮) に boid multi-call binary 1 個 +
  各 shim 名の symlink 群が置かれる (`boid`, `gh`, `docker` 等)。 PATH は
  `/opt/boid/bin` が先頭
- 現状の「絶対パス bind 上書き」は消滅。 `hostCommandMounts` 全廃
- broker 側 policy 表と `BOID_HOST_COMMAND_RULES` の key は short name
  (`gh`, `docker` 等) に統一。 `ExecRequest.Command` も short name
- コンテナ backend では `/opt/boid/bin` はイメージに焼き込み。
  userns backend では tmpfs 上に symlink を dispatch 時に生成 + PATH prepend

### 5b 完了時

- サンドボックス内 `~/.boid/context/*` ファイル配布と `~/.boid/attachments/` bind
  は廃止。 tmpfs overlay も撤去。 Phase 4 完了時点の暫定オーバーレイが消える
- エージェント (スキル + adapter) はタスクコンテキストを
  `boid task current` / `boid task instructions` / `boid task env` /
  `boid task payload` / `boid task attachments list|get` の CLI 経由で取得
- claude adapter の `readSessionsFromPayload` は broker RPC で payload を取得
  (実体は `boid task payload --field artifact.claude_code.sessions` 相当)
- 環境変数は `BOID_TASK_ID` / `BOID_JOB_ID` を最小 handle として残し、
  `BOID_INSTRUCTIONS` (JSON dump) 等の重量 env は廃止 (RPC で取れる)
- `environment.yaml` 相当のデータは `boid task env` が 2 項目
  (`allowed_domains`, `host_commands`) のみ返す縮退モデル
- コンテナモデルで自然に成立する契約に揃うため、 ステップ 6 (backend swap)
  ではハーネス / スキル側の書き換えが不要になる

---

## PR 分割案

### 5a: shim 固定ディレクトリ化 (先行トラック)

1. **`ResolveHostCommands` の返り値を `{shortName → CommandDef}` に分離**
   (bind 用の絶対パス map と、 policy 用の short name map を返す)。 broker 登録側と
   `BOID_HOST_COMMAND_RULES` を short name key に順次差し替え。 shim mount は既存
   絶対パス方式を維持 (段階的 cutover のためのステージング)。 予約名 (現状
   `validateBuiltinHostConflict` が管理する `boid`/`fetch` 等) も short name 空間で
   回るようテスト追加。 テストは policy 表 drift test + broker exec 経路のユニットで
   covers
2. **shim の broker への Command key を short name に切り替え**。 sandbox 内で
   `sandbox.ShimExec` が `filepath.Base(os.Executable())` (もしくは argv[0]) を
   `ExecRequest.Command` に載せるように。 5a-1 で broker 側が short name を
   受け付ける状態を先に作っているので、 rollback 可能な小さい差分
3. **shim 配置を `/opt/boid/bin` (仮) 固定 dir + symlink 群に切り替え**。
   `hostCommandMounts` を廃止し、 `buildSymlinkDir` (新設) が dispatch 時に
   `runtimes/<id>/shim-bin/` へ boid バイナリ + symlink を材料化して RO bind、
   `buildPATH` は `/opt/boid/bin` (mount target) を先頭に prepend
4. **dogfood + e2e 通し**。 現行の全 host_commands (`gh` 等) が短名経路で正常
   dispatch される確認。 fake host command を使う e2e シナリオがあれば経路照合

### 5b: タスクコンテキスト RPC 化 (独立トラック)

1. **新 RPC 4 種 + CLI コマンド追加** (`boid task current` / `instructions` / `env` /
   `payload`)。 broker 側は既存 `TaskAppService` の拡張 (`GetTask` / `GetInstructions` /
   `GetPayload`) + dispatcher が持つ AllowedDomains + HostCommands を返す
   `WorkspaceEnvView` を返す新 endpoint。 出力は `--format json|yaml`、 `--field`
   dotted path 対応 (`task_get` と揃える)。 この時点ではファイル配布と CLI が並走
2. **attachments RPC + CLI 追加** (`boid task attachments list` / `get <name>`)。
   実体は broker 経由で `<AttachmentsRoot>/tasks/<id>/attachments/` から
   bytes を返す。 CLI は `--output <path>` or stdout に書き出し。 attachments bind も
   並走 (5b-6 で撤去)
3. **adapter bootstrap prompt を CLI 経路に書き換え** (claude/codex/opencode 各
   `run.go`)。 「Step 2: `boid task current` / `boid task env` を実行しろ」に置換。
   `readSessionsFromPayload` は `boid task payload --field artifact.claude_code.sessions`
   の JSON パースへ (adapter は shim 経由で `boid` を叩ける)
4. **スキル書き換え** (`boid-task/SKILL.md`, `references/data-model.md`,
   `boid-orchestrate/SKILL.md`)。 Step 0 の「4 files 表」を「CLI 表」に。
   readonly / project_dir / writable の情報源を「`boid task current` 出力」に統一。
   `docs/ja|en/reference/hook-contract.md` 追随
5. **`environment.yaml` セクション縮退の実装**。 `environmentDoc` のセクションを
   `allowed_domains` + `host_commands` の 2 項目のみに削減。 `notes` は削除、
   `boid-task/SKILL.md` の該当セクションに移設。 `boid task env` が返す JSON /
   YAML と 1:1 対応。 file 配布はまだ残す (5b-6 で撤去)
6. **file 配布と attachments bind の撤去 + tmpfs overlay 撤去**。 `contextFiles` から
   `task.yaml` / `instructions.yaml` / `environment.yaml` / `payload.json/yaml` の
   FileWrite 全廃、 attachments bind 削除、 `~/.boid` の job scope tmpfs overlay
   (Phase 4 の暫定) 撤去。 この PR がステップ 5 の目玉 cutover
   **(landed)**: `contextFiles`/`buildEnvironmentYAML`/`marshalTaskYAML`/
   `marshalInstructionsYAML`/`EnvironmentInput`/`attachments_path.go` を削除。
   `homeMounts` の `$HOME/.boid` tmpfs overlay は workspace-home-bind 分岐から
   撤去 (ProfileInit 分岐の tmpfs は別目的につき維持)。 副作用対策として
   `payload_patch.json` の job 開始時 defensive cleanup
   (`sandbox.Spec.RemoveFiles`) を追加 (論点の対策 (a))。
   `SandboxRuntimeInfo.WorkspacePeerAdvertise` / `Runner.buildPeerAdvertise` は
   「PR 跨ぎで inert」パターンを継続 (削除しない) — 判断は下記未解決論点を参照。
   `e2e/scenarios/git-gateway-peer-fetch` は引き続き `skip` (理由は同ファイル参照)。
   `e2e/scenarios/workspace-home-boid-isolated` (Phase 4 PR6 が pin していた
   「`$HOME/.boid` は job ごとに隔離される」契約) は tmpfs overlay 撤去で契約が
   逆転したため `workspace-home-boid-persists` に改名・全面書き換え —
   「`$HOME/.boid` は $HOME 同様 workspace 内で永続する」+
   「`payload_patch.json` のみ defensive cleanup で job 開始時に消える」の
   2 点を新たに pin。 Web UI の attachments 参照 (paste-attach JS + templ 2 枚)
   も `~/.boid/attachments/<name>` パス表記から `[attachment: <name>]` +
   `boid task attachments get <name>` 表記に書き換え (bind 撤去に伴う実害の
   事前防止)。
   `git-gateway-reopen-reclone` は `boid task payload --field` 経由に書き換え済み
7. **`job_done` の payload_patch 直渡し版 RPC + CLI** (`boid task update
   --payload-patch @-`)。 現行 file 経路 (`~/.boid/output/payload_patch.json` +
   `StdoutCaptureFile`) は runner-inner-child 側で fallback として残す
   (enforcement 差し替え時に一括退役)。 スキル / SKILL.md の done 契約も
   CLI 経路を一次に
8. **e2e シナリオ書き換え + dogfood**。 `git-gateway-peer-fetch` の
   `grep clone_url environment.yaml` を `boid workspace peers` (仮) に置換。
   全 shopping scenario の走行と主要ハーネス (claude/codex/opencode) での dogfood

### 順序と依存

- 5a と 5b は独立で並走可能。 dispatch layer の変更集中を避けるため
  1 週間程度は cutover PR (5a-3, 5b-6) を同時に投げない
- 5b-1 → 5b-3 → 5b-4 → 5b-5 → 5b-6 は順序依存。 5b-2 (attachments) は 5b-6 まで
  ならどこでも入れられる
- 5b-7 は file → CLI の一次経路切替なので、 5b-6 の後がベター (file 経路がまだ
  一次のうちに RPC を先に足すのは複雑さの一時的増加を招く)

---

## 未解決論点

- **`boid task env` を JobKind ごとにどう分けるか**: hook と session と exec で
  返すべき情報が微妙に違う (`session.harness` は session のみ、 workspace_projects は
  hook/session)。 単一 endpoint で全部返して CLI 側で `--field` で削るか、
  JobKind ごとに CLI を分けるか
- **workspace_projects (peer advertise) の CLI 帰属**: `boid task env` の
  1 セクションとして持つか、 `boid workspace peers` として独立させるか。
  peer clone は dispatch 時に runner が済ませるので、 agent 側からの
  「peer 一覧の発見」が主用途 → 独立 CLI 寄り。
  **5b-6 での判断 (2026-07-21)**: `boid task env` へのバンドルは見送り (独立 CLI
  路線を維持)。 `SandboxRuntimeInfo.WorkspacePeerAdvertise` /
  `Runner.buildPeerAdvertise` (`gitgateway_wire.go`) は削除せず「PR 跨ぎで
  inert」のまま継続保持 (5b-6 では新しい consumer を配線しない、 という
  deliberate な選択)。 `boid workspace peers` 実装時にこのデータをそのまま
  再利用する想定
- **`BOID_INSTRUCTIONS` env の廃止判定**: instructions.yaml file を消しても、
  BOID_INSTRUCTIONS env が残ると agent 側の入手経路が 2 系統になる。
  RPC 一本化のため env 側も同時に廃止するのが正だが、 一部スキル / hook が
  依存していないかの棚卸しが必要
- **`~/.boid/output/` の去就**: payload_patch.json と StdoutCaptureFile を
  RPC 化した後、 このディレクトリの残存需要は何があるか (runner-state.json 系は
  Phase 6 の診断成果物回収で別途決着)。 空になるなら overlay も要らない
- **`~/.boid/attachments/` の tmp 落とし先**: CLI 経路で pull した attachments を
  agent が読むローカルパスの規約。 コンテナモデルでは `/tmp/boid/attachments/<name>`
  等が素直 (job スコープ + tmpfs)
- **broker RPC の粒度と rate**: 従来ファイル 1 発で取れていた情報を毎回 CLI で
  pull すると agent 起動時に 5-6 回 shim → broker が走る。 startup ラウンドトリップ
  数の実測、 必要なら `boid task context` (バルク取得) の追加検討
- **shim `/opt/boid/bin` のパス確定**: 名前空間 (`/opt/boid/bin` vs
  `/var/lib/boid/bin` vs `/usr/local/boid/bin`)、 コンテナイメージ焼き込み時と
  userns backend での実装差分
- **short name 衝突**: workspace が `host_commands.docker` を宣言したとき
  サンドボックス内の実 docker CLI (もし居れば) と衝突する。 現行は絶対パス bind
  で強制上書きしていたが、 PATH 方式ではイメージ内実バイナリの存在自体を排除
  する必要がある (コンテナイメージの docker CLI は非同梱にする、等の契約)。
  なお git は Phase 2 で shim 経路から退役済みなため衝突対象外 —
  sandbox 内実 git を `/usr` rbind でそのまま使い、 transport のみ gateway に
  向く現行の形が本 phase 後もそのまま維持される
- **e2e で shim 経路が壊れないための保護テスト**: 5a-3 の cutover で回帰しないよう、
  「サンドボックス内から `which gh` を叩いた path が `/opt/boid/bin/gh` であること」
  等の低水準保護シナリオを 1 本追加する
- **broker RPC のスキーマ安定性契約**: `boid task current` / `payload` の JSON
  出力はスキル (SKILL.md) が field 名で参照する契約になる。 変更時の semver
  ポリシーを doc に明記
- **task-less job での `BOID_TASK_ID` 未消去 (5b-4 codex review、 2026-07-21)**:
  `BuildSandboxSpec` (`internal/dispatcher/sandbox_builder.go`) の
  `setIfNonEmpty(env, "BOID_TASK_ID", spec.TaskID)` は `spec.TaskID` が
  非空のときしか値を「設定」しない — `spec.TaskID == ""` (task-less session)
  でも、 project/workspace の `env:` 設定 (`meta.Env`、
  `internal/server/wire.go` の `sessionDispatcherAdapter.StartSession` 経由で
  `spec.Env` にそのまま流れ込む) に同名キーが紛れていれば、 そのまま
  sandbox env に残る。 boid-orchestrate skill (5b-4) は起動コンテキスト検出を
  `~/.boid/context/task.yaml` の存在チェックから `$BOID_TASK_ID` の非空判定に
  切り替えたため、 この経路の脆弱性がスキル側の誤判定として顕在化しうる
  ようになった (5b-4 では `&& boid task current` の成功チェックを併用する
  緩和策のみ導入、 実在する古いタスク ID が偶然紛れ込むケースまでは
  塞げない)。 恒久対応は `spec.TaskID == ""` のとき `BuildSandboxSpec` が
  `env` から予約済み `BOID_*` キー (少なくとも `BOID_TASK_ID`) を明示的に
  削除する防御、 または project.yaml/workspace.yaml の `env:` で `BOID_*`
  prefix 自体を予約語として拒否するバリデーション。 5b-4 の codex review で
  指摘されたが sandbox_builder.go の変更を伴うため 5b-4 のスコープ外と判断し、
  別 PR に送る (5b-4 PR body に明記)

---

## 参考

- 現状棚卸しの一次情報 (2026-07-20 調査):
  - shim 配置: `internal/dispatcher/sandbox_builder.go` `hostCommandMounts` /
    `buildPATH`、 `internal/dispatcher/host_commands.go` `ResolveHostCommands`
  - タスクコンテキスト: `internal/dispatcher/sandbox_builder.go` `contextFiles` /
    `buildEnvironmentYAML`、 `internal/orchestrator/planner.go`
    `snapshotTask` / `PlanHook`
  - RPC 経路: `internal/sandbox/protocol.go` `BoidOp*`、
    `internal/server/boid_executor.go`
  - adapter 読者: `internal/adapters/claude/run.go` `readSessionsFromPayload`,
    `internal/adapters/{codex,opencode}/run.go` `taskBootstrapPrompt`
- 親計画: [container-based-boid.md](container-based-boid.md)
  - ステップ 5 (本 phase): L780-784
  - shim 配置方針: L549-568 「boid shim の配置: 固定ディレクトリに回帰」
  - タスクコンテキスト方針: L570-590 「タスクコンテキストの伝搬」
- 隣接完結済 phase: [home-workspace-volume.md](home-workspace-volume.md) (Phase 4)、
  [cli-remote-connection.md](cli-remote-connection.md) (Phase 3)、
  [workspace-db-consolidation.md](workspace-db-consolidation.md) (Phase 2.5)
